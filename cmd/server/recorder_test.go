package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEncodeWAVHeader(t *testing.T) {
	samples := make([]int16, 1600) // 0.1 s @ 16 kHz
	wav := encodeWAV(samples, recSampleRate)

	if len(wav) != 44+len(samples)*2 {
		t.Fatalf("tamaño WAV = %d; quería %d", len(wav), 44+len(samples)*2)
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" ||
		string(wav[12:16]) != "fmt " || string(wav[36:40]) != "data" {
		t.Fatalf("cabecera WAV inválida: %q", string(wav[0:16]))
	}
	// sample rate en offset 24 (LE)
	var sr uint32
	binary.Read(bytes.NewReader(wav[24:28]), binary.LittleEndian, &sr)
	if sr != recSampleRate {
		t.Fatalf("sample rate = %d; quería %d", sr, recSampleRate)
	}
	// numChannels (offset 22) == 1, bitsPerSample (offset 34) == 16
	if wav[22] != 1 || wav[34] != 16 {
		t.Fatalf("channels=%d bits=%d", wav[22], wav[34])
	}
}

func TestRecorderTooShort(t *testing.T) {
	r := newCallRecorder()
	r.writePeer(make([]float32, recSampleRate)) // 1 s < 3 s mínimo
	if _, _, ok := r.finishWAV(); ok {
		t.Fatal("una llamada de 1 s no debería generar grabación")
	}
}

func TestRecorderMixAndDuration(t *testing.T) {
	r := newCallRecorder()
	n := recSampleRate * 3 // 3 s exactos
	peer := make([]float32, n)
	browser := make([]float32, n)
	for i := range peer {
		peer[i] = 0.5
		browser[i] = 0.5 // suma = 1.0 (clamp)
	}
	r.writePeer(peer)
	r.writeBrowser(browser)

	wav, seconds, ok := r.finishWAV()
	if !ok || seconds != 3 {
		t.Fatalf("ok=%v seconds=%d; quería true, 3", ok, seconds)
	}
	// primera muestra mezclada ~ 32767 (1.0 * 32767)
	var first int16
	binary.Read(bytes.NewReader(wav[44:46]), binary.LittleEndian, &first)
	if first < 32000 {
		t.Fatalf("mezcla: primera muestra = %d; esperaba ~32767", first)
	}
}

func TestRecorderNilNoop(t *testing.T) {
	var r *callRecorder // grabación desactivada
	// No debe hacer panic.
	r.writePeer([]float32{0.1, 0.2})
	r.writeBrowser([]float32{0.3})
}

func TestRecordingStore(t *testing.T) {
	store, ctx := newTestStore(t)
	if store.getRecording(ctx, "s1") {
		t.Fatal("por defecto debería estar desactivada")
	}
	if err := store.setRecording(ctx, "s1", true); err != nil {
		t.Fatalf("setRecording: %v", err)
	}
	if !store.getRecording(ctx, "s1") {
		t.Fatal("debería estar activada tras set true")
	}
	_ = store.setRecording(ctx, "s1", false)
	if store.getRecording(ctx, "s1") {
		t.Fatal("debería estar desactivada tras set false")
	}
}

func TestPostPrivateNoteSetsPrivate(t *testing.T) {
	var gotPrivate, gotType, gotFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		gotPrivate = r.FormValue("private")
		gotType = r.FormValue("message_type")
		if fhs := r.MultipartForm.File["attachments[]"]; len(fhs) == 1 {
			gotFile = fhs[0].Filename
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := ChatwootConfig{URL: srv.URL, AccountID: 2, AccountToken: "t", InboxID: 1}
	if err := cfg.postPrivateNote(context.Background(), 5, "grabación", "x.wav", "audio/wav", []byte("WAV")); err != nil {
		t.Fatalf("postPrivateNote: %v", err)
	}
	if gotPrivate != "true" || gotType != "outgoing" || gotFile != "x.wav" {
		t.Fatalf("private=%q message_type=%q file=%q; quería true/outgoing/x.wav", gotPrivate, gotType, gotFile)
	}
}
