package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/httpapi"
	"github.com/dkta-labs/agentd/internal/store"
	"github.com/dkta-labs/agentd/internal/supervisor"
)

type adminService struct {
	stopped    chan string
	registered chan config.Workspace
}

func (s *adminService) Create(context.Context, supervisor.CreateRequest) (store.Job, error) {
	return store.Job{}, errors.New("not implemented")
}
func (s *adminService) Update(context.Context, string, supervisor.CreateRequest) (store.Job, error) {
	return store.Job{}, errors.New("not implemented")
}
func (s *adminService) List(context.Context) ([]store.Job, error) {
	return nil, errors.New("not implemented")
}
func (s *adminService) Get(context.Context, string) (store.Job, error) {
	return store.Job{}, errors.New("not implemented")
}
func (s *adminService) Runs(context.Context, string, int) ([]store.Run, error) {
	return nil, errors.New("not implemented")
}
func (s *adminService) StartJob(context.Context, string) (store.Job, error) {
	return store.Job{}, errors.New("not implemented")
}
func (s *adminService) PauseJob(context.Context, string) (store.Job, error) {
	return store.Job{}, errors.New("not implemented")
}
func (s *adminService) RunNow(context.Context, string) (store.Job, error) {
	return store.Job{}, errors.New("not implemented")
}
func (s *adminService) StopJob(_ context.Context, id string) (store.Job, error) {
	s.stopped <- id
	return store.Job{ID: id, State: "stopping", ActiveRunID: "run-active"}, nil
}
func (s *adminService) RegisterWorkspace(_ context.Context, workspace config.Workspace) (config.Workspace, error) {
	if s.registered == nil {
		return config.Workspace{}, errors.New("not implemented")
	}
	s.registered <- workspace
	return workspace, nil
}
func (s *adminService) ListWorkspaces(context.Context) ([]config.Workspace, error) {
	return nil, errors.New("not implemented")
}

