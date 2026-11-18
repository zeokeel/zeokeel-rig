package fsguard_test

// fsguard 的黑盒测试。每个用例都围绕一条"能被真实攻击/真实误用触发"的语义：
// 逃逸（绝对路径 / ".." / 符号链接）、写入的链接语义、原子失败不留垃圾、并发。

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/fsguard"
)

// openRoot 在临时目录上打开一个 Root，并在测试结束时关闭。
func openRoot(t *testing.T) (*fsguard.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := fsguard.Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// writeOutside 在根外写一个文件并返回绝对路径。
func writeOutside(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	return path
}

// mustWrite 用 fsguard 写一个文件。
func mustWrite(t *testing.T, root *fsguard.Root, rel, content string) {
	t.Helper()
	if err := root.WriteFileAtomic(rel, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic(%q): %v", rel, err)
	}
}

// mustRead 用 fsguard 读一个文件并返回字符串。
func mustRead(t *testing.T, root *fsguard.Root, rel string) string {
	t.Helper()
	data, err := root.ReadFile(rel)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", rel, err)
	}
	return string(data)
}

// requireEscape 断言 err 是逃逸错误。
func requireEscape(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an escape error, got nil")
	}
	if !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("error = %v, want errors.Is(ErrEscape)", err)
	}
	if !fsguard.IsEscape(err) {
		t.Fatalf("IsEscape(%v) = false", err)
	}
}

// requireNotExist 断言 err 同时满足 fsguard.ErrNotExist 与 os.ErrNotExist。
func requireNotExist(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a not-exist error, got nil")
	}
	if !errors.Is(err, fsguard.ErrNotExist) {
		t.Fatalf("error = %v, want errors.Is(fsguard.ErrNotExist)", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want errors.Is(os.ErrNotExist) as well", err)
	}
}

// leftoverTemps 返回根下所有 fsguard 临时文件（用来证明失败后没留垃圾）。
func leftoverTemps(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.Contains(entry.Name(), ".fsguard-") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// 打开 / 关闭 / 零值
// ---------------------------------------------------------------------------

func TestOpenAndName(t *testing.T) {
	root, dir := openRoot(t)
	if got, want := root.Name(), dir; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
	// 相对路径 + 尾部分隔符会被清理成绝对路径。
	rel, err := fsguard.Open(dir + string(filepath.Separator))
	if err != nil {
		t.Fatalf("Open with trailing separator: %v", err)
	}
	defer rel.Close()
	if got := rel.Name(); got != dir {
		t.Fatalf("Name() = %q, want the cleaned absolute %q", got, dir)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := fsguard.Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
	if _, err := fsguard.Open(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Open(missing) succeeded")
	}
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := fsguard.Open(file); err == nil {
		t.Fatal("Open(a regular file) succeeded")
	}
}

func TestClosedRootReturnsError(t *testing.T) {
	dir := t.TempDir()
	root, err := fsguard.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil (idempotent)", err)
	}
	if _, err := root.ReadFile("a.txt"); err == nil {
		t.Fatal("ReadFile on a closed root succeeded")
	}
	if err := root.WriteFileAtomic("a.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFileAtomic on a closed root succeeded")
	}
}

func TestNilRootMethodsReturnError(t *testing.T) {
	var root *fsguard.Root
	if root.Name() != "" {
		t.Fatalf("nil Name() = %q, want empty", root.Name())
	}
	if err := root.Close(); err != nil {
		t.Fatalf("nil Close() = %v, want nil", err)
	}
	checks := []error{
		func() error { _, err := root.ReadFile("a"); return err }(),
		func() error { _, err := root.ReadFileLimit("a", 1); return err }(),
		func() error { _, err := root.Stat("a"); return err }(),
		func() error { _, err := root.Lstat("a"); return err }(),
		func() error { _, err := root.Readlink("a"); return err }(),
		root.WriteFileAtomic("a", nil, 0o644),
		root.Rename("a", "b"),
		root.Remove("a"),
		root.MkdirAll("a", 0o755),
		root.Chmod("a", 0o644),
	}
	for index, err := range checks {
		if err == nil {
			t.Fatalf("nil root method #%d returned nil error", index)
		}
	}
}

// ---------------------------------------------------------------------------
// 路径校验
// ---------------------------------------------------------------------------

func TestAbsolutePathRejected(t *testing.T) {
	root, dir := openRoot(t)
	for _, path := range []string{filepath.Join(dir, "a.txt"), "/etc/passwd", `C:\Windows\evil.txt`, `\\server\share\evil.txt`} {
		_, err := root.ReadFile(path)
		requireEscape(t, err)
		if err := root.WriteFileAtomic(path, []byte("x"), 0o644); err == nil {
			t.Fatalf("WriteFileAtomic(%q) succeeded", path)
		}
	}
}

