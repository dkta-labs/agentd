package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/runtime"
)

type DB struct {
	db *sql.DB
}

type SessionRecord struct {
	ID              string
	WorkspaceID     string
	WorkspacePath   string
	HarnessID       string
	State           string
	NodeID          string
	NodeName        string
	PlacementReason string
	Remote          bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type DeviceRecord struct {
	ID                  string
	Name                string
	TokenHash           string
	NotifyReady         bool
	NotifyInputRequired bool
	NotifyFailed        bool
	CreatedAt           time.Time
	LastSeenAt          time.Time
}

type NodeRecord struct {
	ID         string
	Name       string
	TokenHash  string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

type PushSubscriptionRecord struct {
	DeviceID string
	Endpoint string
	P256dh   string
	Auth     string
}

var ErrEnrollmentTokenUsed = errors.New("enrollment token has already been used")

var ErrInputConflict = errors.New("idempotency key conflicts with a different input")

type InputRecord struct {
	SessionID      string
	ID             string
	IdempotencyKey string
	Mode           runtime.InputMode
	Text           string
	Status         string
	Error          string
	EventSequence  uint64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func Open(ctx context.Context, dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, "agentd.sqlite")
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000"
	database, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	database.SetMaxOpenConns(4)
	database.SetMaxIdleConns(4)
	store := &DB{db: database}
	if err := store.migrate(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE sessions
		SET state = 'interrupted', updated_at = ?
		WHERE state IN ('starting', 'running', 'idle', 'blocked')
	`, encodeTime(time.Now().UTC())); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("mark stale sessions interrupted: %w", err)
	}
	if err := store.recoverActiveLoops(ctx, time.Now().UTC()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

func (s *DB) Close() error {
	return s.db.Close()
}

func (s *DB) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			workspace_path TEXT NOT NULL,
			harness_id TEXT NOT NULL DEFAULT 'omp',
			node_id TEXT NOT NULL DEFAULT '',
			node_name TEXT NOT NULL DEFAULT '',
			placement_reason TEXT NOT NULL DEFAULT '',
			remote INTEGER NOT NULL DEFAULT 0,
			state TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			source_id TEXT,
			type TEXT NOT NULL,
			payload_json BLOB NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS inputs (
			session_id TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			input_id TEXT NOT NULL,
			mode TEXT NOT NULL,
			text TEXT NOT NULL,
			status TEXT NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			event_sequence INTEGER,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (session_id, idempotency_key),
			UNIQUE (session_id, input_id),
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS loops (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			workspace_id TEXT NOT NULL,
			harness_id TEXT NOT NULL DEFAULT 'omp',
			prompt TEXT NOT NULL,
			cadence_seconds INTEGER NOT NULL,
			timeout_seconds INTEGER NOT NULL,
			desired_state TEXT NOT NULL,
			state TEXT NOT NULL,
			iteration INTEGER NOT NULL DEFAULT 0,
			run_requested INTEGER NOT NULL DEFAULT 0,
			active_run_id TEXT NOT NULL DEFAULT '',
			active_session_id TEXT NOT NULL DEFAULT '',
			last_event_sequence INTEGER NOT NULL DEFAULT 0,
			next_run_at TEXT,
			last_run_at TEXT,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS loop_runs (
			id TEXT PRIMARY KEY,
			loop_id TEXT NOT NULL,
			iteration INTEGER NOT NULL,
			session_id TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			state TEXT NOT NULL,
			last_event_sequence INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			finished_at TEXT,
			UNIQUE (loop_id, iteration),
			FOREIGN KEY (loop_id) REFERENCES loops(id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS devices (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			notify_ready INTEGER NOT NULL DEFAULT 1,
			notify_input_required INTEGER NOT NULL DEFAULT 1,
			notify_failed INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS nodes (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS enrollment_tokens (
			token_hash TEXT PRIMARY KEY,
			used_at TEXT NOT NULL,
			device_id TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS push_subscriptions (
			device_id TEXT PRIMARY KEY,
			endpoint TEXT NOT NULL UNIQUE,
			p256dh TEXT NOT NULL,
			auth TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS events_session_sequence
		ON events(session_id, sequence);
		CREATE INDEX IF NOT EXISTS loops_schedule
		ON loops(state, run_requested, next_run_at);
		CREATE INDEX IF NOT EXISTS loop_runs_loop_iteration
		ON loop_runs(loop_id, iteration DESC);
	`)
	if err != nil {
		return fmt.Errorf("migrate SQLite: %w", err)
	}
	if err := s.ensureSessionHarnessColumn(ctx); err != nil {
		return err
	}
	if err := s.ensureSessionPlacementColumns(ctx); err != nil {
		return err
	}
	if err := s.ensureEventSourceColumn(ctx); err != nil {
		return err
	}
	if err := s.normalizeLegacyEventTypes(ctx); err != nil {
		return err
	}
	if err := s.removeEnrollmentDeviceForeignKey(ctx); err != nil {
		return err
	}
	return nil
}

func (s *DB) ensureSessionHarnessColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(sessions)`)
	if err != nil {
		return fmt.Errorf("inspect session schema: %w", err)
	}
	hasHarnessID := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan session schema: %w", err)
		}
		if name == "harness_id" {
			hasHarnessID = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read session schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close session schema query: %w", err)
	}
	if hasHarnessID {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE sessions ADD COLUMN harness_id TEXT NOT NULL DEFAULT 'omp'`); err != nil {
		return fmt.Errorf("add session harness column: %w", err)
	}
	return nil
}

