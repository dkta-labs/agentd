package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/store"
)

var (
	ErrNotFound   = errors.New("session not found")
	ErrNotRunning = errors.New("session runtime is not running")
)

type Session struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId"`
	HarnessID   string    `json:"harnessId"`
	State       string    `json:"state"`
	NodeID      string    `json:"nodeId,omitempty"`
	NodeName    string    `json:"nodeName,omitempty"`
	Placement   string    `json:"placementReason,omitempty"`
	Remote      bool      `json:"remote"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type Interaction struct {
	ID          string     `json:"id"`
	Method      string     `json:"method"`
	Title       string     `json:"title"`
	Message     string     `json:"message,omitempty"`
	Options     []string   `json:"options,omitempty"`
	Placeholder string     `json:"placeholder,omitempty"`
	Prefill     string     `json:"prefill,omitempty"`
	Timeout     int64      `json:"timeout,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
}

type WorkspaceResolver interface {
	Resolve(context.Context, string) (config.Workspace, error)
}
type AttachmentController interface {
	Attach(context.Context, config.Workspace, string) error
	Report(context.Context, string, string, string)
	Detach(context.Context, string) error
}
type Notifier interface {
	Notify(sessionID, category, title, body string)
}

type Manager struct {
	ctx         context.Context
	cancel      context.CancelFunc
	harnesses   *runtime.Registry
	broker      *events.Broker
	persistence *store.DB
	workspaces  WorkspaceResolver
	attachments AttachmentController
	notifier    Notifier

	mu       sync.RWMutex
	sessions map[string]*managedSession
	inputs   map[string]map[string]store.InputRecord
}

type managedSession struct {
	metadata  Session
	runtime   runtime.Session
	normalize runtime.EventNormalizer
	closing   bool
}

