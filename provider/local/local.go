package local

// 本文件是 provider/local 的装配面：配置归一化、请求构造与 Stream 的错误分类。
//
// 与 provider/openai 的关系：两者说的是同一条协议（流式 chat-completions），但目标
// 不同——openai 面向托管服务（默认 https + API key + 严格终止标记），local 面向
// 本机/自建服务（默认 http、无鉴权、容忍各家实现差异）。差异集中在 Quirks 里，由
// ollama / lmstudio 两个薄封装按需打开。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// DefaultName 是 rig.Provider.Name() 的默认返回值。
	DefaultName = "local"
	// DefaultBaseURL 是未配置 BaseURL 时使用的地址：llama.cpp server / vLLM 等
	// 本地服务的常见默认端口（8080）加 /v1。
	DefaultBaseURL = "http://127.0.0.1:8080/v1"
	// DefaultChatPath / DefaultModelsPath 是 OpenAI 兼容端点上的路径。
	DefaultChatPath   = "/chat/completions"
	DefaultModelsPath = "/models"
	// DefaultAPIKeyEnv 是 APIKey 与 APIKeyEnv 都为空时读取的环境变量名。
	//
	// 刻意不是 OPENAI_API_KEY：本地服务不需要 key，而"顺手把用户真实的 OpenAI
	// key 发到 127.0.0.1"是一种凭据泄漏——默认只认本地专用的环境变量。
	DefaultAPIKeyEnv = "LOCAL_API_KEY"
	// DefaultMaxRetries 是零值 MaxRetries 对应的重试预算；负值禁用重试。
	DefaultMaxRetries = 2
	// DefaultRetryBackoff 是零值 RetryBackoff 对应的首次重试延迟（每次翻倍）。
	DefaultRetryBackoff = 500 * time.Millisecond

	// userAgent 标识本 SDK。
	userAgent = "zeokeel-rig"
	// errorBodyMax 限制错误响应体被引用的字节数。
	errorBodyMax = 1024

	// DefaultHeaderTimeout 是等待响应头时间的默认值。流式响应体本身由调用方的
	// context 约束（长时间生成是合法的），因此这里只限制"等到第一响应字节"。
	DefaultHeaderTimeout = 60 * time.Second
)

// ErrIncompleteStream 表示流在收到结束原因或 [DONE] 之前就关闭了（回答可能被截断）。
// 只有 Quirks.RequireDone 为 true 时才返回：本地服务常常直接关连接。
var ErrIncompleteStream = fmt.Errorf("local: stream ended before a completion terminator or finish reason: %w", io.ErrUnexpectedEOF)

// Quirks 是"OpenAI 兼容但细节不同"的开关集合。零值即本包推荐的本地默认值
// （宽松：容忍缺失的终止标记、允许服务端省略 index、自动兼容各家写法）。
type Quirks struct {
	// RequireDone 要求流必须以 finish_reason 或 [DONE] 终止；两者都没出现就关连接
	// 时 Recv 返回 ErrIncompleteStream，而不是把提前断开当成正常结束。
	RequireDone bool
	// DisableTools 为 true 时不发送 tools：给不支持 function calling 的本地模型用，
	// 否则部分服务会直接 400。
	DisableTools bool
	// DisableStreamOptions 为 true 时不发送 stream_options.include_usage：给不认识
	// 该字段的严格服务端用（代价是拿不到 token 用量）。
	DisableStreamOptions bool
	// FinishReasonAliases 把服务端特有的 finish_reason 归一化成 rig 约定的
	// stop / length / tool_calls。内置别名见 defaultFinishReasonAliases。
	FinishReasonAliases map[string]string
	// ReasoningKeys 是额外的"推理文本"字段名（在 reasoning_content / reasoning
	// 之外）。例如某些网关用 thinking 或 analysis。
	ReasoningKeys []string
	// StrictFinishReasons 为 true 时不归一化 finish_reason：不转小写、不套别名，
	// 原样透传。托管服务（provider/openai）用它保持历史语义。
	StrictFinishReasons bool
	// StrictDeltas 为 true 时只接受规范写法：content 与 tool_calls 的 arguments
	// 必须是 JSON 字符串，不使用 choice.message 回退，也不认旧式 function_call。
	// 托管服务（provider/openai）用它保持"形状不对就跳过该事件"的历史语义。
	StrictDeltas bool
}

