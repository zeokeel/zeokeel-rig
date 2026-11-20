package events

import (
	"encoding/json"
	"errors"
	"time"
)

// Version 是线格式版本。不兼容变更（改名、删字段、改语义、改 payload 编码）
// 必须 +1；新增 kind 或新增字段是兼容变更，不必 +1。
const Version = 1

// MaxLineBytes 是单行（不含结尾换行）允许的最大字节数：1 MiB。
//
// 上限存在的意义是让"写坏了的事件"尽早暴露：Emitter 在写出之前拒绝超长事件，
// Decoder 遇到超长行返回 ErrLineTooLong，而不是让一整条流因为一个巨型 payload
// 变得不可解析。
const MaxLineBytes = 1 << 20

// 事件 kind。这些字符串是线格式契约的一部分，重命名即不兼容变更。
const (
	// KindRunStart 一次运行开始。
	KindRunStart = "run.start"
	// KindRunEnd 一次运行结束（含累计用量与错误）。
	KindRunEnd = "run.end"
	// KindMessage 一条完整的 assistant/user 消息。
	KindMessage = "message"
	// KindMessageDelta 流式文本增量。
	KindMessageDelta = "message.delta"
	// KindReasoning 思考/推理增量。
	KindReasoning = "reasoning.delta"
	// KindToolCall 模型请求了一次工具调用。
	KindToolCall = "tool.call"
	// KindToolResult 一次工具调用结束。
	KindToolResult = "tool.result"
	// KindToolActivated 动态工具加载：一批已注册工具被激活。
	KindToolActivated = "tool.activated"
	// KindApprovalRequest 请求人工审批。
	KindApprovalRequest = "approval.request"
	// KindApprovalResponse 审批结论。
	KindApprovalResponse = "approval.response"
	// KindSandboxEscalation 一次"逐命令提权"请求（无论批准或拒绝）。
	//
	// 它是**审计痕迹**：提权是唯一"让人决定是否跳出沙箱"的入口，必须可追溯。
	// payload 见 kit 的 escalationPayload（tool/argv/decision/reason 摘要）。
	KindSandboxEscalation = "sandbox.escalation"
	// KindDiff 一次文件改动。
	KindDiff = "diff"
	// KindNotice 诊断/提示（无 payload 或自由 payload）。
	KindNotice = "notice"
	// KindError 错误。
	KindError = "error"
)

// 本包返回的哨兵错误。调用方用 errors.Is 判断。
var (
	// ErrUnsupportedVersion 表示事件行的 version 与本包不同（更高，或缺失/非正）。
	// 读到它时调用方应停止消费该流，而不是猜测字段含义。
	ErrUnsupportedVersion = errors.New("events: 不支持的事件版本")
	// ErrLineTooLong 表示单行超过 MaxLineBytes。
	ErrLineTooLong = errors.New("events: 事件行超过单行字节上限")
	// ErrMalformed 表示某行不是合法的 Envelope JSON（含被截断的行）。
	ErrMalformed = errors.New("events: 事件行不是合法 JSON")
)

// Envelope 是一条事件的外壳。
//
// Payload 是原始 JSON，因此"未知 kind 的 payload"可以被原样搬运：消费方不必
// 理解 payload 也能转发、存储、回放它。
type Envelope struct {
	// Version 是线格式版本；Emitter 总是写 Version，Decoder 拒绝其他版本。
	Version int `json:"version"`
	// Seq 是单 emitter 内从 1 开始的单调序号。
	Seq int64 `json:"seq"`
	// Time 是事件发生时刻（UTC，RFC3339Nano）。
	Time time.Time `json:"time"`
	// Kind 是事件类型，见 Kind* 常量；未知值必须被透传。
	Kind string `json:"kind"`
	// Payload 是事件体；nil 时省略该字段。
	Payload json.RawMessage `json:"payload,omitempty"`
}

// 以下 payload 类型是内置 kind 的线格式形状。
//
// 约定：字段名稳定（snake_case）；零值字段优先省略（omitempty），消费方必须把
// "缺失"等同"零值"。没有用 map 的 payload，因此编码字节确定。

