//go:build linux

package osrunner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// linuxTestPolicy 构造一个指定模式的 Linux 测试策略，根为 roots。
// scratchParentIsTempRoot 报告候选父目录是否落在策略可写的临时根里。
func scratchParentIsTempRoot(parent string) bool {
	parent = filepath.Clean(parent)
	for _, root := range platformTempRoots() {
		root = filepath.Clean(root)
		if parent == root || strings.HasPrefix(parent, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func linuxTestPolicy(t *testing.T, mode sandbox.Mode, roots ...string) *sandbox.Policy {
	t.Helper()
	return sandbox.New(sandbox.WithRoots(roots...), sandbox.WithMode(mode))
}

// linuxHasArg 报告 args 是否包含 want 这个完整元素。
func linuxHasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// linuxFlagValues 收集 args 中所有 "--flag value" 形式的 value。
func linuxFlagValues(args []string, flag string) []string {
	var values []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			values = append(values, args[i+1])
			i++
		}
	}
	return values
}

// linuxCanonical 返回路径的真实绝对形式，与 profiles.go 的规范化保持一致。
func linuxCanonical(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs(%q): %v", path, err)
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(real)
	}
	return abs
}

// linuxShellQuote 把一个路径转成 /bin/sh 安全的单引号字面量。
func linuxShellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// runConfinedCommand 执行一次 Confine 的结果，返回合并输出与退出码。
func runConfinedCommand(t *testing.T, confined sandbox.ConfinedCommand) (string, int) {
	t.Helper()
	if len(confined.Argv) == 0 {
		t.Fatalf("ConfinedCommand.Argv is empty")
	}
	cmd := exec.Command(confined.Argv[0], confined.Argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run %v: %v", confined.Argv, err)
	}
	return string(out), exitErr.ExitCode()
}

// requireLandlockABI 返回内核协商的 Landlock ABI；不支持时跳过运行期强制测试。
func requireLandlockABI(t *testing.T) int {
	t.Helper()
	abi, ok := probeLandlock(5 * time.Second)
	if !ok || abi < 1 {
		t.Skip("主机内核不支持 Landlock，跳过运行期强制测试")
	}
	return abi
}

// linuxScratchDir 在已授权临时目录之外创建一个可写目录，用于证明“外部写入被拒”。
//
// platformTempRoots 会把 os.TempDir() 与 /tmp 授权为可写，因此 /var/tmp（或包目录）
// 才能代表“未授权但当前用户可写”的位置。两处都不可写时跳过运行期测试。
// linuxScratchDir 返回一个**不在平台临时目录里**的宿主目录，用于"根外写入必须
// 被拒"这类断言。/var/tmp 与 /tmp 都是策略的可写临时根（见 platformTempRoots），
// 用它们当"outside"会让断言空转，所以候选只取用户目录与工作目录，并显式排除
// 任何落在临时根之下的候选。
func linuxScratchDir(t *testing.T) string {
	t.Helper()
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, home)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	for _, parent := range candidates {
		if scratchParentIsTempRoot(parent) {
			continue
		}
		dir, err := os.MkdirTemp(parent, ".rig-landlock-outside-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	t.Skip("没有位于授权临时目录之外的可写暂存目录，跳过运行期强制测试")
	return ""
}

// TestHandledAccessMask 校验按协商 ABI 生成的 handled_access_fs 位掩码。
func TestHandledAccessMask(t *testing.T) {
	const abi1 = uint64(1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7 | 1<<8 | 1<<9 | 1<<10 | 1<<11 | 1<<12)
	cases := []struct {
		name string
		abi  int
		want uint64
	}{
		{"unsupported-zero", 0, 0},
		{"unsupported-negative", -1, 0},
		{"abi1", 1, abi1},
		{"abi2-adds-refer", 2, abi1 | 1<<13},
		{"abi3-adds-truncate", 3, abi1 | 1<<13 | 1<<14},
		{"abi4-no-new-fs-rights", 4, abi1 | 1<<13 | 1<<14},
		{"abi5-adds-ioctl-dev", 5, abi1 | 1<<13 | 1<<14 | 1<<15},
		{"abi6-superset", 6, abi1 | 1<<13 | 1<<14 | 1<<15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := handledAccessMask(tc.abi); got != tc.want {
				t.Fatalf("handledAccessMask(%d) = %#x, want %#x", tc.abi, got, tc.want)
			}
		})
	}
}

