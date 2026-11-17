package mcpserver_test

// 审计：生命周期与并发（panic 隔离、双 Serve、HandleLine 与 Serve 并行、取消后的
// 在途表清理、Serve 期间的日志通知）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// TestAuditLifecycle_PanicDoesNotStopServing 证明工具 panic 只影响该次调用，
// Serve 继续服务后续请求。
func TestAuditLifecycle_PanicDoesNotStopServing(t *testing.T) {
	tool := &fakeTool{
		name:   "boom",
		schema: map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			panic("audit panic")
		},
	}
	server := newServer(t, tool, echoTool("echo"))
	var out lockedBuilder
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}` + "\n")
	if err := server.Serve(context.Background(), input, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	// 响应由各自的 goroutine 写出，顺序不保证：按 id 索引。
	byID := make(map[float64]string, 3)
	lines := nonEmptyLines(out.String())
	if len(lines) != 3 {
		t.Fatalf("responses = %d, want 3\n%s", len(lines), out.String())
	}
	for _, line := range lines {
		var envelope struct {
			ID *float64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil || envelope.ID == nil {
			t.Fatalf("bad response line: %v (%s)", err, truncateForLog(line))
		}
		byID[*envelope.ID] = line
	}
	if !strings.Contains(byID[1], "panic") || !strings.Contains(byID[1], `"isError":true`) {
		t.Fatalf("panic response = %s, want an isError result mentioning the panic", truncateForLog(byID[1]))
	}
	if !strings.Contains(byID[2], `"result"`) {
		t.Fatalf("ping after panic = %s, want a normal result", truncateForLog(byID[2]))
	}
	if !strings.Contains(byID[3], "echo|") {
		t.Fatalf("call after panic = %s, want the tool output", truncateForLog(byID[3]))
	}
}

// TestAuditLifecycle_SecondServeRejected 证明同一 server 上并发第二次 Serve 被拒绝，
// 且第一次不受影响。
func TestAuditLifecycle_SecondServeRejected(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	inputReader, inputWriter := io.Pipe()
	var first lockedBuilder
	firstDone := make(chan error, 1)
	go func() { firstDone <- server.Serve(context.Background(), inputReader, &first) }()

	// 先证明第一次 Serve 真的在跑（一次 ping 往返），再发起第二次。
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(first.String(), `"result"`) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(first.String(), `"result"`) {
		t.Fatalf("first Serve never answered a ping: %s", first.String())
	}

	var second lockedBuilder
	err := server.Serve(context.Background(), strings.NewReader(""), &second)
	if err == nil {
		t.Fatal("second concurrent Serve was accepted")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second Serve error = %v, want an `already running` error", err)
	}

	// 被拒绝之后，第一次 Serve 仍然可用。
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":2,"method":"ping"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && strings.Count(first.String(), `"result"`) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if strings.Count(first.String(), `"result"`) < 2 {
		t.Fatalf("first Serve stopped working after a rejected second Serve: %s", first.String())
	}
	_ = inputWriter.Close()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first Serve returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first Serve did not return after EOF")
	}
}

// TestAuditLifecycle_HandleLineConcurrentWithServe 证明 HandleLine 与 Serve 并行
// 没有数据竞争（配合 -race），且两边都能正常应答。
func TestAuditLifecycle_HandleLineConcurrentWithServe(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	inputReader, inputWriter := io.Pipe()
	var served lockedBuilder
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background(), inputReader, &served) }()

	var wait sync.WaitGroup
	var failures atomic.Int32
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < 10; index++ {
				line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"echo"}}`, worker*100+index)
				response, handled, err := server.HandleLine(context.Background(), []byte(line))
				if err != nil || !handled || !strings.Contains(string(response), `"result"`) {
					failures.Add(1)
				}
			}
		}(worker)
	}
	wait.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent HandleLine calls failed", failures.Load())
	}
	_ = inputWriter.Close()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
}

// TestAuditLifecycle_CancelThenReuseSameID 证明取消后同一 id 可以重新使用
// （在途表已清理，不会把新请求误取消）。
func TestAuditLifecycle_CancelThenReuseSameID(t *testing.T) {
	var calls atomic.Int32
	blocked := make(chan struct{}, 2)
	tool := &fakeTool{
		name:   "slow",
		schema: map[string]any{"type": "object"},
		invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			if calls.Add(1) == 1 {
				blocked <- struct{}{}
				<-ctx.Done()
				return rig.ToolResult{Content: "first cancelled", IsError: true}, nil
			}
			return rig.ToolResult{Content: "second ran"}, nil
		},
	}
	server := newServer(t, tool)
	inputReader, inputWriter := io.Pipe()
	var out lockedBuilder
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), inputReader, &out) }()

	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":"same","method":"tools/call","params":{"name":"slow"}}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("first call never started")
	}
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"same"}}`); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":"same","method":"tools/call","params":{"name":"slow"}}`); err != nil {
		t.Fatalf("write second: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), "second ran") {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "second ran") {
		t.Fatalf("reused id did not run after cancellation: %s", out.String())
	}
	_ = inputWriter.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
}

// TestAuditLifecycle_NotifyLogDuringServe 证明 Serve 期间并发写日志通知是安全的
// （输出被 writeMu 串行化），且不影响请求响应。
func TestAuditLifecycle_NotifyLogDuringServe(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	inputReader, inputWriter := io.Pipe()
	var out lockedBuilder
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), inputReader, &out) }()

	// 先用一次 ping 往返确认 Serve 已 attach（否则 NotifyLog 会在 attach 前失败）。
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), `"result"`) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(out.String(), `"result"`) {
		t.Fatalf("Serve never answered the ping: %s", out.String())
	}

	var wait sync.WaitGroup
	var failures atomic.Int32
	for worker := 0; worker < 4; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < 10; index++ {
				if err := server.NotifyLog("", fmt.Sprintf("log-%d-%d", worker, index)); err != nil {
					failures.Add(1)
				}
			}
		}(worker)
	}
	wait.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d NotifyLog calls failed", failures.Load())
	}
	_ = inputWriter.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
	lines := nonEmptyLines(out.String())
	notifications := 0
	for _, line := range lines {
		if strings.Contains(line, "notifications/message") {
			notifications++
			continue
		}
		if !strings.Contains(line, `"result"`) {
			t.Fatalf("unexpected output line: %s", truncateForLog(line))
		}
	}
	if notifications != 40 {
		t.Fatalf("notifications = %d, want 40 (lines=%d)", notifications, len(lines))
	}
}
