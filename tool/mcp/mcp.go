// Package mcp implements a dependency-free client for the Model Context
// Protocol (MCP) over the stdio transport: newline-delimited JSON-RPC 2.0
// messages exchanged with a child process.
//
// The implemented subset is what a tool-using agent needs:
//
//   - the initialize / notifications/initialized handshake, with protocol
//     version negotiation from SupportedProtocolVersions and server-info
//     validation;
//   - tools/list with cursor pagination and duplicate suppression;
//   - tools/call, mapping text, image, audio and resource content;
//   - ping;
//   - concurrent calls (request ids matched through a pending map, writes
//     serialized), context cancellation, a per-request timeout, and an
//     idempotent Close that reaps the child process.
//
// Tools adapts a client to contracts/v1.Tool values named
// <prefix><server>__<tool> (the default prefix is "mcp__"), so an MCP server's
// tools behave like any other Rig tool:
//
//	client, err := mcp.Start(ctx, mcp.Config{Command: "my-mcp-server"})
//	if err != nil { ... }
//	defer client.Close()
//	agentSpec.Tools = append(agentSpec.Tools, mcp.Tools(client, "")...)
//
// # Out of scope
//
// This is the stdio transport only. HTTP/SSE transports, resources/, prompts/,
// subscriptions, sampling/elicitation/roots (server-to-client requests) and
// server-side MCP are not implemented. Server notifications are ignored;
// unsupported server requests are not answered. The client is safe for
// concurrent calls; Close must not be called concurrently with itself.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultTimeout bounds one request (handshake, list, call, ping) when
	// Config.Timeout is zero. A caller context with an earlier deadline wins.
	DefaultTimeout = 30 * time.Second
	// DefaultClientName is the clientInfo name sent when Config.ClientName is
	// empty.
	DefaultClientName = "zeokeel-rig"
	// DefaultClientVersion is the clientInfo version sent when
	// Config.ClientVersion is empty.
	DefaultClientVersion = "0.3.0"
	// MaxMessageBytes is the largest single JSON-RPC message accepted from the
	// server. MCP tool listings and results can be large, so this is well above
	// the 4 MiB required by the protocol's stdio convention.
	MaxMessageBytes = 8 << 20
	// closeGrace is how long Close waits for a cooperative server exit before
	// killing the child process.
	closeGrace = 2 * time.Second
	// maxListPages bounds tools/list pagination so a buggy server cannot make
	// the client loop forever.
	maxListPages = 1000
)

// SupportedProtocolVersions lists the MCP protocol versions this client
// understands, newest first. The client requests the first entry; a server must
// answer with one of these versions.
var SupportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// ErrClosed is returned by operations on a closed client.
var ErrClosed = errors.New("mcp: client is closed")

// Config configures Start.
type Config struct {
	// Command is the MCP server executable (required). It is looked up with the
	// same rules as exec.Command.
	Command string
	// Args are the server command-line arguments.
	Args []string
	// Env is the child environment. Nil inherits the parent environment; a
	// non-nil slice replaces it (pass append(os.Environ(), ...) to extend).
	Env []string
	// Dir is the child working directory (empty inherits the current one).
	Dir string
	// ClientName and ClientVersion populate initialize clientInfo. Empty values
	// use DefaultClientName / DefaultClientVersion.
	ClientName    string
	ClientVersion string
	// ServerName overrides the name used in tool names. Empty uses
	// serverInfo.name from the initialize response.
	ServerName string
	// Timeout bounds one request. Zero means DefaultTimeout; negative disables
	// the client-side timeout (the caller context still applies).
	Timeout time.Duration
}

// ServerInfo is the server identity reported by initialize.
type ServerInfo struct {
	Name    string
	Version string
}

// Client is one running MCP server connection over stdio. It is safe for
// concurrent calls; accessors must not be called before Start returns.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tailBuffer

	timeout       time.Duration
	clientName    string
	clientVersion string
	wantServer    string

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[uint64]chan rpcResponse
	nextID  atomic.Uint64

	closed    atomic.Bool
	closeOnce sync.Once
	abortOnce sync.Once
	done      chan struct{}
	abortErr  error

	processDone chan struct{}

	serverName      string
	serverVersion   string
	protocolVersion string
}

// rpcResponse is the outcome of one request delivered to its pending waiter.
type rpcResponse struct {
	result json.RawMessage
	err    error
}

