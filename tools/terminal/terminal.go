// Package terminal provides stateful shell-session tools: terminal_open,
// terminal_send, terminal_read, terminal_signal and terminal_close.
//
// # Security
//
// The sandbox policy's command allowlist bounds which shell binary a session may
// start; once the session is running, the interactive shell accepts whatever
// terminal_send writes, so per-command allow/deny does not apply to terminal
// sessions. Use the approval Gate (or an OS-level sandbox) to bound that surface
// when the model is not trusted.
//
// # Transports
//
// A session talks to its shell over one of two transports, selected by
// WithTransport (default TransportAuto):
//
//   - PTY (default on unix): a real pseudo-terminal (/dev/ptmx + slave, the
//     child is a session leader with the slave as its controlling terminal).
//     The shell runs interactively, so prompts, line editing, job control,
//     Ctrl-C and full-screen programs work. terminal_send detects the end of a
//     command by a quiet period (no new output for DefaultQuietPeriod) instead
//     of a marker. PTY output is sanitized: ANSI escape sequences are stripped
//     (WithStripANSI, default true) and CRLF is normalized to LF.
//   - Pipe: a persistent /bin/sh with plain stdio pipes and a unique
//     `printf '__RIG_DONE_<random>__\n'` marker per send. Deterministic and
//     available everywhere; commands that consume stdin themselves (cat with no
//     file, interactive REPLs) swallow the marker, so such a send times out and
//     the session is marked desynchronized. Windows (no ConPTY in v0.5) and any
//     host where /dev/ptmx is unavailable use this transport automatically.
//
// # Completion semantics
//
// PTY sends return when the session has been quiet for the configured quiet
// period (or on timeout, or when the shell exits). A long-running, silent
// command therefore returns early with whatever output exists; use
// terminal_read with wait_ms to follow it, or terminal_signal to interrupt it.
// Pipe sends wait for the marker and report a timeout as an error.
//
// # Lifecycle
//
// A Manager owns the sessions; terminal_open starts a shell in its own session/
// process group, terminal_close (or Manager.CloseAll) kills the group, closes
// the terminal and reaps the process. Finished sessions stay registered until
// closed so terminal_read can drain their last output.
package terminal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	// DefaultTimeout bounds one terminal_send wait.
	DefaultTimeout = 30 * time.Second
	// MaxTimeout caps a per-call timeout_ms.
	MaxTimeout = 10 * time.Minute
	// DefaultMaxOutputBytes caps one tool response.
	DefaultMaxOutputBytes = 32 << 10
	// DefaultBufferBytes is the per-session output ring capacity.
	DefaultBufferBytes = 1 << 20
	// MaxSessions bounds concurrently open sessions per Manager.
	MaxSessions = 64
	// DefaultReadCount is the terminal_read page size when count is omitted.
	DefaultReadCount = 500
	// MaxReadCount caps one terminal_read page.
	MaxReadCount = 2000
	// markerPrefix is the pipe-transport completion-marker prefix; the marker
	// is unique per send so command output can never collide with it.
	markerPrefix = "__RIG_DONE_"
)

// Tool names. They are stable model-facing identifiers.
const (
	ToolTerminalOpen   = "terminal_open"
	ToolTerminalSend   = "terminal_send"
	ToolTerminalRead   = "terminal_read"
	ToolTerminalSignal = "terminal_signal"
	ToolTerminalClose  = "terminal_close"
)

// Option configures a Manager.
type Option func(*Manager)

// WithPolicy runs every session under a sandbox policy: read-only policies
// refuse to open terminals, terminal_send validates the first command against
// CheckCommand, and the shell inherits only CommandEnv.
func WithPolicy(policy *sandbox.Policy) Option {
	return func(manager *Manager) { manager.policy = policy }
}

// WithBackgroundJobs attaches the host's background-job manager, enabling
// terminal_send(run_in_background: true): the in-flight send is registered as a
// job and its output can be collected with job_output or stopped with job_kill
// (which delivers SIGINT to the terminal's foreground process group).
func WithBackgroundJobs(manager background.Manager) Option {
	return func(managerOptions *Manager) { managerOptions.background = manager }
}

