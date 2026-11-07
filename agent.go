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
	// MaxIterations 限制模型↔工具轮数；0 表示使用 agent 默认值。
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

// AgentSpec 描述一个 agent 的静态配置。agent 实现根据 spec 构造自身；
// 调用方传入已解码的配置。
type AgentSpec struct {
	// Model 是默认模型名。
	Model string
	// SystemPrompt 非空时会在每次运行前加到请求最前面。
	SystemPrompt string
	// MaxIterations 是默认轮数上限（0 表示 20）。
	MaxIterations int
	// Tools 是暴露给模型的工具。
	Tools []Tool
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