func (s *DB) ensureSessionPlacementColumns(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(sessions)`)
	if err != nil {
		return fmt.Errorf("inspect session placement schema: %w", err)
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan session placement schema: %w", err)
		}
		existing[name] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close session placement schema query: %w", err)
	}
	columns := []struct {
		name string
		sql  string
	}{
		{"node_id", `ALTER TABLE sessions ADD COLUMN node_id TEXT NOT NULL DEFAULT ''`},
		{"node_name", `ALTER TABLE sessions ADD COLUMN node_name TEXT NOT NULL DEFAULT ''`},
		{"placement_reason", `ALTER TABLE sessions ADD COLUMN placement_reason TEXT NOT NULL DEFAULT ''`},
		{"remote", `ALTER TABLE sessions ADD COLUMN remote INTEGER NOT NULL DEFAULT 0`},
	}
	for _, column := range columns {
		if existing[column.name] {
			continue
		}
		if _, err := s.db.ExecContext(ctx, column.sql); err != nil {
			return fmt.Errorf("add session %s column: %w", column.name, err)
		}
	}
	return nil
}

func (s *DB) ensureEventSourceColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(events)`)
	if err != nil {
		return fmt.Errorf("inspect event schema: %w", err)
	}
	hasSourceID := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan event schema: %w", err)
		}
		if name == "source_id" {
			hasSourceID = true
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close event schema query: %w", err)
	}
	if !hasSourceID {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE events ADD COLUMN source_id TEXT`); err != nil {
			return fmt.Errorf("add event source column: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE UNIQUE INDEX IF NOT EXISTS events_session_source
		ON events(session_id, source_id)
	`); err != nil {
		return fmt.Errorf("create event source index: %w", err)
	}
	return nil
}