func TestParentEscapeRejected(t *testing.T) {
	root, _ := openRoot(t)
	for _, path := range []string{"..", "../x", "a/../../x", "./../x", "a/.."} {
		_, err := root.ReadFile(path)
		if path == "a/.." {
			// "a/.." Clean 之后是 "."：读根本身是允许的（目录读出 EISDIR，属正常）。
			if err != nil && !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "is a directory") {
				t.Fatalf("ReadFile(%q) = %v, want the root itself", path, err)
			}
			continue
		}
		requireEscape(t, err)
	}
}

func TestRootTargetRejectedForWrites(t *testing.T) {
	root, _ := openRoot(t)
	if err := root.WriteFileAtomic(".", []byte("x"), 0o644); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("WriteFileAtomic(\".\") = %v, want ErrEscape", err)
	}
	if err := root.Remove("."); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("Remove(\".\") = %v, want ErrEscape", err)
	}
	if err := root.Chmod(".", 0o700); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("Chmod(\".\") = %v, want ErrEscape", err)
	}
	// 读根本身是合法的。
	if _, err := root.Stat("."); err != nil {
		t.Fatalf("Stat(\".\") = %v, want success", err)
	}
}

func TestDotRelativePathsAccepted(t *testing.T) {
	root, _ := openRoot(t)
	if err := root.MkdirAll("sub", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustWrite(t, root, "./sub/./a.txt", "content")
	if got, want := mustRead(t, root, "sub/a.txt"), "content"; got != want {
		t.Fatalf("read back = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

func TestReadFileMissing(t *testing.T) {
	root, _ := openRoot(t)
	_, err := root.ReadFile("nope.txt")
	requireNotExist(t, err)
	if _, err := root.Stat("nope.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat = %v, want os.ErrNotExist", err)
	}
}

func TestStatFollowsSymlinkLstatDoesNot(t *testing.T) {
	root, dir := openRoot(t)
	mustWrite(t, root, "target.txt", "hello")
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	stat, err := root.Stat("link.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if stat.Size() != 5 {
		t.Fatalf("Stat(link).Size = %d, want the target size 5", stat.Size())
	}
	lstat, err := root.Lstat("link.txt")
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if lstat.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Lstat(link).Mode = %v, want a symlink", lstat.Mode())
	}
}

func TestReadlink(t *testing.T) {
	root, dir := openRoot(t)
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got, err := root.Readlink("link.txt")
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if got != "target.txt" {
		t.Fatalf("Readlink = %q, want the raw target text", got)
	}
	// 绝对目标的链接文本原样返回（读取链接本身不需要"在根内"）。
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "abs.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if got, err := root.Readlink("abs.txt"); err != nil || got != "/etc/passwd" {
		t.Fatalf("Readlink(abs) = (%q, %v), want the raw absolute text", got, err)
	}
}

func TestReadFileLimitBoundaries(t *testing.T) {
	root, _ := openRoot(t)
	mustWrite(t, root, "exact.txt", "0123456789") // 10 字节
	cases := []struct {
		name    string
		limit   int64
		want    string
		wantErr bool
	}{
		{"limit larger", 11, "0123456789", false},
		{"limit exactly equal", 10, "0123456789", false},
		{"limit one byte short", 9, "", true},
		{"limit zero means unlimited", 0, "0123456789", false},
		{"limit negative means unlimited", -1, "0123456789", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := root.ReadFileLimit("exact.txt", tc.limit)
			if tc.wantErr {
				if !errors.Is(err, fsguard.ErrTooLarge) {
					t.Fatalf("err = %v, want ErrTooLarge", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFileLimit(%d): %v", tc.limit, err)
			}
			if string(data) != tc.want {
				t.Fatalf("data = %q, want %q", data, tc.want)
			}
		})
	}
}

// 稀疏文件 10 GiB + 64 字节上限：必须立刻返回 ErrTooLarge（整读会 OOM/耗秒级）。
func TestReadFileLimitDoesNotReadWholeFile(t *testing.T) {
	root, dir := openRoot(t)
	big := filepath.Join(dir, "sparse.bin")
	file, err := os.Create(big)
	if err != nil {
		t.Fatalf("create sparse: %v", err)
	}
	const size = int64(10) << 30
	if err := file.Truncate(size); err != nil {
		file.Close()
		t.Skipf("truncate unsupported here: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close sparse: %v", err)
	}

	start := time.Now()
	_, err = root.ReadFileLimit("sparse.bin", 64)
	elapsed := time.Since(start)
	if !errors.Is(err, fsguard.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ReadFileLimit took %v for a 10 GiB sparse file: it must not read the whole file", elapsed)
	}
}

// ---------------------------------------------------------------------------
// 原子写
// ---------------------------------------------------------------------------

func TestWriteFileAtomicCreatesFile(t *testing.T) {
	root, dir := openRoot(t)
	if err := root.MkdirAll("dir/nested", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustWrite(t, root, "dir/a.txt", "hello")
	data, err := os.ReadFile(filepath.Join(dir, "dir", "a.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("content = %q", data)
	}
	if leftovers := leftoverTemps(t, dir); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestWriteFileAtomicOverwritesAndKeepsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不报告 POSIX 权限位")
	}
	root, dir := openRoot(t)
	mustWrite(t, root, "script.sh", "old")
	mustWrite(t, root, "script.sh", "new content")
	info, err := os.Stat(filepath.Join(dir, "script.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644 as requested", info.Mode().Perm())
	}
	if got := mustRead(t, root, "script.sh"); got != "new content" {
		t.Fatalf("content = %q", got)
	}
}

func TestWriteFileAtomicMissingParentFails(t *testing.T) {
	root, dir := openRoot(t)
	err := root.WriteFileAtomic("missing/dir/a.txt", []byte("x"), 0o644)
	if err == nil {
		t.Fatal("write into a missing parent succeeded")
	}
	if leftovers := leftoverTemps(t, dir); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind after failure: %v", leftovers)
	}
}

func TestWriteFileAtomicTargetIsDirectoryFails(t *testing.T) {
	root, dir := openRoot(t)
	if err := root.MkdirAll("sub", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	err := root.WriteFileAtomic("sub", []byte("x"), 0o644)
	if err == nil {
		t.Fatal("write onto a directory succeeded")
	}
	info, statErr := os.Stat(filepath.Join(dir, "sub"))
	if statErr != nil || !info.IsDir() {
		t.Fatalf("sub was replaced: %v %v", info, statErr)
	}
	if leftovers := leftoverTemps(t, dir); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind after failure: %v", leftovers)
	}
}

// 写穿符号链接：写入链接目标，链接本身保持为链接。
func TestWriteFileAtomicWritesThroughSymlink(t *testing.T) {
	root, dir := openRoot(t)
	mustWrite(t, root, "target.txt", "old")
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := root.WriteFileAtomic("link.txt", []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic(link): %v", err)
	}
	if got := mustRead(t, root, "target.txt"); got != "new" {
		t.Fatalf("target content = %q, want the write to land on the target", got)
	}
	info, err := os.Lstat(filepath.Join(dir, "link.txt"))
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file")
	}
}

// 链式符号链接也要写到最终目标。
func TestWriteFileAtomicChainedSymlink(t *testing.T) {
	root, dir := openRoot(t)
	mustWrite(t, root, "target.txt", "old")
	if err := os.Symlink("target.txt", filepath.Join(dir, "second.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink("second.txt", filepath.Join(dir, "first.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := root.WriteFileAtomic("first.txt", []byte("through two links"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if got := mustRead(t, root, "target.txt"); got != "through two links" {
		t.Fatalf("target content = %q", got)
	}
}

// 指向根外的符号链接：写入必须被拒绝，且链接目标不被修改。
func TestWriteFileAtomicRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, dir := openRoot(t)
	outside := writeOutside(t, "SECRET")
	if err := os.Symlink(outside, filepath.Join(dir, "escape.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	err := root.WriteFileAtomic("escape.txt", []byte("overwritten"), 0o644)
	requireEscape(t, err)
	data, readErr := os.ReadFile(outside)
	if readErr != nil {
		t.Fatalf("read outside: %v", readErr)
	}
	if string(data) != "SECRET" {
		t.Fatalf("outside file was modified: %q", data)
	}
}

// 指向根外的相对符号链接（../outside）同样拒绝。
func TestWriteFileAtomicRejectsRelativeSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, dir := openRoot(t)
	writeOutside(t, "SECRET") // 只为确保外部目录存在
	if err := os.Symlink("../outside.txt", filepath.Join(dir, "escape.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := root.WriteFileAtomic("escape.txt", []byte("x"), 0o644); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("err = %v, want ErrEscape", err)
	}
}

func TestWriteFileAtomicSymlinkLoopRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, dir := openRoot(t)
	if err := os.Symlink("loop.txt", filepath.Join(dir, "loop.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	err := root.WriteFileAtomic("loop.txt", []byte("x"), 0o644)
	if err == nil {
		t.Fatal("write through a symlink loop succeeded")
	}
	if !strings.Contains(err.Error(), "symbolic link") && !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("err = %v, want a symbolic-link error", err)
	}
}

// ---------------------------------------------------------------------------
// 改名 / 删除 / 建目录 / 改权限
// ---------------------------------------------------------------------------

func TestRenameMovesFile(t *testing.T) {
	root, dir := openRoot(t)
	mustWrite(t, root, "a.txt", "content")
	if err := root.Rename("a.txt", "sub/b.txt"); err == nil {
		t.Fatal("Rename into a missing parent succeeded (os.Rename semantics forbid it)")
	}
	if err := root.MkdirAll("sub", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := root.Rename("a.txt", "sub/b.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a.txt still exists: %v", err)
	}
	if got := mustRead(t, root, "sub/b.txt"); got != "content" {
		t.Fatalf("content = %q", got)
	}
}

func TestRenameEscapeRejected(t *testing.T) {
	root, _ := openRoot(t)
	mustWrite(t, root, "a.txt", "x")
	if err := root.Rename("a.txt", "../escaped.txt"); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("Rename to .. = %v, want ErrEscape", err)
	}
	if err := root.Rename("../a.txt", "b.txt"); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("Rename from .. = %v, want ErrEscape", err)
	}
}

// 改名作用于链接本身：目标文件不动。
func TestRenameOperatesOnSymlinkItself(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, dir := openRoot(t)
	mustWrite(t, root, "target.txt", "hello")
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := root.Rename("link.txt", "moved.txt"); err != nil {
		t.Fatalf("Rename(link): %v", err)
	}
	info, err := os.Lstat(filepath.Join(dir, "moved.txt"))
	if err != nil {
		t.Fatalf("lstat moved: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the moved entry is not a symlink")
	}
	if got := mustRead(t, root, "target.txt"); got != "hello" {
		t.Fatalf("target content = %q", got)
	}
}

func TestRemoveFileAndSymlink(t *testing.T) {
	root, dir := openRoot(t)
	mustWrite(t, root, "a.txt", "x")
	if err := root.Remove("a.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a.txt still exists: %v", err)
	}
	// 删除符号链接只删链接。
	if runtime.GOOS != "windows" {
		mustWrite(t, root, "target.txt", "keep")
		if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if err := root.Remove("link.txt"); err != nil {
			t.Fatalf("Remove(link): %v", err)
		}
		if got := mustRead(t, root, "target.txt"); got != "keep" {
			t.Fatalf("target content = %q, want it untouched", got)
		}
	}
	if err := root.Remove("missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove(missing) = %v, want os.ErrNotExist", err)
	}
}

func TestMkdirAll(t *testing.T) {
	root, dir := openRoot(t)
	if err := root.MkdirAll("a/b/c", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "a", "b", "c"))
	if err != nil || !info.IsDir() {
		t.Fatalf("dir not created: %v %v", info, err)
	}
	// 已存在是 no-op。
	if err := root.MkdirAll("a/b/c", 0o755); err != nil {
		t.Fatalf("MkdirAll(existing) = %v, want nil", err)
	}
	if err := root.MkdirAll("../out", 0o755); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("MkdirAll(..) = %v, want ErrEscape", err)
	}
}

