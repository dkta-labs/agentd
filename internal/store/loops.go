package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrLoopActive       = errors.New("loop has an active run")
	ErrLoopManual       = errors.New("manual loops cannot be started on a cadence")
	ErrLoopRunRequested = errors.New("loop run is already requested")
)

type LoopRecord struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	WorkspaceID       string     `json:"workspaceId"`
	HarnessID         string     `json:"harnessId"`
	Prompt            string     `json:"prompt"`
	CadenceSeconds    int        `json:"cadenceSeconds"`
	TimeoutSeconds    int        `json:"timeoutSeconds"`
	DesiredState      string     `json:"desiredState"`
	State             string     `json:"state"`
	Iteration         int64      `json:"iteration"`
	RunRequested      bool       `json:"runRequested"`
	ActiveRunID       string     `json:"activeRunId,omitempty"`
	ActiveSessionID   string     `json:"activeSessionId,omitempty"`
	LastEventSequence uint64     `json:"lastEventSequence"`
	NextRunAt         *time.Time `json:"nextRunAt,omitempty"`
	LastRunAt         *time.Time `json:"lastRunAt,omitempty"`
	LastError         string     `json:"lastError,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

type LoopRunRecord struct {
	ID                string     `json:"id"`
	LoopID            string     `json:"loopId"`
	Iteration         int64      `json:"iteration"`
	SessionID         string     `json:"sessionId"`
	IdempotencyKey    string     `json:"idempotencyKey"`
	State             string     `json:"state"`
	LastEventSequence uint64     `json:"lastEventSequence"`
	Error             string     `json:"error,omitempty"`
	StartedAt         time.Time  `json:"startedAt"`
	FinishedAt        *time.Time `json:"finishedAt,omitempty"`
}

type rowScanner interface {
	Scan(...any) error
}

const loopColumns = `
	id, name, workspace_id, harness_id, prompt, cadence_seconds, timeout_seconds,
	desired_state, state, iteration, run_requested, active_run_id,
	active_session_id, last_event_sequence, next_run_at, last_run_at, last_error,
	created_at, updated_at`

func (s *DB) CreateLoop(ctx context.Context, record LoopRecord) (LoopRecord, error) {
	now := time.Now().UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO loops(
			id, name, workspace_id, harness_id, prompt, cadence_seconds,
			timeout_seconds, desired_state, state, iteration, run_requested,
			active_run_id, active_session_id, last_event_sequence, next_run_at,
			last_run_at, last_error, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.Name, record.WorkspaceID, record.HarnessID, record.Prompt,
		record.CadenceSeconds, record.TimeoutSeconds, record.DesiredState,
		record.State, record.Iteration, record.RunRequested, record.ActiveRunID,
		record.ActiveSessionID, record.LastEventSequence, encodeOptionalTime(record.NextRunAt),
		encodeOptionalTime(record.LastRunAt), record.LastError, encodeTime(record.CreatedAt),
		encodeTime(record.UpdatedAt))
	if err != nil {
		return LoopRecord{}, fmt.Errorf("insert loop: %w", err)
	}
	return record, nil
}

func (s *DB) UpdateLoopDefinition(ctx context.Context, record LoopRecord) (LoopRecord, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE loops
		SET name = ?, workspace_id = ?, harness_id = ?, prompt = ?,
		    cadence_seconds = ?, timeout_seconds = ?, updated_at = ?
		WHERE id = ? AND state NOT IN ('starting', 'running', 'blocked')
	`, record.Name, record.WorkspaceID, record.HarnessID, record.Prompt,
		record.CadenceSeconds, record.TimeoutSeconds, encodeTime(time.Now().UTC()), record.ID)
	if err != nil {
		return LoopRecord{}, fmt.Errorf("update loop definition: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return LoopRecord{}, fmt.Errorf("read loop definition update count: %w", err)
	}
	if updated == 0 {
		if _, lookupErr := s.Loop(ctx, record.ID); lookupErr != nil {
			return LoopRecord{}, lookupErr
		}
		return LoopRecord{}, ErrLoopActive
	}
	return s.Loop(ctx, record.ID)
}

func (s *DB) Loop(ctx context.Context, id string) (LoopRecord, error) {
	return scanLoop(s.db.QueryRowContext(ctx, `SELECT `+loopColumns+` FROM loops WHERE id = ?`, id))
}

