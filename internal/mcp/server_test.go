package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dkta-labs/agentd/internal/store"
	"github.com/dkta-labs/agentd/internal/supervisor"
)

type stubService struct {
	stopped   string
	created   supervisor.CreateRequest
	updated   supervisor.CreateRequest
	updatedID string
}

func (s *stubService) Create(_ context.Context, request supervisor.CreateRequest) (store.Job, error) {
	s.created = request
	return store.Job{ID: "created", GoalKey: request.GoalKey}, nil
}
func (s *stubService) Update(_ context.Context, id string, request supervisor.CreateRequest) (store.Job, error) {
	s.updatedID, s.updated = id, request
	return store.Job{ID: id, GoalKey: request.GoalKey}, nil
}
func (*stubService) List(context.Context) ([]store.Job, error) { return []store.Job{}, nil }
func (*stubService) Get(context.Context, string) (store.Job, error) {
	return store.Job{}, errors.New("not implemented")
}
func (*stubService) Runs(context.Context, string, int) ([]store.Run, error) {
	return nil, errors.New("not implemented")
}
func (*stubService) StartJob(context.Context, string) (store.Job, error) {
	return store.Job{}, errors.New("not implemented")
}
func (*stubService) PauseJob(context.Context, string) (store.Job, error) {
	return store.Job{}, errors.New("not implemented")
}
func (*stubService) RunNow(context.Context, string) (store.Job, error) {
	return store.Job{}, errors.New("not implemented")
}
func (s *stubService) StopJob(_ context.Context, id string) (store.Job, error) {
	s.stopped = id
	return store.Job{ID: id, State: "stopping"}, nil
}

func invoke(t *testing.T, server http.Handler, payload string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(payload))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestInitializeAndToolsListUseMCPJSONRPC(t *testing.T) {
	server := New(&stubService{})
	initialized := invoke(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if initialized["jsonrpc"] != "2.0" || initialized["error"] != nil {
		t.Fatalf("initialize = %#v", initialized)
	}
	result := initialized["result"].(map[string]any)
	if result["protocolVersion"] != protocolVersion {
		t.Fatalf("protocol version = %v", result["protocolVersion"])
	}
	listed := invoke(t, server, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	tools := listed["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 9 {
		t.Fatalf("tools = %d", len(tools))
	}
	for _, raw := range tools {
		name := raw.(map[string]any)["name"].(string)
		if len(name) < len("agentd_") || name[:len("agentd_")] != "agentd_" {
			t.Fatalf("non-namespaced tool %q", name)
		}
	}
}

func TestToolCallRoutesStopAndReturnsTextContent(t *testing.T) {
	service := &stubService{}
	server := New(service)
	response := invoke(t, server, `{"jsonrpc":"2.0","id":"stop-1","method":"tools/call","params":{"name":"agentd_job_stop","arguments":{"id":"job-active"}}}`)
	if service.stopped != "job-active" {
		t.Fatalf("stopped id = %q", service.stopped)
	}
	result := response["result"].(map[string]any)
	if result["isError"] != false {

		t.Fatalf("tool result = %#v", result)
	}
	content := result["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("content = %#v", content)
	}
}
func TestJobToolsThreadGoalKeyAndExposeSchema(t *testing.T) {
	service := &stubService{}
	server := New(service)
	response := invoke(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agentd_job_create","arguments":{"name":"job","workspaceId":"workspace","invocationRequest":"do work","goalKey":"goal-42"}}}`)
	if service.created.GoalKey != "goal-42" {
		t.Fatalf("created goal key = %q", service.created.GoalKey)
	}
	response = invoke(t, server, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"agentd_job_update","arguments":{"id":"job-1","name":"job","workspaceId":"workspace","invocationRequest":"do work","goalKey":"goal-43"}}}`)
	if service.updatedID != "job-1" || service.updated.GoalKey != "goal-43" {
		t.Fatalf("updated request = %#v, id = %q", service.updated, service.updatedID)
	}
	listed := invoke(t, server, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
	for _, raw := range listed["result"].(map[string]any)["tools"].([]any) {
		definition := raw.(map[string]any)
		if definition["name"] != "agentd_job_create" && definition["name"] != "agentd_job_update" {
			continue
		}
		schema := definition["inputSchema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		goalKey := properties["goalKey"].(map[string]any)
		if goalKey["type"] != "string" {
			t.Fatalf("goalKey schema = %#v", goalKey)
		}
		if _, advertised := goalKey["maxLength"]; advertised {
			t.Fatalf("goalKey schema advertises maxLength = %#v", goalKey["maxLength"])
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("schema additionalProperties = %#v", schema["additionalProperties"])
		}
		for _, required := range schema["required"].([]any) {
			if required == "goalKey" {
				t.Fatal("goalKey must remain optional")
			}
		}
	}
	_ = response
}

func TestHostileOriginIsRejectedBeforeToolDispatch(t *testing.T) {
	service := &stubService{}
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agentd_job_stop","arguments":{"id":"job-active"}}}`))
	request.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	New(service).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if service.stopped != "" {
		t.Fatalf("hostile origin stopped %q", service.stopped)
	}
}

func TestUnsupportedProtocolVersionIsRejected(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	request.Header.Set("MCP-Protocol-Version", "1999-01-01")
	response := httptest.NewRecorder()
	New(&stubService{}).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}
