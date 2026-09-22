package main

// Endpoints HTTP de la integración Chatwoot:
//   POST   /api/sessions/{sid}/chatwoot          configura la conexión
//   GET    /api/sessions/{sid}/chatwoot          devuelve la config (token redactado)
//   DELETE /api/sessions/{sid}/chatwoot          borra la config
//   POST   /api/sessions/{sid}/chatwoot/webhook  recibe eventos de Chatwoot (outgoing -> WhatsApp)

import (
	"crypto/subtle"
	"net/http"
	"strconv"
)

func (s *server) handleSetChatwoot(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionByID(w, r.PathValue("sid"))
	if sess == nil {
		return
	}
	var cfg ChatwootConfig
	if err := decodeBody(w, r, &cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if !cfg.valid() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url, account_id, account_token e inbox_id son obligatorios"})
		return
	}
	if err := s.sessions.store.setChatwoot(r.Context(), sess.id, cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	applied, _ := s.sessions.store.getChatwoot(r.Context(), sess.id)
	// El secreto se devuelve UNA vez acá, para pegar la URL completa en
	// Chatwoot en el momento — GET no lo vuelve a mostrar (redactChatwoot lo
	// omite), igual que con account_token.
	writeJSON(w, http.StatusOK, map[string]any{
		"chatwoot": redactChatwoot(cfg), "enabled": true,
		"webhook_url": webhookHint(sess.id, applied.WebhookSecret),
	})
}

func (s *server) handleGetChatwoot(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionByID(w, r.PathValue("sid"))
	if sess == nil {
		return
	}
	cfg, ok := s.sessions.store.getChatwoot(r.Context(), sess.id)
	writeJSON(w, http.StatusOK, map[string]any{"chatwoot": redactChatwoot(cfg), "enabled": ok})
}

func (s *server) handleDeleteChatwoot(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionByID(w, r.PathValue("sid"))
	if sess == nil {
		return
	}
	if err := s.sessions.store.deleteChatwoot(r.Context(), sess.id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleChatwootWebhook(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionByID(w, r.PathValue("sid"))
	if sess == nil {
		return
	}
	// Auth del webhook: esta ruta está exenta de X-API-Key porque Chatwoot no
	// permite agregar headers a sus webhooks — el secreto viaja por query
	// string, que es lo único que Chatwoot sí controla (vía la URL del
	// webhook_url del inbox). Sin una config de Chatwoot no hay a quién firmar
	// el mensaje y se rechaza: aceptar aquí permitiría reenviar WhatsApp a
	// cualquier destinatario del payload sin credenciales. Antes esto era
	// fail-open (sesión sin config o secret vacío aceptaba); se endurece a
	// fail-closed para no dar acceso de envío a quien no tiene secret.
	cfg, ok := s.sessions.store.getChatwoot(r.Context(), sess.id)
	if !ok || cfg.WebhookSecret == "" {
		sess.log.Warn("chatwoot: webhook rechazado: sesión sin config o sin secreto", "session", sess.id)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !secretMatches(r.URL.Query().Get("secret"), cfg.WebhookSecret) {
		sess.log.Warn("chatwoot: webhook rechazado: secreto inválido", "session", sess.id)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var p chatwootWebhookPayload
	if err := decodeBody(w, r, &p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if sess.client.Store.ID == nil {
		// No pareada: aceptamos el webhook (2xx, para que Chatwoot no reintente)
		// pero no hay a quién enviar.
		writeJSON(w, http.StatusOK, map[string]string{"status": "session not paired"})
		return
	}
	if err := sess.deliverToWhatsApp(r.Context(), p); err != nil {
		sess.log.Error("chatwoot: deliver to whatsapp failed", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleChatwootResolve mapea (account_id, conversation_id) de Chatwoot a la
// sesión de WhatsApp desde la que llamar y al teléfono del contacto. Lo usa el
// widget de llamada embebido en Chatwoot.
func (s *server) handleChatwootResolve(w http.ResponseWriter, r *http.Request) {
	accountID, _ := strconv.Atoi(r.URL.Query().Get("account_id"))
	convID, _ := strconv.Atoi(r.URL.Query().Get("conversation_id"))
	if accountID == 0 || convID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account_id y conversation_id requeridos"})
		return
	}
	configs, err := s.sessions.store.configsByAccount(r.Context(), accountID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(configs) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sin sesión para esa cuenta de Chatwoot"})
		return
	}
	for _, sc := range configs {
		// La config puede haber quedado huérfana: la sesión ya no existe pero
		// su fila sobrevivió (bases anteriores al borrado en cascada). Sin
		// este guard le devolvemos al widget un id muerto y el POST a /calls
		// falla con "no such session", sin pista de por qué.
		if _, viva := s.sessions.Get(sc.SessionID); !viva {
			continue
		}
		inboxID, phone, name, err := sc.Cfg.getConversation(r.Context(), convID)
		if err != nil {
			continue // esta config no puede leer la conversación; probamos la siguiente
		}
		if inboxID != sc.Cfg.InboxID {
			continue // la conversación es de otro inbox
		}
		digits := onlyDigits(phone)
		if digits == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "el contacto no tiene teléfono"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": sc.SessionID, "inbox_id": inboxID, "phone": digits, "name": name,
		})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "sin sesión para esa conversación"})
}

// handleSetRecording activa/desactiva la grabación de llamadas de la sesión.
func (s *server) handleSetRecording(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionByID(w, r.PathValue("sid"))
	if sess == nil {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled requerido"})
		return
	}
	if err := s.sessions.store.setRecording(r.Context(), sess.id, body.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": body.Enabled})
}

// handleGetRecording devuelve el estado del toggle de grabación.
func (s *server) handleGetRecording(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionByID(w, r.PathValue("sid"))
	if sess == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.sessions.store.getRecording(r.Context(), sess.id)})
}

// redactChatwoot oculta el account_token para no exponerlo en respuestas GET.
func redactChatwoot(c ChatwootConfig) ChatwootConfig {
	if c.AccountToken != "" {
		c.AccountToken = ""
	}
	return c
}

// secretMatches compara el secret del webhook en tiempo constante para no
// filtrar información por temporización.
func secretMatches(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
