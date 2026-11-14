package mcp

// stdioTransport 是子进程传输：在进程 stdin/stdout 上使用换行分隔的 JSON-RPC 2.0，stderr
// 被捕获用于诊断。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/procattr"
)

// stdioTransport 表示一个运行中的子进程。
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

// newStdioTransport 启动配置的子进程。调用方必须调用 Close 回收它。
func newStdioTransport(cfg Config) (*stdioTransport, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("mcp: Config.Command is required")
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	procattr.HideWindow(cmd)
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

// Request 通过 stdin 发送一个请求并等待其响应。
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

// Notify 通过 stdin 发送一个通知。
func (t *stdioTransport) Notify(_ context.Context, msg notificationMessage) error {
	if t.closed.Load() {
		return ErrClosed
	}
	return t.write(msg)
}

// Close 终止子进程：进行中的请求以 ErrClosed 失败，stdin 被关闭，closeGrace 内未退出的
// 进程会被强杀。
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

// write 序列化并发送一条消息。写入被串行化，避免并发调用在子进程 stdin 上交错出半行。
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

// forget 在请求落定后移除它。
func (t *stdioTransport) forget(id uint64) {
	t.mu.Lock()
	delete(t.pending, id)
	t.mu.Unlock()
}

// abortError 返回传输停止的原因（若已记录）。
func (t *stdioTransport) abortError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.abortErr != nil {
		return t.abortErr
	}
	return ErrClosed
}

// abort 记录终止错误、关闭 done channel 并让所有进行中请求失败。只有第一个原因生效。
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
			default: // 等待者已继续前进（上下文/已落定）
			}
		}
	})
}

// readLoop 从服务端 stdout 读取换行分隔的消息。
func (t *stdioTransport) readLoop() {
	defer func() {
		// 等待进程 watcher，它报告更有用的 "process exited" 错误；除非进程挂起期间连接已关闭。
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

// dispatch 把一条解码后的消息路由到对应请求。通知与畸形行被忽略：客户端只发起请求，因此
// 其他内容都不需要应答。
func (t *stdioTransport) dispatch(line []byte) {
	var envelope responseEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return
	}
	id, ok := decodeResponseID(envelope.ID)
	if !ok {
		return // 通知（或我们忽略的、不支持的服务端请求）
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

// waitLoop 回收子进程，并把其退出作为传输失败原因上报（除非传输是被有意关闭的）。
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

// tailBuffer 保留最后 max 字节。它是子进程 stderr 的 sink；内容会包含在进程退出错误中。
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

// newTailBuffer 创建一个只保留最后 max 字节的缓冲区。
func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

// Write 实现 io.Writer，保留最近的字节。
func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, data...)
	if len(b.buf) > b.max {
		b.buf = slices.Clone(b.buf[len(b.buf)-b.max:])
	}
	return len(data), nil
}

// String 返回保留的尾部内容。
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
