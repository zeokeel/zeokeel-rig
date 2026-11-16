package builtin_test

// Audit fixes RG-05 and RG-03 (zeokeel audit round, rig.md):
//
//   - RG-05: the bash/pwsh model-facing description must follow the actual
//     composition. Without a runner the historical "not an OS sandbox"
//     sentence stays; with a runner injected the description must say that an
//     OS sandbox confines file effects and the approval gate sits on top.
//   - RG-03: when a confined command fails and its merged output matches one
//     of the runner's DenialSignatures, the tool result gains a structured
//     "[sandbox: ...]" attribution line so the model can tell an OS sandbox
//     denial apart from the command failing on its own. Success paths, and
//     failures that do not match a signature, must stay byte-identical.
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

// sandboxNotePrefix 是沙箱拒绝归因行的稳定前缀。
const sandboxNotePrefix = "[sandbox: suspected OS sandbox denial"

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

// requireSandboxNote 断言结果文本携带匹配 dialect 的结构化归因行。
func requireSandboxNote(t *testing.T, content, dialect string, enforcement sandbox.Enforcement) {
	t.Helper()
	requireContains(t, content,
		sandboxNotePrefix,
		fmt.Sprintf("matched %q", dialect),
		"policy mode workspace-write",
		"enforcement "+string(enforcement),
	)
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
			if !strings.HasSuffix(result.Content, "]") {
				t.Fatalf("attribution line must terminate the result: %q", result.Content)
			}
		})
	}
}

// TestBashSandboxDenialCanonicalNote 逐字钉住一条完整归因行（bwrap 方言、
// workspace-write、full），保证模型可见文案稳定。
func TestBashSandboxDenialCanonicalNote(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	_, policy := newRunnerRoot(t, "echo")
	runner := denialRunner([]string{"read-only file system"}, "Read-only file system", 3)
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))

	result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original"}`)
	want := "[sandbox: suspected OS sandbox denial (matched \"read-only file system\"; policy mode workspace-write, enforcement full); " +
		"the OS sandbox blocked a file effect rather than the command failing on its own. " +
		"Keep file effects inside the sandbox policy, or ask the operator to adjust the sandbox mode; per-call escalation is not available]"
	if !strings.Contains(result.Content, want) {
		t.Fatalf("canonical attribution line missing:\ngot  %q\nwant %q", result.Content, want)
	}
}

// TestBashSandboxDenialControls 是归因的对照组：不匹配方言的失败、成功路径
// （即使输出含方言）、无 runner、runner 无方言，都不得出现归因行。
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
	t.Run("success-with-dialect-output", func(t *testing.T) {
		_, policy := newRunnerRoot(t, "echo")
		runner := denialRunner([]string{"read-only file system"}, "read-only file system", 0)
		tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))
		result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original"}`)
		if result.IsError {
			t.Fatalf("bash = %q, want success", result.Content)
		}
		requireContains(t, result.Content, "read-only file system")
		requireNotContains(t, result.Content, sandboxNotePrefix, "[exit code:")
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
	requireNotContains(t, success.Content, sandboxNotePrefix)
}
