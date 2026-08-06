package githubhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigNormalizesRelativePaths(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "hook.json")
	content := `{
		"listen":"127.0.0.1:7448",
		"agentdUrl":"http://127.0.0.1:7337",
		"secretFile":"hook.secret",
		"dataDir":"data",
		"rules":[{
			"id":"merged-pr",
			"event":"pull_request",
			"action":"closed",
			"repository":"dkta-labs/agentd",
			"merged":true,
			"jobId":"job-webhook"
		}]
	}`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecretFile != filepath.Join(directory, "hook.secret") || cfg.DataDir != filepath.Join(directory, "data") {
		t.Fatalf("normalized config = %#v", cfg)
	}
	if cfg.Rules[0].Merged == nil || !*cfg.Rules[0].Merged {
		t.Fatalf("merged rule = %#v", cfg.Rules[0])
	}
}

func TestLoadConfigRejectsRemoteBindingsAndUnknownFields(t *testing.T) {
	for name, test := range map[string]struct {
		replacement string
		expected    string
	}{
		"remote listen": {`"listen":"0.0.0.0:7448",`, "loopback-only"},
		"remote agentd": {`"agentdUrl":"https://agentd.example.test",`, "origin-only loopback"},
		"unknown field": {`"unknown":true,`, "unknown field"},
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			content := `{
				` + test.replacement + `
				"secretFile":"hook.secret",
				"dataDir":"data",
				"rules":[{"id":"rule","event":"pull_request","action":"closed","repository":"dkta-labs/agentd","jobId":"job-one"}]
			}`
			path := filepath.Join(directory, "hook.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("error = %v, expected %q", err, test.expected)
			}
		})
	}
}

func TestLoadSecretRequiresPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.secret")
	if err := os.WriteFile(path, []byte(strings.Repeat("s", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := LoadSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(secret) != strings.Repeat("s", 32) {
		t.Fatalf("secret length = %d", len(secret))
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecret(path); err == nil {
		t.Fatal("expected public secret file rejection")
	}
}
