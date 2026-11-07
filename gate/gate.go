// Package gate 提供工具调用的内置审批门禁（approval gate）：全部放行、全部拒绝、
// 委托给函数，或按工具名白名单放行。
//
// 门禁（rig.Gate）由 agent 在工具执行之前调用，用于决定一次工具调用是否允许运行。
// 它可以阻塞等待用户输入（这正是产品层审批 UI 的接入点），因此调用发生在 agent
// 运行循环所在的 goroutine 上、按调用顺序串行进行。内置门禁都支持并发使用：
// 静态门禁（AllowAll/DenyAll/Func/AllowTools）无状态，动态门禁（Review/Ask）的
// 并发与状态语义见各自类型文档。
//
// # 主要类型
//
//   - rig.Gate：审批接口，Approve(ctx, tool, call) 返回 rig.Decision。
//   - rig.Decision：审批结果，取值为 DecisionAllow（本次放行）、
//     DecisionAllowAlways（本次放行，并在同一轮 Run 内记住“工具名+参数”以跳过
//     后续相同调用）和 DecisionDeny（拒绝，拒绝原因会作为工具错误回传给模型）。
//   - funcGate：把普通函数适配为 rig.Gate 的内部实现，由本包各构造函数返回。
//
// # 典型用法
//
// 只读场景可以直接使用 AllowAll；需要在受信任边界内运行、但只允许部分工具时，
// 使用 AllowTools 白名单；接入产品审批 UI 时使用 Func：
//
//	import (
//		"context"
//
//		"github.com/zeokeel/zeokeel-rig"
//		"github.com/zeokeel/zeokeel-rig/agent"
//		"github.com/zeokeel/zeokeel-rig/gate"
//	)
//
//	// 只允许读取类工具，其余一律拒绝。
//	a, err := agent.New(rig.AgentSpec{
//		Provider: provider,
//		Model:    "gpt-4o-mini",
//		Tools:    tools,
//		Gate:     gate.AllowTools("read_file", "list_dir", "glob", "grep"),
//	})
//	if err != nil {
//		return err
//	}
//
//	// 接入审批 UI：返回空 Decision 时按拒绝处理（fail-closed）。
//	custom := gate.Func(func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
//		approved, err := askUser(ctx, tool.Name(), call.Arguments)
//		if err != nil {
//			return "", err
//		}
//		if approved {
//			return rig.DecisionAllow, nil
//		}
//		return rig.DecisionDeny, nil
//	})
//
// # 自动审查与人工审批
//
// 两个开箱即用的动态门禁补充上面的静态策略：
//
//   - ReviewGate（Review）：先让可插拔的 ToolReviewer（规则引擎或模型）审查每个
//     待批调用；审查允许则放行、明确拒绝则 DecisionDeny（作为工具错误回传）；
//     审查超时/出错时可回退到底层门禁（例如人工审批），未配置回退时失败即拒绝。
//     可用 WithReviewAllowlist 让低风险工具跳过审查；拒绝原因通过
//     WithReviewObserver 交给宿主展示/审计。
//   - AskGate（Ask）：把工具调用变成一次 rig.Asker 提问，用户可选 allow（本次）、
//     allow-always（按工具记住，不再询问）或 deny。等待超时/调用方取消时返回包装
//     ErrApprovalExpired 与 ctx.Err() 的错误；并发安全，且等待审批时不持锁。
//
// 常见组合是“模型先审、拿不准再问人”：
//
//	g := gate.Review(reviewer,
//		gate.WithReviewTimeout(15*time.Second),
//		gate.WithReviewFallback(gate.Ask(host.Asker)),
//	)
//
// # 不变量与安全说明
//
//   - 失败即拒绝（fail-closed）：Func 传入 nil 函数时，每次调用都会返回错误；
//     函数返回空 Decision 时按 DecisionDeny 处理。
//   - AllowTools 使用 Tool.Name() 精确匹配（调用时优先取 tool.Name()，tool 为
//     nil 时退回 call.Name）；空白名字被忽略，传入零个名字等价于 DenyAll。
//   - AllowAll 只适用于只读、无副作用的工具，不能用它守护会修改状态的工具。
//   - 门禁是进程内策略，不是操作系统级安全边界；不受信任的模型应配合 OS 沙箱或
//     容器使用。
//
// 与 agent 循环的关系：agent 会在每个模型轮次解析出工具调用后、执行之前按调用
// 顺序逐个询问门禁；被拒绝的调用不会执行，拒绝信息会作为工具错误结果回传给
// 模型，运行本身继续。门禁返回未知 Decision 或 error 时，本次 Run 会以错误结束。
package gate

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// funcGate 把普通函数适配为 rig.Gate。
type funcGate struct {
	fn func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error)
}

// Approve 实现 rig.Gate。fn 为 nil 时失败即拒绝（返回错误）；fn 返回空
// Decision 时按拒绝处理（fail-closed）。
func (g funcGate) Approve(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	if g.fn == nil {
		return "", fmt.Errorf("gate: nil approval function")
	}
	decision, err := g.fn(ctx, tool, call)
	if err != nil {
		return "", err
	}
	if decision == "" {
		return rig.DecisionDeny, nil
	}
	return decision, nil
}

// AllowAll 返回放行所有调用的门禁。它是只读取状态类工具的受信任默认值；
// 不要用它守护有副作用的工具。
func AllowAll() rig.Gate {
	return funcGate{fn: func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
		return rig.DecisionAllow, nil
	}}
}

// DenyAll 返回拒绝所有调用的门禁。agent 会把拒绝结果报告给模型
// （或在配置要求时让本次运行失败）。
func DenyAll() rig.Gate {
	return funcGate{fn: func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
		return rig.DecisionDeny, nil
	}}
}

// Func 返回由 fn 驱动的门禁。fn 在 agent 运行的 goroutine 上被调用，可以阻塞。
// fn 为 nil 时，返回的门禁对每次调用都报错；返回值原样透传，但空 Decision 会
// 按拒绝处理（fail-closed）。
func Func(fn func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error)) rig.Gate {
	return funcGate{fn: fn}
}

// AllowTools 返回只放行指定工具名、拒绝其余调用的门禁。名字与 Tool.Name()
// 精确比较；空白名字被忽略。不传任何名字时等价于 DenyAll。
func AllowTools(names ...string) rig.Gate {
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		allowed[name] = struct{}{}
	}
	return funcGate{fn: func(_ context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
		name := call.Name
		if tool != nil {
			name = tool.Name()
		}
		if _, ok := allowed[name]; ok {
			return rig.DecisionAllow, nil
		}
		return rig.DecisionDeny, nil
	}}
}

// 编译期检查：内置门禁实现了契约接口。
var _ rig.Gate = funcGate{}
