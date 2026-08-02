package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/runner"
	"github.com/dkta-labs/agentd/internal/store"
)

const MaxInvocationBytes = 64 * 1024

type WorkspaceResolver interface {
	Workspace(context.Context, string) (config.Workspace, error)
}
type VisibilityReporter interface {
	Report(context.Context, config.Workspace, Visibility) error
}
type Visibility struct {
	Status   string
	JobID    string
	JobName  string
	RunID    string
	Result   string
	Evidence string
}
type CreateRequest struct {
	Name              string `json:"name"`
	WorkspaceID       string `json:"workspaceId"`
	Runner            string `json:"runner"`
	InvocationRequest string `json:"invocationRequest"`
	CadenceSeconds    int    `json:"cadenceSeconds"`
}

type activeRun struct {
	jobID         string
	process       runner.Process
	stopRequested bool
}

type Supervisor struct {
	store        *store.DB
	workspaces   WorkspaceResolver
	runners      runner.Resolver
	reporter     VisibilityReporter
	logger       *slog.Logger
	wake         chan struct{}
	mu           sync.Mutex
	transitionMu sync.Mutex
	visibilityMu sync.Mutex
	active       map[string]activeRun
	closing      bool
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	startOnce    sync.Once
}

func New(db *store.DB, workspaces WorkspaceResolver, runners runner.Resolver, logger *slog.Logger) (*Supervisor, error) {
	if db == nil || workspaces == nil || runners == nil {
		return nil, errors.New("store, workspaces, and runners are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{store: db, workspaces: workspaces, runners: runners, logger: logger, wake: make(chan struct{}, 1), active: make(map[string]activeRun)}, nil
}
func (s *Supervisor) SetVisibilityReporter(reporter VisibilityReporter) {
	s.reporter = reporter
}

func (s *Supervisor) SyncVisibility(ctx context.Context) error {
	jobs, err := s.store.Jobs(ctx)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(jobs))
	var first error
	for _, job := range jobs {
		if _, ok := seen[job.WorkspaceID]; ok {
			continue
		}
		seen[job.WorkspaceID] = struct{}{}
		if err := s.reportWorkspaceBounded(ctx, job.WorkspaceID); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Supervisor) Start(parent context.Context) {
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
		s.cancel = cancel
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.loop(ctx) }()
	})
}
func (s *Supervisor) loop(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}
func (s *Supervisor) dispatch(ctx context.Context) {
	for {
		runID, err := newID("run_")
		if err != nil {
			s.logger.Error("create run id", "error", err)
			return
		}
		s.transitionMu.Lock()
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			s.transitionMu.Unlock()
			return
		}
		job, run, claimed, err := s.store.ClaimNext(ctx, time.Now().UTC(), runID)
		if err != nil {
			s.transitionMu.Unlock()
			s.logger.Error("claim job run", "error", err)
			return
		}
		if !claimed {
			s.transitionMu.Unlock()
			return
		}
		s.mu.Lock()
		s.active[run.ID] = activeRun{jobID: job.ID}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.execute(job, run)
		}()
		s.transitionMu.Unlock()
	}
}
func (s *Supervisor) BeginClose() {
	s.transitionMu.Lock()
	defer s.transitionMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return
	}
	s.closing = true
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Supervisor) Close(ctx context.Context) error {
	s.BeginClose()
	s.mu.Lock()
	jobs := make([]string, 0, len(s.active))
	for _, active := range s.active {
		jobs = append(jobs, active.jobID)
	}
	s.mu.Unlock()
	var first error
	for _, jobID := range jobs {
		var stopErr error
		for {
			_, stopErr = s.StopJob(ctx, jobID)
			if stopErr == nil {
				break
			}
			select {
			case <-ctx.Done():
				break
			case <-time.After(100 * time.Millisecond):
				continue
			}
			break
		}
		if stopErr != nil && first == nil {
			first = stopErr
		}
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		if first == nil {
			first = ctx.Err()
		}
	}
	return first
}

