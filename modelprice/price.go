package modelprice

import (
	"sort"
	"strings"
)

// TableUpdated 是价格表的采集日期（YYYY-MM-DD）。UI 可以把它显示成"价目表更新于"。
const TableUpdated = "2026-11-17"

// Price 是一个模型的价目，单位是**美元 / 每百万 token**。
type Price struct {
	// Model 是表内规范名（查找结果的键，例如 "gpt-4o"）。
	Model string `json:"model"`
	// Provider 是厂商名（openai/anthropic/deepseek/…）；由 provider 前缀限定查询时
	// 会覆盖成查询里的 provider。
	Provider string `json:"provider,omitempty"`
	// InputPerMillion 是未命中缓存的输入价。
	InputPerMillion float64 `json:"input_per_million"`
	// CachedInputPerMillion 是缓存命中的输入价；0 表示没有公开折扣（按输入价计）。
	CachedInputPerMillion float64 `json:"cached_input_per_million,omitempty"`
	// OutputPerMillion 是输出价。
	OutputPerMillion float64 `json:"output_per_million"`
	// Estimated 表示这个价格是**估算值**（会随厂商调价而过时）。
	Estimated bool `json:"estimated"`
	// Note 是给 UI/日志用的一句话说明。
	Note string `json:"note,omitempty"`
	// Updated 是该条目的采集日期。
	Updated string `json:"updated,omitempty"`
}

// 本地 provider 前缀：这些推理后端没有 API 计费。
var localProviders = map[string]bool{
	"ollama":                true,
	"lmstudio":              true,
	"llamacpp":              true,
	"llama.cpp":             true,
	"vllm":                  true,
	"mlx":                   true,
	"local":                 true,
	"text-generation-webui": true,
}

// openWeightFamilies 是"看到 name:tag 形式就认为是本地权重"的家族名。
//
// 只收录**没有官方按 token 计费 API** 的家族；qwen/mistral 这类既有开放权重又有
// 商业 API 的家族刻意不收，避免把付费调用算成 0（宁高不低）。
var openWeightFamilies = map[string]bool{
	"llama":       true,
	"llama2":      true,
	"llama3":      true,
	"llama3.1":    true,
	"llama3.2":    true,
	"llama3.3":    true,
	"gemma":       true,
	"gemma2":      true,
	"phi":         true,
	"phi3":        true,
	"phi4":        true,
	"codellama":   true,
	"deepseek-r1": true, // 本地蒸馏版常用这个 tag
	"qwq":         true,
}

