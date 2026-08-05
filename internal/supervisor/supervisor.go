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
type CreateRequest struct {
	Name              string `json:"name"`
	WorkspaceID       string `json:"workspaceId"`
	Runner            string `json:"runner"`
	InvocationRequest string `json:"invocationRequest"`
	CadenceSeconds    int    `json:"cadenceSeconds"`
	GoalKey           string `json:"goalKey"`
}

type activeRun struct {
	process       runner.Process
	stopRequested bool
	waitCancel    context.CancelFunc
}

type Supervisor struct {
	store        *store.DB
	workspaces   WorkspaceResolver
	runners      runner.Resolver
	logger       *slog.Logger
	wake         chan struct{}
	mu           sync.Mutex
	transitionMu sync.Mutex
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
		s.active[run.ID] = activeRun{}
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
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for _, active := range s.active {
		if active.waitCancel != nil {
			cancels = append(cancels, active.waitCancel)
		}
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recover reattaches background watchers to Herdr-owned interactive sessions.
// Healthy workers survive Agentd restarts; only missing owners are interrupted.
func (s *Supervisor) Recover(ctx context.Context) error {
	jobs, err := s.store.Jobs(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, job := range jobs {
		if job.ActiveRunID == "" {
			continue
		}
		run, runErr := s.store.Run(ctx, job.ID, job.ActiveRunID)
		if runErr != nil {
			if first == nil {
				first = runErr
			}
			continue
		}
		configured, resolveErr := s.runners.Runner(job.Runner)
		workspace, workspaceErr := s.workspaces.Workspace(ctx, job.WorkspaceID)
		if resolveErr != nil || workspaceErr != nil || run.ProcessReference == "" {
			message := "cannot reattach Herdr owner after Agentd restart"
			_, finishErr := s.store.FinishRun(ctx, job.ID, run.ID, "interrupted", nil, "", message, run.Stdout, run.Stderr, time.Now().UTC(), true)
			if first == nil {
				first = errors.Join(resolveErr, workspaceErr, finishErr)
			}
			continue
		}
		jobValue := runner.Job{
			ID: job.ID, RunID: run.ID, WorkspaceID: job.WorkspaceID,
			WorkspacePath: workspace.Path, GoalKey: job.GoalKey, InvocationRequest: job.InvocationRequest,
		}
		var process runner.Process
		var prepared runner.Prepared
		if run.State == "starting" {
			reattacher, ok := configured.(runner.PreparedReattacher)
			if !ok {
				message := "cannot reattach prepared Herdr owner after Agentd restart"
				_, finishErr := s.store.FinishRun(ctx, job.ID, run.ID, "interrupted", nil, "", message, run.Stdout, run.Stderr, time.Now().UTC(), true)
				if first == nil {
					first = finishErr
				}
				continue
			}
			prepared, runErr = reattacher.AttachPrepared(ctx, jobValue, run.ProcessReference, run.PreparedSequence)
			if runErr == nil {
				process = prepared.Process()
			}
		} else {
			reattacher, ok := configured.(runner.Reattacher)
			if !ok {
				runErr = errors.New("runner does not support restart attachment")
			} else {
				process, runErr = reattacher.Attach(ctx, jobValue, run.ProcessReference)
			}
		}
		if runErr != nil {
			message := "reattach Herdr owner after Agentd restart: " + runErr.Error()
			_, finishErr := s.store.FinishRun(ctx, job.ID, run.ID, "interrupted", nil, "", message, run.Stdout, run.Stderr, time.Now().UTC(), true)
			if first == nil {
				first = finishErr
			}
			continue
		}
		runCtx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		s.active[run.ID] = activeRun{process: process, waitCancel: cancel}
		s.mu.Unlock()
		s.wg.Add(1)
		if prepared != nil {
			go func(job store.Job, run store.Run, process runner.Process, prepared runner.Prepared, runCtx context.Context, cancel context.CancelFunc) {
				defer s.wg.Done()
				defer cancel()
				s.recoverPrepared(job, run, process, prepared, runCtx)
			}(job, run, process, prepared, runCtx, cancel)
		} else {
			go func(job store.Job, run store.Run, process runner.Process, runCtx context.Context, cancel context.CancelFunc) {
				defer s.wg.Done()
				defer cancel()
				s.watchProcess(job, run, process, runCtx, nil)
			}(job, run, process, runCtx, cancel)
		}
	}
	return first
}

func (s *Supervisor) recoverPrepared(job store.Job, run store.Run, process runner.Process, prepared runner.Prepared, runCtx context.Context) {
	var stateErr error
	if err := prepared.Dispatch(runCtx); err != nil {
		stateErr = fmt.Errorf("dispatch assignment after restart: %w", err)
	} else {
		stateErr = s.store.SetRunRunning(context.Background(), job.ID, run.ID, executionReference(process), processReference(process))
		if stateErr != nil {
			stateErr = fmt.Errorf("record running state after restart: %w", stateErr)
		}
	}
	if stateErr != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		stopErr := process.Stop(stopCtx)
		stopCancel()
		if stopErr != nil {
			stateErr = errors.Join(stateErr, fmt.Errorf("stop Herdr owner: %w", stopErr))
			_ = s.store.SetStopFailure(context.Background(), job.ID, run.ID, stateErr.Error())
		}
	}
	s.watchProcess(job, run, process, runCtx, stateErr)
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
	return s.store.CreateJob(ctx, store.Job{ID: id, Name: definition.Name, WorkspaceID: definition.WorkspaceID, GoalKey: definition.GoalKey, Runner: definition.Runner, InvocationRequest: definition.InvocationRequest, CadenceSeconds: definition.CadenceSeconds})
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
	if owned {
		active.stopRequested = true
		s.active[j.ActiveRunID] = active
	}
	if owned && active.process == nil {
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
	request.GoalKey = strings.TrimSpace(request.GoalKey)
	if request.GoalKey != "" && len(request.GoalKey) > 120 {
		return store.Job{}, errors.New("goalKey must not exceed 120 bytes")
	}
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
	return store.Job{Name: request.Name, WorkspaceID: request.WorkspaceID, GoalKey: request.GoalKey, Runner: request.Runner, InvocationRequest: request.InvocationRequest, CadenceSeconds: request.CadenceSeconds}, nil
}

func (s *Supervisor) startRun(ctx context.Context, job store.Job, run store.Run) (runner.Process, func(context.Context) error, uint64, bool, error) {
	workspace, err := s.workspaces.Workspace(ctx, job.WorkspaceID)
	if err != nil {
		return nil, nil, 0, false, err
	}
	configured, err := s.runners.Runner(job.Runner)
	if err != nil {
		return nil, nil, 0, false, err
	}
	jobValue := runner.Job{
		ID: job.ID, RunID: run.ID, WorkspaceID: job.WorkspaceID,
		WorkspacePath: workspace.Path, GoalKey: job.GoalKey, InvocationRequest: job.InvocationRequest,
	}
	if preparer, ok := configured.(runner.Preparer); ok {
		prepared, err := preparer.Prepare(ctx, jobValue)
		if err != nil {
			return nil, nil, 0, false, err
		}
		return prepared.Process(), prepared.Dispatch, preparedSequence(prepared.Process()), true, nil
	}
	process, err := configured.Start(ctx, jobValue)
	return process, nil, 0, false, err
}

func (s *Supervisor) execute(job store.Job, run store.Run) {
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.mu.Lock()
	active := s.active[run.ID]
	active.waitCancel = cancel
	s.active[run.ID] = active
	s.mu.Unlock()

	process, dispatch, prePromptSequence, prepared, startErr := s.startRun(runCtx, job, run)
	if startErr != nil {
		s.mu.Lock()
		closing := s.closing
		delete(s.active, run.ID)
		s.mu.Unlock()
		if closing && errors.Is(startErr, context.Canceled) {
			return
		}
		_, _ = s.store.FinishRun(context.Background(), job.ID, run.ID, "failed", nil, "", startErr.Error(), "", "", time.Now().UTC(), true)
		s.notify()
		return
	}
	s.mu.Lock()
	active = s.active[run.ID]
	active.process = process
	stopRequested := active.stopRequested
	s.active[run.ID] = active
	s.mu.Unlock()

	var stateErr error
	if prepared {
		stateErr = s.store.SetRunPreparedEvidence(context.Background(), job.ID, run.ID, executionReference(process), processReference(process), prePromptSequence)
		if stateErr == nil && !stopRequested && dispatch != nil {
			if dispatchErr := dispatch(runCtx); dispatchErr != nil {
				stateErr = fmt.Errorf("dispatch assignment: %w", dispatchErr)
			}
		}
		if stateErr == nil && !stopRequested {
			stateErr = s.store.SetRunRunning(context.Background(), job.ID, run.ID, executionReference(process), processReference(process))
		}
	} else {
		stateErr = s.store.SetRunRunning(context.Background(), job.ID, run.ID, executionReference(process), processReference(process))
	}
	if stateErr != nil {
		stateErr = fmt.Errorf("record run state: %w", stateErr)
	}
	shouldStop := stopRequested || stateErr != nil
	if shouldStop {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		stopErr := process.Stop(stopCtx)
		stopCancel()
		if stopErr != nil {
			stateErr = errors.Join(stateErr, fmt.Errorf("stop Herdr owner: %w", stopErr))
			_ = s.store.SetStopFailure(context.Background(), job.ID, run.ID, stateErr.Error())
		}
	}
	s.watchProcess(job, run, process, runCtx, stateErr)
}

func (s *Supervisor) watchProcess(job store.Job, run store.Run, process runner.Process, runCtx context.Context, stateErr error) {
	defer func() {
		s.mu.Lock()
		delete(s.active, run.ID)
		s.mu.Unlock()
	}()
	exit, waitErr := process.Wait(runCtx)
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing && errors.Is(waitErr, context.Canceled) {
		return
	}
	stdout, stderr := output(process)
	state, message := "completed", ""
	if stateErr != nil {
		state, message = "failed", stateErr.Error()
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
	s.notify()
}
func preparedSequence(p runner.Process) uint64 {
	if evidence, ok := p.(runner.PreparedEvidence); ok {
		return evidence.PreparedSequence()
	}
	return 0
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
