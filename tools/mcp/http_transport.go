package mcp

// httpTransport 是 MCP Streamable HTTP 传输：JSON-RPC 消息 POST 到单一端点，响应可以是
// 单个 application/json 体，也可以是携带 JSON-RPC 消息的 SSE 流；会话/协议状态放在请求头
// 中（Mcp-Session-Id、MCP-Protocol-Version）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	// headerSessionID 是服务端在 initialize 时设置、客户端在后续请求中回传的 MCP 会话头。
	headerSessionID = "Mcp-Session-Id"
	// headerProtocolVersion 在初始化后携带协商出的 MCP 协议版本。
	headerProtocolVersion = "MCP-Protocol-Version"
	// httpErrorBodyMax 限制错误信息中引用的非 2xx 响应体长度。
	httpErrorBodyMax = 1024
	// httpNotificationDrainMax 限制被忽略通知响应体的读取量，以便复用连接。
	httpNotificationDrainMax = 64 << 10
)

// httpTransport 表示一个远端 MCP 端点。
type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client

	mu              sync.Mutex
	sessionID       string
	protocolVersion string
	initialized     bool
	closed          bool
}

var _ transport = (*httpTransport)(nil)

// newHTTPTransport 校验 cfg.URL 并构建 HTTP 传输。
func newHTTPTransport(cfg Config) (*httpTransport, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	if rawURL == "" {
		return nil, errors.New("mcp: Config.URL is required for the http transport")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("mcp: invalid URL %q: %w", cfg.URL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("mcp: invalid URL %q: scheme must be http or https", cfg.URL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("mcp: invalid URL %q: missing host", cfg.URL)
	}
	headers := make(map[string]string, len(cfg.Headers))
	for name, value := range cfg.Headers {
		headers[name] = value
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &httpTransport{url: rawURL, headers: headers, client: client}, nil
}

// SetProtocolVersion 记录协商出的协议版本（由 Client 在校验 initialize 结果后调用），
// 并把传输标记为已初始化。后续请求会携带 MCP-Protocol-Version 头。
func (t *httpTransport) SetProtocolVersion(version string) {
	t.mu.Lock()
	t.protocolVersion = version
	t.initialized = true
	t.mu.Unlock()
}

// SessionID 返回从 initialize 响应捕获的服务端会话 id；无状态服务端时为 ""。
func (t *httpTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// setSessionID 记录服务端提供的会话 id。
func (t *httpTransport) setSessionID(sessionID string) {
	t.mu.Lock()
	t.sessionID = sessionID
	t.mu.Unlock()
}

// Request POST 一个请求并返回其 JSON-RPC 响应。
func (t *httpTransport) Request(ctx context.Context, msg requestMessage) (rpcResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t.isClosed() {
		return rpcResponse{}, ErrClosed
	}
	// 握手完成前只允许 initialize；这样意外未初始化的传输会显式失败，而不是 POST 无会话
	// 请求。
	if msg.Method != "initialize" && !t.isInitialized() {
		return rpcResponse{}, ErrNotInitialized
	}

	resp, err := t.post(ctx, msg)
	if err != nil {
		return rpcResponse{}, err
	}
	defer resp.Body.Close()
	if err := checkHTTPStatus(resp); err != nil {
		return rpcResponse{}, err
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch strings.ToLower(mediaType) {
	case "application/json":
		envelope, err := decodeJSONEnvelope(resp.Body)
		if err != nil {
			return rpcResponse{}, err
		}
		id, ok := decodeResponseID(envelope.ID)
		if !ok {
			return rpcResponse{}, fmt.Errorf("mcp: server returned a notification instead of a response to %q", msg.Method)
		}
		if id != msg.ID {
			return rpcResponse{}, fmt.Errorf("mcp: response id %d does not match request id %d", id, msg.ID)
		}
		return responseFromEnvelope(envelope, msg.ID), nil
	case "text/event-stream":
		return t.readSSEResponse(ctx, resp.Body, msg.ID)
	default:
		return rpcResponse{}, fmt.Errorf("mcp: server returned Content-Type %q for %q; expected application/json or text/event-stream",
			contentType, msg.Method)
	}
}

// Notify POST 一个通知。服务端通常以 202 且无响应体应答；任何 2xx 都被接受（会读取少量
// 响应体以复用连接）。
func (t *httpTransport) Notify(ctx context.Context, msg notificationMessage) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if t.isClosed() {
		return ErrClosed
	}
	if !t.isInitialized() {
		return ErrNotInitialized
	}
	resp, err := t.post(ctx, msg)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkHTTPStatus(resp); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, httpNotificationDrainMax))
	return nil
}

// Close 把传输标记为已关闭。幂等。有意不发送 MCP DELETE-session 请求：服务端可能不支持，
// 而且关闭客户端不应因尽力而为的清理而失败。
func (t *httpTransport) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

// post 发送一条 JSON-RPC 消息并返回原始 HTTP 响应。调用方拥有响应体且必须关闭它。
func (t *httpTransport) post(ctx context.Context, message any) (*http.Response, error) {
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode message: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("mcp: build http request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")

	t.mu.Lock()
	sessionID, protocolVersion, closed := t.sessionID, t.protocolVersion, t.closed
	overrides := t.headers
	t.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if sessionID != "" {
		request.Header.Set(headerSessionID, sessionID)
	}
	if protocolVersion != "" {
		request.Header.Set(headerProtocolVersion, protocolVersion)
	}
	// 额外请求头优先，因此宿主可以添加认证或为非规范网关覆盖默认值。
	for name, value := range overrides {
		request.Header.Set(name, value)
	}

	resp, err := t.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("mcp: http request: %w", err)
	}
	if serverSession := resp.Header.Get(headerSessionID); serverSession != "" {
		t.setSessionID(serverSession)
	}
	return resp, nil
}

// readSSEResponse 从单个 SSE 响应读取 JSON-RPC 消息，直到出现匹配 id 的响应。通知、服务端
// 请求与其他 id 的响应都会被忽略。
func (t *httpTransport) readSSEResponse(ctx context.Context, body io.Reader, id uint64) (rpcResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxMessageBytes)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found || field != "data" {
			continue
		}
		payload, _ := strings.CutPrefix(value, " ")
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var envelope responseEnvelope
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			continue // 容忍畸形事件；下一个事件可能匹配
		}
		responseID, ok := decodeResponseID(envelope.ID)
		if !ok || responseID != id {
			continue
		}
		return responseFromEnvelope(&envelope, id), nil
	}
	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return rpcResponse{}, ctxErr
		}
		return rpcResponse{}, fmt.Errorf("mcp: read SSE response: %w", err)
	}
	return rpcResponse{}, fmt.Errorf("mcp: SSE response ended without a response for request %d", id)
}

