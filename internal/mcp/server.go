package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/loops"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/sessions"
	"github.com/dkta-labs/agentd/internal/store"
)

const protocolVersion = "2025-11-25"

type WorkspaceCatalog interface {
	List(context.Context) []config.Workspace
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
	catalog     WorkspaceCatalog
	sessions    *sessions.Manager
	broker      *events.Broker
	loopManager LoopAPI
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func New(catalog WorkspaceCatalog, sessionManager *sessions.Manager, broker *events.Broker) *Server {
	return NewWithLoops(catalog, sessionManager, broker, nil)
}

func NewWithLoops(catalog WorkspaceCatalog, sessionManager *sessions.Manager, broker *events.Broker, loopManager LoopAPI) *Server {
	return &Server{catalog: catalog, sessions: sessionManager, broker: broker, loopManager: loopManager}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !validOrigin(r.Header.Get("Origin")) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !supportedProtocolVersion(version) {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var incoming request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&incoming); err != nil {
		writeResponse(w, response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "invalid JSON-RPC request"}})
		return
	}
	if len(incoming.ID) == 0 || string(incoming.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	id := decodeID(incoming.ID)
	result, rpcErr := s.handle(r.Context(), incoming)
	writeResponse(w, response{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
}

func (s *Server) handle(ctx context.Context, incoming request) (any, *rpcError) {
	switch incoming.Method {
	case "initialize":
		version := protocolVersion
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(incoming.Params, &params) == nil && supportedProtocolVersion(params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "agentd", "version": "0.1.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools()}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(incoming.Params, &params); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid tool arguments"}
		}
		return s.callTool(ctx, params.Name, params.Arguments), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func (s *Server) callTool(ctx context.Context, name string, arguments json.RawMessage) toolResult {
	var value any
	var err error
	switch name {
	case "agentd_list_workspaces":
		available := s.catalog.List(ctx)
		value = make([]map[string]string, 0, len(available))
		for _, workspace := range available {
			value = append(value.([]map[string]string), map[string]string{"id": workspace.ID, "name": workspace.Name})
		}
	case "agentd_run_omp":
		value, err = s.runOMP(ctx, arguments)
	case "agentd_send_omp":
		var input struct {
			SessionID      string            `json:"sessionId"`
			Mode           runtime.InputMode `json:"mode"`
			Text           string            `json:"text"`
			IdempotencyKey string            `json:"idempotencyKey"`
		}
		if err = decodeArguments(arguments, &input); err == nil {
			if input.Mode == "" {
				input.Mode = runtime.InputFollowUp
			}
			err = s.sessions.Send(ctx, input.SessionID, runtime.Input{
				IdempotencyKey: input.IdempotencyKey,
				Mode:           input.Mode,
				Text:           input.Text,
			})
			value = map[string]any{"sessionId": input.SessionID, "accepted": err == nil}
		}
	case "agentd_get_session":
		var input struct {
			SessionID string `json:"sessionId"`
			After     uint64 `json:"after"`
		}
		if err = decodeArguments(arguments, &input); err == nil {
			var session sessions.Session
			session, err = s.sessions.Get(input.SessionID)
			if err == nil {
				var stored []events.Event
				stored, err = s.broker.After(ctx, input.SessionID, input.After)
				if err == nil {
					var pending []sessions.Interaction
					pending, err = s.sessions.PendingInteractions(ctx, input.SessionID)
					if err == nil {
						value = map[string]any{"session": session, "events": stored, "pendingInteractions": pending}
					}
				}
			}
		}
	case "agentd_respond_omp":
		var input struct {
			SessionID string  `json:"sessionId"`
			RequestID string  `json:"requestId"`
			Value     *string `json:"value"`
			Confirmed *bool   `json:"confirmed"`
			Cancelled bool    `json:"cancelled"`
		}
		if err = decodeArguments(arguments, &input); err == nil {
			err = s.sessions.Respond(ctx, input.SessionID, runtime.InteractionResponse{
				RequestID: input.RequestID,
				Value:     input.Value,
				Confirmed: input.Confirmed,
				Cancelled: input.Cancelled,
			})
			value = map[string]any{"sessionId": input.SessionID, "requestId": input.RequestID, "accepted": err == nil}
		}
	case "agentd_abort_omp":
		var input struct {
			SessionID string `json:"sessionId"`
		}
		if err = decodeArguments(arguments, &input); err == nil {
			err = s.sessions.Abort(ctx, input.SessionID)
			value = map[string]any{"sessionId": input.SessionID, "aborted": err == nil}
		}
	case "agentd_list_loops":
		if s.loopManager == nil {
			err = errors.New("loop supervisor is unavailable")
			break
		}
		value, err = s.loopManager.List(ctx)
	case "agentd_create_loop":
		if s.loopManager == nil {
			err = errors.New("loop supervisor is unavailable")
			break
		}
		var input loops.CreateRequest
		if err = decodeArguments(arguments, &input); err == nil {
			value, err = s.loopManager.Create(ctx, input)
		}
	case "agentd_update_loop":
		if s.loopManager == nil {
			err = errors.New("loop supervisor is unavailable")
			break
		}
		var input struct {
			LoopID string `json:"loopId"`
			loops.CreateRequest
		}
		if err = decodeArguments(arguments, &input); err == nil {
			value, err = s.loopManager.Update(ctx, input.LoopID, input.CreateRequest)
		}
	case "agentd_get_loop":
		if s.loopManager == nil {
			err = errors.New("loop supervisor is unavailable")
			break
		}
		var input struct {
			LoopID   string `json:"loopId"`
			RunLimit int    `json:"runLimit"`
		}
		if err = decodeArguments(arguments, &input); err == nil {
			var loop store.LoopRecord
			loop, err = s.loopManager.Get(ctx, input.LoopID)
			if err == nil {
				var runs []store.LoopRunRecord
				runs, err = s.loopManager.Runs(ctx, input.LoopID, input.RunLimit)
				if err == nil {
					value = map[string]any{"loop": loop, "runs": runs}
				}
			}
		}
	case "agentd_control_loop":
		if s.loopManager == nil {
			err = errors.New("loop supervisor is unavailable")
			break
		}
		var input struct {
			LoopID string `json:"loopId"`
			Action string `json:"action"`
		}
		if err = decodeArguments(arguments, &input); err == nil {
			switch input.Action {
			case "start":
				value, err = s.loopManager.ResumeLoop(ctx, input.LoopID)
			case "pause":
				value, err = s.loopManager.PauseLoop(ctx, input.LoopID)
			case "run":
				value, err = s.loopManager.RunLoop(ctx, input.LoopID)
			default:
				err = errors.New("action must be start, pause, or run")
			}
		}
	default:
		err = fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return textResult(err.Error(), true)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return textResult(err.Error(), true)
	}
	return textResult(string(encoded), false)
}

func (s *Server) runOMP(ctx context.Context, arguments json.RawMessage) (any, error) {
	var input struct {
		WorkspaceID    string `json:"workspaceId"`
		Prompt         string `json:"prompt"`
		TimeoutSeconds int    `json:"timeoutSeconds"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.WorkspaceID) == "" || strings.TrimSpace(input.Prompt) == "" {
		return nil, errors.New("workspaceId and prompt are required")
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 900
	}
	if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 1800 {
		return nil, errors.New("timeoutSeconds must be between 1 and 1800")
	}
	session, err := s.sessions.Start(ctx, input.WorkspaceID)
	if err != nil {
		return nil, err
	}
	updates, unsubscribe := s.broker.Subscribe(session.ID)
	defer unsubscribe()
	if err := s.sessions.Send(ctx, session.ID, runtime.Input{Mode: runtime.InputPrompt, Text: input.Prompt}); err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	var answer strings.Builder
	for {
		select {
		case event, open := <-updates:
			if !open {
				return nil, errors.New("session event stream closed")
			}
			if event.Type == runtime.EventAssistantDelta {
				appendAssistantDelta(&answer, event.Payload)
			}
			if event.Type == runtime.EventInteractionRequested {
				pending, pendingErr := s.sessions.PendingInteractions(waitCtx, session.ID)
				if pendingErr != nil {
					return nil, pendingErr
				}
				if len(pending) > 0 {
					return map[string]any{
						"sessionId":           session.ID,
						"state":               "input_required",
						"answer":              answer.String(),
						"pendingInteractions": pending,
					}, nil
				}
			}
			if event.Type == runtime.EventTurnCompleted {
				return map[string]any{"sessionId": session.ID, "state": "idle", "answer": answer.String()}, nil
			}
			if event.Type == runtime.EventSessionExited {
				return nil, fmt.Errorf("agent process exited: %s", event.Payload)
			}
		case <-waitCtx.Done():
			return map[string]any{"sessionId": session.ID, "state": "running", "answer": answer.String()}, fmt.Errorf("OMP did not finish before timeout: %w", waitCtx.Err())
		}
	}
}

func appendAssistantDelta(destination *strings.Builder, payload json.RawMessage) {
	var update struct {
		AssistantMessageEvent struct {
			Delta any `json:"delta"`
		} `json:"assistantMessageEvent"`
	}
	if json.Unmarshal(payload, &update) != nil {
		return
	}
	if delta, ok := update.AssistantMessageEvent.Delta.(string); ok {
		destination.WriteString(delta)
	}
}

func decodeArguments(raw json.RawMessage, destination any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, destination); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func textResult(text string, isError bool) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: text}}, IsError: isError}
}

func decodeID(raw json.RawMessage) any {
	var id any
	if json.Unmarshal(raw, &id) != nil {
		return nil
	}
	return id
}

func supportedProtocolVersion(version string) bool {
	switch version {
	case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		return true
	default:
		return false
	}
}

func validOrigin(raw string) bool {
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func writeResponse(w http.ResponseWriter, value response) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", protocolVersion)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(value)
}

func loopDefinitionSchema(requireID bool) map[string]any {
	properties := map[string]any{
		"name":           map[string]any{"type": "string", "minLength": 1, "maxLength": 120},
		"workspaceId":    map[string]any{"type": "string"},
		"harnessId":      map[string]any{"type": "string"},
		"prompt":         map[string]any{"type": "string", "minLength": 1, "maxLength": 65536},
		"cadenceSeconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 2592000},
		"timeoutSeconds": map[string]any{"type": "integer", "minimum": 30, "maximum": 1800},
	}
	required := []string{"name", "workspaceId", "prompt", "cadenceSeconds"}
	if requireID {
		properties["loopId"] = map[string]any{"type": "string"}
		required = append(required, "loopId")
	}
	return map[string]any{
		"type": "object", "properties": properties,
		"required": required, "additionalProperties": false,
	}
}

func Tools() []map[string]any {
	return tools(false)
}

func ToolsWithLoops() []map[string]any {
	return tools(true)
}

func (s *Server) tools() []map[string]any {
	return tools(s.loopManager != nil)
}

func tools(includeLoops bool) []map[string]any {
	result := []map[string]any{
		{
			"name":        "agentd_list_workspaces",
			"description": "List the Herdr-backed workspaces available to the local agent gateway.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
		{
			"name":        "agentd_run_omp",
			"description": "Start OMP in an allowlisted Herdr workspace and send one prompt. Wait until OMP becomes idle, requests user input, exits, or reaches timeout; return the sessionId, accumulated answer, state, and any pending interactions.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workspaceId":    map[string]any{"type": "string"},
					"prompt":         map[string]any{"type": "string"},
					"timeoutSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 1800},
				},
				"required": []string{"workspaceId", "prompt"}, "additionalProperties": false,
			},
		},
		{
			"name":        "agentd_send_omp",
			"description": "Send a prompt, steer, or follow-up to a running agentd OMP session. Reuse idempotencyKey when retrying an ambiguous submission.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId":      map[string]any{"type": "string"},
					"mode":           map[string]any{"type": "string", "enum": []string{"prompt", "steer", "follow_up"}},
					"text":           map[string]any{"type": "string"},
					"idempotencyKey": map[string]any{"type": "string"},
				},
				"required": []string{"sessionId", "text"}, "additionalProperties": false,
			},
		},
		{
			"name":        "agentd_get_session",
			"description": "Read a durable agentd session, committed OMP events after a sequence cursor, and all currently pending confirmation, selection, or text interactions.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"sessionId": map[string]any{"type": "string"}, "after": map[string]any{"type": "integer", "minimum": 0}},
				"required":   []string{"sessionId"}, "additionalProperties": false,
			},
		},
		{
			"name":        "agentd_respond_omp",
			"description": "Answer one pending OMP interaction. Provide exactly one of value (selection/input/editor), confirmed (confirmation), or cancelled. Read pendingInteractions with agentd_get_session first.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string"},
					"requestId": map[string]any{"type": "string"},
					"value":     map[string]any{"type": "string"},
					"confirmed": map[string]any{"type": "boolean"},
					"cancelled": map[string]any{"type": "boolean"},
				},
				"required": []string{"sessionId", "requestId"}, "additionalProperties": false,
			},
		},
		{
			"name":        "agentd_abort_omp",
			"description": "Request cancellation of the active OMP turn while keeping its agentd session available for later input.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"sessionId": map[string]any{"type": "string"}},
				"required":   []string{"sessionId"}, "additionalProperties": false,
			},
		},
	}
	if !includeLoops {
		return result
	}
	return append(result, []map[string]any{
		{
			"name":        "agentd_list_loops",
			"description": "List durable supervised loops, their desired and observed states, schedules, active agentd session IDs, and last errors.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
		{
			"name":        "agentd_create_loop",
			"description": "Create a paused durable loop for one allowlisted Herdr workspace. cadenceSeconds 0 creates a manual-only loop; recurring loops must be explicitly started.",
			"inputSchema": loopDefinitionSchema(false),
		},
		{
			"name":        "agentd_update_loop",
			"description": "Replace the definition of a loop that has no active run.",
			"inputSchema": loopDefinitionSchema(true),
		},
		{
			"name":        "agentd_get_loop",
			"description": "Read one durable loop and its recent runs. Session events remain the authoritative transcript and evidence.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"loopId":   map[string]any{"type": "string"},
					"runLimit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				},
				"required": []string{"loopId"}, "additionalProperties": false,
			},
		},
		{
			"name":        "agentd_control_loop",
			"description": "Start a recurring loop, pause future iterations, or request one immediate bounded run. This never answers OMP interactions or approves high-impact actions.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"loopId": map[string]any{"type": "string"},
					"action": map[string]any{"type": "string", "enum": []string{"start", "pause", "run"}},
				},
				"required": []string{"loopId", "action"}, "additionalProperties": false,
			},
		},
	}...)
}
