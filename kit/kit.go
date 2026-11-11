package kit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/provider/openai"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/presets"
	"github.com/zeokeel/zeokeel-rig/tools/ptc"
)

// DefaultRoot 是 Config.Root 为空时使用的工作区根目录。
const DefaultRoot = "."

// DefaultMaxIterations 是 Config.MaxIterations 为零或负数时使用的模型轮数上限。
// 它与 agent.DefaultMaxIterations 相同。
const DefaultMaxIterations = agent.DefaultMaxIterations

// defaultAllowedTools 是默认审批门禁放行的只读工具集。其余工具调用都会被拒绝并
// 报告给模型；需要写文件或执行 shell 的宿主应显式配置 Config.Gate。
var defaultAllowedTools = []string{"read_file", "list_dir", "glob", "grep"}

// ApprovalMode 是 Config.Gate 为 nil 时使用的内置审批策略。
type ApprovalMode string

const (
	// ApprovalAsk 是失败即拒绝（fail-safe）的默认值：只放行只读工具集，其余调用
	// 一律拒绝。想要交互式询问的宿主应自行提供 Config.Gate（它可以阻塞等待用户
	// 输入）。
	ApprovalAsk ApprovalMode = "ask"
	// ApprovalAuto 放行所有工具调用。应与部署所信任的权限 Mode 搭配使用；它是
	// “自动批准”预设。
	ApprovalAuto ApprovalMode = "auto"
	// ApprovalNever 拒绝所有工具调用。
	ApprovalNever ApprovalMode = "never"
)

// Config 配置 New。Model 是唯一必填字段，其余字段都有文档化的默认值，因此
// “零值 Config + 一个模型”就是可用且刻意保守的 agent。
//
// 默认值：
//   - Provider：用 BaseURL/APIKey/APIKeyEnv 构造的 openai.Provider，
//     DefaultModel 设为 Model。构造本身不需要 API key；没有 key 时第一次 Run 会
//     报告 provider 错误。
//   - Policy：sandbox.New(WithRoots(Root), WithMode(Mode),
//     WithNetwork(AllowNetwork), WithCommandAllowlist(CommandAllowlist...))。
//     默认没有 shell 允许列表，因此 shell 执行会被策略以及门禁双重拒绝。
//   - Tools：使用该 policy 与 Root 的 tools/full.All（完整内置工具集，包含受沙箱
//     约束的文件工具与 shell）。
//   - Gate：由 Approval 决定："ask"（默认）放行 read_file、list_dir、glob 与
//     grep，拒绝其余；"auto" 放行所有调用；"never" 拒绝所有调用。
//     非 nil 的 Config.Gate 始终优先。
//   - History：session.NewMemory()（内存实现，按 SessionID 隔离）。
//   - Root："."（构造时进程的工作目录）。
//   - MaxIterations：20。
type Config struct {
	// Model 是发送给 provider 的模型名。必填。
	Model string
	// SystemPrompt 非空时会在每次模型请求前加上。
	SystemPrompt string
	// MaxIterations 限制模型↔工具轮数；零或负数表示 DefaultMaxIterations（20）。
	MaxIterations int
	// ContextBudget 是估算的 prompt token 预算；>0 且 Compactor 非 nil 时启用
	// 自动上下文压缩。常用实现见 session/compact。
	ContextBudget int
	// Compactor 压缩超预算对话；nil 表示不压缩。
	Compactor rig.Compactor
	// Spill 把过大的工具结果落盘；nil 表示只按工具自身限制截断。
	Spill *rig.SpillPolicy
	// CheckpointStore 在回合边界保存恢复检查点；nil 表示关闭。
	CheckpointStore rig.CheckpointStore

	// ReasoningEffort 是默认的模型推理强度（""、"low"、"medium"、"high"），
	// 仅在使用默认 OpenAI 兼容 provider 时生效；自定义 Provider 由宿主自行处理。
	ReasoningEffort string
	// BaseURL 覆盖 chat-completions API 根地址，例如
	// "https://api.deepseek.com/v1"。为空时使用 OpenAI 默认地址。
	BaseURL string
	// APIKey 是 bearer token。为空时从环境变量 APIKeyEnv（默认
	// OPENAI_API_KEY）读取。
	APIKey string
	// APIKeyEnv 指定保存 API key 的环境变量名。为空表示 OPENAI_API_KEY。
	APIKeyEnv string
	// Provider 覆盖默认的 OpenAI 兼容 provider。非 nil 时 BaseURL、APIKey 与
	// APIKeyEnv 被忽略。
	Provider Provider

	// Tools 覆盖默认工具集。nil 切片表示使用 tools/full 的完整默认集合；
	// 非 nil 的空切片表示不使用任何工具。
	Tools []Tool
	// ToolPreset 按预设过滤最终工具集（minimal/standard/all）；空字符串表示
	// 不过滤。未知预设返回错误。预设只做工具子集，不改变 system prompt。
	ToolPreset string
	// Gate 覆盖默认的只读审批门禁。
	Gate Gate
	// History 覆盖默认的内存历史。nil History 表示新建 session.NewMemory()。
	History History

	// Root 是沙箱策略与根感知工具使用的工作区根目录。为空表示 DefaultRoot
	// （"."）。
	Root string
	// Policy 完全覆盖沙箱策略。非 nil 时，Root 仍用于定位根感知的报告类工具，但
	// Mode、AllowNetwork 与 CommandAllowlist 被忽略（这些决定归传入的策略所有）。
	Policy *sandbox.Policy
	// SandboxRunner 是可选的 OS 沙箱 runner（见 sandbox/osrunner）。非 nil 时
	// 默认工具集的 bash/pwsh、jobs、terminal 与 worktree 会在 spawn 前经
	// Runner.Confine 包装；失败即拒绝执行。nil 保持默认行为。仅在 Tools 为 nil
	// （使用默认 full.All 工具集）时生效。
	SandboxRunner sandbox.Runner
	// Mode 在 Policy 为 nil 时选择权限模式：sandbox.ModeWorkspaceWrite（默认）、
	// sandbox.ModeReadOnly 或 sandbox.ModeDangerFullAccess。只读模式拒绝文件修改
	// 与 shell 执行；danger-full-access 移除工作区/允许列表围栏，只应与显式或
	// 自动审批搭配。
	Mode sandbox.Mode
	// PTC 启用模型侧的 run_code 工具：模型编写的程序在系统已安装的 Python 3 或
	// Node.js 运行时中执行，并通过已配置的审批门禁调用其他工具。默认关闭；内层
	// 调用与普通工具调用一样走同一个 Gate。
	PTC bool
	// Approval 在 Gate 为 nil 时选择内置门禁如何处理工具调用：ApprovalAsk
	// （默认）只放行只读工具集、拒绝其余（需要询问的宿主自行安装 Gate），
	// ApprovalAuto 放行所有调用，ApprovalNever 拒绝所有调用。
	Approval ApprovalMode
	// AllowNetwork 在 Policy 为 nil 时启用对外网络访问。即使启用，沙箱仍会拒绝
	// loopback/私有/链路本地/元数据等目标。默认 false。
	AllowNetwork bool
	// CommandAllowlist 在 Policy 为 nil 时允许列出的 shell 命令基名。默认为空：
	// shell 执行被拒绝。
	CommandAllowlist []string
}

