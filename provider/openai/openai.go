// Package openai 为任何兼容 OpenAI chat-completions 协议的端点实现 Rig 的模型
// Provider：OpenAI、DeepSeek、vLLM、llama.cpp server、Ollama 的 OpenAI 兼容层、
// OpenRouter 等。
//
// Provider 使用流式 chat-completions 协议：向 {BaseURL}/chat/completions 发送
// 带 "stream": true 的 POST 请求，然后把 SSE 响应解码为 rig.Chunk（正文、推理
// 文本、工具调用片段、用量、结束原因）。它只依赖 Go 标准库，并且并发安全：
// Provider 不保存任何按流的状态。
//
//	p, err := openai.New(openai.Config{
//		BaseURL:      "https://api.deepseek.com/v1",
//		APIKeyEnv:    "DEEPSEEK_API_KEY",
//		DefaultModel: "deepseek-chat",
//	})
//	stream, err := p.Stream(ctx, rig.Request{...})
//
// 传输失败、非 2xx 状态码与请求编码错误会在 Stream 调用中同步返回。HTTP 429/5xx
// 响应会以有界的指数退避重试，并尊重 Retry-After。Stream 返回之后，Stream 归
// 调用方所有；最后一个分片之后 Recv 返回 io.EOF，并关闭响应体。
//
// # 与 provider/local 的关系（同一份协议实现）
//
// 请求体构造、重试/退避、SSE 分帧与终止判定、用量解析全部由 provider/local 提供
// （`local.BuildChatRequest`、`local.Transport`、`local.NewStream`）。本包只保留
// **托管服务特有**的部分：官方默认地址与 API key 环境变量、错误前缀与文案、
// stream_options 的 IncludeUsage 语义、DefaultReasoningEffort。
//
// 两者的解析差异用 local.Quirks 表达，而不是复制一份代码（见 streamQuirks）：
//
//	RequireDone          ← Config.RequireCompleteStream
//	StrictFinishReasons  finish_reason 原样透传（本地 provider 会归一化变体）
//	StrictDeltas         content/arguments 必须是字符串、不认 message 回退与旧式
//	                     function_call（本地 provider 全部容忍）
//
// 这些差异有跨 provider 契约用例钉住（audit_cross_provider_test.go）。
//
// # 与 agent 循环的关系
//
// agent 包通过 rig.Provider / rig.Stream 消费本 provider：把它放进
// rig.AgentSpec.Provider（或 kit.Config.Provider）之后，agent 会为每个模型轮次
// 调用 Provider.Stream 并读尽返回的流。agent 路径上模型名始终非空
// （RunRequest.Model 或 AgentSpec.Model），因此 Config.DefaultModel 只在直接调用
// Stream 且 Request.Model 为空时生效。Provider 并发安全，一个实例可以被多个
// agent 共享。
package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/local"
)

const (
	// DefaultBaseURL 在 Config.BaseURL 为空时使用。
	DefaultBaseURL = "https://api.openai.com/v1"
	// DefaultAPIKeyEnv 在 Config.APIKey 与 Config.APIKeyEnv 都为空时读取的环境
	// 变量。
	DefaultAPIKeyEnv = "OPENAI_API_KEY"
	// DefaultMaxRetries 是 Config.MaxRetries 为零值时使用的重试预算。负值表示
	// 禁用重试。
	//
	// 与 provider/local 共用同一个常量与同一份重试实现（local.Transport）。
	DefaultMaxRetries = local.DefaultMaxRetries
	// DefaultRetryBackoff 是 Config.RetryBackoff 为零值时使用的首次重试基础延迟。
	// 退避每次尝试翻倍；负值表示立即重试。
	DefaultRetryBackoff = local.DefaultRetryBackoff

	// userAgent 向端点标识本 SDK。
	userAgent = "zeokeel-rig"
	// errorBodyMax 限制失败响应体在返回错误中被引用的字节数。
	errorBodyMax = 1024
	// maxRetryDelay 限制单次退避等待（含 Retry-After）。长时间等待应由调用方决定；
	// provider 的重试永远不会睡上好几分钟。值与 provider/local 的同名常量一致。
	maxRetryDelay = local.MaxRetryDelay
	// defaultHeaderTimeout 限制等待响应头的时间。流式响应体本身由调用方的 context
	// 约束（长时间生成是合法的），因此没有设置整体的 http.Client 超时。
	defaultHeaderTimeout = 60 * time.Second
)

// ErrIncompleteStream 表示事件流在收到结束原因或 [DONE] 终止标记之前就关闭了：
// 回答可能被截断。只有 Config.RequireCompleteStream 为 true 时才会返回；默认
// 关闭，以兼容直接关闭连接、不发送终止事件的 OpenAI 兼容服务器。
var ErrIncompleteStream = fmt.Errorf("openai: stream ended before a completion terminator or finish reason: %w", io.ErrUnexpectedEOF)