func TestChmod(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不报告 POSIX 权限位")
	}
	root, dir := openRoot(t)
	mustWrite(t, root, "a.txt", "x")
	if err := root.Chmod("a.txt", 0o600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	if err := root.Chmod("../a.txt", 0o600); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("Chmod(..) = %v, want ErrEscape", err)
	}
}

// ---------------------------------------------------------------------------
// 符号链接目录逃逸
// ---------------------------------------------------------------------------

func TestSymlinkDirectoryEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, dir := openRoot(t)
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	for _, rel := range []string{"escape/secret.txt", "escape", "escape/new.txt"} {
		if _, err := root.ReadFile(rel); err != nil && !errors.Is(err, fsguard.ErrEscape) && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadFile(%q) = %v, want an escape error", rel, err)
		}
	}
	if err := root.WriteFileAtomic("escape/new.txt", []byte("x"), 0o644); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("WriteFileAtomic through a directory symlink = %v, want ErrEscape", err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file was created outside the root")
	}
	// 对照：根内的目录符号链接是允许的。
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "inside")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := root.WriteFileAtomic("inside/ok.txt", []byte("ok"), 0o644); err != nil {
		t.Fatalf("write through an in-root dir symlink: %v", err)
	}
	if got := mustRead(t, root, "real/ok.txt"); got != "ok" {
		t.Fatalf("content = %q", got)
	}
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

