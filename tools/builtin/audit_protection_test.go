package builtin_test

// 全项目对抗审计 第 1 轮 —— "假默认值"专项（保护路径 vs 文件写工具）。
//
// 目标：证明"`.git` 这类元数据/凭据目录即使在工作区内也是只读的"这条承诺，在
// write_file / edit_file 上真的成立，并**尝试推翻**它：
//   - 直接路径、绝对路径、`../` 逃逸、`.git/..` 回绕；
//   - 符号链接穿透（`alias -> .git`，再写 `alias/config`）；
//   - 大小写变体（`.GIT`）与 Windows 分隔符（`.git\config`）。
//
// 只读审计：本文件不改实现，只断言当前行为。

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

// protectedFixture 构造一个"工作区 + 保护路径"的沙箱策略，并返回按名字索引的工具。
type protectedFixture struct {
	root  string
	tools map[string]rig.Tool
}

func newProtectedFixture(t *testing.T) *protectedFixture {
	t.Helper()
	root := t.TempDir()
	// 造出 .git 目录与一个正常文件。
	if err := os.MkdirAll(filepath.Join(root, ".git", "refs"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	policy.AddProtectedPaths(filepath.Join(root, ".git"))

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

// 1.2a【核心】write_file 不能写保护目录里的任何文件。
func TestAuditWriteFileRefusesProtectedPaths(t *testing.T) {
	fixture := newProtectedFixture(t)

	cases := []struct {
		name string
		path string
	}{
		{"直接路径", ".git/config"},
		{"深层路径", ".git/refs/heads/main"},
		{"点号回绕", ".git/../.git/config"},
		{"绝对路径", filepath.Join(fixture.root, ".git", "config")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := fixture.invoke(t, "write_file", map[string]any{"path": tc.path, "content": "pwned\n"})
			if !result.IsError {
				t.Fatalf("write_file %q 竟然成功: %q", tc.path, result.Content)
			}
			// 内容必须没变。
			raw, err := os.ReadFile(filepath.Join(fixture.root, ".git", "config"))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if string(raw) != "[core]\n" {
				t.Fatalf("保护路径被改写: %q", raw)
			}
		})
	}
}

// 1.2b：edit_file 同样不能改保护目录里的文件。
func TestAuditEditFileRefusesProtectedPaths(t *testing.T) {
	fixture := newProtectedFixture(t)
	result := fixture.invoke(t, "edit_file", map[string]any{
		"path": ".git/config", "old_string": "[core]", "new_string": "[pwned]",
	})
	if !result.IsError {
		t.Fatalf("edit_file 竟然改了保护文件: %q", result.Content)
	}
	raw, err := os.ReadFile(filepath.Join(fixture.root, ".git", "config"))
	if err != nil || string(raw) != "[core]\n" {
		t.Fatalf("保护文件被改写: %q (%v)", raw, err)
	}
}

// 1.2c【尝试推翻】符号链接穿透：`alias -> .git`，然后写 `alias/config`。
func TestAuditWriteFileRefusesProtectedPathViaSymlink(t *testing.T) {
	fixture := newProtectedFixture(t)
	if err := os.Symlink(filepath.Join(fixture.root, ".git"), filepath.Join(fixture.root, "alias")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}

	result := fixture.invoke(t, "write_file", map[string]any{"path": "alias/config", "content": "pwned\n"})
	if !result.IsError {
		t.Fatalf("经符号链接写保护目录成功（严重）: %q", result.Content)
	}
	raw, err := os.ReadFile(filepath.Join(fixture.root, ".git", "config"))
	if err != nil || string(raw) != "[core]\n" {
		t.Fatalf("保护文件被符号链接改写: %q (%v)", raw, err)
	}

	// 二级跳转：`a -> b`，`b -> .git`。
	if err := os.Symlink(filepath.Join(fixture.root, ".git"), filepath.Join(fixture.root, "b")); err != nil {
		t.Skipf("无法创建二级符号链接: %v", err)
	}
	if err := os.Symlink(filepath.Join(fixture.root, "b"), filepath.Join(fixture.root, "a")); err != nil {
		t.Skipf("无法创建二级符号链接: %v", err)
	}
	result = fixture.invoke(t, "write_file", map[string]any{"path": "a/config", "content": "pwned\n"})
	if !result.IsError {
		t.Fatalf("经二级符号链接写保护目录成功（严重）: %q", result.Content)
	}
}

// 1.2d：控制组——保护路径之外必须照常可写（别把人锁死）。
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

// 1.2g【行为记录】Windows 分隔符在 Linux 上是普通字符：`.git\config` 会成功，
// 但落盘的是**另一个文件**（字面名 `.git\config`），而不是保护目录里的 config。
//
// 结论：不构成越权（Linux 上碰不到 .git/config），但会误导模型（它以为写的是
// `.git/config`）并留下一个怪文件名。建议实现层在非 Windows 平台拒绝含 `\\` 的
// 路径，或在结果里提示"按字面文件名创建"。
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
		t.Fatalf("保护文件竟然被改了: %q (%v)", raw, err)
	}
	t.Logf("⚠️ 非 Windows 平台：`.git\\config` 被当作字面文件名创建（未触及保护路径，但会误导模型）")
}

// 1.2h【行为记录】任何含 `..` 的路径都会被拒（不只是逃逸的那些）。
//
// sandbox.ResolvePath 里有 `hasDotDotComponent` 直接拒绝，因此 `.git/../ok.txt`
// 也报 "contains a parent-directory component"。这是刻意的 fail-closed 设计（词法
// 上不做归一化推理），但模型会觉得"我明明没逃逸"——建议在工具描述或错误文案里
// 明确写"路径不允许包含 ..，请直接用等价的无 .. 路径"。
func TestAuditParentComponentAnywhereIsRejected(t *testing.T) {
	fixture := newProtectedFixture(t)
	result := fixture.invoke(t, "write_file", map[string]any{"path": ".git/../outside.txt", "content": "x\n"})
	if !result.IsError {
		t.Fatalf("含 .. 的路径被接受了: %q", result.Content)
	}
	requireContains(t, result.Content, "parent-directory component")
	t.Logf("行为：含 `..` 一律拒绝（错误文案 %q）", result.Content)
}

// 1.2e：`../` 逃出工作区必须被拒（与保护路径无关，是沙箱根边界）。
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

// 1.2f【平台风险，只记录】大小写变体 `.GIT`。
//
// `sandbox/path.go` 的保护判定用 `pathWithin`（纯前缀比较，**不折叠大小写**），
// 因此在**大小写不敏感**的文件系统（macOS APFS 默认、Windows NTFS 默认）上，
// `.GIT/config` 指向的就是同一个目录，但保护判定不会命中。
//
// Linux 是大小写敏感的，所以这里断言的是"在 Linux 上它只是另一个普通目录"——
// 真正的风险在 darwin/windows，本机无法执行，只能记录。
func TestAuditProtectedPathIsCaseSensitive(t *testing.T) {
	fixture := newProtectedFixture(t)
	result := fixture.invoke(t, "write_file", map[string]any{"path": ".GIT/config", "content": "pwned\n"})

	if result.IsError {
		t.Logf("大小写变体被拒绝（说明实现已做大小写折叠）: %s", result.Content)
		return
	}
	t.Logf("⚠️ 平台风险（darwin/windows）：`.GIT/config` 在大小写不敏感的文件系统上就是 "+
		"`.git/config`，但 sandbox/path.go 的 pathWithin 是纯前缀比较，不会命中保护。 "+
		"Linux 上它只是新建了一个普通目录，所以本机表现为『成功』：%s", result.Content)
}
