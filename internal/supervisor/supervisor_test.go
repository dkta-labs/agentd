package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/runner"
	"github.com/dkta-labs/agentd/internal/store"
)

type testWorkspaces struct{ path string }

func (w testWorkspaces) Workspace(_ context.Context, id string) (config.Workspace, error) {
	switch id {
	case "workspace":
		return config.Workspace{ID: id, Name: id, Path: w.path}, nil
	case "workspace-two":
		return config.Workspace{ID: id, Name: id, Path: w.path + "-two"}, nil
	default:
		return config.Workspace{}, errors.New("workspace not found")
	}
}

type testResolver struct{ runner runner.Runner }

func (r testResolver) Runner(id string) (runner.Runner, error) {
	if id != "test" {
		return nil, errors.New("runner not found")
	}
	return r.runner, nil
}

type testRunner struct {
	mu           sync.Mutex
	started      chan *testProcess
	startEntered chan struct{}
	startRelease chan struct{}
	latest       *testProcess
	attachCount  int
}

func (r *testRunner) Start(_ context.Context, job runner.Job) (runner.Process, error) {
	if r.startEntered != nil {
		close(r.startEntered)
		<-r.startRelease
	}
	process := &testProcess{
		job: job, done: make(chan struct{}), execution: "execution/" + job.RunID,
		process: "herdr-agent-" + job.RunID, stdout: "stdout", stderr: "stderr",
	}
	r.mu.Lock()
	r.latest = process
	r.mu.Unlock()
	r.started <- process
	return process, nil
}

func (r *testRunner) Attach(_ context.Context, _ runner.Job, reference string) (runner.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latest == nil || r.latest.process != reference {
		return nil, runner.ErrOwnerUnavailable
	}
	r.attachCount++
	return r.latest, nil
}

type preparedTestRunner struct {
	mu             sync.Mutex
	prepared       *preparedTestProcess
	prepareEntered chan struct{}
	prepareRelease chan struct{}
	created        chan *testProcess
	dispatchCalls  int
	persisted      func() bool
}

type preparedTestProcess struct {
	process  *testProcess
	dispatch func(context.Context) error
}

func (p *preparedTestProcess) Process() runner.Process { return p.process }

func (p *preparedTestProcess) Dispatch(ctx context.Context) error {
	if p.dispatch == nil {
		return nil
	}
	return p.dispatch(ctx)
}

func (r *preparedTestRunner) Start(_ context.Context, job runner.Job) (runner.Process, error) {
	return r.newProcess(job), nil
}

func (r *preparedTestRunner) Prepare(_ context.Context, job runner.Job) (runner.Prepared, error) {
	if r.prepareEntered != nil {
		close(r.prepareEntered)
		<-r.prepareRelease
	}
	process := r.newProcess(job)
	return &preparedTestProcess{
		process: process,
		dispatch: func(context.Context) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.persisted != nil && !r.persisted() {
				return errors.New("prepared ownership was not persisted")
			}
			r.dispatchCalls++
			return nil
		},
	}, nil
}
func (p *preparedTestProcess) PreparedSequence() uint64 {
	return p.process.PreparedSequence()
}

func (r *preparedTestRunner) newProcess(job runner.Job) *testProcess {
	process := &testProcess{
		job: job, done: make(chan struct{}), execution: "execution/" + job.RunID,
		process: "prepared-agent-" + job.RunID, preparedSequence: 17, stdout: "stdout", stderr: "stderr",
	}
	r.mu.Lock()
	r.prepared = &preparedTestProcess{process: process}
	r.mu.Unlock()
	if r.created != nil {
		r.created <- process
	}
	return process
}

func (r *preparedTestRunner) Attach(_ context.Context, _ runner.Job, reference string) (runner.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prepared == nil || r.prepared.process.process != reference {
		return nil, runner.ErrOwnerUnavailable
	}
	return r.prepared.process, nil
}
func (r *preparedTestRunner) AttachPrepared(_ context.Context, _ runner.Job, reference string, sequence uint64) (runner.Prepared, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prepared == nil || r.prepared.process.process != reference {
		return nil, runner.ErrOwnerUnavailable
	}
	r.prepared.process.preparedSequence = sequence
	return &preparedTestProcess{
		process: r.prepared.process,
		dispatch: func(context.Context) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.dispatchCalls++
			return nil
		},
	}, nil
}

