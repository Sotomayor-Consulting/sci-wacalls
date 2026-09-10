package main

// Integración con Chatwoot (canal API). Puente bidireccional de mensajes entre
// una sesión de WhatsApp y una bandeja de entrada (inbox) tipo API de Chatwoot:
//   - WhatsApp -> Chatwoot: los mensajes entrantes 1:1 crean/actualizan un
//     contacto y una conversación y se postean como mensaje "incoming".
//   - Chatwoot -> WhatsApp: el webhook de Chatwoot (message_created/outgoing)
//     dispara el envío del texto al contacto por WhatsApp.
//
// Código original de sci-wacalls. Usa la Application API pública de Chatwoot
// (header api_access_token) documentada en chatwoot.com/developers.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// cwChatIDAttr es el custom attribute del contacto de Chatwoot donde guardamos
// el JID de WhatsApp del chat. Es la fuente autoritativa para rutear el saliente
// (más robusto que reconstruir desde phone_number, que falta en contactos LID).
const cwChatIDAttr = "wacalls_chat_id"

// ChatwootConfig es la conexión de una sesión con una cuenta+inbox de Chatwoot.
type ChatwootConfig struct {
	URL             string `json:"url"`
	AccountID       int    `json:"account_id"`
	AccountToken    string `json:"account_token"`
	InboxID         int    `json:"inbox_id"`
	InboxIdentifier string `json:"inbox_identifier"`
}

func (c ChatwootConfig) valid() bool {
	return c.URL != "" && c.AccountID != 0 && c.AccountToken != "" && c.InboxID != 0
}

func (c ChatwootConfig) base() string {
	return strings.TrimRight(c.URL, "/") + "/api/v1/accounts/" + strconv.Itoa(c.AccountID)
}

var cwHTTP = &http.Client{Timeout: 30 * time.Second}

