package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/herdr"
	"github.com/dkta-labs/agentd/internal/surface"
)

type recordedHerdrCall struct {
	method string
	params map[string]any
}

type fakeHerdrRuntime struct {
	sessions  []herdr.Session
	snapshots map[string]herdr.Snapshot
	calls     []recordedHerdrCall
}

func (f *fakeHerdrRuntime) Sessions(context.Context) ([]herdr.Session, error) {
	return append([]herdr.Session(nil), f.sessions...), nil
}

func (f *fakeHerdrRuntime) Snapshot(_ context.Context, session herdr.Session) (herdr.Snapshot, error) {
	snapshot, ok := f.snapshots[session.Name]
	if !ok {
		return herdr.Snapshot{}, errors.New("snapshot unavailable")
	}
	return snapshot, nil
}

func (f *fakeHerdrRuntime) Call(_ context.Context, _ string, method string, params, result any) error {
	mapped, _ := params.(map[string]any)
	f.calls = append(f.calls, recordedHerdrCall{method: method, params: mapped})
	if method == "pane.read" {
		encoded, _ := json.Marshal(map[string]any{
			"read": map[string]any{
				"pane_id":   "p_allowed",
				"text":      "recent output",
				"revision":  42,
				"truncated": false,
			},
		})
		return json.Unmarshal(encoded, result)
	}
	return nil
}

type fakeAttachmentLookup map[string]herdr.AgentSession

func (f fakeAttachmentLookup) AgentSession(socketPath, paneID string) (herdr.AgentSession, bool) {
	session, ok := f[socketPath+"\x00"+paneID]
	return session, ok
}

func TestHerdrOverviewFiltersOutsideForegroundPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	allowedPath := filepath.Join(root, "repo")
	if err := os.Mkdir(allowedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &fakeHerdrRuntime{
		sessions: []herdr.Session{{Name: "default", Running: true, SocketPath: "/tmp/herdr.sock"}},
		snapshots: map[string]herdr.Snapshot{"default": {
			Workspaces: []herdr.Workspace{{ID: "w1", Label: "/Users/private/repo", Focused: true}},
			Tabs:       []herdr.Tab{{ID: "t1", WorkspaceID: "w1", Label: "agents /Users/private/repo", Focused: true}},
			Panes: []herdr.Pane{
				{
					ID: "p_allowed", WorkspaceID: "w1", TabID: "t1", CWD: allowedPath, ForegroundCWD: allowedPath,
					Focused: true, Agent: "omp", AgentStatus: "working", TerminalTitleStripped: "OMP /Users/private/repo",
					AgentSession: &herdr.AgentSession{Source: "agentd:omp", Agent: "omp", Kind: "id", Value: "ses_test"},
				},
				{ID: "p_escape", WorkspaceID: "w1", TabID: "t1", CWD: allowedPath, ForegroundCWD: outside, Agent: "claude", AgentStatus: "blocked"},
			},
		}},
	}
	catalog := NewCatalog(config.Config{WorkspaceRoots: []string{root}}, discardLogger())
	catalog.herdr = fake

	fleet, err := catalog.Fleet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet.Surfaces) != 1 || len(fleet.Surfaces[0].Workspaces) != 1 {
		t.Fatalf("unexpected fleet: %#v", fleet)
	}
	targets := fleet.Surfaces[0].Workspaces[0].Views[0].Targets
	if len(targets) != 1 || targets[0].ID != "p_allowed" || targets[0].Label != "OMP" {
		t.Fatalf("unexpected visible targets: %#v", targets)
	}
	if fleet.Surfaces[0].Workspaces[0].Label != "repo" || fleet.Surfaces[0].Workspaces[0].Views[0].Label != "Tab 1" {
		t.Fatalf("host-path labels were not sanitized: %#v", fleet.Surfaces[0].Workspaces[0])
	}
	if targets[0].Agent == nil || targets[0].Agent.SessionProvider != "agentd:omp" || targets[0].Agent.SessionReference != "ses_test" {
		t.Fatalf("generic agent attachment missing: %#v", targets[0].Agent)
	}
	if len(targets[0].Presentations) != 2 ||
		targets[0].Presentations[0] != surface.PresentationConversation ||
		targets[0].Presentations[1] != surface.PresentationScreen ||
		targets[0].PreferredPresentation != surface.PresentationConversation {
		t.Fatalf("managed target presentation contract = %#v, preferred %q", targets[0].Presentations, targets[0].PreferredPresentation)
	}
	encoded, err := json.Marshal(fleet)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || containsAny(string(encoded), root, outside, "/tmp/herdr.sock", "/Users/private") {
		t.Fatalf("overview exposed host paths or socket: %s", encoded)
	}
	if err := catalog.FocusTarget(context.Background(), "default", "p_escape"); !errors.Is(err, surface.ErrTargetNotFound) {
		t.Fatalf("outside target focus error = %v, want %v", err, surface.ErrTargetNotFound)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("outside pane reached Herdr: %#v", fake.calls)
	}
}
func TestHerdrOverviewReportsCompleteSnapshotFailure(t *testing.T) {
	root := t.TempDir()
	fake := &fakeHerdrRuntime{
		sessions:  []herdr.Session{{Name: "default", Running: true, SocketPath: "/tmp/herdr.sock"}},
		snapshots: map[string]herdr.Snapshot{},
	}
	catalog := NewCatalog(config.Config{WorkspaceRoots: []string{root}}, discardLogger())
	catalog.herdr = fake
	if _, err := catalog.Fleet(context.Background()); err == nil {
		t.Fatal("complete Herdr snapshot failure was reported as an empty healthy overview")
	}
}

