package kit_test

// D1：`kit.Runtime` —— 长生命周期共享服务 + 每会话/每 Agent 轻量重建。
//
// 这些用例钉住四件事：
//  1. 进程级资源**真的**共享（后台任务跨 Agent 可见）；
//  2. 每 Agent 的状态**真的**独立（Policy/Mode/工具面互不影响）；
//  3. `Close` 是硬边界：之后不能再建 Agent，已存在的 Agent 也不能再跑；
//  4. 并发 `NewAgent` 不 panic、不产生"半个"结果（`-race` 不可用时靠这层断言）。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// runtimeInvoke 调一次工具并把参数编码成 JSON。
func runtimeInvoke(t *testing.T, tool rig.Tool, args map[string]any) rig.ToolResult {
	t.Helper()
	payload, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID:        "call-" + tool.Name(),
		Name:      tool.Name(),
		Arguments: string(payload),
	})
	if err != nil {
		t.Fatalf("%s.Invoke: %v", tool.Name(), err)
	}
	return result
}

// TestRuntimeSharesBackgroundJobsAcrossAgents：a1 起的后台任务，a2 的 job_list
// 必须看得见——这是"后台任务跨会话"的核心承诺。
func TestRuntimeSharesBackgroundJobsAcrossAgents(t *testing.T) {
	root := t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	cfg := kit.Config{Model: "test-model", Root: root, Gate: gate.AllowAll()}
	a1, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(a1): %v", err)
	}
	t.Cleanup(func() { _ = a1.Shutdown(context.Background()) })
	a2, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(a2): %v", err)
	}
	t.Cleanup(func() { _ = a2.Shutdown(context.Background()) })

	if a1.Jobs() == nil || a2.Jobs() == nil {
		t.Fatalf("Agent.Jobs() 为 nil（a1=%v a2=%v）：Runtime 必须交出共享管理器", a1.Jobs(), a2.Jobs())
	}
	if a1.Jobs() != a2.Jobs() {
		t.Fatalf("两个 Agent 的 jobs.Manager 不是同一个实例：a1=%p a2=%p", a1.Jobs(), a2.Jobs())
	}

	start, ok := a1.Tool("job_start")
	if !ok {
		t.Fatal("a1 没有装配 job_start")
	}
	started := runtimeInvoke(t, start, map[string]any{"command": "echo runtime-shared-job"})
	if started.IsError {
		t.Fatalf("job_start 失败: %q", started.Content)
	}
	data, ok := started.Data.(map[string]any)
	if !ok {
		t.Fatalf("job_start 结果缺少结构化数据: %#v", started.Data)
	}
	jobID, _ := data["id"].(string)
	if jobID == "" {
		t.Fatalf("job_start 没有返回 id: %#v", data)
	}

	list, ok := a2.Tool("job_list")
	if !ok {
		t.Fatal("a2 没有装配 job_list")
	}
	listed := runtimeInvoke(t, list, map[string]any{})
	if listed.IsError {
		t.Fatalf("a2.job_list 失败: %q", listed.Content)
	}
	if !strings.Contains(listed.Content, jobID) {
		t.Fatalf("a2 的 job_list 看不到 a1 的任务 %q:\n%s", jobID, listed.Content)
	}
}

// TestRuntimeNewAgentUsesOwnPolicyAndSession：每个 Agent 的 Policy/Mode 由自己的
// Config 决定，互不影响；工具面裁剪也跟着各自的策略走。
func TestRuntimeNewAgentUsesOwnPolicyAndSession(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	writable, err := rt.NewAgent(kit.Config{
		Model: "test-model", Root: rootA,
	})
	if err != nil {
		t.Fatalf("NewAgent(workspace-write): %v", err)
	}
	t.Cleanup(func() { _ = writable.Shutdown(context.Background()) })
	readOnly, err := rt.NewAgent(kit.Config{
		Model: "test-model", Root: rootB,
		Mode: sandbox.ModeReadOnly,
	})
	if err != nil {
		t.Fatalf("NewAgent(read-only): %v", err)
	}
	t.Cleanup(func() { _ = readOnly.Shutdown(context.Background()) })

	if writable.Policy() == nil || readOnly.Policy() == nil {
		t.Fatal("Agent.Policy() 为 nil")
	}
	if writable.Policy() == readOnly.Policy() {
		t.Fatal("两个 Agent 共享了同一个 *sandbox.Policy：策略必须每 Agent 一份")
	}
	if got := writable.Policy().Mode(); got != sandbox.ModeWorkspaceWrite {
		t.Fatalf("a1 的 Mode = %q, want %q", got, sandbox.ModeWorkspaceWrite)
	}
	if got := readOnly.Policy().Mode(); got != sandbox.ModeReadOnly {
		t.Fatalf("a2 的 Mode = %q, want %q（策略被另一个 Agent 影响了）", got, sandbox.ModeReadOnly)
	}
	rootsA, rootsB := writable.Policy().Roots(), readOnly.Policy().Roots()
	if len(rootsA) == 0 || len(rootsB) == 0 || rootsA[0] == rootsB[0] {
		t.Fatalf("两个 Agent 的 workspace roots 没有按各自 Config 生效：%v vs %v", rootsA, rootsB)
	}

	// 工具面同样按各自的 Mode 裁剪：read-only 不注册纯写工具。
	if _, ok := writable.Tool("write_file"); !ok {
		t.Fatal("workspace-write 的 Agent 没有 write_file")
	}
	if _, ok := readOnly.Tool("write_file"); ok {
		t.Fatal("read-only 的 Agent 仍然注册了 write_file：Mode 裁剪串到别的 Agent 上去了")
	}
	if writable.PermissionPreset() == readOnly.PermissionPreset() {
		t.Fatalf("两个 Agent 的 PermissionPreset 相同（%q）：策略档位没有按 Agent 生效",
			writable.PermissionPreset())
	}
}