func (s *Supervisor) Create(ctx context.Context, request CreateRequest) (store.Job, error) {
	definition, err := s.validate(ctx, request)
	if err != nil {
		return store.Job{}, err
	}
	id, err := newID("job_")
	if err != nil {
		return store.Job{}, err
	}
	return s.store.CreateJob(ctx, store.Job{ID: id, Name: definition.Name, WorkspaceID: definition.WorkspaceID, Runner: definition.Runner, InvocationRequest: definition.InvocationRequest, CadenceSeconds: definition.CadenceSeconds})
}
func (s *Supervisor) Update(ctx context.Context, id string, request CreateRequest) (store.Job, error) {
	definition, err := s.validate(ctx, request)
	if err != nil {
		return store.Job{}, err
	}
	definition.ID = id
	s.transitionMu.Lock()
	defer s.transitionMu.Unlock()
	return s.store.UpdateJob(ctx, definition)
}
func (s *Supervisor) List(ctx context.Context) ([]store.Job, error) { return s.store.Jobs(ctx) }
func (s *Supervisor) Get(ctx context.Context, id string) (store.Job, error) {
	return s.store.Job(ctx, id)
}
func (s *Supervisor) Runs(ctx context.Context, id string, limit int) ([]store.Run, error) {
	if _, err := s.store.Job(ctx, id); err != nil {
		return nil, err
	}
	return s.store.Runs(ctx, id, limit)
}
func (s *Supervisor) StartJob(ctx context.Context, id string) (store.Job, error) {
	s.transitionMu.Lock()
	defer s.transitionMu.Unlock()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return store.Job{}, errors.New("agentd is shutting down")
	}
	j, err := s.store.StartJob(ctx, id, time.Now().UTC())
	s.mu.Unlock()
	if err == nil {
		s.notify()
	}
	return j, err
}
func (s *Supervisor) PauseJob(ctx context.Context, id string) (store.Job, error) {
	s.transitionMu.Lock()
	defer s.transitionMu.Unlock()
	return s.store.PauseJob(ctx, id, time.Now().UTC())
}
func (s *Supervisor) RunNow(ctx context.Context, id string) (store.Job, error) {
	s.transitionMu.Lock()
	defer s.transitionMu.Unlock()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return store.Job{}, errors.New("agentd is shutting down")
	}
	j, err := s.store.RequestRun(ctx, id, time.Now().UTC())
	s.mu.Unlock()
	if err == nil {
		s.notify()
	}
	return j, err
}
func (s *Supervisor) StopJob(ctx context.Context, id string) (store.Job, error) {
	s.transitionMu.Lock()
	j, err := s.store.StopJob(ctx, id, time.Now().UTC())
	s.transitionMu.Unlock()
	if err != nil {
		return store.Job{}, err
	}
	if j.ActiveRunID == "" {
		return j, nil
	}
	s.mu.Lock()
	active, owned := s.active[j.ActiveRunID]
	if owned && active.process == nil {
		active.stopRequested = true
		s.active[j.ActiveRunID] = active
		s.mu.Unlock()
		return j, nil
	}
	s.mu.Unlock()
	if !owned {
		message := "active process is not owned by this daemon"
		_ = s.store.SetStopFailure(ctx, j.ID, j.ActiveRunID, message)
		return j, errors.New(message)
	}
	p := active.process
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = p.Stop(stopCtx)
	cancel()
	if err != nil {
		message := "stop process: " + err.Error()
		if persistErr := s.store.SetStopFailure(ctx, j.ID, j.ActiveRunID, message); persistErr != nil {
			return j, fmt.Errorf("%s; persist stop failure: %w", message, persistErr)
		}
		return j, errors.New(message)
	}
	return s.store.Job(ctx, id)
}

