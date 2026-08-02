package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"

	"github.com/dkta-labs/agentd/internal/store"
	"github.com/dkta-labs/agentd/internal/supervisor"
)

const protocolVersion = "2025-06-18"

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
}

type Server struct{ service Service }

func New(service Service) *Server { return &Server{service: service} }

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !loopbackOrigin(origin) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && version != protocolVersion {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var call request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&call); err != nil {
		writeRPC(w, response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: err.Error()}})
		return
	}
	if call.JSONRPC != "2.0" || call.Method == "" {
		writeRPC(w, response{JSONRPC: "2.0", ID: call.ID, Error: &rpcError{Code: -32600, Message: "invalid JSON-RPC request"}})
		return
	}
	if len(call.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := s.dispatch(r.Context(), call.Method, call.Params)
	writeRPC(w, response{JSONRPC: "2.0", ID: call.ID, Result: result, Error: rpcErr})
}

func loopbackOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "agentd", "version": "1.0.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools()}, nil
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := decodeParams(params, &call); err != nil || call.Name == "" {
			if err == nil {
				err = errors.New("tool name is required")
			}
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		value, err := s.callTool(ctx, call.Name, call.Arguments)
		return toolResult(value, err), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func (s *Server) callTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	var arguments struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		WorkspaceID       string `json:"workspaceId"`
		Runner            string `json:"runner"`
		InvocationRequest string `json:"invocationRequest"`
		CadenceSeconds    int    `json:"cadenceSeconds"`
		Limit             int    `json:"limit"`
	}
	if err := decodeParams(raw, &arguments); err != nil {
		return nil, err
	}
	definition := supervisor.CreateRequest{
		Name: arguments.Name, WorkspaceID: arguments.WorkspaceID, Runner: arguments.Runner,
		InvocationRequest: arguments.InvocationRequest, CadenceSeconds: arguments.CadenceSeconds,
	}
	switch name {
	case "agentd_job_create":
		return s.service.Create(ctx, definition)
	case "agentd_job_update":
		return s.service.Update(ctx, arguments.ID, definition)
	case "agentd_job_list":
		return s.service.List(ctx)
	case "agentd_job_get":
		return s.service.Get(ctx, arguments.ID)
	case "agentd_job_runs":
		return s.service.Runs(ctx, arguments.ID, arguments.Limit)
	case "agentd_job_start":
		return s.service.StartJob(ctx, arguments.ID)
	case "agentd_job_pause":
		return s.service.PauseJob(ctx, arguments.ID)
	case "agentd_job_run":
		return s.service.RunNow(ctx, arguments.ID)
	case "agentd_job_stop":
		return s.service.StopJob(ctx, arguments.ID)
	default:
		return nil, errors.New("unknown tool")
	}
}

func decodeParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func toolResult(value any, err error) map[string]any {
	payload := value
	isError := false
	if err != nil {
		payload = map[string]string{"error": err.Error()}
		isError = true
	}
	encoded, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		encoded = []byte(`{"error":"encode tool result"}`)
		isError = true
	}
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": string(encoded)}},
		"isError": isError,
	}
}

func tools() []tool {
	definitionProperties := map[string]any{
		"name": map[string]string{"type": "string"}, "workspaceId": map[string]string{"type": "string"},
		"runner": map[string]string{"type": "string"}, "invocationRequest": map[string]string{"type": "string"},
		"cadenceSeconds": map[string]string{"type": "integer"},
	}
	definitionSchema := map[string]any{"type": "object", "properties": definitionProperties, "required": []string{"name", "workspaceId", "invocationRequest"}, "additionalProperties": false}
	idSchema := map[string]any{"type": "object", "properties": map[string]any{"id": map[string]string{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false}
	return []tool{
		{Name: "agentd_job_create", Description: "Create a paused scheduled-agent job", InputSchema: definitionSchema},
		{Name: "agentd_job_update", Description: "Update an inactive job definition", InputSchema: map[string]any{"type": "object", "properties": mergeProperties(definitionProperties, map[string]any{"id": map[string]string{"type": "string"}}), "required": []string{"id", "name", "workspaceId", "invocationRequest"}, "additionalProperties": false}},
		{Name: "agentd_job_list", Description: "List configured jobs", InputSchema: emptySchema()},
		{Name: "agentd_job_get", Description: "Inspect one job", InputSchema: idSchema},
		{Name: "agentd_job_runs", Description: "List durable run evidence for one job", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]string{"type": "string"}, "limit": map[string]string{"type": "integer"}}, "required": []string{"id"}, "additionalProperties": false}},
		{Name: "agentd_job_start", Description: "Enable scheduled recurrence for a job", InputSchema: idSchema},
		{Name: "agentd_job_pause", Description: "Disable future recurrence without stopping active work", InputSchema: idSchema},
		{Name: "agentd_job_run", Description: "Request one immediate run", InputSchema: idSchema},
		{Name: "agentd_job_stop", Description: "Disable recurrence and terminate the active process tree", InputSchema: idSchema},
	}
}

func emptySchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

func mergeProperties(left, right map[string]any) map[string]any {
	merged := make(map[string]any, len(left)+len(right))
	for key, value := range left {
		merged[key] = value
	}
	for key, value := range right {
		merged[key] = value
	}
	return merged
}

func writeRPC(w http.ResponseWriter, value response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
