package editor_test

// DSH 对齐后，"根内只读子路径"（WritableRoot.ReadOnlySubpaths）是**唯一保留**的
// 写面限制：`sandbox.Policy.CheckWritablePath → AllowsWrite` 会拒绝它。
//
// str_replace_editor 的 Description 已经宣称遵守 "read-only subpaths"，但实现只
// 检查了 `policy.ReadOnly()` + `OpenPath`（后者只做根围栏），所以 create /
// str_replace / insert 能写进 `.git` 这类只读子路径——本文件先钉住这个漏洞（红），
// 修完实现后转为绿。
//
// 仍然保留的行为：view 可读（只读不是不可见）、根外路径失败、普通路径成功。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/editor"
)

// newReadOnlySubpathFixture 构造一个 workspace-write 策略，并把 `.git` 整棵子树
// 声明成根内只读子路径（DSH 对齐后唯一保留的写面限制）。
func newReadOnlySubpathFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithReadOnlySubpaths(root, ".git"),
	)
	f := &fixture{root: root, tool: editor.New(policy)}
	f.write(t, ".git/config", "[core]\n")
	return f
}

// TestCreateRefusesReadOnlySubpath：create 不得写进根内只读子路径。
func TestCreateRefusesReadOnlySubpath(t *testing.T) {
	f := newReadOnlySubpathFixture(t)
	result := f.invoke(t, `{"command":"create","path":".git/config","file_text":"pwned\n"}`)
	if !result.IsError {
		t.Fatalf("create 写进了只读子路径 .git/config: %q", result.Content)
	}
	if got := f.read(t, ".git/config"); got != "[core]\n" {
		t.Fatalf(".git/config 被改写: %q", got)
	}
	if !strings.Contains(strings.ToLower(result.Content), "read-only") {
		t.Fatalf("错误文案应说明只读子路径: %q", result.Content)
	}

	// 新建文件也必须被拒（不是"已存在"那条分支）。
	result = f.invoke(t, `{"command":"create","path":".git/new-file","file_text":"x\n"}`)
	if !result.IsError {
		t.Fatalf("create 在只读子路径里新建了文件: %q", result.Content)
	}
	if _, err := os.Stat(filepath.Join(f.root, ".git", "new-file")); err == nil {
		t.Fatal(".git/new-file 被创建了")
	}
}

// TestStrReplaceRefusesReadOnlySubpath：str_replace 不得改写根内只读子路径。
func TestStrReplaceRefusesReadOnlySubpath(t *testing.T) {
	f := newReadOnlySubpathFixture(t)
	result := f.invoke(t, `{"command":"str_replace","path":".git/config","old_str":"[core]","new_str":"[pwned]"}`)
	if !result.IsError {
		t.Fatalf("str_replace 改写了只读子路径: %q", result.Content)
	}
	if got := f.read(t, ".git/config"); got != "[core]\n" {
		t.Fatalf(".git/config 被改写: %q", got)
	}
}

// TestInsertRefusesReadOnlySubpath：insert 不得写入根内只读子路径。
func TestInsertRefusesReadOnlySubpath(t *testing.T) {
	f := newReadOnlySubpathFixture(t)
	result := f.invoke(t, `{"command":"insert","path":".git/config","insert_line":0,"new_str":"injected"}`)
	if !result.IsError {
		t.Fatalf("insert 写入了只读子路径: %q", result.Content)
	}
	if got := f.read(t, ".git/config"); got != "[core]\n" {
		t.Fatalf(".git/config 被改写: %q", got)
	}
}

// TestViewStillReadsReadOnlySubpath：只读子路径仍然可读（只读 ≠ 不可见）。
func TestViewStillReadsReadOnlySubpath(t *testing.T) {
	f := newReadOnlySubpathFixture(t)
	result := f.invoke(t, `{"command":"view","path":".git/config"}`)
	if result.IsError {
		t.Fatalf("view 读不了只读子路径（应当只读而不是禁止读）: %q", result.Content)
	}
	if !strings.Contains(result.Content, "[core]") {
		t.Fatalf("view 内容 = %q", result.Content)
	}
}

// TestReadOnlySubpathKeepsOrdinaryPathsWritable：修复不能"一刀切拒绝"。
func TestReadOnlySubpathKeepsOrdinaryPathsWritable(t *testing.T) {
	f := newReadOnlySubpathFixture(t)
	created := f.invoke(t, `{"command":"create","path":"src/new.txt","file_text":"hello\n"}`)
	if created.IsError {
		t.Fatalf("普通路径 create 被误拒: %q", created.Content)
	}
	replaced := f.invoke(t, `{"command":"str_replace","path":"src/new.txt","old_str":"hello","new_str":"bye"}`)
	if replaced.IsError {
		t.Fatalf("普通路径 str_replace 被误拒: %q", replaced.Content)
	}
	if got := f.read(t, "src/new.txt"); got != "bye\n" {
		t.Fatalf("src/new.txt = %q, want 编辑生效", got)
	}
	// `.git` 旁边（非子路径）仍然可写。
	if res := f.invoke(t, `{"command":"create","path":".gitignore","file_text":"*.log\n"}`); res.IsError {
		t.Fatalf(".gitignore 被误拒: %q", res.Content)
	}
}

// TestWriteOutsideRootStillFails：根围栏不受影响。
func TestWriteOutsideRootStillFails(t *testing.T) {
	f := newReadOnlySubpathFixture(t)
	outside := filepath.Join(t.TempDir(), "escape.txt")
	result := f.invoke(t, `{"command":"create","path":"`+filepath.ToSlash(outside)+`","file_text":"x\n"}`)
	if !result.IsError {
		t.Fatalf("create 写到根外却成功了: %q", result.Content)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("根外文件被创建了")
	}

	result = f.invoke(t, `{"command":"create","path":"../escape.txt","file_text":"x\n"}`)
	if !result.IsError {
		t.Fatalf("create 用 .. 逃出根却成功了: %q", result.Content)
	}
}
