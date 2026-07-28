package loops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/sessions"
	"github.com/dkta-labs/agentd/internal/store"
)

const (
	minimumCadenceSeconds = 300
	maximumCadenceSeconds = 30 * 24 * 60 * 60
	minimumTimeoutSeconds = 30
	maximumTimeoutSeconds = 1800
	defaultTimeoutSeconds = 900
	maximumPromptBytes    = 64 * 1024
	defaultConcurrency    = 3
)

type WorkspaceResolver interface {
	Resolve(context.Context, string) (config.Workspace, error)
}

type SessionManager interface {
	Harnesses() []runtime.Descriptor
	StartWithHarnessAndID(context.Context, string, string, string) (sessions.Session, error)
	Send(context.Context, string, runtime.Input) error
	Stop(context.Context, string) error
}

type EventBroker interface {
	After(context.Context, string, uint64) ([]events.Event, error)
	Subscribe(string) (<-chan events.Event, func())
}

type CreateRequest struct {
	Name           string `json:"name"`
	WorkspaceID    string `json:"workspaceId"`
	HarnessID      string `json:"harnessId"`
	Prompt         string `json:"prompt"`
	CadenceSeconds int    `json:"cadenceSeconds"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

type Supervisor struct {
	store      *store.DB
	workspaces WorkspaceResolver
	sessions   SessionManager
	broker     EventBroker
	logger     *slog.Logger
	wake       chan struct{}
	semaphore  chan struct{}

	startOnce sync.Once
	closeOnce sync.Once
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func New(persistence *store.DB, workspaces WorkspaceResolver, sessionManager SessionManager, broker EventBroker, logger *slog.Logger) (*Supervisor, error) {
	if persistence == nil || workspaces == nil || sessionManager == nil || broker == nil {
		return nil, errors.New("loop persistence, workspaces, sessions, and broker are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{
		store: persistence, workspaces: workspaces, sessions: sessionManager,
		broker: broker, logger: logger, wake: make(chan struct{}, 1),
		semaphore: make(chan struct{}, defaultConcurrency),
	}, nil
}

func (s *Supervisor) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		var runCtx context.Context
		runCtx, s.cancel = context.WithCancel(context.WithoutCancel(ctx))
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.run(runCtx)
		}()
	})
}

func (s *Supervisor) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	complete := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(complete)
	}()
	select {
	case <-complete:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) Create(ctx context.Context, request CreateRequest) (store.LoopRecord, error) {
	definition, err := s.validateDefinition(ctx, request)
	if err != nil {
		return store.LoopRecord{}, err
	}
	id, err := newID("loop_")
	if err != nil {
		return store.LoopRecord{}, err
	}
	now := time.Now().UTC()
	return s.store.CreateLoop(ctx, store.LoopRecord{
		ID: id, Name: definition.Name, WorkspaceID: definition.WorkspaceID,
		HarnessID: definition.HarnessID, Prompt: definition.Prompt,
		CadenceSeconds: definition.CadenceSeconds, TimeoutSeconds: definition.TimeoutSeconds,
		DesiredState: "paused", State: "paused", CreatedAt: now, UpdatedAt: now,
	})
}

func (s *Supervisor) Update(ctx context.Context, id string, request CreateRequest) (store.LoopRecord, error) {
	definition, err := s.validateDefinition(ctx, request)
	if err != nil {
		return store.LoopRecord{}, err
	}
	definition.ID = id
	return s.store.UpdateLoopDefinition(ctx, definition)
}

func (s *Supervisor) List(ctx context.Context) ([]store.LoopRecord, error) {
	return s.store.Loops(ctx)
}

func (s *Supervisor) Get(ctx context.Context, id string) (store.LoopRecord, error) {
	return s.store.Loop(ctx, id)
}

func (s *Supervisor) Runs(ctx context.Context, id string, limit int) ([]store.LoopRunRecord, error) {
	if _, err := s.store.Loop(ctx, id); err != nil {
		return nil, err
	}
	return s.store.LoopRuns(ctx, id, limit)
}

func (s *Supervisor) ResumeLoop(ctx context.Context, id string) (store.LoopRecord, error) {
	record, err := s.store.StartLoop(ctx, id, time.Now().UTC())
	if err == nil {
		s.notify()
	}
	return record, err
}

func (s *Supervisor) PauseLoop(ctx context.Context, id string) (store.LoopRecord, error) {
	return s.store.PauseLoop(ctx, id, time.Now().UTC())
}

func (s *Supervisor) RunLoop(ctx context.Context, id string) (store.LoopRecord, error) {
	record, err := s.store.RequestLoopRun(ctx, id, time.Now().UTC())
	if err == nil {
		s.notify()
	}
	return record, err
}

func (s *Supervisor) validateDefinition(ctx context.Context, request CreateRequest) (store.LoopRecord, error) {
	request.Name = strings.TrimSpace(request.Name)
	request.WorkspaceID = strings.TrimSpace(request.WorkspaceID)
	request.HarnessID = strings.TrimSpace(request.HarnessID)
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.Name == "" || len(request.Name) > 120 {
		return store.LoopRecord{}, errors.New("name must contain between 1 and 120 bytes")
	}
	if request.WorkspaceID == "" {
		return store.LoopRecord{}, errors.New("workspaceId is required")
	}
	if request.Prompt == "" || len(request.Prompt) > maximumPromptBytes {
		return store.LoopRecord{}, fmt.Errorf("prompt must contain between 1 and %d bytes", maximumPromptBytes)
	}
	if request.CadenceSeconds != 0 && (request.CadenceSeconds < minimumCadenceSeconds || request.CadenceSeconds > maximumCadenceSeconds) {
		return store.LoopRecord{}, fmt.Errorf("cadenceSeconds must be 0 or between %d and %d", minimumCadenceSeconds, maximumCadenceSeconds)
	}
	if request.TimeoutSeconds == 0 {
		request.TimeoutSeconds = defaultTimeoutSeconds
	}
	if request.TimeoutSeconds < minimumTimeoutSeconds || request.TimeoutSeconds > maximumTimeoutSeconds {
		return store.LoopRecord{}, fmt.Errorf("timeoutSeconds must be between %d and %d", minimumTimeoutSeconds, maximumTimeoutSeconds)
	}
	if _, err := s.workspaces.Resolve(ctx, request.WorkspaceID); err != nil {
		return store.LoopRecord{}, err
	}
	if request.HarnessID == "" {
		request.HarnessID = "omp"
	}
	knownHarness := false
	for _, descriptor := range s.sessions.Harnesses() {
		if descriptor.ID == request.HarnessID {
			knownHarness = true
			break
		}
	}
	if !knownHarness {
		return store.LoopRecord{}, fmt.Errorf("harness %q is not registered", request.HarnessID)
	}
	return store.LoopRecord{
		Name: request.Name, WorkspaceID: request.WorkspaceID, HarnessID: request.HarnessID,
		Prompt: request.Prompt, CadenceSeconds: request.CadenceSeconds,
		TimeoutSeconds: request.TimeoutSeconds,
	}, nil
}

func (s *Supervisor) run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
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
		select {
		case s.semaphore <- struct{}{}:
		case <-ctx.Done():
			return
		default:
			return
		}
		runID, err := newID("run_")
		if err != nil {
			<-s.semaphore
			s.logger.Error("create loop run id", "error", err)
			return
		}
		sessionID, err := newID("ses_loop_")
		if err != nil {
			<-s.semaphore
			s.logger.Error("create loop session id", "error", err)
			return
		}
		loop, run, claimed, err := s.store.ClaimNextLoop(ctx, time.Now().UTC(), runID, sessionID)
		if err != nil {
			<-s.semaphore
			s.logger.Error("claim loop run", "error", err)
			return
		}
		if !claimed {
			<-s.semaphore
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.semaphore }()
			s.execute(ctx, loop, run)
		}()
	}
}

func (s *Supervisor) execute(supervisorCtx context.Context, loop store.LoopRecord, run store.LoopRunRecord) {
	runCtx, cancel := context.WithTimeout(supervisorCtx, time.Duration(loop.TimeoutSeconds)*time.Second)
	defer cancel()
	started := false
	sequence := uint64(0)
	finish := func(state, failure string) {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cleanupCancel()
		if started {
			if err := s.sessions.Stop(cleanupCtx, run.SessionID); err != nil && !errors.Is(err, sessions.ErrNotFound) {
				if failure == "" {
					failure = "close loop session: " + err.Error()
					state = "failed"
				} else {
					failure += "; close loop session: " + err.Error()
				}
			}
		}
		if _, err := s.store.FinishLoopRun(cleanupCtx, loop.ID, run.ID, state, sequence, failure, time.Now().UTC()); err != nil {
			s.logger.Error("finish loop run", "loop", loop.ID, "run", run.ID, "error", err)
		}
		s.notify()
	}

	if _, err := s.sessions.StartWithHarnessAndID(runCtx, loop.WorkspaceID, loop.HarnessID, run.SessionID); err != nil {
		finish("failed", "start loop session: "+err.Error())
		return
	}
	started = true
	updates, unsubscribe := s.broker.Subscribe(run.SessionID)
	defer unsubscribe()
	if err := s.store.SetLoopRunState(runCtx, loop.ID, run.ID, "running", sequence); err != nil {
		finish("failed", "persist running loop state: "+err.Error())
		return
	}
	if err := s.sessions.Send(runCtx, run.SessionID, runtime.Input{
		IdempotencyKey: run.IdempotencyKey,
		Mode:           runtime.InputPrompt,
		Text:           buildPrompt(loop, run),
	}); err != nil {
		finish("failed", "send loop prompt: "+err.Error())
		return
	}

	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for {
		stored, err := s.broker.After(runCtx, run.SessionID, sequence)
		if err != nil {
			finish("failed", "read loop events: "+err.Error())
			return
		}
		for _, event := range stored {
			if event.Sequence > sequence {
				sequence = event.Sequence
			}
			switch event.Type {
			case runtime.EventTurnStarted, runtime.EventInteractionDelivered, runtime.EventInteractionExpired:
				if err := s.store.SetLoopRunState(runCtx, loop.ID, run.ID, "running", sequence); err != nil {
					finish("failed", "persist loop progress: "+err.Error())
					return
				}
			case runtime.EventInteractionRequested:
				if !interactionRequiresInput(event.Payload) {
					break
				}
				if err := s.store.SetLoopRunState(runCtx, loop.ID, run.ID, "blocked", sequence); err != nil {
					finish("failed", "persist blocked loop state: "+err.Error())
					return
				}
			case runtime.EventTurnCompleted:
				finish("completed", "")
				return
			case runtime.EventInputFailed, runtime.EventSessionProtocolError, runtime.EventSessionExited:
				finish("failed", fmt.Sprintf("loop session emitted %s: %s", event.Type, event.Payload))
				return
			}
		}
		select {
		case <-runCtx.Done():
			state := "failed"
			failure := "loop run timed out"
			if supervisorCtx.Err() != nil {
				state = "interrupted"
				failure = "agentd stopped during loop run"
			}
			finish(state, failure)
			return
		case _, open := <-updates:
			if !open {
				updates = nil
			}
		case <-poll.C:
		}
	}
}

func (s *Supervisor) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func buildPrompt(loop store.LoopRecord, run store.LoopRunRecord) string {
	return fmt.Sprintf(`You are running agentd loop %q, iteration %d. Complete exactly one bounded iteration in the current worktree.

Safety boundary:
- Read and follow project-local instructions before editing.
- Work only in this loop's current branch and worktree.
- Do not deploy, merge, push, publish, spend funds, use private keys, change secrets, or make external side effects.
- Do not weaken authentication, payment, approval, or production-safety boundaries.
- If the useful next action crosses a boundary above, request explicit user input and stop.
- Finish only after observable verification. Report the exact files changed, commands or scenarios verified, blockers, and risks.

Project loop contract:
%s`, loop.Name, run.Iteration, loop.Prompt)
}

func interactionRequiresInput(payload json.RawMessage) bool {
	var frame struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(payload, &frame) != nil {
		return true
	}
	switch frame.Method {
	case "setWidget", "cancel":
		return false
	default:
		return true
	}
}

func newID(prefix string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create identifier: %w", err)
	}
	return prefix + hex.EncodeToString(random[:]), nil
}
