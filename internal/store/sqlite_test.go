package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/runtime"
)

func TestEventReplaySurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.CreateSession(ctx, SessionRecord{
		ID:            "ses_test",
		WorkspaceID:   "agentd",
		WorkspacePath: "/private/repo",
		State:         "running",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	broker := events.NewBroker(database)
	if err := broker.Publish(ctx, runtime.Event{
		SessionID: "ses_test",
		Type:      runtime.EventAssistantDelta,
		Payload:   json.RawMessage(`{"delta":"hello"}`),
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	replayed, err := database.EventsAfter(ctx, "ses_test", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 {
		t.Fatalf("replayed %d events, want 1", len(replayed))
	}
	if replayed[0].Sequence == 0 || replayed[0].Type != runtime.EventAssistantDelta || string(replayed[0].Payload) != `{"delta":"hello"}` {
		t.Fatalf("unexpected replayed event: %#v", replayed[0])
	}
	records, err := database.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State != "interrupted" {
		t.Fatalf("unexpected restored sessions: %#v", records)
	}
}
func TestEventSourceIDPreventsRetryDuplication(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	if err := database.CreateSession(ctx, SessionRecord{
		ID: "ses_dedupe", WorkspaceID: "agentd", WorkspacePath: "/private/repo",
		HarnessID: "omp", State: "running", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	broker := events.NewBroker(database)
	incoming := runtime.Event{
		SessionID: "ses_dedupe",
		Type:      runtime.EventAssistantDelta,
		SourceID:  "node_1:event_1",
		Payload:   json.RawMessage(`{"delta":"once"}`),
		CreatedAt: now,
	}
	if err := broker.Publish(ctx, incoming); err != nil {
		t.Fatal(err)
	}
	if err := broker.Publish(ctx, incoming); err != nil {
		t.Fatal(err)
	}
	replayed, err := database.EventsAfter(ctx, "ses_dedupe", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 {
		t.Fatalf("retried worker event persisted %d times", len(replayed))
	}
}

func TestReopenNormalizesLegacyEventTypes(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.CreateSession(ctx, SessionRecord{
		ID: "ses_legacy", WorkspaceID: "agentd", WorkspacePath: "/private/repo",
		HarnessID: "omp", State: "idle", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `
		INSERT INTO events(session_id, type, payload_json, created_at)
		VALUES (?, 'message_update', ?, ?)
	`, "ses_legacy", []byte(`{"assistantMessageEvent":{"delta":"legacy"}}`), encodeTime(now)); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	replayed, err := database.EventsAfter(ctx, "ses_legacy", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 || replayed[0].Type != runtime.EventAssistantDelta {
		t.Fatalf("legacy replay = %#v", replayed)
	}
}

func TestInputReservationIsIdempotentAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.CreateSession(ctx, SessionRecord{
		ID:            "ses_input",
		WorkspaceID:   "agentd",
		WorkspacePath: "/private/repo",
		State:         "running",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	input := InputRecord{
		SessionID:      "ses_input",
		ID:             "inp_original",
		IdempotencyKey: "stable-key",
		Mode:           runtime.InputPrompt,
		Text:           "dispatch once",
	}
	reserved, intent, created, err := database.ReserveInput(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !created || reserved.Status != "pending" || intent.Type != runtime.EventInputSubmitted {
		t.Fatalf("unexpected initial reservation: %#v %#v created=%t", reserved, intent, created)
	}
	completed, err := database.CompleteInput(ctx, input.SessionID, input.IdempotencyKey, "delivered", "")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Type != runtime.EventInputDelivered {
		t.Fatalf("completion event = %#v", completed)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	input.ID = "inp_retry_may_differ"
	reserved, _, created, err = database.ReserveInput(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if created || reserved.ID != "inp_original" || reserved.Status != "delivered" {
		t.Fatalf("reopened reservation = %#v created=%t", reserved, created)
	}
	input.Text = "different"
	if _, _, _, err := database.ReserveInput(ctx, input); !errors.Is(err, ErrInputConflict) {
		t.Fatalf("conflicting input error = %v, want %v", err, ErrInputConflict)
	}
	replayed, err := database.EventsAfter(ctx, input.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 2 {
		t.Fatalf("replayed %d input events, want 2: %#v", len(replayed), replayed)
	}
}

func TestPendingInputIsNotRedispatchedAfterReopen(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.CreateSession(ctx, SessionRecord{
		ID:            "ses_pending",
		WorkspaceID:   "agentd",
		WorkspacePath: "/private/repo",
		State:         "running",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	input := InputRecord{
		SessionID:      "ses_pending",
		ID:             "inp_pending",
		IdempotencyKey: "pending-key",
		Mode:           runtime.InputPrompt,
		Text:           "uncertain dispatch",
	}
	if _, _, created, err := database.ReserveInput(ctx, input); err != nil || !created {
		t.Fatalf("initial reservation created=%t error=%v", created, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	reserved, _, created, err := database.ReserveInput(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if created || reserved.Status != "pending" {
		t.Fatalf("pending retry = %#v created=%t", reserved, created)
	}
	replayed, err := database.EventsAfter(ctx, input.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 || replayed[0].Type != runtime.EventInputSubmitted {
		t.Fatalf("pending replay = %#v", replayed)
	}
}

func TestEnrollmentTokenMigrationPreservesConsumptionAfterRevocation(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite3", filepath.Join(dataDir, "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		PRAGMA foreign_keys = ON;
		CREATE TABLE devices (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			notify_ready INTEGER NOT NULL DEFAULT 1,
			notify_input_required INTEGER NOT NULL DEFAULT 1,
			notify_failed INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL
		);
		CREATE TABLE enrollment_tokens (
			token_hash TEXT PRIMARY KEY,
			used_at TEXT NOT NULL,
			device_id TEXT NOT NULL,
			FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
		);
		INSERT INTO devices(id, name, token_hash, created_at, last_seen_at)
		VALUES ('dev_legacy', 'Legacy phone', 'device-hash', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
		INSERT INTO enrollment_tokens(token_hash, used_at, device_id)
		VALUES ('enrollment-hash', '2026-01-01T00:00:00Z', 'dev_legacy');
	`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.DeleteDevice(ctx, "dev_legacy"); err != nil {
		t.Fatal(err)
	}
	var consumed int
	if err := database.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM enrollment_tokens WHERE token_hash = 'enrollment-hash'
	`).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != 1 {
		t.Fatalf("consumed enrollment token count = %d, want 1", consumed)
	}
	rows, err := database.db.QueryContext(ctx, `PRAGMA foreign_key_list(enrollment_tokens)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("enrollment token schema still cascades through a device foreign key")
	}
}

func TestEnrollmentTokenLookupDoesNotInterruptLiveSession(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.CreateSession(ctx, SessionRecord{
		ID:            "ses_live",
		WorkspaceID:   "agentd",
		WorkspacePath: "/private/repo",
		State:         "running",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	used, err := EnrollmentTokenUsedAt(ctx, dataDir, "unused-token-hash")
	if err != nil {
		t.Fatal(err)
	}
	if used {
		t.Fatal("unused enrollment token reported as consumed")
	}

	raw, err := sql.Open("sqlite3", filepath.Join(dataDir, "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var state string
	if err := raw.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id = 'ses_live'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("pairing token lookup changed live session state to %q", state)
	}
}