type testProcess struct {
	job                runner.Job
	done               chan struct{}
	doneOnce           sync.Once
	mu                 sync.Mutex
	exit               runner.Exit
	waitErr            error
	stopErrors         []error
	stopCalls          int
	execution, process string
	preparedSequence   uint64
	stdout, stderr     string
}

func (p *testProcess) Wait(ctx context.Context) (runner.Exit, error) {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.exit, p.waitErr
	case <-ctx.Done():
		return runner.Exit{}, ctx.Err()
	}
}
func (p *testProcess) Stop(context.Context) error {
	p.mu.Lock()
	p.stopCalls++
	if len(p.stopErrors) > 0 {
		err := p.stopErrors[0]
		p.stopErrors = p.stopErrors[1:]
		p.mu.Unlock()
		if err != nil {
			return err
		}
	} else {
		p.mu.Unlock()
	}
	p.complete(runner.Exit{Code: -1, Signal: "terminated"})
	return nil
}
func (p *testProcess) complete(exit runner.Exit) {
	p.mu.Lock()
	p.exit = exit
	p.mu.Unlock()
	p.doneOnce.Do(func() { close(p.done) })
}
func (p *testProcess) ExecutionReference() string { return p.execution }
func (p *testProcess) ProcessReference() string   { return p.process }
func (p *testProcess) Output() (string, string)   { return p.stdout, p.stderr }
func (p *testProcess) PreparedSequence() uint64   { return p.preparedSequence }

func newTestSupervisor(t *testing.T) (*Supervisor, *testRunner) {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fake := &testRunner{started: make(chan *testProcess, 10)}
	sup, err := New(db, testWorkspaces{path: t.TempDir()}, testResolver{runner: fake}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	sup.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = sup.Close(ctx)
	})
	return sup, fake
}

func newPreparedSupervisor(t *testing.T, fake *preparedTestRunner) (*Supervisor, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sup, err := New(db, testWorkspaces{path: t.TempDir()}, testResolver{runner: fake}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	sup.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = sup.Close(ctx)
	})
	return sup, db
}

