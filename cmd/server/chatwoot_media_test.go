package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestExtractIncomingMedia(t *testing.T) {
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Mimetype: proto.String("image/jpeg"), Caption: proto.String("hola"),
	}}
	if m, ok := extractIncomingMedia(img); !ok || m.kind != "image" || m.caption != "hola" {
		t.Fatalf("image: %+v ok=%v", m, ok)
	}

	doc := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Mimetype: proto.String("application/pdf"), FileName: proto.String("factura.pdf"),
	}}
	if m, ok := extractIncomingMedia(doc); !ok || m.kind != "document" || m.filename != "factura.pdf" {
		t.Fatalf("document: %+v ok=%v", m, ok)
	}

	audio := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg")}}
	if m, ok := extractIncomingMedia(audio); !ok || m.kind != "audio" {
		t.Fatalf("audio: %+v ok=%v", m, ok)
	}

	// solo texto -> sin media
	txt := &waE2E.Message{Conversation: proto.String("hola")}
	if _, ok := extractIncomingMedia(txt); ok {
		t.Fatal("texto no debería reportar media")
	}
}

func TestMediaFilename(t *testing.T) {
	if got := mediaFilename("document", "application/pdf", "factura"); !strings.HasSuffix(got, ".pdf") {
		t.Fatalf("pdf: %q", got)
	}
	if got := mediaFilename("image", "", "foto"); got != "foto.jpg" {
		t.Fatalf("fallback image: %q", got)
	}
	if got := mediaFilename("audio", "", ""); got != "audio.ogg" {
		t.Fatalf("fallback audio: %q", got)
	}
	// nota de voz de WhatsApp: opus con parámetro codecs → audio.ogg (no .oga)
	if got := mediaFilename("audio", "audio/ogg; codecs=opus", "audio"); got != "audio.ogg" {
		t.Fatalf("voz opus: %q; quería audio.ogg", got)
	}
	if got := cleanMimetype("audio/ogg; codecs=opus"); got != "audio/ogg" {
		t.Fatalf("cleanMimetype: %q", got)
	}
}

func TestChatwootFileTypeToMedia(t *testing.T) {
	cases := map[string]whatsmeow.MediaType{
		"image": whatsmeow.MediaImage,
		"video": whatsmeow.MediaVideo,
		"audio": whatsmeow.MediaAudio,
		"file":  whatsmeow.MediaDocument,
		"otro":  whatsmeow.MediaDocument,
	}
	for ft, want := range cases {
		if got := chatwootFileTypeToMedia(ft, "x"); got != want {
			t.Fatalf("%s -> %v; quería %v", ft, got, want)
		}
	}
}

func TestBuildOutgoingMedia(t *testing.T) {
	up := whatsmeow.UploadResponse{URL: "https://wa/u", DirectPath: "/d", MediaKey: []byte("k")}

	img := buildOutgoingMedia("image", "image/png", "x.png", "pie", 123, up)
	if img.GetImageMessage() == nil || img.GetImageMessage().GetCaption() != "pie" ||
		img.GetImageMessage().GetMimetype() != "image/png" || img.GetImageMessage().GetFileLength() != 123 {
		t.Fatalf("image message mal armado: %+v", img.GetImageMessage())
	}

	doc := buildOutgoingMedia("file", "application/pdf", "f.pdf", "", 9, up)
	if doc.GetDocumentMessage() == nil || doc.GetDocumentMessage().GetFileName() != "f.pdf" {
		t.Fatalf("document message mal armado: %+v", doc.GetDocumentMessage())
	}

	aud := buildOutgoingMedia("audio", "audio/ogg", "a.ogg", "ignorado", 5, up)
	if aud.GetAudioMessage() == nil || aud.GetAudioMessage().GetMimetype() != "audio/ogg" {
		t.Fatalf("audio message mal armado: %+v", aud.GetAudioMessage())
	}
}

