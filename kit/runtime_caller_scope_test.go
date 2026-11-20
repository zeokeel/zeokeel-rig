package kit_test

// D1 审计修复：共享管理器"按调用方解析围栏"的守护用例。
//
// 三条契约：
//  1. `job_start` / `terminal_open` 用的是**本次调用那个 Agent** 的 policy/runner，
//     不是共享管理器第一次构造时绑死的那一份；
//  2. `Runtime.Close` 取消并等待在飞回合（硬边界，不留下跑着的 goroutine）；
//  3. 能力字段被共享值覆盖时 `slog.Warn` 必须留痕；`Agent.Shutdown` 不关共享句柄。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// TestRuntimeJobsUseCallingAgentPolicy：后台任务的围栏取**调用方**的策略。
//
//	a1 = workspace-write（rootA），a2 = read-only（rootB，无 runner）。
//	a2 对 rootA 的 job_start 必须被拒；a1 对自己的 root 必须能起任务。
func TestRuntimeJobsUseCallingAgentPolicy(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Provider: &scriptProvider{}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	a1, err := rt.NewAgent(kit.Config{Model: "test-model", Root: rootA, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent(a1): %v", err)
	}
	t.Cleanup(func() { _ = a1.Shutdown(context.Background()) })
	a2, err := rt.NewAgent(kit.Config{
		Model: "test-model", Root: rootB,
		Mode: sandbox.ModeReadOnly, Gate: gate.AllowAll(),
	})
	if err != nil {
		t.Fatalf("NewAgent(a2): %v", err)
	}
	t.Cleanup(func() { _ = a2.Shutdown(context.Background()) })

	startA2, ok := a2.Tool("job_start")
	if !ok {
		t.Fatal("a2 没有装配 job_start")
	}
	crossed := runtimeInvoke(t, startA2, map[string]any{"cwd": rootA, "command": "touch probe.txt"})
	if !crossed.IsError {
		t.Fatalf("read-only 的 a2 在 a1 的 root 里起任务成功了（越权）: %q", crossed.Content)
	}
	if !strings.Contains(crossed.Content, "read-only") && !strings.Contains(crossed.Content, "outside the configured roots") {
		t.Fatalf("拒绝文案既没说 read-only 也没说 roots: %q", crossed.Content)
	}

	startA1, ok := a1.Tool("job_start")
	if !ok {
		t.Fatal("a1 没有装配 job_start")
	}
	own := runtimeInvoke(t, startA1, map[string]any{"cwd": rootA, "command": "echo own-root-ok"})
	if own.IsError {
		t.Fatalf("workspace-write 的 a1 对自己的 root 起任务被拒（围栏取错了策略）: %q", own.Content)
	}
}

// TestRuntimeJobsUseCallingAgentPolicyReverseOrder：先建 read-only、后建
// workspace-write 时，后者也必须能起任务（不能沿用第一个 Agent 的只读围栏）。
func TestRuntimeJobsUseCallingAgentPolicyReverseOrder(t *testing.T) {
	rootRO, rootRW := t.TempDir(), t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Provider: &scriptProvider{}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	ro, err := rt.NewAgent(kit.Config{
		Model: "test-model", Root: rootRO,
		Mode: sandbox.ModeReadOnly, Gate: gate.AllowAll(),
	})
	if err != nil {
		t.Fatalf("NewAgent(read-only): %v", err)
	}
	t.Cleanup(func() { _ = ro.Shutdown(context.Background()) })
	rw, err := rt.NewAgent(kit.Config{Model: "test-model", Root: rootRW, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent(workspace-write): %v", err)
	}
	t.Cleanup(func() { _ = rw.Shutdown(context.Background()) })

	start, ok := rw.Tool("job_start")
	if !ok {
		t.Fatal("workspace-write 的 Agent 没有装配 job_start")
	}
	result := runtimeInvoke(t, start, map[string]any{"cwd": rootRW, "command": "echo reverse-ok"})
	if result.IsError {
		t.Fatalf("workspace-write 的 Agent 被第一个 read-only Agent 的策略挡住: %q", result.Content)
	}
}

// TestRuntimeTerminalsUseCallingAgentPolicy：PTY 的围栏同样取调用方策略。
func TestRuntimeTerminalsUseCallingAgentPolicy(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Provider: &scriptProvider{}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	a1, err := rt.NewAgent(kit.Config{Model: "test-model", Root: rootA, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent(a1): %v", err)
	}
	t.Cleanup(func() { _ = a1.Shutdown(context.Background()) })
	a2, err := rt.NewAgent(kit.Config{
		Model: "test-model", Root: rootB,
		Mode: sandbox.ModeReadOnly, Gate: gate.AllowAll(),
	})
	if err != nil {
		t.Fatalf("NewAgent(a2): %v", err)
	}
	t.Cleanup(func() { _ = a2.Shutdown(context.Background()) })

	openA2, ok := a2.Tool("terminal_open")
	if !ok {
		t.Fatal("a2 没有装配 terminal_open")
	}
	crossed := runtimeInvoke(t, openA2, map[string]any{"cwd": rootA})
	if !crossed.IsError {
		t.Fatalf("read-only 的 a2 在 a1 的 root 上开出了 PTY（越权）: %q", crossed.Content)
	}
	if !strings.Contains(crossed.Content, "read-only") && !strings.Contains(crossed.Content, "outside the configured roots") {
		t.Fatalf("拒绝文案既没说 read-only 也没说 roots: %q", crossed.Content)
	}

	// a1 对自己的 root 必须能开终端（反向证明不是"全都拒"）。
	openA1, ok := a1.Tool("terminal_open")
	if !ok {
		t.Fatal("a1 没有装配 terminal_open")
	}
	own := runtimeInvoke(t, openA1, map[string]any{"cwd": rootA})
	if own.IsError {
		t.Fatalf("workspace-write 的 a1 打不开自己的 root: %q", own.Content)
	}
	if closeTool, ok := a1.Tool("terminal_close"); ok {
		if id := terminalSessionID(own.Content); id != "" {
			_ = runtimeInvoke(t, closeTool, map[string]any{"session_id": id})
		}
	}
}

// terminalSessionID 从 terminal_open 的人读文本里取出 session_id。
func terminalSessionID(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "session_id: "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// runtimeBlockingProvider 的回合阻塞到 ctx 被取消。
type runtimeBlockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *runtimeBlockingProvider) Name() string { return "runtime-blocking" }

func (p *runtimeBlockingProvider) Stream(ctx context.Context, _ kit.Request) (kit.Stream, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestRuntimeCloseStopsInFlightRun：Close 是硬边界——返回之后在飞回合必须已经
// 退出（ErrRuntimeClosed 或 ctx canceled），不能留下跑到 500ms 之后的 goroutine。
func TestRuntimeCloseStopsInFlightRun(t *testing.T) {
	provider := &runtimeBlockingProvider{started: make(chan struct{})}
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Provider: provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	a, err := rt.NewAgent(kit.Config{Model: "test-model", Root: t.TempDir(), Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown(context.Background()) })

	done := make(chan error, 1)
	go func() {
		_, _, runErr := a.RunText(context.Background(), "s1", "hello", nil)
		done <- runErr
	}()
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("在飞回合没有开始（provider 未被调用）")
	}

	closed := make(chan error, 1)
	go func() { closed <- rt.Close() }()
	select {
	case closeErr := <-closed:
		if closeErr != nil {
			t.Fatalf("Close: %v", closeErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close 没有在上界内返回")
	}

	// Close 返回后，在飞回合必须已经退出——这是本次修复的核心。
	select {
	case runErr := <-done:
		if runErr == nil {
			t.Fatal("Close 之后在飞回合返回了 nil 错误")
		}
		if !errors.Is(runErr, kit.ErrRuntimeClosed) && !errors.Is(runErr, context.Canceled) {
			t.Fatalf("在飞回合的错误 = %v, want ErrRuntimeClosed 或 context.Canceled", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Close 返回后 1s，在飞回合仍在跑（Close 没有等待取消生效）")
	}
}

// TestRuntimeHonorsPerAgentSandboxOptOut：Runtime 有共享 runner 时，每 Agent
// 用 Config.Sandbox=false 可以退出 OS 沙箱（不报错），围栏回到进程内策略。
func TestRuntimeHonorsPerAgentSandboxOptOut(t *testing.T) {
	runner := &kitRunner{}
	rt, err := kit.NewRuntime(kit.RuntimeConfig{
		Provider:      &scriptProvider{},
		SandboxRunner: runner,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	a, err := rt.NewAgent(kit.Config{
		Model: "test-model", Root: t.TempDir(),
		Mode: sandbox.ModeReadOnly, Gate: gate.AllowAll(),
		Sandbox: kit.Bool(false),
	})
	if err != nil {
		t.Fatalf("Config.Sandbox=false 在 Runtime 下被硬拒: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown(context.Background()) })
	if a == nil {
		t.Fatal("NewAgent 返回 nil Agent")
	}

	// 退出 runner 的证据：read-only + 无 runner 时后台任务被整体拒绝。
	// 若共享 runner 仍被注入，shellConfined 会放行，这里就会成功。
	start, ok := a.Tool("job_start")
	if !ok {
		t.Fatal("read-only 的 Agent 没有装配 job_start")
	}
	result := runtimeInvoke(t, start, map[string]any{"command": "echo should-not-run"})
	if !result.IsError || !strings.Contains(result.Content, "read-only") {
		t.Fatalf("Config.Sandbox=false 没有退出 Runtime 的 runner（read-only 仍被放行）: %q", result.Content)
	}
}

// TestRuntimeConflictMessagesNameTheField：冲突错误的文案必须指向真实字段
// （Aggregate 不能被报成 Storage，Jobs 要给出 RuntimeConfig.Jobs 这个出口）。
func TestRuntimeConflictMessagesNameTheField(t *testing.T) {
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Provider: &scriptProvider{}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	_, aggregateErr := rt.NewAgent(kit.Config{
		Model: "test-model", Root: t.TempDir(),
		Aggregate: &fakeAggregate{},
	})
	if aggregateErr == nil {
		t.Fatal("Config.Aggregate 在 Runtime 下没有报错")
	}
	if !strings.Contains(aggregateErr.Error(), "Aggregate") {
		t.Fatalf("错误文案没有点出 Config.Aggregate: %v", aggregateErr)
	}
	if strings.Contains(aggregateErr.Error(), "Config.Storage") {
		t.Fatalf("错误文案把 Aggregate 误报成 Config.Storage: %v", aggregateErr)
	}

	_, jobsErr := rt.NewAgent(kit.Config{
		Model: "test-model", Root: t.TempDir(),
		Jobs: jobs.NewManager(),
	})
	if jobsErr == nil {
		t.Fatal("Config.Jobs 在 Runtime 下没有报错")
	}
	if !strings.Contains(jobsErr.Error(), "RuntimeConfig.Jobs") {
		t.Fatalf("Config.Jobs 的错误没有指向 RuntimeConfig.Jobs: %v", jobsErr)
	}
}

// TestRuntimeOverrideOfCapabilityWarns：能力字段被共享值覆盖时必须 slog.Warn
// （宿主会以为自己在按 Agent 配 Searcher，实际用的是 Runtime 的）。
func TestRuntimeOverrideOfCapabilityWarns(t *testing.T) {
	warnings := captureSlog(t)
	rt, err := kit.NewRuntime(kit.RuntimeConfig{
		Provider: &scriptProvider{},
		Searcher: &auditSearcher{},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	a, err := rt.NewAgent(kit.Config{
		Model: "test-model", Root: t.TempDir(),
		Searcher: &auditSearcher{},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown(context.Background()) })
	if text := warnings(); !strings.Contains(text, "Searcher") {
		t.Fatalf("能力字段被覆盖但没有 slog.Warn 留痕: %q", text)
	}
}

// TestRuntimeAgentShutdownKeepsSharedJobs：一个 Agent 的 Shutdown 不能关掉共享
// 的 jobs.Manager——那是 Runtime.Close 的责任。
func TestRuntimeAgentShutdownKeepsSharedJobs(t *testing.T) {
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Provider: &scriptProvider{}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	root := t.TempDir()
	a1, err := rt.NewAgent(kit.Config{Model: "test-model", Root: root, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent(a1): %v", err)
	}
	t.Cleanup(func() { _ = a1.Shutdown(context.Background()) })
	a2, err := rt.NewAgent(kit.Config{Model: "test-model", Root: root, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent(a2): %v", err)
	}

	start, ok := a1.Tool("job_start")
	if !ok {
		t.Fatal("a1 没有装配 job_start")
	}
	started := runtimeInvoke(t, start, map[string]any{"command": "sleep 5"})
	if started.IsError {
		t.Fatalf("job_start: %q", started.Content)
	}
	data, _ := started.Data.(map[string]any)
	jobID, _ := data["id"].(string)
	if jobID == "" {
		t.Fatalf("job_start 没有返回 id: %#v", started.Data)
	}

	if err := a2.Shutdown(context.Background()); err != nil {
		t.Fatalf("a2.Shutdown: %v", err)
	}

	list, ok := a1.Tool("job_list")
	if !ok {
		t.Fatal("a1 没有装配 job_list")
	}
	listed := runtimeInvoke(t, list, map[string]any{})
	if listed.IsError {
		t.Fatalf("job_list: %q", listed.Content)
	}
	if !strings.Contains(listed.Content, jobID) {
		t.Fatalf("a2.Shutdown 之后 a1 的后台任务 %q 不见了（共享管理器被关了）:\n%s", jobID, listed.Content)
	}
}
