package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkta-labs/agentd/internal/config"
)

type testCatalog []config.Workspace

func (c testCatalog) List(context.Context) []config.Workspace { return c }

func TestToolDiscoveryAndOriginValidation(t *testing.T) {
	server := New(testCatalog{{ID: "gateway", Name: "Agent Gateway"}}, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	request.Header.Set("Accept", "application/json, text/event-stream")
	responseRecorder := httptest.NewRecorder()
	server.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", responseRecorder.Code, responseRecorder.Body.String())
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(responseRecorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Result.Tools) != 6 ||
		response.Result.Tools[1].Name != "agentd_run_omp" ||
		response.Result.Tools[4].Name != "agentd_respond_omp" {
		t.Fatalf("unexpected tools: %#v", response.Result.Tools)
	}

	request = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	request.Header.Set("Origin", "https://attacker.example")
	responseRecorder = httptest.NewRecorder()
	server.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusForbidden {
		t.Fatalf("untrusted origin status = %d, want %d", responseRecorder.Code, http.StatusForbidden)
	}

	request = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"ping"}`))
	request.Header.Set("MCP-Protocol-Version", "1900-01-01")
	responseRecorder = httptest.NewRecorder()
	server.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusBadRequest {
		t.Fatalf("unsupported protocol status = %d, want %d", responseRecorder.Code, http.StatusBadRequest)
	}
}