// WithTimeout sets the default terminal_send wait (non-positive keeps
// DefaultTimeout).
func WithTimeout(timeout time.Duration) Option {
	return func(manager *Manager) {
		if timeout > 0 {
			manager.defaultTimeout = timeout
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

// WithBufferBytes sets the per-session output ring capacity (non-positive keeps
// DefaultBufferBytes).
func WithBufferBytes(capacity int) Option {
	return func(manager *Manager) {
		if capacity > 0 {
			manager.bufferBytes = capacity
		}
	}
}

// WithMaxSessions caps concurrently open sessions (non-positive keeps
// MaxSessions).
func WithMaxSessions(max int) Option {
	return func(manager *Manager) {
		if max > 0 {
			manager.maxSessions = max
		}
	}
}

// Manager owns terminal sessions. It is safe for concurrent use and every
// method is safe to call from multiple goroutines.
type Manager struct {
	mu sync.Mutex

	sessions map[string]*session
	policy   *sandbox.Policy

	transport  Transport
	background background.Manager

	defaultTimeout time.Duration
	maxOutputBytes int
	bufferBytes    int
	maxSessions    int
	quietPeriod    time.Duration
	stripANSI      bool
	ptyCols        int
	ptyRows        int
	ptyWarned      bool
}

// NewManager creates an empty session manager with the package defaults: PTY
// transport on unix (pipe fallback), 80x24 window, 150ms quiet period, ANSI
// stripping enabled.
func NewManager(opts ...Option) *Manager {
	manager := &Manager{
		sessions:       make(map[string]*session),
		transport:      TransportAuto,
		defaultTimeout: DefaultTimeout,
		maxOutputBytes: DefaultMaxOutputBytes,
		bufferBytes:    DefaultBufferBytes,
		maxSessions:    MaxSessions,
		quietPeriod:    DefaultQuietPeriod,
		stripANSI:      true,
		ptyCols:        DefaultPTYCols,
		ptyRows:        DefaultPTYRows,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(manager)
		}
	}
	return manager
}

// Tools returns the five terminal tools bound to manager (convenience wrapper
// around (*Manager).Tools for assembly code).
func Tools(manager *Manager) []rig.Tool {
	if manager == nil {
		return nil
	}
	return manager.Tools()
}

// Tools returns the five terminal tools bound to this manager. The tools share
// the manager's sessions; register them together with the other Rig tools.
func (m *Manager) Tools() []rig.Tool {
	return []rig.Tool{
		m.openTool(),
		m.sendTool(),
		m.readTool(),
		m.signalTool(),
		m.closeTool(),
	}
}

// Len reports the number of registered sessions (running or finished).
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// CloseAll closes and reaps every session. It is the host shutdown hook; it is
// safe to call multiple times and sessions can be opened again afterwards.
func (m *Manager) CloseAll() error {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for id, current := range m.sessions {
		sessions = append(sessions, current)
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	var errs []error
	for _, current := range sessions {
		if err := current.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// session is one persistent shell (PTY or pipe transport).
type session struct {
	manager    *Manager
	id         string
	name       string
	transport  Transport
	cmd        *exec.Cmd
	stdin      io.WriteCloser // pipe stdin writer or PTY master
	reader     io.Reader      // pipe reader or PTY master
	pipeWriter *io.PipeWriter // pipe transport only
	ptyMaster  *os.File       // PTY transport only
	filter     *ansiFilter    // PTY only, nil when stripping is disabled
	cwd        string         // initial working directory for sandbox checks

	sendMu sync.Mutex // serializes command writes / completion waits

	mu                sync.Mutex
	notify            chan struct{}
	buffer            *outputBuffer
	consumed          int64
	currentMarker     string
	commandDone       bool
	postMarkerNewline bool
	desynced          bool
	exited            bool
	exitCode          int
	exitErr           error
	closed            bool

	done     chan struct{}
	doneOnce sync.Once
}

// ptyStart is startPTYCommand behind a variable so tests can force a PTY
// failure and exercise the pipe fallback path (production code never reassigns
// it).
var ptyStart = startPTYCommand

// open starts one persistent shell and registers it. It returns the session id,
// the transport actually used, and a warning when a requested PTY was
// unavailable and the pipe fallback was used (reported once per manager).
func (m *Manager) open(cwd, shell, name, sessionType string) (string, Transport, string, error) {
	if runtime.GOOS == "windows" {
		return "", TransportPipe, "", errors.New("terminal: interactive terminal sessions require a unix PTY; Windows ConPTY support is not implemented in this release")
	}
	if m.policy != nil && m.policy.ReadOnly() {
		return "", TransportPipe, "", errors.New("terminal: sandbox policy is read-only; terminals are disabled")
	}
	if len(m.sessionsSlice()) >= m.maxSessions {
		return "", TransportPipe, "", fmt.Errorf("terminal: too many open sessions (limit %d)", m.maxSessions)
	}

	name = strings.TrimSpace(name)
	if name != "" {
		if err := m.checkName(name); err != nil {
			return "", TransportPipe, "", err
		}
	}
	requestedTransport := m.transport
	if requested := strings.ToLower(strings.TrimSpace(sessionType)); requested != "" {
		switch requested {
		case "shell", "auto":
			requestedTransport = m.transport
		case "pty":
			requestedTransport = TransportPTY
		case "pipe":
			requestedTransport = TransportPipe
		default:
			return "", TransportPipe, "", fmt.Errorf("terminal: unknown terminal type %q (available: shell, pty, pipe)", sessionType)
		}
	}
	shellPath := strings.TrimSpace(shell)
	if shellPath == "" {
		shellPath = "/bin/sh"
	}
	if !filepath.IsAbs(shellPath) {
		resolved, err := exec.LookPath(shellPath)
		if err != nil {
			return "", TransportPipe, "", fmt.Errorf("terminal: shell %q: %w", shellPath, err)
		}
		shellPath = resolved
	}
	workdir, err := m.resolveCwd(cwd)
	if err != nil {
		return "", TransportPipe, "", err
	}
	if m.policy != nil {
		// The allowlist bounds which shell binary a session may start. Check the
		// bare basename: the policy rejects path forms by design.
		if err := m.policy.CheckCommand([]string{filepath.Base(shellPath)}, workdir); err != nil {
			return "", TransportPipe, "", fmt.Errorf("terminal: shell %q: %w", shellPath, err)
		}
	}

	usePTY := false
	switch requestedTransport {
	case TransportPipe:
		usePTY = false
	case TransportPTY:
		usePTY = true
	default:
		usePTY = ptySupported()
	}

	var warning string
	if usePTY {
		master, command, ptyErr := ptyStart(shellPath, workdir, m.shellEnv(), m.ptyCols, m.ptyRows)
		if ptyErr != nil {
			warning = m.notePTYFallback(ptyErr)
		} else {
			current := &session{
				manager:   m,
				id:        newID("term-"),
				name:      name,
				transport: TransportPTY,
				cmd:       command,
				stdin:     master,
				reader:    master,
				ptyMaster: master,
				cwd:       workdir,
				notify:    make(chan struct{}),
				buffer:    newOutputBuffer(m.bufferBytes),
				done:      make(chan struct{}),
			}
			if m.stripANSI {
				current.filter = &ansiFilter{}
			}
			go current.readLoop(master)
			go current.waitLoop()
			if err := m.register(current); err != nil {
				_ = current.close()
				return "", TransportPTY, warning, err
			}
			return current.id, TransportPTY, warning, nil
		}
	}

	current, err := m.openPipeSession(shellPath, workdir, name)
	if err != nil {
		return "", TransportPipe, warning, err
	}
	if err := m.register(current); err != nil {
		_ = current.close()
		return "", TransportPipe, warning, err
	}
	return current.id, TransportPipe, warning, nil
}

// openPipeSession starts the deterministic marker-based pipe transport.
func (m *Manager) openPipeSession(shellPath, workdir, name string) (*session, error) {
	command := exec.Command(shellPath)
	configureProcessGroup(command)
	command.Dir = workdir
	if m.policy != nil {
		command.Env = m.policy.CommandEnv(os.Environ())
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("terminal: shell stdin: %w", err)
	}
	reader, writer := io.Pipe()
	command.Stdout = writer
	command.Stderr = writer
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = writer.Close()
		_ = reader.Close()
		return nil, fmt.Errorf("terminal: start shell %q: %w", shellPath, err)
	}
	current := &session{
		manager:    m,
		id:         newID("term-"),
		name:       name,
		transport:  TransportPipe,
		cmd:        command,
		stdin:      stdin,
		reader:     reader,
		pipeWriter: writer,
		cwd:        workdir,
		notify:     make(chan struct{}),
		buffer:     newOutputBuffer(m.bufferBytes),
		done:       make(chan struct{}),
	}
	go current.readLoop(reader)
	go current.waitLoop()
	return current, nil
}

// shellEnv is the environment for the shell process (PTY mode).
func (m *Manager) shellEnv() []string {
	if m.policy != nil {
		return m.policy.CommandEnv(os.Environ())
	}
	return os.Environ()
}

// notePTYFallback records that the PTY transport failed and returns the
// one-time warning text (empty after the first report).
func (m *Manager) notePTYFallback(err error) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ptyWarned {
		return ""
	}
	m.ptyWarned = true
	return fmt.Sprintf("warning: PTY transport unavailable (%v); using pipe transport", err)
}

// register adds a started session to the manager.
func (m *Manager) register(current *session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) >= m.maxSessions {
		return fmt.Errorf("terminal: too many open sessions (limit %d)", m.maxSessions)
	}
	if _, exists := m.sessions[current.id]; exists {
		return errors.New("terminal: session id collision")
	}
	m.sessions[current.id] = current
	return nil
}

// sessionsSlice snapshots registered sessions (used for the capacity check).
func (m *Manager) sessionsSlice() []*session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*session, 0, len(m.sessions))
	for _, current := range m.sessions {
		out = append(out, current)
	}
	return out
}

