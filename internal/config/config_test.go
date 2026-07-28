package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadNormalizesConfiguredPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agentd.json")
	content := `{
		"listen": "127.0.0.1:9000",
		"dataDir": "state",
		"workspaces": [{"id": "agentd", "name": "", "path": "repo"}]
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := cfg.DataDir, filepath.Join(dir, "state"); got != want {
		t.Fatalf("DataDir = %q, want %q", got, want)
	}
	if got, want := cfg.Workspaces[0].Path, filepath.Join(dir, "repo"); got != want {
		t.Fatalf("workspace path = %q, want %q", got, want)
	}
	if got, want := cfg.Workspaces[0].Name, "agentd"; got != want {
		t.Fatalf("workspace name = %q, want %q", got, want)
	}
}

func TestLoadRejectsDuplicateWorkspaceIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agentd.json")
	content := `{
		"listen": "127.0.0.1:9000",
		"dataDir": "state",
		"workspaces": [
			{"id": "same", "path": "one"},
			{"id": "same", "path": "two"}
		]
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load() succeeded with duplicate workspace IDs")
	}
}

func TestLoadRequiresAuthAndValidSubjectForPush(t *testing.T) {
	for name, content := range map[string]string{
		"auth disabled": `{
			"listen": "127.0.0.1:9000",
			"dataDir": "state",
			"push": {"enabled": true, "subject": "mailto:test@example.com"}
		}`,
		"invalid subject": `{
			"listen": "127.0.0.1:9000",
			"dataDir": "state",
			"auth": {"enabled": true},
			"push": {"enabled": true, "subject": "test@example.com"}
		}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agentd.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load() accepted invalid push configuration")
			}
		})
	}
}

func TestLoadNormalizesHTTPSPublicURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentd.json")
	if err := os.WriteFile(path, []byte(`{
		"listen": "127.0.0.1:9000",
		"publicUrl": " https://agent.example/ ",
		"dataDir": "state"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://agent.example" {
		t.Fatalf("PublicURL = %q, want https://agent.example", cfg.PublicURL)
	}
}

func TestLoadRejectsUnsafePublicURL(t *testing.T) {
	for _, publicURL := range []string{
		"http://agent.example",
		"https://user:secret@agent.example",
		"https://agent.example/pair",
		"https://agent.example?token=secret",
		"https://agent.example/#enroll=secret",
	} {
		t.Run(publicURL, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agentd.json")
			content := fmt.Sprintf(`{
				"listen": "127.0.0.1:9000",
				"publicUrl": %q,
				"dataDir": "state"
			}`, publicURL)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("Load() accepted unsafe public URL %q", publicURL)
			}
		})
	}
}
