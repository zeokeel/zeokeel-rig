package undo_test

// undo 的语义测试：只钉"用户按撤销键"应该看到的行为，不钉实现（临时索引、隐藏提交、
// 元数据 blob 这些都在 internal/gitsnap 的测试里）。

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// requireGit 在 git 不可用时跳过。
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git 不可用: %v", err)
	}
}

// runGit 执行 git 命令。
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	out, err := command.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("git %v: %v (%s)", args, err, stderr)
	}
	return string(out)
}

// newRepo 建一个配好身份的临时仓库。
func newRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", ".")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	runGit(t, dir, "config", "core.autocrlf", "false")
	return dir
}

// writeTree 批量写文件。
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %q: %v", rel, err)
		}
	}
}

// commitAll 提交全部改动。
func commitAll(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "snapshot")
}

// read 读文件内容。
func read(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("ReadFile %q: %v", rel, err)
	}
	return string(data)
}

// take 拍快照（断言成功）。
func take(t *testing.T, dir string, opts undo.Options) *undo.Snapshot {
	t.Helper()
	snapshot, err := undo.Take(dir, opts)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	return snapshot
}

// changesByPath 把变更列表变成 path → change。
func changesByPath(changes []undo.Change) map[string]undo.Change {
	byPath := make(map[string]undo.Change, len(changes))
	for _, change := range changes {
		byPath[change.Path] = change
	}
	return byPath
}

// TestTakeRequiresGitRepo 证明非 git 仓库直接返回 ErrNotGitRepo（调用方据此降级）。
func TestTakeRequiresGitRepo(t *testing.T) {
	requireGit(t)
	plain := t.TempDir()
	if _, err := undo.Take(plain, undo.Options{}); !errors.Is(err, undo.ErrNotGitRepo) {
		t.Fatalf("Take(plain) = %v, want ErrNotGitRepo", err)
	}
	if ok, err := undo.IsGitRepo(context.Background(), plain); err != nil || ok {
		t.Fatalf("IsGitRepo(plain) = %v, %v", ok, err)
	}
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"a.txt": "a\n"})
	commitAll(t, repo)
	if ok, err := undo.IsGitRepo(context.Background(), repo); err != nil || !ok {
		t.Fatalf("IsGitRepo(repo) = %v, %v", ok, err)
	}
}

// TestTakeReportsScale 证明快照记录路径数、且 SDK 不持有内容（Bytes 恒为 0）。
func TestTakeReportsScale(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"a.txt": "a\n", "sub/b.txt": "b\n"})
	commitAll(t, repo)
	snapshot := take(t, repo, undo.Options{})
	if got := snapshot.Files(); got != 2 {
		t.Fatalf("Files() = %d, want 2", got)
	}
	if got := snapshot.Bytes(); got != 0 {
		t.Fatalf("Bytes() = %d, want 0（内容在 git 对象库里，不在 SDK 内存里）", got)
	}
	if len(snapshot.ID()) != 40 {
		t.Fatalf("ID() = %q, want a 40-char git commit", snapshot.ID())
	}
	if snapshot.Root() == "" {
		t.Fatal("Root() is empty")
	}
}

// TestChangesAndRevert 覆盖三种基本变更的检测与回退。
func TestChangesAndRevert(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"keep.txt": "keep\n", "gone.txt": "gone\n", "edit.txt": "l1\nl2\n"})
	commitAll(t, repo)
	snapshot := take(t, repo, undo.Options{})

	writeTree(t, repo, map[string]string{"edit.txt": "l1\nl2\nl3\n", "new.txt": "n1\nn2\n"})
	if err := os.Remove(filepath.Join(repo, "gone.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	byPath := changesByPath(changes)
	if got := byPath["new.txt"]; got.Op != "add" || got.Added != 2 {
		t.Fatalf("new.txt = %+v, want add +2", got)
	}
	if got := byPath["gone.txt"]; got.Op != "delete" {
		t.Fatalf("gone.txt = %+v, want delete", got)
	}
	if got := byPath["edit.txt"]; got.Op != "modify" || got.Added != 1 {
		t.Fatalf("edit.txt = %+v, want modify +1", got)
	}
	if _, ok := byPath["keep.txt"]; ok {
		t.Fatal("keep.txt 不该出现在变更里")
	}

	applied, err := snapshot.Revert()
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	for _, change := range applied {
		if !change.Reverted {
			t.Fatalf("change 未标记为已回退: %+v", change)
		}
	}
	if got := read(t, repo, "edit.txt"); got != "l1\nl2\n" {
		t.Fatalf("edit.txt = %q", got)
	}
	if got := read(t, repo, "gone.txt"); got != "gone\n" {
		t.Fatalf("gone.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt 应被删除: err=%v", err)
	}
	// 幂等：再回退一次没有变更。
	again, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("回退之后仍有变更: %+v", again)
	}
}