// lookup returns one registered session.
func (m *Manager) lookup(id string) (*session, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("terminal: session_id is required")
	}
	m.mu.Lock()
	current, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("terminal: unknown session %q", id)
	}
	return current, nil
}

// remove unregisters a session.
func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// send writes one payload and waits for the transport's completion rule,
// returning the output produced since the send started. submit appends Enter
// (and the pipe marker); submit=false writes raw text for control characters or
// incomplete REPL input.
func (s *session) send(ctx context.Context, input string, submit bool, timeout time.Duration) (output string, timedOut bool, err error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	start, err := s.startSend(input, submit)
	if err != nil {
		return "", false, err
	}
	return s.waitSend(ctx, start, submit, timeout)
}

// startSend validates the session state and writes one payload. It returns the
// absolute buffer offset at which the send's output starts.
func (s *session) startSend(input string, submit bool) (int64, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, errors.New("terminal: session is closed")
	}
	if s.desynced {
		s.mu.Unlock()
		return 0, errors.New("terminal: session is desynchronized after a timeout; close and reopen the terminal")
	}
	if s.exited {
		code := s.exitCode
		s.mu.Unlock()
		return 0, fmt.Errorf("terminal: shell has exited (exit code %d); close and reopen the terminal", code)
	}
	start := s.buffer.Total()
	if s.transport == TransportPipe && submit {
		marker := markerPrefix + randomHex(12) + "__"
		s.currentMarker = marker
		s.commandDone = false
		s.postMarkerNewline = false
		s.mu.Unlock()
		payload := input
		if !strings.HasSuffix(payload, "\n") {
			payload += "\n"
		}
		payload += "printf '" + marker + "\\n'\n"
		if _, writeErr := io.WriteString(s.stdin, payload); writeErr != nil {
			s.markExited(-1, writeErr)
			return 0, fmt.Errorf("terminal: write to shell: %w", writeErr)
		}
		return start, nil
	}
	s.mu.Unlock()
	line := input
	if submit && !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := io.WriteString(s.stdin, line); err != nil {
		s.markExited(-1, err)
		return 0, fmt.Errorf("terminal: write to pty: %w", err)
	}
	return start, nil
}

