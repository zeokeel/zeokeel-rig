package local

// 本文件是模型列表探测：GET {BaseURL}{ModelsPath}，把各家略有差异的响应统一成
// []ModelInfo。宿主的"provider 选择 + 模型预设"UI 通过 ModelLister 拿这个列表，
// 不需要知道具体是 ollama、LM Studio 还是别的 OpenAI 兼容服务。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ModelInfo 是模型列表里的一项（跨服务统一形态）。
type ModelInfo struct {
	// ID 是请求时要用的模型名（OpenAI 兼容端点上的 model 字段）。
	ID string `json:"id"`
	// Name 是展示名；服务端没给时等于 ID。
	Name string `json:"name,omitempty"`
	// Owner 是归属方（owned_by / publisher），未知为空。
	Owner string `json:"owner,omitempty"`
	// Size 是本地权重文件大小（字节）；未知为 0。
	Size int64 `json:"size,omitempty"`
	// Modified 是权重文件的修改时间（Ollama 的 modified_at）；零值表示未知。
	Modified time.Time `json:"modified,omitempty"`
	// ContextLength 是服务端声明的上下文窗口；0 表示未知。
	ContextLength int `json:"context_length,omitempty"`
}

// ModelLister 是"能列出可用模型"的可选能力。kit 的 provider 选择器用它做模型预设。
type ModelLister interface {
	Models(ctx context.Context) ([]ModelInfo, error)
}

// Endpoint 描述一种本地服务端点，供宿主的 provider 选择器展示与装配。
type Endpoint struct {
	// Name 与 rig.Provider.Name() 一致（"ollama"、"lmstudio"、"local"）。
	Name string
	// DefaultBaseURL 是默认 API 根（不含 /chat/completions）。
	DefaultBaseURL string
	// AuthRequired 报告这类服务通常是否需要 API key（本地服务一般 false）。
	AuthRequired bool
	// ModelsPath 是模型列表路径（相对 BaseURL 或绝对路径）。
	ModelsPath string
}

// Endpoint 返回本 provider 的端点描述。
func (p *Provider) Endpoint() Endpoint {
	return Endpoint{
		Name:           p.cfg.name,
		DefaultBaseURL: p.cfg.baseURL,
		AuthRequired:   p.cfg.apiKey != "" && !p.cfg.disableAuth,
		ModelsPath:     p.cfg.modelsPath,
	}
}

// Models 实现 ModelLister：GET {BaseURL}{ModelsPath} 并解析成 []ModelInfo。
func (p *Provider) Models(ctx context.Context) ([]ModelInfo, error) {
	body, err := p.transport.GetBody(ctx, p.cfg.baseURL+p.cfg.modelsPath, p.modelHeaders())
	if err != nil {
		return nil, err
	}
	models, err := decodeModels(body)
	if err != nil {
		return nil, fmt.Errorf("%s: decode model list: %w", p.cfg.name, err)
	}
	return models, nil
}

// modelHeaders 是模型列表请求的头（保留鉴权，但不要 Accept: text/event-stream）。
func (p *Provider) modelHeaders() http.Header {
	headers := make(http.Header, len(p.cfg.headers)+2)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", userAgent)
	if p.cfg.apiKey != "" && !p.cfg.disableAuth {
		headers.Set("Authorization", "Bearer "+p.cfg.apiKey)
	}
	for name, value := range p.cfg.headers {
		headers.Set(name, value)
	}
	return headers
}

// modelsPayload 覆盖两种常见形状：OpenAI 的 {"data":[...]} 与
// {"models":[...]}（部分网关）。
type modelsPayload struct {
	Data   []modelEntry `json:"data"`
	Models []modelEntry `json:"models"`
}

// modelEntry 是模型列表里的一项（宽松建模：各家字段名不同）。
type modelEntry struct {
	ID               string `json:"id"`
	Model            string `json:"model"`
	Name             string `json:"name"`
	OwnedBy          string `json:"owned_by"`
	Publisher        string `json:"publisher"`
	Size             int64  `json:"size"`
	Created          int64  `json:"created"`
	ModifiedAt       string `json:"modified_at"`
	ContextLength    int    `json:"context_length"`
	MaxContextLength int    `json:"max_context_length"`
	LoadedContext    int    `json:"loaded_context_length"`
}

// decodeModels 把模型列表响应体解析成 []ModelInfo（按 ID 排序，去重）。
func decodeModels(body []byte) ([]ModelInfo, error) {
	var payload modelsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	entries := payload.Data
	if len(entries) == 0 {
		entries = payload.Models
	}
	seen := make(map[string]struct{}, len(entries))
	models := make([]ModelInfo, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			id = strings.TrimSpace(entry.Model)
		}
		if id == "" {
			id = strings.TrimSpace(entry.Name)
		}
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		info := ModelInfo{
			ID:            id,
			Name:          firstNonEmpty(entry.Name, entry.Model, id),
			Owner:         firstNonEmpty(entry.OwnedBy, entry.Publisher),
			Size:          entry.Size,
			ContextLength: firstPositive(entry.ContextLength, entry.MaxContextLength, entry.LoadedContext),
		}
		if entry.ModifiedAt != "" {
			if when, err := time.Parse(time.RFC3339Nano, entry.ModifiedAt); err == nil {
				info.Modified = when
			}
		}
		if info.Modified.IsZero() && entry.Created > 0 {
			info.Modified = time.Unix(entry.Created, 0).UTC()
		}
		models = append(models, info)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// firstPositive 返回第一个正数（0 视为"未知"）。
func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
