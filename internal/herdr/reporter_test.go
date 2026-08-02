package herdr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/supervisor"
)

func TestReporterMapsAgentdVisibilityToWorkspaceMetadata(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "args")
	binary := filepath.Join(dir, "herdr")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n", output)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	visibility := supervisor.Visibility{
		Status: "working", JobID: "job-one", JobName: "Build one thing",
		RunID: "run-one", Result: "running", Evidence: "/tmp/evidence/run-one",
	}
	if err := (Reporter{Binary: binary}).Report(context.Background(), config.Workspace{ID: "herdr:default:wN:hash"}, visibility); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		"workspace", "report-metadata", "wN", "--source", "agentd",
		"--token", "agentd_status=working",
		"--token", "agentd_job=Build one thing",
		"--token", "agentd_result=running",
		"--token", "agentd_job_id=job-one",
		"--token", "agentd_run=run-one",
		"--token", "agentd_evidence=/tmp/evidence/run-one",
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("arguments = %#v, want %#v", args, want)
	}
}

func TestReporterClearsOptionalTokensAndIgnoresNonHerdrWorkspaces(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "args")
	binary := filepath.Join(dir, "herdr")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n", output)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	reporter := Reporter{Binary: binary}
	visibility := supervisor.Visibility{Status: "idle", JobID: "job", JobName: "Job", Result: "completed"}
	if err := reporter.Report(context.Background(), config.Workspace{ID: "herdr:default:w1:hash"}, visibility); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "--clear-token\nagentd_run\n") || !strings.Contains(text, "--clear-token\nagentd_evidence\n") {
		t.Fatalf("clear arguments missing: %q", text)
	}
	if err := (Reporter{Binary: filepath.Join(dir, "missing")}).Report(context.Background(), config.Workspace{ID: "ordinary"}, visibility); err != nil {
		t.Fatalf("non-Herdr workspace report = %v", err)
	}
}

func TestHerdrWorkspaceIDRejectsMalformedMappings(t *testing.T) {
	for _, id := range []string{"", "ordinary", "herdr:default::hash", "other:default:w1:hash"} {
		if workspace, ok := herdrWorkspaceID(id); ok {
			t.Fatalf("herdrWorkspaceID(%q) = %q, true", id, workspace)
		}
	}
}
