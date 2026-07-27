package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/store"
)

func TestBuildPairingURLKeepsCredentialInFragment(t *testing.T) {
	const token = "setup-token_secret"
	pairingURL, err := buildPairingURL("https://agent.example/", token)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(pairingURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RawQuery != "" || parsed.Path != "" {
		t.Fatalf("pairing URL leaked data outside fragment: %q", pairingURL)
	}
	fragment, err := url.ParseQuery(parsed.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	if fragment.Get("enroll") != token {
		t.Fatalf("fragment token = %q, want %q", fragment.Get("enroll"), token)
	}
	if strings.Contains(strings.Split(pairingURL, "#")[0], token) {
		t.Fatalf("pairing credential appears before URL fragment: %q", pairingURL)
	}
}

func TestBuildPairingURLRequiresOriginAndToken(t *testing.T) {
	for name, inputs := range map[string][2]string{
		"missing origin": {"", "token"},
		"missing token":  {"https://agent.example", ""},
		"query":          {"https://agent.example?secret=value", "token"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildPairingURL(inputs[0], inputs[1]); err == nil {
				t.Fatal("buildPairingURL() accepted unsafe input")
			}
		})
	}
}

func TestResolvePairingConfigPathUsesAgentdConfigHome(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(configHome, "agentd", "agentd.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"publicUrl":"https://agent.example"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolvePairingConfigPath("")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != path {
		t.Fatalf("resolved config = %q, want %q", resolved, path)
	}
}

func TestEnrollmentTokenForPairingReadsPrivateAdjacentEnv(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agentd.json")
	envPath := filepath.Join(dir, "agentd.env")
	if err := os.WriteFile(envPath, []byte("export OTHER=value\nAGENTD_ENROLLMENT_TOKEN='file-token'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := enrollmentTokenForPairing(configPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if token != "file-token" {
		t.Fatalf("token = %q, want file-token", token)
	}
	if token, err = enrollmentTokenForPairing(configPath, "environment-token"); err != nil || token != "environment-token" {
		t.Fatalf("environment override = %q, %v", token, err)
	}
}

func TestEnrollmentTokenForPairingRejectsExposedEnvFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions are unavailable")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agentd.json")
	if err := os.WriteFile(filepath.Join(dir, "agentd.env"), []byte("AGENTD_ENROLLMENT_TOKEN=secret\\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := enrollmentTokenForPairing(configPath, ""); err == nil {
		t.Fatal("enrollmentTokenForPairing() accepted a group/world-readable secret file")
	}
}

func TestPrintPairingQRDoesNotWritePlaintextCredential(t *testing.T) {
	var output bytes.Buffer
	const token = "plaintext-token-must-not-appear"
	if err := printPairingQR(&output, "https://agent.example", token); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), token) {
		t.Fatal("terminal output contained the plaintext enrollment token")
	}
	if output.Len() == 0 {
		t.Fatal("terminal QR output was empty")
	}
}

func TestEnsurePairingTokenUnusedRejectsConsumedCredential(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	const token = "consumed-enrollment-token"
	digest := sha256.Sum256([]byte(token))
	database, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.EnrollDevice(ctx, hex.EncodeToString(digest[:]), store.DeviceRecord{
		ID:                  "dev_test",
		Name:                "Test device",
		TokenHash:           "device-token-hash",
		NotifyReady:         true,
		NotifyInputRequired: true,
		NotifyFailed:        true,
		CreatedAt:           now,
		LastSeenAt:          now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	err = ensurePairingTokenUnused(ctx, dataDir, token)
	if err == nil || !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("ensurePairingTokenUnused() error = %v, want consumed-token error", err)
	}
}
