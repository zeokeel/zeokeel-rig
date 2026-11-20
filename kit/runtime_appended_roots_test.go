package kit_test

// `Runtime.AddRoot` / `RemoveRoot` 并发时对 `agent.kitAppendedRoots` 的访问：
// AddRoot 持 r.mu 往这个 slice 里 append（可能重新分配底层数组），因此 RemoveRoot
// 也必须在同一把锁内先取快照，再在锁外使用那份副本——否则一边 append、另一边无锁
// 读同一个 slice 头就是数据竞争。
//
// 本仓构建/测试一律 CGO_ENABLED=0，`-race` 在本机不可用（go 报 "-race requires
// cgo"），所以这条用例在本地只能守住功能面；它是给 CI 的 test job 准备的确定性
// 竞争负载——竞争存在时 -race 必然报 DATA RACE。

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestRuntimeAddRootRemoveRootConcurrent：多个 goroutine 同时对一个共用显式
// Policy 的 Agent 加根/撤根，最后所有根都被撤销、没有任何 Agent 还能写它们。
func TestRuntimeAddRootRemoveRootConcurrent(t *testing.T) {
	root := t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Provider: &scriptProvider{}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// 宿主自己的策略：AddRoot 会把根声明进它，并记进 kitAppendedRoots。
	shared := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	agent, err := rt.NewAgent(kit.Config{Model: "test-model", Root: root, Policy: shared, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = agent.Shutdown(context.Background()) })

	// 每轮用一个**新**目录：只有新根才会让 AddRoot 真的往 kitAppendedRoots 里
	// append（重复添加是幂等的），这样才能持续制造"一边 append、一边读"的窗口。
	const (
		workers = 8
		rounds  = 25
	)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < rounds; round++ {
				dir := t.TempDir()
				if err := rt.AddRoot(dir); err != nil {
					t.Errorf("AddRoot(%q): %v", dir, err)
					return
				}
				if err := rt.RemoveRoot(dir); err != nil {
					t.Errorf("RemoveRoot(%q): %v", dir, err)
					return
				}
				if agent.Policy().AllowsWrite(filepath.Join(dir, "out.txt")) {
					t.Errorf("并发撤销后仍能写 %q", dir)
					return
				}
			}
		}()
	}
	wg.Wait()

	// 功能面：撤销后 Agent 只剩下自己的主根，没有残留的可写根。
	if leftover := agent.Policy().WritableRoots()[1:]; len(leftover) != 0 {
		t.Fatalf("撤销后仍残留可写根: %v", leftover)
	}
}