func TestConcurrentReadWrite(t *testing.T) {
	root, _ := openRoot(t)
	if err := root.MkdirAll("w", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const workers = 20
	var wait sync.WaitGroup
	errs := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			rel := filepath.ToSlash(filepath.Join("w", "file"+string(rune('a'+index))+".txt"))
			for round := 0; round < 5; round++ {
				if err := root.WriteFileAtomic(rel, []byte("data"), 0o644); err != nil {
					errs <- err
					return
				}
				if _, err := root.ReadFile(rel); err != nil {
					errs <- err
					return
				}
				if _, err := root.Stat("w"); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent operation failed: %v", err)
	}
}

// 20 个 goroutine 写同一个目标：最终内容必须是某一次的完整内容（不撕裂）。
func TestConcurrentWriteSameTargetIsNotTorn(t *testing.T) {
	root, dir := openRoot(t)
	payloads := make([]string, 20)
	for index := range payloads {
		payloads[index] = strings.Repeat(string(rune('A'+index)), 4096)
	}
	var wait sync.WaitGroup
	errs := make(chan error, len(payloads))
	for index := range payloads {
		wait.Add(1)
		go func(payload string) {
			defer wait.Done()
			if err := root.WriteFileAtomic("same.txt", []byte(payload), 0o644); err != nil {
				errs <- err
			}
		}(payloads[index])
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "same.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	matched := false
	for _, payload := range payloads {
		if string(data) == payload {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("final content is torn (%d bytes)", len(data))
	}
	if leftovers := leftoverTemps(t, dir); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

func TestErrorsIsBothDirections(t *testing.T) {
	root, _ := openRoot(t)
	_, err := root.ReadFile("missing.txt")
	if !errors.Is(err, fsguard.ErrNotExist) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want both fsguard.ErrNotExist and os.ErrNotExist", err)
	}
	if errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("not-exist error also matched ErrEscape: %v", err)
	}
	_, escapeErr := root.ReadFile("../x")
	if errors.Is(escapeErr, os.ErrNotExist) {
		t.Fatalf("escape error also matched os.ErrNotExist: %v", escapeErr)
	}
	if !fsguard.IsEscape(escapeErr) {
		t.Fatalf("IsEscape(%v) = false", escapeErr)
	}
	if fsguard.IsEscape(err) {
		t.Fatalf("IsEscape(not-exist) = true")
	}
	if fsguard.IsEscape(nil) {
		t.Fatal("IsEscape(nil) = true")
	}
}

func TestPermissionErrorKeepsIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的权限模型不同")
	}
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时 EACCES 不会发生")
	}
	root, dir := openRoot(t)
	mustWrite(t, root, "locked.txt", "x")
	if err := os.Chmod(filepath.Join(dir, "locked.txt"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err := root.ReadFile("locked.txt")
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err = %v, want os.ErrPermission to stay identifiable", err)
	}
}

