package mcp

// P2-7 通知流测试：GET SSE 帧解析与通知分发、Last-Event-ID 断线恢复、
// 不支持通知流的服务端兼容、Close 的 session DELETE，以及通知流与
// 请求/响应并发互不阻塞。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// notificationServerState 记录假服务端观察到的通知流/删除请求。
type notificationServerState struct {
	mu               sync.Mutex
	getCount         int
	deleteCount      int
	lastEventIDs     []string
	deleteSessionIDs []string
	getHeaders       []http.Header
}

// snapshot 返回计数的并发安全快照。
func (s *notificationServerState) snapshot() (getCount, deleteCount int, lastEventIDs, deleteSessionIDs []string, headers []http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCount, s.deleteCount,
		append([]string(nil), s.lastEventIDs...),
		append([]string(nil), s.deleteSessionIDs...),
		append([]http.Header(nil), s.getHeaders...)
}

// newNotificationServer 启动一个同时支持 JSON-RPC POST、GET SSE 通知流与
// DELETE 会话的假 MCP 服务端。onStream 为 nil 时 GET 返回 405。
func newNotificationServer(t *testing.T, onStream func(w http.ResponseWriter, r *http.Request, attempt int)) (*notificationServerState, string) {
	t.Helper()
	state := &notificationServerState{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			state.mu.Lock()
			state.getCount++
			attempt := state.getCount
			state.lastEventIDs = append(state.lastEventIDs, r.Header.Get("Last-Event-ID"))
			state.getHeaders = append(state.getHeaders, r.Header.Clone())
			state.mu.Unlock()
			if onStream == nil {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			onStream(w, r, attempt)
		case http.MethodDelete:
			state.mu.Lock()
			state.deleteCount++
			state.deleteSessionIDs = append(state.deleteSessionIDs, r.Header.Get(headerSessionID))
			state.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			msg := readHTTPRPCRequest(t, r)
			switch msg.Method {
			case "initialize":
				w.Header().Set(headerSessionID, httpTestSession)
				writeHTTPRPCResult(t, w, msg.ID, httpInitializeResult())
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			case "tools/list":
				writeHTTPRPCResult(t, w, msg.ID, map[string]any{"tools": []any{httpToolJSON("echo", "Echo text")}})
			case "ping":
				writeHTTPRPCResult(t, w, msg.ID, map[string]any{})
			default:
				writeHTTPRPCError(t, w, msg.ID, -32601, "method not found", nil)
			}
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
	return state, newHTTPTestServer(t, handler)
}

// startNotificationClient 启动一个 HTTP MCP client 并注册清理。
func startNotificationClient(t *testing.T, url string) *Client {
	t.Helper()
	client, err := StartHTTP(context.Background(), HTTPConfig{URL: url, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("StartHTTP: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// writeSSEEvent 写一个带可选 event/id 的 SSE data 帧并 flush。
func writeSSEEvent(t *testing.T, w http.ResponseWriter, id, event, data string) {
	t.Helper()
	if _, err := io.WriteString(w, buildSSEEvent(id, event, data)); err != nil {
		t.Errorf("write SSE event: %v", err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	} else {
		t.Errorf("ResponseWriter is not a Flusher")
	}
}

// buildSSEEvent 渲染一个 SSE 帧；空字段被省略。
func buildSSEEvent(id, event, data string) string {
	var builder strings.Builder
	if event != "" {
		fmt.Fprintf(&builder, "event: %s\n", event)
	}
	if id != "" {
		fmt.Fprintf(&builder, "id: %s\n", id)
	}
	fmt.Fprintf(&builder, "data: %s\n\n", data)
	return builder.String()
}

// notifications 收集通知直到收到 want 条或超时。
func collectNotifications(t *testing.T, channel <-chan Notification, want int) []Notification {
	t.Helper()
	collected := make([]Notification, 0, want)
	deadline := time.After(5 * time.Second)
	for len(collected) < want {
		select {
		case notification := <-channel:
			collected = append(collected, notification)
		case <-deadline:
			t.Fatalf("received %d/%d notifications: %+v", len(collected), want, collected)
		}
	}
	return collected
}

func TestHTTPNotificationStreamDispatches(t *testing.T) {
	state, url := newNotificationServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSEEvent(t, w, "", "message", `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
		writeSSEEvent(t, w, "evt-1", "message",
			`{"jsonrpc":"2.0","method":"notifications/resources/list_changed","params":{"uri":"file:///a.txt"}}`)
		// 没有 event: 字段的 data 帧同样按 JSON-RPC 通知处理。
		writeSSEEvent(t, w, "evt-2", "", `{"jsonrpc":"2.0","method":"notifications/prompts/list_changed"}`)
		// 这些帧必须被忽略：POST 响应、服务端→客户端请求、畸形 JSON。
		writeSSEEvent(t, w, "", "message", `{"jsonrpc":"2.0","id":41,"result":{}}`)
		writeSSEEvent(t, w, "", "message", `{"jsonrpc":"2.0","id":42,"method":"sampling/createMessage","params":{}}`)
		writeSSEEvent(t, w, "", "message", `{not json`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// 保持通知流打开，直到客户端关闭连接。
		<-r.Context().Done()
	})

	client := startNotificationClient(t, url)
	notifications := make(chan Notification, 8)
	client.OnNotification(func(notification Notification) { notifications <- notification })

	received := collectNotifications(t, notifications, 3)
	wantMethods := []string{
		NotificationToolsListChanged,
		NotificationResourcesListChanged,
		NotificationPromptsListChanged,
	}
	for index, want := range wantMethods {
		if received[index].Method != want {
			t.Fatalf("notification %d method = %q, want %q", index, received[index].Method, want)
		}
	}
	if got := string(received[1].Params); !strings.Contains(got, "file:///a.txt") {
		t.Fatalf("resources notification params = %q", got)
	}
	// 不支持的帧不会被分发。
	select {
	case unexpected := <-notifications:
		t.Fatalf("unexpected extra notification: %+v", unexpected)
	case <-time.After(100 * time.Millisecond):
	}

	getCount, _, lastEventIDs, _, headers := state.snapshot()
	if getCount != 1 {
		t.Fatalf("GET count = %d, want 1", getCount)
	}
	if got := headers[0].Get("Accept"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("notification GET Accept = %q", got)
	}
	if got := headers[0].Get(headerSessionID); got != httpTestSession {
		t.Fatalf("notification GET session header = %q", got)
	}
	if got := headers[0].Get(headerProtocolVersion); got != httpTestProtocol {
		t.Fatalf("notification GET protocol header = %q", got)
	}
	if lastEventIDs[0] != "" {
		t.Fatalf("first GET Last-Event-ID = %q, want empty", lastEventIDs[0])
	}

	// Close 取消通知流并发送一次 DELETE；重复 Close 不再发送。
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, deleteCount, _, deleteSessionIDs, _ := state.snapshot()
	if deleteCount != 1 {
		t.Fatalf("DELETE count = %d, want 1", deleteCount)
	}
	if deleteSessionIDs[0] != httpTestSession {
		t.Fatalf("DELETE session header = %q, want %q", deleteSessionIDs[0], httpTestSession)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, deleteCount, _, _, _ = state.snapshot(); deleteCount != 1 {
		t.Fatalf("second Close sent another DELETE: count = %d", deleteCount)
	}
}

func TestHTTPNotificationStreamResumesWithLastEventID(t *testing.T) {
	previous := notificationRetryDelay
	notificationRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { notificationRetryDelay = previous })

	state, url := newNotificationServer(t, func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		switch attempt {
		case 1:
			// 发一个事件后主动断开连接，触发带 Last-Event-ID 的重连。
			writeSSEEvent(t, w, "7", "message",
				`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
		default:
			writeSSEEvent(t, w, "8", "message",
				`{"jsonrpc":"2.0","method":"notifications/resources/list_changed"}`)
			<-r.Context().Done()
		}
	})

	client := startNotificationClient(t, url)
	notifications := make(chan Notification, 8)
	client.OnNotification(func(notification Notification) { notifications <- notification })
	received := collectNotifications(t, notifications, 2)
	if received[0].Method != NotificationToolsListChanged || received[1].Method != NotificationResourcesListChanged {
		t.Fatalf("resumed notifications = %+v", received)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		getCount, _, lastEventIDs, _, _ := state.snapshot()
		if getCount >= 2 {
			if lastEventIDs[0] != "" {
				t.Fatalf("first GET Last-Event-ID = %q, want empty", lastEventIDs[0])
			}
			if lastEventIDs[1] != "7" {
				t.Fatalf("resumed GET Last-Event-ID = %q, want 7", lastEventIDs[1])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d GET attempts observed", getCount)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHTTPNotificationStreamUnsupportedServer(t *testing.T) {
	// 服务端不支持 GET 通知流：报告一次错误、不重试，普通请求继续可用。
	state, url := newNotificationServer(t, nil)
	client := startNotificationClient(t, url)
	errors := make(chan error, 4)
	client.OnNotificationError(func(err error) { errors <- err })
	client.OnNotification(func(Notification) {})

	select {
	case err := <-errors:
		if !strings.Contains(err.Error(), "does not support") {
			t.Fatalf("notification error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no notification error for an unsupported stream")
	}
	time.Sleep(100 * time.Millisecond)
	if getCount, _, _, _, _ := state.snapshot(); getCount != 1 {
		t.Fatalf("GET count = %d, want no retries", getCount)
	}
	// 请求/响应路径不受影响。
	tools, err := client.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %+v, %v", tools, err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, deleteCount, _, _, _ := state.snapshot(); deleteCount != 1 {
		t.Fatalf("DELETE count = %d, want 1", deleteCount)
	}
}

func TestHTTPNoNotificationStreamWithoutHandler(t *testing.T) {
	// 没有注册通知回调时不打开 GET 流（兼容不支持订阅的服务端）。
	state, url := newNotificationServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})
	client := startNotificationClient(t, url)
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if getCount, _, _, _, _ := state.snapshot(); getCount != 0 {
		t.Fatalf("GET count = %d, want 0 without a handler", getCount)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 有状态服务端即使没有通知流也在 Close 时收到 DELETE。
	if _, deleteCount, _, _, _ := state.snapshot(); deleteCount != 1 {
		t.Fatalf("DELETE count = %d, want 1", deleteCount)
	}
}

func TestHTTPNotificationStreamDoesNotBlockRequests(t *testing.T) {
	state, url := newNotificationServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	client := startNotificationClient(t, url)
	client.OnNotification(func(Notification) {})

	// 等通知流建立，然后并发发请求：两者使用独立连接，不应互相阻塞。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if getCount, _, _, _, _ := state.snapshot(); getCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notification stream was not opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tools, err := client.ListTools(ctx)
			if err == nil && len(tools) != 1 {
				err = fmt.Errorf("tools = %+v", tools)
			}
			errs[index] = err
		}(index)
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent ListTools #%d: %v", index, err)
		}
	}
}

func TestHTTPNotificationStreamStatelessServerNoDelete(t *testing.T) {
	// 无状态服务端（initialize 不返回会话 id）在 Close 时不应发送 DELETE。
	var (
		mu          sync.Mutex
		deleteCount int
	)
	server := newHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			mu.Lock()
			deleteCount++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			msg := readHTTPRPCRequest(t, r)
			if msg.Method == "initialize" {
				writeHTTPRPCResult(t, w, msg.ID, httpInitializeResult())
				return
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	client := startNotificationClient(t, server)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deleteCount != 0 {
		t.Fatalf("DELETE count = %d, want 0 for a stateless server", deleteCount)
	}
}