// TestUntrackedFiles 覆盖未跟踪文件的三个方向。
func TestUntrackedFiles(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"tracked.txt": "t\n"})
	commitAll(t, repo)
	writeTree(t, repo, map[string]string{"edited-untracked.txt": "u1\n", "deleted-untracked.txt": "d1\n"})
	snapshot := take(t, repo, undo.Options{})

	writeTree(t, repo, map[string]string{"edited-untracked.txt": "u2\n", "created-untracked.txt": "c\n"})
	if err := os.Remove(filepath.Join(repo, "deleted-untracked.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	byPath := changesByPath(changes)
	if got := byPath["edited-untracked.txt"]; got.Op != "modify" {
		t.Fatalf("edited-untracked.txt = %+v, want modify", got)
	}
	if got := byPath["deleted-untracked.txt"]; got.Op != "delete" {
		t.Fatalf("deleted-untracked.txt = %+v, want delete", got)
	}
	if got := byPath["created-untracked.txt"]; got.Op != "add" {
		t.Fatalf("created-untracked.txt = %+v, want add", got)
	}

	if _, err := snapshot.Revert(); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := read(t, repo, "edited-untracked.txt"); got != "u1\n" {
		t.Fatalf("edited-untracked.txt = %q, want u1", got)
	}
	if got := read(t, repo, "deleted-untracked.txt"); got != "d1\n" {
		t.Fatalf("deleted-untracked.txt = %q, want d1", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "created-untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("created-untracked.txt 应被删除: err=%v", err)
	}
	// 恢复出来的未跟踪文件不能被顺手 add 进索引。
	if porcelain := runGit(t, repo, "status", "--porcelain"); strings.Contains(porcelain, "A  ") {
		t.Fatalf("回退把未跟踪文件加进了暂存区: %q", porcelain)
	}
}

// TestGitignoredFilesAreUntouched 证明 .gitignore 的语义由 git 负责：既不进快照，也不删。
func TestGitignoredFilesAreUntouched(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{".gitignore": "*.log\nbuild/\n", "a.txt": "a\n"})
	commitAll(t, repo)
	snapshot := take(t, repo, undo.Options{})
	writeTree(t, repo, map[string]string{"debug.log": "noise\n", "build/out.bin": "artifact\n"})

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("被忽略的文件出现在变更里: %+v", changes)
	}
	if _, err := snapshot.Revert(); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := read(t, repo, "debug.log"); got != "noise\n" {
		t.Fatalf("被忽略的文件被改了: %q", got)
	}
	if got := read(t, repo, "build/out.bin"); got != "artifact\n" {
		t.Fatalf("被忽略的目录被改了: %q", got)
	}
}

// TestOnlyThisTurnIsReverted 证明快照之前的脏改动不会被撤销（"只回退本回合"）。
func TestOnlyThisTurnIsReverted(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"a.txt": "base\n", "b.txt": "base\n"})
	commitAll(t, repo)
	writeTree(t, repo, map[string]string{"a.txt": "user-preturn\n"})
	snapshot := take(t, repo, undo.Options{})
	writeTree(t, repo, map[string]string{"b.txt": "agent\n"})

	if _, err := snapshot.Revert(); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := read(t, repo, "a.txt"); got != "user-preturn\n" {
		t.Fatalf("回合前的脏改动被抹掉了: %q", got)
	}
	if got := read(t, repo, "b.txt"); got != "base\n" {
		t.Fatalf("b.txt = %q, want base", got)
	}
}

