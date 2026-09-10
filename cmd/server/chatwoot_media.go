package main

// Media (adjuntos) del puente Chatwoot, en ambos sentidos:
//   - WhatsApp -> Chatwoot: descarga el media entrante y lo sube a Chatwoot como
//     adjunto multipart en un mensaje "incoming".
//   - Chatwoot -> WhatsApp: descarga el adjunto desde el data_url de Chatwoot, lo
//     sube a WhatsApp y arma el mensaje de media correspondiente.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// incomingMedia describe el media de un mensaje entrante de WhatsApp.
type incomingMedia struct {
	kind     string // image | audio | video | document
	mimetype string
	filename string
	caption  string
}

// extractIncomingMedia detecta media en un mensaje entrante y devuelve sus
// metadatos. ok=false si el mensaje no trae media descargable.
func extractIncomingMedia(m *waE2E.Message) (incomingMedia, bool) {
	switch {
	case m.GetImageMessage() != nil:
		im := m.GetImageMessage()
		return incomingMedia{"image", im.GetMimetype(), mediaFilename("image", im.GetMimetype(), "imagen"), im.GetCaption()}, true
	case m.GetVideoMessage() != nil:
		vm := m.GetVideoMessage()
		return incomingMedia{"video", vm.GetMimetype(), mediaFilename("video", vm.GetMimetype(), "video"), vm.GetCaption()}, true
	case m.GetAudioMessage() != nil:
		am := m.GetAudioMessage()
		return incomingMedia{"audio", am.GetMimetype(), mediaFilename("audio", am.GetMimetype(), "audio"), ""}, true
	case m.GetDocumentMessage() != nil:
		dm := m.GetDocumentMessage()
		fn := firstNonEmpty(dm.GetFileName(), mediaFilename("document", dm.GetMimetype(), "documento"))
		return incomingMedia{"document", dm.GetMimetype(), fn, dm.GetCaption()}, true
	case m.GetStickerMessage() != nil:
		sm := m.GetStickerMessage()
		return incomingMedia{"image", sm.GetMimetype(), mediaFilename("image", sm.GetMimetype(), "sticker"), ""}, true
	}
	return incomingMedia{}, false
}

// mediaFilename arma un nombre de archivo con la extensión inferida del mimetype.
func mediaFilename(kind, mimetype, stem string) string {
	if stem == "" {
		stem = kind
	}
	if exts, err := mime.ExtensionsByType(mimetype); err == nil && len(exts) > 0 {
		return stem + exts[0]
	}
	switch kind {
	case "image":
		return stem + ".jpg"
	case "video":
		return stem + ".mp4"
	case "audio":
		return stem + ".ogg"
	default:
		return stem + ".bin"
	}
}

// postAttachment crea un mensaje en Chatwoot con un adjunto (multipart/form-data).
func (c ChatwootConfig) postAttachment(ctx context.Context, convID int, content, filename, mimetype string, data []byte, dir string) error {
	return c.postMultipartMessage(ctx, convID, content, filename, mimetype, data, dir, false)
}

// postPrivateNote sube un adjunto como NOTA PRIVADA (no se reenvía al cliente).
// Se usa para las grabaciones de llamada.
func (c ChatwootConfig) postPrivateNote(ctx context.Context, convID int, content, filename, mimetype string, data []byte) error {
	return c.postMultipartMessage(ctx, convID, content, filename, mimetype, data, "outgoing", true)
}

// postMultipartMessage crea un mensaje con adjunto en Chatwoot.
func (c ChatwootConfig) postMultipartMessage(ctx context.Context, convID int, content, filename, mimetype string, data []byte, dir string, private bool) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("message_type", dir)
	if private {
		_ = mw.WriteField("private", "true")
	}
	if strings.TrimSpace(content) != "" {
		_ = mw.WriteField("content", content)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="attachments[]"; filename=%q`, filename))
	if mimetype != "" {
		h.Set("Content-Type", mimetype)
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	url := c.base() + fmt.Sprintf("/conversations/%d/messages", convID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("api_access_token", c.AccountToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := cwHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("post attachment: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// ---------- Chatwoot -> WhatsApp ----------

// sendChatwootAttachment descarga el adjunto desde Chatwoot y lo envía a WhatsApp.
func (s *Session) sendChatwootAttachment(ctx context.Context, jid types.JID, att cwAttachment, caption string) error {
	data, ctype, filename, err := downloadURL(ctx, att.DataURL)
	if err != nil {
		return fmt.Errorf("descargar adjunto: %w", err)
	}
	mimetype := firstNonEmpty(ctype, "application/octet-stream")
	mediaType := chatwootFileTypeToMedia(att.FileType, mimetype)

	up, err := s.client.Upload(ctx, data, mediaType)
	if err != nil {
		return fmt.Errorf("subir a whatsapp: %w", err)
	}
	msg := buildOutgoingMedia(att.FileType, mimetype, filename, caption, uint64(len(data)), up)
	if msg == nil {
		return fmt.Errorf("tipo de adjunto no soportado: %s", att.FileType)
	}
	resp, err := s.client.SendMessage(ctx, jid, msg)
	if err != nil {
		return err
	}
	s.markSelfSent(resp.ID) // para no re-espejar este adjunto cuando vuelva como fromMe
	return nil
}

// chatwootFileTypeToMedia mapea el file_type de Chatwoot al MediaType de whatsmeow.
func chatwootFileTypeToMedia(fileType, mimetype string) whatsmeow.MediaType {
	switch fileType {
	case "image":
		return whatsmeow.MediaImage
	case "video":
		return whatsmeow.MediaVideo
	case "audio":
		return whatsmeow.MediaAudio
	default:
		return whatsmeow.MediaDocument
	}
}

// buildOutgoingMedia arma el mensaje de WhatsApp para un adjunto ya subido.
func buildOutgoingMedia(fileType, mimetype, filename, caption string, size uint64, up whatsmeow.UploadResponse) *waE2E.Message {
	switch fileType {
	case "image":
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption: optString(caption), Mimetype: proto.String(mimetype),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &size,
		}}
	case "video":
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			Caption: optString(caption), Mimetype: proto.String(mimetype),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &size,
		}}
	case "audio":
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			Mimetype: proto.String(mimetype),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &size,
		}}
	default: // documento
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			Caption: optString(caption), Mimetype: proto.String(mimetype),
			FileName: proto.String(filename), Title: proto.String(filename),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &size,
		}}
	}
}

// downloadURL descarga un recurso y devuelve datos, content-type y un nombre de
// archivo derivado de la ruta de la URL.
func downloadURL(ctx context.Context, rawURL string) (data []byte, contentType, filename string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	resp, err := cwHTTP.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	name := path.Base(rawURL)
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	if name == "" || name == "." || name == "/" {
		name = "archivo"
	}
	return data, ct, name, nil
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
