package openai

import "encoding/json"

// This file is the wire format: the OpenAI-compatible chat-completions request
// body and the SSE event payloads. Only fields the provider reads or writes are
// modelled; servers' extra fields are ignored by encoding/json.

// chatRequest is the request body sent to POST {BaseURL}/chat/completions.
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

// streamOptions asks the server to append a final SSE event carrying token
// accounting. Without it OpenAI-compatible streams report no usage.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatMessage is one wire-format conversation entry. Content is always present:
// a plain JSON string for text-only messages (including tool/system messages)
// and an array of parts when the message carries images.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// chatContentPart is one element of the multimodal content array.
type chatContentPart struct {
	Type     string        `json:"type"`
	Text     *string       `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

// chatImageURL is the image_url payload of one image part.
type chatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// chatToolCall is an assistant message's tool invocation in the wire format.
type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatTool is one function declaration in the tools array.
type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// streamEvent is one decoded `data:` payload. Servers normally put one choice
// delta in each event; usage arrives in a final event with an empty choices
// array; an error object may replace the delta after the stream is established.
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

// streamToolCall is one partial tool call. Index identifies the call; ID and
// Name arrive in the first fragment and Arguments accumulates across fragments.
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

// streamUsage is the OpenAI usage object. Cached prompt tokens are reported by
// OpenAI nested under prompt_tokens_details and by DeepSeek at the flat
// prompt_cache_hit_tokens key; both are understood.
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

// streamError is the in-band error object some servers emit after the response
// has already been established with a 2xx status.
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

// encodeJSON is a tiny helper so callers of marshal helpers stay readable.
func encodeJSON(value any) ([]byte, error) { return json.Marshal(value) }
