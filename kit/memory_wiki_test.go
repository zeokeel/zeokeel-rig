package kit_test

// 记忆接线（`kit.Config.Memory`）的用例：
//   - 记忆默认**常驻**（memory 工具在 active 集合里，不是"按需激活"）；
//   - nil 时按工作区自动开一个文件 wiki（用户级状态目录，不写进仓库）；
//   - 同一工作区跨进程/跨实例可续用，不同工作区互不可见；
//   - 显式注入的 Wiki 优先。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
)

// memoryCall 直接调用某个 agent 装配里的 memory 工具。
func memoryCall(t *testing.T, a *kit.Agent, argsJSON string) rig.ToolResult {
	t.Helper()
	// kit 的 Agent 实现了 rig.Agent；工具集通过 Agent.Tools() 暴露名字。这里借用
	// 底层的 Wiki 直接验证存储，再用一次真实 Invoke 验证工具可用。
	wiki := a.MemoryWiki()
	if wiki == nil {
		t.Fatal("Agent.MemoryWiki() 为 nil：记忆没有接线")
	}
	tool := memory.Tools(wiki)[0]
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: memory.ToolName, Arguments: argsJSON})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return result
}

// useStateDir 把状态目录指到临时目录（记忆必须落在用户级状态目录，不进仓库）。
func useStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	// Windows 分支读 LOCALAPPDATA，这里一并覆盖，保证跨平台测试语义一致。
	t.Setenv("LOCALAPPDATA", dir)
	return dir
}

// TestMemoryToolIsAlwaysActive 断言 memory 在默认 active 集合里（固定常开）。
func TestMemoryToolIsAlwaysActive(t *testing.T) {
	useStateDir(t)
	a := mustNew(t, kit.Config{Model: "m", Provider: &usageProvider{}, Root: t.TempDir()})

	registered := a.RegisteredTools()
	if !memoryContains(registered, memory.ToolName) {
		t.Fatalf("memory 没有注册: %v", registered)
	}
	active := a.ToolsFor("s1")
	if !memoryContains(active, memory.ToolName) {
		t.Fatalf("memory 不在 active 集合里（记忆必须常开）: %v", active)
	}
}

// TestMemoryWikiDefaultsToWorkspaceFileWiki 断言 nil 时自动落到
// `<stateDir>/rig/memory/<workspaceID>`，且跨实例可见。
func TestMemoryWikiDefaultsToWorkspaceFileWiki(t *testing.T) {
	stateDir := useStateDir(t)
	root := t.TempDir()

	a := mustNew(t, kit.Config{Model: "m", Provider: &usageProvider{}, Root: root})
	if err := a.MemoryWikiError(); err != nil {
		t.Fatalf("自动建记忆目录失败: %v", err)
	}
	if result := memoryCall(t, a, `{"action":"write","name":"build-flaky","summary":"偶发失败与重试","content":"重试即可"}`); result.IsError {
		t.Fatalf("write: %s", result.Content)
	}

	workspaceID, err := memory.WorkspaceID(root)
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	pagePath := filepath.Join(stateDir, "rig", "memory", workspaceID, "pages", "build-flaky.md")
	raw, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatalf("页面没有落到状态目录（%s）: %v", pagePath, err)
	}
	if !strings.Contains(string(raw), "偶发失败与重试") {
		t.Fatalf("页面内容不对:\n%s", raw)
	}
	// 绝不能写进仓库。
	if _, err := os.Stat(filepath.Join(root, "memory")); err == nil {
		t.Fatal("记忆被写进了工作区仓库")
	}

	// 第二个实例（模拟重启）读到同一页。
	second := mustNew(t, kit.Config{Model: "m", Provider: &usageProvider{}, Root: root})
	result := memoryCall(t, second, `{"action":"read","name":"build-flaky"}`)
	if result.IsError || !strings.Contains(result.Content, "重试即可") {
		t.Fatalf("重启后读不到记忆: %+v", result)
	}
}

// TestMemoryWikiIsolatedPerWorkspace 断言不同工作区互不可见（关键）。
func TestMemoryWikiIsolatedPerWorkspace(t *testing.T) {
	useStateDir(t)
	rootA := t.TempDir()
	rootB := t.TempDir()

	alpha := mustNew(t, kit.Config{Model: "m", Provider: &usageProvider{}, Root: rootA})
	if result := memoryCall(t, alpha, `{"action":"write","name":"only-alpha","summary":"A 的记忆","content":"body"}`); result.IsError {
		t.Fatalf("write: %s", result.Content)
	}

	beta := mustNew(t, kit.Config{Model: "m", Provider: &usageProvider{}, Root: rootB})
	result := memoryCall(t, beta, `{"action":"index"}`)
	if !strings.Contains(result.Content, "还没有记忆") {
		t.Fatalf("B 看到了 A 的记忆: %q", result.Content)
	}
}

// TestMemoryWikiInjection 断言显式注入的 Wiki 优先于自动文件 wiki。
//
// 2026-11 钉子复核（docs/PLATFORM.md §10）：这条同时是"将来的 Storage 注入路径
// 会不会偷偷建文件 wiki"的取证用例，所以断言从"状态目录里没有页面"收紧成
// "**状态目录这棵树根本没有被创建/写过**"——原来只 ReadDir 了 `<state>/rig/memory`
// 且把 err != nil 当作通过，一个写到 `<state>/rig/` 下别的名字的 bug 会漏过去。
func TestMemoryWikiInjection(t *testing.T) {
	stateDir := useStateDir(t)
	injected := memory.NewMemoryWiki()
	a := mustNew(t, kit.Config{
		Model: "m", Provider: &usageProvider{}, Root: t.TempDir(), Memory: injected,
	})
	if got := a.MemoryWiki(); got != memory.Wiki(injected) {
		t.Fatalf("MemoryWiki() 没有返回注入的实现: %T", got)
	}
	if err := a.MemoryWikiError(); err != nil {
		t.Fatalf("注入实现不应该有装配错误: %v", err)
	}
	if result := memoryCall(t, a, `{"action":"write","name":"injected","summary":"内存实现","content":"x"}`); result.IsError {
		t.Fatalf("write: %s", result.Content)
	}
	// 注入的是内存实现：整棵状态目录必须**一个条目都没有**。
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("读状态目录失败: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("注入了记忆实现，但 kit 仍然在状态目录里建了东西（自动建 wiki 没被跳过）: %v", names)
	}
	// 内存实现本身要真的收到了这次写入（否则上面"没写盘"可能只是"没写进去"）。
	page, err := injected.Read(context.Background(), "injected")
	if err != nil || !strings.Contains(page.Content, "x") {
		t.Fatalf("注入的内存实现没有收到写入: %+v err=%v", page, err)
	}
}

// mustStateDir 返回当前测试设置的状态目录。
func mustStateDir(t *testing.T) string {
	t.Helper()
	return os.Getenv("XDG_STATE_HOME")
}

// memoryContains 报告切片里是否有某个名字（kit_test 包里已有 contains，避免重名）。
func memoryContains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