func TestAdminStopRoutesThroughOwningDaemon(t *testing.T) {
	service := &adminService{stopped: make(chan string, 1)}
	server := httptest.NewServer(httpapi.New(service).Handler())
	defer server.Close()
	cfg := config.Config{Listen: strings.TrimPrefix(server.URL, "http://"), DataDir: "/path/that/must/not/be/opened"}
	var output bytes.Buffer
	err := admin(cfg, []string{"jobs", "stop", "job-active"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-service.stopped:
		if id != "job-active" {
			t.Fatalf("stopped id = %q", id)
		}
	default:
		t.Fatal("daemon stop endpoint was not called")
	}
	if !strings.Contains(output.String(), `"activeRunId":"run-active"`) {
		t.Fatalf("CLI output = %q", output.String())
	}
}

func TestAdminWaitActiveToTerminalIncludesLatestRun(t *testing.T) {
	var jobGets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jobs/job-active":
			job := store.Job{ID: "job-active", State: "paused"}
			if jobGets.Add(1) == 1 {
				job.State = "running"
				job.ActiveRunID = "run-current"
			}
			_ = json.NewEncoder(w).Encode(job)
		case "/jobs/job-active/runs":
			if r.URL.Query().Get("limit") != "1" {
				t.Errorf("runs limit = %q", r.URL.Query().Get("limit"))
			}
			_ = json.NewEncoder(w).Encode([]store.Run{{
				ID:       "run-current",
				JobID:    "job-active",
				State:    "completed",
				ExitCode: new(0),
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Config{Listen: strings.TrimPrefix(server.URL, "http://")}
	var output bytes.Buffer
	if err := waitForJob(context.Background(), cfg, "job-active", time.Second, time.Millisecond, &output); err != nil {
		t.Fatal(err)
	}
	var result jobWaitOutput
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Job.ID != "job-active" || result.Job.State != "paused" {
		t.Fatalf("final job = %#v", result.Job)
	}
	if result.LatestRun == nil || result.LatestRun.ID != "run-current" || result.LatestRun.State != "completed" {
		t.Fatalf("latest run = %#v", result.LatestRun)
	}
	if jobGets.Load() < 2 {
		t.Fatalf("job GET count = %d; want at least 2", jobGets.Load())
	}
}
func TestAdminWaitDueScheduledBeforeClaim(t *testing.T) {
	var jobGets, runGets atomic.Int32
	due := time.Now().UTC().Add(-time.Second)
	future := time.Now().UTC().Add(time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jobs/job-due":
			switch jobGets.Add(1) {
			case 1, 2:
				_ = json.NewEncoder(w).Encode(store.Job{
					ID: "job-due", DesiredState: "running", State: "scheduled", CadenceSeconds: 60, NextRunAt: &due,
				})
			case 3:
				_ = json.NewEncoder(w).Encode(store.Job{
					ID: "job-due", DesiredState: "running", State: "running", ActiveRunID: "run-due",
				})
			default:
				_ = json.NewEncoder(w).Encode(store.Job{
					ID: "job-due", DesiredState: "running", State: "scheduled", CadenceSeconds: 60, NextRunAt: &future,
				})
			}
		case "/jobs/job-due/runs":
			runGets.Add(1)
			if r.URL.Query().Get("limit") != "1" {
				t.Errorf("runs limit = %q", r.URL.Query().Get("limit"))
			}
			_ = json.NewEncoder(w).Encode([]store.Run{{
				ID: "run-due", JobID: "job-due", State: "completed", ExitCode: new(0),
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Config{Listen: strings.TrimPrefix(server.URL, "http://")}
	var output bytes.Buffer
	if err := waitForJob(context.Background(), cfg, "job-due", time.Second, time.Millisecond, &output); err != nil {
		t.Fatal(err)
	}
	var result jobWaitOutput
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Job.State != "scheduled" || result.Job.DesiredState != "running" || result.LatestRun == nil || result.LatestRun.ID != "run-due" {
		t.Fatalf("wait result = %#v", result)
	}
	if jobGets.Load() < 4 {
		t.Fatalf("job GET count = %d; want due observation, claim, and settlement", jobGets.Load())
	}
	if runGets.Load() != 1 {
		t.Fatalf("run GET count = %d; want 1", runGets.Load())
	}
}
func TestAdminWaitDueScheduledRespectsCollisionBoundary(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	workspacePath := t.TempDir()
	if err := db.SeedWorkspace(ctx, config.Workspace{ID: "shared", Name: "shared", Path: workspacePath}); err != nil {
		t.Fatal(err)
	}
	active, err := db.CreateJob(ctx, store.Job{ID: "active", Name: "active", WorkspaceID: "shared", InvocationRequest: "active"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.RequestRun(ctx, active.ID, now); err != nil {
		t.Fatal(err)
	}
	active, activeRun, claimed, err := db.ClaimNext(ctx, now, "run-active")
	if err != nil || !claimed {
		t.Fatalf("active claim = %#v, %v, %v", active, err, claimed)
	}
	due, err := db.CreateJob(ctx, store.Job{
		ID: "due", Name: "due", WorkspaceID: "shared", InvocationRequest: "due", CadenceSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartJob(ctx, due.ID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, claimed, err := db.ClaimNext(ctx, now, "run-blocked"); err != nil || claimed {
		t.Fatalf("collision claim = claimed %v, err %v; want blocked", claimed, err)
	}

	var jobGets, runGets, mutating atomic.Int32
	firstGet := make(chan struct{})
	var firstGetOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutating.Add(1)
		}
		switch r.URL.Path {
		case "/jobs/due":
			job, err := db.Job(ctx, due.ID)
			if err != nil {
				t.Errorf("read due job: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(job)
			jobGets.Add(1)
			firstGetOnce.Do(func() { close(firstGet) })
		case "/jobs/due/runs":
			runGets.Add(1)
			runs, err := db.Runs(ctx, due.ID, 1)
			if err != nil {
				t.Errorf("read due runs: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(runs)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Config{Listen: strings.TrimPrefix(server.URL, "http://")}
	var output bytes.Buffer
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- waitForJob(ctx, cfg, due.ID, time.Second, time.Millisecond, &output)
	}()
	select {
	case <-firstGet:
	case <-time.After(time.Second):
		t.Fatal("wait did not observe due scheduled job")
	}
	select {
	case err := <-waitResult:
		t.Fatalf("wait settled while collision was held: %v", err)
	default:
	}
	if runGets.Load() != 0 {
		t.Fatalf("run GET count before claim = %d; want 0", runGets.Load())
	}

	exitCode := 0
	if _, err := db.FinishRun(ctx, active.ID, activeRun.ID, "completed", &exitCode, "", "", "", "", now, true); err != nil {
		t.Fatal(err)
	}
	_, dueRun, claimed, err := db.ClaimNext(ctx, now, "run-due")
	if err != nil || !claimed {
		t.Fatalf("due claim after release = claimed %v, err %v", claimed, err)
	}
	if _, err := db.FinishRun(ctx, due.ID, dueRun.ID, "completed", &exitCode, "", "", "", "", now.Add(time.Second), true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not settle after collision release")
	}
	var result jobWaitOutput
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.LatestRun == nil || result.LatestRun.ID != dueRun.ID || result.Job.State != "scheduled" {
		t.Fatalf("wait result = %#v", result)
	}
	if jobGets.Load() < 2 || runGets.Load() != 1 || mutating.Load() != 0 {
		t.Fatalf("request counts = jobs %d, runs %d, mutating %d", jobGets.Load(), runGets.Load(), mutating.Load())
	}
}

func TestAdminWaitFutureScheduledReturnsImmediately(t *testing.T) {
	var jobGets, runGets atomic.Int32
	future := time.Now().UTC().Add(time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jobs/job-future":
			jobGets.Add(1)
			_ = json.NewEncoder(w).Encode(store.Job{
				ID: "job-future", DesiredState: "running", State: "scheduled", NextRunAt: &future,
			})
		case "/jobs/job-future/runs":
			runGets.Add(1)
			_ = json.NewEncoder(w).Encode([]store.Run(nil))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Config{Listen: strings.TrimPrefix(server.URL, "http://")}
	var output bytes.Buffer
	if err := waitForJob(context.Background(), cfg, "job-future", time.Second, time.Hour, &output); err != nil {
		t.Fatal(err)
	}
	if jobGets.Load() != 1 || runGets.Load() != 1 {
		t.Fatalf("GET counts = job %d, runs %d; want 1 each", jobGets.Load(), runGets.Load())
	}
	var result jobWaitOutput
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Job.State != "scheduled" || result.LatestRun != nil {
		t.Fatalf("wait result = %#v", result)
	}
}

func TestAdminWaitReturnsImmediatelyForTerminalJob(t *testing.T) {
	for _, state := range []string{"paused", "failed", "interrupted", "stopped"} {
		t.Run(state, func(t *testing.T) {
			var jobGets, runGets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/jobs/job-terminal":
					jobGets.Add(1)
					_ = json.NewEncoder(w).Encode(store.Job{ID: "job-terminal", State: state})
				case "/jobs/job-terminal/runs":
					runGets.Add(1)
					_ = json.NewEncoder(w).Encode([]store.Run(nil))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			cfg := config.Config{Listen: strings.TrimPrefix(server.URL, "http://")}
			var output bytes.Buffer
			if err := waitForJob(context.Background(), cfg, "job-terminal", time.Second, time.Hour, &output); err != nil {
				t.Fatal(err)
			}
			if jobGets.Load() != 1 || runGets.Load() != 1 {
				t.Fatalf("GET counts = job %d, runs %d; want 1 each", jobGets.Load(), runGets.Load())
			}
			var result jobWaitOutput
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Job.State != state || result.LatestRun != nil {
				t.Fatalf("wait result = %#v", result)
			}
		})
	}
}

func TestParseWaitArgsRejectsEmptyTimeout(t *testing.T) {
	if _, _, err := parseWaitArgs([]string{"job-active", "--timeout="}); err == nil {
		t.Fatal("empty timeout unexpectedly succeeded")
	}
}

func TestRunWaitTimeoutHasDistinctExitAndDoesNotStopRun(t *testing.T) {
	var mutatingRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutatingRequests.Add(1)
		}
		_ = json.NewEncoder(w).Encode(store.Job{
			ID:          "job-active",
			State:       "running",
			ActiveRunID: "run-current",
		})
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, strings.TrimPrefix(server.URL, "http://"))
	var stdout, stderr bytes.Buffer
	code := runContext(context.Background(), []string{
		"-config", configPath,
		"jobs", "wait", "job-active", "--timeout", "20ms",
	}, &stdout, &stderr)
	if code != 124 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `timed out waiting for job "job-active" after 20ms`) {
		t.Fatalf("timeout stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("timeout stdout = %q", stdout.String())
	}
	if mutatingRequests.Load() != 0 {
		t.Fatalf("wait sent %d mutating requests", mutatingRequests.Load())
	}
}

func TestRunWaitCancellationDoesNotStopRun(t *testing.T) {
	requestStarted := make(chan struct{})
	var mutatingRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutatingRequests.Add(1)
		}
		close(requestStarted)
		<-r.Context().Done()
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, strings.TrimPrefix(server.URL, "http://"))
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	result := make(chan int, 1)
	go func() {
		result <- runContext(ctx, []string{
			"-config", configPath,
			"jobs", "wait", "job-active",
		}, &stdout, &stderr)
	}()

	select {
	case <-requestStarted:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("wait request did not start")
	}
	select {
	case code := <-result:
		if code != 130 {
			t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not stop after cancellation")
	}
	if !strings.Contains(stderr.String(), context.Canceled.Error()) {
		t.Fatalf("cancellation stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("cancellation stdout = %q", stdout.String())
	}
	if mutatingRequests.Load() != 0 {
		t.Fatalf("wait sent %d mutating requests", mutatingRequests.Load())
	}
}

func TestAdminRegistersWorkspaceThroughRunningDaemon(t *testing.T) {
	service := &adminService{registered: make(chan config.Workspace, 1)}
	server := httptest.NewServer(httpapi.New(service).Handler())
	defer server.Close()
	cfg := config.Config{Listen: strings.TrimPrefix(server.URL, "http://")}
	path := t.TempDir()
	err := admin(cfg, []string{"workspaces", "register", "-id", "workspace-one", "-name", "Workspace One", "-path", path}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case workspace := <-service.registered:
		if workspace.ID != "workspace-one" || workspace.Name != "Workspace One" || workspace.Path != path {
			t.Fatalf("registered workspace = %#v", workspace)
		}
	default:
		t.Fatal("daemon workspace endpoint was not called")
	}
}

func TestAdminRegisterRequiresExplicitIDAndPath(t *testing.T) {
	for name, args := range map[string][]string{
		"missing id":   {"workspaces", "register", "-path", "."},
		"missing path": {"workspaces", "register", "-id", "workspace"},
	} {
		t.Run(name, func(t *testing.T) {
			err := admin(config.Config{Listen: "127.0.0.1:1"}, args, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "register -id") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRunHelpIsSuccessfulAndSelfDescribing(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missingConfig := filepath.Join(t.TempDir(), "missing.json")
	if code := run([]string{"-config", missingConfig, "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	for _, expected := range []string{"status [--json]", "jobs <command>", "workspaces <command>", "help [command]"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("help output does not contain %q:\n%s", expected, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("help stderr = %q", stderr.String())
	}
}

func TestRunCommandHelpListsJobOperations(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help", "jobs"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	for _, expected := range []string{"create", "update", "start", "pause", "run", "stop", "runs", "wait JOB_ID", "-goal KEY"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("jobs help does not contain %q:\n%s", expected, stdout.String())
		}
	}
}

func TestRunSubcommandHelpDoesNotLoadConfigOrContactDaemon(t *testing.T) {
	missingConfig := filepath.Join(t.TempDir(), "missing.json")
	for name, test := range map[string]struct {
		args     []string
		expected string
	}{
		"jobs list": {
			args:     []string{"-config", missingConfig, "jobs", "list", "--help"},
			expected: "jobs list",
		},
		"jobs nested help": {
			args:     []string{"-config", missingConfig, "jobs", "help", "list"},
			expected: "jobs list",
		},
		"jobs wait": {
			args:     []string{"-config", missingConfig, "jobs", "wait", "--help"},
			expected: "jobs wait JOB_ID [--timeout DURATION]",
		},
		"workspace register": {
			args:     []string{"-config", missingConfig, "workspaces", "register", "--help"},
			expected: "workspaces register -id ID",
		},
		"workspace nested help": {
			args:     []string{"-config", missingConfig, "workspaces", "help", "register"},
			expected: "workspaces register -id ID",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.expected) {
				t.Fatalf("help output does not contain %q:\n%s", test.expected, stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("help stderr = %q", stderr.String())
			}
		})
	}
}

func TestHelpValueIsNotMisclassifiedAsHelpFlag(t *testing.T) {
	var output bytes.Buffer
	handled, err := handleHelp([]string{"jobs", "create", "-request", "help"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatalf("request value was handled as help: %q", output.String())
	}
}
func TestParseAdminWriteIncludesGoalKey(t *testing.T) {
	request, id, err := parseAdminWrite([]string{"jobs", "update", "-name", "job", "-workspace", "workspace", "-request", "do work", "-goal", "  goal-42  ", "job-id"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "job-id" || request.GoalKey != "  goal-42  " {
		t.Fatalf("parsed request = %#v, id = %q", request, id)
	}
}

func TestRunStatusReportsTextAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	configPath := writeCLIConfig(t, strings.TrimPrefix(server.URL, "http://"))

	var textOutput, textError bytes.Buffer
	if code := run([]string{"-config", configPath, "status"}, &textOutput, &textError); code != 0 {
		t.Fatalf("text exit code = %d, stderr = %q", code, textError.String())
	}
	for _, expected := range []string{"agentd is running", "status: ok", "address: " + server.URL} {
		if !strings.Contains(textOutput.String(), expected) {
			t.Fatalf("text status does not contain %q:\n%s", expected, textOutput.String())
		}
	}

	var jsonOutput, jsonError bytes.Buffer
	if code := run([]string{"-config", configPath, "status", "--json"}, &jsonOutput, &jsonError); code != 0 {
		t.Fatalf("JSON exit code = %d, stderr = %q", code, jsonError.String())
	}
	var result statusOutput
	if err := json.Unmarshal(jsonOutput.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Running || result.Status != "ok" || result.Address != server.URL {
		t.Fatalf("JSON status = %#v", result)
	}
}

func TestRunStatusNamesUnreachableDaemon(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := writeCLIConfig(t, address)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-config", configPath, "status"}, &stdout, &stderr); code == 0 {
		t.Fatalf("status unexpectedly succeeded: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "daemon is not reachable at http://"+address) {
		t.Fatalf("status error = %q", stderr.String())
	}
}

func writeCLIConfig(t *testing.T, listen string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agentd.json")
	data, err := json.Marshal(config.Config{
		Listen:      listen,
		DataDir:     t.TempDir(),
		HerdrBinary: "herdr",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProductionHandlerExposesHealthRoute(t *testing.T) {
	server := httptest.NewServer(newHandler(service{}))
	defer server.Close()
	response, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %s", response.Status)
	}
}

func TestServeBindsBeforeMigratingDatabase(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dataDir := filepath.Join(t.TempDir(), "data")
	err = serve(config.Config{Listen: listener.Addr().String(), DataDir: dataDir})
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("serve error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "agentd.sqlite")); !os.IsNotExist(statErr) {
		t.Fatalf("database was created before bind: %v", statErr)
	}
}
