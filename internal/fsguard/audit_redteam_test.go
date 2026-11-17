package fsguard_test

// 对抗审计（只读）：正面攻击 fsguard 的逃逸判定、原子写语义、错误归一化与并发。
//
// 只加测试，不改实现。断言分两类：
//   - 安全断言（必须成立）：逃逸一律 ErrEscape、绝不被当成"不存在"、失败不留临时文件；
//   - 现状断言（记录已知限制）：FIFO 会阻塞、perm=0 就是 0、绝对链接目标连读都不行。

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/fsguard"
)

// escapeTargets 是"必须被拒绝"的路径形态（不含符号链接，链接单独测）。
func escapeTargets() []string {
	targets := []string{
		"..",
		"../x",
		"../../etc/passwd",
		"a/../../x",
		"a/b/../../../x",
		"./../x",
		"/etc/passwd",
		"//etc/passwd",
		`\\server\share\x`,
		`\\?\C:\x`,
		`C:\x`,
		"C:/x",
		`c:\windows\system32`,
	}
	if runtime.GOOS == "windows" {
		targets = append(targets, `C:x`, `..\..\x`, `\x`)
	}
	return targets
}

// TestAuditHostilePathMatrix 把逃逸形态打到每一个入口上：都必须返回 ErrEscape，
// 而不是"被读/写成功"或"被当成不存在"。
func TestAuditHostilePathMatrix(t *testing.T) {
	root, _ := openRoot(t)
	mustWrite(t, root, "ok.txt", "ok")

	operations := map[string]func(string) error{
		"read":       func(p string) error { _, err := root.ReadFile(p); return err },
		"readlimit":  func(p string) error { _, err := root.ReadFileLimit(p, 1024); return err },
		"stat":       func(p string) error { _, err := root.Stat(p); return err },
		"lstat":      func(p string) error { _, err := root.Lstat(p); return err },
		"readlink":   func(p string) error { _, err := root.Readlink(p); return err },
		"write":      func(p string) error { return root.WriteFileAtomic(p, []byte("x"), 0o644) },
		"remove":     func(p string) error { return root.Remove(p) },
		"mkdir":      func(p string) error { return root.MkdirAll(p, 0o755) },
		"chmod":      func(p string) error { return root.Chmod(p, 0o644) },
		"rename-src": func(p string) error { return root.Rename(p, "safe.txt") },
		"rename-dst": func(p string) error { return root.Rename("ok.txt", p) },
	}
	for _, target := range escapeTargets() {
		for name, operation := range operations {
			err := operation(target)
			if err == nil {
				t.Errorf("%s(%q) 成功了：逃逸路径必须被拒绝", name, target)
				continue
			}
			if !errors.Is(err, fsguard.ErrEscape) {
				t.Errorf("%s(%q) err = %v，期望 errors.Is(ErrEscape)", name, target, err)
			}
		}
	}
}

// TestAuditEscapeIsNeverNotExist 逃逸不能被归一化成"不存在"（否则调用方会走
// "那就创建它"的分支，等于把逃逸变成了写入）。
func TestAuditEscapeIsNeverNotExist(t *testing.T) {
	root, _ := openRoot(t)
	for _, target := range escapeTargets() {
		for name, operation := range map[string]func(string) error{
			"read":   func(p string) error { _, err := root.ReadFile(p); return err },
			"stat":   func(p string) error { _, err := root.Stat(p); return err },
			"lstat":  func(p string) error { _, err := root.Lstat(p); return err },
			"write":  func(p string) error { return root.WriteFileAtomic(p, []byte("x"), 0o644) },
			"remove": func(p string) error { return root.Remove(p) },
		} {
			err := operation(target)
			if err == nil {
				t.Fatalf("%s(%q) 不该成功", name, target)
			}
			if errors.Is(err, fsguard.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s(%q) 被当成不存在: %v", name, target, err)
			}
		}
	}
}