// 悬挂符号链接：链接目标不存在时，写入应当在根内**创建目标**（仍然是"写穿链接"）。
func TestWriteFileAtomicThroughDanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, dir := openRoot(t)
	if err := os.Symlink("created.txt", filepath.Join(dir, "dangling.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := root.WriteFileAtomic("dangling.txt", []byte("made"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic(dangling link): %v", err)
	}
	if got := mustRead(t, root, "created.txt"); got != "made" {
		t.Fatalf("target content = %q, want the write to create the target", got)
	}
	info, err := os.Lstat(filepath.Join(dir, "dangling.txt"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the dangling symlink was replaced: %v %v", info, err)
	}
}

// 删除非空目录必须失败（Remove 不是 RemoveAll）。
func TestRemoveNonEmptyDirectoryFails(t *testing.T) {
	root, _ := openRoot(t)
	if err := root.MkdirAll("dir", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustWrite(t, root, "dir/a.txt", "x")
	if err := root.Remove("dir"); err == nil {
		t.Fatal("Remove(non-empty dir) succeeded")
	}
	if got := mustRead(t, root, "dir/a.txt"); got != "x" {
		t.Fatalf("content = %q, want the file untouched", got)
	}
}

// 文件名里含 "path escapes from parent" 时，不能把 NotExist 误判成逃逸。
func TestEscapeMessageInFileNameIsNotEscape(t *testing.T) {
	root, _ := openRoot(t)
	_, err := root.ReadFile("path escapes from parent")
	if fsguard.IsEscape(err) {
		t.Fatalf("err = %v, misclassified as an escape because of the file name", err)
	}
	requireNotExist(t, err)
}

// POSIX 上 "a:b.txt" 是合法文件名（只有 Windows 把它当盘符路径）。
func TestColonInFileNameFollowsPlatform(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 把 a:b.txt 解释成驱动器相对路径，应当拒绝")
	}
	root, _ := openRoot(t)
	mustWrite(t, root, "a:b.txt", "colon")
	if got := mustRead(t, root, "a:b.txt"); got != "colon" {
		t.Fatalf("content = %q", got)
	}
}

// Chmod 不跟随符号链接：改链接只应报错，绝不动目标权限。
func TestChmodRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不报告 POSIX 权限位")
	}
	root, dir := openRoot(t)
	mustWrite(t, root, "target.txt", "x")
	if err := root.Chmod("target.txt", 0o644); err != nil {
		t.Fatalf("Chmod(target): %v", err)
	}
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := root.Chmod("link.txt", 0o600); err == nil {
		t.Fatal("Chmod(link) succeeded; it must refuse to dereference")
	}
	info, err := os.Stat(filepath.Join(dir, "target.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("target mode = %v, want it untouched at 0644", info.Mode().Perm())
	}
}

