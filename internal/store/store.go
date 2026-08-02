package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrNotFound        = sql.ErrNoRows
	ErrActive          = errors.New("job has an active run")
	ErrRunRequested    = errors.New("job run is already requested")
	ErrManualSchedule  = errors.New("manual jobs cannot be started on a cadence")
	ErrStopping        = errors.New("job is stopping")
	ErrWorkspaceExists = errors.New("workspace id is already registered with different metadata")
)

type DB struct{ db *sql.DB }

type Job struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	WorkspaceID       string     `json:"workspaceId"`
	Runner            string     `json:"runner"`
	InvocationRequest string     `json:"invocationRequest"`
	CadenceSeconds    int        `json:"cadenceSeconds"`
	DesiredState      string     `json:"desiredState"`
	State             string     `json:"state"`
	Iteration         int64      `json:"iteration"`
	RunRequested      bool       `json:"runRequested"`
	ActiveRunID       string     `json:"activeRunId,omitempty"`
	NextRunAt         *time.Time `json:"nextRunAt,omitempty"`
	LastRunAt         *time.Time `json:"lastRunAt,omitempty"`
	LastError         string     `json:"lastError,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

type Run struct {
	ID                 string     `json:"id"`
	JobID              string     `json:"jobId"`
	Iteration          int64      `json:"iteration"`
	Runner             string     `json:"runner"`
	ExecutionReference string     `json:"executionReference,omitempty"`
	ProcessReference   string     `json:"processReference,omitempty"`
	State              string     `json:"state"`
	ExitCode           *int       `json:"exitCode,omitempty"`
	ExitSignal         string     `json:"exitSignal,omitempty"`
	Error              string     `json:"error,omitempty"`
	Stdout             string     `json:"stdout,omitempty"`
	Stderr             string     `json:"stderr,omitempty"`
	StartedAt          time.Time  `json:"startedAt"`
	FinishedAt         *time.Time `json:"finishedAt,omitempty"`
}

type rowScanner interface{ Scan(...any) error }

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
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	db := &DB{db: database}
	if err := db.migrate(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return db, nil
}

func OpenReadOnly(ctx context.Context, dataDir string) (*DB, error) {
	path := filepath.Join(dataDir, "agentd.sqlite")
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_foreign_keys=on&_busy_timeout=5000"
	database, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only SQLite: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("open read-only SQLite: %w", err)
	}
	return &DB{db: database}, nil
}
func (s *DB) Close() error { return s.db.Close() }
func (s *DB) SeedWorkspace(ctx context.Context, workspace config.Workspace) error {
	now := encodeTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspaces(id,name,path,created_at,updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,path=excluded.path,updated_at=excluded.updated_at`,
		workspace.ID, workspace.Name, workspace.Path, now, now)
	return err
}

