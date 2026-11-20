package rig

import "context"

// EventType 标识一个 agent 事件。事件从运行 goroutine 同步发出；不能阻塞的
// 消费者应自行缓冲。
type EventType string

const (
	EventTurnStart      EventType = "turn_start"
	EventTextDelta      EventType = "text_delta"
	EventReasoningDelta EventType = "reasoning_delta"
	EventToolCall       EventType = "tool_call"   // 模型请求了一次调用
	EventToolResult     EventType = "tool_result" // 一次调用结束
	EventUsage          EventType = "usage"       // 一次模型调用的 token 统计
	EventMessage        EventType = "message"     // 追加了一条完整的消息
	EventTurnEnd        EventType = "turn_end"    // 一个模型轮次结束
	EventError          EventType = "error"
	// EventContextCompacted 表示 agent 压缩了内存中的对话（历史被折叠成摘要）。
	// 个人客户端要据此告诉用户"历史没丢，只是被折叠了"。
	EventContextCompacted EventType = "context_compacted"
)

// Event 是 agent 运行中的一个可观察步骤。只有与 Type 相关的字段会被设置；
// 消费者必须忽略其余字段。
type Event struct {
	Type      EventType
	Text      string
	Reasoning string
	Call      *ToolCall
	Result    *ToolResult
	Message   *Message
	Usage     *Usage
	Iteration int
	Err       error
	// Compaction 非 nil 时携带一次上下文压缩的统计（Type == EventContextCompacted）。
	Compaction *CompactionInfo
}

// CompactionInfo 描述一次上下文压缩：压缩前后的消息条数。
type CompactionInfo struct {
	Before int
	After  int
}

// RunRequest 是某个会话的一次 agent 轮次（或继续）。
type RunRequest struct {
	// SessionID 选择对话历史；为空表示使用全新历史。
	SessionID string
	// Input 在运行开始前作为下一条（或几条）用户消息追加。
	// 自行管理历史的调用方留空即可。
	Input []Message
	// Model 覆盖 agent 的默认模型。
	Model string
	// MaxIterations 限制本次运行的模型↔工具轮数；>0 时覆盖 agent 默认值，
	// <=0 表示使用 agent 默认（spec 未配置上限时不限制轮数）。
	MaxIterations int
	// Gate 非 nil 时覆盖 agent 本次运行的审批门禁。
	Gate Gate
	// Emit 观察事件；nil 表示丢弃事件。
	Emit func(Event)
}

// RunResult 是一次 agent 运行的结果。
type RunResult struct {
	// Messages 是本次运行按顺序追加到历史的消息。
	Messages []Message
	// Usage 是本次运行所有模型调用的累计用量。
	Usage Usage
	// Iterations 是实际执行的模型轮数。
	Iterations int
	// FinishReason 是最终模型结束原因（"stop"、"length" 等）。
	FinishReason string
}

