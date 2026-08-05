package herdr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dkta-labs/agentd/internal/runner"
)

const (
	defaultPollInterval  = 250 * time.Millisecond
	defaultRecoveryGrace = 6 * time.Second
	promptWaitTimeoutMS  = "6000"
	tabShellRetryDelay   = 100 * time.Millisecond
	tabShellReadyLimit   = 10 * time.Second
)

// Runner dispatches work into normal interactive OMP agents owned by Herdr.
// Agentd keeps only the scheduling and observation responsibility.
type Runner struct {
	Binary            string
	AgentArgs         []string
	AgentEnv          map[string]string
	CoordinatorTarget string
	PollInterval      time.Duration
	RecoveryGrace     time.Duration
}

type agentInfo struct {
	Name       string
	Status     string
	Sequence   uint64
	TabID      string
	SessionRef string
}

type process struct {
	runner         *Runner
	owner          string
	coordinator    string
	startSeq       uint64
	observedWork   bool
	sessionRef     string
	pollInterval   time.Duration
	recoveryGrace  time.Duration
	recoveryAttach bool

	mu      sync.Mutex
	stopped bool
}

func (p *process) recordObservation(info agentInfo) {
	p.mu.Lock()
	p.observedWork = info.Status == "working" || info.Status == "blocked" || info.Sequence > p.startSeq
	if info.SessionRef != "" {
		p.sessionRef = info.SessionRef
	}
	p.mu.Unlock()
}

func (p *process) PreparedSequence() uint64 { return p.startSeq }

func (r Runner) Start(ctx context.Context, job runner.Job) (runner.Process, error) {
	prepared, err := r.Prepare(ctx, job)
	if err != nil {
		return nil, err
	}
	if err := prepared.Dispatch(ctx); err != nil {
		return nil, err
	}
	return prepared.Process(), nil
}

func (r Runner) Prepare(ctx context.Context, job runner.Job) (runner.Prepared, error) {
	workspaceID, ok := herdrWorkspaceID(job.WorkspaceID)
	if !ok {
		return nil, fmt.Errorf("workspace %q is not mapped to Herdr", job.WorkspaceID)
	}
	ownerKey := ownerFingerprint(job)
	owner := ownerName(ownerKey)
	info, err := r.get(ctx, owner)
	switch {
	case err == nil && (info.Status == "working" || info.Status == "blocked"):
		return nil, fmt.Errorf("%w: %s is %s", runner.ErrOwnerUnavailable, owner, info.Status)
	case err == nil && (info.Status == "idle" || info.Status == "done"):
		// Reuse the recurring job's existing interactive session.
	case err == nil:
		owner = ownerName(ownerKey + "\x00" + job.RunID)
		info, err = r.create(ctx, workspaceID, owner, job)
	case errors.Is(err, errAgentNotFound):
		info, err = r.create(ctx, workspaceID, owner, job)
	}
	if err != nil {
		return nil, err
	}
	return r.prepared(job, owner, info, info.Sequence, false), nil
}

func (r Runner) prepared(job runner.Job, owner string, info agentInfo, prePromptSequence uint64, recoveryAttach bool) runner.Prepared {
	interval := r.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	grace := r.RecoveryGrace
	if grace <= 0 {
		grace = defaultRecoveryGrace
	}
	return &prepared{
		process: &process{
			runner:         &r,
			owner:          owner,
			coordinator:    strings.TrimSpace(r.CoordinatorTarget),
			startSeq:       prePromptSequence,
			sessionRef:     info.SessionRef,
			pollInterval:   interval,
			recoveryGrace:  grace,
			recoveryAttach: recoveryAttach,
			observedWork:   info.Status == "working" || info.Status == "blocked" || info.Sequence > prePromptSequence,
		},
		prompt: ownershipPrompt(job, owner, strings.TrimSpace(r.CoordinatorTarget), r.Binary),
	}
}

type prepared struct {
	process *process
	prompt  string
}

func (p *prepared) Process() runner.Process { return p.process }

