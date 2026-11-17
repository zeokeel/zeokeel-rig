package jobs_test

// job_kill 的顺序语义审计。
//
// 关注的不是"能不能杀"，而是**报告的状态是否与事实一致**：
//   - 对已经结束的任务发 kill，必须报 state: exited + outcome: already-finished，
//     绝不能谎报 killed（那会让模型以为它终止了某个还在跑的东西）；
//   - 对仍在运行的操作任务发 kill，必须把原因转发给生产者，并在生产者报告 killed
//     之后结算成 killed；
//   - 生产者报告 completed 的任务被再 kill 一次，不得被改写成 killed。

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// auditOperation 是可控的操作任务生产者（Cancel 不会自动结算，便于验证顺序）。
type auditOperation struct {
	done chan background.Outcome

	mu      sync.Mutex
	cancels []string
}

func newAuditOperation(done chan background.Outcome) *auditOperation {
	return &auditOperation{done: done}
}

func (f *auditOperation) Done() <-chan background.Outcome { return f.done }

func (f *auditOperation) Cancel(reason string) {
	f.mu.Lock()
	f.cancels = append(f.cancels, reason)
	f.mu.Unlock()
}

func (f *auditOperation) cancelReasons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancels...)
}

// auditFinishedSource 是"立刻结束"的输出源。
type auditFinishedSource struct{}

func (auditFinishedSource) Read(context.Context) (string, bool, error) { return "", true, nil }

// startAuditOperation 把一个 auditOperation 登记成后台任务并返回任务 id。
func startAuditOperation(t *testing.T, manager *jobs.Manager, operation *auditOperation) string {
	t.Helper()
	job, err := manager.StartOperation(background.OperationSpec{
		Kind:    "audit-fake",
		Label:   "audit fake operation",
		Sources: []background.Source{auditFinishedSource{}},
		Run:     func() (background.Operation, error) { return operation, nil },
	})
	if err != nil {
		t.Fatalf("StartOperation: %v", err)
	}
	return job.ID()
}

// TestJobKillAfterNaturalExitStaysExited 覆盖进程任务：自然结束后再 kill，
// 必须报 exited（不是 killed）。
func TestJobKillAfterNaturalExitStaysExited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 sh -c")
	}
	_, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"sh -c 'exit 0'"}`)

	// 等到任务真的结束（wait=true 会等到终态）。
	waited := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if waited.IsError || !strings.Contains(waited.Content, "state: exited") {
		t.Fatalf("job_output = %q", waited.Content)
	}

	killed := invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","reason":"too late"}`)
	for _, fragment := range []string{"outcome: already-finished", "state: exited"} {
		if !strings.Contains(killed.Content, fragment) {
			t.Fatalf("kill of an exited job missing %q:\n%s", fragment, killed.Content)
		}
	}
	if strings.Contains(killed.Content, "state: killed") {
		t.Fatalf("kill of an exited job must not claim killed:\n%s", killed.Content)
	}
	// 退出码也要是真实的那一个，而不是"被杀"的 -1。
	if !strings.Contains(killed.Content, "exit_code: 0") {
		t.Fatalf("exit code rewritten by a late kill:\n%s", killed.Content)
	}
}

// TestJobKillSettledOperationIsNotReportedAsKilled 覆盖操作任务：生产者已经报告
// completed 之后，kill 不得改写状态，也不得再调用生产者的 Cancel。
func TestJobKillSettledOperationIsNotReportedAsKilled(t *testing.T) {
	manager, tools := newJobTools(t)
	operation := newAuditOperation(make(chan background.Outcome, 1))
	operation.done <- background.Outcome{Status: background.StatusCompleted, Detail: "wait: inferred_idle"}
	jobID := startAuditOperation(t, manager, operation)

	waited := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if waited.IsError || !strings.Contains(waited.Content, "state: exited") {
		t.Fatalf("job_output = %q", waited.Content)
	}

	killed := invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","reason":"late"}`)
	if !strings.Contains(killed.Content, "outcome: already-finished") || !strings.Contains(killed.Content, "state: exited") {
		t.Fatalf("kill of a settled operation = %q", killed.Content)
	}
	if strings.Contains(killed.Content, "state: killed") {
		t.Fatalf("kill of a settled operation must not claim killed: %q", killed.Content)
	}
	if reasons := operation.cancelReasons(); len(reasons) != 0 {
		t.Fatalf("Cancel 被调用在已结束的操作上: %v", reasons)
	}
}

// TestJobKillRunningOperationReportsKilled 覆盖操作任务的正向路径：kill 必须
// 立刻把原因转发给生产者，并在生产者报告 killed 后结算成 killed。
func TestJobKillRunningOperationReportsKilled(t *testing.T) {
	manager, tools := newJobTools(t)
	operation := newAuditOperation(make(chan background.Outcome, 1))
	jobID := startAuditOperation(t, manager, operation)

	// 先确认任务在跑（不等待，读到 state: running 即可）。
	first := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":false}`)
	if first.IsError || !strings.Contains(first.Content, "state: running") {
		t.Fatalf("job_output = %q", first.Content)
	}

	// kill 会转发 Cancel，然后最多等 killGrace；生产者必须在这个窗口内报告 killed。
	killDone := make(chan rig.ToolResult, 1)
	go func() {
		killDone <- invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","reason":"no longer needed"}`)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if reasons := operation.cancelReasons(); len(reasons) > 0 {
			if reasons[0] != "no longer needed" {
				t.Fatalf("Cancel reason = %q", reasons[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("kill 没有把原因转发给生产者")
		}
		time.Sleep(2 * time.Millisecond)
	}
	operation.done <- background.Outcome{Status: background.StatusKilled, Detail: "signal: SIGINT"}
	close(operation.done)

	result := <-killDone
	if result.IsError || !strings.Contains(result.Content, "state: killed") {
		t.Fatalf("kill of a running operation = %q", result.Content)
	}
	if !strings.Contains(result.Content, "no longer needed") {
		t.Fatalf("kill result missing the reason: %q", result.Content)
	}
}

// TestJobKillCompletedOperationKeepsCompletedStatus 覆盖"生产者报告 completed，
// 但 kill 在结算之后到达"的边界：状态必须是 completed 而不是 killed。
func TestJobKillCompletedOperationKeepsCompletedStatus(t *testing.T) {
	manager, tools := newJobTools(t)
	operation := newAuditOperation(make(chan background.Outcome, 1))
	jobID := startAuditOperation(t, manager, operation)

	// 先 kill（此时在跑），再让生产者报告 completed（而不是 killed）。
	killDone := make(chan rig.ToolResult, 1)
	go func() {
		killDone <- invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","reason":"race"}`)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(operation.cancelReasons()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("kill 没有转发到生产者")
		}
		time.Sleep(2 * time.Millisecond)
	}
	operation.done <- background.Outcome{Status: background.StatusCompleted, Detail: "finished anyway"}
	close(operation.done)

	result := <-killDone
	// 规则：终态**以生产者的报告为准**。kill 只是"请求"，不能把已完成的命令
	// 改写成 killed。
	if strings.Contains(result.Content, "state: killed") {
		t.Fatalf("生产者报告 completed，但任务被标成 killed:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "state: exited") {
		t.Fatalf("生产者的 completed 没有体现在状态里:\n%s", result.Content)
	}
}
