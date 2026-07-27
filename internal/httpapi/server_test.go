package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/devices"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/store"
)

type acceptingPushSender struct{}

func (acceptingPushSender) Send(context.Context, []byte, devices.PushSubscription, string, string) (int, error) {
	return http.StatusCreated, nil
}

func TestHealthAndWorkspaceEndpoints(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := staticCatalog{{ID: "gateway", Name: "Agent Gateway", Path: "/private/repo"}}
	server, err := New(catalog, logger, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("health", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
		}
	})

	t.Run("workspaces hide host paths", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces", nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
		}
		rawBody := response.Body.Bytes()
		var body []map[string]any
		if err := json.Unmarshal(rawBody, &body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 1 || body[0]["id"] != "gateway" {
			t.Fatalf("unexpected body: %#v", body)
		}
		if strings.Contains(string(rawBody), "/private/repo") {
			t.Fatal("workspace response exposed the host path")
		}
	})
	t.Run("capabilities match Hermes MCP catalog", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
		}
		var body struct {
			Harnesses []runtime.Descriptor `json:"harnesses"`
			Tools     []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Harnesses) != 0 || len(body.Tools) != 6 {
			t.Fatalf("unexpected capabilities: %#v", body)
		}
		if body.Tools[4].Name != "agentd_respond_omp" || body.Tools[4].Description == "" {
			t.Fatalf("unexpected interaction capability: %#v", body.Tools[4])
		}
	})
}

func TestEmbeddedUI(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(staticCatalog{}, logger, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), `id="herdr-overview"`) {
		t.Fatal("embedded UI response did not contain the Herdr operations shell")
	}
	if response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers missing: %#v", response.Header())
	}
}

func TestAuthDisabledDoesNotExposeDeviceManagement(t *testing.T) {
	database, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	if err := database.EnrollDevice(context.Background(), "stale-enrollment", store.DeviceRecord{
		ID: "dev_stale", Name: "Stale phone", TokenHash: "stale-device", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	deviceService, err := devices.New(database, devices.Options{})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(staticCatalog{}, logger, nil, nil, nil, deviceService)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil),
		httptest.NewRequest(http.MethodDelete, "/api/v1/devices/dev_stale", nil),
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code < http.StatusBadRequest {
			t.Fatalf("%s %s unexpectedly succeeded with status %d", request.Method, request.URL.Path, response.Code)
		}
	}
	records, err := database.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != "dev_stale" {
		t.Fatalf("auth-disabled request changed device inventory: %#v", records)
	}
}

func TestDeviceAuthenticationProtectsAPIs(t *testing.T) {
	database, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	deviceService, err := devices.New(database, devices.Options{
		Enabled:         true,
		EnrollmentToken: "http-test-enrollment-token-32-chars",
		PushEnabled:     true,
		VAPIDPublicKey:  "test-public-key",
		VAPIDPrivateKey: "test-private-key",
		VAPIDSubject:    "mailto:test@example.com",
		PushSender:      acceptingPushSender{},
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(staticCatalog{}, logger, nil, nil, nil, deviceService)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/surfaces", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated surface status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/surfaces/default/targets/p1/events", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated Herdr stream status = %d, want %d", response.Code, http.StatusUnauthorized)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/devices/enroll", strings.NewReader(
		`{"name":"test phone","token":"http-test-enrollment-token-32-chars"}`,
	))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("enrollment status = %d, body = %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("enrollment cookies = %#v", cookies)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "tokenHash") {
		t.Fatalf("device inventory response = %d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPut, "/api/v1/device/push", strings.NewReader(
		`{"endpoint":"https://fcm.googleapis.com/fcm/send/browser","expirationTime":null,"keys":{"p256dh":"p256dh","auth":"auth"}}`,
	))
	request.AddCookie(cookies[0])
	request.Header.Set("Origin", "http://example.com")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("browser push subscription status = %d, body = %s", response.Code, response.Body.String())
	}
	subscriptions, err := database.PushSubscriptions(context.Background(), "ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != 1 {
		t.Fatalf("stored browser subscriptions = %#v", subscriptions)
	}
}

func TestRequestedCursorUsesLatestSSECursor(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/stream?after=4", nil)
	request.Header.Set("Last-Event-ID", "9")
	cursor, err := requestedCursor(request)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 9 {
		t.Fatalf("cursor = %d, want 9", cursor)
	}
}