// ---------------------------------------------------------------------------
// Symlink（创建链接也要经过根）
// ---------------------------------------------------------------------------

func TestSymlinkCreatesRelativeLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, dir := openRoot(t)
	mustWrite(t, root, "target.txt", "hello")
	if err := root.Symlink("link.txt", "target.txt"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if got, err := root.Readlink("link.txt"); err != nil || got != "target.txt" {
		t.Fatalf("Readlink = (%q, %v)", got, err)
	}
	if got := mustRead(t, root, "link.txt"); got != "hello" {
		t.Fatalf("through link = %q", got)
	}
	info, err := os.Lstat(filepath.Join(dir, "link.txt"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("not a symlink: %v %v", info, err)
	}
	// 子目录里的相对目标按**链接所在目录**解析。
	if err := root.MkdirAll("sub", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := root.Symlink("sub/up.txt", "../target.txt"); err != nil {
		t.Fatalf("Symlink(../target): %v", err)
	}
	if got := mustRead(t, root, "sub/up.txt"); got != "hello" {
		t.Fatalf("sub link content = %q", got)
	}
}

func TestSymlinkRejectsAbsoluteTarget(t *testing.T) {
	root, _ := openRoot(t)
	for _, target := range []string{"/etc/passwd", `C:\Windows\win.ini`, `\\server\share\x`} {
		err := root.Symlink("link.txt", target)
		requireEscape(t, err)
		if !strings.Contains(err.Error(), "absolute target") {
			t.Fatalf("error = %v, want it to name the absolute target", err)
		}
	}
}

func TestSymlinkRejectsEscapingTarget(t *testing.T) {
	root, _ := openRoot(t)
	for _, target := range []string{"..", "../outside.txt", "sub/../../outside.txt"} {
		if err := root.Symlink("link.txt", target); !errors.Is(err, fsguard.ErrEscape) {
			t.Fatalf("Symlink(target=%q) = %v, want ErrEscape", target, err)
		}
	}
	if err := root.Symlink("link.txt", ""); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("Symlink(empty target) = %v, want ErrEscape", err)
	}
}

func TestSymlinkDoesNotOverwrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, _ := openRoot(t)
	mustWrite(t, root, "existing.txt", "keep")
	err := root.Symlink("existing.txt", "target.txt")
	if !errors.Is(err, fsguard.ErrExists) {
		t.Fatalf("Symlink onto an existing file = %v, want ErrExists", err)
	}
	if got := mustRead(t, root, "existing.txt"); got != "keep" {
		t.Fatalf("existing file was clobbered: %q", got)
	}
	// 已存在的链接同样不覆盖。
	if err := root.Symlink("link.txt", "existing.txt"); err != nil {
		t.Fatalf("first Symlink: %v", err)
	}
	if err := root.Symlink("link.txt", "existing.txt"); !errors.Is(err, fsguard.ErrExists) {
		t.Fatalf("second Symlink = %v, want ErrExists", err)
	}
	if err := root.Symlink("../escaped.txt", "existing.txt"); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("Symlink at an escaping path = %v, want ErrEscape", err)
	}
}

// 通过创建的链接写入 → 落到目标（与 WriteFileAtomic 的写穿语义一致）。
func TestSymlinkThenWriteThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, _ := openRoot(t)
	mustWrite(t, root, "target.txt", "old")
	if err := root.Symlink("link.txt", "target.txt"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := root.WriteFileAtomic("link.txt", []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic(link): %v", err)
	}
	if got := mustRead(t, root, "target.txt"); got != "new" {
		t.Fatalf("target = %q", got)
	}
}

// ---------------------------------------------------------------------------
// B4：绝对链接目标的文案
// ---------------------------------------------------------------------------

func TestAbsoluteSymlinkTargetMessage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, dir := openRoot(t)
	mustWrite(t, root, "target.txt", "inside")
	absolute := filepath.Join(dir, "target.txt")
	if err := os.Symlink(absolute, filepath.Join(dir, "abs.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// 写入：文案必须说明是"目标是绝对路径"，而不是含糊地说"逃出根"。
	err := root.WriteFileAtomic("abs.txt", []byte("x"), 0o644)
	requireEscape(t, err)
	for _, want := range []string{"has an absolute target", "only relative targets inside the root are supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "points outside the root") {
		t.Fatalf("error = %v, still uses the misleading 'points outside the root' wording", err)
	}

	// 读取：即使绝对目标就在根内，os.Root 也拒绝跟随 → 必须是可识别的逃逸错误。
	if _, err := root.ReadFile("abs.txt"); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("ReadFile(abs link) = %v, want ErrEscape (os.Root refuses absolute link targets)", err)
	}
	if _, err := root.Stat("abs.txt"); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("Stat(abs link) = %v, want ErrEscape", err)
	}
}