func NewManager(
	ctx context.Context,
	harnesses *runtime.Registry,
	broker *events.Broker,
	persistence *store.DB,
	workspaces WorkspaceResolver,
	attachments AttachmentController,
	notifiers ...Notifier,
) (*Manager, error) {
	managerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	manager := &Manager{
		ctx:         managerCtx,
		cancel:      cancel,
		harnesses:   harnesses,
		broker:      broker,
		persistence: persistence,
		workspaces:  workspaces,
		attachments: attachments,
		sessions:    make(map[string]*managedSession),
		inputs:      make(map[string]map[string]store.InputRecord),
	}
	if len(notifiers) > 0 {
		manager.notifier = notifiers[0]
	}
	if harnesses == nil {
		cancel()
		return nil, errors.New("harness registry is required")
	}
	if _, err := harnesses.Resolve(""); err != nil {
		cancel()
		return nil, err
	}
	if persistence == nil {
		return manager, nil
	}
	records, err := persistence.Sessions(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	for _, record := range records {
		manager.sessions[record.ID] = &managedSession{metadata: sessionFromRecord(record)}
	}
	return manager, nil
}

func (m *Manager) Start(ctx context.Context, workspaceID string) (Session, error) {
	return m.StartWithHarness(ctx, workspaceID, "")
}

func (m *Manager) StartWithHarness(ctx context.Context, workspaceID, harnessID string) (Session, error) {
	workspace, err := m.workspaces.Resolve(ctx, workspaceID)
	if err != nil {
		return Session{}, err
	}
	return m.StartWorkspaceWithHarness(ctx, workspace, harnessID)
}

func (m *Manager) StartWorkspace(ctx context.Context, workspace config.Workspace) (Session, error) {
	return m.StartWorkspaceWithHarness(ctx, workspace, "")
}

func (m *Manager) StartWorkspaceWithHarness(ctx context.Context, workspace config.Workspace, harnessID string) (Session, error) {
	if workspace.ID == "" || workspace.Path == "" {
		return Session{}, errors.New("workspace ID and path are required")
	}
	registration, err := m.harnesses.Resolve(harnessID)
	if err != nil {
		return Session{}, err
	}
	id, err := newID()
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	metadata := Session{ID: id, WorkspaceID: workspace.ID, HarnessID: registration.Descriptor.ID, State: "starting", CreatedAt: now, UpdatedAt: now}
	if m.persistence != nil {
		if err := m.persistence.CreateSession(m.ctx, store.SessionRecord{
			ID:            id,
			WorkspaceID:   workspace.ID,
			WorkspacePath: workspace.Path,
			HarnessID:     registration.Descriptor.ID,
			State:         metadata.State,
			CreatedAt:     now,
			UpdatedAt:     now,
		}); err != nil {
			return Session{}, err
		}
	}
	m.mu.Lock()
	m.sessions[id] = &managedSession{metadata: metadata, normalize: registration.Normalize}
	m.mu.Unlock()

	running, err := registration.Driver.Start(m.ctx, runtime.SessionSpec{
		ID:            id,
		WorkspaceID:   workspace.ID,
		WorkspacePath: workspace.Path,
	}, sessionSink{manager: m, sessionID: id})
	if err != nil {
		_ = m.setState(ctx, id, "failed")
		return Session{}, err
	}
	attachLocally := true
	if preference, ok := running.(runtime.AttachmentPreference); ok {
		attachLocally = preference.AttachLocally()
	}
	if m.attachments != nil && attachLocally {
		if err := m.attachments.Attach(ctx, workspace, id); err != nil {
			_ = running.Close(context.Background())
			_ = m.setState(ctx, id, "failed")
			return Session{}, err
		}
	}
	m.mu.Lock()
	managed := m.sessions[id]
	managed.runtime = running
	managed.metadata.State = "idle"
	managed.metadata.UpdatedAt = time.Now().UTC()
	if placed, ok := running.(runtime.PlacedSession); ok {
		placement := placed.Placement()
		managed.metadata.NodeID = placement.NodeID
		managed.metadata.NodeName = placement.NodeName
		managed.metadata.Placement = placement.Reason
		managed.metadata.Remote = placement.Remote
	}
	metadata = managed.metadata
	m.mu.Unlock()
	if m.persistence != nil {
		if err := m.persistence.SetSessionPlacement(
			ctx,
			id,
			metadata.NodeID,
			metadata.NodeName,
			metadata.Placement,
			metadata.Remote,
		); err != nil {
			_ = running.Close(context.Background())
			return Session{}, err
		}
	}
	if err := m.setState(ctx, id, "idle"); err != nil {
		_ = running.Close(context.Background())
		return Session{}, err
	}
	return metadata, nil
}

func (m *Manager) Harnesses() []runtime.Descriptor {
	return m.harnesses.List()
}

func (m *Manager) List() []Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		result = append(result, session.metadata)
	}
	return result
}

func (m *Manager) Get(id string) (Session, error) {
	session, err := m.get(id)
	if err != nil {
		return Session{}, err
	}
	return session.metadata, nil
}

func (m *Manager) Send(ctx context.Context, sessionID string, input runtime.Input) error {
	session, err := m.getRunning(sessionID)
	if err != nil {
		return err
	}
	if input.IdempotencyKey == "" {
		if input.ID == "" {
			input.ID, err = newInputID()
			if err != nil {
				return err
			}
		}
		input.IdempotencyKey = input.ID
	} else if input.ID == "" {
		input.ID = inputIDForKey(input.IdempotencyKey)
	}
	reserved, created, err := m.reserveInput(ctx, sessionID, input)
	if err != nil {
		return err
	}
	if !created {
		if reserved.Status == "failed" {
			return fmt.Errorf("input %s previously failed: %s", reserved.ID, reserved.Error)
		}
		return nil
	}
	if err := session.runtime.Send(ctx, input); err != nil {
		completeErr := m.completeInputDetached(ctx, sessionID, input.IdempotencyKey, "failed", err.Error())
		if m.notifier != nil {
			m.notifier.Notify(sessionID, "failed", "OMP input failed", err.Error())
		}
		return errors.Join(err, completeErr)
	}
	if err := m.completeInputDetached(ctx, sessionID, input.IdempotencyKey, "delivered", ""); err != nil {
		slog.Error("persist input delivery marker failed", "sessionId", sessionID, "inputId", input.ID, "error", err)
	}
	if err := m.setState(ctx, sessionID, "running"); err != nil {
		slog.Error("persist running session state failed", "sessionId", sessionID, "inputId", input.ID, "error", err)
	}
	if m.attachments != nil {
		m.attachments.Report(ctx, sessionID, "working", "mobile prompt running")
	}
	return nil
}

