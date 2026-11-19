// Package gate 提供工具调用的内置审批门禁（approval gate）：全部放行、全部拒绝、
// 委托给函数，或按工具名白名单放行。
//
// 门禁（rig.Gate）由 agent 在工具执行之前调用，用于决定一次工具调用是否允许运行。
// 它可以阻塞等待用户输入（这正是产品层审批 UI 的接入点），因此调用发生在 agent
// 运行循环所在的 goroutine 上、按调用顺序串行进行。内置门禁都支持并发使用：
// 静态门禁（AllowAll/DenyAll/Func/AllowTools）无状态，动态门禁（Review/Ask）的
// 并发与状态语义见各自类型文档。
//
// # 沙箱与审批是两种独立的成本
//
// 一条命令已经被 OS 层限制到"只能写工作区、不能连网"时，再要求人工逐条同意是把
// 两个成本叠加：用户会为了少点几十次弹窗而直接关掉审批（连必要的审批也一起
// 丢了）。因此把"已沙箱化"当作免审批的**前提**，而不是额外条件——安全性由围栏
// 保证，而不是由点击次数保证。这就是 `AllowSandboxedCommands` 的存在理由：
//
//	gate.PersonalDefault(gate.AllowSandboxedCommands(opts, gate.Ask(host.Asker)))
//
// 免问判据：工具是 shell 类、runner 非 nil 且自报能强制、策略不是
// danger-full-access。与 DSH 对齐后**不看命令文本**：围栏已生效，命令内容不再参与
// 免问判定。免问仍会写一条 approval 事件（decision=allow、reason=sandboxed），让 UI
// 能显示"这条没问是因为已隔离"，也让审计能复查。文件工具不经 OS runner，因此不受
// 影响、照旧问人。
//
// # 免问白名单的判据
//
// `SandboxReadOnlyTools` 里放什么，判据是**"是否触及用户资源（文件 / 进程 /
// 网络）"**，不是"是否只读"：`memory` 会写字节、`todo_write` 会改 agent 状态，
// 但它们写的是**我们自己 app 的状态**，不是用户的文件与进程——逐条弹窗只会让常驻
// 工具变成不可用（"常驻但不可用"就是欺骗用户）。反过来，修改用户文件、执行命令、
// 长期改变行为或产生用户可见交付的工具一律问人（见个人默认档 PersonalDefault）。
//
// # 主要类型
//
//   - rig.Gate：审批接口，Approve(ctx, tool, call) 返回 rig.Decision。
//   - rig.Decision：审批结果，取值为 DecisionAllow（本次放行）、
//     DecisionAllowAlways（本次放行，并在同一轮 Run 内记住“工具名+参数”以跳过
//     后续相同调用）和 DecisionDeny（拒绝，拒绝原因会作为工具错误回传给模型）。
//   - funcGate：把普通函数适配为 rig.Gate 的内部实现，由本包各构造函数返回。
//   - denialExplainer：**可选**的拒绝理由通道（ExplainDenial）。实现了它的门禁会让
//     agent 把"命中哪条规则 / 谁拒绝的 / 该怎么办"拼进模型可见的工具错误；没有实现
//     的门禁，模型看到的仍是历史文案。这是给个人客户端的核心能力：模型看不到原因
//     就会原样重试同一条被拒命令。
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
//     allow-always（按工具记住，不再询问）或 deny。**默认一直等**（不设内部超时，
//     与 DSH 一致）；只有调用方取消（轮次中止）才撤回请求，此时返回包装
//     ErrApprovalExpired 与 ctx.Err() 的错误。并发安全，且等待审批时不持锁。
//
// 常见组合是“模型先审、拿不准再问人”：
//
//	g := gate.Review(reviewer,
//		gate.WithReviewTimeout(15*time.Second),
//		gate.WithReviewFallback(gate.Ask(host.Asker)),
//	)
//
// # 个人客户端推荐组合（审批两档，与 DSH 对齐）
//
// 产品定位是单用户的个人 agent 客户端，推荐直接用 defaults.go 里的构造函数，
// 而不是手工叠门禁：
//
//	// ask（默认）：只读工具免问，其余（含危险命令）人工审批
//	g := gate.PersonalAsk(gate.Ask(host.Asker))
//	// auto：不问人，也不做命令分类（等于 AllowAll）
//	g := gate.PersonalAuto()
//	// never：拒绝一切，并说明原因
//	g := gate.PersonalNever()
//
// 与 DSH 一致，**不存在"危险命令硬拒"这一档**：`rm -rf /` 在 ask 档下就是一次普通
// 的人工审批，在 never 档下与其它调用一样被拒。命令级风险门禁 `CommandRiskGate`
// 仍然保留、可用，但**默认预设不再使用它**（见该函数文档的迁移说明）。
//
// ⚠️ 不要用 PersonalDefault(nil)：需要审批却没有内层门禁时虽然不会中止 run
// （返回 deny + 理由），但**每次需要审批的调用都必然被拒**。没有审批通道时用
// gate.PersonalAskWithoutAsker()（文案是"没有可用的审批通道"，与真实状态一致）；
// **不要**用 PersonalAsk(DenyAll())，它的文案会说 `approval=never`，会让模型误判
// 状态并反复重试。
//
// # 失败模式：缺配置 ≠ 崩溃
//
// 门禁有两类失败，处理方式刻意不同：
//
//   - **可恢复（返回 DecisionDeny + 理由，对话继续）**：门禁"想审批/想委托"但**没有
//     通道**——inner gate 为 nil、Asker 为 nil。这类只是"少配了一个可选件"，下一次
//     调用换个方式就可能成功。agent 会把它当作普通工具错误回给模型（配合
//     agent.DenialExplainer，理由会进模型上下文），模型有机会换方案，用户也不会看到
//     "运行失败"。见 NoApprovalChannelReason / NoApprovalChannelToolReason /
//     NoAskChannelReason。
//   - **不可恢复（返回 error，run 中止）**：门禁自身坏了或调用方违约——Func(nil)、
//     nil *AskGate 接收者、Review 没有 reviewer 且没有 fallback、Asker 返回错误、
//     context 取消/超时。这些重试也不会好，必须让宿主看见。
//
// 判据：**"少了一个可选项"是可恢复的；"门禁无法工作或调用方违约"是不可恢复的**。
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
	"sync"

	"github.com/zeokeel/zeokeel-rig"
)