func createJob(t *testing.T, sup *Supervisor, name string, cadence int) store.Job {
	t.Helper()
	job, err := sup.Create(context.Background(), CreateRequest{
		Name: name, WorkspaceID: "workspace", Runner: "test",
		InvocationRequest: "do one bounded thing", CadenceSeconds: cadence,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job
}
func TestGoalKeyValidationAndRunnerThreading(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	job, err := sup.Create(context.Background(), CreateRequest{
		Name: "goal", WorkspaceID: "workspace", Runner: "test",
		InvocationRequest: "do one bounded thing", GoalKey: "  goal-42  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.GoalKey != "goal-42" {
		t.Fatalf("created goal key = %q", job.GoalKey)
	}
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.started)
	if process.job.GoalKey != "goal-42" {
		t.Fatalf("runner goal key = %q", process.job.GoalKey)
	}
	process.complete(runner.Exit{Code: 0})
	waitState(t, sup, job.ID, "paused")

	_, err = sup.Create(context.Background(), CreateRequest{
		Name: "too-long-goal", WorkspaceID: "workspace", Runner: "test",
		InvocationRequest: "do one bounded thing", GoalKey: strings.Repeat("x", 121),
	})
	if err == nil || !strings.Contains(err.Error(), "goalKey") {
		t.Fatalf("long goal key error = %v", err)
	}
}
func TestPreparedRunnerPersistsOwnershipBeforeDispatch(t *testing.T) {
	fake := &preparedTestRunner{created: make(chan *testProcess, 1)}
	sup, _ := newPreparedSupervisor(t, fake)
	job := createJob(t, sup, "prepared ordering", 0)
	fake.persisted = func() bool {
		current, err := sup.Get(context.Background(), job.ID)
		return err == nil && current.State == "starting" && current.OwnerTarget != ""
	}
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.created)
	waitState(t, sup, job.ID, "running")
	fake.mu.Lock()
	dispatches := fake.dispatchCalls
	fake.mu.Unlock()
	if dispatches != 1 {
		t.Fatalf("dispatch calls = %d", dispatches)
	}
	process.complete(runner.Exit{Code: 0})
	waitState(t, sup, job.ID, "paused")
}

func TestPreparedRunnerDoesNotDispatchAfterStopRequest(t *testing.T) {
	fake := &preparedTestRunner{
		prepareEntered: make(chan struct{}),
		prepareRelease: make(chan struct{}),
		created:        make(chan *testProcess, 1),
	}
	sup, _ := newPreparedSupervisor(t, fake)
	job := createJob(t, sup, "prepared stop", 0)
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.prepareEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("prepared runner did not enter prepare")
	}
	if _, err := sup.StopJob(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	close(fake.prepareRelease)
	process := receiveProcess(t, fake.created)
	waitState(t, sup, job.ID, "stopped")
	fake.mu.Lock()
	dispatches := fake.dispatchCalls
	fake.mu.Unlock()
	if dispatches != 0 {
		t.Fatalf("dispatch calls after stop = %d", dispatches)
	}
	process.mu.Lock()
	stopCalls := process.stopCalls
	process.mu.Unlock()
	if stopCalls != 1 {
		t.Fatalf("prepared process stop calls = %d", stopCalls)
	}
}

func TestPreparedRunnerDoesNotDispatchAfterPersistenceFailure(t *testing.T) {
	fake := &preparedTestRunner{
		prepareEntered: make(chan struct{}),
		prepareRelease: make(chan struct{}),
		created:        make(chan *testProcess, 1),
	}
	sup, db := newPreparedSupervisor(t, fake)
	job := createJob(t, sup, "prepared persistence failure", 0)
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.prepareEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("prepared runner did not enter prepare")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	close(fake.prepareRelease)
	process := receiveProcess(t, fake.created)
	time.Sleep(20 * time.Millisecond)
	fake.mu.Lock()
	dispatches := fake.dispatchCalls
	fake.mu.Unlock()
	if dispatches != 0 {
		t.Fatalf("dispatch calls after persistence failure = %d", dispatches)
	}
	process.mu.Lock()
	stopCalls := process.stopCalls
	process.mu.Unlock()
	if stopCalls != 1 {
		t.Fatalf("prepared process stop calls = %d", stopCalls)
	}
}

func waitState(t *testing.T, sup *Supervisor, id, state string) store.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := sup.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == state {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := sup.Get(context.Background(), id)
	t.Fatalf("job %s state = %q, want %q", id, job.State, state)
	return store.Job{}
}

func receiveProcess(t *testing.T, started <-chan *testProcess) *testProcess {
	t.Helper()
	select {
	case process := <-started:
		return process
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
		return nil
	}
}

func TestManualRunExclusionSuccessAndEvidence(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	job := createJob(t, sup, "manual", 0)
	if job.State != "paused" {
		t.Fatalf("created state = %q", job.State)
	}
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.started)
	running := waitState(t, sup, job.ID, "running")
	if running.ActiveRunID == "" {
		t.Fatal("running job has no run id")
	}
	if _, err := sup.RunNow(context.Background(), job.ID); !errors.Is(err, store.ErrActive) {
		t.Fatalf("overlap error = %v", err)
	}
	process.complete(runner.Exit{Code: 0})
	waitState(t, sup, job.ID, "paused")
	runs, err := sup.Runs(context.Background(), job.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != "completed" || runs[0].ExecutionReference != process.execution || runs[0].ProcessReference != process.process || runs[0].Stdout != "stdout" || runs[0].Stderr != "stderr" {
		t.Fatalf("run evidence = %#v", runs)
	}
}

func TestUnrelatedJobsRunConcurrently(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	first := createJob(t, sup, "first", 0)
	second, err := sup.Create(context.Background(), CreateRequest{
		Name: "second", WorkspaceID: "workspace-two", Runner: "test",
		InvocationRequest: "do one bounded thing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sup.RunNow(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.RunNow(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	one := receiveProcess(t, fake.started)
	two := receiveProcess(t, fake.started)
	if one.job.ID == two.job.ID {
		t.Fatalf("both processes belong to %q", one.job.ID)
	}
	waitState(t, sup, one.job.ID, "running")
	waitState(t, sup, two.job.ID, "running")
	one.complete(runner.Exit{Code: 0})
	two.complete(runner.Exit{Code: 0})
	waitState(t, sup, first.ID, "paused")
	waitState(t, sup, second.ID, "paused")
}

func TestFailedExitIsRecorded(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	job := createJob(t, sup, "failure", 0)
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.started)
	waitState(t, sup, job.ID, "running")
	process.complete(runner.Exit{Code: 7})
	failed := waitState(t, sup, job.ID, "failed")
	if failed.LastError != "process exited with code 7" {
		t.Fatalf("failure = %q", failed.LastError)
	}
	runs, err := sup.Runs(context.Background(), job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ExitCode == nil || *runs[0].ExitCode != 7 || runs[0].FinishedAt == nil {
		t.Fatalf("failed run = %#v", runs)
	}
}

func TestWaitFailureDoesNotFabricateExitCodeZero(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	job := createJob(t, sup, "wait failure", 0)
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.started)
	waitState(t, sup, job.ID, "running")
	process.mu.Lock()
	process.waitErr = errors.New("wait status unavailable")
	process.mu.Unlock()
	process.complete(runner.Exit{})
	waitState(t, sup, job.ID, "failed")
	runs, err := sup.Runs(context.Background(), job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ExitCode != nil || runs[0].Error != "wait status unavailable" {
		t.Fatalf("wait-failed run = %#v", runs)
	}
}

func TestStopFailureRetainsOwnershipAndRetrySettles(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	job := createJob(t, sup, "stop retry", 0)
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.started)
	waitState(t, sup, job.ID, "running")
	process.mu.Lock()
	process.stopErrors = []error{errors.New("cannot terminate"), nil}
	process.mu.Unlock()
	if _, err := sup.StopJob(context.Background(), job.ID); err == nil {
		t.Fatal("first stop succeeded")
	}
	stopping := waitState(t, sup, job.ID, "stopping")
	if stopping.ActiveRunID == "" || stopping.LastError == "" {
		t.Fatalf("failed stop = %#v", stopping)
	}
	if _, err := sup.StopJob(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	stopped := waitState(t, sup, job.ID, "stopped")
	if stopped.ActiveRunID != "" || stopped.LastError != "" {
		t.Fatalf("stopped job = %#v", stopped)
	}
	process.mu.Lock()
	calls := process.stopCalls
	process.mu.Unlock()
	if calls != 2 {
		t.Fatalf("stop calls = %d", calls)
	}
}

func TestCompletionSchedulesNextRunFromCompletion(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	job := createJob(t, sup, "scheduled", 60)
	if _, err := sup.StartJob(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.started)
	waitState(t, sup, job.ID, "running")
	completedAt := time.Now().UTC()
	process.complete(runner.Exit{Code: 0})
	scheduled := waitState(t, sup, job.ID, "scheduled")
	if scheduled.NextRunAt == nil || scheduled.NextRunAt.Before(completedAt.Add(59*time.Second)) {
		t.Fatalf("next run = %v, completion = %v", scheduled.NextRunAt, completedAt)
	}
}

func TestRecoverReattachesExistingOwnerWithoutRelaunching(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workspacePath := t.TempDir()
	if err := db.SeedWorkspace(ctx, config.Workspace{ID: "workspace", Name: "workspace", Path: workspacePath}); err != nil {
		t.Fatal(err)
	}
	fake := &testRunner{started: make(chan *testProcess, 1)}
	resolver := testResolver{runner: fake}
	first, err := New(db, testWorkspaces{path: workspacePath}, resolver, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	first.Start(ctx)
	job := createJob(t, first, "restart recovery", 0)
	if _, err := first.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.started)
	running := waitState(t, first, job.ID, "running")
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	if err := first.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	cancel()

	second, err := New(db, testWorkspaces{path: workspacePath}, resolver, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_ = second.Close(closeCtx)
	}()
	if err := second.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	attachments := fake.attachCount
	fake.mu.Unlock()
	if attachments != 1 {
		t.Fatalf("attachment count = %d", attachments)
	}
	recovered, err := second.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ActiveRunID != running.ActiveRunID || recovered.OwnerTarget != running.OwnerTarget {
		t.Fatalf("recovered owner = %#v, original = %#v", recovered, running)
	}
	process.complete(runner.Exit{Code: 0})
	settled := waitState(t, second, job.ID, "paused")
	if settled.ActiveRunID != "" || settled.OwnerTarget != "" {
		t.Fatalf("settled job = %#v", settled)
	}
	runs, err := second.Runs(ctx, job.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != running.ActiveRunID {
		t.Fatalf("runs after recovery = %#v", runs)
	}
}

func TestRecoverInterruptsRunWhenOwnerIsGone(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workspacePath := t.TempDir()
	if err := db.SeedWorkspace(ctx, config.Workspace{ID: "workspace", Name: "workspace", Path: workspacePath}); err != nil {
		t.Fatal(err)
	}
	fake := &testRunner{started: make(chan *testProcess, 1)}
	resolver := testResolver{runner: fake}
	first, err := New(db, testWorkspaces{path: workspacePath}, resolver, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	first.Start(ctx)
	job := createJob(t, first, "missing restart owner", 0)
	if _, err := first.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	receiveProcess(t, fake.started)
	waitState(t, first, job.ID, "running")
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	if err := first.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	cancel()
	fake.mu.Lock()
	fake.latest = nil
	fake.mu.Unlock()

	second, err := New(db, testWorkspaces{path: workspacePath}, resolver, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	interrupted := waitState(t, second, job.ID, "interrupted")
	if interrupted.ActiveRunID != "" || interrupted.OwnerTarget != "" || interrupted.DesiredState != "paused" {
		t.Fatalf("interrupted job = %#v", interrupted)
	}
	runs, err := second.Runs(ctx, job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != "interrupted" || runs[0].FinishedAt == nil {
		t.Fatalf("interrupted runs = %#v", runs)
	}
}

func TestShutdownDetachesWithoutStoppingActiveOwner(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	job := createJob(t, sup, "shutdown", 0)
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.started)
	running := waitState(t, sup, job.ID, "running")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sup.Close(ctx); err != nil {
		t.Fatal(err)
	}
	process.mu.Lock()
	calls := process.stopCalls
	process.mu.Unlock()
	if calls != 0 {
		t.Fatalf("shutdown stop calls = %d", calls)
	}
	persisted, err := sup.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != "running" || persisted.ActiveRunID != running.ActiveRunID || persisted.OwnerTarget == "" {
		t.Fatalf("detached job = %#v", persisted)
	}
	if _, err := sup.RunNow(context.Background(), job.ID); err == nil || err.Error() != "agentd is shutting down" {
		t.Fatalf("post-shutdown run error = %v", err)
	}
}

func TestStopCancelsClaimedRunBeforeProcessRegistration(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	fake.startEntered = make(chan struct{})
	fake.startRelease = make(chan struct{})
	job := createJob(t, sup, "claim stop", 0)
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.startEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("runner start was not entered")
	}
	if _, err := sup.StopJob(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	close(fake.startRelease)
	process := receiveProcess(t, fake.started)
	waitState(t, sup, job.ID, "stopped")
	runs, err := sup.Runs(context.Background(), job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ExecutionReference == "" || runs[0].ProcessReference == "" {
		t.Fatalf("stopped run evidence = %#v", runs)
	}
	process.mu.Lock()
	calls := process.stopCalls
	process.mu.Unlock()
	if calls != 1 {
		t.Fatalf("stop calls = %d", calls)
	}
}

func TestCloseDeadlineAppliesWhileRunnerStartIsPending(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	fake.startEntered = make(chan struct{})
	fake.startRelease = make(chan struct{})
	job := createJob(t, sup, "pending close", 0)
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.startEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("runner start was not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := sup.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error = %v", err)
	}
	close(fake.startRelease)
	receiveProcess(t, fake.started)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := sup.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	persisted, err := sup.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ActiveRunID == "" {
		t.Fatalf("pending owner was cleared during detach: %#v", persisted)
	}
}
