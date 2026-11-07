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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// DefaultBaseURL 在 Config.BaseURL 为空时使用。
	DefaultBaseURL = "https://api.openai.com/v1"
	// DefaultAPIKeyEnv 在 Config.APIKey 与 Config.APIKeyEnv 都为空时读取的环境
	// 变量。
	DefaultAPIKeyEnv = "OPENAI_API_KEY"
	// DefaultMaxRetries 是 Config.MaxRetries 为零值时使用的重试预算。负值表示
	// 禁用重试。
	DefaultMaxRetries = 2
	// DefaultRetryBackoff 是 Config.RetryBackoff 为零值时使用的首次重试基础延迟。
	// 退避每次尝试翻倍；负值表示立即重试。
	DefaultRetryBackoff = 500 * time.Millisecond

	// userAgent 向端点标识本 SDK。
	userAgent = "zeokeel-rig"
	// errorBodyMax 限制失败响应体在返回错误中被引用的字节数。
	errorBodyMax = 1024
	// maxRetryDelay 限制单次退避等待（含 Retry-After）。长时间等待应由调用方决定；
	// provider 的重试永远不会睡上好几分钟。
	maxRetryDelay = 30 * time.Second
	// defaultHeaderTimeout 限制等待响应头的时间。流式响应体本身由调用方的 context
	// 约束（长时间生成是合法的），因此没有设置整体的 http.Client 超时。
	defaultHeaderTimeout = 60 * time.Second
)

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
	// IncludeUsage 为配置兼容性保留：Rig 总是向端点请求流式用量统计
	// （stream_options.include_usage），因此零值即 SDK 默认行为。
	IncludeUsage bool
}

// Provider 是一个 OpenAI 兼容的 rig.Provider。它不保存按流的状态，并发安全。
type Provider struct {
	baseURL      string
	apiKey       string
	defaultModel string
	headers      map[string]string
	client       *http.Client
	maxRetries   int
	backoff      time.Duration
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

	return &Provider{
		baseURL:      base,
		apiKey:       apiKey,
		defaultModel: strings.TrimSpace(cfg.DefaultModel),
		headers:      headers,
		client:       client,
		maxRetries:   maxRetries,
		backoff:      backoff,
	}, nil
}

// Name 实现 rig.Provider。
func (p *Provider) Name() string { return "openai" }

// Stream 发起一次流式补全。它会在端点接受请求并响应事件流之后返回；随后解析在
// Recv 中惰性进行。
//
// 可重试的请求失败（HTTP 429/408/425/5xx 与传输错误）最多按 MaxRetries 次数做
// 指数退避重试，并尊重 Retry-After 头。流建立之后的失败不会重试：已经部分发出的
// 回答无法收回。
func (p *Provider) Stream(ctx context.Context, req rig.Request) (rig.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}

	attempts := p.maxRetries + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		resp, err := p.post(ctx, payload)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, canceledError(ctxErr)
			}
			lastErr = err
			if attempt+1 >= attempts || !retryableTransportError(err) {
				return nil, err
			}
			if waitErr := sleepCtx(ctx, p.retryDelay(attempt, "")); waitErr != nil {
				return nil, canceledError(waitErr)
			}
			continue
		}

		if resp.StatusCode >= http.StatusOK && resp.StatusCode <= 299 {
			if err := checkStreamingResponse(resp); err != nil {
				return nil, err
			}
			return newSSEStream(ctx, resp), nil
		}

		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyMax))
		_ = resp.Body.Close()
		statusErr := statusError(resp, snippet)
		lastErr = statusErr
		if attempt+1 >= attempts || !retryableStatus(resp.StatusCode) {
			return nil, statusErr
		}
		delay := p.retryDelay(attempt, resp.Header.Get("Retry-After"))
		if waitErr := sleepCtx(ctx, delay); waitErr != nil {
			return nil, canceledError(waitErr)
		}
	}
	return nil, lastErr
}

// post 为 payload 执行一次 HTTP 请求。
func (p *Provider) post(ctx context.Context, payload []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", userAgent)
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for name, value := range p.headers {
		httpReq.Header.Set(name, value)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: chat completions request: %w", err)
	}
	return resp, nil
}

// checkStreamingResponse 拒绝不是 SSE 流的 2xx 响应：本 provider 不实现非流式
// 回退。
func checkStreamingResponse(resp *http.Response) error {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" || strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyMax))
	_ = resp.Body.Close()
	body := strings.TrimSpace(string(snippet))
	if body == "" {
		return fmt.Errorf("openai: expected a text/event-stream response, got Content-Type %q; non-streaming responses are not supported", contentType)
	}
	return fmt.Errorf("openai: expected a text/event-stream response, got Content-Type %q; non-streaming responses are not supported: %s", contentType, body)
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

// canceledError 以 errors.Is 可匹配的方式报告 ctx 取消。
func canceledError(err error) error {
	return fmt.Errorf("openai: request canceled: %w", err)
}

// retryableStatus 报告值得重试的状态码：限流、请求超时/过早以及服务端故障。
func retryableStatus(code int) bool {
	switch {
	case code == http.StatusRequestTimeout, code == http.StatusTooEarly, code == http.StatusTooManyRequests:
		return true
	case code >= 500 && code <= 599:
		return true
	default:
		return false
	}
}

// retryableTransportError 报告请求是否在收到任何响应之前就失败（因此重放是安全
// 的）。context 错误由调用方处理，这里永不重试。
func retryableTransportError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	_, ok := errors.AsType[*url.Error](err)
	return ok
}