// Config 配置一个本地 OpenAI 兼容 provider。零值可用（BaseURL 默认 8080/v1、
// 无鉴权、本地宽松解析）。
type Config struct {
	// Name 是 rig.Provider.Name() 的取值，默认 "local"。
	Name string
	// BaseURL 是 API 根（不含 /chat/completions），例如
	// "http://127.0.0.1:11434/v1"。结尾斜杠会被去掉。
	BaseURL string
	// ChatPath / ModelsPath 覆盖补全与模型列表路径（默认 /chat/completions、
	// /models）。它们相对 BaseURL 拼接。
	ChatPath   string
	ModelsPath string
	// APIKey 作为 bearer token 发送；为空时读 APIKeyEnv（默认 LOCAL_API_KEY）。
	APIKey string
	// APIKeyEnv 指定保存 key 的环境变量名。
	APIKeyEnv string
	// DisableAuth 强制不发送 Authorization 头（即使配置了 key）。本地服务通常
	// 无鉴权，且不应该把 key 发给本地端口。
	DisableAuth bool
	// DefaultModel 在 Request 未指定模型时使用。
	DefaultModel string
	// Headers 是额外请求头；在 SDK 自身请求头之后应用，因此可以覆盖。
	Headers map[string]string
	// HTTPClient 覆盖 HTTP 客户端；nil 表示使用本包构造的 client（响应头超时见
	// HeaderTimeout）。
	HTTPClient *http.Client
	// HeaderTimeout 是等待响应头的时间上限（只影响本包构造的默认 client）。
	// 零表示 DefaultHeaderTimeout。本地模型加载可能很慢，服务商封装可以把默认值
	// 调大（见 ollama.DefaultHeaderTimeout）。
	HeaderTimeout time.Duration
	// MaxRetries 是可重试失败（429/408/425/5xx 与传输错误）之后的额外尝试次数。
	// 零表示 DefaultMaxRetries，负值禁用重试。
	MaxRetries int
	// RetryBackoff 是首次重试前的基础延迟，每次翻倍；零表示
	// DefaultRetryBackoff，负值表示立即重试。
	RetryBackoff time.Duration
	// Quirks 是服务端差异开关。
	Quirks Quirks
	// TransportHint 会附加到"连不上服务"的错误上，给用户一句可执行的提示
	// （例如 "ollama 未启动？试试 `ollama serve`"）。
	TransportHint string
}

// options 是归一化之后的配置。
type options struct {
	name          string
	baseURL       string
	chatPath      string
	modelsPath    string
	apiKey        string
	disableAuth   bool
	defaultModel  string
	headers       map[string]string
	maxRetries    int
	backoff       time.Duration
	quirks        quirks
	transportHint string
	headerTimeout time.Duration
}

// quirks 是归一化之后的差异开关。
type quirks struct {
	requireDone         bool
	disableTools        bool
	disableStreamOption bool
	finishReasonAliases map[string]string
	reasoningKeys       []string
	strictFinishReasons bool
	strictDeltas        bool
}

// Provider 是一个本地 OpenAI 兼容 provider。它不保存按流的状态，并发安全。
type Provider struct {
	cfg       options
	transport *Transport
}

var _ rig.Provider = (*Provider)(nil)

// defaultFinishReasonAliases 把常见变体归一化成 rig 的三个约定值。
//
// 注意方向是"服务端 → rig"，不是反过来：agent 只关心"是否有工具调用 / 是否被
// 长度截断 / 正常结束"。
var defaultFinishReasonAliases = map[string]string{
	"tool_call":         "tool_calls",
	"function_call":     "tool_calls",
	"max_tokens":        "length",
	"max_output_tokens": "length",
	"stop_sequence":     "stop",
	"eos":               "stop",
	"end_turn":          "stop",
}

