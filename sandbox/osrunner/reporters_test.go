package osrunner

// Reporter 面、诊断面与导出 API 的单元测试（平台无关）。
//
// 这层很薄但对外可见：`sandbox_status`、rigdoctor 与 `sandbox.Preflight` 都直接读
// Reporter/NetworkReporter 的返回值，所以每个后端的"自我描述"必须是确定的字符串，
// 不能随模式或平台漂移。DSH 对齐后四个后端的网络覆盖度统一是 none（不再渲染网络
// 隔离参数），这三条断言就是那条结论的可执行版本。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestReporterSurfaceSeatbelt 覆盖 Seatbelt 后端的 Reporter 接口。
func TestReporterSurfaceSeatbelt(t *testing.T) {
	runner := &seatbeltRunner{execPath: "/usr/bin/sandbox-exec"}
	if got := runner.RunnerName(); got != RunnerSeatbelt {
		t.Fatalf("RunnerName = %q, want %q", got, RunnerSeatbelt)
	}
	if got := runner.RunnerEnforcement(); got != sandbox.EnforcementFull {
		t.Fatalf("RunnerEnforcement = %q, want full", got)
	}
	if got := runner.RunnerLimits(); got != "" {
		t.Fatalf("RunnerLimits = %q, want empty（Seatbelt 的 file-write* 覆盖是完整的）", got)
	}
	if got := runner.NetworkCoverage(); got != "none" {
		t.Fatalf("NetworkCoverage = %q, want none（DSH 不渲染网络隔离）", got)
	}
	// nil 接收者不能 panic：sandbox.RunnerStatus 会在类型断言失败时按"未知"处理。
	var nilRunner *seatbeltRunner
	if got := nilRunner.NetworkCoverage(); got != "none" {
		t.Fatalf("nil NetworkCoverage = %q, want none", got)
	}
}

// TestLandlockGitDegradeToggle 覆盖那个已无渲染效果的进程级开关（导出 API）。
func TestLandlockGitDegradeToggle(t *testing.T) {
	original := LandlockGitDegradeEnabled()
	t.Cleanup(func() { WithLandlockGitDegrade(original) })
	if !original {
		t.Fatalf("默认值 = %v, want true", original)
	}
	WithLandlockGitDegrade(false)
	if LandlockGitDegradeEnabled() {
		t.Fatal("WithLandlockGitDegrade(false) 之后仍然返回 true")
	}
	WithLandlockGitDegrade(true)
	if !LandlockGitDegradeEnabled() {
		t.Fatal("WithLandlockGitDegrade(true) 之后仍然返回 false")
	}
}

// TestWritableRootsMatchesProfile 断言导出 API 与 Profile 同源。
func TestWritableRootsMatchesProfile(t *testing.T) {
	workspace := t.TempDir()
	for _, mode := range []sandbox.Mode{sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite} {
		policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(mode))
		profile, err := BuildProfile(policy)
		if err != nil || profile == nil {
			t.Fatalf("BuildProfile(%s): %v", mode, err)
		}
		got := WritableRoots(policy)
		if strings.Join(got, "\x00") != strings.Join(profile.ReadWrite, "\x00") {
			t.Fatalf("WritableRoots(%s) = %q, want Profile.ReadWrite %q", mode, got, profile.ReadWrite)
		}
	}
	// danger-full-access 没有渲染，也没有可写面。
	if got := WritableRoots(sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))); got != nil {
		t.Fatalf("WritableRoots(danger-full-access) = %q, want nil", got)
	}
	// nil 策略 fail closed（返回 nil，不 panic）。
	if got := WritableRoots(nil); got != nil {
		t.Fatalf("WritableRoots(nil) = %q, want nil", got)
	}
}

// TestDescribeRuntime 覆盖诊断快照：没有 runner 时 NetworkCoverage 是 none，
// 有 runner 时取后端自述，且"对齐后不再渲染"的字段一律为空。
func TestDescribeRuntime(t *testing.T) {
	empty := DescribeRuntime(nil, nil)
	if empty.NetworkCoverage != "none" {
		t.Fatalf("DescribeRuntime(nil,nil).NetworkCoverage = %q, want none", empty.NetworkCoverage)
	}
	if len(empty.ToolchainCaches) != 0 || len(empty.MaskedSockets) != 0 || empty.PrivateTmpDisabledReason != "" {
		t.Fatalf("空策略的诊断应当全空：%+v", empty)
	}

	workspace := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	report := DescribeRuntime(policy, &seatbeltRunner{execPath: "/usr/bin/sandbox-exec"})
	if report.NetworkCoverage != "none" {
		t.Fatalf("NetworkCoverage = %q, want none（DSH 不治理网络）", report.NetworkCoverage)
	}
	if len(report.ToolchainCaches) != 0 {
		t.Fatalf("ToolchainCaches = %v, want empty（对齐后不授权缓存根）", report.ToolchainCaches)
	}
	if len(report.MissingToolchainCaches) != 0 || len(report.MaskedSockets) != 0 {
		t.Fatalf("对齐后不应再有缓存/socket 项：%+v", report)
	}
}

