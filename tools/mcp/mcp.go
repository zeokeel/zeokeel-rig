// Package mcp implements a dependency-free client for the Model Context
// Protocol (MCP) over two transports:
//
//   - stdio: newline-delimited JSON-RPC 2.0 messages exchanged with a child
//     process (Config.Command);
//   - Streamable HTTP: JSON-RPC messages POSTed to a remote endpoint, with
//     single-JSON or SSE responses, Mcp-Session-Id and MCP-Protocol-Version
//     headers (Config.Transport = TransportHTTP + Config.URL, or StartHTTP).
//
// The implemented subset is what a tool-using agent needs:
//
//   - the initialize / notifications/initialized handshake, with protocol
//     version negotiation from SupportedProtocolVersions and server-info
//     validation;
//   - tools/list with cursor pagination and duplicate suppression;
//   - tools/call, mapping text, image, audio and resource content;
//   - resources/list + resources/read + resources/templates/list (cursor
//     pagination, text/blob contents, URI templates);
//   - prompts/list + prompts/get (arguments, message rendering);
//   - ping;
//   - concurrent calls, context cancellation, a per-request timeout, and an
//     idempotent Close.
//
// Tools adapts a client to contracts/rig.Tool values named
// <prefix><server>__<tool> (the default prefix is "mcp__"), so an MCP server's
// tools behave like any other Rig tool:
//
//	client, err := mcp.Start(ctx, mcp.Config{Command: "my-mcp-server"})
//	if err != nil { ... }
//	defer client.Close()
//	agentSpec.Tools = append(agentSpec.Tools, mcp.Tools(client, "")...)
//
// A remote server uses the same client:
//
//	client, err := mcp.StartHTTP(ctx, mcp.HTTPConfig{
//		URL:     "https://mcp.example.com/mcp",
//		Headers: map[string]string{"Authorization": "Bearer " + token},
//	})
//
// # Resources and prompts
//
// ListResources/ReadResource and ListPrompts/GetPrompt cover the resources/ and
// prompts/ subsets (cursor pagination, duplicate suppression, text/blob
// contents, prompt argument rendering). ResourceTools adapts them to four
// model-facing tools. Both subsets are optional in MCP: a server that does not
// declare the capability answers their methods with a JSON-RPC error, which is
// returned to the caller unchanged.
//
// # Out of scope
//
// Both transports implement client-initiated requests only. Server
// notifications are ignored and unsupported server requests are not answered.
// The HTTP transport does not implement GET notification streams, resumability
// (Last-Event-ID), session DELETE or OAuth; subscriptions,
// sampling/elicitation/roots and server-side MCP are out of scope as well. The
// client is safe for concurrent calls; Close must not be called concurrently
// with itself.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	// server. Tool listings and results can be large, so this is well above the
	// 4 MiB required by the protocol's stdio convention.
	MaxMessageBytes = 8 << 20
	// closeGrace is how long Close waits for a cooperative stdio server exit
	// before killing the child process.
	closeGrace = 2 * time.Second
	// maxListPages bounds cursor pagination so a buggy server cannot make the
	// client loop forever.
	maxListPages = 1000

	// TransportStdio runs the MCP server as a child process (the default).
	TransportStdio = "stdio"
	// TransportHTTP connects to a Streamable HTTP MCP endpoint.
	TransportHTTP = "http"
)

// SupportedProtocolVersions lists the MCP protocol versions this client
// understands, newest first. The client requests the first entry; a server must
// answer with one of these versions.
var SupportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// ErrClosed is returned by operations on a closed client.
var ErrClosed = errors.New("mcp: client is closed")