func (p *prepared) Dispatch(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	latest, err := p.process.runner.get(ctx, p.process.owner)
	if err != nil {
		return fmt.Errorf("observe Herdr owner before prompt: %w", err)
	}
	if p.accepted(latest) {
		return nil
	}
	if p.process.recoveryAttach && (latest.Status == "idle" || latest.Status == "done") && latest.Sequence == p.process.startSeq {
		if latest, err = p.waitForTransition(ctx); err != nil {
			return err
		}
		if p.accepted(latest) {
			return nil
		}
	}
	args := []string{"agent", "prompt", p.process.owner, p.prompt, "--wait",
		"--until", "working", "--until", "blocked", "--until", "idle",
		"--until", "done", "--until", "unknown", "--timeout", promptWaitTimeoutMS}
	if _, err := p.process.runner.command(ctx, args...); err != nil {
		return fmt.Errorf("prompt Herdr owner: %w", err)
	}
	latest, err = p.process.runner.get(ctx, p.process.owner)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	p.process.recordObservation(latest)
	return nil
}

func (p *prepared) accepted(info agentInfo) bool {
	if info.Status == "working" || info.Status == "blocked" || info.Sequence > p.process.startSeq {
		p.process.recordObservation(info)
		return true
	}
	return false
}

func (p *prepared) waitForTransition(ctx context.Context) (agentInfo, error) {
	grace := p.process.recoveryGrace
	if grace <= 0 {
		grace = defaultRecoveryGrace
	}
	waitCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	ticker := time.NewTicker(p.process.pollInterval)
	defer ticker.Stop()
	latest := agentInfo{}
	for {
		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return latest, nil
			}
			return latest, waitCtx.Err()
		default:
		}
		info, err := p.process.runner.get(waitCtx, p.process.owner)
		if err == nil {
			latest = info
			if info.Status == "working" || info.Status == "blocked" || info.Sequence > p.process.startSeq {
				return info, nil
			}
		} else if waitCtx.Err() != nil {
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return latest, nil
			}
			return latest, waitCtx.Err()
		}
		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return latest, nil
			}
			return latest, waitCtx.Err()
		case <-ticker.C:
		}
	}
}
func (p *prepared) PreparedSequence() uint64 { return p.process.startSeq }

func (r Runner) AttachPrepared(ctx context.Context, job runner.Job, owner string, prePromptSequence uint64) (runner.Prepared, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, fmt.Errorf("%w: missing owner target", runner.ErrOwnerUnavailable)
	}
	info, err := r.get(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", runner.ErrOwnerUnavailable, err)
	}
	if info.Status == "unknown" {
		return nil, fmt.Errorf("%w: %s is unknown", runner.ErrOwnerUnavailable, owner)
	}
	return r.prepared(job, owner, info, prePromptSequence, true), nil
}

func (r Runner) Attach(ctx context.Context, _ runner.Job, owner string) (runner.Process, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, fmt.Errorf("%w: missing owner target", runner.ErrOwnerUnavailable)
	}
	info, err := r.get(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", runner.ErrOwnerUnavailable, err)
	}
	if info.Status == "unknown" {
		return nil, fmt.Errorf("%w: %s is unknown", runner.ErrOwnerUnavailable, owner)
	}
	interval := r.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	return &process{
		runner:       &r,
		owner:        owner,
		coordinator:  strings.TrimSpace(r.CoordinatorTarget),
		startSeq:     info.Sequence,
		observedWork: true,
		sessionRef:   info.SessionRef,
		pollInterval: interval,
	}, nil
}

