package workspaces

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/dkta-labs/agentd/internal/surface"
)

type herdrOverviewSource interface {
	Fleet(context.Context) (surface.Fleet, error)
}

type targetNotifier interface {
	NotifyTarget(targetID, targetURL, category, title, body string)
}

type monitoredPane struct {
	status string
	agent  string
}

type HerdrStatusMonitor struct {
	source      herdrOverviewSource
	notifier    targetNotifier
	logger      *slog.Logger
	initialized bool
	panes       map[string]monitoredPane
}

func NewHerdrStatusMonitor(source herdrOverviewSource, notifier targetNotifier, logger *slog.Logger) *HerdrStatusMonitor {
	return &HerdrStatusMonitor{
		source:   source,
		notifier: notifier,
		logger:   logger,
		panes:    make(map[string]monitoredPane),
	}
}

func (m *HerdrStatusMonitor) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	m.observe(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.observe(ctx)
		}
	}
}

func (m *HerdrStatusMonitor) observe(ctx context.Context) {
	if m == nil || m.source == nil || m.notifier == nil {
		return
	}
	fleet, err := m.source.Fleet(ctx)
	if err != nil {
		if m.logger != nil {
			m.logger.Debug("Herdr status poll failed", "error", err)
		}
		return
	}
	next := make(map[string]monitoredPane)
	for _, instance := range fleet.Surfaces {
		for _, workspace := range instance.Workspaces {
			for _, view := range workspace.Views {
				for _, target := range view.Targets {
					if target.Agent == nil || strings.HasPrefix(strings.ToLower(target.Agent.SessionReference), "agentd:") {
						continue
					}
					status := normalizeAgentStatus(target.Agent.Status)
					if status == "unknown" {
						status = normalizeAgentStatus(target.Status)
					}
					key := instance.ID + "\x00" + target.ID
					current := monitoredPane{status: status, agent: displayAgentName(target.Agent.Name)}
					next[key] = current
					previous, existed := m.panes[key]
					if m.initialized && existed && previous.status != current.status {
						m.notifyTransition(instance.ID, target.ID, previous, current)
					}
				}
			}
		}
	}
	m.panes = next
	m.initialized = true
}

func (m *HerdrStatusMonitor) notifyTransition(surfaceID, targetID string, previous, current monitoredPane) {
	category := ""
	title := ""
	switch current.status {
	case "blocked":
		category = "input_required"
		title = current.agent + " needs input"
	case "idle", "done":
		if previous.status != "working" && previous.status != "blocked" {
			return
		}
		category = "ready"
		title = current.agent + " is ready"
	case "failed", "exited":
		category = "failed"
		title = current.agent + " stopped"
	default:
		return
	}
	query := url.Values{"surface": {surfaceID}, "target": {targetID}}
	m.notifier.NotifyTarget(
		"surface:"+surfaceID+":"+targetID,
		"/?"+query.Encode(),
		category,
		title,
		"Open agentd to review this target.",
	)
}

func displayAgentName(name string) string {
	name = safeDisplayLabel(name, "Agent")
	switch strings.ToLower(name) {
	case "omp":
		return "OMP"
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "hermes":
		return "Hermes"
	default:
		return name
	}
}
