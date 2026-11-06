package jobs_test

// Tests for the background job tool set: start/output/wait/kill/list, output
// ring buffering and truncation, timeouts, sandbox policy integration and
// concurrency.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

func newJobTools(t *testing.T, opts ...jobs.Option) (*jobs.Manager, map[string]rig.Tool) {
	t.Helper()
	manager := jobs.NewManager(opts...)
	t.Cleanup(func() { _ = manager.CloseAll() })
	byName := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		byName[tool.Name()] = tool
	}
	return manager, byName
}

func invokeJob(t *testing.T, tools map[string]rig.Tool, name, arguments string) rig.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("tool %q not found", name)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", name, err)
	}
	return result
}

func invokeJobError(t *testing.T, tools map[string]rig.Tool, name, arguments string) string {
	t.Helper()
	result := invokeJob(t, tools, name, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", name, result.Content)
	}
	return result.Content
}

// startJob starts a background job and returns its id.
func startJob(t *testing.T, tools map[string]rig.Tool, arguments string) string {
	t.Helper()
	result := invokeJob(t, tools, jobs.ToolJobStart, arguments)
	if result.IsError {
		t.Fatalf("job_start: %q", result.Content)
	}
	for _, line := range strings.Split(result.Content, "\n") {
		if strings.HasPrefix(line, "job_id: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "job_id: "))
		}
	}
	t.Fatalf("job_start output has no job id: %q", result.Content)
	return ""
}

func jsonEscape(input string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return replacer.Replace(input)
}

func TestToolsShape(t *testing.T) {
	_, tools := newJobTools(t)
	if len(tools) != 5 {
		t.Fatalf("job tools = %d, want 5", len(tools))
	}
	for _, name := range []string{
		jobs.ToolJobStart, jobs.ToolJobOutput, jobs.ToolJobKill, jobs.ToolJobList, jobs.ToolJobWait,
	} {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if tool.Description() == "" || tool.Schema() == nil {
			t.Fatalf("tool %q has no description/schema", name)
		}
		if tool.Schema()["type"] != "object" {
			t.Fatalf("tool %q schema type = %v", name, tool.Schema()["type"])
		}
	}
}

func TestShortCommandExitCode(t *testing.T) {
	manager, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"echo hello; exit 3"}`)
	if !strings.HasPrefix(jobID, "job-") {
		t.Fatalf("job id = %q", jobID)
	}
	if manager.Len() != 1 {
		t.Fatalf("Len = %d, want 1", manager.Len())
	}
	result := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":5000}`)
	if result.IsError {
		t.Fatalf("job_wait: %q", result.Content)
	}
	for _, fragment := range []string{"state: exited", "exit_code: 3", "hello", "duration_ms:"} {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("job_wait output missing %q:\n%s", fragment, result.Content)
		}
	}
}

func TestJobOutputNewOutputAndTail(t *testing.T) {
	_, tools := newJobTools(t)
	command := "for i in 1 2 3 4 5; do echo line$i; done"
	jobID := startJob(t, tools, `{"command":"`+jsonEscape(command)+`"}`)
	waitResult := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":5000}`)
	if waitResult.IsError || !strings.Contains(waitResult.Content, "state: exited") {
		t.Fatalf("job_wait = %q (IsError=%v)", waitResult.Content, waitResult.IsError)
	}
	if !strings.Contains(waitResult.Content, "line5") {
		t.Fatalf("wait output missing line5: %q", waitResult.Content)
	}
	// tail selects only the last lines; earlier lines are absent from the
	// output section (the command line in the header uses $i, not line1).
	tailResult := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","tail":2}`)
	if tailResult.IsError || !strings.Contains(tailResult.Content, "line4") || !strings.Contains(tailResult.Content, "line5") {
		t.Fatalf("tail output = %q", tailResult.Content)
	}
	if strings.Contains(tailResult.Content, "line1") {
		t.Fatalf("tail returned too many lines: %q", tailResult.Content)
	}
	// New-output reads drain the cursor: a second call has nothing left.
	empty := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`"}`)
	if empty.IsError || !strings.Contains(empty.Content, "output: (none)") {
		t.Fatalf("second output = %q (IsError=%v)", empty.Content, empty.IsError)
	}
}

func TestJobOutputOffset(t *testing.T) {
	_, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"printf 'abcdef\\n'"}`)
	invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":5000}`)
	fromStart := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","offset":0}`)
	if fromStart.IsError || !strings.Contains(fromStart.Content, "abcdef") {
		t.Fatalf("offset=0 output = %q", fromStart.Content)
	}
	fromTwo := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","offset":2}`)
	if fromTwo.IsError || !strings.Contains(fromTwo.Content, "cdef") {
		t.Fatalf("offset=2 output = %q", fromTwo.Content)
	}
}

