package jobs_test

// DSH-compatibility tests for the background-job manager: job_output(wait,
// timeout_ms), job_kill(reason), kind/label/status reporting, completion
// notices, awaited-settlement suppression and the producer-facing
// background.Manager contract used by the shell and terminal tools.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// waitForState polls job_output until the rendered state matches want or the
// deadline expires.
func waitForState(t *testing.T, tools map[string]rig.Tool, jobID, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`"}`).Content
		if strings.Contains(last, "state: "+want) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never reached state %q; last output:\n%s", jobID, want, last)
	return ""
}

func TestJobOutputWaitSettles(t *testing.T) {
	_, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"echo waited; exit 2"}`)
	started := time.Now()
	result := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	elapsed := time.Since(started)
	if result.IsError {
		t.Fatalf("job_output(wait): %q", result.Content)
	}
	for _, fragment := range []string{"state: exited", "exit_code: 2", "waited", "status: completed"} {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("job_output(wait) missing %q:\n%s", fragment, result.Content)
		}
	}
	if elapsed > 3*time.Second {
		t.Fatalf("job_output(wait) took %s", elapsed)
	}
}

func TestJobOutputWaitTimeoutLeavesJobRunning(t *testing.T) {
	_, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"sleep 30"}`)
	started := time.Now()
	result := invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":100}`)
	elapsed := time.Since(started)
	if result.IsError {
		t.Fatalf("job_output(wait): %q", result.Content)
	}
	if !strings.Contains(result.Content, "state: running") {
		t.Fatalf("timed-out wait should report a running job: %q", result.Content)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("timed-out wait took %s", elapsed)
	}
	invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`"}`)
}

func TestJobKillReasonIsRecordedAndForwarded(t *testing.T) {
	manager, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"sleep 30"}`)
	time.Sleep(50 * time.Millisecond)
	result := invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","reason":"no longer needed"}`)
	if result.IsError {
		t.Fatalf("job_kill: %q", result.Content)
	}
	for _, fragment := range []string{"outcome: cancellation-requested", "state: killed", "no longer needed"} {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("job_kill missing %q:\n%s", fragment, result.Content)
		}
	}
	// A second kill reports the already final state.
	again := invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","reason":"again"}`)
	if again.IsError || !strings.Contains(again.Content, "outcome: already-finished") || !strings.Contains(again.Content, "state: killed") {
		t.Fatalf("second job_kill = %q (IsError=%v)", again.Content, again.IsError)
	}
	// The reason is retained in the job record.
	list := invokeJob(t, tools, jobs.ToolJobList, `{}`)
	if list.IsError || !strings.Contains(list.Content, "[bash]") || !strings.Contains(list.Content, "no longer needed") {
		t.Fatalf("job_list = %q (IsError=%v)", list.Content, list.IsError)
	}
	_ = manager
}

func TestJobListReportsKindLabelAndStatus(t *testing.T) {
	_, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"echo listed"}`)
	waitForState(t, tools, jobID, "exited")
	list := invokeJob(t, tools, jobs.ToolJobList, `{}`)
	for _, fragment := range []string{"[bash]", "status=completed", "command=echo listed"} {
		if !strings.Contains(list.Content, fragment) {
			t.Fatalf("job_list missing %q:\n%s", fragment, list.Content)
		}
	}
}

func TestCompletionNoticeForUnawaitedJob(t *testing.T) {
	manager, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"echo notice-me"}`)
	// Reading without waiting does not mark the settlement awaited.
	waitForState(t, tools, jobID, "exited")
	var notices []jobs.Notice
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if notices = manager.PendingNotices(); len(notices) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(notices) != 1 {
		t.Fatalf("PendingNotices = %d, want 1", len(notices))
	}
	notice := notices[0]
	if notice.JobID != jobID || notice.Kind != "bash" || notice.Status != background.StatusCompleted {
		t.Fatalf("notice = %+v", notice)
	}
	for _, fragment := range []string{"background job " + jobID, "(bash: echo notice-me)", "finished [status: completed", "job_output"} {
		if !strings.Contains(notice.Text, fragment) {
			t.Fatalf("notice text %q missing %q", notice.Text, fragment)
		}
	}
	// Draining is idempotent.
	if again := manager.PendingNotices(); len(again) != 0 {
		t.Fatalf("second PendingNotices = %v, want empty", again)
	}
}

