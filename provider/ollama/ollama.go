// Package ollama 为本地 Ollama 服务实现 rig.Provider。
//
// Ollama 提供两层 API：原生的 /api/*（chat、tags 等）与 OpenAI 兼容的
// /v1/chat/completions。本包只用**兼容层**跑对话（这样工具调用、多模态、用的都是
// 与其它 provider 相同的一套语义），但用**原生 /api/tags** 列出模型——原生接口的
// 信息更全（权重大小、量化、修改时间），而 /v1/models 只给名字。
//
//	p, err := ollama.New(ollama.Config{DefaultModel: "llama3.2"})
//	stream, err := p.Stream(ctx, rig.Request{Model: "llama3.2", Messages: ...})
//	models, err := p.Models(ctx) // GET http://127.0.0.1:11434/api/tags
//
// # 本地服务的默认值
//
//   - BaseURL 默认 http://127.0.0.1:11434（可以写成带 /v1 的形式，本包会归一化）；
//   - **不发 Authorization**：本机 ollama 无鉴权。只有你把 ollama 放在带鉴权的反代
//     后面时才需要配置 Config.APIKey；
//   - 容忍缺失的 [DONE]（ollama 常常直接关连接），并归一化 finish_reason 变体；
//   - 连不上时错误里带一句可执行提示（未启动？先 `ollama serve`）。
//
// # 请求头与鉴权的作用范围
//
// 本包向两类端点发请求，**两者都**使用 Config.Headers 与 Config.APIKey，来源一致：
//
//   - 对话（`/v1/chat/completions`，OpenAI 兼容层，实际由 provider/local 执行）：
//     Content-Type / Accept / User-Agent 由 SDK 设置；Authorization: Bearer 来自
//     Config.APIKey（为空则**不发**）；最后叠加 Config.Headers（同名键覆盖 SDK 默认值）。
//   - 模型列表（`/api/tags`，原生 API，本包自己发）：Accept / User-Agent +
//     同样的 Authorization 与 Config.Headers。
//
// 也就是说：**在 Config.Headers 里加的自定义头，对话请求也会带**（例如网关的
// 归因头、`X-Org-Id`）。本包不提供"只给列表请求加头"的分流；需要那种粒度时请
// 直接用 provider/local 自己装。
//
// # 与托管服务的差异（会让用户困惑的地方）
//
//   - Ollama 的兼容层**只支持部分 OpenAI 参数**：ResponseFormat/json_schema、
//     logit_bias、presence/frequency_penalty 等会被忽略或报错；采样参数建议通过
//     原生 options（本包不发送，避免兼容层拒绝）。
//   - 工具调用需要模型本身支持（llama3.1+、qwen2.5 等）；不支持时可用
//     Config.Quirks.DisableTools 关掉 tools，避免服务端 400。
//   - 上下文窗口由**模型自身的 Modelfile**（num_ctx）决定，请求里的 max_tokens 只
//     限制输出，不吃掉输入预算。
package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/local"
)

const (
	// Name 是 rig.Provider.Name() 的取值。
	Name = "ollama"
	// DefaultBaseURL 是 Ollama 的默认监听地址（不含 /v1）。
	DefaultBaseURL = "http://127.0.0.1:11434"
	// ModelsPath 是原生模型列表路径（相对 BaseURL）。
	ModelsPath = "/api/tags"
	// chatPrefix 是 OpenAI 兼容层的路径前缀。
	chatPrefix = "/v1"
	// DefaultHeaderTimeout 是等待响应头的默认上限：本地模型冷启动（加载权重）可能
	// 需要几十秒到几分钟，60s 的通用默认值在这里太短。
	DefaultHeaderTimeout = 5 * time.Minute

	// unreachableHint 附加在"连不上"错误之后。
	unreachableHint = "ollama 未启动？先运行 `ollama serve`（或设置 OLLAMA_HOST 指向已有服务）"
)

// orDefault 返回 value，零值时用 fallback。
func orDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

// Config 配置 Provider。零值即可用（默认本机 11434、无鉴权、默认模型由
// DefaultModel 提供）。
type Config struct {
	// BaseURL 是 Ollama 服务地址，默认 DefaultBaseURL。
	// 写成带 /v1 的形式也可以（本包会归一化成 根 + /v1 的兼容层地址）。
	BaseURL string
	// DefaultModel 在 Request 未指定模型时使用（例如 "llama3.2"）。
	DefaultModel string
	// APIKey 只有把 ollama 放在带鉴权的反向代理后面时才需要；默认不发鉴权头。
	APIKey string
	// Headers 是额外请求头。
	Headers map[string]string
	// HTTPClient 覆盖 HTTP 客户端。
	HTTPClient *http.Client
	// MaxRetries 是可重试失败的额外尝试次数；零表示默认（2），负值禁用。
	MaxRetries int
	// RetryBackoff 是首次重试的基础延迟；零表示默认（500ms），负值立即重试。
	RetryBackoff time.Duration
	// HeaderTimeout 是等待响应头的时间上限；零表示 DefaultHeaderTimeout
	// （本地模型首次加载可能很慢，因此默认 5 分钟）。
	HeaderTimeout time.Duration
	// Quirks 透传给共享基座（例如 DisableTools、RequireDone、自定义 finish_reason
	// 别名与额外 reasoning 键名）。
	Quirks local.Quirks
}

