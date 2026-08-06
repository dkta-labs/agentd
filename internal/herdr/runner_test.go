package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/runner"
)

type helperState struct {
	Exists        bool   `json:"exists"`
	Status        string `json:"status"`
	Sequence      uint64 `json:"sequence"`
	TabID         string `json:"tabId"`
	Session       string `json:"session"`
	StartAttempts int    `json:"startAttempts"`
	GetFailures   int    `json:"getFailures"`
	CloseFailures int    `json:"closeFailures"`
}

func TestHerdrHelperProcess(t *testing.T) {
	if os.Getenv("AGENTD_HERDR_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}
	args = args[1:]
	statePath := os.Getenv("AGENTD_HERDR_STATE")
	logPath := os.Getenv("AGENTD_HERDR_LOG")
	state := readHelperState(statePath)
	appendHelperLog(logPath, strings.Join(args, "\t"))

	switch strings.Join(args[:2], " ") {
	case "tab create":
		fmt.Print(`{"result":{"root_pane":{"pane_id":"w7:p9"},"tab":{"tab_id":"w7:t9"}}}`)
	case "agent start":
		state.StartAttempts++
		if os.Getenv("AGENTD_HERDR_BUSY_START") == "1" && state.StartAttempts < 3 {
			writeHelperState(statePath, state)
			fmt.Fprint(os.Stderr, `{"error":{"code":"agent_pane_busy"}}`)
			os.Exit(1)
		}
		state = helperState{Exists: true, Status: "idle", Sequence: 1, TabID: "w7:t9", Session: "/sessions/worker.jsonl", StartAttempts: state.StartAttempts}
		writeHelperState(statePath, state)
		fmt.Print(`{"result":{"type":"agent_started"}}`)
	case "agent get":
		if state.GetFailures > 0 {
			state.GetFailures--
			writeHelperState(statePath, state)
			fmt.Fprint(os.Stderr, `{"error":{"code":"temporary_observation"}}`)
			os.Exit(1)
		}
		if !state.Exists {
			fmt.Fprint(os.Stderr, `{"error":{"code":"agent_not_found"}}`)
			os.Exit(1)
		}
		fmt.Printf(`{"result":{"agent":{"name":%q,"agent_status":%q,"state_change_seq":%d,"tab_id":%q,"agent_session":{"value":%q}}}}`, args[2], state.Status, state.Sequence, state.TabID, state.Session)
	case "agent prompt":
		if !state.Exists {
			os.Exit(1)
		}
		if os.Getenv("AGENTD_HERDR_DELAY_PROMPT") == "1" {
			fmt.Print(`{"result":{"type":"agent_prompted"}}`)
			break
		}
		state.Status = "working"
		state.Sequence++
		writeHelperState(statePath, state)
		fmt.Print(`{"result":{"type":"agent_prompted"}}`)
	case "tab close":
		if state.CloseFailures > 0 {
			state.CloseFailures--
			writeHelperState(statePath, state)
			fmt.Fprint(os.Stderr, `{"error":{"code":"temporary_close_failure"}}`)
			os.Exit(1)
		}
		state.Status = "unknown"
		state.Sequence++
		writeHelperState(statePath, state)
		fmt.Print(`{"result":{"type":"ok"}}`)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestRunnerDispatchesInteractiveOwnerAndReturnsBeforeCompletion(t *testing.T) {
	r, statePath, logPath := testRunner(t)
	job := runner.Job{ID: "job-one", RunID: "run-one", GoalKey: "goal-nightly", WorkspaceID: "herdr:default:w7:mapping", WorkspacePath: t.TempDir(), InvocationRequest: "perform bounded work"}
	processValue, err := r.Start(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	evidence := processValue.(runner.Evidence)
	if evidence.ProcessReference() != ownerName(ownerFingerprint(job)) || evidence.ExecutionReference() != "/sessions/worker.jsonl" {
		t.Fatalf("evidence = %q %q", evidence.ProcessReference(), evidence.ExecutionReference())
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBytes)
	for _, fragment := range []string{
		"tab\tcreate",
		"--label\tgoal-nightly\t--no-focus",
		"agent\tstart",
		"agent\tprompt",
		"perform bounded work",
		"Goal key: goal-nightly",
		"Dispatch authorizes assigned-scope work end-to-end: investigation, edits, tests, commit, push, PR, independent review, merge after required CI, existing deployment, production verification, and fix-forward.",
		"Do not stop for approval",
		"never touch another goal's worktree, branch, issue, or PR",
		"Wake the coordinator exactly once only for irreducible ambiguity or a genuine external blocker",
		"coordinator",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("command log missing %q:\n%s", fragment, logText)
		}
	}

	done := make(chan error, 1)
	go func() {
		_, waitErr := processValue.Wait(context.Background())
		done <- waitErr
	}()
	select {
	case err := <-done:
		t.Fatalf("wait returned while owner working: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	state := readHelperState(statePath)
	state.Status = "idle"
	state.Sequence++
	writeHelperState(statePath, state)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("wait did not observe interactive owner settling")
	}
}

func TestOwnershipPromptWithoutCoordinatorHasNoWakeInstruction(t *testing.T) {
	job := runner.Job{ID: "job-no-coordinator", RunID: "run-no-coordinator", InvocationRequest: "work"}
	prompt := ownershipPrompt(job, "owner", "")
	if strings.Contains(prompt, "Goal key:") || strings.Contains(prompt, "herdr agent prompt") || strings.Contains(prompt, "Wake the coordinator") {
		t.Fatalf("prompt contains coordinator or empty goal wake instruction:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Do not stop for approval") || !strings.Contains(prompt, "never touch another goal's worktree, branch, issue, or PR") {
		t.Fatalf("prompt lost autonomous isolation contract:\n%s", prompt)
	}
}

func TestRunnerJobGoalKeySerialization(t *testing.T) {
	encoded, err := json.Marshal(runner.Job{GoalKey: "goal-nightly"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"goalKey":"goal-nightly"`) {
		t.Fatalf("serialized goal key missing or renamed: %s", encoded)
	}
	empty, err := json.Marshal(runner.Job{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), `"goalKey"`) {
		t.Fatalf("empty goal key should be omitted: %s", empty)
	}
}

func TestRunnerWaitsForCreatedTabShell(t *testing.T) {
	r, statePath, _ := testRunner(t)
	t.Setenv("AGENTD_HERDR_BUSY_START", "1")
	job := runner.Job{ID: "job-busy-shell", RunID: "run-busy-shell", WorkspaceID: "herdr:default:w7:mapping", WorkspacePath: t.TempDir(), InvocationRequest: "perform bounded work"}
	if _, err := r.Start(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if attempts := readHelperState(statePath).StartAttempts; attempts != 3 {
		t.Fatalf("agent start attempts = %d", attempts)
	}
}

func TestWaitDoesNotSettleBeforeReusedOwnerCompletesTransition(t *testing.T) {
	r, statePath, _ := testRunner(t)
	t.Setenv("AGENTD_HERDR_DELAY_PROMPT", "1")
	job := runner.Job{ID: "job-delayed", RunID: "run-delayed", WorkspaceID: "herdr:default:w7:mapping", WorkspacePath: t.TempDir(), InvocationRequest: "run delayed work"}
	writeHelperState(statePath, helperState{Exists: true, Status: "done", Sequence: 4, TabID: "w7:t2", Session: "/sessions/reused.jsonl"})
	processValue, err := r.Start(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, waitErr := processValue.Wait(context.Background())
		done <- waitErr
	}()
	select {
	case err := <-done:
		t.Fatalf("wait settled before work transition: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	state := readHelperState(statePath)
	state.Sequence += 2
	writeHelperState(statePath, state)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("wait did not observe completed transition")
	}
}

func TestRunnerReusesSettledOwnerAndReattachesBlockedOwner(t *testing.T) {
	r, statePath, logPath := testRunner(t)
	job := runner.Job{ID: "job-reuse", RunID: "run-two", WorkspaceID: "herdr:default:w7:mapping", WorkspacePath: t.TempDir(), InvocationRequest: "continue recurring work"}
	writeHelperState(statePath, helperState{Exists: true, Status: "done", Sequence: 4, TabID: "w7:t2", Session: "/sessions/reused.jsonl"})
	processValue, err := r.Start(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	logBytes, _ := os.ReadFile(logPath)
	if strings.Contains(string(logBytes), "tab\tcreate") || !strings.Contains(string(logBytes), "agent\tprompt") {
		t.Fatalf("settled owner was not reused:\n%s", logBytes)
	}

	state := readHelperState(statePath)
	state.Status = "blocked"
	state.Sequence++
	writeHelperState(statePath, state)
	attached, err := r.Attach(context.Background(), job, processValue.(runner.Evidence).ProcessReference())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, waitErr := attached.Wait(context.Background())
		done <- waitErr
	}()
	select {
	case <-done:
		t.Fatal("blocked owner was treated as settled")
	case <-time.After(30 * time.Millisecond):
	}
	state.Status = "idle"

	state.Sequence++
	writeHelperState(statePath, state)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("reattached owner did not settle")
	}
	if err := attached.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readHelperState(statePath).Status; got != "unknown" {
		t.Fatalf("stop status = %q", got)
	}
}
func TestPrepareDoesNotPromptUntilDispatch(t *testing.T) {
	r, _, logPath := testRunner(t)
	job := runner.Job{ID: "prepare-only", RunID: "run-prepare-only", WorkspaceID: "herdr:default:w7:mapping", WorkspacePath: t.TempDir(), InvocationRequest: "prepare work"}
	prepared, err := r.Prepare(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logBytes), "agent\tprompt") {
		t.Fatalf("prepare dispatched prompt:\n%s", logBytes)
	}
	if err := prepared.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	logBytes, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "agent\tprompt") {
		t.Fatalf("dispatch did not prompt:\n%s", logText)
	}
	for _, flag := range []string{"--wait", "--until\tworking", "--until\tblocked", "--until\tidle", "--until\tdone", "--until\tunknown", "--timeout\t6000"} {
		if !strings.Contains(logText, flag) {
			t.Fatalf("dispatch missing %q:\n%s", flag, logText)
		}
	}
}
func TestPreparedDispatchUsesSequenceAsIdempotencyEvidence(t *testing.T) {
	r, statePath, logPath := testRunner(t)
	job := runner.Job{ID: "prepared-idempotent", RunID: "run-prepared-idempotent", WorkspaceID: "herdr:default:w7:mapping", WorkspacePath: t.TempDir(), InvocationRequest: "prepare work"}
	writeHelperState(statePath, helperState{Exists: true, Status: "idle", Sequence: 8, TabID: "w7:t9", Session: "/sessions/worker.jsonl"})
	prepared, err := r.AttachPrepared(context.Background(), job, ownerName(ownerFingerprint(job)), 8)
	if err != nil {
		t.Fatal(err)
	}
	evidence, ok := prepared.Process().(runner.PreparedEvidence)
	if !ok || evidence.PreparedSequence() != 8 {
		t.Fatalf("prepared evidence = %#v", evidence)
	}
	if err := prepared.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstLog, _ := os.ReadFile(logPath)
	firstPrompts := strings.Count(string(firstLog), "agent\tprompt")
	if firstPrompts != 1 {
		t.Fatalf("initial prompt count = %d", firstPrompts)
	}
	if err := prepared.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondLog, _ := os.ReadFile(logPath)
	if got := strings.Count(string(secondLog), "agent\tprompt"); got != firstPrompts {
		t.Fatalf("idempotent prompt count = %d", got)
	}
}
func TestPreparedRecoveryWaitsForAcceptedPromptTransition(t *testing.T) {
	r, statePath, logPath := testRunner(t)
	r.PollInterval = time.Millisecond
	r.RecoveryGrace = 40 * time.Millisecond
	job := runner.Job{ID: "prepared-recovery-delay", RunID: "run-prepared-recovery-delay", WorkspaceID: "herdr:default:w7:mapping", WorkspacePath: t.TempDir(), InvocationRequest: "prepare work"}
	owner := ownerName(ownerFingerprint(job))
	writeHelperState(statePath, helperState{Exists: true, Status: "idle", Sequence: 8, TabID: "w7:t9", Session: "/sessions/worker.jsonl"})
	go func() {
		time.Sleep(8 * time.Millisecond)
		writeHelperState(statePath, helperState{Exists: true, Status: "working", Sequence: 9, TabID: "w7:t9", Session: "/sessions/worker.jsonl"})
	}()
	prepared, err := r.AttachPrepared(context.Background(), job, owner, 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	logBytes, _ := os.ReadFile(logPath)
	if strings.Contains(string(logBytes), "agent\tprompt") {
		t.Fatalf("recovery re-prompted accepted delayed prompt:\n%s", logBytes)
	}
}

func TestOwnerFingerprintSeparatesWorkspaceIdentityAndPath(t *testing.T) {
	job := runner.Job{ID: "same-job", WorkspaceID: "herdr:default:w7:mapping", WorkspacePath: "/tmp/work/../work"}
	samePath := job
	samePath.WorkspacePath = "/tmp/work"
	changedPath := job
	changedPath.WorkspacePath = "/tmp/other"
	if ownerName(ownerFingerprint(job)) != ownerName(ownerFingerprint(samePath)) {
		t.Fatal("clean equivalent workspace paths should reuse owner")
	}
	if ownerName(ownerFingerprint(job)) == ownerName(ownerFingerprint(changedPath)) {
		t.Fatal("changed workspace path reused owner")
	}
	changedID := job
	changedID.WorkspaceID = "herdr:default:w8:mapping"
	if ownerName(ownerFingerprint(job)) == ownerName(ownerFingerprint(changedID)) {
		t.Fatal("changed workspace id reused owner")
	}
}

func TestTabLabelUsesHumanGoalAndWorkspaceMetadata(t *testing.T) {
	tests := []struct {
		name string
		job  runner.Job
		want string
	}{
		{
			name: "repository role",
			job:  runner.Job{GoalKey: "agent-tools/repository-scout"},
			want: "agent-tools · scout",
		},
		{
			name: "single segment goal",
			job:  runner.Job{GoalKey: "goal-nightly"},
			want: "goal-nightly",
		},
		{
			name: "control characters",
			job:  runner.Job{GoalKey: "\x1bDKT-75/portfolio-triage\x7f"},
			want: "DKT-75 · portfolio-triage",
		},
		{
			name: "workspace fallback",
			job:  runner.Job{WorkspacePath: "/tmp/tavernbench-server"},
			want: "Agentd · tavernbench-server",
		},
		{
			name: "empty fallback",
			job:  runner.Job{},
			want: "Agentd worker",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := tabLabel(test.job); got != test.want {
				t.Fatalf("tabLabel() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTabLabelIsBounded(t *testing.T) {
	got := tabLabel(runner.Job{GoalKey: "project/" + strings.Repeat("x", 100)})
	if len([]rune(got)) != 80 || !strings.HasSuffix(got, "…") {
		t.Fatalf("tabLabel() = %q (%d runes), want 80 runes ending in ellipsis", got, len([]rune(got)))
	}
}

func TestWaitRetriesTransientObservationErrors(t *testing.T) {
	r, statePath, _ := testRunner(t)
	writeHelperState(statePath, helperState{Exists: true, Status: "idle", Sequence: 3, TabID: "w7:t9", Session: "/sessions/worker.jsonl", GetFailures: 2})
	process := &process{runner: &r, owner: "owner", startSeq: 1, pollInterval: 5 * time.Millisecond}
	if exit, err := process.Wait(context.Background()); err != nil || !exit.Successful() {
		t.Fatalf("wait after transient observations = %#v, %v", exit, err)
	}
}

func TestWaitCancellationReturnsContextCancellation(t *testing.T) {
	r, statePath, _ := testRunner(t)
	writeHelperState(statePath, helperState{Exists: true, Status: "working", Sequence: 2, TabID: "w7:t9", Session: "/sessions/worker.jsonl"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	process := &process{runner: &r, owner: "owner", startSeq: 1, pollInterval: 5 * time.Millisecond}
	if _, err := process.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait cancellation error = %v", err)
	}
}

func TestStopRetriesAfterTransientCloseFailure(t *testing.T) {
	r, statePath, logPath := testRunner(t)
	writeHelperState(statePath, helperState{Exists: true, Status: "done", Sequence: 3, TabID: "w7:t9", Session: "/sessions/worker.jsonl", CloseFailures: 1})
	process := &process{runner: &r, owner: "owner", pollInterval: 5 * time.Millisecond}
	if err := process.Stop(context.Background()); err == nil {
		t.Fatal("first stop succeeded")
	}
	if err := process.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := process.Stop(context.Background()); err != nil {
		t.Fatal("completed stop was not idempotent: " + err.Error())
	}
	if got := readHelperState(statePath).Status; got != "unknown" {
		t.Fatalf("stop status = %q", got)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(logBytes), "tab\tclose") != 2 {
		t.Fatalf("tab close attempts = %d:\n%s", strings.Count(string(logBytes), "tab\tclose"), logBytes)
	}
}

func TestOwnershipPromptQuotesConfiguredWakeBinary(t *testing.T) {
	job := runner.Job{ID: "job-quote", RunID: "run-quote", InvocationRequest: "work"}
	prompt := ownershipPrompt(job, "owner", "coord'inate", "/opt/herdr path/herdr'bin")
	want := `'/opt/herdr path/herdr'"'"'bin' agent prompt 'coord'"'"'inate'`
	if !strings.Contains(prompt, want) {
		t.Fatalf("wake command = %q, want fragment %q", prompt, want)
	}
}

func TestOutputOmitsTerminalControlCharacters(t *testing.T) {
	process := &process{owner: "owner", coordinator: "coord\x1b[31m\x7f"}
	stdout, stderr := process.Output()
	if strings.ContainsAny(stdout+stderr, "\x00\x1b\x7f") {
		t.Fatalf("terminal output contains controls: %q %q", stdout, stderr)
	}
}

func testRunner(t *testing.T) (Runner, string, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	logPath := filepath.Join(dir, "commands.log")
	binary := filepath.Join(dir, "herdr")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestHerdrHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTD_HERDR_HELPER", "1")
	t.Setenv("AGENTD_HERDR_STATE", statePath)
	t.Setenv("AGENTD_HERDR_LOG", logPath)
	return Runner{Binary: binary, AgentArgs: []string{"--config=/worker.yml"}, AgentEnv: map[string]string{"LINEAR_API_KEY_FILE": "/secrets/linear"}, CoordinatorTarget: "coordinator", PollInterval: 5 * time.Millisecond}, statePath, logPath
}

func readHelperState(path string) helperState {
	var state helperState
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &state)
	}
	return state
}

func writeHelperState(path string, state helperState) {
	data, _ := json.Marshal(state)
	tmp := path + ".tmp"
	_ = os.WriteFile(tmp, data, 0o600)
	_ = os.Rename(tmp, path)
}

func appendHelperLog(path, line string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = fmt.Fprintln(file, line)
		_ = file.Close()
	}
}
