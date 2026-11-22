package observability

import "time"

// 本文件定义两类结构体：持久事件的 JSON payload（宿主写入侧，也是折叠的输入），
// 以及折叠后的只读模型（宿主读取侧）。payload 与模型分开，是因为 payload 必须
// 与已经写进日志的历史数据保持兼容（键名、默认值），而模型可以按读取需要演进。

// 持久事件 payload。宿主用 json.Marshal 把下面的结构体写进
// rig.SessionEvent.Payload；折叠时按事件类型解码对应的结构体。

// ContextSnapshotPayload 是 context_snapshot 事件的 payload：每次 run 一条，在
// 第一次 provider 调用前记录。
type ContextSnapshotPayload struct {
	// Model 是本轮使用的模型名。
	Model string `json:"model"`
	// Window 是模型的上下文窗口（token，0 表示未知）。
	Window int `json:"window"`
	// SystemPromptBytes 与 SystemPromptTokens 是最终渲染的 system prompt 大小。
	SystemPromptBytes  int `json:"systemPromptBytes"`
	SystemPromptTokens int `json:"systemPromptTokens"`
	// Sections 是按段拆分的 system prompt 明细（可为空）。
	Sections []Section `json:"sections,omitempty"`
	// Tools 是本轮请求携带的工具目录。
	Tools []Tool `json:"tools,omitempty"`
	// ToolCount 是工具数量；宿主通常填 len(Tools)，折叠时以它兜底。
	ToolCount int `json:"toolCount"`
	// History 是 run 开始时的历史消息计数。
	History History `json:"history"`
	// Injected 记录哪些可选注入项出现在最终 prompt 里。
	Injected Injected `json:"injected"`
	// EstimatedPrompt 是第一次 provider 调用前的 prompt token 估算。
	EstimatedPrompt int `json:"estimatedPrompt"`
}