// TestDirtyWorktreeCombinations 覆盖暂存/未暂存/未跟踪/删除混合的脏工作区。
func TestDirtyWorktreeCombinations(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"staged.txt": "s1\n", "unstaged.txt": "u1\n", "removed.txt": "r1\n"})
	commitAll(t, repo)

	// 快照前：一个已暂存、一个未暂存、一个已删除、一个未跟踪。
	writeTree(t, repo, map[string]string{"staged.txt": "s2\n"})
	runGit(t, repo, "add", "staged.txt")
	writeTree(t, repo, map[string]string{"unstaged.txt": "u2\n", "preexisting-untracked.txt": "p\n"})
	if err := os.Remove(filepath.Join(repo, "removed.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	snapshot := take(t, repo, undo.Options{})

	// 回合里：改一个、加一个、删一个。
	writeTree(t, repo, map[string]string{"staged.txt": "s3\n", "agent-new.txt": "n\n"})
	if err := os.Remove(filepath.Join(repo, "unstaged.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := snapshot.Revert(); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := read(t, repo, "staged.txt"); got != "s2\n" {
		t.Fatalf("staged.txt = %q, want s2（回到回合开始时的暂存内容）", got)
	}
	if got := read(t, repo, "unstaged.txt"); got != "u2\n" {
		t.Fatalf("unstaged.txt = %q, want u2", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "removed.txt")); !os.IsNotExist(err) {
		t.Fatalf("removed.txt 在回合开始时就已删除，回退不该把它变回来: err=%v", err)
	}
	if got := read(t, repo, "preexisting-untracked.txt"); got != "p\n" {
		t.Fatalf("preexisting-untracked.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "agent-new.txt")); !os.IsNotExist(err) {
		t.Fatalf("agent-new.txt 应被删除: err=%v", err)
	}
}

// TestSymlinkAndModeChanges 覆盖符号链接与可执行位的回退。
func TestSymlinkAndModeChanges(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"a.txt": "a\n", "b.txt": "b\n", "script.sh": "#!/bin/sh\n"})
	if err := os.Symlink("a.txt", filepath.Join(repo, "link")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	commitAll(t, repo)
	snapshot := take(t, repo, undo.Options{})

	// 回合里：改链接目标、把 b.txt 换成链接、去掉可执行位。
	if err := os.Remove(filepath.Join(repo, "link")); err != nil {
		t.Fatalf("Remove link: %v", err)
	}
	if err := os.Symlink("b.txt", filepath.Join(repo, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "b.txt")); err != nil {
		t.Fatalf("Remove b.txt: %v", err)
	}
	if err := os.Symlink("a.txt", filepath.Join(repo, "b.txt")); err != nil {
		t.Fatalf("Symlink b.txt: %v", err)
	}
	if err := os.Chmod(filepath.Join(repo, "script.sh"), 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	byPath := changesByPath(changes)
	if got := byPath["link"]; got.TypeChange != "symlink->symlink" {
		t.Fatalf("link = %+v, want symlink->symlink", got)
	}
	if got := byPath["b.txt"]; got.TypeChange != "file->symlink" {
		t.Fatalf("b.txt = %+v, want file->symlink", got)
	}

	if _, err := snapshot.Revert(); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(repo, "link")); err != nil || target != "a.txt" {
		t.Fatalf("link = %q (err=%v), want a.txt", target, err)
	}
	if got := read(t, repo, "b.txt"); got != "b\n" {
		t.Fatalf("b.txt = %q, want 普通文件 b", got)
	}
	info, err := os.Stat(filepath.Join(repo, "script.sh"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("script.sh 权限 = %v, want 0644", info.Mode().Perm())
	}
}

// TestRenameIsReportedAsRename 证明 git mv 被识别为 rename 而不是删+加。
func TestRenameIsReportedAsRename(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"old.txt": strings.Repeat("line\n", 20)})
	commitAll(t, repo)
	snapshot := take(t, repo, undo.Options{})
	runGit(t, repo, "mv", "old.txt", "new.txt")

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Op != "rename" || changes[0].OldPath != "old.txt" || changes[0].Path != "new.txt" {
		t.Fatalf("changes = %+v, want a single rename old.txt -> new.txt", changes)
	}
	if _, err := snapshot.Revert(); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt 应消失: err=%v", err)
	}
	if got := read(t, repo, "old.txt"); got != strings.Repeat("line\n", 20) {
		t.Fatalf("old.txt 内容不对: %q", got)
	}
}

// TestDryRunWritesNothing 证明 Changes 不落盘（内容、mtime、目录条目都不变）。
func TestDryRunWritesNothing(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"a.txt": "one\n"})
	commitAll(t, repo)
	snapshot := take(t, repo, undo.Options{})
	writeTree(t, repo, map[string]string{"a.txt": "two\n", "b.txt": "added\n"})
	before, err := os.Stat(filepath.Join(repo, "a.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if _, err := snapshot.Changes(); err != nil {
		t.Fatalf("Changes: %v", err)
	}
	after, err := os.Stat(filepath.Join(repo, "a.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("只读对比改了 mtime: %v → %v", before.ModTime(), after.ModTime())
	}
	if got := read(t, repo, "a.txt"); got != "two\n" {
		t.Fatalf("只读对比改了内容: %q", got)
	}
	entries, err := os.ReadDir(repo)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "a.txt,b.txt" {
		t.Fatalf("dry-run 之后工作区条目变了: %v", names)
	}
}

// TestPathFilter 覆盖定向回退与路径校验。
func TestPathFilter(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"a.txt": "a1\n", "b.txt": "b1\n"})
	commitAll(t, repo)
	snapshot := take(t, repo, undo.Options{})
	writeTree(t, repo, map[string]string{"a.txt": "a2\n", "b.txt": "b2\n"})

	changes, err := snapshot.RevertPath("a.txt")
	if err != nil {
		t.Fatalf("RevertPath: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "a.txt" {
		t.Fatalf("changes = %+v, want only a.txt", changes)
	}
	if got := read(t, repo, "a.txt"); got != "a1\n" {
		t.Fatalf("a.txt = %q, want a1", got)
	}
	if got := read(t, repo, "b.txt"); got != "b2\n" {
		t.Fatalf("b.txt = %q, want untouched b2", got)
	}
	if _, err := snapshot.RevertPath("/etc/passwd"); err == nil {
		t.Fatal("绝对路径被接受了")
	}
	if _, err := snapshot.RevertPath("../escape"); err == nil {
		t.Fatal("../ 逃逸被接受了")
	}
	if _, err := snapshot.RevertPath("no-such-file.txt"); err == nil {
		t.Fatal("不存在的路径应当报错")
	}
}

