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
	"net/url"
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
// (texto o adjunto), en ambas direcciones:
//   - entrante (del cliente): se postea como "incoming".
//   - propio del aparato (respondido desde WhatsApp Web/celular, fuera de
//     Chatwoot): se espeja como NOTA PRIVADA para que el agente vea en Chatwoot
//     lo que se respondió por fuera. Los mensajes que enviamos por Chatwoot no se
//     re-espejan (se filtran por isSelfSent).
//
// Solo conversaciones 1:1 (los grupos quedan fuera: el Inbox B es una línea 1:1).
// Acepta teléfono (s.whatsapp.net) y LID (@lid) — WhatsApp está migrando a LID.
const deviceMirrorPrefix = "📲 Enviado desde WhatsApp:\n"

func (s *Session) handleIncomingMessage(evt *events.Message) {
	isGroup := evt.Info.Chat.Server == types.GroupServer
	if !isGroup {
		switch evt.Info.Chat.Server {
		case types.DefaultUserServer, types.HiddenUserServer:
			// 1:1 por teléfono o por LID — OK
		default:
			return // newsletter, broadcast, etc.
		}
	}
	own := evt.Info.IsFromMe
	if own && s.isSelfSent(evt.Info.ID) {
		return // eco de un mensaje que ya enviamos por Chatwoot — no duplicar
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

	t, ok := s.chatTarget(evt, isGroup, own)
	if !ok {
		return
	}

	convID, err := s.mgr.store.lookupConversation(s.mgr.appCtx, s.id, t.chatID)
	if err != nil {
		s.log.Error("chatwoot: lookup conversation failed", "err", err)
		return
	}
	if convID == 0 {
		convID, err = s.ensureChatwootConversation(cfg, t.chatID, t.phone, t.name, t.avatar)
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
		caption := t.prefix + media.caption
		if own {
			err = cfg.postPrivateNote(s.mgr.appCtx, convID, caption, media.filename, media.mimetype, data)
		} else {
			err = cfg.postAttachment(s.mgr.appCtx, convID, caption, media.filename, media.mimetype, data, "incoming")
		}
		if err != nil {
			s.log.Error("chatwoot: post attachment failed", "err", err, "own", own)
			return
		}
		s.log.Info("chatwoot: media → Chatwoot", "chat", t.chatID, "conv", convID, "kind", media.kind, "own", own, "grupo", isGroup)
		return
	}

	if own {
		err = cfg.postTextNote(s.mgr.appCtx, convID, t.prefix+text)
	} else {
		err = cfg.postMessage(s.mgr.appCtx, convID, t.prefix+text, "incoming")
	}
	if err != nil {
		s.log.Error("chatwoot: post message failed", "err", err, "own", own)
		return
	}
	s.log.Info("chatwoot: texto → Chatwoot", "chat", t.chatID, "conv", convID, "own", own, "grupo", isGroup)
}

// chatTarget describe a quién representa la conversación de Chatwoot.
type chatTarget struct {
	chatID string // JID normalizado: teléfono@s.whatsapp.net, o el grupo @g.us
	phone  string // vacío en grupos: un grupo no tiene teléfono
	name   string
	avatar string
	prefix string // se antepone al mensaje; identifica al autor dentro del grupo
}

// chatTarget resuelve la identidad de la conversación. En 1:1 el contacto es la
// persona; en GRUPO el contacto es el grupo y el autor va como prefijo del
// mensaje, porque una conversación de Chatwoot tiene un solo contacto y en el
// grupo escriben varios.
func (s *Session) chatTarget(evt *events.Message, isGroup, own bool) (chatTarget, bool) {
	if isGroup {
		t := chatTarget{chatID: evt.Info.Chat.String()}
		t.name, t.avatar = s.groupIdentity(evt.Info.Chat)
		author := evt.Info.PushName
		if own {
			author = firstNonEmpty(s.client.Store.PushName, "yo")
			t.prefix = deviceMirrorPrefix + "*" + author + "*:\n"
		} else {
			if author == "" {
				author = s.realPhone(evt.Info.Sender)
			}
			t.prefix = "*" + author + "*:\n"
		}
		return t, true
	}

	phone := s.peerPhone(evt.Info.MessageSource)
	if phone == "" {
		s.log.Warn("chatwoot: no se pudo resolver el teléfono del par", "chat", evt.Info.Chat.String())
		return chatTarget{}, false
	}
	// Normalizamos el chatID al JID de teléfono para que entrante y saliente
	// mapeen a la MISMA conversación de Chatwoot.
	t := chatTarget{
		chatID: phone + "@" + string(types.DefaultUserServer),
		phone:  phone,
		name:   phone,
	}
	if !own {
		if pn := evt.Info.PushName; pn != "" {
			t.name = pn // el PushName de un fromMe es el nuestro, no el del contacto
		}
	} else {
		t.prefix = deviceMirrorPrefix
	}
	return t, true
}

// groupIdentity devuelve el asunto y la foto del grupo, para que en Chatwoot se
// reconozca por su nombre y no por el JID. Solo consulta WhatsApp.
func (s *Session) groupIdentity(group types.JID) (name, avatar string) {
	name = group.String()
	if gi, err := s.client.GetGroupInfo(s.mgr.appCtx, group); err == nil && gi.Name != "" {
		name = gi.Name
	}
	if pp, err := s.client.GetProfilePictureInfo(s.mgr.appCtx, group, nil); err == nil && pp != nil {
		avatar = pp.URL
	}
	return name, avatar
}

// peerPhone devuelve el teléfono real (PN) del OTRO participante del 1:1,
// resolviendo el LID. En un fromMe el "par" es el destinatario (RecipientAlt);
// en un entrante es el remitente (SenderAlt). Chat es el par en ambos casos.
func (s *Session) peerPhone(src types.MessageSource) string {
	if src.Chat.Server == types.DefaultUserServer {
		return src.Chat.User
	}
	alt := src.SenderAlt
	if src.IsFromMe {
		alt = src.RecipientAlt
	}
	if alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.User
	}
	return s.realPhone(src.Chat)
}

