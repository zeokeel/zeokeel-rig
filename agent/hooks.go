package agent

// 循环钩子的接线与调用点（契约见根包 agent.go 的 rig.TurnHook）。
//
// 为什么钩子必须在循环内部：一轮 Run 里可能有多轮模型调用与多次工具执行，回合
// 边界（谁开始、谁结束、这次调用被改成什么）只有循环自己知道。宿主在 Run 外面
// 包一层只能拿到"跑完了"这一个时间点，于是回合同步、快照、成本归属都只能退化
// 成"每次 Run 一次"。

import (
	"context"

	"github.com/zeokeel/zeokeel-rig"
)

// compactHooks 去掉 nil 并复制切片：Run 期间不再引用调用方的切片，构造之后
// 调用方改动 spec.Hooks 不会影响已构造的 agent。
func compactHooks(hooks []rig.TurnHook) []rig.TurnHook {
	if len(hooks) == 0 {
		return nil
	}
	out := make([]rig.TurnHook, 0, len(hooks))
	for _, hook := range hooks {
		if hook != nil {
			out = append(out, hook)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// beforeRun 依次调用钩子；第一个错误中止本次运行。
func (a *Agent) beforeRun(ctx context.Context, req rig.RunRequest) error {
	for _, hook := range a.hooks {
		if err := hook.BeforeRun(ctx, req); err != nil {
			return err
		}
	}
	return nil
}

// afterRun 依次调用钩子。这里不再中断：运行已经结束，钩子的返回值没有意义，
// 而 panic 与错误都不该改变调用方看到的结果。
func (a *Agent) afterRun(ctx context.Context, result rig.RunResult) {
	for _, hook := range a.hooks {
		hook.AfterRun(ctx, result)
	}
}

// beforeTool 依次调用钩子；第一个错误让这次调用失败（不执行、不过 gate）。
func (a *Agent) beforeTool(ctx context.Context, call rig.ToolCall) error {
	for _, hook := range a.hooks {
		if err := hook.BeforeTool(ctx, call); err != nil {
			return err
		}
	}
	return nil
}

// afterTool 依次让钩子改写结果（按切片顺序，后一个看到前一个的改写）。
func (a *Agent) afterTool(ctx context.Context, call rig.ToolCall, result *rig.ToolResult) {
	for _, hook := range a.hooks {
		hook.AfterTool(ctx, call, result)
	}
}