// TestAuditSymlinkEscapeMatrix 覆盖符号链接的各种逃逸形态，并要求"读"也拒绝
// （否则读操作就是一条把根外内容带进上下文的通道）。
func TestAuditSymlinkEscapeMatrix(t *testing.T) {
	root, dir := openRoot(t)
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("SECRET"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	mustWrite(t, root, "inside.txt", "inside")

	links := map[string]string{
		"abs-outside":  outsideFile,                      // 绝对路径指向根外
		"rel-outside":  "../outside-link-target",         // 相对路径逃出根
		"dir-outside":  outsideDir,                       // 目录链接指向根外
		"abs-inside":   filepath.Join(dir, "inside.txt"), // 绝对路径指回根内（os.Root 也拒绝）
		"dangling-out": "../not-there.txt",               // 悬空 + 逃逸
		"loop-a":       "loop-b",                         // 环
		"loop-b":       "loop-a",
		"chain-out-1":  "chain-out-2", // 链式逃逸
		"chain-out-2":  "../outside-link-target",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Skipf("符号链接不可用: %v", err)
		}
	}

	// 读/写/stat 都必须报错，且都不能被当成"不存在"。
	for name := range links {
		if _, err := root.ReadFile(name); err == nil {
			t.Errorf("ReadFile(%q) 成功：经符号链接逃逸", name)
		} else if errors.Is(err, fsguard.ErrNotExist) {
			t.Errorf("ReadFile(%q) 被当成不存在: %v", name, err)
		}
		if _, err := root.Stat(name); err == nil {
			t.Errorf("Stat(%q) 成功：经符号链接逃逸", name)
		}
		if _, err := root.ReadFileLimit(name, 1024); err == nil {
			t.Errorf("ReadFileLimit(%q) 成功：经符号链接逃逸", name)
		}
	}

	// 指回根内的绝对链接目标：写必须拒绝（fsguard 的显式语义）。
	requireEscape(t, root.WriteFileAtomic("abs-inside", []byte("x"), 0o644))
	// 目录链接指向根外：经它写子路径必须拒绝。
	requireEscape(t, root.WriteFileAtomic("dir-outside/evil.txt", []byte("x"), 0o644))
	requireEscape(t, root.Remove("dir-outside/secret.txt"))

	// 越界写不能真的落地：核对宿主上的根外目录。
	if _, err := os.Stat(filepath.Join(outsideDir, "evil.txt")); err == nil {
		t.Fatal("根外文件被创建了：逃逸写入成功")
	}
	if data, err := os.ReadFile(outsideFile); err != nil || string(data) != "SECRET" {
		t.Fatalf("根外文件被改写: %q err=%v", data, err)
	}
}

// TestAuditEscapeMessageAsDirectoryName：把 stdlib 的逃逸文案当成**目录名**与
// **路径中间段**时，不能误判成逃逸（那会让合法的 NotFound 变成"安全事件"）。
func TestAuditEscapeMessageAsDirectoryName(t *testing.T) {
	root, _ := openRoot(t)
	const weird = "path escapes from parent"
	if err := root.MkdirAll(weird, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", weird, err)
	}
	mustWrite(t, root, weird+"/inner.txt", "inner")
	if got := mustRead(t, root, weird+"/inner.txt"); got != "inner" {
		t.Fatalf("内容 = %q", got)
	}
	// 不存在的合法路径：必须是 NotExist，不能因为目录名像错误文案就变成 ErrEscape。
	_, err := root.ReadFile(weird + "/missing.txt")
	requireNotExist(t, err)
	if errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("合法路径 %q 被误判成逃逸: %v", weird+"/missing.txt", err)
	}
}

