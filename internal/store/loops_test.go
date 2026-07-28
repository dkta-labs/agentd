package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestActiveLoopRunIsInterruptedAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	created, err := database.CreateLoop(ctx, LoopRecord{
		ID: "loop_recovery", Name: "Recovery", WorkspaceID: "workspace",
		HarnessID: "omp", Prompt: "do one thing", CadenceSeconds: 300,
		TimeoutSeconds: 900, DesiredState: "paused", State: "paused",
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "paused" {
		t.Fatalf("created state = %q, want paused", created.State)
	}
	if _, err := database.StartLoop(ctx, created.ID, now); err != nil {
		t.Fatal(err)
	}
	loop, run, claimed, err := database.ClaimNextLoop(ctx, now, "run_recovery", "ses_loop_recovery")
	if err != nil {
		t.Fatal(err)
	}
	if !claimed || loop.State != "starting" || run.Iteration != 1 {
		t.Fatalf("unexpected claim: loop=%#v run=%#v claimed=%t", loop, run, claimed)
	}
	if err := database.SetLoopRunState(ctx, loop.ID, run.ID, "blocked", 7); err != nil {
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
	recovered, err := database.Loop(ctx, loop.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != "interrupted" || recovered.DesiredState != "paused" {
		t.Fatalf("recovered loop = %#v", recovered)
	}
	if recovered.ActiveRunID != "" || recovered.ActiveSessionID != "" || recovered.NextRunAt != nil {
		t.Fatalf("recovered loop retained active scheduling state: %#v", recovered)
	}
	if !strings.Contains(recovered.LastError, "restarted") {
		t.Fatalf("recovery error = %q", recovered.LastError)
	}
	runs, err := database.LoopRuns(ctx, loop.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != "interrupted" || runs[0].FinishedAt == nil {
		t.Fatalf("recovered runs = %#v", runs)
	}
}

func TestPausingScheduledManualRunCancelsRequest(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	created, err := database.CreateLoop(ctx, LoopRecord{
		ID: "loop_manual", Name: "Manual", WorkspaceID: "workspace",
		HarnessID: "omp", Prompt: "do one thing", CadenceSeconds: 0,
		TimeoutSeconds: 900, DesiredState: "paused", State: "paused",
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	requested, err := database.RequestLoopRun(ctx, created.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if requested.State != "scheduled" || !requested.RunRequested {
		t.Fatalf("requested loop = %#v", requested)
	}
	paused, err := database.PauseLoop(ctx, created.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != "paused" || paused.DesiredState != "paused" || paused.RunRequested {
		t.Fatalf("paused loop retained request: %#v", paused)
	}
	requestedAgain, err := database.RequestLoopRun(ctx, created.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("request after pause: %v", err)
	}
	if requestedAgain.State != "scheduled" || !requestedAgain.RunRequested {
		t.Fatalf("requested loop after pause = %#v", requestedAgain)
	}
}

func TestRestartingPausedActiveRecurringLoopSchedulesFromCompletion(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	created, err := database.CreateLoop(ctx, LoopRecord{
		ID: "loop_cadence", Name: "Cadence", WorkspaceID: "workspace",
		HarnessID: "omp", Prompt: "do one thing", CadenceSeconds: 600,
		TimeoutSeconds: 900, DesiredState: "paused", State: "paused",
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartLoop(ctx, created.ID, now); err != nil {
		t.Fatal(err)
	}
	loop, run, claimed, err := database.ClaimNextLoop(ctx, now, "run_cadence", "ses_loop_cadence")
	if err != nil || !claimed {
		t.Fatalf("claim error=%v claimed=%t", err, claimed)
	}
	if loop.NextRunAt == nil {
		t.Fatalf("claimed loop has no scheduling time: %#v", loop)
	}
	originalNextRunAt := *loop.NextRunAt
	paused, err := database.PauseLoop(ctx, loop.ID, now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != "starting" || paused.DesiredState != "paused" {
		t.Fatalf("paused active loop = %#v", paused)
	}
	restarted, err := database.StartLoop(ctx, loop.ID, now.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if restarted.State != "starting" || restarted.DesiredState != "running" {
		t.Fatalf("restarted active loop = %#v", restarted)
	}
	if restarted.NextRunAt == nil || !restarted.NextRunAt.Equal(originalNextRunAt) {
		t.Fatalf("restarted next run = %v, want %s", restarted.NextRunAt, originalNextRunAt)
	}
	completedAt := now.Add(42 * time.Second)
	completed, err := database.FinishLoopRun(ctx, loop.ID, run.ID, "completed", 11, "", completedAt)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != "scheduled" || completed.DesiredState != "running" || completed.NextRunAt == nil {
		t.Fatalf("completed loop = %#v", completed)
	}
	wantNext := completedAt.Add(600 * time.Second)
	if !completed.NextRunAt.Equal(wantNext) {
		t.Fatalf("next run = %s, want %s", completed.NextRunAt, wantNext)
	}
}