// TestLandlockEnforcement 校验覆盖程度：无论协商到哪个 ABI 都只能是 partial。
//
// 旧契约在 ABI >= 3 时报 full，理由是 REFER 与 TRUNCATE 都受治理。但它高估了
// 承诺：Landlock 的 handled_access_fs 里没有元数据变更位，chmod/chown/utimensat
// 完全不受治理（实测 read-only 域下仍能 chmod 任意文件），所以整体只能是 partial。
func TestLandlockEnforcement(t *testing.T) {
	cases := []struct {
		abi  int
		want sandbox.Enforcement
	}{
		{-1, sandbox.EnforcementPartial},
		{0, sandbox.EnforcementPartial},
		{1, sandbox.EnforcementPartial},
		{2, sandbox.EnforcementPartial},
		{3, sandbox.EnforcementPartial},
		{4, sandbox.EnforcementPartial},
		{5, sandbox.EnforcementPartial},
	}
	for _, tc := range cases {
		if got := landlockEnforcement(tc.abi); got != tc.want {
			t.Fatalf("landlockEnforcement(%d) = %q, want %q", tc.abi, got, tc.want)
		}
	}
}

// TestLandlockRunnerConfineArgv 校验 Landlock 启动器的 argv 形状。
func TestLandlockRunnerConfineArgv(t *testing.T) {
	root := t.TempDir()
	runner := &landlockRunner{exe: "/opt/rig/host", abi: 4}
	confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi"}, linuxTestPolicy(t, sandbox.ModeWorkspaceWrite, root))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := confined.Argv[0], "/opt/rig/host"; got != want {
		t.Fatalf("Argv[0] = %q, want %q", got, want)
	}
	if got, want := confined.Argv[1], HelperArgLandlock; got != want {
		t.Fatalf("Argv[1] = %q, want %q", got, want)
	}
	if !linuxHasArg(linuxFlagValues(confined.Argv, "--ro"), "/") {
		t.Fatalf("Argv = %v, want --ro /", confined.Argv)
	}
	readWrite := linuxFlagValues(confined.Argv, "--rw")
	if !linuxHasArg(readWrite, "/dev/null") {
		t.Fatalf("Argv = %v, want --rw /dev/null", confined.Argv)
	}
	if !linuxHasArg(readWrite, linuxCanonical(t, root)) {
		t.Fatalf("Argv = %v, want --rw %q", confined.Argv, root)
	}
	sep := -1
	for i, arg := range confined.Argv {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+3 >= len(confined.Argv) {
		t.Fatalf("Argv = %v, want ... -- /bin/sh -c echo hi", confined.Argv)
	}
	if got, want := strings.Join(confined.Argv[sep+1:], " "), "/bin/sh -c echo hi"; got != want {
		t.Fatalf("trailing argv = %q, want %q", got, want)
	}
	if confined.Enforcement != sandbox.EnforcementPartial {
		t.Fatalf("Enforcement = %q, want partial", confined.Enforcement)
	}
	if got, want := strings.Join(confined.DenialSignatures, "|"), strings.Join(DenialSignatures(RunnerLandlock), "|"); got != want {
		t.Fatalf("DenialSignatures = %v, want %v", confined.DenialSignatures, DenialSignatures(RunnerLandlock))
	}
}

// TestLandlockRunnerReadOnlyGrants 校验只读模式的写授权只剩设备节点。
//
// 旧契约断言“恰好只有 /dev/null”；新契约把 /dev/ptmx、/dev/pts、/dev/tty、
// /dev/shm（存在才加）也纳入——没有它们，交互式命令与共享内存会莫名失败。仍然
// 不授予 workspace、/tmp 或 $TMPDIR。
func TestLandlockRunnerReadOnlyGrants(t *testing.T) {
	root := t.TempDir()
	runner := &landlockRunner{exe: "/opt/rig/host", abi: 3}
	confined, err := runner.Confine([]string{"/bin/true"}, linuxTestPolicy(t, sandbox.ModeReadOnly, root))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	readWrite := linuxFlagValues(confined.Argv, "--rw")
	if !linuxHasArg(readWrite, "/dev/null") {
		t.Fatalf("read-write grants = %v, want /dev/null", readWrite)
	}
	for _, grant := range readWrite {
		if !strings.HasPrefix(grant, "/dev/") {
			t.Fatalf("read-write grants = %v, contains non-device %q", readWrite, grant)
		}
	}
}