// waitSend waits for the transport's completion rule and returns the output
// produced since start. timeout <= 0 means no deadline (background sends).
func (s *session) waitSend(ctx context.Context, start int64, submit bool, timeout time.Duration) (string, bool, error) {
	if s.transport == TransportPipe && submit {
		return s.waitSendPipe(ctx, start, timeout)
	}
	return s.waitSendPTY(ctx, start, submit, timeout)
}

// sendTimeoutDeadline returns the wait deadline for one send (zero = none).
func sendTimeoutDeadline(timeout time.Duration) time.Time {
	if timeout <= 0 {
		return time.Time{}
	}
	return time.Now().Add(timeout)
}

// waitSendPipe waits for the completion marker (pipe transport).
func (s *session) waitSendPipe(ctx context.Context, start int64, timeout time.Duration) (output string, timedOut bool, err error) {
	deadline := sendTimeoutDeadline(timeout)
	done, waitErr := s.waitFor(ctx, deadline, func() bool { return s.commandDone || s.exited })
	if waitErr != nil {
		return "", false, waitErr
	}
	if done {
		output = s.takeOutput(start)
		s.mu.Lock()
		exited := s.exited
		code := s.exitCode
		s.mu.Unlock()
		if exited && !strings.Contains(output, "shell exited") {
			output += fmt.Sprintf("\n[terminal: shell exited with code %d]", code)
		}
		return output, false, nil
	}

	// Timeout: the marker may arrive later (the command may still be running)
	// or never (a command that consumed stdin swallowed it). Either way the
	// command/output pairing is no longer trustworthy, so the session is marked
	// desynchronized and must be closed and reopened.
	s.mu.Lock()
	if !s.commandDone && !s.exited {
		s.desynced = true
	}
	s.mu.Unlock()
	output = s.takeOutput(start)
	notice := fmt.Sprintf("\n[timeout: command did not finish within %s]", timeout)
	notice += "\n[terminal: session desynchronized after the timeout; run terminal_close and terminal_open again]"
	return output + notice, true, nil
}

