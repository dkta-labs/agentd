package devices

import (
	"context"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWebPushSenderDoesNotDoublePrefixMailtoSubject(t *testing.T) {
	headers := make(chan http.Header, 1)
	pushService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushService.Close()

	publicKey, privateKey, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	sender := NewWebPushSender(publicKey, privateKey, "mailto:test@example.com")
	status, err := sender.Send(
		context.Background(),
		[]byte(`{"title":"ready"}`),
		testPushSubscription(t, pushService.URL+"/subscription"),
		"ready",
		"agentd-ready",
	)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusCreated {
		t.Fatalf("push status = %d, want %d", status, http.StatusCreated)
	}
	requestHeaders := <-headers
	if requestHeaders.Get("Topic") != "agentd-ready" {
		t.Fatalf("Web Push topic = %q, want agentd-ready", requestHeaders.Get("Topic"))
	}
	parts := strings.Split(requestHeaders.Get("Authorization"), ".")
	if len(parts) < 2 {
		t.Fatalf("malformed VAPID authorization header %q", requestHeaders.Get("Authorization"))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "mailto:test@example.com" {
		t.Fatalf("VAPID subject = %q, want %q", claims.Subject, "mailto:test@example.com")
	}
}

func TestWebPushSenderDoesNotFollowRedirects(t *testing.T) {
	var targetVisited atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetVisited.Store(true)
		w.WriteHeader(http.StatusCreated)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	publicKey, privateKey, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	sender := NewWebPushSender(publicKey, privateKey, "https://example.com")
	status, err := sender.Send(
		context.Background(),
		[]byte(`{"title":"ready"}`),
		testPushSubscription(t, redirect.URL),
		"ready",
		"agentd-ready",
	)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusTemporaryRedirect {
		t.Fatalf("push status = %d, want %d", status, http.StatusTemporaryRedirect)
	}
	if targetVisited.Load() {
		t.Fatal("Web Push client followed a redirect")
	}
}

func testPushSubscription(t *testing.T, endpoint string) PushSubscription {
	t.Helper()
	_, clientX, clientY, err := elliptic.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatal(err)
	}
	return PushSubscription{
		Endpoint: endpoint,
		Keys: PushKeys{
			P256dh: base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), clientX, clientY)),
			Auth:   base64.RawURLEncoding.EncodeToString(authSecret),
		},
	}
}
