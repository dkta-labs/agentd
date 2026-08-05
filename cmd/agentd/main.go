package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/herdr"
	"github.com/dkta-labs/agentd/internal/httpapi"
	"github.com/dkta-labs/agentd/internal/mcp"
	"github.com/dkta-labs/agentd/internal/runner"
	"github.com/dkta-labs/agentd/internal/store"
	"github.com/dkta-labs/agentd/internal/supervisor"
	"github.com/dkta-labs/agentd/internal/top"
)

type workspaces struct{ store *store.DB }

func (w workspaces) Workspace(ctx context.Context, id string) (config.Workspace, error) {
	return w.store.Workspace(ctx, id)
}

type service struct {
	supervisor *supervisor.Supervisor
	store      *store.DB
}

func (s service) Create(c context.Context, r supervisor.CreateRequest) (store.Job, error) {
	return s.supervisor.Create(c, r)
}
func (s service) Update(c context.Context, id string, r supervisor.CreateRequest) (store.Job, error) {
	return s.supervisor.Update(c, id, r)
}
func (s service) List(c context.Context) ([]store.Job, error)         { return s.supervisor.List(c) }
func (s service) Get(c context.Context, id string) (store.Job, error) { return s.supervisor.Get(c, id) }
func (s service) Runs(c context.Context, id string, l int) ([]store.Run, error) {
	return s.supervisor.Runs(c, id, l)
}
func (s service) StartJob(c context.Context, id string) (store.Job, error) {
	return s.supervisor.StartJob(c, id)
}
func (s service) PauseJob(c context.Context, id string) (store.Job, error) {
	return s.supervisor.PauseJob(c, id)
}
func (s service) RunNow(c context.Context, id string) (store.Job, error) {
	return s.supervisor.RunNow(c, id)
}
func (s service) StopJob(c context.Context, id string) (store.Job, error) {
	return s.supervisor.StopJob(c, id)
}
func (s service) RegisterWorkspace(ctx context.Context, workspace config.Workspace) (config.Workspace, error) {
	workspace.ID = strings.TrimSpace(workspace.ID)
	workspace.Name = strings.TrimSpace(workspace.Name)
	workspace.Path = strings.TrimSpace(workspace.Path)
	if workspace.ID == "" || len(workspace.ID) > 200 {
		return config.Workspace{}, errors.New("workspace id must contain between 1 and 200 bytes")
	}
	if workspace.Name == "" {
		workspace.Name = workspace.ID
	}
	if len(workspace.Name) > 120 {
		return config.Workspace{}, errors.New("workspace name must not exceed 120 bytes")
	}
	if !filepath.IsAbs(workspace.Path) {
		return config.Workspace{}, errors.New("workspace path must be absolute")
	}
	info, err := os.Stat(workspace.Path)
	if err != nil {
		return config.Workspace{}, fmt.Errorf("inspect workspace path: %w", err)
	}
	if !info.IsDir() {
		return config.Workspace{}, errors.New("workspace path must be a directory")
	}
	workspace.Path = filepath.Clean(workspace.Path)
	return s.store.RegisterWorkspace(ctx, workspace)
}
func (s service) ListWorkspaces(ctx context.Context) ([]config.Workspace, error) {
	return s.store.Workspaces(ctx)
}

const waitPollInterval = 250 * time.Millisecond

var errWaitTimeout = errors.New("timed out waiting for job")

