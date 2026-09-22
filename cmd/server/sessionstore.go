package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
)

type sessionRow struct {
	ID   string
	Name string
	JID  string
}

type sessionStore struct{ db *sql.DB }

func newSessionStore(ctx context.Context, db *sql.DB) (*sessionStore, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sessions (
		id   TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		jid  TEXT
	)`)
	if err != nil {
		return nil, err
	}
	// Las tablas de Chatwoot son parte del esquema de este store: delete()
	// borra en cascada sobre ellas, así que un store sin ellas está roto por
	// construcción. Es idempotente (CREATE IF NOT EXISTS).
	if err := ensureChatwootTables(ctx, db); err != nil {
		return nil, err
	}
	return &sessionStore{db: db}, nil
}

func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *sessionStore) list(ctx context.Context) ([]sessionRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, COALESCE(jid, '') FROM sessions ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.ID, &r.Name, &r.JID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *sessionStore) insert(ctx context.Context, id, name string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, name, jid) VALUES (?, ?, NULL)`, id, name)
	return err
}

func (s *sessionStore) setJID(ctx context.Context, id, jid string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET jid = ? WHERE id = ?`, jid, id)
	return err
}

// delete borra la sesión Y todo lo que cuelga de ella. El id es irrepetible
// (hex aleatorio), así que esas filas no le sirven a nadie más: dejarlas
// convertía la config de Chatwoot en huérfana, y handleChatwootResolve se la
// entregaba al widget como si la sesión siguiera viva — el síntoma era un
// "no such session" al llamar, después de re-parear el número.
func (s *sessionStore) delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return err
	}
	if err := s.deleteChatwoot(ctx, id); err != nil { // arrastra las conversaciones
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM session_recording WHERE session_id = ?`, id)
	return err
}