// TestRuntimeCloseStopsNewAgents：Close 之后不能建新 Agent，已存在的 Agent 也不能
// 再 Run（共享后台任务/PTY 已经收尾）。
func TestRuntimeCloseStopsNewAgents(t *testing.T) {
	root := t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	cfg := kit.Config{Model: "test-model", Root: root, Gate: gate.AllowAll()}
	agent, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 幂等：第二次是 no-op。
	if err := rt.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if _, err := rt.NewAgent(cfg); !errors.Is(err, kit.ErrRuntimeClosed) {
		t.Fatalf("Close 之后 NewAgent 的错误 = %v, want ErrRuntimeClosed", err)
	}
	if _, _, err := agent.RunText(context.Background(), "session", "hi", nil); !errors.Is(err, kit.ErrRuntimeClosed) {
		t.Fatalf("Close 之后已存在 Agent 的 Run 错误 = %v, want ErrRuntimeClosed", err)
	}
}

// TestRuntimeConcurrentNewAgent：8 个 goroutine 同时建 Agent；再让 8 个 goroutine
// 与 Close 竞争。断言只有两种合法结果（成功建出 Agent / ErrRuntimeClosed），
// 不出现"半个 Agent"或"既没 Agent 也没错误"。
func TestRuntimeConcurrentNewAgent(t *testing.T) {
	rt, err := kit.NewRuntime(kit.RuntimeConfig{})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	const workers = 8
	root, err := os.MkdirTemp("", "kit-runtime-concurrent-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	agents := make([]*kit.Agent, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			agents[i], errs[i] = rt.NewAgent(kit.Config{
				Model: "test-model", Root: root, Gate: gate.AllowAll(),
			})
		}(i)
	}
	wg.Wait()

	policies := make(map[*sandbox.Policy]struct{}, workers)
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("并发 NewAgent[%d]: %v", i, errs[i])
		}
		if agents[i] == nil {
			t.Fatalf("并发 NewAgent[%d] 返回 (nil, nil)", i)
		}
		policy := agents[i].Policy()
		if policy == nil {
			t.Fatalf("并发 NewAgent[%d].Policy() 为 nil", i)
		}
		policies[policy] = struct{}{}
	}
	if len(policies) != workers {
		t.Fatalf("%d 个并发 Agent 只得到 %d 份策略：策略被共享了", workers, len(policies))
	}

	// Close 与 NewAgent 互斥：每个 goroutine 要么拿到一个可用的 Agent，要么拿到
	// ErrRuntimeClosed——绝不出现"nil Agent + nil error"。
	racy := make([]*kit.Agent, workers)
	racyErr := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			racy[i], racyErr[i] = rt.NewAgent(kit.Config{
				Model: "test-model", Root: root, Gate: gate.AllowAll(),
			})
		}(i)
	}
	closeErr := make(chan error, 1)
	go func() { closeErr <- rt.Close() }()
	wg.Wait()
	if err := <-closeErr; err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i := range workers {
		switch {
		case racyErr[i] == nil && racy[i] == nil:
			t.Fatalf("并发 NewAgent 与 Close 竞争时返回 (nil, nil)")
		case racyErr[i] != nil && !errors.Is(racyErr[i], kit.ErrRuntimeClosed):
			t.Fatalf("并发竞争得到非预期错误: %v", racyErr[i])
		case racy[i] != nil:
			if _, err := rt.NewAgent(kit.Config{Model: "test-model", Root: root}); !errors.Is(err, kit.ErrRuntimeClosed) {
				t.Fatalf("Close 之后的 NewAgent 错误 = %v, want ErrRuntimeClosed", err)
			}
		}
	}
}
