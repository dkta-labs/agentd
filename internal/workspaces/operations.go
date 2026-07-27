package workspaces

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/herdr"
	"github.com/dkta-labs/agentd/internal/surface"
)

func (c *Catalog) Fleet(ctx context.Context) (surface.Fleet, error) {
	fleet := surface.Fleet{UpdatedAt: time.Now().UTC(), Surfaces: []surface.Instance{}}
	sessions, err := c.herdr.Sessions(ctx)
	if err != nil {
		return fleet, err
	}
	runningSessions := 0
	snapshotSuccesses := 0
	var snapshotError error
	for _, session := range sessions {
		if !session.Running || session.SocketPath == "" {
			continue
		}
		runningSessions++
		snapshot, err := c.herdr.Snapshot(ctx, session)
		if err != nil {
			if c.logger != nil {
				c.logger.Warn("Herdr operations snapshot failed", "session", session.Name, "error", err)
			}
			snapshotError = err
			continue
		}
		snapshotSuccesses++
		instance := c.surfaceInstance(session, snapshot)
		if len(instance.Workspaces) > 0 {
			fleet.Surfaces = append(fleet.Surfaces, instance)
		}
	}
	if runningSessions > 0 && snapshotSuccesses == 0 && snapshotError != nil {
		return fleet, fmt.Errorf("read Herdr surface state: %w", snapshotError)
	}
	return fleet, nil
}

func (c *Catalog) TargetOutput(ctx context.Context, surfaceID, targetID string, lines int) (surface.Output, error) {
	session, _, _, err := c.herdrPaneTarget(ctx, surfaceID, targetID)
	if err != nil {
		return surface.Output{}, err
	}
	if lines <= 0 {
		lines = 80
	} else if lines > 200 {
		lines = 200
	}
	var result struct {
		Read struct {
			PaneID    string `json:"pane_id"`
			Text      string `json:"text"`
			Revision  uint64 `json:"revision"`
			Truncated bool   `json:"truncated"`
		} `json:"read"`
	}
	if err := c.herdr.Call(ctx, session.SocketPath, "pane.read", map[string]any{
		"pane_id": targetID,
		"source":  "recent_unwrapped",
		"lines":   lines,
		"format":  "text",
	}, &result); err != nil {
		return surface.Output{}, err
	}
	return surface.Output{
		TargetID:  result.Read.PaneID,
		Text:      result.Read.Text,
		Revision:  result.Read.Revision,
		Truncated: result.Read.Truncated,
	}, nil
}

func (c *Catalog) FocusTarget(ctx context.Context, surfaceID, targetID string) error {
	session, _, _, err := c.herdrPaneTarget(ctx, surfaceID, targetID)
	if err != nil {
		return err
	}
	return c.herdr.Call(ctx, session.SocketPath, "pane.focus", map[string]any{"pane_id": targetID}, nil)
}

func (c *Catalog) SendToTarget(ctx context.Context, surfaceID, targetID, text string) error {
	session, pane, _, err := c.herdrPaneTarget(ctx, surfaceID, targetID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("message must not be empty")
	}
	if len(text) > 32*1024 {
		return errors.New("message exceeds 32 KiB")
	}
	if pane.Agent == "" {
		return surface.ErrAgentUnavailable
	}
	if isAgentdOMPSession(c.agentSessionForPane(session, pane)) {
		return surface.ErrAgentManaged
	}
	return c.herdr.Call(ctx, session.SocketPath, "pane.send_input", map[string]any{
		"pane_id": targetID,
		"text":    text,
		"keys":    []string{"enter"},
	}, nil)
}

func (c *Catalog) CloseTarget(ctx context.Context, surfaceID, targetID string) error {
	session, pane, _, err := c.herdrPaneTarget(ctx, surfaceID, targetID)
	if err != nil {
		return err
	}
	if isAgentdOMPSession(c.agentSessionForPane(session, pane)) {
		return surface.ErrAgentManaged
	}
	return c.herdr.Call(ctx, session.SocketPath, "pane.close", map[string]any{"pane_id": targetID}, nil)
}