// ---------------------------------------------------------------------------
// OpenRegular
// ---------------------------------------------------------------------------

func TestOpenRegularOpensRegularFile(t *testing.T) {
	root, _ := openRoot(t)
	mustWrite(t, root, "a.txt", "payload")
	file, err := root.OpenRegular("a.txt")
	if err != nil {
		t.Fatalf("OpenRegular: %v", err)
	}
	defer file.Close()
	data := make([]byte, 7)
	if _, err := file.Read(data); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("data = %q", data)
	}
}

func TestOpenRegularRejectsDirectoryAndMissing(t *testing.T) {
	root, _ := openRoot(t)
	if err := root.MkdirAll("sub", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := root.OpenRegular("sub"); !errors.Is(err, fsguard.ErrNotRegular) {
		t.Fatalf("OpenRegular(dir) = %v, want ErrNotRegular", err)
	}
	if _, err := root.OpenRegular("."); !errors.Is(err, fsguard.ErrNotRegular) {
		t.Fatalf("OpenRegular(\".\") = %v, want ErrNotRegular", err)
	}
	if _, err := root.OpenRegular("missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenRegular(missing) = %v, want os.ErrNotExist", err)
	}
}

// FIFO 必须被识别成"非普通文件"，而且不能把调用方阻塞住。
func TestOpenRegularRejectsFIFOWithoutBlocking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 mkfifo")
	}
	root, dir := openRoot(t)
	fifo := filepath.Join(dir, "pipe")
	if err := mkfifoForTest(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := root.OpenRegular("pipe")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, fsguard.ErrNotRegular) {
			t.Fatalf("OpenRegular(fifo) = %v, want ErrNotRegular", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OpenRegular(fifo) blocked: opening a FIFO must not hang the caller")
	}
}

// ---------------------------------------------------------------------------
// RenameNoReplace
// ---------------------------------------------------------------------------

func TestRenameNoReplaceMovesWhenTargetMissing(t *testing.T) {
	root, dir := openRoot(t)
	mustWrite(t, root, "a.txt", "content")
	if err := root.RenameNoReplace("a.txt", "b.txt"); err != nil {
		t.Fatalf("RenameNoReplace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists: %v", err)
	}
	if got := mustRead(t, root, "b.txt"); got != "content" {
		t.Fatalf("content = %q", got)
	}
}

func TestRenameNoReplaceRefusesExistingTarget(t *testing.T) {
	root, dir := openRoot(t)
	mustWrite(t, root, "a.txt", "source")
	mustWrite(t, root, "b.txt", "destination")
	err := root.RenameNoReplace("a.txt", "b.txt")
	if !errors.Is(err, fsguard.ErrExists) {
		t.Fatalf("RenameNoReplace onto an existing file = %v, want ErrExists", err)
	}
	if got := mustRead(t, root, "b.txt"); got != "destination" {
		t.Fatalf("destination was clobbered: %q", got)
	}
	if got := mustRead(t, root, "a.txt"); got != "source" {
		t.Fatalf("source disappeared: %q", got)
	}
	// 与 Rename 的对照：Rename 会覆盖。
	if err := root.Rename("a.txt", "b.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := mustRead(t, root, "b.txt"); got != "source" {
		t.Fatalf("Rename did not replace: %q", got)
	}
	_ = dir
}

func TestRenameNoReplaceSymlinkMovesLinkOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, dir := openRoot(t)
	mustWrite(t, root, "target.txt", "keep")
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := root.RenameNoReplace("link.txt", "moved.txt"); err != nil {
		t.Fatalf("RenameNoReplace(link): %v", err)
	}
	info, err := os.Lstat(filepath.Join(dir, "moved.txt"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("moved entry is not a symlink: %v %v", info, err)
	}
	if got := mustRead(t, root, "target.txt"); got != "keep" {
		t.Fatalf("target = %q, want it untouched", got)
	}
}

