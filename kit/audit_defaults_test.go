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

// TestDefaultProtectsWorkspaceMetadata 断言默认（零值 Config）就保护工作区元数据。
//
// v0.22 起 `.git` 不再**整体**保护：`git commit` 要写 index/objects/refs，整体只读
// 会让"帮我提交"在沙箱里做不到。改成"数据面可写、控制面只读"：
//   - 全局保护路径只剩 `.aws`/`.codex`（以及主目录凭据位置）；
//   - `.git/config`、`.git/hooks`、`.git/info` 由 policy 的只读子路径挡住写入；
//   - `.git/index`、`.git/objects/*`、`.git/refs/*` 可写（见 AllowsWrite 用例）。
func TestDefaultProtectsWorkspaceMetadata(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	for _, dir := range []string{"objects", "refs", "hooks", "info"} {
		if err := os.MkdirAll(filepath.Join(gitDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	agent := mustNew(t, kit.Config{Model: "m", Provider: &scriptProvider{}, Root: root})
	policy := agent.Policy()

	protected := policy.ProtectedPaths()
	for _, want := range []string{
		filepath.Join(root, ".aws"),
		filepath.Join(root, ".codex"),
	} {
		if !containsPath(protected, want) {
			t.Fatalf("默认保护路径缺少 %q: %v", want, protected)
		}
	}
	if containsPath(protected, gitDir) {
		t.Fatalf(".git 不应再整体出现在 ProtectedPaths（git commit 需要写 index/objects/refs）: %v", protected)
	}
	// 控制面只读。
	for _, path := range []string{
		filepath.Join(gitDir, "config"),
		filepath.Join(gitDir, "hooks", "pre-commit"),
		filepath.Join(gitDir, "info", "exclude"),
	} {
		if err := policy.CheckWritablePath(path); err == nil {
			t.Fatalf("控制面路径 %q 仍可写", path)
		}
	}
	// 数据面可写——这是 git commit 的前提。
	for _, path := range []string{
		filepath.Join(gitDir, "index"),
		filepath.Join(gitDir, "objects", "ab", "cdef"),
		filepath.Join(gitDir, "refs", "heads", "main"),
	} {
		if err := policy.CheckWritablePath(path); err != nil {
			t.Fatalf("数据面路径 %q 被拒（git commit 会失败）: %v", path, err)
		}
	}
}

// TestDefaultProtectsHomeCredentialPaths 断言默认会保护**存在**的主目录凭据位置，
// 不存在的跳过（不制造幻影路径）。
func TestDefaultProtectsHomeCredentialPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows 语义
	present := []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".config", "gh"),
	}
	for _, dir := range present {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	missing := filepath.Join(home, ".netrc") // 故意不创建

	agent := mustNew(t, kit.Config{Model: "m", Provider: &scriptProvider{}, Root: t.TempDir()})
	protected := agent.Policy().ProtectedPaths()
	for _, want := range present {
		if !containsPath(protected, want) {
			t.Fatalf("默认保护路径缺少主目录凭据位置 %q: %v", want, protected)
		}
	}
	for _, unwanted := range []string{missing} {
		if containsPath(protected, unwanted) {
			t.Fatalf("不存在的路径被加进了保护列表: %q (%v)", unwanted, protected)
		}
	}
}

// TestDisableProtectionSkipsPaths 断言 DisableProtection=true 时一个都不追加。
func TestDisableProtectionSkipsPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	agent := mustNew(t, kit.Config{
		Model:             "m",
		Provider:          &scriptProvider{},
		Root:              root,
		DisableProtection: true,
	})
	if protected := agent.Policy().ProtectedPaths(); len(protected) != 0 {
		t.Fatalf("DisableProtection=true 仍然追加了保护路径: %v", protected)
	}
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

// TestDefaultProtectionBlocksPatchWrites 端到端验证"保护路径命中时 write 被拒"：
// 用工厂真正会交给工具的那份策略去跑 apply_patch，写 .git/config 必须被拒。
func TestDefaultProtectionBlocksPatchWrites(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("orig\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	agent := mustNew(t, kit.Config{Model: "m", Provider: &scriptProvider{}, Root: root})

	tool := patch.New(root, patch.WithPolicy(agent.Policy()))
	result := invokePatchTool(t, tool, "*** Begin Patch\n*** Update File: .git/config\n@@\n-orig\n+hijacked\n*** End Patch\n")
	if !result.IsError {
		t.Fatalf("写保护路径没有被拒绝: %s", result.Content)
	}
	data, err := os.ReadFile(filepath.Join(root, ".git", "config"))
	if err != nil || string(data) != "orig\n" {
		t.Fatalf("保护路径被改写了: %q err=%v", data, err)
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
