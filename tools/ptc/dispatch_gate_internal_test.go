package ptc

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// ctxGateKey 模拟 kit 在**当次 Run** 的 context 上挂门禁的位置（真实实现是
// kit 内部的 runGateKey，这里只需要一个可区分的键）。
type ctxGateKey struct{}

// recordingGate 是一个记录调用的假门禁：不启动任何解释器，只回答固定 decision。
// 两个实例（构造期 / ctx 上）靠"谁被调用"区分，因此能证明 dispatch 取的是哪一份。
type recordingGate struct {
	decision rig.Decision
	calls    int
	names    []string
}

func (g *recordingGate) Approve(_ context.Context, _ rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	g.calls++
	g.names = append(g.names, call.Name)
	return g.decision, nil
}

// recordingTool 是内层调用的目标工具：只记录被调用，不碰任何外部进程。
type recordingTool struct{ calls int }

func (t *recordingTool) Name() string           { return "echo_tool" }
func (t *recordingTool) Description() string    { return "Record the call and echo its arguments." }
func (t *recordingTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *recordingTool) Invoke(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	t.calls++
	return rig.ToolResult{Content: "stub:" + call.Arguments}, nil
}

// TestRunCodeDispatchUsesContextGateWithoutInterpreter 直接驱动内层 dispatch，
// 覆盖"门禁按当次 Run 取"这条分支——**不需要 python3/node**，因此在没有解释器
// 的宿主/CI 上也不会静默失去覆盖（两条端到端用例会 Skip，这条不会）。
//
// 断言：传入 ctx 的门禁优先于构造期那份；ctx 上没有时回退构造期；当次门禁返回
// allow-always 与 allow 一样放行（用户"记住这条"不能被内层当成拒绝）。
func TestRunCodeDispatchUsesContextGateWithoutInterpreter(t *testing.T) {
	newTool := func(constructed *recordingGate, inner *recordingTool) *Tool {
		return New(Config{
			Tools: []rig.Tool{inner},
			Gate:  constructed,
			// 与 kit 的接线同形：ctx 上有当次门禁就用它，没有就返回 nil（回退 Gate）。
			GateSource: func(ctx context.Context) rig.Gate {
				if current, ok := ctx.Value(ctxGateKey{}).(rig.Gate); ok {
					return current
				}
				return nil
			},
		})
	}
	request := callRequest{Name: "echo_tool", Arguments: map[string]any{"text": "hi"}}

	t.Run("ctx 上的门禁优先于构造期那份", func(t *testing.T) {
		constructed := &recordingGate{decision: rig.DecisionDeny}
		perRun := &recordingGate{decision: rig.DecisionAllow}
		inner := &recordingTool{}
		tool := newTool(constructed, inner)

		ctx := context.WithValue(context.Background(), ctxGateKey{}, rig.Gate(perRun))
		response := tool.dispatch(ctx, request)

		if response.Error != "" {
			t.Fatalf("dispatch = %+v, want the ctx gate to allow it", response)
		}
		if !strings.Contains(response.Content, "stub:") || !strings.Contains(response.Content, "hi") {
			t.Fatalf("content = %q, want the inner tool result", response.Content)
		}
		if perRun.calls != 1 || inner.calls != 1 {
			t.Fatalf("ctx gate calls = %d, inner tool calls = %d, want 1/1", perRun.calls, inner.calls)
		}
		if constructed.calls != 0 {
			t.Fatalf("构造期门禁被调用了 %d 次，dispatch 没有取 ctx 上的当次门禁", constructed.calls)
		}
	})

	t.Run("ctx 上没有当次门禁时回退构造期", func(t *testing.T) {
		constructed := &recordingGate{decision: rig.DecisionDeny}
		inner := &recordingTool{}
		tool := newTool(constructed, inner)

		response := tool.dispatch(context.Background(), request)

		if !strings.Contains(response.Error, "denied by approval gate") {
			t.Fatalf("dispatch = %+v, want a denial from the constructed gate", response)
		}
		if constructed.calls != 1 || inner.calls != 0 {
			t.Fatalf("构造期门禁 calls = %d、内层工具 calls = %d，want 1/0", constructed.calls, inner.calls)
		}
	})

	t.Run("当次门禁的 allow-always 也放行", func(t *testing.T) {
		constructed := &recordingGate{decision: rig.DecisionDeny}
		perRun := &recordingGate{decision: rig.DecisionAllowAlways}
		inner := &recordingTool{}
		tool := newTool(constructed, inner)

		ctx := context.WithValue(context.Background(), ctxGateKey{}, rig.Gate(perRun))
		response := tool.dispatch(ctx, request)

		if response.Error != "" {
			t.Fatalf("dispatch = %+v, want allow-always to run the inner call", response)
		}
		if inner.calls != 1 || constructed.calls != 0 {
			t.Fatalf("内层工具 calls = %d、构造期门禁 calls = %d，want 1/0", inner.calls, constructed.calls)
		}
	})
}