// funcGate 把普通函数适配为 rig.Gate。
type funcGate struct {
	fn func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error)
	// explain 是可选的拒绝理由（实现 agent 的可选接口 DenialExplainer）。
	// 为 nil 时退回 agent 的历史文案。
	explain func(tool rig.Tool, call rig.ToolCall) string
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

// 下面三条是"需要审批但没有审批通道"时**回给模型**的理由文案。
//
// 这是本包最重要的失败模式约定（审计 2C）：门禁**缺少可选项**（inner/Asker 为 nil）
// 时，必须返回 DecisionDeny + 理由，让对话继续、让模型换个办法；**绝不能**返回 Go
// error——agent 见到 gate 错误会中止整个 run，模型没有下一轮，用户只看到"运行失败"。
const (
	// NoApprovalChannelReason 用于命令级门禁（CommandRiskGate）：需要人工批准但
	// inner 为 nil。
	NoApprovalChannelReason = "这条命令需要人工批准，但门禁没有配置审批通道（inner gate 为 nil）；请宿主接上 gate.Ask(Asker) 或改用 gate.PersonalAsk*，本条命令未执行"
	// NoApprovalChannelToolReason 用于工具级白名单（AllowThenAsk）：非白名单调用需要
	// 审批但 inner 为 nil。措辞用"这次调用"而不是"这条命令"——AllowThenAsk 位于组合
	// 链中部，既可能是工具调用也可能是命令调用（CommandRiskGate 在它外层）。
	NoApprovalChannelToolReason = "这次调用需要人工批准，但门禁没有配置审批通道（inner gate 为 nil）；请宿主接上 gate.Ask(Asker) 或改用 gate.PersonalAsk*，本次调用未执行"
	// NoAskChannelReason 用于 AskGate 没有 Asker 的情况。
	NoAskChannelReason = "当前没有可用的审批通道（宿主未传 Asker）；本条命令未执行"
)

// denialExplainer 是本包对"拒绝理由通道"的本地别名。它与 agent.DenialExplainer
// 的方法集完全一致（ExplainDenial(rig.Tool, rig.ToolCall) string），因此两侧都不
// 需要 import 对方：agent 只依赖 rig，门禁只要提供同名方法就自动满足。
type denialExplainer interface {
	ExplainDenial(tool rig.Tool, call rig.ToolCall) string
}

// ExplainDenial 实现拒绝理由通道：把组合链上更深一层门禁给出的理由透出去。
//
// 组合门禁（AllowThenAsk / CommandRiskGate）自己没有理由时，用户看到的拒绝原因
// 往往来自内层（例如人工审批说了什么），所以必须逐层委托，否则组合一下理由就丢了。
func (g funcGate) ExplainDenial(tool rig.Tool, call rig.ToolCall) string {
	if g.explain == nil {
		return ""
	}
	return g.explain(tool, call)
}

// explainFrom 取出 inner 的拒绝理由函数；inner 未实现该通道时返回 nil。
func explainFrom(inner rig.Gate) func(tool rig.Tool, call rig.ToolCall) string {
	if inner == nil {
		return nil
	}
	explainer, ok := inner.(denialExplainer)
	if !ok {
		return nil
	}
	return explainer.ExplainDenial
}