// TestLandlockRunnerNilPolicyFailsClosed 校验 nil 策略拒绝，绝不返回未受限 argv。
func TestLandlockRunnerNilPolicyFailsClosed(t *testing.T) {
	runner := &landlockRunner{exe: "/opt/rig/host", abi: 4}
	if _, err := runner.Confine([]string{"/bin/sh"}, nil); err == nil {
		t.Fatal("Confine(nil policy) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("Confine(nil policy) error = %v, want ErrSandboxUnavailable", err)
	}
}

// TestLandlockRunnerDangerFullAccessPassesThrough 校验 danger-full-access 直接透传。
func TestLandlockRunnerDangerFullAccessPassesThrough(t *testing.T) {
	runner := &landlockRunner{exe: "/opt/rig/host", abi: 4}
	argv := []string{"/bin/sh", "-c", "echo hi"}
	confined, err := runner.Confine(argv, sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess)))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := strings.Join(confined.Argv, " "), "/bin/sh -c echo hi"; got != want {
		t.Fatalf("Argv = %q, want %q", got, want)
	}
	// 透传没有被任何 runner 包装：EnforcementNone，而不是 Full。
	if confined.Enforcement != sandbox.EnforcementNone {
		t.Fatalf("Enforcement = %q, want %q", confined.Enforcement, sandbox.EnforcementNone)
	}
	confined.Argv[0] = "mutated"
	if argv[0] != "/bin/sh" {
		t.Fatal("Confine aliased the caller argv")
	}
}

// TestLandlockRunnerInvalidModeStaysConfined 记录共享 Policy API 的已知限制：未知
// 模式在 sandbox.New 里被静默降级为 workspace-write，runner 无法观测到 modeErr；
// 但降级后仍然是受限形式，绝不裸跑。
func TestLandlockRunnerInvalidModeStaysConfined(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.Mode("bogus")))
	runner := &landlockRunner{exe: "/opt/rig/host", abi: 4}
	confined, err := runner.Confine([]string{"/bin/true"}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if len(confined.Argv) < 2 || confined.Argv[1] != HelperArgLandlock {
		t.Fatalf("Argv = %v, want confined helper form", confined.Argv)
	}
}

// TestBwrapRunnerConfineArgv 校验 bwrap 包装的 argv 形状：与 DSH 逐 token 一致。
func TestBwrapRunnerConfineArgv(t *testing.T) {
	root := t.TempDir()
	runner := &bwrapRunner{path: "/usr/bin/bwrap"}
	confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi"}, linuxTestPolicy(t, sandbox.ModeWorkspaceWrite, root))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	wantRoot := linuxCanonical(t, root)
	want := []string{
		"/usr/bin/bwrap",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
		"--tmpfs", "/tmp",
		"--bind", wantRoot, wantRoot,
		"--", "/bin/sh", "-c", "echo hi",
	}
	if strings.Join(confined.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Argv = %q\nwant = %q", confined.Argv, want)
	}
	if confined.Enforcement != sandbox.EnforcementFull {
		t.Fatalf("Enforcement = %q, want full", confined.Enforcement)
	}
	if got, wantSigs := strings.Join(confined.DenialSignatures, "|"), strings.Join(DenialSignatures(RunnerBwrap), "|"); got != wantSigs {
		t.Fatalf("DenialSignatures = %v, want %v", confined.DenialSignatures, DenialSignatures(RunnerBwrap))
	}
}

// TestBwrapRunnerReadOnlyArgv 校验只读模式：与 DSH 一致，**没有**私有 tmpfs、
// 没有 --remount-ro /dev，也没有任何宿主可写 bind。
func TestBwrapRunnerReadOnlyArgv(t *testing.T) {
	runner := &bwrapRunner{path: "/usr/bin/bwrap"}
	confined, err := runner.Confine([]string{"/bin/true"}, linuxTestPolicy(t, sandbox.ModeReadOnly, t.TempDir()))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	want := []string{
		"/usr/bin/bwrap",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
		"--", "/bin/true",
	}
	if strings.Join(confined.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("read-only Argv = %q\nwant = %q", confined.Argv, want)
	}
}

