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
	id, src, err := cfg.ensureContact(context.Background(), "593998175516@s.whatsapp.net", "593998175516", "Mary", "")
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
	id, src, err := cfg.ensureContact(context.Background(), "593@s.whatsapp.net", "593", "X", "")
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

func TestShouldRelay(t *testing.T) {
	mk := func(event, mt string, private bool, sourceID string) chatwootWebhookPayload {
		var p chatwootWebhookPayload
		p.Event = event
		p.MessageType = mt
		p.Private = private
		p.SourceID = sourceID
		return p
	}
	cases := []struct {
		name string
		p    chatwootWebhookPayload
		want bool
	}{
		{"outgoing nuevo", mk("message_created", "outgoing", false, ""), true},
		{"incoming se ignora", mk("message_created", "incoming", false, ""), false},
		{"nota privada se ignora", mk("message_created", "outgoing", true, ""), false},
		{"con source_id (eco) se ignora", mk("message_created", "outgoing", false, "ABC123"), false},
		{"conversation_updated se ignora", mk("conversation_updated", "outgoing", false, ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRelay(tc.p); got != tc.want {
				t.Fatalf("shouldRelay = %v; quería %v", got, tc.want)
			}
		})
	}
}

func TestWebhookChatID(t *testing.T) {
	mk := func(phone, ident string, custom map[string]any) chatwootWebhookPayload {
		var p chatwootWebhookPayload
		p.Conversation.Meta.Sender.PhoneNumber = phone
		p.Conversation.Meta.Sender.Identifier = ident
		p.Conversation.Meta.Sender.CustomAttributes = custom
		return p
	}
	// prioridad: custom attribute wacalls_chat_id
	if got := webhookChatID(mk("+593", "id@lid", map[string]any{cwChatIDAttr: "593998175516@s.whatsapp.net"})); got != "593998175516@s.whatsapp.net" {
		t.Fatalf("custom attr: %q", got)
	}
	// luego identifier
	if got := webhookChatID(mk("+593", "120@g.us", nil)); got != "120@g.us" {
		t.Fatalf("identifier: %q", got)
	}
	// luego teléfono (sin +)
	if got := webhookChatID(mk("+593998175516", "", nil)); got != "593998175516" {
		t.Fatalf("phone: %q", got)
	}
}

func TestResolveRecipient(t *testing.T) {
	// JID completo se respeta
	if j, err := resolveRecipient("593998175516@s.whatsapp.net"); err != nil || j.User != "593998175516" || j.Server != "s.whatsapp.net" {
		t.Fatalf("jid: %v %v", j, err)
	}
	// LID se respeta
	if j, err := resolveRecipient("221543019864111@lid"); err != nil || j.Server != "lid" {
		t.Fatalf("lid: %v %v", j, err)
	}
	// teléfono suelto → s.whatsapp.net
	if j, err := resolveRecipient("593998175516"); err != nil || j.Server != "s.whatsapp.net" {
		t.Fatalf("phone: %v %v", j, err)
	}
	// vacío → error
	if _, err := resolveRecipient(""); err == nil {
		t.Fatal("vacío debería dar error")
	}
}

// Las notas (espejo del aparato, llamada perdida, grabación) deben ir como
// private=true: si se postearan como mensaje normal, Chatwoot las reenviaría al
// cliente por el webhook del inbox API.
func TestPostTextNoteIsPrivate(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := ChatwootConfig{URL: srv.URL, AccountID: 1, AccountToken: "t", InboxID: 2}
	if err := cfg.postTextNote(context.Background(), 7, "📞 Llamada perdida de 593999"); err != nil {
		t.Fatalf("postTextNote: %v", err)
	}
	if got["private"] != true {
		t.Fatalf("private=%v; la nota se reenviaría al cliente", got["private"])
	}
	if got["message_type"] != "outgoing" {
		t.Fatalf("message_type=%v", got["message_type"])
	}
	if got["content"] != "📞 Llamada perdida de 593999" {
		t.Fatalf("content=%v", got["content"])
	}
}