func TestJobKillSleep(t *testing.T) {
	_, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"sleep 30"}`)
	time.Sleep(100 * time.Millisecond)
	started := time.Now()
	result := invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","signal":"KILL"}`)
	elapsed := time.Since(started)
	if result.IsError {
		t.Fatalf("job_kill: %q", result.Content)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("kill took %s", elapsed)
	}
	for _, fragment := range []string{"state: killed", "exit_code:", "output:"} {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("kill output missing %q:\n%s", fragment, result.Content)
		}
	}
	// Killing again reports the finished state instead of failing.
	again := invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","signal":"TERM"}`)
	if again.IsError || !strings.Contains(again.Content, "state: killed") {
		t.Fatalf("second kill = %q (IsError=%v)", again.Content, again.IsError)
	}
}

func TestJobWaitTimeout(t *testing.T) {
	_, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"sleep 5"}`)
	started := time.Now()
	result := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":100}`)
	elapsed := time.Since(started)
	if result.IsError || !strings.Contains(result.Content, "state: running") {
		t.Fatalf("wait timeout output = %q (IsError=%v)", result.Content, result.IsError)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("wait took %s", elapsed)
	}
	invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`"}`)
}

func TestJobList(t *testing.T) {
	_, tools := newJobTools(t)
	exitedID := startJob(t, tools, `{"command":"echo done"}`)
	runningID := startJob(t, tools, `{"command":"sleep 5"}`)
	invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+exitedID+`","timeout_ms":5000}`)

	list := invokeJob(t, tools, jobs.ToolJobList, `{}`)
	if list.IsError {
		t.Fatalf("job_list: %q", list.Content)
	}
	for _, fragment := range []string{"jobs: 2", exitedID, runningID, "state=exited", "state=running", "command="} {
		if !strings.Contains(list.Content, fragment) {
			t.Fatalf("job_list missing %q:\n%s", fragment, list.Content)
		}
	}
	invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+runningID+`"}`)
	empty := invokeJob(t, tools, jobs.ToolJobList, `{}`)
	if ok := strings.Contains(empty.Content, "jobs: 2"); !ok {
		t.Fatalf("finished jobs should stay listed: %q", empty.Content)
	}
}

func TestUnknownJobIDs(t *testing.T) {
	_, tools := newJobTools(t)
	for name, arguments := range map[string]string{
		jobs.ToolJobOutput: `{"job_id":"nope"}`,
		jobs.ToolJobKill:   `{"job_id":"nope"}`,
		jobs.ToolJobWait:   `{"job_id":"nope"}`,
	} {
		text := invokeJobError(t, tools, name, arguments)
		if !strings.Contains(text, "unknown job") {
			t.Fatalf("%s unknown id error = %q", name, text)
		}
	}
}

func TestOutputRingTruncation(t *testing.T) {
	_, tools := newJobTools(t, jobs.WithBufferBytes(128), jobs.WithMaxOutputBytes(4096))
	command := "i=0; while [ $i -lt 100 ]; do echo line$i; i=$((i+1)); done"
	jobID := startJob(t, tools, `{"command":"`+jsonEscape(command)+`"}`)
	result := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":5000}`)
	if result.IsError {
		t.Fatalf("job_wait: %q", result.Content)
	}
	if !strings.Contains(result.Content, "truncated") {
		t.Fatalf("ring-buffer truncation not reported:\n%s", result.Content)
	}
	if strings.Contains(result.Content, "line1\n") {
		t.Fatalf("old output should have been discarded:\n%s", result.Content)
	}
}

func TestConcurrentJobs(t *testing.T) {
	manager, tools := newJobTools(t)
	const count = 4
	ids := make([]string, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for index := 0; index < count; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			result, err := tools[jobs.ToolJobStart].Invoke(context.Background(), rig.ToolCall{
				Arguments: `{"command":"echo job` + string(rune('0'+index)) + `; exit ` + string(rune('0'+index)) + `"}`,
			})
			if err != nil {
				errs[index] = err
				return
			}
			if result.IsError {
				errs[index] = nil
				return
			}
			for _, line := range strings.Split(result.Content, "\n") {
				if strings.HasPrefix(line, "job_id: ") {
					ids[index] = strings.TrimSpace(strings.TrimPrefix(line, "job_id: "))
				}
			}
		}(index)
	}
	wg.Wait()
	for index := range ids {
		if errs[index] != nil {
			t.Fatalf("start %d: %v", index, errs[index])
		}
		if !strings.HasPrefix(ids[index], "job-") {
			t.Fatalf("job %d id = %q", index, ids[index])
		}
	}
	if manager.Len() != count {
		t.Fatalf("Len = %d, want %d", manager.Len(), count)
	}
	for index, id := range ids {
		result := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+id+`","timeout_ms":5000}`)
		if result.IsError {
			t.Fatalf("wait %d: %q", index, result.Content)
		}
		if !strings.Contains(result.Content, "job"+string(rune('0'+index))) {
			t.Fatalf("job %d output = %q", index, result.Content)
		}
	}
}