// TestAuditWriteAtomicPermZero 记录 perm=0 的语义：文件被创建成 0000，而不是
// "用默认权限"（调用方传 0 就等于不可读文件）。
func TestAuditWriteAtomicPermZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的权限位语义不同")
	}
	root, dir := openRoot(t)
	if err := root.WriteFileAtomic("zero.txt", []byte("x"), 0); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "zero.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0 {
		t.Fatalf("perm=0 的文件实际权限 = %04o，期望 0000（记录现状）", mode)
	}
	// 覆盖已有文件时不会"保留原权限"：perm 是调用方给的，这是 fsguard 的契约。
	if err := os.Chmod(filepath.Join(dir, "zero.txt"), 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := root.WriteFileAtomic("zero.txt", []byte("y"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err = os.Stat(filepath.Join(dir, "zero.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Fatalf("覆盖写后权限 = %04o，期望 0644（perm 由调用方决定，不是保留原值）", mode)
	}
}

// TestAuditWriteAtomicNoResidueOnFailure 覆盖各类失败路径：都不能留下 .fsguard-* 临时文件。
func TestAuditWriteAtomicNoResidueOnFailure(t *testing.T) {
	root, dir := openRoot(t)
	if err := os.MkdirAll(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("../outside", filepath.Join(dir, "escape-link")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	failures := map[string]func() error{
		"父目录不存在": func() error { return root.WriteFileAtomic("no/such/dir/x.txt", []byte("x"), 0o644) },
		"目标是目录":  func() error { return root.WriteFileAtomic("adir", []byte("x"), 0o644) },
		"链接逃逸":   func() error { return root.WriteFileAtomic("escape-link", []byte("x"), 0o644) },
		"根目标":    func() error { return root.WriteFileAtomic(".", []byte("x"), 0o644) },
		"绝对路径":   func() error { return root.WriteFileAtomic("/tmp/x", []byte("x"), 0o644) },
	}
	for name, failure := range failures {
		if err := failure(); err == nil {
			t.Fatalf("%s: 期望失败，实际成功", name)
		}
		if temps := leftoverTemps(t, dir); len(temps) != 0 {
			t.Fatalf("%s: 留下临时文件 %v", name, temps)
		}
	}
}

// TestAuditWriteAtomicConcurrentSameTarget：20 个并发写同一目标，最终内容必须是
// 某一次写入的**完整内容**（不能撕裂、不能串字节），且不留临时文件。
func TestAuditWriteAtomicConcurrentSameTarget(t *testing.T) {
	root, dir := openRoot(t)
	const writers = 20
	payloads := make([][]byte, writers)
	for i := range payloads {
		payloads[i] = []byte(strings.Repeat(fmt.Sprintf("w%02d", i), 4096))
	}
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- root.WriteFileAtomic("target.txt", payloads[i], 0o644)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发写失败: %v", err)
		}
	}
	final, err := os.ReadFile(filepath.Join(dir, "target.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	matched := false
	for _, payload := range payloads {
		if string(final) == string(payload) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("最终内容不是任何一次写入的完整内容（len=%d）", len(final))
	}
	if temps := leftoverTemps(t, dir); len(temps) != 0 {
		t.Fatalf("并发写留下临时文件: %v", temps)
	}
}

// TestAuditCloseDuringOperations：Close 与操作并发时不能 panic（窗口里应当报错）。
func TestAuditCloseDuringOperations(t *testing.T) {
	dir := t.TempDir()
	root, err := fsguard.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := root.WriteFileAtomic("seed.txt", []byte("seed"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var wg sync.WaitGroup
	panics := make(chan any, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					panics <- recovered
				}
			}()
			for j := 0; j < 50; j++ {
				_, _ = root.ReadFile("seed.txt")
				_ = root.WriteFileAtomic("seed.txt", []byte("x"), 0o644)
				_, _ = root.Stat("seed.txt")
			}
		}(i)
	}
	time.Sleep(2 * time.Millisecond)
	_ = root.Close()
	wg.Wait()
	close(panics)
	for recovered := range panics {
		t.Fatalf("Close 与并发操作之间发生 panic: %v", recovered)
	}
}

// TestAuditReadFileLimitOnFIFOBlocks 记录已知限制（作者在 doc.go 里承认）：
// FIFO 会让 OpenFile 一直阻塞，ReadFileLimit 的 Stat 短路救不了它。
//
// 影响面：apply_patch 更新一个 FIFO 路径会**永久挂住**（工具没有超时），因此
// 调用方必须自己先 Stat 拒掉非普通文件。
func TestAuditReadFileLimitOnFIFOBlocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 FIFO")
	}
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("没有 mkfifo")
	}
	root, dir := openRoot(t)
	fifo := filepath.Join(dir, "pipe")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo: %v %s", err, out)
	}

	done := make(chan error, 1)
	go func() {
		_, err := root.ReadFileLimit("pipe", 1024)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("FIFO 读取没有阻塞（err=%v），与 doc.go 的描述不一致", err)
	case <-time.After(300 * time.Millisecond):
		// 期望：仍然阻塞。用写端把它解开，避免把测试挂死。
	}
	writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open fifo for write: %v", err)
	}
	_, _ = writer.Write([]byte("x"))
	_ = writer.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("FIFO 在写端出现后仍未返回")
	}
}