type jobWaitOutput struct {
	Job       store.Job  `json:"job"`
	LatestRun *store.Run `json:"latestRun"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("agentd", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { printRootUsage(stdout) }
	configPath := global.String("config", "", "configuration JSON path")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	args = global.Args()
	if handled, err := handleHelp(args, stdout); handled {
		if err != nil {
			fmt.Fprintln(stderr, "agentd:", err)
			return 2
		}
		return 0
	}
	command := "daemon"
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "daemon":
		if len(args) > 1 {
			fmt.Fprintln(stderr, "agentd: usage: agentd [global options] daemon")
			return 2
		}
	case "status", "top", "jobs", "workspaces":
	default:
		fmt.Fprintf(stderr, "agentd: unknown command %q\n\n", command)
		printRootUsage(stderr)
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "agentd:", err)
		return 1
	}
	switch command {
	case "daemon":
		err = serve(cfg)
	case "status":
		err = adminStatus(cfg, args[1:], stdout, stderr)
	case "top":
		err = runTop(cfg, args[1:], stdout)
	default:
		if command == "jobs" && len(args) > 1 && args[1] == "wait" {
			waitCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			err = adminContext(waitCtx, cfg, args, stdout)
			stop()
		} else {
			err = adminContext(ctx, cfg, args, stdout)
		}
	}

	if err != nil {
		fmt.Fprintln(stderr, "agentd:", err)
		switch {
		case errors.Is(err, errWaitTimeout):
			return 124
		case errors.Is(err, context.Canceled):
			return 130
		default:
			return 1
		}
	}
	return 0
}

func handleHelp(args []string, output io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	var path []string
	switch {
	case args[0] == "help":
		path = args[1:]
	case len(args) >= 2 && args[1] == "help":
		path = append([]string{args[0]}, args[2:]...)
	case isHelpFlag(args[len(args)-1]):
		path = args[:len(args)-1]
	default:
		return false, nil
	}
	return true, printHelpPath(output, path)
}

func isHelpFlag(value string) bool {
	return value == "-h" || value == "--help"
}

func printHelpPath(output io.Writer, path []string) error {
	if len(path) == 0 {
		printRootUsage(output)
		return nil
	}
	if len(path) == 1 {
		switch path[0] {
		case "daemon":
			fmt.Fprintln(output, "Usage: agentd [global options] daemon\n\nStart the Agentd daemon in the foreground. Running agentd without a command does the same thing.")
		case "status":
			printStatusUsage(output)
		case "top":
			fmt.Fprintln(output, "Usage: agentd [global options] top [--once] [--interval DURATION] [--no-clear] [--address URL]\n\nRefresh a read-only terminal view of job, run, and Herdr owner lifecycle state.")
		case "jobs":
			printJobsUsage(output)
		case "workspaces":
			printWorkspacesUsage(output)
		default:
			return fmt.Errorf("unknown help topic %q", path[0])
		}
		return nil
	}
	if len(path) == 2 {
		switch path[0] {
		case "jobs":
			return printJobCommandUsage(output, path[1])
		case "workspaces":
			return printWorkspaceCommandUsage(output, path[1])
		}
	}
	return fmt.Errorf("unknown help topic %q", strings.Join(path, " "))
}

func printRootUsage(output io.Writer) {
	fmt.Fprint(output, `Usage:
  agentd [global options]                         Start the daemon
  agentd [global options] daemon                  Start the daemon explicitly
  agentd [global options] status [--json]         Check daemon health
  agentd [global options] top [options]             Watch job, run, and owner lifecycle
  agentd [global options] jobs <command>           Manage scheduled jobs
  agentd [global options] workspaces <command>     Manage workspace registry
  agentd help [command]                            Show command help

Global options:
  -config PATH   Configuration JSON path
  -h, --help     Show this help

Run "agentd help <command>" for command-specific usage.
`)
}

func printStatusUsage(output io.Writer) {
	fmt.Fprint(output, `Usage: agentd [global options] status [--json]

Check the configured loopback daemon's /health endpoint.

Options:
  --json   Emit stable machine-readable output
`)
}

func runTop(cfg config.Config, args []string, output io.Writer) error {
	options, err := top.ParseOptions(args, "http://"+cfg.Listen)
	if err != nil {
		return err
	}
	interactive := false
	if file, ok := output.(*os.File); ok {
		if info, statErr := file.Stat(); statErr == nil {
			interactive = info.Mode()&os.ModeCharDevice != 0
		}
	}
	if !interactive {
		options.Once = true
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return top.Execute(ctx, options, output, interactive)
}

func printJobsUsage(output io.Writer) {
	fmt.Fprint(output, `Usage: agentd [global options] jobs <command>

Commands:
  list
  get JOB_ID
  create -name NAME -workspace ID -request TEXT [-runner ID] [-cadence SECONDS] [-goal KEY]
  update -name NAME -workspace ID -request TEXT [-runner ID] [-cadence SECONDS] [-goal KEY] JOB_ID
  start JOB_ID
  pause JOB_ID
  run JOB_ID
  stop JOB_ID
  runs JOB_ID
  wait JOB_ID [--timeout DURATION]
`)
}

func printWorkspacesUsage(output io.Writer) {
	fmt.Fprint(output, `Usage: agentd [global options] workspaces <command>

Commands:
  list
  register -id ID [-name NAME] -path PATH
`)
}

func printJobCommandUsage(output io.Writer, command string) error {
	switch command {
	case "list":
		fmt.Fprintln(output, "Usage: agentd [global options] jobs list")
	case "get", "start", "pause", "run", "stop", "runs":
		fmt.Fprintf(output, "Usage: agentd [global options] jobs %s JOB_ID\n", command)
	case "wait":
		fmt.Fprintln(output, "Usage: agentd [global options] jobs wait JOB_ID [--timeout DURATION]")
	case "create":
		fmt.Fprintln(output, "Usage: agentd [global options] jobs create -name NAME -workspace ID -request TEXT [-runner ID] [-cadence SECONDS] [-goal KEY]")
	case "update":
		fmt.Fprintln(output, "Usage: agentd [global options] jobs update -name NAME -workspace ID -request TEXT [-runner ID] [-cadence SECONDS] [-goal KEY] JOB_ID")
	default:
		return fmt.Errorf("unknown jobs command %q", command)
	}
	return nil
}

func printWorkspaceCommandUsage(output io.Writer, command string) error {
	switch command {
	case "list":
		fmt.Fprintln(output, "Usage: agentd [global options] workspaces list")
	case "register":
		fmt.Fprintln(output, "Usage: agentd [global options] workspaces register -id ID [-name NAME] -path PATH")
	default:
		return fmt.Errorf("unknown workspaces command %q", command)
	}
	return nil
}

type statusOutput struct {
	Running bool   `json:"running"`
	Status  string `json:"status"`
	Address string `json:"address"`
}

func adminStatus(cfg config.Config, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { printStatusUsage(stderr) }
	jsonOutput := flags.Bool("json", false, "emit machine-readable output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: agentd [global options] status [--json]")
	}
	result, err := fetchStatus(context.Background(), cfg)
	if err != nil {
		return err
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(result)
	}
	fmt.Fprintln(stdout, "agentd is running")
	fmt.Fprintf(stdout, "status: %s\n", result.Status)
	fmt.Fprintf(stdout, "address: %s\n", result.Address)
	return nil
}

func fetchStatus(ctx context.Context, cfg config.Config) (statusOutput, error) {
	address := "http://" + cfg.Listen
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/health", nil)
	if err != nil {
		return statusOutput{}, err
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return statusOutput{}, fmt.Errorf("daemon is not reachable at %s: %w", address, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return statusOutput{}, fmt.Errorf("read daemon health response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return statusOutput{}, fmt.Errorf("daemon at %s returned %s", address, response.Status)
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(payload, &health); err != nil {
		return statusOutput{}, fmt.Errorf("decode daemon health response: %w", err)
	}
	if health.Status != "ok" {
		return statusOutput{}, fmt.Errorf("daemon at %s reported status %q", address, health.Status)
	}
	return statusOutput{Running: true, Status: health.Status, Address: address}, nil
}
func open(cfg config.Config) (*store.DB, *supervisor.Supervisor, error) {
	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	for _, workspace := range cfg.Workspaces {
		if err := db.SeedWorkspace(ctx, workspace); err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("seed workspace %q: %w", workspace.ID, err)
		}
	}
	registry := runner.NewRegistry(map[string]runner.Runner{"omp": herdr.Runner{
		Binary: cfg.HerdrBinary, AgentArgs: cfg.AgentArgs, AgentEnv: cfg.AgentEnv,
		CoordinatorTarget: cfg.CoordinatorTarget,
	}})
	sup, err := supervisor.New(db, workspaces{store: db}, registry, slog.Default())
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return db, sup, nil
}
func serve(cfg config.Config) error {
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	defer listener.Close()
	db, sup, err := open(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := sup.Recover(context.Background()); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sup.Start(ctx)
	api := service{supervisor: sup, store: db}
	server := &http.Server{Handler: newHandler(api), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	var terminalErr error
	select {
	case serveErr := <-serveErr:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			terminalErr = serveErr
		}
	case <-ctx.Done():
	}
	stop()
	sup.BeginClose()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	closeErr := sup.Close(shutdownCtx)
	if closeErr != nil {
		slog.Error("shutdown could not detach every Agentd watcher within the deadline", "error", closeErr)
	}
	return errors.Join(terminalErr, shutdownErr, closeErr)
}
func newHandler(api service) http.Handler {
	handler := httpapi.New(api)
	mux := http.NewServeMux()
	mux.Handle("/health", handler.Handler())
	mux.Handle("/jobs", handler.Handler())
	mux.Handle("/jobs/", handler.Handler())
	mux.Handle("/workspaces", handler.Handler())
	mux.Handle("/mcp", mcp.New(api))
	return mux
}
func admin(cfg config.Config, args []string, stdout io.Writer) error {
	return adminContext(context.Background(), cfg, args, stdout)
}

func adminContext(ctx context.Context, cfg config.Config, args []string, stdout io.Writer) error {
	if len(args) < 2 {
		return errors.New("usage: agentd <jobs|workspaces> <command>")
	}
	if args[0] == "workspaces" {
		return adminWorkspaces(cfg, args[1:], stdout)
	}
	if args[0] != "jobs" {
		return errors.New("usage: agentd <jobs|workspaces> <command>")
	}
	method, path := http.MethodGet, "/jobs"
	var body []byte
	switch args[1] {
	case "list":
	case "get", "runs", "start", "pause", "run", "stop":
		if len(args) < 3 {
			return errors.New("job id is required")
		}
		path = "/jobs/" + url.PathEscape(args[2])
		if args[1] != "get" {
			path += "/" + args[1]
		}
		if args[1] == "runs" {
			path += "?limit=20"
		} else if args[1] != "get" {
			method = http.MethodPost
		}
	case "wait":
		id, timeout, err := parseWaitArgs(args[2:])
		if err != nil {
			return err
		}
		return waitForJob(ctx, cfg, id, timeout, waitPollInterval, stdout)
	case "create", "update":
		request, id, err := parseAdminWrite(args)
		if err != nil {
			return err
		}
		body, err = json.Marshal(request)
		if err != nil {
			return err
		}
		method = http.MethodPost
		if args[1] == "update" {
			method = http.MethodPut
			path = "/jobs/" + url.PathEscape(id)
		}
	default:
		return errors.New("unknown jobs command")
	}
	return callAdmin(ctx, cfg, method, path, body, stdout)
}

func parseWaitArgs(args []string) (string, time.Duration, error) {
	const usage = "usage: agentd jobs wait JOB_ID [--timeout DURATION]"
	var (
		id         string
		timeout    time.Duration
		timeoutSet bool
	)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		var timeoutValue string
		timeoutOption := false
		switch {
		case arg == "-timeout" || arg == "--timeout":
			timeoutOption = true
			if index+1 >= len(args) {
				return "", 0, errors.New(usage)
			}
			index++
			timeoutValue = args[index]
		case strings.HasPrefix(arg, "-timeout="):
			timeoutOption = true
			timeoutValue = strings.TrimPrefix(arg, "-timeout=")
		case strings.HasPrefix(arg, "--timeout="):
			timeoutOption = true
			timeoutValue = strings.TrimPrefix(arg, "--timeout=")
		case strings.HasPrefix(arg, "-"):
			return "", 0, fmt.Errorf("%s: unknown option %q", usage, arg)
		case id == "":
			id = arg
		default:
			return "", 0, errors.New(usage)
		}
		if !timeoutOption {
			continue
		}
		if timeoutSet {
			return "", 0, fmt.Errorf("%s: timeout specified more than once", usage)
		}
		parsed, err := time.ParseDuration(timeoutValue)
		if err != nil {
			return "", 0, fmt.Errorf("invalid timeout %q: %w", timeoutValue, err)
		}
		if parsed <= 0 {
			return "", 0, errors.New("timeout must be greater than zero")
		}
		timeout, timeoutSet = parsed, true
	}
	if id == "" {
		return "", 0, errors.New(usage)
	}
	return id, timeout, nil
}

func waitForJob(ctx context.Context, cfg config.Config, id string, timeout, pollInterval time.Duration, stdout io.Writer) error {
	waitCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		timeoutErr := fmt.Errorf("%w %q after %s", errWaitTimeout, id, timeout)
		waitCtx, cancel = context.WithTimeoutCause(ctx, timeout, timeoutErr)
	}
	defer cancel()

	if pollInterval <= 0 {
		pollInterval = waitPollInterval
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	client := &http.Client{Timeout: 15 * time.Second}
	jobPath := "/jobs/" + url.PathEscape(id)

	for {
		var job store.Job
		if err := getAdminJSON(waitCtx, client, cfg, jobPath, &job); err != nil {
			if cause := context.Cause(waitCtx); cause != nil {
				return cause
			}
			return err
		}
		if !jobIsActive(job) {
			var runs []store.Run
			if err := getAdminJSON(waitCtx, client, cfg, jobPath+"/runs?limit=1", &runs); err != nil {
				if cause := context.Cause(waitCtx); cause != nil {
					return cause
				}
				return err
			}
			var latest *store.Run
			if len(runs) > 0 {
				latest = &runs[0]
			}
			return json.NewEncoder(stdout).Encode(jobWaitOutput{Job: job, LatestRun: latest})
		}
		select {
		case <-waitCtx.Done():
			return context.Cause(waitCtx)
		case <-ticker.C:
		}
	}
}

func jobIsActive(job store.Job) bool {
	if job.RunRequested || job.ActiveRunID != "" {
		return true
	}
	switch job.State {
	case "starting", "running", "stopping":
		return true
	default:
		return false
	}
}

func getAdminJSON(ctx context.Context, client *http.Client, cfg config.Config, path string, target any) error {
	payload, err := requestAdmin(ctx, client, cfg, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("decode agentd daemon response: %w", err)
	}
	return nil
}
func parseAdminWrite(args []string) (supervisor.CreateRequest, string, error) {
	fs := flag.NewFlagSet(args[1], flag.ContinueOnError)
	name := fs.String("name", "", "job name")
	workspace := fs.String("workspace", "", "workspace id")
	request := fs.String("request", "", "invocation request")
	runnerName := fs.String("runner", "omp", "runner id")
	cadence := fs.Int("cadence", 0, "cadence seconds")
	goalKey := fs.String("goal", "", "optional durable goal key")
	if err := fs.Parse(args[2:]); err != nil {
		return supervisor.CreateRequest{}, "", err
	}
	id := ""
	if args[1] == "update" {
		if fs.NArg() < 1 {
			return supervisor.CreateRequest{}, "", errors.New("job id is required")
		}
		id = fs.Arg(0)
	}
	return supervisor.CreateRequest{Name: *name, WorkspaceID: *workspace, Runner: *runnerName, InvocationRequest: *request, CadenceSeconds: *cadence, GoalKey: *goalKey}, id, nil
}
func adminWorkspaces(cfg config.Config, args []string, stdout io.Writer) error {
	if len(args) < 1 {
		return errors.New("usage: agentd workspaces <list|register>")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: agentd workspaces list")
		}
		return callAdmin(context.Background(), cfg, http.MethodGet, "/workspaces", nil, stdout)
	case "register":
		fs := flag.NewFlagSet("register", flag.ContinueOnError)
		id := fs.String("id", "", "workspace id")
		name := fs.String("name", "", "workspace name")
		path := fs.String("path", "", "workspace path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if strings.TrimSpace(*id) == "" || strings.TrimSpace(*path) == "" || fs.NArg() != 0 {
			return errors.New("usage: agentd workspaces register -id ID [-name NAME] -path PATH")
		}
		absolute, err := filepath.Abs(*path)
		if err != nil {
			return fmt.Errorf("resolve workspace path: %w", err)
		}
		body, err := json.Marshal(config.Workspace{ID: *id, Name: *name, Path: absolute})
		if err != nil {
			return err
		}
		return callAdmin(context.Background(), cfg, http.MethodPost, "/workspaces", body, stdout)
	default:
		return errors.New("usage: agentd workspaces <list|register>")
	}
}

func callAdmin(ctx context.Context, cfg config.Config, method, path string, body []byte, stdout io.Writer) error {
	payload, err := requestAdmin(ctx, &http.Client{Timeout: 15 * time.Second}, cfg, method, path, body)
	if err != nil {
		return err
	}
	_, err = stdout.Write(payload)
	return err
}

func requestAdmin(ctx context.Context, client *http.Client, cfg config.Config, method, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://"+cfg.Listen+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("contact agentd daemon: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &failure) == nil && strings.TrimSpace(failure.Error) != "" {
			return nil, errors.New(failure.Error)
		}
		return nil, fmt.Errorf("agentd daemon returned %s", response.Status)
	}
	return payload, nil
}