// Config configures Start.
type Config struct {
	// Transport selects the transport: "" or TransportStdio (default) starts
	// Command as a child process; TransportHTTP connects to URL over Streamable
	// HTTP.
	Transport string

	// Command is the MCP server executable for the stdio transport (required
	// there). It is looked up with the same rules as exec.Command.
	Command string
	// Args are the stdio server command-line arguments.
	Args []string
	// Env is the child environment. Nil inherits the parent environment; a
	// non-nil slice replaces it (pass append(os.Environ(), ...) to extend).
	Env []string
	// Dir is the child working directory (empty inherits the current one).
	Dir string

	// URL is the Streamable HTTP endpoint (required for TransportHTTP), for
	// example "https://mcp.example.com/mcp".
	URL string
	// Headers are extra HTTP request headers (TransportHTTP only). They are
	// applied after the protocol headers, so they may override them (for
	// example a gateway-specific Authorization or User-Agent).
	Headers map[string]string
	// HTTPClient overrides the HTTP client (TransportHTTP only). Nil uses a
	// client with no overall timeout: the caller context and Config.Timeout
	// bound each request.
	HTTPClient *http.Client

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

// HTTPConfig configures StartHTTP, the convenience entry point for a remote MCP
// server over Streamable HTTP.
type HTTPConfig struct {
	// URL is the MCP endpoint URL (required).
	URL string
	// Headers are extra request headers (for example Authorization). Applied
	// after the protocol headers, so they may override them.
	Headers map[string]string
	// HTTPClient overrides the default client (nil means no overall timeout;
	// Timeout and the caller context bound each request).
	HTTPClient *http.Client
	// ClientName and ClientVersion populate initialize clientInfo.
	ClientName    string
	ClientVersion string
	// ServerName overrides the name used in tool names.
	ServerName string
	// Timeout bounds one request; zero means DefaultTimeout, negative disables
	// the client-side timeout.
	Timeout time.Duration
}

// ServerInfo is the server identity reported by initialize.
type ServerInfo struct {
	Name    string
	Version string
}

// Client is one MCP connection (stdio child process or remote HTTP endpoint). It
// is safe for concurrent calls; accessors must not be called before Start
// returns.
type Client struct {
	transport transport

	timeout       time.Duration
	clientName    string
	clientVersion string
	wantServer    string
	nextID        atomic.Uint64

	closed    atomic.Bool
	closeOnce sync.Once

	serverName      string
	serverVersion   string
	protocolVersion string
}

// Start connects to an MCP server using cfg.Transport, performs the
// initialize/notifications/initialized handshake and returns a connected
// client.
//
// ctx bounds the handshake (and any request when a caller context is not used);
// the client timeout applies per request. On error the transport is closed and
// the error is returned.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.normalized()

	var (
		tr  transport
		err error
	)
	switch cfg.Transport {
	case "", TransportStdio:
		if strings.TrimSpace(cfg.Command) == "" {
			return nil, errors.New("mcp: Config.Command is required")
		}
		tr, err = newStdioTransport(cfg)
	case TransportHTTP:
		tr, err = newHTTPTransport(cfg)
	default:
		return nil, fmt.Errorf("mcp: unknown transport %q (want %q or %q)", cfg.Transport, TransportStdio, TransportHTTP)
	}
	if err != nil {
		return nil, err
	}

	client := &Client{
		transport:     tr,
		timeout:       cfg.Timeout,
		clientName:    cfg.ClientName,
		clientVersion: cfg.ClientVersion,
		wantServer:    cfg.ServerName,
	}
	if err := client.handshake(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// StartHTTP connects to a remote MCP server over Streamable HTTP. It is
// shorthand for Start with Config{Transport: TransportHTTP, URL: cfg.URL, ...}.
func StartHTTP(ctx context.Context, cfg HTTPConfig) (*Client, error) {
	return Start(ctx, Config{
		Transport:     TransportHTTP,
		URL:           cfg.URL,
		Headers:       cfg.Headers,
		HTTPClient:    cfg.HTTPClient,
		ClientName:    cfg.ClientName,
		ClientVersion: cfg.ClientVersion,
		ServerName:    cfg.ServerName,
		Timeout:       cfg.Timeout,
	})
}

// normalized applies Config defaults.
func (cfg Config) normalized() Config {
	cfg.Transport = strings.ToLower(strings.TrimSpace(cfg.Transport))
	cfg.Command = strings.TrimSpace(cfg.Command)
	cfg.ClientName = strings.TrimSpace(cfg.ClientName)
	if cfg.ClientName == "" {
		cfg.ClientName = DefaultClientName
	}
	cfg.ClientVersion = strings.TrimSpace(cfg.ClientVersion)
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
	// The HTTP transport carries the negotiated version (and the session id it
	// captured from the initialize response) on subsequent requests.
	if setter, ok := c.transport.(interface{ SetProtocolVersion(string) }); ok {
		setter.SetProtocolVersion(result.ProtocolVersion)
	}
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
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

// SessionID returns the server session id for the HTTP transport (the
// Mcp-Session-Id header captured from the initialize response). It is empty for
// the stdio transport and for stateless HTTP servers.
func (c *Client) SessionID() string {
	if c == nil || c.transport == nil {
		return ""
	}
	if session, ok := c.transport.(interface{ SessionID() string }); ok {
		return session.SessionID()
	}
	return ""
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

// Close shuts the client down. It is idempotent and safe after the transport
// already failed.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.transport != nil {
			_ = c.transport.Close()
		}
	})
	return nil
}

// request performs one JSON-RPC request and waits for the matching response or
// a context/transport failure.
func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.closed.Load() || c.transport == nil {
		return nil, ErrClosed
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	id := c.nextID.Add(1)
	resp, err := c.transport.Request(ctx, requestMessage{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, err
	}
	if resp.err != nil {
		return nil, resp.err
	}
	return resp.result, nil
}

// notify sends one JSON-RPC notification (no response expected). The client
// timeout applies here too: a server that accepts a notification but never
// answers it must not be able to hang the handshake.
func (c *Client) notify(ctx context.Context, method string, params any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.closed.Load() || c.transport == nil {
		return ErrClosed
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	return c.transport.Notify(ctx, notificationMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
}