// TestAuditRenameMkdirChmodEscapes 覆盖 rename/mkdir/chmod 的逃逸面。
func TestAuditRenameMkdirChmodEscapes(t *testing.T) {
	root, _ := openRoot(t)
	mustWrite(t, root, "a.txt", "a")
	for _, bad := range []string{"../x", "/tmp/x", ".."} {
		if err := root.Rename("a.txt", bad); err == nil {
			t.Errorf("Rename(a.txt, %q) 成功", bad)
		} else if !errors.Is(err, fsguard.ErrEscape) {
			t.Errorf("Rename 到 %q: err = %v，期望 ErrEscape", bad, err)
		}
		if err := root.Rename(bad, "b.txt"); err == nil {
			t.Errorf("Rename(%q, b.txt) 成功", bad)
		}
		if err := root.MkdirAll(bad, 0o755); err == nil {
			t.Errorf("MkdirAll(%q) 成功", bad)
		}
		if err := root.Chmod(bad, 0o644); err == nil {
			t.Errorf("Chmod(%q) 成功", bad)
		}
	}
	// MkdirAll(".") 是 no-op；Chmod(".") 必须拒绝（根本身不能被改）。
	if err := root.MkdirAll(".", 0o755); err != nil {
		t.Errorf("MkdirAll(\".\"): %v", err)
	}
	requireEscape(t, root.Chmod(".", 0o644))
}

// TestAuditWindowsReservedDeviceNames 在 Windows 上锁定保留设备名（NUL/CON/COM1…）
// 的行为：这些名字在 Windows 上会被当作设备打开，写进去的数据会静默消失。
//
// 在 Linux 上它们只是普通文件名（这里断言"可以当普通文件用"，记录平台差异）。
func TestAuditWindowsReservedDeviceNames(t *testing.T) {
	root, dir := openRoot(t)
	reserved := []string{"NUL", "CON", "PRN", "AUX", "COM1", "LPT1", "nul.txt", "con.txt"}
	if runtime.GOOS == "windows" {
		for _, name := range reserved {
			err := root.WriteFileAtomic(name, []byte("data"), 0o644)
			if err == nil {
				// 若"成功"，必须真的落在普通文件上，而不是被系统吞进设备。
				if _, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil {
					t.Errorf("%q 写入报告成功但磁盘上没有文件（被当成设备了）", name)
				}
			}
		}
		return
	}
	for _, name := range reserved {
		if err := root.WriteFileAtomic(name, []byte("data"), 0o644); err != nil {
			t.Errorf("POSIX 上 %q 应当是普通文件名，却被拒绝: %v", name, err)
			continue
		}
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != "data" {
			t.Errorf("POSIX 上 %q 写入后读不回: %q err=%v", name, data, err)
		}
	}
}