// resolveHTTPClient 返回一个**一定带"等待响应头"超时**的 HTTP client。
//
// 这里的坑很具体：调用方注入自己的 client 往往是为了代理/TLS/指标，但 `http.Client`
// 本身没有"等待响应头"的字段，超时在 `Transport.ResponseHeaderTimeout` 上。如果
// 我们只在"自建 client"时设置它，注入 client 的宿主就会**静默**失去这道兜底——
// 服务端接了连接却不回响应头时（本地模型加载卡住、反代挂住），用户看到的是无限
// 转圈，而不是 5 分钟后一条超时错误。
//
// 因此按来源分别处理：
//
//   - 没有 client / Transport 为 nil（等价于 DefaultTransport）→ 克隆默认 transport 并补超时；
//   - Transport 是 *http.Transport 且已设了 ResponseHeaderTimeout → 尊重调用方的选择；
//   - Transport 是 *http.Transport 但没设 → 克隆一份补上（保留 CheckRedirect/Jar/Timeout）；
//   - 其它（自定义 RoundTripper）→ **无法**替它设超时，用 slog.Warn 明确告知，绝不静默。
func resolveHTTPClient(client *http.Client, headerTimeout time.Duration) *http.Client {
	withTimeout := func(base *http.Transport) *http.Transport {
		cloned := base.Clone()
		cloned.ResponseHeaderTimeout = headerTimeout
		return cloned
	}
	if client == nil {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			warnCustomTransport("http.DefaultTransport", headerTimeout)
			return &http.Client{}
		}
		return &http.Client{Transport: withTimeout(base)}
	}
	cloned := *client // 保留 CheckRedirect / Jar / Timeout / 其它字段
	switch transport := client.Transport.(type) {
	case nil:
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			warnCustomTransport("http.DefaultTransport", headerTimeout)
			return client
		}
		cloned.Transport = withTimeout(base)
		return &cloned
	case *http.Transport:
		if transport.ResponseHeaderTimeout > 0 {
			return client // 调用方已经自己设了，尊重
		}
		cloned.Transport = withTimeout(transport)
		return &cloned
	default:
		warnCustomTransport(fmt.Sprintf("%T", client.Transport), headerTimeout)
		return client
	}
}

// warnCustomTransport 明确告知"你的 Transport 不归我们管"，而不是静默失去超时。
func warnCustomTransport(kind string, headerTimeout time.Duration) {
	slog.Warn("provider/local: Config.HeaderTimeout cannot be applied to a custom http.RoundTripper; "+
		"the header wait is unbounded unless you set it yourself "+
		"(http.Transport.ResponseHeaderTimeout), so a server that never responds will hang until your context expires",
		"transport", kind, "header_timeout", headerTimeout.String())
}

// New 校验 cfg、应用默认值并返回可用的 Provider。
func New(cfg Config) (*Provider, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("local: invalid base URL %q: %w", cfg.BaseURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("local: invalid base URL %q: scheme must be http or https", cfg.BaseURL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("local: invalid base URL %q: missing host", cfg.BaseURL)
	}

	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		env := strings.TrimSpace(cfg.APIKeyEnv)
		if env == "" {
			env = DefaultAPIKeyEnv
		}
		apiKey = strings.TrimSpace(os.Getenv(env))
	}
	if cfg.DisableAuth {
		apiKey = ""
	}

	headers := make(map[string]string, len(cfg.Headers))
	for name, value := range cfg.Headers {
		headers[name] = value
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

	headerTimeout := cfg.HeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = DefaultHeaderTimeout
	}
	normalized := options{
		name:          strings.TrimSpace(cfg.Name),
		baseURL:       base,
		chatPath:      normalizePath(cfg.ChatPath, DefaultChatPath),
		modelsPath:    normalizePath(cfg.ModelsPath, DefaultModelsPath),
		apiKey:        apiKey,
		disableAuth:   cfg.DisableAuth,
		defaultModel:  strings.TrimSpace(cfg.DefaultModel),
		headers:       headers,
		maxRetries:    maxRetries,
		backoff:       backoff,
		transportHint: strings.TrimSpace(cfg.TransportHint),
		headerTimeout: headerTimeout,
		quirks:        normalizeQuirks(cfg.Quirks),
	}
	if normalized.name == "" {
		normalized.name = DefaultName
	}

	client := resolveHTTPClient(cfg.HTTPClient, headerTimeout)

	return &Provider{
		cfg: normalized,
		transport: &Transport{
			Name:       normalized.name,
			Client:     client,
			MaxRetries: normalized.maxRetries,
			Backoff:    normalized.backoff,
			Hint:       normalized.transportHint,
		},
	}, nil
}

