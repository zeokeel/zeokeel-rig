package kit_test

// 后台任务对宿主可见：宿主注入的 jobs.Manager 必须就是工具在用的那一个。
//
// 旧行为：`Config.BackgroundJobs` 只把句柄登记给 Shutdown，**不**传给
// full.NewRuntime，于是工具在 kit 自建的管理器里跑，宿主手里的那个永远是空的
// （UI 画不出后台任务列表，只能去解析 job_* 的人读文本）。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// TestKitInjectsHostJobsManager 钉住"注入即生效"：同一个实例，工具跑在它里面。
func TestKitInjectsHostJobsManager(t *testing.T) {
	host := jobs.NewManager()
	agent, err := kit.New(kit.Config{
		Model: "test-model",
		Root:  t.TempDir(),
		Jobs:  host,
		Gate:  gate.AllowAll(),
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()

	if agent.Jobs() != host {
		t.Fatalf("Agent.Jobs() 不是注入的那个管理器：kit 自己又建了一个")
	}

	start, ok := agent.Tool("job_start")
	if !ok {
		t.Fatal("kit 没有装配 job_start")
	}
	payload, err := json.Marshal(map[string]any{"command": "echo kit-jobs-visible"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result, err := start.Invoke(context.Background(), rig.ToolCall{ID: "start", Name: "job_start", Arguments: string(payload)})
	if err != nil {
		t.Fatalf("job_start.Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("job_start 失败: %q", result.Content)
	}

	// 宿主的管理器里必须看得到这个任务（不需要解析文本）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if host.Len() >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("注入的 jobs.Manager 里没有任何任务：工具跑在别的管理器里（Len=%d）", host.Len())
}

// TestKitProvidesJobsManagerWithoutInjection 保证不注入时也有一个可用的、
// 工具真正在用的管理器（默认工具集由 kit 自建，句柄要交还给宿主）。
func TestKitProvidesJobsManagerWithoutInjection(t *testing.T) {
	agent, err := kit.New(kit.Config{Model: "test-model", Root: t.TempDir()})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()

	if agent.Jobs() == nil {
		t.Fatal("Agent.Jobs() 为 nil：宿主拿不到后台任务管理器")
	}
}
