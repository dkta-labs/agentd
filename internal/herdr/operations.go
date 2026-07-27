package herdr

import "time"

type Overview struct {
	UpdatedAt time.Time           `json:"updatedAt"`
	Sessions  []OperationsSession `json:"sessions"`
}

type OperationsSession struct {
	ID         string                `json:"id"`
	Label      string                `json:"label"`
	Focused    bool                  `json:"focused"`
	Status     string                `json:"status"`
	Workspaces []OperationsWorkspace `json:"workspaces"`
}

type OperationsWorkspace struct {
	ID      string          `json:"id"`
	Label   string          `json:"label"`
	Focused bool            `json:"focused"`
	Status  string          `json:"status"`
	Tabs    []OperationsTab `json:"tabs"`
}

type OperationsTab struct {
	ID      string           `json:"id"`
	Label   string           `json:"label"`
	Focused bool             `json:"focused"`
	Status  string           `json:"status"`
	Panes   []OperationsPane `json:"panes"`
}

type OperationsPane struct {
	ID      string           `json:"id"`
	Label   string           `json:"label"`
	Focused bool             `json:"focused"`
	Status  string           `json:"status"`
	Agent   *OperationsAgent `json:"agent,omitempty"`
	Actions []string         `json:"actions"`
}

type OperationsAgent struct {
	Name             string `json:"name"`
	Status           string `json:"status"`
	SessionSource    string `json:"sessionSource,omitempty"`
	SessionKind      string `json:"sessionKind,omitempty"`
	SessionReference string `json:"sessionReference,omitempty"`
}

type PaneOutput struct {
	PaneID    string `json:"paneId"`
	Text      string `json:"text"`
	Revision  uint64 `json:"revision"`
	Truncated bool   `json:"truncated"`
}