// denialNoteLimit 限制"最近拒绝理由"缓存的条目数。
//
// 为什么需要缓存：agent 在 Approve 返回 DecisionDeny **之后**才调用 ExplainDenial，
// 而审查者/审批人的理由只在那一次 Approve 里拿得到。把最近若干条按"工具名+参数"
// 记住，既能给出精确理由，又不会让缓存无限增长（长会话里拒绝次数可能很多）。
const denialNoteLimit = 64

// denialNotes 是按"工具名+参数"索引的有界拒绝理由缓存（先进先出淘汰）。
//
// 键用调用本身而不是工具名：同一工具的不同参数可能得到不同结论，理由不能串台。
// 并发安全（Approve 与 ExplainDenial 可能来自不同 Run）。
type denialNotes struct {
	mu    sync.Mutex
	order []string
	notes map[string]string
}

// put 记录一条拒绝理由（空理由不记录：它等价于"没有理由"）。
func (n *denialNotes) put(key, note string) {
	if n == nil {
		return
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.notes == nil {
		n.notes = make(map[string]string, denialNoteLimit)
	}
	if _, exists := n.notes[key]; !exists {
		if len(n.order) >= denialNoteLimit {
			oldest := n.order[0]
			n.order = n.order[1:]
			delete(n.notes, oldest)
		}
		n.order = append(n.order, key)
	}
	n.notes[key] = note
}

// get 读取一条拒绝理由（没有时返回空串）。
func (n *denialNotes) get(key string) string {
	if n == nil {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.notes[key]
}

// denialKey 是拒绝理由缓存的键：工具名 + 原始参数。
func denialKey(tool rig.Tool, call rig.ToolCall) string {
	return toolCallName(tool, call) + "\x00" + call.Arguments
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
	// 语义是"never 档：拒绝所有工具调用"，因此文案说明审批模式；"ask 但没有审批
	// 通道"的场景请用 DenyAllWithReason（文案不同，见下）。
	return denyAllGate(`当前处于"拒绝所有工具调用"模式（approval=never）；请让用户切换审批模式`)
}

// DenyAllNoChannelReason 是"ask 模式但没有审批通道"时的内置兜底文案。
const DenyAllNoChannelReason = "当前没有可用的审批通道（宿主未提供 Asker）；请接上 Ask，或把审批模式改为 auto"

// DenyAllWithReason 与 DenyAll 相同（拒绝一切），但把 reason 作为解释器文案。
//
// 用途：宿主处于"ask 模式但没有审批通道"时的兜底——此时语义不是 approval=never，
// 而是"没有通道可问"。直接用 DenyAll 兜底会让模型看到
// `当前处于"拒绝所有工具调用"模式（approval=never）`，与真实状态不符，还可能诱导它
// 反复重试；用本函数给出准确原因，模型才知道该请用户接上审批通道。
//
// reason 为空（或全空白）时回落到 DenyAllNoChannelReason——**不会**返回空串，因为
// 空串会让 agent 退回历史文案，等于没有解释。拒绝决策与 DenyAll 完全一致。
func DenyAllWithReason(reason string) rig.Gate {
	note := strings.TrimSpace(reason)
	if note == "" {
		note = DenyAllNoChannelReason
	}
	return denyAllGate(note)
}

// denyAllGate 构造"拒绝一切、固定理由"的门禁（DenyAll 与 DenyAllWithReason 共用）。
//
// DenyAll 拒绝一切，因此 explain 总是有理由；组合门禁只会在真正被拒的调用上查询它
// （放行的调用根本不会走到这条 explain），所以这里不需要额外的精确判断。
func denyAllGate(note string) rig.Gate {
	return funcGate{
		fn: func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
			return rig.DecisionDeny, nil
		},
		explain: func(rig.Tool, rig.ToolCall) string {
			return note
		},
	}
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
	return funcGate{
		fn: func(_ context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
			name := call.Name
			if tool != nil {
				name = tool.Name()
			}
			if _, ok := allowed[name]; ok {
				return rig.DecisionAllow, nil
			}
			return rig.DecisionDeny, nil
		},
		// 白名单拒绝是最常见的一次性拒绝：说明"不在名单里"并给两条出路，
		// 比只说 denied 有用得多。
		explain: func(tool rig.Tool, call rig.ToolCall) string {
			name := toolCallName(tool, call)
			if name == "" {
				return ""
			}
			if _, ok := allowed[name]; ok {
				return ""
			}
			return "工具 " + name + " 不在当前允许的白名单里；改用只读方式（read_file/glob/grep），或请用户在设置里放开这个工具"
		},
	}
}

// 编译期检查：内置门禁实现了契约接口，并实现了可选的拒绝理由通道。
var (
	_ rig.Gate        = funcGate{}
	_ denialExplainer = funcGate{}
	_ denialExplainer = (*AskGate)(nil)
	_ denialExplainer = (*ReviewGate)(nil)
)
