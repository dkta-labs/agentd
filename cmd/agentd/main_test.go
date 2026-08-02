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
	"testing"

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
	for _, expected := range []string{"create", "update", "start", "pause", "run", "stop", "runs"} {
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
		"workspace register": {
			args:     []string{"-config", missingConfig, "workspaces", "register", "--help"},
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
		OMPBinary:   "omp",
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
	err = serve(config.Config{Listen: listener.Addr().String(), DataDir: dataDir, OMPBinary: "omp"})
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("serve error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "agentd.sqlite")); !os.IsNotExist(statErr) {
		t.Fatalf("database was created before bind: %v", statErr)
	}
}