func (s *DB) normalizeLegacyEventTypes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE events
		SET type = CASE type
			WHEN 'ready' THEN 'session_ready'
			WHEN 'agent_start' THEN 'turn_started'
			WHEN 'agent_end' THEN 'turn_completed'
			WHEN 'message_start' THEN 'assistant_started'
			WHEN 'message_update' THEN 'assistant_delta'
			WHEN 'message_end' THEN 'assistant_completed'
			WHEN 'extension_ui_request' THEN 'interaction_requested'
			WHEN 'extension_ui_response' THEN 'interaction_submitted'
			WHEN 'extension_ui_response_delivered' THEN 'interaction_delivered'
			WHEN 'extension_ui_response_failed' THEN 'interaction_failed'
			WHEN 'extension_ui_expired' THEN 'interaction_expired'
			WHEN 'user_input' THEN 'input_submitted'
			WHEN 'user_input_delivered' THEN 'input_delivered'
			WHEN 'user_input_failed' THEN 'input_failed'
			WHEN 'tool_execution_start' THEN 'tool_started'
			WHEN 'tool_execution_update' THEN 'tool_updated'
			WHEN 'tool_execution_end' THEN 'tool_completed'
			WHEN 'stderr' THEN 'session_stderr'
			WHEN 'rpc_protocol_error' THEN 'session_protocol_error'
			WHEN 'rpc_read_error' THEN 'session_protocol_error'
			WHEN 'rpc_write_error' THEN 'session_protocol_error'
			WHEN 'process_exit' THEN 'session_exited'
			ELSE type
		END
		WHERE type IN (
			'ready', 'agent_start', 'agent_end',
			'message_start', 'message_update', 'message_end',
			'extension_ui_request', 'extension_ui_response',
			'extension_ui_response_delivered', 'extension_ui_response_failed', 'extension_ui_expired',
			'user_input', 'user_input_delivered', 'user_input_failed',
			'tool_execution_start', 'tool_execution_update', 'tool_execution_end',
			'stderr', 'rpc_protocol_error', 'rpc_read_error', 'rpc_write_error', 'process_exit'
		)
	`)
	if err != nil {
		return fmt.Errorf("normalize legacy event types: %w", err)
	}
	return nil
}

func (s *DB) removeEnrollmentDeviceForeignKey(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_list(enrollment_tokens)`)
	if err != nil {
		return fmt.Errorf("inspect enrollment token schema: %w", err)
	}
	hasForeignKey := rows.Next()
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read enrollment token schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close enrollment token schema query: %w", err)
	}
	if !hasForeignKey {
		return nil
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin enrollment token migration: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `
		CREATE TABLE enrollment_tokens_without_device_fk (
			token_hash TEXT PRIMARY KEY,
			used_at TEXT NOT NULL,
			device_id TEXT NOT NULL
		);
		INSERT INTO enrollment_tokens_without_device_fk(token_hash, used_at, device_id)
		SELECT token_hash, used_at, device_id FROM enrollment_tokens;
		DROP TABLE enrollment_tokens;
		ALTER TABLE enrollment_tokens_without_device_fk RENAME TO enrollment_tokens;
	`); err != nil {
		return fmt.Errorf("migrate enrollment token schema: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit enrollment token migration: %w", err)
	}
	return nil
}

