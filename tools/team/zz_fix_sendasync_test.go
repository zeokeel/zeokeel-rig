package team

// zz_fix_sendasync_test.go —— 修复 send_message 在队友运行期间阻塞的回归测试。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// 队友正在跑（同步 Send 占住成员）时，SendAsync 必须仍然立刻返回。
func TestSendAsyncDoesNotBlockWhileMemberRuns(t *testing.T) {
	runner := newFakeRunner()
	m := NewManager(runner)
	if _, err := m.Spawn(context.Background(), rig.SubagentSpec{Name: "mate", Prompt: "work"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	sub := runner.subagents()[0]
	block := make(chan struct{})
	sub.mu.Lock()
	sub.sendBlock = block
	sub.mu.Unlock()

	// 用一次同步 Send 占住成员：它就是"队友正在运行"的状态。
	busyDone := make(chan struct{})
	go func() {
		defer close(busyDone)
		_, _ = m.Send(context.Background(), "mate", "running")
	}()
	select {
	case <-sub.sendGate:
	case <-time.After(2 * time.Second):
		t.Fatal("同步 Send 没有进入 fake subagent")
	}

	for i := 0; i < 5; i++ {
		done := make(chan error, 1)
		go func() { done <- m.SendAsync(context.Background(), "mate", fmt.Sprintf("msg-%d", i)) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("SendAsync #%d: %v", i, err)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("SendAsync #%d 在队友运行期间阻塞超过 100ms", i)
		}
	}

	close(block)
	<-busyDone
}

// SendAsync 必须尊重调用方 ctx：已取消的 ctx 立刻返回错误，不投递。
func TestSendAsyncHonoursCallerContext(t *testing.T) {
	runner := newFakeRunner()
	m := NewManager(runner)
	if _, err := m.Spawn(context.Background(), rig.SubagentSpec{Name: "mate", Prompt: "work"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.SendAsync(ctx, "mate", "hello"); err == nil {
		t.Fatal("已取消的 ctx 必须返回错误")
	}
}
