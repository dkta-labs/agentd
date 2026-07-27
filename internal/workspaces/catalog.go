package workspaces

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/herdr"
)

type herdrRuntime interface {
	Sessions(context.Context) ([]herdr.Session, error)
	Snapshot(context.Context, herdr.Session) (herdr.Snapshot, error)
	Call(context.Context, string, string, any, any) error
}

type agentAttachmentLookup interface {
	AgentSession(socketPath, paneID string) (herdr.AgentSession, bool)
}

type Catalog struct {
	configured  []config.Workspace
	roots       []string
	herdr       herdrRuntime
	logger      *slog.Logger
	attachments agentAttachmentLookup

	mu        sync.Mutex
	cached    []config.Workspace
	refreshed time.Time
}

func NewCatalog(cfg config.Config, logger *slog.Logger, attachmentLookups ...agentAttachmentLookup) *Catalog {
	roots := make([]string, 0, len(cfg.WorkspaceRoots))
	var attachments agentAttachmentLookup
	if len(attachmentLookups) > 0 {
		attachments = attachmentLookups[0]
	}
	for _, root := range cfg.WorkspaceRoots {
		resolved, err := canonicalPath(root)
		if err != nil {
			if logger != nil {
				logger.Warn("Workspace root canonicalization failed", "path", root, "error", err)
			}
			continue
		}
		roots = append(roots, resolved)
	}
	configured := make([]config.Workspace, 0, len(cfg.Workspaces))
	for _, workspace := range cfg.Workspaces {
		resolved, err := canonicalPath(workspace.Path)
		if err != nil {
			if logger != nil {
				logger.Warn("Configured workspace canonicalization failed", "id", workspace.ID, "path", workspace.Path, "error", err)
			}
			continue
		}
		workspace.Path = resolved
		workspace.Name = safeDisplayLabel(workspace.Name, filepath.Base(resolved))
		configured = append(configured, workspace)
	}
	return &Catalog{
		configured:  configured,
		roots:       roots,
		herdr:       herdr.Client{Binary: cfg.HerdrBinary},
		logger:      logger,
		attachments: attachments,
	}
}

func (c *Catalog) List(ctx context.Context) []config.Workspace {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.refreshed) < time.Second && c.cached != nil {
		return append([]config.Workspace(nil), c.cached...)
	}
	discovered, err := c.discover(ctx)
	if err != nil {
		c.logger.Warn("Herdr workspace discovery failed", "error", err)
	}
	byPath := make(map[string]config.Workspace, len(c.configured)+len(discovered))
	for _, workspace := range c.configured {
		byPath[filepath.Clean(workspace.Path)] = workspace
	}
	for _, workspace := range discovered {
		byPath[filepath.Clean(workspace.Path)] = workspace
	}
	result := make([]config.Workspace, 0, len(byPath))
	for _, workspace := range byPath {
		result = append(result, workspace)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].ID < result[j].ID
		}
		return result[i].Name < result[j].Name
	})
	c.cached = result
	c.refreshed = time.Now()
	return append([]config.Workspace(nil), result...)
}

func (c *Catalog) Resolve(ctx context.Context, id string) (config.Workspace, error) {
	for _, workspace := range c.List(ctx) {
		if workspace.ID == id {
			return workspace, nil
		}
	}
	return config.Workspace{}, fmt.Errorf("workspace %q not found", id)
}

func (c *Catalog) discover(ctx context.Context) ([]config.Workspace, error) {
	sessions, err := c.herdr.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	var discovered []config.Workspace
	for _, session := range sessions {
		if !session.Running || session.SocketPath == "" {
			continue
		}
		snapshot, err := c.herdr.Snapshot(ctx, session)
		if err != nil {
			c.logger.Warn("Herdr session snapshot failed", "session", session.Name, "error", err)
			continue
		}
		workspaceLabels := make(map[string]string, len(snapshot.Workspaces))
		for _, workspace := range snapshot.Workspaces {
			workspaceLabels[workspace.ID] = workspace.Label
		}
		seen := make(map[string]struct{})
		for _, pane := range snapshot.Panes {
			path := pane.ForegroundCWD
			if path == "" {
				path = pane.CWD
			}
			path = filepath.Clean(path)
			if path == "." {
				continue
			}
			resolvedPath, allowed := c.allowed(path)
			if !allowed {
				continue
			}
			path = resolvedPath
			key := pane.WorkspaceID + "\x00" + path
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			discovered = append(discovered, config.Workspace{
				ID:               herdrWorkspaceID(session.Name, pane.WorkspaceID, path),
				Name:             herdrWorkspaceName(session.Name, workspaceLabels[pane.WorkspaceID], path),
				Path:             path,
				HerdrSession:     session.Name,
				HerdrWorkspaceID: pane.WorkspaceID,
				HerdrSocketPath:  session.SocketPath,
			})
		}
	}
	return discovered, nil
}

func (c *Catalog) allowed(path string) (string, bool) {
	if strings.TrimSpace(path) == "" {
		return "", false
	}
	resolved, err := canonicalPath(path)
	if err != nil {
		return "", false
	}
	for _, root := range c.roots {
		relative, err := filepath.Rel(root, resolved)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			return resolved, true
		}
	}
	return "", false
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func herdrWorkspaceID(sessionName, workspaceID, path string) string {
	digest := sha256.Sum256([]byte(path))
	return fmt.Sprintf("herdr:%s:%s:%x", sessionName, workspaceID, digest[:6])
}

func herdrWorkspaceName(sessionName, workspaceLabel, path string) string {
	base := safeDisplayLabel(filepath.Base(path), "Workspace")
	name := safeDisplayLabel(workspaceLabel, "")
	if name == "" || name == "~" || name == "workspace" || name == base {
		name = base
	} else {
		name += " · " + base
	}
	sessionName = safeDisplayLabel(sessionName, "")
	if sessionName != "" && sessionName != "default" {
		name = sessionName + " / " + name
	}
	return name
}