func (m *Manager) PendingInteractions(ctx context.Context, sessionID string) ([]Interaction, error) {
	if _, err := m.get(sessionID); err != nil {
		return nil, err
	}
	stored, err := m.broker.After(ctx, sessionID, 0)
	if err != nil {
		return nil, err
	}
	_, _, pending := reduceInteractions(stored, time.Now().UTC())
	return pending, nil
}

func (m *Manager) Respond(
	ctx context.Context,
	sessionID string,
	response runtime.InteractionResponse,
) error {
	session, err := m.getRunning(sessionID)
	if err != nil {
		return err
	}
	created, err := m.reserveInteractionResponse(ctx, sessionID, response)
	if err != nil || !created {
		return err
	}
	if err := session.runtime.Respond(ctx, response); err != nil {
		completeErr := m.completeInteractionDetached(ctx, sessionID, response.RequestID, "failed", err.Error())
		if m.notifier != nil {
			m.notifier.Notify(sessionID, "failed", "OMP response failed", err.Error())
		}
		return errors.Join(err, completeErr)
	}
	durableCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := m.completeInteraction(durableCtx, sessionID, response.RequestID, "delivered", ""); err != nil {
		slog.Error("persist interaction delivery marker failed", "sessionId", sessionID, "requestId", response.RequestID, "error", err)
	}
	pending, pendingErr := m.PendingInteractions(durableCtx, sessionID)
	if pendingErr != nil {
		slog.Error("read pending interactions after response", "sessionId", sessionID, "requestId", response.RequestID, "error", pendingErr)
		return nil
	}
	state := "running"
	attachmentState := "working"
	message := "mobile response delivered"
	if len(pending) > 0 {
		state = "blocked"
		attachmentState = "blocked"
		message = "input required on mobile"
	}
	if err := m.setState(durableCtx, sessionID, state); err != nil {
		slog.Error("persist post-interaction session state failed", "sessionId", sessionID, "requestId", response.RequestID, "error", err)
	}
	if m.attachments != nil {
		m.attachments.Report(durableCtx, sessionID, attachmentState, message)
	}
	return nil
}

func (m *Manager) reserveInteractionResponse(
	ctx context.Context,
	sessionID string,
	response runtime.InteractionResponse,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, err := m.broker.After(ctx, sessionID, 0)
	if err != nil {
		return false, err
	}
	requests, states, _ := reduceInteractions(stored, time.Now().UTC())
	request, found := requests[response.RequestID]
	if !found {
		return false, fmt.Errorf("interaction request %q not found", response.RequestID)
	}
	switch states[response.RequestID] {
	case "responded", "cancelled":
		return false, nil
	case "expired":
		return false, fmt.Errorf("interaction request %q expired", response.RequestID)
	case "pending":
	default:
		return false, fmt.Errorf("interaction request %q has invalid state", response.RequestID)
	}
	if err := validateInteractionResponse(request, response); err != nil {
		return false, err
	}
	payload := map[string]any{
		"id":     response.RequestID,
		"status": "pending",
	}
	if response.Value != nil {
		payload["value"] = *response.Value
	}
	if response.Confirmed != nil {
		payload["confirmed"] = *response.Confirmed
	}
	if response.Cancelled {
		payload["cancelled"] = true
	}
	if err := m.publishEvent(ctx, sessionID, runtime.EventInteractionSubmitted, payload); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) completeInteractionDetached(
	ctx context.Context,
	sessionID string,
	requestID string,
	status string,
	failure string,
) error {
	durableCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return m.completeInteraction(durableCtx, sessionID, requestID, status, failure)
}

