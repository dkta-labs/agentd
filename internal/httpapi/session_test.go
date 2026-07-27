package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/omp"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/sessions"
)

type recordingDriver struct {
	started      chan runtime.SessionSpec
	inputs       chan runtime.Input
	interactions chan runtime.InteractionResponse
	sendError    error
	sink         runtime.Sink
	newSession   func() runtime.Session
}

func (d *recordingDriver) Start(_ context.Context, spec runtime.SessionSpec, sink runtime.Sink) (runtime.Session, error) {
	d.started <- spec
	d.sink = sink
	if d.newSession != nil {
		return d.newSession(), nil
	}
	return &recordingSession{inputs: d.inputs, interactions: d.interactions, sendError: d.sendError}, nil
}

func testHarnessRegistry(t *testing.T, driver runtime.Driver) *runtime.Registry {
	t.Helper()
	registry, err := runtime.NewRegistry("omp", runtime.Registration{
		Descriptor: runtime.Descriptor{
			ID:           "omp",
			Name:         "OMP",
			Capabilities: []runtime.Capability{runtime.CapabilityPrompt, runtime.CapabilitySteer, runtime.CapabilityFollowUp, runtime.CapabilityAbort, runtime.CapabilityInteractions, runtime.CapabilityToolEvents},
		},
		Driver:    driver,
		Normalize: omp.NormalizeEvent,
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

type recordingSession struct {
	inputs       chan runtime.Input
	interactions chan runtime.InteractionResponse
	sendError    error
}

func (s *recordingSession) Send(_ context.Context, input runtime.Input) error {
	s.inputs <- input
	return s.sendError
}
func (s *recordingSession) Respond(_ context.Context, response runtime.InteractionResponse) error {
	if s.interactions != nil {
		s.interactions <- response
	}
	return nil
}
func (*recordingSession) Abort(context.Context) error { return nil }
func (*recordingSession) Close(context.Context) error { return nil }

type blockingCloseSession struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (*blockingCloseSession) Send(context.Context, runtime.Input) error { return nil }
func (*blockingCloseSession) Respond(context.Context, runtime.InteractionResponse) error {
	return nil
}
func (*blockingCloseSession) Abort(context.Context) error { return nil }
func (s *blockingCloseSession) Close(ctx context.Context) error {
	s.entered <- struct{}{}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type contextExitDriver struct {
	runtimeContext chan context.Context
	exited         chan struct{}
}

func (d *contextExitDriver) Start(ctx context.Context, spec runtime.SessionSpec, sink runtime.Sink) (runtime.Session, error) {
	d.runtimeContext <- ctx
	go func() {
		<-ctx.Done()
		_ = sink.Publish(context.Background(), runtime.Event{
			SessionID: spec.ID,
			Type:      "process_exit",
			Payload:   json.RawMessage(`{"success":false,"error":"signal: killed"}`),
			CreatedAt: time.Now().UTC(),
		})
		close(d.exited)
	}()
	return &recordingSession{}, nil
}

type recordedNotification struct {
	sessionID string
	category  string
	title     string
	body      string
}

type recordingNotifier chan recordedNotification

func (n recordingNotifier) Notify(sessionID, category, title, body string) {
	n <- recordedNotification{sessionID: sessionID, category: category, title: title, body: body}
}

type staticCatalog []config.Workspace

func (c staticCatalog) List(context.Context) []config.Workspace {
	return c
}

func (c staticCatalog) Resolve(_ context.Context, id string) (config.Workspace, error) {
	for _, workspace := range c {
		if workspace.ID == id {
			return workspace, nil
		}
	}
	return config.Workspace{}, sessions.ErrNotFound
}

func TestSessionStartAndPrompt(t *testing.T) {
	driver := &recordingDriver{
		started: make(chan runtime.SessionSpec, 1),
		inputs:  make(chan runtime.Input, 1),
	}
	broker := events.NewBroker(nil)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	catalog := staticCatalog{workspace}
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), broker, nil, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(catalog, logger, manager, broker, nil)
	if err != nil {
		t.Fatal(err)
	}

	startRequest := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"workspaceId":"gateway"}`))
	startResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(startResponse, startRequest)
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body = %s", startResponse.Code, startResponse.Body.String())
	}
	started := <-driver.started
	if started.WorkspacePath != workspace.Path {
		t.Fatalf("workspace path = %q, want %q", started.WorkspacePath, workspace.Path)
	}

	var startedSession sessions.Session
	if err := json.Unmarshal(startResponse.Body.Bytes(), &startedSession); err != nil {
		t.Fatal(err)
	}
	sessionID := startedSession.ID
	promptRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/sessions/"+sessionID+"/input",
		strings.NewReader(`{"mode":"prompt","text":"hello"}`),
	)
	promptResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(promptResponse, promptRequest)
	if promptResponse.Code != http.StatusAccepted {
		t.Fatalf("prompt status = %d, body = %s", promptResponse.Code, promptResponse.Body.String())
	}
	input := <-driver.inputs
	if input.Mode != runtime.InputPrompt || input.Text != "hello" {
		t.Fatalf("unexpected input: %#v", input)
	}
}

func TestInteractionResponseIsValidatedAndDispatchedOnce(t *testing.T) {
	driver := &recordingDriver{
		started:      make(chan runtime.SessionSpec, 1),
		inputs:       make(chan runtime.Input, 1),
		interactions: make(chan runtime.InteractionResponse, 1),
	}
	broker := events.NewBroker(nil)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	catalog := staticCatalog{workspace}
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), broker, nil, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(catalog, logger, manager, broker, nil)
	if err != nil {
		t.Fatal(err)
	}
	startRequest := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"workspaceId":"gateway"}`))
	startResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(startResponse, startRequest)
	var started sessions.Session
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if err := broker.Publish(context.Background(), runtime.Event{
		SessionID: started.ID,
		Type:      runtime.EventInteractionRequested,
		Payload:   json.RawMessage(`{"type":"extension_ui_request","id":"confirm-1","method":"confirm","title":"Deploy?","message":"Proceed now?"}`),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := manager.PendingInteractions(context.Background(), started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "confirm-1" || pending[0].Method != "confirm" {
		t.Fatalf("pending interactions = %#v", pending)
	}
	respond := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/sessions/"+started.ID+"/interactions/confirm-1/response",
			strings.NewReader(`{"confirmed":false}`),
		)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	for attempt := range 2 {
		response := respond()
		if response.Code != http.StatusAccepted {
			t.Fatalf("response %d status = %d, body = %s", attempt, response.Code, response.Body.String())
		}
	}
	delivered := <-driver.interactions
	if delivered.RequestID != "confirm-1" || delivered.Confirmed == nil || *delivered.Confirmed {
		t.Fatalf("delivered interaction = %#v", delivered)
	}
	select {
	case duplicate := <-driver.interactions:
		t.Fatalf("interaction redispatched: %#v", duplicate)
	default:
	}
	pending, err = manager.PendingInteractions(context.Background(), started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("resolved interactions remain pending: %#v", pending)
	}
	replay, err := broker.After(context.Background(), started.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 3 ||
		replay[0].Type != runtime.EventInteractionRequested ||
		replay[1].Type != runtime.EventInteractionSubmitted ||
		replay[2].Type != runtime.EventInteractionDelivered {
		t.Fatalf("interaction replay = %#v", replay)
	}
}

func TestTimedInteractionExpiresAndUnblocksSession(t *testing.T) {
	driver := &recordingDriver{
		started:      make(chan runtime.SessionSpec, 1),
		inputs:       make(chan runtime.Input, 1),
		interactions: make(chan runtime.InteractionResponse, 1),
	}
	broker := events.NewBroker(nil)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	catalog := staticCatalog{workspace}
	notifier := make(recordingNotifier, 1)
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), broker, nil, catalog, nil, notifier)
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.Start(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.sink.Publish(context.Background(), runtime.Event{
		SessionID: started.ID,
		Type:      "extension_ui_request",
		Payload:   json.RawMessage(`{"type":"extension_ui_request","id":"timed-1","method":"confirm","title":"Quick","message":"Answer quickly","timeout":10}`),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case notification := <-notifier:
		if notification.sessionID != started.ID || notification.category != "input_required" || notification.title != "Quick" {
			t.Fatalf("unexpected input-required notification: %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("input-required notification was not emitted")
	}
	deadline := time.Now().Add(time.Second)
	for {
		replay, replayErr := broker.After(context.Background(), started.ID, 0)
		if replayErr != nil {
			t.Fatal(replayErr)
		}
		if len(replay) == 2 && replay[1].Type == runtime.EventInteractionExpired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed interaction did not expire: %#v", replay)
		}
		time.Sleep(5 * time.Millisecond)
	}
	pending, err := manager.PendingInteractions(context.Background(), started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("expired interaction remains pending: %#v", pending)
	}
	session, err := manager.Get(started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.State != "running" {
		t.Fatalf("post-expiry state = %q, want running", session.State)
	}
	confirmed := true
	err = manager.Respond(context.Background(), started.ID, runtime.InteractionResponse{
		RequestID: "timed-1",
		Confirmed: &confirmed,
	})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired response error = %v", err)
	}
	select {
	case response := <-driver.interactions:
		t.Fatalf("expired interaction dispatched: %#v", response)
	default:
	}
	if err := driver.sink.Publish(context.Background(), runtime.Event{
		SessionID: started.ID,
		Type:      "agent_end",
		Payload:   json.RawMessage(`{}`),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case notification := <-notifier:
		if notification.category != "ready" || notification.title != "OMP is ready" {
			t.Fatalf("unexpected ready notification: %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("ready notification was not emitted")
	}
}

func TestProcessExitNotificationDistinguishesFailure(t *testing.T) {
	driver := &recordingDriver{
		started: make(chan runtime.SessionSpec, 1),
		inputs:  make(chan runtime.Input, 1),
	}
	broker := events.NewBroker(nil)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	notifier := make(recordingNotifier, 2)
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), broker, nil, staticCatalog{workspace}, nil, notifier)
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.Start(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	publishExit := func(payload string) {
		t.Helper()
		if err := driver.sink.Publish(context.Background(), runtime.Event{
			SessionID: started.ID,
			Type:      "process_exit",
			Payload:   json.RawMessage(payload),
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	publishExit(`{"success":true}`)
	select {
	case notification := <-notifier:
		t.Fatalf("successful process exit notified: %#v", notification)
	default:
	}
	publishExit(`{"success":false,"error":"exit status 2"}`)
	select {
	case notification := <-notifier:
		if notification.category != "failed" || notification.body != "exit status 2" {
			t.Fatalf("unexpected crash notification: %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("process crash notification was not emitted")
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	publishExit(`{"success":false,"error":"signal: killed"}`)
	select {
	case notification := <-notifier:
		t.Fatalf("intentional manager shutdown notified: %#v", notification)
	default:
	}
	settled, err := manager.Get(started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != "stopped" {
		t.Fatalf("planned process exit state = %q, want stopped", settled.State)
	}
}

func TestInputIdempotencyKeyDispatchesExactlyOnce(t *testing.T) {
	driver := &recordingDriver{
		started: make(chan runtime.SessionSpec, 1),
		inputs:  make(chan runtime.Input, 1),
	}
	broker := events.NewBroker(nil)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	catalog := staticCatalog{workspace}
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), broker, nil, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(catalog, logger, manager, broker, nil)
	if err != nil {
		t.Fatal(err)
	}
	startRequest := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"workspaceId":"gateway"}`))
	startResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(startResponse, startRequest)
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body = %s", startResponse.Code, startResponse.Body.String())
	}
	var started sessions.Session
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	send := func(text string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/sessions/"+started.ID+"/input",
			strings.NewReader(`{"idempotencyKey":"stable-submit","mode":"prompt","text":"`+text+`"}`),
		)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	for attempt := range 2 {
		response := send("once")
		if response.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status = %d, body = %s", attempt, response.Code, response.Body.String())
		}
	}
	input := <-driver.inputs
	if input.IdempotencyKey != "stable-submit" || input.Text != "once" {
		t.Fatalf("unexpected dispatched input: %#v", input)
	}
	select {
	case duplicate := <-driver.inputs:
		t.Fatalf("duplicate input dispatched: %#v", duplicate)
	default:
	}
	conflict := send("different")
	if conflict.Code != http.StatusBadRequest {
		t.Fatalf("conflicting retry status = %d, want %d", conflict.Code, http.StatusBadRequest)
	}
	replay, err := broker.After(context.Background(), started.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 2 || replay[0].Type != runtime.EventInputSubmitted || replay[1].Type != runtime.EventInputDelivered {
		t.Fatalf("idempotent replay = %#v", replay)
	}
}

func TestFailedInputIntentIsDurableAndNotRedispatched(t *testing.T) {
	driver := &recordingDriver{
		started:   make(chan runtime.SessionSpec, 1),
		inputs:    make(chan runtime.Input, 1),
		sendError: io.ErrClosedPipe,
	}
	broker := events.NewBroker(nil)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	catalog := staticCatalog{workspace}
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), broker, nil, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(catalog, logger, manager, broker, nil)
	if err != nil {
		t.Fatal(err)
	}
	startRequest := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"workspaceId":"gateway"}`))
	startResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(startResponse, startRequest)
	var started sessions.Session
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	send := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/sessions/"+started.ID+"/input",
			strings.NewReader(`{"idempotencyKey":"failed-submit","mode":"prompt","text":"fails once"}`),
		)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if response := send(); response.Code != http.StatusBadRequest {
		t.Fatalf("failed input status = %d, body = %s", response.Code, response.Body.String())
	}
	<-driver.inputs
	if response := send(); response.Code != http.StatusBadRequest {
		t.Fatalf("failed retry status = %d, body = %s", response.Code, response.Body.String())
	}
	select {
	case duplicate := <-driver.inputs:
		t.Fatalf("failed input redispatched: %#v", duplicate)
	default:
	}
	replay, err := broker.After(context.Background(), started.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 2 || replay[0].Type != runtime.EventInputSubmitted || replay[1].Type != runtime.EventInputFailed {
		t.Fatalf("failed input replay = %#v", replay)
	}
	var failed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(replay[1].Payload, &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.Error == "" {
		t.Fatalf("failed input payload = %#v", failed)
	}
}

func TestSessionReplayRestoresUserAndAssistantExactlyOnce(t *testing.T) {
	driver := &recordingDriver{
		started: make(chan runtime.SessionSpec, 1),
		inputs:  make(chan runtime.Input, 1),
	}
	broker := events.NewBroker(nil)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	catalog := staticCatalog{workspace}
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), broker, nil, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(catalog, logger, manager, broker, nil)
	if err != nil {
		t.Fatal(err)
	}

	startRequest := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(`{"workspaceId":"gateway"}`))
	startResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(startResponse, startRequest)
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body = %s", startResponse.Code, startResponse.Body.String())
	}
	var started sessions.Session
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	inputRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/sessions/"+started.ID+"/input",
		strings.NewReader(`{"mode":"prompt","text":"persist me"}`),
	)
	inputResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(inputResponse, inputRequest)
	if inputResponse.Code != http.StatusAccepted {
		t.Fatalf("input status = %d, body = %s", inputResponse.Code, inputResponse.Body.String())
	}
	if err := broker.Publish(context.Background(), runtime.Event{
		SessionID: started.ID,
		Type:      runtime.EventAssistantDelta,
		Payload:   json.RawMessage(`{"assistantMessageEvent":{"delta":"restored answer"}}`),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	replay := func() []events.Event {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+started.ID+"/events?after=0", nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("replay status = %d, body = %s", response.Code, response.Body.String())
		}
		var restored []events.Event
		if err := json.Unmarshal(response.Body.Bytes(), &restored); err != nil {
			t.Fatal(err)
		}
		return restored
	}

	for attempt := range 2 {
		restored := replay()
		if len(restored) != 3 {
			t.Fatalf("reopen %d restored %d events, want 3: %#v", attempt, len(restored), restored)
		}
		if restored[0].Type != runtime.EventInputSubmitted ||
			restored[1].Type != runtime.EventInputDelivered ||
			restored[2].Type != runtime.EventAssistantDelta {
			t.Fatalf("reopen %d restored wrong event order: %#v", attempt, restored)
		}
		var userInput struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(restored[0].Payload, &userInput); err != nil {
			t.Fatal(err)
		}
		if userInput.Text != "persist me" {
			t.Fatalf("reopen %d user input = %q", attempt, userInput.Text)
		}
		var assistantUpdate struct {
			AssistantMessageEvent struct {
				Delta string `json:"delta"`
			} `json:"assistantMessageEvent"`
		}
		if err := json.Unmarshal(restored[2].Payload, &assistantUpdate); err != nil {
			t.Fatal(err)
		}
		if assistantUpdate.AssistantMessageEvent.Delta != "restored answer" {
			t.Fatalf("reopen %d assistant response = %q", attempt, assistantUpdate.AssistantMessageEvent.Delta)
		}
	}
}

func TestManagerCloseStopsSessionsConcurrently(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	driver := &recordingDriver{
		started: make(chan runtime.SessionSpec, 2),
		newSession: func() runtime.Session {
			return &blockingCloseSession{entered: entered, release: release}
		},
	}
	broker := events.NewBroker(nil)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), broker, nil, staticCatalog{workspace}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := manager.Start(context.Background(), workspace.ID); err != nil {
			t.Fatal(err)
		}
	}

	closed := make(chan error, 1)
	go func() {
		closed <- manager.Close(context.Background())
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("session shutdown was serialized")
		}
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("manager shutdown did not complete")
	}
}

func TestParentCancellationCannotRacePlannedSessionShutdown(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	driver := &contextExitDriver{
		runtimeContext: make(chan context.Context, 1),
		exited:         make(chan struct{}),
	}
	notifier := make(recordingNotifier, 1)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	manager, err := sessions.NewManager(parent, testHarnessRegistry(t, driver), events.NewBroker(nil), nil, staticCatalog{workspace}, nil, notifier)
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.Start(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeContext := <-driver.runtimeContext
	cancelParent()
	if runtimeContext.Err() != nil {
		t.Fatal("parent cancellation reached the runtime before Manager.Close")
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-driver.exited:
	case <-time.After(time.Second):
		t.Fatal("manager shutdown did not cancel the runtime context")
	}
	select {
	case notification := <-notifier:
		t.Fatalf("planned runtime cancellation notified: %#v", notification)
	default:
	}
	settled, err := manager.Get(started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != "stopped" {
		t.Fatalf("planned runtime cancellation state = %q, want stopped", settled.State)
	}
}
