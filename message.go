package rig

import "strings"

// Role 标识一条消息由谁产生。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType 是一个内容分片的类型。当前契约支持文本与图片 URL；新增类型属于
// 增量式变更。
type PartType string

const (
	PartText     PartType = "text"
	PartImageURL PartType = "image_url"
)

// ContentPart 是消息内容中的一个分片。文本分片携带 Text；图片分片携带
// ImageURL（以及可选的 Detail 提示）。
type ContentPart struct {
	Type     PartType `json:"type"`
	Text     string   `json:"text,omitempty"`
	ImageURL string   `json:"image_url,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// TextPart 构造一个文本内容分片。
func TextPart(text string) ContentPart { return ContentPart{Type: PartText, Text: text} }

// ImagePart 用 URL（http(s) 或 data URL）构造一个图片内容分片。
func ImagePart(url, detail string) ContentPart {
	return ContentPart{Type: PartImageURL, ImageURL: url, Detail: detail}
}

// ToolCall 是模型请求的一次工具调用。Arguments 是模型产出的原始 JSON 对象
// 字符串。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message 是一条对话消息。assistant 消息上的工具调用与回答它们的 tool 结果消息
// 是分开的两条消息，与 OpenAI 兼容线格式一致。
type Message struct {
	Role       Role          `json:"role"`
	Content    []ContentPart `json:"content,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"` // role=tool
	Name       string        `json:"name,omitempty"`         // 工具名或参与者名
	// Reasoning 携带 provider 暴露的推理文本（例如思考摘要），供历史与 UI 展示；
	// 它永远不会作为内容被回放。
	Reasoning string `json:"reasoning,omitempty"`
}

// NewMessage 构造一个只含单个文本分片的消息。
func NewMessage(role Role, text string) Message {
	return Message{Role: role, Content: []ContentPart{TextPart(text)}}
}

// Text 拼接消息中所有文本分片的文本。
func (m Message) Text() string {
	var b strings.Builder
	for _, part := range m.Content {
		if part.Type == PartText {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// Usage 是一次模型调用的 token 统计（provider 未上报用量时为零值；调用方不得
// 编造数字）。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
}

// Add 累加另一条用量记录。
func (u *Usage) Add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	u.CachedTokens += other.CachedTokens
}

// ToolSpec 是向模型声明的一个工具规格。
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"` // JSON Schema 对象
}
