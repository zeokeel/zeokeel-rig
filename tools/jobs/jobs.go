// Package jobs provides background-process tools: job_start, job_output,
// job_kill, job_list and job_wait.
//
// A Manager owns the jobs of one agent session. Each job is a `/bin/sh -c`
// process in its own process group with merged stdout+stderr written to a
// bounded ring buffer (default 1 MiB per job). The tools never block on a job:
// job_start returns immediately, job_output reads the buffered output, and
// job_wait is an explicit blocking call with its own timeout.
//
// # Lifecycle
//
// Finished jobs stay registered so their output and exit code remain readable;
// job_list shows them as exited/killed/timed_out. The host calls
// Manager.CloseAll on shutdown (or when the agent instance goes away) to kill
// running jobs and release every buffer. A job that exceeds its
// job_start timeout_ms is killed and reported as timed_out.
//
// # Sandbox policy
//
// WithPolicy applies a sandbox.Policy to every job: a read-only policy refuses
// to start jobs, the command is checked against Policy.CheckCommand, the
// working directory must be inside a policy root, and the process inherits only
// Policy.CommandEnv. Policy.MaxOutputBytes caps one tool response when it is
// smaller than the manager's own cap.
//
// # Limits
//
// The output ring keeps the newest bytes and drops the oldest; readers see a
// "[truncated: ...]" notice when they missed data. Tool responses are capped
// (DefaultMaxOutputBytes) so one job cannot flood the model context. A manager
// holds at most DefaultMaxJobs with retained output.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// Defaults and limits. They are exported so hosts and tests can assert the
// documented behavior.
const (
	// DefaultBufferBytes is the per-job output ring capacity.
	DefaultBufferBytes = 1 << 20
	// DefaultMaxOutputBytes caps one tool response.
	DefaultMaxOutputBytes = 32 << 10
	// DefaultWaitTimeout bounds job_wait when the call has no timeout_ms.
	DefaultWaitTimeout = 30 * time.Second
	// MaxWaitTimeout caps a per-call wait timeout_ms.
	MaxWaitTimeout = 10 * time.Minute
	// DefaultMaxJobs bounds the retained job table per Manager.
	DefaultMaxJobs = 64
	// DefaultTailLines is how many trailing lines job_wait returns.
	DefaultTailLines = 100
	// killGrace is how long job_kill/CloseAll waits for a killed process.
	killGrace = 2 * time.Second
	// waitDelay closes inherited stdio pipes shortly after the process dies.
	waitDelay = time.Second
)

// Tool names. They are stable model-facing identifiers.
const (
	ToolJobStart  = "job_start"
	ToolJobOutput = "job_output"
	ToolJobKill   = "job_kill"
	ToolJobList   = "job_list"
	ToolJobWait   = "job_wait"
)

// Option configures a Manager.
type Option func(*Manager)

// WithPolicy applies a sandbox policy to every job of this manager.
func WithPolicy(policy *sandbox.Policy) Option {
	return func(manager *Manager) { manager.policy = policy }
}

// WithBufferBytes sets the per-job output ring capacity (non-positive keeps
// DefaultBufferBytes).
func WithBufferBytes(capacity int) Option {
	return func(manager *Manager) {
		if capacity > 0 {
			manager.bufferBytes = capacity
		}
	}
}

// WithMaxOutputBytes caps one tool response (non-positive keeps
// DefaultMaxOutputBytes).
func WithMaxOutputBytes(maxBytes int) Option {
	return func(manager *Manager) {
		if maxBytes > 0 {
			manager.maxOutputBytes = maxBytes
		}
	}
}

// WithMaxJobs bounds the retained job table (non-positive keeps DefaultMaxJobs).
func WithMaxJobs(max int) Option {
	return func(manager *Manager) {
		if max > 0 {
			manager.maxJobs = max
		}
	}
}

// WithWaitTimeout sets the default job_wait timeout (non-positive keeps
// DefaultWaitTimeout).
func WithWaitTimeout(timeout time.Duration) Option {
	return func(manager *Manager) {
		if timeout > 0 {
			manager.waitTimeout = timeout
		}
	}
}

// Manager owns background jobs. It is safe for concurrent use.
type Manager struct {
	mu sync.Mutex

	jobs map[string]*job
	// policy is optional; nil means no sandbox policy (the host's approval
	// gate is then the only boundary).
	policy *sandbox.Policy

	bufferBytes    int
	maxOutputBytes int
	maxJobs        int
	waitTimeout    time.Duration
}

// NewManager creates an empty job manager with the package defaults.
func NewManager(opts ...Option) *Manager {
	manager := &Manager{
		jobs:           make(map[string]*job),
		bufferBytes:    DefaultBufferBytes,
		maxOutputBytes: DefaultMaxOutputBytes,
		maxJobs:        DefaultMaxJobs,
		waitTimeout:    DefaultWaitTimeout,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(manager)
		}
	}
	return manager
}

