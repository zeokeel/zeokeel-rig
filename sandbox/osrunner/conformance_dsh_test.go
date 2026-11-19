package osrunner

// DSH 对齐的一致性门禁（与平台无关，任何宿主都能跑）。
//
// 这些用例把"本包的渲染结果 == DSH（sandbox-local）的渲染结果"钉死成**逐 token /
// 逐字符**断言。它不是"行为看起来差不多"的回归，而是一份可执行的规格：
//
//	bwrap read-only       --ro-bind / /  --dev /dev  --unshare-pid  --proc /proc  --die-with-parent
//	bwrap workspace-write 上面 + --tmpfs /tmp  --bind <workspaceRoot> <workspaceRoot>
//	Landlock              --ro /  --rw /dev/null      （workspace-write 再 --rw /tmp --rw <ws>）
//	Seatbelt read-only    (version 1) (allow default) (deny file-write*) (allow file-write* (literal "/dev/null"))
//	Seatbelt workspace-w  上面 + (allow file-write* (subpath "<ws>") (subpath "<tmp>") (subpath "<tmpdir>"))
//	Windows               read-only 不授权任何根；workspace-write 授权 ws + 本次私有 temp
//
// 断言风格：失败信息里同时给出 want 与 got 的 token 序列，避免"少了什么"要靠猜。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// dshParityRoots 返回一对确定的工作区/临时目录，供渲染断言使用。
//
// TMPDIR 必须显式设置：否则 platformTempRoots() 会取宿主的 os.tmpdir()，断言就
// 变成"跟宿主环境有关"。设置成 /tmp 之外的目录后，`/tmp` 与 `<tmpdir>` 是两个
// 不同的 subpath，正好验证 DSH 那个"两个临时根都放行"的形状。
func dshParityRoots(t *testing.T) (workspace, tmpdir string) {
	t.Helper()
	base := t.TempDir()
	workspace = filepath.Join(base, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	tmpdir = filepath.Join(base, "tmpdir")
	if err := os.MkdirAll(tmpdir, 0o755); err != nil {
		t.Fatalf("mkdir tmpdir: %v", err)
	}
	t.Setenv("TMPDIR", tmpdir)
	return workspace, tmpdir
}

// TestDSHParityBwrapArgsReadOnly 断言 read-only 的 bwrap 参数**完全等于** DSH 的
// 10 个 token（顺序也一致）。
func TestDSHParityBwrapArgsReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bwrap 是 Linux 后端；Windows 上不渲染这些参数")
	}
	workspace, _ := dshParityRoots(t)
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeReadOnly))
	got := BwrapArgs(policy)
	want := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("BwrapArgs(read-only)\n got = %q\nwant = %q", got, want)
	}
}

// TestDSHParityBwrapArgsWorkspaceWrite 断言 workspace-write = 10 个基础 token +
// `--tmpfs /tmp` + `--bind <ws> <ws>`。
func TestDSHParityBwrapArgsWorkspaceWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bwrap 是 Linux 后端；Windows 上不渲染这些参数")
	}
	workspace, _ := dshParityRoots(t)
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	got := BwrapArgs(policy)
	want := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
		"--tmpfs", "/tmp",
		"--bind", workspace, workspace,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("BwrapArgs(workspace-write)\n got = %q\nwant = %q", got, want)
	}
	// 只有工作区被 bind：临时目录由 --tmpfs 承载，宿主 /tmp /var/tmp 不再 bind。
	binds := 0
	for index, arg := range got {
		if arg != "--bind" {
			continue
		}
		binds++
		if index+2 < len(got) && got[index+1] != workspace {
			t.Fatalf("BwrapArgs 出现了非工作区的 --bind 目标：%q", got[index+1])
		}
	}
	if binds != 1 {
		t.Fatalf("BwrapArgs 的 --bind 数量 = %d, want 1（只 bind 工作区）: %q", binds, got)
	}
}

