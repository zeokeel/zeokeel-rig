// Package jobs provides background-process tools: job_start, job_output,
// job_kill, job_list and job_wait.
//
// A Manager owns the jobs of one agent session. Each process job is a command
// in its own process group with merged stdout+stderr written to a bounded ring
// buffer (default 1 MiB per job). Operation jobs carry any producer-owned
// asynchronous work (a persistent terminal send) behind the same lifecycle,
// output ring, wait/kill controls and completion notices.
//
// # DSH compatibility
//
// The manager also implements the producer-facing background contract in
// tools/internal/background, so the bash, pwsh and terminal tools can register
// their background execution without importing this package. The model-facing
// controls accept the DSH argument names: job_output accepts wait and
// timeout_ms, job_kill accepts reason, and job_list reports each job's kind and
// label. Settled jobs stay registered until the host closes the manager or a
// foreground caller drops a job it collected itself.
//
// # Lifecycle
//
// Finished jobs stay registered so their output and exit code remain readable;
// job_list shows them as exited/killed/timed_out/failed. The host calls
// Manager.CloseAll on shutdown (or when the agent instance goes away) to kill
// running jobs and release every buffer. A job that exceeds its timeout is
// killed and reported as timed_out.
//
// # Completion notices
//
// When a job settles without a caller waiting on it (and without a model kill),
// the manager queues a deterministic completion notice; hosts read them with
// PendingNotices or register WithNoticeHandler. A settlement that releases a
// live wait — job_output(wait: true), job_wait, or a foreground shell/tool wait
// — is marked "awaited" and produces no notice, matching DSH.
//
// # Sandbox policy
//
// WithPolicy applies a sandbox.Policy to every job: a read-only policy refuses
// to start jobs, process commands are checked against Policy.CheckCommand, the
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
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// Defaults and limits. They are exported so hosts and tests can assert the
// documented behavior.
const (
	// DefaultBufferBytes is the per-job output ring capacity.
	DefaultBufferBytes = 1 << 20
	// DefaultMaxOutputBytes caps one tool response.
	DefaultMaxOutputBytes = 32 << 10
	// DefaultWaitTimeout bounds job_output(wait)/job_wait when the call has no
	// timeout_ms.
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
	// operationDrainGrace bounds how long settlement waits for operation
	// sources to deliver their final tail before closing the ring.
	operationDrainGrace = 2 * time.Second
)

// Tool names. They are stable model-facing identifiers.
const (
	ToolJobStart  = "job_start"
	ToolJobOutput = "job_output"
	ToolJobKill   = "job_kill"
	ToolJobList   = "job_list"
	ToolJobWait   = "job_wait"
)

// errJobFinished is returned when a signal/kill targets an already settled job.
var errJobFinished = errors.New("jobs: job has already finished")

// IsFinishedError reports whether err only means the job had already finished.
// Tool callers still return the final state in that case.
func IsFinishedError(err error) bool { return err != nil && errors.Is(err, errJobFinished) }

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

// WithWaitTimeout sets the default job_output(wait)/job_wait timeout
// (non-positive keeps DefaultWaitTimeout).
func WithWaitTimeout(timeout time.Duration) Option {
	return func(manager *Manager) {
		if timeout > 0 {
			manager.waitTimeout = timeout
		}
	}
}

// WithNoticeHandler registers a completion-notice callback. The handler runs on
// its own goroutine after a job settles without an awaited caller. Registering a
// handler is optional — PendingNotices works without one — and a host that does
// not want notices can simply ignore both.
func WithNoticeHandler(handler func(Notice)) Option {
	return func(manager *Manager) { manager.noticeHandler = handler }
}

// Notice re-exports the background completion notice type for hosts that import
// only tools/jobs.
type Notice = background.Notice

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

	notices       []background.Notice
	noticeHandler func(background.Notice)
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

