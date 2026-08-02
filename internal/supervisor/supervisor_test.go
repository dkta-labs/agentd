package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/runner"
	"github.com/dkta-labs/agentd/internal/store"
)

type testWorkspaces struct{ path string }

func (w testWorkspaces) Workspace(_ context.Context, id string) (config.Workspace, error) {
	if id != "workspace" {
		return config.Workspace{}, errors.New("workspace not found")
	}
	return config.Workspace{ID: id, Name: id, Path: w.path}, nil
}

type testResolver struct{ runner runner.Runner }

func (r testResolver) Runner(id string) (runner.Runner, error) {
	if id != "test" {
		return nil, errors.New("runner not found")
	}
	return r.runner, nil
}

type testRunner struct {
	started      chan *testProcess
	startEntered chan struct{}
	startRelease chan struct{}
}

func (r *testRunner) Start(_ context.Context, job runner.Job) (runner.Process, error) {
	if r.startEntered != nil {
		close(r.startEntered)
		<-r.startRelease
	}
	process := &testProcess{
		job: job, done: make(chan struct{}), execution: "execution/" + job.RunID,
		process: "pid=100 pgid=100", stdout: "stdout", stderr: "stderr",
	}
	r.started <- process
	return process, nil
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
	second := createJob(t, sup, "second", 0)
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

func TestShutdownStopsActiveRunAndRejectsNewStarts(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	job := createJob(t, sup, "shutdown", 0)
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.started)
	waitState(t, sup, job.ID, "running")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sup.Close(ctx); err != nil {
		t.Fatal(err)
	}
	process.mu.Lock()
	calls := process.stopCalls
	process.mu.Unlock()
	if calls != 1 {
		t.Fatalf("shutdown stop calls = %d", calls)
	}
	stopped := waitState(t, sup, job.ID, "stopped")
	if stopped.ActiveRunID != "" {
		t.Fatalf("shutdown job = %#v", stopped)
	}
	if _, err := sup.RunNow(context.Background(), job.ID); err == nil || err.Error() != "agentd is shutting down" {
		t.Fatalf("post-shutdown run error = %v", err)
	}
}

func TestShutdownRetriesFailedTerminationBeforeReturning(t *testing.T) {
	sup, fake := newTestSupervisor(t)
	job := createJob(t, sup, "shutdown retry", 0)
	if _, err := sup.RunNow(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	process := receiveProcess(t, fake.started)
	waitState(t, sup, job.ID, "running")
	process.mu.Lock()
	process.stopErrors = []error{errors.New("transient stop failure"), nil}
	process.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sup.Close(ctx); err != nil {
		t.Fatal(err)
	}
	process.mu.Lock()
	calls := process.stopCalls
	process.mu.Unlock()
	if calls != 2 {
		t.Fatalf("shutdown stop calls = %d", calls)
	}
	waitState(t, sup, job.ID, "stopped")
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
	waitState(t, sup, job.ID, "stopped")
}

type serialVisibilityReporter struct {
	mu      sync.Mutex
	active  int
	max     int
	calls   int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *serialVisibilityReporter) Report(context.Context, config.Workspace, Visibility) error {
	r.mu.Lock()
	r.active++
	r.calls++
	if r.active > r.max {
		r.max = r.active
	}
	r.once.Do(func() { close(r.entered) })
	r.mu.Unlock()
	<-r.release
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return nil
}

func TestWorkspaceVisibilityReportsAreSerialized(t *testing.T) {
	sup, _ := newTestSupervisor(t)
	createJob(t, sup, "visibility", 0)
	reporter := &serialVisibilityReporter{entered: make(chan struct{}), release: make(chan struct{})}
	sup.SetVisibilityReporter(reporter)

	results := make(chan error, 2)
	go func() { results <- sup.reportWorkspace(context.Background(), "workspace") }()
	select {
	case <-reporter.entered:
	case <-time.After(time.Second):
		t.Fatal("first visibility report did not start")
	}
	go func() { results <- sup.reportWorkspace(context.Background(), "workspace") }()
	time.Sleep(20 * time.Millisecond)

	reporter.mu.Lock()
	maxActive := reporter.max
	callsBeforeRelease := reporter.calls
	reporter.mu.Unlock()
	if maxActive != 1 || callsBeforeRelease != 1 {
		t.Fatalf("concurrent reports before release: max=%d calls=%d", maxActive, callsBeforeRelease)
	}
	close(reporter.release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if reporter.max != 1 || reporter.calls != 2 {
		t.Fatalf("serialized reports: max=%d calls=%d", reporter.max, reporter.calls)
	}
}