// Start launches the configured server, performs the initialize/
// notifications/initialized handshake and returns a connected client.
//
// ctx bounds the handshake (and any request when a caller context is not used);
// the client timeout applies per request. On error the child process is
// terminated and the error is returned.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.normalized()
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

	client := &Client{
		cmd:           cmd,
		stdin:         stdin,
		stdout:        stdout,
		stderr:        stderr,
		timeout:       cfg.Timeout,
		clientName:    cfg.ClientName,
		clientVersion: cfg.ClientVersion,
		wantServer:    cfg.ServerName,
		pending:       make(map[uint64]chan rpcResponse),
		done:          make(chan struct{}),
		processDone:   make(chan struct{}),
	}
	go client.waitLoop()
	go client.readLoop()

	if err := client.handshake(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// normalized applies Config defaults.
func (cfg Config) normalized() Config {
	cfg.Command = strings.TrimSpace(cfg.Command)
	if cfg.ClientName == "" {
		cfg.ClientName = DefaultClientName
	}
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = DefaultClientVersion
	}
	cfg.ServerName = strings.TrimSpace(cfg.ServerName)
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	} else if cfg.Timeout < 0 {
		cfg.Timeout = 0
	}
	return cfg
}

// handshake performs initialize and notifications/initialized.
func (c *Client) handshake(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": SupportedProtocolVersions[0],
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    c.clientName,
			"version": c.clientVersion,
		},
	}
	raw, err := c.request(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	var result initializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("mcp: decode initialize result: %w", err)
	}
	if result.ProtocolVersion == "" {
		return errors.New("mcp: initialize result is missing protocolVersion")
	}
	if !supportedProtocolVersion(result.ProtocolVersion) {
		return fmt.Errorf("mcp: server protocol version %q is not supported (supported: %s)",
			result.ProtocolVersion, strings.Join(SupportedProtocolVersions, ", "))
	}
	if result.ServerInfo.Name == "" {
		return errors.New("mcp: initialize result is missing serverInfo.name")
	}
	c.serverName = result.ServerInfo.Name
	c.serverVersion = result.ServerInfo.Version
	c.protocolVersion = result.ProtocolVersion
	if c.wantServer != "" {
		c.serverName = c.wantServer
	}
	if err := c.notify("notifications/initialized", nil); err != nil {
		return fmt.Errorf("mcp: send initialized notification: %w", err)
	}
	return nil
}

// supportedProtocolVersion reports whether version is in the supported list.
func supportedProtocolVersion(version string) bool {
	for _, supported := range SupportedProtocolVersions {
		if version == supported {
			return true
		}
	}
	return false
}

// ServerName returns the name used in tool names: Config.ServerName when set,
// otherwise serverInfo.name from the handshake.
func (c *Client) ServerName() string {
	if c == nil {
		return ""
	}
	return c.serverName
}

// ServerVersion returns serverInfo.version from the handshake.
func (c *Client) ServerVersion() string {
	if c == nil {
		return ""
	}
	return c.serverVersion
}

// ProtocolVersion returns the negotiated MCP protocol version.
func (c *Client) ProtocolVersion() string {
	if c == nil {
		return ""
	}
	return c.protocolVersion
}

// ListTools returns every tool the server exposes, following nextCursor
// pagination and suppressing duplicate names across pages.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	seen := make(map[string]struct{})
	cursors := make(map[string]struct{})
	var tools []ToolInfo
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("mcp: tools/list exceeded %d pages", maxListPages)
		}
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		raw, err := c.request(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var result wireToolListResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("mcp: decode tools/list result: %w", err)
		}
		for _, tool := range result.Tools {
			name := strings.TrimSpace(tool.Name)
			if name == "" {
				continue
			}
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			seen[name] = struct{}{}
			schema := bytes.TrimSpace(tool.InputSchema)
			if len(schema) == 0 {
				schema = []byte(`{"type":"object"}`)
			}
			tools = append(tools, ToolInfo{
				Name:        name,
				Description: tool.Description,
				InputSchema: append(json.RawMessage(nil), schema...),
			})
		}
		next := strings.TrimSpace(result.NextCursor)
		if next == "" || next == cursor {
			return tools, nil
		}
		if _, repeated := cursors[next]; repeated {
			return nil, fmt.Errorf("mcp: tools/list cursor %q repeated", next)
		}
		cursors[next] = struct{}{}
		cursor = next
	}
}

// CallTool invokes one MCP tool. args must be a JSON object (or nil/empty for
// no arguments); invalid JSON is rejected before anything is sent.
//
// A JSON-RPC/protocol failure is returned as an error (RPCError for server
// errors). A tool-level failure arrives as CallResult.IsError with a nil error,
// so the agent can feed it back to the model.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return CallResult{}, errors.New("mcp: tool name is required")
	}
	params := map[string]any{"name": name}
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return CallResult{}, fmt.Errorf("mcp: tool %q arguments must be a JSON object: %w", name, err)
		}
		params["arguments"] = json.RawMessage(trimmed)
	}
	raw, err := c.request(ctx, "tools/call", params)
	if err != nil {
		return CallResult{}, err
	}
	var wire wireCallToolResult
	if err := json.Unmarshal(raw, &wire); err != nil {
		return CallResult{}, fmt.Errorf("mcp: decode tools/call result: %w", err)
	}
	result := CallResult{
		IsError:           wire.IsError,
		StructuredContent: append(json.RawMessage(nil), bytes.TrimSpace(wire.StructuredContent)...),
	}
	for _, content := range wire.Content {
		result.Content = append(result.Content, mapContent(content))
	}
	return result, nil
}