func TestPostAttachmentMultipart(t *testing.T) {
	var gotType, gotContent, gotFilename string
	var gotBytes []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		gotType = r.FormValue("message_type")
		gotContent = r.FormValue("content")
		if fhs := r.MultipartForm.File["attachments[]"]; len(fhs) == 1 {
			gotFilename = fhs[0].Filename
			f, _ := fhs[0].Open()
			gotBytes, _ = io.ReadAll(f)
			f.Close()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := ChatwootConfig{URL: srv.URL, AccountID: 2, AccountToken: "t", InboxID: 1}
	err := cfg.postAttachment(context.Background(), 5, "pie de foto", "foto.jpg", "image/jpeg", []byte("BYTES"), "incoming")
	if err != nil {
		t.Fatalf("postAttachment: %v", err)
	}
	if gotType != "incoming" || gotContent != "pie de foto" || gotFilename != "foto.jpg" || string(gotBytes) != "BYTES" {
		t.Fatalf("multipart: type=%q content=%q file=%q bytes=%q", gotType, gotContent, gotFilename, string(gotBytes))
	}
}

func TestDownloadURL(t *testing.T) {
	// httptest escucha en 127.0.0.1; el loopback está bloqueado por defecto
	// (política SSRF), así que el test lo habilita explícitamente.
	t.Setenv("WACALLS_SSRF_ALLOW_LOOPBACK", "true")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf; charset=binary")
		w.Write([]byte("PDFDATA"))
	}))
	defer srv.Close()

	data, ct, name, err := downloadURL(context.Background(), srv.URL+"/rails/blob/factura.pdf?disposition=inline")
	if err != nil {
		t.Fatalf("downloadURL: %v", err)
	}
	if string(data) != "PDFDATA" || ct != "application/pdf" || name != "factura.pdf" {
		t.Fatalf("data=%q ct=%q name=%q", string(data), ct, name)
	}
}

func TestDownloadURLRejectsUnsafe(t *testing.T) {
	ctx := context.Background()

	// Esquema no-http(s) → rechazado.
	for _, u := range []string{"file:///etc/passwd", "ftp://host/x", "gopher://host/1"} {
		if _, _, _, err := downloadURL(ctx, u); err == nil {
			t.Fatalf("esquema inseguro %q aceptado", u)
		}
	}

	// Loopback sin habilitar → rechazado (esto NO setea el env, default false).
	lb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("x")) }))
	defer lb.Close()
	if _, _, _, err := downloadURL(ctx, lb.URL); err == nil {
		t.Fatal("loopback debería estar bloqueado por defecto")
	}

	// URL malformada → rechazada.
	if _, _, _, err := downloadURL(ctx, "://sin esuama"); err == nil {
		t.Fatal("URL malformada aceptada")
	}
}

func TestSSRFDisallowed(t *testing.T) {
	// Casos con la política por defecto (loopback off, privadas on).
	for _, loc := range []string{"127.0.0.1", "::1"} {
		ip := net.ParseIP(loc)
		if !ssrfDisallowed(ip) {
			t.Fatalf("%s debería estar bloqueado", loc)
		}
	}
	for _, loc := range []string{"169.254.169.254", "169.254.10.1", "fe80::1", "224.0.0.1", "0.0.0.0"} {
		ip := net.ParseIP(loc)
		if !ssrfDisallowed(ip) {
			t.Fatalf("%s debería estar bloqueado", loc)
		}
	}
	// Privadas Docker (red del stack) permitidas por defecto.
	for _, loc := range []string{"172.17.0.5", "10.0.0.4", "192.168.1.20"} {
		ip := net.ParseIP(loc)
		if ssrfDisallowed(ip) {
			t.Fatalf("%s debería estar permitido con privadas on", loc)
		}
	}

	// Con WACALLS_SSRF_ALLOW_PRIVATE=false, las privadas se bloquean.
	t.Setenv("WACALLS_SSRF_ALLOW_PRIVATE", "false")
	for _, loc := range []string{"172.17.0.5", "10.0.0.4", "192.168.1.20"} {
		ip := net.ParseIP(loc)
		if !ssrfDisallowed(ip) {
			t.Fatalf("%s debería estar bloqueado con privadas off", loc)
		}
	}
}
