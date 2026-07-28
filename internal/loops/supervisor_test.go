package loops

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/sessions"
	"github.com/dkta-labs/agentd/internal/store"
)

type staticWorkspace struct {
	workspace config.Workspace
}

func (s staticWorkspace) Resolve(_ context.Context, id string) (config.Workspace, error) {
	if id != s.workspace.ID {
		return config.Workspace{}, context.Canceled
	}
	return s.workspace, nil
}

type completingSessions struct {
	broker *events.Broker
	mu     sync.Mutex
	prompt runtime.Input
	start  string
	stop   string
}

func (s *completingSessions) Harnesses() []runtime.Descriptor {
	return []runtime.Descriptor{{ID: "omp", Name: "OMP"}}
}

func (s *completingSessions) StartWithHarnessAndID(_ context.Context, workspaceID, harnessID, id string) (sessions.Session, error) {
	s.mu.Lock()
	s.start = id
	s.mu.Unlock()
	return sessions.Session{ID: id, WorkspaceID: workspaceID, HarnessID: harnessID, State: "idle"}, nil
}

func (s *completingSessions) Send(ctx context.Context, sessionID string, input runtime.Input) error {
	s.mu.Lock()
	s.prompt = input
	s.mu.Unlock()
	now := time.Now().UTC()
	if err := s.broker.Publish(ctx, runtime.Event{
		SessionID: sessionID, Type: runtime.EventTurnStarted,
		Payload: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return err
	}
	return s.broker.Publish(ctx, runtime.Event{
		SessionID: sessionID, Type: runtime.EventTurnCompleted,
		Payload: json.RawMessage(`{}`), CreatedAt: now.Add(time.Millisecond),
	})
}

func (s *completingSessions) Stop(_ context.Context, sessionID string) error {
	s.mu.Lock()
	s.stop = sessionID
	s.mu.Unlock()
	return nil
}

func TestManualLoopRunsOneBoundedSessionAndPersistsEvidencePointer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	broker := events.NewBroker(nil)
	manager := &completingSessions{broker: broker}
	workspace := config.Workspace{ID: "herdr:default:w1:test", Name: "Test loop", Path: t.TempDir()}
	supervisor, err := New(database, staticWorkspace{workspace: workspace}, manager, broker, nil)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.Start(ctx)
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
		defer closeCancel()
		if err := supervisor.Close(closeCtx); err != nil {
			t.Fatal(err)
		}
	}()
	created, err := supervisor.Create(ctx, CreateRequest{
		Name: "Bounded test", WorkspaceID: workspace.ID, Prompt: "make the smallest safe change",
		CadenceSeconds: 0, TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "paused" || created.DesiredState != "paused" {
		t.Fatalf("created loop = %#v", created)
	}
	if _, err := supervisor.RunLoop(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(4 * time.Second)
	var settled store.LoopRecord
	for time.Now().Before(deadline) {
		settled, err = supervisor.Get(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if settled.Iteration == 1 && settled.State == "paused" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if settled.Iteration != 1 || settled.State != "paused" || settled.ActiveSessionID != "" {
		t.Fatalf("settled loop = %#v", settled)
	}
	runs, err := supervisor.Runs(ctx, created.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != "completed" || runs[0].SessionID == "" || runs[0].LastEventSequence == 0 {
		t.Fatalf("loop runs = %#v", runs)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.start == "" || manager.stop != manager.start {
		t.Fatalf("session lifecycle start=%q stop=%q", manager.start, manager.stop)
	}
	if manager.prompt.IdempotencyKey != runs[0].IdempotencyKey || manager.prompt.Mode != runtime.InputPrompt {
		t.Fatalf("loop input = %#v", manager.prompt)
	}
	if !strings.Contains(manager.prompt.Text, "Do not deploy, merge, push") || !strings.Contains(manager.prompt.Text, "make the smallest safe change") {
		t.Fatalf("loop prompt omitted safety or project contract: %q", manager.prompt.Text)
	}
}

func TestOnlyBlockingOMPInteractionsPauseLoopState(t *testing.T) {
	for _, test := range []struct {
		payload string
		want    bool
	}{
		{payload: `{"method":"confirm"}`, want: true},
		{payload: `{"method":"select"}`, want: true},
		{payload: `{"method":"input"}`, want: true},
		{payload: `{"method":"editor"}`, want: true},
		{payload: `{"method":"setWidget"}`, want: false},
		{payload: `{"method":"futureApproval"}`, want: true},
		{payload: `{}`, want: true},
		{payload: `{`, want: true},
	} {
		if got := interactionRequiresInput(json.RawMessage(test.payload)); got != test.want {
			t.Errorf("interactionRequiresInput(%s) = %t, want %t", test.payload, got, test.want)
		}
	}
}
