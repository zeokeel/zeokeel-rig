package builtin_test

// Audit fixes RG-05 and RG-03 (2026-11 ZeoKeel security/usability audit):
//
//   - RG-05: the bash/pwsh model-facing description must follow the actual
//     composition. Without a runner the historical "not an OS sandbox"
//     sentence stays; with a runner injected the description must say that an
//     OS sandbox confines file effects and the approval gate sits on top.
//   - RG-03 (upgraded): when a confined command's merged output matches one of
//     the runner's DenialSignatures, the tool result gains the structured,
//     actionable "[sandbox: denied ...]" note (denied path, matched dialect,
//     writable roots, next steps) so the model can tell an OS sandbox denial
//     apart from the command failing on its own.
//
// The exit code is deliberately NOT part of the condition any more: CLIs often
// wrap a failed write into their own error text and still exit 0, or print the
// denial while the wrapper "succeeds". Only results that do not match a
// dialect stay byte-identical, which keeps the successful-and-clean path
// pinned to the old bytes.
//
// The four platform dialects (bwrap/Landlock/Seatbelt/Windows ACL, including
// the Chinese Windows variant) are exercised through the fake runner, so the
// matching logic is tested platform-independently on Linux.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// wantRunnerBoundary 是挂接 runner 后 bash/pwsh 描述必须包含的准确边界句
// （模型可见文案，逐字钉住以保持稳定可测试）。
const wantRunnerBoundary = "This tool runs under an OS sandbox runner that confines file effects according to the sandbox policy (danger-full-access runs unconfined); the approval gate applies on top of it."

// sandboxNotePrefix 是结构化沙箱拒绝说明的稳定前缀（Denial.Summary 的第一行）。
const sandboxNotePrefix = "[sandbox: denied "

// denialRunner 返回一个 fake runner：Confine 报告给定拒绝方言，并把 argv 改写
// 为「向 stderr 打印 emitted 并以 exitCode 退出」的 /bin/sh 命令。emitted 不得
// 包含单引号。
func denialRunner(signatures []string, emitted string, exitCode int) *fakeRunner {
	return &fakeRunner{
		signatures: signatures,
		rewrite: func([]string) ([]string, error) {
			return []string{"/bin/sh", "-c", fmt.Sprintf("echo '%s' >&2; exit %d", emitted, exitCode)}, nil
		},
	}
}

// requireSandboxNote 断言结果文本携带匹配 dialect 的结构化归因说明：首行是
// Denial.Summary（被拒目标、命中方言、模式与 enforcement），后随可写面与
// 通用处置顺序。
func requireSandboxNote(t *testing.T, content, dialect string, enforcement sandbox.Enforcement) {
	t.Helper()
	requireContains(t, content,
		sandboxNotePrefix,
		fmt.Sprintf("matched %q", dialect),
		"mode workspace-write",
		"enforcement "+string(enforcement),
		"writable root",
	)
}

// requireNoteTerminates 断言归因说明位于结果末尾（而不是被后续文案盖住）。
func requireNoteTerminates(t *testing.T, content string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "sandbox") {
		t.Fatalf("attribution must terminate the result, last line = %q:\n%s", last, content)
	}
}

// --- RG-05: description follows the runner injection -----------------------

// TestBashDescriptionWithoutRunner 证明未挂接 runner 时（遗留与策略两种模式）
// bash 描述保留原「not an OS sandbox」边界句。
func TestBashDescriptionWithoutRunner(t *testing.T) {
	legacy := builtin.NewShell(t.TempDir(), time.Second)
	requireContains(t, legacy.Description(), "This tool is not an OS sandbox: the approval gate is the boundary.")
	requireNotContains(t, legacy.Description(), "runs under an OS sandbox runner")

	_, policy := newRunnerRoot(t, "echo")
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy))
	requireContains(t, tools[builtin.ToolBash].Description(), "This tool is not an OS sandbox: the approval gate is the boundary.")
	requireNotContains(t, tools[builtin.ToolBash].Description(), "runs under an OS sandbox runner")
}