// TestBwrapRunnerFailClosed 校验 nil 策略拒绝、danger-full-access 透传且不共享 argv。
func TestBwrapRunnerFailClosed(t *testing.T) {
	runner := &bwrapRunner{path: "/usr/bin/bwrap"}
	if _, err := runner.Confine([]string{"/bin/sh"}, nil); err == nil {
		t.Fatal("Confine(nil policy) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("Confine(nil policy) error = %v, want ErrSandboxUnavailable", err)
	}

	argv := []string{"/bin/sh", "-c", "echo hi"}
	confined, err := runner.Confine(argv, sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess)))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := strings.Join(confined.Argv, " "), "/bin/sh -c echo hi"; got != want {
		t.Fatalf("Argv = %q, want %q", got, want)
	}
	// 透传没有被任何 runner 包装：EnforcementNone，而不是 Full。
	if confined.Enforcement != sandbox.EnforcementNone {
		t.Fatalf("Enforcement = %q, want %q", confined.Enforcement, sandbox.EnforcementNone)
	}
	confined.Argv[0] = "mutated"
	if argv[0] != "/bin/sh" {
		t.Fatal("Confine aliased the caller argv")
	}
}

// TestParseLandlockHelperArgs 校验 helper 参数的解析与 fail-closed 报错。
func TestParseLandlockHelperArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantRO  []string
		wantRW  []string
		wantArg []string
		wantErr bool
	}{
		{
			name:    "full",
			args:    []string{"--ro", "/", "--ro", "/usr", "--rw", "/dev/null", "--rw", "/tmp", "--", "/bin/sh", "-c", "true"},
			wantRO:  []string{"/", "/usr"},
			wantRW:  []string{"/dev/null", "/tmp"},
			wantArg: []string{"/bin/sh", "-c", "true"},
		},
		{
			name:    "no-grants",
			args:    []string{"--", "/bin/true"},
			wantArg: []string{"/bin/true"},
		},
		{name: "missing-separator", args: []string{"--ro", "/"}, wantErr: true},
		{name: "empty-argv", args: []string{"--ro", "/", "--"}, wantErr: true},
		{name: "missing-ro-value", args: []string{"--ro"}, wantErr: true},
		{name: "empty-ro-value", args: []string{"--ro", ""}, wantErr: true},
		{name: "unknown-flag", args: []string{"--bogus", "--", "/bin/true"}, wantErr: true},
		{name: "positional-before-separator", args: []string{"/bin/true", "--", "/bin/true"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseLandlockHelperArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLandlockHelperArgs(%v) succeeded, want error", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLandlockHelperArgs(%v): %v", tc.args, err)
			}
			if got, want := strings.Join(cfg.readOnly, ","), strings.Join(tc.wantRO, ","); got != want {
				t.Fatalf("readOnly = %v, want %v", cfg.readOnly, tc.wantRO)
			}
			if got, want := strings.Join(cfg.readWrite, ","), strings.Join(tc.wantRW, ","); got != want {
				t.Fatalf("readWrite = %v, want %v", cfg.readWrite, tc.wantRW)
			}
			if got, want := strings.Join(cfg.argv, " "), strings.Join(tc.wantArg, " "); got != want {
				t.Fatalf("argv = %v, want %v", cfg.argv, tc.wantArg)
			}
		})
	}
}

// TestRunHelperIgnoresNonHelperArgs 校验普通宿主参数不会被当成 helper。
func TestRunHelperIgnoresNonHelperArgs(t *testing.T) {
	if handled, _ := runHelper(nil); handled {
		t.Fatal("runHelper(nil) handled an empty invocation")
	}
	if handled, _ := runHelper([]string{"--help"}); handled {
		t.Fatal("runHelper(--help) handled a normal argument")
	}
}

// TestLandlockHelperSubprocessReportsParseError 通过真实子进程校验 helper 接线：
// 非法参数必须打印 "rig-sandbox: " 前缀并以 125 退出。
func TestLandlockHelperSubprocessReportsParseError(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, HelperArgLandlock, "--bogus")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper accepted --bogus, output %q", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("helper run: %v", err)
	}
	if got, want := exitErr.ExitCode(), 125; got != want {
		t.Fatalf("helper exit code = %d, want %d (output %q)", got, want, out)
	}
	if !strings.Contains(string(out), "rig-sandbox: ") {
		t.Fatalf("helper output = %q, want rig-sandbox: prefix", out)
	}
}

