package builtin_test

// Tests for DSH background execution of the shell tools: run_in_background via
// a real jobs.Manager, foreground timeout promotion, DSH argument aliases,
// sandbox-escalation rejection and the per-job output cap.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// newBackgroundBash builds a bash tool wired to a real background-job manager.
func newBackgroundBash(t *testing.T, opts ...builtin.Option) (string, rig.Tool, *jobs.Manager, map[string]rig.Tool) {
	t.Helper()
	root := t.TempDir()
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	options := append([]builtin.Option{builtin.WithBackgroundJobs(manager)}, opts...)
	tool := builtin.NewShellWithOptions(root, 5*time.Second, options...)
	jobTools := make(map[string]rig.Tool)
	for _, jobTool := range manager.Tools() {
		jobTools[jobTool.Name()] = jobTool
	}
	return root, tool, manager, jobTools
}

// invokeBackgroundJob invokes one job tool and requires no Go error.
func invokeBackgroundJob(t *testing.T, tools map[string]rig.Tool, name, arguments string) rig.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("job tool %q not found", name)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", name, err)
	}
	return result
}

// backgroundJobID extracts the job id from a run_in_background acknowledgement.
func backgroundJobID(t *testing.T, text string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "started background job ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "started background job "))
		}
	}
	t.Fatalf("no background job id in %q", text)
	return ""
}