func TestAwaitedSettlementSuppressesNotice(t *testing.T) {
	manager, tools := newJobTools(t)
	// A short-lived but not instant command: the job must still be running when
	// the waiter registers, otherwise it may settle first and legitimately queue
	// a completion notice (the -race suite made that race observable).
	jobID := startJob(t, tools, `{"command":"sleep 0.2"}`)
	// A live wait collects the settlement: no completion notice follows.
	result := invokeJob(t, tools, jobs.ToolJobWait, `{"job_id":"`+jobID+`","timeout_ms":5000}`)
	if result.IsError || !strings.Contains(result.Content, "state: exited") {
		t.Fatalf("job_wait = %q (IsError=%v)", result.Content, result.IsError)
	}
	if notices := manager.PendingNotices(); len(notices) != 0 {
		t.Fatalf("awaited settlement queued notices: %+v", notices)
	}

	// job_output(wait) is the other awaited path.
	second := startJob(t, tools, `{"command":"sleep 0.2"}`)
	invokeJob(t, tools, jobs.ToolJobOutput, `{"job_id":"`+second+`","wait":true,"timeout_ms":5000}`)
	if notices := manager.PendingNotices(); len(notices) != 0 {
		t.Fatalf("job_output(wait) queued notices: %+v", notices)
	}
}

func TestKilledJobSuppressesNotice(t *testing.T) {
	manager, tools := newJobTools(t)
	jobID := startJob(t, tools, `{"command":"sleep 30"}`)
	time.Sleep(50 * time.Millisecond)
	invokeJob(t, tools, jobs.ToolJobKill, `{"job_id":"`+jobID+`"}`)
	time.Sleep(50 * time.Millisecond)
	if notices := manager.PendingNotices(); len(notices) != 0 {
		t.Fatalf("model kill queued notices: %+v", notices)
	}
}

func TestNoticeHandlerReceivesNotice(t *testing.T) {
	received := make(chan jobs.Notice, 1)
	manager := jobs.NewManager(jobs.WithNoticeHandler(func(notice jobs.Notice) {
		select {
		case received <- notice:
		default:
		}
	}))
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		tools[tool.Name()] = tool
	}
	jobID := startJob(t, tools, `{"command":"echo handler"}`)
	waitForState(t, tools, jobID, "exited")
	select {
	case notice := <-received:
		if notice.JobID != jobID {
			t.Fatalf("handler notice = %+v", notice)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("notice handler was not called")
	}
	if pending := manager.PendingNotices(); len(pending) != 1 {
		t.Fatalf("handler delivery should not suppress the pending queue: %+v", pending)
	}
}

// --- producer-facing background.Manager contract --------------------------

func startBackgroundProcess(t *testing.T, manager *jobs.Manager, command string) background.Job {
	t.Helper()
	job, err := manager.StartProcess(background.ProcessSpec{
		Kind:    string(background.KindBash),
		Label:   command,
		Command: command,
		Argv:    []string{"/bin/sh", "-c", command},
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	return job
}

func TestStartProcessJobLifecycle(t *testing.T) {
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	job := startBackgroundProcess(t, manager, "echo producer; exit 5")
	if !strings.HasPrefix(job.ID(), "bash-") {
		t.Fatalf("job id = %q, want bash- prefix", job.ID())
	}
	view, settled, err := job.Wait(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !settled || view.Status != background.StatusCompleted || view.ExitCode != 5 {
		t.Fatalf("Wait view = %+v settled=%v", view, settled)
	}
	output, _ := job.Read()
	if !strings.Contains(output, "producer") {
		t.Fatalf("Read = %q", output)
	}
	if manager.Len() != 1 {
		t.Fatalf("Len = %d, want 1", manager.Len())
	}
	// A foreground caller that collected the job drops it; the model never sees
	// its id.
	if err := job.Drop(); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len after Drop = %d, want 0", manager.Len())
	}
	if notices := manager.PendingNotices(); len(notices) != 0 {
		t.Fatalf("awaited foreground job queued notices: %+v", notices)
	}
}

func TestStartProcessCancel(t *testing.T) {
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	job := startBackgroundProcess(t, manager, "sleep 30")
	view, settled, err := job.Wait(context.Background(), 100*time.Millisecond)
	if err != nil || settled || view.Status != background.StatusRunning {
		t.Fatalf("Wait(running) = %+v settled=%v err=%v", view, settled, err)
	}
	job.Cancel("test cancel")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, _, _ = job.Wait(context.Background(), time.Second)
		if view.Status.Settled() {
			break
		}
	}
	if view.Status != background.StatusKilled {
		t.Fatalf("canceled job status = %s", view.Status)
	}
	if !strings.Contains(view.Detail, "test cancel") {
		t.Fatalf("cancel detail = %q", view.Detail)
	}
	if err := job.Drop(); err != nil {
		t.Fatalf("Drop: %v", err)
	}
}

// fakeOperation is a producer operation controlled by the test.
type fakeOperation struct {
	done      chan background.Outcome
	cancelMu  sync.Mutex
	reason    string
	cancelled bool
}

func newFakeOperation() *fakeOperation {
	return &fakeOperation{done: make(chan background.Outcome, 1)}
}

func (o *fakeOperation) Cancel(reason string) {
	o.cancelMu.Lock()
	if !o.cancelled {
		o.cancelled = true
		o.reason = reason
	}
	o.cancelMu.Unlock()
	o.settle(background.Outcome{Status: background.StatusKilled, Detail: "signal: SIGINT"})
}

func (o *fakeOperation) settle(outcome background.Outcome) {
	select {
	case o.done <- outcome:
	default:
	}
}

func (o *fakeOperation) Done() <-chan background.Outcome { return o.done }

// fakeSource is a scripted pull source.
type fakeSource struct {
	mu     sync.Mutex
	chunks []string
	closed chan struct{}
}

func newFakeSource() *fakeSource {
	return &fakeSource{closed: make(chan struct{})}
}

func (s *fakeSource) push(text string) {
	s.mu.Lock()
	s.chunks = append(s.chunks, text)
	close(s.closed)
	s.closed = make(chan struct{})
	s.mu.Unlock()
}

func (s *fakeSource) Read(ctx context.Context) (string, bool, error) {
	for {
		s.mu.Lock()
		if len(s.chunks) > 0 {
			text := s.chunks[0]
			s.chunks = s.chunks[1:]
			s.mu.Unlock()
			return text, false, nil
		}
		wait := s.closed
		s.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return "", true, nil
		}
	}
}

