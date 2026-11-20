package sandbox_test

// 私有 scratch/TMPDIR 可写面（A4）。
//
// read-only 档以前只有 /dev/null 可写，于是任何需要临时文件的命令（`go build`、
// `mktemp`、编辑器写 swap）都会失败，而唯一能用的目录是与其他进程共享的 /tmp。
// WithScratchDir 给这一档一个**私有**可写目录，并把 TMPDIR/TMP/TEMP 指过去。

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestScratchDirIsCreated0700 钉住构造期副作用：目录不存在就建出来（0700），
// 并且 ScratchDir 返回绝对路径。
func TestScratchDirIsCreated0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "scratch")
	policy := sandbox.New(sandbox.WithScratchDir(dir))

	if got := policy.ScratchDir(); got != filepath.Clean(dir) {
		t.Fatalf("ScratchDir() = %q, want %q", got, filepath.Clean(dir))
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("scratch 目录没有被创建: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("scratch 不是目录: %v", info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("scratch 权限 = %o, want 700", perm)
	}
	if err := policy.ScratchDirError(); err != nil {
		t.Fatalf("ScratchDirError() = %v, want nil", err)
	}
}

// TestScratchDirRelativeResolvedAgainstGetwd 钉住相对路径语义：按 os.Getwd()
// 解析成绝对路径（文档里写清）。
func TestScratchDirRelativeResolvedAgainstGetwd(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)

	policy := sandbox.New(sandbox.WithScratchDir(filepath.Join("tmp", "scratch")))
	want := filepath.Join(base, "tmp", "scratch")
	if got := policy.ScratchDir(); got != want {
		t.Fatalf("ScratchDir() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(policy.ScratchDir()) {
		t.Fatalf("ScratchDir() = %q, want an absolute path", policy.ScratchDir())
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("相对 scratch 没有被创建: %v", err)
	}
}

// TestScratchDirFailureIsVisible 钉住"建不出来不静默"：错误必须能从策略上取回。
func TestScratchDirFailureIsVisible(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	policy := sandbox.New(sandbox.WithScratchDir(filepath.Join(blocker, "scratch")))
	if err := policy.ScratchDirError(); err == nil {
		t.Fatal("ScratchDirError() = nil，MkdirAll 失败必须对宿主可见")
	}
}

// TestCommandEnvScratchOverridesTempKeys 钉住环境注入：三个临时目录键都指向
// scratch，其它键原样保留，且三个键各只出现一次。
func TestCommandEnvScratchOverridesTempKeys(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(sandbox.WithScratchDir(scratch))
	base := []string{
		"PATH=/bin",
		"TMPDIR=/host/tmp",
		"TMP=/host/tmp",
		"TEMP=/host/tmp",
		"HOME=/home/a",
	}
	env := policy.CommandEnv(base)
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if got := envValue(t, env, key); got != scratch {
			t.Fatalf("%s = %q, want %q（env=%v）", key, got, scratch, env)
		}
		if n := countEnvKey(env, key); n != 1 {
			t.Fatalf("%s 出现了 %d 次, want 1（env=%v）", key, n, env)
		}
	}
	if got := envValue(t, env, "PATH"); got != "/bin" {
		t.Fatalf("PATH = %q, want /bin（其它键必须保留）", got)
	}
	if got := envValue(t, env, "HOME"); got != "/home/a" {
		t.Fatalf("HOME = %q, want /home/a", got)
	}
}

// TestCommandEnvScratchAddsMissingKeys 钉住 base 里原本没有临时键时也要补上。
func TestCommandEnvScratchAddsMissingKeys(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(sandbox.WithScratchDir(scratch))
	env := policy.CommandEnv([]string{"PATH=/bin"})
	if len(env) != 4 {
		t.Fatalf("CommandEnv = %v, want PATH + 三个临时键", env)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if got := envValue(t, env, key); got != scratch {
			t.Fatalf("%s = %q, want %q", key, got, scratch)
		}
	}
}

// TestCommandEnvWithoutScratchIsByteIdentical 钉住"未设置 scratch 时逐字节不变"。
func TestCommandEnvWithoutScratchIsByteIdentical(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()))
	base := []string{"PATH=/bin", "TMPDIR=/tmp", "TMP=/tmp", "TMP=/dup", "TEMP=/tmp"}
	if env := policy.CommandEnv(base); !slices.Equal(env, base) {
		t.Fatalf("CommandEnv = %v, want 与入参逐字节一致 %v", env, base)
	}
}