// waitSendPTY waits for the terminal to be quiet for the configured quiet
// period (or the timeout elapses, or the shell exits).
func (s *session) waitSendPTY(ctx context.Context, start int64, submit bool, timeout time.Duration) (string, bool, error) {
	quiet := s.manager.quietPeriod
	if quiet <= 0 {
		quiet = DefaultQuietPeriod
	}
	deadline := sendTimeoutDeadline(timeout)
	lastTotal := start
	lastData := time.Now()
	exited := false

	for {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			break
		}
		s.mu.Lock()
		closed := s.closed
		exited = s.exited
		s.mu.Unlock()
		if closed {
			return "", false, errors.New("terminal: session is closed")
		}
		if exited {
			break
		}
		next := lastData.Add(quiet)
		if !deadline.IsZero() && deadline.Before(next) {
			next = deadline
		}
		ready, waitErr := s.waitFor(ctx, next, func() bool {
			return s.buffer.Total() > lastTotal || s.exited
		})
		if waitErr != nil {
			return "", false, waitErr
		}
		if total := s.buffer.Total(); total > lastTotal {
			lastTotal = total
			lastData = time.Now()
			if ready {
				continue
			}
		}
		if !ready {
			// Quiet period or overall deadline elapsed.
			if !deadline.IsZero() && !time.Now().Before(deadline) {
				break
			}
			if time.Since(lastData) >= quiet {
				break
			}
		}
	}

	output := s.takeOutput(start)
	s.mu.Lock()
	exited = s.exited
	code := s.exitCode
	s.mu.Unlock()
	if exited {
		output += fmt.Sprintf("\n[terminal: shell exited with code %d]", code)
		return output, false, nil
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		output += fmt.Sprintf("\n[timeout: no quiet period within %s; the command may still be running]", timeout)
		return output, true, nil
	}
	return output, false, nil
}

// --- background sends (terminal_send run_in_background) --------------------

// startBackgroundSend starts one send as a managed background job and returns
// the job id. The send holds the session's one-send reservation until it
// settles; a second send fails until then.
func (m *Manager) startBackgroundSend(current *session, input string, submit bool) (string, error) {
	if m.background == nil {
		return "", errors.New("terminal: background sends require a background job manager")
	}
	label := current.id + ": " + input
	if len(label) > 120 {
		label = label[:120] + "…"
	}
	var operation *sendOperation
	source := &sendSource{bind: func() *sendOperation { return operation }}
	job, err := m.background.StartOperation(background.OperationSpec{
		Kind:             string(background.KindPTYSend),
		Label:            label,
		OutputLimitBytes: m.outputLimit(),
		Sources:          []background.Source{source},
		Run: func() (background.Operation, error) {
			started, startErr := current.beginBackgroundSend(input, submit)
			if startErr != nil {
				return nil, startErr
			}
			operation = started
			return started, nil
		},
	})
	if err != nil {
		return "", err
	}
	return job.ID(), nil
}

// beginBackgroundSend reserves the session's single send slot and starts the
// send in the background.
func (s *session) beginBackgroundSend(input string, submit bool) (*sendOperation, error) {
	if !s.sendMu.TryLock() {
		return nil, errors.New("terminal: another send is already active on this session")
	}
	start, err := s.startSend(input, submit)
	if err != nil {
		s.sendMu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	operation := &sendOperation{
		session:  s,
		start:    start,
		cursor:   start,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan background.Outcome, 1),
		finished: make(chan struct{}),
	}
	go func() {
		defer s.sendMu.Unlock()
		_, timedOut, sendErr := s.waitSend(ctx, start, submit, 0)
		operation.settle(s.backgroundOutcome(ctx, timedOut, sendErr))
	}()
	return operation, nil
}

// backgroundOutcome maps one settled send onto the DSH job outcome vocabulary.
func (s *session) backgroundOutcome(ctx context.Context, timedOut bool, sendErr error) background.Outcome {
	s.mu.Lock()
	exited, code := s.exited, s.exitCode
	s.mu.Unlock()
	switch {
	case sendErr != nil && ctx.Err() != nil:
		return background.Outcome{Status: background.StatusKilled, Detail: "signal: SIGINT"}
	case sendErr != nil:
		return background.Outcome{Status: background.StatusFailed, Detail: sendErr.Error()}
	case timedOut:
		return background.Outcome{Status: background.StatusFailed, Detail: "timed out"}
	case exited:
		return background.Outcome{Status: background.StatusCompleted, Detail: fmt.Sprintf("session exited: %d", code)}
	default:
		return background.Outcome{Status: background.StatusCompleted, Detail: "wait: inferred_idle"}
	}
}