// TestMissingProtectedDiagnostics 覆盖保护路径的**诊断**层（对齐后它不再有强制语义）。
func TestMissingProtectedDiagnostics(t *testing.T) {
	base := t.TempDir()
	explicit := filepath.Join(base, "declared")
	if err := os.MkdirAll(explicit, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	missing := filepath.Join(explicit, "gone")
	policy := sandbox.New(sandbox.WithRoots(base), sandbox.WithMode(sandbox.ModeWorkspaceWrite))

	profile := &Profile{
		Mode:             sandbox.ModeWorkspaceWrite,
		Protected:        []string{explicit},
		MissingProtected: []string{missing},
	}
	// 策略层（DSH 对齐）已把保护路径降级为"仅记录"（相关 API 已 Deprecated，
	// 本包不再调用），因此这里必然挑不出任何"显式声明"的项。断言这一点，避免
	// 有人误以为保护路径还在被强制。
	if got := requiredMissingProtected(profile, policy); len(got) != 0 {
		t.Fatalf("requiredMissingProtected = %q, want empty（保护路径已降级为仅记录）", got)
	}
	// 路径归属判断本身仍然可用（RuntimeReport 用它过滤诊断项）。
	if !withinAnyPath(missing, []string{explicit}) {
		t.Fatalf("withinAnyPath(%q, [%q]) = false, want true", missing, explicit)
	}
	if withinAnyPath(explicit, []string{missing}) {
		t.Fatal("withinAnyPath 反方向判断错误")
	}
	warning := missingProtectedWarning(profile)
	if !strings.Contains(warning, explicit) || !strings.Contains(warning, "in-process fence") {
		t.Fatalf("missingProtectedWarning = %q，应当指出路径与「只有进程内围栏」", warning)
	}
	if got := missingProtectedWarning(nil); got != "" {
		t.Fatalf("missingProtectedWarning(nil) = %q, want empty", got)
	}
	if got := missingProtectedWarning(&Profile{}); got != "" {
		t.Fatalf("没有保护项的 profile 不应产生警告：%q", got)
	}
}

// TestExportToolchainCacheRootsFor 覆盖导出诊断函数：纯函数、无副作用、无重复。
func TestExportToolchainCacheRootsFor(t *testing.T) {
	home := t.TempDir()
	env := func(key string) string {
		switch key {
		case "HOME", "USERPROFILE":
			return home
		case "LOCALAPPDATA":
			return filepath.Join(home, "Local")
		case "APPDATA":
			return filepath.Join(home, "Roaming")
		}
		return ""
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		roots := ExportToolchainCacheRootsFor(goos, env)
		if len(roots) == 0 {
			t.Fatalf("%s: 清单不该为空", goos)
		}
		seen := make(map[string]struct{}, len(roots))
		for _, root := range roots {
			if root.Root == "" {
				t.Fatalf("%s: 出现空路径", goos)
			}
			if _, dup := seen[root.Root]; dup {
				t.Fatalf("%s: 重复项 %q", goos, root.Root)
			}
			seen[root.Root] = struct{}{}
		}
	}
	// 没有任何环境变量时返回 nil（不猜路径）。
	if got := ExportToolchainCacheRootsFor("linux", func(string) string { return "" }); got != nil {
		t.Fatalf("空环境 = %v, want nil", got)
	}
	// 这是纯函数：不能在宿主上创建任何目录。
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("read home: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ExportToolchainCacheRootsFor 产生了副作用：%v", entries)
	}
}

// TestPathWithinOS 覆盖显式平台语义的路径包含判断（纯函数）。
func TestPathWithinOS(t *testing.T) {
	cases := []struct {
		goos, root, candidate string
		want                  bool
	}{
		{"linux", "/a/b", "/a/b", true},
		{"linux", "/a/b", "/a/b/c", true},
		{"linux", "/a/b", "/a/bc", false},
		{"linux", "/a/b", "/a/B/c", false},
		{"windows", "/a/b", "/a/B/c", true},
		{"darwin", "/a/b", "/A/B", true},
		{"windows", `C:\ws`, `C:\ws\x`, true},
		{"linux", "/", "/anything", true},
		{"linux", "", "/x", false},
		{"linux", "/a", "", false},
	}
	for _, tc := range cases {
		if got := pathWithinOS(tc.goos, tc.root, tc.candidate); got != tc.want {
			t.Fatalf("pathWithinOS(%q,%q,%q) = %v, want %v", tc.goos, tc.root, tc.candidate, got, tc.want)
		}
	}
}

