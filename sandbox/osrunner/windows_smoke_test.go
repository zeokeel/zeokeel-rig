//go:build windows

package osrunner

// Windows 真实场景冒烟测试（对齐 Codex 的 windows-sandbox-rs/sandbox_smoketests.py）。
//
// 与 windows_runtime_test.go 的区别：那些用例直接调用内部 helper 计划，验证的是
// 后端实现；这里的每条用例都走**产品真实路径**——`sandbox.Policy` → `Runner.Confine`
// → 真的 spawn `cmd.exe` → 把 stderr 喂给 `sandbox.DetectDenialWithExit`。因此它验证
// 的是"用户看到的那一层"：命令被拒了吗、归因对不对、提示有没有把模型带偏。
//
// 门控：只在真机 + `RIG_WINDOWS_SANDBOX_TEST=1` 时执行（与 windows_runtime_test.go
// 一致）；没有 ACL 后端的环境 skip。每条用例都把**实际观测**（退出码、归因类别、
// 原始 stderr 片段）写进日志——失败信息里贴原文，要的是事实而不是好看。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/procattr"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// smokeRunner 懒加载一次（New 会做功能探测，逐用例探测会让整包慢一倍）。
var smokeRunner struct {
	once   sync.Once
	runner sandbox.Runner
	err    error
}

// requireWindowsSmoke 在非真机/未显式开启的环境里跳过。
func requireWindowsSmoke(t *testing.T) {
	t.Helper()
	if os.Getenv("RIG_WINDOWS_SANDBOX_TEST") != "1" {
		t.Skip("set RIG_WINDOWS_SANDBOX_TEST=1 to run the real Windows sandbox smoke tests")
	}
	if err := probeWindowsACL(); err != nil {
		t.Skipf("windows ACL backend unavailable on this host: %v", err)
	}
}

// smokeRunnerInstance 返回共享的 runner 实例。
func smokeRunnerInstance(t *testing.T) sandbox.Runner {
	t.Helper()
	smokeRunner.once.Do(func() {
		smokeRunner.runner, smokeRunner.err = New(Config{})
	})
	if smokeRunner.err != nil {
		t.Skipf("osrunner.New: %v", smokeRunner.err)
	}
	return smokeRunner.runner
}

// smokeObservation 是一次冒烟执行的实际观测。
type smokeObservation struct {
	// exitCode 是子进程退出码（0 表示命令自认成功）。
	exitCode int
	// output 是合并后的 stdout+stderr（原样保留，失败信息里贴它）。
	output string
	// denial 是喂给 sandbox.DetectDenialWithExit 之后的归因（可能为 nil）。
	denial *sandbox.Denial
	// enforcement 是 runner 对本次执行的覆盖程度。
	enforcement sandbox.Enforcement
}

// observe 是"真的跑一次受沙箱约束的命令"的唯一入口。
//
// 它走产品路径：Confine → spawn → 观测 → 归因。错误处理遵循"事实优先"：
// 拿不到退出码就是 Fatal（那不是被测行为），拿到非零退出码是正常观测结果。
func observe(t *testing.T, policy *sandbox.Policy, argv []string) smokeObservation {
	t.Helper()
	runner := smokeRunnerInstance(t)
	confined, err := runner.Confine(argv, policy)
	if err != nil {
		t.Fatalf("Confine(%v): %v", argv, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, confined.Argv[0], confined.Argv[1:]...)
	command.Env = os.Environ()
	procattr.HideWindow(command)
	raw, runErr := command.CombinedOutput()
	code := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("spawn %v: %v (output: %s)", confined.Argv, runErr, snippet(string(raw)))
		}
		code = exitErr.ExitCode()
	}
	observation := smokeObservation{
		exitCode:    code,
		output:      string(raw),
		enforcement: confined.Enforcement,
	}
	observation.denial = sandbox.DetectDenialWithExit(observation.output, code, confined.DenialSignatures, policy, confined.Enforcement)
	return observation
}

// snippet 裁剪输出，便于放进日志/失败信息。
func snippet(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "(no output)"
	}
	if len(text) > 400 {
		return text[:400] + "…"
	}
	return text
}

// logObservation 把实际观测写进用例日志（五条用例共用的"证据格式"）。
func logObservation(t *testing.T, label string, observation smokeObservation) {
	t.Helper()
	kind := "none"
	if observation.denial != nil {
		kind = string(observation.denial.Kind)
		if observation.denial.Resource != "" {
			kind += "/" + observation.denial.Resource
		}
	}
	t.Logf("%s: exit=%d denial=%s enforcement=%s stderr=%q",
		label, observation.exitCode, kind, observation.enforcement, snippet(observation.output))
}