func (s *DB) RegisterWorkspace(ctx context.Context, workspace config.Workspace) (config.Workspace, error) {
	now := encodeTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspaces(id,name,path,created_at,updated_at) VALUES(?,?,?,?,?)`,
		workspace.ID, workspace.Name, workspace.Path, now, now)
	if err == nil {
		return workspace, nil
	}
	existing, lookupErr := s.Workspace(ctx, workspace.ID)
	if lookupErr == nil {
		if existing.Name == workspace.Name && existing.Path == workspace.Path {
			return existing, nil
		}
		return config.Workspace{}, ErrWorkspaceExists
	}
	return config.Workspace{}, err
}

func (s *DB) Workspace(ctx context.Context, id string) (config.Workspace, error) {
	var workspace config.Workspace
	err := s.db.QueryRowContext(ctx, `SELECT id,name,path FROM workspaces WHERE id=?`, id).Scan(&workspace.ID, &workspace.Name, &workspace.Path)
	return workspace, err
}

func (s *DB) Workspaces(ctx context.Context) ([]config.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,path FROM workspaces ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []config.Workspace
	for rows.Next() {
		var workspace config.Workspace
		if err := rows.Scan(&workspace.ID, &workspace.Name, &workspace.Path); err != nil {
			return nil, err
		}
		out = append(out, workspace)
	}
	return out, rows.Err()
}

func (s *DB) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS workspaces (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            path TEXT NOT NULL,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS jobs (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            workspace_id TEXT NOT NULL,
            runner TEXT NOT NULL DEFAULT 'omp',
            invocation_request TEXT NOT NULL,
            cadence_seconds INTEGER NOT NULL DEFAULT 0,
            desired_state TEXT NOT NULL DEFAULT 'paused',
            state TEXT NOT NULL DEFAULT 'paused',
            iteration INTEGER NOT NULL DEFAULT 0,
            run_requested INTEGER NOT NULL DEFAULT 0,
            active_run_id TEXT NOT NULL DEFAULT '',
            next_run_at TEXT,
            last_run_at TEXT,
            last_error TEXT NOT NULL DEFAULT '',
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS runs (
            id TEXT PRIMARY KEY,
            job_id TEXT NOT NULL,
            iteration INTEGER NOT NULL,
            runner TEXT NOT NULL DEFAULT 'omp',
            execution_reference TEXT NOT NULL DEFAULT '',
            process_reference TEXT NOT NULL DEFAULT '',
            state TEXT NOT NULL,
            exit_code INTEGER,
            exit_signal TEXT NOT NULL DEFAULT '',
            error TEXT NOT NULL DEFAULT '',
            stdout TEXT NOT NULL DEFAULT '',
            stderr TEXT NOT NULL DEFAULT '',
            started_at TEXT NOT NULL,
            finished_at TEXT,
            UNIQUE(job_id, iteration),
            FOREIGN KEY(job_id) REFERENCES jobs(id) ON DELETE CASCADE
        );
        CREATE INDEX IF NOT EXISTS jobs_schedule ON jobs(state, run_requested, next_run_at);
        CREATE INDEX IF NOT EXISTS runs_job_iteration ON runs(job_id, iteration DESC);
    `); err != nil {
		return fmt.Errorf("create job/run schema: %w", err)
	}
	if err := migrateLegacy(ctx, tx); err != nil {
		return err
	}
	if err := dropLegacy(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

func tableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
	return count == 1, err
}
func columns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result[name] = true
	}
	return result, rows.Err()
}
func col(expr string, available map[string]bool, fallback string) string {
	if available[expr] {
		return expr
	}
	return fallback
}

