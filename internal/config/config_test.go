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

func TestLoadAcceptsMinimalLoopbackConfiguration(t *testing.T) {
	path := writeConfig(t, `{"listen":"127.0.0.1:7337","dataDir":"data","ompBinary":"omp","workspaces":[{"id":"repo","path":"workspace"}]}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(path)
	if cfg.DataDir != filepath.Join(base, "data") || cfg.Workspaces[0].Path != filepath.Join(base, "workspace") || cfg.Workspaces[0].Name != "repo" {
		t.Fatalf("normalized config = %#v", cfg)
	}
}

func TestLoadRejectsLegacyAndNonLoopbackConfiguration(t *testing.T) {
	for name, test := range map[string]struct {
		body     string
		fragment string
	}{
		"legacy field":    {`{"listen":"127.0.0.1:7337","publicUrl":"https://example.test"}`, "unknown field"},
		"all interfaces":  {`{"listen":"0.0.0.0:7337"}`, "loopback-only"},
		"missing port":    {`{"listen":"127.0.0.1"}`, "host:port"},
		"trailing object": {`{} {}`, "exactly one"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, test.body))
			if err == nil || !strings.Contains(err.Error(), test.fragment) {
				t.Fatalf("error = %v, want fragment %q", err, test.fragment)
			}
		})
	}
}

func TestLoadNormalizesWorkerArgumentsAndEnvironmentFiles(t *testing.T) {
	path := writeConfig(t, `{
		"ompArgs":[" --max-time=30m "],
		"ompEnvFiles":{" LINEAR_API_KEY ":"secrets/linear"},
		"workspaces":[]
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OMPArgs) != 1 || cfg.OMPArgs[0] != "--max-time=30m" {
		t.Fatalf("omp args = %#v", cfg.OMPArgs)
	}
	wantPath := filepath.Join(filepath.Dir(path), "secrets", "linear")
	if len(cfg.OMPEnvFiles) != 1 || cfg.OMPEnvFiles["LINEAR_API_KEY"] != wantPath {
		t.Fatalf("omp env files = %#v, want LINEAR_API_KEY=%q", cfg.OMPEnvFiles, wantPath)
	}
}

func TestLoadRejectsWorkerConfigurationThatConflictsWithLifecycleOwnership(t *testing.T) {
	for name, test := range map[string]struct {
		body     string
		fragment string
	}{
		"session argument": {
			`{"ompArgs":["--session-dir=/tmp/stolen"]}`,
			"agentd-owned",
		},
		"reserved environment": {
			`{"ompEnvFiles":{"OMP_SESSION_ID":"secret"}}`,
			"reserved",
		},
		"duplicate normalized environment": {
			`{"ompEnvFiles":{"TOKEN":"one"," TOKEN ":"two"}}`,
			"duplicate normalized",
		},
		"invalid environment name": {
			`{"ompEnvFiles":{"NOT-VALID":"secret"}}`,
			"valid environment variable",
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