func (s *DB) CreateSession(ctx context.Context, record SessionRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, workspace_id, workspace_path, harness_id, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.WorkspaceID, record.WorkspacePath, record.HarnessID, record.State, encodeTime(record.CreatedAt), encodeTime(record.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

func (s *DB) ReserveInput(ctx context.Context, record InputRecord) (InputRecord, events.Event, bool, error) {
	if record.SessionID == "" || record.ID == "" || record.IdempotencyKey == "" {
		return InputRecord{}, events.Event{}, false, errors.New("input session, id, and idempotency key are required")
	}
	now := time.Now().UTC()
	record.Status = "pending"
	record.CreatedAt = now
	record.UpdatedAt = now
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InputRecord{}, events.Event{}, false, fmt.Errorf("begin input reservation: %w", err)
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(ctx, `
		INSERT OR IGNORE INTO inputs(
			session_id, idempotency_key, input_id, mode, text, status, error,
			event_sequence, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, 'pending', '', NULL, ?, ?)
	`, record.SessionID, record.IdempotencyKey, record.ID, string(record.Mode), record.Text, encodeTime(now), encodeTime(now))
	if err != nil {
		return InputRecord{}, events.Event{}, false, fmt.Errorf("reserve input: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return InputRecord{}, events.Event{}, false, fmt.Errorf("read input reservation count: %w", err)
	}
	if inserted == 0 {
		var existing InputRecord
		var mode, createdAt, updatedAt string
		var eventSequence sql.NullInt64
		err := transaction.QueryRowContext(ctx, `
			SELECT session_id, input_id, idempotency_key, mode, text, status, error,
			       event_sequence, created_at, updated_at
			FROM inputs
			WHERE session_id = ? AND idempotency_key = ?
		`, record.SessionID, record.IdempotencyKey).Scan(
			&existing.SessionID,
			&existing.ID,
			&existing.IdempotencyKey,
			&mode,
			&existing.Text,
			&existing.Status,
			&existing.Error,
			&eventSequence,
			&createdAt,
			&updatedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return InputRecord{}, events.Event{}, false, ErrInputConflict
		}
		if err != nil {
			return InputRecord{}, events.Event{}, false, fmt.Errorf("read reserved input: %w", err)
		}
		existing.Mode = runtime.InputMode(mode)
		if eventSequence.Valid {
			existing.EventSequence = uint64(eventSequence.Int64)
		}
		existing.CreatedAt, err = decodeTime(createdAt)
		if err != nil {
			return InputRecord{}, events.Event{}, false, err
		}
		existing.UpdatedAt, err = decodeTime(updatedAt)
		if err != nil {
			return InputRecord{}, events.Event{}, false, err
		}
		if existing.Mode != record.Mode || existing.Text != record.Text {
			return InputRecord{}, events.Event{}, false, ErrInputConflict
		}
		return existing, events.Event{}, false, nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":     record.ID,
		"mode":   record.Mode,
		"text":   record.Text,
		"status": record.Status,
	})
	if err != nil {
		return InputRecord{}, events.Event{}, false, fmt.Errorf("encode input intent: %w", err)
	}
	eventResult, err := transaction.ExecContext(ctx, `
		INSERT INTO events(session_id, type, payload_json, created_at)
		VALUES (?, ?, ?, ?)
	`, record.SessionID, runtime.EventInputSubmitted, []byte(payload), encodeTime(now))
	if err != nil {
		return InputRecord{}, events.Event{}, false, fmt.Errorf("insert input intent event: %w", err)
	}
	sequence, err := eventResult.LastInsertId()
	if err != nil {
		return InputRecord{}, events.Event{}, false, fmt.Errorf("read input intent sequence: %w", err)
	}
	record.EventSequence = uint64(sequence)
	if _, err := transaction.ExecContext(ctx, `
		UPDATE inputs SET event_sequence = ? WHERE session_id = ? AND idempotency_key = ?
	`, sequence, record.SessionID, record.IdempotencyKey); err != nil {
		return InputRecord{}, events.Event{}, false, fmt.Errorf("link input intent event: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return InputRecord{}, events.Event{}, false, fmt.Errorf("commit input reservation: %w", err)
	}
	return record, events.Event{
		Sequence:  record.EventSequence,
		SessionID: record.SessionID,
		Type:      runtime.EventInputSubmitted,
		Payload:   payload,
		CreatedAt: now,
	}, true, nil
}

func (s *DB) CompleteInput(
	ctx context.Context,
	sessionID string,
	idempotencyKey string,
	status string,
	failure string,
) (events.Event, error) {
	if status != "delivered" && status != "failed" {
		return events.Event{}, fmt.Errorf("unsupported input status %q", status)
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return events.Event{}, fmt.Errorf("begin input completion: %w", err)
	}
	defer transaction.Rollback()
	var inputID string
	if err := transaction.QueryRowContext(ctx, `
		SELECT input_id FROM inputs WHERE session_id = ? AND idempotency_key = ?
	`, sessionID, idempotencyKey).Scan(&inputID); err != nil {
		return events.Event{}, fmt.Errorf("read input completion target: %w", err)
	}
	now := time.Now().UTC()
	if _, err := transaction.ExecContext(ctx, `
		UPDATE inputs SET status = ?, error = ?, updated_at = ?
		WHERE session_id = ? AND idempotency_key = ?
	`, status, failure, encodeTime(now), sessionID, idempotencyKey); err != nil {
		return events.Event{}, fmt.Errorf("complete input: %w", err)
	}
	eventType := runtime.EventInputDelivered
	if status == "failed" {
		eventType = runtime.EventInputFailed
	}
	payload, err := json.Marshal(map[string]any{
		"id":     inputID,
		"status": status,
		"error":  failure,
	})
	if err != nil {
		return events.Event{}, fmt.Errorf("encode input completion: %w", err)
	}
	result, err := transaction.ExecContext(ctx, `
		INSERT INTO events(session_id, type, payload_json, created_at)
		VALUES (?, ?, ?, ?)
	`, sessionID, eventType, []byte(payload), encodeTime(now))
	if err != nil {
		return events.Event{}, fmt.Errorf("insert input completion event: %w", err)
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return events.Event{}, fmt.Errorf("read input completion sequence: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return events.Event{}, fmt.Errorf("commit input completion: %w", err)
	}
	return events.Event{
		Sequence:  uint64(sequence),
		SessionID: sessionID,
		Type:      eventType,
		Payload:   payload,
		CreatedAt: now,
	}, nil
}

func (s *DB) SetSessionState(ctx context.Context, id, state string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET state = ?, updated_at = ? WHERE id = ?`, state, encodeTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("update session state: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read session state update count: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *DB) SetSessionPlacement(ctx context.Context, id, nodeID, nodeName, reason string, remote bool) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET node_id = ?, node_name = ?, placement_reason = ?, remote = ?, updated_at = ?
		WHERE id = ?
	`, nodeID, nodeName, reason, remote, encodeTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("update session placement: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read session placement update count: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *DB) Sessions(ctx context.Context) ([]SessionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, workspace_id, workspace_path, harness_id, state,
		       node_id, node_name, placement_reason, remote, created_at, updated_at
		FROM sessions
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()
	var records []SessionRecord
	for rows.Next() {
		var record SessionRecord
		var createdAt, updatedAt string
		if err := rows.Scan(
			&record.ID,
			&record.WorkspaceID,
			&record.WorkspacePath,
			&record.HarnessID,
			&record.State,
			&record.NodeID,
			&record.NodeName,
			&record.PlacementReason,
			&record.Remote,
			&createdAt,
			&updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		var err error
		record.CreatedAt, err = decodeTime(createdAt)
		if err != nil {
			return nil, err
		}
		record.UpdatedAt, err = decodeTime(updatedAt)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return records, nil
}

func (s *DB) AppendEvent(ctx context.Context, incoming runtime.Event) (events.Event, error) {
	payload := append(json.RawMessage(nil), incoming.Payload...)
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return events.Event{}, fmt.Errorf("begin event transaction: %w", err)
	}
	defer transaction.Rollback()
	var sourceID any
	if incoming.SourceID != "" {
		sourceID = incoming.SourceID
	}
	result, err := transaction.ExecContext(ctx, `
		INSERT INTO events(session_id, source_id, type, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id, source_id) DO NOTHING
	`, incoming.SessionID, sourceID, incoming.Type, []byte(payload), encodeTime(incoming.CreatedAt))
	if err != nil {
		return events.Event{}, fmt.Errorf("insert event: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return events.Event{}, fmt.Errorf("read event insertion count: %w", err)
	}
	if inserted == 0 {
		return events.Event{}, events.ErrDuplicate
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return events.Event{}, fmt.Errorf("read event sequence: %w", err)
	}
	if state := stateForEvent(incoming.Type); state != "" {
		if _, err := transaction.ExecContext(ctx, `
			UPDATE sessions SET state = ?, updated_at = ? WHERE id = ?
		`, state, encodeTime(incoming.CreatedAt), incoming.SessionID); err != nil {
			return events.Event{}, fmt.Errorf("update session with event: %w", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return events.Event{}, fmt.Errorf("commit event: %w", err)
	}
	return events.Event{
		Sequence:  uint64(sequence),
		SessionID: incoming.SessionID,
		Type:      incoming.Type,
		Payload:   payload,
		CreatedAt: incoming.CreatedAt,
	}, nil
}

func stateForEvent(eventType string) string {
	switch eventType {
	case runtime.EventSessionReady, runtime.EventTurnCompleted:
		return "idle"
	case runtime.EventTurnStarted:
		return "running"
	case runtime.EventSessionExited:
		return "exited"
	default:
		return ""
	}
}

func (s *DB) EventsAfter(ctx context.Context, sessionID string, sequence uint64) ([]events.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sequence, session_id, type, payload_json, created_at
		FROM events
		WHERE session_id = ? AND sequence > ?
		ORDER BY sequence ASC
	`, sessionID, sequence)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	var result []events.Event
	for rows.Next() {
		var event events.Event
		var payload []byte
		var createdAt string
		if err := rows.Scan(&event.Sequence, &event.SessionID, &event.Type, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		event.Payload = append(json.RawMessage(nil), payload...)
		event.CreatedAt, err = decodeTime(createdAt)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return result, nil
}

func (s *DB) EnrollDevice(ctx context.Context, enrollmentHash string, record DeviceRecord) error {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin device enrollment: %w", err)
	}
	defer transaction.Rollback()
	_, err = transaction.ExecContext(ctx, `
		INSERT INTO devices(
			id, name, token_hash, notify_ready, notify_input_required, notify_failed,
			created_at, last_seen_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.Name, record.TokenHash, record.NotifyReady, record.NotifyInputRequired,
		record.NotifyFailed, encodeTime(record.CreatedAt), encodeTime(record.LastSeenAt))
	if err != nil {
		return fmt.Errorf("insert device: %w", err)
	}
	result, err := transaction.ExecContext(ctx, `
		INSERT OR IGNORE INTO enrollment_tokens(token_hash, used_at, device_id)
		VALUES (?, ?, ?)
	`, enrollmentHash, encodeTime(record.CreatedAt), record.ID)
	if err != nil {
		return fmt.Errorf("consume enrollment token: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read enrollment token result: %w", err)
	}
	if inserted == 0 {
		return ErrEnrollmentTokenUsed
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit device enrollment: %w", err)
	}
	return nil
}

func EnrollmentTokenUsedAt(ctx context.Context, dataDir, tokenHash string) (bool, error) {
	path := filepath.Join(dataDir, "agentd.sqlite")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect agentd state: %w", err)
	}
	location := &url.URL{Scheme: "file", Path: path}
	query := location.Query()
	query.Set("mode", "ro")
	query.Set("_busy_timeout", "5000")
	location.RawQuery = query.Encode()
	database, err := sql.Open("sqlite3", location.String())
	if err != nil {
		return false, fmt.Errorf("open agentd state read-only: %w", err)
	}
	defer database.Close()
	var used bool
	if err := database.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM enrollment_tokens WHERE token_hash = ?
		)
	`, tokenHash).Scan(&used); err != nil {
		return false, fmt.Errorf("read enrollment token state: %w", err)
	}
	return used, nil
}

func (s *DB) EnrollmentTokenUsed(ctx context.Context, tokenHash string) (bool, error) {
	var used bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM enrollment_tokens WHERE token_hash = ?
		)
	`, tokenHash).Scan(&used); err != nil {
		return false, fmt.Errorf("read enrollment token state: %w", err)
	}
	return used, nil
}

func (s *DB) DeviceByTokenHash(ctx context.Context, tokenHash string, seenAt time.Time) (DeviceRecord, error) {
	var record DeviceRecord
	var createdAt, lastSeenAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, token_hash, notify_ready, notify_input_required, notify_failed,
		       created_at, last_seen_at
		FROM devices
		WHERE token_hash = ?
	`, tokenHash).Scan(
		&record.ID,
		&record.Name,
		&record.TokenHash,
		&record.NotifyReady,
		&record.NotifyInputRequired,
		&record.NotifyFailed,
		&createdAt,
		&lastSeenAt,
	)
	if err != nil {
		return DeviceRecord{}, err
	}
	record.CreatedAt, err = decodeTime(createdAt)
	if err != nil {
		return DeviceRecord{}, err
	}
	record.LastSeenAt, err = decodeTime(lastSeenAt)
	if err != nil {
		return DeviceRecord{}, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen_at = ? WHERE id = ?`, encodeTime(seenAt), record.ID); err != nil {
		return DeviceRecord{}, fmt.Errorf("update device last seen time: %w", err)
	}
	record.LastSeenAt = seenAt
	return record, nil
}

func (s *DB) UpdateDevicePreferences(
	ctx context.Context,
	deviceID string,
	notifyReady bool,
	notifyInputRequired bool,
	notifyFailed bool,
) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE devices
		SET notify_ready = ?, notify_input_required = ?, notify_failed = ?
		WHERE id = ?
	`, notifyReady, notifyInputRequired, notifyFailed, deviceID)
	if err != nil {
		return fmt.Errorf("update device preferences: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read device preference update count: %w", err)
	}
	if updated == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *DB) UpsertPushSubscription(ctx context.Context, record PushSubscriptionRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO push_subscriptions(device_id, endpoint, p256dh, auth, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			endpoint = excluded.endpoint,
			p256dh = excluded.p256dh,
			auth = excluded.auth,
			updated_at = excluded.updated_at
	`, record.DeviceID, record.Endpoint, record.P256dh, record.Auth, encodeTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("upsert push subscription: %w", err)
	}
	return nil
}

func (s *DB) DeletePushSubscription(ctx context.Context, deviceID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE device_id = ?`, deviceID); err != nil {
		return fmt.Errorf("delete push subscription: %w", err)
	}
	return nil
}

func (s *DB) PushSubscriptions(ctx context.Context, category string) ([]PushSubscriptionRecord, error) {
	var preference string
	switch category {
	case "ready":
		preference = "notify_ready"
	case "input_required":
		preference = "notify_input_required"
	case "failed":
		preference = "notify_failed"
	default:
		return nil, fmt.Errorf("unsupported push category %q", category)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT push_subscriptions.device_id, endpoint, p256dh, auth
		FROM push_subscriptions
		JOIN devices ON devices.id = push_subscriptions.device_id
		WHERE `+preference+` = 1
	`)
	if err != nil {
		return nil, fmt.Errorf("query push subscriptions: %w", err)
	}
	defer rows.Close()
	var records []PushSubscriptionRecord
	for rows.Next() {
		var record PushSubscriptionRecord
		if err := rows.Scan(&record.DeviceID, &record.Endpoint, &record.P256dh, &record.Auth); err != nil {
			return nil, fmt.Errorf("scan push subscription: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate push subscriptions: %w", err)
	}
	return records, nil
}

func (s *DB) Devices(ctx context.Context) ([]DeviceRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, token_hash, notify_ready, notify_input_required, notify_failed,
		       created_at, last_seen_at
		FROM devices
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query devices: %w", err)
	}
	defer rows.Close()
	var records []DeviceRecord
	for rows.Next() {
		var record DeviceRecord
		var createdAt, lastSeenAt string
		if err := rows.Scan(
			&record.ID,
			&record.Name,
			&record.TokenHash,
			&record.NotifyReady,
			&record.NotifyInputRequired,
			&record.NotifyFailed,
			&createdAt,
			&lastSeenAt,
		); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		record.CreatedAt, err = decodeTime(createdAt)
		if err != nil {
			return nil, err
		}
		record.LastSeenAt, err = decodeTime(lastSeenAt)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate devices: %w", err)
	}
	return records, nil
}

func (s *DB) DeleteDevice(ctx context.Context, deviceID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, deviceID)
	if err != nil {
		return fmt.Errorf("delete device: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read device deletion count: %w", err)
	}
	if deleted == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *DB) CreateNode(ctx context.Context, record NodeRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO nodes (id, name, token_hash, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)
	`, record.ID, record.Name, record.TokenHash, encodeTime(record.CreatedAt), encodeTime(record.LastSeenAt))
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	return nil
}

func (s *DB) NodeByTokenHash(ctx context.Context, tokenHash string, seenAt time.Time) (NodeRecord, error) {
	var record NodeRecord
	var createdAt, lastSeenAt string
	err := s.db.QueryRowContext(ctx, `
		UPDATE nodes
		SET last_seen_at = ?
		WHERE token_hash = ?
		RETURNING id, name, token_hash, created_at, last_seen_at
	`, encodeTime(seenAt), tokenHash).Scan(
		&record.ID,
		&record.Name,
		&record.TokenHash,
		&createdAt,
		&lastSeenAt,
	)
	if err != nil {
		return NodeRecord{}, err
	}
	record.CreatedAt, err = decodeTime(createdAt)
	if err != nil {
		return NodeRecord{}, err
	}
	record.LastSeenAt, err = decodeTime(lastSeenAt)
	if err != nil {
		return NodeRecord{}, err
	}
	return record, nil
}

func (s *DB) Nodes(ctx context.Context) ([]NodeRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, token_hash, created_at, last_seen_at
		FROM nodes
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query nodes: %w", err)
	}
	defer rows.Close()
	var records []NodeRecord
	for rows.Next() {
		var record NodeRecord
		var createdAt, lastSeenAt string
		if err := rows.Scan(&record.ID, &record.Name, &record.TokenHash, &createdAt, &lastSeenAt); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		record.CreatedAt, err = decodeTime(createdAt)
		if err != nil {
			return nil, err
		}
		record.LastSeenAt, err = decodeTime(lastSeenAt)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate nodes: %w", err)
	}
	return records, nil
}

func encodeTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func decodeTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("decode stored time: %w", err)
	}
	return parsed, nil
}
