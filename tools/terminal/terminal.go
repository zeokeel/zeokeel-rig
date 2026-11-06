// Package terminal provides stateful shell-session tools: terminal_open,
// terminal_send, terminal_read, terminal_signal and terminal_close.
//
// # Not a PTY
//
// Sessions are persistent /bin/sh processes with plain stdio pipes, not a
// pseudo-terminal: there is no terminal emulation, no job control and no
// interactive program support (a program that puts its stdin in raw mode, asks
// for a password, paints a TUI or waits for keyboard escape sequences will not
// work as it does in a real terminal). A PTY-backed implementation is a future
// addition; the tool names and Manager API are designed to survive it.
//
// # Completion markers
//
// terminal_send writes the command text followed by a unique
// `printf '__RIG_DONE_<random>__\n'` line and waits until that marker appears in
// the merged stdout+stderr stream. The marker line is removed from the returned
// output. This gives command completion without a PTY prompt, but it has the
// documented consequence: a command that consumes stdin itself (for example
// `cat` with no file, or an interactive REPL) will swallow the marker line and
// the call will time out. The timeout text says so, and a desynchronized
// session must be closed and reopened.
//
// # Lifecycle
//
// A Manager owns the sessions; terminal_open starts a shell in its own process
// group, terminal_close (or Manager.CloseAll) kills the group and reaps the
// process. Finished sessions stay registered until closed so terminal_read can
// drain their last output.

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
	"strings"
	"sync"
	"time"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// Defaults and limits. They are exported so hosts and tests can assert the
// documented behavior.
const (
	// DefaultTimeout bounds one terminal_send waiting for its marker.
	DefaultTimeout = 30 * time.Second
	// MaxTimeout caps a per-call timeout_ms.
	MaxTimeout = 10 * time.Minute
	// DefaultMaxOutputBytes caps one tool response.
	DefaultMaxOutputBytes = 32 << 10
	// DefaultBufferBytes is the per-session output ring capacity.
	DefaultBufferBytes = 1 << 20
	// MaxSessions bounds concurrently open sessions per Manager.
	MaxSessions = 64
	// markerPrefix is the completion-marker prefix; the marker is unique per
	// send so command output can never collide with it by accident.
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

// WithTimeout sets the default terminal_send marker wait (non-positive keeps
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

	defaultTimeout time.Duration
	maxOutputBytes int
	bufferBytes    int
	maxSessions    int
}

// NewManager creates an empty session manager with the package defaults.
func NewManager(opts ...Option) *Manager {
	manager := &Manager{
		sessions:       make(map[string]*session),
		defaultTimeout: DefaultTimeout,
		maxOutputBytes: DefaultMaxOutputBytes,
		bufferBytes:    DefaultBufferBytes,
		maxSessions:    MaxSessions,
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
func Tools(manager *Manager) []v1.Tool {
	if manager == nil {
		return nil
	}
	return manager.Tools()
}

// Tools returns the five terminal tools bound to this manager. The tools share
// the manager's sessions; register them together with the other Rig tools.
func (m *Manager) Tools() []v1.Tool {
	return []v1.Tool{
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

// session is one persistent shell.
type session struct {
	manager *Manager
	id      string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *io.PipeWriter
	cwd     string // initial working directory; used for sandbox command checks

	sendMu sync.Mutex // serializes command writes / marker waits

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

// open starts one persistent shell and registers it.
func (m *Manager) open(cwd, shell string) (string, error) {
	if m.policy != nil && m.policy.ReadOnly() {
		return "", errors.New("terminal: sandbox policy is read-only; terminals are disabled")
	}
	if len(m.sessionsSlice()) >= m.maxSessions {
		return "", fmt.Errorf("terminal: too many open sessions (limit %d)", m.maxSessions)
	}

	shellPath := strings.TrimSpace(shell)
	if shellPath == "" {
		shellPath = "/bin/sh"
	}
	if !filepath.IsAbs(shellPath) {
		resolved, err := exec.LookPath(shellPath)
		if err != nil {
			return "", fmt.Errorf("terminal: shell %q: %w", shellPath, err)
		}
		shellPath = resolved
	}
	workdir, err := m.resolveCwd(cwd)
	if err != nil {
		return "", err
	}

	cmd := exec.Command(shellPath)
	configureProcessGroup(cmd)
	cmd.Dir = workdir
	if m.policy != nil {
		cmd.Env = m.policy.CommandEnv(os.Environ())
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("terminal: shell stdin: %w", err)
	}
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = writer.Close()
		_ = reader.Close()
		return "", fmt.Errorf("terminal: start shell %q: %w", shellPath, err)
	}

	current := &session{
		manager: m,
		id:      newID("term-"),
		cmd:     cmd,
		stdin:   stdin,
		stdout:  writer,
		cwd:     workdir,
		notify:  make(chan struct{}),
		buffer:  newOutputBuffer(m.bufferBytes),
		done:    make(chan struct{}),
	}
	go current.readLoop(reader)
	go current.waitLoop()

	m.mu.Lock()
	if _, exists := m.sessions[current.id]; exists {
		m.mu.Unlock()
		_ = current.close()
		return "", errors.New("terminal: session id collision")
	}
	m.sessions[current.id] = current
	m.mu.Unlock()
	return current.id, nil
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

// send writes one command and waits for its completion marker.
func (s *session) send(ctx context.Context, input string, timeout time.Duration) (output string, timedOut bool, err error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

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
	// desynchronized and must be closed and reopened; terminal_signal KILL or
	// terminal_close stop the foreground command.
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

// signal sends one signal to the shell's process group.
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
	return signalProcessGroup(s.cmd, spec)
}

// close kills the process group, closes stdin and reaps the shell. It is
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
		// Last resort: make sure the pipe writer cannot keep the reader alive.
		_ = s.stdout.Close()
	}
	_ = s.stdout.Close()
	return nil
}

// takeOutput returns output from absolute offset from, caps it, and advances
// the consumed cursor.
func (s *session) takeOutput(from int64) string {
	text, _ := s.buffer.Slice(from)
	s.mu.Lock()
	s.consumed = s.buffer.Total()
	s.mu.Unlock()
	return truncateText(text, s.manager.outputLimit())
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
// trailing newline when it arrives in a later chunk.
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

// readLoop reads the merged stdout/stderr pipe, extracts completion markers and
// appends the rest to the output buffer.
func (s *session) readLoop(reader io.Reader) {
	var pending []byte
	chunk := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(chunk)
		if n > 0 {
			pending = s.consumeOutput(chunk[:n], pending)
		}
		if readErr != nil {
			break
		}
	}
	if len(pending) > 0 {
		s.appendOutput(pending)
	}
	// The pipe reached EOF (the shell is gone or its stdout closed). Exit state
	// is owned by waitLoop, which has the real exit code: wake waiters so a
	// blocked send/read observes it as soon as markExited runs.
	s.mu.Lock()
	s.wakeLocked()
	s.mu.Unlock()
}

// consumeOutput scans one chunk (plus pending partial marker bytes) for the
// current completion marker. It returns bytes that may be the beginning of a
// marker split across reads.
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
	_ = s.stdout.Close() // EOF for readLoop
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

// checkCommand validates one input line against the sandbox command policy.
func (m *Manager) checkCommand(input, cwd string) error {
	if m.policy == nil {
		return nil
	}
	argv, err := splitShellCommand(input)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return nil
	}
	return m.policy.CheckCommand(commandForPolicy(argv), cwd)
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
