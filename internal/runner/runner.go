package runner

import "context"

type Job struct {
	ID                string
	RunID             string
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
