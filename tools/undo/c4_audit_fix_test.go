package undo_test

// C4 审计（.audit/audit/c4-undo.md）里文件后端发现的守护用例。
//
// 每条用例对应一个发现，断言的是"用户/模型能看到且不会被破坏什么"，不碰内部结构：
//   - C4-1 清单与镜像同命名空间：工作区里的 manifest.json 不能被清单覆盖；
//   - C4-2 符号链接写穿：不跟随链接写，回退结果如实报告失败；
//   - C4-5 同 Dir 两个实例的回合号不能撞车；
//   - C4-7 文件换成目录：原因文案准确，且不销毁目录里的新文件；
//   - C4-8 只读目标的回退（以非特权用户复跑）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// TestFileBackendManifestNameDoesNotCollide 守护 C4-1：工作区根有 manifest.json 时，
// 它的快照副本不能与清单同名。旧实现里未改动也会报 modify，Revert 更会把清单 JSON
// 写回用户文件（内容销毁）。
func TestFileBackendManifestNameDoesNotCollide(t *testing.T) {
	tracker, root := newFileTracker(t, undo.FileOptions{})
	const userManifest = "{\"mine\":true}\n"
	writeTree(t, root, map[string]string{"manifest.json": userManifest})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	changes, err := tracker.Changes(0, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("未改动的 manifest.json 被误报为变更: %+v", changes)
	}

	// 真的改了它之后，回退必须写回用户原来的 JSON，而不是清单。
	writeTree(t, root, map[string]string{"manifest.json": "{\"mine\":\"changed\"}\n"})
	changes, err = tracker.Changes(0, "")
	if err != nil {
		t.Fatalf("Changes after edit: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "manifest.json" || changes[0].Op != "modify" {
		t.Fatalf("Changes = %+v, want 只有 manifest.json 的 modify", changes)
	}
	if _, err := tracker.Revert(0); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := read(t, root, "manifest.json"); got != userManifest {
		t.Fatalf("Revert 把用户文件 manifest.json 写成了 %q, want %q", got, userManifest)
	}
}

// TestFileBackendSymlinkReplacementIsReportedNotWritten 守护 C4-2：回合里把普通文件
// 换成符号链接后，变更要按"类型变化"报告（不是 delete），Revert 不能跟随链接写到
// 工作区外，也不能宣称成功。
func TestFileBackendSymlinkReplacementIsReportedNotWritten(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	tracker, root := newFileTracker(t, undo.FileOptions{})
	writeTree(t, root, map[string]string{"a.txt": "v1\n"})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.Symlink(outside, filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	changes, err := tracker.Changes(0, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Changes = %+v, want 只有 a.txt 一条", changes)
	}
	if got := changes[0]; got.Op != "modify" || got.TypeChange != "file->symlink" || got.Reason == "" {
		t.Fatalf("符号链接替换的报告 = %+v, want modify + type file->symlink + 不可回退原因", got)
	}

	reverted, err := tracker.Revert(0)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if len(reverted) != 1 {
		t.Fatalf("Revert = %+v, want 一条", reverted)
	}
	if reverted[0].Reverted || reverted[0].Reason == "" {
		t.Fatalf("符号链接目标上的 Revert = %+v, want 假成功被拒（Reverted=false + Reason）", reverted[0])
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Fatalf("Revert 写穿了符号链接，在工作区外创建了文件（err=%v）", err)
	}
	info, err := os.Lstat(filepath.Join(root, "a.txt"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("工作区的 a.txt 仍然是符号链接时不该被改动: mode=%v err=%v", info.Mode(), err)
	}
}

// TestFileTrackerTurnNumbersDoNotCollideAcrossInstances 守护 C4-5：同一 Dir 上的两个
// Tracker 顺序 Begin 不能拿到同一个回合号（否则后拍者会覆盖前者的快照）。
func TestFileTrackerTurnNumbersDoNotCollideAcrossInstances(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "undo")
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	first, err := undo.NewFileTracker(root, undo.FileOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewFileTracker first: %v", err)
	}
	second, err := undo.NewFileTracker(root, undo.FileOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewFileTracker second: %v", err)
	}
	if got, err := first.Begin(); err != nil || got != 1 {
		t.Fatalf("first.Begin = %d, %v; want 1", got, err)
	}
	if got, err := second.Begin(); err != nil || got != 2 {
		t.Fatalf("second.Begin = %d, %v; want 2", got, err)
	}
	if got, err := first.Begin(); err != nil || got != 3 {
		t.Fatalf("另一个实例 Begin 之后，first.Begin = %d, %v; want 3（不能撞号）", got, err)
	}
}

// TestFileBackendFileReplacedByDirectoryIsReported 守护 C4-7：回合里把文件换成同名
// 目录时要给出真实原因（当前路径成了目录，不是副本不可读），并且不能把目录里的新
// 文件当"本回合新增"删掉。
func TestFileBackendFileReplacedByDirectoryIsReported(t *testing.T) {
	tracker, root := newFileTracker(t, undo.FileOptions{})
	writeTree(t, root, map[string]string{"a.txt": "v1\n"})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt/inner.txt": "new\n"})

	changes, err := tracker.Changes(0, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Changes = %+v, want 只有 a.txt 一条（目录内容不应被当成新增）", changes)
	}
	change := changes[0]
	if change.Op != "modify" || !strings.Contains(change.Reason, "directory") {
		t.Fatalf("报告 = %+v, want modify + 说明当前路径成了目录", change)
	}
	if strings.Contains(change.Reason, "snapshot copy is unreadable") {
		t.Fatalf("原因文案错了（副本是好的）: %q", change.Reason)
	}

	reverted, err := tracker.Revert(0)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if len(reverted) != 1 || reverted[0].Reverted || reverted[0].Reason == "" {
		t.Fatalf("Revert = %+v, want 一条如实的不可回退项", reverted)
	}
	if got := read(t, root, "a.txt/inner.txt"); got != "new\n" {
		t.Fatalf("目录里的新文件被销毁了: %q", got)
	}
	if info, err := os.Lstat(filepath.Join(root, "a.txt")); err != nil || !info.IsDir() {
		t.Fatalf("a.txt 仍然应当是我们看到的目录: info=%v err=%v", info, err)
	}
}

// TestFileBackendRestoresReadOnlyTarget 守护 C4-8：目标文件当前是 0444 时也要能回退
// （回退后恢复快照里的权限位）。
//
// 以 root 运行时 EACCES 不会发生，用例会把测试二进制复制到可执行位置后以 uid 65534
// 复跑自己；没有 setpriv 时跳过。
func TestFileBackendRestoresReadOnlyTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的权限位语义不同")
	}
	if os.Geteuid() == 0 {
		rerunAsUnprivileged(t)
		return
	}
	tracker, root := newFileTracker(t, undo.FileOptions{})
	writeTree(t, root, map[string]string{"a.txt": "v1\n"})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// 改成 v2 之后把写位去掉：非特权用户对 0444 文件没有写权限。
	writeTree(t, root, map[string]string{"a.txt": "v2\n"})
	if err := os.Chmod(filepath.Join(root, "a.txt"), 0o444); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	changes, err := tracker.Changes(0, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Op != "modify" {
		t.Fatalf("Changes = %+v, want 只有 a.txt 的 modify", changes)
	}
	reverted, err := tracker.Revert(0)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if len(reverted) != 1 || !reverted[0].Reverted || reverted[0].Reason != "" {
		t.Fatalf("只读目标的 Revert = %+v, want 成功回退", reverted)
	}
	if got := read(t, root, "a.txt"); got != "v1\n" {
		t.Fatalf("a.txt = %q, want v1", got)
	}
	info, err := os.Stat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("回退后权限 = %v, want 0644（快照记录的权限位）", info.Mode().Perm())
	}
}

