package jobs_test

// 跨 owner 围栏：任务池跨 Agent 共享（job_list/job_output 可见），但 job_kill 是
// **写操作**——它给别人的进程投 SIGKILL/改任务终态。与 terminal_send/terminal_signal/
// terminal_close 同口径：只接受启动该任务的同一个 Agent。

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// jobToolForOwner 从绑定到某个 owner 的工具集中取一个任务工具。
func jobToolForOwner(t *testing.T, manager *jobs.Manager, owner, name string) rig.Tool {
	t.Helper()
	for _, tool := range manager.ToolsFor(owner) {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found for owner %q", name, owner)
	return nil
}

// invokeOwnedJob 用一个已绑定 owner 的工具执行一次调用。
func invokeOwnedJob(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", tool.Name(), err)
	}
	return result
}

func TestJobKillRejectsCrossOwnerJob(t *testing.T) {
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	owned := startOwnedProcess(t, manager, "a1")

	kill := jobToolForOwner(t, manager, "a2", jobs.ToolJobKill)
	result := invokeOwnedJob(t, kill, `{"job_id":"`+owned.ID()+`","reason":"cross owner"}`)
	if !result.IsError {
		t.Fatalf("a2 杀掉了 a1 的后台任务（跨 owner 写操作）: %q", result.Content)
	}
	for _, fragment := range []string{"another agent", "started it"} {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("job_kill 的拒绝文案缺少 %q: %q", fragment, result.Content)
		}
	}
	// 任务没被改动：仍在运行，SIGKILL 没投出去。
	if state := jobStateOf(t, manager, owned.ID()); state != "running" {
		t.Fatalf("被拒的跨 owner job_kill 改动了任务状态: %s", state)
	}
	// 共享池的可见性不变：a2 仍能在 job_list 里看到 a1 的任务。
	listed := invokeOwnedJob(t, jobToolForOwner(t, manager, "a2", jobs.ToolJobList), `{}`)
	if listed.IsError || !strings.Contains(listed.Content, owned.ID()) {
		t.Fatalf("job_list 看不到共享池里的任务（可见性被误改）: %q", listed.Content)
	}
}

func TestJobKillAllowsSameOwner(t *testing.T) {
	manager := jobs.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	owned := startOwnedProcess(t, manager, "a1")

	kill := jobToolForOwner(t, manager, "a1", jobs.ToolJobKill)
	result := invokeOwnedJob(t, kill, `{"job_id":"`+owned.ID()+`","reason":"own job"}`)
	if result.IsError {
		t.Fatalf("a1 杀不掉自己的任务: %q", result.Content)
	}
	if status := jobStateOf(t, manager, owned.ID()); status != "killed" {
		t.Fatalf("同 owner 的 job_kill 之后状态 = %s, want killed（%q）", status, result.Content)
	}
}

// jobState 从管理器的宿主视图里取某个任务的 state 词。
func jobStateOf(t *testing.T, manager *jobs.Manager, id string) string {
	t.Helper()
	for _, snapshot := range manager.Snapshot() {
		if snapshot.ID == id {
			return snapshot.State
		}
	}
	t.Fatalf("任务 %s 不在管理器的视图里", id)
	return ""
}