func migrateLegacy(ctx context.Context, tx *sql.Tx) error {
	exists, err := tableExists(ctx, tx, "crons")
	if err != nil {
		return fmt.Errorf("inspect legacy jobs: %w", err)
	}
	if !exists {
		return nil
	}
	c, err := columns(ctx, tx, "crons")
	if err != nil {
		return fmt.Errorf("inspect legacy job columns: %w", err)
	}
	invocation := "''"
	if c["prompt"] {
		invocation = "prompt"
	} else if c["invocation_request"] {
		invocation = "invocation_request"
	}
	runner := "'omp'"
	if c["harness_id"] {
		runner = "COALESCE(NULLIF(harness_id,''),'omp')"
	} else if c["runner"] {
		runner = "COALESCE(NULLIF(runner,''),'omp')"
	}
	defaults := func(name, fallback string) string { return col(name, c, fallback) }
	query := fmt.Sprintf(`INSERT OR IGNORE INTO jobs(
            id,name,workspace_id,runner,invocation_request,cadence_seconds,desired_state,state,iteration,run_requested,active_run_id,next_run_at,last_run_at,last_error,created_at,updated_at)
        SELECT id,name,workspace_id,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s FROM crons`, runner, invocation,
		defaults("cadence_seconds", "0"), defaults("desired_state", "'paused'"), defaults("state", "'paused'"), defaults("iteration", "0"), defaults("run_requested", "0"), defaults("active_run_id", "''"), defaults("next_run_at", "NULL"), defaults("last_run_at", "NULL"), defaults("last_error", "''"), defaults("created_at", "CURRENT_TIMESTAMP"), defaults("updated_at", "CURRENT_TIMESTAMP"))
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("migrate crons to jobs: %w", err)
	}
	runsExist, err := tableExists(ctx, tx, "cron_runs")
	if err != nil {
		return fmt.Errorf("inspect legacy runs: %w", err)
	}
	if !runsExist {
		return nil
	}
	rc, err := columns(ctx, tx, "cron_runs")
	if err != nil {
		return fmt.Errorf("inspect legacy run columns: %w", err)
	}
	jobID := "cron_id"
	if !rc["cron_id"] && rc["job_id"] {
		jobID = "job_id"
	}
	execution := "''"
	if rc["session_id"] {
		execution = "COALESCE(session_id,'')"
	} else if rc["execution_reference"] {
		execution = "COALESCE(execution_reference,'')"
	}
	query = fmt.Sprintf(`INSERT OR IGNORE INTO runs(id,job_id,iteration,runner,execution_reference,process_reference,state,exit_code,exit_signal,error,stdout,stderr,started_at,finished_at)
        SELECT r.id,r.%s,r.iteration,j.runner,%s,'',r.state,NULL,'',COALESCE(r.error,''),'','',r.started_at,r.finished_at
        FROM cron_runs r JOIN jobs j ON j.id=r.%s`, jobID, execution, jobID)
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("migrate cron runs to runs: %w", err)
	}
	return nil
}

func dropLegacy(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{"events", "inputs", "push_subscriptions", "enrollment_tokens", "sessions", "devices", "nodes", "automation", "loop_step_runs", "loop_edges", "loop_steps", "loop_executions", "loop_runs", "loops", "cron_runs", "crons"} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			return fmt.Errorf("drop obsolete table %s: %w", table, err)
		}
	}
	for _, index := range []string{"events_session_sequence", "crons_schedule", "cron_runs_cron_iteration", "loops_schedule", "loop_runs_loop_iteration"} {
		_, _ = tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+index)
	}
	return nil
}

func (s *DB) RecoverInterrupted(ctx context.Context, now time.Time) error {
	stamp := encodeTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin restart recovery: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET state='interrupted', error=CASE WHEN error='' THEN 'agentd restarted during active run' ELSE error END, finished_at=? WHERE finished_at IS NULL AND state IN ('starting','running','stopping')`, stamp); err != nil {
		return fmt.Errorf("interrupt active runs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET desired_state='paused', state='interrupted', active_run_id='', run_requested=0, next_run_at=NULL, last_error=CASE WHEN last_error='' THEN 'agentd restarted during active run' ELSE last_error END, updated_at=? WHERE active_run_id <> '' OR state IN ('starting','running','stopping')`, stamp); err != nil {
		return fmt.Errorf("interrupt active jobs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit restart recovery: %w", err)
	}
	return nil
}

func encodeTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func encodeOptionalTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return encodeTime(*t)
}
func decodeTime(value string) time.Time { t, _ := time.Parse(time.RFC3339Nano, value); return t }
func decodeOptionalTime(value sql.NullString) *time.Time {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	t := decodeTime(value.String)
	return &t
}

const jobColumns = `id,name,workspace_id,runner,invocation_request,cadence_seconds,desired_state,state,iteration,run_requested,active_run_id,next_run_at,last_run_at,last_error,created_at,updated_at`