// New 根据 cfg 装配一个可直接运行的 Agent。
//
// cfg.Model 为空时返回包装 rig.ErrModelRequired 的错误；无法构造默认 provider 时
// 返回 provider/配置错误；自定义 Tool 集合非法（nil 工具、空名字或重名）时返回
// 校验错误。kit 自行构造沙箱策略时，工作区根目录不存在同样是错误。
func New(cfg Config) (*Agent, error) {
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("kit: %w", rig.ErrModelRequired)
	}
	root := strings.TrimSpace(cfg.Root)
	if root == "" {
		root = DefaultRoot
	}
	maxIterations := cfg.MaxIterations
	if maxIterations <= 0 {
		maxIterations = DefaultMaxIterations
	}

	provider := cfg.Provider
	if provider == nil {
		defaultProvider, err := openai.New(openai.Config{
			BaseURL:                cfg.BaseURL,
			APIKey:                 cfg.APIKey,
			APIKeyEnv:              cfg.APIKeyEnv,
			DefaultModel:           model,
			DefaultReasoningEffort: cfg.ReasoningEffort,
		})
		if err != nil {
			return nil, fmt.Errorf("kit: provider: %w", err)
		}
		provider = defaultProvider
	}

	mode := cfg.Mode
	if mode == "" {
		mode = sandbox.ModeWorkspaceWrite
	}
	switch mode {
	case sandbox.ModeWorkspaceWrite, sandbox.ModeReadOnly, sandbox.ModeDangerFullAccess:
	default:
		return nil, fmt.Errorf("kit: unknown permission mode %q", mode)
	}
	approval := cfg.Approval
	if approval == "" {
		approval = ApprovalAsk
	}
	switch approval {
	case ApprovalAsk, ApprovalAuto, ApprovalNever:
	default:
		return nil, fmt.Errorf("kit: unknown approval mode %q", approval)
	}

	policy := cfg.Policy
	if policy == nil {
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("kit: workspace root %q: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("kit: workspace root %q is not a directory", root)
		}
		policy = sandbox.New(
			sandbox.WithRoots(root),
			sandbox.WithMode(mode),
			sandbox.WithNetwork(cfg.AllowNetwork),
			sandbox.WithCommandAllowlist(cfg.CommandAllowlist...),
		)
	}

	toolSet := cfg.Tools
	if toolSet == nil {
		toolSet = full.All(full.AllOptions{Policy: policy, Root: root, SandboxRunner: cfg.SandboxRunner})
	}
	if preset := strings.TrimSpace(cfg.ToolPreset); preset != "" {
		filtered, err := presets.Filter(preset, toolSet)
		if err != nil {
			return nil, fmt.Errorf("kit: %w", err)
		}
		toolSet = filtered
	}

	gateImpl := cfg.Gate
	if gateImpl == nil {
		switch approval {
		case ApprovalAuto:
			gateImpl = gate.AllowAll()
		case ApprovalNever:
			gateImpl = gate.DenyAll()
		default:
			gateImpl = gate.AllowTools(defaultAllowedTools...)
		}
	}

	if cfg.PTC {
		toolSet = append(toolSet, ptc.New(ptc.Config{
			Tools: slices.Clone(toolSet),
			Gate:  gateImpl,
			Root:  root,
		}))
	}

	history := cfg.History
	if history == nil {
		history = session.NewMemory()
	}

	underlying, err := agent.New(rig.AgentSpec{
		Provider:        provider,
		Model:           model,
		SystemPrompt:    cfg.SystemPrompt,
		MaxIterations:   maxIterations,
		Tools:           toolSet,
		Gate:            gateImpl,
		History:         history,
		ContextBudget:   cfg.ContextBudget,
		Compactor:       cfg.Compactor,
		Spill:           cfg.Spill,
		CheckpointStore: cfg.CheckpointStore,
	})
	if err != nil {
		return nil, fmt.Errorf("kit: %w", err)
	}
	return &Agent{agent: underlying, compactor: cfg.Compactor, contextBudget: cfg.ContextBudget, history: history}, nil
}

