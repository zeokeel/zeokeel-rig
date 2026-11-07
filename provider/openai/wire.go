package openai

import "encoding/json"

// 本文件是线格式（wire format）：OpenAI 兼容 chat-completions 的请求体以及 SSE
// 事件载荷。只建模 provider 读取或写入的字段；服务器返回的额外字段会被
// encoding/json 忽略。

// chatRequest 是发送到 POST {BaseURL}/chat/completions 的请求体。
type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Tools         []chatTool     `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
}

// streamOptions 要求服务器追加一个携带 token 统计的最终 SSE 事件。没有它时，
// OpenAI 兼容流不会上报用量。
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatMessage 是线格式中的一条对话记录。Content 始终存在：纯文本消息（包括
// tool/system 消息）使用普通 JSON 字符串，带图片的消息使用分片数组。
type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// chatContentPart 是多模态 content 数组中的一个元素。
type chatContentPart struct {
	Type     string        `json:"type"`
	Text     *string       `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

// chatImageURL 是一个图片分片的 image_url 载荷。
type chatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// chatToolCall 是 assistant 消息中的一次工具调用（线格式）。
type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatTool 是 tools 数组中的一个函数声明。
type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// streamEvent 是一个解码后的 `data:` 载荷。服务器通常在每个事件里放一个 choice
// 增量；用量出现在 choices 数组为空的最终事件中；流建立之后，错误对象可能取代
// 增量出现。
type streamEvent struct {
	Choices []streamChoice `json:"choices"`
	Usage   *streamUsage   `json:"usage"`
	Error   *streamError   `json:"error"`
}

type streamChoice struct {
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	Reasoning        string           `json:"reasoning"`
	ToolCalls        []streamToolCall `json:"tool_calls"`
}

// streamToolCall 是一个不完整的工具调用。Index 标识该调用；ID 与 Name 在第一个
// 片段中到达，Arguments 跨片段累积。
type streamToolCall struct {
	Index    int              `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function streamToolCallFn `json:"function"`
}

type streamToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// streamUsage 是 OpenAI 的用量对象。缓存命中的 prompt token 在 OpenAI 中嵌套于
// prompt_tokens_details，在 DeepSeek 中位于扁平的 prompt_cache_hit_tokens 键；
// 两种都支持。
type streamUsage struct {
	PromptTokens            int                  `json:"prompt_tokens"`
	CompletionTokens        int                  `json:"completion_tokens"`
	TotalTokens             int                  `json:"total_tokens"`
	PromptTokensDetails     *promptTokensDetails `json:"prompt_tokens_details"`
	CompletionTokensDetails *completionDetails   `json:"completion_tokens_details"`
	PromptCacheHitTokens    int                  `json:"prompt_cache_hit_tokens"`
}

type promptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type completionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// streamError 是部分服务器在已用 2xx 建立响应之后，仍在带内发出的错误对象。
type streamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

func (e *streamError) text() string {
	if e == nil {
		return ""
	}
	switch {
	case e.Message != "" && e.Type != "":
		return e.Type + ": " + e.Message
	case e.Message != "":
		return e.Message
	case e.Type != "":
		return e.Type
	default:
		return "stream error"
	}
}

// encodeJSON 是一个小助手，让各个 marshal 辅助函数的调用点保持易读。
func encodeJSON(value any) ([]byte, error) { return json.Marshal(value) }
