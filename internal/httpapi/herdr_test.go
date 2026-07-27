package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/sessions"
	"github.com/dkta-labs/agentd/internal/surface"
)

type fakeHerdrCatalog struct {
	staticCatalog
	launchWorkspace config.Workspace
	calls           []string
	upstreamError   error
}

func (c *fakeHerdrCatalog) Fleet(context.Context) (surface.Fleet, error) {
	if c.upstreamError != nil {
		return surface.Fleet{}, c.upstreamError
	}
	return surface.Fleet{Surfaces: []surface.Instance{{
		ID: "default", Provider: "herdr", Label: "Herdr", Status: "working", Workspaces: []surface.Workspace{{
			ID: "w1", Label: "Gateway", Status: "working", Views: []surface.View{{
				ID: "t1", Label: "agents", Status: "working", Targets: []surface.Target{{
					ID: "p1", Label: "Implement API", Status: "working", Actions: []surface.Action{
						surface.ActionFocus, surface.ActionRead, surface.ActionSend, surface.ActionClose, surface.ActionLaunchAgent,
					},
					Agent:                 &surface.Agent{Name: "omp", Status: "working", SessionProvider: "agentd:omp", SessionKind: "id", SessionReference: "ses_test"},
					Presentations:         []surface.Presentation{surface.PresentationConversation, surface.PresentationScreen},
					PreferredPresentation: surface.PresentationConversation,
				}},
			}},
		}},
	}}}, nil
}

func (c *fakeHerdrCatalog) TargetOutput(context.Context, string, string, int) (surface.Output, error) {
	if c.upstreamError != nil {
		return surface.Output{}, c.upstreamError
	}
	c.calls = append(c.calls, "read")
	return surface.Output{TargetID: "p1", Text: "safe output", Revision: 7}, nil
}

func (c *fakeHerdrCatalog) FocusTarget(context.Context, string, string) error {
	if c.upstreamError != nil {
		return c.upstreamError
	}
	c.calls = append(c.calls, "focus")
	return nil
}

func (c *fakeHerdrCatalog) SendToTarget(_ context.Context, _, _, text string) error {
	if c.upstreamError != nil {
		return c.upstreamError
	}
	c.calls = append(c.calls, "send:"+text)
	return nil
}

func (c *fakeHerdrCatalog) CloseTarget(context.Context, string, string) error {
	if c.upstreamError != nil {
		return c.upstreamError
	}
	c.calls = append(c.calls, "close")
	return nil
}

func (c *fakeHerdrCatalog) WorkspaceForTarget(context.Context, string, string) (config.Workspace, error) {
	if c.upstreamError != nil {
		return config.Workspace{}, c.upstreamError
	}
	c.calls = append(c.calls, "launchAgent")
	return c.launchWorkspace, nil
}

