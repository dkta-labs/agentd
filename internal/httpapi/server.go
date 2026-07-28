package httpapi

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/devices"
	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/loops"
	"github.com/dkta-labs/agentd/internal/mcp"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/sessions"
	"github.com/dkta-labs/agentd/internal/store"
	"github.com/dkta-labs/agentd/internal/surface"
)

//go:embed web/*
var webFiles embed.FS

type WorkspaceCatalog interface {
	List(context.Context) []config.Workspace
}

type SurfaceOperations interface {
	Fleet(context.Context) (surface.Fleet, error)
	TargetOutput(context.Context, string, string, int) (surface.Output, error)
	FocusTarget(context.Context, string, string) error
	SendToTarget(context.Context, string, string, string) error
	CloseTarget(context.Context, string, string) error
	WorkspaceForTarget(context.Context, string, string) (config.Workspace, error)
}

type FleetAPI interface {
	Register(*http.ServeMux)
}

type LoopAPI interface {
	Create(context.Context, loops.CreateRequest) (store.LoopRecord, error)
	Update(context.Context, string, loops.CreateRequest) (store.LoopRecord, error)
	List(context.Context) ([]store.LoopRecord, error)
	Get(context.Context, string) (store.LoopRecord, error)
	Runs(context.Context, string, int) ([]store.LoopRunRecord, error)
	ResumeLoop(context.Context, string) (store.LoopRecord, error)
	PauseLoop(context.Context, string) (store.LoopRecord, error)
	RunLoop(context.Context, string) (store.LoopRecord, error)
}

type Server struct {
	handler http.Handler
}

func New(
	catalog WorkspaceCatalog,
	logger *slog.Logger,
	sessionManager *sessions.Manager,
	broker *events.Broker,
	mcpHandler http.Handler,
	deviceServices ...*devices.Service,
) (*Server, error) {
	return NewWithFleetAndLoops(catalog, logger, sessionManager, broker, mcpHandler, nil, nil, deviceServices...)
}

func NewWithFleet(
	catalog WorkspaceCatalog,
	logger *slog.Logger,
	sessionManager *sessions.Manager,
	broker *events.Broker,
	mcpHandler http.Handler,
	fleetAPI FleetAPI,
	deviceServices ...*devices.Service,
) (*Server, error) {
	return NewWithFleetAndLoops(catalog, logger, sessionManager, broker, mcpHandler, fleetAPI, nil, deviceServices...)
}