// RunStart 是 KindRunStart 的 payload。
type RunStart struct {
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Workspace string `json:"workspace,omitempty"`
}

// RunEnd 是 KindRunEnd 的 payload。
type RunEnd struct {
	// Turns 是**模型轮数**（一次 Run 里发起过几次模型调用），对应
	// rig.RunResult.Iterations。旧版本曾误填消息条数，消费方如仍按消息数用，请
	// 改读 Messages。
	Turns int `json:"turns"`
	// Messages 是本次 Run 追加的消息条数（含输入、assistant 与 tool 消息）。
	Messages         int    `json:"messages,omitempty"`
	ToolCalls        int    `json:"tool_calls,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	Err              string `json:"err,omitempty"`
}

// Message 是 KindMessage 的 payload：一条完整消息。
type Message struct {
	Role      string `json:"role"`
	Text      string `json:"text"`
	ToolCalls int    `json:"tool_calls,omitempty"`
}

// MsgDelta 是 KindMessageDelta 与 KindReasoning 的 payload：流式增量。
type MsgDelta struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// ToolCall 是 KindToolCall 的 payload。Arguments 是模型给出的原始 JSON 文本
// （不保证合法 JSON，消费方需自行容错）。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolResult 是 KindToolResult 的 payload。
type ToolResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content,omitempty"`
	// IsError 表示工具执行失败；Denied 表示失败源于审批/沙箱拒绝。
	IsError    bool  `json:"is_error,omitempty"`
	Denied     bool  `json:"denied,omitempty"`
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Data 是宿主的结构化工具结果（rig.ToolResult.Data 原样转发），例如
	// 后台任务的 id/状态/退出码。它是原始 JSON，因此回放/UI 不必从 Content 的
	// 人读文本里反解析状态；nil 表示这次调用没有结构化结果（字段被省略）。
	// 工具给出的值即使是 typed-nil（例如 `(*T)(nil)`、`json.RawMessage(nil)`）也
	// 按 nil 处理——不会写出 `"data":null`。
	//
	// 工具返回的值不可 JSON 序列化时，事件桥会留空该字段并补一条 KindNotice
	// （"tool result data was not serializable"），tool.result 本身照发。
	Data json.RawMessage `json:"data,omitempty"`
}

// ToolActivated 是 KindToolActivated 的 payload。
//
// 动态工具加载是**纯增量**的：这次激活之后，请求里的 tools 数组只在末尾追加。
// UI 可以据此提示"已加载 N 个工具"，或高亮"正在使用扩展能力"。
type ToolActivated struct {
	// Tools 是本次真正新增的工具名（已 active 的名字不会出现在这里）。
	Tools []string `json:"tools"`
	// Trigger 是谁触发的："model"（模型通过 tool_search 激活）或 "host"
	// （宿主直接调用 ActivateTools）。
	Trigger string `json:"trigger"`
}

// ApprovalRequest 是 KindApprovalRequest 的 payload。
type ApprovalRequest struct {
	Tool   string `json:"tool"`
	Args   string `json:"args,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ApprovalResponse 是 KindApprovalResponse 的 payload。
type ApprovalResponse struct {
	Tool string `json:"tool"`
	// Decision 是审批结论，约定取值 "allow" / "deny"（由批准方决定用词）。
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// Diff 是 KindDiff 的 payload。Op 约定取 "add" / "modify" / "delete"。
type Diff struct {
	Path    string `json:"path"`
	Op      string `json:"op"`
	Added   int    `json:"added,omitempty"`
	Removed int    `json:"removed,omitempty"`
}

// Error 是 KindError 的 payload。Stage 标识出错阶段（例如 "provider"、
// "tool"、"approval"），便于消费方分流。
type Error struct {
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

// normalizeTime 把时刻规范成 UTC。
//
// 线格式只出现 UTC：本地时区偏移会让同一时刻在不同机器上产生不同字节，也会让
// 快照与日志比对失效。
func normalizeTime(t time.Time) time.Time {
	return t.UTC()
}