// rerunAsUnprivileged 以 uid 65534 复跑当前用例（只有非 root 才会真的撞上 EACCES）。
func rerunAsUnprivileged(t *testing.T) {
	t.Helper()
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		t.Skip("以 root 运行且没有 setpriv：无法以非特权用户复现 EACCES")
	}
	// 测试二进制在 go-build 缓存里，nobody 不一定能执行；复制到一个交给它的目录里。
	// 同一个目录也当 TMPDIR：本环境 /tmp 是 0755 root，nobody 写不进去。这里不能用
	// t.TempDir()——它的上层目录是 root 0700，nobody 连穿都穿不过去。
	base, err := os.MkdirTemp("", "undo-c4-readonly-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	stage := filepath.Join(base, "undo-readonly.test")
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatalf("读取测试二进制: %v", err)
	}
	if err := os.WriteFile(stage, self, 0o755); err != nil {
		t.Fatalf("复制测试二进制: %v", err)
	}
	if err := os.Chown(base, 65534, 65534); err != nil {
		t.Skipf("无法把临时目录交给 uid 65534: %v", err)
	}
	command := exec.Command(setpriv,
		"--reuid=65534", "--regid=65534", "--clear-groups",
		stage, "-test.run=^TestFileBackendRestoresReadOnlyTarget$", "-test.v")
	command.Env = append(os.Environ(), "TMPDIR="+base, "HOME="+base)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("以非特权用户复跑失败: %v\n%s", err, out)
	}
}

// TestFileBackendRestoresFileWhenParentBecameFile 守护 C4-7 的另一面：快照里有 a/b.txt、
// 回合里把目录 a 换成了同名文件时，回退 a/b.txt 必须先删掉挡路的文件 a（只删文件/链接）
// 再建目录，而不是报 "not a directory"。
func TestFileBackendRestoresFileWhenParentBecameFile(t *testing.T) {
	tracker, root := newFileTracker(t, undo.FileOptions{})
	writeTree(t, root, map[string]string{"a/b.txt": "v1\n"})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "a")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	writeTree(t, root, map[string]string{"a": "not a directory\n"})

	reverted, err := tracker.RevertPath(0, "a/b.txt")
	if err != nil {
		t.Fatalf("RevertPath: %v", err)
	}
	if len(reverted) != 1 || !reverted[0].Reverted || reverted[0].Reason != "" {
		t.Fatalf("RevertPath = %+v, want 成功恢复 a/b.txt", reverted)
	}
	if got := read(t, root, "a/b.txt"); got != "v1\n" {
		t.Fatalf("a/b.txt = %q, want v1", got)
	}
}