func TestPolicyReadOnlyDisablesJobs(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithReadOnly(true))
	_, tools := newJobTools(t, jobs.WithPolicy(policy))
	text := invokeJobError(t, tools, jobs.ToolJobStart, `{"command":"echo hi"}`)
	if !strings.Contains(text, "read-only") {
		t.Fatalf("read-only start error = %q", text)
	}
}

func TestPolicyCommandAllowlist(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithCommandAllowlist("echo"))
	_, tools := newJobTools(t, jobs.WithPolicy(policy))
	jobID := startJob(t, tools, `{"command":"echo allowed"}`)
	result := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":5000}`)
	if result.IsError || !strings.Contains(result.Content, "allowed") {
		t.Fatalf("allowed job = %q (IsError=%v)", result.Content, result.IsError)
	}
	denied := invokeJobError(t, tools, jobs.ToolJobStart, `{"command":"ls /"}`)
	if !strings.Contains(denied, "not in the allowlist") {
		t.Fatalf("denied job error = %q", denied)
	}
}

func TestPolicyEnvWhitelist(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	t.Setenv("RIG_JOB_VISIBLE", "visible-value")
	t.Setenv("RIG_JOB_HIDDEN", "hidden-value")
	policy := sandbox.New(
		sandbox.WithRoots(realRoot),
		sandbox.WithCommandAllowlist("echo"),
		sandbox.WithEnvAllowlist("RIG_JOB_VISIBLE"),
	)
	_, tools := newJobTools(t, jobs.WithPolicy(policy))
	jobID := startJob(t, tools, `{"command":"echo v=$RIG_JOB_VISIBLE h=$RIG_JOB_HIDDEN"}`)
	result := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":5000}`)
	if result.IsError {
		t.Fatalf("job_wait: %q", result.Content)
	}
	if !strings.Contains(result.Content, "v=visible-value") {
		t.Fatalf("allowed env missing: %q", result.Content)
	}
	if strings.Contains(result.Content, "hidden-value") || !strings.Contains(result.Content, "h=") {
		t.Fatalf("hidden env leaked: %q", result.Content)
	}
}

func TestJobTimeoutAutoKill(t *testing.T) {
	_, tools := newJobTools(t)
	started := time.Now()
	jobID := startJob(t, tools, `{"command":"sleep 30","timeout_ms":100}`)
	result := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":5000}`)
	elapsed := time.Since(started)
	if result.IsError {
		t.Fatalf("job_wait: %q", result.Content)
	}
	if !strings.Contains(result.Content, "state: timed_out") {
		t.Fatalf("timeout state = %q", result.Content)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("auto-kill took %s", elapsed)
	}
}

func TestJobCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	_, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"pwd","cwd":"`+jsonEscape(sub)+`"}`)
	result := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":5000}`)
	if result.IsError || !strings.Contains(result.Content, sub) {
		t.Fatalf("pwd output = %q (IsError=%v)", result.Content, result.IsError)
	}
	// A missing working directory is a start error.
	missing := filepath.Join(root, "missing")
	text := invokeJobError(t, tools, jobs.ToolJobStart, `{"command":"pwd","cwd":"`+jsonEscape(missing)+`"}`)
	if !strings.Contains(text, "cwd") {
		t.Fatalf("missing cwd error = %q", text)
	}
}

func TestMaxJobs(t *testing.T) {
	_, tools := newJobTools(t, jobs.WithMaxJobs(2))
	first := startJob(t, tools, `{"command":"sleep 5"}`)
	startJob(t, tools, `{"command":"sleep 5"}`)
	text := invokeJobError(t, tools, jobs.ToolJobStart, `{"command":"echo third"}`)
	if !strings.Contains(text, "too many") {
		t.Fatalf("max jobs error = %q", text)
	}
	invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+first+`"}`)
}

func TestCloseAll(t *testing.T) {
	manager, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"sleep 30"}`)
	started := time.Now()
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("CloseAll took %s", elapsed)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len after CloseAll = %d", manager.Len())
	}
	text := invokeJobError(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`"}`)
	if !strings.Contains(text, "unknown job") {
		t.Fatalf("wait after CloseAll = %q", text)
	}
}