// realPhone devuelve el teléfono (PN) de un JID. Los JID de llamadas y de chats
// migrados llegan como LID (@lid), que no es un número marcable ni el
// identificador con el que el contacto existe en Chatwoot; en ese caso lo
// traducimos con el mapa LID->PN del store de whatsmeow.
func (s *Session) realPhone(jid types.JID) string {
	if jid.User == "" {
		return ""
	}
	if jid.Server == types.DefaultUserServer {
		return jid.User
	}
	if s.client != nil && s.client.Store != nil {
		if pn, err := s.client.Store.LIDs.GetPNForLID(s.mgr.appCtx, jid); err == nil && pn.User != "" {
			return pn.User
		}
	}
	return jid.User
}

// ensureChatwootConversation crea (una sola vez) el contacto, el contact_inbox y
// la conversación en Chatwoot para un chat de WhatsApp, y guarda el mapeo local
// para reusarlo en los mensajes siguientes.
func (s *Session) ensureChatwootConversation(cfg ChatwootConfig, chatID, phone, name, avatar string) (int, error) {
	contactID, sourceID, err := cfg.ensureContact(s.mgr.appCtx, chatID, phone, name, avatar)
	if err != nil {
		return 0, err
	}
	if sourceID == "" {
		sourceID, err = cfg.ensureSourceID(s.mgr.appCtx, contactID)
		if err != nil {
			return 0, err
		}
	}
	// Reutilizar una conversación abierta del contacto antes de crear otra: sin
	// esto, si el agente escribió primero (ese camino no guarda mapeo local) o
	// si se perdió el mapeo, el mensaje entrante abriría una conversación
	// DUPLICADA y el hilo del cliente quedaría partido en dos.
	convID := cfg.findOpenConversation(s.mgr.appCtx, contactID)
	if convID == 0 {
		var err error
		convID, err = cfg.createConversation(s.mgr.appCtx, sourceID)
		if err != nil {
			return 0, err
		}
	}
	if err := s.mgr.store.saveConversation(s.mgr.appCtx, s.id, chatID, contactID, sourceID, convID); err != nil {
		return 0, err
	}
	return convID, nil
}

