package mcp

// 本文件实现 MCP Streamable HTTP 的服务端通知流（P2-7）：
//
//   - initialize 之后，客户端用 GET + Accept: text/event-stream 打开 SSE 通知流；
//   - 逐帧解析 event:/data:/id:（按 SSE 规范，空行分帧），把 JSON-RPC 通知
//     （无 id、有 method）分发给 Client.OnNotification 注册的回调；
//   - 记录每个事件的 id，断线重连时用 Last-Event-ID 请求头恢复，实现可恢复性；
//   - Close 时取消通知流并尽力发送 Mcp-Session-Id 的 HTTP DELETE 结束会话。
//
// 通知流与请求/响应路径完全分离：POST 请求各自读取自己的响应，通知流只在
// 后台 goroutine 中解析 SSE，两者不共享锁或 pending 表，因此不会互相阻塞或
// 死锁。客户端不支持服务端到客户端的请求（sampling/roots/elicitation），
// 这类帧会被忽略。

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
	"strings"
	"time"
)

const (
	// NotificationResourcesListChanged 是服务端资源列表变化的通知方法。
	NotificationResourcesListChanged = "notifications/resources/list_changed"
	// NotificationToolsListChanged 是服务端工具列表变化的通知方法。
	NotificationToolsListChanged = "notifications/tools/list_changed"
	// NotificationPromptsListChanged 是服务端提示列表变化的通知方法。
	NotificationPromptsListChanged = "notifications/prompts/list_changed"

	// notificationCloseGrace 是 Close 等待通知流 goroutine 退出的上限。
	notificationCloseGrace = 2 * time.Second
	// maxNotificationRetryDelay 是通知流重连退避的上限。
	maxNotificationRetryDelay = 5 * time.Second
)

// errNotificationStreamUnsupported 表示服务端不提供 GET 通知流（HTTP 404
// 之外的 4xx/5xx 语义：405/501）。此时客户端停止重连并报告一次。
var errNotificationStreamUnsupported = errors.New("mcp: server does not support the notification stream")

// notificationRetryDelay 是通知流首次重连前的退避。它是变量，便于测试加速；
// 生产代码不重新赋值。
var notificationRetryDelay = 200 * time.Millisecond

// Notification 是一条服务端发起的 JSON-RPC 通知。Params 是原始 JSON（无
// params 时为 nil）；客户端不对方法做白名单，便于服务端扩展。
type Notification struct {
	Method string
	Params json.RawMessage
}

// NotificationHandler 接收服务端通知。它在通知流的读取 goroutine 上同步
// 调用：必须快速返回，且不得阻塞（阻塞会延迟后续通知）。回调 panic 会被
// 恢复，并通过 NotificationErrorHandler 报告，通知流继续运行。
type NotificationHandler func(Notification)

// NotificationErrorHandler 接收通知流的非致命错误（连接失败、重连、不支持
// 通知流、回调 panic）。调用发生在通知流 goroutine 上；和通知回调一样不得
// 阻塞。
type NotificationErrorHandler func(error)

// notificationStreamer 是传输可选实现的接口：HTTP 传输用它启动 GET SSE
// 通知流。stdio 传输不实现它，通知能力只有远端传输才有。
type notificationStreamer interface {
	// startNotifications 启动后台通知流；幂等。deliver 在流 goroutine 上
	// 同步调用，report 接收非致命错误。
	startNotifications(deliver func(Notification), report func(error)) error
}

// OnNotification 注册服务端通知回调，并在 HTTP 传输上启动 GET SSE 通知流。
// 重复调用会替换回调（流只启动一次）；传 nil 只是停止分发，流保持打开直到
// Client.Close。stdio 传输下该方法不产生任何效果（服务端通知仍被忽略）。
//
// 通知流启动失败（例如服务端不支持 GET）不会使客户端不可用：错误交给
// OnNotificationError 注册的回调（未注册时忽略），普通请求/响应继续工作。
func (c *Client) OnNotification(handler NotificationHandler) {
	if c == nil {
		return
	}
	c.notificationMu.Lock()
	c.notificationHandler = handler
	if handler == nil || c.notificationStarted {
		c.notificationMu.Unlock()
		return
	}
	streamer, ok := c.transport.(notificationStreamer)
	if !ok {
		c.notificationMu.Unlock()
		return
	}
	c.notificationStarted = true
	c.notificationMu.Unlock()
	if err := streamer.startNotifications(c.dispatchNotification, c.reportNotificationError); err != nil {
		c.notificationMu.Lock()
		c.notificationStarted = false
		c.notificationMu.Unlock()
		c.reportNotificationError(err)
	}
}

