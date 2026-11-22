package terminal_test

// 跨 owner 围栏：terminal_send 早就只接受"自己 Agent 的会话"，但 signal 与 close
// 曾能操作别人的会话（read-only 的 Agent 能给另一个 Agent 的会话投 SIGINT）。
// 口径与 terminal_send 一致：会话池可见（terminal_list/terminal_read 跨 Agent），
// 但**改变状态的写入**必须同 owner——一个会话只能由创建它的 Agent 改信号/关闭。

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

// anotherOwnerDenial 断言拒绝文案说明"会话归另一个 Agent，只有创建它的 Agent 能
// 改信号/关闭"。
func anotherOwnerDenial(t *testing.T, tool, content string) {
	t.Helper()
	for _, fragment := range []string{"another agent", "created it"} {
		if !strings.Contains(content, fragment) {
			t.Fatalf("%s 的拒绝文案缺少 %q: %q", tool, fragment, content)
		}
	}
}

func TestTerminalSignalRejectsCrossOwnerSession(t *testing.T) {
	manager := newOwnedTerminalManager(t)
	alice := openOwned(t, manager, "a1")

	bobSignal := toolForOwner(t, manager, "a2", terminal.ToolTerminalSignal)
	result := invokeTool(t, bobSignal, `{"sessionId":"`+alice.ID+`","signal":"SIGINT"}`)
	if !result.IsError {
		t.Fatalf("a2 给 a1 的会话投 SIGINT 成功了（跨 owner 写入通道）: %q", result.Content)
	}
	anotherOwnerDenial(t, "terminal_signal", result.Content)
	// 会话本身没被改动：仍在池里且还在跑。
	if info, ok := findSession(manager, alice.ID); !ok || !info.Running {
		t.Fatalf("被拒的跨 owner signal 改动了会话状态: %+v ok=%v", info, ok)
	}
	// 共享池的可见性不变：a2 仍能列出 a1 的会话（读不改变状态）。
	var list rig.Tool
	for _, tool := range manager.ToolsWithListFor("a2") {
		if tool.Name() == terminal.ToolTerminalList {
			list = tool
		}
	}
	if list == nil {
		t.Fatal("a2 的工具集里没有 terminal_list")
	}
	listed := invokeTool(t, list, `{}`)
	if listed.IsError || !strings.Contains(listed.Content, alice.ID) {
		t.Fatalf("terminal_list 看不到共享池里的会话（可见性被误改）: %q", listed.Content)
	}
}

func TestTerminalSignalAllowsSameOwner(t *testing.T) {
	manager := newOwnedTerminalManager(t)
	alice := openOwned(t, manager, "a1")
	signal := toolForOwner(t, manager, "a1", terminal.ToolTerminalSignal)
	result := invokeTool(t, signal, `{"sessionId":"`+alice.ID+`","signal":"SIGINT"}`)
	if result.IsError {
		t.Fatalf("a1 给自己的会话投信号被拒: %q", result.Content)
	}
}

func TestTerminalCloseRejectsCrossOwnerSession(t *testing.T) {
	manager := newOwnedTerminalManager(t)
	alice := openOwned(t, manager, "a1")

	bobClose := toolForOwner(t, manager, "a2", terminal.ToolTerminalClose)
	result := invokeTool(t, bobClose, `{"sessionId":"`+alice.ID+`"}`)
	if !result.IsError {
		t.Fatalf("a2 关掉了 a1 的会话（跨 owner 写入通道）: %q", result.Content)
	}
	anotherOwnerDenial(t, "terminal_close", result.Content)
	if info, ok := findSession(manager, alice.ID); !ok || !info.Running {
		t.Fatalf("被拒的跨 owner close 移除了会话: %+v ok=%v", info, ok)
	}
}

func TestTerminalCloseAllowsSameOwner(t *testing.T) {
	manager := newOwnedTerminalManager(t)
	alice := openOwned(t, manager, "a1")
	closeTool := toolForOwner(t, manager, "a1", terminal.ToolTerminalClose)
	result := invokeTool(t, closeTool, `{"sessionId":"`+alice.ID+`"}`)
	if result.IsError {
		t.Fatalf("a1 关不掉自己的会话: %q", result.Content)
	}
	if _, ok := findSession(manager, alice.ID); ok {
		t.Fatalf("同 owner 的 terminal_close 没有移除会话 %s", alice.ID)
	}
}