// ensureContact busca el contacto por teléfono; si no existe lo crea. Devuelve
// el id del contacto y, si la respuesta de creación ya lo trae, el source_id del
// contact_inbox.
func (c ChatwootConfig) ensureContact(ctx context.Context, chatID, phone, name, avatar string) (contactID int, sourceID string, err error) {
	if id := c.searchContact(ctx, phone, chatID); id != 0 {
		return id, "", nil
	}
	payload := map[string]any{
		"inbox_id":          c.InboxID,
		"name":              name,
		"identifier":        chatID,
		"custom_attributes": map[string]any{cwChatIDAttr: chatID},
	}
	// Un GRUPO no tiene teléfono: mandar phone_number vacío (o un "+" solo) hace
	// que Chatwoot rechace el contacto por formato inválido.
	if phone != "" {
		payload["phone_number"] = "+" + phone
	}
	if avatar != "" {
		payload["avatar_url"] = avatar
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
// searchContact busca el contacto por teléfono y, si no hay (grupos), por el
// identifier —que es el chatID—. La búsqueda de Chatwoot cubre ambos campos.
func (c ChatwootConfig) searchContact(ctx context.Context, phone, chatID string) int {
	q := phone
	byIdentifier := phone == ""
	if byIdentifier {
		q = chatID
	}
	if q == "" {
		return 0
	}
	data, code, err := c.req(ctx, http.MethodGet, "/contacts/search?q="+url.QueryEscape(q), nil)
	if err != nil || code < 200 || code >= 300 {
		return 0
	}
	var out struct {
		Payload []struct {
			ID         int    `json:"id"`
			Phone      string `json:"phone_number"`
			Identifier string `json:"identifier"`
		} `json:"payload"`
	}
	if json.Unmarshal(data, &out) != nil {
		return 0
	}
	for _, ct := range out.Payload {
		if byIdentifier {
			if ct.Identifier == chatID {
				return ct.ID
			}
			continue
		}
		if onlyDigits(ct.Phone) == onlyDigits(phone) {
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
// findOpenConversation devuelve la conversación viva del contacto en este inbox,
// o 0 si no hay. Las cerradas (resolved) no se reabren: un mensaje nuevo merece
// una conversación nueva.
func (c ChatwootConfig) findOpenConversation(ctx context.Context, contactID int) int {
	data, code, err := c.req(ctx, http.MethodGet, fmt.Sprintf("/contacts/%d/conversations", contactID), nil)
	if err != nil || code < 200 || code >= 300 {
		return 0
	}
	var out struct {
		Payload []struct {
			ID      int    `json:"id"`
			InboxID int    `json:"inbox_id"`
			Status  string `json:"status"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0
	}
	for _, conv := range out.Payload {
		if conv.InboxID != c.InboxID {
			continue
		}
		switch conv.Status {
		case "open", "pending", "snoozed":
			return conv.ID
		}
	}
	return 0
}

func (c ChatwootConfig) createConversation(ctx context.Context, sourceID string) (int, error) {
	payload := map[string]any{"source_id": sourceID, "inbox_id": c.InboxID, "status": "open"}
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

// postTextNote crea una NOTA PRIVADA de texto (no se reenvía al cliente; tampoco
// dispara el webhook de salida porque shouldRelay ignora las privadas). Se usa
// para espejar en Chatwoot los mensajes respondidos desde el aparato.
func (c ChatwootConfig) postTextNote(ctx context.Context, convID int, content string) error {
	payload := map[string]any{"content": content, "message_type": "outgoing", "private": true}
	path := fmt.Sprintf("/conversations/%d/messages", convID)
	data, code, err := c.req(ctx, http.MethodPost, path, payload)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("post note: status %d: %s", code, string(data))
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
	// Chatwoot compone en Markdown y WhatsApp no lo entiende: sin traducir, un
	// **negrita** llega con los asteriscos literales.
	content := chatwootToWhatsApp(p.Content)
	hasText := strings.TrimSpace(content) != ""
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
				caption = content // el texto acompaña al primer adjunto
			}
			if err := s.sendChatwootAttachment(ctx, jid, att, caption); err != nil {
				return err
			}
		}
		s.log.Info("chatwoot: saliente (media) → WhatsApp", "to", jid.String(), "n", len(p.Attachments))
		return nil
	}
	resp, err := s.client.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(content)})
	if err != nil {
		return err
	}
	s.markSelfSent(resp.ID) // para no re-espejar este mensaje cuando vuelva como fromMe
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

// resolvePeerIdentity traduce el JID crudo de un peer de llamada (que casi
// siempre llega como LID) al teléfono real y, cuando lo conocemos, al nombre del
// contacto. Lo usa el evento de llamada entrante: el LID no es mostrable ni
// marcable, así que sin esto el agente ve un número interno sin sentido.
func (s *Session) resolvePeerIdentity(jidStr string) (phone, name string) {
	jid, err := resolveRecipient(jidStr)
	if err != nil {
		return "", ""
	}
	phone = onlyDigits(s.realPhone(jid))
	if s.client == nil || s.client.Store == nil {
		return phone, ""
	}
	// Los contactos suelen estar indexados por teléfono (PN), no por LID, así
	// que probamos ambas claves.
	lookup := []types.JID{jid}
	if phone != "" {
		lookup = append(lookup, types.NewJID(phone, types.DefaultUserServer))
	}
	for _, j := range lookup {
		ci, err := s.client.Store.Contacts.GetContact(s.mgr.appCtx, j)
		if err != nil || !ci.Found {
			continue
		}
		if n := firstNonEmpty(ci.FullName, ci.PushName, ci.BusinessName); n != "" {
			return phone, n
		}
	}
	return phone, ""
}
