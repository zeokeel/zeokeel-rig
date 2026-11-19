package builtin_test

// 全项目对抗审计 第 1 轮 —— "保护路径 vs 文件写工具"。
//
// **DSH 对齐后的有意变更**：沙箱只做可写根围栏。保护路径
// （WithProtectedPaths / AddProtectedPaths / ProtectedPaths）与拒绝 glob
// （WithDeniedGlobs）都已停用为"记录型 no-op"，所以 `.git/config`、`.env`、
// `secrets/creds.md` 这些以前被拒的落点现在**必须允许写**。仍然拒绝的是根外
// 写入、含 `..` 的路径与逃逸根目录的符号链接——那几条用例保留。
//
// fixture 只声明可写根：`.git/config`、`.env`、`secrets/creds.md` 现在可写，
// 原因是"它们在可写根内"，与已停用的保护路径/拒绝 glob 无关。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// protectedFixture 构造一个"工作区 + 已声明的保护路径/拒绝 glob"的策略，并返回按
// 名字索引的工具。这些声明现在只是记录，不再产生任何写入限制。
type protectedFixture struct {
	root  string
	tools map[string]rig.Tool
}

func newProtectedFixture(t *testing.T) *protectedFixture {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{filepath.Join(root, ".git", "refs"), filepath.Join(root, "secrets")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 刻意不声明 WithProtectedPaths / WithDeniedGlobs：两者已 Deprecated 且不参与
	// 强制（DSH 对齐）。这里只需要一个普通 workspace-write 策略。
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)

	tools := builtin.NewWithPolicy(policy)
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return &protectedFixture{root: root, tools: byName}
}

// invoke 调一次工具，返回结果。
func (f *protectedFixture) invoke(t *testing.T, name string, args map[string]any) rig.ToolResult {
	t.Helper()
	payload, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	tool, ok := f.tools[name]
	if !ok {
		t.Fatalf("没有工具 %q", name)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "audit", Name: name, Arguments: string(payload)})
	if err != nil {
		t.Fatalf("%s.Invoke 返回 Go error: %v", name, err)
	}
	return result
}

// TestAuditWriteFileWritesFormerlyProtectedPaths 是旧用例
// TestAuditWriteFileRefusesProtectedPaths 的**反向**版本（DSH 对齐：有意变更）。
//
// 以前这里断言四个落点被拒；现在策略层没有保护路径，它们都必须成功并且真的落盘。
func TestAuditWriteFileWritesFormerlyProtectedPaths(t *testing.T) {
	fixture := newProtectedFixture(t)

	cases := []struct {
		name string
		path string
	}{
		{"直接路径", ".git/config"},
		{"深层路径", ".git/refs/heads/main"},
		{"拒绝 glob 形态（顶层）", ".env"},
		{"拒绝 glob 形态（目录）", "secrets/creds.md"},
		{"绝对路径", filepath.Join(fixture.root, ".git", "config")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := fixture.invoke(t, "write_file", map[string]any{"path": tc.path, "content": "legit\n"})
			if result.IsError {
				t.Fatalf("write_file %q 被拒绝（DSH 对齐后应当允许）: %q", tc.path, result.Content)
			}
			abs := tc.path
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(fixture.root, filepath.FromSlash(tc.path))
			}
			raw, err := os.ReadFile(abs)
			if err != nil {
				t.Fatalf("读取落盘结果 %q: %v", abs, err)
			}
			if string(raw) != "legit\n" {
				t.Fatalf("%q 内容 = %q, want 写入生效", tc.path, raw)
			}
		})
	}
}

// TestAuditEditFileEditsFormerlyProtectedPath 旧用例
// TestAuditEditFileRefusesProtectedPaths 的反向版本。
func TestAuditEditFileEditsFormerlyProtectedPath(t *testing.T) {
	fixture := newProtectedFixture(t)
	result := fixture.invoke(t, "edit_file", map[string]any{
		"path": ".git/config", "old_string": "[core]", "new_string": "[legit]",
	})
	if result.IsError {
		t.Fatalf("edit_file 被拒绝（DSH 对齐后应当允许）: %q", result.Content)
	}
	raw, err := os.ReadFile(filepath.Join(fixture.root, ".git", "config"))
	if err != nil || string(raw) != "[legit]\n" {
		t.Fatalf(".git/config = %q (%v), want 编辑生效", raw, err)
	}
}

// TestAuditWriteFileStillWritesNormalPaths 是控制组——普通路径当然可写。
func TestAuditWriteFileStillWritesNormalPaths(t *testing.T) {
	fixture := newProtectedFixture(t)

	for _, tc := range []struct {
		name, path, content string
	}{
		{"普通文件", "ok.txt", "updated\n"},
		{"新文件", "src/new.txt", "hello\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := fixture.invoke(t, "write_file", map[string]any{"path": tc.path, "content": tc.content})
			if result.IsError {
				t.Fatalf("普通路径被误拒: %q", result.Content)
			}
		})
	}
}

// TestAuditBackslashPathCreatesLiteralFile【行为记录】Windows 分隔符在 Linux 上是
// 普通字符：`.git\config` 会成功，但落盘的是**另一个文件**（字面名 `.git\config`），
// 而不是 `.git/config`。
func TestAuditBackslashPathCreatesLiteralFile(t *testing.T) {
	fixture := newProtectedFixture(t)
	result := fixture.invoke(t, "write_file", map[string]any{"path": `.git\config`, "content": "pwned\n"})
	if result.IsError {
		t.Logf("含反斜杠的路径被拒绝（更安全）: %s", result.Content)
		return
	}
	if _, err := os.Stat(filepath.Join(fixture.root, `.git\config`)); err != nil {
		t.Fatalf("没有看到字面文件名 .git\\config: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(fixture.root, ".git", "config"))
	if err != nil || string(raw) != "[core]\n" {
		t.Fatalf(".git/config 被意外改写: %q (%v)", raw, err)
	}
	t.Logf("⚠️ 非 Windows 平台：`.git\\config` 被当作字面文件名创建（未触及 .git/config，但会误导模型）")
}

// TestAuditParentComponentAnywhereIsRejected 任何含 `..` 的路径都会被拒（保留下来的
// 围栏：sandbox.ResolvePath 的 hasDotDotComponent）。
func TestAuditParentComponentAnywhereIsRejected(t *testing.T) {
	fixture := newProtectedFixture(t)
	result := fixture.invoke(t, "write_file", map[string]any{"path": ".git/../outside.txt", "content": "x\n"})
	if !result.IsError {
		t.Fatalf("含 .. 的路径被接受了: %q", result.Content)
	}
	requireContains(t, result.Content, "parent-directory component")
	t.Logf("行为：含 `..` 一律拒绝（错误文案 %q）", result.Content)
}

// TestAuditWriteFileRefusesParentEscape：`../` 逃出工作区必须被拒（保留的根围栏）。
func TestAuditWriteFileRefusesParentEscape(t *testing.T) {
	fixture := newProtectedFixture(t)
	for _, path := range []string{"../escape.txt", "src/../../escape.txt"} {
		result := fixture.invoke(t, "write_file", map[string]any{"path": path, "content": "x\n"})
		if !result.IsError {
			t.Fatalf("路径 %q 逃出工作区却成功: %q", path, result.Content)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(fixture.root), "escape.txt")); err == nil {
		t.Fatal("工作区外被创建了文件")
	}
}
