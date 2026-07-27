package workspaces

import (
	"context"
	"testing"

	"github.com/dkta-labs/agentd/internal/surface"
)

type mutableOverviewSource struct {
	fleet surface.Fleet
}

func (s *mutableOverviewSource) Fleet(context.Context) (surface.Fleet, error) {
	return s.fleet, nil
}

type recordedTargetNotification struct {
	targetID  string
	targetURL string
	category  string
	title     string
	body      string
}

type recordingTargetNotifier []recordedTargetNotification

func (n *recordingTargetNotifier) NotifyTarget(targetID, targetURL, category, title, body string) {
	*n = append(*n, recordedTargetNotification{
		targetID: targetID, targetURL: targetURL, category: category, title: title, body: body,
	})
}

func TestHerdrStatusMonitorNotifiesTransitionsWithoutStartupNoise(t *testing.T) {
	source := &mutableOverviewSource{fleet: statusFleet("working", "external")}
	notifier := recordingTargetNotifier{}
	monitor := NewHerdrStatusMonitor(source, &notifier, discardLogger())

	monitor.observe(context.Background())
	if len(notifier) != 0 {
		t.Fatalf("initial observation sent notifications: %#v", notifier)
	}

	source.fleet = statusFleet("blocked", "external")
	monitor.observe(context.Background())
	if len(notifier) != 1 {
		t.Fatalf("blocked transition notifications = %d, want 1", len(notifier))
	}
	blocked := notifier[0]
	if blocked.targetID != "surface:default:p1" || blocked.targetURL != "/?surface=default&target=p1" || blocked.category != "input_required" || blocked.title != "Claude needs input" {
		t.Fatalf("unexpected blocked notification: %#v", blocked)
	}
	if blocked.body != "Open agentd to review this target." {
		t.Fatalf("notification body exposed target content: %q", blocked.body)
	}

	monitor.observe(context.Background())
	if len(notifier) != 1 {
		t.Fatalf("unchanged status sent duplicate notification: %#v", notifier)
	}

	source.fleet = statusFleet("idle", "external")
	monitor.observe(context.Background())
	if len(notifier) != 2 || notifier[1].category != "ready" || notifier[1].title != "Claude is ready" {
		t.Fatalf("unexpected ready notification: %#v", notifier)
	}
}

func TestHerdrStatusMonitorDefersAgentdOMPAlertsToSessionManager(t *testing.T) {
	source := &mutableOverviewSource{fleet: statusFleet("working", "agentd:omp")}
	notifier := recordingTargetNotifier{}
	monitor := NewHerdrStatusMonitor(source, &notifier, discardLogger())
	monitor.observe(context.Background())
	source.fleet = statusFleet("blocked", "agentd:omp")
	monitor.observe(context.Background())
	if len(notifier) != 0 {
		t.Fatalf("agentd OMP transition was notified twice: %#v", notifier)
	}
}

func statusFleet(status, provider string) surface.Fleet {
	return surface.Fleet{Surfaces: []surface.Instance{{
		ID: "default", Workspaces: []surface.Workspace{{
			ID: "w1", Views: []surface.View{{
				ID: "t1", Targets: []surface.Target{{
					ID: "p1", Label: "secret terminal title", Status: status,
					Agent: &surface.Agent{Name: "claude", Status: status, SessionProvider: provider, SessionReference: provider},
				}},
			}},
		}},
	}}}
}