func (m *Manager) completeInteraction(
	ctx context.Context,
	sessionID string,
	requestID string,
	status string,
	failure string,
) error {
	eventType := runtime.EventInteractionDelivered
	if status == "failed" {
		eventType = runtime.EventInteractionFailed
	}
	return m.publishEvent(ctx, sessionID, eventType, map[string]any{
		"id":     requestID,
		"status": status,
		"error":  failure,
	})
}

func (m *Manager) reserveInput(
	ctx context.Context,
	sessionID string,
	input runtime.Input,
) (store.InputRecord, bool, error) {
	record := store.InputRecord{
		SessionID:      sessionID,
		ID:             input.ID,
		IdempotencyKey: input.IdempotencyKey,
		Mode:           input.Mode,
		Text:           input.Text,
	}
	if m.persistence != nil {
		reserved, event, created, err := m.persistence.ReserveInput(ctx, record)
		if err != nil {
			return store.InputRecord{}, false, err
		}
		if created {
			m.broker.Broadcast(event)
		}
		return reserved, created, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inputs[sessionID] == nil {
		m.inputs[sessionID] = make(map[string]store.InputRecord)
	}
	if existing, found := m.inputs[sessionID][input.IdempotencyKey]; found {
		if existing.Mode != input.Mode || existing.Text != input.Text {
			return store.InputRecord{}, false, store.ErrInputConflict
		}
		return existing, false, nil
	}
	record.Status = "pending"
	record.CreatedAt = time.Now().UTC()
	record.UpdatedAt = record.CreatedAt
	m.inputs[sessionID][input.IdempotencyKey] = record
	if err := m.publishEvent(ctx, sessionID, runtime.EventInputSubmitted, map[string]any{
		"id":     input.ID,
		"mode":   input.Mode,
		"text":   input.Text,
		"status": "pending",
	}); err != nil {
		delete(m.inputs[sessionID], input.IdempotencyKey)
		return store.InputRecord{}, false, err
	}
	return record, true, nil
}

func (m *Manager) completeInputDetached(
	ctx context.Context,
	sessionID string,
	idempotencyKey string,
	status string,
	failure string,
) error {
	durableCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return m.completeInput(durableCtx, sessionID, idempotencyKey, status, failure)
}

func (m *Manager) completeInput(
	ctx context.Context,
	sessionID string,
	idempotencyKey string,
	status string,
	failure string,
) error {
	if m.persistence != nil {
		event, err := m.persistence.CompleteInput(ctx, sessionID, idempotencyKey, status, failure)
		if err != nil {
			return err
		}
		m.broker.Broadcast(event)
		return nil
	}
	m.mu.Lock()
	record := m.inputs[sessionID][idempotencyKey]
	record.Status = status
	record.Error = failure
	record.UpdatedAt = time.Now().UTC()
	m.inputs[sessionID][idempotencyKey] = record
	m.mu.Unlock()
	eventType := runtime.EventInputDelivered
	if status == "failed" {
		eventType = runtime.EventInputFailed
	}
	return m.publishEvent(ctx, sessionID, eventType, map[string]any{
		"id":     record.ID,
		"status": status,
		"error":  failure,
	})
}

func (m *Manager) scheduleInteractionExpiry(
	sessionID string,
	requestID string,
	createdAt time.Time,
	timeoutMilliseconds int64,
) {
	if timeoutMilliseconds <= 0 {
		return
	}
	expiresAt := createdAt.Add(time.Duration(timeoutMilliseconds) * time.Millisecond)
	go func() {
		timer := time.NewTimer(time.Until(expiresAt))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-m.ctx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), 5*time.Second)
		defer cancel()
		m.expireInteraction(ctx, sessionID, requestID)
	}()
}