func TestSurfaceOperationsHTTPContract(t *testing.T) {
	baseWorkspace := config.Workspace{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}
	launchWorkspace := config.Workspace{ID: "herdr:dynamic", Name: "Dynamic pane", Path: "/private/dynamic"}
	catalog := &fakeHerdrCatalog{staticCatalog: staticCatalog{baseWorkspace}, launchWorkspace: launchWorkspace}
	driver := &recordingDriver{started: make(chan runtime.SessionSpec, 1)}
	broker := events.NewBroker(nil)
	manager, err := sessions.NewManager(context.Background(), testHarnessRegistry(t, driver), broker, nil, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(catalog, logger, manager, broker, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("legacy provider route is removed", func(t *testing.T) {
		response := performSurfaceRequest(server, http.MethodGet, "/api/v1/herdr", "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("legacy route status = %d, want %d", response.Code, http.StatusNotFound)
		}
	})

	t.Run("capabilities advertise registered harnesses", func(t *testing.T) {
		response := performSurfaceRequest(server, http.MethodGet, "/api/v1/capabilities", "")
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var body struct {
			Harnesses []runtime.Descriptor `json:"harnesses"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Harnesses) != 1 || body.Harnesses[0].ID != "omp" {
			t.Fatalf("harnesses = %#v", body.Harnesses)
		}
	})

	t.Run("unknown harness is rejected before surface mutation", func(t *testing.T) {
		callCount := len(catalog.calls)
		response := performSurfaceRequest(server, http.MethodPost, "/api/v1/surfaces/default/targets/p1/agent-sessions", `{"harnessId":"missing"}`)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if len(catalog.calls) != callCount {
			t.Fatalf("invalid harness reached surface: %#v", catalog.calls[callCount:])
		}
	})

	t.Run("fleet is provider-neutral and sanitized", func(t *testing.T) {
		response := performSurfaceRequest(server, http.MethodGet, "/api/v1/surfaces", "")
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "/private/repo") {
			t.Fatal("surface fleet exposed a host path")
		}
		var fleet surface.Fleet
		if err := json.Unmarshal(response.Body.Bytes(), &fleet); err != nil {
			t.Fatal(err)
		}
		target := fleet.Surfaces[0].Workspaces[0].Views[0].Targets[0]
		if target.Agent == nil || target.Agent.Name != "omp" || target.Agent.SessionReference != "ses_test" {
			t.Fatalf("unexpected target attachment: %#v", target)
		}
		if len(target.Presentations) != 2 || target.PreferredPresentation != surface.PresentationConversation {
			t.Fatalf("HTTP layer changed catalog presentation capabilities: %#v", target)
		}
	})

	for _, test := range []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantCall   string
	}{
		{"read", http.MethodGet, "/api/v1/surfaces/default/targets/p1/output?lines=120", "", http.StatusOK, "read"},
		{"focus", http.MethodPost, "/api/v1/surfaces/default/targets/p1/focus", "", http.StatusNoContent, "focus"},
		{"send", http.MethodPost, "/api/v1/surfaces/default/targets/p1/send", "{\"text\":\"  continue\\n\"}", http.StatusNoContent, "send:  continue\n"},
		{"close", http.MethodDelete, "/api/v1/surfaces/default/targets/p1", "", http.StatusNoContent, "close"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performSurfaceRequest(server, test.method, test.path, test.body)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.wantStatus, response.Body.String())
			}
			if got := catalog.calls[len(catalog.calls)-1]; got != test.wantCall {
				t.Fatalf("call = %q, want %q", got, test.wantCall)
			}
		})
	}

	t.Run("new harness session is explicitly modeled", func(t *testing.T) {
		response := performSurfaceRequest(server, http.MethodPost, "/api/v1/surfaces/default/targets/p1/agent-sessions", `{"harnessId":"omp"}`)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if got := catalog.calls[len(catalog.calls)-1]; got != "launchAgent" {
			t.Fatalf("call = %q, want launchAgent", got)
		}
		started := <-driver.started
		if started.WorkspacePath != launchWorkspace.Path || started.WorkspaceID != launchWorkspace.ID {
			t.Fatalf("started workspace = %#v, want %#v with omp harness", started, launchWorkspace)
		}
	})
	t.Run("upstream failures do not expose socket paths", func(t *testing.T) {
		catalog.upstreamError = errors.New("dial unix /private/herdr.sock: connection refused")
		defer func() { catalog.upstreamError = nil }()
		for _, path := range []string{
			"/api/v1/surfaces",
			"/api/v1/surfaces/default/targets/p1/output",
			"/api/v1/surfaces/default/targets/p1/events",
		} {
			response := performSurfaceRequest(server, http.MethodGet, path, "")
			if response.Code != http.StatusBadGateway {
				t.Fatalf("%s status = %d, want %d", path, response.Code, http.StatusBadGateway)
			}
			if strings.Contains(response.Body.String(), "/private/herdr.sock") || strings.Contains(response.Body.String(), ".sock") {
				t.Fatalf("%s exposed socket path: %s", path, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "surface operation failed") {
				t.Fatalf("%s omitted generic failure: %s", path, response.Body.String())
			}
		}
	})

}

func performSurfaceRequest(server *Server, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}