func TestHerdrOverviewUsesControllerAttachmentFallback(t *testing.T) {
	root := t.TempDir()
	fake := &fakeHerdrRuntime{
		sessions: []herdr.Session{{Name: "default", Running: true, SocketPath: "/tmp/herdr.sock"}},
		snapshots: map[string]herdr.Snapshot{"default": {
			Workspaces: []herdr.Workspace{{ID: "w1", Label: "Repo"}},
			Tabs:       []herdr.Tab{{ID: "t1", WorkspaceID: "w1"}},
			Panes:      []herdr.Pane{{ID: "p1", WorkspaceID: "w1", TabID: "t1", CWD: root, Agent: "omp", AgentStatus: "idle"}},
		}},
	}
	attachments := fakeAttachmentLookup{
		"/tmp/herdr.sock\x00p1": {Source: "agentd:omp", Agent: "omp", Kind: "id", Value: "ses_fallback"},
	}
	catalog := NewCatalog(config.Config{WorkspaceRoots: []string{root}}, discardLogger(), attachments)
	catalog.herdr = fake

	fleet, err := catalog.Fleet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	agent := fleet.Surfaces[0].Workspaces[0].Views[0].Targets[0].Agent
	if agent == nil || agent.SessionProvider != "agentd:omp" || agent.SessionKind != "id" || agent.SessionReference != "ses_fallback" {
		t.Fatalf("controller attachment missing from fleet: %#v", agent)
	}
	target := fleet.Surfaces[0].Workspaces[0].Views[0].Targets[0]
	for _, action := range target.Actions {
		if action == surface.ActionSend {
			t.Fatalf("agentd-managed OMP target exposed send: %#v", target.Actions)
		}
	}
	if len(target.Presentations) != 2 ||
		target.Presentations[0] != surface.PresentationConversation ||
		target.Presentations[1] != surface.PresentationScreen ||
		target.PreferredPresentation != surface.PresentationConversation {
		t.Fatalf("fallback target presentation contract = %#v, preferred %q", target.Presentations, target.PreferredPresentation)
	}
	if err := catalog.SendToTarget(context.Background(), "default", "p1", "misdirected"); !errors.Is(err, surface.ErrAgentManaged) {
		t.Fatalf("managed OMP send error = %v, want %v", err, surface.ErrAgentManaged)
	}
	if err := catalog.CloseTarget(context.Background(), "default", "p1"); !errors.Is(err, surface.ErrAgentManaged) {
		t.Fatalf("managed OMP close error = %v, want %v", err, surface.ErrAgentManaged)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("managed OMP mutation reached the Herdr pane: %#v", fake.calls)
	}
}

func TestHerdrPaneActionsRevalidateAndUseExpectedMethods(t *testing.T) {
	root := t.TempDir()
	fake := &fakeHerdrRuntime{
		sessions: []herdr.Session{{Name: "agents", Running: true, SocketPath: "/tmp/agents.sock"}},
		snapshots: map[string]herdr.Snapshot{"agents": {
			Workspaces: []herdr.Workspace{{ID: "w1", Label: "Repo"}},
			Tabs:       []herdr.Tab{{ID: "t1", WorkspaceID: "w1"}},
			Panes:      []herdr.Pane{{ID: "p1", WorkspaceID: "w1", TabID: "t1", CWD: root, Agent: "claude", AgentStatus: "idle"}},
		}},
	}
	catalog := NewCatalog(config.Config{WorkspaceRoots: []string{root}}, discardLogger())
	catalog.herdr = fake
	ctx := context.Background()

	output, err := catalog.TargetOutput(ctx, "agents", "p1", 500)
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "recent output" || output.Revision != 42 || fake.calls[0].params["lines"] != 200 {
		t.Fatalf("unexpected pane output: %#v calls=%#v", output, fake.calls)
	}
	if err := catalog.FocusTarget(ctx, "agents", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SendToTarget(ctx, "agents", "p1", "  continue\n"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.CloseTarget(ctx, "agents", "p1"); err != nil {
		t.Fatal(err)
	}
	methods := []string{fake.calls[0].method, fake.calls[1].method, fake.calls[2].method, fake.calls[3].method}
	want := []string{"pane.read", "pane.focus", "pane.send_input", "pane.close"}
	for index := range want {
		if methods[index] != want[index] {
			t.Fatalf("call methods = %#v, want %#v", methods, want)
		}
	}
	if got := fake.calls[2].params["text"]; got != "  continue\n" {
		t.Fatalf("agent prompt whitespace changed: %q", got)
	}
	if got := fake.calls[2].params["pane_id"]; got != "p1" {
		t.Fatalf("prompt pane = %q, want p1", got)
	}
	if got, ok := fake.calls[2].params["keys"].([]string); !ok || len(got) != 1 || got[0] != "enter" {
		t.Fatalf("prompt submit keys = %#v, want [enter]", fake.calls[2].params["keys"])
	}
	workspace, err := catalog.WorkspaceForTarget(ctx, "agents", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.HerdrWorkspaceID != "w1" || workspace.Path == "" || workspace.HerdrSocketPath != "/tmp/agents.sock" {
		t.Fatalf("unexpected launch workspace: %#v", workspace)
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if candidate != "" && strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