func (m *Manager) expireInteraction(ctx context.Context, sessionID string, requestID string) {
	m.mu.Lock()
	stored, err := m.broker.After(ctx, sessionID, 0)
	if err != nil {
		m.mu.Unlock()
		slog.Error("read interaction expiry state", "sessionId", sessionID, "requestId", requestID, "error", err)
		return
	}
	_, states, _ := reduceInteractions(stored, time.Now().UTC())
	if states[requestID] != "expired" {
		m.mu.Unlock()
		return
	}
	err = m.publishEvent(ctx, sessionID, runtime.EventInteractionExpired, map[string]any{
		"id":     requestID,
		"status": "expired",
	})
	m.mu.Unlock()
	if err != nil {
		slog.Error("persist interaction expiry", "sessionId", sessionID, "requestId", requestID, "error", err)
		return
	}
	pending, err := m.PendingInteractions(ctx, sessionID)
	if err != nil || len(pending) > 0 {
		return
	}
	session, err := m.Get(sessionID)
	if err != nil || session.State != "blocked" {
		return
	}
	if err := m.setState(ctx, sessionID, "running"); err != nil {
		slog.Error("persist post-expiry session state", "sessionId", sessionID, "requestId", requestID, "error", err)
	}
	if m.attachments != nil {
		m.attachments.Report(ctx, sessionID, "working", "OMP working")
	}
}

func reduceInteractions(stored []events.Event, now time.Time) (map[string]Interaction, map[string]string, []Interaction) {
	requests := make(map[string]Interaction)
	states := make(map[string]string)
	order := make([]string, 0)
	for _, event := range stored {
		switch event.Type {
		case runtime.EventInteractionRequested:
			var frame struct {
				ID          string   `json:"id"`
				Timeout     int64    `json:"timeout"`
				Method      string   `json:"method"`
				Title       string   `json:"title"`
				Message     string   `json:"message"`
				Options     []string `json:"options"`
				Placeholder string   `json:"placeholder"`
				TargetID    string   `json:"targetId"`
				Prefill     string   `json:"prefill"`
			}
			if json.Unmarshal(event.Payload, &frame) != nil {
				continue
			}
			if frame.Method == "cancel" {
				states[frame.TargetID] = "cancelled"
				continue
			}
			if frame.Method != "confirm" && frame.Method != "select" &&
				frame.Method != "input" && frame.Method != "editor" {
				continue
			}
			if _, found := requests[frame.ID]; !found {
				order = append(order, frame.ID)
			}
			request := Interaction{
				ID:          frame.ID,
				Method:      frame.Method,
				Title:       frame.Title,
				Message:     frame.Message,
				Options:     frame.Options,
				Placeholder: frame.Placeholder,
				Prefill:     frame.Prefill,
				Timeout:     frame.Timeout,
			}
			if frame.Timeout > 0 {
				expiresAt := event.CreatedAt.Add(time.Duration(frame.Timeout) * time.Millisecond)
				request.ExpiresAt = &expiresAt
			}
			requests[frame.ID] = request
			states[frame.ID] = "pending"
		case runtime.EventInteractionSubmitted:
			var frame struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event.Payload, &frame) == nil {
				states[frame.ID] = "responded"
			}
		case runtime.EventInteractionFailed:
			var frame struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event.Payload, &frame) == nil {
				states[frame.ID] = "pending"
			}
		case runtime.EventInteractionDelivered:
			var frame struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event.Payload, &frame) == nil {
				states[frame.ID] = "responded"
			}
		case runtime.EventInteractionExpired:
			var frame struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event.Payload, &frame) == nil {
				states[frame.ID] = "expired"
			}
		}
	}
	for id, request := range requests {
		if states[id] == "pending" && request.ExpiresAt != nil && !now.Before(*request.ExpiresAt) {
			states[id] = "expired"
		}
	}
	result := make([]Interaction, 0)
	for _, id := range order {
		if states[id] == "pending" {
			result = append(result, requests[id])
		}
	}
	return requests, states, result
}

