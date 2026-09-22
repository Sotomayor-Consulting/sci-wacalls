package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestGetConversation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/conversations/14") {
			t.Errorf("path inesperado: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"inbox_id": 1,
			"meta":     map[string]any{"sender": map[string]any{"phone_number": "+593998175516", "name": "Mary"}},
		})
	}))
	defer srv.Close()

	cfg := ChatwootConfig{URL: srv.URL, AccountID: 2, AccountToken: "t", InboxID: 1}
	inbox, phone, name, err := cfg.getConversation(context.Background(), 14)
	if err != nil || inbox != 1 || phone != "+593998175516" || name != "Mary" {
		t.Fatalf("inbox=%d phone=%q name=%q err=%v", inbox, phone, name, err)
	}
}

func TestConfigsByAccount(t *testing.T) {
	store, ctx := newTestStore(t)
	_ = store.setChatwoot(ctx, "s1", ChatwootConfig{URL: "u", AccountID: 2, AccountToken: "t", InboxID: 1})
	_ = store.setChatwoot(ctx, "s2", ChatwootConfig{URL: "u", AccountID: 9, AccountToken: "t", InboxID: 3})

	got, err := store.configsByAccount(ctx, 2)
	if err != nil {
		t.Fatalf("configsByAccount: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "s1" || got[0].Cfg.InboxID != 1 {
		t.Fatalf("got=%+v", got)
	}
	if none, _ := store.configsByAccount(ctx, 100); len(none) != 0 {
		t.Fatalf("esperaba 0 para cuenta inexistente, got %d", len(none))
	}
}

func newTestServer(t *testing.T) *server {
	t.Helper()
	srv, err := newServer(context.Background(), filepath.Join(t.TempDir(), "srv.db"), "", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return srv
}

func TestResolveHandler(t *testing.T) {
	// Chatwoot simulado: la conversación 14 pertenece al inbox 1 con teléfono.
	cw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"inbox_id": 1,
			"meta":     map[string]any{"sender": map[string]any{"phone_number": "+593998175516", "name": "Mary"}},
		})
	}))
	defer cw.Close()

	srv := newTestServer(t)
	_ = srv.sessions.store.setChatwoot(context.Background(), "sess-A",
		ChatwootConfig{URL: cw.URL, AccountID: 2, AccountToken: "t", InboxID: 1})
	// La sesión tiene que estar VIVA: resolve descarta las configs huérfanas,
	// porque devolver su id le daría al widget una sesión inexistente y el
	// POST a /calls fallaría con "no such session".
	srv.sessions.register(&Session{id: "sess-A", name: "test"})

	do := func(q string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		srv.handleChatwootResolve(rec, httptest.NewRequest(http.MethodGet, "/api/chatwoot/resolve?"+q, nil))
		var body map[string]any
		json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	code, body := do("account_id=2&conversation_id=14")
	if code != 200 || body["session_id"] != "sess-A" || body["phone"] != "593998175516" {
		t.Fatalf("ok: code=%d body=%v", code, body)
	}

	if code, _ := do("account_id=999&conversation_id=14"); code != 404 {
		t.Fatalf("cuenta sin config debería dar 404, got %d", code)
	}
	if code, _ := do("conversation_id=14"); code != 400 {
		t.Fatalf("falta account_id -> 400, got %d", code)
	}
}

func TestResolveInboxMismatch(t *testing.T) {
	// La conversación pertenece al inbox 5, pero la config es del inbox 1.
	cw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"inbox_id": 5,
			"meta":     map[string]any{"sender": map[string]any{"phone_number": "+593", "name": "X"}},
		})
	}))
	defer cw.Close()

	srv := newTestServer(t)
	_ = srv.sessions.store.setChatwoot(context.Background(), "sess-A",
		ChatwootConfig{URL: cw.URL, AccountID: 2, AccountToken: "t", InboxID: 1})
	// La sesión tiene que estar VIVA: resolve descarta las configs huérfanas,
	// porque devolver su id le daría al widget una sesión inexistente y el
	// POST a /calls fallaría con "no such session".
	srv.sessions.register(&Session{id: "sess-A", name: "test"})

	rec := httptest.NewRecorder()
	srv.handleChatwootResolve(rec, httptest.NewRequest(http.MethodGet, "/api/chatwoot/resolve?account_id=2&conversation_id=14", nil))
	if rec.Code != 404 {
		t.Fatalf("inbox distinto debería dar 404, got %d", rec.Code)
	}
}

