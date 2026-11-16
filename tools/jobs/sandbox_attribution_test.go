package jobs_test

// Audit fix RG-03 (2026-11 ZeoKeel security/usability audit), jobs side: when a confined
// background job fails and its retained output matches one of the runner's
// DenialSignatures, every result view of that job (job_output/job_wait/
// job_kill) gains the structured "[sandbox: ...]" attribution line. Running
// jobs, successful jobs, non-matching failures and model-killed jobs must not
// carry it. The four platform dialects are exercised through the fake runner
// (platform-independent matching logic).

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// sandboxNotePrefix 是沙箱拒绝归因行的稳定前缀（与 builtin 侧一致）。
const sandboxNotePrefix = "[sandbox: suspected OS sandbox denial"

// denialJobRunner 返回一个 fake runner：Confine 报告给定拒绝方言，并把 argv
// 改写为「向 stderr 打印 emitted 并以 exitCode 退出」的 /bin/sh 命令。emitted
// 不得包含单引号。
func denialJobRunner(signatures []string, emitted string, exitCode int) *fakeRunner {
	return &fakeRunner{
		signatures: signatures,
		rewrite: func([]string) ([]string, error) {
			return []string{"/bin/sh", "-c", fmt.Sprintf("echo '%s' >&2; exit %d", emitted, exitCode)}, nil
		},
	}
}

// jobIDFromFormat 从 job_start 的格式化结果中提取 job id。
func jobIDFromFormat(t *testing.T, content string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "job_id: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "job_id: "))
		}
	}
	t.Fatalf("job_start output has no job id: %q", content)
	return ""
}

// requireJobNote 断言结果文本携带匹配 dialect 的结构化归因行。
func requireJobNote(t *testing.T, content, dialect string, enforcement sandbox.Enforcement) {
	t.Helper()
	for _, fragment := range []string{
		sandboxNotePrefix,
		fmt.Sprintf("matched %q", dialect),
		"policy mode workspace-write",
		"enforcement " + string(enforcement),
	} {
		if !strings.Contains(content, fragment) {
			t.Fatalf("attribution line missing %q in:\n%s", fragment, content)
		}
	}
}

// TestJobSandboxDenialAttribution 逐一覆盖四平台拒绝方言（含中文 Windows
// 变体与大小写不敏感匹配）：job_start 启动时（running）无归因行；失败结束后
// job_output 携带归因行。
func TestJobSandboxDenialAttribution(t *testing.T) {
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
			root := t.TempDir()
			policy := newRunnerPolicy(t, root, "echo")
			runner := denialJobRunner(tc.signatures, tc.emitted, 5)
			runner.enforcement = tc.enforcement
			_, tools := newJobTools(t, jobs.WithPolicy(policy), jobs.WithRunner(runner))

			started := invokeJob(t, tools, jobs.ToolJobStart, fmt.Sprintf(`{"command":"echo original","cwd":%q}`, root))
			if strings.Contains(started.Content, sandboxNotePrefix) {
				t.Fatalf("running job must not carry attribution:\n%s", started.Content)
			}
			jobID := jobIDFromFormat(t, started.Content)
			output := invokeJob(t, tools, jobs.ToolJobOutput,
				`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
			if !strings.Contains(output.Content, tc.emitted) || !strings.Contains(output.Content, "exit_code: 5") {
				t.Fatalf("job_output = %q, want the dialect output and exit_code: 5", output.Content)
			}
			requireJobNote(t, output.Content, tc.matched, tc.enforcement)
			if !strings.HasSuffix(output.Content, "]\n") {
				t.Fatalf("attribution line must terminate the result: %q", output.Content)
			}
		})
	}
}

// TestJobWaitAndKillShowSandboxDenial 证明 job_wait 与 job_kill（对已结束
// 任务）的结果视图同样携带归因行。
func TestJobWaitAndKillShowSandboxDenial(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	root := t.TempDir()
	policy := newRunnerPolicy(t, root, "echo")
	runner := denialJobRunner([]string{"permission denied"}, "Permission denied", 5)
	_, tools := newJobTools(t, jobs.WithPolicy(policy), jobs.WithRunner(runner))

	jobID := startJob(t, tools, fmt.Sprintf(`{"command":"echo original","cwd":%q}`, root))
	waited := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":5000}`)
	requireJobNote(t, waited.Content, "permission denied", sandbox.EnforcementFull)

	killed := invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`"}`)
	if !strings.Contains(killed.Content, "outcome: already-finished") {
		t.Fatalf("job_kill = %q, want already-finished", killed.Content)
	}
	requireJobNote(t, killed.Content, "permission denied", sandbox.EnforcementFull)
}