// Config 配置 Provider。零值可用：BaseURL 默认为 DefaultBaseURL，API key 从
// OPENAI_API_KEY 读取，最多重试两次、基础退避 500ms，并请求流式用量统计。
type Config struct {
	// BaseURL 是 API 根地址，不含 /chat/completions 后缀，例如
	// "https://api.openai.com/v1"。结尾斜杠会被去掉。
	BaseURL string
	// APIKey 作为 bearer token 发送。为空时从环境变量 APIKeyEnv（或
	// OPENAI_API_KEY）读取。支持不需要 key 的本地服务器：解析结果为空时直接省略
	// 该请求头。
	APIKey string
	// APIKeyEnv 指定保存 API key 的环境变量名。为空表示 OPENAI_API_KEY。
	APIKeyEnv string
	// DefaultModel 在 Request 未指定模型时使用。
	DefaultModel string
	// Headers 是额外的请求头（例如网关的归因头）。它们在 SDK 自身的请求头之后
	// 应用，因此可以覆盖它们。
	Headers map[string]string
	// HTTPClient 覆盖 HTTP 客户端。nil 表示使用一个把响应头等待限制在 60s 的
	// client；流式响应体由请求 context 约束。
	HTTPClient *http.Client
	// MaxRetries 是可重试失败（429、408、425、5xx、传输错误）之后的额外尝试次数。
	// 零表示 DefaultMaxRetries；负值表示禁用重试。
	MaxRetries int
	// RetryBackoff 是第一次重试前的基础延迟，每次尝试翻倍。零表示
	// DefaultRetryBackoff；负值表示不等待直接重试。
	RetryBackoff time.Duration
	// IncludeUsage 控制是否在请求里发送 `stream_options.include_usage`。
	//
	//	nil      → 发送（默认）。用量对成本/上下文统计很重要，本地模型也通常支持；
	//	&false   → **不发送**：给不认识该字段的严格网关（它们会 400）。
	//	&true    → 发送（显式声明，与 nil 等价）。
	//
	// 用 *bool 而不是 bool：零值 bool 无法区分"没设"与"显式关掉"，而这个区别正是
	// 之前"假配置项"（设 false 也照样发）的根因。
	IncludeUsage *bool
	// RequireCompleteStream 要求每个流都以 finish_reason 或 [DONE] 终止；若两者
	// 都没出现连接就关闭了，Recv 返回 ErrIncompleteStream（回答可能被截断），
	// 而不是把提前断开当成正常结束。默认 false：兼容直接关闭连接、不发送终止
	// 事件的服务器。
	RequireCompleteStream bool
	// DefaultReasoningEffort 是每个请求默认携带的 reasoning_effort。
	// 只接受 "low"、"medium"、"high" 或空串（空 = 不发送）；大小写/首尾空白会被
	// 归一化，其他值在 New 中返回错误。显式 rig.Request.Extra["reasoning_effort"]
	// 总是覆盖该默认值。
	DefaultReasoningEffort string
}

// Provider 是一个 OpenAI 兼容的 rig.Provider。它不保存按流的状态，并发安全。
type Provider struct {
	baseURL      string
	apiKey       string
	defaultModel string
	headers      map[string]string
	maxRetries   int
	backoff      time.Duration

	// transport 是共享的重试传输层（provider/local），负责 429/408/425/5xx 与
	// 传输错误的重试、Retry-After、指数退避与"请求被取消"的文案。openai 只提供
	// 错误前缀（openai:）与请求头。
	transport *local.Transport

	// defaultReasoningEffort 是已归一化的 low/medium/high；空表示不发送。
	defaultReasoningEffort string

	// requireComplete 是 Config.RequireCompleteStream 的归一化副本：为 true 时
	// 流必须带 finish_reason 或 [DONE] 才算正常结束。
	requireComplete bool

	// includeUsage 是 Config.IncludeUsage 的副本：nil（默认）与 true 都发送
	// stream_options.include_usage，只有显式 false 才省略。
	includeUsage *bool
}

var _ rig.Provider = (*Provider)(nil)

// New 校验 cfg、应用默认值并返回可用的 Provider。
func New(cfg Config) (*Provider, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("openai: invalid base URL %q: %w", cfg.BaseURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("openai: invalid base URL %q: scheme must be http or https", cfg.BaseURL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("openai: invalid base URL %q: missing host", cfg.BaseURL)
	}

	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		env := strings.TrimSpace(cfg.APIKeyEnv)
		if env == "" {
			env = DefaultAPIKeyEnv
		}
		apiKey = strings.TrimSpace(os.Getenv(env))
	}

	headers := make(map[string]string, len(cfg.Headers))
	for name, value := range cfg.Headers {
		headers[name] = value
	}

	client := cfg.HTTPClient
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if transport.ResponseHeaderTimeout == 0 {
			transport.ResponseHeaderTimeout = defaultHeaderTimeout
		}
		client = &http.Client{Transport: transport}
	}

	maxRetries := cfg.MaxRetries
	switch {
	case maxRetries == 0:
		maxRetries = DefaultMaxRetries
	case maxRetries < 0:
		maxRetries = 0
	}
	backoff := cfg.RetryBackoff
	switch {
	case backoff == 0:
		backoff = DefaultRetryBackoff
	case backoff < 0:
		backoff = 0
	}

	reasoningEffort, err := normalizeReasoningEffort(cfg.DefaultReasoningEffort)
	if err != nil {
		return nil, err
	}

	return &Provider{
		baseURL:                base,
		apiKey:                 apiKey,
		defaultModel:           strings.TrimSpace(cfg.DefaultModel),
		headers:                headers,
		maxRetries:             maxRetries,
		backoff:                backoff,
		defaultReasoningEffort: reasoningEffort,
		requireComplete:        cfg.RequireCompleteStream,
		includeUsage:           cfg.IncludeUsage,
		transport: &local.Transport{
			Name:               "openai",
			Client:             client,
			MaxRetries:         maxRetries,
			Backoff:            backoff,
			RequestErrorPrefix: "chat completions request",
		},
	}, nil
}