// Provider 是 Ollama 的 rig.Provider 实现（并发安全）。
type Provider struct {
	*local.Provider
	// root 是不带 /v1 的服务根地址（原生 API 与兼容层都从它派生）。
	root string
	// apiKey / headers 只用于原生 /api/tags 请求（兼容层由 local 负责）。
	apiKey  string
	headers map[string]string
}

// New 校验配置并构造 Provider。
func New(cfg Config) (*Provider, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	root, err := normalizeRoot(base)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	inner, err := local.New(local.Config{
		Name:          Name,
		BaseURL:       root + chatPrefix,
		ChatPath:      local.DefaultChatPath,
		ModelsPath:    local.DefaultModelsPath, // 兼容层也有 /v1/models，但我们用原生 /api/tags
		APIKey:        cfg.APIKey,
		DisableAuth:   strings.TrimSpace(cfg.APIKey) == "",
		DefaultModel:  cfg.DefaultModel,
		Headers:       cfg.Headers,
		HTTPClient:    cfg.HTTPClient,
		MaxRetries:    cfg.MaxRetries,
		RetryBackoff:  cfg.RetryBackoff,
		HeaderTimeout: orDefault(cfg.HeaderTimeout, DefaultHeaderTimeout),
		Quirks:        cfg.Quirks,
		TransportHint: unreachableHint,
	})
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	return &Provider{Provider: inner, root: root, apiKey: strings.TrimSpace(cfg.APIKey), headers: cfg.Headers}, nil
}

// normalizeRoot 把各种写法的 BaseURL 归一化成"不带结尾 /v1 的服务根"。
//
// 支持的写法：http://127.0.0.1:11434、…/11434/、…/11434/v1、以及带路径前缀的
// 反代地址（http://gateway/ollama → http://gateway/ollama）。
func normalizeRoot(base string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid base URL %q: %w", base, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid base URL %q: scheme must be http or https", base)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("invalid base URL %q: missing host", base)
	}
	path := strings.TrimRight(parsed.Path, "/")
	path = strings.TrimSuffix(path, chatPrefix)
	parsed.Path = path
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

// Endpoint 返回端点描述（配置期使用，供宿主的 provider 选择器展示）。
func Endpoint() local.Endpoint {
	return local.Endpoint{
		Name:           Name,
		DefaultBaseURL: DefaultBaseURL,
		AuthRequired:   false,
		ModelsPath:     ModelsPath,
	}
}

// Endpoint 返回本实例的端点描述（服务从哪个根地址、模型列表走哪条路径）。
func (p *Provider) Endpoint() local.Endpoint {
	endpoint := Endpoint()
	endpoint.DefaultBaseURL = p.root
	endpoint.AuthRequired = p.apiKey != ""
	return endpoint
}

// tagsPayload 是 GET /api/tags 的响应体。
type tagsPayload struct {
	Models []tagEntry `json:"models"`
}

// tagEntry 是 /api/tags 里的一个模型。
type tagEntry struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	ModifiedAt string `json:"modified_at"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
	Details    struct {
		Family            string `json:"family"`
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
}

// Models 实现 local.ModelLister：GET {root}/api/tags，按模型名排序返回。
//
// 返回的 ID 就是可以直接用在 Request.Model 里的名字（带 tag，例如
// "llama3.2:latest"）。
func (p *Provider) Models(ctx context.Context) ([]local.ModelInfo, error) {
	body, err := p.Transport().GetBody(ctx, p.root+ModelsPath, p.modelHeaders())
	if err != nil {
		return nil, err
	}
	var payload tagsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("ollama: decode /api/tags: %w", err)
	}
	models := make([]local.ModelInfo, 0, len(payload.Models))
	for _, entry := range payload.Models {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = strings.TrimSpace(entry.Model)
		}
		if name == "" {
			continue
		}
		info := local.ModelInfo{ID: name, Name: name, Size: entry.Size}
		if entry.ModifiedAt != "" {
			if when, err := time.Parse(time.RFC3339Nano, entry.ModifiedAt); err == nil {
				info.Modified = when
			}
		}
		models = append(models, info)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

// modelHeaders 是原生 API 请求的头（不要 Accept: text/event-stream）。
func (p *Provider) modelHeaders() http.Header {
	headers := http.Header{
		"Accept":     {"application/json"},
		"User-Agent": {"zeokeel-rig"},
	}
	if p.apiKey != "" {
		headers.Set("Authorization", "Bearer "+p.apiKey)
	}
	for name, value := range p.headers {
		headers.Set(name, value)
	}
	return headers
}

var (
	_ rig.Provider      = (*Provider)(nil)
	_ local.ModelLister = (*Provider)(nil)
)
