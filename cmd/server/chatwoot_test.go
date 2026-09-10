package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// --- store ---

func newTestStore(t *testing.T) (*sessionStore, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := newSessionStore(ctx, db)
	if err != nil {
		t.Fatalf("newSessionStore: %v", err)
	}
	if err := ensureChatwootTables(ctx, db); err != nil {
		t.Fatalf("ensureChatwootTables: %v", err)
	}
	return store, ctx
}

func TestChatwootConfigStore(t *testing.T) {
	store, ctx := newTestStore(t)

	if _, ok := store.getChatwoot(ctx, "s1"); ok {
		t.Fatal("esperaba sin config para sesión nueva")
	}

	cfg := ChatwootConfig{URL: "http://rails:3000", AccountID: 2, AccountToken: "tok", InboxID: 1, InboxIdentifier: "abc"}
	if err := store.setChatwoot(ctx, "s1", cfg); err != nil {
		t.Fatalf("setChatwoot: %v", err)
	}
	got, ok := store.getChatwoot(ctx, "s1")
	if !ok || got != cfg {
		t.Fatalf("getChatwoot = %+v, %v; quería %+v", got, ok, cfg)
	}

	// upsert: sobreescribe
	cfg.AccountToken = "tok2"
	if err := store.setChatwoot(ctx, "s1", cfg); err != nil {
		t.Fatalf("setChatwoot upsert: %v", err)
	}
	if got, _ := store.getChatwoot(ctx, "s1"); got.AccountToken != "tok2" {
		t.Fatalf("upsert no aplicó: %q", got.AccountToken)
	}

	if err := store.deleteChatwoot(ctx, "s1"); err != nil {
		t.Fatalf("deleteChatwoot: %v", err)
	}
	if _, ok := store.getChatwoot(ctx, "s1"); ok {
		t.Fatal("esperaba sin config tras delete")
	}
}

func TestChatwootConversationMap(t *testing.T) {
	store, ctx := newTestStore(t)
	chat := "593998175516@s.whatsapp.net"

	if id, err := store.lookupConversation(ctx, "s1", chat); err != nil || id != 0 {
		t.Fatalf("lookup vacío = %d, %v", id, err)
	}
	if err := store.saveConversation(ctx, "s1", chat, 10, "src-1", 42); err != nil {
		t.Fatalf("saveConversation: %v", err)
	}
	if id, err := store.lookupConversation(ctx, "s1", chat); err != nil || id != 42 {
		t.Fatalf("lookup = %d, %v; quería 42", id, err)
	}
}

// --- cliente de Chatwoot contra un servidor simulado ---

func TestEnsureContactSearchHit(t *testing.T) {
	var searched, created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contacts/search"):
			searched = true
			json.NewEncoder(w).Encode(map[string]any{
				"payload": []map[string]any{{"id": 7, "phone_number": "+593998175516"}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/contacts"):
			created = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("llamada inesperada: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := ChatwootConfig{URL: srv.URL, AccountID: 2, AccountToken: "t", InboxID: 1}
	id, src, err := cfg.ensureContact(context.Background(), "593998175516@s.whatsapp.net", "593998175516", "Mary")
	if err != nil {
		t.Fatalf("ensureContact: %v", err)
	}
	if id != 7 || src != "" {
		t.Fatalf("id=%d src=%q; quería 7, \"\"", id, src)
	}
	if !searched || created {
		t.Fatalf("searched=%v created=%v; quería search sí, create no", searched, created)
	}
}

func TestEnsureContactCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/contacts/search"):
			json.NewEncoder(w).Encode(map[string]any{"payload": []any{}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/contacts"):
			json.NewEncoder(w).Encode(map[string]any{
				"payload": map[string]any{
					"contact": map[string]any{
						"id": 15,
						"contact_inboxes": []map[string]any{
							{"source_id": "src-xyz", "inbox": map[string]any{"id": 1}},
						},
					},
				},
			})
		default:
			t.Errorf("llamada inesperada: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := ChatwootConfig{URL: srv.URL, AccountID: 2, AccountToken: "t", InboxID: 1}
	id, src, err := cfg.ensureContact(context.Background(), "593@s.whatsapp.net", "593", "X")
	if err != nil {
		t.Fatalf("ensureContact: %v", err)
	}
	if id != 15 || src != "src-xyz" {
		t.Fatalf("id=%d src=%q; quería 15, src-xyz", id, src)
	}
}

func TestCreateConversationAndPostMessage(t *testing.T) {
	var gotConvBody, gotMsgBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/conversations"):
			json.NewDecoder(r.Body).Decode(&gotConvBody)
			json.NewEncoder(w).Encode(map[string]any{"id": 99})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"):
			json.NewDecoder(r.Body).Decode(&gotMsgBody)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("llamada inesperada: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := ChatwootConfig{URL: srv.URL, AccountID: 2, AccountToken: "t", InboxID: 1}
	ctx := context.Background()
	convID, err := cfg.createConversation(ctx, "src-1")
	if err != nil || convID != 99 {
		t.Fatalf("createConversation = %d, %v", convID, err)
	}
	if gotConvBody["source_id"] != "src-1" {
		t.Fatalf("source_id enviado = %v", gotConvBody["source_id"])
	}
	if err := cfg.postMessage(ctx, 99, "hola", "incoming"); err != nil {
		t.Fatalf("postMessage: %v", err)
	}
	if gotMsgBody["content"] != "hola" || gotMsgBody["message_type"] != "incoming" {
		t.Fatalf("mensaje enviado = %v", gotMsgBody)
	}
}

// --- filtrado del webhook Chatwoot -> WhatsApp ---

func TestOutgoingWhatsAppTarget(t *testing.T) {
	mk := func(mt, content string, private bool, phone, ident string) chatwootWebhookPayload {
		var p chatwootWebhookPayload
		p.MessageType = mt
		p.Content = content
		p.Private = private
		p.Conversation.Meta.Sender.PhoneNumber = phone
		p.Conversation.Meta.Sender.Identifier = ident
		return p
	}
	cases := []struct {
		name      string
		p         chatwootWebhookPayload
		wantPhone string
		wantSend  bool
	}{
		{"outgoing con teléfono", mk("outgoing", "hola", false, "+593998175516", ""), "593998175516", true},
		{"outgoing por identifier", mk("outgoing", "hola", false, "", "593998175516@s.whatsapp.net"), "593998175516", true},
		{"incoming se ignora", mk("incoming", "hola", false, "+593", ""), "", false},
		{"nota privada se ignora", mk("outgoing", "nota", true, "+593", ""), "", false},
		{"sin contenido se ignora", mk("outgoing", "   ", false, "+593", ""), "", false},
		{"sin destino se ignora", mk("outgoing", "hola", false, "", ""), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			phone, text, send := outgoingWhatsAppTarget(tc.p)
			if send != tc.wantSend || phone != tc.wantPhone {
				t.Fatalf("phone=%q text=%q send=%v; quería phone=%q send=%v", phone, text, send, tc.wantPhone, tc.wantSend)
			}
			if send && text != tc.p.Content {
				t.Fatalf("text=%q; quería %q", text, tc.p.Content)
			}
		})
	}
}
