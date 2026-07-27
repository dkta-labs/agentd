package omp

import (
	"encoding/json"
	"testing"

	"github.com/dkta-labs/agentd/internal/runtime"
)

func TestNormalizeEventMapsKnownAndEncapsulatesUnknownKinds(t *testing.T) {
	known := NormalizeEvent(runtime.Event{Type: "message_update", Payload: json.RawMessage(`{"assistantMessageEvent":{"delta":"hi"}}`)})
	if known.Type != runtime.EventAssistantDelta || string(known.Payload) != `{"assistantMessageEvent":{"delta":"hi"}}` {
		t.Fatalf("known event = %#v", known)
	}

	unknown := NormalizeEvent(runtime.Event{Type: "agent_thought_update", Payload: json.RawMessage(`{"delta":"private"}`)})
	if unknown.Type != runtime.EventAdapterEvent {
		t.Fatalf("unknown kind = %q, want %q", unknown.Type, runtime.EventAdapterEvent)
	}
	var envelope struct {
		NativeType string          `json:"nativeType"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(unknown.Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.NativeType != "agent_thought_update" || string(envelope.Data) != `{"delta":"private"}` {
		t.Fatalf("unknown envelope = %#v", envelope)
	}
}