// normalizePath 归一化路径配置：空值用默认，确保以 '/' 开头。
func normalizePath(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return value
}

// mergeAliases 合并别名表（用户配置覆盖内置）。
func mergeAliases(base, extra map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(extra))
	for name, value := range base {
		merged[name] = value
	}
	for name, value := range extra {
		if key := strings.ToLower(strings.TrimSpace(name)); key != "" {
			merged[key] = strings.TrimSpace(value)
		}
	}
	return merged
}

// Name 实现 rig.Provider。
func (p *Provider) Name() string { return p.cfg.name }

// Transport 返回共享的 HTTP 传输层（带重试与提示）。宿主的厂商封装（ollama /
// lmstudio）用它拉模型列表，避免各写一份重试逻辑。
func (p *Provider) Transport() *Transport { return p.transport }

// Stream 实现 rig.Provider：POST {BaseURL}{ChatPath}，把 SSE 解码为 rig.Chunk。
func (p *Provider) Stream(ctx context.Context, req rig.Request) (rig.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := p.transport.Do(ctx, http.MethodPost, p.cfg.baseURL+p.cfg.chatPath, payload, p.requestHeaders())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode > 299 {
		snippet := readSnippet(resp)
		_ = resp.Body.Close()
		return nil, statusError(p.cfg.name, resp, snippet)
	}
	if err := CheckStreamingResponse(p.cfg.name, resp); err != nil {
		return nil, err
	}
	return newStreamWith(ctx, resp.Body, p.cfg.name, p.cfg.quirks, nil), nil
}

// requestHeaders 组装一次请求的头部（鉴权 + 用户覆盖）。
func (p *Provider) requestHeaders() http.Header {
	headers := make(http.Header, len(p.cfg.headers)+3)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("User-Agent", userAgent)
	if p.cfg.apiKey != "" && !p.cfg.disableAuth {
		headers.Set("Authorization", "Bearer "+p.cfg.apiKey)
	}
	for name, value := range p.cfg.headers {
		headers.Set(name, value)
	}
	return headers
}

// readSnippet 读取错误响应体的一小段。
func readSnippet(resp *http.Response) []byte {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyMax))
	return snippet
}

