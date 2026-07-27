package surface

import (
	"errors"
	"time"
)

var (
	ErrTargetNotFound   = errors.New("surface target not found")
	ErrAgentUnavailable = errors.New("surface target has no active agent")
	ErrAgentManaged     = errors.New("use the agentd session API for this managed target")
)

type Action string

const (
	ActionFocus       Action = "focus"
	ActionRead        Action = "read"
	ActionSend        Action = "send"
	ActionClose       Action = "close"
	ActionLaunchAgent Action = "launchAgent"
)

type Presentation string

const (
	PresentationConversation Presentation = "conversation"
	PresentationScreen       Presentation = "screen"
)

type Fleet struct {
	UpdatedAt time.Time  `json:"updatedAt"`
	Surfaces  []Instance `json:"surfaces"`
}

type Instance struct {
	ID         string      `json:"id"`
	Provider   string      `json:"provider"`
	Label      string      `json:"label"`
	Focused    bool        `json:"focused"`
	Status     string      `json:"status"`
	Workspaces []Workspace `json:"workspaces"`
}

type Workspace struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Focused bool   `json:"focused"`
	Status  string `json:"status"`
	Views   []View `json:"views"`
}

type View struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Focused bool     `json:"focused"`
	Status  string   `json:"status"`
	Targets []Target `json:"targets"`
}

type Target struct {
	ID                    string         `json:"id"`
	Label                 string         `json:"label"`
	Focused               bool           `json:"focused"`
	Status                string         `json:"status"`
	Agent                 *Agent         `json:"agent,omitempty"`
	Actions               []Action       `json:"actions"`
	Presentations         []Presentation `json:"presentations"`
	PreferredPresentation Presentation   `json:"preferredPresentation"`
}

type Agent struct {
	Name             string `json:"name"`
	Status           string `json:"status"`
	SessionProvider  string `json:"sessionProvider,omitempty"`
	SessionKind      string `json:"sessionKind,omitempty"`
	SessionReference string `json:"sessionReference,omitempty"`
}

type Output struct {
	TargetID  string `json:"targetId"`
	Text      string `json:"text"`
	Revision  uint64 `json:"revision"`
	Truncated bool   `json:"truncated"`
}
