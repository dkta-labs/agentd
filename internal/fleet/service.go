package fleet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/store"
)

const nodeOfflineAfter = 30 * time.Second

type Options struct {
	EnrollmentToken string
	Logger          *slog.Logger
}

type Service struct {
	store      *store.DB
	enrollment string
	logger     *slog.Logger

	mu       sync.Mutex
	nodes    map[string]Node
	commands map[string][]Command
	next     map[string]uint64
	waiters  map[string][]chan struct{}
	results  map[string]chan CommandResult
	sinks    map[string]runtime.Sink
	owners   map[string]string
}

func NewService(database *store.DB, options Options) *Service {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:      database,
		enrollment: strings.TrimSpace(options.EnrollmentToken),
		logger:     logger,
		nodes:      make(map[string]Node),
		commands:   make(map[string][]Command),
		next:       make(map[string]uint64),
		waiters:    make(map[string][]chan struct{}),
		results:    make(map[string]chan CommandResult),
		sinks:      make(map[string]runtime.Sink),
		owners:     make(map[string]string),
	}
}

func (s *Service) Enabled() bool { return s != nil }

func (s *Service) EnrollmentEnabled() bool { return s != nil && s.enrollment != "" }

func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/workers/enroll", s.handleEnroll)
	mux.HandleFunc("POST /api/v1/workers/heartbeat", s.withWorker(s.handleHeartbeat))
	mux.HandleFunc("GET /api/v1/workers/commands", s.withWorker(s.handleCommands))
	mux.HandleFunc("POST /api/v1/workers/commands/{commandID}/result", s.withWorker(s.handleCommandResult))
	mux.HandleFunc("POST /api/v1/workers/events", s.withWorker(s.handleEvent))
	mux.HandleFunc("GET /api/v1/nodes", s.handleList)
}

func (s *Service) List(ctx context.Context) ([]Node, error) {
	records, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Node, 0, len(records))
	for _, record := range records {
		node := Node{ID: record.ID, Name: record.Name, LastSeenAt: record.LastSeenAt}
		if live, ok := s.nodes[record.ID]; ok {
			node = live
		}
		node.Online = now.Sub(node.LastSeenAt) <= nodeOfflineAfter
		result = append(result, node)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *Service) Select(workspaceID string) (Node, error) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []Node
	for _, node := range s.nodes {
		if now.Sub(node.LastSeenAt) > nodeOfflineAfter {
			continue
		}
		for _, workspace := range node.Workspaces {
			if workspace.ID == workspaceID {
				node.Online = true
				candidates = append(candidates, node)
				break
			}
		}
	}
	if len(candidates) == 0 {
		return Node{}, errors.New("no online worker has the requested workspace")
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ActiveRuns != candidates[j].ActiveRuns {
			return candidates[i].ActiveRuns < candidates[j].ActiveRuns
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], nil
}

func (s *Service) Attach(sessionID, nodeID string, sink runtime.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners[sessionID] = nodeID
	s.sinks[sessionID] = sink
}

func (s *Service) Detach(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.owners, sessionID)
	delete(s.sinks, sessionID)
}

func (s *Service) Dispatch(ctx context.Context, nodeID, commandType, sessionID string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode worker command: %w", err)
	}
	commandID, err := randomToken(18)
	if err != nil {
		return err
	}
	result := make(chan CommandResult, 1)
	s.mu.Lock()
	s.next[nodeID]++
	command := Command{ID: commandID, Sequence: s.next[nodeID], Type: commandType, SessionID: sessionID, Payload: encoded}
	s.commands[nodeID] = append(s.commands[nodeID], command)
	s.results[commandID] = result
	waiters := s.waiters[nodeID]
	delete(s.waiters, nodeID)
	s.mu.Unlock()
	for _, waiter := range waiters {
		close(waiter)
	}
	select {
	case completed := <-result:
		if completed.Error != "" {
			return errors.New(completed.Error)
		}
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (s *Service) poll(ctx context.Context, nodeID string, after uint64) []Command {
	for {
		s.mu.Lock()
		available := commandsAfter(s.commands[nodeID], after)
		if len(available) > 0 {
			s.mu.Unlock()
			return available
		}
		waiter := make(chan struct{})
		s.waiters[nodeID] = append(s.waiters[nodeID], waiter)
		s.mu.Unlock()
		select {
		case <-waiter:
		case <-ctx.Done():
			return nil
		}
	}
}

func commandsAfter(commands []Command, after uint64) []Command {
	result := make([]Command, 0)
	for _, command := range commands {
		if command.Sequence > after {
			result = append(result, command)
		}
	}
	return result
}

func (s *Service) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.EnrollmentEnabled() {
		writeError(w, http.StatusNotFound, "worker enrollment is disabled")
		return
	}
	var request EnrollRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(request.Name) == "" || subtle.ConstantTimeCompare([]byte(request.Token), []byte(s.enrollment)) != 1 {
		writeError(w, http.StatusUnauthorized, "worker enrollment denied")
		return
	}
	nodeID, err := randomToken(12)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	token, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now().UTC()
	record := store.NodeRecord{ID: "node_" + nodeID, Name: strings.TrimSpace(request.Name), TokenHash: hashToken(token), CreatedAt: now, LastSeenAt: now}
	if err := s.store.CreateNode(r.Context(), record); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, EnrollResponse{Node: Node{ID: record.ID, Name: record.Name, LastSeenAt: now, Online: true}, Token: token})
}

