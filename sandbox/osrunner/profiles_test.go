package osrunner

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件锁定 Profile（profile.go）与四个后端渲染（profiles.go、windows_plan.go）
// 的契约。语义变化见各处注释：read-only 不再“没有任何可写路径”，而是“只有设备
// 节点 + bwrap 私有 tmpfs /tmp”；workspace-write 的可写集是策略根 ∪ 显式追加根
// ∪（按开关过滤的）临时目录，并且保护路径在三个后端里都落在正确位置。

// workspacePolicy 构造一个 workspace-write 策略，根为 root。
func workspacePolicy(t *testing.T, root string) *sandbox.Policy {
	t.Helper()
	return sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
}

// contains 报告 args 是否包含 want。
func contains(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// indexOf 返回 want 在 args 中的下标，不存在返回 -1。
func indexOf(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}

// flagValues 收集 args 里所有 "--flag value" 形式的 value。
func flagValues(args []string, flag string) []string {
	var values []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			values = append(values, args[i+1])
			i++
		}
	}
	return values
}

// canonicalPath 返回路径的真实绝对形式（与 profile 规范化一致）。
func canonicalPath(t *testing.T, path string) string {
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

// customTempDir 返回一个非 /tmp 且已存在的临时目录，用于验证 $TMPDIR 的渲染。
// 它把 TMPDIR 指向该目录，因此 os.TempDir()（非 Windows）就是它。
func customTempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("os.TempDir 在 Windows 上不读 TMPDIR")
	}
	dir := canonicalPath(t, t.TempDir())
	t.Setenv("TMPDIR", dir)
	return dir
}

// ---------------------------------------------------------------------------
// BuildProfile
// ---------------------------------------------------------------------------

func TestBuildProfileDangerFullAccessIsNil(t *testing.T) {
	profile, err := BuildProfile(sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess)))
	if err != nil {
		t.Fatalf("BuildProfile(danger-full-access) error = %v, want nil", err)
	}
	if profile != nil {
		t.Fatalf("BuildProfile(danger-full-access) = %+v, want nil profile (passthrough)", profile)
	}
}

