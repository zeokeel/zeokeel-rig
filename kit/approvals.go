package kit

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
	eventspkg "github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// 本文件是审批原语在 kit 上的接线：进程内审批记忆、按 Run 的审批包装
// （记忆 + 本轮去重），以及给宿主看的 SessionApprovals / ForgetApprovals。
//
// 一句话口径：本产品是**个人 agent、单用户**。记住的是"用户已经回答过的问题"
// （同一个目标不再打扰），不是"把围栏换成规则引擎"——每次执行仍然经过沙箱策略
// 与工具自身的检查。记忆读写失败只记日志，不打断回合。

// memoryApprovals 是进程内的审批记忆（重启即丢）。要跨重启保留，宿主自己实现
// rig.ApprovalStore（落文件/数据库），kit 不改它的语义。
type memoryApprovals struct {
	mu    sync.Mutex
	rules []rig.ApprovalRule
}

// NewMemoryApprovals 返回一个进程内的审批记忆 store，可直接填进 Config.Approvals。
//
// 它是"零配置也能用"的起点：规则只活在本进程里，重启后重新问人。宿主想要跨重启
// 的记忆就自己实现 rig.ApprovalStore（Load/Save 整体替换），kit 不需要知道细节。
func NewMemoryApprovals() rig.ApprovalStore {
	return &memoryApprovals{}
}

// Load 实现 rig.ApprovalStore：返回规则副本（调用方可以随意修改）。
func (m *memoryApprovals) Load(ctx context.Context) ([]rig.ApprovalRule, error) {
	if m == nil {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]rig.ApprovalRule(nil), m.rules...), nil
}