func (r Runner) create(ctx context.Context, workspaceID, owner string, job runner.Job) (agentInfo, error) {
	args := []string{"tab", "create", "--workspace", workspaceID, "--cwd", job.WorkspacePath, "--label", owner, "--no-focus"}
	names := make([]string, 0, len(r.AgentEnv))
	for name := range r.AgentEnv {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		args = append(args, "--env", name+"="+r.AgentEnv[name])
	}
	output, err := r.command(ctx, args...)
	if err != nil {
		return agentInfo{}, fmt.Errorf("create Herdr tab: %w", err)
	}
	var response struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
			Tab struct {
				TabID string `json:"tab_id"`
			} `json:"tab"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &response); err != nil || response.Result.RootPane.PaneID == "" || response.Result.Tab.TabID == "" {
		if err == nil {
			err = errors.New("missing result.root_pane.pane_id or result.tab.tab_id")
		}
		return agentInfo{}, fmt.Errorf("decode Herdr tab: %w", err)
	}
	startArgs := []string{"agent", "start", owner, "--kind", "omp", "--pane", response.Result.RootPane.PaneID, "--timeout", "300000", "--"}
	startArgs = append(startArgs, r.AgentArgs...)
	if err := r.startInteractive(ctx, startArgs); err != nil {
		_, _ = r.command(context.Background(), "tab", "close", response.Result.Tab.TabID)
		return agentInfo{}, err
	}
	return r.get(ctx, owner)
}

func (r Runner) startInteractive(ctx context.Context, args []string) error {
	deadline := time.Now().Add(tabShellReadyLimit)
	for {
		output, err := r.command(ctx, args...)
		if err == nil {
			return nil
		}
		if !strings.Contains(string(output), `"code":"agent_pane_busy"`) || time.Now().After(deadline) {
			return fmt.Errorf("start interactive OMP agent: %w", err)
		}
		timer := time.NewTimer(tabShellRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

var errAgentNotFound = errors.New("Herdr agent not found")

func (r Runner) get(ctx context.Context, target string) (agentInfo, error) {
	output, err := r.command(ctx, "agent", "get", target)
	if err != nil {
		if ctx.Err() != nil {
			return agentInfo{}, ctx.Err()
		}
		if strings.Contains(string(output), `"code":"agent_not_found"`) {
			return agentInfo{}, errAgentNotFound
		}
		return agentInfo{}, err
	}
	var response struct {
		Result struct {
			Agent struct {
				Name     string `json:"name"`
				Status   string `json:"agent_status"`
				Sequence uint64 `json:"state_change_seq"`
				TabID    string `json:"tab_id"`
				Session  *struct {
					Value string `json:"value"`
				} `json:"agent_session"`
			} `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return agentInfo{}, fmt.Errorf("decode Herdr agent: %w", err)
	}
	result := agentInfo{
		Name: response.Result.Agent.Name, Status: response.Result.Agent.Status,
		Sequence: response.Result.Agent.Sequence, TabID: response.Result.Agent.TabID,
	}
	if response.Result.Agent.Session != nil {
		result.SessionRef = response.Result.Agent.Session.Value
	}
	if result.Status == "" || result.TabID == "" {
		return agentInfo{}, errors.New("decode Herdr agent: missing status or tab id")
	}
	return result, nil
}

func (r Runner) command(ctx context.Context, args ...string) ([]byte, error) {
	binary := configuredBinary(r.Binary)
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err == nil {
		return output, nil
	}
	if ctx.Err() != nil {
		return output, ctx.Err()
	}
	message := strings.TrimSpace(string(output))
	if len(message) > 512 {
		message = message[:512]
	}
	if message == "" {
		return output, err
	}
	return output, fmt.Errorf("%w: %s", err, message)
}