// 目录无法用硬链接模拟"不覆盖改名" → ErrUnsupported（跨设备走同一条映射）。
func TestRenameNoReplaceDirectoryIsUnsupported(t *testing.T) {
	root, dir := openRoot(t)
	if err := root.MkdirAll("dir/sub", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	err := root.RenameNoReplace("dir", "dir2")
	if !errors.Is(err, fsguard.ErrUnsupported) {
		t.Fatalf("RenameNoReplace(dir) = %v, want ErrUnsupported", err)
	}
	if info, statErr := os.Stat(filepath.Join(dir, "dir")); statErr != nil || !info.IsDir() {
		t.Fatalf("source dir was disturbed: %v %v", info, statErr)
	}
}

func TestRenameNoReplaceEscapeRejected(t *testing.T) {
	root, _ := openRoot(t)
	mustWrite(t, root, "a.txt", "x")
	if err := root.RenameNoReplace("a.txt", "../escaped.txt"); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("RenameNoReplace(..) = %v, want ErrEscape", err)
	}
	if err := root.RenameNoReplace("a.txt", "a.txt"); err != nil {
		t.Fatalf("RenameNoReplace(same path) = %v, want nil no-op", err)
	}
}

// SymlinkLiteral 按原样写回链接文本（还原快照用），但位置仍受根约束，
// 且这样造出的链接无法被 fsguard 跟随（读取必须报 ErrEscape）。
func TestSymlinkLiteralKeepsRawTargetText(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	root, dir := openRoot(t)
	outside := writeOutside(t, "SECRET")
	for index, target := range []string{outside, "../../etc/passwd"} {
		rel := "link" + string(rune('a'+index))
		if err := root.SymlinkLiteral(rel, target); err != nil {
			t.Fatalf("SymlinkLiteral(%q): %v", target, err)
		}
		got, err := root.Readlink(rel)
		if err != nil || got != target {
			t.Fatalf("Readlink = (%q, %v), want the raw text %q", got, err, target)
		}
		if _, statErr := root.Stat(rel); !errors.Is(statErr, fsguard.ErrEscape) {
			t.Fatalf("Stat(link to %q) = %v, want ErrEscape", target, statErr)
		}
	}
	// 位置逃逸仍然被拒。
	if err := root.SymlinkLiteral("../escaped.txt", "target.txt"); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("SymlinkLiteral at an escaping path = %v, want ErrEscape", err)
	}
	// 与 Symlink 的对照：Symlink 必须拒绝同一个绝对目标。
	if err := root.Symlink("strict.txt", outside); !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("Symlink(absolute target) = %v, want ErrEscape", err)
	}
	_ = dir
}

// 策略守卫：进程内写入必须与 OS 沙箱用同一份可写面。
func TestWriteGuardBlocksInProcessWrites(t *testing.T) {
	dir := t.TempDir()
	root, err := fsguard.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer root.Close()
	if err := root.MkdirAll("allowed", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := root.WriteFileAtomic("allowed/a.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("prep write: %v", err)
	}

	// 模拟 sandbox.Policy.AllowsWrite：只允许 allowed/ 之下。
	allowedDir := filepath.Join(dir, "allowed") + string(filepath.Separator)
	guard := func(abs string) error {
		if strings.HasPrefix(abs, allowedDir) {
			return nil
		}
		return errors.New("denied by policy")
	}
	root.SetWriteGuard(guard)

	// 允许的写入照常成功。
	if err := root.WriteFileAtomic("allowed/b.txt", []byte("ok"), 0o644); err != nil {
		t.Fatalf("allowed write was denied: %v", err)
	}
	if err := root.Chmod("allowed/b.txt", 0o600); err != nil {
		t.Fatalf("allowed chmod was denied: %v", err)
	}
	if err := root.Rename("allowed/a.txt", "allowed/c.txt"); err != nil {
		t.Fatalf("allowed rename was denied: %v", err)
	}
	if err := root.Remove("allowed/c.txt"); err != nil {
		t.Fatalf("allowed remove was denied: %v", err)
	}

	// 越界的写入必须被拒（ErrDenied），且不产生副作用。
	// 目的地越界的操作必须被拒（ErrDenied），且不产生副作用。
	for _, op := range []struct {
		name string
		fn   func() error
	}{
		{"WriteFileAtomic", func() error { return root.WriteFileAtomic("outside.txt", []byte("x"), 0o644) }},
		{"MkdirAll", func() error { return root.MkdirAll("outside-dir", 0o755) }},
		{"Rename-dest", func() error { return root.Rename("allowed/b.txt", "moved.txt") }},
	} {
		err := op.fn()
		if err == nil {
			t.Errorf("%s 被守卫放行", op.name)
			continue
		}
		if !errors.Is(err, fsguard.ErrDenied) {
			t.Errorf("%s err = %v, want fsguard.ErrDenied", op.name, err)
		}
	}
	// 源在 allowed/ 之内的删除是允许的。
	if err := root.Remove("allowed/b.txt"); err != nil {
		t.Fatalf("allowed remove must succeed: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "outside.txt")); !os.IsNotExist(statErr) {
		t.Fatal("被拒的写入产生了文件")
	}
	// 读不受守卫影响。
	if _, err := root.ReadFile("allowed/b.txt"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("守卫不应影响读取: %v", err)
	}
}