// Reapuntar la sesión a otro inbox debe invalidar los mapeos chat->conversación:
// si sobreviven, el motor postea en conversaciones del inbox anterior y Chatwoot
// responde 404 para siempre.
func TestSetChatwootClearsStaleConversations(t *testing.T) {
	st, ctx := newTestStore(t)
	sid := "s1"
	old := ChatwootConfig{URL: "http://cw", AccountID: 2, AccountToken: "t", InboxID: 1}
	if err := st.setChatwoot(ctx, sid, old); err != nil {
		t.Fatal(err)
	}
	if err := st.saveConversation(ctx, sid, "593999@s.whatsapp.net", 7, "src", 42); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.lookupConversation(ctx, sid, "593999@s.whatsapp.net"); got != 42 {
		t.Fatalf("mapeo no guardado: %d", got)
	}

	// Mismo inbox: el mapeo se conserva (rotar el token no debe perderlo).
	same := old
	same.AccountToken = "t2"
	if err := st.setChatwoot(ctx, sid, same); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.lookupConversation(ctx, sid, "593999@s.whatsapp.net"); got != 42 {
		t.Fatalf("mapeo perdido al rotar token: %d", got)
	}

	// Inbox distinto: se invalida.
	moved := old
	moved.InboxID = 5
	if err := st.setChatwoot(ctx, sid, moved); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.lookupConversation(ctx, sid, "593999@s.whatsapp.net"); got != 0 {
		t.Fatalf("mapeo viejo sobrevivió al cambio de inbox: %d", got)
	}
}

func TestChatwootFromEnv(t *testing.T) {
	for _, k := range []string{"WACALLS_CHATWOOT_URL", "WACALLS_CHATWOOT_TOKEN",
		"WACALLS_CHATWOOT_ACCOUNT_ID", "WACALLS_CHATWOOT_INBOX_ID",
		"WACALLS_CHATWOOT_INBOX_IDENTIFIER", "WACALLS_CHATWOOT_SESSION"} {
		t.Setenv(k, "")
	}
	if _, name, ok := chatwootFromEnv(); ok || name != defaultChatwootEnvSession {
		t.Fatalf("sin variables no debería haber config; name=%q ok=%v", name, ok)
	}

	t.Setenv("WACALLS_CHATWOOT_URL", "http://rails:3000")
	t.Setenv("WACALLS_CHATWOOT_TOKEN", "tok")
	t.Setenv("WACALLS_CHATWOOT_ACCOUNT_ID", "2")
	t.Setenv("WACALLS_CHATWOOT_INBOX_ID", "5")
	t.Setenv("WACALLS_CHATWOOT_INBOX_IDENTIFIER", "ident")
	t.Setenv("WACALLS_CHATWOOT_SESSION", "linea-1")
	cfg, name, ok := chatwootFromEnv()
	if !ok || name != "linea-1" {
		t.Fatalf("ok=%v name=%q", ok, name)
	}
	if !cfg.valid() || cfg.AccountID != 2 || cfg.InboxID != 5 || cfg.InboxIdentifier != "ident" {
		t.Fatalf("cfg mal parseada: %+v", cfg)
	}

	// Config a medias: se detecta como presente pero inválida, para poder
	// reportarla en vez de escribir algo que rompa la integración en silencio.
	t.Setenv("WACALLS_CHATWOOT_TOKEN", "")
	cfg, _, ok = chatwootFromEnv()
	if !ok {
		t.Fatal("con algunas variables puestas debe reportarse presente")
	}
	if cfg.valid() {
		t.Fatal("sin token no debería ser válida")
	}
}

// Un grupo no tiene teléfono: el contacto debe crearse SIN phone_number (que
// Chatwoot rechazaría) y con el identifier = chatID, que es cómo se lo vuelve a
// encontrar después.
func TestEnsureContactGroupSinTelefono(t *testing.T) {
	var created map[string]any
	var searchQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			searchQ = r.URL.Query().Get("q")
			w.Write([]byte(`{"payload":[]}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&created)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"payload":{"contact":{"id":77,"contact_inboxes":[{"source_id":"src","inbox":{"id":1}}]}}}`))
	}))
	defer srv.Close()

	cfg := ChatwootConfig{URL: srv.URL, AccountID: 1, AccountToken: "t", InboxID: 1}
	chatID := "120363000000000000@g.us"
	id, src, err := cfg.ensureContact(context.Background(), chatID, "", "Grupo DAHER", "https://x/av.jpg")
	if err != nil {
		t.Fatalf("ensureContact: %v", err)
	}
	if id != 77 || src != "src" {
		t.Fatalf("id=%d src=%q", id, src)
	}
	if searchQ != chatID {
		t.Fatalf("sin teléfono debe buscar por identifier; buscó %q", searchQ)
	}
	if _, tiene := created["phone_number"]; tiene {
		t.Fatalf("no debe mandar phone_number en un grupo: %v", created["phone_number"])
	}
	if created["identifier"] != chatID {
		t.Fatalf("identifier=%v", created["identifier"])
	}
	if created["name"] != "Grupo DAHER" || created["avatar_url"] != "https://x/av.jpg" {
		t.Fatalf("name/avatar mal: %v / %v", created["name"], created["avatar_url"])
	}
}
