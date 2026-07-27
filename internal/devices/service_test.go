package devices

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

	"github.com/dkta-labs/agentd/internal/store"
)

const testEnrollmentToken = "test-enrollment-token-32-characters"

type recordedPush struct {
	payload  []byte
	category string
	topic    string
}

type recordingPushSender struct {
	status int
	calls  chan recordedPush
}

func (s *recordingPushSender) Send(_ context.Context, payload []byte, _ PushSubscription, category, topic string) (int, error) {
	s.calls <- recordedPush{payload: append([]byte(nil), payload...), category: category, topic: topic}
	return s.status, nil
}

func TestEnrollmentAuthenticationAndOneTimeToken(t *testing.T) {
	database, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, err := New(database, Options{Enabled: true, CookieSecure: true, EnrollmentToken: testEnrollmentToken})
	if err != nil {
		t.Fatal(err)
	}

	device, token, err := service.Enroll(context.Background(), "Personal Android", testEnrollmentToken)
	if err != nil {
		t.Fatal(err)
	}
	if device.Name != "Personal Android" || len(token) < 40 {
		t.Fatalf("unexpected enrollment result: device=%#v token=%q", device, token)
	}
	if _, _, err := service.Enroll(context.Background(), "Other device", testEnrollmentToken); !errors.Is(err, store.ErrEnrollmentTokenUsed) {
		t.Fatalf("second enrollment error = %v, want %v", err, store.ErrEnrollmentTokenUsed)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	authenticated, fromCookie, err := service.AuthenticateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.ID != device.ID || fromCookie {
		t.Fatalf("unexpected bearer authentication: %#v, cookie=%v", authenticated, fromCookie)
	}

	cookieResponse := httptest.NewRecorder()
	service.SetCookie(cookieResponse, token)
	cookies := cookieResponse.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected device cookie: %#v", cookies)
	}

	handler := service.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	mutation := httptest.NewRequest(http.MethodPost, "/api/v1/test", nil)
	mutation.AddCookie(cookies[0])
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, mutation)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want %d", response.Code, http.StatusForbidden)
	}
	mutation = httptest.NewRequest(http.MethodPost, "https://agent.example/api/v1/test", nil)
	mutation.Host = "agent.example"
	mutation.Header.Set("Origin", "https://agent.example")
	mutation.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, mutation)
	if response.Code != http.StatusNoContent {
		t.Fatalf("same-origin status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if err := service.Revoke(context.Background(), device.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Enroll(context.Background(), "Replacement", testEnrollmentToken); !errors.Is(err, store.ErrEnrollmentTokenUsed) {
		t.Fatalf("enrollment after revocation error = %v, want %v", err, store.ErrEnrollmentTokenUsed)
	}
}

func TestPushDeliveryHonorsPreferencesAndPrunesExpiredSubscription(t *testing.T) {
	database, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sender := &recordingPushSender{status: http.StatusGone, calls: make(chan recordedPush, 1)}
	service, err := New(database, Options{
		Enabled:         true,
		EnrollmentToken: testEnrollmentToken,
		PushEnabled:     true,
		VAPIDPublicKey:  "public-key",
		VAPIDPrivateKey: "private-key",
		VAPIDSubject:    "mailto:test@example.com",
		PushSender:      sender,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	device, _, err := service.Enroll(context.Background(), "Phone", testEnrollmentToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Subscribe(context.Background(), device.ID, PushSubscription{
		Endpoint: "https://fcm.googleapis.com/fcm/send/example",
		Keys:     PushKeys{P256dh: "p256dh", Auth: "auth"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdatePreferences(context.Background(), device.ID, true, false, true); err != nil {
		t.Fatal(err)
	}
	service.NotifyTarget("herdr:default:p1", "/?herdrSession=default&pane=p1", "ready", "claude is ready", "Review this pane")
	select {
	case call := <-sender.calls:
		var payload map[string]string
		if err := json.Unmarshal(call.payload, &payload); err != nil {
			t.Fatal(err)
		}
		if call.category != "ready" || call.topic != pushTopic("herdr:default:p1", "ready") || len(call.topic) > 32 || payload["sessionId"] != "herdr:default:p1" || payload["url"] != "/?herdrSession=default&pane=p1" {
			t.Fatalf("unexpected push call: %#v payload=%#v", call, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("push notification was not delivered")
	}
	service.Close()
	records, err := database.PushSubscriptions(context.Background(), "ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("expired subscription was retained: %#v", records)
	}
}

func TestPushSubscriptionRejectsArbitraryEndpoints(t *testing.T) {
	if err := validatePushSubscription(PushSubscription{
		Endpoint: "https://127.0.0.1/internal",
		Keys:     PushKeys{P256dh: "p256dh", Auth: "auth"},
	}); err == nil {
		t.Fatal("arbitrary push endpoint was accepted")
	}
}

func TestPushTopicSeparatesSessions(t *testing.T) {
	first := pushTopic("ses_one", "ready")
	second := pushTopic("ses_two", "ready")
	if first == second {
		t.Fatalf("different sessions received the same push topic %q", first)
	}
	if first != pushTopic("ses_one", "ready") {
		t.Fatal("push topic is not deterministic")
	}
	if len(first) > 32 {
		t.Fatalf("push topic length = %d, exceeds RFC 8030 limit", len(first))
	}
	for _, character := range first {
		if !(character == '-' || character >= '0' && character <= '9' || character >= 'a' && character <= 'z') {
			t.Fatalf("push topic contains non-URL-safe character %q", character)
		}
	}
}