// retryDelay 返回第 attempt 次（从 0 开始）重试前的延迟。合法的 Retry-After 头
// （delta 秒或 HTTP 日期）优先于指数退避；两者都受 maxRetryDelay 上限约束。
func (p *Provider) retryDelay(attempt int, retryAfter string) time.Duration {
	if delay, ok := parseRetryAfter(retryAfter); ok {
		return delay
	}
	if p.backoff <= 0 {
		return 0
	}
	delay := p.backoff
	for i := 0; i < attempt; i++ {
		if delay >= maxRetryDelay {
			return maxRetryDelay
		}
		delay *= 2
	}
	delay = min(delay, maxRetryDelay)
	return delay
}

// parseRetryAfter 解析 Retry-After 头（delta 秒或 HTTP 日期）。
func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		delay := time.Duration(seconds) * time.Second
		delay = min(delay, maxRetryDelay)
		return delay, true
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := time.Until(when)
		if delay < 0 {
			delay = 0
		}
		delay = min(delay, maxRetryDelay)
		return delay, true
	}
	return 0, false
}

// sleepCtx 等待 delay 或 ctx 取消，先到者为准。
func sleepCtx(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// buildRequest 把 rig.Request 映射为 OpenAI 兼容的 JSON 请求体。
func (p *Provider) buildRequest(req rig.Request) ([]byte, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = p.defaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("openai: no model configured: set Config.DefaultModel or Request.Model: %w", rig.ErrModelRequired)
	}

	messages := make([]chatMessage, 0, len(req.Messages))
	for _, message := range req.Messages {
		wire := chatMessage{
			Role:       string(message.Role),
			Content:    wireContent(message),
			ToolCallID: message.ToolCallID,
			Name:       message.Name,
		}
		for _, call := range message.ToolCalls {
			arguments := strings.TrimSpace(call.Arguments)
			if arguments == "" {
				arguments = "{}" // 规范的“无参数”对象
			}
			wire.ToolCalls = append(wire.ToolCalls, chatToolCall{
				ID:   call.ID,
				Type: "function",
				Function: chatFunctionCall{
					Name:      call.Name,
					Arguments: arguments,
				},
			})
		}
		messages = append(messages, wire)
	}

	tools := make([]chatTool, 0, len(req.Tools))
	for _, spec := range req.Tools {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			return nil, fmt.Errorf("openai: tool with empty name")
		}
		tools = append(tools, chatTool{
			Type: "function",
			Function: chatFunction{
				Name:        name,
				Description: spec.Description,
				Parameters:  spec.Parameters,
			},
		})
	}

	body := chatRequest{
		Model:         model,
		Messages:      messages,
		Tools:         tools,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Temperature:   cloneFloat(req.Temperature),
		TopP:          cloneFloat(req.TopP),
		MaxTokens:     req.MaxTokens,
	}
	raw, err := encodeJSON(body)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	if len(req.Extra) == 0 {
		return raw, nil
	}
	return mergeExtra(raw, req.Extra)
}

// reservedRequestFields 由 provider 拥有；Extra 不能覆盖它们。
var reservedRequestFields = map[string]bool{
	"model": true, "messages": true, "tools": true, "stream": true,
	"stream_options": true, "temperature": true, "top_p": true, "max_tokens": true,
}

// mergeExtra 把 provider 专属的顶层字段（例如 reasoning_effort）合并进已编码的
// 请求，同时不允许它们覆盖 provider 自己拥有的字段。
func mergeExtra(raw []byte, extra map[string]any) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	for name, value := range extra {
		if reservedRequestFields[name] {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("openai: encode extra field %q: %w", name, err)
		}
		fields[name] = encoded
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	return merged, nil
}

// wireContent 映射一条消息的内容。纯文本消息以及非 user/assistant 角色保持普通
// 字符串形式；带至少一个可用图片的 user/assistant 消息转换为 OpenAI 多模态内容
// 数组，并保持原始分片顺序。
func wireContent(message rig.Message) any {
	if len(message.Content) == 0 {
		return ""
	}
	if message.Role != rig.RoleUser && message.Role != rig.RoleAssistant {
		return message.Text()
	}
	hasImage := false
	for _, part := range message.Content {
		if part.Type == rig.PartImageURL && part.ImageURL != "" {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return message.Text()
	}
	parts := make([]chatContentPart, 0, len(message.Content))
	for _, part := range message.Content {
		switch part.Type {
		case rig.PartText:
			text := part.Text
			parts = append(parts, chatContentPart{Type: "text", Text: &text})
		case rig.PartImageURL:
			if part.ImageURL == "" {
				continue
			}
			detail := part.Detail
			if detail == "" {
				detail = "auto"
			}
			parts = append(parts, chatContentPart{
				Type:     "image_url",
				ImageURL: &chatImageURL{URL: part.ImageURL, Detail: detail},
			})
		}
	}
	if len(parts) == 0 {
		return message.Text()
	}
	return parts
}

// cloneFloat 复制指针，避免调用方后续修改影响已构造的请求。
func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