func (s *Service) handleHeartbeat(w http.ResponseWriter, r *http.Request, record store.NodeRecord) {
	var request HeartbeatRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	node := Node{ID: record.ID, Name: record.Name, OS: request.OS, Architecture: request.Architecture, Capabilities: request.Capabilities, Workspaces: request.Workspaces, OMPVersion: request.OMPVersion, ActiveRuns: request.ActiveRuns, Online: true, LastSeenAt: now}
	s.mu.Lock()
	s.nodes[node.ID] = node
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, node)
}

func (s *Service) handleCommands(w http.ResponseWriter, r *http.Request, record store.NodeRecord) {
	var after uint64
	if raw := r.URL.Query().Get("after"); raw != "" {
		if _, err := fmt.Sscan(raw, &after); err != nil {
			writeError(w, http.StatusBadRequest, "invalid command cursor")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.poll(ctx, record.ID, after))
}

func (s *Service) handleCommandResult(w http.ResponseWriter, r *http.Request, record store.NodeRecord) {
	var result CommandResult
	if err := decodeJSON(r, &result); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	commandID := r.PathValue("commandID")
	if result.CommandID == "" {
		result.CommandID = commandID
	}
	s.mu.Lock()
	commands := s.commands[record.ID]
	commandIndex := -1
	for index, command := range commands {
		if command.ID == commandID {
			commandIndex = index
			break
		}
	}
	if commandIndex < 0 {
		for nodeID, pending := range s.commands {
			if nodeID == record.ID {
				continue
			}
			for _, command := range pending {
				if command.ID == commandID {
					s.mu.Unlock()
					writeError(w, http.StatusNotFound, "worker command belongs to another node")
					return
				}
			}
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.commands[record.ID] = append(commands[:commandIndex], commands[commandIndex+1:]...)
	channel := s.results[commandID]
	delete(s.results, commandID)
	s.mu.Unlock()
	if channel != nil {
		select {
		case channel <- result:
		default:
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleEvent(w http.ResponseWriter, r *http.Request, record store.NodeRecord) {
	var envelope EventEnvelope
	if err := decodeJSON(r, &envelope); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if envelope.ID == "" {
		writeError(w, http.StatusBadRequest, "worker event id is required")
		return
	}
	s.mu.Lock()
	owner := s.owners[envelope.SessionID]
	sink := s.sinks[envelope.SessionID]
	s.mu.Unlock()
	if owner != record.ID || sink == nil {
		writeError(w, http.StatusConflict, "worker does not own this live session")
		return
	}
	if err := sink.Publish(r.Context(), runtime.Event{
		SessionID: envelope.SessionID,
		Type:      envelope.Type,
		SourceID:  record.ID + ":" + envelope.ID,
		Payload:   envelope.Payload,
		CreatedAt: envelope.CreatedAt,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

func (s *Service) withWorker(next func(http.ResponseWriter, *http.Request, store.NodeRecord)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authorization := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if authorization == "" {
			writeError(w, http.StatusUnauthorized, "worker authentication required")
			return
		}
		record, err := s.store.NodeByTokenHash(r.Context(), hashToken(authorization), time.Now().UTC())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "worker authentication required")
			return
		}
		next(w, r, record)
	}
}

func hashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func decodeJSON(r *http.Request, destination any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("encode fleet response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