// normalizeReasoningEffort 校验并归一化 Config.DefaultReasoningEffort：只接受
// low/medium/high 或空串；大小写与首尾空白会被归一化。
func normalizeReasoningEffort(value string) (string, error) {
	effort := strings.ToLower(strings.TrimSpace(value))
	switch effort {
	case "", "low", "medium", "high":
		return effort, nil
	}
	return "", fmt.Errorf("openai: invalid DefaultReasoningEffort %q (want low, medium, high or empty)", value)
}

// Name 实现 rig.Provider。
func (p *Provider) Name() string { return "openai" }

// Stream 发起一次流式补全。它会在端点接受请求并响应事件流之后返回；随后解析在
// Recv 中惰性进行。
//
// 可重试的请求失败（HTTP 429/408/425/5xx 与传输错误）最多按 MaxRetries 次数做
// 指数退避重试，并尊重 Retry-After 头；重试实现来自 provider/local（全仓只有一份）。
// 流建立之后的失败不会重试：已经部分发出的回答无法收回。
func (p *Provider) Stream(ctx context.Context, req rig.Request) (rig.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := p.transport.Do(ctx, http.MethodPost, p.baseURL+"/chat/completions", payload, p.requestHeaders())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyMax))
		_ = resp.Body.Close()
		return nil, statusError(resp, snippet)
	}
	if err := local.CheckStreamingResponse("openai", resp); err != nil {
		return nil, err
	}
	return local.NewStream(ctx, local.StreamOptions{
		Name:             "openai",
		Body:             resp.Body,
		Quirks:           p.streamQuirks(),
		IncompleteStream: ErrIncompleteStream,
	}), nil
}

// streamQuirks 返回 openai 在共享 SSE 解码器上使用的**严格**档案：
//
//   - RequireDone 跟随 Config.RequireCompleteStream：开启时"既无 finish_reason
//     也无 [DONE]"的提前 EOF 会返回 ErrIncompleteStream；
//   - StrictFinishReasons：finish_reason 原样透传（不转小写、不套别名）；
//   - StrictDeltas：content / tool_calls.arguments 必须是 JSON 字符串，不使用
//     choice.message 回退，也不认旧式 function_call（形状不对就跳过该事件）。
//
// 这些差异是**契约**而不是巧合：provider/local 面对本地服务要宽容，托管服务要严格。
func (p *Provider) streamQuirks() local.Quirks {
	return local.Quirks{
		RequireDone:         p.requireComplete,
		StrictFinishReasons: true,
		StrictDeltas:        true,
	}
}

// requestHeaders 构造本次请求的头部：SDK 默认头在前，用户 Headers 覆盖它们。
func (p *Provider) requestHeaders() http.Header {
	headers := make(http.Header, len(p.headers)+4)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("User-Agent", userAgent)
	if p.apiKey != "" {
		headers.Set("Authorization", "Bearer "+p.apiKey)
	}
	for name, value := range p.headers {
		headers.Set(name, value)
	}
	return headers
}

// statusError 渲染非 2xx 响应，并引用一小段响应体（许多兼容服务器会把真正原因
// 放在这里）。
func statusError(resp *http.Response, snippet []byte) error {
	body := strings.TrimSpace(string(snippet))
	if body == "" {
		return fmt.Errorf("openai: chat completions failed: %s", resp.Status)
	}
	return fmt.Errorf("openai: chat completions failed: %s: %s", resp.Status, body)
}

// buildRequest 把 rig.Request 映射为 OpenAI 兼容的 JSON 请求体。
//
// 请求体构造与 provider/local 共用 local.BuildChatRequest：两者的差别只有"发不发
// tools / stream_options"与默认 reasoning_effort，都是字段而不是两份实现。
func (p *Provider) buildRequest(req rig.Request) ([]byte, error) {
	includeUsage := p.includeUsage == nil || *p.includeUsage
	return local.BuildChatRequest(local.ChatRequestOptions{
		Name:                   "openai",
		Model:                  req.Model,
		DefaultModel:           p.defaultModel,
		Messages:               req.Messages,
		Tools:                  req.Tools,
		Temperature:            req.Temperature,
		TopP:                   req.TopP,
		MaxTokens:              req.MaxTokens,
		IncludeTools:           true,
		IncludeUsage:           includeUsage,
		Extra:                  req.Extra,
		DefaultReasoningEffort: p.defaultReasoningEffort,
	})
}
