package events

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/dkta-labs/agentd/internal/runtime"
)

type Event struct {
	Sequence  uint64          `json:"sequence"`
	SessionID string          `json:"sessionId"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

var ErrDuplicate = errors.New("event already persisted")

type Store interface {
	AppendEvent(context.Context, runtime.Event) (Event, error)
	EventsAfter(context.Context, string, uint64) ([]Event, error)
}

type Broker struct {
	store Store

	mu          sync.Mutex
	subscribers map[string]map[chan Event]struct{}
}

func NewBroker(store Store) *Broker {
	if store == nil {
		store = newMemoryStore()
	}
	return &Broker{
		store:       store,
		subscribers: make(map[string]map[chan Event]struct{}),
	}
}

func (b *Broker) Publish(ctx context.Context, incoming runtime.Event) error {
	event, err := b.store.AppendEvent(ctx, incoming)
	if errors.Is(err, ErrDuplicate) {
		return nil
	}
	if err != nil {
		return err
	}
	b.Broadcast(event)
	return nil
}

func (b *Broker) Broadcast(event Event) {
	b.mu.Lock()
	for subscriber := range b.subscribers[event.SessionID] {
		select {
		case subscriber <- event:
		default:
			delete(b.subscribers[event.SessionID], subscriber)
			close(subscriber)
		}
	}
	b.mu.Unlock()
}

func (b *Broker) After(ctx context.Context, sessionID string, sequence uint64) ([]Event, error) {
	return b.store.EventsAfter(ctx, sessionID, sequence)
}

func (b *Broker) Subscribe(sessionID string) (<-chan Event, func()) {
	channel := make(chan Event, 64)
	b.mu.Lock()
	if b.subscribers[sessionID] == nil {
		b.subscribers[sessionID] = make(map[chan Event]struct{})
	}
	b.subscribers[sessionID][channel] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		if _, exists := b.subscribers[sessionID][channel]; exists {
			delete(b.subscribers[sessionID], channel)
			close(channel)
		}
		b.mu.Unlock()
	}
	return channel, unsubscribe
}

type memoryStore struct {
	mu     sync.RWMutex
	next   uint64
	events map[string][]Event
	seen   map[string]struct{}
}

func newMemoryStore() *memoryStore {
	return &memoryStore{next: 1, events: make(map[string][]Event), seen: make(map[string]struct{})}
}

func (s *memoryStore) AppendEvent(_ context.Context, incoming runtime.Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if incoming.SourceID != "" {
		key := incoming.SessionID + "\x00" + incoming.SourceID
		if _, exists := s.seen[key]; exists {
			return Event{}, ErrDuplicate
		}
		s.seen[key] = struct{}{}
	}
	event := Event{
		Sequence:  s.next,
		SessionID: incoming.SessionID,
		Type:      incoming.Type,
		Payload:   append(json.RawMessage(nil), incoming.Payload...),
		CreatedAt: incoming.CreatedAt,
	}
	s.next++
	s.events[event.SessionID] = append(s.events[event.SessionID], event)
	return event, nil
}

func (s *memoryStore) EventsAfter(_ context.Context, sessionID string, sequence uint64) ([]Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored := s.events[sessionID]
	result := make([]Event, 0, len(stored))
	for _, event := range stored {
		if event.Sequence > sequence {
			result = append(result, event)
		}
	}
	return result, nil
}