func (s *DB) Loops(ctx context.Context) ([]LoopRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+loopColumns+` FROM loops ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("query loops: %w", err)
	}
	defer rows.Close()
	var result []LoopRecord
	for rows.Next() {
		record, err := scanLoop(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loops: %w", err)
	}
	return result, nil
}

func (s *DB) StartLoop(ctx context.Context, id string, now time.Time) (LoopRecord, error) {
	record, err := s.Loop(ctx, id)
	if err != nil {
		return LoopRecord{}, err
	}
	if record.CadenceSeconds == 0 {
		return LoopRecord{}, ErrLoopManual
	}
	now = now.UTC()
	_, err = s.db.ExecContext(ctx, `
		UPDATE loops
		SET desired_state = 'running',
		    state = CASE WHEN state IN ('starting', 'running', 'blocked') THEN state ELSE 'scheduled' END,
		    next_run_at = CASE WHEN state IN ('starting', 'running', 'blocked') THEN next_run_at ELSE ? END,
		    last_error = '', updated_at = ?
		WHERE id = ?
	`, encodeTime(now), encodeTime(now), id)
	if err != nil {
		return LoopRecord{}, fmt.Errorf("start loop: %w", err)
	}
	return s.Loop(ctx, id)
}

func (s *DB) PauseLoop(ctx context.Context, id string, now time.Time) (LoopRecord, error) {
	now = now.UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE loops
		SET desired_state = 'paused',
		    run_requested = 0,
		    state = CASE WHEN state IN ('starting', 'running', 'blocked') THEN state ELSE 'paused' END,
		    next_run_at = CASE WHEN state IN ('starting', 'running', 'blocked') THEN next_run_at ELSE NULL END,
		    updated_at = ?
		WHERE id = ?
	`, encodeTime(now), id)
	if err != nil {
		return LoopRecord{}, fmt.Errorf("pause loop: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return LoopRecord{}, fmt.Errorf("read loop pause count: %w", err)
	}
	if updated == 0 {
		return LoopRecord{}, sql.ErrNoRows
	}
	return s.Loop(ctx, id)
}

func (s *DB) RequestLoopRun(ctx context.Context, id string, now time.Time) (LoopRecord, error) {
	record, err := s.Loop(ctx, id)
	if err != nil {
		return LoopRecord{}, err
	}
	if loopStateActive(record.State) {
		return LoopRecord{}, ErrLoopActive
	}
	if record.RunRequested {
		return LoopRecord{}, ErrLoopRunRequested
	}
	now = now.UTC()
	_, err = s.db.ExecContext(ctx, `
		UPDATE loops
		SET run_requested = 1, state = 'scheduled', last_error = '', updated_at = ?
		WHERE id = ?
	`, encodeTime(now), id)
	if err != nil {
		return LoopRecord{}, fmt.Errorf("request loop run: %w", err)
	}
	return s.Loop(ctx, id)
}