// Save 实现 rig.ApprovalStore：整体替换规则集合（保存副本，不持有调用方切片）。
func (m *memoryApprovals) Save(ctx context.Context, rules []rig.ApprovalRule) error {
	if m == nil {
		return fmt.Errorf("kit: nil memory approval store")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = append([]rig.ApprovalRule(nil), rules...)
	return nil
}

// gatewayForRun 返回本次 Run 实际使用的门禁。
//
// 两条规则：
//
//   - 本轮显式 RunRequest.Gate 是宿主的**最终决定**：原样返回，不套审批记忆、也不
//     套本轮 memo。否则记忆命中会把宿主本轮的 PersonalNever 之类的硬拒绝悄悄变成
//     "记住的放行"并真的执行。
//   - 否则在构造期决定的门禁上，把审批记忆直接贴到**最内层**审批门禁（AskGate）上，
//     再用同一个组合重建外层（只读/已沙箱化免问、auto 档的预审）。记忆按
//     ApprovalTarget 决定"问过没有"，AskGate 只负责"要不要问"；memo 套在最外层，
//     只对同一 Run 内完全相同的调用做 single-flight。
//
// 为什么在 Run 入口组装而不是构造期：规则的作用域含 SessionID，而一个 Agent 要
// 服务多个会话；memo 的生命周期是"单次 Run"。两层包装都很薄，代价可以忽略。
// 门禁为 nil（全放行）时原样返回 nil，不改变默认行为。
func (a *Agent) gatewayForRun(req RunRequest) rig.Gate {
	if req.Gate != nil {
		return req.Gate
	}
	base := a.gate
	if base == nil {
		return nil
	}
	if a.approvals != nil && a.askInner != nil && a.gateLayer != nil {
		memory := gate.WithApprovalMemory(a.approvals, a.approvalScope,
			gate.WithApprovalSessionID(req.SessionID))
		base = a.gateLayer(memory(a.askInner))
	}
	return gate.NewRoundMemo().Wrap(base)
}

// runGateKey 是"本次 Run 生效的门禁"在工具 context 上的键。
type runGateKey struct{}

// runGateBox 把门禁包一层：ctx 里直接存 nil 时 Value 返回 nil 接口，会让
// "本轮全放行"退化成"没有门禁信息"，反过来又回退到构造期门禁。
type runGateBox struct{ gate rig.Gate }

// withRunGate 把本次 Run 的门禁挂到 context 上。
//
// 工具只收到 context，拿不到 Agent 的字段；而 run_code（PTC）是构造期创建的，
// 它的内层调用必须用**当次 Run** 的门禁（审批记忆按 SessionID 记录规则、memo 的
// 生命周期是单次 Run），不能沿用构造期快照。因此门禁经 context 传到工具，再由
// ptc.Config.GateSource 取出。用 context 而不是 Agent 字段：同一个 Agent 可以被
// 多个 Run 并发使用，只有 context 能按 Run 区分。
func withRunGate(ctx context.Context, gate rig.Gate) context.Context {
	return context.WithValue(ctx, runGateKey{}, runGateBox{gate: gate})
}

// runGateFrom 取本次 Run 的门禁；ok=false 表示不在 kit.Agent.Run 的 context 里
// （宿主经 Agent.Tool 直接调用工具），调用方应回退到构造期门禁。
func runGateFrom(ctx context.Context) (rig.Gate, bool) {
	if ctx == nil {
		return nil, false
	}
	box, ok := ctx.Value(runGateKey{}).(runGateBox)
	return box.gate, ok
}

// normalizeApprovalScope 校验并规范化 Config.ApprovalScope（空值 = session）。
func normalizeApprovalScope(scope string) (string, error) {
	switch strings.TrimSpace(scope) {
	case "", rig.ApprovalScopeSession:
		return rig.ApprovalScopeSession, nil
	case rig.ApprovalScopeGlobal:
		return rig.ApprovalScopeGlobal, nil
	default:
		return "", fmt.Errorf("kit: 未知的 ApprovalScope %q（want %q 或 %q）",
			scope, rig.ApprovalScopeSession, rig.ApprovalScopeGlobal)
	}
}

// approvalPrompt 返回生效的审批文案渲染器：默认就是 gate.SandboxGrantPrompt
// （现有行为，默认不引入 diff），配置了 ApprovalPromptChain 时按顺序拼接。
func approvalPrompt(chain []gate.Prompt) gate.Prompt {
	if len(chain) == 0 {
		return gate.SandboxGrantPrompt
	}
	return gate.PromptChain(chain...)
}

// approvalMemoryStore 决定本 Agent 是否真的启用审批记忆。
//
// 两种情况不挂 store：
//
//   - 宿主显式传了 Config.Gate：那里面可能叠了硬策略（例如 CommandRiskGate），
//     而记忆命中会直接绕过 gate，等于把宿主自己的"硬拦"拆掉。这种组合由宿主自己
//     用 gate.WithApprovalMemory 决定包在哪一层。
//   - approval=never 是"拒绝一切"的口径（见 gate.PersonalNever）：记忆不能把它
//     悄悄变成"记住的放行"。
func approvalMemoryStore(store rig.ApprovalStore, kitOwnsGate bool, approval ApprovalMode) rig.ApprovalStore {
	if store == nil || !kitOwnsGate {
		return nil
	}
	if approval == ApprovalNever {
		return nil
	}
	return store
}

// SessionApprovals 返回 store 里**会话作用域**的审批记忆（供 UI 展示"记住过什么"、
// 或让用户逐条撤销）。
//
// 口径是单用户个人 agent：它返回存储中全部 session 规则，不按"当前会话"过滤
// （Agent 可以被多个会话共享，没有唯一的"当前"）。global 规则不在这里——它们属于
// 整个宿主，用 Config.Approvals 自己的 Load 去看。没有配置 store 时返回 nil。
func (a *Agent) SessionApprovals() []rig.ApprovalRule {
	if a == nil || a.approvals == nil {
		return nil
	}
	rules, err := a.approvals.Load(context.Background())
	if err != nil {
		if a.events != nil {
			// 记忆读不出来不该让宿主看到一条"回合失败"；只留一条 notice 提示。
			a.events.emit(eventspkg.KindNotice, noticePayload{Message: "审批记忆读取失败：" + err.Error()})
		}
		return nil
	}
	session := make([]rig.ApprovalRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Scope == rig.ApprovalScopeSession {
			session = append(session, rule)
		}
	}
	return session
}

// ForgetApprovals 清掉 store 里的**会话作用域**审批记忆（global 规则保留：它们
// 属于整个宿主，撤销要宿主自己做）。没有配置 store 时是 no-op。
//
// 用途是"把记住的答案全部撤销，回到逐条问人"。写失败会返回错误——这是宿主主动
// 发起的撤销，静默失败会让用户以为已经清干净了。
func (a *Agent) ForgetApprovals(ctx context.Context) error {
	if a == nil || a.approvals == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rules, err := a.approvals.Load(ctx)
	if err != nil {
		return fmt.Errorf("kit: load approvals: %w", err)
	}
	kept := make([]rig.ApprovalRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Scope != rig.ApprovalScopeSession {
			kept = append(kept, rule)
		}
	}
	if err := a.approvals.Save(ctx, kept); err != nil {
		return fmt.Errorf("kit: save approvals: %w", err)
	}
	return nil
}

// approvalChainSummary 是 Preflight 里"当前用的是哪条审批提示链"的一句话。
func (a *Agent) approvalChainSummary() string {
	if len(a.approvalPrompts) == 0 {
		return "审批提示链：默认（gate.SandboxGrantPrompt；其余工具回退到 AskGate 的默认文案）"
	}
	return fmt.Sprintf("审批提示链：自定义 %d 段，按顺序拼接（空段跳过，全部为空时回退默认文案）",
		len(a.approvalPrompts))
}
