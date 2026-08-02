package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/store"
	"github.com/dkta-labs/agentd/internal/supervisor"
)

type Service interface {
	Create(context.Context, supervisor.CreateRequest) (store.Job, error)
	Update(context.Context, string, supervisor.CreateRequest) (store.Job, error)
	List(context.Context) ([]store.Job, error)
	Get(context.Context, string) (store.Job, error)
	Runs(context.Context, string, int) ([]store.Run, error)
	StartJob(context.Context, string) (store.Job, error)
	PauseJob(context.Context, string) (store.Job, error)
	RunNow(context.Context, string) (store.Job, error)
	StopJob(context.Context, string) (store.Job, error)
	RegisterWorkspace(context.Context, config.Workspace) (config.Workspace, error)
	ListWorkspaces(context.Context) ([]config.Workspace, error)
}

type Server struct{ service Service }

func New(service Service) *Server       { return &Server{service: service} }
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.handle) }
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/workspaces" {
		switch r.Method {
		case http.MethodGet:
			value, err := s.service.ListWorkspaces(r.Context())
			writeResult(w, value, err)
		case http.MethodPost:
			var request config.Workspace
			if !decodeJSON(w, r, &request) {
				return
			}
			value, err := s.service.RegisterWorkspace(r.Context(), request)
			writeResultStatus(w, value, err, http.StatusCreated)
		default:
			methodNotAllowed(w)
		}
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/jobs") {
		http.NotFound(w, r)
		return
	}
	remainder := strings.TrimPrefix(r.URL.Path, "/jobs")
	if remainder == "" || remainder == "/" {
		if r.Method == http.MethodGet {
			value, err := s.service.List(r.Context())
			writeResult(w, value, err)
			return
		}
		if r.Method == http.MethodPost {
			var request supervisor.CreateRequest
			if !decodeJSON(w, r, &request) {
				return
			}
			value, err := s.service.Create(r.Context(), request)
			writeResultStatus(w, value, err, http.StatusCreated)
			return
		}
		methodNotAllowed(w)
		return
	}
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			value, err := s.service.Get(r.Context(), id)
			writeResult(w, value, err)
		case http.MethodPut:
			var request supervisor.CreateRequest
			if !decodeJSON(w, r, &request) {
				return
			}
			value, err := s.service.Update(r.Context(), id, request)
			writeResult(w, value, err)
		default:
			methodNotAllowed(w)
		}
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "runs":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		value, err := s.service.Runs(r.Context(), id, limit)
		writeResult(w, value, err)
	case "start":
		s.action(w, r, id, s.service.StartJob)
	case "pause":
		s.action(w, r, id, s.service.PauseJob)
	case "run":
		s.action(w, r, id, s.service.RunNow)
	case "stop":
		s.action(w, r, id, s.service.StopJob)
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) action(w http.ResponseWriter, r *http.Request, id string, fn func(context.Context, string) (store.Job, error)) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	value, err := fn(r.Context(), id)
	writeResult(w, value, err)
}
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}
func writeResult(w http.ResponseWriter, value any, err error) {
	writeResultStatus(w, value, err, http.StatusOK)
}
func writeResultStatus(w http.ResponseWriter, value any, err error, status int) {
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		if errors.Is(err, store.ErrActive) || errors.Is(err, store.ErrRunRequested) || errors.Is(err, store.ErrStopping) {
			code = http.StatusConflict
		}
		if errors.Is(err, store.ErrWorkspaceExists) {
			code = http.StatusConflict
		}
		writeError(w, code, err)
		return
	}
	writeJSON(w, status, value)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func methodNotAllowed(w http.ResponseWriter) { w.WriteHeader(http.StatusMethodNotAllowed) }