func TestWidgetServed(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleWidgetJS(rec, httptest.NewRequest(http.MethodGet, "/widget.js", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("content-type=%q", ct)
	}
	if !strings.Contains(rec.Body.String(), "chatwoot/resolve") {
		t.Fatal("widget.js no contiene la llamada a resolve")
	}
}

// TestRealPhonePN: un JID que ya es teléfono se devuelve tal cual, sin tocar el
// store (que en este test no existe). La rama LID->PN requiere cliente vivo.
func TestRealPhonePN(t *testing.T) {
	s := &Session{}
	if got := s.realPhone(types.NewJID("5219991112233", types.DefaultUserServer)); got != "5219991112233" {
		t.Fatalf("PN: %q", got)
	}
	if got := s.realPhone(types.JID{}); got != "" {
		t.Fatalf("JID vacío: %q", got)
	}
	// LID sin cliente: cae al fallback (User crudo) en vez de entrar en pánico.
	if got := s.realPhone(types.NewJID("65266390200563", types.HiddenUserServer)); got != "65266390200563" {
		t.Fatalf("LID fallback: %q", got)
	}
}

// La UI nativa no sabe enviar la API key: index.html debe salir con el
// bootstrap inyectado, y los assets reales sin tocar.
func TestStaticIndexInjectsAuthBootstrap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<html><head><title>WaCalls</title></head><body></body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &server{staticDir: dir}
	h := srv.staticHandler()

	for _, path := range []string{"/", "/index.html", "/sessions"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if !strings.Contains(rec.Body.String(), "wacallsApiKey") {
			t.Fatalf("%s: index sin bootstrap de auth", path)
		}
		if !strings.Contains(rec.Body.String(), "<title>WaCalls</title>") {
			t.Fatalf("%s: se perdió el html original", path)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if rec.Body.String() != "ORIGINAL" {
		t.Fatalf("asset modificado: %q", rec.Body.String())
	}
}

// Una config cuya sesión ya no existe no debe entregarse al widget: si se
// entrega, el POST a /calls responde "no such session" y el agente no tiene
// forma de saber por qué. Pasa al re-parear un número.
func TestResolveIgnoraConfigHuerfana(t *testing.T) {
	cw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"inbox_id": 1,
			"meta":     map[string]any{"sender": map[string]any{"phone_number": "+593998175516", "name": "Mary"}},
		})
	}))
	defer cw.Close()

	srv := newTestServer(t)
	cfg := ChatwootConfig{URL: cw.URL, AccountID: 2, AccountToken: "t", InboxID: 1}
	// "muerta" quedó en la base pero no está registrada; "viva" sí.
	_ = srv.sessions.store.setChatwoot(context.Background(), "muerta", cfg)

	rec := httptest.NewRecorder()
	srv.handleChatwootResolve(rec, httptest.NewRequest(http.MethodGet, "/api/chatwoot/resolve?account_id=2&conversation_id=14", nil))
	if rec.Code != 404 {
		t.Fatalf("con la sesión muerta debería dar 404, got %d body=%s", rec.Code, rec.Body)
	}

	_ = srv.sessions.store.setChatwoot(context.Background(), "viva", cfg)
	srv.sessions.register(&Session{id: "viva", name: "test"})

	rec = httptest.NewRecorder()
	srv.handleChatwootResolve(rec, httptest.NewRequest(http.MethodGet, "/api/chatwoot/resolve?account_id=2&conversation_id=14", nil))
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body["session_id"] != "viva" {
		t.Fatalf("debería elegir la sesión viva: code=%d body=%v", rec.Code, body)
	}
}
