package terminal_test

// Owner 作用域测试：会话记录管理器/打开时的 owner 标签，terminal_open 与
// terminal_list 会传播它，Manager.CloseAllOwned 只清理目标 owner 的会话。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

// newOwnedTerminalManager 创建一个强制 pipe 传输的终端管理器。
func newOwnedTerminalManager(t *testing.T, opts ...terminal.Option) *terminal.Manager {
	t.Helper()
	options := append([]terminal.Option{terminal.WithTransport(terminal.TransportPipe)}, opts...)
	manager := terminal.NewManager(options...)
	t.Cleanup(func() { _ = manager.CloseAll() })
	return manager
}

func openOwned(t *testing.T, manager *terminal.Manager, owner string) terminal.SessionInfo {
	t.Helper()
	info, err := manager.Open(terminal.OpenOptions{Type: "pipe", Owner: owner})
	if err != nil {
		t.Fatalf("Open(owner=%q): %v", owner, err)
	}
	if info.Owner != owner {
		t.Fatalf("Open(owner=%q).Owner = %q", owner, info.Owner)
	}
	if !info.Running || info.Transport != "pipe" {
		t.Fatalf("Open(owner=%q) = %+v", owner, info)
	}
	return info
}

func TestOwnerScopeCleanupOnlyTarget(t *testing.T) {
	manager := newOwnedTerminalManager(t)
	aliceFirst := openOwned(t, manager, "alice")
	aliceSecond := openOwned(t, manager, "alice")
	bob := openOwned(t, manager, "bob")
	unowned := openOwned(t, manager, "")
	if manager.Len() != 4 {
		t.Fatalf("Len = %d, want 4", manager.Len())
	}

	closed, err := manager.CloseAllOwned("alice")
	if err != nil {
		t.Fatalf("CloseAllOwned(alice): %v", err)
	}
	if closed != 2 {
		t.Fatalf("CloseAllOwned(alice) = %d, want 2", closed)
	}
	if manager.Len() != 2 {
		t.Fatalf("Len after alice cleanup = %d, want 2", manager.Len())
	}
	for _, know := range []struct {
		id    string
		owner string
	}{{bob.ID, "bob"}, {unowned.ID, ""}} {
		info, ok := findSession(manager, know.id)
		if !ok || info.Owner != know.owner {
			t.Fatalf("surviving session %s = %+v ok=%v, want owner %q", know.id, info, ok, know.owner)
		}
	}
	// 重复清理同一 owner 是幂等的。
	if closed, err = manager.CloseAllOwned("alice"); err != nil || closed != 0 {
		t.Fatalf("second CloseAllOwned(alice) = %d, %v", closed, err)
	}
	// 目标外的会话保持可用：向 bob 发送命令。
	bobTool := toolByName(t, manager, terminal.ToolTerminalSend)
	result := invokeTool(t, bobTool, `{"sessionId":"`+bob.ID+`","text":"echo bob-alive"}`)
	if result.IsError || !strings.Contains(result.Content, "bob-alive") {
		t.Fatalf("bob session after alice cleanup = %q (IsError=%v)", result.Content, result.IsError)
	}
	if _, ok := findSession(manager, aliceFirst.ID); ok {
		t.Fatalf("closed session %s is still listed", aliceFirst.ID)
	}
	if _, ok := findSession(manager, aliceSecond.ID); ok {
		t.Fatalf("closed session %s is still listed", aliceSecond.ID)
	}

	if closed, err = manager.CloseAllOwned("bob"); err != nil || closed != 1 {
		t.Fatalf("CloseAllOwned(bob) = %d, %v", closed, err)
	}
	if closed, err = manager.CloseAllOwned(""); err != nil || closed != 1 {
		t.Fatalf(`CloseAllOwned("") = %d, %v`, closed, err)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len = %d, want 0", manager.Len())
	}
}

// findSession 在 Sessions() 中按 id 查找会话。
func findSession(manager *terminal.Manager, id string) (terminal.SessionInfo, bool) {
	for _, info := range manager.Sessions() {
		if info.ID == id {
			return info, true
		}
	}
	return terminal.SessionInfo{}, false
}

