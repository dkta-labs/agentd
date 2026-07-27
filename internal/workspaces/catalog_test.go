package workspaces

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/herdr"
)

func TestAllowedRejectsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}

	catalog := NewCatalog(config.Config{WorkspaceRoots: []string{root}}, discardLogger())
	if resolved, allowed := catalog.allowed(escape); allowed {
		t.Fatalf("symlink escape resolved to %q and was allowed", resolved)
	}
}

func TestAllowedReturnsCanonicalContainedPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	catalog := NewCatalog(config.Config{WorkspaceRoots: []string{root}}, discardLogger())
	resolved, allowed := catalog.allowed(link)
	if !allowed {
		t.Fatal("contained symlink was rejected")
	}
	expected, err := canonicalPath(target)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != expected {
		t.Fatalf("resolved path = %q, want %q", resolved, expected)
	}
}

func TestListSanitizesConfiguredAndDiscoveredWorkspaceNames(t *testing.T) {
	root := t.TempDir()
	configuredPath := filepath.Join(root, "configured")
	discoveredPath := filepath.Join(root, "discovered")
	if err := os.Mkdir(configuredPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(discoveredPath, 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog(config.Config{
		WorkspaceRoots: []string{root},
		Workspaces: []config.Workspace{{
			ID: "configured", Name: "/Users/private/configured", Path: configuredPath,
		}},
	}, discardLogger())
	catalog.herdr = &fakeHerdrRuntime{
		sessions: []herdr.Session{{Name: "default", Running: true, SocketPath: "/tmp/herdr.sock"}},
		snapshots: map[string]herdr.Snapshot{"default": {
			Workspaces: []herdr.Workspace{{ID: "w1", Label: "/Users/private/discovered"}},
			Panes:      []herdr.Pane{{ID: "p1", WorkspaceID: "w1", CWD: discoveredPath}},
		}},
	}

	workspaces := catalog.List(context.Background())
	if len(workspaces) != 2 {
		t.Fatalf("workspace count = %d, want 2: %#v", len(workspaces), workspaces)
	}
	for _, workspace := range workspaces {
		if strings.Contains(workspace.Name, "/Users/private") || strings.ContainsAny(workspace.Name, "/\\") {
			t.Fatalf("workspace name exposed a host path: %#v", workspace)
		}
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
