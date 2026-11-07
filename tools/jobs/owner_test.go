package jobs_test

// Owner 作用域测试：任务记录管理器默认 owner 或 StartProcess/StartOperation
// 显式 owner，job_start/job_list 会传播它，Manager.CloseAllOwned 只清理目标
// owner 的任务。

import (
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// startOwnedProcess 通过 background.Manager 契约启动一个 owner 指定的任务。
func startOwnedProcess(t *testing.T, manager *jobs.Manager, owner string) background.Job {
	t.Helper()
	job, err := manager.StartProcess(background.ProcessSpec{
		Kind:    string(background.KindBash),
		Label:   "sleep 30",
		Command: "sleep 30",
		Argv:    []string{"/bin/sh", "-c", "sleep 30"},
		Owner:   owner,
	})
	if err != nil {
		t.Fatalf("StartProcess(owner=%q): %v", owner, err)
	}
	return job
}

func TestJobsOwnerScopeCleanupOnlyTarget(t *testing.T) {
	manager, tools := newJobTools(t, jobs.WithOwner("alice"))
	if got := manager.Owner(); got != "alice" {
		t.Fatalf("Owner() = %q, want alice", got)
	}
	// 工具路径的任务继承管理器默认 owner，并在 job_start/job_list 中传播。
	jobStartResult := invokeJob(t, tools, jobs.ToolJobStart, `{"command":"sleep 30"}`)
	if jobStartResult.IsError {
		t.Fatalf("job_start: %q", jobStartResult.Content)
	}
	if !strings.Contains(jobStartResult.Content, "owner: alice") {
		t.Fatalf("job_start output missing owner: %q", jobStartResult.Content)
	}
	listResult := invokeJob(t, tools, jobs.ToolJobList, `{}`)
	if listResult.IsError || !strings.Contains(listResult.Content, "owner=alice") {
		t.Fatalf("job_list = %q (IsError=%v)", listResult.Content, listResult.IsError)
	}

	// 显式 owner 覆盖管理器默认值；空 owner 回退到默认 owner。
	bob := startOwnedProcess(t, manager, "bob")
	startOwnedProcess(t, manager, "")
	alice := ""
	_ = alice
	if manager.Len() != 3 {
		t.Fatalf("Len = %d, want 3", manager.Len())
	}
	closed, err := manager.CloseAllOwned("bob")
	if err != nil || closed != 1 {
		t.Fatalf("CloseAllOwned(bob) = %d, %v", closed, err)
	}
	if manager.Len() != 2 {
		t.Fatalf("Len after bob cleanup = %d, want 2", manager.Len())
	}
	closed, err = manager.CloseAllOwned("alice")
	if err != nil || closed != 2 {
		t.Fatalf("CloseAllOwned(alice) = %d, %v", closed, err)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len = %d, want 0", manager.Len())
	}
	_ = bob
}

func TestJobsOwnerEmptyScopeCleanup(t *testing.T) {
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	empty := startOwnedProcess(t, manager, "")
	owned := startOwnedProcess(t, manager, "bob")
	closed, err := manager.CloseAllOwned("")
	if err != nil || closed != 1 {
		t.Fatalf(`CloseAllOwned("") = %d, %v`, closed, err)
	}
	if manager.Len() != 1 {
		t.Fatalf("Len = %d, want only the bob job", manager.Len())
	}
	closed, err = manager.CloseAllOwned("bob")
	if err != nil || closed != 1 {
		t.Fatalf("CloseAllOwned(bob) = %d, %v", closed, err)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len = %d, want 0", manager.Len())
	}
	_ = empty
	_ = owned
}

func TestJobsOwnerOperationScope(t *testing.T) {
	manager := jobs.NewManager(jobs.WithOwner("alice"))
	t.Cleanup(func() { _ = manager.CloseAll() })
	operation := newFakeOperation()
	source := newFakeSource()
	if _, err := manager.StartOperation(background.OperationSpec{
		Kind:    string(background.KindPTYSend),
		Label:   "term-owner: sleep",
		Sources: []background.Source{source},
		Run:     func() (background.Operation, error) { return operation, nil },
		Owner:   "carol",
	}); err != nil {
		t.Fatalf("StartOperation: %v", err)
	}
	// 显式 owner 覆盖默认 owner：只有 carol 的任务被清理。
	if closed, err := manager.CloseAllOwned("alice"); err != nil || closed != 0 {
		t.Fatalf("CloseAllOwned(alice) = %d, %v", closed, err)
	}
	closed, err := manager.CloseAllOwned("carol")
	if err != nil || closed != 1 {
		t.Fatalf("CloseAllOwned(carol) = %d, %v", closed, err)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len = %d, want 0", manager.Len())
	}
}

func TestJobsOwnerScopeConcurrent(t *testing.T) {
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	const count = 8
	jobsByOwner := make([]background.Job, count)
	var wg sync.WaitGroup
	for index := 0; index < count; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			owner := "alice"
			if index%2 == 1 {
				owner = "bob"
			}
			job, err := manager.StartProcess(background.ProcessSpec{
				Kind:    string(background.KindBash),
				Label:   "sleep 30",
				Command: "sleep 30",
				Argv:    []string{"/bin/sh", "-c", "sleep 30"},
				Owner:   owner,
			})
			if err != nil {
				t.Errorf("StartProcess #%d: %v", index, err)
				return
			}
			jobsByOwner[index] = job
		}(index)
	}
	wg.Wait()

	var closedAlice, closedBob int
	var errAlice, errBob error
	wg.Add(2)
	go func() {
		defer wg.Done()
		closedAlice, errAlice = manager.CloseAllOwned("alice")
	}()
	go func() {
		defer wg.Done()
		closedBob, errBob = manager.CloseAllOwned("bob")
	}()
	wg.Wait()
	if errAlice != nil || errBob != nil {
		t.Fatalf("concurrent CloseAllOwned errors: %v / %v", errAlice, errBob)
	}
	if closedAlice != count/2 || closedBob != count/2 {
		t.Fatalf("closed alice/bob = %d/%d, want %d/%d", closedAlice, closedBob, count/2, count/2)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len = %d, want 0", manager.Len())
	}
	// CloseAllOwned 已移除的任务可安全 Drop（幂等）。
	for _, job := range jobsByOwner {
		if job != nil {
			_ = job.Drop()
		}
	}
}
