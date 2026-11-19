package kit

// 个人客户端的"配置档"：让宿主**一行**就能得到一组合理的默认值。
//
// 设计意图：个人用户不写配置文件。他要的是"本地模型 + 不打扰我"或
// "云模型 + 每步问我"。所以这里给的是**命名档**（profile），而不是十几个
// 需要理解的开关；每个档只填宿主没显式设置的字段。

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/lmstudio"
	"github.com/zeokeel/zeokeel-rig/provider/local"
	"github.com/zeokeel/zeokeel-rig/provider/ollama"
	"github.com/zeokeel/zeokeel-rig/provider/openai"
)

// ProviderKind 指定"从零构造 provider"时用哪一套协议。
//
// 个人用户会天天换模型：今天某个闭源前沿模型、明天兼容网关、后天本地 ollama。
// 让宿主自己 import 具体 provider 包并挑参数是没必要的负担——给 kind + 地址即可。
type ProviderKind string

const (
	// ProviderOpenAI 是托管 OpenAI（或任意 OpenAI 兼容网关）。
	ProviderOpenAI ProviderKind = "openai"
	// ProviderOllama 是本地 Ollama（http://127.0.0.1:11434）。
	ProviderOllama ProviderKind = "ollama"
	// ProviderLMStudio 是本地 LM Studio（http://127.0.0.1:1234）。
	ProviderLMStudio ProviderKind = "lmstudio"
	// ProviderLocal 是自建的 OpenAI 兼容端点（vLLM/llama.cpp/反代）。
	ProviderLocal ProviderKind = "local"
)

// Profile 是一组命名的默认值。零值字段表示"不改宿主设置"。
//
// 注意：Profile 只决定 Approval/ToolPreset/ProviderKind/MaxIterations；要选沙箱档位
// 用 Config.PermissionPreset（它比 Profile 更具体，先落地）。
type Profile struct {
	// Name 是档名，便于日志与错误信息。
	Name string
	// Approval 建议的审批档（宿主显式设置 Config.Approval 时不覆盖）。
	Approval ApprovalMode
	// ToolPreset 建议的工具集预设。
	ToolPreset string
	// ProviderKind 建议的 provider 协议（仅在 Config.Provider 为 nil 且
	// Config.ProviderKind 为空时采用）。
	ProviderKind ProviderKind
	// MaxIterations 建议的轮数上限。
	MaxIterations int
	// Note 是一句人话说明，可直接显示在设置界面上。
	Note string
}

// Profiles 返回内置的配置档（按名称索引）。
//
// 三档覆盖个人用户最常见的三种心态：
//   - "careful"：每步问我（写文件、跑命令都要我点同意）；
//   - "personal"：只读免问、其余（含危险命令）问我（推荐默认）；
//   - "local"：跑本地模型、不联网、不问人（ApprovalAuto = 全部放行，离线/省钱）。
func Profiles() map[string]Profile {
	return map[string]Profile{
		ProfileCareful: {
			Name:          ProfileCareful,
			Approval:      ApprovalAsk,
			ToolPreset:    ToolPresetPersonal,
			ProviderKind:  ProviderOpenAI,
			MaxIterations: DefaultSafeMaxIterations,
			Note:          "每一步都问我：只读操作免问，其余写/执行都要我确认",
		},
		ProfilePersonal: {
			Name:          ProfilePersonal,
			Approval:      ApprovalAsk,
			ToolPreset:    ToolPresetPersonal,
			ProviderKind:  ProviderOpenAI,
			MaxIterations: DefaultSafeMaxIterations,
			Note:          "推荐：只读免问，其余（含危险命令）问我",
		},
		ProfileLocal: {
			Name:          ProfileLocal,
			Approval:      ApprovalAuto,
			ToolPreset:    ToolPresetPersonal,
			ProviderKind:  ProviderOllama,
			MaxIterations: DefaultSafeMaxIterations,
			Note:          "本地模型：不问人（全部放行）；默认不联网",
		},
	}
}