func NewWithFleetAndLoops(
	catalog WorkspaceCatalog,
	logger *slog.Logger,
	sessionManager *sessions.Manager,
	broker *events.Broker,
	mcpHandler http.Handler,
	fleetAPI FleetAPI,
	loopAPI LoopAPI,
	deviceServices ...*devices.Service,
) (*Server, error) {
	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		return nil, err
	}
	if len(deviceServices) > 1 {
		return nil, errors.New("only one device service may be configured")
	}
	var deviceService *devices.Service
	if len(deviceServices) == 1 {
		deviceService = deviceServices[0]
	}

	mux := http.NewServeMux()
	if fleetAPI != nil {
		fleetAPI.Register(mux)
	}
	surfaceOperations, _ := catalog.(SurfaceOperations)
	var targetEvents *targetEventBroker
	if surfaceOperations != nil {
		targetEvents = newTargetEventBroker(surfaceOperations, targetEventPollInterval)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/auth/status", func(w http.ResponseWriter, r *http.Request) {
		authenticated := false
		if deviceService != nil && deviceService.Enabled() {
			_, _, authenticatedError := deviceService.AuthenticateRequest(r)
			authenticated = authenticatedError == nil
		}
		writeJSON(w, http.StatusOK, map[string]bool{
			"enabled":       deviceService != nil && deviceService.Enabled(),
			"authenticated": authenticated,
			"pushEnabled":   deviceService != nil && deviceService.PushEnabled(),
		})
	})
	if deviceService != nil && deviceService.Enabled() {
		mux.HandleFunc("POST /api/v1/devices/enroll", func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Name  string `json:"name"`
				Token string `json:"token"`
			}
			if err := decodeJSON(r.Body, &request); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			device, token, err := deviceService.Enroll(r.Context(), request.Name, request.Token)
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, devices.ErrEnrollmentDenied) {
					status = http.StatusUnauthorized
				} else if errors.Is(err, store.ErrEnrollmentTokenUsed) {
					status = http.StatusConflict
				}
				writeError(w, status, err)
				return
			}
			deviceService.SetCookie(w, token)
			writeJSON(w, http.StatusCreated, map[string]any{"device": device, "token": token})
		})
		mux.HandleFunc("GET /api/v1/devices", func(w http.ResponseWriter, r *http.Request) {
			available, err := deviceService.List(r.Context())
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, http.StatusOK, available)
		})
		mux.HandleFunc("DELETE /api/v1/devices/{deviceID}", func(w http.ResponseWriter, r *http.Request) {
			if err := deviceService.Revoke(r.Context(), r.PathValue("deviceID")); err != nil {
				status := http.StatusInternalServerError
				if errors.Is(err, sql.ErrNoRows) {
					status = http.StatusNotFound
				}
				writeError(w, status, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("GET /api/v1/device", func(w http.ResponseWriter, r *http.Request) {
			device, ok := devices.FromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errors.New("device authentication required"))
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"device":         device,
				"pushEnabled":    deviceService.PushEnabled(),
				"vapidPublicKey": deviceService.VAPIDPublicKey(),
			})
		})
		mux.HandleFunc("PUT /api/v1/device/preferences", func(w http.ResponseWriter, r *http.Request) {
			device, ok := devices.FromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errors.New("device authentication required"))
				return
			}
			var request struct {
				Ready         bool `json:"ready"`
				InputRequired bool `json:"inputRequired"`
				Failed        bool `json:"failed"`
			}
			if err := decodeJSON(r.Body, &request); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			if err := deviceService.UpdatePreferences(r.Context(), device.ID, request.Ready, request.InputRequired, request.Failed); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("PUT /api/v1/device/push", func(w http.ResponseWriter, r *http.Request) {
			device, ok := devices.FromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errors.New("device authentication required"))
				return
			}
			var subscription devices.PushSubscription
			if err := decodeJSON(r.Body, &subscription); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			if err := deviceService.Subscribe(r.Context(), device.ID, subscription); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("DELETE /api/v1/device/push", func(w http.ResponseWriter, r *http.Request) {
			device, ok := devices.FromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errors.New("device authentication required"))
				return
			}
			if err := deviceService.Unsubscribe(r.Context(), device.ID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("GET /api/v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		var harnesses []runtime.Descriptor
		if sessionManager != nil {
			harnesses = sessionManager.Harnesses()
		}
		tools := mcp.Tools()
		if loopAPI != nil {
			tools = mcp.ToolsWithLoops()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"harnesses": harnesses,
			"tools":     tools,
		})
	})
	mux.HandleFunc("GET /api/v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		type workspaceResponse struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		available := catalog.List(r.Context())
		workspaces := make([]workspaceResponse, 0, len(available))
		for _, workspace := range available {
			workspaces = append(workspaces, workspaceResponse{ID: workspace.ID, Name: workspace.Name})
		}
		writeJSON(w, http.StatusOK, workspaces)
	})
	mux.HandleFunc("GET /api/v1/loops", func(w http.ResponseWriter, r *http.Request) {
		if loopAPI == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("loop supervisor is unavailable"))
			return
		}
		available, err := loopAPI.List(r.Context())
		if err != nil {
			writeLoopError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, available)
	})
	mux.HandleFunc("POST /api/v1/loops", func(w http.ResponseWriter, r *http.Request) {
		if loopAPI == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("loop supervisor is unavailable"))
			return
		}
		var request loops.CreateRequest
		if err := decodeJSON(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		created, err := loopAPI.Create(r.Context(), request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	})
	mux.HandleFunc("GET /api/v1/loops/{loopID}", func(w http.ResponseWriter, r *http.Request) {
		if loopAPI == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("loop supervisor is unavailable"))
			return
		}
		record, err := loopAPI.Get(r.Context(), r.PathValue("loopID"))
		if err != nil {
			writeLoopError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, record)
	})
	mux.HandleFunc("PUT /api/v1/loops/{loopID}", func(w http.ResponseWriter, r *http.Request) {
		if loopAPI == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("loop supervisor is unavailable"))
			return
		}
		var request loops.CreateRequest
		if err := decodeJSON(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		updated, err := loopAPI.Update(r.Context(), r.PathValue("loopID"), request)
		if err != nil {
			writeLoopError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})
	mux.HandleFunc("GET /api/v1/loops/{loopID}/runs", func(w http.ResponseWriter, r *http.Request) {
		if loopAPI == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("loop supervisor is unavailable"))
			return
		}
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil && r.URL.Query().Get("limit") != "" {
			writeError(w, http.StatusBadRequest, errors.New("limit must be an integer"))
			return
		}
		runs, err := loopAPI.Runs(r.Context(), r.PathValue("loopID"), limit)
		if err != nil {
			writeLoopError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, runs)
	})
	mux.HandleFunc("POST /api/v1/loops/{loopID}/start", func(w http.ResponseWriter, r *http.Request) {
		if loopAPI == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("loop supervisor is unavailable"))
			return
		}
		record, err := loopAPI.ResumeLoop(r.Context(), r.PathValue("loopID"))
		if err != nil {
			writeLoopError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, record)
	})
	mux.HandleFunc("POST /api/v1/loops/{loopID}/pause", func(w http.ResponseWriter, r *http.Request) {
		if loopAPI == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("loop supervisor is unavailable"))
			return
		}
		record, err := loopAPI.PauseLoop(r.Context(), r.PathValue("loopID"))
		if err != nil {
			writeLoopError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, record)
	})
	mux.HandleFunc("POST /api/v1/loops/{loopID}/run", func(w http.ResponseWriter, r *http.Request) {
		if loopAPI == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("loop supervisor is unavailable"))
			return
		}
		record, err := loopAPI.RunLoop(r.Context(), r.PathValue("loopID"))
		if err != nil {
			writeLoopError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, record)
	})
	mux.HandleFunc("GET /api/v1/surfaces", func(w http.ResponseWriter, r *http.Request) {
		if surfaceOperations == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("surface operations are unavailable"))
			return
		}
		fleet, err := surfaceOperations.Fleet(r.Context())
		if err != nil {
			writeSurfaceUpstreamError(w, logger, "read fleet", err)
			return
		}
		writeJSON(w, http.StatusOK, fleet)
	})
	mux.HandleFunc("GET /api/v1/surfaces/{surfaceID}/targets/{targetID}/output", func(w http.ResponseWriter, r *http.Request) {
		if surfaceOperations == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("surface operations are unavailable"))
			return
		}
		lines, err := strconv.Atoi(r.URL.Query().Get("lines"))
		if err != nil && r.URL.Query().Get("lines") != "" {
			writeError(w, http.StatusBadRequest, errors.New("lines must be an integer"))
			return
		}
		output, err := surfaceOperations.TargetOutput(r.Context(), r.PathValue("surfaceID"), r.PathValue("targetID"), lines)
		if err != nil {
			writeSurfaceError(w, logger, err)
			return
		}
		writeJSON(w, http.StatusOK, output)
	})
	mux.HandleFunc("GET /api/v1/surfaces/{surfaceID}/targets/{targetID}/events", func(w http.ResponseWriter, r *http.Request) {
		if targetEvents == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("surface operations are unavailable"))
			return
		}
		if _, err := surfaceOperations.TargetOutput(r.Context(), r.PathValue("surfaceID"), r.PathValue("targetID"), 1); err != nil {
			writeSurfaceError(w, logger, err)
			return
		}
		streamTargetEvents(w, r, targetEvents)
	})
	mux.HandleFunc("POST /api/v1/surfaces/{surfaceID}/targets/{targetID}/focus", func(w http.ResponseWriter, r *http.Request) {
		if surfaceOperations == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("surface operations are unavailable"))
			return
		}
		if err := surfaceOperations.FocusTarget(r.Context(), r.PathValue("surfaceID"), r.PathValue("targetID")); err != nil {
			writeSurfaceError(w, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/surfaces/{surfaceID}/targets/{targetID}/send", func(w http.ResponseWriter, r *http.Request) {
		if surfaceOperations == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("surface operations are unavailable"))
			return
		}
		var request struct {
			Text string `json:"text"`
		}
		if err := decodeJSON(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(request.Text) == "" || len(request.Text) > 32*1024 {
			writeError(w, http.StatusBadRequest, errors.New("text must contain between 1 byte and 32 KiB"))
			return
		}
		if err := surfaceOperations.SendToTarget(r.Context(), r.PathValue("surfaceID"), r.PathValue("targetID"), request.Text); err != nil {
			writeSurfaceError(w, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/v1/surfaces/{surfaceID}/targets/{targetID}", func(w http.ResponseWriter, r *http.Request) {
		if surfaceOperations == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("surface operations are unavailable"))
			return
		}
		if err := surfaceOperations.CloseTarget(r.Context(), r.PathValue("surfaceID"), r.PathValue("targetID")); err != nil {
			writeSurfaceError(w, logger, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/surfaces/{surfaceID}/targets/{targetID}/agent-sessions", func(w http.ResponseWriter, r *http.Request) {
		if surfaceOperations == nil || sessionManager == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("agent launch is unavailable"))
			return
		}
		var request struct {
			HarnessID string `json:"harnessId"`
		}
		if err := decodeJSON(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if request.HarnessID != "" {
			registered := false
			for _, harness := range sessionManager.Harnesses() {
				if harness.ID == request.HarnessID {
					registered = true
					break
				}
			}
			if !registered {
				writeError(w, http.StatusBadRequest, fmt.Errorf("harness %q is not registered", request.HarnessID))
				return
			}
		}
		workspace, err := surfaceOperations.WorkspaceForTarget(r.Context(), r.PathValue("surfaceID"), r.PathValue("targetID"))
		if err != nil {
			writeSurfaceError(w, logger, err)
			return
		}
		session, err := sessionManager.StartWorkspaceWithHarness(r.Context(), workspace, request.HarnessID)
		if err != nil {
			writeSurfaceUpstreamError(w, logger, "start agent session", err)
			return
		}
		writeJSON(w, http.StatusCreated, session)
	})
	mux.HandleFunc("POST /api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			WorkspaceID string `json:"workspaceId"`
			HarnessID   string `json:"harnessId"`
		}
		if err := decodeJSON(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		session, err := sessionManager.StartWithHarness(r.Context(), request.WorkspaceID, request.HarnessID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, session)
	})
	mux.HandleFunc("GET /api/v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, sessionManager.List())
	})
	mux.HandleFunc("GET /api/v1/sessions/{sessionID}", func(w http.ResponseWriter, r *http.Request) {
		session, err := sessionManager.Get(r.PathValue("sessionID"))
		if err != nil {
			writeSessionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
	})
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/input", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID             string            `json:"id"`
			IdempotencyKey string            `json:"idempotencyKey"`
			Mode           runtime.InputMode `json:"mode"`
			Text           string            `json:"text"`
		}
		if err := decodeJSON(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !validInputMode(request.Mode) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported input mode %q", request.Mode))
			return
		}
		err := sessionManager.Send(r.Context(), r.PathValue("sessionID"), runtime.Input{
			ID:             request.ID,
			IdempotencyKey: request.IdempotencyKey,
			Mode:           request.Mode,
			Text:           request.Text,
		})
		if err != nil {
			writeSessionError(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/interactions/{requestID}/response", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Value     *string `json:"value"`
			Confirmed *bool   `json:"confirmed"`
			Cancelled bool    `json:"cancelled"`
		}
		if err := decodeJSON(r.Body, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		err := sessionManager.Respond(r.Context(), r.PathValue("sessionID"), runtime.InteractionResponse{
			RequestID: r.PathValue("requestID"),
			Value:     request.Value,
			Confirmed: request.Confirmed,
			Cancelled: request.Cancelled,
		})
		if err != nil {
			writeSessionError(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/abort", func(w http.ResponseWriter, r *http.Request) {
		if err := sessionManager.Abort(r.Context(), r.PathValue("sessionID")); err != nil {
			writeSessionError(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("DELETE /api/v1/sessions/{sessionID}", func(w http.ResponseWriter, r *http.Request) {
		if err := sessionManager.Stop(r.Context(), r.PathValue("sessionID")); err != nil {
			writeSessionError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/sessions/{sessionID}/events", func(w http.ResponseWriter, r *http.Request) {
		after, err := parseSequence(r.URL.Query().Get("after"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		stored, err := broker.After(r.Context(), r.PathValue("sessionID"), after)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, stored)
	})
	mux.HandleFunc("GET /api/v1/sessions/{sessionID}/stream", func(w http.ResponseWriter, r *http.Request) {
		streamEvents(w, r, broker)
	})
	if mcpHandler != nil {
		mux.Handle("GET /mcp", mcpHandler)
		mux.Handle("POST /mcp", mcpHandler)
	}
	mux.Handle("GET /", http.FileServerFS(assets))

	handler := http.Handler(mux)
	if deviceService != nil {
		handler = deviceService.Middleware(handler)
	}
	return &Server{handler: requestLogger(logger, securityHeaders(handler))}, nil
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func streamEvents(w http.ResponseWriter, r *http.Request, broker *events.Broker) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is unavailable"))
		return
	}
	after, err := requestedCursor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	updates, unsubscribe := broker.Subscribe(r.PathValue("sessionID"))
	defer unsubscribe()

	last := after
	stored, err := broker.After(r.Context(), r.PathValue("sessionID"), after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, event := range stored {
		if err := writeEvent(w, event); err != nil {
			return
		}
		last = event.Sequence
	}
	flusher.Flush()

	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case event, open := <-updates:
			if !open {
				return
			}
			if event.Sequence <= last {
				continue
			}
			if err := writeEvent(w, event); err != nil {
				return
			}
			last = event.Sequence
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeEvent(w io.Writer, event events.Event) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, encoded)
	return err
}

func decodeJSON(reader io.Reader, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request must contain exactly one JSON object")
		}
		return fmt.Errorf("decode trailing request content: %w", err)
	}
	return nil
}

func validInputMode(mode runtime.InputMode) bool {
	return mode == runtime.InputPrompt || mode == runtime.InputSteer || mode == runtime.InputFollowUp
}

func requestedCursor(r *http.Request) (uint64, error) {
	queryCursor, err := parseSequence(r.URL.Query().Get("after"))
	if err != nil {
		return 0, err
	}
	headerCursor, err := parseSequence(r.Header.Get("Last-Event-ID"))
	if err != nil {
		return 0, err
	}
	if headerCursor > queryCursor {
		return headerCursor, nil
	}
	return queryCursor, nil
}

func parseSequence(raw string) (uint64, error) {
	if raw == "" {
		return 0, nil
	}
	sequence, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid event sequence: %w", err)
	}
	return sequence, nil
}

func writeSessionError(w http.ResponseWriter, err error) {
	if errors.Is(err, sessions.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func writeSurfaceError(w http.ResponseWriter, logger *slog.Logger, err error) {
	switch {
	case errors.Is(err, surface.ErrTargetNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, surface.ErrAgentUnavailable), errors.Is(err, surface.ErrAgentManaged):
		writeError(w, http.StatusConflict, err)
	default:
		writeSurfaceUpstreamError(w, logger, "execute operation", err)
	}
}

func writeSurfaceUpstreamError(w http.ResponseWriter, logger *slog.Logger, operation string, err error) {
	if logger != nil {
		logger.Error("surface operation failed", "operation", operation, "error", err)
	}
	writeError(w, http.StatusBadGateway, errors.New("surface operation failed"))
}

func writeLoopError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrLoopActive),
		errors.Is(err, store.ErrLoopManual),
		errors.Is(err, store.ErrLoopRunRequested):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'self'; manifest-src 'self'; object-src 'none'; script-src 'self'; style-src 'self'; worker-src 'self'")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Debug("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
	})
}