// statusError 渲染非 2xx 响应：`provider: METHOD URL: status: body (该怎么办)`。
//
// 三件事缺一不可：
//   - **method + URL**：本地服务常被反代改写路径，只给 404 无法定位是哪个端点；
//   - 服务端响应体（本地服务把真正原因放这里，例如 ollama 的
//     `{"error":"model \"x\" not found"}`）；
//   - 一句可执行提示：401/403/404/429/5xx 对个人用户来说，状态码本身没有信息量。
func statusError(name string, resp *http.Response, snippet []byte) error {
	if resp == nil {
		return fmt.Errorf("%s: request failed", name)
	}
	where := ""
	if resp.Request != nil && resp.Request.URL != nil {
		where = resp.Request.Method + " " + resp.Request.URL.String() + ": "
	}
	status := resp.Status
	if status == "" {
		status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	message := name + ": " + where + status
	if body := strings.TrimSpace(string(snippet)); body != "" {
		message += ": " + body
	}
	if hint := statusHint(resp.StatusCode); hint != "" {
		message += " (" + hint + ")"
	}
	return errors.New(message)
}

// statusHint 返回"该怎么办"的一句话；没有可说的就返回空串。
func statusHint(code int) string {
	switch {
	case code == http.StatusBadRequest:
		return "请求被服务端拒绝：模型名/参数可能不被该服务支持（本地模型常不支持 tools 或某些采样参数）"
	case code == http.StatusUnauthorized:
		return "鉴权失败：检查 Config.APIKey，或反代要求的鉴权头"
	case code == http.StatusForbidden:
		return "拒绝访问：检查 Config.APIKey 的权限范围，或反代是否允许该路径"
	case code == http.StatusNotFound:
		return "端点不存在：确认服务地址与路径（本地服务版本过旧，或反代改写了路径）"
	case code == http.StatusTooManyRequests:
		return "请求过多：稍后重试，或降低并发"
	case code >= 500:
		return "服务端错误：可稍后重试；若持续，查看本地模型服务的日志"
	default:
		return ""
	}
}

// checkStreamingResponse 拒绝不是 SSE 的 2xx 响应：本 provider 只支持流式。
// CheckStreamingResponse 拒绝不是 SSE 流的 2xx 响应（本 SDK 不实现非流式回退）。
//
// 导出以便 provider/openai 复用同一判断与文案。
func CheckStreamingResponse(name string, resp *http.Response) error {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" || strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return nil
	}
	snippet := readSnippet(resp)
	_ = resp.Body.Close()
	body := strings.TrimSpace(string(snippet))
	if body == "" {
		return fmt.Errorf("%s: expected a text/event-stream response, got Content-Type %q; non-streaming responses are not supported", name, contentType)
	}
	return fmt.Errorf("%s: expected a text/event-stream response, got Content-Type %q; non-streaming responses are not supported: %s", name, contentType, body)
}

// canceledError 以 errors.Is 可匹配的方式报告 ctx 取消。
func canceledError(name string, err error) error {
	return fmt.Errorf("%s: request canceled: %w", name, err)
}

// buildRequest 把 rig.Request 映射为 chat-completions 请求体（共享实现见
// BuildChatRequest：provider/openai 走同一份构造逻辑）。
func (p *Provider) buildRequest(req rig.Request) ([]byte, error) {
	return BuildChatRequest(ChatRequestOptions{
		Name:         p.cfg.name,
		Model:        req.Model,
		DefaultModel: p.cfg.defaultModel,
		Messages:     req.Messages,
		Tools:        req.Tools,
		Temperature:  req.Temperature,
		TopP:         req.TopP,
		MaxTokens:    req.MaxTokens,
		IncludeTools: !p.cfg.quirks.disableTools,
		IncludeUsage: !p.cfg.quirks.disableStreamOption,
		Extra:        req.Extra,
	})
}

// ChatRequestOptions 是一次 chat-completions 请求体的构造参数。
//
// provider/openai 与本地 provider 共用这一个构造器：两者的差别只有"发不发
// tools"、"发不发 stream_options"与"默认 reasoning_effort"，而这些都是字段，
// 不是两份协议实现。
type ChatRequestOptions struct {
	// Name 是错误前缀（"openai"、"local"…）。
	Name string
	// Model 是请求指定的模型；为空时回退 DefaultModel。
	Model string
	// DefaultModel 是 Model 为空时的默认模型。
	DefaultModel string
	// Messages 是对话。内容映射见 WireContent。
	Messages []rig.Message
	// Tools 是工具声明；IncludeTools 为 false 时整段省略。
	Tools []rig.ToolSpec
	// Temperature / TopP / MaxTokens 是采样选项（零值表示不发送）。
	Temperature *float64
	TopP        *float64
	MaxTokens   int
	// IncludeTools 为 false 时不发 tools（不支持 function calling 的本地模型）。
	IncludeTools bool
	// IncludeUsage 为 true 时发 stream_options.include_usage（拿得到 token 用量）。
	IncludeUsage bool
	// Extra 是 provider 专属的顶层字段；不能覆盖 provider 拥有的字段。
	Extra map[string]any
	// DefaultReasoningEffort 非空时注入 reasoning_effort（Extra 显式值优先）。
	DefaultReasoningEffort string
}

