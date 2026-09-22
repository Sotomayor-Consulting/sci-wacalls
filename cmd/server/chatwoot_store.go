package main

// Persistencia (SQLite) de la integración Chatwoot:
//   - chatwoot_configs: la conexión Chatwoot de cada sesión.
//   - chatwoot_conversations: mapeo chat de WhatsApp -> contacto/conversación de
//     Chatwoot, para no recrearlos en cada mensaje.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"strings"
)

// ensureChatwootTables crea las tablas de la integración si no existen.
func ensureChatwootTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chatwoot_configs (
			session_id       TEXT PRIMARY KEY,
			url              TEXT NOT NULL,
			account_id       INTEGER NOT NULL,
			account_token    TEXT NOT NULL,
			inbox_id         INTEGER NOT NULL,
			inbox_identifier TEXT NOT NULL DEFAULT '',
			webhook_secret   TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS chatwoot_conversations (
			session_id      TEXT NOT NULL,
			wa_chat_id      TEXT NOT NULL,
			contact_id      INTEGER NOT NULL,
			source_id       TEXT NOT NULL,
			conversation_id INTEGER NOT NULL,
			PRIMARY KEY (session_id, wa_chat_id)
		);
		CREATE TABLE IF NOT EXISTS session_recording (
			session_id TEXT PRIMARY KEY,
			enabled    INTEGER NOT NULL
		);`)
	if err != nil {
		return err
	}
	// SQLite no tiene "ADD COLUMN IF NOT EXISTS" en todas las versiones que
	// podemos encontrar en el campo: para una base ya existente (creada antes
	// de este campo), se intenta el ALTER y se ignora el único error posible
	// ("duplicate column name"), que significa que ya estaba.
	if _, err := db.ExecContext(ctx, `ALTER TABLE chatwoot_configs ADD COLUMN webhook_secret TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

// newWebhookSecret genera un secreto aleatorio para autenticar el webhook.
func newWebhookSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// setRecording activa/desactiva la grabación de llamadas de una sesión.
func (s *sessionStore) setRecording(ctx context.Context, sessionID string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_recording (session_id, enabled) VALUES (?, ?)
		ON CONFLICT(session_id) DO UPDATE SET enabled=excluded.enabled`, sessionID, v)
	return err
}

// getRecording indica si la grabación está activada para la sesión.
func (s *sessionStore) getRecording(ctx context.Context, sessionID string) bool {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM session_recording WHERE session_id = ?`, sessionID).Scan(&v)
	return err == nil && v == 1
}

func (s *sessionStore) setChatwoot(ctx context.Context, sessionID string, c ChatwootConfig) error {
	prev, had := s.getChatwoot(ctx, sessionID)
	// Si la config apunta a otra cuenta u otro inbox, los mapeos guardados
	// referencian conversaciones que no existen ahí: postear en ellas devuelve
	// 404 indefinidamente. Se invalidan para que se recreen en el próximo
	// mensaje.
	if had && (prev.AccountID != c.AccountID || prev.InboxID != c.InboxID || prev.URL != c.URL) {
		if err := s.clearConversations(ctx, sessionID); err != nil {
			return err
		}
	}
	// El secreto del webhook se genera una vez y se conserva en los updates
	// siguientes (rotar el token del agente, por ejemplo, no debe invalidar la
	// URL que ya está cargada en Chatwoot). Solo se genera de nuevo si la
	// config anterior no tenía uno (compatibilidad con configs de antes de
	// este campo).
	secret := c.WebhookSecret
	if had && prev.WebhookSecret != "" {
		secret = prev.WebhookSecret
	} else if secret == "" {
		secret = newWebhookSecret()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chatwoot_configs (session_id, url, account_id, account_token, inbox_id, inbox_identifier, webhook_secret)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			url=excluded.url, account_id=excluded.account_id, account_token=excluded.account_token,
			inbox_id=excluded.inbox_id, inbox_identifier=excluded.inbox_identifier,
			webhook_secret=excluded.webhook_secret`,
		sessionID, c.URL, c.AccountID, c.AccountToken, c.InboxID, c.InboxIdentifier, secret)
	return err
}

// getChatwoot devuelve la config de la sesión y ok=false si no hay ninguna.
func (s *sessionStore) getChatwoot(ctx context.Context, sessionID string) (ChatwootConfig, bool) {
	var c ChatwootConfig
	err := s.db.QueryRowContext(ctx, `
		SELECT url, account_id, account_token, inbox_id, inbox_identifier, webhook_secret
		FROM chatwoot_configs WHERE session_id = ?`, sessionID).
		Scan(&c.URL, &c.AccountID, &c.AccountToken, &c.InboxID, &c.InboxIdentifier, &c.WebhookSecret)
	if err != nil {
		return ChatwootConfig{}, false
	}
	return c, true
}

func (s *sessionStore) deleteChatwoot(ctx context.Context, sessionID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chatwoot_configs WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	// Los mapeos chat->conversación pertenecen al inbox que se está quitando:
	// si sobreviven, una config nueva heredaría conversaciones ajenas.
	return s.clearConversations(ctx, sessionID)
}

// clearConversations borra el mapeo local chat de WhatsApp -> conversación de
// Chatwoot de una sesión. Las conversaciones se vuelven a crear solas en el
// próximo mensaje.
func (s *sessionStore) clearConversations(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM chatwoot_conversations WHERE session_id = ?`, sessionID)
	return err
}

// sessionChatwoot asocia una sesión con su config de Chatwoot.
type sessionChatwoot struct {
	SessionID string
	Cfg       ChatwootConfig
}

// configsByAccount devuelve las sesiones cuya config apunta a la cuenta de
// Chatwoot dada (para resolver desde qué sesión llamar a un contacto).
func (s *sessionStore) configsByAccount(ctx context.Context, accountID int) ([]sessionChatwoot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, url, account_id, account_token, inbox_id, inbox_identifier
		FROM chatwoot_configs WHERE account_id = ?`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionChatwoot
	for rows.Next() {
		var sc sessionChatwoot
		if err := rows.Scan(&sc.SessionID, &sc.Cfg.URL, &sc.Cfg.AccountID, &sc.Cfg.AccountToken, &sc.Cfg.InboxID, &sc.Cfg.InboxIdentifier); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// lookupConversation devuelve el id de conversación de Chatwoot mapeado a un chat
// de WhatsApp, o 0 si todavía no existe.
func (s *sessionStore) lookupConversation(ctx context.Context, sessionID, waChatID string) (int, error) {
	var convID int
	err := s.db.QueryRowContext(ctx, `
		SELECT conversation_id FROM chatwoot_conversations
		WHERE session_id = ? AND wa_chat_id = ?`, sessionID, waChatID).Scan(&convID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return convID, err
}

func (s *sessionStore) saveConversation(ctx context.Context, sessionID, waChatID string, contactID int, sourceID string, convID int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chatwoot_conversations (session_id, wa_chat_id, contact_id, source_id, conversation_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id, wa_chat_id) DO UPDATE SET
			contact_id=excluded.contact_id, source_id=excluded.source_id, conversation_id=excluded.conversation_id`,
		sessionID, waChatID, contactID, sourceID, convID)
	return err
}