func scanJob(row rowScanner) (Job, error) {
	var j Job
	var next, last sql.NullString
	var created, updated string
	err := row.Scan(&j.ID, &j.Name, &j.WorkspaceID, &j.Runner, &j.InvocationRequest, &j.CadenceSeconds, &j.DesiredState, &j.State, &j.Iteration, &j.RunRequested, &j.ActiveRunID, &next, &last, &j.LastError, &created, &updated)
	j.NextRunAt = decodeOptionalTime(next)
	j.LastRunAt = decodeOptionalTime(last)
	j.CreatedAt = decodeTime(created)
	j.UpdatedAt = decodeTime(updated)
	return j, err
}
func (s *DB) Job(ctx context.Context, id string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?`, id))
}
func (s *DB) Jobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, e := scanJob(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *DB) CreateJob(ctx context.Context, j Job) (Job, error) {
	now := time.Now().UTC()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now
	}
	if j.UpdatedAt.IsZero() {
		j.UpdatedAt = j.CreatedAt
	}
	if j.DesiredState == "" {
		j.DesiredState = "paused"
	}
	if j.State == "" {
		j.State = "paused"
	}
	if j.Runner == "" {
		j.Runner = "omp"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,name,workspace_id,runner,invocation_request,cadence_seconds,desired_state,state,iteration,run_requested,active_run_id,next_run_at,last_run_at,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.Name, j.WorkspaceID, j.Runner, j.InvocationRequest, j.CadenceSeconds, j.DesiredState, j.State, j.Iteration, j.RunRequested, j.ActiveRunID, encodeOptionalTime(j.NextRunAt), encodeOptionalTime(j.LastRunAt), j.LastError, encodeTime(j.CreatedAt), encodeTime(j.UpdatedAt))
	if err != nil {
		return Job{}, fmt.Errorf("insert job: %w", err)
	}
	return j, nil
}
func (s *DB) UpdateJob(ctx context.Context, j Job) (Job, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET name=?,workspace_id=?,runner=?,invocation_request=?,cadence_seconds=?,updated_at=? WHERE id=? AND state NOT IN ('starting','running','stopping')`, j.Name, j.WorkspaceID, j.Runner, j.InvocationRequest, j.CadenceSeconds, encodeTime(now), j.ID)
	if err != nil {
		return Job{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		cur, e := s.Job(ctx, j.ID)
		if e != nil {
			return Job{}, e
		}
		if cur.ActiveRunID != "" {
			return Job{}, ErrActive
		}
		return Job{}, errors.New("job cannot be updated")
	}
	return s.Job(ctx, j.ID)
}
func (s *DB) StartJob(ctx context.Context, id string, now time.Time) (Job, error) {
	j, e := s.Job(ctx, id)
	if e != nil {
		return Job{}, e
	}
	if j.CadenceSeconds <= 0 {
		return Job{}, ErrManualSchedule
	}
	if j.State == "stopping" || j.ActiveRunID != "" {
		return Job{}, ErrStopping
	}
	now = now.UTC()
	_, e = s.db.ExecContext(ctx, `UPDATE jobs SET desired_state='running',state=CASE WHEN state IN ('starting','running') THEN state ELSE 'scheduled' END,next_run_at=CASE WHEN state IN ('starting','running') THEN next_run_at ELSE ? END,last_error='',updated_at=? WHERE id=?`, encodeTime(now), encodeTime(now), id)
	if e != nil {
		return Job{}, e
	}
	return s.Job(ctx, id)
}
func (s *DB) PauseJob(ctx context.Context, id string, now time.Time) (Job, error) {
	now = now.UTC()
	res, e := s.db.ExecContext(ctx, `UPDATE jobs SET desired_state='paused',run_requested=0,state=CASE WHEN state IN ('starting','running','stopping') THEN state ELSE 'paused' END,next_run_at=CASE WHEN state IN ('starting','running','stopping') THEN next_run_at ELSE NULL END,updated_at=? WHERE id=?`, encodeTime(now), id)
	if e != nil {
		return Job{}, e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Job{}, ErrNotFound
	}
	return s.Job(ctx, id)
}
func (s *DB) RequestRun(ctx context.Context, id string, now time.Time) (Job, error) {
	j, e := s.Job(ctx, id)
	if e != nil {
		return Job{}, e
	}
	if j.ActiveRunID != "" || j.State == "starting" || j.State == "running" || j.State == "stopping" {
		return Job{}, ErrActive
	}
	if j.RunRequested {
		return Job{}, ErrRunRequested
	}
	_, e = s.db.ExecContext(ctx, `UPDATE jobs SET run_requested=1,state='scheduled',last_error='',updated_at=? WHERE id=?`, encodeTime(now.UTC()), id)
	if e != nil {
		return Job{}, e
	}
	return s.Job(ctx, id)
}
func (s *DB) StopJob(ctx context.Context, id string, now time.Time) (Job, error) {
	now = now.UTC()
	res, e := s.db.ExecContext(ctx, `UPDATE jobs SET desired_state='paused',run_requested=0,state=CASE WHEN active_run_id<>'' OR state IN ('starting','running','stopping') THEN 'stopping' ELSE 'stopped' END,next_run_at=NULL,updated_at=? WHERE id=?`, encodeTime(now), id)
	if e != nil {
		return Job{}, e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Job{}, ErrNotFound
	}
	return s.Job(ctx, id)
}

