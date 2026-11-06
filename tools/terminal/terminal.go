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

	transport Transport

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
func (m *Manager) open(cwd, shell string) (string, Transport, string, error) {
	if runtime.GOOS == "windows" {
		return "", TransportPipe, "", errors.New("terminal: interactive terminal sessions require a unix PTY; Windows ConPTY support is not implemented in this release")
	}
	if m.policy != nil && m.policy.ReadOnly() {
		return "", TransportPipe, "", errors.New("terminal: sandbox policy is read-only; terminals are disabled")
	}
	if len(m.sessionsSlice()) >= m.maxSessions {
		return "", TransportPipe, "", fmt.Errorf("terminal: too many open sessions (limit %d)", m.maxSessions)
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
	switch m.transport {
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

	current, err := m.openPipeSession(shellPath, workdir)
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
func (m *Manager) openPipeSession(shellPath, workdir string) (*session, error) {
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

// send writes one command and waits for completion, using the transport's
// completion rule.
func (s *session) send(ctx context.Context, input string, timeout time.Duration) (output string, timedOut bool, err error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.transport == TransportPTY {
		return s.sendPTY(ctx, input, timeout)
	}
	return s.sendPipe(ctx, input, timeout)
}

// sendPipe writes one command and waits for its completion marker.
func (s *session) sendPipe(ctx context.Context, input string, timeout time.Duration) (output string, timedOut bool, err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", false, errors.New("terminal: session is closed")
	}
	if s.desynced {
		s.mu.Unlock()
		return "", false, errors.New("terminal: session is desynchronized after a timeout; close and reopen the terminal")
	}
	if s.exited {
		code := s.exitCode
		s.mu.Unlock()
		return "", false, fmt.Errorf("terminal: shell has exited (exit code %d); close and reopen the terminal", code)
	}
	start := s.buffer.Total()
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
		return "", false, fmt.Errorf("terminal: write to shell: %w", writeErr)
	}

	deadline := time.Now().Add(timeout)
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

// sendPTY writes one command and returns when the terminal has been quiet for
// the configured quiet period (or the timeout elapses). No marker is used: the
// PTY shell echoes input and prints prompts, so "no new output" is the
// completion signal. A silent long-running command returns early with whatever
// output it has produced.
func (s *session) sendPTY(ctx context.Context, input string, timeout time.Duration) (string, bool, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", false, errors.New("terminal: session is closed")
	}
	if s.exited {
		code := s.exitCode
		s.mu.Unlock()
		return "", false, fmt.Errorf("terminal: shell has exited (exit code %d); close and reopen the terminal", code)
	}
	start := s.buffer.Total()
	s.mu.Unlock()

	line := input
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := io.WriteString(s.stdin, line); err != nil {
		s.markExited(-1, err)
		return "", false, fmt.Errorf("terminal: write to pty: %w", err)
	}

	quiet := s.manager.quietPeriod
	if quiet <= 0 {
		quiet = DefaultQuietPeriod
	}
	deadline := time.Now().Add(timeout)
	lastTotal := start
	lastData := time.Now()
	exited := false

	for {
		if !time.Now().Before(deadline) {
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
		if deadline.Before(next) {
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
			if !time.Now().Before(deadline) {
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
	if !time.Now().Before(deadline) {
		output += fmt.Sprintf("\n[timeout: no quiet period within %s; the command may still be running]", timeout)
		return output, true, nil
	}
	return output, false, nil
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
		if index := bytes.Index(data, markerBytes); index >= 0 {
			s.appendOutput(data[:index])
			rest := data[index+len(markerBytes):]
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
			return append([]byte(nil), data[flush:]...)
		}
		return append([]byte(nil), data...)
	}
}

// waitLoop reaps the shell process and records its exit status.
func (s *session) waitLoop() {
	waitErr := s.cmd.Wait()
	code := -1
	if waitErr == nil {
		code = 0
	} else {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
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
	if err := m.policy.CheckShellCommand(input); err != nil {
		return err
	}
	argv, err := splitShellCommand(input)
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