// BuildChatRequest 把一次请求映射为 chat-completions 的 JSON 请求体。
func BuildChatRequest(opts ChatRequestOptions) ([]byte, error) {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = DefaultName
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = strings.TrimSpace(opts.DefaultModel)
	}
	if model == "" {
		return nil, fmt.Errorf("%s: no model configured: set Config.DefaultModel or Request.Model: %w", name, rig.ErrModelRequired)
	}

	messages := make([]chatMessage, 0, len(opts.Messages))
	for _, message := range opts.Messages {
		wire := chatMessage{
			Role:       string(message.Role),
			Content:    WireContent(message),
			ToolCallID: message.ToolCallID,
			Name:       message.Name,
		}
		for _, call := range message.ToolCalls {
			arguments := strings.TrimSpace(call.Arguments)
			if arguments == "" {
				arguments = "{}" // 规范的"无参数"对象
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

	var tools []chatTool
	if opts.IncludeTools {
		tools = make([]chatTool, 0, len(opts.Tools))
		for _, spec := range opts.Tools {
			toolName := strings.TrimSpace(spec.Name)
			if toolName == "" {
				return nil, fmt.Errorf("%s: tool with empty name", name)
			}
			tools = append(tools, chatTool{
				Type: "function",
				Function: chatFunction{
					Name:        toolName,
					Description: spec.Description,
					Parameters:  spec.Parameters,
				},
			})
		}
	}

	body := chatRequest{
		Model:       model,
		Messages:    messages,
		Tools:       tools,
		Stream:      true,
		Temperature: cloneFloat(opts.Temperature),
		TopP:        cloneFloat(opts.TopP),
		MaxTokens:   opts.MaxTokens,
	}
	if opts.IncludeUsage {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", name, err)
	}
	extra := opts.Extra
	if effort := strings.TrimSpace(opts.DefaultReasoningEffort); effort != "" {
		if _, explicit := extra["reasoning_effort"]; !explicit {
			// 拷贝后注入，避免修改调用方传入的 map。
			merged := make(map[string]any, len(extra)+1)
			for field, value := range extra {
				merged[field] = value
			}
			merged["reasoning_effort"] = effort
			extra = merged
		}
	}
	if len(extra) == 0 {
		return raw, nil
	}
	return mergeExtra(name, raw, extra)
}

// reservedRequestFields 由 provider 拥有；Extra 不能覆盖它们。
var reservedRequestFields = map[string]bool{
	"model": true, "messages": true, "tools": true, "stream": true,
	"stream_options": true, "temperature": true, "top_p": true, "max_tokens": true,
}

// mergeExtra 合并 provider 专属的顶层字段（例如本地服务特有的 seed、num_ctx），
// 同时不允许覆盖 provider 自己拥有的字段。
func mergeExtra(name string, raw []byte, extra map[string]any) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", name, err)
	}
	for field, value := range extra {
		if reservedRequestFields[field] {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("%s: encode extra field %q: %w", name, field, err)
		}
		fields[field] = encoded
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", name, err)
	}
	return merged, nil
}

// wireContent 映射一条消息的内容：纯文本用字符串；user/assistant 带图片时转成
// 多模态数组（本地服务多数支持这种形状，不支持时会明确报错）。
// WireContent 映射一条消息的内容：纯文本用字符串；user/assistant 带图片时转成
// 多模态数组。
//
// 导出以便 provider/openai 的请求构造与内部基准复用同一份映射。
func WireContent(message rig.Message) any {
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