func (c *Catalog) WorkspaceForTarget(ctx context.Context, surfaceID, targetID string) (config.Workspace, error) {
	session, pane, path, err := c.herdrPaneTarget(ctx, surfaceID, targetID)
	if err != nil {
		return config.Workspace{}, err
	}
	snapshot, err := c.herdr.Snapshot(ctx, session)
	if err != nil {
		return config.Workspace{}, err
	}
	label := ""
	for _, workspace := range snapshot.Workspaces {
		if workspace.ID == pane.WorkspaceID {
			label = workspace.Label
			break
		}
	}
	return config.Workspace{
		ID:               herdrWorkspaceID(session.Name, pane.WorkspaceID, path),
		Name:             herdrWorkspaceName(session.Name, label, path),
		Path:             path,
		HerdrSession:     session.Name,
		HerdrWorkspaceID: pane.WorkspaceID,
		HerdrSocketPath:  session.SocketPath,
	}, nil
}

func (c *Catalog) surfaceInstance(session herdr.Session, snapshot herdr.Snapshot) surface.Instance {
	targetsByView := make(map[string][]surface.Target)
	workspaceFallbackLabels := make(map[string]string)
	for _, pane := range snapshot.Panes {
		path, allowed := c.allowed(paneSecurityPath(pane))
		if !allowed {
			continue
		}
		if _, exists := workspaceFallbackLabels[pane.WorkspaceID]; !exists {
			workspaceFallbackLabels[pane.WorkspaceID] = safeDisplayLabel(filepath.Base(path), "Workspace")
		}
		view := surface.Target{
			ID:                    pane.ID,
			Label:                 paneDisplayLabel(pane),
			Focused:               pane.Focused,
			Status:                normalizeAgentStatus(pane.AgentStatus),
			Actions:               []surface.Action{surface.ActionFocus, surface.ActionRead, surface.ActionClose, surface.ActionLaunchAgent},
			Presentations:         []surface.Presentation{surface.PresentationScreen},
			PreferredPresentation: surface.PresentationScreen,
		}
		if pane.Agent != "" {
			view.Agent = &surface.Agent{Name: displayAgentName(pane.Agent), Status: view.Status}
			agentSession := c.agentSessionForPane(session, pane)
			if agentSession != nil && agentSession.Kind == "id" && safeReference(agentSession.Source) && safeReference(agentSession.Value) {
				view.Agent.SessionProvider = agentSession.Source
				view.Agent.SessionKind = agentSession.Kind
				view.Agent.SessionReference = agentSession.Value
				if isAgentdOMPSession(agentSession) {
					view.Presentations = append([]surface.Presentation{surface.PresentationConversation}, view.Presentations...)
					view.PreferredPresentation = surface.PresentationConversation
				}
			}
			if !isAgentdOMPSession(agentSession) {
				view.Actions = append(view.Actions, surface.ActionSend)
			}
		}
		targetsByView[pane.TabID] = append(targetsByView[pane.TabID], view)
	}

	viewsByWorkspace := make(map[string][]surface.View)
	for index, tab := range snapshot.Tabs {
		targets := targetsByView[tab.ID]
		if len(targets) == 0 {
			continue
		}
		label := safeDisplayLabel(tab.Label, "")
		if label == "" {
			label = fmt.Sprintf("Tab %d", index+1)
		}
		viewsByWorkspace[tab.WorkspaceID] = append(viewsByWorkspace[tab.WorkspaceID], surface.View{
			ID:      tab.ID,
			Label:   label,
			Focused: tab.Focused,
			Status:  aggregateTargetStatus(targets),
			Targets: targets,
		})
	}

	instance := surface.Instance{
		ID:         session.Name,
		Provider:   "herdr",
		Label:      safeDisplayLabel(session.Name, "Herdr"),
		Workspaces: []surface.Workspace{},
	}
	if instance.Label == "default" {
		instance.Label = "Herdr"
	}
	for index, workspace := range snapshot.Workspaces {
		views := viewsByWorkspace[workspace.ID]
		if len(views) == 0 {
			continue
		}
		label := safeDisplayLabel(workspace.Label, workspaceFallbackLabels[workspace.ID])
		if label == "" || label == "~" {
			label = fmt.Sprintf("Workspace %d", index+1)
		}
		workspaceView := surface.Workspace{
			ID:      workspace.ID,
			Label:   label,
			Focused: workspace.Focused,
			Status:  aggregateViewStatus(views),
			Views:   views,
		}
		instance.Workspaces = append(instance.Workspaces, workspaceView)
	}
	instance.Status = aggregateWorkspaceStatus(instance.Workspaces)
	for _, workspace := range instance.Workspaces {
		if workspace.Focused {
			instance.Focused = true
			break
		}
	}
	return instance
}