func (s *DB) ClaimNext(ctx context.Context, now time.Time, runID string) (Job, Run, bool, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Job{}, Run{}, false, e
	}
	defer tx.Rollback()
	now = now.UTC()
	j, e := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE state='scheduled' AND (run_requested=1 OR (desired_state='running' AND cadence_seconds>0 AND next_run_at IS NOT NULL AND next_run_at<=?)) AND active_run_id='' ORDER BY run_requested DESC,next_run_at,created_at LIMIT 1`, encodeTime(now)))
	if errors.Is(e, sql.ErrNoRows) {
		return Job{}, Run{}, false, nil
	}
	if e != nil {
		return Job{}, Run{}, false, e
	}
	iteration := j.Iteration + 1
	res, e := tx.ExecContext(ctx, `UPDATE jobs SET state='starting',iteration=?,run_requested=0,active_run_id=?,last_run_at=?,updated_at=? WHERE id=? AND state='scheduled' AND active_run_id=''`, iteration, runID, encodeTime(now), encodeTime(now), j.ID)
	if e != nil {
		return Job{}, Run{}, false, e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Job{}, Run{}, false, nil
	}
	r := Run{ID: runID, JobID: j.ID, Iteration: iteration, Runner: j.Runner, State: "starting", StartedAt: now}
	if _, e = tx.ExecContext(ctx, `INSERT INTO runs(id,job_id,iteration,runner,state,started_at) VALUES(?,?,?,?,?,?)`, r.ID, r.JobID, r.Iteration, r.Runner, r.State, encodeTime(r.StartedAt)); e != nil {
		return Job{}, Run{}, false, e
	}
	if e = tx.Commit(); e != nil {
		return Job{}, Run{}, false, e
	}
	j.Iteration = iteration
	j.RunRequested = false
	j.State = "starting"
	j.ActiveRunID = runID
	j.LastRunAt = &now
	j.UpdatedAt = now
	return j, r, true, nil
}

func (s *DB) SetRunRunning(ctx context.Context, jobID, runID, execution, process string) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, `UPDATE runs SET execution_reference=?,process_reference=? WHERE id=? AND job_id=?`, execution, process, runID, jobID)
	if e != nil {
		return e
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	res, e = tx.ExecContext(ctx, `UPDATE jobs SET state='running',updated_at=? WHERE id=? AND active_run_id=? AND state<>'stopping'`, encodeTime(time.Now().UTC()), jobID, runID)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var state string
		if e = tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=? AND active_run_id=?`, jobID, runID).Scan(&state); e != nil {
			return e
		}
		if state != "stopping" {
			return sql.ErrNoRows
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		return ErrStopping
	}
	if _, e = tx.ExecContext(ctx, `UPDATE runs SET state='running' WHERE id=? AND job_id=?`, runID, jobID); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *DB) SetRunEvidence(ctx context.Context, jobID, runID, stdout, stderr string) error {
	_, e := s.db.ExecContext(ctx, `UPDATE runs SET stdout=?,stderr=? WHERE id=? AND job_id=?`, stdout, stderr, runID, jobID)
	return e
}
func (s *DB) SetStopFailure(ctx context.Context, jobID, runID, message string) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	stamp := encodeTime(time.Now().UTC())
	if _, e = tx.ExecContext(ctx, `UPDATE jobs SET state='stopping',last_error=?,updated_at=? WHERE id=? AND active_run_id=?`, message, stamp, jobID, runID); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE runs SET state='stopping',error=? WHERE id=? AND job_id=?`, message, runID, jobID); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *DB) FinishRun(ctx context.Context, jobID, runID, state string, exitCode *int, signal, message, stdout, stderr string, finished time.Time, terminationConfirmed bool) (Job, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Job{}, e
	}
	defer tx.Rollback()
	j, e := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?`, jobID))
	if e != nil {
		return Job{}, e
	}
	if j.ActiveRunID != runID {
		return j, nil
	}
	if j.State == "stopping" && !terminationConfirmed {
		return j, ErrStopping
	}
	if j.State == "stopping" {
		state = "stopped"
		message = ""
	}
	finished = finished.UTC()
	if _, e = tx.ExecContext(ctx, `UPDATE runs SET state=?,exit_code=?,exit_signal=?,error=?,stdout=?,stderr=?,finished_at=? WHERE id=? AND job_id=?`, state, exitCode, signal, message, stdout, stderr, encodeTime(finished), runID, jobID); e != nil {
		return Job{}, e
	}
	desired := j.DesiredState
	jobState := state
	var next *time.Time
	if state == "completed" && desired == "running" && j.CadenceSeconds > 0 {
		jobState = "scheduled"
		n := finished.Add(time.Duration(j.CadenceSeconds) * time.Second)
		next = &n
	} else {
		desired = "paused"
		if state == "completed" {
			jobState = "paused"
		}
	}
	if _, e = tx.ExecContext(ctx, `UPDATE jobs SET desired_state=?,state=?,active_run_id='',next_run_at=?,last_error=?,updated_at=? WHERE id=? AND active_run_id=?`, desired, jobState, encodeOptionalTime(next), message, encodeTime(finished), jobID, runID); e != nil {
		return Job{}, e
	}
	if e = tx.Commit(); e != nil {
		return Job{}, e
	}
	j.DesiredState = desired
	j.State = jobState
	j.ActiveRunID = ""
	j.NextRunAt = next
	j.LastError = message
	j.UpdatedAt = finished
	return j, nil
}
func (s *DB) Runs(ctx context.Context, jobID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, e := s.db.QueryContext(ctx, `SELECT id,job_id,iteration,runner,execution_reference,process_reference,state,exit_code,exit_signal,error,stdout,stderr,started_at,finished_at FROM runs WHERE job_id=? ORDER BY iteration DESC LIMIT ?`, jobID, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, e := scanRun(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func scanRun(row rowScanner) (Run, error) {
	var r Run
	var exit sql.NullInt64
	var finish sql.NullString
	var started string
	e := row.Scan(&r.ID, &r.JobID, &r.Iteration, &r.Runner, &r.ExecutionReference, &r.ProcessReference, &r.State, &exit, &r.ExitSignal, &r.Error, &r.Stdout, &r.Stderr, &started, &finish)
	if exit.Valid {
		v := int(exit.Int64)
		r.ExitCode = &v
	}
	r.StartedAt = decodeTime(started)
	r.FinishedAt = decodeOptionalTime(finish)
	return r, e
}
