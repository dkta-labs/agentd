package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/dkta-labs/agentd/internal/surface"
)

const targetEventPollInterval = 500 * time.Millisecond

type targetOutputReader interface {
	TargetOutput(context.Context, string, string, int) (surface.Output, error)
}

type targetEvent struct {
	Type     string `json:"type"`
	TargetID string `json:"targetId"`
	Revision uint64 `json:"revision"`
}

type targetEventKey struct {
	surfaceID string
	targetID  string
}

type targetEventFeed struct {
	cancel      context.CancelFunc
	subscribers map[chan targetEvent]struct{}
}

type targetEventBroker struct {
	reader   targetOutputReader
	interval time.Duration

	mu    sync.Mutex
	feeds map[targetEventKey]*targetEventFeed
}

func newTargetEventBroker(reader targetOutputReader, interval time.Duration) *targetEventBroker {
	return &targetEventBroker{
		reader:   reader,
		interval: interval,
		feeds:    make(map[targetEventKey]*targetEventFeed),
	}
}

func (b *targetEventBroker) subscribe(surfaceID, targetID string) (<-chan targetEvent, func()) {
	key := targetEventKey{surfaceID: surfaceID, targetID: targetID}
	updates := make(chan targetEvent, 1)

	b.mu.Lock()
	feed := b.feeds[key]
	if feed == nil {
		ctx, cancel := context.WithCancel(context.Background())
		feed = &targetEventFeed{cancel: cancel, subscribers: make(map[chan targetEvent]struct{})}
		b.feeds[key] = feed
		go b.watch(ctx, key, feed)
	}
	feed.subscribers[updates] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			current := b.feeds[key]
			if current != feed {
				return
			}
			delete(feed.subscribers, updates)
			close(updates)
			if len(feed.subscribers) == 0 {
				delete(b.feeds, key)
				feed.cancel()
			}
		})
	}
	return updates, unsubscribe
}

func (b *targetEventBroker) watch(ctx context.Context, key targetEventKey, feed *targetEventFeed) {
	output, err := b.reader.TargetOutput(ctx, key.surfaceID, key.targetID, 120)
	if err != nil {
		b.fail(key, feed)
		return
	}
	lastText := output.Text
	lastRevision := output.Revision
	sequence := output.Revision + 1
	b.publish(key, feed, targetEvent{Type: "target_output_changed", TargetID: key.targetID, Revision: sequence})

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			output, err = b.reader.TargetOutput(ctx, key.surfaceID, key.targetID, 120)
			if err != nil {
				b.fail(key, feed)
				return
			}
			if output.Revision == lastRevision && output.Text == lastText {
				continue
			}
			lastText = output.Text
			lastRevision = output.Revision
			if output.Revision > sequence {
				sequence = output.Revision
			} else {
				sequence++
			}
			b.publish(key, feed, targetEvent{Type: "target_output_changed", TargetID: key.targetID, Revision: sequence})
		}
	}
}

func (b *targetEventBroker) publish(key targetEventKey, feed *targetEventFeed, event targetEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.feeds[key] != feed {
		return
	}
	for subscriber := range feed.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
}

func (b *targetEventBroker) fail(key targetEventKey, feed *targetEventFeed) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.feeds[key] != feed {
		return
	}
	delete(b.feeds, key)
	for subscriber := range feed.subscribers {
		close(subscriber)
	}
	feed.cancel()
}

func streamTargetEvents(w http.ResponseWriter, r *http.Request, broker *targetEventBroker) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is unavailable"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	updates, unsubscribe := broker.subscribe(r.PathValue("surfaceID"), r.PathValue("targetID"))
	defer unsubscribe()
	if _, err := io.WriteString(w, "retry: 1000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case event, open := <-updates:
			if !open {
				return
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
				return
			}
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
