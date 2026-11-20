// Package store persists agent sessions in SQLite via modernc.org/sqlite
// (pure Go, no CGO). It backs resume (turn replay), rewind (tail deletion),
// and append-only auditing. Writes take immediate locks so concurrent
// sessions on one file stay consistent; every fault wraps its cause.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound marks a missing session.
var ErrNotFound = errors.New("store: not found")

// Turn kinds. Tool interactions are first-class rows so rewind and audit see
// them exactly like messages.
const (
	TurnMessage    = "message"     // Role: user|assistant|system
	TurnToolCall   = "tool_call"   // Data: {"name": ..., "params": ...}
	TurnToolResult = "tool_result" // Data: {"name": ..., "is_error": ...}
	TurnEvent      = "event"       // Data: structured marker (compaction, escalation)
)

// Session is one persisted agent session header.
type Session struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
	Mode      string
	Provider  string
	Model     string
	Workspace string
	Metadata  json.RawMessage
}

// Turn is one persisted conversation element. Data carries structured
// payloads (tool name/params/result, usage) as JSON.
type Turn struct {
	Seq       int64
	Kind      string
	Role      string
	Content   string
	Data      json.RawMessage
	CreatedAt time.Time
}

// AuditEntry is one append-only audit record.
type AuditEntry struct {
	ID        int64
	SessionID string
	At        time.Time
	Kind      string
	Detail    string
}

// Store is one opened database handle. Use is concurrency-safe.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions(
    id         TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    mode       TEXT NOT NULL,
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    workspace  TEXT NOT NULL,
    metadata   TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS turns(
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    kind       TEXT NOT NULL,
    role       TEXT NOT NULL DEFAULT '',
    content    TEXT NOT NULL DEFAULT '',
    data       TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, seq)
);
CREATE INDEX IF NOT EXISTS turns_seq ON turns(session_id, seq);
CREATE TABLE IF NOT EXISTS audit(
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    ts         INTEGER NOT NULL,
    kind       TEXT NOT NULL,
    detail     TEXT NOT NULL
);
`

// Open opens (creating if needed) the SQLite database at path. The special
// path ":memory:" yields a transient in-memory store.
func Open(path string) (*Store, error) {
	dsn := path
	if dsn != ":memory:" {
		dsn = "file:" + dsn
	}
	// Immediate transaction locks keep multi-session writers consistent on
	// one file; WAL tolerates concurrent readers; busy timeout absorbs
	// brief lock contention instead of failing.
	dsn += "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// CreateSession registers a new session and returns its header.
func (s *Store) CreateSession(ctx context.Context, mode, provider, model, workspace string, metadata json.RawMessage) (Session, error) {
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}
	now := time.Now()
	sess := Session{
		ID:        newID(),
		CreatedAt: now,
		UpdatedAt: now,
		Mode:      mode,
		Provider:  provider,
		Model:     model,
		Workspace: workspace,
		Metadata:  metadata,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, created_at, updated_at, mode, provider, model, workspace, metadata)
		 VALUES(?,?,?,?,?,?,?,?)`,
		sess.ID, now.UnixMilli(), now.UnixMilli(), mode, provider, model, workspace, string(metadata))
	if err != nil {
		return Session{}, fmt.Errorf("store: create session: %w", err)
	}
	return sess, nil
}

func (s *Store) scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var (
		sess Session
		ca   int64
		ua   int64
		meta string
	)
	if err := row.Scan(&sess.ID, &ca, &ua, &sess.Mode, &sess.Provider, &sess.Model, &sess.Workspace, &meta); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("store: scan session: %w", err)
	}
	sess.CreatedAt = time.UnixMilli(ca)
	sess.UpdatedAt = time.UnixMilli(ua)
	sess.Metadata = json.RawMessage(meta)
	if sess.Metadata == nil {
		sess.Metadata = json.RawMessage("{}")
	}
	return sess, nil
}

// GetSession fetches one session header by ID.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, created_at, updated_at, mode, provider, model, workspace, metadata
		 FROM sessions WHERE id = ?`, id)
	return s.scanSession(row)
}

// ListSessions returns all session headers, newest activity first.
func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, updated_at, mode, provider, model, workspace, metadata
		 FROM sessions ORDER BY updated_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := s.scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	return out, nil
}

// AppendTurn appends one conversation element to a session, returning its
// sequence number. Sequences are contiguous per session; the append and the
// header timestamp update commit atomically.
func (s *Store) AppendTurn(ctx context.Context, sessionID, kind, role, content string, data any) (int64, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("store: encode turn data: %w", err)
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var seq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM turns WHERE session_id = ?`,
		sessionID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: next seq: %w", err)
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO turns(session_id, seq, kind, role, content, data, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		sessionID, seq, kind, role, content, string(payload), now); err != nil {
		return 0, fmt.Errorf("store: insert turn: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		return 0, fmt.Errorf("store: touch session: %w", err)
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`,
		sessionID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: verify session: %w", err)
	}
	if n == 0 {
		return 0, fmt.Errorf("store: session %s: %w", sessionID, ErrNotFound)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit append: %w", err)
	}
	return seq, nil
}

// LoadTurns replays a session's conversation elements in sequence order.
func (s *Store) LoadTurns(ctx context.Context, sessionID string) ([]Turn, error) {
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, kind, role, content, data, created_at
		 FROM turns WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: load turns: %w", err)
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		var (
			t   Turn
			cs  int64
			raw string
		)
		if err := rows.Scan(&t.Seq, &t.Kind, &t.Role, &t.Content, &raw, &cs); err != nil {
			return nil, fmt.Errorf("store: scan turn: %w", err)
		}
		t.Data = json.RawMessage(raw)
		t.CreatedAt = time.UnixMilli(cs)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load turns: %w", err)
	}
	return out, nil
}

// Rewind truncates a session's conversation to turns with seq <= upToSeq,
// returning how many turns were deleted. Audit history is untouched.
func (s *Store) Rewind(ctx context.Context, sessionID string, upToSeq int64) (int64, error) {
	if upToSeq < 0 {
		return 0, fmt.Errorf("store: rewind seq must be >= 0")
	}
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin rewind: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM turns WHERE session_id = ? AND seq > ?`, sessionID, upToSeq)
	if err != nil {
		return 0, fmt.Errorf("store: delete turns: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), sessionID); err != nil {
		return 0, fmt.Errorf("store: touch session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit rewind: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Audit appends one audit record. Audit rows are never rewritten.
func (s *Store) Audit(ctx context.Context, sessionID, kind, detail string) error {
	if _, err := s.GetSession(ctx, sessionID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO audit(session_id, ts, kind, detail) VALUES(?,?,?,?)`,
		sessionID, time.Now().UnixMilli(), kind, detail); err != nil {
		return fmt.Errorf("store: audit: %w", err)
	}
	return nil
}

// AuditLog returns the newest audit records for a session, capped at limit.
func (s *Store) AuditLog(ctx context.Context, sessionID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, ts, kind, detail
		 FROM audit WHERE session_id = ? ORDER BY id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: audit log: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var (
			e  AuditEntry
			ts int64
		)
		if err := rows.Scan(&e.ID, &e.SessionID, &ts, &e.Kind, &e.Detail); err != nil {
			return nil, fmt.Errorf("store: scan audit: %w", err)
		}
		e.At = time.UnixMilli(ts)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: audit log: %w", err)
	}
	return out, nil
}