// PendingNotices returns and clears the queued completion notices. Hosts that
// poll (instead of WithNoticeHandler) use it after each agent step to turn
// settled background jobs into in-session messages.
func (m *Manager) PendingNotices() []background.Notice {
	m.mu.Lock()
	defer m.mu.Unlock()
	notices := m.notices
	m.notices = nil
	return notices
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
	m.notices = nil
	m.mu.Unlock()
	var errs []error
	for _, running := range current {
		running.mu.Lock()
		running.teardown = true
		exited := running.exited
		running.mu.Unlock()
		if !exited {
			if err := running.terminate("teardown"); err != nil && !IsFinishedError(err) {
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

// StartProcess implements background.Manager: it starts one command as a
// managed background job and returns its handle immediately.
func (m *Manager) StartProcess(spec background.ProcessSpec) (background.Job, error) {
	if strings.TrimSpace(spec.Kind) == "" {
		return nil, errors.New("jobs: background job kind is required")
	}
	if len(spec.Argv) == 0 {
		return nil, errors.New("jobs: background job argv is required")
	}
	current, err := m.startExec(processRequest{
		kind:          spec.Kind,
		label:         spec.Label,
		command:       spec.Command,
		argv:          spec.Argv,
		cwd:           spec.Dir,
		env:           spec.Env,
		timeout:       spec.Timeout,
		outputLimit:   spec.OutputLimitBytes,
		policyChecked: spec.PolicyChecked,
	})
	if err != nil {
		return nil, err
	}
	return current, nil
}

// StartOperation implements background.Manager: it starts one producer-owned
// operation as a managed background job and returns its handle immediately.
func (m *Manager) StartOperation(spec background.OperationSpec) (background.Job, error) {
	if strings.TrimSpace(spec.Kind) == "" {
		return nil, errors.New("jobs: background job kind is required")
	}
	if spec.Run == nil {
		return nil, errors.New("jobs: operation run function is required")
	}
	if len(spec.Sources) == 0 {
		return nil, errors.New("jobs: operation output source is required")
	}
	m.mu.Lock()
	if len(m.jobs) >= m.maxJobs {
		limit := m.maxJobs
		m.mu.Unlock()
		return nil, fmt.Errorf("jobs: too many retained jobs (limit %d); list and let the host clean up", limit)
	}
	m.mu.Unlock()

	operation, err := spec.Run()
	if err != nil {
		return nil, fmt.Errorf("jobs: start operation %q: %w", spec.Kind, err)
	}
	if operation == nil || operation.Done() == nil {
		return nil, errors.New("jobs: operation returned no done channel")
	}
	current := &job{
		manager:         m,
		id:              newJobIDForKind(spec.Kind),
		kind:            spec.Kind,
		label:           spec.Label,
		command:         spec.Label,
		started:         time.Now(),
		buffer:          newOutputBuffer(m.bufferBytes),
		done:            make(chan struct{}),
		exitCode:        -1,
		outputLimit:     spec.OutputLimitBytes,
		cancelOperation: operation.Cancel,
		operationDone:   operation.Done(),
	}
	current.pumpCtx, current.pumpCancel = context.WithCancel(context.Background())
	m.mu.Lock()
	m.jobs[current.id] = current
	m.mu.Unlock()
	go current.pumpOperation(spec.Sources, operation)
	return current, nil
}

// start launches one background shell process for the job_start tool.
func (m *Manager) start(command, cwd string, timeout time.Duration) (*job, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("jobs: background shell jobs require a unix /bin/sh (Unix-only in this release); use the pwsh tool on Windows")
	}
	return m.startExec(processRequest{
		kind:     string(background.KindBash),
		label:    command,
		command:  command,
		argv:     []string{"/bin/sh", "-c", command},
		cwd:      cwd,
		timeout:  timeout,
		idPrefix: "job-",
	})
}

// processRequest is one process job to spawn (legacy job_start or a producer
// background run).
type processRequest struct {
	kind          string
	label         string
	command       string
	argv          []string
	cwd           string
	env           []string
	timeout       time.Duration
	outputLimit   int
	policyChecked bool
	idPrefix      string
}

// startExec spawns one process job owned by the manager.
func (m *Manager) startExec(request processRequest) (*job, error) {
	command := strings.TrimSpace(request.command)
	if command == "" {
		command = strings.TrimSpace(request.label)
	}
	if command == "" {
		return nil, errors.New("jobs: command is required")
	}
	if strings.ContainsRune(command, 0) {
		return nil, errors.New("jobs: command contains a NUL byte")
	}
	for _, arg := range request.argv {
		if strings.ContainsRune(arg, 0) {
			return nil, errors.New("jobs: command argument contains a NUL byte")
		}
	}
	if m.policy != nil && m.policy.ReadOnly() {
		return nil, errors.New("jobs: sandbox policy is read-only; background jobs are disabled")
	}
	m.mu.Lock()
	if len(m.jobs) >= m.maxJobs {
		limit := m.maxJobs
		m.mu.Unlock()
		return nil, fmt.Errorf("jobs: too many retained jobs (limit %d); list and let the host clean up", limit)
	}
	m.mu.Unlock()

	workdir, err := m.resolveCwd(request.cwd)
	if err != nil {
		return nil, err
	}
	timeout := request.timeout
	if m.policy != nil {
		if !request.policyChecked && request.kind == string(background.KindBash) {
			// Validate the producer's command text with the policy's POSIX
			// parser; pwsh keeps its producer-side check because the PowerShell
			// dialect is not POSIX.
			argv, parseErr := m.policy.ParseShellCommand(command)
			if parseErr != nil {
				return nil, parseErr
			}
			if err := m.policy.CheckCommand(argv, workdir); err != nil {
				return nil, err
			}
		}
		if timeout > m.policy.Timeout() {
			timeout = m.policy.Timeout()
		}
	}

	cmd := exec.Command(request.argv[0], request.argv[1:]...)
	configureProcessGroup(cmd)
	cmd.Dir = workdir
	cmd.WaitDelay = waitDelay
	switch {
	case m.policy != nil:
		cmd.Env = m.policy.CommandEnv(os.Environ())
	case request.env != nil:
		cmd.Env = request.env
	}
	output := newOutputBuffer(m.bufferBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("jobs: start %q: %w", command, err)
	}
	prefix := request.idPrefix
	if prefix == "" {
		prefix = request.kind + "-"
	}
	current := &job{
		manager:     m,
		id:          newJobIDWithPrefix(prefix),
		kind:        request.kind,
		label:       request.label,
		command:     command,
		cmd:         cmd,
		started:     time.Now(),
		buffer:      output,
		done:        make(chan struct{}),
		exitCode:    -1,
		outputLimit: request.outputLimit,
	}
	go current.waitLoop(timeout)
	m.mu.Lock()
	m.jobs[current.id] = current
	m.mu.Unlock()
	return current, nil
}

// job is one background process or operation and its retained output.
type job struct {
	manager *Manager
	id      string
	kind    string
	label   string
	command string
	cmd     *exec.Cmd
	started time.Time
	buffer  *outputBuffer
	done    chan struct{}

	// operation jobs only
	cancelOperation func(reason string)
	operationDone   <-chan background.Outcome
	pumpCtx         context.Context
	pumpCancel      context.CancelFunc

	outputLimit int

	doneOnce sync.Once
	mu       sync.Mutex
	exited   bool
	exitCode int
	exitErr  error
	ended    time.Time
	timedOut bool
	// killRequested is set when the model (job_kill/terminal_signal) or the
	// host (CloseAll) asked to stop the job; it marks the job "stopping" and
	// suppresses its completion notice.
	killRequested bool
	killReason    string
	status        background.Status
	detail        string
	consumed      int64
	waiters       int
	teardown      bool
	dropped       bool
}

// waitLoop reaps the process, enforcing the job timeout.
func (j *job) waitLoop(timeout time.Duration) {
	waitDone := make(chan error, 1)
	go func() { waitDone <- j.cmd.Wait() }()
	if timeout <= 0 {
		j.finishProcess(<-waitDone)
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waitDone:
		j.finishProcess(err)
	case <-timer.C:
		j.mu.Lock()
		if !j.exited {
			j.timedOut = true
		}
		j.mu.Unlock()
		_ = signalProcessGroup(j.cmd, signalSpec{name: "KILL"})
		j.finishProcess(<-waitDone)
	}
}

// pumpOperation pumps every output source into the ring, watches the producer's
// outcome and settles the job.
func (j *job) pumpOperation(sources []background.Source, operation background.Operation) {
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var sourceErr error
	for _, source := range sources {
		if source == nil {
			continue
		}
		wg.Add(1)
		go func(source background.Source) {
			defer wg.Done()
			for {
				text, final, err := source.Read(j.pumpCtx)
				if text != "" {
					_, _ = j.buffer.Write([]byte(text))
				}
				if err != nil {
					errMu.Lock()
					if sourceErr == nil {
						sourceErr = err
					}
					errMu.Unlock()
					return
				}
				if final {
					return
				}
			}
		}(source)
	}

	outcome := <-j.operationDone
	// Tell the pumps to deliver any last buffered text and stop, then bound how
	// long settlement waits for a misbehaving source before closing the ring.
	if j.pumpCancel != nil {
		j.pumpCancel()
	}
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(operationDrainGrace):
	}
	errMu.Lock()
	readErr := sourceErr
	errMu.Unlock()
	if readErr != nil && outcome.Status == background.StatusCompleted {
		outcome = background.Outcome{Status: background.StatusFailed, Detail: readErr.Error()}
	}
	j.finishOperation(outcome)
}

// finishProcess records the exit state of a process job and settles it.
func (j *job) finishProcess(err error) {
	code := -1
	if err == nil {
		code = 0
	} else {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			code = exitErr.ExitCode()
		}
	}
	j.mu.Lock()
	if j.exited {
		j.mu.Unlock()
		return
	}
	j.exited = true
	j.exitCode = code
	j.exitErr = err
	j.ended = time.Now()
	status, detail := background.StatusCompleted, fmt.Sprintf("exit code: %d", code)
	switch {
	case j.timedOut:
		status, detail = background.StatusFailed, "timed out"
	case j.killRequested:
		status, detail = background.StatusKilled, killedDetail(j.killReason)
	}
	j.status = status
	j.detail = detail
	awaited := j.waiters > 0
	j.mu.Unlock()
	j.doneOnce.Do(func() { close(j.done) })
	j.manager.publishNotice(j, awaited)
}

// finishOperation records the terminal outcome of an operation job.
func (j *job) finishOperation(outcome background.Outcome) {
	status := outcome.Status
	switch status {
	case background.StatusCompleted, background.StatusKilled, background.StatusFailed:
	default:
		status = background.StatusFailed
	}
	detail := strings.TrimSpace(outcome.Detail)
	j.mu.Lock()
	if j.exited {
		j.mu.Unlock()
		return
	}
	j.exited = true
	j.ended = time.Now()
	j.status = status
	if detail != "" {
		j.detail = detail
	} else {
		j.detail = string(status)
	}
	if status == background.StatusKilled {
		j.killRequested = true
		if j.killReason != "" {
			j.detail = killedDetail(j.killReason)
		}
	}
	awaited := j.waiters > 0
	j.mu.Unlock()
	j.doneOnce.Do(func() { close(j.done) })
	j.manager.publishNotice(j, awaited)
}

// killedDetail renders the terminal detail of a canceled job.
func killedDetail(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" || reason == "teardown" {
		return "killed"
	}
	return "killed: " + reason
}

