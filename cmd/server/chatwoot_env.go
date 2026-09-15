package main

// Configuración de la integración con Chatwoot por variables de entorno.
//
// Por API (POST /api/sessions/{sid}/chatwoot) hay que conocer el id de sesión,
// que se genera en runtime: sirve para varias sesiones o para reconfigurar en
// caliente, pero en un despliegue de una sola línea obliga a un paso manual
// después de cada arranque limpio. Con estas variables el despliegue queda
// declarativo: el compose las trae y no hay que llamar a la API.
//
// Las variables MANDAN sobre lo guardado en SQLite. Es a propósito: si la base
// pudiera ganarle al entorno, cambiar el compose no tendría efecto y la causa
// sería muy difícil de ver — el mismo problema que tiene Chatwoot con sus
// installation_configs pisando el entorno.

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// chatwootEnvSession es el NOMBRE de sesión al que se aplica la config del
// entorno. Solo hace falta si se corre más de una sesión.
const defaultChatwootEnvSession = "inbox-b"

// chatwootFromEnv arma la config desde el entorno. ok=false si no está definida
// (ninguna WACALLS_CHATWOOT_*), que es el caso de quien configura por API.
func chatwootFromEnv() (cfg ChatwootConfig, sessionName string, ok bool) {
	cfg = ChatwootConfig{
		URL:             strings.TrimSpace(os.Getenv("WACALLS_CHATWOOT_URL")),
		AccountToken:    strings.TrimSpace(os.Getenv("WACALLS_CHATWOOT_TOKEN")),
		InboxIdentifier: strings.TrimSpace(os.Getenv("WACALLS_CHATWOOT_INBOX_IDENTIFIER")),
	}
	cfg.AccountID, _ = strconv.Atoi(strings.TrimSpace(os.Getenv("WACALLS_CHATWOOT_ACCOUNT_ID")))
	cfg.InboxID, _ = strconv.Atoi(strings.TrimSpace(os.Getenv("WACALLS_CHATWOOT_INBOX_ID")))

	sessionName = strings.TrimSpace(os.Getenv("WACALLS_CHATWOOT_SESSION"))
	if sessionName == "" {
		sessionName = defaultChatwootEnvSession
	}
	if cfg.URL == "" && cfg.AccountToken == "" && cfg.AccountID == 0 && cfg.InboxID == 0 {
		return ChatwootConfig{}, sessionName, false
	}
	return cfg, sessionName, true
}

// applyChatwootEnv guarda la config del entorno en la sesión que corresponda.
// Se llama tras restaurar las sesiones y al crear una nueva, así el pareo de un
// número en un despliegue nuevo queda integrado sin tocar la API.
//
// Una config incompleta se reporta y se ignora: escribirla a medias dejaría la
// integración caída sin decir por qué.
func (m *SessionManager) applyChatwootEnv(ctx context.Context) {
	cfg, name, ok := chatwootFromEnv()
	if !ok {
		return
	}
	log := m.log.With("session_name", name)
	if !cfg.valid() {
		log.Error("chatwoot (env): configuración incompleta, se ignora",
			"url", cfg.URL != "", "account_id", cfg.AccountID,
			"token", cfg.AccountToken != "", "inbox_id", cfg.InboxID)
		return
	}
	sessions := m.all()
	// Con UNA sola sesión aplicamos la config sin exigir que el nombre coincida:
	// el nombre lo elige quien crea la sesión (la UI sugiere "WhatsApp"), y no
	// hay ambigüedad posible. Antes, un nombre distinto hacía que el bucle no
	// encontrara nada y la integración quedaba sin configurar SIN avisar.
	if len(sessions) == 1 && sessions[0].name != name {
		log.Warn("chatwoot (env): la única sesión tiene otro nombre; se le aplica igual",
			"sesion_nombre", sessions[0].name, "esperado", name)
		name = sessions[0].name
	}
	matched := 0
	for _, s := range sessions {
		if s.name != name {
			continue
		}
		matched++
		prev, had := m.store.getChatwoot(ctx, s.id)
		if had && prev == cfg {
			continue // ya está así: no toquetear el mapeo de conversaciones
		}
		if err := m.store.setChatwoot(ctx, s.id, cfg); err != nil {
			log.Error("chatwoot (env): no se pudo guardar", "session", s.id, "err", err)
			continue
		}
		log.Info("chatwoot (env): configuración aplicada",
			"session", s.id, "account_id", cfg.AccountID, "inbox_id", cfg.InboxID,
			"webhook", webhookHint(s.id), slog.String("nota", "el webhook_url del inbox se configura en Chatwoot"))
	}
	// Ninguna sesión coincidió: hay que decirlo. Callarse deja la integración
	// muda con las variables aparentemente bien puestas — el síntoma es que los
	// mensajes SALEN (el webhook no usa esta config) pero no ENTRAN.
	if matched == 0 {
		nombres := make([]string, 0, len(sessions))
		for _, s := range sessions {
			nombres = append(nombres, s.name)
		}
		log.Error("chatwoot (env): ninguna sesión coincide con WACALLS_CHATWOOT_SESSION; la integración queda SIN configurar",
			"esperado", name, "sesiones_existentes", nombres)
	}
}

// webhookHint devuelve la ruta que hay que poner como webhook_url del inbox API
// en Chatwoot. Se loguea al aplicar la config porque es el único paso que no se
// puede automatizar desde acá.
func webhookHint(sessionID string) string {
	return "/api/sessions/" + sessionID + "/chatwoot/webhook"
}