// Agent 在一个会话历史上运行模型↔工具循环。
type Agent interface {
	// Run 执行一个轮次。它在模型不再请求工具、达到轮数上限、context 被取消或
	// 发生致命错误时返回。context 被取消时必须返回 ctx.Err()。
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// ToolActivator 是 Agent 的可选扩展：按会话**纯增量**激活已注册的工具。
//
// 为什么是可选接口而不是 Agent 的方法：rig.Agent 只有一个方法（Run），宿主可以
// 自己实现并替换；把动态加载塞进主接口会让所有实现（包括测试替身）被迫跟进。
// 这与 HistoryReplacer / NoticeSource / DenialExplainer 的做法一致。
//
// 语义（缓存契约）：
//   - ActivateTools 只加不减：已 active 的名字也会出现在 activated 里（幂等），
//     但内部集合与注册顺序不变；
//   - 未知名字进 unknown，不报错、不改变任何状态；
//   - 请求里的 tools 用**会话的 active 顺序**（激活时 append），因此激活只在末尾
//     追加，已激活部分逐字节稳定（provider 的 prefix cache 不会因为激活而整段失效）。
type ToolActivator interface {
	// ActivateTools 激活 names 里的工具（纯增量），返回生效名字（含已激活的）
	// 与未知名字。sessionID 为空表示默认会话。
	ActivateTools(sessionID string, names ...string) (activated, unknown []string)
	// ActiveTools 返回该会话当前可调用的工具名（按注册顺序）。
	ActiveTools(sessionID string) []string
}

// AgentSpec 描述一个 agent 的静态配置。agent 实现根据 spec 构造自身；
// 调用方传入已解码的配置。
type AgentSpec struct {
	// Model 是默认模型名。
	Model string
	// SystemPrompt 非空时会在每次运行前加到请求最前面。
	SystemPrompt string
	// SystemPromptFunc 非 nil 时**按运行**计算系统提示词，覆盖 SystemPrompt。
	//
	// 它解决的是"长驻进程里的环境块会过期"：日期、git 分支、当前工作区、当前
	// 权限档这类内容在构造 agent 的那一刻就冻结了，跨天或切分支之后模型看到的仍是
	// 旧值。返回空串表示这次运行没有系统提示词（不会回退到 SystemPrompt）。
	//
	// 每次 Run 调用一次，时机在**回合钩子 BeforeRun 之后、读取历史之前**；同一轮里
	// 的多次模型调用共用本次算出的值（保证前缀稳定）。实现应当快速、幂等，不要在
	// 这里做 I/O 密集的事。子 agent **不继承**它（子 spec 的提示词自己决定）。
	SystemPromptFunc func(ctx context.Context, req RunRequest) string
	// MaxIterations 是默认轮数上限；<=0 表示不限制轮数（unlimited，默认），
	// >0 时是显式上限，超限返回包装 ErrMaxIterations 的错误。
	MaxIterations int
	// Tools 是暴露给模型的工具。
	Tools []Tool
	// InitialActiveTools 指定会话开始时"可调用"的工具名（只影响发给模型的
	// tools 数组与派发，不影响注册）。nil 表示全部可调用（v0.20 行为）；非 nil 时
	// 其余工具仍然注册，但既不会出现在请求里，被模型调用时也会得到可读错误。
	//
	// 之后用 rig.ToolActivator.ActivateTools 做**纯增量**激活：只加不减，因此
	// 请求前缀在稳定后不会因为激活而整体变化。
	InitialActiveTools []string
	// Provider 是模型后端。
	Provider Provider
	// History 保存对话；nil 表示关闭持久化。
	History History
	// Gate 是默认审批门禁；nil 表示全部放行。
	Gate Gate
	// Temperature/TopP/MaxTokens 是默认采样选项（0/nil 表示未设置）。
	Temperature *float64
	TopP        *float64
	MaxTokens   int
	// ContextBudget 是估算的 prompt token 预算；>0 且 Compactor 非 nil 时，
	// agent 在每次模型调用前检查对话大小并在超预算时压缩。
	ContextBudget int
	// Compactor 压缩超预算的对话；nil 表示不压缩。常用实现见 session/compact。
	Compactor Compactor
	// Spill 把超过阈值的工具结果落盘，模型只看到摘要与文件路径；nil 表示不落盘。
	Spill *SpillPolicy
	// CheckpointStore 在回合边界持久化恢复检查点；nil 表示关闭检查点。配置后
	// agent 会在模型请求前、工具执行前与回合正常结束时写入检查点，并在 Run
	// 开始时恢复被中断的回合。
	CheckpointStore CheckpointStore
	// Logger 接收诊断输出；nil 表示丢弃。
	Logger Logger
	// Hooks 在循环边界被调用（回合开始/结束、工具调用前后），按切片顺序执行。
	//
	// 它是宿主接入"循环内部"那几件事的缝：回合快照、事件双写、成本记账、工具
	// 结果后处理（脱敏/追加提示）。这些事情发生在 Run 内部，宿主在 Run 外面包一层
	// 是够不着的（一轮里可能有多次模型调用）。
	Hooks []TurnHook
	// ActivationStore 非 nil 时持久化按会话的动态工具激活结果：首次访问某个
	// SessionID 的 active 集合时 Load 并与 InitialActiveTools 合并，ActivateTools
	// 成功后 Save。SessionID 为空时跳过持久化。
	//
	// 它在 InitialActiveTools 为 nil（全部 active，v0.20 行为）时不参与：那时
	// "激活"没有意义，Load 出来的集合也只是全集的一部分。
	ActivationStore ToolActivationStore
}

// TurnHook 观察并参与一次运行的边界。
//
// 调用时机与线程：同一次 Run 内**按顺序串行**调用（工具执行本身是并行的，但钩子
// 不会在同一轮里并发进入）。注意 Agent **允许并发 Run**：同一个 hook 实例会被多个
// Run 并发进入，实现需要自己加锁（字段是共享的）。不要在钩子里跑长任务（会阻塞
// 这一轮；要长任务请自己起 goroutine）。
//
// 四个边界：
//   - BeforeRun：读取历史、追加本次 Input 之前。返回错误会中止本次运行，错误原样
//     返回给调用方（适合"前置条件不满足就拒绝"）；
//   - AfterRun：**一定**调用一次——成功、失败、取消，以及 BeforeRun 拒绝的那次
//     （此时 RunResult 是零值）。它用于收尾 BeforeRun 打开的回合，panic 会被隔离
//     （记日志，不改变调用方看到的结果）；
//   - BeforeTool：每个调用在**审批与执行之前**。返回错误会让这次调用直接失败
//     （不执行、也不去问人），错误文本作为工具结果回给模型，整轮继续；
//   - AfterTool：调用执行完成、写入历史与事件**之前**，可以改写 result（例如补丁
//     结果、加提示、脱敏）。只对真正执行过的调用调用一次；被拒绝的调用不会走到
//     这里（它们的结果由审批理由生成）。
//
// 子 agent 不继承父的钩子（子 spec 是父 spec 的浅拷贝，构造时显式清空）：父会话的
// 快照/记账不该为子 agent 的运行再触发一次。
type TurnHook interface {
	BeforeRun(ctx context.Context, req RunRequest) error
	AfterRun(ctx context.Context, result RunResult)
	BeforeTool(ctx context.Context, call ToolCall) error
	AfterTool(ctx context.Context, call ToolCall, result *ToolResult)
}

// Logger 是 Rig 组件可以使用的最小日志接口。
type Logger interface {
	Printf(format string, args ...any)
}

// Settings 是一个极简的键值设置接口（产品通常落到文件）。
type Settings interface {
	Get(key, def string) string
	Set(key, value string) error
}