// Tools returns the five job tools bound to manager (convenience wrapper
// around (*Manager).Tools for assembly code).
func Tools(manager *Manager) []rig.Tool {
	if manager == nil {
		return nil
	}
	return manager.Tools()
}

// Tools returns the five job tools bound to this manager.
func (m *Manager) Tools() []rig.Tool {
	return []rig.Tool{
		m.startTool(),
		m.outputTool(),
		m.killTool(),
		m.listTool(),
		m.waitTool(),
	}
}

// Len reports the number of retained jobs (running and finished).
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.jobs)
}

// CloseAll kills every running job, waits for it to be reaped and clears the
// job table. It is the host shutdown hook and is safe to call repeatedly.
func (m *Manager) CloseAll() error {
	m.mu.Lock()
	current := make([]*job, 0, len(m.jobs))
	for id, running := range m.jobs {
		current = append(current, running)
		delete(m.jobs, id)
	}
	m.mu.Unlock()
	var errs []error
	for _, running := range current {
		running.mu.Lock()
		exited := running.exited
		running.mu.Unlock()
		if !exited {
			if err := signalProcessGroup(running.cmd, signalSpec{name: "KILL"}); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, running := range current {
		select {
		case <-running.done:
		case <-time.After(killGrace):
			errs = append(errs, fmt.Errorf("jobs: job %s did not exit after kill", running.id))
		}
	}
	return errors.Join(errs...)
}

// job is one background process and its retained output.
type job struct {
	manager *Manager
	id      string
	command string
	cmd     *exec.Cmd
	started time.Time
	buffer  *outputBuffer
	done    chan struct{}

	doneOnce sync.Once
	mu       sync.Mutex
	exited   bool
	exitCode int
	exitErr  error
	ended    time.Time
	timedOut bool
	killed   bool
	consumed int64
}

// start launches one background process.
func (m *Manager) start(command, cwd string, timeout time.Duration) (*job, error) {
	if m.policy != nil && m.policy.ReadOnly() {
		return nil, errors.New("jobs: sandbox policy is read-only; background jobs are disabled")
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("jobs: command is required")
	}
	if strings.ContainsRune(command, 0) {
		return nil, errors.New("jobs: command contains a NUL byte")
	}
	m.mu.Lock()
	if len(m.jobs) >= m.maxJobs {
		limit := m.maxJobs
		m.mu.Unlock()
		return nil, fmt.Errorf("jobs: too many retained jobs (limit %d); list and let the host clean up", limit)
	}
	m.mu.Unlock()

	workdir, err := m.resolveCwd(cwd)
	if err != nil {
		return nil, err
	}
	if m.policy != nil {
		if err := m.policy.CheckShellCommand(command); err != nil {
			return nil, err
		}
		argv, splitErr := splitShellCommand(command)
		if splitErr != nil {
			return nil, splitErr
		}
		if err := m.policy.CheckCommand(commandForPolicy(argv), workdir); err != nil {
			return nil, err
		}
		// An explicit job timeout may not exceed the policy's command timeout.
		if timeout > m.policy.Timeout() {
			timeout = m.policy.Timeout()
		}
	}

	cmd := exec.Command("/bin/sh", "-c", command)
	configureProcessGroup(cmd)
	cmd.Dir = workdir
	cmd.WaitDelay = waitDelay
	if m.policy != nil {
		cmd.Env = m.policy.CommandEnv(os.Environ())
	}
	output := newOutputBuffer(m.bufferBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("jobs: start %q: %w", command, err)
	}
	current := &job{
		manager: m,
		id:      newJobID(),
		command: command,
		cmd:     cmd,
		started: time.Now(),
		buffer:  output,
		done:    make(chan struct{}),
	}
	go current.waitLoop(timeout)
	m.mu.Lock()
	m.jobs[current.id] = current
	m.mu.Unlock()
	return current, nil
}

// waitLoop reaps the process, enforcing the job timeout.
func (j *job) waitLoop(timeout time.Duration) {
	waitDone := make(chan error, 1)
	go func() { waitDone <- j.cmd.Wait() }()
	if timeout <= 0 {
		j.finish(<-waitDone)
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waitDone:
		j.finish(err)
	case <-timer.C:
		j.mu.Lock()
		if !j.exited {
			j.timedOut = true
		}
		j.mu.Unlock()
		_ = signalProcessGroup(j.cmd, signalSpec{name: "KILL"})
		j.finish(<-waitDone)
	}
}

// finish records the exit state and closes the done channel.
func (j *job) finish(err error) {
	code := -1
	if err == nil {
		code = 0
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
	}
	j.mu.Lock()
	if !j.exited {
		j.exited = true
		j.exitCode = code
		j.exitErr = err
		j.ended = time.Now()
	}
	j.mu.Unlock()
	j.doneOnce.Do(func() { close(j.done) })
}

// signal sends one signal to the job's process group.
func (j *job) signal(spec signalSpec) error {
	j.mu.Lock()
	exited := j.exited
	j.mu.Unlock()
	if exited {
		return errors.New("jobs: job has already finished")
	}
	if err := signalProcessGroup(j.cmd, spec); err != nil {
		return err
	}
	j.mu.Lock()
	if !j.exited {
		j.killed = true
	}
	j.mu.Unlock()
	return nil
}

// jobStatus is one immutable snapshot of a job.
type jobStatus struct {
	state    string
	exitCode int
	duration time.Duration
}

// snapshot returns the current state.
func (j *job) snapshot() jobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	duration := time.Since(j.started)
	if j.exited {
		duration = j.ended.Sub(j.started)
	}
	state := "running"
	exitCode := -1
	switch {
	case !j.exited:
		state = "running"
	case j.timedOut:
		state = "timed_out"
		exitCode = j.exitCode
	case j.killed:
		state = "killed"
		exitCode = j.exitCode
	default:
		state = "exited"
		exitCode = j.exitCode
	}
	return jobStatus{state: state, exitCode: exitCode, duration: duration}
}

// newOutput returns output produced since the previous new-output read and
// advances the cursor.
func (j *job) newOutput() string {
	j.mu.Lock()
	from := j.consumed
	j.mu.Unlock()
	text, _ := j.buffer.Slice(from)
	j.mu.Lock()
	j.consumed = j.buffer.Total()
	j.mu.Unlock()
	return text
}

// outputFrom returns output starting at an absolute offset.
func (j *job) outputFrom(offset int64) string {
	text, _ := j.buffer.Slice(offset)
	return text
}

// tailOutput returns the last lines and drains the new-output cursor.
func (j *job) tailOutput() string {
	text := j.buffer.LastLines(DefaultTailLines)
	j.mu.Lock()
	j.consumed = j.buffer.Total()
	j.mu.Unlock()
	return text
}

// format renders the status header plus output.
func (j *job) format(output string) string {
	status := j.snapshot()
	var builder strings.Builder
	fmt.Fprintf(&builder, "job_id: %s\n", j.id)
	fmt.Fprintf(&builder, "command: %s\n", j.command)
	fmt.Fprintf(&builder, "state: %s\n", status.state)
	fmt.Fprintf(&builder, "exit_code: %d\n", status.exitCode)
	fmt.Fprintf(&builder, "duration_ms: %d\n", status.duration.Milliseconds())
	output = toolutil.TruncateBytes(output, j.manager.outputLimit(), fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", j.manager.outputLimit()))
	if output == "" {
		builder.WriteString("output: (none)\n")
	} else {
		builder.WriteString("output:\n")
		builder.WriteString(output)
		if !strings.HasSuffix(output, "\n") {
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

// outputLimit is the per-response cap: the manager's cap, tightened by the
// sandbox policy when the policy is stricter.
func (m *Manager) outputLimit() int {
	limit := m.maxOutputBytes
	if m.policy != nil && m.policy.MaxOutputBytes() < limit {
		limit = m.policy.MaxOutputBytes()
	}
	return limit
}

// lookup finds one retained job.
func (m *Manager) lookup(id string) (*job, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("jobs: job_id is required")
	}
	m.mu.Lock()
	current, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("jobs: unknown job %q", id)
	}
	return current, nil
}

// jobsSnapshot returns retained jobs ordered by start time.
func (m *Manager) jobsSnapshot() []*job {
	m.mu.Lock()
	out := make([]*job, 0, len(m.jobs))
	for _, current := range m.jobs {
		out = append(out, current)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].started.Equal(out[j].started) {
			return out[i].id < out[j].id
		}
		return out[i].started.Before(out[j].started)
	})
	return out
}

// wait blocks until the job finishes or timeout elapses; ctx cancellation aborts
// the wait without touching the job.
func (m *Manager) wait(ctx context.Context, current *job, timeout time.Duration) (bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-current.done:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	}
}

// resolveCwd validates the job working directory. With a policy the default is
// the primary root and the directory must be inside a root; without a policy
// the default is the process working directory.
func (m *Manager) resolveCwd(cwd string) (string, error) {
	if m.policy != nil {
		requested := strings.TrimSpace(cwd)
		if requested == "" {
			requested = "."
		}
		abs, err := m.policy.ResolvePath(requested)
		if err != nil {
			return "", fmt.Errorf("jobs: cwd: %w", err)
		}
		info, statErr := os.Stat(abs)
		if statErr != nil {
			return "", fmt.Errorf("jobs: cwd: %w", statErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("jobs: cwd %q is not a directory", cwd)
		}
		return abs, nil
	}
	dir := strings.TrimSpace(cwd)
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("jobs: cwd: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("jobs: cwd: %w", err)
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return "", fmt.Errorf("jobs: cwd: %w", statErr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("jobs: cwd %q is not a directory", cwd)
	}
	return abs, nil
}

// newJobID returns a short random job id.
func newJobID() string { return "job-" + randomHex(6) }

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	}
	return hex.EncodeToString(buf)
}