// publishNotice queues (and optionally delivers) one completion notice unless
// the settlement was awaited, model-killed, dropped or part of teardown.
func (m *Manager) publishNotice(current *job, awaited bool) {
	current.mu.Lock()
	if !current.exited || awaited || current.killRequested || current.teardown || current.dropped {
		current.mu.Unlock()
		return
	}
	notice := background.Notice{
		JobID:  current.id,
		Kind:   current.kind,
		Label:  current.label,
		Status: current.status,
		Detail: current.detail,
	}
	notice.Text = noticeText(notice)
	current.mu.Unlock()

	m.mu.Lock()
	m.notices = append(m.notices, notice)
	handler := m.noticeHandler
	m.mu.Unlock()
	if handler != nil {
		go handler(notice)
	}
}

// noticeText renders one DSH-shaped completion notice.
func noticeText(notice background.Notice) string {
	status := "[status: " + string(notice.Status)
	if notice.Detail != "" {
		status += ", " + notice.Detail
	}
	status += "]"
	label := notice.Label
	if label == "" {
		label = notice.JobID
	}
	return fmt.Sprintf("background job %s (%s: %s) finished %s. Read its output with job_output.",
		notice.JobID, notice.Kind, label, status)
}

// terminate requests cancellation of the job (process: KILL the group;
// operation: forward the reason to the producer) and marks it stopping.
func (j *job) terminate(reason string) error {
	j.mu.Lock()
	if j.exited {
		j.mu.Unlock()
		return errJobFinished
	}
	j.killRequested = true
	if j.killReason == "" {
		j.killReason = reason
	}
	cancel := j.cancelOperation
	cmd := j.cmd
	j.mu.Unlock()
	if cancel != nil {
		cancel(reason)
		return nil
	}
	return signalProcessGroup(cmd, signalSpec{name: "KILL"})
}

