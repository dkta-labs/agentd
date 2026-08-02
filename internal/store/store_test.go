package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	_ "github.com/mattn/go-sqlite3"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createManualJob(t *testing.T, db *DB, id string) Job {
	t.Helper()
	job, err := db.CreateJob(context.Background(), Job{
		ID: id, Name: id, WorkspaceID: "workspace", Runner: "test",
		InvocationRequest: "do one thing", CadenceSeconds: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func claimManualRun(t *testing.T, db *DB, jobID, runID string, now time.Time) (Job, Run) {
	t.Helper()
	if _, err := db.RequestRun(context.Background(), jobID, now); err != nil {
		t.Fatal(err)
	}
	job, run, claimed, err := db.ClaimNext(context.Background(), now, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("run was not claimed")
	}
	return job, run
}

func TestManualRunPersistsEvidenceAndSuccessfulExit(t *testing.T) {
	db := openTestDB(t)
	job := createManualJob(t, db, "job-one")
	if job.State != "paused" || job.DesiredState != "paused" {
		t.Fatalf("new job = state %q desired %q", job.State, job.DesiredState)
	}
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	_, run := claimManualRun(t, db, job.ID, "run-one", now)
	if err := db.SetRunRunning(context.Background(), job.ID, run.ID, "session/path.jsonl", "pid=42 pgid=42"); err != nil {
		t.Fatal(err)
	}
	code := 0
	finished, err := db.FinishRun(context.Background(), job.ID, run.ID, "completed", &code, "", "", "stdout", "stderr", now.Add(time.Second), true)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != "paused" || finished.ActiveRunID != "" {
		t.Fatalf("finished job = state %q active %q", finished.State, finished.ActiveRunID)
	}
	runs, err := db.Runs(context.Background(), job.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	got := runs[0]
	if got.State != "completed" || got.ExitCode == nil || *got.ExitCode != 0 || got.ExecutionReference != "session/path.jsonl" || got.ProcessReference != "pid=42 pgid=42" || got.Stdout != "stdout" || got.Stderr != "stderr" || got.FinishedAt == nil {
		t.Fatalf("run evidence = %#v", got)
	}
}

func TestSameJobExcludedAndUnrelatedJobsRemainClaimable(t *testing.T) {
	db := openTestDB(t)
	first := createManualJob(t, db, "first")
	second := createManualJob(t, db, "second")
	now := time.Now().UTC()
	_, firstRun := claimManualRun(t, db, first.ID, "run-first", now)
	if _, err := db.RequestRun(context.Background(), first.ID, now); !errors.Is(err, ErrActive) {
		t.Fatalf("same-job request error = %v", err)
	}
	if _, err := db.RequestRun(context.Background(), second.ID, now); err != nil {
		t.Fatal(err)
	}
	claimedJob, _, claimed, err := db.ClaimNext(context.Background(), now, "run-second")
	if err != nil {
		t.Fatal(err)
	}
	if !claimed || claimedJob.ID != second.ID {
		t.Fatalf("unrelated claim = claimed %v job %q", claimed, claimedJob.ID)
	}
	if firstRun.JobID != first.ID {
		t.Fatalf("first run belongs to %q", firstRun.JobID)
	}
}

func TestFailureAndStopConfirmationOwnActiveRun(t *testing.T) {
	db := openTestDB(t)
	job := createManualJob(t, db, "stop-job")
	now := time.Now().UTC()
	_, run := claimManualRun(t, db, job.ID, "run-stop", now)
	if err := db.SetRunRunning(context.Background(), job.ID, run.ID, "execution", "pid=50 pgid=50"); err != nil {
		t.Fatal(err)
	}
	stopping, err := db.StopJob(context.Background(), job.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if stopping.State != "stopping" || stopping.ActiveRunID != run.ID {
		t.Fatalf("stopping job = %#v", stopping)
	}
	if _, err := db.FinishRun(context.Background(), job.ID, run.ID, "failed", nil, "", "unconfirmed", "", "", now.Add(2*time.Second), false); !errors.Is(err, ErrStopping) {
		t.Fatalf("unconfirmed finish error = %v", err)
	}
	if err := db.SetStopFailure(context.Background(), job.ID, run.ID, "termination failed"); err != nil {
		t.Fatal(err)
	}
	stillOwned, err := db.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillOwned.ActiveRunID != run.ID || stillOwned.State != "stopping" || stillOwned.LastError != "termination failed" {
		t.Fatalf("failed stop ownership = %#v", stillOwned)
	}
	stopped, err := db.FinishRun(context.Background(), job.ID, run.ID, "failed", nil, "terminated", "ignored", "out", "err", now.Add(3*time.Second), true)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "stopped" || stopped.ActiveRunID != "" || stopped.LastError != "" {
		t.Fatalf("confirmed stop = %#v", stopped)
	}
}

func TestRestartRecoveryIsExplicitAndDoesNotRunOnAdministrativeOpen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	job := createManualJob(t, db, "restart-job")
	now := time.Now().UTC()
	_, run := claimManualRun(t, db, job.ID, "restart-run", now)
	if err := db.SetRunRunning(ctx, job.ID, run.ID, "execution", "process"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	admin, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	before, err := admin.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != "running" || before.ActiveRunID != run.ID {
		t.Fatalf("administrative open mutated active job: %#v", before)
	}
	if err := admin.RecoverInterrupted(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, err := admin.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "interrupted" || after.ActiveRunID != "" || after.DesiredState != "paused" {
		t.Fatalf("recovered job = %#v", after)
	}
	runs, err := admin.Runs(ctx, job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != "interrupted" || runs[0].FinishedAt == nil {
		t.Fatalf("recovered runs = %#v", runs)
	}
}

func TestLegacyMigrationPreservesJobsAndRunsThenDropsObsoleteTables(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "agentd.sqlite")
	legacy, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE sessions(id TEXT PRIMARY KEY);
		CREATE TABLE events(id INTEGER PRIMARY KEY, session_id TEXT REFERENCES sessions(id));
		CREATE TABLE inputs(id INTEGER PRIMARY KEY, session_id TEXT REFERENCES sessions(id));
		CREATE TABLE devices(id TEXT PRIMARY KEY);
		CREATE TABLE push_subscriptions(id INTEGER PRIMARY KEY, device_id TEXT REFERENCES devices(id));
		CREATE TABLE enrollment_tokens(id TEXT PRIMARY KEY, device_id TEXT REFERENCES devices(id));
		CREATE TABLE loops(id TEXT PRIMARY KEY, objective TEXT);
		CREATE TABLE loop_runs(id TEXT PRIMARY KEY, loop_id TEXT REFERENCES loops(id));
		CREATE TABLE crons(
			id TEXT PRIMARY KEY, name TEXT, workspace_id TEXT, harness_id TEXT, prompt TEXT,
			cadence_seconds INTEGER, desired_state TEXT, state TEXT, iteration INTEGER,
			run_requested INTEGER, active_run_id TEXT, next_run_at TEXT, last_run_at TEXT,
			last_error TEXT, created_at TEXT, updated_at TEXT
		);
		CREATE TABLE cron_runs(
			id TEXT PRIMARY KEY, cron_id TEXT REFERENCES crons(id), iteration INTEGER,
			session_id TEXT, state TEXT, error TEXT, started_at TEXT, finished_at TEXT
		);
		INSERT INTO sessions VALUES('session-1');
		INSERT INTO events VALUES(1,'session-1');
		INSERT INTO inputs VALUES(1,'session-1');
		INSERT INTO devices VALUES('device-1');
		INSERT INTO push_subscriptions VALUES(1,'device-1');
		INSERT INTO enrollment_tokens VALUES('token-1','device-1');
		INSERT INTO loops VALUES('obsolete-loop','judge an objective');
		INSERT INTO loop_runs VALUES('obsolete-loop-run','obsolete-loop');
		INSERT INTO crons VALUES('legacy-job','Legacy','workspace','omp','legacy request',60,'paused','paused',1,0,'',NULL,NULL,'','2026-07-30T12:00:00Z','2026-07-30T12:00:00Z');
		INSERT INTO cron_runs VALUES('legacy-run','legacy-job',1,'legacy-session','completed','', '2026-07-30T12:00:00Z','2026-07-30T12:01:00Z');
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	job, err := migrated.Job(ctx, "legacy-job")
	if err != nil {
		t.Fatal(err)
	}
	if job.InvocationRequest != "legacy request" || job.Runner != "omp" {
		t.Fatalf("migrated job = %#v", job)
	}
	runs, err := migrated.Runs(ctx, job.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ExecutionReference != "legacy-session" || runs[0].State != "completed" {
		t.Fatalf("migrated runs = %#v", runs)
	}
	rows, err := migrated.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if len(tables) != 3 || tables[0] != "jobs" || tables[1] != "runs" || tables[2] != "workspaces" {
		t.Fatalf("remaining tables = %v", tables)
	}
}

func TestRegisterWorkspaceIsIdempotentButRejectsConflictingMetadata(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	workspace := config.Workspace{ID: "workspace-one", Name: "One", Path: filepath.Join(t.TempDir(), "one")}
	first, err := db.RegisterWorkspace(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.RegisterWorkspace(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if first != workspace || second != workspace {
		t.Fatalf("registrations = %#v %#v", first, second)
	}
	conflict := workspace
	conflict.Path = filepath.Join(t.TempDir(), "other")
	if _, err := db.RegisterWorkspace(ctx, conflict); !errors.Is(err, ErrWorkspaceExists) {
		t.Fatalf("conflict error = %v", err)
	}
	stored, err := db.Workspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored != workspace {
		t.Fatalf("stored workspace = %#v, want %#v", stored, workspace)
	}
}