// TestBashDescriptionWithRunner 证明挂接 runner 后 bash 描述改写为准确的
// OS 沙箱边界句，且不再声称「not an OS sandbox」；描述稳定（两次调用一致）。
func TestBashDescriptionWithRunner(t *testing.T) {
	_, policy := newRunnerRoot(t, "echo")
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(&fakeRunner{})))
	description := tools[builtin.ToolBash].Description()
	requireContains(t, description, wantRunnerBoundary)
	requireNotContains(t, description, "not an OS sandbox")
	if again := tools[builtin.ToolBash].Description(); again != description {
		t.Fatalf("description is not stable: %q vs %q", description, again)
	}

	legacy := builtin.NewShellWithOptions(t.TempDir(), time.Second, builtin.WithRunner(&fakeRunner{}))
	requireContains(t, legacy.Description(), wantRunnerBoundary)
	requireNotContains(t, legacy.Description(), "not an OS sandbox")
}

// TestPwshDescriptionWithoutRunner 证明未挂接 runner 时 pwsh 描述保留原句。
func TestPwshDescriptionWithoutRunner(t *testing.T) {
	legacy := builtin.NewPwsh(t.TempDir(), time.Second)
	requireContains(t, legacy.Description(), "This tool is not an OS sandbox: the approval gate is.")
	requireNotContains(t, legacy.Description(), "runs under an OS sandbox runner")

	_, policy := newRunnerRoot(t, "sh")
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Policy: policy, Binary: "/bin/sh"})
	requireContains(t, tool.Description(), "This tool is not an OS sandbox: the approval gate is.")
	requireNotContains(t, tool.Description(), "runs under an OS sandbox runner")
}

// TestPwshDescriptionWithRunner 证明挂接 runner 后 pwsh 描述与 bash 同构改写。
func TestPwshDescriptionWithRunner(t *testing.T) {
	_, policy := newRunnerRoot(t, "sh")
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Policy: policy, Binary: "/bin/sh", Runner: &fakeRunner{}})
	description := tool.Description()
	requireContains(t, description, wantRunnerBoundary)
	requireNotContains(t, description, "not an OS sandbox")
	if again := tool.Description(); again != description {
		t.Fatalf("description is not stable: %q vs %q", description, again)
	}

	viaOptions := builtin.NewPwshWithPolicyOptions(policy, builtin.WithRunner(&fakeRunner{}))
	requireContains(t, viaOptions.Description(), wantRunnerBoundary)
	requireNotContains(t, viaOptions.Description(), "not an OS sandbox")
}

// --- RG-03: bash foreground attribution ------------------------------------

// TestBashSandboxDenialAttribution 逐一覆盖四平台拒绝方言（含中文 Windows
// 变体与大小写不敏感匹配）：命令失败且输出匹配方言时，结果在退出标记之后追加
// 结构化归因行；非零退出仍是供模型解读的结果（IsError=false，语义不变）。
func TestBashSandboxDenialAttribution(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	cases := []struct {
		name        string
		signatures  []string
		matched     string
		emitted     string
		enforcement sandbox.Enforcement
	}{
		{"bwrap", []string{"read-only file system"}, "read-only file system", "Read-only file system", sandbox.EnforcementFull},
		{"bwrap-uppercase", []string{"read-only file system"}, "read-only file system", "READ-ONLY FILE SYSTEM", sandbox.EnforcementFull},
		{"landlock", []string{"permission denied"}, "permission denied", "Permission denied", sandbox.EnforcementFull},
		{"seatbelt", []string{"operation not permitted"}, "operation not permitted", "Operation not permitted", sandbox.EnforcementFull},
		{"windows-acl", []string{"access is denied", "拒绝访问"}, "access is denied", "Access is denied", sandbox.EnforcementPartial},
		{"windows-acl-zh", []string{"access is denied", "拒绝访问"}, "拒绝访问", "拒绝访问", sandbox.EnforcementPartial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, policy := newRunnerRoot(t, "echo")
			runner := denialRunner(tc.signatures, tc.emitted, 3)
			runner.enforcement = tc.enforcement
			tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))

			result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original"}`)
			if result.IsError {
				t.Fatalf("bash IsError = true, want the DSH non-zero-exit result semantics (content %q)", result.Content)
			}
			requireContains(t, result.Content, tc.emitted, "[exit code: 3]")
			requireSandboxNote(t, result.Content, tc.matched, tc.enforcement)
			requireNoteTerminates(t, result.Content)
		})
	}
}