// Agent 是由 New 装配好的、可直接运行的 agent。它实现 rig.Agent
// （RunRequest/RunResult 是契约类型的别名），因此可以传给任何期望该接口的地方。
type Agent struct {
	agent         rig.Agent
	compactor     rig.Compactor
	contextBudget int
	history       rig.History
}

var _ rig.Agent = (*Agent)(nil)

// Run 实现 rig.Agent。它执行模型↔工具循环的一个轮次。
func (a *Agent) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	return a.agent.Run(ctx, req)
}

// CompactNow 立即压缩 sessionID 的历史并持久化替换结果（手动 /compact 语义）。
// 需要 Config.Compactor 非 nil、ContextBudget > 0，且 Config.History 实现
// rig.HistoryReplacer（默认的 session.Memory 已实现）；否则返回可读错误。
// 返回是否实际写入了压缩后的历史。
func (a *Agent) CompactNow(ctx context.Context, sessionID string) (bool, error) {
	if a.compactor == nil || a.contextBudget <= 0 {
		return false, errors.New("kit: manual compaction requires ContextBudget and Compactor")
	}
	replacer, ok := a.history.(rig.HistoryReplacer)
	if !ok {
		return false, errors.New("kit: configured History does not implement rig.HistoryReplacer")
	}
	messages, err := a.history.Load(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("kit: load history: %w", err)
	}
	if len(messages) == 0 {
		return false, nil
	}
	compacted, err := a.compactor.Compact(ctx, sessionID, messages, a.contextBudget)
	if err != nil {
		return false, fmt.Errorf("kit: compact history: %w", err)
	}
	if compacted == nil {
		return false, nil
	}
	if err := replacer.Replace(ctx, sessionID, compacted); err != nil {
		return false, fmt.Errorf("kit: replace history: %w", err)
	}
	return true, nil
}

// RunText 在 sessionID 中运行一个 prompt，并返回拼接后的助手文本。emit 可以为
// nil；非 nil 时它会按顺序观察每个事件（与 RunRequest.Emit 回调收到的事件相同）。
// 出错时返回的文本是失败前已发出的部分文本，RunResult 是部分结果。
func (a *Agent) RunText(ctx context.Context, sessionID, prompt string, emit func(Event)) (string, RunResult, error) {
	var text strings.Builder
	req := RunRequest{
		SessionID: sessionID,
		Input:     []Message{NewMessage(RoleUser, prompt)},
		Emit: func(event Event) {
			if event.Type == EventTextDelta {
				text.WriteString(event.Text)
			}
			if emit != nil {
				emit(event)
			}
		},
	}
	result, err := a.Run(ctx, req)
	return text.String(), result, err
}
