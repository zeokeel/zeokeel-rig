package gitsnap_test

// gitsnap 的行为测试：全部在真实的临时 git 仓库里跑（git 不可用时跳过）。
//
// 这里只钉**语义**：快照不动 HEAD/索引、diff 认出增删改、回退把工作区恢复原样、
// 未跟踪文件与 .gitignore 的边界。实现细节（临时索引、元数据提交）不在这里断言。

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

	"github.com/zeokeel/zeokeel-rig/internal/gitsnap"
)

// requireGit 在 git 不可用时跳过测试。
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git 不可用: %v", err)
	}
}

// runGit 执行 git 命令并返回 stdout；失败即 Fatal。
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

// newRepo 创建一个配置好身份的临时仓库。
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

// writeFile 写文件（自动建父目录）。
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %q: %v", rel, err)
	}
}

// readFile 读文件内容。
func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("ReadFile %q: %v", rel, err)
	}
	return string(data)
}

// status 返回 `git status --porcelain` 输出（用于断言我们没有弄脏索引/工作区）。
func status(t *testing.T, dir string) string {
	t.Helper()
	return runGit(t, dir, "status", "--porcelain")
}

// changesByPath 把变更列表变成 path → change。
func changesByPath(changes []gitsnap.FileChange) map[string]gitsnap.FileChange {
	byPath := make(map[string]gitsnap.FileChange, len(changes))
	for _, change := range changes {
		byPath[change.Path] = change
	}
	return byPath
}

// TestIsRepo 覆盖仓库/非仓库/路径不存在。
func TestIsRepo(t *testing.T) {
	requireGit(t)
	repo := newRepo(t)
	if ok, err := gitsnap.IsRepo(context.Background(), repo); err != nil || !ok {
		t.Fatalf("IsRepo(repo) = %v, %v", ok, err)
	}
	plain := t.TempDir()
	if ok, err := gitsnap.IsRepo(context.Background(), plain); err != nil || ok {
		t.Fatalf("IsRepo(plain) = %v, %v", ok, err)
	}
	if _, err := gitsnap.IsRepo(context.Background(), filepath.Join(plain, "missing")); err == nil {
		t.Fatal("IsRepo(missing) succeeded")
	}
}

// TestSnapshotLeavesHeadAndIndexAlone 证明快照不移动 HEAD、不动分支、不弄脏索引。
func TestSnapshotLeavesHeadAndIndexAlone(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "one\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	// 回合前的脏状态：一个已暂存、一个未暂存、一个未跟踪。
	writeFile(t, repo, "a.txt", "two\n")
	runGit(t, repo, "add", "a.txt")
	writeFile(t, repo, "b.txt", "b\n")
	writeFile(t, repo, "u.txt", "u\n")

	headBefore := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	statusBefore := status(t, repo)

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(id) != 40 {
		t.Fatalf("snapshot id = %q, want a 40-char commit id", id)
	}
	if headAfter := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); headAfter != headBefore {
		t.Fatalf("HEAD 被移动了: %s → %s", headBefore, headAfter)
	}
	if branches := strings.TrimSpace(runGit(t, repo, "for-each-ref", "refs/heads", "--format=%(refname)")); branches != "refs/heads/master" && branches != "refs/heads/main" {
		t.Fatalf("分支被动了: %q", branches)
	}
	if statusAfter := status(t, repo); statusAfter != statusBefore {
		t.Fatalf("索引/工作区被弄脏:\nbefore=%q\nafter =%q", statusBefore, statusAfter)
	}
}