// toolByName 从管理器的工具集中取一个工具。
func toolByName(t *testing.T, manager *terminal.Manager, name string) rig.Tool {
	t.Helper()
	for _, tool := range manager.ToolsWithList() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

// invokeTool 调用一个已绑定工具。
func invokeTool(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", tool.Name(), err)
	}
	return result
}

func TestOwnerDefaultPropagation(t *testing.T) {
	manager := newOwnedTerminalManager(t, terminal.WithOwner("alice"))
	if got := manager.Owner(); got != "alice" {
		t.Fatalf("Owner() = %q, want alice", got)
	}
	// 工具路径打开的会话继承管理器默认 owner，并在打开结果与列表中传播。
	opened := invokeTool(t, toolByName(t, manager, terminal.ToolTerminalOpen), `{"type":"pipe","name":"main"}`)
	if opened.IsError {
		t.Fatalf("terminal_open: %q", opened.Content)
	}
	for _, fragment := range []string{"name: main", "owner: alice"} {
		if !strings.Contains(opened.Content, fragment) {
			t.Fatalf("terminal_open output missing %q: %q", fragment, opened.Content)
		}
	}
	sessions := manager.Sessions()
	if len(sessions) != 1 || sessions[0].Owner != "alice" || sessions[0].Name != "main" {
		t.Fatalf("Sessions() = %+v", sessions)
	}
	listed := invokeTool(t, toolByName(t, manager, terminal.ToolTerminalList), `{}`)
	if listed.IsError || !strings.Contains(listed.Content, "owner: alice") {
		t.Fatalf("terminal_list = %q (IsError=%v)", listed.Content, listed.IsError)
	}
}

func TestOwnerEmptyScopeCleanup(t *testing.T) {
	// 显式 owner 为空时，CloseAllOwned("") 只清理无 owner 的会话。
	manager := newOwnedTerminalManager(t)
	unowned := openOwned(t, manager, "")
	owned := openOwned(t, manager, "bob")
	closed, err := manager.CloseAllOwned("")
	if err != nil || closed != 1 {
		t.Fatalf(`CloseAllOwned("") = %d, %v`, closed, err)
	}
	if manager.Len() != 1 {
		t.Fatalf("Len = %d, want the bob session only", manager.Len())
	}
	if _, ok := findSession(manager, unowned.ID); ok {
		t.Fatal("unowned session survived CloseAllOwned(\"\")")
	}
	if _, ok := findSession(manager, owned.ID); !ok {
		t.Fatal("owned session was removed by the empty-owner cleanup")
	}
}

func TestOwnerScopeConcurrent(t *testing.T) {
	manager := newOwnedTerminalManager(t)
	const count = 8
	infos := make([]terminal.SessionInfo, count)
	var wg sync.WaitGroup
	for index := 0; index < count; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			owner := "alice"
			if index%2 == 1 {
				owner = "bob"
			}
			info, err := manager.Open(terminal.OpenOptions{Type: "pipe", Owner: owner})
			if err != nil {
				t.Errorf("Open #%d: %v", index, err)
				return
			}
			infos[index] = info
		}(index)
	}
	wg.Wait()

	var closedAlice, closedBob int
	var errAlice, errBob error
	wg.Add(2)
	go func() {
		defer wg.Done()
		closedAlice, errAlice = manager.CloseAllOwned("alice")
	}()
	go func() {
		defer wg.Done()
		closedBob, errBob = manager.CloseAllOwned("bob")
	}()
	wg.Wait()
	if errAlice != nil || errBob != nil {
		t.Fatalf("concurrent CloseAllOwned errors: %v / %v", errAlice, errBob)
	}
	if closedAlice != count/2 || closedBob != count/2 {
		t.Fatalf("closed alice/bob = %d/%d, want %d/%d", closedAlice, closedBob, count/2, count/2)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len = %d, want 0", manager.Len())
	}

	// 打开与作用域清理并发不会死锁或报告竞态。
	for round := 0; round < 4; round++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for index := 0; index < 3; index++ {
				_, _ = manager.Open(terminal.OpenOptions{Type: "pipe", Owner: "alice"})
			}
		}()
		go func() {
			defer wg.Done()
			_, _ = manager.CloseAllOwned("alice")
		}()
		wg.Wait()
	}
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len after CloseAll = %d, want 0", manager.Len())
	}
}
