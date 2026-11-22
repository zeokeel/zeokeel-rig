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

// DiscoverableTool 是 Tool 的**可选**扩展：声明这个工具能不能被 `tool_search`
// 搜到（动态工具加载的清单）。
//
// 默认（不实现该接口）是可发现。返回 false 的工具不会出现在 `tool_search` 的
// 清单里，但**仍然注册**：宿主可以显式 ActivateTools 激活它，已经在 active 集合
// 里时模型也能直接调用。用途是"内部/管理类工具"——它们不该被模型翻出来，但宿主
// 的 UI 仍要能按名字打开开关。
//
// 它只影响发现清单，不影响权限、审批、账本或 prefix cache 契约。
type DiscoverableTool interface {
	// Discoverable 返回该工具是否出现在 tool_search 的清单里。
	Discoverable() bool
}

// IsToolDiscoverable 报告 tool 是否可被 tool_search 发现：没实现 DiscoverableTool
// 的工具默认可见。nil 工具总返回 false（没有名字可供发现）。
func IsToolDiscoverable(tool Tool) bool {
	if tool == nil {
		return false
	}
	if declared, ok := tool.(DiscoverableTool); ok {
		return declared.Discoverable()
	}
	return true
}

// ToolResult 是工具返回值。Content 是回填给模型、作为 tool 消息的文本（可能被
// 调用方的策略截断）；IsError 标记执行失败。
//
// Parts 携带可选的非文本附件（例如 read_image 工具读到的图片）。Provider 无法把
// 它们放进 tool 消息本身，因此 agent 会在工具结果之后追加一条 user 消息来传递；
// 该字段是增量式的，纯文本工具保持 nil。
//
// Data 是**给宿主**的可选结构化结果（例如后台任务的 id / 状态 / 退出码）。它只会
// 出现在 `rig.Event{Type: EventToolResult}.Result` 里，**不会**进入模型消息：模型
// 面永远是 Content。宿主的状态栏、任务卡与事件订阅因此不必去解析人读文本（"state:
// exited" 这类文本格式对模型友好，对机器是脆弱的）。
//
// 约定：Data 必须是可 JSON 序列化的值（map/切片/标量）；工具不必保证类型稳定，
// 但同一个工具在不同状态之间应尽量保持字段名一致。nil 表示"没有结构化结果"。
//
// Data 与 Content 一样原样发出：SDK 不做输出脱敏（见 agent.New 的说明）。
type ToolResult struct {
	Content string
	IsError bool
	Parts   []ContentPart
	Data    any
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

// HistoryReplacer 是 History 的可选扩展：原子替换一个会话的全部消息。手动上下文
// 压缩（kit.Agent.CompactNow）需要持久化新的对话窗口时使用它。
type HistoryReplacer interface {
	Replace(ctx context.Context, sessionID string, messages []Message) error
}