func TestStartOperationJobLifecycle(t *testing.T) {
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	operation := newFakeOperation()
	source := newFakeSource()
	job, err := manager.StartOperation(background.OperationSpec{
		Kind:    string(background.KindPTYSend),
		Label:   "term-1: echo hi",
		Sources: []background.Source{source},
		Run: func() (background.Operation, error) {
			go func() {
				source.push("hello from pty\n")
				operation.settle(background.Outcome{Status: background.StatusCompleted, Detail: "wait: inferred_idle"})
			}()
			return operation, nil
		},
	})
	if err != nil {
		t.Fatalf("StartOperation: %v", err)
	}
	if !strings.HasPrefix(job.ID(), "pty-send-") {
		t.Fatalf("job id = %q", job.ID())
	}
	view, settled, err := job.Wait(context.Background(), 5*time.Second)
	if err != nil || !settled {
		t.Fatalf("Wait settled=%v err=%v", settled, err)
	}
	if view.Status != background.StatusCompleted {
		t.Fatalf("status = %s", view.Status)
	}
	// The source tail is drained before the ring closes.
	output, _ := job.Read()
	if !strings.Contains(output, "hello from pty") {
		t.Fatalf("Read = %q", output)
	}
	if err := job.Drop(); err != nil {
		t.Fatalf("Drop: %v", err)
	}
}

func TestStartOperationCancelForwardsReason(t *testing.T) {
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	operation := newFakeOperation()
	source := newFakeSource()
	job, err := manager.StartOperation(background.OperationSpec{
		Kind:    string(background.KindPTYSend),
		Label:   "term-2: sleep",
		Sources: []background.Source{source},
		Run:     func() (background.Operation, error) { return operation, nil },
	})
	if err != nil {
		t.Fatalf("StartOperation: %v", err)
	}
	job.Cancel("stop the send")
	view, _, err := job.Wait(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if view.Status != background.StatusKilled {
		t.Fatalf("status = %s", view.Status)
	}
	operation.cancelMu.Lock()
	reason := operation.reason
	operation.cancelMu.Unlock()
	if reason != "stop the send" {
		t.Fatalf("cancel reason = %q", reason)
	}
	if err := job.Drop(); err != nil {
		t.Fatalf("Drop: %v", err)
	}
}

func TestStartOperationUnawaitedSendsNotice(t *testing.T) {
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	operation := newFakeOperation()
	source := newFakeSource()
	job, err := manager.StartOperation(background.OperationSpec{
		Kind:    string(background.KindPTYSend),
		Label:   "term-3: done",
		Sources: []background.Source{source},
		Run: func() (background.Operation, error) {
			go operation.settle(background.Outcome{Status: background.StatusCompleted, Detail: "wait: session_exit"})
			return operation, nil
		},
	})
	if err != nil {
		t.Fatalf("StartOperation: %v", err)
	}
	// Wait for settlement through a read loop, which is not an awaited wait.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, view := job.Read(); view.Status.Settled() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var notices []background.Notice
	for time.Now().Before(deadline) {
		if notices = manager.PendingNotices(); len(notices) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(notices) != 1 || notices[0].Kind != string(background.KindPTYSend) {
		t.Fatalf("notices = %+v", notices)
	}
}
