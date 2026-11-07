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

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// DefaultTimeout 在 Config.Timeout 为零时限制单次请求（握手、list、call、ping）。调用方
	// 上下文若有更早的截止时间则以其为准。
	DefaultTimeout = 30 * time.Second
	// DefaultClientName 是 Config.ClientName 为空时发送的 clientInfo 名称。
	DefaultClientName = "zeokeel-rig"
	// DefaultClientVersion 是 Config.ClientVersion 为空时发送的 clientInfo 版本。
	DefaultClientVersion = rig.Version
	// MaxMessageBytes 是从服务端接受的最大单条 JSON-RPC 消息。工具列表与结果可能很大，因此
	// 该值远高于协议 stdio 约定要求的 4 MiB。
	MaxMessageBytes = 8 << 20
	// closeGrace 是 Close 在强杀子进程之前等待 stdio 服务端配合退出的时长。
	closeGrace = 2 * time.Second
	// maxListPages 限制 cursor 分页，避免有 bug 的服务端让客户端无限循环。
	maxListPages = 1000

	// TransportStdio 把 MCP 服务端作为子进程运行（默认）。
	TransportStdio = "stdio"
	// TransportHTTP 连接 Streamable HTTP MCP 端点。
	TransportHTTP = "http"
)

// SupportedProtocolVersions 列出客户端理解的 MCP 协议版本，最新在前。客户端请求第一个
// 条目；服务端必须返回其中之一。
var SupportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// ErrClosed 是已关闭客户端上的操作返回的错误。
var ErrClosed = errors.New("mcp: client is closed")

// Config 配置 Start。
type Config struct {
	// Transport 选择传输："" 或 TransportStdio（默认）会把 Command 作为子进程启动；
	// TransportHTTP 通过 Streamable HTTP 连接 URL。
	Transport string

	// Command 是 stdio 传输的 MCP 服务端可执行文件（该传输必填）。查找规则与 exec.Command
	// 相同。
	Command string
	// Args 是 stdio 服务端的命令行参数。
	Args []string
	// Env 是子进程环境。nil 继承父进程环境；非 nil 切片会完全替换它（用
	// append(os.Environ(), ...) 扩展）。
	Env []string
	// Dir 是子进程工作目录（空值继承当前目录）。
	Dir string

	// URL 是 Streamable HTTP 端点（TransportHTTP 必填），例如
	// "https://mcp.example.com/mcp"。
	URL string
	// Headers 是额外 HTTP 请求头（仅 TransportHTTP）。它们在协议头之后应用，因此可以覆盖协议
	// 头（例如网关专用的 Authorization 或 User-Agent）。
	Headers map[string]string
	// HTTPClient 覆盖 HTTP 客户端（仅 TransportHTTP）。nil 表示整体无超时的客户端：每个请求
	// 由调用方上下文与 Config.Timeout 约束。
	HTTPClient *http.Client

	// ClientName 与 ClientVersion 填充 initialize 的 clientInfo。空值使用
	// DefaultClientName / DefaultClientVersion。
	ClientName    string
	ClientVersion string
	// ServerName 覆盖工具名中使用的名称。空值使用 initialize 响应中的 serverInfo.name。
	ServerName string
	// Timeout 限制单次请求。零表示 DefaultTimeout；负数关闭客户端侧超时（调用方上下文仍然
	// 生效）。
	Timeout time.Duration
}

// HTTPConfig 配置 StartHTTP——远端 MCP server（Streamable HTTP）的便捷入口。
type HTTPConfig struct {
	// URL 是 MCP 端点 URL（必填）。
	URL string
	// Headers 是额外请求头（例如 Authorization）。在协议头之后应用，因此可以覆盖它们。
	Headers map[string]string
	// HTTPClient 覆盖默认客户端（nil 表示整体无超时；由 Timeout 与调用方上下文约束每个
	// 请求）。
	HTTPClient *http.Client
	// ClientName 与 ClientVersion 填充 initialize 的 clientInfo。
	ClientName    string
	ClientVersion string
	// ServerName 覆盖工具名中使用的名称。
	ServerName string
	// Timeout 限制单次请求；零表示 DefaultTimeout，负数关闭客户端侧超时。
	Timeout time.Duration
}

// ServerInfo 是 initialize 报告的服务端身份。
type ServerInfo struct {
	Name    string
	Version string
}

// Client 是一条 MCP 连接（stdio 子进程或远端 HTTP 端点）。它可安全并发调用；在 Start
// 返回之前不得调用访问器。
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

// Start 使用 cfg.Transport 连接 MCP server，执行 initialize/notifications/initialized
// 握手并返回已连接的 client。
//
// ctx 约束握手（以及在未使用调用方上下文时的任意请求）；客户端超时按请求生效。出错时会
// 关闭传输并返回错误。
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

// StartHTTP 通过 Streamable HTTP 连接远端 MCP server。它是 Start 的简写：
// Config{Transport: TransportHTTP, URL: cfg.URL, ...}。
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

// normalized 应用 Config 默认值。
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

// handshake 执行 initialize 与 notifications/initialized。
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
	// HTTP 传输会在后续请求中携带协商版本（以及它从 initialize 响应捕获的会话 id）。
	if setter, ok := c.transport.(interface{ SetProtocolVersion(string) }); ok {
		setter.SetProtocolVersion(result.ProtocolVersion)
	}
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("mcp: send initialized notification: %w", err)
	}
	return nil
}

// supportedProtocolVersion 报告 version 是否在支持列表中。
func supportedProtocolVersion(version string) bool {
	for _, supported := range SupportedProtocolVersions {
		if version == supported {
			return true
		}
	}
	return false
}

// ServerName 返回工具名中使用的名称：设置了 Config.ServerName 时用它，否则用握手中
// 的 serverInfo.name。
func (c *Client) ServerName() string {
	if c == nil {
		return ""
	}
	return c.serverName
}

// ServerVersion 返回握手中的 serverInfo.version。
func (c *Client) ServerVersion() string {
	if c == nil {
		return ""
	}
	return c.serverVersion
}

// ProtocolVersion 返回协商出的 MCP 协议版本。
func (c *Client) ProtocolVersion() string {
	if c == nil {
		return ""
	}
	return c.protocolVersion
}

// SessionID 返回 HTTP 传输的服务端会话 id（从 initialize 响应捕获的 Mcp-Session-Id
// 头）。stdio 传输与无状态 HTTP 服务端返回空值。
func (c *Client) SessionID() string {
	if c == nil || c.transport == nil {
		return ""
	}
	if session, ok := c.transport.(interface{ SessionID() string }); ok {
		return session.SessionID()
	}
	return ""
}

// ListTools 返回服务端暴露的所有工具，跟随 nextCursor 分页并跨页去除重复名称。
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

// CallTool 调用一个 MCP 工具。args 必须是 JSON 对象（无参数时可为 nil/空）；非法 JSON 会
// 在发送前被拒绝。
//
// JSON-RPC/协议失败以 error 返回（服务端错误为 RPCError）。工具级失败以
// CallResult.IsError 且 error 为 nil 的形式返回，便于 agent 把它回灌给模型。
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

// Ping 发送 MCP ping 请求并等待响应。
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.request(ctx, "ping", nil)
	return err
}

// Close 关闭客户端。它是幂等的，且在传输已经失败后仍可安全调用。
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

// request 发送一个 JSON-RPC 请求并等待匹配的响应，或上下文/传输失败。
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

// notify 发送一个 JSON-RPC 通知（不期待响应）。客户端超时在这里同样生效：接受通知却永不
// 应答的服务端不能挂住握手。
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
