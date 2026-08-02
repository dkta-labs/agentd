package omp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dkta-labs/agentd/internal/runner"
)

const (
	MaxOutputBytes   = 1 << 20
	MaxEnvValueBytes = 64 << 10
)

type Runner struct {
	Binary      string
	Args        []string
	EnvFiles    map[string]string
	SessionRoot string
	MaxOutput   int
}

func (r Runner) Start(ctx context.Context, job runner.Job) (runner.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if job.WorkspacePath == "" {
		return nil, errors.New("workspace path is required")
	}
	if job.InvocationRequest == "" {
		return nil, errors.New("invocation request is required")
	}
	binary := r.Binary
	if binary == "" {
		binary = "omp"
	}
	root := r.SessionRoot
	if root == "" {
		root = os.TempDir()
	}
	sessionDir := filepath.Join(root, job.RunID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, fmt.Errorf("create isolated run storage: %w", err)
	}
	args := make([]string, 0, len(r.Args)+5)
	args = append(args, r.Args...)
	args = append(args, "-p", "--session-dir", sessionDir, "--", job.InvocationRequest)
	cmd := exec.Command(binary, args...)
	cmd.Dir = job.WorkspacePath
	environment, err := loadEnvFiles(os.Environ(), r.EnvFiles)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(environment, "OMP_SESSION_DIR="+sessionDir, "OMP_SESSION_PATH="+sessionDir, "OMP_SESSION_ID="+job.RunID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start omp: %w", err)
	}
	p := &process{cmd: cmd, pid: cmd.Process.Pid, execution: sessionDir, outputLimit: r.MaxOutput, done: make(chan struct{})}
	if p.outputLimit <= 0 {
		p.outputLimit = MaxOutputBytes
	}
	p.processReference = "pid=" + strconv.Itoa(p.pid) + " pgid=" + strconv.Itoa(p.pid)
	var output sync.WaitGroup
	output.Add(2)
	go func() {
		defer output.Done()
		_, _ = io.Copy(boundedWriter{target: &p.stdout, limit: p.outputLimit, mu: &p.outputMu}, stdout)
	}()
	go func() {
		defer output.Done()
		_, _ = io.Copy(boundedWriter{target: &p.stderr, limit: p.outputLimit, mu: &p.outputMu}, stderr)
	}()
	go func() {
		err := cmd.Wait()
		output.Wait()
		p.mu.Lock()
		p.exit = exitFrom(err)
		p.waitErr = waitError(err)
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}
func loadEnvFiles(environment []string, files map[string]string) ([]string, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file, err := os.Open(files[name])
		if err != nil {
			return nil, fmt.Errorf("open environment file for %s: %w", name, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, MaxEnvValueBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read environment file for %s: %w", name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close environment file for %s: %w", name, closeErr)
		}
		if len(data) > MaxEnvValueBytes {
			return nil, fmt.Errorf("environment file for %s exceeds %d bytes", name, MaxEnvValueBytes)
		}
		value := strings.TrimRight(string(data), "\r\n")
		if value == "" {
			return nil, fmt.Errorf("environment file for %s is empty", name)
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("environment file for %s contains a NUL byte", name)
		}
		environment = setEnvironment(environment, name, value)
	}
	return environment, nil
}
func setEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	replacement := prefix + value
	for i := range environment {
		if strings.HasPrefix(environment[i], prefix) {
			environment[i] = replacement
			return environment
		}
	}
	return append(environment, replacement)
}

type boundedWriter struct {
	target *bytes.Buffer
	limit  int
	mu     *sync.Mutex
}

func (w boundedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := len(data)
	if remaining := w.limit - w.target.Len(); remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = w.target.Write(data)
	}
	return total, nil
}

type process struct {
	cmd                         *exec.Cmd
	pid                         int
	execution, processReference string
	outputLimit                 int
	stdout, stderr              bytes.Buffer
	outputMu                    sync.Mutex
	done                        chan struct{}
	mu                          sync.Mutex
	exit                        runner.Exit
	waitErr                     error
	stopOnce                    sync.Mutex
}

func (p *process) ExecutionReference() string { return p.execution }
func (p *process) ProcessReference() string   { return p.processReference }
func (p *process) Output() (string, string) {
	p.outputMu.Lock()
	defer p.outputMu.Unlock()
	return boundedString(p.stdout.Bytes(), p.outputLimit), boundedString(p.stderr.Bytes(), p.outputLimit)
}
func boundedString(data []byte, limit int) string {
	if len(data) > limit {
		data = data[:limit]
	}
	return string(data)
}
func (p *process) Wait(ctx context.Context) (runner.Exit, error) {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.exit, p.waitErr
	case <-ctx.Done():
		return runner.Exit{}, ctx.Err()
	}
}
func (p *process) Stop(ctx context.Context) error {
	p.stopOnce.Lock()
	defer p.stopOnce.Unlock()
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := syscall.Kill(-p.pid, syscall.SIGTERM); err != nil {
		select {
		case <-p.done:
			return nil
		default:
			return fmt.Errorf("terminate process group %d: %w", p.pid, err)
		}
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
	}
	if err := syscall.Kill(-p.pid, syscall.SIGKILL); err != nil {
		select {
		case <-p.done:
			return nil
		default:
			return fmt.Errorf("kill process group %d: %w", p.pid, err)
		}
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func exitFrom(err error) runner.Exit {
	if err == nil {
		return runner.Exit{Code: 0}
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return runner.Exit{Err: err}
	}
	status, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		return runner.Exit{Err: ee}
	}
	e := runner.Exit{Code: status.ExitStatus()}
	if status.Signaled() {
		e.Code = -1
		e.Signal = status.Signal().String()
	}
	return e
}
func waitError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return nil
	}
	return err
}