// Ping sends the MCP ping request and waits for the response.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.request(ctx, "ping", nil)
	return err
}

// Close shuts the client down: it fails in-flight requests with ErrClosed,
// closes the child stdin, waits up to closeGrace for a cooperative exit and
// kills the process if it does not exit. It is idempotent and safe after the
// server has already exited.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.abort(ErrClosed)
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		select {
		case <-c.processDone:
			return
		case <-time.After(closeGrace):
		}
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		select {
		case <-c.processDone:
		case <-time.After(closeGrace):
		}
	})
	return nil
}

// request performs one JSON-RPC request and waits for the matching response or
// a context/process failure.
func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.closed.Load() {
		return nil, ErrClosed
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	id := c.nextID.Add(1)
	response := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[id] = response
	c.mu.Unlock()
	defer c.forget(id)

	if err := c.write(requestMessage{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case resp := <-response:
		if resp.err != nil {
			return nil, resp.err
		}
		return resp.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.abortError()
	}
}

// notify sends one JSON-RPC notification (no response expected).
func (c *Client) notify(method string, params any) error {
	if c.closed.Load() {
		return ErrClosed
	}
	return c.write(notificationMessage{JSONRPC: "2.0", Method: method, Params: params})
}

// write serializes and sends one message. Writes are serialized so concurrent
// calls cannot interleave partial lines on the child stdin.
func (c *Client) write(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("mcp: encode message: %w", err)
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	_, writeErr := c.stdin.Write(data)
	c.writeMu.Unlock()
	if writeErr != nil {
		err := fmt.Errorf("mcp: write to server: %w", writeErr)
		c.abort(err)
		return err
	}
	return nil
}

// forget drops one pending request after it settles.
func (c *Client) forget(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// abortError returns the reason the client stopped, if one was recorded.
func (c *Client) abortError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.abortErr != nil {
		return c.abortErr
	}
	return ErrClosed
}

// abort records the terminal error, closes the done channel and fails every
// pending request. Only the first reason wins.
func (c *Client) abort(err error) {
	c.abortOnce.Do(func() {
		c.mu.Lock()
		c.abortErr = err
		pending := c.pending
		c.pending = nil
		c.mu.Unlock()
		close(c.done)
		for _, waiter := range pending {
			select {
			case waiter <- rpcResponse{err: err}:
			default: // the waiter already moved on (context/settled)
			}
		}
	})
}

// readLoop reads newline-delimited messages from the server stdout.
func (c *Client) readLoop() {
	defer func() {
		// Wait for the process watcher, which reports the more useful "process
		// exited" error, unless the connection closed while the process hangs.
		select {
		case <-c.processDone:
		case <-time.After(500 * time.Millisecond):
			c.abort(errors.New("mcp: server closed its stdout"))
		}
	}()
	scanner := bufio.NewScanner(c.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		c.dispatch(line)
	}
	if err := scanner.Err(); err != nil {
		c.abort(fmt.Errorf("mcp: read server message: %w", err))
	}
}

// dispatch routes one decoded message to its pending request. Notifications and
// malformed lines are ignored: the client only initiated requests, so nothing
// else needs an answer.
func (c *Client) dispatch(line []byte) {
	var envelope responseEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return
	}
	id, ok := decodeResponseID(envelope.ID)
	if !ok {
		return // notification (or an unsupported server request we ignore)
	}
	response := rpcResponse{result: envelope.Result}
	if envelope.Error != nil {
		response.err = envelope.Error
	} else if len(bytes.TrimSpace(envelope.Result)) == 0 {
		response.err = fmt.Errorf("mcp: response %d carries neither result nor error", id)
	}
	c.mu.Lock()
	waiter := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if waiter != nil {
		select {
		case waiter <- response:
		default:
		}
	}
}

// waitLoop reaps the child process and reports its exit as the client failure
// reason (unless the client is being closed on purpose).
func (c *Client) waitLoop() {
	err := c.cmd.Wait()
	close(c.processDone)
	if c.closed.Load() {
		return
	}
	if err != nil {
		message := fmt.Sprintf("mcp: server process exited: %v", err)
		if stderr := strings.TrimSpace(c.stderr.String()); stderr != "" {
			message += fmt.Sprintf(" (stderr: %q)", stderr)
		}
		c.abort(errors.New(message))
		return
	}
	c.abort(errors.New("mcp: server process exited unexpectedly"))
}

// decodeResponseID parses a JSON-RPC id echoed by the server. The client sends
// numeric ids; a string form of the same number is tolerated as well.
func decodeResponseID(raw json.RawMessage) (uint64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false
	}
	text := string(trimmed)
	if strings.HasPrefix(text, `"`) {
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return 0, false
		}
	}
	id, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
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