// OnNotificationError 注册通知流的非致命错误回调。HTTP 传输在连接失败、
// 重连、服务端不支持通知流或通知回调 panic 时调用它。传 nil 取消回调。
func (c *Client) OnNotificationError(handler NotificationErrorHandler) {
	if c == nil {
		return
	}
	c.notificationMu.Lock()
	c.notificationErrorHandler = handler
	c.notificationMu.Unlock()
}

// dispatchNotification 把一条通知交给当前回调；回调 panic 被恢复并报告。
func (c *Client) dispatchNotification(notification Notification) {
	c.notificationMu.Lock()
	handler := c.notificationHandler
	c.notificationMu.Unlock()
	if handler == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			c.reportNotificationError(fmt.Errorf("mcp: notification handler panicked: %v", recovered))
		}
	}()
	handler(notification)
}

// reportNotificationError 把通知错误交给当前错误回调（未注册时忽略）。
func (c *Client) reportNotificationError(err error) {
	if err == nil {
		return
	}
	c.notificationMu.Lock()
	handler := c.notificationErrorHandler
	c.notificationMu.Unlock()
	if handler != nil {
		handler(err)
	}
}

// --- httpTransport 通知流实现 ----------------------------------------------

// notificationStream 是一次后台通知流。
type notificationStream struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startNotifications 启动 GET SSE 通知流（幂等）。它只允许在 initialize
// 之后调用，与 MCP Streamable HTTP 的顺序一致。
func (t *httpTransport) startNotifications(deliver func(Notification), report func(error)) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	closed, initialized := t.closed, t.initialized
	t.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if !initialized {
		return ErrNotInitialized
	}
	t.streamMu.Lock()
	if t.stream != nil {
		t.streamMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &notificationStream{cancel: cancel, done: make(chan struct{})}
	t.stream = stream
	t.streamMu.Unlock()
	go func() {
		defer close(stream.done)
		t.notificationLoop(ctx, deliver, report)
	}()
	return nil
}

// stopNotifications 取消通知流并等待其 goroutine 退出（最多
// notificationCloseGrace）。幂等。
func (t *httpTransport) stopNotifications() {
	t.streamMu.Lock()
	stream := t.stream
	t.stream = nil
	t.streamMu.Unlock()
	if stream == nil {
		return
	}
	stream.cancel()
	select {
	case <-stream.done:
	case <-time.After(notificationCloseGrace):
	}
}

// notificationLoop 维持通知流：连接、分发、断线后带 Last-Event-ID 重连，
// 直到 ctx 取消、传输关闭或服务端明确不支持。
func (t *httpTransport) notificationLoop(ctx context.Context, deliver func(Notification), report func(error)) {
	lastEventID := ""
	delay := notificationRetryDelay
	for {
		if ctx.Err() != nil || t.isClosed() {
			return
		}
		connected, err := t.readNotificationStream(ctx, &lastEventID, deliver)
		if connected {
			delay = notificationRetryDelay
		}
		if ctx.Err() != nil || t.isClosed() {
			return
		}
		if err != nil {
			report(err)
			if errors.Is(err, errNotificationStreamUnsupported) || errors.Is(err, ErrSessionExpired) {
				return
			}
		}
		// 正常断线（服务端关闭 SSE）或可恢复错误：退避后带 Last-Event-ID
		// 重连，服务端据此补发错过的事件。
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, maxNotificationRetryDelay)
	}
}