// ModelCallPayload 是 model_call 事件的 payload：每次 provider 调用一条。
type ModelCallPayload struct {
	// Iteration 是本轮 run 内的模型/工具轮次（1 起）。
	Iteration int `json:"iteration"`
	// Model 是本次调用使用的模型名。
	Model string `json:"model"`
	// Prompt、Cached、Completion 与 Total 是 provider 报告的 token 用量。
	// Cached 是 Prompt 的子集。
	PromptTokens     int `json:"promptTokens"`
	CachedTokens     int `json:"cachedTokens,omitempty"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
	// ContextTokens 是这次调用真实看到的上下文 token 数（通常等于 PromptTokens）。
	ContextTokens int `json:"contextTokens"`
}

// CompactPayload 是 compact 事件的 payload：一次手动/自动压缩的结果。
type CompactPayload struct {
	// Kind 是压缩类型标签；空字符串在折叠时归一为 "manual"。
	Kind string `json:"kind"`
	// Before 与 After 是压缩前后的上下文 token 数。
	Before int `json:"before"`
	After  int `json:"after"`
	// Replaced 与 Saved 是被替换/被保留的消息条数。
	Replaced int `json:"replaced"`
	Saved    int `json:"saved"`
	// Inferred 标记"由 prompt token 骤降推断出的"自动压缩（agent 内部的自动
	// 压缩不可直接观测，只能这样记录）。
	Inferred bool `json:"inferred,omitempty"`
	// Outcome 是压缩失败时的原因（"failed"/"no-reduction"/"archive-failed"…），
	// 空字符串表示成功或手动/推断压缩。
	Outcome string `json:"outcome,omitempty"`
}

// SpillPayload 是 spill 事件的 payload：一条工具结果被落盘摘要替换。
type SpillPayload struct {
	// CallID 是被替换结果所属的工具调用 id。
	CallID string `json:"callId"`
	// Tool 是工具名。
	Tool string `json:"tool"`
	// Path 是文件后端摘要的落盘位置（store 后端为空）。
	Path string `json:"path,omitempty"`
	// Ref 是 store 后端的句柄（文件后端为空）。
	Ref string `json:"ref,omitempty"`
	// Bytes 是被落盘内容的字节数。
	Bytes int `json:"bytes"`
	// Preview 是摘要里的一行预览；折叠时会被截断到 MaxPreviewRunes。
	Preview string `json:"preview,omitempty"`
}

// Section 是 system prompt 的一段。
type Section struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Source string `json:"source"`
	Bytes  int    `json:"bytes"`
	Tokens int    `json:"tokens"`
	Hash   string `json:"hash"`
}

// Tool 是工具目录里的一个工具。
type Tool struct {
	Name   string `json:"name"`
	Group  string `json:"group,omitempty"`
	Tokens int    `json:"tokens,omitempty"`
}

// History 是请求历史的消息计数明细。
type History struct {
	// Messages 是消息总数。
	Messages int `json:"messages"`
	// User、Assistant 按角色计数；Tool 是 assistant 消息携带的工具调用数。
	User      int `json:"user"`
	Assistant int `json:"assistant"`
	Tool      int `json:"tool"`
}

// Injected 记录哪些可选的 prompt 注入项存在。
type Injected struct {
	// AgentsMD 表示工作区指令文件（如 AGENTS.md）被注入。
	AgentsMD bool `json:"agentsMD,omitempty"`
	// Preset 表示 agent preset 的指令被注入。
	Preset bool `json:"preset,omitempty"`
	// Memory 表示记忆摘要被注入。
	Memory bool `json:"memory,omitempty"`
	// Skills 表示技能目录被注入。
	Skills bool `json:"skills,omitempty"`
}

// 折叠后的只读模型。

// Snapshot 是最近一次 context_snapshot 的折叠结果。
type Snapshot struct {
	// Seq 是快照事件的持久序号（0 表示未知）。
	Seq int64 `json:"seq,omitempty"`
	// At 是快照事件的时间，原样携带。
	At time.Time `json:"at,omitempty"`
	// Model 是模型名（已去空白）。
	Model string `json:"model,omitempty"`
	// Window 是快照记录的上下文窗口（0 表示未知）。
	Window int `json:"window,omitempty"`
	// SystemPromptBytes 与 SystemPromptTokens 是 system prompt 的大小。
	SystemPromptBytes  int `json:"systemPromptBytes,omitempty"`
	SystemPromptTokens int `json:"systemPromptTokens,omitempty"`
	// Sections 是 system prompt 的分段明细。
	Sections []Section `json:"sections,omitempty"`
	// Tools 是工具目录。
	Tools []Tool `json:"tools,omitempty"`
	// ToolCount 是工具目录的大小；宿主只填 ToolCount 时以 payload 的值为准。
	ToolCount int `json:"toolCount,omitempty"`
	// History 是 run 开始时的历史消息计数。
	History History `json:"history"`
	// Injected 记录哪些可选注入项存在。
	Injected Injected `json:"injected"`
	// EstimatedPrompt 是第一次 provider 调用前的 prompt token 估算。
	EstimatedPrompt int `json:"estimatedPrompt,omitempty"`
}

// Call 是折叠后的一次 provider 调用记录。
type Call struct {
	// Seq 是调用事件的持久序号（0 表示未知）。
	Seq int64 `json:"seq,omitempty"`
	// At 是调用事件的时间，原样携带。
	At time.Time `json:"at,omitempty"`
	// Iteration 是本轮 run 内的模型/工具轮次（1 起）。
	Iteration int `json:"iteration,omitempty"`
	// Model 是本次调用的模型名（已去空白）。
	Model string `json:"model,omitempty"`
	// Prompt、Cached、Completion、Total 是 provider 报告的 token 用量。
	Prompt     int `json:"prompt,omitempty"`
	Cached     int `json:"cached,omitempty"`
	Completion int `json:"completion,omitempty"`
	Total      int `json:"total,omitempty"`
	// ContextTokens 是这次调用真实看到的上下文 token 数。
	ContextTokens int `json:"contextTokens,omitempty"`
}

// Compaction 是一次压缩审计记录。
type Compaction struct {
	// Seq 是压缩事件的持久序号（0 表示未知）。
	Seq int64 `json:"seq,omitempty"`
	// At 是压缩事件的时间，原样携带。
	At time.Time `json:"at,omitempty"`
	// Kind 是压缩类型标签（空 payload 归一为 "manual"）。
	Kind string `json:"kind,omitempty"`
	// Before 与 After 是压缩前后的上下文 token 数。
	Before int `json:"before,omitempty"`
	After  int `json:"after,omitempty"`
	// Replaced 与 Saved 是被替换/被保留的消息条数。
	Replaced int `json:"replaced,omitempty"`
	Saved    int `json:"saved,omitempty"`
	// Inferred 标记推断出的自动压缩。
	Inferred bool `json:"inferred,omitempty"`
	// Outcome 是压缩失败原因，空字符串表示成功。
	Outcome string `json:"outcome,omitempty"`
}

// Spill 是一次工具结果落盘审计记录。
type Spill struct {
	// Seq 是 spill 事件的持久序号（0 表示未知）。
	Seq int64 `json:"seq,omitempty"`
	// At 是 spill 事件的时间，原样携带。
	At time.Time `json:"at,omitempty"`
	// CallID 是被替换结果所属的工具调用 id。
	CallID string `json:"callId,omitempty"`
	// Tool 是工具名（已去空白）。
	Tool string `json:"tool,omitempty"`
	// Path 是文件后端的落盘路径。
	Path string `json:"path,omitempty"`
	// Ref 是 store 后端的句柄。
	Ref string `json:"ref,omitempty"`
	// Bytes 是被落盘内容的字节数。
	Bytes int `json:"bytes,omitempty"`
	// Preview 是截断到 MaxPreviewRunes 个 rune 的预览。
	Preview string `json:"preview,omitempty"`
}

// Tokens 是一个会话累计的 provider token 用量。
//
// 求和覆盖折叠过的全部 model_call（包括因 MaxCalls 被挤出列表的早期调用），
// 因此它不会随列表上界变化。Cached 是 Prompt 的子集，不是额外增量。
type Tokens struct {
	Prompt     int `json:"prompt,omitempty"`
	Cached     int `json:"cached,omitempty"`
	Completion int `json:"completion,omitempty"`
	Total      int `json:"total,omitempty"`
}

// Timeline 是一个会话折叠后的上下文可观测视图。
//
// 它是纯值：同样的输入事件序列永远得到同样的 Timeline。切片按日志顺序保存
// （最旧在前、最新在后），超过上界时丢弃最旧的元素，但 *Seen 计数与 Tokens
// 仍然覆盖全部事件。
type Timeline struct {
	// Revision 是成功折叠的事件条数；被跳过的事件不计入。
	Revision uint64 `json:"revision,omitempty"`
	// HasSnapshot 报告是否见过 context_snapshot（Snapshot 零值无法区分）。
	HasSnapshot bool `json:"hasSnapshot,omitempty"`
	// Snapshot 是最近一次 context_snapshot。
	Snapshot Snapshot `json:"snapshot"`
	// Calls 是最近 MaxCalls 次 provider 调用。
	Calls []Call `json:"calls,omitempty"`
	// Compactions 是最近 MaxEvents 次压缩审计记录。
	Compactions []Compaction `json:"compactions,omitempty"`
	// Spills 是最近 MaxEvents 次工具结果落盘审计记录。
	Spills []Spill `json:"spills,omitempty"`
	// Tokens 是全部调用累计的 token 用量。
	Tokens Tokens `json:"tokens"`
	// CallsSeen、CompactionsSeen、SpillsSeen 是折叠过的总条数（不受上界影响）。
	CallsSeen       int `json:"callsSeen,omitempty"`
	CompactionsSeen int `json:"compactionsSeen,omitempty"`
	SpillsSeen      int `json:"spillsSeen,omitempty"`
	// Iterations 是观测到的最大迭代序号（0 表示没有调用记录）。
	Iterations int `json:"iterations,omitempty"`
	// ToolCalls 是最近快照里历史明细报告的工具调用数（run 开始时的累计值）。
	ToolCalls int `json:"toolCalls,omitempty"`
	// Used 是当前上下文占用的 token 数：没有调用记录时取快照估算，否则取最近
	// 一次 provider 调用的 ContextTokens，压缩/恢复事件会把它改成压缩后的值。
	Used int `json:"used,omitempty"`
	// Window 是已知的上下文窗口：先取快照，可用模型目录信息覆盖（本包不查目录）。
	Window int `json:"window,omitempty"`
	// Skipped 是被跳过的事件条数：未知类型、空或 null payload、无法解码或字段
	// 类型不匹配的 payload。其它会话的事件不算跳过。
	Skipped int `json:"skipped,omitempty"`
}
