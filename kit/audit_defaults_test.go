package kit_test

// 个人客户端定位下的默认装配契约（TDD：先写测试，再改实现）：
//
//  1. 输出脱敏**不再是**工具边的默认装饰：挂 Redactor 也不会改写工具结果，明文直通。
//     脱敏只剩"用户主动导出日志"这一条路径（secrets.RedactForExport），默认零开销。
//  2. 保护路径**默认开启**：workspace 元数据（.git/.aws/.codex）+ 主目录下存在的
//     凭据位置（~/.ssh、~/.aws、~/.config/gh、~/.netrc）。它是防误伤的软护栏，
//     不是沙箱。
//  3. DisableProtection=true 时不追加；宿主显式提供 Policy 时 kit 完全不碰它。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/secrets"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// defaultEchoTool 回显参数，用于观察"工具结果有没有被改写"。
type defaultEchoTool struct{ name string }

func (t defaultEchoTool) Name() string        { return t.name }
func (t defaultEchoTool) Description() string { return "echo" }
func (t defaultEchoTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}

func (t defaultEchoTool) Invoke(_ context.Context, call kit.ToolCall) (kit.ToolResult, error) {
	return kit.ToolResult{Content: "echo: " + call.Arguments}, nil
}

// TestExplicitRedactorStillRedactsAtAgentLayer 断言"保留能力"这一半契约：宿主**显式**
// 传了 Redactor 时，脱敏仍然生效（现在发生在 agent 层，而不是 kit 的工具装饰器），
// 工具收到的入参保持原文。
//
// 默认路径（不传 Redactor）的明文直通由 TestDefaultWithoutRedactorPassesPlaintext
// 覆盖——两者合起来才是"默认不脱敏、想要才脱敏"。
func TestExplicitRedactorStillRedactsAtAgentLayer(t *testing.T) {
	ctx := wiringContext(t)
	const secret = "sk-personalclientnotredacted0123456"
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "echo_secret", Arguments: `{"token":"` + secret + `"}`}),
		textScript("done"),
	}}
	agent := mustNew(t, kit.Config{
		Model:    "m",
		Provider: provider,
		Root:     t.TempDir(),
		Tools:    []kit.Tool{defaultEchoTool{name: "echo_secret"}},
		Gate:     gate.AllowAll(),
		Redactor: secrets.New(secret),
	})
	if _, _, err := agent.RunText(ctx, "s1", "echo the token", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	message, ok := findToolMessage(provider.request(1).Messages, "echo_secret")
	if !ok {
		t.Fatalf("模型没有收到工具结果: %+v", provider.request(1).Messages)
	}
	if !strings.Contains(message.Text(), "[REDACTED") {
		t.Fatalf("显式传 Redactor 时应当仍然脱敏（能力保留）: %q", message.Text())
	}
	if strings.Contains(message.Text(), secret) {
		t.Fatalf("显式传 Redactor 时明文泄漏: %q", message.Text())
	}
}

// TestDefaultWithoutRedactorPassesPlaintext 是对照组：不挂 Redactor 时同样是明文，
// 说明上一条不是因为"没走到工具"而通过的。
func TestDefaultWithoutRedactorPassesPlaintext(t *testing.T) {
	ctx := wiringContext(t)
	const secret = "sk-personalclientnotredacted0123456"
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "echo_secret", Arguments: `{"token":"` + secret + `"}`}),
		textScript("done"),
	}}
	agent := mustNew(t, kit.Config{
		Model:    "m",
		Provider: provider,
		Root:     t.TempDir(),
		Tools:    []kit.Tool{defaultEchoTool{name: "echo_secret"}},
		Gate:     gate.AllowAll(),
	})
	if _, _, err := agent.RunText(ctx, "s1", "echo the token", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	message, ok := findToolMessage(provider.request(1).Messages, "echo_secret")
	if !ok || !strings.Contains(message.Text(), secret) {
		t.Fatalf("未挂 Redactor 时应当是明文: %+v", message)
	}
}

// secludeHome 把主目录相关环境变量指到空的临时目录，让用例不依赖运行机器。
//
// 三个都要设：POSIX 读 HOME，Windows 读 USERPROFILE，而 Windows 的缓存根
// （`%LOCALAPPDATA%` / `%APPDATA%\npm-cache`）只认后两个。
func secludeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	return home
}

// TestExplicitPolicyIsNotModified 断言宿主显式提供 Policy 时 kit 完全不碰它
// （保护策略归宿主所有）。
func TestExplicitPolicyIsNotModified(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	agent := mustNew(t, kit.Config{
		Model:    "m",
		Provider: &scriptProvider{},
		Root:     root,
		Policy:   policy,
	})
	if protected := agent.Policy().ProtectedPaths(); len(protected) != 0 {
		t.Fatalf("kit 改写了宿主提供的 Policy 的保护路径: %v", protected)
	}
	if agent.Policy() != policy {
		t.Fatal("Agent.Policy() 不是宿主传入的那个策略实例")
	}
}

// TestGitConfigIsWritableWithoutProtection：DSH 对齐后 kit 不再追加默认保护路径，
// 所以工作区自己的 .git/config 可以正常写入（保护要由宿主自己的 Policy 决定）。
func TestGitConfigIsWritableWithoutProtection(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("orig\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	agent := mustNew(t, kit.Config{Model: "m", Provider: &scriptProvider{}, Root: root})
	if protected := agent.Policy().ProtectedPaths(); len(protected) != 0 {
		t.Fatalf("默认装配不应有保护路径: %v", protected)
	}

	tool := patch.New(root, patch.WithPolicy(agent.Policy()))
	result := invokePatchTool(t, tool, "*** Begin Patch\n*** Update File: .git/config\n@@\n-orig\n+hijacked\n*** End Patch\n")
	if result.IsError {
		t.Fatalf("没有默认保护时写 .git/config 不该被拒: %s", result.Content)
	}
	data, err := os.ReadFile(filepath.Join(root, ".git", "config"))
	if err != nil || string(data) != "hijacked\n" {
		t.Fatalf("写入没有生效: %q err=%v", data, err)
	}
}

// containsPath 报告 paths 里是否含 want（按 Clean 比较）。
func containsPath(paths []string, want string) bool {
	want = filepath.Clean(want)
	for _, path := range paths {
		if filepath.Clean(path) == want {
			return true
		}
	}
	return false
}

// invokePatchTool 调一次 apply_patch。
func invokePatchTool(t *testing.T, tool kit.Tool, patchText string) kit.ToolResult {
	t.Helper()
	result, err := tool.Invoke(wiringContext(t), kit.ToolCall{
		ID:        "patch-1",
		Name:      tool.Name(),
		Arguments: `{"patch":` + jsonString(patchText) + `}`,
	})
	if err != nil {
		t.Fatalf("patch Invoke: %v", err)
	}
	return result
}

// jsonString 把文本编码成 JSON 字符串字面量。
func jsonString(text string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range text {
		switch r {
		case '\\':
			builder.WriteString(`\\`)
		case '"':
			builder.WriteString(`\"`)
		case '\n':
			builder.WriteString(`\n`)
		case '\t':
			builder.WriteString(`\t`)
		case '\r':
			builder.WriteString(`\r`)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
