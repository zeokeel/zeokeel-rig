package rig

import "context"

// Request 是一次模型补全请求。当前契约代中 Stream 恒为 true：流式接口是唯一的
// 模型传输方式，从而保证任何地方都能观察到部分输出。
type Request struct {
	Model       string
	Messages    []Message
	Tools       []ToolSpec
	Temperature *float64
	TopP        *float64
	MaxTokens   int
	// Extra 携带 provider 专属选项（例如 reasoning effort），避免为此拓宽接口；
	// provider 会忽略自己不认识的键。
	Extra map[string]any
}

// ToolCallDelta 是流式响应中一个工具调用的片段。Index 标识响应内的第几个调用；
// ID 与 Name 先到，Arguments 逐片段累积。
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Chunk 是补全流中的一个分片。
type Chunk struct {
	Delta        string
	Reasoning    string
	ToolCalls    []ToolCallDelta
	Usage        *Usage
	FinishReason string // "stop"、"tool_calls"、"length"；流式过程中为空
}

// Stream 是一次模型响应流。最后一个分片之后 Recv 返回 io.EOF。实现必须保证同一
// 时刻只有一个 goroutine 调用 Recv，并在 Recv 返回错误或 io.EOF 时释放资源。
type Stream interface {
	Recv() (Chunk, error)
}

// Provider 是模型后端。第三方实现它即可接入新的厂商。
type Provider interface {
	// Name 是稳定的标识（"openai"、"my-llm" 等）。
	Name() string
	// Stream 发起一次补全。返回的 Stream 归调用方所有。
	Stream(ctx context.Context, req Request) (Stream, error)
}