// TestBashSandboxDenialCanonicalNote 逐字钉住归因说明的首行（bwrap 方言、
// workspace-write、full），并断言它带有可行动的处置顺序，保证模型可见文案稳定。
func TestBashSandboxDenialCanonicalNote(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	_, policy := newRunnerRoot(t, "echo")
	runner := denialRunner([]string{"read-only file system"}, "Read-only file system", 3)
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))

	result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original"}`)
	// 带退出码后，摘要行会追加 ", exit N"，让审计/排查能区分"命令失败"与"被拒"。
	wantSummary := "[sandbox: denied unknown to a path outside the writable roots " +
		"(matched \"read-only file system\"; mode workspace-write, enforcement full, exit 3)]"
	if !strings.Contains(result.Content, wantSummary) {
		t.Fatalf("canonical summary missing:\ngot  %q\nwant %q", result.Content, wantSummary)
	}
	// 处置顺序：先重定向工具自己的状态，再申请具体可写根，别重试同一命令。
	requireContains(t, result.Content, "redirecting", "writable root", "Do not repeat")
}

// TestBashSandboxDenialAttributionOnExitZero 钉住新契约的关键语义变化：命令
// 自称成功（退出 0）但输出命中拒绝方言时同样追加归因说明。这正是许多 CLI 把
// 落盘失败包成自己的文案后仍退 0 的场景。
func TestBashSandboxDenialAttributionOnExitZero(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	_, policy := newRunnerRoot(t, "echo")
	runner := denialRunner([]string{"read-only file system"}, "read-only file system", 0)
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))

	result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original"}`)
	if result.IsError {
		t.Fatalf("bash = %q, want the DSH success semantics for exit 0", result.Content)
	}
	requireContains(t, result.Content, "read-only file system")
	requireNotContains(t, result.Content, "[exit code:")
	requireSandboxNote(t, result.Content, "read-only file system", sandbox.EnforcementFull)
	requireNoteTerminates(t, result.Content)
}

// TestBashSandboxDenialControls 是归因的对照组：不匹配方言的失败、不带方言的
// 成功路径、无 runner、runner 无方言，都不得出现归因，且内容逐字节不变。
func TestBashSandboxDenialControls(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	t.Run("non-matching-failure", func(t *testing.T) {
		_, policy := newRunnerRoot(t, "echo")
		runner := denialRunner([]string{"read-only file system"}, "No such file or directory", 1)
		tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))
		result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original"}`)
		requireContains(t, result.Content, "No such file or directory", "[exit code: 1]")
		requireNotContains(t, result.Content, sandboxNotePrefix)
	})
	t.Run("clean-success-is-byte-identical", func(t *testing.T) {
		_, policy := newRunnerRoot(t, "echo")
		runner := denialRunner([]string{"read-only file system"}, "all good", 0)
		tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))
		result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original"}`)
		if result.IsError {
			t.Fatalf("bash = %q, want success", result.Content)
		}
		// 未命中方言的成功结果保持逐字节不变：只有命令输出本身。
		if want := "all good\n"; result.Content != want {
			t.Fatalf("clean success content = %q, want %q", result.Content, want)
		}
	})
	t.Run("no-runner", func(t *testing.T) {
		_, policy := newRunnerRoot(t, "sh")
		tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy))
		result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"sh -c \"echo 'permission denied' >&2; exit 1\""}`)
		requireContains(t, result.Content, "permission denied", "[exit code: 1]")
		requireNotContains(t, result.Content, sandboxNotePrefix)
	})
	t.Run("runner-without-signatures", func(t *testing.T) {
		_, policy := newRunnerRoot(t, "echo")
		runner := denialRunner(nil, "permission denied", 1)
		tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))
		result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original"}`)
		requireContains(t, result.Content, "[exit code: 1]")
		requireNotContains(t, result.Content, sandboxNotePrefix)
	})
}

// TestBashSandboxDenialTimeoutAttribution 证明超时失败（IsError=true）且已捕获
// 输出匹配方言时同样追加归因行。
func TestBashSandboxDenialTimeoutAttribution(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	_, policy := newRunnerRoot(t, "echo")
	runner := &fakeRunner{
		signatures: []string{"read-only file system"},
		rewrite: func([]string) ([]string, error) {
			return []string{"/bin/sh", "-c", "echo 'Read-only file system' >&2; sleep 5"}, nil
		},
	}
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))
	result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original","timeout_ms":300}`)
	if !result.IsError {
		t.Fatalf("bash = %q, want a timeout tool error", result.Content)
	}
	requireContains(t, result.Content, "[timeout:")
	requireSandboxNote(t, result.Content, "read-only file system", sandbox.EnforcementFull)
}

