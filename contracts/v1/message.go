package v1

import "strings"

// Role identifies who produced a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType is the kind of one content part. v1 supports text and image URLs;
// new kinds are additive.
type PartType string

const (
	PartText     PartType = "text"
	PartImageURL PartType = "image_url"
)

// ContentPart is one piece of a message's content. Text parts carry Text;
// image parts carry ImageURL (and an optional Detail hint).
type ContentPart struct {
	Type     PartType `json:"type"`
	Text     string   `json:"text,omitempty"`
	ImageURL string   `json:"image_url,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// TextPart builds a text content part.
func TextPart(text string) ContentPart { return ContentPart{Type: PartText, Text: text} }

// ImagePart builds an image content part from a URL (http(s) or data URL).
func ImagePart(url, detail string) ContentPart {
	return ContentPart{Type: PartImageURL, ImageURL: url, Detail: detail}
}

// ToolCall is one tool invocation requested by the model. Arguments is the raw
// JSON object string exactly as the model produced it.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one conversation message. Tool calls on an assistant message and
// the tool result messages that answer them are separate messages, as in the
// OpenAI-compatible wire format.
type Message struct {
	Role       Role          `json:"role"`
	Content    []ContentPart `json:"content,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"` // role=tool
	Name       string        `json:"name,omitempty"`         // tool name or participant
	// Reasoning carries provider-exposed reasoning text (e.g. a thinking
	// summary) so history and UI can show it; it is never replayed as content.
	Reasoning string `json:"reasoning,omitempty"`
}

// NewMessage builds a single-text-part message.
func NewMessage(role Role, text string) Message {
	return Message{Role: role, Content: []ContentPart{TextPart(text)}}
}

// Text concatenates the text parts of the message.
func (m Message) Text() string {
	var b strings.Builder
	for _, part := range m.Content {
		if part.Type == PartText {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// Usage is token accounting for one model call (zeros when the provider does
// not report usage; callers must not fabricate numbers).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
}

// Add accumulates another usage record.
func (u *Usage) Add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	u.CachedTokens += other.CachedTokens
}

// ToolSpec is the schema advertised to the model for one tool.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"` // JSON Schema object
}
