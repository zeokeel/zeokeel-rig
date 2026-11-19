package full_test

// Runtime 的契约：full 构造出来的后台句柄必须有交还路径。
//
// 旧行为是 full.All 内部 jobs.NewManager / terminal.NewManager 之后把句柄丢掉——
// 全仓 CloseAll 只有测试调用，生产路径从来没有关过任何后台进程。
// NewRuntime 把句柄交出来，All 保持原签名不变。

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

// TestNewRuntimeHandsOutBackgroundHandles：两个后台管理器都必须非 nil、真的是
// 本 Runtime 自己的实例，并且 Close 可以重复调用。
func TestNewRuntimeHandsOutBackgroundHandles(t *testing.T) {
	rt, err := full.NewRuntime(full.AllOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if len(rt.Tools) == 0 {
		t.Fatal("NewRuntime 没有返回任何工具")
	}
	if rt.Jobs() == nil {
		t.Fatal("Runtime.Jobs() == nil：jobs.Manager 又被丢掉了（硬阻塞回归）")
	}
	if rt.Terminals() == nil {
		t.Fatal("Runtime.Terminals() == nil：terminal.Manager 又被丢掉了（硬阻塞回归）")
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 幂等：宿主既可能在 defer 里关一次，也可能显式关一次。
	if err := rt.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestNewRuntimeUsesInjectedBackgroundJobs：宿主自己持有 jobs.Manager 时（例如
// 多个工具集共享一个后台池），full 必须用注入的那个，而不是另建一个。
func TestNewRuntimeUsesInjectedBackgroundJobs(t *testing.T) {
	injected := jobs.NewManager()
	rt, err := full.NewRuntime(full.AllOptions{Root: t.TempDir(), BackgroundJobs: injected})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if rt.Jobs() != injected {
		t.Fatal("AllOptions.BackgroundJobs 未被使用：Runtime.Jobs() 不是注入的实例")
	}
	if rt.Terminals() == nil {
		t.Fatal("Runtime.Terminals() == nil")
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestAllStaysBackwardCompatible：All 的签名与工具面不能因为 NewRuntime 而改变，
// 否则所有既有调用点（kit、presets、catalog 覆盖测试）都会受影响。
func TestAllStaysBackwardCompatible(t *testing.T) {
	opts := full.AllOptions{Root: t.TempDir()}
	tools := full.All(opts)
	rt, err := full.NewRuntime(opts)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if len(tools) != len(rt.Tools) {
		t.Fatalf("All 工具数 = %d, NewRuntime 工具数 = %d；两者必须一致", len(tools), len(rt.Tools))
	}
	for i := range tools {
		if tools[i].Name() != rt.Tools[i].Name() {
			t.Fatalf("第 %d 个工具: All = %q, NewRuntime = %q", i, tools[i].Name(), rt.Tools[i].Name())
		}
	}
	// 账本要求的两个后台工具都在（它们背后就是 hand-out 的两个 Manager）。
	found := map[string]bool{}
	for _, tool := range rt.Tools {
		found[tool.Name()] = true
	}
	for _, name := range []string{"job_list", terminal.ToolTerminalOpen} {
		if !found[name] {
			t.Errorf("工具面缺少 %q", name)
		}
	}
}

// TestRuntimeCloseKillsBackgroundJob 是"硬阻塞"的行为验证：Runtime.Close 必须
// 真的杀掉一个正在跑的后台命令，而不只是把句柄交出来放着。
//
// 回归方式：删掉 Runtime.Close（或让它不调 CloseAll），Len() 会停在 1，本用例红。
func TestRuntimeCloseKillsBackgroundJob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("本用例用 /bin/sh 起一个长命令；Windows 上的等价路径由 terminal 测试覆盖")
	}
	rt, err := full.NewRuntime(full.AllOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	job, err := rt.Jobs().StartProcess(background.ProcessSpec{
		Kind:    string(background.KindBash),
		Label:   "sleep 60",
		Command: "sleep 60",
		Argv:    []string{"/bin/sh", "-c", "sleep 60"},
		// 测试只关心"任务被关掉"，不在这里复测策略层（那条路径由 tools/jobs 的
		// 沙箱归属用例覆盖）。
		PolicyChecked: true,
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	if got := rt.Jobs().Len(); got != 1 {
		t.Fatalf("启动之后 Len() = %d, want 1", got)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := rt.Jobs().Len(); got != 0 {
		t.Fatalf("Close 之后 Len() = %d, want 0（后台任务没有被 CloseAll）", got)
	}
	// 任务必须已经在收尾状态；CloseAll 会等进程回收，所以这里不需要轮询。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	view, finished, err := job.Wait(ctx, time.Second)
	if err != nil {
		t.Fatalf("job.Wait: %v", err)
	}
	if !finished {
		t.Fatalf("Close 之后任务仍在运行: %+v", view)
	}
}
