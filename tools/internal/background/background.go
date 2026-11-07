// Package background defines the small producer-facing background-job contract
// that the shell (bash, pwsh) and terminal tools use to hand asynchronous work
// to a host-provided job manager.
//
// It is internal on purpose. The concrete implementation lives in tools/jobs
// (which imports this package), and tools/builtin and tools/terminal depend only
// on these interfaces, so the three packages never import each other and the
// host wires them together:
//
//	background := jobs.NewManager(jobs.WithPolicy(policy))
//	bash := builtin.NewShellWithOptions(root, timeout, builtin.WithBackgroundJobs(background))
//	terminals := terminal.NewManager(terminal.WithBackgroundJobs(background))
//
// A manager owns job identity, lifecycle, the bounded output ring, waiting,
// cancellation and completion notices; producers only provide the process argv
// or an opaque operation plus its incremental output source.
package background

import (
	"context"
	"time"
)

// Kind is a producer-defined job kind. The kind is also the job id prefix
// (`bash-1a2b3c`, `pty-send-4d5e6f`) and is shown by job_list.
type Kind string

// Job kinds used by the Rig producers.
const (
	KindBash    Kind = "bash"
	KindPwsh    Kind = "pwsh"
	KindPTYSend Kind = "pty-send"
)

// Status is the DSH-compatible lifecycle state of one job:
// running, optionally stopping, then exactly one terminal state.
type Status string

// Job lifecycle states.
const (
	StatusRunning   Status = "running"
	StatusStopping  Status = "stopping"
	StatusCompleted Status = "completed"
	StatusKilled    Status = "killed"
	StatusFailed    Status = "failed"
)

// Settled reports whether status is terminal.
func (s Status) Settled() bool {
	switch s {
	case StatusCompleted, StatusKilled, StatusFailed:
		return true
	default:
		return false
	}
}

// JobView is one immutable projection of a managed background job. It never
// carries output; reads consume the job's ring separately.
type JobView struct {
	// ID is the manager-issued job id (`<kind>-<hex>`).
	ID string
	// Kind is the producer kind the job was registered with.
	Kind string
	// Label is the producer-supplied one-line label (the command text).
	Label string
	// Status is the lifecycle state.
	Status Status
	// Detail is the terminal reason once settled (`exit code: 3`) or a
	// description of what the job is doing while live.
	Detail string
	// ExitCode is the top-level process exit code (-1 when running or when the
	// producer has no process).
	ExitCode int
	// Started is the registration time.
	Started time.Time
	// Finished is the settlement time (zero while running).
	Finished time.Time
}

// Job is the producer's handle to one started background job.
type Job interface {
	// ID returns the manager-issued job id.
	ID() string
	// Wait blocks until the job settles or timeout elapses. timeout <= 0 means
	// "no deadline" (bounded only by ctx). It reports whether the job settled;
	// a timed-out or canceled wait leaves the job running. A live wait is
	// registered as "awaited", so a settlement it collects produces no
	// completion notice.
	Wait(ctx context.Context, timeout time.Duration) (JobView, bool, error)
	// Read returns the output produced since the previous consuming Read plus a
	// fresh projection. It never blocks.
	Read() (string, JobView)
	// Cancel requests termination without waiting. The reason is recorded and
	// forwarded to the producer; a canceled job suppresses its completion
	// notice because the caller already knows.
	Cancel(reason string)
	// Drop removes a settled job that the caller collected itself (a foreground
	// command), so the model never sees its id and a pending completion notice
	// is discarded. Dropping a live job is an error.
	Drop() error
}

// Manager is the background-job surface a host attaches to the shell and
// terminal tools. *jobs.Manager implements it.
type Manager interface {
	// StartProcess starts one command as a managed background job and returns
	// its handle immediately.
	StartProcess(spec ProcessSpec) (Job, error)
	// StartOperation starts an opaque producer operation (a terminal send) as a
	// managed background job and returns its handle immediately.
	StartOperation(spec OperationSpec) (Job, error)
}

// ProcessSpec describes one process-backed background job.
type ProcessSpec struct {
	// Kind is the producer kind (KindBash, KindPwsh).
	Kind string
	// Label is the one-line model-facing label (the command text).
	Label string
	// Command is the original command text. It is used for display and, when
	// the manager has a sandbox policy and PolicyChecked is false, for the
	// policy's shell-syntax check.
	Command string
	// Argv is the complete process argument vector (argv[0] is the binary).
	Argv []string
	// Dir is the working directory; empty means the manager's default.
	Dir string
	// Env is the process environment; nil means the manager's default. With a
	// sandbox policy the manager always applies the policy's environment
	// allowlist instead.
	Env []string
	// Timeout, when positive, kills the job after that duration. Background
	// jobs pass zero; a foreground job that is promoted keeps running.
	Timeout time.Duration
	// OutputLimitBytes caps one model-facing output read (0 means the manager
	// default).
	OutputLimitBytes int
	// PolicyChecked says the producer already validated Command and Argv
	// against the same sandbox policy the manager holds, so the manager only
	// re-applies environment, working-directory and read-only checks.
	PolicyChecked bool
}

// OperationSpec describes one opaque producer operation (a persistent terminal
// send) exposed as a background job.
type OperationSpec struct {
	// Kind is the producer kind (KindPTYSend).
	Kind string
	// Label is the one-line model-facing label.
	Label string
	// OutputLimitBytes caps one model-facing output read (0 means the manager
	// default).
	OutputLimitBytes int
	// Sources are the pull sources the manager pumps into the job's bounded
	// output ring. The manager drains every source once more before settlement.
	Sources []Source
	// Run starts the operation synchronously. A returned error leaves nothing
	// registered. Run is called once, after admission.
	Run func() (Operation, error)
}

// Source is one incremental output source the manager pumps into a job's ring.
type Source interface {
	// Read blocks until text is available, the source finishes, or ctx is
	// canceled. final reports that no further text will ever arrive (text may
	// still be non-empty on the final read). An error ends the job as failed.
	//
	// The manager cancels ctx once the operation settles; on cancellation Read
	// must return any text it already holds before reporting final=true, so a
	// settlement never loses the producer's last output.
	Read(ctx context.Context) (text string, final bool, err error)
}

// Operation is the producer side of a started operation job.
type Operation interface {
	// Cancel requests termination; reason is the caller-supplied job_kill
	// reason ("" when none). It must be idempotent and must not block.
	Cancel(reason string)
	// Done settles after the operation actually stops (resources released).
	// Exactly one Outcome must be delivered; the manager reads it once.
	Done() <-chan Outcome
}

// Outcome is the terminal result a producer reports for an operation job.
type Outcome struct {
	// Status must be StatusCompleted, StatusKilled or StatusFailed.
	Status Status
	// Detail is the terminal reason rendered by job_output/job_list
	// (`wait: session_exit`, `signal: SIGINT`).
	Detail string
}

// Notice is one completion notice the manager queued for the owning agent.
// Hosts (the agent loop) inject or wake on these; the jobs tools themselves
// never consume them.
type Notice struct {
	// JobID is the settled job id.
	JobID string
	// Kind is the producer kind.
	Kind string
	// Label is the producer-supplied label.
	Label string
	// Status is the terminal DSH status.
	Status Status
	// Detail is the terminal reason.
	Detail string
	// Text is a ready-to-inject model-facing notice.
	Text string
}
