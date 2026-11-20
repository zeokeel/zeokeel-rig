package jobs_test

// read-only + runner 的口径：job_start 与前台 bash 一致——挂了 OS runner 时允许
// （命令仍由 runner 包装、策略校验照旧），没挂 runner 时仍然拒绝，且拒绝文案要
// 说清"缺的是 runner"。

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// TestJobStartReadOnlyWithRunnerRunsConfined 钉住 read-only + runner 下
// job_start 能真的跑起来，并且命令确实经过了 runner 包装。
func TestJobStartReadOnlyWithRunnerRunsConfined(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithCommandAllowlist("echo"), sandbox.WithReadOnly(true))
	runner := &fakeRunner{}
	_, tools := newJobTools(t, jobs.WithPolicy(policy), jobs.WithRunner(runner))

	jobID := startJob(t, tools, fmt.Sprintf(`{"command":"echo confined-job","cwd":%q}`, realRoot))
	output := invokeJob(t, tools, jobs.ToolJobOutput,
		`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if output.IsError {
		t.Fatalf("job_output = %q", output.Content)
	}
	if !strings.Contains(output.Content, "confined-job") {
		t.Fatalf("job output = %q, want confined-job", output.Content)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1（read-only 下命令必须经 runner 包装）", runner.callCount())
	}
}

// TestJobStartReadOnlyWithoutRunnerIsDenied 钉住没有 runner 时 read-only 仍然
// 整体拒绝，并且错误文案要提 runner（与 bash 的措辞一致）。
func TestJobStartReadOnlyWithoutRunnerIsDenied(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithCommandAllowlist("echo"), sandbox.WithReadOnly(true))
	_, tools := newJobTools(t, jobs.WithPolicy(policy))

	text := invokeJobError(t, tools, jobs.ToolJobStart, `{"command":"echo hi"}`)
	for _, want := range []string{"read-only", "runner"} {
		if !strings.Contains(text, want) {
			t.Fatalf("read-only start error = %q, want it to mention %q", text, want)
		}
	}
}
