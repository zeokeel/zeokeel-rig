package kit

import "github.com/zeokeel/zeokeel-rig/sandbox"

import "github.com/zeokeel/zeokeel-rig"

// 以下所有别名都指向根包中的确切类型，因此用 kit 构造的值可以与
// github.com/zeokeel/zeokeel-rig、agent、tools 以及其他所有包直接互操作，无需
// 类型转换。这组别名就是“一次导入”的契约面：基本消费者定义消息、实现
// Provider/Tool/Gate/History、观察事件所需的类型都在这里。

// Agent 侧契约。
type (
	// Provider 是模型后端；实现它可以接入自定义模型。
	Provider = rig.Provider
	// Request 是一次模型补全请求（Provider 侧契约）。
	Request = rig.Request
	// Stream 是一次模型响应流（Provider 侧契约）。
	Stream = rig.Stream
	// Chunk 是补全流中的一个分片（Provider 侧契约）。
	Chunk = rig.Chunk
	// ToolSpec 是向模型声明的一个工具规格。
	ToolSpec = rig.ToolSpec
	// ToolCallDelta 是流式响应中一个工具调用的片段。
	ToolCallDelta = rig.ToolCallDelta
	// Tool 是模型可以调用的一项能力。
	Tool = rig.Tool
	// ToolCall 是模型请求的一次工具调用。
	ToolCall = rig.ToolCall
	// ToolResult 是工具返回值。
	ToolResult = rig.ToolResult
	// Gate 决定一次工具调用是否允许执行。
	Gate = rig.Gate
	// Decision 是一个待执行工具调用的审批结果。
	Decision = rig.Decision
	// History 按会话保存对话消息。
	History = rig.History
	// RunRequest 是某个会话的一次 agent 轮次（或继续）。
	RunRequest = rig.RunRequest
	// RunResult 是一次 agent 运行的结果。
	RunResult = rig.RunResult
	// Event 是 agent 运行中的一个可观察步骤。
	Event = rig.Event
	// EventType 标识一个 agent 事件。
	EventType = rig.EventType
	// Message 是一条对话消息。
	Message = rig.Message
	// Role 标识一条消息由谁产生。
	Role = rig.Role
	// ContentPart 是消息内容中的一个分片。
	ContentPart = rig.ContentPart
	// PartType 是一个内容分片的类型。
	PartType = rig.PartType
	// Usage 是一次模型调用的 token 统计。
	Usage = rig.Usage
)

// 消息角色。与 OpenAI 兼容线格式一致。
const (
	RoleSystem    = rig.RoleSystem
	RoleUser      = rig.RoleUser
	RoleAssistant = rig.RoleAssistant
	RoleTool      = rig.RoleTool
)

// 内容分片类型。
const (
	PartText     = rig.PartText
	PartImageURL = rig.PartImageURL
)

// Gate 返回的审批结果。
const (
	DecisionAllow       = rig.DecisionAllow
	DecisionAllowAlways = rig.DecisionAllowAlways
	DecisionDeny        = rig.DecisionDeny
)

// 通过 RunRequest.Emit / Agent.RunText 的 emit 观察到的事件类型。
const (
	EventTurnStart      = rig.EventTurnStart
	EventTextDelta      = rig.EventTextDelta
	EventReasoningDelta = rig.EventReasoningDelta
	EventToolCall       = rig.EventToolCall
	EventToolResult     = rig.EventToolResult
	EventUsage          = rig.EventUsage
	EventMessage        = rig.EventMessage
	EventTurnEnd        = rig.EventTurnEnd
	EventError          = rig.EventError
)

// Version 是 ZeoKeel Rig 的模块版本（与 rig.Version 相同，门面无需第二次 import
// 即可上报模块版本）。
const Version = rig.Version

// NewMessage 构造一个只含单个文本分片的消息。
func NewMessage(role Role, text string) Message { return rig.NewMessage(role, text) }

// TextPart 构造一个文本内容分片。
func TextPart(text string) ContentPart { return rig.TextPart(text) }

// ImagePart 用 URL（http(s) 或 data URL）构造一个图片内容分片。
func ImagePart(url, detail string) ContentPart { return rig.ImagePart(url, detail) }

// 哨兵错误，调用方可用 errors.Is 判断。它们是根包值的别名；Provider 与工具可以
// 用上下文信息包装它们。
var (
	// ErrProviderRequired：agent 未配置 provider 时返回。
	ErrProviderRequired = rig.ErrProviderRequired
	// ErrModelRequired：没有任何可用的模型名时返回。
	ErrModelRequired = rig.ErrModelRequired
	// ErrToolNotFound：工具注册表被要求调用一个未注册的工具时返回。
	ErrToolNotFound = rig.ErrToolNotFound
	// ErrMaxIterations：循环达到轮数上限、但模型仍在请求工具时返回。
	ErrMaxIterations = rig.ErrMaxIterations
)

// 权限模式是本模块 sandbox 包提供的权限模式的别名。
type Mode = sandbox.Mode

const (
	ModeWorkspaceWrite   = sandbox.ModeWorkspaceWrite
	ModeReadOnly         = sandbox.ModeReadOnly
	ModeDangerFullAccess = sandbox.ModeDangerFullAccess
)