// TestSnapshotOnCleanTree 证明干净工作区也能拍快照，且 Diff 为空。
func TestSnapshotOnCleanTree(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "one\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	changes, err := gitsnap.Diff(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

// TestSnapshotUnbornRepo 证明没有任何提交的仓库也能拍快照（git init 之后的状态）。
func TestSnapshotUnbornRepo(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "one\n")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot(unborn): %v", err)
	}
	writeFile(t, repo, "a.txt", "two\n")
	writeFile(t, repo, "b.txt", "b\n")
	changes, err := gitsnap.Diff(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	byPath := changesByPath(changes)
	if byPath["a.txt"].Op != gitsnap.OpModify {
		t.Fatalf("a.txt = %+v, want modify", byPath["a.txt"])
	}
	if byPath["b.txt"].Op != gitsnap.OpAdd {
		t.Fatalf("b.txt = %+v, want add", byPath["b.txt"])
	}
	if _, err := gitsnap.Revert(context.Background(), repo, id); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := readFile(t, repo, "a.txt"); got != "one\n" {
		t.Fatalf("a.txt = %q, want one", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("b.txt 应被删除: err=%v", err)
	}
}

// TestDiffDetectsAddModifyDelete 覆盖三种基本变更与行数。
func TestDiffDetectsAddModifyDelete(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "keep.txt", "keep\n")
	writeFile(t, repo, "gone.txt", "gone\n")
	writeFile(t, repo, "edit.txt", "line1\nline2\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	writeFile(t, repo, "edit.txt", "line1\nline2\nline3\n")
	if err := os.Remove(filepath.Join(repo, "gone.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeFile(t, repo, "new.txt", "n1\nn2\n")

	changes, err := gitsnap.Diff(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	byPath := changesByPath(changes)
	if got := byPath["new.txt"]; got.Op != gitsnap.OpAdd || got.Added != 2 {
		t.Fatalf("new.txt = %+v, want add +2", got)
	}
	if got := byPath["gone.txt"]; got.Op != gitsnap.OpDelete || got.Removed != 1 {
		t.Fatalf("gone.txt = %+v, want delete -1", got)
	}
	if got := byPath["edit.txt"]; got.Op != gitsnap.OpModify || got.Added != 1 || got.Removed != 0 {
		t.Fatalf("edit.txt = %+v, want modify +1/-0", got)
	}
	if _, tracked := byPath["keep.txt"]; tracked {
		t.Fatalf("keep.txt 不该出现在变更里: %+v", byPath["keep.txt"])
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %d 条, want 3", len(changes))
	}
	for _, change := range changes {
		if change.Path != "new.txt" && change.Path != "gone.txt" && change.Path != "edit.txt" && change.Path != "keep.txt" {
			t.Fatalf("unexpected change %+v", change)
		}
	}
}

// TestRevertRestoresWorktree 证明回退恢复修改/删除并删除新增。
func TestRevertRestoresWorktree(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "edit.txt", "v1\n")
	writeFile(t, repo, "gone.txt", "gone\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	writeFile(t, repo, "edit.txt", "v2\n")
	if err := os.Remove(filepath.Join(repo, "gone.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeFile(t, repo, "added.txt", "a\n")

	applied, err := gitsnap.Revert(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	for _, change := range applied {
		if !change.Reverted {
			t.Fatalf("change 未标记为已回退: %+v", change)
		}
	}
	if got := readFile(t, repo, "edit.txt"); got != "v1\n" {
		t.Fatalf("edit.txt = %q, want v1", got)
	}
	if got := readFile(t, repo, "gone.txt"); got != "gone\n" {
		t.Fatalf("gone.txt = %q, want gone", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "added.txt")); !os.IsNotExist(err) {
		t.Fatalf("added.txt 应被删除: err=%v", err)
	}
	// 再回退一次应当没有任何变更（幂等）。
	again, err := gitsnap.Diff(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("第二次对比仍有变更: %+v", again)
	}
}

// TestRevertUntrackedFiles 覆盖未跟踪文件的两个方向：改过要恢复，删掉要重建。
func TestRevertUntrackedFiles(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "tracked.txt", "t\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")
	writeFile(t, repo, "edited-untracked.txt", "u1\n")
	writeFile(t, repo, "deleted-untracked.txt", "d1\n")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	writeFile(t, repo, "edited-untracked.txt", "u2\n")
	if err := os.Remove(filepath.Join(repo, "deleted-untracked.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	changes, err := gitsnap.Diff(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	byPath := changesByPath(changes)
	if got := byPath["edited-untracked.txt"]; got.Op != gitsnap.OpModify {
		t.Fatalf("edited-untracked.txt = %+v, want modify", got)
	}
	if got := byPath["deleted-untracked.txt"]; got.Op != gitsnap.OpDelete {
		t.Fatalf("deleted-untracked.txt = %+v, want delete", got)
	}

	if _, err := gitsnap.Revert(context.Background(), repo, id); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := readFile(t, repo, "edited-untracked.txt"); got != "u1\n" {
		t.Fatalf("edited-untracked.txt = %q, want u1", got)
	}
	if got := readFile(t, repo, "deleted-untracked.txt"); got != "d1\n" {
		t.Fatalf("deleted-untracked.txt = %q, want d1", got)
	}
	// 恢复之后它们仍然是未跟踪（不能顺手把它们 add 进索引）。
	if porcelain := status(t, repo); strings.Contains(porcelain, "A  ") {
		t.Fatalf("回退把未跟踪文件加进了暂存区: %q", porcelain)
	}
}

// TestRevertIgnoresGitignored 证明被 .gitignore 忽略的文件既不进快照，也不会被删。
func TestRevertIgnoresGitignored(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, ".gitignore", "*.log\nbuild/\n")
	writeFile(t, repo, "a.txt", "a\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	writeFile(t, repo, "debug.log", "noise\n")
	writeFile(t, repo, "build/out.bin", "artifact\n")

	changes, err := gitsnap.Diff(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("被忽略的文件出现在变更里: %+v", changes)
	}
	if _, err := gitsnap.Revert(context.Background(), repo, id); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := readFile(t, repo, "debug.log"); got != "noise\n" {
		t.Fatalf("被忽略的文件被删/改了: %q", got)
	}
	if got := readFile(t, repo, "build/out.bin"); got != "artifact\n" {
		t.Fatalf("被忽略的目录被删/改了: %q", got)
	}
}

// TestRevertUnstagesNewlyAddedFile 证明回合里被 git add 的新文件回退后不再留在暂存区。
func TestRevertUnstagesNewlyAddedFile(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "a\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	writeFile(t, repo, "staged.txt", "s\n")
	runGit(t, repo, "add", "staged.txt")

	if _, err := gitsnap.Revert(context.Background(), repo, id); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "staged.txt")); !os.IsNotExist(err) {
		t.Fatalf("staged.txt 应被删除: err=%v", err)
	}
	if porcelain := status(t, repo); strings.Contains(porcelain, "staged.txt") {
		t.Fatalf("回退之后 staged.txt 仍在状态里: %q", porcelain)
	}
}

// TestRevertOnlyThisTurn 证明快照之前就存在的脏改动不会被回退掉。
func TestRevertOnlyThisTurn(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "base\n")
	writeFile(t, repo, "b.txt", "base\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	// 回合之前的用户改动（应当保留）。
	writeFile(t, repo, "a.txt", "user-preturn\n")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// 回合里的改动。
	writeFile(t, repo, "b.txt", "agent-changed\n")

	if _, err := gitsnap.Revert(context.Background(), repo, id); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := readFile(t, repo, "a.txt"); got != "user-preturn\n" {
		t.Fatalf("回合前的用户改动被抹掉了: %q", got)
	}
	if got := readFile(t, repo, "b.txt"); got != "base\n" {
		t.Fatalf("b.txt = %q, want base", got)
	}
}

// TestRevertRenameAndModeAndSymlink 覆盖 rename / 可执行位 / 符号链接三类变更。
func TestRevertRenameAndModeAndSymlink(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "old.txt", "content\n")
	writeFile(t, repo, "script.sh", "#!/bin/sh\n")
	writeFile(t, repo, "target.txt", "t\n")
	if err := os.Symlink("target.txt", filepath.Join(repo, "link")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	runGit(t, repo, "mv", "old.txt", "new.txt")
	if err := os.Chmod(filepath.Join(repo, "script.sh"), 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "link")); err != nil {
		t.Fatalf("Remove link: %v", err)
	}
	writeFile(t, repo, "link", "now a file\n")

	changes, err := gitsnap.Diff(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	byPath := changesByPath(changes)
	if got := byPath["new.txt"]; got.Op != gitsnap.OpRename || got.OldPath != "old.txt" {
		t.Fatalf("new.txt = %+v, want rename from old.txt", got)
	}
	if got := byPath["script.sh"]; !got.ModeOnly {
		t.Fatalf("script.sh = %+v, want ModeOnly", got)
	}
	if got := byPath["link"]; got.TypeChange != "symlink->file" {
		t.Fatalf("link = %+v, want symlink->file", got)
	}

	if _, err := gitsnap.Revert(context.Background(), repo, id); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := readFile(t, repo, "old.txt"); got != "content\n" {
		t.Fatalf("old.txt = %q, want restored", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt 应被删除: err=%v", err)
	}
	info, err := os.Stat(filepath.Join(repo, "script.sh"))
	if err != nil {
		t.Fatalf("Stat script.sh: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("script.sh 的权限没有恢复到 0644: %v", info.Mode())
	}
	if target, err := os.Readlink(filepath.Join(repo, "link")); err != nil || target != "target.txt" {
		t.Fatalf("link 没有恢复成符号链接: %q (err=%v)", target, err)
	}
}

// TestRevertPathOnlyTouchesTarget 证明定向回退只动一条路径。
func TestRevertPathOnlyTouchesTarget(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "a1\n")
	writeFile(t, repo, "b.txt", "b1\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	writeFile(t, repo, "a.txt", "a2\n")
	writeFile(t, repo, "b.txt", "b2\n")

	if _, err := gitsnap.RevertPath(context.Background(), repo, id, "a.txt"); err != nil {
		t.Fatalf("RevertPath: %v", err)
	}
	if got := readFile(t, repo, "a.txt"); got != "a1\n" {
		t.Fatalf("a.txt = %q, want a1", got)
	}
	if got := readFile(t, repo, "b.txt"); got != "b2\n" {
		t.Fatalf("b.txt = %q, want untouched b2", got)
	}
	if _, err := gitsnap.RevertPath(context.Background(), repo, id, "../escape"); err == nil {
		t.Fatal("RevertPath(../escape) succeeded")
	}
	if _, err := gitsnap.RevertPath(context.Background(), repo, id, "/etc/passwd"); err == nil {
		t.Fatal("RevertPath(absolute) succeeded")
	}
}

// TestSubdirScope 证明从子目录工作时只覆盖该子树，且返回的路径相对该子目录。
func TestSubdirScope(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "outside.txt", "o1\n")
	writeFile(t, repo, "sub/inside.txt", "i1\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	sub := filepath.Join(repo, "sub")
	id, err := gitsnap.Snapshot(context.Background(), sub)
	if err != nil {
		t.Fatalf("Snapshot(sub): %v", err)
	}
	writeFile(t, repo, "outside.txt", "o2\n")
	writeFile(t, repo, "sub/inside.txt", "i2\n")

	changes, err := gitsnap.Diff(context.Background(), sub, id)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "inside.txt" {
		t.Fatalf("changes = %+v, want only inside.txt（路径应相对工作区根）", changes)
	}
	if _, err := gitsnap.Revert(context.Background(), sub, id); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := readFile(t, repo, "sub/inside.txt"); got != "i1\n" {
		t.Fatalf("inside.txt = %q, want i1", got)
	}
	if got := readFile(t, repo, "outside.txt"); got != "o2\n" {
		t.Fatalf("outside.txt = %q, want untouched o2", got)
	}
}

// TestPathsAndPrune 覆盖快照路径清单与引用修剪。
func TestPathsAndPrune(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "a\n")
	writeFile(t, repo, "sub/b.txt", "b\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	paths, err := gitsnap.Paths(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	if strings.Join(paths, ",") != "a.txt,sub/b.txt" {
		t.Fatalf("paths = %v", paths)
	}
	for index := 0; index < 5; index++ {
		if _, err := gitsnap.Snapshot(context.Background(), repo); err != nil {
			t.Fatalf("Snapshot #%d: %v", index, err)
		}
	}
	if err := gitsnap.Prune(context.Background(), repo, 2); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	refs := strings.Fields(runGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/rig/undo/"))
	if len(refs) != 2 {
		t.Fatalf("修剪后剩 %d 个引用, want 2", len(refs))
	}
	// 仍然能回退到被保留的快照之一。
	latest := runGit(t, repo, "rev-parse", refs[len(refs)-1])
	if _, err := gitsnap.Diff(context.Background(), repo, strings.TrimSpace(latest)); err != nil {
		t.Fatalf("Diff(latest): %v", err)
	}
}

// TestLargeFileIsNotCopied 证明大文件不会以"内容复制"的方式进入 SDK：
// 快照不读内容到内存、不改工作区文件，且耗时与文件大小无关（git 只存对象）。
func TestLargeFileIsNotCopied(t *testing.T) {
	repo := newRepo(t)
	blob := strings.Repeat("0123456789abcdef", 1<<20) // 16 MiB
	writeFile(t, repo, "big.bin", blob)
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")
	before, err := os.Stat(filepath.Join(repo, "big.bin"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	started := time.Now()
	id, err := gitsnap.Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed > 10*time.Second {
		t.Fatalf("快照 16MiB 文件用了 %v（不该逐字节复制到 SDK 侧）", elapsed)
	}
	after, err := os.Stat(filepath.Join(repo, "big.bin"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("快照改动了工作区文件: %v → %v", before.ModTime(), after.ModTime())
	}
	changes, err := gitsnap.Diff(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
	// 大文件仍然在快照里（可以恢复）。
	paths, err := gitsnap.Paths(context.Background(), repo, id)
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	if len(paths) != 1 || paths[0] != "big.bin" {
		t.Fatalf("paths = %v", paths)
	}
}

// TestConcurrentSnapshots 证明并发拍快照互不干扰（每回合 git stash 语义的成本可控）。
func TestConcurrentSnapshots(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "a\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")
	writeFile(t, repo, "a.txt", "dirty\n")

	const workers = 8
	ids := make([]string, workers)
	errs := make([]error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			ids[slot], errs[slot] = gitsnap.Snapshot(context.Background(), repo)
		}(index)
	}
	wait.Wait()
	// 注意：相同状态 + 相同父提交 + 相同消息会产生同一个 commit ID（内容寻址的自然
	// 结果），因此这里不断言 ID 互不相同；只要求每个 ID 都能正常对比、且引用都写成功。
	for index := 0; index < workers; index++ {
		if errs[index] != nil {
			t.Fatalf("worker %d: %v", index, errs[index])
		}
		if len(ids[index]) != 40 {
			t.Fatalf("worker %d id = %q", index, ids[index])
		}
		if _, err := gitsnap.Diff(context.Background(), repo, ids[index]); err != nil {
			t.Fatalf("worker %d Diff: %v", index, err)
		}
	}
	refs := strings.Fields(runGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/rig/undo/"))
	if len(refs) != workers {
		t.Fatalf("引用数 = %d, want %d（每次快照都应留下引用）", len(refs), workers)
	}
}

// TestNotGitRepoErrors 证明非仓库上各入口都返回 ErrNotGitRepo。
func TestNotGitRepoErrors(t *testing.T) {
	requireGit(t)
	plain := t.TempDir()
	if _, err := gitsnap.Snapshot(context.Background(), plain); !errors.Is(err, gitsnap.ErrNotGitRepo) {
		t.Fatalf("Snapshot = %v, want ErrNotGitRepo", err)
	}
	if _, err := gitsnap.Diff(context.Background(), plain, "HEAD"); !errors.Is(err, gitsnap.ErrNotGitRepo) {
		t.Fatalf("Diff = %v, want ErrNotGitRepo", err)
	}
	if _, err := gitsnap.Revert(context.Background(), plain, "HEAD"); !errors.Is(err, gitsnap.ErrNotGitRepo) {
		t.Fatalf("Revert = %v, want ErrNotGitRepo", err)
	}
}

// TestDiffDetectsSameSizeRewrite 是 racy-git 的回归守卫。
//
// 同一时间片内、**大小不变**的改写（"i1\n" → "i2\n"）曾经被漏判：临时索引是刚创建
// 的（mtime=now），git 认为所有条目都比索引旧，于是跳过重新哈希。修法是把副本索引的
// mtime 设成原索引的 mtime，把"是否可能被改写"的判定交回 git。
func TestDiffDetectsSameSizeRewrite(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "same-size.txt", "v0\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "init")

	for round := 0; round < 25; round++ {
		writeFile(t, repo, "same-size.txt", "v1\n")
		id, err := gitsnap.Snapshot(context.Background(), repo)
		if err != nil {
			t.Fatalf("round %d Snapshot: %v", round, err)
		}
		writeFile(t, repo, "same-size.txt", "v2\n")
		changes, err := gitsnap.Diff(context.Background(), repo, id)
		if err != nil {
			t.Fatalf("round %d Diff: %v", round, err)
		}
		if len(changes) != 1 || changes[0].Op != gitsnap.OpModify {
			t.Fatalf("round %d: changes = %+v, want one modify（同尺寸改写必须被发现）", round, changes)
		}
		if _, err := gitsnap.Revert(context.Background(), repo, id); err != nil {
			t.Fatalf("round %d Revert: %v", round, err)
		}
		if got := readFile(t, repo, "same-size.txt"); got != "v1\n" {
			t.Fatalf("round %d: content = %q, want v1", round, got)
		}
	}
}