func validateInteractionResponse(request Interaction, response runtime.InteractionResponse) error {
	variants := 0
	if response.Value != nil {
		variants++
	}
	if response.Confirmed != nil {
		variants++
	}
	if response.Cancelled {
		variants++
	}
	if variants != 1 {
		return errors.New("interaction response requires exactly one value, confirmation, or cancellation")
	}
	if response.Cancelled {
		return nil
	}
	switch request.Method {
	case "confirm":
		if response.Confirmed == nil {
			return errors.New("confirmation response requires confirmed")
		}
	case "select":
		if response.Value == nil {
			return errors.New("selection response requires value")
		}
		for _, option := range request.Options {
			if option == *response.Value {
				return nil
			}
		}
		return fmt.Errorf("selection %q is not an available option", *response.Value)
	case "input", "editor":
		if response.Value == nil {
			return errors.New("text response requires value")
		}
	default:
		return fmt.Errorf("unsupported interaction method %q", request.Method)
	}
	return nil
}

func (m *Manager) publishEvent(ctx context.Context, sessionID, eventType string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", eventType, err)
	}
	return m.broker.Publish(ctx, runtime.Event{
		SessionID: sessionID,
		Type:      eventType,
		Payload:   payload,
		CreatedAt: time.Now().UTC(),
	})
}

func (m *Manager) Abort(ctx context.Context, sessionID string) error {
	session, err := m.getRunning(sessionID)
	if err != nil {
		return err
	}
	return session.runtime.Abort(ctx)
}

