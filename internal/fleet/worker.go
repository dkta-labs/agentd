package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/runtime"
)

type WorkerOptions struct {
	ControllerURL string
	Token         string
	NodeID        string
	Name          string
	Workspaces    []config.Workspace
	Driver        runtime.Driver
	Attachments   WorkerAttachments
	Logger        *slog.Logger
	HTTPClient    *http.Client
}

type WorkerAttachments interface {
	Attach(context.Context, config.Workspace, string) error
	Detach(context.Context, string) error
}

type Worker struct {
	controller *url.URL
	token      string
	nodeID     string
	name       string
	workspaces map[string]config.Workspace
	driver     runtime.Driver
	attach     WorkerAttachments
	logger     *slog.Logger
	client     *http.Client
	events     chan EventEnvelope

	mu        sync.Mutex
	sessions  map[string]runtime.Session
	completed map[string]CommandResult
}

func NewWorker(options WorkerOptions) (*Worker, error) {
	controller, err := url.Parse(strings.TrimRight(options.ControllerURL, "/"))
	if err != nil || (controller.Scheme != "http" && controller.Scheme != "https") || controller.Host == "" {
		return nil, errors.New("controller URL must be an HTTP or HTTPS origin")
	}
	if options.Token == "" || options.NodeID == "" || options.Driver == nil {
		return nil, errors.New("worker token, node ID, and driver are required")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	workspaces := make(map[string]config.Workspace, len(options.Workspaces))
	for _, workspace := range options.Workspaces {
		workspaces[workspace.ID] = workspace
	}
	return &Worker{controller: controller, token: options.Token, nodeID: options.NodeID, name: options.Name, workspaces: workspaces, driver: options.Driver, attach: options.Attachments, logger: logger, client: client, events: make(chan EventEnvelope, 2048), sessions: make(map[string]runtime.Session), completed: make(map[string]CommandResult)}, nil
}

func EnrollWorker(ctx context.Context, client *http.Client, controllerURL string, request EnrollRequest) (EnrollResponse, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	var response EnrollResponse
	if err := requestJSON(ctx, client, http.MethodPost, strings.TrimRight(controllerURL, "/")+"/api/v1/workers/enroll", "", request, &response); err != nil {
		return EnrollResponse{}, err
	}
	return response, nil
}

func (w *Worker) Run(ctx context.Context) error {
	errChannel := make(chan error, 3)
	go func() { errChannel <- w.heartbeatLoop(ctx) }()
	go func() { errChannel <- w.eventLoop(ctx) }()
	go func() { errChannel <- w.commandLoop(ctx) }()
	select {
	case err := <-errChannel:
		if err == nil && ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (w *Worker) heartbeatLoop(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if err := w.sendHeartbeat(ctx); err != nil {
			w.logger.Warn("worker heartbeat failed", "error", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

func (w *Worker) sendHeartbeat(ctx context.Context) error {
	w.mu.Lock()
	active := len(w.sessions)
	w.mu.Unlock()
	request := HeartbeatRequest{OS: goruntime.GOOS, Architecture: goruntime.GOARCH, Capabilities: []string{"omp", "git"}, Workspaces: w.workspaceReplicas(ctx), OMPVersion: commandOutput(ctx, "omp", "--version"), ActiveRuns: active}
	return w.doJSON(ctx, http.MethodPost, "/api/v1/workers/heartbeat", request, nil)
}

func (w *Worker) workspaceReplicas(ctx context.Context) []WorkspaceReplica {
	result := make([]WorkspaceReplica, 0, len(w.workspaces))
	for _, workspace := range w.workspaces {
		revision := commandOutputAt(ctx, workspace.Path, "git", "rev-parse", "--short", "HEAD")
		dirty := commandOutputAt(ctx, workspace.Path, "git", "status", "--porcelain") != ""
		result = append(result, WorkspaceReplica{ID: workspace.ID, Name: workspace.Name, Path: workspace.Path, Revision: revision, Dirty: dirty})
	}
	return result
}

func (w *Worker) commandLoop(ctx context.Context) error {
	for ctx.Err() == nil {
		var commands []Command
		path := "/api/v1/workers/commands"
		if err := w.doJSON(ctx, http.MethodGet, path, nil, &commands); err != nil {
			w.logger.Warn("worker command poll failed", "error", err)
			if !sleepContext(ctx, time.Second) {
				break
			}
			continue
		}
		for _, command := range commands {
			result := w.executeCommand(ctx, command)
			for {
				err := w.doJSON(ctx, http.MethodPost, "/api/v1/workers/commands/"+url.PathEscape(command.ID)+"/result", result, nil)
				if err == nil {
					break
				}
				w.logger.Warn("worker command result failed", "commandId", command.ID, "error", err)
				if !sleepContext(ctx, time.Second) {
					return context.Cause(ctx)
				}
			}
		}
	}
	return context.Cause(ctx)
}

func (w *Worker) executeCommand(ctx context.Context, command Command) CommandResult {
	w.mu.Lock()
	if completed, ok := w.completed[command.ID]; ok {
		w.mu.Unlock()
		return completed
	}
	w.mu.Unlock()
	result := CommandResult{CommandID: command.ID}
	var err error
	switch command.Type {
	case "start":
		err = w.start(ctx, command)
	case "input":
		err = w.input(ctx, command)
	case "respond":
		err = w.respond(ctx, command)
	case "abort":
		err = w.withSession(command.SessionID, func(session runtime.Session) error { return session.Abort(ctx) })
	case "close":
		err = w.close(ctx, command.SessionID)
	default:
		err = fmt.Errorf("unknown worker command %q", command.Type)
	}
	if err != nil {
		result.Error = err.Error()
	}
	w.mu.Lock()
	w.completed[command.ID] = result
	w.mu.Unlock()
	return result
}

func (w *Worker) start(ctx context.Context, command Command) error {
	w.mu.Lock()
	_, exists := w.sessions[command.SessionID]
	w.mu.Unlock()
	if exists {
		return nil
	}
	var payload StartPayload
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		return err
	}
	workspace, ok := w.workspaces[payload.WorkspaceID]
	if !ok {
		return fmt.Errorf("workspace %q is unavailable on worker", payload.WorkspaceID)
	}
	session, err := w.driver.Start(context.WithoutCancel(ctx), runtime.SessionSpec{ID: command.SessionID, WorkspaceID: workspace.ID, WorkspacePath: workspace.Path}, workerSink{worker: w})
	if err != nil {
		return err
	}
	if w.attach != nil {
		if err := w.attach.Attach(ctx, workspace, command.SessionID); err != nil {
			_ = session.Close(context.Background())
			return err
		}
	}
	w.mu.Lock()
	w.sessions[command.SessionID] = session
	w.mu.Unlock()
	return nil
}

func (w *Worker) input(ctx context.Context, command Command) error {
	var payload InputPayload
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		return err
	}
	return w.withSession(command.SessionID, func(session runtime.Session) error {
		return session.Send(ctx, runtime.Input{ID: payload.ID, IdempotencyKey: payload.IdempotencyKey, Mode: runtime.InputMode(payload.Mode), Text: payload.Text})
	})
}

func (w *Worker) respond(ctx context.Context, command Command) error {
	var payload InteractionPayload
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		return err
	}
	return w.withSession(command.SessionID, func(session runtime.Session) error {
		return session.Respond(ctx, runtime.InteractionResponse{RequestID: payload.RequestID, Value: payload.Value, Confirmed: payload.Confirmed, Cancelled: payload.Cancelled})
	})
}

func (w *Worker) close(ctx context.Context, sessionID string) error {
	err := w.withSession(sessionID, func(session runtime.Session) error { return session.Close(ctx) })
	if err != nil {
		return err
	}
	if w.attach != nil {
		if err := w.attach.Detach(ctx, sessionID); err != nil {
			return err
		}
	}
	w.mu.Lock()
	delete(w.sessions, sessionID)
	w.mu.Unlock()
	return nil
}

func (w *Worker) withSession(sessionID string, operation func(runtime.Session) error) error {
	w.mu.Lock()
	session := w.sessions[sessionID]
	w.mu.Unlock()
	if session == nil {
		return fmt.Errorf("session %q is not running on worker", sessionID)
	}
	return operation(session)
}

func (w *Worker) eventLoop(ctx context.Context) error {
	for {
		select {
		case event := <-w.events:
			for {
				if err := w.doJSON(ctx, http.MethodPost, "/api/v1/workers/events", event, nil); err == nil {
					break
				} else {
					w.logger.Warn("worker event delivery failed", "sessionId", event.SessionID, "error", err)
				}
				if !sleepContext(ctx, time.Second) {
					return context.Cause(ctx)
				}
			}
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

func (w *Worker) Close(ctx context.Context) error {
	w.mu.Lock()
	sessions := make(map[string]runtime.Session, len(w.sessions))
	for id, session := range w.sessions {
		sessions[id] = session
	}
	w.mu.Unlock()
	var joined error
	for id, session := range sessions {
		if err := session.Close(ctx); err != nil {
			joined = errors.Join(joined, fmt.Errorf("close session %s: %w", id, err))
		}
		if w.attach != nil {
			if err := w.attach.Detach(ctx, id); err != nil {
				joined = errors.Join(joined, fmt.Errorf("detach session %s: %w", id, err))
			}
		}
		w.mu.Lock()
		delete(w.sessions, id)
		w.mu.Unlock()
	}
	return joined
}

func (w *Worker) doJSON(ctx context.Context, method, path string, body, destination any) error {
	return requestJSON(ctx, w.client, method, w.controller.String()+path, w.token, body, destination)
}

type workerSink struct{ worker *Worker }

func (s workerSink) Publish(ctx context.Context, event runtime.Event) error {
	eventID, err := randomToken(16)
	if err != nil {
		return err
	}
	envelope := EventEnvelope{ID: eventID, SessionID: event.SessionID, Type: event.Type, Payload: event.Payload, CreatedAt: event.CreatedAt}
	select {
	case s.worker.events <- envelope:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func requestJSON(ctx context.Context, client *http.Client, method, target, token string, body, destination any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("controller returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	if destination != nil {
		return json.NewDecoder(response.Body).Decode(destination)
	}
	return nil
}

func commandOutput(ctx context.Context, name string, args ...string) string {
	return commandOutputAt(ctx, "", name, args...)
}

func commandOutputAt(ctx context.Context, directory, name string, args ...string) string {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
