package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agentd.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAcceptsInteractiveHerdrConfiguration(t *testing.T) {
	path := writeConfig(t, `{
		"listen":"127.0.0.1:7337",
		"dataDir":"data",
		"herdrBinary":"herdr",
		"agentArgs":[" --config=/worker.yml "],
		"agentEnv":{" LINEAR_API_KEY_FILE ":"/secrets/linear"},
		"coordinatorTarget":"w7:p1",
		"workspaces":[{"id":"repo","path":"workspace"}]
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(path)
	if cfg.DataDir != filepath.Join(base, "data") || cfg.Workspaces[0].Path != filepath.Join(base, "workspace") || cfg.Workspaces[0].Name != "repo" {
		t.Fatalf("normalized paths = %#v", cfg)
	}
	if len(cfg.AgentArgs) != 1 || cfg.AgentArgs[0] != "--config=/worker.yml" {
		t.Fatalf("agent args = %#v", cfg.AgentArgs)
	}
	if cfg.AgentEnv["LINEAR_API_KEY_FILE"] != "/secrets/linear" || cfg.CoordinatorTarget != "w7:p1" {
		t.Fatalf("Herdr worker config = %#v", cfg)
	}
}

func TestLoadRejectsLegacyAndNonLoopbackConfiguration(t *testing.T) {
	for name, test := range map[string]struct {
		body     string
		fragment string
	}{
		"legacy runner field": {`{"ompBinary":"omp"}`, "unknown field"},
		"legacy env files":    {`{"ompEnvFiles":{"TOKEN":"secret"}}`, "unknown field"},
		"all interfaces":      {`{"listen":"0.0.0.0:7337"}`, "loopback-only"},
		"missing port":        {`{"listen":"127.0.0.1"}`, "host:port"},
		"trailing object":     {`{} {}`, "exactly one"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, test.body))
			if err == nil || !strings.Contains(err.Error(), test.fragment) {
				t.Fatalf("error = %v, want fragment %q", err, test.fragment)
			}
		})
	}
}

func TestLoadRejectsInteractiveSessionOwnershipConflicts(t *testing.T) {
	for name, test := range map[string]struct {
		body     string
		fragment string
	}{
		"print mode": {
			`{"agentArgs":["--print"]}`,
			"Agentd-owned",
		},
		"session resume": {
			`{"agentArgs":["--resume=old"]}`,
			"Agentd-owned",
		},
		"duplicate normalized environment": {
			`{"agentEnv":{"TOKEN_FILE":"one"," TOKEN_FILE ":"two"}}`,
			"duplicate normalized",
		},
		"invalid environment name": {
			`{"agentEnv":{"NOT-VALID":"secret"}}`,
			"valid environment variable",
		},
		"invalid coordinator": {
			`{"coordinatorTarget":"Not Valid"}`,
			"coordinatorTarget",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, test.body))
			if err == nil || !strings.Contains(err.Error(), test.fragment) {
				t.Fatalf("error = %v, want fragment %q", err, test.fragment)
			}
		})
	}
}