// TestProducerSpecDenialSignaturesAttributed 证明生产者自行 Confine
// （PolicyChecked=true）并经 ProcessSpec 报告方言时，管理器不再二次 Confine，
// 但失败结果视图仍携带归因行（补充覆盖；端到端路径另见 builtin 侧
// TestBashBackgroundSandboxDenialAttribution）。
func TestProducerSpecDenialSignaturesAttributed(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	root := t.TempDir()
	policy := newRunnerPolicy(t, root, "sh")
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return nil, sandbox.ErrSandboxUnavailable
	}}
	manager := jobs.NewManager(jobs.WithPolicy(policy), jobs.WithRunner(runner))
	t.Cleanup(func() { _ = manager.CloseAll() })

	job, err := manager.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindBash),
		Label:            "echo producer",
		Command:          "echo producer",
		Argv:             []string{"/bin/sh", "-c", "echo 'Permission denied' >&2; exit 7"},
		Dir:              root,
		PolicyChecked:    true,
		DenialSignatures: []string{"permission denied"},
		Enforcement:      sandbox.EnforcementPartial,
	})
	if err != nil {
		t.Fatalf("StartProcess(PolicyChecked) = %v", err)
	}
	if _, settled, waitErr := job.Wait(context.Background(), 5*time.Second); waitErr != nil || !settled {
		t.Fatalf("job.Wait = settled=%v err=%v", settled, waitErr)
	}
	byName := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		byName[tool.Name()] = tool
	}
	output := invokeJob(t, byName, jobs.ToolJobOutput, `{"job_id":"`+job.ID()+`","tail":100}`)
	if !strings.Contains(output.Content, "exit_code: 7") {
		t.Fatalf("job_output = %q, want exit_code: 7", output.Content)
	}
	requireJobNote(t, output.Content, "permission denied", sandbox.EnforcementPartial)
	if runner.callCount() != 0 {
		t.Fatalf("runner calls = %d, want 0 (PolicyChecked must skip re-wrapping)", runner.callCount())
	}
}

// TestJobSandboxDenialControls 是归因的对照组：成功任务（即使输出含方言）、
// 不匹配方言的失败、无 runner 的失败、被模型杀死的任务，都不得出现归因行。
func TestJobSandboxDenialControls(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	t.Run("success-with-dialect-output", func(t *testing.T) {
		root := t.TempDir()
		policy := newRunnerPolicy(t, root, "echo")
		runner := denialJobRunner([]string{"read-only file system"}, "read-only file system", 0)
		_, tools := newJobTools(t, jobs.WithPolicy(policy), jobs.WithRunner(runner))
		jobID := startJob(t, tools, fmt.Sprintf(`{"command":"echo original","cwd":%q}`, root))
		output := invokeJob(t, tools, jobs.ToolJobOutput,
			`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
		if !strings.Contains(output.Content, "exit_code: 0") {
			t.Fatalf("job_output = %q, want exit_code: 0", output.Content)
		}
		if strings.Contains(output.Content, sandboxNotePrefix) {
			t.Fatalf("successful job must not carry attribution:\n%s", output.Content)
		}
	})
	t.Run("non-matching-failure", func(t *testing.T) {
		root := t.TempDir()
		policy := newRunnerPolicy(t, root, "echo")
		runner := denialJobRunner([]string{"read-only file system"}, "No such file or directory", 1)
		_, tools := newJobTools(t, jobs.WithPolicy(policy), jobs.WithRunner(runner))
		jobID := startJob(t, tools, fmt.Sprintf(`{"command":"echo original","cwd":%q}`, root))
		output := invokeJob(t, tools, jobs.ToolJobOutput,
			`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
		if !strings.Contains(output.Content, "exit_code: 1") {
			t.Fatalf("job_output = %q, want exit_code: 1", output.Content)
		}
		if strings.Contains(output.Content, sandboxNotePrefix) {
			t.Fatalf("non-matching failure must not carry attribution:\n%s", output.Content)
		}
	})
	t.Run("no-runner", func(t *testing.T) {
		root := t.TempDir()
		policy := newRunnerPolicy(t, root, "sh")
		_, tools := newJobTools(t, jobs.WithPolicy(policy))
		jobID := startJob(t, tools, fmt.Sprintf(`{"command":"sh -c \"echo 'permission denied' >&2; exit 1\"","cwd":%q}`, root))
		output := invokeJob(t, tools, jobs.ToolJobOutput,
			`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
		if !strings.Contains(output.Content, "permission denied") {
			t.Fatalf("job_output = %q, want the dialect output", output.Content)
		}
		if strings.Contains(output.Content, sandboxNotePrefix) {
			t.Fatalf("job without a runner must not carry attribution:\n%s", output.Content)
		}
	})
	t.Run("killed-job", func(t *testing.T) {
		root := t.TempDir()
		policy := newRunnerPolicy(t, root, "echo")
		runner := &fakeRunner{
			signatures: []string{"access is denied", "拒绝访问"},
			rewrite: func([]string) ([]string, error) {
				return []string{"/bin/sh", "-c", "echo 'Access is denied' >&2; sleep 5"}, nil
			},
		}
		_, tools := newJobTools(t, jobs.WithPolicy(policy), jobs.WithRunner(runner))
		jobID := startJob(t, tools, fmt.Sprintf(`{"command":"echo original","cwd":%q}`, root))
		// 等待方言输出进入输出环，再杀死任务。
		seen := false
		for i := 0; i < 100; i++ {
			poll := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","tail":100}`)
			if strings.Contains(poll.Content, "Access is denied") {
				seen = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !seen {
			t.Fatal("dialect output never reached the job ring")
		}
		killed := invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","reason":"stop the probe"}`)
		if !strings.Contains(killed.Content, "state: killed") {
			t.Fatalf("job_kill = %q, want state: killed", killed.Content)
		}
		if strings.Contains(killed.Content, sandboxNotePrefix) {
			t.Fatalf("model-killed job must not carry attribution:\n%s", killed.Content)
		}
		after := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","tail":100}`)
		if strings.Contains(after.Content, sandboxNotePrefix) {
			t.Fatalf("killed job view must not carry attribution:\n%s", after.Content)
		}
	})
}
