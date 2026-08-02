package omp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/runner"
)

func executable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunnerLaunchesPrintModeWithIsolatedSessionAndBoundedEvidence(t *testing.T) {
	workspace := t.TempDir()
	sessionRoot := t.TempDir()
	binary := executable(t, `printf '%s\n' "$@" > "$PWD/args"
printf 'abcdefgh'
printf 'errors!!' >&2
exit 7
`)
	process, err := (Runner{Binary: binary, Args: []string{"--max-time=30m"}, SessionRoot: sessionRoot, MaxOutput: 4}).Start(context.Background(), runner.Job{
		ID: "job", RunID: "run-one", WorkspacePath: workspace, InvocationRequest: "do one thing",
	})
	if err != nil {
		t.Fatal(err)
	}
	exit, waitErr := process.Wait(context.Background())
	if waitErr != nil {
		t.Fatal(waitErr)
	}
	if exit.Code != 7 || exit.Signal != "" || exit.Err != nil {
		t.Fatalf("exit = %#v", exit)
	}
	evidence, ok := process.(runner.Evidence)
	if !ok {
		t.Fatal("process does not expose evidence")
	}
	expectedSession := filepath.Join(sessionRoot, "run-one")
	if evidence.ExecutionReference() != expectedSession || !strings.Contains(evidence.ProcessReference(), "pid=") || !strings.Contains(evidence.ProcessReference(), "pgid=") {
		t.Fatalf("references = %q %q", evidence.ExecutionReference(), evidence.ProcessReference())
	}
	stdout, stderr := evidence.Output()
	if stdout != "abcd" || stderr != "erro" {
		t.Fatalf("bounded output = %q %q", stdout, stderr)
	}
	args, err := os.ReadFile(filepath.Join(workspace, "args"))
	if err != nil {
		t.Fatal(err)
	}
	want := "--max-time=30m\n-p\n--session-dir\n" + expectedSession + "\n--\ndo one thing\n"
	if string(args) != want {
		t.Fatalf("arguments = %q, want %q", args, want)
	}
}

func TestLoadEnvFilesOverridesInheritedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("new-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment, err := loadEnvFiles([]string{"TOKEN=old", "OTHER=kept"}, map[string]string{"TOKEN": path})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(environment, "\n") != "TOKEN=new-value\nOTHER=kept" {
		t.Fatalf("environment = %#v", environment)
	}
}

func TestLoadEnvFilesRejectsUnsafeValues(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":     nil,
		"nul":       []byte("secret\x00value"),
		"oversized": make([]byte, MaxEnvValueBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "value")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadEnvFiles(nil, map[string]string{"TOKEN": path}); err == nil {
				t.Fatal("expected environment file error")
			}
		})
	}
}

func TestStopTerminatesProcessGroup(t *testing.T) {
	workspace := t.TempDir()
	binary := executable(t, `sleep 30 &
child=$!
printf '%s' "$child" > "$PWD/child.pid"
wait "$child"
`)
	process, err := (Runner{Binary: binary, SessionRoot: t.TempDir()}).Start(context.Background(), runner.Job{
		ID: "job", RunID: "run-stop", WorkspacePath: workspace, InvocationRequest: "wait",
	})
	if err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(workspace, "child.pid")
	deadline := time.Now().Add(2 * time.Second)
	var childPID int
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(childPath)
		if readErr == nil {
			childPID, err = strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child process did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := process.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	exit, err := process.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if exit.Signal == "" {
		t.Fatalf("stopped exit = %#v", exit)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err = syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d still exists: %v", childPID, err)
}
