package fleet

import (
	"encoding/json"
	"time"
)

type WorkspaceReplica struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"-"`
	Revision string `json:"revision,omitempty"`
	Dirty    bool   `json:"dirty"`
}

type Node struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	OS           string             `json:"os"`
	Architecture string             `json:"architecture"`
	Capabilities []string           `json:"capabilities"`
	Workspaces   []WorkspaceReplica `json:"workspaces"`
	OMPVersion   string             `json:"ompVersion,omitempty"`
	ActiveRuns   int                `json:"activeRuns"`
	Online       bool               `json:"online"`
	LastSeenAt   time.Time          `json:"lastSeenAt"`
}

type EnrollRequest struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

type EnrollResponse struct {
	Node  Node   `json:"node"`
	Token string `json:"token"`
}

type HeartbeatRequest struct {
	OS           string             `json:"os"`
	Architecture string             `json:"architecture"`
	Capabilities []string           `json:"capabilities"`
	Workspaces   []WorkspaceReplica `json:"workspaces"`
	OMPVersion   string             `json:"ompVersion,omitempty"`
	ActiveRuns   int                `json:"activeRuns"`
}

type Command struct {
	ID        string          `json:"id"`
	Sequence  uint64          `json:"sequence"`
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type CommandResult struct {
	CommandID string `json:"commandId"`
	Error     string `json:"error,omitempty"`
}

type EventEnvelope struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionId"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

type StartPayload struct {
	WorkspaceID string `json:"workspaceId"`
	HarnessID   string `json:"harnessId"`
}

type InputPayload struct {
	ID             string `json:"id"`
	IdempotencyKey string `json:"idempotencyKey"`
	Mode           string `json:"mode"`
	Text           string `json:"text"`
}

type InteractionPayload struct {
	RequestID string  `json:"requestId"`
	Value     *string `json:"value,omitempty"`
	Confirmed *bool   `json:"confirmed,omitempty"`
	Cancelled bool    `json:"cancelled,omitempty"`
}