// smokeWorkspace 建一个工作区 + workspace-write 策略。
func smokeWorkspace(t *testing.T) (string, *sandbox.Policy) {
	t.Helper()
	workspace := t.TempDir()
	policy := sandbox.New(
		sandbox.WithRoots(workspace),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	return workspace, policy
}

// smokeWrite 用 cmd.exe 的重定向写一个文件（重定向必须写进同一个命令字符串）。
func smokeWrite(shell, target string, content string) []string {
	return []string{shell, "/c", "echo " + content + "> " + quoteWindowsPath(target)}
}

// TestWindowsSmokeWriteOutsideWorkspaceIsDenied 是冒烟 1：工作区外写入被拒，
// 且归因是**路径类**、提示里带上可写面。
func TestWindowsSmokeWriteOutsideWorkspaceIsDenied(t *testing.T) {
	requireWindowsSmoke(t)
	workspace, policy := smokeWorkspace(t)
	shell := winShellCommand()

	// 用 %USERPROFILE% 下的专属子目录（真实用户日志里就是 ~/.dws 之类的位置），
	// 用例结束尽力清理——只有"没被拦住"时才会真的存在。
	profile := os.Getenv("USERPROFILE")
	if profile == "" {
		t.Skip("USERPROFILE is empty; cannot build an outside-workspace target")
	}
	outsideDir := filepath.Join(profile, fmt.Sprintf(".rigsmoke-%d", os.Getpid()))
	target := filepath.Join(outsideDir, "x.txt")
	t.Cleanup(func() { _ = os.RemoveAll(outsideDir) })

	observation := observe(t, policy, smokeWrite(shell, target, "denied"))
	logObservation(t, "outside write", observation)

	if observation.exitCode == 0 {
		t.Fatalf("写入工作区外**未被拦截**（exit 0，文件存在=%v）：这是真实的沙箱漏洞，不是测试问题。stderr=%q",
			fileExists(target), snippet(observation.output))
	}
	if fileExists(target) {
		t.Fatalf("命令声称失败但文件被创建了：%q（stderr=%q）", target, snippet(observation.output))
	}
	if observation.denial == nil {
		t.Fatalf("未能归因：stderr=%q", snippet(observation.output))
	}
	if observation.denial.Kind != sandbox.DenialPath {
		t.Fatalf("Kind = %q, want path（这是路径类拒绝）; stderr=%q", observation.denial.Kind, snippet(observation.output))
	}
	note := observation.denial.Note()
	if !strings.Contains(note, "workspace roots: ") || !strings.Contains(note, workspace) {
		t.Fatalf("归因里没有可写面提示（workspace roots）:\n%s", note)
	}
}

// TestWindowsSmokeRegistryWriteIsDeniedOrReported 是冒烟 2：注册表写入。
//
// 期望是**拒绝**，并且 `DetectDenial` 归类成 DenialRegistry；如果真机上实际没拦住，
// 用例**如实记录"未拦截"并 t.Log**（不伪装成通过，也不把宿主能力问题算成代码回归）。
func TestWindowsSmokeRegistryWriteIsDeniedOrReported(t *testing.T) {
	requireWindowsSmoke(t)
	_, policy := smokeWorkspace(t)
	shell := winShellCommand()
	key := `HKCU\Software\RigSmoke`
	argv := []string{shell, "/c", `reg add "` + key + `" /v t /d 1 /f`}

	observation := observe(t, policy, argv)
	logObservation(t, "registry write", observation)

	if observation.exitCode == 0 {
		// 事实：没拦住。清理现场后如实记录（这是"我们想知道的事实"，不是通过）。
		t.Cleanup(func() {
			cleanup := exec.Command(shell, "/c", `reg delete "`+key+`" /f`)
			_ = cleanup.Run()
		})
		t.Logf("实测结果：注册表写入**未拦截**（restricted token + ACL 后端不治理 HKCU）。"+
			"这类写入无法用可写目录救济，归因层必须把 tip 指向提权；stderr=%q", snippet(observation.output))
		return
	}
	if observation.denial == nil {
		t.Fatalf("注册表写入被拒（exit %d）但没有归因：stderr=%q", observation.exitCode, snippet(observation.output))
	}
	if observation.denial.Kind != sandbox.DenialRegistry {
		t.Fatalf("Kind = %q, want registry; stderr=%q", observation.denial.Kind, snippet(observation.output))
	}
	note := observation.denial.Note()
	if !strings.Contains(note, "NON-FILESYSTEM resource") {
		t.Fatalf("归因没有说明这是非文件系统资源:\n%s", note)
	}
	// 处置顺序：非路径资源必须**先**指向提权（或"让用户开提权"），重定向只能排后面。
	// 具体措辞由本组合可用的通道决定（Options.HasEscalation/HasGrantTool），因此这里
	// 只断言顺序不变量，不断言某一句话。
	steps := observation.denial.NextSteps()
	if len(steps) == 0 {
		t.Fatal("NextSteps 为空：注册表拒绝必须给出处置顺序")
	}
	first := steps[0]
	if !strings.Contains(first, "not a path") && !strings.Contains(first, "without the sandbox") {
		t.Fatalf("NextSteps[0] = %q, want the non-path/escalation step first", first)
	}
	if strings.Contains(first, "redirect") {
		t.Fatalf("NextSteps[0] = %q, want redirect AFTER escalation for registry denials", first)
	}
}

// TestWindowsSmokeWorkspaceWriteSucceeds 是冒烟 3：对照组，工作区内写入必须成功。
func TestWindowsSmokeWorkspaceWriteSucceeds(t *testing.T) {
	requireWindowsSmoke(t)
	workspace, policy := smokeWorkspace(t)
	shell := winShellCommand()
	target := filepath.Join(workspace, "inside.txt")

	observation := observe(t, policy, smokeWrite(shell, target, "inside"))
	logObservation(t, "workspace write", observation)

	if observation.exitCode != 0 {
		t.Fatalf("工作区内写入失败（exit %d）：stderr=%q", observation.exitCode, snippet(observation.output))
	}
	if !fileExists(target) {
		t.Fatalf("工作区内写入没有落盘：%q（stderr=%q）", target, snippet(observation.output))
	}
	if observation.denial != nil {
		t.Fatalf("成功写入却带归因：%+v", observation.denial)
	}
}

// TestWindowsSmokeTempIsWritable 是冒烟 4：workspace-write 的既定承诺——%TEMP% 可写。
func TestWindowsSmokeTempIsWritable(t *testing.T) {
	requireWindowsSmoke(t)
	_, policy := smokeWorkspace(t)
	shell := winShellCommand()
	tempDir := os.Getenv("TEMP")
	if tempDir == "" {
		t.Skip("TEMP is empty on this host")
	}
	dir, err := os.MkdirTemp(tempDir, "rigsmoke-")
	if err != nil {
		t.Skipf("cannot create a %%TEMP%% fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	target := filepath.Join(dir, "temp.txt")

	observation := observe(t, policy, smokeWrite(shell, target, "temp"))
	logObservation(t, "temp write", observation)

	if observation.exitCode != 0 {
		t.Fatalf("workspace-write 模式下 %%TEMP%% 不可写（exit %d）：stderr=%q", observation.exitCode, snippet(observation.output))
	}
	if !fileExists(target) {
		t.Fatalf("%%TEMP%% 写入没有落盘：%q", target)
	}
}

// TestWindowsSmokeProtectedPathIsCaseInsensitive 是冒烟 5：保护路径的大小写变体。
//
// Codex 在这一点上有已知漏判（`.GiT` 绕过）；我们已修过，用它当回归：命令写
// `.GIT\config`，Windows 上会被解析到受保护的 `.git`，必须被拒。
func TestWindowsSmokeProtectedPathIsCaseInsensitive(t *testing.T) {
	requireWindowsSmoke(t)
	workspace := t.TempDir()
	gitDir := filepath.Join(workspace, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatalf("write .git/config: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(workspace),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(gitDir),
	)
	shell := winShellCommand()
	target := filepath.Join(workspace, ".GIT", "config")

	observation := observe(t, policy, smokeWrite(shell, target, "pwned"))
	logObservation(t, "protected .GIT write", observation)

	if observation.exitCode == 0 {
		content, _ := os.ReadFile(filepath.Join(gitDir, "config"))
		t.Fatalf("`.GIT` 大小写变体绕过了保护路径（exit 0）：.git/config 现在=%q；stderr=%q",
			snippet(string(content)), snippet(observation.output))
	}
	content, _ := os.ReadFile(filepath.Join(gitDir, "config"))
	if strings.Contains(string(content), "pwned") {
		t.Fatalf(".git/config 是被写入了（只是命令报错）：%q", snippet(string(content)))
	}
}

// fileExists 报告路径是否存在。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
