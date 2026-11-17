package undo_test

// 第 2D 轮对抗审计（只读）：撤销会不会骗人 / 数据会不会丢。
//
// 本文件用**真实 git 仓库**验证 tools/undo + internal/gitsnap 的核心承诺：
//   - 五种改动（改文件 / 新建 / 删除 / 改权限 / 改名）回退后工作区逐字节回到快照；
//   - 新建的文件被删掉、.gitignore 忽略的文件不动；
//   - 暂存区语义（含"回合前就 staged 的改动"会不会被误伤）；
//   - 指定回合回退是否准确；
//   - refs/rig/undo/* 不泄漏、Prune 真的生效；
//   - git 缺失 / 非仓库 / merge 中间态 / index.lock 下的报错与"不动工作区"。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/internal/gitsnap"
	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// readFileOrEmpty 读文件；不存在返回 ""。
func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(raw)
}

// fileMode 返回权限位；不存在返回 0。
func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Mode().Perm()
}

// refCount 统计 refs/rig/undo/* 的数量。
func refCount(t *testing.T, dir string) int {
	t.Helper()
	out := runGit(t, dir, "for-each-ref", "--format=%(refname)", "refs/rig/undo/")
	fields := strings.Fields(out)
	return len(fields)
}

// U1【最核心】五种改动 → Revert(0) → 工作区逐字节回到快照。
func TestAuditRevertRestoresFiveChangeKinds(t *testing.T) {
	dir := newRepo(t)
	writeTree(t, dir, map[string]string{
		"keep.txt":      "unchanged\n",
		"modify.txt":    "original\n",
		"delete-me.txt": "will vanish\n",
		"rename-me.txt": "rename body\n",
		"chmod-me.sh":   "#!/bin/sh\necho hi\n",
		"ignored/cache": "ignored content\n",
	})
	writeTree(t, dir, map[string]string{".gitignore": "ignored/\n"})
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")

	tracker := undo.NewTracker(dir, undo.Options{})
	turn, err := tracker.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// 五种改动。
	writeTree(t, dir, map[string]string{"modify.txt": "MODIFIED deeply\nsecond line\n"}) // 改文件
	writeTree(t, dir, map[string]string{"brand-new.txt": "new file\n"})                  // 新建
	if err := os.Remove(filepath.Join(dir, "delete-me.txt")); err != nil {               // 删除
		t.Fatalf("Remove: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "chmod-me.sh"), 0o755); err != nil { // 改权限
		t.Fatalf("Chmod: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, "rename-me.txt"), filepath.Join(dir, "renamed.txt")); err != nil { // 改名
		t.Fatalf("Rename: %v", err)
	}
	writeTree(t, dir, map[string]string{"ignored/cache": "agent wrote here too\n"}) // 被忽略的文件也改了

	changes, err := tracker.Revert(0)
	if err != nil {
		t.Fatalf("Revert(0): %v", err)
	}

	// 1) 改文件：逐字节回到快照。
	if got := readFileOrEmpty(t, filepath.Join(dir, "modify.txt")); got != "original\n" {
		t.Fatalf("modify.txt 没回到快照: %q", got)
	}
	// 2) 新建文件：必须被删掉。
	if _, err := os.Stat(filepath.Join(dir, "brand-new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("回合里新建的文件没有被删除: %v", err)
	}
	// 3) 删除的文件：必须回来且内容一致。
	if got := readFileOrEmpty(t, filepath.Join(dir, "delete-me.txt")); got != "will vanish\n" {
		t.Fatalf("delete-me.txt 没恢复: %q", got)
	}
	// 4) 权限：回到 0644。
	if mode := fileMode(t, filepath.Join(dir, "chmod-me.sh")); mode != 0o644 {
		t.Fatalf("chmod-me.sh 权限没回到 0644: %o", mode)
	}
	// 5) 改名：源回来、目标消失。
	if got := readFileOrEmpty(t, filepath.Join(dir, "rename-me.txt")); got != "rename body\n" {
		t.Fatalf("rename-me.txt 没恢复: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "renamed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("改名后的目标文件没有被删除")
	}
	// 未改动文件不受影响。
	if got := readFileOrEmpty(t, filepath.Join(dir, "keep.txt")); got != "unchanged\n" {
		t.Fatalf("无关文件被动了: %q", got)
	}
	// 6) .gitignore 忽略的文件：不动（文档承诺）。
	if got := readFileOrEmpty(t, filepath.Join(dir, "ignored/cache")); got != "agent wrote here too\n" {
		t.Fatalf("被 .gitignore 忽略的文件被回退了（应当不动）: %q", got)
	}
	// 7) 工作区必须干净（回到 base 提交的状态）。
	status := runGit(t, dir, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Fatalf("回退后工作区不干净:\n%s", status)
	}
	// 8) 变更清单要覆盖五种改动。
	ops := make([]string, 0, len(changes))
	for _, change := range changes {
		ops = append(ops, change.Op+":"+change.Path)
	}
	sort.Strings(ops)
	t.Logf("turn=%d 回退了 %d 条: %v", turn, len(changes), ops)
	for _, want := range []string{"modify", "add", "delete", "rename"} {
		found := false
		for _, item := range ops {
			if strings.HasPrefix(item, want+":") {
				found = true
			}
		}
		if !found {
			t.Fatalf("变更清单缺少 %s 类: %v", want, ops)
		}
	}
	// chmod 只改权限：应当被识别为 modify（内容未变）。
	if !strings.Contains(strings.Join(ops, ","), "chmod-me.sh") {
		t.Fatalf("权限变更没有被报告: %v", ops)
	}
}

// U2【暂存区】回合前就 staged 的改动会不会被误伤？
//
// 两种情形分开测：
//
//	A. 用户 staged 后工作区没再改（index == worktree）：回退后暂存内容应当保留。
//	B. 用户 staged v2 后又把工作区改成 v3（index != worktree）：回退后 index 会变成
//	   快照里的工作区内容 v3——**v2 的暂存状态消失了**，这是当前语义的边界。
func TestAuditRevertAndPreTurnStagedChanges(t *testing.T) {
	t.Run("A_index_等于_worktree", func(t *testing.T) {
		dir := newRepo(t)
		writeTree(t, dir, map[string]string{"a.txt": "v1\n"})
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-qm", "base")

		// 回合前：用户 stage 了 v2。
		writeTree(t, dir, map[string]string{"a.txt": "v2 staged\n"})
		runGit(t, dir, "add", "a.txt")

		tracker := undo.NewTracker(dir, undo.Options{})
		if _, err := tracker.Begin(); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		// 回合里继续改。
		writeTree(t, dir, map[string]string{"a.txt": "v3 during turn\n"})

		if _, err := tracker.Revert(0); err != nil {
			t.Fatalf("Revert: %v", err)
		}

		if got := readFileOrEmpty(t, filepath.Join(dir, "a.txt")); got != "v2 staged\n" {
			t.Fatalf("工作区没回到回合前: %q", got)
		}
		cached := runGit(t, dir, "diff", "--cached", "--", "a.txt")
		if !strings.Contains(cached, "+v2 staged") {
			t.Fatalf("回合前 staged 的内容丢了（--cached diff %q）", cached)
		}
	})

	t.Run("B_index_不等于_worktree", func(t *testing.T) {
		dir := newRepo(t)
		writeTree(t, dir, map[string]string{"a.txt": "v1\n"})
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-qm", "base")

		// 回合前：stage v2，再把工作区改成 v3（典型的"部分暂存"）。
		writeTree(t, dir, map[string]string{"a.txt": "v2 staged\n"})
		runGit(t, dir, "add", "a.txt")
		writeTree(t, dir, map[string]string{"a.txt": "v3 worktree\n"})

		tracker := undo.NewTracker(dir, undo.Options{})
		if _, err := tracker.Begin(); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		writeTree(t, dir, map[string]string{"a.txt": "v4 during turn\n"})
		if _, err := tracker.Revert(0); err != nil {
			t.Fatalf("Revert: %v", err)
		}

		worktree := readFileOrEmpty(t, filepath.Join(dir, "a.txt"))
		if worktree != "v3 worktree\n" {
			t.Fatalf("工作区没回到回合前的工作区内容: %q", worktree)
		}
		cached := runGit(t, dir, "diff", "--cached", "--", "a.txt")
		// 记录实际语义：index 被 checkout 成了快照的工作区内容（v3），v2 的暂存状态不再
		// 单独存在。若这里变成了 v2，说明实现升级为"精确恢复用户的 index"。
		if strings.Contains(cached, "+v2 staged") {
			t.Logf("index 被精确恢复成回合前的暂存内容 v2（更强）")
		} else {
			t.Logf("⚠️ 语义边界：回合前 staged 的 v2 被快照的工作区内容 v3 取代（--cached diff=%q）", cached)
		}
	})
}

// U3：五个回合后 Revert(2) —— 回退"从第 2 回合开始"的所有改动。
func TestAuditRevertSpecificTurnAfterFiveTurns(t *testing.T) {
	dir := newRepo(t)
	writeTree(t, dir, map[string]string{"base.txt": "base\n"})
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")

	tracker := undo.NewTracker(dir, undo.Options{})
	for turn := 1; turn <= 5; turn++ {
		if _, err := tracker.Begin(); err != nil {
			t.Fatalf("Begin(%d): %v", turn, err)
		}
		writeTree(t, dir, map[string]string{fileNameForTurn(turn): "turn content\n"})
	}

	// 第 2 回合开始时的状态里，turn2..turn5 的文件都不存在。
	changes, err := tracker.Revert(2)
	if err != nil {
		t.Fatalf("Revert(2): %v", err)
	}
	for turn := 2; turn <= 5; turn++ {
		name := fileNameForTurn(turn)
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s（第 %d 回合创建）应当被回退删除", name, turn)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "base.txt")); err != nil {
		t.Fatalf("base.txt 不该被删: %v", err)
	}
	if len(changes) != 4 {
		t.Fatalf("回退条数 = %d, want 4: %+v", len(changes), changes)
	}
	t.Logf("保留的回合: %v", tracker.Turns())
}

// fileNameForTurn 给每个回合一个不同的文件名。
func fileNameForTurn(turn int) string {
	return "turn" + string(rune('0'+turn)) + ".txt"
}

// U4：refs/rig/undo/* 不泄漏，Prune 生效。
func TestAuditUndoRefsDoNotLeak(t *testing.T) {
	dir := newRepo(t)
	writeTree(t, dir, map[string]string{"a.txt": "a\n"})
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")

	// 直接打 20 份快照（不经过 Tracker，测 gitsnap 层的引用增长）。
	for i := 0; i < 20; i++ {
		writeTree(t, dir, map[string]string{"a.txt": strings.Repeat("x", i+1) + "\n"})
		if _, err := undo.Take(dir, undo.Options{}); err != nil {
			t.Fatalf("Take(%d): %v", i, err)
		}
	}
	refs := refCount(t, dir)
	if refs != 20 {
		t.Fatalf("20 次 Take 后引用数 = %d, want 20", refs)
	}
	// gitsnap.Prune 只保留 5 个（undo 包本身不导出 Prune；自动裁剪在 Snapshot 里）。
	if err := gitsnap.Prune(context.Background(), dir, 5); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if refs = refCount(t, dir); refs != 5 {
		t.Fatalf("Prune(5) 后引用数 = %d, want 5", refs)
	}
	t.Logf("引用增长/修剪符合预期：20 → 5")
}

// U5：git 不在 PATH → 报错可读，且不动工作区。
func TestAuditUndoWithoutGitBinary(t *testing.T) {
	dir := newRepo(t)
	writeTree(t, dir, map[string]string{"a.txt": "a\n"})
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")

	before := readFileOrEmpty(t, filepath.Join(dir, "a.txt"))

	// 把 PATH 指向一个空目录，让 git 找不到。
	empty := t.TempDir()
	saved := os.Getenv("PATH")
	if err := os.Setenv("PATH", empty); err != nil {
		t.Fatalf("Setenv: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", saved) })

	if _, err := undo.Take(dir, undo.Options{}); err == nil {
		t.Fatal("git 不可用却没有报错")
	} else {
		t.Logf("git 不可用时的错误：%v", err)
	}
	// 工作区没有被破坏。
	if got := readFileOrEmpty(t, filepath.Join(dir, "a.txt")); got != before {
		t.Fatalf("git 不可用时工作区被改了: %q → %q", before, got)
	}
}

// U6：不是 git 仓库 → ErrNotGitRepo，工作区不动。
func TestAuditUndoOnNonRepo(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.txt": "a\n"})

	_, err := undo.Take(dir, undo.Options{})
	if err == nil {
		t.Fatal("非 git 仓库却成功了")
	}
	if !errors.Is(err, undo.ErrNotGitRepo) {
		t.Fatalf("错误不是 ErrNotGitRepo: %v", err)
	}
	if got := readFileOrEmpty(t, filepath.Join(dir, "a.txt")); got != "a\n" {
		t.Fatalf("非仓库目录被改了: %q", got)
	}
}

// U7：merge 中间态（MERGE_HEAD 存在）→ 不破坏用户状态。
func TestAuditUndoDuringMerge(t *testing.T) {
	dir := newRepo(t)
	writeTree(t, dir, map[string]string{"a.txt": "base\n"})
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	// 造一个 MERGE_HEAD，模拟"正在 merge"。
	if err := os.WriteFile(filepath.Join(dir, ".git", "MERGE_HEAD"), []byte(head+"\n"), 0o644); err != nil {
		t.Fatalf("写 MERGE_HEAD: %v", err)
	}

	tracker := undo.NewTracker(dir, undo.Options{})
	turn, err := tracker.Begin()
	if err != nil {
		t.Logf("merge 中间态 Begin 失败（可接受，只要报错可读且不动环境）: %v", err)
		// 工作区与 MERGE_HEAD 必须还在。
		if _, statErr := os.Stat(filepath.Join(dir, ".git", "MERGE_HEAD")); statErr != nil {
			t.Fatalf("MERGE_HEAD 被弄丢了: %v", statErr)
		}
		if got := readFileOrEmpty(t, filepath.Join(dir, "a.txt")); got != "base\n" {
			t.Fatalf("工作区被改了: %q", got)
		}
		return
	}
	// 允许成功，但回退后 MERGE_HEAD 与工作区都必须完好。
	writeTree(t, dir, map[string]string{"a.txt": "turn change\n"})
	if _, err := tracker.Revert(turn); err != nil {
		t.Logf("merge 中间态 Revert 报错（可接受）: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".git", "MERGE_HEAD")); statErr != nil {
		t.Fatalf("回退把 MERGE_HEAD 删了（会破坏用户的 merge）: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".git", "HEAD")); statErr != nil {
		t.Fatalf("HEAD 没了: %v", statErr)
	}
}

// U8【正面验证】陈旧的 .git/index.lock **不会**阻塞 undo，也不会让快照变错。
//
// 原因值得记下来：internal/gitsnap 的 worktreeTree 不直接动真实 index，而是复制一份
// 到临时文件并设 GIT_INDEX_FILE，再用 `git add -A` + `write-tree`。因此即使真的
// index.lock 存在（另一个 git 进程在跑，或上次崩溃留下），快照仍然是**工作区**的
// 正确状态。
//
// 这条用例的锋利之处在"脏工作区"：如果实现退化成"stash create 失败 → 退回 HEAD"，
// 那么随后的 Revert 会**丢掉用户回合前就有的未提交改动**。这里正是要证明它没退化。
func TestAuditStaleIndexLockKeepsSnapshotCorrect(t *testing.T) {
	dir := newRepo(t)
	writeTree(t, dir, map[string]string{"a.txt": "v1\n"})
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")

	// 回合前：用户自己在编辑器里改了文件（未提交、未暂存）。
	writeTree(t, dir, map[string]string{"a.txt": "USER UNCOMMITTED\n"})

	// 陈旧的锁。
	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("写 index.lock: %v", err)
	}

	tracker := undo.NewTracker(dir, undo.Options{})
	turn, err := tracker.Begin()
	if err != nil {
		t.Fatalf("陈旧 index.lock 不该阻塞快照（实现用了临时 index）: %v", err)
	}

	// 回合里 agent 改了同一个文件，锁也清掉了。
	if err := os.Remove(lock); err != nil {
		t.Fatalf("Remove lock: %v", err)
	}
	writeTree(t, dir, map[string]string{"a.txt": "AGENT CHANGE\n"})

	if _, err := tracker.Revert(turn); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	// 必须回到"用户回合前的未提交内容"，而不是 HEAD 的 v1。
	if got := readFileOrEmpty(t, filepath.Join(dir, "a.txt")); got != "USER UNCOMMITTED\n" {
		t.Fatalf("快照在 index.lock 下退化了：回退到 %q，期望保留用户未提交的 %q", got, "USER UNCOMMITTED\n")
	}
}

// requireGitBinary 报告 git 是否可用（U5 会在进程内改 PATH，单独隔离）。