func TestBuildProfileNilPolicyFailsClosed(t *testing.T) {
	if _, err := BuildProfile(nil); err == nil {
		t.Fatal("BuildProfile(nil) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("BuildProfile(nil) error = %v, want ErrSandboxUnavailable", err)
	}
}

// 新契约（旧测试断言 WritableRoots(read-only) 为空）：
// read-only 仍然不授予 workspace、/tmp、$TMPDIR，但不再是“没有任何可写路径”——
// ReadWrite 只剩设备节点（/dev/null 必备）。私有 tmpfs /tmp 是 bwrap 的运行时
// 机制，不是宿主路径，因此不会出现在 ReadWrite 里。
func TestBuildProfileReadOnlyGrantsOnlyDevices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX 设备节点（/dev/*）在 Windows 上不存在；这些断言验证的是 POSIX 渲染")
	}
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if profile.Mode != sandbox.ModeReadOnly {
		t.Fatalf("Mode = %q, want read-only", profile.Mode)
	}
	if len(profile.ReadOnly) != 1 || profile.ReadOnly[0] != "/" {
		t.Fatalf("ReadOnly = %v, want [/]", profile.ReadOnly)
	}
	if !contains(profile.ReadWrite, "/dev/null") {
		t.Fatalf("ReadWrite = %v, want /dev/null", profile.ReadWrite)
	}
	workspace := canonicalPath(t, root)
	for _, path := range profile.ReadWrite {
		if !strings.HasPrefix(path, "/dev/") {
			t.Fatalf("ReadWrite = %v, contains non-device path %q", profile.ReadWrite, path)
		}
		if path == workspace {
			t.Fatalf("ReadWrite = %v, must not grant the workspace", profile.ReadWrite)
		}
	}
	if len(profile.Protected) != 0 {
		t.Fatalf("Protected = %v, want empty for read-only", profile.Protected)
	}
	if !profile.PrivateTmp {
		t.Fatal("PrivateTmp = false, want true for read-only")
	}
}

// /dev/shm 是可写的 tmpfs：read-only 下授予它等于授予一个真实落盘的写面
// （实测 `touch /dev/shm/probe` 的文件会留在宿主上）。只有 workspace-write 才给。
func TestBuildProfileSharedMemoryOnlyInWorkspaceWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX 设备节点（/dev/*）在 Windows 上不存在；这些断言验证的是 POSIX 渲染")
	}
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("/dev/shm 不存在，跳过")
	}
	readOnly, err := BuildProfile(sandbox.New(sandbox.WithMode(sandbox.ModeReadOnly)))
	if err != nil {
		t.Fatalf("BuildProfile(read-only): %v", err)
	}
	if contains(readOnly.ReadWrite, "/dev/shm") {
		t.Fatalf("read-only ReadWrite = %v, must not grant /dev/shm", readOnly.ReadWrite)
	}
	workspace, err := BuildProfile(sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeWorkspaceWrite)))
	if err != nil {
		t.Fatalf("BuildProfile(workspace-write): %v", err)
	}
	if !contains(workspace.ReadWrite, "/dev/shm") {
		t.Fatalf("workspace-write ReadWrite = %v, want /dev/shm", workspace.ReadWrite)
	}
}

func TestBuildProfileWorkspaceWritableSet(t *testing.T) {
	tempDir := customTempDir(t)
	root := t.TempDir()
	extra := canonicalPath(t, t.TempDir())
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithWritableRoots(extra),
	)
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if profile.Mode != sandbox.ModeWorkspaceWrite {
		t.Fatalf("Mode = %q, want workspace-write", profile.Mode)
	}
	for _, want := range []string{canonicalPath(t, root), extra, tempDir, "/tmp", "/dev/null"} {
		if !contains(profile.ReadWrite, want) {
			t.Fatalf("ReadWrite = %v, want it to contain %q", profile.ReadWrite, want)
		}
	}
	if !profile.PrivateTmp {
		t.Fatal("PrivateTmp = false, want true for workspace-write")
	}
	if profile.Network {
		t.Fatal("Network = true, want false by default")
	}
	if len(profile.Dropped) != 0 {
		t.Fatalf("Dropped = %v, want empty", profile.Dropped)
	}
}

// BuildProfile 不再静默丢弃宿主显式配置的可写根：被丢弃的项从 ReadWrite 里排除，
// 同时出现在 Profile.Dropped 里，让调用方能报警。
func TestBuildProfileReportsDroppedWritableRoots(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.MkdirAll(gone, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 策略构造期会解析符号链接/短名，所以期望值也要用规范形式（Windows CI 的
	// Temp 是 8.3 短名）。
	goneCanonical := canonicalForTest(gone)
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithWritableRoots(gone),
	)
	// 策略构造之后再删除：模拟“配置了但运行期不存在”。
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if contains(profile.ReadWrite, goneCanonical) {
		t.Fatalf("ReadWrite = %v, contains the removed root %q", profile.ReadWrite, goneCanonical)
	}
	if !contains(profile.Dropped, goneCanonical) {
		t.Fatalf("Dropped = %v, want it to report %q", profile.Dropped, goneCanonical)
	}
}

// IncludeSlashTmp(false)：字面 /tmp 不再进入可写集（$TMPDIR 仍可用）。
func TestBuildProfileIncludeSlashTmpFalse(t *testing.T) {
	tempDir := customTempDir(t)
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithSlashTmp(false),
	)
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if contains(profile.ReadWrite, "/tmp") {
		t.Fatalf("ReadWrite = %v, want no /tmp when IncludeSlashTmp=false", profile.ReadWrite)
	}
	if !contains(profile.ReadWrite, tempDir) {
		t.Fatalf("ReadWrite = %v, want $TMPDIR %q", profile.ReadWrite, tempDir)
	}
}

// IncludeTmpdirEnv(false)：$TMPDIR 不再进入可写集（字面 /tmp 仍然可用）。
func TestBuildProfileIncludeTmpdirEnvFalse(t *testing.T) {
	tempDir := customTempDir(t)
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithTmpdirEnv(false),
	)
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if contains(profile.ReadWrite, tempDir) {
		t.Fatalf("ReadWrite = %v, want no $TMPDIR %q when IncludeTmpdirEnv=false", profile.ReadWrite, tempDir)
	}
	if !contains(profile.ReadWrite, "/tmp") {
		t.Fatalf("ReadWrite = %v, want /tmp to stay writable", profile.ReadWrite)
	}
}

// ---------------------------------------------------------------------------
// bwrap
// ---------------------------------------------------------------------------

// 新契约：read-only 有 --tmpfs /tmp（PrivateTmp，私有不落宿主）与
// --remount-ro /dev，但没有任何 --bind。
func TestBwrapArgsReadOnlyHasPrivateTmpAndReadOnlyDev(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap / Landlock 是 Linux 后端")
	}
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeReadOnly))
	args := BwrapArgs(policy)
	for _, want := range []string{"--ro-bind", "/", "--dev", "/dev", "--unshare-pid", "--proc", "/proc", "--die-with-parent"} {
		if !contains(args, want) {
			t.Fatalf("BwrapArgs(read-only) = %v, missing %q", args, want)
		}
	}
	if tmpfs := indexOf(args, "--tmpfs"); tmpfs < 0 || args[tmpfs+1] != "/tmp" {
		t.Fatalf("BwrapArgs(read-only) = %v, want --tmpfs /tmp (private tmp)", args)
	}
	// R-03/R-05：--dev 挂的是新的可写 tmpfs，必须 remount-ro 才是“除设备外无写”。
	remount, dev := indexOf(args, "--remount-ro"), indexOf(args, "--dev")
	if remount < 0 || args[remount+1] != "/dev" {
		t.Fatalf("BwrapArgs(read-only) = %v, want --remount-ro /dev", args)
	}
	if remount < dev {
		t.Fatalf("BwrapArgs(read-only) = %v, --remount-ro must come after --dev", args)
	}
	if binds := flagValues(args, "--bind"); len(binds) != 0 {
		t.Fatalf("BwrapArgs(read-only) = %v, must not grant a writable bind", args)
	}
	if !contains(args, "--cap-drop") || !contains(args, "ALL") {
		t.Fatalf("BwrapArgs(read-only) = %v, want --cap-drop ALL", args)
	}
	if !contains(args, "--unshare-net") {
		t.Fatalf("BwrapArgs(read-only) = %v, want --unshare-net", args)
	}
}

// R-02 回归：workspace-write 必须把 ReadWrite 里的宿主路径逐个 bind，尤其是
// $TMPDIR —— 旧实现只 bind policy.Roots()，TMPDIR=/var/tmp 之类的配置完全失效。
func TestBwrapArgsWorkspaceBindsTmpdirAndRoot(t *testing.T) {
	tempDir := customTempDir(t)
	root := t.TempDir()
	args := BwrapArgs(workspacePolicy(t, root))
	if tmpfs := indexOf(args, "--tmpfs"); tmpfs < 0 || args[tmpfs+1] != "/tmp" {
		t.Fatalf("BwrapArgs(workspace) = %v, want --tmpfs /tmp", args)
	}
	binds := flagValues(args, "--bind")
	for _, want := range []string{canonicalPath(t, root), tempDir} {
		if !contains(binds, want) {
			t.Fatalf("--bind targets = %v, want it to contain %q", binds, want)
		}
	}
	// /dev 下的设备由 --dev 提供：不能再 bind 宿主 /dev（那会更宽）；/tmp 由私有
	// tmpfs 承载，bind 宿主 /tmp 会让 PrivateTmp 失效。
	for _, bind := range binds {
		if bind == "/dev" || strings.HasPrefix(bind, "/dev/") || bind == "/tmp" {
			t.Fatalf("--bind targets = %v, must not bind %q", binds, bind)
		}
	}
	// workspace-write 不禁 /dev：不出现 remount-ro。
	if contains(args, "--remount-ro") {
		t.Fatalf("BwrapArgs(workspace) = %v, must not remount /dev read-only", args)
	}
}

func TestBwrapArgsProtectedOverridesAfterBind(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(gitDir),
	)
	args := BwrapArgs(policy)
	wantRoot, wantGit := canonicalPath(t, root), canonicalPath(t, gitDir)
	if binds := flagValues(args, "--bind"); !contains(binds, wantRoot) {
		t.Fatalf("--bind targets = %v, want the workspace root", binds)
	}
	if roBinds := flagValues(args, "--ro-bind"); !contains(roBinds, wantGit) {
		t.Fatalf("--ro-bind targets = %v, want the protected %q", roBinds, wantGit)
	}
	// Protected 的 ro-bind 必须在 workspace 的 bind 之后（后挂载覆盖前面的 rw）。
	bindIndex, roIndex := -1, -1
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && args[i+1] == wantRoot {
			bindIndex = i
		}
		if args[i] == "--ro-bind" && args[i+1] == wantGit {
			roIndex = i
		}
	}
	if bindIndex < 0 || roIndex < 0 || roIndex < bindIndex {
		t.Fatalf("BwrapArgs = %v, want the protected --ro-bind after the workspace --bind", args)
	}
}

func TestBwrapArgsNetworkAllowedOmitsUnshareNet(t *testing.T) {
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(true),
	)
	if args := BwrapArgs(policy); contains(args, "--unshare-net") {
		t.Fatalf("BwrapArgs(network allowed) = %v, must not unshare the network", args)
	}
}

func TestBwrapArgsDangerFullAccessRendersNothing(t *testing.T) {
	if args := BwrapArgs(sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))); args != nil {
		t.Fatalf("BwrapArgs(danger) = %v, want nil", args)
	}
}

// ---------------------------------------------------------------------------
// Landlock
// ---------------------------------------------------------------------------

func TestLandlockGrantsReadOnlyIsDevicesOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap / Landlock 是 Linux 后端")
	}
	root := t.TempDir()
	readOnly, readWrite := LandlockGrants(sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly)))
	if len(readOnly) != 1 || readOnly[0] != "/" {
		t.Fatalf("read-only grants = %v, want [/]", readOnly)
	}
	if !contains(readWrite, "/dev/null") {
		t.Fatalf("read-write grants = %v, want /dev/null", readWrite)
	}
	workspace := canonicalPath(t, root)
	for _, path := range readWrite {
		if !strings.HasPrefix(path, "/dev/") {
			t.Fatalf("read-write grants = %v, contains non-device %q", readWrite, path)
		}
		if path == workspace || path == "/tmp" {
			t.Fatalf("read-write grants = %v, must not grant %q", readWrite, path)
		}
	}
}

func TestLandlockGrantsWorkspaceAndProtected(t *testing.T) {
	tempDir := customTempDir(t)
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	srcDir := filepath.Join(root, "src")
	for _, dir := range []string{gitDir, srcDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	existing := filepath.Join(root, "ok.txt")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(gitDir),
	)
	readOnly, readWrite := LandlockGrants(policy)
	wantRoot, wantGit := canonicalPath(t, root), canonicalPath(t, gitDir)

	// 设备节点与临时目录仍然 --rw。
	for _, want := range []string{tempDir, "/dev/null"} {
		if !contains(readWrite, want) {
			t.Fatalf("read-write grants = %v, want %q", readWrite, want)
		}
	}
	// 存在保护路径的根必须“下移”：根本身只读、根里的其它子项读写。Landlock 的
	// 规则是并集语义（实测），父目录 rw + 子路径 ro 不会收窄子路径。
	if contains(readWrite, wantRoot) {
		t.Fatalf("read-write grants = %v, must not grant the container root %q", readWrite, wantRoot)
	}
	for _, want := range []string{wantGit, wantRoot} {
		if !contains(readOnly, want) {
			t.Fatalf("read-only grants = %v, want %q", readOnly, want)
		}
	}
	for _, want := range []string{canonicalPath(t, srcDir), canonicalPath(t, existing)} {
		if !contains(readWrite, want) {
			t.Fatalf("read-write grants = %v, want the sibling %q", readWrite, want)
		}
	}
	if contains(readWrite, wantGit) || contains(readWrite, canonicalPath(t, gitDir)+"/x") {
		t.Fatalf("read-write grants = %v, must not grant the protected subtree", readWrite)
	}
}

// TestLandlockGrantsNestedProtected 校验保护路径嵌套时只沿保护链下沉：根的其它
// 子项与链上中间目录的其它子项拿到 --rw，链上目录本身与被保护子树只读。
func TestLandlockGrantsNestedProtected(t *testing.T) {
	root := t.TempDir()
	nestedGit := filepath.Join(root, "sub", ".git")
	other := filepath.Join(root, "other")
	libDir := filepath.Join(root, "sub", "lib")
	for _, dir := range []string{nestedGit, other, libDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(nestedGit),
	)
	readOnly, readWrite := LandlockGrants(policy)
	wantRoot := canonicalPath(t, root)
	wantSub := canonicalPath(t, filepath.Join(root, "sub"))
	wantGit := canonicalPath(t, nestedGit)
	// 保护链上的目录（root → sub → .git）全部只读。
	for _, want := range []string{wantRoot, wantSub, wantGit} {
		if !contains(readOnly, want) {
			t.Fatalf("read-only grants = %v, want %q", readOnly, want)
		}
		if contains(readWrite, want) {
			t.Fatalf("read-write grants = %v, must not grant %q", readWrite, want)
		}
	}
	// 兄弟目录（root/other、root/sub/lib）仍然读写。
	for _, want := range []string{canonicalPath(t, other), canonicalPath(t, libDir)} {
		if !contains(readWrite, want) {
			t.Fatalf("read-write grants = %v, want %q", readWrite, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Seatbelt
// ---------------------------------------------------------------------------

// 新契约：read-only 的 profile 只放行设备节点，绝不出现 workspace/tmp 的 subpath。
func TestSeatbeltProfileReadOnlyDeniesWrites(t *testing.T) {
	root := t.TempDir()
	profile := SeatbeltProfile(sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly)))
	for _, want := range []string{"(version 1)", "(allow default)", "(deny file-write*)", `(literal "/dev/null")`, "(deny network*)"} {
		if !strings.Contains(profile, want) {
			t.Fatalf("SeatbeltProfile(read-only) = %q, missing %q", profile, want)
		}
	}
	if subpath := canonicalPath(t, root); strings.Contains(profile, "(subpath "+sbplString(subpath)+")") {
		t.Fatalf("SeatbeltProfile(read-only) = %q, must not grant the workspace %q", profile, subpath)
	}
	if strings.Contains(profile, `(subpath "/tmp")`) {
		t.Fatalf("SeatbeltProfile(read-only) = %q, must not grant /tmp", profile)
	}
}

func TestSeatbeltProfileWorkspaceGrantsRootAndTmpdir(t *testing.T) {
	tempDir := customTempDir(t)
	root := t.TempDir()
	profile := SeatbeltProfile(workspacePolicy(t, root))
	for _, want := range []string{canonicalPath(t, root), tempDir, "/tmp"} {
		if !strings.Contains(profile, "(subpath "+sbplString(want)+")") {
			t.Fatalf("SeatbeltProfile = %q, want subpath %q", profile, want)
		}
	}
}

func TestSeatbeltProfileProtectedDenyComesLast(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(gitDir),
	)
	profile := SeatbeltProfile(policy)
	wantRoot, wantGit := canonicalPath(t, root), canonicalPath(t, gitDir)
	allow := strings.Index(profile, "(allow file-write* (subpath "+sbplString(wantRoot)+")")
	deny := strings.LastIndex(profile, "(deny file-write* (subpath "+sbplString(wantGit)+")")
	if allow < 0 || deny < 0 {
		t.Fatalf("SeatbeltProfile = %q, want both the workspace allow and the protected deny", profile)
	}
	// SBPL 是 last-match-wins：deny 必须在 allow 之后。
	if deny < allow {
		t.Fatalf("SeatbeltProfile = %q, want the protected deny after the workspace allow", profile)
	}
}

func TestSeatbeltProfileNetworkAllowedOmitsDeny(t *testing.T) {
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithNetwork(true),
	)
	if profile := SeatbeltProfile(policy); strings.Contains(profile, "(deny network*)") {
		t.Fatalf("SeatbeltProfile(network allowed) = %q, must not deny network", profile)
	}
}

func TestSeatbeltProfileDangerFullAccessIsEmpty(t *testing.T) {
	if profile := SeatbeltProfile(sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))); profile != "" {
		t.Fatalf("SeatbeltProfile(danger) = %q, want empty", profile)
	}
}

func TestSBPLStringEscapes(t *testing.T) {
	if got, want := sbplString(`/a"b\c`), `"/a\"b\\c"`; got != want {
		t.Fatalf("sbplString = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// 四后端 × 三模式 的渲染矩阵
// ---------------------------------------------------------------------------

// TestProfilesRenderAllBackendsAndModes 是四后端 × 三模式的渲染矩阵：每种组合都
// 断言“该模式的核心承诺”是否被渲染出来，防止某个后端悄悄回到旧契约。
func TestProfilesRenderAllBackendsAndModes(t *testing.T) {
	tempDir := customTempDir(t)
	root := canonicalPath(t, t.TempDir())

	type rendering struct {
		bwrap     []string
		landlockR []string
		landlockW []string
		seatbelt  string
		windows   []string
		windowsE  sandbox.Enforcement
	}
	render := func(policy *sandbox.Policy) rendering {
		t.Helper()
		confined, err := buildWindowsConfinedCommand(`rig.exe`, []string{"cmd.exe"}, policy)
		if err != nil {
			t.Fatalf("buildWindowsConfinedCommand: %v", err)
		}
		readOnly, readWrite := LandlockGrants(policy)
		return rendering{
			bwrap:     BwrapArgs(policy),
			landlockR: readOnly,
			landlockW: readWrite,
			seatbelt:  SeatbeltProfile(policy),
			windows:   confined.Argv,
			windowsE:  confined.Enforcement,
		}
	}

	t.Run("read-only", func(t *testing.T) {
		got := render(sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly)))
		if binds := flagValues(got.bwrap, "--bind"); len(binds) != 0 {
			t.Fatalf("bwrap = %v, read-only must not bind anything", got.bwrap)
		}
		if !contains(got.bwrap, "--remount-ro") {
			t.Fatalf("bwrap = %v, want --remount-ro /dev", got.bwrap)
		}
		if contains(got.landlockW, root) {
			t.Fatalf("landlock rw = %v, read-only must not grant the workspace", got.landlockW)
		}
		if strings.Contains(got.seatbelt, "(subpath "+sbplString(root)+")") {
			t.Fatalf("seatbelt = %q, read-only must not grant the workspace", got.seatbelt)
		}
		if !contains(got.windows, winModeReadOnly) {
			t.Fatalf("windows argv = %v, want --mode read-only", got.windows)
		}
		if got.windowsE != sandbox.EnforcementPartial {
			t.Fatalf("windows Enforcement = %q, want partial", got.windowsE)
		}
	})

	t.Run("workspace-write", func(t *testing.T) {
		got := render(sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite)))
		if !contains(flagValues(got.bwrap, "--bind"), root) {
			t.Fatalf("bwrap = %v, want --bind %q", got.bwrap, root)
		}
		if !contains(got.bwrap, "--unshare-net") {
			t.Fatalf("bwrap = %v, want --unshare-net", got.bwrap)
		}
		if !contains(got.landlockW, root) || !contains(got.landlockW, tempDir) {
			t.Fatalf("landlock rw = %v, want workspace and $TMPDIR", got.landlockW)
		}
		if !strings.Contains(got.seatbelt, "(subpath "+sbplString(root)+")") {
			t.Fatalf("seatbelt = %q, want the workspace subpath", got.seatbelt)
		}
		if !contains(got.windows, winModeWorkspaceWrite) || !contains(flagValues(got.windows, winFlagWorkspace), root) {
			t.Fatalf("windows argv = %v, want --workspace %q --mode workspace-write", got.windows, root)
		}
	})

	t.Run("danger-full-access", func(t *testing.T) {
		got := render(sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess)))
		if got.bwrap != nil || got.landlockR != nil || got.landlockW != nil || got.seatbelt != "" {
			t.Fatalf("danger-full-access rendered a profile: %+v", got)
		}
		if got.windowsE != sandbox.EnforcementNone {
			t.Fatalf("windows Enforcement = %q, want none", got.windowsE)
		}
		if contains(got.windows, HelperArgWindowsACL) {
			t.Fatalf("windows argv = %v, want an unwrapped passthrough", got.windows)
		}
	})
}

// ---------------------------------------------------------------------------
// 其它
// ---------------------------------------------------------------------------

func TestDenialSignatures(t *testing.T) {
	for _, runner := range []string{RunnerBwrap, RunnerLandlock, RunnerSeatbelt, RunnerWindowsACL} {
		if signatures := DenialSignatures(runner); len(signatures) == 0 {
			t.Fatalf("DenialSignatures(%q) is empty", runner)
		}
	}
	if signatures := DenialSignatures("unknown"); signatures != nil {
		t.Fatalf("DenialSignatures(unknown) = %v, want nil", signatures)
	}
}

func TestCustomRunnerWrapsWithBwrapProfile(t *testing.T) {
	runner, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"myns: failed"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi"}, workspacePolicy(t, t.TempDir()))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := confined.Argv[0], "myns"; got != want {
		t.Fatalf("Argv[0] = %q, want %q", got, want)
	}
	if !contains(confined.Argv, "--ro-bind") {
		t.Fatalf("Argv = %v, want bwrap profile args", confined.Argv)
	}
	if sep := indexOf(confined.Argv, "--"); sep < 0 || sep+1 >= len(confined.Argv) || confined.Argv[sep+1] != "/bin/sh" {
		t.Fatalf("Argv = %v, want -- /bin/sh ...", confined.Argv)
	}
	if confined.Enforcement != sandbox.EnforcementFull {
		t.Fatalf("Enforcement = %q, want full", confined.Enforcement)
	}
	// bwrap 的拒绝方言现在包含 seccomp 的 EPERM（connect 被拒）与守护进程 socket
	// 客户端的 "permission denied"；第一条仍是文件系统的只读报错。
	if len(confined.DenialSignatures) != 3 || confined.DenialSignatures[0] != "read-only file system" {
		t.Fatalf("DenialSignatures = %v", confined.DenialSignatures)
	}
}

func TestCustomRunnerDangerFullAccessPassesThrough(t *testing.T) {
	runner, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"myns: failed"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	policy := sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))
	confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := strings.Join(confined.Argv, " "), "/bin/sh -c echo hi"; got != want {
		t.Fatalf("Argv = %q, want %q", got, want)
	}
	// 透传没有被任何 runner 包装。
	if confined.Enforcement != sandbox.EnforcementNone {
		t.Fatalf("Enforcement = %q, want %q", confined.Enforcement, sandbox.EnforcementNone)
	}
}

func TestCustomRunnerRequiresSignatures(t *testing.T) {
	if _, err := New(Config{RunnerCommand: []string{"myns"}}); err == nil {
		t.Fatal("New accepted RunnerCommand without RunnerFailureSignatures")
	}
	if _, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"bad\nline"}}); err == nil {
		t.Fatal("New accepted a multi-line failure signature")
	}
}

func TestCustomRunnerNilPolicyFailsClosed(t *testing.T) {
	runner, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"myns: failed"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runner.Confine([]string{"/bin/sh"}, nil); err == nil {
		t.Fatal("Confine(nil policy) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("Confine(nil policy) error = %v, want ErrSandboxUnavailable", err)
	}
}

func TestCanonicalSplitDropsMissingAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	kept, dropped := canonicalSplit([]string{dir, dir, missing})
	if len(kept) != 1 || kept[0] != canonicalPath(t, dir) {
		t.Fatalf("canonicalSplit kept = %v, want one existing path", kept)
	}
	if len(dropped) != 1 || dropped[0] != missing {
		t.Fatalf("canonicalSplit dropped = %v, want [%q]", dropped, missing)
	}
	if got := canonicalUnique([]string{dir, dir, missing}); len(got) != 1 {
		t.Fatalf("canonicalUnique = %v, want one existing path", got)
	}
}

func TestPlatformTempRootsIncludeOSTemp(t *testing.T) {
	roots := platformTempRoots()
	if len(roots) == 0 {
		t.Fatal("platformTempRoots is empty")
	}
	if runtime.GOOS != "windows" && !contains(roots, "/tmp") {
		t.Fatalf("platformTempRoots = %v, want /tmp", roots)
	}
}

// TestBuildProfileIncludesGrantedWritableRoots 证明运行期授权立即进入 profile：
// 被约束的 agent 申请一个尚不存在的目录后，BuildProfile 必须把它列为可写，否则
// "批准了却仍然写不进"——这正是审计中发现的竞态。
func TestBuildProfileIncludesGrantedWritableRoots(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))

	grantPath := filepath.Join(base, "tool-creds")
	if _, err := policy.AddWritableRoot(grantPath, "the tool keeps credentials here"); err != nil {
		t.Fatalf("AddWritableRoot: %v", err)
	}
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	found := false
	for _, path := range profile.ReadWrite {
		if real, realErr := filepath.EvalSymlinks(grantPath); realErr == nil && path == real {
			found = true
		}
		if path == grantPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("granted root %q missing from ReadWrite %v", grantPath, profile.ReadWrite)
	}
	if _, err := os.Stat(grantPath); err != nil {
		t.Fatalf("granted directory was not created: %v", err)
	}
}

// TestSeatbeltProfileClosesWriteBypassChannels 钉住三条"allow-default 下必须
// 显式关闭"的写绕过通道，以及 pty 的收窄写法。这些规则一旦丢失，Seatbelt 的
// 写围栏会在没有权限提示的情况下被绕过。
func TestSeatbeltProfileClosesWriteBypassChannels(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	profile := SeatbeltProfile(policy)
	for _, want := range []string{
		"(deny system-fcntl (fcntl-command 80 110))",
		"(deny user-preference-write)",
		"(allow pseudo-tty)",
		`(allow file-write* (regex #"^/dev/ttys[0-9]+$"))`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile is missing %q:\n%s", want, profile)
		}
	}
	// 绝不能整目录放行 /dev/pts（等于允许写同用户任意 pty）。
	if strings.Contains(profile, `(subpath "/dev/pts")`) || strings.Contains(profile, `(subpath "/dev")`) {
		t.Fatalf("profile must not grant a /dev subpath:\n%s", profile)
	}
	// 所有 deny 必须排在 allow 之后（SBPL last-match-wins）。
	lastAllow := strings.LastIndex(profile, "(allow ")
	firstWriteDeny := strings.Index(profile, "(deny user-preference-write)")
	if lastAllow == -1 || firstWriteDeny == -1 || firstWriteDeny < lastAllow {
		t.Fatalf("deny rules must come after every allow rule:\n%s", profile)
	}
}

// TestSeatbeltProfileReadOnlyStillGrantsNoWorkspace 确认收窄 pty 之后 read-only
// 仍然不会放行工作区写。
func TestSeatbeltProfileReadOnlyStillGrantsNoWorkspace(t *testing.T) {
	root := t.TempDir()
	profile := SeatbeltProfile(sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly)))
	if strings.Contains(profile, `(subpath "`+root+`")`) {
		t.Fatalf("read-only profile granted the workspace:\n%s", profile)
	}
}

// TestUntrustedExecutableReason 覆盖"沙箱本体可以被替换"的三种情况：可写二进制、
// 世界可写目录、以及落在 agent 工作区里的候选。
func TestUntrustedExecutableReason(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		// Windows 没有 POSIX 的 group/world 位（Go 只映射只读位），"0755 即可信"
		// 这条前提不成立；Windows 上真正有意义的是下面的 cwd 检查。
		if reason := untrustedExecutableReason(bin); reason != "" {
			t.Fatalf("reason = %q, want trusted for a 0755 file in a 0700 dir", reason)
		}
	}

	// Windows 的权限模型没有 POSIX 的 group/world 位（Go 在 Windows 上只映射
	// 只读位），这两条断言只在 POSIX 上有意义。
	if runtime.GOOS != "windows" {
		if err := os.Chmod(bin, 0o777); err != nil {
			t.Fatal(err)
		}
		if reason := untrustedExecutableReason(bin); !strings.Contains(reason, "writable by group or others") {
			t.Fatalf("reason = %q, want the world-writable binary diagnostic", reason)
		}
		if err := os.Chmod(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if reason := untrustedExecutableReason(bin); !strings.Contains(reason, "world-writable") {
			t.Fatalf("reason = %q, want the world-writable directory diagnostic", reason)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// 落在当前工作目录里的候选一律不信任：那是 agent 可写的地方。
	t.Chdir(dir)
	if reason := untrustedExecutableReason(bin); !strings.Contains(reason, "current working directory") {
		t.Fatalf("reason = %q, want the cwd diagnostic", reason)
	}
}
