package jobs_test

// read-only + runner 的口径：job_start 与前台 bash 一致——挂了 OS runner 时允许
// （命令仍由 runner 包装、策略校验照旧），没挂 runner 时仍然拒绝，且拒绝文案要
// 说清"缺的是 runner"。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// TestJobStartReadOnlyWithRunnerRunsConfined 钉住 read-only + runner 下
// job_start 能真的跑起来，并且**执行的是 runner 返回的 argv**。
//
// 只断言 "Confine 被调用过" 强度不够：变异证明"调了 Confine 但丢弃返回的 argv
// （裸跑原命令）"仍会绿。所以让假 runner 把命令改写成 `echo runner-wrapped`，
// 断言产出是改写后的内容，而且原命令（`touch marker.txt`）的副作用根本没发生。
func TestJobStartReadOnlyWithRunnerRunsConfined(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithCommandAllowlist("touch"), sandbox.WithReadOnly(true))
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return []string{"/bin/sh", "-c", "echo runner-wrapped"}, nil
	}}
	_, tools := newJobTools(t, jobs.WithPolicy(policy), jobs.WithRunner(runner))

	// 原命令带一个只有**真的执行了才会出现**的副作用（在可写的 cwd 里落一个文件）：
	// 输出头里的 "command:" 只是标签，不能用来判断谁被执行了。
	jobID := startJob(t, tools, fmt.Sprintf(`{"command":"touch marker.txt","cwd":%q}`, realRoot))
	output := invokeJob(t, tools, jobs.ToolJobOutput,
		`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if output.IsError {
		t.Fatalf("job_output = %q", output.Content)
	}
	if !strings.Contains(output.Content, "runner-wrapped") {
		t.Fatalf("job output = %q, want runner-wrapped（job_start 必须执行 runner 返回的 argv）", output.Content)
	}
	if _, err := os.Stat(filepath.Join(realRoot, "marker.txt")); err == nil {
		t.Fatalf("marker.txt 落盘了：原命令被执行，runner 返回的 argv 被丢弃（read-only 下等于裸跑）")
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
