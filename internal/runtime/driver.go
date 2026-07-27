package runtime

import (
	"context"
	"encoding/json"
	"time"
)

type InputMode string

const (
	InputPrompt   InputMode = "prompt"
	InputSteer    InputMode = "steer"
	InputFollowUp InputMode = "follow_up"
)

type SessionSpec struct {
	ID            string
	WorkspaceID   string
	WorkspacePath string
}

type Input struct {
	ID             string
	IdempotencyKey string
	Mode           InputMode
	Text           string
}

type InteractionResponse struct {
	RequestID string
	Value     *string
	Confirmed *bool
	Cancelled bool
}

const (
	EventSessionReady         = "session_ready"
	EventTurnStarted          = "turn_started"
	EventTurnCompleted        = "turn_completed"
	EventAssistantStarted     = "assistant_started"
	EventAssistantDelta       = "assistant_delta"
	EventAssistantCompleted   = "assistant_completed"
	EventInteractionRequested = "interaction_requested"
	EventInteractionSubmitted = "interaction_submitted"
	EventInteractionDelivered = "interaction_delivered"
	EventInteractionFailed    = "interaction_failed"
	EventInteractionExpired   = "interaction_expired"
	EventInputSubmitted       = "input_submitted"
	EventInputDelivered       = "input_delivered"
	EventInputFailed          = "input_failed"
	EventToolStarted          = "tool_started"
	EventToolUpdated          = "tool_updated"
	EventToolCompleted        = "tool_completed"
	EventSubagentLifecycle    = "subagent_lifecycle"
	EventSubagentProgress     = "subagent_progress"
	EventSessionStderr        = "session_stderr"
	EventSessionProtocolError = "session_protocol_error"
	EventSessionExited        = "session_exited"
	EventAdapterEvent         = "adapter_event"
)

type EventNormalizer func(Event) Event

type Event struct {
	SessionID string
	Type      string
	SourceID  string
	Payload   json.RawMessage
	CreatedAt time.Time
}

type Sink interface {
	Publish(context.Context, Event) error
}

type Placement struct {
	NodeID   string
	NodeName string
	Reason   string
	Remote   bool
}

type PlacedSession interface {
	Placement() Placement
}

type AttachmentPreference interface {
	AttachLocally() bool
}

type Session interface {
	Send(context.Context, Input) error
	Respond(context.Context, InteractionResponse) error
	Abort(context.Context) error
	Close(context.Context) error
}

type Driver interface {
	Start(context.Context, SessionSpec, Sink) (Session, error)
}