// sendOperation is one in-flight background send.
type sendOperation struct {
	session *session
	start   int64
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan background.Outcome
	// finished is closed after the outcome is queued, so output sources wake.
	finished chan struct{}

	mu           sync.Mutex
	cursor       int64
	settled      bool
	canceled     bool
	cancelReason string
}

// Cancel implements background.Operation: it delivers SIGINT to the terminal's
// foreground process group (Ctrl-C on a PTY) and aborts the wait.
func (op *sendOperation) Cancel(reason string) {
	op.mu.Lock()
	if op.canceled {
		op.mu.Unlock()
		return
	}
	op.canceled = true
	op.cancelReason = reason
	op.mu.Unlock()
	_ = op.session.interrupt()
	op.cancel()
}

// Done implements background.Operation.
func (op *sendOperation) Done() <-chan background.Outcome { return op.done }

// settle records the terminal outcome, queues it and wakes output sources.
func (op *sendOperation) settle(outcome background.Outcome) {
	op.mu.Lock()
	if op.settled {
		op.mu.Unlock()
		return
	}
	op.settled = true
	reason := op.cancelReason
	op.mu.Unlock()
	if outcome.Status == background.StatusKilled && strings.TrimSpace(reason) != "" {
		outcome.Detail = "signal: SIGINT (" + reason + ")"
	}
	op.done <- outcome
	close(op.finished)
}

// takeBuffered returns the output produced since the previous delivery, and
// advances the session's consuming cursor so terminal_read does not repeat it.
func (op *sendOperation) takeBuffered() (string, bool) {
	op.mu.Lock()
	from := op.cursor
	op.mu.Unlock()
	total := op.session.buffer.Total()
	if total <= from {
		return "", false
	}
	text, _ := op.session.buffer.Slice(from)
	op.mu.Lock()
	op.cursor = total
	op.mu.Unlock()
	op.session.mu.Lock()
	op.session.consumed = total
	op.session.mu.Unlock()
	return text, true
}

// sendSource adapts one in-flight background send to the job manager's pull
// source contract. It binds lazily because the operation starts inside the
// job's starter, after admission.
type sendSource struct {
	bind func() *sendOperation
}

// Read implements background.Source.
func (src *sendSource) Read(ctx context.Context) (string, bool, error) {
	operation := src.bind()
	if operation == nil {
		return "", true, nil
	}
	for {
		if text, ok := operation.takeBuffered(); ok {
			return text, false, nil
		}
		if text, ok := operation.drain(); ok {
			return text, false, nil
		}
		if operation.isFinished() {
			return "", true, nil
		}
		notify := operation.session.notifyChan()
		select {
		case <-notify:
		case <-operation.finished:
		case <-ctx.Done():
			if text, ok := operation.drain(); ok {
				return text, true, nil
			}
			return "", true, nil
		}
	}
}

// drain returns any output that arrived after the last takeBuffered call and
// reports whether the operation has settled.
func (op *sendOperation) drain() (string, bool) {
	op.mu.Lock()
	from := op.cursor
	op.mu.Unlock()
	total := op.session.buffer.Total()
	if total <= from {
		return "", false
	}
	text, _ := op.session.buffer.Slice(from)
	op.mu.Lock()
	op.cursor = total
	op.mu.Unlock()
	op.session.mu.Lock()
	op.session.consumed = total
	op.session.mu.Unlock()
	return text, true
}

// isFinished reports whether the operation settled.
func (op *sendOperation) isFinished() bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.settled
}

// interrupt delivers SIGINT to the terminal's foreground process group: Ctrl-C
// (0x03) on a PTY, or a real SIGINT to the shell's process group otherwise.
func (s *session) interrupt() error {
	return s.signal(signalSpec{name: "INT"})
}

// notifyChan returns the session's current wake channel.
func (s *session) notifyChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notify
}

// --- paginated retained-output reads --------------------------------------

// readPage returns one newest-relative page of retained lines:
// offset is the newest-relative line offset of the page (0 = newest page) and
// count its maximum line count.
func (s *session) readPage(offset, count int) (text string, total, lineBegin, lineEnd int, truncated bool) {
	data, dropped := s.buffer.Snapshot()
	lines := splitRetainedLines(data)
	total = len(lines)
	if offset < 0 {
		offset = 0
	}
	if count <= 0 {
		count = DefaultReadCount
	}
	count = min(count, MaxReadCount)
	if offset >= total {
		return "", total, offset, offset, dropped
	}
	end := total - offset
	start := end - count
	if start < 0 {
		start = 0
	}
	text = strings.Join(lines[start:end], "\n")
	return text, total, offset, offset + (end - start), dropped || start > 0
}

