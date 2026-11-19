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
