package jobs_test

// 围栏解析器（jobs.WithScopeResolver）下的 owner 契约：
//
// 共享任务池是给 kit.Runtime 用的，池子本身**没有**默认策略；一个任务该被哪份
// policy/runner 围住，只能由调用方 owner 指定。因此：
//   - 配了 resolver、owner 为空、而生产者又没自己包装 argv（PolicyChecked=false）
//     → fail closed（否则宿主用公开的 Agent.Jobs().StartProcess 就能裸跑，还丢掉
//     策略输出上限）；
//   - 生产路径（builtin bash/pwsh 的后台命令，PolicyChecked=true 且 argv 已被
//     runner 包装）不带 owner 也必须继续工作——它是"已经围好栏"的调用；
//   - 显式给了 owner 就按该 owner 的 scope 执行。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// resolverManager 造一个"每个 owner 一份策略"的共享管理器（Runtime 的形状）。
func resolverManager(t *testing.T, root string) *jobs.Manager {
	t.Helper()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithAnyCommandAllowed(true))
	manager := jobs.NewManager(jobs.WithScopeResolver(func(owner string) (jobs.JobScope, bool) {
		if owner != "agent-1" {
			return jobs.JobScope{}, false
		}
		return jobs.JobScope{Policy: policy}, true
	}))
	t.Cleanup(func() { _ = manager.CloseAll() })
	return manager
}

// TestStartProcessRejectsEmptyOwnerWithScopeResolver：owner 为空时不能回落到
// "管理器没有策略"的裸跑路径。
func TestStartProcessRejectsEmptyOwnerWithScopeResolver(t *testing.T) {
	manager := resolverManager(t, t.TempDir())
	_, err := manager.StartProcess(background.ProcessSpec{
		Kind:    string(background.KindBash),
		Label:   "no-owner",
		Command: "echo no-owner",
		Argv:    []string{"/bin/sh", "-c", "echo no-owner"},
	})
	if err == nil {
		t.Fatal("owner 为空时 StartProcess 起了一个无围栏的后台任务")
	}
	for _, fragment := range []string{"kit.Runtime", "owning agent's id"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("拒绝文案缺少 %q: %v", fragment, err)
		}
	}
	if manager.Len() != 0 {
		t.Fatalf("被拒的任务仍然登记进了池子（Len = %d）", manager.Len())
	}
}

// TestStartProcessAllowsPolicyCheckedWithoutOwner：生产路径不能被上面那条围栏
// 打死——builtin 的后台命令自带 PolicyChecked 与已包装 argv。
func TestStartProcessAllowsPolicyCheckedWithoutOwner(t *testing.T) {
	manager := resolverManager(t, t.TempDir())
	job, err := manager.StartProcess(background.ProcessSpec{
		Kind:          string(background.KindBash),
		Label:         "policy-checked",
		Command:       "echo checked",
		Argv:          []string{"/bin/sh", "-c", "echo checked"},
		PolicyChecked: true,
	})
	if err != nil {
		t.Fatalf("PolicyChecked 的生产路径被拒: %v", err)
	}
	view, settled, err := job.Wait(context.Background(), 5*time.Second)
	if err != nil || !settled || view.Status != background.StatusCompleted {
		t.Fatalf("Wait = %+v settled=%v err=%v", view, settled, err)
	}
	output, _ := job.Read()
	if !strings.Contains(output, "checked") {
		t.Fatalf("Read = %q", output)
	}
}

// TestStartProcessUsesExplicitOwnerScope：给了 owner 就用该 owner 的 scope——
// 任务 cwd 落在策略根内，命令被允许。
func TestStartProcessUsesExplicitOwnerScope(t *testing.T) {
	root := t.TempDir()
	manager := resolverManager(t, root)
	job, err := manager.StartProcess(background.ProcessSpec{
		Kind:    string(background.KindBash),
		Label:   "owned",
		Command: "echo owned",
		Argv:    []string{"/bin/sh", "-c", "echo owned"},
		Dir:     root,
		Owner:   "agent-1",
	})
	if err != nil {
		t.Fatalf("StartProcess(owner=agent-1): %v", err)
	}
	view, settled, err := job.Wait(context.Background(), 5*time.Second)
	if err != nil || !settled || view.Status != background.StatusCompleted {
		t.Fatalf("Wait = %+v settled=%v err=%v", view, settled, err)
	}
}
