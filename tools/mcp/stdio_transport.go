package mcp

// stdioTransport is the child-process transport: newline-delimited JSON-RPC 2.0
// over the process stdin/stdout, with stderr captured for diagnostics.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// stdioTransport is one running child process.
type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tailBuffer

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[uint64]chan rpcResponse

	closed    atomic.Bool
	closeOnce sync.Once
	abortOnce sync.Once
	done      chan struct{}
	abortErr  error

	processDone chan struct{}
}

var _ transport = (*stdioTransport)(nil)

// newStdioTransport starts the configured child process. The caller must call
// Close to reap it.
func newStdioTransport(cfg Config) (*stdioTransport, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("mcp: Config.Command is required")
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	if cfg.Env != nil {
		cmd.Env = cfg.Env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: create server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp: create server stdout: %w", err)
	}
	stderr := newTailBuffer(4096)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("mcp: start server %q: %w", cfg.Command, err)
	}

	transport := &stdioTransport{
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		pending:     make(map[uint64]chan rpcResponse),
		done:        make(chan struct{}),
		processDone: make(chan struct{}),
	}
	go transport.waitLoop()
	go transport.readLoop()
	return transport, nil
}

// Request sends one request over stdin and waits for its response.
func (t *stdioTransport) Request(ctx context.Context, msg requestMessage) (rpcResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t.closed.Load() {
		return rpcResponse{}, ErrClosed
	}
	response := make(chan rpcResponse, 1)
	t.mu.Lock()
	if t.closed.Load() {
		t.mu.Unlock()
		return rpcResponse{}, ErrClosed
	}
	t.pending[msg.ID] = response
	t.mu.Unlock()
	defer t.forget(msg.ID)

	if err := t.write(msg); err != nil {
		return rpcResponse{}, err
	}
	select {
	case resp := <-response:
		return resp, nil
	case <-ctx.Done():
		return rpcResponse{}, ctx.Err()
	case <-t.done:
		return rpcResponse{}, t.abortError()
	}
}

// Notify sends one notification over stdin.
func (t *stdioTransport) Notify(_ context.Context, msg notificationMessage) error {
	if t.closed.Load() {
		return ErrClosed
	}
	return t.write(msg)
}

// Close terminates the child process: in-flight requests fail with ErrClosed,
// stdin is closed, and a process that does not exit within closeGrace is
// killed.
func (t *stdioTransport) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		t.abort(ErrClosed)
		if t.stdin != nil {
			_ = t.stdin.Close()
		}
		select {
		case <-t.processDone:
			return
		case <-time.After(closeGrace):
		}
		if t.cmd != nil && t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		select {
		case <-t.processDone:
		case <-time.After(closeGrace):
		}
	})
	return nil
}

// write serializes and sends one message. Writes are serialized so concurrent
// calls cannot interleave partial lines on the child stdin.
func (t *stdioTransport) write(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("mcp: encode message: %w", err)
	}
	data = append(data, '\n')
	t.writeMu.Lock()
	_, writeErr := t.stdin.Write(data)
	t.writeMu.Unlock()
	if writeErr != nil {
		writeError := fmt.Errorf("mcp: write to server: %w", writeErr)
		t.abort(writeError)
		return writeError
	}
	return nil
}

// forget drops one pending request after it settles.
func (t *stdioTransport) forget(id uint64) {
	t.mu.Lock()
	delete(t.pending, id)
	t.mu.Unlock()
}

// abortError returns the reason the transport stopped, if one was recorded.
func (t *stdioTransport) abortError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.abortErr != nil {
		return t.abortErr
	}
	return ErrClosed
}

// abort records the terminal error, closes the done channel and fails every
// pending request. Only the first reason wins.
func (t *stdioTransport) abort(err error) {
	t.abortOnce.Do(func() {
		t.mu.Lock()
		t.abortErr = err
		pending := t.pending
		t.pending = nil
		t.mu.Unlock()
		close(t.done)
		for _, waiter := range pending {
			select {
			case waiter <- rpcResponse{err: err}:
			default: // the waiter already moved on (context/settled)
			}
		}
	})
}

// readLoop reads newline-delimited messages from the server stdout.
func (t *stdioTransport) readLoop() {
	defer func() {
		// Wait for the process watcher, which reports the more useful "process
		// exited" error, unless the connection closed while the process hangs.
		select {
		case <-t.processDone:
		case <-time.After(500 * time.Millisecond):
			t.abort(errors.New("mcp: server closed its stdout"))
		}
	}()
	scanner := bufio.NewScanner(t.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		t.dispatch(line)
	}
	if err := scanner.Err(); err != nil {
		t.abort(fmt.Errorf("mcp: read server message: %w", err))
	}
}

// dispatch routes one decoded message to its pending request. Notifications and
// malformed lines are ignored: the client only initiated requests, so nothing
// else needs an answer.
func (t *stdioTransport) dispatch(line []byte) {
	var envelope responseEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return
	}
	id, ok := decodeResponseID(envelope.ID)
	if !ok {
		return // notification (or an unsupported server request we ignore)
	}
	response := responseFromEnvelope(&envelope, id)
	t.mu.Lock()
	waiter := t.pending[id]
	delete(t.pending, id)
	t.mu.Unlock()
	if waiter != nil {
		select {
		case waiter <- response:
		default:
		}
	}
}

// waitLoop reaps the child process and reports its exit as the transport
// failure reason (unless the transport is being closed on purpose).
func (t *stdioTransport) waitLoop() {
	err := t.cmd.Wait()
	close(t.processDone)
	if t.closed.Load() {
		return
	}
	if err != nil {
		message := fmt.Sprintf("mcp: server process exited: %v", err)
		if stderr := strings.TrimSpace(t.stderr.String()); stderr != "" {
			message += fmt.Sprintf(" (stderr: %q)", stderr)
		}
		t.abort(errors.New(message))
		return
	}
	t.abort(errors.New("mcp: server process exited unexpectedly"))
}

// tailBuffer keeps the last max bytes written to it. It is the child stderr
// sink; its content is included in process-exit errors.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

// newTailBuffer creates a buffer that retains the last max bytes.
func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

// Write implements io.Writer, preserving the most recent bytes.
func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, data...)
	if len(b.buf) > b.max {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.max:]...)
	}
	return len(data), nil
}

// String returns the retained tail.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
