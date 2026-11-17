package kit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
	"github.com/zeokeel/zeokeel-rig/costmeter"
	eventspkg "github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/instructions"
	"github.com/zeokeel/zeokeel-rig/provider/openai"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/secrets"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/session/store"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/presets"
	"github.com/zeokeel/zeokeel-rig/tools/ptc"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxgrant"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxstatus"
	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// DefaultRoot 是 Config.Root 为空时使用的工作区根目录。
const DefaultRoot = "."

// DefaultMaxIterations 是 Config.MaxIterations 为零或负数时使用的模型轮数上限
// 默认值：0，表示不限制轮数（unlimited）。它与 agent.DefaultMaxIterations 相同。
const DefaultMaxIterations = agent.DefaultMaxIterations

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
//   - MaxIterations：0（不限制模型↔工具轮数；需要兜底上限时显式配置 >0）。
type Config struct {
	// Model 是发送给 provider 的模型名。必填。
	Model string
	// SystemPrompt 非空时会在每次模型请求前加上。
	SystemPrompt string
	// MaxIterations 限制模型↔工具轮数；>0 是显式上限，零或负数表示不限制轮数
	// （DefaultMaxIterations = 0，unlimited）。
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
	// History 覆盖默认的内存历史。nil History 表示新建 session.NewMemory()
	// （或在 SessionStoreDir 非空时，使用该目录下的持久化会话）。
	History History
	// SessionStoreDir 非空时启用内置持久化：kit 打开一个 session/store 目录，并把
	// History 挂成其中的默认会话。要求 History 为 nil（两者同时设置会报错）。
	// 注意：同一目录只能由**一个** store 实例写（store 不做跨进程/跨实例加锁；
	// 两个实例同时写同一会话会互相覆盖索引）。重启时先释放前一个实例。
	SessionStoreDir string

	// SessionStore 让宿主自己持有 store 实例（与 SessionStoreDir/History 互斥）：
	// 个人客户端的"最近会话"侧栏需要用**同一个实例** `List`/`Load`（单目录单实例），
	// 所以要么由宿主传进来，要么用 Agent.SessionStore() 取 kit 建的那个。
	SessionStore *store.Dir

	// Asker 是"审批模式（approval=ask）下真正去问人"的通道。为 nil 时 kit 使用
	// gate.PersonalAskWithoutAsker()：只读放行、危险拒绝、其余拒绝并说明
	// "没有可用的审批通道"——而不是静默拒绝或假装 approval=never。
	Asker rig.Asker

	// Meter 非空时按 EventUsage 累计本次会话的花费（`costmeter`）。个人用户
	// 自己付钱，"这个会话花了多少"是产品能力而不是可观测性装饰；UI 读
	// `Meter.Session().CostUSD` 即可。
	Meter *costmeter.Meter

	// DisableShellAccess 关闭 shell（bash/pwsh）执行：不设置"允许任意程序"的默认，
	// 且没有显式 CommandAllowlist 时所有命令都会被策略拒绝。默认 false（可用）。
	DisableShellAccess bool
	// Instructions 非空时把 AGENTS.md 层级文档拼进系统提示词末尾（在 Environment
	// 之前）。只应 Load 一次：system prompt 是请求前缀，任何字节变化都会打掉
	// provider 的 prompt 缓存。
	Instructions *instructions.Set
	// Environment 非空时把 <environment> 块拼进系统提示词（cwd/平台/日期/git）。
	Environment *instructions.Environment
	// Redactor 是**可选**的凭据脱敏器，默认 nil（不脱敏）。
	//
	// 个人客户端的定位是"单用户、所有数据都是用户自己的"，因此 kit **不再**在
	// 工具边界做输出脱敏：模型看到的工具结果就是工具返回的原文。需要脱敏的只剩
	// 一条路径——用户主动导出/上报日志（见 secrets.RedactForExport）。
	//
	// 显式设置后：它被传给 agent.NewWithRedactor（agent 内部的入参脱敏）与
	// EventWriter 的事件流（日志通道），**不会**再装饰工具返回内容。nil 时零开销、
	// 明文直通。
	Redactor *secrets.Redactor
	// GitUndo 打开"每回合自动快照 + 撤销按钮"：每个 Run 开始前用 git 拍一份隐藏快照
	// （`git stash create` 语义，不动 HEAD），用户可以通过 undo 工具/UI 回退该回合的
	// 文件改动。默认 **false**：快照不是免费的（每回合要哈希变化的文件），而且它只对
	// git 工作区有意义。
	//
	// 工作区不是 git 仓库时这个开关会被静默忽略（undo 工具不会出现）。
	GitUndo bool

	// UndoTracker 是宿主自建的回合快照跟踪器。
	//
	// 已废弃：优先用 GitUndo。保留它是为了兼容既有宿主（同一个 tracker 可以被多个
	// agent 共享，也能自定义 Options）。
	UndoTracker *undo.Tracker
	// EventWriter 非空时把 agent 事件桥接成 events.Envelope（NDJSON）写入它。
	// 一个 Run 一条流；Emitter 由 kit 内部创建，seq 在同一 writer 上连续。
	EventWriter io.Writer

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
	// ProtectWorkspaceMetadata 已废弃，保留仅为源码兼容：等价于保持默认行为
	// （保护默认开启）。要关闭保护请用 DisableProtection=true。
	//
	// Deprecated: 保护路径现在默认开启，本字段不再改变任何行为。设 true 没有额外
	// 效果（本来就开）；设 false **不会**关闭保护——要关闭请用
	// DisableProtection=true。
	ProtectWorkspaceMetadata bool
	// ProtectPaths 是"默认 true"的显式表达：设为 true 与不设等价，都是保护开启。
	// 因为 Go 布尔零值是 false，这个字段**无法**表达"关"，关闭只有
	// DisableProtection=true 一个入口（它优先级最高）。保留本字段是为了让宿主
	// 在配置里把意图写出来，并给"未来默认值可能变化"留一个显式锚点。
	//
	// 被保护的位置 = 每个策略根下的工作区元数据（.git 含 worktree gitdir 指针、
	// .aws、.codex）+ 用户主目录下**存在**的凭据位置（~/.ssh、~/.aws、
	// ~/.config/gh、~/.netrc）。它们即使在可写根内也必须保持只读。
	//
	// 这是**防误伤**，不是沙箱：它约束的是文件工具与（挂接 Runtime 时的）执行
	// 工具的落点，不改变进程权限，也不阻止用户自己用外部编辑器修改这些路径。
	//
	// 只在 Policy 为 nil（kit 自己构造策略）时生效；宿主显式提供 Policy 时保护
	// 策略归宿主所有，kit 不追加。
	ProtectPaths bool
	// DisableProtection 关闭上面的默认保护（一个位置都不追加）。默认 false
	// （保护开启）。给"我就要 agent 能改 .git"的高级用户一个明确出口。
	DisableProtection bool
	// SandboxDenialSink 接收每一次被识别的沙箱拒绝（结构化 sandbox.Denial）。
	// 它挂在 policy 上，一次配置覆盖所有执行工具；回调必须并发安全。
	SandboxDenialSink func(sandbox.Denial)
	// SandboxGrants 在 Tools 为 nil（使用默认工具集）时追加
	// request_sandbox_access 与 sandbox_status 两个工具：前者让模型为"某个工具
	// 自己的凭据/缓存目录"申请会话级可写授权，批准后立即对所有工具生效；后者是
	// 只读诊断工具，报告当前可写面、已批准的授权与剩余预算。授权次数受
	// sandbox.WithGrantBudget 限制（默认 sandbox.DefaultGrantBudget，5 次）。
	// 默认 false，保持现有行为完全不变；Tools 非 nil 时不追加（自定义工具集由
	// 宿主自行装配）。
	//
	// 安全默认：开启后内置门禁仍然只放行只读工具（defaultAllowedTools），
	// request_sandbox_access 与 sandbox_status 都**不在**默认允许列表里，因此默认
	// 配置下它们的调用会被门禁拒绝并回传给模型。放宽沙箱必须由宿主显式配置审批
	// （Approval 换为 ApprovalAuto、或提供 Ask/Review/自定义 Gate）——这是刻意的
	// 人工边界。
	//
	// 注意：若同时设置 ToolPreset，预设按名字过滤工具集，而预设的名字表来自
	// tools/catalog.Static——这两个工具尚未登记进该账本，因此会被预设过滤掉。
	// 需要两者并用时，应先把工具名补进 tools/catalog.Static 与 preset 名字表。
	SandboxGrants bool
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

// workspaceRoot 选一个稳定的"工作区显示根"：优先策略主根（已规范化），
// 否则退回 kit 的 root。用它渲染 AGENTS.md 的相对标题，避免 cfg.Root 为空时
// 退化成"当前进程 cwd 的绝对路径"（那会让 system prompt 随启动目录漂移）。
func workspaceRoot(policy *sandbox.Policy, fallback string) string {
	if policy != nil && len(policy.Roots()) > 0 {
		return policy.Roots()[0]
	}
	return fallback
}

// withoutTool 从工具集中移除指定名字（不存在时原样返回）。
func withoutTool(tools []rig.Tool, name string) []rig.Tool {
	out := make([]rig.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			out = append(out, tool) // nil 留给 agent 的校验报错
			continue
		}
		if tool.Name() == name {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// appendUniqueTools 追加工具集里还没有的工具（按名字去重）。nil 工具被跳过：
// 它们由 agent.New 报错，kit 不代替它做校验。用于把可选工具并进默认工具集时
// 保持幂等——同名的现有工具优先（它可能来自宿主配置的 tools/full.All）。
func appendUniqueTools(toolSet []rig.Tool, tools ...rig.Tool) []rig.Tool {
	seen := make(map[string]struct{}, len(toolSet)+len(tools))
	for _, tool := range toolSet {
		if tool == nil {
			continue
		}
		seen[tool.Name()] = struct{}{}
	}
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := tool.Name()
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		toolSet = append(toolSet, tool)
	}
	return toolSet
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
	// <=0 表示不限制轮数（unbounded）；负值归一化为 0。
	maxIterations := max(cfg.MaxIterations, 0)

	undoTracker := cfg.UndoTracker
	if undoTracker == nil && cfg.GitUndo {
		// 只有 git 工作区才有"回合快照"可言；不是仓库时安静地不提供撤销，
		// 而不是每回合都往事件流里打一条失败。
		if isRepo, err := undo.IsGitRepo(context.Background(), root); err == nil && isRepo {
			undoTracker = undo.NewTracker(root, undo.Options{})
		}
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
		opts := []sandbox.Option{
			sandbox.WithRoots(root),
			sandbox.WithMode(mode),
			sandbox.WithNetwork(cfg.AllowNetwork),
		}
		if len(cfg.CommandAllowlist) > 0 {
			opts = append(opts, sandbox.WithCommandAllowlist(cfg.CommandAllowlist...))
		} else if !cfg.DisableShellAccess {
			// 个人客户端默认：不限制命令名（否则 workspace-write 下 bash/pwsh
			// 一条命令都跑不了，"帮我跑测试"直接失效）。危险性由
			// gate.CommandRiskGate 的命令分类 + 人工审批判定，而不是靠一张
			// 永远不全的程序白名单。
			opts = append(opts, sandbox.WithAnyCommandAllowed(true))
		}
		policy = sandbox.New(opts...)
	}

	if cfg.SandboxDenialSink != nil {
		policy.SetDenialSink(cfg.SandboxDenialSink)
	}
	if cfg.Policy == nil && protectionEnabled(cfg) {
		addDefaultProtection(policy)
	}
	toolSet := cfg.Tools
	if toolSet == nil {
		toolSet = full.All(full.AllOptions{
			Policy:        policy,
			Root:          root,
			SandboxRunner: cfg.SandboxRunner,
			UndoTracker:   undoTracker,
		})
		if cfg.SandboxGrants {
			// 授权工具与状态工具共享同一个 policy：批准后立即对所有工具生效，
			// 状态工具读到的是同一份实时视图。按名字去重：若默认工具集以后自己
			// 登记了这两个工具（tools/full.All），这里不会再追加一份重名工具
			// （agent.New 会拒绝重名）。
			toolSet = appendUniqueTools(toolSet,
				sandboxgrant.New(policy),
				sandboxstatus.New(policy),
			)
		}
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
			// 自动模式**不等于** AllowAll：危险命令（rm -rf /、mkfs、curl|sh…）
			// 仍然被拒，只读免审批，其余不问人。个人客户端切 auto 的动机是
			// "别老问我"，不是"什么都别管"。
			gateImpl = gate.PersonalAuto()
		case ApprovalNever:
			gateImpl = gate.PersonalNever()
		default:
			// 审批模式：只读工具与只读命令免问，危险命令直接拒（理由回模型），
			// 其余交给人工。宿主没接 Asker 时用"没有审批通道"的兜底，而不是
			// 假装 approval=never（那会让模型以为用户主动禁止了一切）。
			if cfg.Asker != nil {
				opts := []gate.AskOption{
					gate.WithAskTimeout(10 * time.Minute),
					gate.WithAskPrompt(gate.SandboxGrantPrompt),
				}
				gateImpl = gate.PersonalAsk(gate.Ask(cfg.Asker, opts...))
			} else {
				gateImpl = gate.PersonalAskWithoutAsker()
			}
		}
	}

	if cfg.PTC {
		toolSet = append(toolSet, ptc.New(ptc.Config{
			Tools:  slices.Clone(toolSet),
			Gate:   gateImpl,
			Root:   root,
			Runner: cfg.SandboxRunner,
			Policy: policy,
		}))
	}

	history := cfg.History
	var storeDir *store.Dir
	if cfg.SessionStore != nil {
		if history != nil || cfg.SessionStoreDir != "" {
			return nil, errors.New("kit: SessionStore, History and SessionStoreDir are mutually exclusive")
		}
		storeDir = cfg.SessionStore
		history = storeDir.History(context.Background(), "default")
	}
	if cfg.SessionStoreDir != "" {
		if history != nil {
			return nil, errors.New("kit: History and SessionStoreDir are mutually exclusive")
		}
		st, err := store.NewDir(cfg.SessionStoreDir)
		if err != nil {
			return nil, fmt.Errorf("kit: session store: %w", err)
		}
		storeDir = st
		history = st.History(context.Background(), "default")
	}
	if history == nil {
		history = session.NewMemory()
	}

	if undoTracker == nil {
		// 没有回合快照就不该把 undo 暴露给模型：它只会返回"没有可用快照"，
		// 白白浪费一个回合。宿主配置 GitUndo 或 UndoTracker 后，full.All 会自动
		// 挂载它。
		toolSet = withoutTool(toolSet, "undo")
	}
	promptRoot := workspaceRoot(policy, root)
	systemPrompt := assembleSystemPrompt(cfg.SystemPrompt, cfg.Environment, cfg.Instructions, promptRoot)

	spec := rig.AgentSpec{
		Provider:        provider,
		Model:           model,
		SystemPrompt:    systemPrompt,
		MaxIterations:   maxIterations,
		Tools:           toolSet,
		Gate:            gateImpl,
		History:         history,
		ContextBudget:   cfg.ContextBudget,
		Compactor:       cfg.Compactor,
		Spill:           cfg.Spill,
		CheckpointStore: cfg.CheckpointStore,
	}
	// 只有宿主显式给了 Redactor 才走带脱敏的装配；默认路径用 agent.New，
	// 不让"裸 Agent"客户端多背一层包装。
	var underlying rig.Agent
	var err2 error
	if cfg.Redactor != nil {
		underlying, err2 = agent.NewWithRedactor(spec, cfg.Redactor)
	} else {
		underlying, err2 = agent.New(spec)
	}
	if err2 != nil {
		return nil, fmt.Errorf("kit: %w", err2)
	}
	return &Agent{
		agent:         underlying,
		compactor:     cfg.Compactor,
		contextBudget: cfg.ContextBudget,
		history:       history,
		undo:          undoTracker,
		events:        newEventBridgeFor(cfg.EventWriter, cfg.Redactor, cfg.Meter, cfg.Model),
		meter:         cfg.Meter,
		model:         cfg.Model,
		store:         storeDir,
		policy:        policy,
	}, nil
}

// Agent 是由 New 装配好的、可直接运行的 agent。它实现 rig.Agent
// （RunRequest/RunResult 是契约类型的别名），因此可以传给任何期望该接口的地方。
type Agent struct {
	agent         rig.Agent
	compactor     rig.Compactor
	contextBudget int
	history       rig.History
	undo          *undo.Tracker
	events        *eventBridge
	policy        *sandbox.Policy
	meter         *costmeter.Meter
	model         string
	store         *store.Dir
}

// SessionStore 返回本 agent 使用的会话存储（未启用持久化时返回 nil）。
//
// "最近会话"侧栏必须用**这个实例**做 List/Load：store 目录是单实例的，
// 宿主自己再 NewDir 同一个目录会导致两边列表不完整。
func (a *Agent) SessionStore() *store.Dir {
	if a == nil {
		return nil
	}
	return a.store
}

// Policy 返回装配时使用的沙箱策略（kit 自建或宿主传入的同一个实例）。
//
// 它是**只读视图**用途：诊断（sandbox_status 同源）、给宿主自己的工具复用同一
// 份策略（例如用 tools/patch 加 patch.WithPolicy(agent.Policy())）。运行时修改
// 它会同时影响已装配的工具，那属于宿主的责任。
func (a *Agent) Policy() *sandbox.Policy {
	if a == nil {
		return nil
	}
	return a.policy
}

var _ rig.Agent = (*Agent)(nil)

// UndoTracker 返回本 agent 使用的回合快照跟踪器（未启用撤销时为 nil）。
//
// 面向用户的"撤销按钮"通过它接到 UI：
//
//	preview, _ := agent.UndoTracker().Changes(0, "")   // 预览（dry-run）
//	applied, err := agent.UndoTracker().Revert(0)      // 撤销最近一回合
//
// 返回 nil 表示本 agent 没有启用撤销（既没设 GitUndo，也没传 UndoTracker）。
func (a *Agent) UndoTracker() *undo.Tracker {
	if a == nil {
		return nil
	}
	return a.undo
}

// Run 实现 rig.Agent。它执行模型↔工具循环的一个轮次。
func (a *Agent) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if a.undo != nil {
		// 快照必须发生在**任何工具执行之前**：否则该回合最早的改动不可回退。
		// Begin 失败只记事件、不影响回合（undo 工具会报"没有可用快照"）。
		if _, err := a.undo.BeginContext(ctx); err != nil && a.events != nil {
			a.events.emit(eventspkg.KindNotice, noticePayload{Message: "undo snapshot failed: " + err.Error()})
		}
	}
	if a.events != nil {
		if a.meter != nil {
			// 模型优先级：RunRequest 覆盖 > Config 默认模型。
			model := strings.TrimSpace(req.Model)
			if model == "" {
				model = a.model
			}
			a.events.setMeter(a.meter, model)
		}
		req.Emit = a.events.wrap(req.Emit)
		a.events.emit(eventspkg.KindRunStart, eventspkg.RunStart{})
	}
	result, runErr := a.agent.Run(ctx, req)
	if a.events != nil {
		payload := eventspkg.RunEnd{Turns: len(result.Messages)}
		if runErr != nil {
			payload.Err = a.events.redact(runErr.Error())
		}
		payload.PromptTokens = result.Usage.PromptTokens
		payload.CompletionTokens = result.Usage.CompletionTokens
		a.events.emit(eventspkg.KindRunEnd, payload)
	}
	return result, runErr
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

// --- 默认保护路径（个人客户端的软护栏） ------------------------------------

// protectionEnabled 报告是否追加默认保护路径。
//
// 默认**开启**：Config 的零值就是"保护工作区元数据与主目录凭据位置"。Go 的布尔
// 零值是 false，所以"显式关闭"必须用 DisableProtection；ProtectPaths=true 只是
// 把默认行为写清楚（表达意图，不改变结果）。
func protectionEnabled(cfg Config) bool { return !cfg.DisableProtection }

// addDefaultProtection 给策略追加默认保护位置：
//
//   - 每个策略根下的工作区元数据（.git 含 worktree gitdir 指针、.aws、.codex）；
//   - 用户主目录下**存在**的凭据位置（~/.ssh、~/.aws、~/.config/gh、~/.netrc）。
//
// 不存在的主目录路径不追加：保护一个幻影路径只会让 sandbox_status 的诊断噪声变大。
// 工作区元数据相反——即使当前不存在也保留，因为会话中途 `git init` 是常态。
//
// 这是防误伤，不是沙箱：它只约束工具落点（文件工具的路径校验、apply_patch、
// 以及挂接 sandbox/osrunner 时的 OS profile），不改变进程权限。
func addDefaultProtection(policy *sandbox.Policy) {
	if policy == nil {
		return
	}
	for _, root := range policy.Roots() {
		policy.AddProtectedPaths(sandbox.WorkspaceMetadataPaths(root)...)
	}
	policy.AddProtectedPaths(HomeSecretPaths()...)
}

// HomeSecretPaths 返回用户主目录下**存在**的凭据位置（.ssh/.aws/.config/gh/
// .netrc）。它给 kit 的默认保护用，也导出给宿主拼自己的策略。
//
// 取不到主目录（没有 HOME/USERPROFILE）时返回 nil：宁可少保护也不要猜错路径。
func HomeSecretPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	home = strings.TrimSpace(home)
	if home == "" {
		return nil
	}
	candidates := []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".config", "gh"),
		filepath.Join(home, ".netrc"),
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if _, statErr := os.Lstat(candidate); statErr == nil {
			out = append(out, candidate)
		}
	}
	return out
}
