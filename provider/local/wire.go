package local

// 本文件是线格式：请求体、SSE 事件载荷，以及为"本地服务的写法差异"准备的弹性
// 解码类型（content 可能是字符串或分片数组；tool_calls 的 arguments 可能是字符串
// 或 JSON 对象；function_call 是旧式单调用）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// chatRequest 是发送到 {BaseURL}{ChatPath} 的请求体。
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

// streamOptions 要求服务端追加一个携带用量的最终事件。
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatMessage 是线格式的一条对话记录。
type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// chatContentPart 是多模态 content 数组的元素。
type chatContentPart struct {
	Type     string        `json:"type"`
	Text     *string       `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

// chatImageURL 是图片分片的载荷。
type chatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// chatToolCall 是 assistant 消息里的一次工具调用。
type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatTool 是 tools 数组里的一份函数声明。
type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// streamEvent 是一个解码后的 data 载荷。
//
// Extra 捕获未知的顶层键？不需要：这里只建模读的字段。delta 里的未知键由
// streamDelta.Extra 按需保留（Quirks.ReasoningKeys 用得到）。
type streamEvent struct {
	Choices []streamChoice `json:"choices"`
	Usage   *streamUsage   `json:"usage"`
	Error   *streamError   `json:"error"`
}

type streamChoice struct {
	Delta        streamDelta  `json:"delta"`
	Message      *streamDelta `json:"message"`
	FinishReason *string      `json:"finish_reason"`
}

// streamDelta 是一个增量。content / reasoning / arguments 都用弹性类型解码。
type streamDelta struct {
	Content          flexText         `json:"content"`
	ReasoningContent flexText         `json:"reasoning_content"`
	Reasoning        flexText         `json:"reasoning"`
	ToolCalls        []streamToolCall `json:"tool_calls"`
	FunctionCall     *streamFunction  `json:"function_call"`
	// Extra 收集未建模的字符串字段，供 Quirks.ReasoningKeys 取用。
	Extra map[string]flexText
}

// UnmarshalJSON 解码 delta，并把未建模的顶层字符串字段收进 Extra。
func (d *streamDelta) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	type plain streamDelta // 避免递归
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*d = streamDelta(decoded)
	known := map[string]bool{
		"content": true, "reasoning_content": true, "reasoning": true,
		"tool_calls": true, "function_call": true, "role": true,
	}
	for name, value := range fields {
		if known[name] {
			continue
		}
		var text flexText
		if err := text.UnmarshalJSON(value); err != nil {
			continue
		}
		if d.Extra == nil {
			d.Extra = make(map[string]flexText, 2)
		}
		d.Extra[name] = text
	}
	return nil
}

// isEmpty 报告增量是否有任何内容（用于判断是否该退回 message 字段）。
func (d streamDelta) isEmpty() bool {
	return d.Content.text() == "" && d.ReasoningContent.text() == "" && d.Reasoning.text() == "" &&
		len(d.ToolCalls) == 0 && d.FunctionCall == nil
}

// streamToolCall 是一个不完整的工具调用：Index 标识调用，ID/Name 先到，Arguments
// 跨片段累积。
type streamToolCall struct {
	// Index 用 *int：必须区分"缺失"与"0"。缺失时按同一事件内的出现顺序补齐，
	// 否则一个事件里的多个 tool_calls 会全部落到 index 0（ID/Name 被覆盖、
	// arguments 被拼接成非法 JSON）——本地网关经常会省掉这个字段。
	Index    *int             `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function streamToolCallFn `json:"function"`
}

type streamToolCallFn struct {
	Name      string        `json:"name"`
	Arguments flexArguments `json:"arguments"`
}

// streamFunction 是旧式 function_call（没有 index）。
type streamFunction struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Arguments flexArguments `json:"arguments"`
}

// streamUsage 是用量对象（兼容 OpenAI 的嵌套 cached_tokens 与 DeepSeek 的扁平键）。
type streamUsage struct {
	PromptTokens            int                  `json:"prompt_tokens"`
	CompletionTokens        int                  `json:"completion_tokens"`
	TotalTokens             int                  `json:"total_tokens"`
	PromptTokensDetails     *promptTokensDetails `json:"prompt_tokens_details"`
	CompletionTokensDetails *completionDetails   `json:"completion_tokens_details"`
	PromptCacheHitTokens    int                  `json:"prompt_cache_hit_tokens"`
}

// completionDetails 是 completion_tokens_details（OpenAI 的推理 token 明细）。
// rig.Usage 不承载它，但保留字段让严格解码与托管服务的历史行为一致。
type completionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type promptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// streamError 是带内错误对象。
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

// flexText 接受 JSON 字符串或分片数组（[{"type":"text","text":"..."}]），并把文本
// 拼起来。本地服务（llama.cpp 的部分版本、各家网关）两种都会发。
type flexText struct {
	value string
}

func (t flexText) text() string { return t.value }