// table 是价目表（键 = 规范名，全部小写）。
//
// 数字来自各厂商公开 list price（USD / 1M tokens）。**改这张表等于改对外承诺**：
// 只允许人工显式修改，并在 CHANGELOG 记一行。
var table = map[string]Price{
	// ---- OpenAI ----
	"gpt-5":        {Model: "gpt-5", Provider: "openai", InputPerMillion: 1.25, CachedInputPerMillion: 0.125, OutputPerMillion: 10.00, Estimated: true, Updated: TableUpdated},
	"gpt-5-mini":   {Model: "gpt-5-mini", Provider: "openai", InputPerMillion: 0.25, CachedInputPerMillion: 0.025, OutputPerMillion: 2.00, Estimated: true, Updated: TableUpdated},
	"gpt-5-nano":   {Model: "gpt-5-nano", Provider: "openai", InputPerMillion: 0.05, CachedInputPerMillion: 0.005, OutputPerMillion: 0.40, Estimated: true, Updated: TableUpdated},
	"gpt-4o":       {Model: "gpt-4o", Provider: "openai", InputPerMillion: 2.50, CachedInputPerMillion: 1.25, OutputPerMillion: 10.00, Estimated: true, Updated: TableUpdated},
	"gpt-4o-mini":  {Model: "gpt-4o-mini", Provider: "openai", InputPerMillion: 0.15, CachedInputPerMillion: 0.075, OutputPerMillion: 0.60, Estimated: true, Updated: TableUpdated},
	"gpt-4.1":      {Model: "gpt-4.1", Provider: "openai", InputPerMillion: 2.00, CachedInputPerMillion: 0.50, OutputPerMillion: 8.00, Estimated: true, Updated: TableUpdated},
	"gpt-4.1-mini": {Model: "gpt-4.1-mini", Provider: "openai", InputPerMillion: 0.40, CachedInputPerMillion: 0.10, OutputPerMillion: 1.60, Estimated: true, Updated: TableUpdated},
	"gpt-4.1-nano": {Model: "gpt-4.1-nano", Provider: "openai", InputPerMillion: 0.10, CachedInputPerMillion: 0.025, OutputPerMillion: 0.40, Estimated: true, Updated: TableUpdated},
	"gpt-4-turbo":  {Model: "gpt-4-turbo", Provider: "openai", InputPerMillion: 10.00, CachedInputPerMillion: 0, OutputPerMillion: 30.00, Estimated: true, Updated: TableUpdated},
	"gpt-4":        {Model: "gpt-4", Provider: "openai", InputPerMillion: 30.00, CachedInputPerMillion: 0, OutputPerMillion: 60.00, Estimated: true, Updated: TableUpdated},
	"o3":           {Model: "o3", Provider: "openai", InputPerMillion: 2.00, CachedInputPerMillion: 0.50, OutputPerMillion: 8.00, Estimated: true, Updated: TableUpdated},
	"o4-mini":      {Model: "o4-mini", Provider: "openai", InputPerMillion: 1.10, CachedInputPerMillion: 0.275, OutputPerMillion: 4.40, Estimated: true, Updated: TableUpdated},

	// ---- Anthropic ----
	"claude-opus-4":     {Model: "claude-opus-4", Provider: "anthropic", InputPerMillion: 15.00, CachedInputPerMillion: 1.50, OutputPerMillion: 75.00, Estimated: true, Updated: TableUpdated},
	"claude-sonnet-4":   {Model: "claude-sonnet-4", Provider: "anthropic", InputPerMillion: 3.00, CachedInputPerMillion: 0.30, OutputPerMillion: 15.00, Estimated: true, Updated: TableUpdated},
	"claude-3-7-sonnet": {Model: "claude-3-7-sonnet", Provider: "anthropic", InputPerMillion: 3.00, CachedInputPerMillion: 0.30, OutputPerMillion: 15.00, Estimated: true, Updated: TableUpdated},
	"claude-3-5-sonnet": {Model: "claude-3-5-sonnet", Provider: "anthropic", InputPerMillion: 3.00, CachedInputPerMillion: 0.30, OutputPerMillion: 15.00, Estimated: true, Updated: TableUpdated},
	"claude-3-5-haiku":  {Model: "claude-3-5-haiku", Provider: "anthropic", InputPerMillion: 0.80, CachedInputPerMillion: 0.08, OutputPerMillion: 4.00, Estimated: true, Updated: TableUpdated},
	"claude-3-opus":     {Model: "claude-3-opus", Provider: "anthropic", InputPerMillion: 15.00, CachedInputPerMillion: 1.50, OutputPerMillion: 75.00, Estimated: true, Updated: TableUpdated},
	"claude-3-haiku":    {Model: "claude-3-haiku", Provider: "anthropic", InputPerMillion: 0.25, CachedInputPerMillion: 0.03, OutputPerMillion: 1.25, Estimated: true, Updated: TableUpdated},

	// ---- DeepSeek ----
	"deepseek-chat":     {Model: "deepseek-chat", Provider: "deepseek", InputPerMillion: 0.27, CachedInputPerMillion: 0.07, OutputPerMillion: 1.10, Estimated: true, Updated: TableUpdated},
	"deepseek-reasoner": {Model: "deepseek-reasoner", Provider: "deepseek", InputPerMillion: 0.55, CachedInputPerMillion: 0.14, OutputPerMillion: 2.19, Estimated: true, Updated: TableUpdated},

	// ---- 阿里云百炼 / DashScope ----
	"qwen-max":   {Model: "qwen-max", Provider: "alibaba", InputPerMillion: 1.60, CachedInputPerMillion: 0, OutputPerMillion: 6.40, Estimated: true, Updated: TableUpdated},
	"qwen-plus":  {Model: "qwen-plus", Provider: "alibaba", InputPerMillion: 0.40, CachedInputPerMillion: 0, OutputPerMillion: 1.20, Estimated: true, Updated: TableUpdated},
	"qwen-turbo": {Model: "qwen-turbo", Provider: "alibaba", InputPerMillion: 0.05, CachedInputPerMillion: 0, OutputPerMillion: 0.20, Estimated: true, Updated: TableUpdated},
}