// 内置档名。
const (
	ProfileCareful  = "careful"
	ProfilePersonal = "personal"
	ProfileLocal    = "local"
	// ToolPresetPersonal 是个人客户端的默认工具集（见 tools/presets）。
	ToolPresetPersonal = "personal"
)

// LookupProfile 按名字取档。空名字返回 (Profile{}, false)。
func LookupProfile(name string) (Profile, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return Profile{}, false
	}
	p, ok := Profiles()[name]
	return p, ok
}

// applyProfile 把档里的建议值填进"宿主没设置"的字段。
func applyProfile(cfg *Config) error {
	p, ok := LookupProfile(cfg.Profile)
	if !ok {
		if strings.TrimSpace(cfg.Profile) == "" {
			return nil
		}
		return fmt.Errorf("kit: unknown profile %q (want one of %s/%s/%s)",
			cfg.Profile, ProfileCareful, ProfilePersonal, ProfileLocal)
	}
	if cfg.Approval == "" {
		cfg.Approval = p.Approval
	}
	if strings.TrimSpace(cfg.ToolPreset) == "" {
		cfg.ToolPreset = p.ToolPreset
	}
	if cfg.ProviderKind == "" {
		cfg.ProviderKind = p.ProviderKind
	}
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = p.MaxIterations
	}
	return nil
}

// BuildProvider 按 kind 构造 provider。宿主传了 Config.Provider 时不会走到这里。
//
// BaseURL/APIKey/APIKeyEnv/Model 四个字段对所有 kind 语义一致；本地 kind 的
// 默认地址由各自包提供，BaseURL 为空时用它。
func BuildProvider(kind ProviderKind, cfg Config) (Provider, error) {
	switch ProviderKind(strings.ToLower(strings.TrimSpace(string(kind)))) {
	case "", ProviderOpenAI:
		return openai.New(openai.Config{
			BaseURL:      cfg.BaseURL,
			APIKey:       cfg.APIKey,
			APIKeyEnv:    cfg.APIKeyEnv,
			DefaultModel: cfg.Model,
		})
	case ProviderOllama:
		return ollama.New(ollama.Config{
			BaseURL:      cfg.BaseURL,
			APIKey:       cfg.APIKey,
			DefaultModel: cfg.Model,
		})
	case ProviderLMStudio:
		return lmstudio.New(lmstudio.Config{
			BaseURL:      cfg.BaseURL,
			APIKey:       cfg.APIKey,
			DefaultModel: cfg.Model,
		})
	case ProviderLocal:
		return local.New(local.Config{
			BaseURL:      cfg.BaseURL,
			APIKey:       cfg.APIKey,
			APIKeyEnv:    cfg.APIKeyEnv,
			DefaultModel: cfg.Model,
		})
	default:
		return nil, fmt.Errorf("kit: unknown provider kind %q (want openai/ollama/lmstudio/local)", kind)
	}
}

// AvailableModels 列出某个 provider 能用的模型——用于"启动即用"的选择器：
// 本地服务上 `Models()[0].ID` 直接可以作为 Config.Model。
//
// provider 没有列模型能力（例如某些网关）时返回 (nil, false, nil)，宿主可以
// 退回手填模型名。
func AvailableModels(ctx context.Context, kind ProviderKind, cfg Config) ([]local.ModelInfo, bool, error) {
	p, err := BuildProvider(kind, cfg)
	if err != nil {
		return nil, false, err
	}
	lister, ok := p.(local.ModelLister)
	if !ok {
		return nil, false, nil
	}
	models, err := lister.Models(ctx)
	if err != nil {
		return nil, true, err
	}
	return models, true, nil
}

// 编译期确认 openai.Provider 满足 rig.Provider。
var _ rig.Provider = (*openai.Provider)(nil)