// req hace una llamada JSON a la Application API y devuelve el body crudo + status.
func (c ChatwootConfig) req(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("api_access_token", c.AccountToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cwHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

// ---------- WhatsApp -> Chatwoot ----------

// handleIncomingMessage empuja un mensaje entrante de WhatsApp a Chatwoot
// (texto o adjunto). Solo procesa conversaciones 1:1 que no vienen de la propia
// cuenta (los grupos quedan fuera de alcance: el Inbox B es una línea 1:1).
// Acepta tanto direccionamiento por teléfono (s.whatsapp.net) como por LID
// (@lid) — WhatsApp está migrando a LID y muchos entrantes llegan así.
func (s *Session) handleIncomingMessage(evt *events.Message) {
	if evt.Info.IsFromMe || evt.Info.IsGroup {
		return
	}
	switch evt.Info.Chat.Server {
	case types.DefaultUserServer, types.HiddenUserServer:
		// 1:1 por teléfono o por LID — OK
	default:
		return // newsletter, broadcast, grupo, etc.
	}
	text := messageText(evt.Message)
	media, hasMedia := extractIncomingMedia(evt.Message)
	if !hasMedia && text == "" {
		return // ni texto ni media (recibos, reacciones, protocolo) — se ignora
	}
	cfg, ok := s.mgr.store.getChatwoot(s.mgr.appCtx, s.id)
	if !ok || !cfg.valid() {
		return
	}

	phone := s.realPhone(evt.Info.MessageSource)
	if phone == "" {
		s.log.Warn("chatwoot: no se pudo resolver el teléfono del entrante", "chat", evt.Info.Chat.String())
		return
	}
	// Normalizamos el chatID al JID de teléfono para que entrante y saliente
	// mapeen a la MISMA conversación de Chatwoot.
	chatID := phone + "@" + string(types.DefaultUserServer)
	name := evt.Info.PushName
	if name == "" {
		name = phone
	}

	convID, err := s.mgr.store.lookupConversation(s.mgr.appCtx, s.id, chatID)
	if err != nil {
		s.log.Error("chatwoot: lookup conversation failed", "err", err)
		return
	}
	if convID == 0 {
		convID, err = s.ensureChatwootConversation(cfg, chatID, phone, name)
		if err != nil {
			s.log.Error("chatwoot: ensure conversation failed", "err", err)
			return
		}
	}

	if hasMedia {
		data, err := s.client.DownloadAny(s.mgr.appCtx, evt.Message)
		if err != nil {
			s.log.Error("chatwoot: download media failed", "err", err)
			return
		}
		if err := cfg.postAttachment(s.mgr.appCtx, convID, media.caption, media.filename, media.mimetype, data, "incoming"); err != nil {
			s.log.Error("chatwoot: post incoming attachment failed", "err", err)
			return
		}
		s.log.Info("chatwoot: entrante (media) → Chatwoot", "phone", phone, "conv", convID, "kind", media.kind)
		return
	}
	if err := cfg.postMessage(s.mgr.appCtx, convID, text, "incoming"); err != nil {
		s.log.Error("chatwoot: post incoming message failed", "err", err)
		return
	}
	s.log.Info("chatwoot: entrante (texto) → Chatwoot", "phone", phone, "conv", convID)
}

// realPhone devuelve el teléfono real (PN) del remitente de un mensaje 1:1,
// resolviendo el LID cuando hace falta. Preferencia: Chat si ya es un teléfono,
// luego SenderAlt (la dirección alternativa que trae whatsmeow), luego el store
// LID→PN, y por último el número del propio LID como fallback.
func (s *Session) realPhone(src types.MessageSource) string {
	if src.Chat.Server == types.DefaultUserServer {
		return src.Chat.User
	}
	if src.SenderAlt.Server == types.DefaultUserServer && src.SenderAlt.User != "" {
		return src.SenderAlt.User
	}
	if pn, err := s.client.Store.LIDs.GetPNForLID(s.mgr.appCtx, src.Chat); err == nil && pn.User != "" {
		return pn.User
	}
	return src.Chat.User
}

// ensureChatwootConversation crea (una sola vez) el contacto, el contact_inbox y
// la conversación en Chatwoot para un chat de WhatsApp, y guarda el mapeo local
// para reusarlo en los mensajes siguientes.
func (s *Session) ensureChatwootConversation(cfg ChatwootConfig, chatID, phone, name string) (int, error) {
	contactID, sourceID, err := cfg.ensureContact(s.mgr.appCtx, chatID, phone, name)
	if err != nil {
		return 0, err
	}
	if sourceID == "" {
		sourceID, err = cfg.ensureSourceID(s.mgr.appCtx, contactID)
		if err != nil {
			return 0, err
		}
	}
	convID, err := cfg.createConversation(s.mgr.appCtx, sourceID)
	if err != nil {
		return 0, err
	}
	if err := s.mgr.store.saveConversation(s.mgr.appCtx, s.id, chatID, contactID, sourceID, convID); err != nil {
		return 0, err
	}
	return convID, nil
}

// ensureContact busca el contacto por teléfono; si no existe lo crea. Devuelve
// el id del contacto y, si la respuesta de creación ya lo trae, el source_id del
// contact_inbox.
func (c ChatwootConfig) ensureContact(ctx context.Context, chatID, phone, name string) (contactID int, sourceID string, err error) {
	if id := c.searchContact(ctx, phone); id != 0 {
		return id, "", nil
	}
	payload := map[string]any{
		"inbox_id":          c.InboxID,
		"name":              name,
		"phone_number":      "+" + phone,
		"identifier":        chatID,
		"custom_attributes": map[string]any{cwChatIDAttr: chatID},
	}
	data, code, err := c.req(ctx, http.MethodPost, "/contacts", payload)
	if err != nil {
		return 0, "", err
	}
	if code < 200 || code >= 300 {
		return 0, "", fmt.Errorf("create contact: status %d: %s", code, string(data))
	}
	var out struct {
		Payload struct {
			Contact struct {
				ID             int `json:"id"`
				ContactInboxes []struct {
					SourceID string `json:"source_id"`
					Inbox    struct {
						ID int `json:"id"`
					} `json:"inbox"`
				} `json:"contact_inboxes"`
			} `json:"contact"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, "", err
	}
	contactID = out.Payload.Contact.ID
	for _, ci := range out.Payload.Contact.ContactInboxes {
		if ci.Inbox.ID == c.InboxID && ci.SourceID != "" {
			sourceID = ci.SourceID
			break
		}
	}
	if contactID == 0 {
		return 0, "", fmt.Errorf("create contact: no id in response: %s", string(data))
	}
	return contactID, sourceID, nil
}

// searchContact devuelve el id del contacto cuyo teléfono coincide, o 0.
func (c ChatwootConfig) searchContact(ctx context.Context, phone string) int {
	data, code, err := c.req(ctx, http.MethodGet, "/contacts/search?q="+phone, nil)
	if err != nil || code < 200 || code >= 300 {
		return 0
	}
	var out struct {
		Payload []struct {
			ID    int    `json:"id"`
			Phone string `json:"phone_number"`
		} `json:"payload"`
	}
	if json.Unmarshal(data, &out) != nil {
		return 0
	}
	want := onlyDigits(phone)
	for _, ct := range out.Payload {
		if onlyDigits(ct.Phone) == want {
			return ct.ID
		}
	}
	return 0
}

// ensureSourceID crea (o recupera) el contact_inbox del contacto para este inbox
// y devuelve su source_id.
func (c ChatwootConfig) ensureSourceID(ctx context.Context, contactID int) (string, error) {
	path := fmt.Sprintf("/contacts/%d/contact_inboxes", contactID)
	data, code, err := c.req(ctx, http.MethodPost, path, map[string]any{"inbox_id": c.InboxID})
	if err != nil {
		return "", err
	}
	if code >= 200 && code < 300 {
		var out struct {
			SourceID string `json:"source_id"`
		}
		if json.Unmarshal(data, &out) == nil && out.SourceID != "" {
			return out.SourceID, nil
		}
	}
	// Ya existía (u otra respuesta): lo leemos del contacto.
	data, code, err = c.req(ctx, http.MethodGet, fmt.Sprintf("/contacts/%d", contactID), nil)
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("get contact: status %d", code)
	}
	var out struct {
		Payload struct {
			ContactInboxes []struct {
				SourceID string `json:"source_id"`
				Inbox    struct {
					ID int `json:"id"`
				} `json:"inbox"`
			} `json:"contact_inboxes"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	for _, ci := range out.Payload.ContactInboxes {
		if ci.Inbox.ID == c.InboxID && ci.SourceID != "" {
			return ci.SourceID, nil
		}
	}
	return "", fmt.Errorf("no source_id for inbox %d", c.InboxID)
}

// createConversation abre una conversación para el contact_inbox dado.
func (c ChatwootConfig) createConversation(ctx context.Context, sourceID string) (int, error) {
	payload := map[string]any{"source_id": sourceID, "inbox_id": c.InboxID}
	data, code, err := c.req(ctx, http.MethodPost, "/conversations", payload)
	if err != nil {
		return 0, err
	}
	if code < 200 || code >= 300 {
		return 0, fmt.Errorf("create conversation: status %d: %s", code, string(data))
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, err
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("create conversation: no id: %s", string(data))
	}
	return out.ID, nil
}

// getConversation lee una conversación de Chatwoot y devuelve su inbox y los
// datos de contacto (teléfono/nombre) — lo que el widget necesita para llamar.
func (c ChatwootConfig) getConversation(ctx context.Context, convID int) (inboxID int, phone, name string, err error) {
	data, code, err := c.req(ctx, http.MethodGet, fmt.Sprintf("/conversations/%d", convID), nil)
	if err != nil {
		return 0, "", "", err
	}
	if code < 200 || code >= 300 {
		return 0, "", "", fmt.Errorf("get conversation: status %d", code)
	}
	var out struct {
		InboxID int `json:"inbox_id"`
		Meta    struct {
			Sender struct {
				PhoneNumber string `json:"phone_number"`
				Name        string `json:"name"`
			} `json:"sender"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, "", "", err
	}
	return out.InboxID, out.Meta.Sender.PhoneNumber, out.Meta.Sender.Name, nil
}

// postMessage crea un mensaje en la conversación. dir es "incoming" o "outgoing".
func (c ChatwootConfig) postMessage(ctx context.Context, convID int, content, dir string) error {
	payload := map[string]any{"content": content, "message_type": dir}
	path := fmt.Sprintf("/conversations/%d/messages", convID)
	data, code, err := c.req(ctx, http.MethodPost, path, payload)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("post message: status %d: %s", code, string(data))
	}
	return nil
}

// ---------- Chatwoot -> WhatsApp ----------

// cwAttachment es un adjunto en el webhook de Chatwoot.
type cwAttachment struct {
	DataURL  string `json:"data_url"`
	FileType string `json:"file_type"` // image | audio | video | file
}

// chatwootWebhookPayload es el subconjunto del webhook de Chatwoot que usamos.
type chatwootWebhookPayload struct {
	Event        string         `json:"event"`
	MessageType  string         `json:"message_type"`
	Content      string         `json:"content"`
	Private      bool           `json:"private"`
	SourceID     string         `json:"source_id"`
	Attachments  []cwAttachment `json:"attachments"`
	Conversation struct {
		Meta struct {
			Sender struct {
				PhoneNumber      string         `json:"phone_number"`
				Identifier       string         `json:"identifier"`
				CustomAttributes map[string]any `json:"custom_attributes"`
			} `json:"sender"`
		} `json:"meta"`
	} `json:"conversation"`
}

// shouldRelay decide si el webhook corresponde a un mensaje nuevo del agente que
// hay que reenviar a WhatsApp. Solo message_created de tipo outgoing, no privado
// (las notas privadas no salen) y sin source_id (los que ya vienen de WhatsApp
// traen source_id → evita el loop).
func shouldRelay(p chatwootWebhookPayload) bool {
	return p.Event == "message_created" && p.MessageType == "outgoing" && !p.Private && p.SourceID == ""
}

// webhookChatID resuelve el destino en WhatsApp desde el webhook, en orden:
// custom attribute wacalls_chat_id (autoritativo) → identifier (JID) → teléfono.
func webhookChatID(p chatwootWebhookPayload) string {
	if v, ok := p.Conversation.Meta.Sender.CustomAttributes[cwChatIDAttr].(string); ok && v != "" {
		return v
	}
	if id := p.Conversation.Meta.Sender.Identifier; id != "" {
		return id
	}
	return strings.TrimPrefix(p.Conversation.Meta.Sender.PhoneNumber, "+")
}

// resolveRecipient convierte un chatID (JID "…@…" o teléfono suelto) en un JID.
func resolveRecipient(chatID string) (types.JID, error) {
	if strings.Contains(chatID, "@") {
		return types.ParseJID(chatID)
	}
	d := onlyDigits(chatID)
	if d == "" {
		return types.JID{}, fmt.Errorf("destino vacío")
	}
	return types.NewJID(d, types.DefaultUserServer), nil
}

// deliverToWhatsApp envía a WhatsApp un mensaje saliente creado por un agente en
// Chatwoot (texto y/o adjuntos). Devuelve nil silencioso para los eventos que no
// corresponde reenviar (entrantes, notas privadas, ecos, vacíos).
func (s *Session) deliverToWhatsApp(ctx context.Context, p chatwootWebhookPayload) error {
	if !shouldRelay(p) {
		return nil
	}
	hasText := strings.TrimSpace(p.Content) != ""
	if len(p.Attachments) == 0 && !hasText {
		return nil
	}
	jid, err := resolveRecipient(webhookChatID(p))
	if err != nil {
		return fmt.Errorf("webhook sin destino resoluble: %w", err)
	}

	if len(p.Attachments) > 0 {
		for i, att := range p.Attachments {
			caption := ""
			if i == 0 && hasText {
				caption = p.Content // el texto acompaña al primer adjunto
			}
			if err := s.sendChatwootAttachment(ctx, jid, att, caption); err != nil {
				return err
			}
		}
		s.log.Info("chatwoot: saliente (media) → WhatsApp", "to", jid.String(), "n", len(p.Attachments))
		return nil
	}
	if _, err := s.client.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(p.Content)}); err != nil {
		return err
	}
	s.log.Info("chatwoot: saliente (texto) → WhatsApp", "to", jid.String())
	return nil
}

// ---------- helpers ----------

func messageText(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	if t := m.GetConversation(); t != "" {
		return t
	}
	if ext := m.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	return ""
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// jidUser extrae la parte de usuario de un JID/identifier tipo "593...@s.whatsapp.net".
func jidUser(id string) string {
	if i := strings.IndexByte(id, '@'); i >= 0 {
		return id[:i]
	}
	return id
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