// prefixKeys 是 table 的键按长度降序排列（最长前缀优先），首次使用时构建。
var prefixKeys = func() []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if len(keys[left]) != len(keys[right]) {
			return len(keys[left]) > len(keys[right])
		}
		return keys[left] < keys[right]
	})
	return keys
}()

// localPrice 构造一条"本地推理，0 价"的结果。
func localPrice(model, provider string) Price {
	return Price{
		Model:     model,
		Provider:  provider,
		Estimated: false,
		Note:      "本地推理，无 API 计费",
		Updated:   TableUpdated,
	}
}

// isBoundary 报告前缀之后的字符是否算"分隔符"。
//
// 没有这一条，`gpt-4oz` 会被当成 `gpt-4`（多算了 12 倍），`claude-3-5-sonnetx`
// 会被当成 `claude-3-5-sonnet`。
func isBoundary(char byte) bool {
	switch char {
	case '-', '.', ':', '@', '/':
		return true
	default:
		return false
	}
}

// lookupKey 在 table 里查一个（已归一化的）名字：精确优先，其次最长前缀。
func lookupKey(name string) (Price, bool) {
	if price, ok := table[name]; ok {
		return price, true
	}
	for _, key := range prefixKeys {
		if len(name) <= len(key) || !strings.HasPrefix(name, key) {
			continue
		}
		if isBoundary(name[len(key)]) {
			price := table[key]
			return price, true
		}
	}
	return Price{}, false
}

// Lookup 查一个模型名的价目。
//
// 支持 `gpt-4o`、`gpt-4o-2024-08-06`、`openai/gpt-4o`、`ollama/llama3.1`、
// `llama3.1:8b` 这些形态；未知模型返回 ok=false 与零值 Price（**绝不编造价格**）。
// 空字符串与纯空白返回 ok=false。
func Lookup(model string) (Price, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return Price{}, false
	}

	// 1) 逐段剥离 provider 前缀（vendor/org/model 也支持）。
	provider := ""
	rest := name
	for {
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 || slash == len(rest)-1 {
			break
		}
		segment := rest[:slash]
		rest = rest[slash+1:]
		if provider == "" {
			provider = segment
		}
		if localProviders[segment] {
			return localPrice(rest, segment), true
		}
		if price, ok := lookupWithProvider(rest, provider); ok {
			return price, true
		}
	}

	// 2) 无 provider 前缀（或剥离后没命中）：查整串。
	if price, ok := lookupWithProvider(name, provider); ok {
		return price, true
	}

	// 3) Ollama 风格 name:tag + 已知开源权重家族 → 本地 0 价。
	if colon := strings.IndexByte(name, ':'); colon > 0 && (provider == "" || localProviders[provider]) {
		family := name[:colon]
		for candidate := range openWeightFamilies {
			if family == candidate || strings.HasPrefix(family, candidate+"-") || strings.HasPrefix(family, candidate+".") {
				return localPrice(name, "local"), true
			}
		}
	}
	return Price{}, false
}

// lookupWithProvider 在表里查名字，并把查询里的 provider 写进结果。
func lookupWithProvider(name, provider string) (Price, bool) {
	price, ok := lookupKey(name)
	if !ok {
		return Price{}, false
	}
	if provider != "" {
		price.Provider = provider
	}
	return price, true
}

// All 返回价目表的副本，按 Model 升序。调用方可以随意修改返回值。
func All() []Price {
	out := make([]Price, 0, len(table))
	for _, price := range table {
		out = append(out, price)
	}
	sort.Slice(out, func(left, right int) bool { return out[left].Model < out[right].Model })
	return out
}