// decodeJSONEnvelope 解码一个 JSON-RPC 信封，大小限制为 MaxMessageBytes。
func decodeJSONEnvelope(reader io.Reader) (*responseEnvelope, error) {
	var envelope responseEnvelope
	decoder := json.NewDecoder(io.LimitReader(reader, MaxMessageBytes))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("mcp: decode json response: %w", err)
	}
	return &envelope, nil
}

// checkHTTPStatus 把非 2xx 响应转换为错误。404/410 会报告为 ErrSessionExpired，因为 MCP
// 服务端用它们表示未知或过期会话。
func checkHTTPStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	snippet := readErrorSnippet(resp.Body)
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		if snippet != "" {
			return fmt.Errorf("%w: %s: %s", ErrSessionExpired, resp.Status, snippet)
		}
		return fmt.Errorf("%w: %s", ErrSessionExpired, resp.Status)
	}
	if snippet != "" {
		return fmt.Errorf("mcp: http request failed: %s: %s", resp.Status, snippet)
	}
	return fmt.Errorf("mcp: http request failed: %s", resp.Status)
}

// readErrorSnippet 读取有界的响应体片段用于诊断。
func readErrorSnippet(body io.Reader) string {
	if body == nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(body, httpErrorBodyMax))
	return strings.TrimSpace(string(data))
}

// isClosed 报告 Close 是否已调用。
func (t *httpTransport) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// isInitialized 报告该传输的握手是否已完成。
func (t *httpTransport) isInitialized() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.initialized
}