func (p *process) Wait(ctx context.Context) (runner.Exit, error) {
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()
	for {
		info, err := p.runner.get(ctx, p.owner)
		if err != nil {
			if ctx.Err() != nil {
				return runner.Exit{}, ctx.Err()
			}
			if errors.Is(err, errAgentNotFound) {
				return runner.Exit{}, fmt.Errorf("%w: %s is missing", runner.ErrOwnerUnavailable, p.owner)
			}
			select {
			case <-ctx.Done():
				return runner.Exit{}, ctx.Err()
			case <-ticker.C:
				continue
			}
		}
		if info.SessionRef != "" {
			p.mu.Lock()
			p.sessionRef = info.SessionRef
			p.mu.Unlock()
		}
		p.mu.Lock()
		observedWork := p.observedWork
		switch info.Status {
		case "working", "blocked":
			p.observedWork = true
			observedWork = true
		case "idle", "done":
			if observedWork || info.Sequence >= p.startSeq+2 {
				p.mu.Unlock()
				return runner.Exit{Code: 0}, nil
			}
		case "unknown":
			p.mu.Unlock()
			return runner.Exit{}, fmt.Errorf("%w: %s became unknown", runner.ErrOwnerUnavailable, p.owner)
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return runner.Exit{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *process) Stop(ctx context.Context) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	info, err := p.runner.get(ctx, p.owner)
	if errors.Is(err, errAgentNotFound) {
		p.markStopped()
		return nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if info.Status == "unknown" {
		p.markStopped()
		return nil
	}
	if _, err = p.runner.command(ctx, "tab", "close", info.TabID); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	p.markStopped()
	return nil
}

func (p *process) markStopped() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
}
func (p *process) ExecutionReference() string {
	if strings.TrimSpace(p.sessionRef) != "" {
		return p.sessionRef
	}
	return p.owner
}

func (p *process) ProcessReference() string { return p.owner }

func (p *process) Output() (string, string) {
	message := "Herdr owner: " + sanitizeTerminal(p.owner)
	if p.coordinator != "" {
		message += "\nCoordinator: " + sanitizeTerminal(p.coordinator)
	}
	return message + "\n", ""
}

func ownerName(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "agentd-" + hex.EncodeToString(digest[:10])
}

func ownerFingerprint(job runner.Job) string {
	return strings.Join([]string{
		job.ID,
		job.WorkspaceID,
		filepath.Clean(job.WorkspacePath),
	}, "\x00")
}

func ownershipPrompt(job runner.Job, owner, coordinator string, binaries ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Agentd detached work assignment.\nJob: %s\nRun: %s\nOwner: %s\n", job.ID, job.RunID, owner)
	if job.GoalKey != "" {
		fmt.Fprintf(&b, "Goal key: %s\n", job.GoalKey)
	}
	b.WriteString("You are a normal interactive OMP worker in Herdr. Dispatch authorizes assigned-scope work end-to-end: investigation, edits, tests, commit, push, PR, independent review, merge after required CI, existing deployment, production verification, and fix-forward. Do not stop for approval or wait for a human gate.\n")
	b.WriteString("Work only in this goal's worktree, branch, issue, and PR; never touch another goal's worktree, branch, issue, or PR. Own the requested work independently; Agentd only records scheduling and ownership. Do not wait for or poll the coordinator.\n")
	if coordinator != "" {
		binary := ""
		if len(binaries) > 0 {
			binary = binaries[0]
		}
		wakePrompt := "Agentd run " + job.RunID + " needs: <exact blocker and requested decision>"
		fmt.Fprintf(&b, "Wake the coordinator exactly once only for irreducible ambiguity or a genuine external blocker: run %s agent prompt %s %s once, then remain available in this session. Do not wake the coordinator for approval, progress updates, or routine choices.\n",
			posixShellQuote(binary), posixShellQuote(coordinator), posixShellQuote(wakePrompt))
	}
	b.WriteString("When the requested turn is complete, leave the final result in this interactive session and return to the OMP prompt.\n\nInvocation request:\n")
	b.WriteString(job.InvocationRequest)
	return b.String()
}

func configuredBinary(binary string) string {
	binary = strings.TrimSpace(binary)
	if binary == "" {
		return "herdr"
	}
	return binary
}

func posixShellQuote(value string) string {
	value = sanitizeTerminal(value)
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func sanitizeTerminal(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
func herdrWorkspaceID(id string) (string, bool) {
	parts := strings.Split(id, ":")
	if len(parts) < 4 || parts[0] != "herdr" || strings.TrimSpace(parts[2]) == "" {
		return "", false
	}
	return parts[2], true
}