// promotedJobID extracts the job id from a promoted foreground result.
func promotedJobID(t *testing.T, text string) string {
	t.Helper()
	const marker = "moved to background job "
	index := strings.Index(text, marker)
	if index < 0 {
		t.Fatalf("no promoted job id in %q", text)
	}
	rest := text[index+len(marker):]
	if end := strings.IndexAny(rest, "]\n"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

func TestBashRunInBackground(t *testing.T) {
	_, bash, manager, jobTools := newBackgroundBash(t)
	schema := bash.Schema()
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{"command", "description", "timeout_ms", "timeoutMs", "workdir", "run_in_background"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("bash schema missing %q: %v", name, properties)
		}
	}

	result := runBash(t, bash, `{"command":"echo background-output; exit 4","run_in_background":true}`)
	if result.IsError {
		t.Fatalf("bash(run_in_background) = %q", result.Content)
	}
	jobID := backgroundJobID(t, result.Content)
	if !strings.HasPrefix(jobID, "bash-") {
		t.Fatalf("job id = %q, want bash- prefix", jobID)
	}
	output := invokeBackgroundJob(t, jobTools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if output.IsError {
		t.Fatalf("job_output: %q", output.Content)
	}
	requireContains(t, output.Content, "background-output", "state: exited", "status: completed", "exit_code: 4")
	if manager.Len() != 1 {
		t.Fatalf("manager.Len = %d, want 1", manager.Len())
	}
}

func TestBashBackgroundNonZeroExitIsResult(t *testing.T) {
	_, bash, _, jobTools := newBackgroundBash(t)
	result := runBash(t, bash, `{"command":"echo before; exit 7","run_in_background":true}`)
	jobID := backgroundJobID(t, result.Content)
	output := invokeBackgroundJob(t, jobTools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if output.IsError {
		t.Fatalf("job_output = %q, want a result, not a tool error", output.Content)
	}
	requireContains(t, output.Content, "before", "exit_code: 7")
}

func TestBashPromotionOnTimeout(t *testing.T) {
	_, bash, _, jobTools := newBackgroundBash(t)
	started := time.Now()
	result := runBash(t, bash, `{"command":"echo first-line; sleep 5","timeout_ms":150}`)
	elapsed := time.Since(started)
	if result.IsError {
		t.Fatalf("promoted bash = %q, want a non-error hand-off", result.Content)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("promotion took %s, want near 150ms", elapsed)
	}
	requireContains(t, result.Content, "first-line", "[still running after", "moved to background job bash-")
	jobID := promotedJobID(t, result.Content)

	// The promoted call took one consuming read; job_output continues after it.
	output := invokeBackgroundJob(t, jobTools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":false}`)
	if output.IsError {
		t.Fatalf("job_output: %q", output.Content)
	}
	requireContains(t, output.Content, "state: running")
	invokeBackgroundJob(t, jobTools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","reason":"test cleanup"}`)
}

func TestBashPromotedOutputContinues(t *testing.T) {
	_, bash, _, jobTools := newBackgroundBash(t)
	result := runBash(t, bash, `{"command":"echo early; sleep 0.2; echo late","timeout_ms":50}`)
	jobID := promotedJobID(t, result.Content)
	output := invokeBackgroundJob(t, jobTools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if output.IsError {
		t.Fatalf("job_output(wait): %q", output.Content)
	}
	// "early" was consumed by the hand-off read; the follow-up read carries the
	// rest.
	if strings.Contains(output.Content, "early\n") {
		t.Fatalf("hand-off read and job_output both carried the same output:\n%s", output.Content)
	}
	requireContains(t, output.Content, "late")
}

func TestBashForegroundThroughBackgroundManager(t *testing.T) {
	_, bash, manager, _ := newBackgroundBash(t)
	// A command that finishes inside the timeout is an ordinary foreground
	// result and its transient job record leaves the registry.
	result := runBash(t, bash, `{"command":"echo normal-output; exit 3"}`)
	if result.IsError {
		t.Fatalf("foreground bash = %q", result.Content)
	}
	requireContains(t, result.Content, "normal-output", "[exit code: 3]")
	if manager.Len() != 0 {
		t.Fatalf("manager.Len after foreground call = %d, want 0", manager.Len())
	}
}

func TestBashRunInBackgroundWithoutManager(t *testing.T) {
	_, bash := newBashForTest(t, time.Second)
	result := runBash(t, bash, `{"command":"echo hi","run_in_background":true}`)
	if !result.IsError || !strings.Contains(result.Content, "background jobs unavailable") {
		t.Fatalf("bash(run_in_background, no manager) = %q (IsError=%v)", result.Content, result.IsError)
	}
	// The schema does not advertise the capability without a manager.
	properties, _ := bash.Schema()["properties"].(map[string]any)
	if _, ok := properties["run_in_background"]; ok {
		t.Fatalf("foreground-only bash advertises run_in_background: %v", properties)
	}
}

func TestBashBackgroundDisabled(t *testing.T) {
	_, bash, _, _ := newBackgroundBash(t, builtin.WithRunInBackground(false))
	result := runBash(t, bash, `{"command":"echo hi","run_in_background":true}`)
	if !result.IsError || !strings.Contains(result.Content, "disabled") {
		t.Fatalf("bash(disabled run_in_background) = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestBashTimeoutKillsWithoutPromotion(t *testing.T) {
	_, bash := newBashForTest(t, 50*time.Millisecond)
	result := runBash(t, bash, `{"command":"sleep 5"}`)
	if !result.IsError || !strings.Contains(result.Content, "timeout") {
		t.Fatalf("bash(timeout, no background) = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestBashDSHArgs(t *testing.T) {
	_, bash := newBashForTest(t, 5*time.Second)
	// description and the camelCase timeoutMs alias are accepted.
	result := runBash(t, bash, `{"description":"Echo a value","command":"echo alias-ok","timeoutMs":1000}`)
	if result.IsError || !strings.Contains(result.Content, "alias-ok") {
		t.Fatalf("bash(timeoutMs) = %q (IsError=%v)", result.Content, result.IsError)
	}
	// sandbox_permissions is accepted on the wire but rejected: no escalation
	// executor is composed.
	result = runBash(t, bash, `{"command":"echo hi","sandbox_permissions":"workspace-write","justification":"needed"}`)
	if !result.IsError || !strings.Contains(result.Content, "not available in this composition") {
		t.Fatalf("bash(sandbox_permissions) = %q (IsError=%v)", result.Content, result.IsError)
	}
	// justification alone is also rejected.
	result = runBash(t, bash, `{"command":"echo hi","justification":"needed"}`)
	if !result.IsError || !strings.Contains(result.Content, "justification") {
		t.Fatalf("bash(justification) = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestBashBackgroundPolicyMode(t *testing.T) {
	realRoot := policyRoot(t)
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithCommandAllowlist("echo"))
	manager := jobs.NewManager(jobs.WithPolicy(policy))
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := builtin.NewWithPolicyOptions(policy, builtin.WithBackgroundJobs(manager))
	bash := policyToolMap(t, tools)[builtin.ToolBash]
	jobTools := make(map[string]rig.Tool)
	for _, jobTool := range manager.Tools() {
		jobTools[jobTool.Name()] = jobTool
	}

	result := invokePolicy(t, map[string]rig.Tool{builtin.ToolBash: bash}, builtin.ToolBash,
		`{"command":"echo policy-bg","run_in_background":true}`)
	if result.IsError {
		t.Fatalf("policy background bash = %q", result.Content)
	}
	jobID := backgroundJobID(t, result.Content)
	output := invokeBackgroundJob(t, jobTools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	requireContains(t, output.Content, "policy-bg", "state: exited")
}

func TestPwshRunInBackgroundAndPromotion(t *testing.T) {
	requireUnixish(t)
	root := t.TempDir()
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	script := fakePwsh(t, `sleep 0.05; echo fake-pwsh-bg`)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{
		Binary:         script,
		Root:           root,
		Timeout:        5 * time.Second,
		BackgroundJobs: manager,
	})
	jobTools := make(map[string]rig.Tool)
	for _, jobTool := range manager.Tools() {
		jobTools[jobTool.Name()] = jobTool
	}

	result := invokePwsh(t, tool, `{"command":"echo fake-pwsh-bg","timeoutMs":1000,"run_in_background":true}`)
	if result.IsError {
		t.Fatalf("pwsh(run_in_background) = %q", result.Content)
	}
	jobID := backgroundJobID(t, result.Content)
	output := invokeBackgroundJob(t, jobTools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	requireContains(t, output.Content, "fake-pwsh-bg", "state: exited")

	// A foreground script that outlives its timeout is promoted.
	timeoutScript := fakePwsh(t, `sleep 5`)
	timeoutTool := builtin.NewPwshWithConfig(builtin.PwshConfig{
		Binary:         timeoutScript,
		Root:           root,
		Timeout:        5 * time.Second,
		BackgroundJobs: manager,
	})
	promoted := invokePwsh(t, timeoutTool, `{"command":"sleep 5","timeout_ms":100}`)
	if promoted.IsError {
		t.Fatalf("promoted pwsh = %q", promoted.Content)
	}
	requireContains(t, promoted.Content, "[still running after", "moved to background job pwsh-")
	invokeBackgroundJob(t, jobTools, jobs.ToolJobKill, `{"job_id":"`+promotedJobID(t, promoted.Content)+`"}`)

	// The schema advertises the DSH aliases while a manager is attached.
	properties, _ := tool.Schema()["properties"].(map[string]any)
	for _, name := range []string{"command", "description", "timeout_ms", "timeoutMs", "workdir", "run_in_background"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("pwsh schema missing %q: %v", name, properties)
		}
	}
}

func TestPwshRunInBackgroundWithoutManager(t *testing.T) {
	requireUnixish(t)
	script := fakePwsh(t, `echo hi`)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Root: t.TempDir()})
	result := invokePwsh(t, tool, `{"command":"echo hi","run_in_background":true}`)
	if !result.IsError || !strings.Contains(result.Content, "background jobs unavailable") {
		t.Fatalf("pwsh(run_in_background, no manager) = %q (IsError=%v)", result.Content, result.IsError)
	}
}