// signal sends one signal to the job: a real signal for process jobs, a
// producer cancellation for operation jobs (whose cancel means the tool-level
// interrupt, e.g. Ctrl-C to a terminal's foreground process group).
func (j *job) signal(spec signalSpec) error {
	j.mu.Lock()
	if j.exited {
		j.mu.Unlock()
		return errJobFinished
	}
	cancel := j.cancelOperation
	j.mu.Unlock()
	if cancel != nil {
		reason := "signal " + spec.label()
		j.mu.Lock()
		j.killRequested = true
		if j.killReason == "" {
			j.killReason = reason
		}
		j.mu.Unlock()
		cancel(reason)
		return nil
	}
	if err := signalProcessGroup(j.cmd, spec); err != nil {
		return err
	}
	j.mu.Lock()
	if !j.exited {
		j.killRequested = true
		if j.killReason == "" {
			j.killReason = "signal " + spec.label()
		}
	}
	j.mu.Unlock()
	return nil
}

// jobStatus is one immutable snapshot of a job.
type jobStatus struct {
	state    string
	status   background.Status
	detail   string
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
	status := background.StatusRunning
	switch {
	case !j.exited:
		if j.killRequested {
			status = background.StatusStopping
		}
	case j.timedOut:
		state = "timed_out"
		status = background.StatusFailed
	case j.status == background.StatusKilled:
		state = "killed"
		status = background.StatusKilled
	case j.status == background.StatusFailed:
		state = "failed"
		status = background.StatusFailed
	default:
		state = "exited"
		status = background.StatusCompleted
	}
	exitCode := -1
	if j.exited {
		exitCode = j.exitCode
	}
	detail := j.detail
	if detail == "" && !j.exited {
		detail = "running"
	}
	return jobStatus{state: state, status: status, detail: detail, exitCode: exitCode, duration: duration}
}