// splitRetainedLines splits retained output into lines, dropping the trailing
// empty element a final newline produces.
func splitRetainedLines(data string) []string {
	if data == "" {
		return nil
	}
	lines := strings.Split(data, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// checkName rejects a duplicate live owner-local session name.
func (m *Manager) checkName(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, current := range m.sessions {
		current.mu.Lock()
		existing := current.name
		closed := current.closed
		current.mu.Unlock()
		if !closed && existing == name {
			return fmt.Errorf("terminal: session name %q is already in use", name)
		}
	}
	return nil
}

// read returns output not yet consumed by a previous read/send, optionally
// waiting up to wait for new output.
func (s *session) read(ctx context.Context, wait time.Duration) (string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("terminal: session is closed")
	}
	after := s.consumed
	s.mu.Unlock()

	deadline := time.Now()
	if wait > 0 {
		deadline = time.Now().Add(wait)
	}
	if _, err := s.waitFor(ctx, deadline, func() bool {
		return s.buffer.Total() > after || s.exited
	}); err != nil {
		return "", err
	}
	return s.takeOutput(after), nil
}

// signal sends one signal to the session. On a PTY, an interrupt is delivered
// as Ctrl-C (0x03) so the terminal's foreground process group receives SIGINT;
// other signals go to the shell's process group.
func (s *session) signal(spec signalSpec) error {
	s.mu.Lock()
	exited := s.exited
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("terminal: session is closed")
	}
	if exited {
		return errors.New("terminal: shell has exited")
	}
	if s.transport == TransportPTY && isInterruptSignal(spec) {
		if _, err := s.stdin.Write([]byte{0x03}); err != nil {
			return fmt.Errorf("terminal: write Ctrl-C: %w", err)
		}
		return nil
	}
	return signalProcessGroup(s.cmd, spec)
}

// isInterruptSignal reports whether spec denotes SIGINT (or the Ctrl-C
// shorthand the tool exposes).
func isInterruptSignal(spec signalSpec) bool {
	if spec.name == "INT" {
		return true
	}
	return spec.name == "" && spec.number == 2
}

// close kills the process group, closes the terminal and reaps the shell. It is
// idempotent.
func (s *session) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	exited := s.exited
	s.mu.Unlock()

	_ = s.stdin.Close()
	if !exited {
		_ = signalProcessGroup(s.cmd, signalSpec{name: "KILL"})
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	if s.pipeWriter != nil {
		_ = s.pipeWriter.Close()
	}
	if s.ptyMaster != nil {
		_ = s.ptyMaster.Close()
	}
	return nil
}

// takeOutput returns output from absolute offset from, caps it, and advances
// the consumed cursor.
func (s *session) takeOutput(from int64) string {
	text, _ := s.buffer.Slice(from)
	s.mu.Lock()
	s.consumed = s.buffer.Total()
	s.mu.Unlock()
	return toolutil.TruncateBytes(text, s.manager.outputLimit(), fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", s.manager.outputLimit()))
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

// waitFor waits until ready (called with the session lock held) or the deadline
// expires. It returns false, nil on timeout and a context error on cancel.
func (s *session) waitFor(ctx context.Context, deadline time.Time, ready func() bool) (bool, error) {
	for {
		s.mu.Lock()
		if ready() {
			s.mu.Unlock()
			return true, nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			s.mu.Unlock()
			return false, nil
		}
		notify := s.notify
		var wait time.Duration
		if !deadline.IsZero() {
			wait = time.Until(deadline)
		}
		s.mu.Unlock()

		var timer *time.Timer
		var timerC <-chan time.Time
		if !deadline.IsZero() {
			if wait < 0 {
				wait = 0
			}
			timer = time.NewTimer(wait)
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return false, ctx.Err()
		case <-notify:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
		}
	}
}

// wakeLocked wakes every waiter. The caller holds s.mu.
func (s *session) wakeLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}

// appendOutput appends bytes to the session buffer, stripping the marker line's
// trailing newline when it arrives in a later chunk (pipe transport).
func (s *session) appendOutput(data []byte) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.postMarkerNewline {
		switch {
		case data[0] == '\n':
			data = data[1:]
			s.postMarkerNewline = false
		case len(data) >= 2 && data[0] == '\r' && data[1] == '\n':
			data = data[2:]
			s.postMarkerNewline = false
		case len(data) == 1 && data[0] == '\r':
			data = nil
			s.postMarkerNewline = false
		default:
			s.postMarkerNewline = false
		}
	}
	if len(data) > 0 {
		_, _ = s.buffer.Write(data)
	}
	s.wakeLocked()
}

