// Package lmstudio 为 LM Studio 的本地服务器实现 rig.Provider。
//
// LM Studio 在本地起一个完整的 OpenAI 兼容服务（默认 http://127.0.0.1:1234，
// 需要在 LM Studio 的 Developer 面板里打开 Local Server）。本包走标准 /v1 路径：
//
//	p, err := lmstudio.New(lmstudio.Config{DefaultModel: "qwen2.5-7b-instruct"})
//	stream, err := p.Stream(ctx, rig.Request{...})
//	models, err := p.Models(ctx) // GET http://127.0.0.1:1234/v1/models
//
// # 默认值
//
//   - BaseURL 默认 http://127.0.0.1:1234（带不带 /v1 都可以，本包会归一化）；
//   - **不发 Authorization**：本机 LM Studio 无鉴权；只有套了带鉴权的反代时才配
//     Config.APIKey；
//   - 与 ollama 共享本地基座（容忍缺失 [DONE]、归一化 finish_reason 变体、弹性解析
//     tool_calls / content 形状），因此行为与 ollama 一致，减少"换模型就坏"的意外。
//
// # 与托管服务的差异
//
//   - 实际可用的模型取决于你在 LM Studio 里加载了哪些（空列表 = 没有加载模型）；
//   - JIT 加载（自动换模型）会带来首字延迟：第一次请求可能几十秒才开始返回；
//   - 部分量化模型不支持工具调用；可用 Config.Quirks.DisableTools 关掉 tools；
//   - 上下文窗口由加载时的配置决定，请求里只建议限制 max_tokens（输出长度）。
package lmstudio

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/local"
)

const (
	// Name 是 rig.Provider.Name() 的取值。
	Name = "lmstudio"
	// DefaultBaseURL 是 LM Studio Local Server 的默认地址（不含 /v1）。
	DefaultBaseURL = "http://127.0.0.1:1234"
	// apiPrefix 是 OpenAI 兼容层的路径前缀。
	apiPrefix = "/v1"
	// DefaultHeaderTimeout 是等待响应头的默认上限：本地模型冷启动（加载权重）可能
	// 需要几十秒到几分钟，60s 的通用默认值在这里太短。
	DefaultHeaderTimeout = 5 * time.Minute

	// unreachableHint 附加在"连不上"错误之后。
	unreachableHint = "LM Studio 未启动？打开 LM Studio → Developer → 开启 Local Server（默认端口 1234）"
)

// orDefault 返回 value，零值时用 fallback。
func orDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

// Config 配置 Provider。零值可用（默认本机 1234、无鉴权）。
type Config struct {
	// BaseURL 是 LM Studio 服务地址，默认 DefaultBaseURL（可带 /v1）。
	BaseURL string
	// DefaultModel 在 Request 未指定模型时使用。
	DefaultModel string
	// APIKey 只有套了带鉴权的反向代理时才需要。
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
	// Quirks 透传给共享基座。
	Quirks local.Quirks
}

// Provider 是 LM Studio 的 rig.Provider 实现（并发安全）。
type Provider struct {
	*local.Provider
	// root 是不带 /v1 的服务根地址。
	root string
}

// New 校验配置并构造 Provider。
func New(cfg Config) (*Provider, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	root, err := normalizeRoot(base)
	if err != nil {
		return nil, fmt.Errorf("lmstudio: %w", err)
	}
	inner, err := local.New(local.Config{
		Name:          Name,
		BaseURL:       root + apiPrefix,
		DefaultModel:  cfg.DefaultModel,
		APIKey:        cfg.APIKey,
		DisableAuth:   strings.TrimSpace(cfg.APIKey) == "",
		Headers:       cfg.Headers,
		HTTPClient:    cfg.HTTPClient,
		MaxRetries:    cfg.MaxRetries,
		RetryBackoff:  cfg.RetryBackoff,
		HeaderTimeout: orDefault(cfg.HeaderTimeout, DefaultHeaderTimeout),
		Quirks:        cfg.Quirks,
		TransportHint: unreachableHint,
	})
	if err != nil {
		return nil, fmt.Errorf("lmstudio: %w", err)
	}
	return &Provider{Provider: inner, root: root}, nil
}

// normalizeRoot 归一化成"不带结尾 /v1 的服务根"。
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
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), apiPrefix)
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

// Endpoint 返回端点描述（配置期使用，供宿主的 provider 选择器展示）。
func Endpoint() local.Endpoint {
	return local.Endpoint{
		Name:           Name,
		DefaultBaseURL: DefaultBaseURL,
		AuthRequired:   false,
		ModelsPath:     apiPrefix + local.DefaultModelsPath,
	}
}

// Endpoint 返回本实例的端点描述：以内层 provider 的真实鉴权状态为准，只把地址与
// 模型列表路径换成 LM Studio 的形态（根地址不带 /v1、列表在 /v1/models）。
func (p *Provider) Endpoint() local.Endpoint {
	endpoint := p.Provider.Endpoint()
	endpoint.DefaultBaseURL = p.root
	endpoint.ModelsPath = apiPrefix + local.DefaultModelsPath
	return endpoint
}

var (
	_ rig.Provider      = (*Provider)(nil)
	_ local.ModelLister = (*Provider)(nil)
)