// TestDSHParityBwrapArgsDangerFullAccess 断言 danger-full-access 不渲染任何参数。
func TestDSHParityBwrapArgsDangerFullAccess(t *testing.T) {
	policy := sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))
	if got := BwrapArgs(policy); len(got) != 0 {
		t.Fatalf("BwrapArgs(danger-full-access) = %q, want empty", got)
	}
	if got := SeatbeltProfile(policy); got != "" {
		t.Fatalf("SeatbeltProfile(danger-full-access) = %q, want empty", got)
	}
}

// TestDSHParityBwrapArgsNoRigOnlyTokens 断言 DSH 没有的加固 token 一个都不再出现。
//
// 这条是"删除清单"的可执行版本：任何人把它们加回来，不管加在哪个模式，都会红。
func TestDSHParityBwrapArgsNoRigOnlyTokens(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bwrap 是 Linux 后端")
	}
	workspace, _ := dshParityRoots(t)
	removed := []string{"--cap-drop", "--remount-ro", "--unshare-net", "--seccomp", "--unshare-user", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup"}
	for _, mode := range []sandbox.Mode{sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite} {
		policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(mode), sandbox.WithNetwork(false))
		args := BwrapArgs(policy)
		for _, token := range removed {
			for _, arg := range args {
				if arg == token {
					t.Fatalf("mode=%s: BwrapArgs 里仍有 DSH 之外的 token %q: %q", mode, token, args)
				}
			}
		}
		// AllowNetwork=false 不得改变渲染结果（DSH 不按网络生成参数）。
		withNet := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(mode), sandbox.WithNetwork(true))
		if strings.Join(BwrapArgs(withNet), "\x00") != strings.Join(args, "\x00") {
			t.Fatalf("mode=%s: AllowNetwork 影响了 BwrapArgs 渲染：%q vs %q", mode, BwrapArgs(withNet), args)
		}
	}
}

func TestDSHParityWindowsGrantShape(t *testing.T) {
	readOnly, err := windowsGrantShape(winModeReadOnly)
	if err != nil {
		t.Fatalf("windowsGrantShape(read-only): %v", err)
	}
	if readOnly.WorkspaceWrite || readOnly.TempWrite {
		t.Fatalf("read-only 形状 = %+v, want 无工作区授权、无 temp", readOnly)
	}

	workspaceWrite, err := windowsGrantShape(winModeWorkspaceWrite)
	if err != nil {
		t.Fatalf("windowsGrantShape(workspace-write): %v", err)
	}
	if !workspaceWrite.WorkspaceWrite || !workspaceWrite.TempWrite {
		t.Fatalf("workspace-write 形状 = %+v, want 工作区授权 + 私有 temp", workspaceWrite)
	}
}

