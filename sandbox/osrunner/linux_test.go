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

// TestBwrapRunnerConfineArgv 校验 bwrap 包装的 argv 形状。
func TestBwrapRunnerConfineArgv(t *testing.T) {
	root := t.TempDir()
	runner := &bwrapRunner{path: "/usr/bin/bwrap"}
	confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi"}, linuxTestPolicy(t, sandbox.ModeWorkspaceWrite, root))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := confined.Argv[0], "/usr/bin/bwrap"; got != want {
		t.Fatalf("Argv[0] = %q, want %q", got, want)
	}
	for _, want := range []string{"--ro-bind", "--dev", "--unshare-pid", "--proc", "--die-with-parent", "--tmpfs", "--bind", "--cap-drop", "--unshare-net"} {
		if !linuxHasArg(confined.Argv, want) {
			t.Fatalf("Argv = %v, missing %q", confined.Argv, want)
		}
	}
	wantRoot := linuxCanonical(t, root)
	found := false
	for i := 0; i+2 < len(confined.Argv); i++ {
		if confined.Argv[i] == "--bind" && confined.Argv[i+1] == wantRoot && confined.Argv[i+2] == wantRoot {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Argv = %v, want --bind %q %q", confined.Argv, wantRoot, wantRoot)
	}
	sep := -1
	for i, arg := range confined.Argv {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+4 != len(confined.Argv) {
		t.Fatalf("Argv = %v, want a single -- command separator", confined.Argv)
	}
	if got, want := strings.Join(confined.Argv[sep+1:], " "), "/bin/sh -c echo hi"; got != want {
		t.Fatalf("trailing argv = %q, want %q", got, want)
	}
	if confined.Enforcement != sandbox.EnforcementFull {
		t.Fatalf("Enforcement = %q, want full", confined.Enforcement)
	}
	if got, want := strings.Join(confined.DenialSignatures, "|"), strings.Join(DenialSignatures(RunnerBwrap), "|"); got != want {
		t.Fatalf("DenialSignatures = %v, want %v", confined.DenialSignatures, DenialSignatures(RunnerBwrap))
	}
}

// TestBwrapRunnerReadOnlyArgv 校验只读模式：有私有 tmpfs /tmp（不落宿主）与
// --remount-ro /dev，但没有任何宿主可写 bind。
//
// 旧契约禁止 --tmpfs。语义变化：私有 tmpfs 属于“不落宿主”的可写区，让 mktemp、
// git status 这类只读意图的命令仍可用；宿主的 workspace、/tmp、$TMPDIR 依然不可写，
// /dev 也被重新挂成只读（R-03/R-05）。
func TestBwrapRunnerReadOnlyArgv(t *testing.T) {
	runner := &bwrapRunner{path: "/usr/bin/bwrap"}
	confined, err := runner.Confine([]string{"/bin/true"}, linuxTestPolicy(t, sandbox.ModeReadOnly, t.TempDir()))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if !linuxHasArg(confined.Argv, "--tmpfs") {
		t.Fatalf("read-only Argv = %v, want --tmpfs /tmp", confined.Argv)
	}
	if !linuxHasArg(confined.Argv, "--remount-ro") {
		t.Fatalf("read-only Argv = %v, want --remount-ro /dev", confined.Argv)
	}
	if binds := linuxFlagValues(confined.Argv, "--bind"); len(binds) != 0 {
		t.Fatalf("read-only Argv = %v, must not bind host paths: %v", confined.Argv, binds)
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

	// 新契约：/tmp 是私有 tmpfs（可写、不落宿主），/dev 的文件系统被 remount-ro
	// 成只读（R-03/R-05），但 /dev/null 等设备节点仍可写。
	t.Run("private-tmp-and-devices", func(t *testing.T) {
		script := strings.Join([]string{
			"mktemp > /dev/null && echo MKTEMP_OK",
			"echo x > /dev/null && echo DEVNULL_OK",
			"touch /dev/zzz 2>/dev/null || echo DEV_TOUCH_DENIED",
			"touch /dev/shm/probe 2>/dev/null || echo DEV_SHM_DENIED",
			"touch /tmp/probe 2>/dev/null && echo PRIVATE_TMP_OK",
		}, "; ")
		confined, err := runner.Confine([]string{"/bin/sh", "-c", script}, policy)
		if err != nil {
			t.Fatalf("Confine: %v", err)
		}
		out, _ := runConfinedCommand(t, confined)
		for _, want := range []string{"MKTEMP_OK", "DEVNULL_OK", "DEV_TOUCH_DENIED", "DEV_SHM_DENIED", "PRIVATE_TMP_OK"} {
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

// TestBwrapRuntimeTmpdirOutsideSlashTmp 是 R-02 的运行期回归：
// 旧实现在 --tmpfs /tmp 之后只 bind policy.Roots()，完全忽略 $TMPDIR，因此
// TMPDIR=/var/tmp 或工作区之外的私有临时目录在工作区里写不进去。新实现把
// Profile.ReadWrite 的每个非 /、非 /dev/*、非 /tmp 路径都 --bind 回原路径。
func TestBwrapRuntimeTmpdirOutsideSlashTmp(t *testing.T) {
	path, ok := probeBwrap(10 * time.Second)
	if !ok {
		t.Skip("宿主机没有可用的 bwrap，跳过运行期强制测试")
	}
	// 工作区与 TMPDIR 都放在 /tmp 之外：/tmp 在域内是私有 tmpfs。
	root := linuxScratchDir(t)
	tmpdir := linuxScratchDir(t)
	// “未授权但宿主可写”的位置：$HOME 在 CI/容器里本身可能已经只读，那样断言就
	// 变成空转；这里用 linuxScratchDir 造一个宿主的当前用户确实能写的位置。
	outside := linuxScratchDir(t)
	t.Setenv("TMPDIR", tmpdir)
	runner := &bwrapRunner{path: path}
	policy := linuxTestPolicy(t, sandbox.ModeWorkspaceWrite, root)

	inside := filepath.Join(root, "workspace.txt")
	tempTarget := filepath.Join(tmpdir, "tmpdir.txt")
	outsideTarget := filepath.Join(outside, "outside.txt")
	script := strings.Join([]string{
		"echo ws > " + linuxShellQuote(inside) + " && echo WORKSPACE_OK",
		"echo tmp > " + linuxShellQuote(tempTarget) + " && echo TMPDIR_OK",
		"echo out > " + linuxShellQuote(outsideTarget) + " 2>/dev/null || echo OUTSIDE_DENIED",
	}, "; ")
	confined, err := runner.Confine([]string{"/bin/sh", "-c", script}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	t.Logf("bwrap argv: %v", confined.Argv)
	out, code := runConfinedCommand(t, confined)
	t.Logf("bwrap output (code %d):\n%s", code, out)
	for _, want := range []string{"WORKSPACE_OK", "TMPDIR_OK", "OUTSIDE_DENIED"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
	for _, file := range []string{inside, tempTarget} {
		if _, statErr := os.Stat(file); statErr != nil {
			t.Fatalf("expected %q to exist after the confined write: %v", file, statErr)
		}
	}
	if _, statErr := os.Stat(outsideTarget); !os.IsNotExist(statErr) {
		t.Fatalf("outside target %q exists after a denied write (stat err %v)", outsideTarget, statErr)
	}
}

// TestBwrapRuntimeProtected 验证 bwrap 的保护路径：--ro-bind 在 --bind 之后覆盖，
// 因此 workspace 可写但 protected 子目录不可写。
func TestBwrapRuntimeProtected(t *testing.T) {
	path, ok := probeBwrap(10 * time.Second)
	if !ok {
		t.Skip("宿主机没有可用的 bwrap，跳过运行期强制测试")
	}
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runner := &bwrapRunner{path: path}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(gitDir),
	)
	okTarget := filepath.Join(root, "ok.txt")
	protectedTarget := filepath.Join(gitDir, "x")
	script := strings.Join([]string{
		"echo ok > " + linuxShellQuote(okTarget) + " && echo WORKSPACE_OK",
		"echo bad > " + linuxShellQuote(protectedTarget) + " 2>&1 || echo PROTECTED_DENIED",
	}, "; ")
	confined, err := runner.Confine([]string{"/bin/sh", "-c", script}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	t.Logf("bwrap argv: %v", confined.Argv)
	out, _ := runConfinedCommand(t, confined)
	t.Logf("bwrap output:\n%s", out)
	for _, want := range []string{"WORKSPACE_OK", "PROTECTED_DENIED"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
	if _, statErr := os.Stat(okTarget); statErr != nil {
		t.Fatalf("workspace write missing: %v", statErr)
	}
	if _, statErr := os.Stat(protectedTarget); !os.IsNotExist(statErr) {
		t.Fatalf("protected target %q exists after a denied write (stat err %v)", protectedTarget, statErr)
	}
}

// TestLandlockRuntimeProtected 验证 Landlock 的保护路径。
//
// Landlock 的规则是并集语义（实测：父 --rw + 子 --ro 时子仍可写），因此实现把
// 写授权“下移”到保护链的兄弟项：根只读、每个子项单独授权。后果是**不能在该根
// 里新建/删除条目**（Landlock 固有局限），但已存在的直接子文件仍可写、子目录
// 仍可读写，而被保护子树（.git）整体只读。
func TestLandlockRuntimeProtected(t *testing.T) {
	abi := requireLandlockABI(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	srcDir := filepath.Join(root, "src")
	for _, dir := range []string{gitDir, srcDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	okTarget := filepath.Join(root, "ok.txt")
	if err := os.WriteFile(okTarget, []byte("seed"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	runner := &landlockRunner{exe: exe, abi: abi}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(gitDir),
	)
	protectedTarget := filepath.Join(gitDir, "x")
	srcTarget := filepath.Join(srcDir, "a.txt")
	script := strings.Join([]string{
		"echo ok > " + linuxShellQuote(okTarget) + " && echo WORKSPACE_OK",
		"echo s > " + linuxShellQuote(srcTarget) + " && echo SUBDIR_OK",
		"echo bad > " + linuxShellQuote(protectedTarget) + " 2>&1 || echo PROTECTED_DENIED",
	}, "; ")
	confined, err := runner.Confine([]string{"/bin/sh", "-c", script}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	t.Logf("landlock argv: %v", confined.Argv)
	out, _ := runConfinedCommand(t, confined)
	t.Logf("landlock output:\n%s", out)
	for _, want := range []string{"WORKSPACE_OK", "SUBDIR_OK", "PROTECTED_DENIED"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
	if content, readErr := os.ReadFile(okTarget); readErr != nil || !strings.Contains(string(content), "ok") {
		t.Fatalf("workspace write missing: content=%q err=%v", content, readErr)
	}
	if _, statErr := os.Stat(srcTarget); statErr != nil {
		t.Fatalf("subdir write missing: %v", statErr)
	}
	if _, statErr := os.Stat(protectedTarget); !os.IsNotExist(statErr) {
		t.Fatalf("protected target %q exists after a denied write (stat err %v)", protectedTarget, statErr)
	}
}

// TestLandlockRuntimeMetadataIsNotGoverned 记录 Landlock 为什么只能是 partial：
// read-only 域下 chmod 依然能改任意文件的权限位（Landlock 没有元数据访问位）。
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