// TestDenialSignaturesAllBackends 覆盖拒绝方言表：四个后端 + 未知后端。
func TestDenialSignaturesAllBackends(t *testing.T) {
	for _, runner := range []string{RunnerBwrap, RunnerLandlock, RunnerSeatbelt, RunnerWindowsACL} {
		sigs := DenialSignatures(runner)
		if len(sigs) == 0 {
			t.Fatalf("%s: 拒绝方言为空", runner)
		}
		for _, sig := range sigs {
			if strings.TrimSpace(sig) == "" {
				t.Fatalf("%s: 出现空签名", runner)
			}
		}
	}
	if got := DenialSignatures("unknown-backend"); got != nil {
		t.Fatalf("未知后端的方言 = %q, want nil", got)
	}
}

// TestNewRejectsBadRunnerConfig 覆盖 New 的配置校验分支。
func TestNewRejectsBadRunnerConfig(t *testing.T) {
	if _, err := New(Config{RunnerCommand: []string{"/usr/bin/bwrap"}}); err == nil {
		t.Fatal("RunnerCommand 缺 RunnerFailureSignatures 时应当报错")
	}
	if _, err := New(Config{
		RunnerCommand:           []string{"/usr/bin/bwrap"},
		RunnerFailureSignatures: []string{"a\nb"},
	}); err == nil {
		t.Fatal("多行失败签名应当被拒绝")
	}
	if _, err := New(Config{
		RunnerCommand:           []string{"/usr/bin/bwrap"},
		RunnerFailureSignatures: []string{""},
	}); err == nil {
		t.Fatal("空失败签名应当被拒绝")
	}
	if _, err := New(Config{
		RunnerCommand:           []string{"/usr/bin/bwrap"},
		RunnerFailureSignatures: []string{"permission denied"},
	}); err != nil {
		t.Fatalf("合法配置被拒：%v", err)
	}
}

// TestCustomRunnerConfineShapes 覆盖 customRunner 的三种出口：透传、包装、nil 策略。
func TestCustomRunnerConfineShapes(t *testing.T) {
	runner, err := New(Config{
		RunnerCommand:           []string{"/usr/bin/bwrap"},
		RunnerFailureSignatures: []string{"permission denied"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	workspace := t.TempDir()
	confined, err := runner.Confine([]string{"/bin/true"}, sandbox.New(sandbox.WithRoots(workspace), sandbox.WithMode(sandbox.ModeWorkspaceWrite)))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if confined.Argv[0] != "/usr/bin/bwrap" {
		t.Fatalf("Argv = %q, want 以自定义 runner 开头", confined.Argv)
	}
	if !contains(confined.Argv, "--ro-bind") {
		t.Fatalf("Argv = %q, want bwrap profile 参数", confined.Argv)
	}
	if confined.Enforcement != sandbox.EnforcementFull {
		t.Fatalf("Enforcement = %q, want full", confined.Enforcement)
	}
	// danger-full-access → 透传，Enforcement=None。
	passed, err := runner.Confine([]string{"/bin/true"}, sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess)))
	if err != nil {
		t.Fatalf("Confine(danger-full-access): %v", err)
	}
	if strings.Join(passed.Argv, " ") != "/bin/true" || passed.Enforcement != sandbox.EnforcementNone {
		t.Fatalf("透传结果 = %+v, want /bin/true + none", passed)
	}
	// nil 策略 fail closed。
	if _, err := runner.Confine([]string{"/bin/true"}, nil); err == nil {
		t.Fatal("nil 策略应当 fail closed")
	}
}

// TestPlatformTempRootsCanonical 覆盖临时根列表：/tmp 与 os.TempDir() 去重后保序。
func TestPlatformTempRootsCanonical(t *testing.T) {
	roots := platformTempRoots()
	if len(roots) == 0 {
		t.Fatal("platformTempRoots 为空")
	}
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			t.Fatalf("临时根 %q 不是绝对路径", root)
		}
		if _, dup := seen[root]; dup {
			t.Fatalf("临时根重复：%q", root)
		}
		seen[root] = struct{}{}
	}
	if runtime.GOOS == "windows" {
		// Windows 只有 os.TempDir()（/tmp 不存在，会被 canonical 过滤掉）。
		if len(roots) != 1 {
			t.Fatalf("Windows 的临时根 = %q, want 只有 os.TempDir()", roots)
		}
	}
}