func (c *Catalog) herdrPaneTarget(ctx context.Context, sessionID, paneID string) (herdr.Session, herdr.Pane, string, error) {
	sessions, err := c.herdr.Sessions(ctx)
	if err != nil {
		return herdr.Session{}, herdr.Pane{}, "", err
	}
	for _, session := range sessions {
		if session.Name != sessionID || !session.Running || session.SocketPath == "" {
			continue
		}
		snapshot, err := c.herdr.Snapshot(ctx, session)
		if err != nil {
			return herdr.Session{}, herdr.Pane{}, "", err
		}
		for _, pane := range snapshot.Panes {
			if pane.ID != paneID {
				continue
			}
			path, allowed := c.allowed(paneSecurityPath(pane))
			if !allowed {
				return herdr.Session{}, herdr.Pane{}, "", surface.ErrTargetNotFound
			}
			return session, pane, path, nil
		}
	}
	return herdr.Session{}, herdr.Pane{}, "", surface.ErrTargetNotFound
}

func paneSecurityPath(pane herdr.Pane) string {
	if pane.ForegroundCWD != "" {
		return pane.ForegroundCWD
	}
	return pane.CWD
}

func paneDisplayLabel(pane herdr.Pane) string {
	fallback := "Terminal pane"
	if strings.TrimSpace(pane.Agent) != "" {
		fallback = displayAgentName(pane.Agent)
	}
	return safeDisplayLabel(pane.TerminalTitleStripped, fallback)
}

func (c *Catalog) agentSessionForPane(session herdr.Session, pane herdr.Pane) *herdr.AgentSession {
	if pane.AgentSession != nil {
		return pane.AgentSession
	}
	if c.attachments != nil {
		if attached, ok := c.attachments.AgentSession(session.SocketPath, pane.ID); ok {
			return &attached
		}
	}
	return nil
}

func isAgentdOMPSession(session *herdr.AgentSession) bool {
	return session != nil &&
		session.Source == "agentd:omp" &&
		session.Kind == "id" &&
		session.Value != ""
}

func safeDisplayLabel(label, fallback string) string {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 96 || strings.ContainsAny(label, "/\\\n\r\t") {
		return fallback
	}
	for _, character := range label {
		if unicode.IsControl(character) {
			return fallback
		}
	}
	return label
}

func safeReference(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("._:-", character) {
			continue
		}
		return false
	}
	return true
}

func normalizeAgentStatus(status string) string {
	switch status {
	case "working", "blocked", "idle", "done", "failed", "exited":
		return status
	default:
		return "unknown"
	}
}

func aggregateTargetStatus(targets []surface.Target) string {
	statuses := make([]string, 0, len(targets))
	for _, target := range targets {
		statuses = append(statuses, target.Status)
	}
	return aggregateStatus(statuses)
}

func aggregateViewStatus(views []surface.View) string {
	statuses := make([]string, 0, len(views))
	for _, view := range views {
		statuses = append(statuses, view.Status)
	}
	return aggregateStatus(statuses)
}

func aggregateWorkspaceStatus(workspaces []surface.Workspace) string {
	statuses := make([]string, 0, len(workspaces))
	for _, workspace := range workspaces {
		statuses = append(statuses, workspace.Status)
	}
	return aggregateStatus(statuses)
}

func aggregateStatus(statuses []string) string {
	priority := map[string]int{"unknown": 0, "done": 1, "idle": 2, "working": 3, "blocked": 4, "exited": 5, "failed": 6}
	selected := "unknown"
	for _, status := range statuses {
		if priority[status] > priority[selected] {
			selected = status
		}
	}
	return selected
}