// TestWritableRootsForReadOnlyIncludesScratch 钉住可写面模型：read-only 下
// 可写面是 {/dev/null, scratch}，且**不含**工作区。
func TestWritableRootsForReadOnlyIncludesScratch(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly), sandbox.WithScratchDir(scratch))

	got := policy.WritableRootsFor(sandbox.ModeReadOnly)
	if !slices.Contains(got, "/dev/null") {
		t.Fatalf("WritableRootsFor(read-only) = %v, want /dev/null", got)
	}
	if !slices.Contains(got, scratch) {
		t.Fatalf("WritableRootsFor(read-only) = %v, want %q", got, scratch)
	}
	for _, path := range got {
		if path == root || path == filepath.Clean(root) {
			t.Fatalf("WritableRootsFor(read-only) = %v, must not grant the workspace", got)
		}
	}
	if got := policy.WritableRootsFor(sandbox.ModeDangerFullAccess); got != nil {
		t.Fatalf("WritableRootsFor(danger-full-access) = %v, want nil（不包装）", got)
	}
}

// TestWritableRootsForWorkspaceWriteIncludesScratch 钉住 workspace-write 下
// scratch 也进入可写面，且与工作区根并列。
func TestWritableRootsForWorkspaceWriteIncludesScratch(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithScratchDir(scratch))

	got := policy.WritableRootsFor(sandbox.ModeWorkspaceWrite)
	if !slices.Contains(got, scratch) {
		t.Fatalf("WritableRootsFor(workspace-write) = %v, want %q", got, scratch)
	}
	if !slices.Contains(got, filepath.Clean(root)) {
		t.Fatalf("WritableRootsFor(workspace-write) = %v, want the workspace root %q", got, root)
	}
}

// TestSlashTmpSwitchIsLive 钉住死旋钮收口：WithSlashTmp(false) 必须真的把
// /tmp 从 workspace-write 的可写面里去掉（不再是只影响状态展示的空开关）。
// Linux 上 /tmp 通常就是 os.TempDir()，因此两个开关一起关（见选项文档的粒度说明）。
func TestSlashTmpSwitchIsLive(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("字面 /tmp 只存在于 POSIX")
	}
	root := t.TempDir()
	loose := sandbox.New(sandbox.WithRoots(root))
	if got := loose.WritableRootsFor(sandbox.ModeWorkspaceWrite); !slices.Contains(got, "/tmp") {
		t.Fatalf("WritableRootsFor(workspace-write) = %v, want /tmp（默认放行）", got)
	}
	strict := sandbox.New(sandbox.WithRoots(root), sandbox.WithSlashTmp(false), sandbox.WithTmpdirEnv(false))
	for _, path := range strict.WritableRootsFor(sandbox.ModeWorkspaceWrite) {
		if filepath.Clean(path) == "/tmp" || filepath.Clean(path) == filepath.Clean(os.TempDir()) {
			t.Fatalf("WritableRootsFor = %v, 两个开关关闭之后仍有临时目录", strict.WritableRootsFor(sandbox.ModeWorkspaceWrite))
		}
	}
}

// envValue 取环境切片里某个键的值（找不到时 t.Fatal）。
func envValue(t *testing.T, env []string, key string) string {
	t.Helper()
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == key {
			return value
		}
	}
	t.Fatalf("环境里没有 %s（env=%v）", key, env)
	return ""
}

// countEnvKey 统计某个键在环境切片里出现的次数。
func countEnvKey(env []string, key string) int {
	count := 0
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok && name == key {
			count++
		}
	}
	return count
}

// TestPreflightMentionsScratch 钉住 Preflight 的可写面描述带上 scratch：
// read-only + scratch 时说"私有临时目录 <path>"，没有 scratch 时不谎称系统临时
// 目录可写。
func TestPreflightMentionsScratch(t *testing.T) {
	runner := &fakeReporter{name: "bwrap", enforcement: sandbox.EnforcementFull, coverage: "full"}
	scratch := filepath.Join(t.TempDir(), "scratch")
	withScratch := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithScratchDir(scratch),
	)
	if text := sandbox.Preflight(withScratch, runner, true).Text(); !strings.Contains(text, scratch) {
		t.Fatalf("Preflight 文本里没有 scratch 路径 %q：\n%s", scratch, text)
	}
	withoutScratch := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeReadOnly))
	if text := sandbox.Preflight(withoutScratch, runner, true).Text(); strings.Contains(text, "以及系统临时目录") {
		t.Fatalf("read-only 无 scratch 时不应声称系统临时目录可写：\n%s", text)
	}
}
