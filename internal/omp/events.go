package omp

import (
	"encoding/json"

	"github.com/dkta-labs/agentd/internal/runtime"
)

var normalizedEventKinds = map[string]string{
	"ready":                 runtime.EventSessionReady,
	"agent_start":           runtime.EventTurnStarted,
	"agent_end":             runtime.EventTurnCompleted,
	"message_start":         runtime.EventAssistantStarted,
	"message_update":        runtime.EventAssistantDelta,
	"message_end":           runtime.EventAssistantCompleted,
	"extension_ui_request":  runtime.EventInteractionRequested,
	"tool_execution_start":  runtime.EventToolStarted,
	"tool_execution_update": runtime.EventToolUpdated,
	"tool_execution_end":    runtime.EventToolCompleted,
	"subagent_lifecycle":    runtime.EventSubagentLifecycle,
	"subagent_progress":     runtime.EventSubagentProgress,
	"stderr":                runtime.EventSessionStderr,
	"rpc_protocol_error":    runtime.EventSessionProtocolError,
	"rpc_read_error":        runtime.EventSessionProtocolError,
	"rpc_write_error":       runtime.EventSessionProtocolError,
	"process_exit":          runtime.EventSessionExited,
}

func NormalizeEvent(event runtime.Event) runtime.Event {
	nativeType := event.Type
	if kind, ok := normalizedEventKinds[nativeType]; ok {
		event.Type = kind
		return event
	}
	payload, err := json.Marshal(struct {
		NativeType string          `json:"nativeType"`
		Data       json.RawMessage `json:"data"`
	}{NativeType: nativeType, Data: event.Payload})
	if err == nil {
		event.Payload = payload
	}
	event.Type = runtime.EventAdapterEvent
	return event
}