func (s *Supervisor) validate(ctx context.Context, request CreateRequest) (store.Job, error) {
	request.Name = strings.TrimSpace(request.Name)
	request.WorkspaceID = strings.TrimSpace(request.WorkspaceID)
	request.Runner = strings.TrimSpace(request.Runner)
	if request.Runner == "" {
		request.Runner = "omp"
	}
	request.InvocationRequest = strings.TrimSpace(request.InvocationRequest)
	if request.Name == "" || len(request.Name) > 120 {
		return store.Job{}, errors.New("name must contain between 1 and 120 bytes")
	}
	if request.WorkspaceID == "" {
		return store.Job{}, errors.New("workspaceId is required")
	}
	if request.InvocationRequest == "" || len(request.InvocationRequest) > MaxInvocationBytes {
		return store.Job{}, fmt.Errorf("invocationRequest must contain between 1 and %d bytes", MaxInvocationBytes)
	}
	if request.CadenceSeconds < 0 {
		return store.Job{}, errors.New("cadenceSeconds must not be negative")
	}
	if _, err := s.workspaces.Workspace(ctx, request.WorkspaceID); err != nil {
		return store.Job{}, err
	}
	if _, err := s.runners.Runner(request.Runner); err != nil {
		return store.Job{}, err
	}
	return store.Job{Name: request.Name, WorkspaceID: request.WorkspaceID, Runner: request.Runner, InvocationRequest: request.InvocationRequest, CadenceSeconds: request.CadenceSeconds}, nil
}

func (s *Supervisor) startRun(job store.Job, run store.Run) (runner.Process, error) {
	workspace, err := s.workspaces.Workspace(context.Background(), job.WorkspaceID)
	if err != nil {
		return nil, err
	}
	configured, err := s.runners.Runner(job.Runner)
	if err != nil {
		return nil, err
	}
	process, err := configured.Start(context.Background(), runner.Job{ID: job.ID, RunID: run.ID, WorkspacePath: workspace.Path, InvocationRequest: job.InvocationRequest})
	if err != nil {
		return nil, err
	}
	return process, nil
}

