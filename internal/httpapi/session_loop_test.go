package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/sessions"
	"github.com/dkta-labs/agentd/internal/store"
)

type blockingStartDriver struct {
	cancelled chan struct{}
}

func (d blockingStartDriver) Start(ctx context.Context, _ runtime.SessionSpec, _ runtime.Sink) (runtime.Session, error) {
	<-ctx.Done()
	close(d.cancelled)
	return nil, ctx.Err()
}

type exitOnCloseDriver struct{}

func (exitOnCloseDriver) Start(_ context.Context, spec runtime.SessionSpec, sink runtime.Sink) (runtime.Session, error) {
	return exitOnCloseSession{sessionID: spec.ID, sink: sink}, nil
}

type exitOnCloseSession struct {
	sessionID string
	sink      runtime.Sink
}

func (exitOnCloseSession) Send(context.Context, runtime.Input) error { return nil }
func (exitOnCloseSession) Respond(context.Context, runtime.InteractionResponse) error {
	return nil
}
func (exitOnCloseSession) Abort(context.Context) error { return nil }
func (s exitOnCloseSession) Close(ctx context.Context) error {
	return s.sink.Publish(ctx, runtime.Event{
		SessionID: s.sessionID,
		Type:      runtime.EventSessionExited,
		Payload:   json.RawMessage(`{"success":true}`),
		CreatedAt: time.Now().UTC(),
	})
}

func TestExplicitSessionIDRuntimeHonorsCallerDeadline(t *testing.T) {
	driver := blockingStartDriver{cancelled: make(chan struct{})}
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), events.NewBroker(nil), nil, staticCatalog{workspace}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := manager.StartWithHarnessAndID(ctx, workspace.ID, "omp", "ses_loop_timeout"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded start error = %v, want deadline exceeded", err)
	}
	select {
	case <-driver.cancelled:
	default:
		t.Fatal("loop runtime start did not observe its caller deadline")
	}
}

func TestExplicitSessionIDIsReservedBeforeRuntimeStart(t *testing.T) {
	driver := &recordingDriver{started: make(chan runtime.SessionSpec, 1)}
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), events.NewBroker(nil), nil, staticCatalog{workspace}, nil)
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.StartWithHarnessAndID(context.Background(), workspace.ID, "omp", "ses_loop_fixed")
	if err != nil {
		t.Fatal(err)
	}
	if started.ID != "ses_loop_fixed" || (<-driver.started).ID != "ses_loop_fixed" {
		t.Fatalf("explicit session ID was not preserved: %#v", started)
	}
	if _, err := manager.StartWithHarnessAndID(context.Background(), workspace.ID, "omp", "ses_loop_fixed"); !errors.Is(err, sessions.ErrAlreadyExists) {
		t.Fatalf("duplicate session error = %v, want %v", err, sessions.ErrAlreadyExists)
	}
}

func TestStoppedSessionRemainsReadableForDurableLoopEvidence(t *testing.T) {
	driver := &recordingDriver{started: make(chan runtime.SessionSpec, 1)}
	broker := events.NewBroker(nil)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	catalog := staticCatalog{workspace}
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), broker, nil, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(catalog, slog.New(slog.NewTextHandler(io.Discard, nil)), manager, broker, nil)
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.StartWithHarnessAndID(context.Background(), workspace.ID, "omp", "ses_loop_evidence")
	if err != nil {
		t.Fatal(err)
	}
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+started.ID, nil)
	deleteResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	getRequest := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+started.ID, nil)
	getResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get stopped status = %d, body = %s", getResponse.Code, getResponse.Body.String())
	}
	var retained sessions.Session
	if err := json.Unmarshal(getResponse.Body.Bytes(), &retained); err != nil {
		t.Fatal(err)
	}
	if retained.ID != started.ID || retained.State != "stopped" {
		t.Fatalf("retained session = %#v", retained)
	}
}

func TestPlannedProcessExitPersistsStoppedStateAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	broker := events.NewBroker(database)
	workspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	manager, err := sessions.NewManager(ctx, testHarnessRegistry(t, exitOnCloseDriver{}), broker, database, staticCatalog{workspace}, nil)
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.StartWithHarnessAndID(ctx, workspace.ID, "omp", "ses_loop_restart_evidence")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(ctx, started.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	records, err := database.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != started.ID || records[0].State != "stopped" {
		t.Fatalf("reopened sessions = %#v", records)
	}
}
