package githubhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type recordingDispatcher struct {
	jobs []string
	err  error
}

func (d *recordingDispatcher) Run(_ context.Context, jobID string) error {
	d.jobs = append(d.jobs, jobID)
	return d.err
}

func newTestServer(t *testing.T, dispatcher Dispatcher) (*Server, []byte) {
	t.Helper()
	secret := bytes.Repeat([]byte("s"), 32)
	store, err := OpenReceiptStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	merged := true
	server, err := NewServer([]Rule{{
		ID: "merged-pr", Event: "pull_request", Action: "closed",
		Repository: "dkta-labs/agentd", Merged: &merged, JobID: "job-webhook",
	}}, secret, store, dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	server.now = func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) }
	return server, secret
}

func signedRequest(t *testing.T, secret []byte, deliveryID string, body []byte) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	request := httptest.NewRequest(http.MethodPost, "/github", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "pull_request")
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return request
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) response {
	t.Helper()
	var value response
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestMergedPullRequestDispatchesOncePerDelivery(t *testing.T) {
	dispatcher := &recordingDispatcher{}
	server, secret := newTestServer(t, dispatcher)
	body := []byte(`{"action":"closed","repository":{"full_name":"dkta-labs/agentd"},"pull_request":{"merged":true}}`)

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, signedRequest(t, secret, "delivery-one", body))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	firstValue := decodeResponse(t, first)
	if firstValue.Dispatched != 1 || len(dispatcher.jobs) != 1 || dispatcher.jobs[0] != "job-webhook" {
		t.Fatalf("first result = %#v, jobs = %#v", firstValue, dispatcher.jobs)
	}

	replay := httptest.NewRecorder()
	server.Handler().ServeHTTP(replay, signedRequest(t, secret, "delivery-one", body))
	if replay.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d, body = %s", replay.Code, replay.Body.String())
	}
	replayValue := decodeResponse(t, replay)
	if replayValue.Duplicates != 1 || replayValue.Dispatched != 0 || len(dispatcher.jobs) != 1 {
		t.Fatalf("replay result = %#v, jobs = %#v", replayValue, dispatcher.jobs)
	}
}

func TestInvalidSignatureAndUnmatchedPayloadDoNotDispatch(t *testing.T) {
	dispatcher := &recordingDispatcher{}
	server, secret := newTestServer(t, dispatcher)
	body := []byte(`{"action":"closed","repository":{"full_name":"dkta-labs/agentd"},"pull_request":{"merged":true}}`)
	invalid := signedRequest(t, secret, "delivery-invalid", body)
	invalid.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	invalidResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d", invalidResponse.Code)
	}

	unmatchedBody := []byte(`{"action":"closed","repository":{"full_name":"dkta-labs/agentd"},"pull_request":{"merged":false}}`)
	unmatched := httptest.NewRecorder()
	server.Handler().ServeHTTP(unmatched, signedRequest(t, secret, "delivery-unmatched", unmatchedBody))
	if unmatched.Code != http.StatusAccepted || decodeResponse(t, unmatched).Status != "ignored" {
		t.Fatalf("unmatched response = %d %s", unmatched.Code, unmatched.Body.String())
	}
	if len(dispatcher.jobs) != 0 {
		t.Fatalf("unexpected dispatched jobs = %#v", dispatcher.jobs)
	}
}

func TestDispatchFailureIsRetryableAndReleasesReceipt(t *testing.T) {
	dispatcher := &recordingDispatcher{err: errors.New("job active")}
	server, secret := newTestServer(t, dispatcher)
	body := []byte(`{"action":"closed","repository":{"full_name":"dkta-labs/agentd"},"pull_request":{"merged":true}}`)
	failed := httptest.NewRecorder()
	server.Handler().ServeHTTP(failed, signedRequest(t, secret, "delivery-retry", body))
	if failed.Code != http.StatusServiceUnavailable || failed.Header().Get("Retry-After") != "30" {
		t.Fatalf("failure response = %d %#v", failed.Code, failed.Header())
	}

	dispatcher.err = nil
	retry := httptest.NewRecorder()
	server.Handler().ServeHTTP(retry, signedRequest(t, secret, "delivery-retry", body))
	if retry.Code != http.StatusAccepted || decodeResponse(t, retry).Dispatched != 1 {
		t.Fatalf("retry response = %d %s", retry.Code, retry.Body.String())
	}
	if len(dispatcher.jobs) != 2 {
		t.Fatalf("dispatch attempts = %#v", dispatcher.jobs)
	}
}

func TestOversizedPayloadIsRejectedBeforeDispatch(t *testing.T) {
	dispatcher := &recordingDispatcher{}
	server, secret := newTestServer(t, dispatcher)
	body := bytes.Repeat([]byte("x"), maxPayloadBytes+1)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, signedRequest(t, secret, "delivery-large", body))
	if response.Code != http.StatusRequestEntityTooLarge || len(dispatcher.jobs) != 0 {
		t.Fatalf("oversized response = %d, jobs = %#v", response.Code, dispatcher.jobs)
	}
}

func TestAgentdDispatcherUsesRunEndpointAndRejectsConflict(t *testing.T) {
	var paths []string
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.Method+" "+request.URL.Path)
		writer.WriteHeader(status)
	}))
	defer server.Close()
	dispatcher, err := NewAgentdDispatcher(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Run(context.Background(), "job-one"); err != nil {
		t.Fatal(err)
	}
	status = http.StatusConflict
	if err := dispatcher.Run(context.Background(), "job-one"); err == nil {
		t.Fatal("expected conflict error")
	}
	if len(paths) != 2 || paths[0] != "POST /jobs/job-one/run" {
		t.Fatalf("requests = %#v", paths)
	}
}