// readLoop reads the shell output (pipe or PTY master), sanitizes/handles it and
// appends it to the output buffer.
func (s *session) readLoop(reader io.Reader) {
	var pending []byte
	chunk := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(chunk)
		if n > 0 {
			if s.transport == TransportPTY {
				data := chunk[:n]
				if s.filter != nil {
					data = s.filter.Feed(data)
				}
				if len(data) > 0 {
					s.appendOutput(data)
				}
			} else {
				pending = s.consumeOutput(chunk[:n], pending)
			}
		}
		if readErr != nil {
			break
		}
	}
	if len(pending) > 0 {
		s.appendOutput(pending)
	}
	// The pipe/pty reached EOF (the shell is gone or its terminal closed). Exit
	// state is owned by waitLoop, which has the real exit code; wake waiters so
	// a blocked send/read observes it as soon as markExited runs.
	s.mu.Lock()
	s.wakeLocked()
	s.mu.Unlock()
}

// consumeOutput scans one chunk (plus pending partial marker bytes) for the
// current completion marker (pipe transport only).
func (s *session) consumeOutput(chunk, pending []byte) []byte {
	data := make([]byte, 0, len(pending)+len(chunk))
	data = append(data, pending...)
	data = append(data, chunk...)
	for {
		s.mu.Lock()
		marker := s.currentMarker
		s.mu.Unlock()
		if marker == "" {
			s.appendOutput(data)
			return nil
		}
		markerBytes := []byte(marker)
		if before, rest, found := bytes.Cut(data, markerBytes); found {
			s.appendOutput(before)
			s.mu.Lock()
			s.commandDone = true
			s.currentMarker = ""
			s.postMarkerNewline = true
			s.wakeLocked()
			s.mu.Unlock()
			if len(rest) == 0 {
				return nil
			}
			data = rest
			continue
		}
		keep := len(markerBytes) - 1
		if keep < 0 {
			keep = 0
		}
		if len(data) > keep {
			flush := len(data) - keep
			s.appendOutput(data[:flush])
			return slices.Clone(data[flush:])
		}
		return slices.Clone(data)
	}
}

// waitLoop reaps the shell process and records its exit status.
func (s *session) waitLoop() {
	waitErr := s.cmd.Wait()
	code := -1
	if waitErr == nil {
		code = 0
	} else {
		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			code = exitErr.ExitCode()
		}
	}
	if s.pipeWriter != nil {
		_ = s.pipeWriter.Close() // EOF for the pipe reader
	}
	s.markExited(code, waitErr)
}

// markExited records the exit state and wakes waiters. It is idempotent.
func (s *session) markExited(code int, err error) {
	s.mu.Lock()
	if !s.exited {
		s.exited = true
		s.exitCode = code
		s.exitErr = err
		s.commandDone = true
		s.currentMarker = ""
		s.wakeLocked()
	}
	s.mu.Unlock()
	s.doneOnce.Do(func() { close(s.done) })
}

// resolveCwd validates the terminal's working directory. With a policy the
// directory must be inside a root (and is returned symlink-resolved); without
// one it must simply exist.
func (m *Manager) resolveCwd(cwd string) (string, error) {
	if m.policy != nil {
		requested := strings.TrimSpace(cwd)
		if requested == "" {
			requested = "."
		}
		abs, err := m.policy.ResolvePath(requested)
		if err != nil {
			return "", fmt.Errorf("terminal: cwd: %w", err)
		}
		info, statErr := os.Stat(abs)
		if statErr != nil {
			return "", fmt.Errorf("terminal: cwd: %w", statErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("terminal: cwd %q is not a directory", cwd)
		}
		return abs, nil
	}
	dir := strings.TrimSpace(cwd)
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("terminal: cwd: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("terminal: cwd: %w", err)
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return "", fmt.Errorf("terminal: cwd: %w", statErr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("terminal: cwd %q is not a directory", cwd)
	}
	return abs, nil
}

// checkCommand validates one input line against the sandbox command policy:
// the line must not contain shell operators or substitutions, and its first
// command must be allow-listed with a cwd inside the policy roots. The
// interactive shell is therefore bounded per line; shell builtins (cd, export,
// ...) need to be allow-listed explicitly when a policy is configured.
func (m *Manager) checkCommand(input, cwd string) error {
	if m.policy == nil {
		return nil
	}
	argv, err := m.policy.ParseShellCommand(input)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return nil
	}
	return m.policy.CheckCommand(argv, cwd)
}

// newID returns a short random identifier with the given prefix.
func newID(prefix string) string { return prefix + randomHex(6) }

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000")))
	}
	return hex.EncodeToString(buf)
}