// TestProbeLandlock 校验功能探测在支持 Landlock 的内核上返回 ABI。
func TestProbeLandlock(t *testing.T) {
	abi, ok := probeLandlock(5 * time.Second)
	if !ok {
		t.Skip("主机内核不支持 Landlock，跳过功能探测测试")
	}
	if abi < 1 {
		t.Fatalf("probeLandlock ABI = %d, want >= 1", abi)
	}
	t.Logf("Landlock ABI = %d", abi)
}

// TestProbeBwrap 校验 bwrap 功能探测。
func TestProbeBwrap(t *testing.T) {
	path, ok := probeBwrap(10 * time.Second)
	if !ok {
		t.Skip("宿主机没有可用的 bwrap，跳过功能探测测试")
	}
	if path == "" {
		t.Fatal("probeBwrap returned an empty path")
	}
	t.Logf("bwrap = %s", path)
}

// TestNewPrefersBwrapWhenAvailable 校验 bwrap 在链中优先被选中。
func TestNewPrefersBwrapWhenAvailable(t *testing.T) {
	if _, ok := probeBwrap(10 * time.Second); !ok {
		t.Skip("宿主机没有可用的 bwrap，跳过链选择测试")
	}
	runner, err := New(Config{ProbeTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := runner.(*bwrapRunner); !ok {
		t.Fatalf("New returned %T, want *bwrapRunner", runner)
	}
}

// TestNewFallsBackWhenBwrapMissing 校验 bwrap 缺失时回退到 Landlock，或明确不可用。
func TestNewFallsBackWhenBwrapMissing(t *testing.T) {
	t.Setenv("PATH", "")
	runner, err := New(Config{ProbeTimeout: 10 * time.Second})
	abi, landlockOK := probeLandlock(10 * time.Second)
	if landlockOK {
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, ok := runner.(*landlockRunner)
		if !ok {
			t.Fatalf("New returned %T, want *landlockRunner", runner)
		}
		if got.abi != abi {
			t.Fatalf("landlockRunner.abi = %d, want %d", got.abi, abi)
		}
		return
	}
	if err == nil {
		t.Fatalf("New returned %T, want ErrSandboxUnavailable", runner)
	}
	if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("New error = %v, want ErrSandboxUnavailable", err)
	}
}

// TestNewSkipsBrokenBwrap 校验探测失败的 bwrap 被跳过而不是被选中。
func TestNewSkipsBrokenBwrap(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake bwrap: %v", err)
	}
	t.Setenv("PATH", dir)
	runner, err := New(Config{ProbeTimeout: 10 * time.Second})
	if _, landlockOK := probeLandlock(10 * time.Second); landlockOK {
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, ok := runner.(*landlockRunner); !ok {
			t.Fatalf("New returned %T, want *landlockRunner (broken bwrap must be skipped)", runner)
		}
		return
	}
	if err == nil {
		t.Fatalf("New returned %T, want ErrSandboxUnavailable", runner)
	}
	if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("New error = %v, want ErrSandboxUnavailable", err)
	}
}