// dshStatus renders the DSH-style status word with detail.
func (s jobStatus) dshStatus() string {
	if s.detail == "" {
		return string(s.status)
	}
	return fmt.Sprintf("%s (%s)", s.status, s.detail)
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
	return j.capOutput(text)
}

// outputFrom returns output starting at an absolute offset.
func (j *job) outputFrom(offset int64) string {
	text, _ := j.buffer.Slice(offset)
	return j.capOutput(text)
}

// tailOutput returns the last lines and drains the new-output cursor.
func (j *job) tailOutput() string {
	text := j.buffer.LastLines(DefaultTailLines)
	j.mu.Lock()
	j.consumed = j.buffer.Total()
	j.mu.Unlock()
	return j.capOutput(text)
}

// capOutput applies the per-job model-facing output cap.
func (j *job) capOutput(text string) string {
	limit := j.modelOutputLimit()
	return toolutil.TruncateBytes(text, limit, fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", limit))
}

// modelOutputLimit is the per-response cap: the manager's cap, tightened by the
// job's producer-supplied cap and by the sandbox policy when stricter.
func (j *job) modelOutputLimit() int {
	limit := j.manager.outputLimit()
	if j.outputLimit > 0 && j.outputLimit < limit {
		limit = j.outputLimit
	}
	return limit
}

// view returns the background-contract projection of this job.
func (j *job) view() background.JobView {
	snapshot := j.snapshot()
	j.mu.Lock()
	started := j.started
	ended := j.ended
	j.mu.Unlock()
	view := background.JobView{
		ID:       j.id,
		Kind:     j.kind,
		Label:    j.label,
		Status:   snapshot.status,
		Detail:   snapshot.detail,
		ExitCode: snapshot.exitCode,
		Started:  started,
	}
	if !ended.IsZero() {
		view.Finished = ended
	}
	return view
}

// format renders the status header plus output.
func (j *job) format(output string) string {
	status := j.snapshot()
	var builder strings.Builder
	fmt.Fprintf(&builder, "job_id: %s\n", j.id)
	fmt.Fprintf(&builder, "kind: %s\n", j.kind)
	if j.label != "" && j.label != j.command {
		fmt.Fprintf(&builder, "label: %s\n", j.label)
	}
	fmt.Fprintf(&builder, "command: %s\n", j.command)
	fmt.Fprintf(&builder, "state: %s\n", status.state)
	fmt.Fprintf(&builder, "status: %s\n", status.dshStatus())
	fmt.Fprintf(&builder, "exit_code: %d\n", status.exitCode)
	fmt.Fprintf(&builder, "duration_ms: %d\n", status.duration.Milliseconds())
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

// ID implements background.Job.
func (j *job) ID() string { return j.id }

// isFinished reports whether the job already settled.
func (j *job) isFinished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.exited
}

// Wait implements background.Job: it blocks until the job settles or timeout
// elapses, registering the wait as awaited so settlement produces no notice.
func (j *job) Wait(ctx context.Context, timeout time.Duration) (background.JobView, bool, error) {
	j.mu.Lock()
	j.waiters++
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		j.waiters--
		j.mu.Unlock()
	}()
	settled, err := j.waitFor(ctx, timeout)
	return j.view(), settled, err
}

