package fleet

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/store"
)

func TestWorkerExecutesRemoteSessionAndReconnectsWithoutDuplicate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service := NewService(database, Options{EnrollmentToken: "worker-bootstrap-secret", Logger: testLogger()})
	mux := http.NewServeMux()
	service.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	enrollment, err := EnrollWorker(ctx, server.Client(), server.URL, EnrollRequest{Name: "test-node", Token: "worker-bootstrap-secret"})
	if err != nil {
		t.Fatal(err)
	}
	local := &fakeDriver{}
	worker, err := NewWorker(WorkerOptions{
		ControllerURL: server.URL,
		Token:         enrollment.Token,
		NodeID:        enrollment.Node.ID,
		Name:          enrollment.Node.Name,
		Workspaces:    []config.Workspace{{ID: "agentd", Name: "agentd", Path: t.TempDir()}},
		Driver:        local,
		Logger:        testLogger(),
		HTTPClient:    server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	waitFor(t, func() bool {
		nodes, listErr := service.List(ctx)
		return listErr == nil && len(nodes) == 1 && nodes[0].Online && len(nodes[0].Workspaces) == 1
	})

	sink := &recordingSink{}
	distributed := Driver{Service: service}
	startCtx, startCancel := context.WithTimeout(ctx, 5*time.Second)
	remote, err := distributed.Start(startCtx, runtime.SessionSpec{ID: "session-1", WorkspaceID: "agentd"}, sink)
	startCancel()
	if err != nil {
		t.Fatal(err)
	}
	if local.starts.Load() != 1 {
		t.Fatalf("expected one local worker process, got %d", local.starts.Load())
	}
	placement := remote.(runtime.PlacedSession).Placement()
	if !placement.Remote || placement.NodeID != enrollment.Node.ID {
		t.Fatalf("unexpected placement: %#v", placement)
	}

	stopWorker()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not disconnect")
	}
	sendDone := make(chan error, 1)
	go func() {
		sendCtx, sendCancel := context.WithTimeout(ctx, 8*time.Second)
		defer sendCancel()
		sendDone <- remote.Send(sendCtx, runtime.Input{ID: "input-1", IdempotencyKey: "input-1", Mode: runtime.InputPrompt, Text: "hello"})
	}()
	select {
	case err := <-sendDone:
		t.Fatalf("input completed while worker was disconnected: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	if local.starts.Load() != 1 {
		t.Fatalf("disconnect duplicated runtime: %d starts", local.starts.Load())
	}
	if local.session.closed.Load() || local.session.cancelled.Load() {
		t.Fatal("transport disconnect stopped the owned runtime")
	}
	workerCtx, stopWorker = context.WithCancel(ctx)
	workerDone = make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	if err := <-sendDone; err != nil {
		t.Fatal(err)
	}
	if local.session.sends.Load() != 1 {
		t.Fatalf("expected one delivered input, got %d", local.session.sends.Load())
	}
	if local.starts.Load() != 1 {
		t.Fatalf("reconnect duplicated runtime: %d starts", local.starts.Load())
	}

	closeCtx, closeCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := remote.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	closeCancel()
	service.mu.Lock()
	pending := len(service.commands[enrollment.Node.ID])
	service.mu.Unlock()
	if pending != 0 {
		t.Fatalf("acknowledged command queue retained %d commands", pending)
	}
	restartedDriver := &fakeDriver{}
	restartedWorker, err := NewWorker(WorkerOptions{
		ControllerURL: server.URL,
		Token:         enrollment.Token,
		NodeID:        enrollment.Node.ID,
		Name:          enrollment.Node.Name,
		Workspaces:    []config.Workspace{{ID: "agentd", Name: "agentd", Path: t.TempDir()}},
		Driver:        restartedDriver,
		Logger:        testLogger(),
		HTTPClient:    server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	restartCtx, stopRestart := context.WithCancel(ctx)
	restartDone := make(chan error, 1)
	go func() { restartDone <- restartedWorker.Run(restartCtx) }()
	time.Sleep(250 * time.Millisecond)
	stopRestart()
	<-restartDone
	if restartedDriver.starts.Load() != 0 {
		t.Fatalf("fresh worker replayed %d acknowledged starts", restartedDriver.starts.Load())
	}
	stopWorker()
	cancel()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestWorkerCommandReplayIsIdempotent(t *testing.T) {
	local := &fakeDriver{}
	worker, err := NewWorker(WorkerOptions{
		ControllerURL: "http://127.0.0.1",
		Token:         "token",
		NodeID:        "node",
		Workspaces:    []config.Workspace{{ID: "agentd", Path: t.TempDir()}},
		Driver:        local,
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(StartPayload{WorkspaceID: "agentd", HarnessID: "omp"})
	command := Command{ID: "command-1", Type: "start", SessionID: "session-1", Payload: payload}
	first := worker.executeCommand(context.Background(), command)
	second := worker.executeCommand(context.Background(), command)
	if first.Error != "" || second.Error != "" {
		t.Fatalf("unexpected replay error: %#v %#v", first, second)
	}
	if local.starts.Load() != 1 {
		t.Fatalf("replayed start created %d runtimes", local.starts.Load())
	}
}

type fakeDriver struct {
	starts  atomic.Int32
	session *fakeSession
}

func (d *fakeDriver) Start(ctx context.Context, _ runtime.SessionSpec, _ runtime.Sink) (runtime.Session, error) {
	d.starts.Add(1)
	d.session = &fakeSession{}
	if ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			d.session.cancelled.Store(true)
		}()
	}
	return d.session, nil
}

type fakeSession struct {
	sends     atomic.Int32
	closed    atomic.Bool
	cancelled atomic.Bool
}

func (s *fakeSession) Send(context.Context, runtime.Input) error {
	s.sends.Add(1)
	return nil
}
func (s *fakeSession) Respond(context.Context, runtime.InteractionResponse) error { return nil }
func (s *fakeSession) Abort(context.Context) error                                { return nil }
func (s *fakeSession) Close(context.Context) error {
	s.closed.Store(true)
	return nil
}

type recordingSink struct {
	mu     sync.Mutex
	events []runtime.Event
}

func (s *recordingSink) Publish(_ context.Context, event runtime.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition was not met")
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
