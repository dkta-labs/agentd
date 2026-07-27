package herdr

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/dkta-labs/agentd/internal/config"
)

const attachmentSource = "agentd:omp"

type Controller struct {
	client Client
	logger *slog.Logger

	mu       sync.RWMutex
	bindings map[string]*binding
}

type binding struct {
	socketPath string
	tabID      string
	paneID     string
	sequence   uint64
}

func NewController(binary string, logger *slog.Logger) *Controller {
	return &Controller{
		client:   Client{Binary: binary},
		logger:   logger,
		bindings: make(map[string]*binding),
	}
}

func (c *Controller) Attach(ctx context.Context, workspace config.Workspace, sessionID string) error {
	if workspace.HerdrSocketPath == "" || workspace.HerdrWorkspaceID == "" {
		return nil
	}
	var created struct {
		Tab struct {
			ID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			ID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := c.client.Call(ctx, workspace.HerdrSocketPath, "tab.create", map[string]any{
		"workspace_id": workspace.HerdrWorkspaceID,
		"cwd":          workspace.Path,
		"label":        "agentd · omp",
		"focus":        false,
	}, &created); err != nil {
		return fmt.Errorf("create Herdr attachment: %w", err)
	}
	if created.Tab.ID == "" || created.RootPane.ID == "" {
		return fmt.Errorf("create Herdr attachment: response omitted tab or pane id")
	}
	attached := &binding{
		socketPath: workspace.HerdrSocketPath,
		tabID:      created.Tab.ID,
		paneID:     created.RootPane.ID,
	}
	c.mu.Lock()
	c.bindings[sessionID] = attached
	c.mu.Unlock()
	if err := c.report(ctx, sessionID, "idle", "mobile session "+shortID(sessionID)); err != nil {
		_ = c.Detach(context.Background(), sessionID)
		return err
	}
	if err := c.client.Call(ctx, attached.socketPath, "pane.report_agent_session", map[string]any{
		"pane_id":          attached.paneID,
		"source":           attachmentSource,
		"agent":            "omp",
		"agent_session_id": sessionID,
	}, nil); err != nil {
		_ = c.Detach(context.Background(), sessionID)
		return fmt.Errorf("report Herdr attachment session: %w", err)
	}
	return nil
}

func (c *Controller) AgentSession(socketPath, paneID string) (AgentSession, bool) {
	if c == nil {
		return AgentSession{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for sessionID, attached := range c.bindings {
		if attached.socketPath == socketPath && attached.paneID == paneID {
			return AgentSession{
				Source: attachmentSource,
				Agent:  "omp",
				Kind:   "id",
				Value:  sessionID,
			}, true
		}
	}
	return AgentSession{}, false
}

func (c *Controller) Report(ctx context.Context, sessionID, state, message string) {
	if err := c.report(ctx, sessionID, state, message); err != nil {
		c.logger.Warn("Herdr attachment state failed", "sessionId", sessionID, "state", state, "error", err)
	}
}

func (c *Controller) report(ctx context.Context, sessionID, state, message string) error {
	c.mu.Lock()
	attached := c.bindings[sessionID]
	if attached == nil {
		c.mu.Unlock()
		return nil
	}
	attached.sequence++
	sequence := attached.sequence
	socketPath := attached.socketPath
	paneID := attached.paneID
	c.mu.Unlock()
	if err := c.client.Call(ctx, socketPath, "pane.report_agent", map[string]any{
		"pane_id":          paneID,
		"source":           attachmentSource,
		"agent":            "omp",
		"state":            state,
		"message":          message,
		"seq":              sequence,
		"agent_session_id": sessionID,
	}, nil); err != nil {
		return fmt.Errorf("report Herdr attachment state: %w", err)
	}
	return nil
}

func (c *Controller) Detach(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	attached := c.bindings[sessionID]
	if attached != nil {
		delete(c.bindings, sessionID)
		attached.sequence++
	}
	c.mu.Unlock()
	if attached == nil {
		return nil
	}
	if err := c.client.Call(ctx, attached.socketPath, "pane.release_agent", map[string]any{
		"pane_id": attached.paneID,
		"source":  attachmentSource,
		"agent":   "omp",
		"seq":     attached.sequence,
	}, nil); err != nil {
		c.logger.Warn("Herdr attachment release failed", "sessionId", sessionID, "error", err)
	}
	if err := c.client.Call(ctx, attached.socketPath, "tab.close", map[string]any{
		"tab_id": attached.tabID,
	}, nil); err != nil {
		return fmt.Errorf("close Herdr attachment: %w", err)
	}
	return nil
}

func shortID(sessionID string) string {
	if len(sessionID) <= 12 {
		return sessionID
	}
	return sessionID[len(sessionID)-12:]
}