func (t *flexText) UnmarshalJSON(raw []byte) error {
	t.value = ""
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	switch trimmed[0] {
	case '"':
		return json.Unmarshal(trimmed, &t.value)
	case '[':
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(trimmed, &parts); err != nil {
			return err
		}
		var builder strings.Builder
		for _, part := range parts {
			if part.Type == "" || part.Type == "text" {
				builder.WriteString(part.Text)
			}
		}
		t.value = builder.String()
		return nil
	default:
		return fmt.Errorf("local: unexpected text shape %q", trimmed)
	}
}

// flexArguments 接受 JSON 字符串或对象：字符串按原样累积（标准增量），对象则
// 重新编码成紧凑 JSON（有的服务一次给完整对象，而不是分片字符串）。
type flexArguments struct {
	value string
}

func (a *flexArguments) UnmarshalJSON(raw []byte) error {
	a.value = ""
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		return json.Unmarshal(trimmed, &a.value)
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, trimmed); err != nil {
		return err
	}
	a.value = buffer.String()
	return nil
}

// 以下类型是"托管服务"（provider/openai）的**严格**事件形状：只接受规范写法。
//
// 与上面的弹性类型相比，严格模式在三处不同，且都是有意的（差异即契约）：
//   - content 与 tool_calls.function.arguments 必须是 JSON 字符串：形状不对时整条
//     事件解码失败并被跳过（与历史实现一致），而不是被"猜"出来；
//   - 没有 choice.message 回退，也不认旧式 function_call；
//   - finish_reason 原样透传（不转小写、不套别名）。
//
// 这样 provider/openai 与 provider/local 共享同一套 SSE 引擎（分帧、终止判定、
// 用量、错误对象、关闭语义），只在**容忍度**上按配置分叉。
type strictEvent struct {
	Choices []strictChoice `json:"choices"`
	Usage   *streamUsage   `json:"usage"`
	Error   *streamError   `json:"error"`
}

type strictChoice struct {
	Delta        strictDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type strictDelta struct {
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	Reasoning        string           `json:"reasoning"`
	ToolCalls        []strictToolCall `json:"tool_calls"`
}

type strictToolCall struct {
	// Index 用 *int：必须区分"缺失"与"0"（见 streamToolCall 的说明）。
	Index    *int             `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function strictToolCallFn `json:"function"`
}

type strictToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// DecodeStrictEvent 解码一条严格模式的 data 载荷（坏 JSON 返回 error，由调用方
// 跳过该事件）。
//
// 它是 provider/openai 与本地 provider 共享的解码入口：openai 的内部 fuzz/回归
// 用例直接喂载荷，生产路径走 NewStream。
func DecodeStrictEvent(payload string) (chunks []rig.Chunk, finishReason string, err error) {
	chunks, finish, hasFinish, err := strictDecoder{}.decode(payload)
	if err != nil {
		return nil, "", err
	}
	if hasFinish {
		return chunks, finish, nil
	}
	return chunks, "", nil
}

// strictDecoder 实现严格模式的事件解码（provider/openai）。
type strictDecoder struct{}

func (strictDecoder) decode(payload string) ([]rig.Chunk, string, bool, error) {
	var event strictEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, "", false, nil // 坏 JSON / 形状不符：跳过该事件（与历史行为一致）
	}
	if event.Error != nil {
		return nil, "", false, fmt.Errorf("%s", event.Error.text())
	}
	var chunks []rig.Chunk
	finishReason := ""
	hasFinish := false
	for _, choice := range event.Choices {
		delta := choice.Delta
		chunk := rig.Chunk{
			Delta:     delta.Content,
			Reasoning: firstNonEmptyText(delta.ReasoningContent, delta.Reasoning),
		}
		for _, call := range delta.ToolCalls {
			// 缺失 index 时交给流级别归并（按 id/name），缺失与 0 必须区分。
			index := toolCallIndexUnknown
			if call.Index != nil {
				index = *call.Index
			}
			chunk.ToolCalls = append(chunk.ToolCalls, rig.ToolCallDelta{
				Index:     index,
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
		if chunk.Delta != "" || chunk.Reasoning != "" || len(chunk.ToolCalls) > 0 {
			chunks = append(chunks, chunk)
		}
		// 严格模式：finish_reason 原样透传（不 trim、不转小写、不套别名）。
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			finishReason = *choice.FinishReason
			hasFinish = true
		}
	}
	if event.Usage != nil {
		chunks = append(chunks, rig.Chunk{Usage: usageFromWire(event.Usage)})
	}
	return chunks, finishReason, hasFinish, nil
}

// firstNonEmptyText 返回第一个非空值（reasoning_content 优先，兼容网关的 reasoning）。
func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// unmarshalEvent 解码一个 SSE 载荷。它在坏 JSON 上返回 error，由调用方跳过该事件。
func unmarshalEvent(payload string, event *streamEvent) error {
	return json.Unmarshal([]byte(payload), event)
}