func (s *DB) ClaimNextLoop(ctx context.Context, now time.Time, runID, sessionID string) (LoopRecord, LoopRunRecord, bool, error) {
	now = now.UTC()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LoopRecord{}, LoopRunRecord{}, false, fmt.Errorf("begin loop claim: %w", err)
	}
	defer transaction.Rollback()
	record, err := scanLoop(transaction.QueryRowContext(ctx, `
		SELECT `+loopColumns+`
		FROM loops
		WHERE state = 'scheduled'
		  AND (
		    run_requested = 1 OR
		    (desired_state = 'running' AND cadence_seconds > 0 AND next_run_at IS NOT NULL AND next_run_at <= ?)
		  )
		ORDER BY run_requested DESC, next_run_at ASC, created_at ASC
		LIMIT 1
	`, encodeTime(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return LoopRecord{}, LoopRunRecord{}, false, nil
	}
	if err != nil {
		return LoopRecord{}, LoopRunRecord{}, false, err
	}
	iteration := record.Iteration + 1
	idempotencyKey := fmt.Sprintf("loop:%s:%d", record.ID, iteration)
	result, err := transaction.ExecContext(ctx, `
		UPDATE loops
		SET state = 'starting', iteration = ?, run_requested = 0,
		    active_run_id = ?, active_session_id = ?, last_event_sequence = 0,
		    last_run_at = ?, updated_at = ?
		WHERE id = ? AND state = 'scheduled'
	`, iteration, runID, sessionID, encodeTime(now), encodeTime(now), record.ID)
	if err != nil {
		return LoopRecord{}, LoopRunRecord{}, false, fmt.Errorf("claim loop: %w", err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return LoopRecord{}, LoopRunRecord{}, false, fmt.Errorf("read loop claim count: %w", err)
	}
	if claimed == 0 {
		return LoopRecord{}, LoopRunRecord{}, false, nil
	}
	run := LoopRunRecord{
		ID: runID, LoopID: record.ID, Iteration: iteration, SessionID: sessionID,
		IdempotencyKey: idempotencyKey, State: "starting", StartedAt: now,
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO loop_runs(
			id, loop_id, iteration, session_id, idempotency_key, state,
			last_event_sequence, error, started_at, finished_at
		)
		VALUES (?, ?, ?, ?, ?, ?, 0, '', ?, NULL)
	`, run.ID, run.LoopID, run.Iteration, run.SessionID, run.IdempotencyKey,
		run.State, encodeTime(run.StartedAt)); err != nil {
		return LoopRecord{}, LoopRunRecord{}, false, fmt.Errorf("insert loop run: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return LoopRecord{}, LoopRunRecord{}, false, fmt.Errorf("commit loop claim: %w", err)
	}
	record.Iteration = iteration
	record.RunRequested = false
	record.State = "starting"
	record.ActiveRunID = runID
	record.ActiveSessionID = sessionID
	record.LastEventSequence = 0
	record.LastRunAt = &now
	record.UpdatedAt = now
	return record, run, true, nil
}

func (s *DB) SetLoopRunState(ctx context.Context, loopID, runID, state string, sequence uint64) error {
	now := time.Now().UTC()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin loop state update: %w", err)
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(ctx, `
		UPDATE loops SET state = ?, last_event_sequence = ?, updated_at = ?
		WHERE id = ? AND active_run_id = ?
	`, state, sequence, encodeTime(now), loopID, runID)
	if err != nil {
		return fmt.Errorf("update loop state: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read loop state update count: %w", err)
	}
	if updated == 0 {
		return sql.ErrNoRows
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE loop_runs SET state = ?, last_event_sequence = ? WHERE id = ? AND loop_id = ?
	`, state, sequence, runID, loopID); err != nil {
		return fmt.Errorf("update loop run state: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit loop state update: %w", err)
	}
	return nil
}

func (s *DB) FinishLoopRun(ctx context.Context, loopID, runID, state string, sequence uint64, failure string, now time.Time) (LoopRecord, error) {
	now = now.UTC()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LoopRecord{}, fmt.Errorf("begin loop completion: %w", err)
	}
	defer transaction.Rollback()
	record, err := scanLoop(transaction.QueryRowContext(ctx, `SELECT `+loopColumns+` FROM loops WHERE id = ?`, loopID))
	if err != nil {
		return LoopRecord{}, err
	}
	if record.ActiveRunID != runID {
		return record, nil
	}
	finishedAt := encodeTime(now)
	if _, err := transaction.ExecContext(ctx, `
		UPDATE loop_runs
		SET state = ?, last_event_sequence = ?, error = ?, finished_at = ?
		WHERE id = ? AND loop_id = ?
	`, state, sequence, failure, finishedAt, runID, loopID); err != nil {
		return LoopRecord{}, fmt.Errorf("finish loop run: %w", err)
	}
	loopState := state
	desiredState := record.DesiredState
	var nextRunAt *time.Time
	if state == "completed" {
		failure = ""
		if desiredState == "running" && record.CadenceSeconds > 0 {
			loopState = "scheduled"
			next := now.Add(time.Duration(record.CadenceSeconds) * time.Second)
			nextRunAt = &next
		} else {
			desiredState = "paused"
			loopState = "paused"
		}
	} else {
		desiredState = "paused"
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE loops
		SET desired_state = ?, state = ?, active_run_id = '', active_session_id = '',
		    last_event_sequence = ?, next_run_at = ?, last_error = ?, updated_at = ?
		WHERE id = ? AND active_run_id = ?
	`, desiredState, loopState, sequence, encodeOptionalTime(nextRunAt), failure,
		encodeTime(now), loopID, runID); err != nil {
		return LoopRecord{}, fmt.Errorf("finish loop: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return LoopRecord{}, fmt.Errorf("commit loop completion: %w", err)
	}
	record.DesiredState = desiredState
	record.State = loopState
	record.ActiveRunID = ""
	record.ActiveSessionID = ""
	record.LastEventSequence = sequence
	record.NextRunAt = nextRunAt
	record.LastError = failure
	record.UpdatedAt = now
	return record, nil
}

func (s *DB) LoopRuns(ctx context.Context, loopID string, limit int) ([]LoopRunRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, loop_id, iteration, session_id, idempotency_key, state,
		       last_event_sequence, error, started_at, finished_at
		FROM loop_runs WHERE loop_id = ? ORDER BY iteration DESC LIMIT ?
	`, loopID, limit)
	if err != nil {
		return nil, fmt.Errorf("query loop runs: %w", err)
	}
	defer rows.Close()
	var result []LoopRunRecord
	for rows.Next() {
		run, err := scanLoopRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loop runs: %w", err)
	}
	return result, nil
}

func (s *DB) recoverActiveLoops(ctx context.Context, now time.Time) error {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin loop recovery: %w", err)
	}
	defer transaction.Rollback()
	encodedNow := encodeTime(now.UTC())
	const reason = "agentd restarted during an active loop run"
	if _, err := transaction.ExecContext(ctx, `
		UPDATE loop_runs
		SET state = 'interrupted', error = ?, finished_at = ?
		WHERE state IN ('starting', 'running', 'blocked')
	`, reason, encodedNow); err != nil {
		return fmt.Errorf("interrupt stale loop runs: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE loops
		SET desired_state = 'paused', state = 'interrupted', active_run_id = '',
		    active_session_id = '', next_run_at = NULL, last_error = ?, updated_at = ?
		WHERE state IN ('starting', 'running', 'blocked')
	`, reason, encodedNow); err != nil {
		return fmt.Errorf("interrupt stale loops: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit loop recovery: %w", err)
	}
	return nil
}

func scanLoop(scanner rowScanner) (LoopRecord, error) {
	var record LoopRecord
	var nextRunAt, lastRunAt sql.NullString
	var createdAt, updatedAt string
	if err := scanner.Scan(
		&record.ID, &record.Name, &record.WorkspaceID, &record.HarnessID,
		&record.Prompt, &record.CadenceSeconds, &record.TimeoutSeconds,
		&record.DesiredState, &record.State, &record.Iteration, &record.RunRequested,
		&record.ActiveRunID, &record.ActiveSessionID, &record.LastEventSequence,
		&nextRunAt, &lastRunAt, &record.LastError, &createdAt, &updatedAt,
	); err != nil {
		return LoopRecord{}, err
	}
	var err error
	if record.NextRunAt, err = decodeOptionalTime(nextRunAt); err != nil {
		return LoopRecord{}, err
	}
	if record.LastRunAt, err = decodeOptionalTime(lastRunAt); err != nil {
		return LoopRecord{}, err
	}
	if record.CreatedAt, err = decodeTime(createdAt); err != nil {
		return LoopRecord{}, err
	}
	if record.UpdatedAt, err = decodeTime(updatedAt); err != nil {
		return LoopRecord{}, err
	}
	return record, nil
}

func scanLoopRun(scanner rowScanner) (LoopRunRecord, error) {
	var record LoopRunRecord
	var startedAt string
	var finishedAt sql.NullString
	if err := scanner.Scan(
		&record.ID, &record.LoopID, &record.Iteration, &record.SessionID,
		&record.IdempotencyKey, &record.State, &record.LastEventSequence,
		&record.Error, &startedAt, &finishedAt,
	); err != nil {
		return LoopRunRecord{}, err
	}
	var err error
	if record.StartedAt, err = decodeTime(startedAt); err != nil {
		return LoopRunRecord{}, err
	}
	if record.FinishedAt, err = decodeOptionalTime(finishedAt); err != nil {
		return LoopRunRecord{}, err
	}
	return record, nil
}

func encodeOptionalTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return encodeTime(*value)
}

func decodeOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	decoded, err := decodeTime(value.String)
	if err != nil {
		return nil, err
	}
	return &decoded, nil
}

func loopStateActive(state string) bool {
	return state == "starting" || state == "running" || state == "blocked"
}