// TestOptionsIgnoreExcludesPaths 证明 Options.Ignore 既不报告也不回退命中的路径。
func TestOptionsIgnoreExcludesPaths(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"keep.txt": "k1\n", "generated/out.go": "o1\n"})
	commitAll(t, repo)
	snapshot := take(t, repo, undo.Options{Ignore: []string{"generated"}})
	writeTree(t, repo, map[string]string{"keep.txt": "k2\n", "generated/out.go": "o2\n"})

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	for _, change := range changes {
		if change.Path == "generated/out.go" {
			t.Fatalf("被忽略的路径出现在变更里: %+v", change)
		}
	}
	if _, err := snapshot.Revert(); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := read(t, repo, "generated/out.go"); got != "o2\n" {
		t.Fatalf("被忽略的路径被回退了: %q", got)
	}
	if got := read(t, repo, "keep.txt"); got != "k1\n" {
		t.Fatalf("keep.txt = %q, want k1", got)
	}
}

// TestSubdirWorkspace 证明工作区根是仓库子目录时只覆盖该子树，路径相对子目录。
func TestSubdirWorkspace(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"outside.txt": "o1\n", "sub/inside.txt": "i1\n"})
	commitAll(t, repo)
	sub := filepath.Join(repo, "sub")

	snapshot := take(t, sub, undo.Options{})
	writeTree(t, repo, map[string]string{"outside.txt": "o2\n", "sub/inside.txt": "i2\n"})

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "inside.txt" {
		t.Fatalf("changes = %+v, want only inside.txt（相对工作区根）", changes)
	}
	if _, err := snapshot.Revert(); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := read(t, repo, "sub/inside.txt"); got != "i1\n" {
		t.Fatalf("inside.txt = %q, want i1", got)
	}
	if got := read(t, repo, "outside.txt"); got != "o2\n" {
		t.Fatalf("outside.txt = %q, want untouched o2", got)
	}
}

// TestLargeFileIsNotCopied 证明大文件不会以内容复制的形式进入 SDK。
func TestLargeFileIsNotCopied(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"big.bin": strings.Repeat("0123456789abcdef", 1<<20)})
	commitAll(t, repo)
	before, err := os.Stat(filepath.Join(repo, "big.bin"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	started := time.Now()
	snapshot := take(t, repo, undo.Options{})
	elapsed := time.Since(started)
	if elapsed > 10*time.Second {
		t.Fatalf("16MiB 文件的快照用了 %v", elapsed)
	}
	if got := snapshot.Bytes(); got != 0 {
		t.Fatalf("Bytes() = %d, want 0", got)
	}
	after, err := os.Stat(filepath.Join(repo, "big.bin"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("快照改动了工作区文件")
	}
	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

// TestConcurrentBegin 证明并发拍快照互不干扰。
func TestConcurrentBegin(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"a.txt": "a\n"})
	commitAll(t, repo)
	writeTree(t, repo, map[string]string{"a.txt": "dirty\n"})
	tracker := undo.NewTracker(repo, undo.Options{})

	const workers = 8
	turns := make([]int, workers)
	errs := make([]error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			turns[slot], errs[slot] = tracker.Begin()
		}(index)
	}
	wait.Wait()
	seen := make(map[int]bool, workers)
	for index := 0; index < workers; index++ {
		if errs[index] != nil {
			t.Fatalf("worker %d: %v", index, errs[index])
		}
		if seen[turns[index]] {
			t.Fatalf("回合号重复: %d", turns[index])
		}
		seen[turns[index]] = true
	}
	if got := len(tracker.Turns()); got != workers {
		t.Fatalf("Turns() = %d, want %d", got, workers)
	}
}