// TestLandlockRuntimeReadOnly 用真实内核 Landlock 验证只读模式：外部写入被拒、
// 读取被允许。
func TestLandlockRuntimeReadOnly(t *testing.T) {
	abi := requireLandlockABI(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	runner := &landlockRunner{exe: exe, abi: abi}
	dir := t.TempDir()
	policy := linuxTestPolicy(t, sandbox.ModeReadOnly, dir)

	t.Run("write-denied", func(t *testing.T) {
		target := filepath.Join(dir, "created.txt")
		confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi > " + linuxShellQuote(target)}, policy)
		if err != nil {
			t.Fatalf("Confine: %v", err)
		}
		out, code := runConfinedCommand(t, confined)
		if code == 0 {
			t.Fatalf("confined write succeeded (output %q), want denial", out)
		}
		if !strings.Contains(strings.ToLower(out), "permission denied") {
			t.Fatalf("output = %q, want a permission denied denial signature", out)
		}
		if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
			t.Fatalf("target %q exists after denied write (stat err %v)", target, statErr)
		}
	})

	t.Run("read-allowed", func(t *testing.T) {
		file := filepath.Join(dir, "existing.txt")
		if err := os.WriteFile(file, []byte("hello-landlock"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		confined, err := runner.Confine([]string{"/bin/cat", file}, policy)
		if err != nil {
			t.Fatalf("Confine: %v", err)
		}
		out, code := runConfinedCommand(t, confined)
		if code != 0 {
			t.Fatalf("confined read failed with code %d: %q", code, out)
		}
		if !strings.Contains(out, "hello-landlock") {
			t.Fatalf("output = %q, want file content", out)
		}
	})
}

// TestLandlockRuntimeWorkspaceWrite 用真实内核 Landlock 验证 workspace-write：
// 工作区根内写入允许，根外写入被拒。
func TestLandlockRuntimeWorkspaceWrite(t *testing.T) {
	abi := requireLandlockABI(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	runner := &landlockRunner{exe: exe, abi: abi}
	root := linuxScratchDir(t)
	outside := linuxScratchDir(t)
	policy := linuxTestPolicy(t, sandbox.ModeWorkspaceWrite, root)

	t.Run("write-inside-allowed", func(t *testing.T) {
		target := filepath.Join(root, "inside.txt")
		confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo inside > " + linuxShellQuote(target)}, policy)
		if err != nil {
			t.Fatalf("Confine: %v", err)
		}
		out, code := runConfinedCommand(t, confined)
		if code != 0 {
			t.Fatalf("confined write inside root failed with code %d: %q", code, out)
		}
		content, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read confined write: %v", err)
		}
		if !strings.Contains(string(content), "inside") {
			t.Fatalf("content = %q, want inside", content)
		}
	})

	t.Run("write-outside-denied", func(t *testing.T) {
		target := filepath.Join(outside, "outside.txt")
		confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo outside > " + linuxShellQuote(target)}, policy)
		if err != nil {
			t.Fatalf("Confine: %v", err)
		}
		out, code := runConfinedCommand(t, confined)
		if code == 0 {
			t.Fatalf("confined write outside root succeeded (output %q), want denial", out)
		}
		if !strings.Contains(strings.ToLower(out), "permission denied") {
			t.Fatalf("output = %q, want a permission denied denial signature", out)
		}
		if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
			t.Fatalf("target %q exists after denied write (stat err %v)", target, statErr)
		}
	})
}

// TestBwrapRuntimeReadOnly 用真实 bwrap 验证只读模式：外部写入被拒、读取允许。
func TestBwrapRuntimeReadOnly(t *testing.T) {
	path, ok := probeBwrap(10 * time.Second)
	if !ok {
		t.Skip("宿主机没有可用的 bwrap，跳过运行期强制测试")
	}
	runner := &bwrapRunner{path: path}
	// 注意：read-only 下 /tmp 是私有 tmpfs，宿主的 /tmp 内容在域内不可见。因此
	// fixture 与“被拒写入的目标”都必须放在 /tmp 之外（linuxScratchDir 优先
	// /var/tmp），否则测到的是“路径不存在”而不是“只读”。
	dir := linuxScratchDir(t)
	policy := linuxTestPolicy(t, sandbox.ModeReadOnly, dir)

	t.Run("host-write-denied", func(t *testing.T) {
		target := filepath.Join(dir, "created.txt")
		confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi > " + linuxShellQuote(target)}, policy)
		if err != nil {
			t.Fatalf("Confine: %v", err)
		}
		out, code := runConfinedCommand(t, confined)
		if code == 0 {
			t.Fatalf("bwrap confined write succeeded (output %q), want denial", out)
		}
		if !strings.Contains(strings.ToLower(out), "read-only file system") {
			t.Fatalf("output = %q, want read-only file system denial signature", out)
		}
		if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
			t.Fatalf("target %q exists after denied write (stat err %v)", target, statErr)
		}
	})

	t.Run("read-allowed", func(t *testing.T) {
		file := filepath.Join(dir, "existing.txt")
		if err := os.WriteFile(file, []byte("hello-bwrap"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		confined, err := runner.Confine([]string{"/bin/cat", file}, policy)
		if err != nil {
			t.Fatalf("Confine: %v", err)
		}
		out, code := runConfinedCommand(t, confined)
		if code != 0 {
			t.Fatalf("bwrap confined read failed with code %d: %q", code, out)
		}
		if !strings.Contains(out, "hello-bwrap") {
			t.Fatalf("output = %q, want file content", out)
		}
	})

	// DSH 对齐后的 read-only：没有私有 tmpfs（不挂 --tmpfs /tmp），也没有
	// --remount-ro /dev。于是 /tmp 与 /dev/shm 的宿主路径都写不进去，而
	// `--dev /dev` 提供的**命名空间内** tmpfs 是普通可写目录（/dev/zzz、
	// /dev/shm/probe 落在那块 tmpfs 里，不会落到宿主）。
	t.Run("read-only-has-no-writable-tmp-or-dev", func(t *testing.T) {
		script := strings.Join([]string{
			"mktemp > /dev/null 2>&1 || echo MKTEMP_DENIED",
			"echo x > /dev/null && echo DEVNULL_OK",
			"touch /dev/zzz 2>/dev/null && echo DEV_TOUCH_OK",
			"touch /dev/shm/probe 2>/dev/null && echo DEV_SHM_OK",
			"touch /tmp/probe 2>/dev/null || echo TMP_DENIED",
		}, "; ")
		confined, err := runner.Confine([]string{"/bin/sh", "-c", script}, policy)
		if err != nil {
			t.Fatalf("Confine: %v", err)
		}
		out, _ := runConfinedCommand(t, confined)
		for _, want := range []string{"MKTEMP_DENIED", "DEVNULL_OK", "DEV_TOUCH_OK", "DEV_SHM_OK", "TMP_DENIED"} {
			if !strings.Contains(out, want) {
				t.Fatalf("output = %q, want %q", out, want)
			}
		}
	})
}

// TestBwrapRuntimeWorkspaceWrite 用真实 bwrap 验证 workspace-write：工作区根内
// 写入允许，根外写入被拒。
func TestBwrapRuntimeWorkspaceWrite(t *testing.T) {
	path, ok := probeBwrap(10 * time.Second)
	if !ok {
		t.Skip("宿主机没有可用的 bwrap，跳过运行期强制测试")
	}
	runner := &bwrapRunner{path: path}
	root := t.TempDir()
	outside := linuxScratchDir(t)
	policy := linuxTestPolicy(t, sandbox.ModeWorkspaceWrite, root)

	insideTarget := filepath.Join(root, "inside.txt")
	confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo inside > " + linuxShellQuote(insideTarget)}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	out, code := runConfinedCommand(t, confined)
	if code != 0 {
		t.Fatalf("bwrap write inside root failed with code %d: %q", code, out)
	}
	content, err := os.ReadFile(insideTarget)
	if err != nil {
		t.Fatalf("read confined write: %v", err)
	}
	if !strings.Contains(string(content), "inside") {
		t.Fatalf("content = %q, want inside", content)
	}

	outsideTarget := filepath.Join(outside, "outside.txt")
	confined, err = runner.Confine([]string{"/bin/sh", "-c", "echo outside > " + linuxShellQuote(outsideTarget)}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	out, code = runConfinedCommand(t, confined)
	if code == 0 {
		t.Fatalf("bwrap write outside root succeeded (output %q), want denial", out)
	}
	if !strings.Contains(strings.ToLower(out), "read-only file system") {
		t.Fatalf("output = %q, want read-only file system denial signature", out)
	}
	if _, statErr := os.Stat(outsideTarget); !os.IsNotExist(statErr) {
		t.Fatalf("target %q exists after denied write (stat err %v)", outsideTarget, statErr)
	}
}

func TestLandlockRuntimeMetadataIsNotGoverned(t *testing.T) {
	abi := requireLandlockABI(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dir := linuxScratchDir(t)
	file := filepath.Join(dir, "metadata.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	runner := &landlockRunner{exe: exe, abi: abi}
	policy := linuxTestPolicy(t, sandbox.ModeReadOnly, dir)
	confined, err := runner.Confine([]string{"/bin/chmod", "0777", file}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	out, code := runConfinedCommand(t, confined)
	info, statErr := os.Stat(file)
	if statErr != nil {
		t.Fatalf("stat: %v", statErr)
	}
	t.Logf("chmod under a read-only landlock domain: code=%d output=%q mode=%v", code, out, info.Mode().Perm())
	// 这就是 partial 的依据：内容写被拒，但元数据写没有被治理。
	if code == 0 && info.Mode().Perm() == 0o777 {
		if got := landlockEnforcement(abi); got != sandbox.EnforcementPartial {
			t.Fatalf("landlockEnforcement(%d) = %q, want partial (metadata is not governed)", abi, got)
		}
		return
	}
	t.Log("此内核/权限下 chmod 被拒；Landlock 的元数据缺口仍然存在，只是这里观测不到")
}

func TestClassifyBwrapFailure(t *testing.T) {
	reason := classifyBwrapFailure("bwrap: No permissions to creating new namespace, likely because the kernel does not allow non-privileged user namespaces", errors.New("exit status 1"))
	if !strings.Contains(reason, "user namespace") {
		t.Fatalf("reason = %q, want the user-namespace diagnostic", reason)
	}
	reason = classifyBwrapFailure("bwrap: Can't find source path /nope: No such file or directory", errors.New("exit status 1"))
	if !strings.Contains(reason, "probe profile") {
		t.Fatalf("reason = %q, want the profile diagnostic", reason)
	}
	reason = classifyBwrapFailure("bwrap: unexpected failure", errors.New("exit status 1"))
	if !strings.Contains(reason, "unexpected failure") {
		t.Fatalf("reason = %q, want the raw detail preserved", reason)
	}
}

// TestBwrapRuntimeNoProtectedRule 记录对齐 DSH 后的语义变化：保护路径**不再**由
// OS 后端渲染（bwrap 不生成覆盖它的 --ro-bind），所以工作区里的 .git 在挂载
// 命名空间里是可写的。进程内围栏（Policy.CheckWritablePath）仍然拦着它；这条
// 用例把"OS 层不再保护"钉死，避免有人误以为它还生效。
func TestBwrapRuntimeNoProtectedRule(t *testing.T) {
	path, ok := probeBwrap(10 * time.Second)
	if !ok {
		t.Skip("宿主机没有可用的 bwrap，跳过运行期强制测试")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(true),
	)
	policy.AddProtectedPaths(filepath.Join(root, ".git"))
	runner := &bwrapRunner{path: path}
	script := "echo ok > " + linuxShellQuote(filepath.Join(root, "ok.txt")) + " && echo WORKSPACE_OK; " +
		"echo bad > " + linuxShellQuote(filepath.Join(root, ".git", "x")) + " 2>/dev/null && echo PROTECTED_WRITABLE"
	confined, err := runner.Confine([]string{"/bin/sh", "-c", script}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	out, _ := runConfinedCommand(t, confined)
	t.Logf("bwrap output: %s", out)
	if !strings.Contains(out, "WORKSPACE_OK") {
		t.Fatalf("output = %q, want WORKSPACE_OK", out)
	}
	if !strings.Contains(out, "PROTECTED_WRITABLE") {
		t.Fatalf("output = %q：对齐 DSH 后保护路径应在 OS 层可写（只由进程内围栏拦）", out)
	}
}

// TestLandlockRuntimeNoProtectedRule 是上面那条的 Landlock 版本。
func TestLandlockRuntimeNoProtectedRule(t *testing.T) {
	if !landlockSupported {
		t.Skip("landlock unsupported on this arch")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	abi, ok := probeLandlock(10 * time.Second)
	if !ok {
		t.Skip("Landlock 不可用，跳过运行期强制测试")
	}
	root := t.TempDir()
	if mkErr := os.MkdirAll(filepath.Join(root, ".git"), 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(true),
	)
	policy.AddProtectedPaths(filepath.Join(root, ".git"))
	runner := &landlockRunner{exe: exe, abi: abi}
	script := "echo ok > " + linuxShellQuote(filepath.Join(root, "ok.txt")) + " && echo WORKSPACE_OK; " +
		"echo bad > " + linuxShellQuote(filepath.Join(root, ".git", "x")) + " 2>/dev/null && echo PROTECTED_WRITABLE"
	confined, err := runner.Confine([]string{"/bin/sh", "-c", script}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	out, _ := runConfinedCommand(t, confined)
	t.Logf("landlock output: %s", out)
	if !strings.Contains(out, "WORKSPACE_OK") || !strings.Contains(out, "PROTECTED_WRITABLE") {
		t.Fatalf("output = %q，want WORKSPACE_OK + PROTECTED_WRITABLE", out)
	}
}