// TestBashPromotedSandboxDenialAttribution 证明超时提升路径在命令于等待期内
// 以非零退出结束时，渲染的结果同样携带归因行。
func TestBashPromotedSandboxDenialAttribution(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	_, policy := newRunnerRoot(t, "echo")
	manager := jobs.NewManager(jobs.WithPolicy(policy))
	t.Cleanup(func() { _ = manager.CloseAll() })
	runner := denialRunner([]string{"permission denied"}, "Permission denied", 3)
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy,
		builtin.WithBackgroundJobs(manager), builtin.WithRunner(runner)))

	result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original","timeout_ms":5000}`)
	requireContains(t, result.Content, "Permission denied", "[exit code: 3]")
	requireSandboxNote(t, result.Content, "permission denied", sandbox.EnforcementFull)
}

// TestBashBackgroundSandboxDenialAttribution 证明 run_in_background 路径把
// producer Confine 得到的方言交给 jobs.Manager：job_output 的失败结果携带
// 归因行（端到端覆盖 ProcessSpec 的方言传递）。
func TestBashBackgroundSandboxDenialAttribution(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	_, policy := newRunnerRoot(t, "echo")
	manager := jobs.NewManager(jobs.WithPolicy(policy))
	t.Cleanup(func() { _ = manager.CloseAll() })
	jobTools := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		jobTools[tool.Name()] = tool
	}
	runner := denialRunner([]string{"access is denied", "拒绝访问"}, "Access is denied", 5)
	runner.enforcement = sandbox.EnforcementPartial
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy,
		builtin.WithBackgroundJobs(manager), builtin.WithRunner(runner)))

	result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original","run_in_background":true}`)
	jobID := backgroundJobID(t, result.Content)
	output := invokeBackgroundJob(t, jobTools, jobs.ToolJobOutput,
		`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	requireContains(t, output.Content, "Access is denied", "exit_code: 5")
	requireSandboxNote(t, output.Content, "access is denied", sandbox.EnforcementPartial)
}

// --- RG-03: pwsh attribution -----------------------------------------------

// TestPwshSandboxDenialAttribution 证明 pwsh 与 bash 同构：失败且匹配方言时
// 追加归因行；成功路径不变。
func TestPwshSandboxDenialAttribution(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	_, policy := newRunnerRoot(t, "sh")
	runner := denialRunner([]string{"operation not permitted"}, "Operation not permitted", 4)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Policy: policy, Binary: "/bin/sh", Runner: runner})

	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "pwsh-attr", Name: builtin.ToolPwsh, Arguments: `{"command":"Write-Output original"}`,
	})
	if err != nil {
		t.Fatalf("pwsh.Invoke returned Go error: %v", err)
	}
	requireContains(t, result.Content, "Operation not permitted", "[exit code: 4]")
	requireSandboxNote(t, result.Content, "operation not permitted", sandbox.EnforcementFull)

	// 退出 0 但命中方言：同样追加归因（不依赖退出码）。
	successRunner := denialRunner([]string{"operation not permitted"}, "operation not permitted", 0)
	successTool := builtin.NewPwshWithConfig(builtin.PwshConfig{Policy: policy, Binary: "/bin/sh", Runner: successRunner})
	success, err := successTool.Invoke(context.Background(), rig.ToolCall{
		ID: "pwsh-attr-ok", Name: builtin.ToolPwsh, Arguments: `{"command":"Write-Output original"}`,
	})
	if err != nil {
		t.Fatalf("pwsh.Invoke returned Go error: %v", err)
	}
	if success.IsError {
		t.Fatalf("pwsh = %q, want success", success.Content)
	}
	requireSandboxNote(t, success.Content, "operation not permitted", sandbox.EnforcementFull)

	// 未命中方言的成功结果逐字节不变。
	cleanRunner := denialRunner([]string{"operation not permitted"}, "all good", 0)
	cleanTool := builtin.NewPwshWithConfig(builtin.PwshConfig{Policy: policy, Binary: "/bin/sh", Runner: cleanRunner})
	clean, err := cleanTool.Invoke(context.Background(), rig.ToolCall{
		ID: "pwsh-attr-clean", Name: builtin.ToolPwsh, Arguments: `{"command":"Write-Output original"}`,
	})
	if err != nil {
		t.Fatalf("pwsh.Invoke returned Go error: %v", err)
	}
	if want := "all good\n"; clean.Content != want {
		t.Fatalf("clean success content = %q, want %q", clean.Content, want)
	}
}
