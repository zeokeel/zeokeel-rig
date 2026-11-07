package rig

import "context"

// Tool 是模型可以调用的一项能力。当 agent 并发执行一批调用时，实现必须保证
// 并发调用安全。
type Tool interface {
	// Name 是模型侧的函数名（稳定，且在注册表中唯一）。
	Name() string
	// Description 告诉模型该在什么场景使用这个工具。
	Description() string
	// Schema 是参数对应的 JSON Schema 对象。
	Schema() map[string]any
	// Invoke 执行一次调用。返回 error 会作为工具错误上报给模型（在 agent 被配置
	// 为继续运行时）；返回 IsError=true 的 ToolResult 则按工具自身的错误文本透传。
	Invoke(ctx context.Context, call ToolCall) (ToolResult, error)
}

// ToolResult 是工具返回值。Content 是回填给模型、作为 tool 消息的文本（可能被
// 调用方的策略截断）；IsError 标记执行失败。
//
// Parts 携带可选的非文本附件（例如 read_image 工具读到的图片）。Provider 无法把
// 它们放进 tool 消息本身，因此 agent 会在工具结果之后追加一条 user 消息来传递；
// 该字段是增量式的，纯文本工具保持 nil。
type ToolResult struct {
	Content string
	IsError bool
	Parts   []ContentPart
}

// Decision 是一个待执行工具调用的审批结果。
type Decision string

const (
	DecisionAllow       Decision = "allow"        // 放行本次调用
	DecisionAllowAlways Decision = "allow-always" // 放行本次及后续相同调用
	DecisionDeny        Decision = "deny"         // 拒绝并向模型报告
)

// Gate 决定一次工具调用是否允许执行。它在 agent 运行的 goroutine 上、执行之前
// 被调用，可以阻塞等待用户输入。nil Gate 表示使用 agent 配置的默认策略（对受信
// 工具通常是“全部放行”）。
type Gate interface {
	Approve(ctx context.Context, tool Tool, call ToolCall) (Decision, error)
}

// ToolLister 由可以枚举自身工具的工具注册表实现。
type ToolLister interface {
	Tools() []Tool
}

// History 按会话保存对话消息。实现必须并发安全；内存实现位于 session/ 包。
type History interface {
	// Load 返回某个会话已存储的消息（未知会话返回 nil）。
	Load(ctx context.Context, sessionID string) ([]Message, error)
	// Append 以一次调用为原子单位存储消息。
	Append(ctx context.Context, sessionID string, messages ...Message) error
}