func (s *Supervisor) execute(job store.Job, run store.Run) {
	defer func() {
		s.mu.Lock()
		delete(s.active, run.ID)
		s.mu.Unlock()
	}()
	s.reportWorkspaceLogged(job.WorkspaceID)

	process, startErr := s.startRun(job, run)
	if startErr != nil {
		_, _ = s.store.FinishRun(context.Background(), job.ID, run.ID, "failed", nil, "", startErr.Error(), "", "", time.Now().UTC(), true)
		s.reportWorkspaceLogged(job.WorkspaceID)
		s.notify()
		return
	}
	s.mu.Lock()
	active := s.active[run.ID]
	active.process = process
	stopRequested := active.stopRequested
	s.active[run.ID] = active
	s.mu.Unlock()

	stateErr := s.store.SetRunRunning(context.Background(), job.ID, run.ID, executionReference(process), processReference(process))
	s.reportWorkspaceLogged(job.WorkspaceID)
	if stopRequested || stateErr != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stopErr := process.Stop(stopCtx)
		cancel()
		if stopErr != nil {
			if stopRequested {
				stateErr = fmt.Errorf("stop requested before process registration: %w", stopErr)
			} else {
				stateErr = fmt.Errorf("stop after state persistence failure: %w", stopErr)
			}
			_ = s.store.SetStopFailure(context.Background(), job.ID, run.ID, stateErr.Error())
		}
	}

	exit, waitErr := process.Wait(context.Background())
	stdout, stderr := output(process)
	state, message := "completed", ""
	if stateErr != nil {
		state, message = "failed", "record running state: "+stateErr.Error()
	} else if waitErr != nil {
		state, message = "failed", waitErr.Error()
	} else if !exit.Successful() {
		state = "failed"
		if exit.Err != nil {
			message = exit.Err.Error()
		} else if exit.Signal != "" {
			message = "process exited with " + exit.Signal
		} else {
			message = fmt.Sprintf("process exited with code %d", exit.Code)
		}
	}
	var exitCode *int
	if waitErr == nil && exit.Err == nil && exit.Code >= 0 {
		value := exit.Code
		exitCode = &value
	}
	if _, err := s.store.FinishRun(context.Background(), job.ID, run.ID, state, exitCode, exit.Signal, message, stdout, stderr, time.Now().UTC(), true); err != nil {
		s.logger.Error("finish run", "job", job.ID, "run", run.ID, "error", err)
	}
	s.reportWorkspaceLogged(job.WorkspaceID)
	s.notify()
}
func (s *Supervisor) reportWorkspaceLogged(workspaceID string) {
	if err := s.reportWorkspaceBounded(context.Background(), workspaceID); err != nil {
		s.logger.Warn("report workspace visibility", "workspace", workspaceID, "error", err)
	}
}
func (s *Supervisor) reportWorkspaceBounded(parent context.Context, workspaceID string) error {
	if s.reporter == nil {
		return nil
	}
	s.visibilityMu.Lock()
	defer s.visibilityMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	return s.reportWorkspaceLocked(ctx, workspaceID)
}
func (s *Supervisor) reportWorkspace(ctx context.Context, workspaceID string) error {
	if s.reporter == nil {
		return nil
	}
	s.visibilityMu.Lock()
	defer s.visibilityMu.Unlock()
	return s.reportWorkspaceLocked(ctx, workspaceID)
}
func (s *Supervisor) reportWorkspaceLocked(ctx context.Context, workspaceID string) error {
	workspace, err := s.workspaces.Workspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	jobs, err := s.store.Jobs(ctx)
	if err != nil {
		return err
	}
	var selected *store.Job
	for i := range jobs {
		job := &jobs[i]
		if job.WorkspaceID != workspaceID {
			continue
		}
		if selected == nil || visibilityRank(*job) > visibilityRank(*selected) || (visibilityRank(*job) == visibilityRank(*selected) && job.UpdatedAt.After(selected.UpdatedAt)) {
			selected = job
		}
	}
	if selected == nil {
		return nil
	}
	status := "idle"
	if visibilityRank(*selected) == 3 {
		status = "working"
	} else if visibilityRank(*selected) == 2 {
		status = "blocked"
	}
	visibility := Visibility{Status: status, JobID: selected.ID, JobName: selected.Name, RunID: selected.ActiveRunID, Result: selected.State}
	runs, runErr := s.store.Runs(ctx, selected.ID, 1)
	if runErr != nil {
		return runErr
	}
	if len(runs) > 0 {
		if visibility.RunID == "" {
			visibility.RunID = runs[0].ID
		}
		visibility.Evidence = runs[0].ExecutionReference
		if status == "idle" {
			visibility.Result = runs[0].State
		}
	}
	if status == "blocked" && selected.LastError != "" {
		visibility.Result = selected.LastError
	}
	visibility.Result = truncateVisibility(visibility.Result, 120)
	return s.reporter.Report(ctx, workspace, visibility)
}
func visibilityRank(job store.Job) int {
	switch job.State {
	case "starting", "running", "stopping":
		return 3
	case "failed", "interrupted":
		return 2
	default:
		if job.LastError != "" {
			return 2
		}
		return 1
	}
}
func truncateVisibility(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit-3] + "..."
}
func executionReference(p runner.Process) string {
	if e, ok := p.(runner.Evidence); ok {
		return e.ExecutionReference()
	}
	return ""
}
func processReference(p runner.Process) string {
	if e, ok := p.(runner.Evidence); ok {
		return e.ProcessReference()
	}
	return ""
}
func output(p runner.Process) (string, string) {
	if e, ok := p.(runner.Evidence); ok {
		return e.Output()
	}
	return "", ""
}
func (s *Supervisor) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func newID(prefix string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}