// TestDSHParityWindowsSurfaceIsWorkspaceOnly 断言 Windows 的授权面只由"策略可写根"
// 构成：没有缓存根、没有保护路径（--protect 不再下发）。
//
// 注意：保护路径在 DSH 对齐后连**表达**都做不到了（`AddProtectedPaths` 已
// Deprecated 且只记录），所以这里只能断言"即使工作区里有 .git，授权面也只有工作区根"。
func TestDSHParityWindowsSurfaceIsWorkspaceOnly(t *testing.T) {
	workspace, _ := dshParityRoots(t)
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	roots, protects := winWritableSurface(policy)
	if len(protects) != 0 {
		t.Fatalf("winWritableSurface protects = %q, want empty（DSH 没有 --protect）", protects)
	}
	if len(roots) != 1 {
		t.Fatalf("winWritableSurface roots = %q, want exactly the workspace root", roots)
	}
	if !samePathForTest(roots[0], workspace) {
		t.Fatalf("winWritableSurface roots[0] = %q, want %q", roots[0], workspace)
	}

	confined, err := buildWindowsConfinedCommand("rig.exe", []string{"cmd.exe", "/c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	if got := windowsFlagValues(confined.Argv, winFlagProtect); len(got) != 0 {
		t.Fatalf("Windows argv 里仍有 --protect：%q", confined.Argv)
	}
	if got := windowsFlagValues(confined.Argv, winFlagWorkspace); len(got) != 1 {
		t.Fatalf("Windows argv 的 --workspace = %q, want 1 个（只有工作区）", got)
	}
}

// TestDSHParityNoHostSideEffects 断言 Confine 不再在宿主上创建任何目录。
//
// 早期实现会在 HOME 里 mkdir 出 ~/.cache、~/.cargo 等十个目录（连 read-only 模式
// 也建）。对齐后这条必须永远为假：CONFINE 只做 argv 渲染。
func TestDSHParityNoHostSideEffects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX 路径断言；Windows 侧的对应项由 TestDSHParityWindowsSurfaceIsWorkspaceOnly 覆盖")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	workspace := t.TempDir()

	for _, mode := range []sandbox.Mode{sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite} {
		policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(mode))
		// 各后端的 Confine 都是纯渲染：任何一侧写盘都会在这里暴露。
		// （Landlock 的同类断言在 conformance_dsh_linux_test.go：landlockRunner 是
		// Linux 专属类型，不能出现在无 build tag 的文件里。）
		if runner, err := New(Config{RunnerCommand: []string{"/usr/bin/bwrap"}, RunnerFailureSignatures: []string{"permission denied"}}); err == nil {
			if _, err := runner.Confine([]string{"/bin/true"}, policy); err != nil {
				t.Fatalf("custom runner Confine(%s): %v", mode, err)
			}
		}
		if _, err := (&seatbeltRunner{execPath: "/usr/bin/sandbox-exec"}).Confine([]string{"/bin/true"}, policy); err != nil {
			t.Fatalf("seatbelt Confine(%s): %v", mode, err)
		}
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("read home: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("Confine 在宿主 HOME 里创建了目录 %v（对齐 DSH 后不应有任何副作用）", names)
	}
}

// TestDSHParityProfileFieldsAreDiagnosticOnly 断言 Profile 里"对齐后不再渲染"的字段
// 确实不再携带会误导后端的数据。
func TestDSHParityProfileFieldsAreDiagnosticOnly(t *testing.T) {
	workspace, _ := dshParityRoots(t)
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if profile.PrivateTmp {
		t.Fatal("PrivateTmp = true，want false（私有 /tmp 由 BwrapArgs 渲染，不由字段驱动）")
	}
	if len(profile.ToolchainCaches) != 0 || len(profile.MissingToolchainCaches) != 0 {
		t.Fatalf("工具链缓存字段必须为空：caches=%v missing=%q", profile.ToolchainCaches, profile.MissingToolchainCaches)
	}
	if len(profile.MaskedSockets) != 0 {
		t.Fatalf("MaskedSockets = %q, want empty（不再遮蔽 daemon socket）", profile.MaskedSockets)
	}
	if profile.TmpDisabledReason != "" {
		t.Fatalf("TmpDisabledReason = %q, want empty", profile.TmpDisabledReason)
	}
	// ReadWrite 只能由"工作区 + 平台临时目录 + /dev/null"构成：这里不硬编码
	// `/tmp`、`/` 之类的 unix 字符串（Windows 上是 `C:\\...`），而是用被测代码
	// 自己的来源推导期望值——平台无关的断言才能在三平台 CI 上跑。
	allowed := map[string]bool{}
	for _, candidate := range append([]string{"/dev/null"}, platformTempRoots()...) {
		allowed[strings.ToLower(filepath.ToSlash(candidate))] = true
	}
	for _, path := range profile.ReadWrite {
		if samePathForTest(path, workspace) {
			continue
		}
		if !allowed[strings.ToLower(filepath.ToSlash(path))] {
			t.Fatalf("ReadWrite 里出现了 DSH 之外的可写面 %q: %q", path, profile.ReadWrite)
		}
	}
	if !contains(profile.ReadWrite, workspace) {
		t.Fatalf("ReadWrite = %q, want it to contain the workspace %q", profile.ReadWrite, workspace)
	}
}
