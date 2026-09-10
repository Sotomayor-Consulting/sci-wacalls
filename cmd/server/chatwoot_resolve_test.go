package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
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