func (m *Manager) Stop(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	session := m.sessions[sessionID]
	if session != nil {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	if session == nil {
		return ErrNotFound
	}
	var joined error
	joined = errors.Join(joined, m.setState(ctx, sessionID, "stopped"))
	results := make(chan error, 2)
	cleanups := 0
	if session.runtime != nil {
		cleanups++
		go func() { results <- session.runtime.Close(ctx) }()
	}
	if m.attachments != nil {
		cleanups++
		go func() { results <- m.detachForShutdown(ctx, sessionID) }()
	}
	for range cleanups {
		joined = errors.Join(joined, <-results)
	}
	return joined
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	running := make([]*managedSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		if session.runtime != nil {
			session.closing = true
			running = append(running, session)
		}
	}
	m.mu.Unlock()
	m.cancel()

	var joined error
	cleanups := len(running)
	if m.attachments != nil {
		cleanups += len(running)
	}
	results := make(chan error, cleanups)
	for _, session := range running {
		joined = errors.Join(joined, m.setState(ctx, session.metadata.ID, "stopped"))
		go func() { results <- session.runtime.Close(ctx) }()
		if m.attachments != nil {
			go func() { results <- m.detachForShutdown(ctx, session.metadata.ID) }()
		}
	}
	for range cleanups {
		joined = errors.Join(joined, <-results)
	}
	return joined
}

func (m *Manager) detachForShutdown(ctx context.Context, sessionID string) error {
	detachCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return m.attachments.Detach(detachCtx, sessionID)
}

func (m *Manager) get(id string) (*managedSession, error) {
	m.mu.RLock()
	session := m.sessions[id]
	m.mu.RUnlock()
	if session == nil {
		return nil, ErrNotFound
	}
	return session, nil
}

func (m *Manager) getRunning(id string) (*managedSession, error) {
	session, err := m.get(id)
	if err != nil {
		return nil, err
	}
	if session.runtime == nil {
		return nil, ErrNotRunning
	}
	return session, nil
}

func (m *Manager) setState(ctx context.Context, id, state string) error {
	now := time.Now().UTC()
	m.mu.Lock()
	if session := m.sessions[id]; session != nil {
		session.metadata.State = state
		session.metadata.UpdatedAt = now
	}
	m.mu.Unlock()
	if m.persistence != nil {
		return m.persistence.SetSessionState(ctx, id, state)
	}
	return nil
}

type sessionSink struct {
	manager   *Manager
	sessionID string
}

func (s sessionSink) Publish(ctx context.Context, event runtime.Event) error {
	s.manager.mu.RLock()
	session := s.manager.sessions[s.sessionID]
	s.manager.mu.RUnlock()
	harnessName := "Agent"
	if session != nil {
		harnessName = session.metadata.HarnessID
		if registration, err := s.manager.harnesses.Resolve(session.metadata.HarnessID); err == nil {
			harnessName = registration.Descriptor.Name
		}
	}
	if session != nil && session.normalize != nil {
		event = session.normalize(event)
	}
	if err := s.manager.broker.Publish(ctx, event); err != nil {
		return err
	}
	switch event.Type {
	case runtime.EventTurnStarted:
		_ = s.manager.setState(ctx, s.sessionID, "running")
		if s.manager.attachments != nil {
			s.manager.attachments.Report(ctx, s.sessionID, "working", harnessName+" working")
		}
	case runtime.EventInteractionRequested:
		var frame struct {
			Method  string `json:"method"`
			ID      string `json:"id"`
			Title   string `json:"title"`
			Timeout int64  `json:"timeout"`
		}
		if json.Unmarshal(event.Payload, &frame) != nil {
			break
		}
		switch frame.Method {
		case "confirm", "select", "input", "editor":
			_ = s.manager.setState(ctx, s.sessionID, "blocked")
			if s.manager.attachments != nil {
				s.manager.attachments.Report(ctx, s.sessionID, "blocked", "input required on mobile")
			}
			s.manager.scheduleInteractionExpiry(s.sessionID, frame.ID, event.CreatedAt, frame.Timeout)
			if s.manager.notifier != nil {
				title := frame.Title
				if title == "" {
					title = harnessName + " needs input"
				}
				s.manager.notifier.Notify(s.sessionID, "input_required", title, "Open agentd to answer.")
			}
		case "cancel":
			pending, err := s.manager.PendingInteractions(ctx, s.sessionID)
			if err == nil && len(pending) == 0 {
				_ = s.manager.setState(ctx, s.sessionID, "running")
				if s.manager.attachments != nil {
					s.manager.attachments.Report(ctx, s.sessionID, "working", harnessName+" working")
				}
			}
		}
	case runtime.EventTurnCompleted:
		_ = s.manager.setState(ctx, s.sessionID, "idle")
		if s.manager.attachments != nil {
			s.manager.attachments.Report(ctx, s.sessionID, "idle", "mobile session ready")
		}
		if s.manager.notifier != nil {
			s.manager.notifier.Notify(s.sessionID, "ready", harnessName+" is ready", "The current turn completed.")
		}
	case runtime.EventSessionProtocolError:
		if s.manager.notifier != nil {
			s.manager.notifier.Notify(s.sessionID, "failed", harnessName+" session failed", "The agent protocol connection failed.")
		}
	case runtime.EventSessionExited:
		s.manager.mu.RLock()
		session := s.manager.sessions[s.sessionID]
		expectedExit := session == nil || session.closing
		s.manager.mu.RUnlock()
		var frame struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(event.Payload, &frame) == nil && !frame.Success && !expectedExit && s.manager.notifier != nil {
			message := frame.Error
			if message == "" {
				message = "The agent process exited unexpectedly."
			}
			s.manager.notifier.Notify(s.sessionID, "failed", harnessName+" session failed", message)
		}
		if !expectedExit {
			_ = s.manager.setState(ctx, s.sessionID, "exited")
			if s.manager.attachments != nil {
				_ = s.manager.attachments.Detach(ctx, s.sessionID)
			}
		}
	}
	return nil
}

func sessionFromRecord(record store.SessionRecord) Session {
	return Session{
		ID:          record.ID,
		WorkspaceID: record.WorkspaceID,
		HarnessID:   record.HarnessID,
		State:       record.State,
		NodeID:      record.NodeID,
		NodeName:    record.NodeName,
		Placement:   record.PlacementReason,
		Remote:      record.Remote,
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
	}
}

func newID() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create session id: %w", err)
	}
	return "ses_" + hex.EncodeToString(random[:]), nil
}

func inputIDForKey(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "inp_" + hex.EncodeToString(digest[:12])
}

func newInputID() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create input id: %w", err)
	}
	return "inp_" + hex.EncodeToString(random[:]), nil
}