// Read implements background.Job: it returns the output produced since the
// previous consuming read plus a fresh projection.
func (j *job) Read() (string, background.JobView) {
	return j.newOutput(), j.view()
}

// Cancel implements background.Job.
func (j *job) Cancel(reason string) { _ = j.terminate(reason) }

// Drop implements background.Job: it removes a settled job the caller collected
// itself and discards any pending completion notice for it.
func (j *job) Drop() error {
	j.mu.Lock()
	if !j.exited {
		j.mu.Unlock()
		return errors.New("jobs: cannot drop a running job")
	}
	j.dropped = true
	j.mu.Unlock()
	j.manager.removeJob(j.id)
	return nil
}

// removeJob unregisters one job and drops its pending notice.
func (m *Manager) removeJob(id string) {
	m.mu.Lock()
	delete(m.jobs, id)
	notices := m.notices[:0]
	for _, notice := range m.notices {
		if notice.JobID != id {
			notices = append(notices, notice)
		}
	}
	m.notices = notices
	m.mu.Unlock()
}

// waitFor blocks until the job finishes or timeout elapses; ctx cancellation
// aborts the wait without touching the job. timeout <= 0 means a context-only
// wait.
func (j *job) waitFor(ctx context.Context, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		select {
		case <-j.done:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-j.done:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	}
}

// waitJob waits on one job while counting the wait as awaited (model-facing
// wait tools use it so a settlement they collect produces no notice).
func (m *Manager) waitJob(ctx context.Context, current *job, timeout time.Duration) (bool, error) {
	current.mu.Lock()
	current.waiters++
	current.mu.Unlock()
	defer func() {
		current.mu.Lock()
		current.waiters--
		current.mu.Unlock()
	}()
	return current.waitFor(ctx, timeout)
}

// wait is the manager-level wait used by the legacy job_wait/job_kill tools.
func (m *Manager) wait(ctx context.Context, current *job, timeout time.Duration) (bool, error) {
	return m.waitJob(ctx, current, timeout)
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
	slices.SortFunc(out, func(a, b *job) int {
		return cmp.Or(a.started.Compare(b.started), strings.Compare(a.id, b.id))
	})
	return out
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

// newJobID returns a short random job id for the legacy job_start tool.
func newJobID() string { return newJobIDWithPrefix("job-") }

// newJobIDForKind returns a DSH-style `<kind>-<hex>` id.
func newJobIDForKind(kind string) string {
	prefix := strings.TrimSpace(kind)
	if prefix == "" {
		prefix = "job"
	}
	return newJobIDWithPrefix(prefix + "-")
}

// newJobIDWithPrefix returns prefix plus six random bytes hex-encoded.
func newJobIDWithPrefix(prefix string) string { return prefix + randomHex(6) }

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	}
	return hex.EncodeToString(buf)
}