// readNotificationStream 读取一次 GET SSE 连接，直到 EOF/错误/ctx 取消。
// connected 报告 HTTP 连接是否成功建立（成功后重置重连退避）；lastEventID
// 在读到 id: 字段时更新，供下一次重连携带。
func (t *httpTransport) readNotificationStream(ctx context.Context, lastEventID *string, deliver func(Notification)) (connected bool, err error) {
	request, err := t.newNotificationRequest(ctx, *lastEventID)
	if err != nil {
		return false, err
	}
	resp, err := t.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, fmt.Errorf("mcp: open notification stream: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, httpErrorBodyMax))
		return false, fmt.Errorf("%w: %s", errNotificationStreamUnsupported, resp.Status)
	case http.StatusNotFound, http.StatusGone:
		return false, fmt.Errorf("%w: %s", ErrSessionExpired, resp.Status)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, fmt.Errorf("mcp: notification stream failed: %s: %s", resp.Status, readErrorSnippet(resp.Body))
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !strings.EqualFold(mediaType, "text/event-stream") {
		return false, fmt.Errorf("mcp: notification stream returned Content-Type %q; expected text/event-stream",
			resp.Header.Get("Content-Type"))
	}
	return true, parseSSEStream(resp.Body, func(frame sseFrame) {
		if frame.id != "" {
			*lastEventID = frame.id
		}
		// event 字段为空或 "message" 是 JSON-RPC 消息事件；其他命名事件
		// （例如服务端自定义事件）留给未来扩展，这里忽略。
		if frame.event != "" && frame.event != "message" {
			return
		}
		payload := strings.TrimSpace(frame.data)
		if payload == "" || payload == "[DONE]" {
			return
		}
		var envelope responseEnvelope
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			return // 容忍畸形帧；下一帧仍可能有效
		}
		if envelope.Method == "" {
			return // 响应由 POST 响应流处理，通知流不消费
		}
		if len(bytes.TrimSpace(envelope.ID)) > 0 {
			return // 服务端→客户端请求（sampling/roots/elicitation）不在本子集内
		}
		deliver(Notification{
			Method: envelope.Method,
			Params: append(json.RawMessage(nil), envelope.Params...),
		})
	})
}

// newNotificationRequest 构建一次 GET SSE 请求，携带会话/协议/宿主自定义头
// 与可选的 Last-Event-ID。
func (t *httpTransport) newNotificationRequest(ctx context.Context, lastEventID string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: build notification request: %w", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cache-Control", "no-cache")

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, ErrClosed
	}
	sessionID, protocolVersion := t.sessionID, t.protocolVersion
	overrides := t.headers
	t.mu.Unlock()
	if sessionID != "" {
		request.Header.Set(headerSessionID, sessionID)
	}
	if protocolVersion != "" {
		request.Header.Set(headerProtocolVersion, protocolVersion)
	}
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	for name, value := range overrides {
		request.Header.Set(name, value)
	}
	return request, nil
}

// sseFrame 是一个已完成的 SSE 事件帧。
type sseFrame struct {
	event string
	data  string
	id    string
}

// parseSSEStream 按 SSE 规范增量解析事件流，每个空行结束一帧并调用
// dispatch。以 ':' 开头的行为注释；字段名与值在第一个 ':' 处分割；同一帧的
// 多个 data: 行用换行连接。EOF 时若还有未派发字段则补发一帧，容忍没有末尾
// 空行的服务端。
func parseSSEStream(body io.Reader, dispatch func(sseFrame)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxMessageBytes)
	var (
		frame sseFrame
		data  []string
	)
	flush := func() {
		if len(data) > 0 || frame.event != "" || frame.id != "" {
			frame.data = strings.Join(data, "\n")
			dispatch(frame)
		}
		frame = sseFrame{}
		data = data[:0]
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			frame.event = value
		case "data":
			data = append(data, value)
		case "id":
			if !strings.ContainsRune(value, 0) {
				frame.id = value
			}
		case "retry":
			// 服务端重连提示：本实现使用自己的退避策略，忽略。
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

// deleteSession 尽力发送 HTTP DELETE 结束服务端会话（携带 Mcp-Session-Id）。
// 无状态服务端（没有会话 id）不发请求；错误被忽略，因为关闭不应因清理而
// 失败。
func (t *httpTransport) deleteSession(sessionID string) {
	if t == nil || sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notificationCloseGrace)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
	if err != nil {
		return
	}
	request.Header.Set("Accept", "application/json, text/event-stream")

	t.mu.Lock()
	protocolVersion := t.protocolVersion
	overrides := t.headers
	t.mu.Unlock()
	request.Header.Set(headerSessionID, sessionID)
	if protocolVersion != "" {
		request.Header.Set(headerProtocolVersion, protocolVersion)
	}
	for name, value := range overrides {
		request.Header.Set(name, value)
	}
	resp, err := t.client.Do(request)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, httpNotificationDrainMax))
}
