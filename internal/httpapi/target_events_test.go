package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/surface"
)

type mutableTargetOutputReader struct {
	mu      sync.Mutex
	text    string
	calls   int
	started chan struct{}
	once    sync.Once
}

func (r *mutableTargetOutputReader) TargetOutput(_ context.Context, _, targetID string, _ int) (surface.Output, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.once.Do(func() { close(r.started) })
	return surface.Output{TargetID: targetID, Text: r.text}, nil
}

func (r *mutableTargetOutputReader) setText(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.text = text
}

func TestTargetEventBrokerSharesWatcherAndCoalescesUpdates(t *testing.T) {
	reader := &mutableTargetOutputReader{text: "initial", started: make(chan struct{})}
	broker := newTargetEventBroker(reader, 5*time.Millisecond)
	first, unsubscribeFirst := broker.subscribe("default", "w1:p1")
	defer unsubscribeFirst()
	second, unsubscribeSecond := broker.subscribe("default", "w1:p1")
	defer unsubscribeSecond()

	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("shared target watcher did not start")
	}
	broker.mu.Lock()
	feedCount := len(broker.feeds)
	subscriberCount := len(broker.feeds[targetEventKey{surfaceID: "default", targetID: "w1:p1"}].subscribers)
	broker.mu.Unlock()
	if feedCount != 1 || subscriberCount != 2 {
		t.Fatalf("watcher topology = %d feeds, %d subscribers; want 1 feed, 2 subscribers", feedCount, subscriberCount)
	}

	for index, updates := range []<-chan targetEvent{first, second} {
		select {
		case event := <-updates:
			if event.Type != "target_output_changed" || event.TargetID != "w1:p1" || event.Revision != 1 {
				t.Fatalf("subscriber %d baseline event = %#v", index, event)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive baseline update", index)
		}
	}

	reader.setText("changed")
	for index, updates := range []<-chan targetEvent{first, second} {
		select {
		case event := <-updates:
			if event.Type != "target_output_changed" || event.TargetID != "w1:p1" || event.Revision != 2 {
				t.Fatalf("subscriber %d changed event = %#v", index, event)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive update", index)
		}
	}
}

func TestStreamTargetEventsEmitsSSEAndStopsWithRequest(t *testing.T) {
	reader := &mutableTargetOutputReader{text: "initial", started: make(chan struct{})}
	broker := newTargetEventBroker(reader, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("GET", "/api/v1/surfaces/default/targets/w1:p1/events", nil).WithContext(ctx)
	request.SetPathValue("surfaceID", "default")
	request.SetPathValue("targetID", "w1:p1")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		streamTargetEvents(response, request, broker)
		close(done)
	}()

	select {
	case <-reader.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("target stream watcher did not start")
	}
	reader.setText("changed")
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("target stream did not stop after request cancellation")
	}

	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", got)
	}
	body := response.Body.String()
	if !strings.Contains(body, `data: {"type":"target_output_changed","targetId":"w1:p1","revision":2}`) {
		t.Fatalf("stream body did not contain changed target update: %q", body)
	}
}
