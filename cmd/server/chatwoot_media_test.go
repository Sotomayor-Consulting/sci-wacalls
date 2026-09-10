package main

import (
	"context"
	"io"
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
