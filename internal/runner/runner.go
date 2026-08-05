package runner

import (
	"context"
	"errors"
)

var ErrOwnerUnavailable = errors.New("Herdr owner is unavailable")

type Job struct {
	ID                string
	RunID             string
	GoalKey           string `json:"goalKey,omitempty"`
	WorkspaceID       string
	WorkspacePath     string
	InvocationRequest string
}

type Exit struct {
	Code   int
	Signal string
	Err    error
}

func (e Exit) Successful() bool { return e.Err == nil && e.Code == 0 && e.Signal == "" }

type Process interface {
	Wait(context.Context) (Exit, error)
	Stop(context.Context) error
}

type Evidence interface {
	ExecutionReference() string
	ProcessReference() string
	Output() (stdout, stderr string)
}

type Runner interface {
	Start(context.Context, Job) (Process, error)
}

// Preparer optionally splits runner startup into owner preparation and prompt
// dispatch. Supervisors use this seam to persist prepared ownership before
// sending work to an interactive runner.
type Preparer interface {
	Prepare(context.Context, Job) (Prepared, error)
}

type Prepared interface {
	Process() Process
	Dispatch(context.Context) error
}

// PreparedEvidence is optional evidence exposed by a prepared process before
// its prompt is dispatched. A zero sequence means that the runner does not
// expose a prepared sequence.
type PreparedEvidence interface {
	PreparedSequence() uint64
}

// Reattacher reconnects Agentd's background watcher to a Herdr-owned session
// after the daemon restarts. It never relaunches or re-prompts the worker.
type Reattacher interface {
	Attach(context.Context, Job, string) (Process, error)
}

// PreparedReattacher reconnects a prepared owner after a restart. Its
// Dispatch implementation must use the persisted pre-prompt sequence to make
// dispatch idempotent.
type PreparedReattacher interface {
	AttachPrepared(context.Context, Job, string, uint64) (Prepared, error)
}
