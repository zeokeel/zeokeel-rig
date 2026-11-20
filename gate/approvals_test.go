package gate_test

// 审批原语（B4）的并发契约：同一 Run 内的相同调用必须 single-flight。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// blockingCountingGate 记录自己的 Approve 被调用几次，并在第一次调用里停一会儿：
// 只有真正的 single-flight 才能让后来者复用这一次的结论。
type blockingCountingGate struct {
	calls   atomic.Int64
	release chan struct{}
	once    sync.Once
}

func newBlockingCountingGate() *blockingCountingGate {
	return &blockingCountingGate{release: make(chan struct{})}
}

func (g *blockingCountingGate) Approve(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	g.calls.Add(1)
	g.once.Do(func() {
		// 给其它 goroutine 足够的时间走到 memo 上等待，而不是各自冲进 inner。
		time.Sleep(20 * time.Millisecond)
		close(g.release)
	})
	select {
	case <-g.release:
		return rig.DecisionAllow, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestRoundMemoIsSingleFlight：8 个并发同参调用，inner 恰好被调用 1 次，8 个调用者
// 都拿到同一个决定（先查后写不是 single-flight，会变成问 8 次）。
func TestRoundMemoIsSingleFlight(t *testing.T) {
	const workers = 8
	inner := newBlockingCountingGate()
	g := gate.NewRoundMemo().Wrap(inner)
	tool := stubTool{name: "write_file"}
	call := rig.ToolCall{ID: "c1", Name: "write_file", Arguments: `{"path":"a.txt","content":"v"}`}

	start := make(chan struct{})
	decisions := make(chan rig.Decision, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			decision, err := g.Approve(context.Background(), tool, call)
			decisions <- decision
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(decisions)
	close(errs)

	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("并发同参调用 inner 应恰好被调用 1 次，实际 %d 次", got)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("并发调用返回错误：%v", err)
		}
	}
	for decision := range decisions {
		if decision != rig.DecisionAllow {
			t.Fatalf("并发调用应复用同一个决定 allow，实际 %q", decision)
		}
	}
}
