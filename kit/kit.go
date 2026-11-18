package kit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
	"github.com/zeokeel/zeokeel-rig/costmeter"
	eventspkg "github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/instructions"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/secrets"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/session/compact"
	"github.com/zeokeel/zeokeel-rig/session/store"
	"github.com/zeokeel/zeokeel-rig/tools/discovery"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
	"github.com/zeokeel/zeokeel-rig/tools/presets"
	"github.com/zeokeel/zeokeel-rig/tools/ptc"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxgrant"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxstatus"
	"github.com/zeokeel/zeokeel-rig/tools/undo"
	"github.com/zeokeel/zeokeel-rig/tools/web"
)

// DefaultRoot 是 Config.Root 为空时使用的工作区根目录。
const DefaultRoot = "."

// DefaultMaxIterations 是 Config.MaxIterations 为零或负数时使用的模型轮数上限
// 默认值：0，表示不限制轮数（unlimited）。它与 agent.DefaultMaxIterations 相同。
//
// 注意：kit 在装配时把"未配置"（0）视为 DefaultSafeMaxIterations，而不是 0——
// 个人客户端的默认配置必须有总闸（见该常量的注释）。要给模型真·无限轮数，显式
// 设置 Config.MaxIterations 为负数或直接用 agent.New 完整装配路径。
const DefaultMaxIterations = agent.DefaultMaxIterations

// DefaultSafeMaxIterations 是 kit 在 Config.MaxIterations 未配置（0）时采用的
// 轮数上限。它不是"功能限制"，而是**成本护栏**：v0.19.0 起 agent 默认不限轮数，
// 模型一旦在两个工具之间来回摇摆（每次签名都不同，重复调用守卫抓不到），没有
// 总闸就是无上限烧钱。200 轮足够覆盖正常复杂任务（读 20 个文件 + 跑 5 次测试 +
// 改 3 个文件通常 30–60 轮），同时把最坏成本压到可控范围。
const DefaultSafeMaxIterations = 200

// DefaultContextBudgetRatio 是自动推导 ContextBudget 时使用的窗口占比。
const DefaultContextBudgetRatio = 0.6

// DefaultModelContextTokens 是拿不到模型上下文窗口时假定的窗口大小（128k）。
const DefaultModelContextTokens = 128_000

// MinContextBudget 是自动推导出的预算下限：小窗口模型也要能装下一份 AGENTS.md
// 与几轮工具往返。
const MinContextBudget = 16_000

// defaultContextBudget 按"模型上下文 × 0.6、下限 16k"推导预算。
func defaultContextBudget(modelContext int) int {
	if modelContext <= 0 {
		modelContext = DefaultModelContextTokens
	}
	budget := int(float64(modelContext) * DefaultContextBudgetRatio)
	if budget < MinContextBudget {
		budget = MinContextBudget
	}
	return budget
}

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
	// Profile 是"配置档"名字（careful / personal / local，见 Profiles()）。
	// 它只填**你没显式设置**的字段：Approval、ToolPreset、ProviderKind、
	// MaxIterations。个人客户端推荐 "personal"（只读免问、危险拒绝、其余问人）。
	Profile string
	// ProviderKind 指定从零构造 provider 时用哪套协议（openai / ollama /
	// lmstudio / local）。Config.Provider 非 nil 时忽略本字段。
	ProviderKind ProviderKind
	// Model 是发送给 provider 的模型名。必填。
	Model string
	// SystemPrompt 非空时会在每次模型请求前加上。
	SystemPrompt string
	// MaxIterations 限制模型↔工具轮数。
	//
	// 未设置（0）时使用 DefaultSafeMaxIterations（200）：个人客户端的默认配置
	// 必须有成本总闸——模型在两个工具之间来回摇摆时，"完全相同的重复调用守卫"
	// 抓不到，没有上限就是无上限烧钱。显式**负数**表示真的不限制轮数；>0 是显式
	// 上限。需要更长的自主任务就调大它，或自己接 costmeter 的花费上限。
	MaxIterations int
	// ContextBudget 是估算的 prompt token 预算。**默认自动推导**（见
	// DefaultContextBudgetRatio）：模型上下文 × 0.6，下限 16k。显式 >0 时以配置
	// 为准；显式 <=0（负数）表示关闭自动压缩。
	//
	// 默认开启的原因：个人客户端一个会话会开一整天，超预算时 provider 会返回一条
	// 用户看不懂的英文 400。"默认安全"比"让用户自己去配"重要。
	ContextBudget int
	// Compactor 压缩超预算对话。默认使用 compact.WindowCompactor（KeepLast 8，
	// 保留最近 4 条工具结果原文）；nil 表示使用默认值，负数不可用。
	//
	// 需要更高质量的摘要时显式设置 compact.LLMCompactor（会多一次模型调用）。
	Compactor rig.Compactor
	// Presenter 让 present 工具能把交付文件交给宿主 UI 打开（对应
	// hosttools.Options.FilePresenter）。
	//
	// 它同时是 present 的**门控**：nil 时 present 会被零配置体检摘掉（留着一个必然
	// 报错的交付工具只会诱导模型重试），非 nil 时 present 保留并**进入初始 active
	// 集**——宿主已经为"能交付文件"付了配置成本，就别再让模型先 tool_search 一次。
	//
	// 注意类型是 rig.FilePresenter（PresentFiles），不是历史上的 rig.Presenter
	// （Present，那是已删除的 present_artifact 协议），别混用。
	Presenter rig.FilePresenter
	// ToolDiscovery 控制**动态工具加载**（lazy tool discovery）。
	//
	//	nil           → 开启（默认）：初始只把核心工具（+ tool_search）发给模型，
	//	                其余工具注册但不 active，模型用 tool_search 按需激活；
	//	Bool(false)   → 关闭（v0.20 行为）：全部注册工具一开始就 active。
	//
	// 用 *bool 而不是 bool：零值必须表示"开启"，而 bool 的零值是 false。这与
	// provider/openai.Config.IncludeUsage 的处理一致。
	ToolDiscovery *bool
	// InitialActiveTools 覆盖 discovery 开启时的初始 active 集（名字来自已注册
	// 工具；未知名字忽略）。nil 表示用 kits 的默认核心集；非 nil 的**空切片**表示
	// 初始一个都不 active（只用 tool_search 也能工作）。
	//
	// 无论怎么配，tool_search 与 memory 都会在初始 active 集里（前者是发现入口，
	// 后者是用户明确要求默认开启的记忆工具）。
	InitialActiveTools []string
	// ModelContextTokens 是模型的上下文窗口（token）。用于默认 ContextBudget 的
	// 自动推导；0 表示未知，按 128k 算。接本地模型时宿主通常知道这个数，填上它
	// 能让压缩阈值贴合真实窗口。
	// ModelContextTokens 只用于**推导** ContextBudget（模型真实窗口 × 0.6、
	// 下限 16k）。显式设置 ContextBudget 时本字段不参与计算。
	ModelContextTokens int
	// MaxRunCostUSD 是单次会话的花费上限（美元）。>0 时 kit 会在每次 Run 期间
	// 监视 Meter 的累计花费，超限以明确错误收尾（"本次会话已花费 $X，超过上限
	// $Y"），并取消本次 Run。0 表示不限制。
	//
	// 它与 MaxIterations 是两种不同的护栏：轮数管"模型抽风"，花费管"账单"。
	MaxRunCostUSD float64
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
	// ToolPreset 按预设过滤最终工具集（minimal/personal/standard/all）。
	//
	// 语义：**kit 自己构建默认工具集**时，空值等价于 "personal"（个人客户端默认）；
	// 宿主传了 Config.Tools 时，只有**显式**设置本字段才会过滤宿主工具集（空值不
	// 过滤——那是宿主自己的选择，不能被静默裁剪）。未知预设返回错误。
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

	// Searcher 提供 web_search。为 nil 时 web_search 会被**从工具集里摘掉**
	// （而不是留一个永远报 "no Searcher configured" 的工具占着 system prompt）。
	Searcher web.Searcher

	// SessionLog 提供 session_search / session_trace / session_event_*。
	// 为 nil 时这些工具会被摘掉。
	SessionLog rig.SessionLog

	// Meter 非空时按 EventUsage 累计本次会话的花费（`costmeter`）。个人用户
	// 自己付钱，"这个会话花了多少"是产品能力而不是可观测性装饰；UI 读
	// `Meter.Session().CostUSD` 即可。
	Meter *costmeter.Meter

	// Memory 是 `memory` 工具的存储（渐进式 wiki 记忆，见 tools/memory）。
	//
	// 记忆应当**默认开启**：nil 且 Root 非空时，kit 自动用
	// `<状态目录>/memory/<workspaceID>` 建一个按工作区分区的文件 wiki（目录 0700、
	// 页面 0600）。记忆**不写进仓库**——它可能含不该提交的内容，且 `.rig/` 出现在
	// `git status` 里会干扰"帮我提交"。想换后端（加密、远程、测试）就自己实现
	// memory.Wiki；想彻底关掉就是让工具集里没有 memory（自定义 Tools）。
	Memory memory.Wiki

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
	//
	// 与 UndoTracker 同时设置时 **UndoTracker 优先**（宿主自己给了 tracker 就用它，
	// GitUndo 不再单独建一个）。
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
	// SandboxGrants 在 Tools 为 nil（使用默认工具集）时**注册**
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
	//
	// **默认开**（nil = 开）：它们是 discoverable 的（不进初始 active 集），所以
	// 注册不增加每个回合的常驻成本，却让"沙箱挡住了我需要的东西"有一条**真实存在**
	// 的出口（否则拒绝建议会把模型指向一个不存在的工具——真实用户日志里正是如此）。
	// 关掉用 kit.Bool(false)。
	SandboxGrants *bool
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
	// 配置档先落地：档只填宿主没显式设置的字段。
	if err := applyProfile(&cfg); err != nil {
		return nil, err
	}

	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("kit: %w", rig.ErrModelRequired)
	}
	root := strings.TrimSpace(cfg.Root)
	if root == "" {
		root = DefaultRoot
	}
	// 注入即失效（审计 A1）：自建工具集**不会**经过 OS 沙箱包装，但 SandboxRunner
	// 仍会被 PTC 使用——同一份配置出现两种行为，而用户以为"还在沙箱里"。
	// 这是"以为有保护其实没有"，必须构造期报错，不能只告警。
	if cfg.Tools != nil && cfg.SandboxRunner != nil {
		return nil, errors.New("kit: Config.Tools 与 Config.SandboxRunner 不能同时设置——" +
			"自建工具集不会经过 OS 沙箱包装；请在 full.All(AllOptions{SandboxRunner: …}) 里挂 runner，或不要传 Tools")
	}
	// <=0 表示不限制轮数（unbounded）；负值归一化为 0。
	// G3（成本护栏）：未配置轮数上限时给 200，而不是 agent 层的 0（不限）。
	// 显式负数表示"我要真的不限"，此时原样传下去。
	maxIterations := cfg.MaxIterations
	if maxIterations == 0 {
		maxIterations = DefaultSafeMaxIterations
	}
	maxIterations = max(maxIterations, 0)

	// L1/L2（长会话护栏）：默认自动压缩。预算按模型上下文推导，压缩器默认用
	// 确定性的窗口裁剪（不额外调用模型）。
	contextBudget := cfg.ContextBudget
	if contextBudget == 0 {
		contextBudget = defaultContextBudget(cfg.ModelContextTokens)
	}
	compactor := cfg.Compactor
	if compactor == nil && contextBudget > 0 {
		compactor = compact.WindowCompactor{
			KeepLast:           compact.DefaultKeepLast,
			ToolResultKeepLast: compact.DefaultToolResultKeepLast,
		}
	}

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
		// ProviderKind 为空/ openai 时走 openai（保持旧行为）；ollama/lmstudio/local
		// 由 BuildProvider 构造。个人用户常常只有"模型名 + 本地服务地址"。
		built, err := BuildProvider(cfg.ProviderKind, Config{
			Model:     model,
			BaseURL:   cfg.BaseURL,
			APIKey:    cfg.APIKey,
			APIKeyEnv: cfg.APIKeyEnv,
		})
		if err != nil {
			return nil, fmt.Errorf("kit: provider: %w", err)
		}
		provider = built
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

	sandboxGrants := cfg.SandboxGrants == nil || *cfg.SandboxGrants

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
	// 记忆存储：显式注入优先；否则按工作区自动开一个文件 wiki。装配失败（例如状态
	// 目录不可写）不阻断 kit：回落进程内 wiki 并让 memory 工具仍然可用，但会在
	// SessionStore/日志里留下可诊断的痕迹——这里选择"可用优先"。
	memoryWiki := cfg.Memory
	var memoryWikiErr error
	if memoryWiki == nil && root != "" {
		if wiki, wikiErr := memory.NewWorkspaceWiki("", root); wikiErr == nil {
			memoryWiki = wiki
		} else {
			memoryWikiErr = wikiErr
		}
	}
	// 逐命令提权通道：审批档位 + Asker 决定它能不能走通；不能走通时连"建议模型去
	// 用它"都不该发生（那同样是死路）。
	approver := newEscalationApprover(cfg.Asker, approval, cfg.Gate != nil, nil)
	if policy != nil {
		policy.SetDenialOptions(sandbox.Options{
			HasGrantTool:  sandboxGrants,
			HasEscalation: approver.Available(),
		})
	}

	toolSet := cfg.Tools
	if toolSet == nil {
		toolSet = full.All(full.AllOptions{
			Policy:        policy,
			Root:          root,
			SandboxRunner: cfg.SandboxRunner,
			UndoTracker:   undoTracker,
			Searcher:      cfg.Searcher,
			SessionLog:    cfg.SessionLog,
			Asker:         cfg.Asker,
			Wiki:          memoryWiki,
			SandboxGrants: sandboxGrants,
			Escalator:     approver,
		})
		if sandboxGrants {
			// 授权工具与状态工具共享同一个 policy：批准后立即对所有工具生效，
			// 状态工具读到的是同一份实时视图。按名字去重：若默认工具集以后自己
			// 登记了这两个工具（tools/full.All），这里不会再追加一份重名工具
			// （agent.New 会拒绝重名）。
			statusOpts := []sandboxstatus.Option{}
			if cfg.SandboxRunner != nil {
				// runner 如实自报覆盖程度（Windows ACL / Landlock = partial，并附
				// "没被治理的部分"），让模型在 sandbox_status 里直接读到。
				runner := cfg.SandboxRunner
				statusOpts = append(statusOpts, sandboxstatus.WithRunnerInfo(
					func() (string, sandbox.Enforcement) {
						name, enforcement, _ := sandbox.RunnerStatus(runner)
						return name, enforcement
					}))
			}
			toolSet = appendUniqueTools(toolSet,
				sandboxgrant.New(policy),
				sandboxstatus.New(policy, statusOpts...),
			)
		}
	}
	// 注入即失效（审计 A1）：这些依赖只喂给默认工具集，自建工具集时不会生效。
	// 不报错（工具缺席是宿主的显式选择），但必须让宿主知道。
	if cfg.Tools != nil {
		ignored := make([]string, 0, 4)
		if cfg.Searcher != nil {
			ignored = append(ignored, "Searcher（web_search 不会出现）")
		}
		if cfg.SessionLog != nil {
			ignored = append(ignored, "SessionLog（session_* 工具不会出现）")
		}
		if cfg.GitUndo || cfg.UndoTracker != nil {
			ignored = append(ignored, "GitUndo/UndoTracker（回合快照照打，但自建工具集里没有 undo 工具）")
		}
		if cfg.Asker != nil {
			ignored = append(ignored, "Asker 的 ask_user 工具不会出现（审批门禁仍会使用 Asker）")
		}
		if cfg.SandboxGrants != nil && *cfg.SandboxGrants {
			ignored = append(ignored, "SandboxGrants（sandbox_status/request_sandbox_access 不会出现）")
		}
		if len(ignored) > 0 {
			slog.Warn("kit: Config.Tools 非 nil 时，这些依赖只作用于默认工具集，对自建工具集不生效",
				"ignored", strings.Join(ignored, "；"))
		}
	}

	// 预设只作用于 **kit 自己构建的默认工具集**：宿主传了 Config.Tools 时，
	// 那些工具是宿主的选择，不能被预设静默裁掉。
	// 个人客户端默认用 personal（standard 去掉多智能体编排与 report）。
	preset := strings.TrimSpace(cfg.ToolPreset)
	if preset == "" && cfg.Tools == nil {
		preset = presets.PresetPersonal
	}
	if preset != "" {
		filtered, err := presets.Filter(preset, toolSet)
		if err != nil {
			return nil, fmt.Errorf("kit: %w", err)
		}
		toolSet = filtered
	}

	// 事件桥要在门禁之前构造：审批观察者（approval.request/response）需要它，
	// 否则"正在等用户确认"这类状态进不了事件流，UI 无从显示。
	bridge := newEventBridgeFor(cfg.EventWriter, cfg.Redactor, cfg.Meter, cfg.Model)
	// 事件桥晚于工具集构造：把提权通道的审计出口补上（允许与拒绝都要留痕）。
	approver.bridge = bridge

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
				if bridge != nil {
					opts = append(opts, gate.WithAskObserver(func(event gate.AskEvent) {
						bridge.emitApproval(event)
					}))
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

	// 零配置体检（审计 2C）：personal 预设里有 13 个工具在 kit 默认装配下
	// **永远无法成功**（缺 Searcher / SessionLog / GoalStore / PlanController /
	// subagent runner），它们只占 system prompt 并诱导模型重试。
	// 原则：默认工具集里**每个工具都必须真的能用**。
	for _, name := range cfgUnavailableTools(cfg) {
		toolSet = withoutTool(toolSet, name)
	}

	if undoTracker == nil {
		// 没有回合快照就不该把 undo 暴露给模型：它只会返回"没有可用快照"，
		// 白白浪费一个回合。宿主配置 GitUndo 或 UndoTracker 后，full.All 会自动
		// 挂载它。
		toolSet = withoutTool(toolSet, "undo")
	}
	// 动态工具加载：注册表里"已注册但不 active"的工具由 tool_search 按需激活。
	// 必须在 append 完所有可选工具（sandboxgrant / PTC / undo 过滤）之后计算，
	// 否则清单会漏掉它们。
	// 默认开，但**自建工具集（Config.Tools）默认不开**：宿主自己挑出来的工具就该
	// 能用，把它们的 schema 藏起来会让"我明明传了这个工具"变成一次往返。要在这个
	// 集合上也开发现，显式写 ToolDiscovery: kit.Bool(true)。
	discoveryEnabled := cfg.Tools == nil
	if cfg.ToolDiscovery != nil {
		discoveryEnabled = *cfg.ToolDiscovery
	}
	// full.All 里有一个"未接线"的 tool_search（账本要求它可构造）；kit 用自己带
	// 回调的实例替换它。关闭 discovery 时干脆不注册：那就是 v0.20 的工具面。
	toolSet = withoutTool(toolSet, discovery.ToolSearch)
	var toolActivator rig.ToolActivator
	var activeTool = func(sessionID string) []string { return nil }
	if discoveryEnabled {
		// 闭包先声明、稍后由 underlying agent 赋值：tool_search 是注册在 agent 之前
		// 构造的，但它的回调只会在运行期被调用。
		listed := toolSet
		searchTool := discovery.New(discovery.Options{
			List: func(ctx context.Context) []discovery.Item {
				active := make(map[string]struct{})
				for _, name := range activeTool(agent.ToolSessionID(ctx)) {
					active[name] = struct{}{}
				}
				items := make([]discovery.Item, 0, len(listed))
				for _, tool := range listed {
					if tool == nil {
						continue
					}
					name := tool.Name()
					if _, isActive := active[name]; isActive {
						continue
					}
					if _, alwaysActive := alwaysActiveTools[name]; alwaysActive {
						continue
					}
					items = append(items, discovery.Item{Name: name, Summary: tool.Description()})
				}
				return items
			},
			Activate: func(ctx context.Context, names []string) ([]string, []string) {
				if toolActivator == nil {
					return nil, names
				}
				sessionID := agent.ToolSessionID(ctx)
				before := make(map[string]struct{})
				for _, name := range activeTool(sessionID) {
					before[name] = struct{}{}
				}
				activated, unknown := toolActivator.ActivateTools(sessionID, names...)
				// 事件只报"真正新增"的工具（幂等的重复激活不该让 UI 抖动）。
				var added []string
				for _, name := range activated {
					if _, existed := before[name]; !existed {
						added = append(added, name)
					}
				}
				emitToolActivated(bridge, added, toolActivatedByModel)
				return activated, unknown
			},
		})
		toolSet = append(toolSet, searchTool)
	}
	initialActive := initialActiveTools(cfg, toolSet, discoveryEnabled)

	promptRoot := workspaceRoot(policy, root)
	systemPrompt := assembleSystemPrompt(cfg.SystemPrompt, cfg.Environment, cfg.Instructions, promptRoot)

	spec := rig.AgentSpec{
		Provider:           provider,
		Model:              model,
		SystemPrompt:       systemPrompt,
		MaxIterations:      maxIterations,
		Tools:              toolSet,
		InitialActiveTools: initialActive,
		Gate:               gateImpl,
		History:            history,
		ContextBudget:      contextBudget,
		Compactor:          compactor,
		Spill:              cfg.Spill,
		CheckpointStore:    cfg.CheckpointStore,
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
	// 把动态加载能力接上：tool_search 的回调与 Agent.ActivateTools 都走它。
	if candidate, ok := underlying.(rig.ToolActivator); ok {
		toolActivator = candidate
		activeTool = func(sessionID string) []string { return candidate.ActiveTools(sessionID) }
	}
	return &Agent{
		agent:           underlying,
		compactor:       compactor,
		contextBudget:   contextBudget,
		history:         history,
		undo:            undoTracker,
		events:          bridge,
		meter:           cfg.Meter,
		model:           cfg.Model,
		store:           storeDir,
		maxRunCost:      cfg.MaxRunCostUSD,
		policy:          policy,
		memoryWiki:      memoryWiki,
		memoryWikiErr:   memoryWikiErr,
		activator:       toolActivator,
		registeredNames: toolNamesOf(toolSet),
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
	// maxRunCost > 0 时对每次 Run 做花费护栏（需要 Meter）。
	maxRunCost float64
	// activator 是底层 agent 的动态工具加载能力（rig.ToolActivator）；discovery
	// 关闭时底层 agent 仍然实现它（InitialActiveTools=nil 时全部 active），因此
	// 这里总是非 nil。
	activator rig.ToolActivator
	// registeredNames 是**全部注册**的工具名（排序），与 active 集合无关。
	registeredNames []string
	// memoryWiki 是 memory 工具实际使用的存储；memoryWikiErr 记录"自动建文件 wiki
	// 失败后回落到进程内实现"的原因（可诊断，不静默）。
	memoryWiki    memory.Wiki
	memoryWikiErr error
}

// MemoryWiki 返回 memory 工具实际使用的存储（宿主可用来预置/导出页面）。
func (a *Agent) MemoryWiki() memory.Wiki {
	if a == nil {
		return nil
	}
	return a.memoryWiki
}

// MemoryWikiError 返回"自动创建持久化记忆目录失败"的原因；nil 表示一切正常。
//
// 非 nil 时 memory 工具仍然可用，但记忆只在进程内（重启即丢）。宿主应当在 UI 里
// 提示用户（例如状态目录不可写），或显式注入 Config.Memory。
func (a *Agent) MemoryWikiError() error {
	if a == nil {
		return nil
	}
	return a.memoryWikiErr
}

// goosForToolFilter 是平台相关工具过滤所用的 OS 名（测试可注入）。
var goosForToolFilter = runtime.GOOS

// cfgUnavailableTools 返回"装配缺失、调用必然失败"的工具名。
//
// 这些工具在 tools/full.All 里是静态构造的（Schema/Description 都在），但它们的
// 执行需要宿主注入能力；kit 没有对应配置时把它们**摘掉**，而不是留给模型一个
// 只会报 "not configured" 的陷阱。
func cfgUnavailableTools(cfg Config) []string {
	var drop []string
	if cfg.Searcher == nil {
		drop = append(drop, "web_search")
	}
	if cfg.SessionLog == nil {
		drop = append(drop,
			"session_search", "session_trace",
			"session_event_read", "session_event_search", "session_event_trace",
		)
	}
	// kit 目前没有 GoalStore / PlanController 的注入入口；宿主需要这些能力时请走
	// full.All + Config.Tools（完整装配路径）。
	// subagent/subagent_fork 已经不在 personal 预设里（个人客户端不提供多智能体
	// 编排），这里不必再列。
	drop = append(drop,
		"get_goal", "create_goal", "update_goal",
		"plan_review", "exit_plan_mode",
	)
	if cfg.Presenter == nil {
		// present 需要 rig.FilePresenter 才能把交付文件交给宿主 UI；零配置没有它，
		// 留着就是一个"必然报错"的工具（审计 2C 的死工具教训）。接上 Presenter 后
		// 它会被保留**并主动进初始 active 集**（见 hostEnabledActiveTools）。
		drop = append(drop, "present")
	}
	if cfg.Asker == nil {
		// ask_user 需要一个能弹窗的宿主；没有 Asker 时它只会报
		// "no interactive UI in this run"。有 Asker 时它会被保留（见 full.AllOptions.Asker）。
		drop = append(drop, "ask_user")
	}
	if !cfg.AllowNetwork {
		// 没有网络授权时 web_fetch 每次都会被沙箱拒绝（network access is disabled）；
		// 留着一个"永远失败"的联网工具只会让模型以为它能上网。
		drop = append(drop, "web_fetch")
	}
	if goosForToolFilter != "windows" {
		// pwsh 只在 Windows 上可用；别的平台上它必然报"找不到命令"。
		drop = append(drop, "pwsh")
	}
	return drop
}

// Tools 返回本 agent 实际装配的工具名（排序后副本）。
//
// 个人客户端的设置界面要能列出"这个 agent 现在能用什么"，而零配置体检会按宿主
// 依赖剪掉一批工具——所以这个列表**不是**预设名单，而是真正注册进去的那些。
func (a *Agent) Tools() []string {
	if a == nil {
		return nil
	}
	return a.ToolsFor("")
}

// RegisteredTools 返回**全部已注册**的工具名（排序）：它们是"装载了但可能没
// 激活"的工具，设置界面用它区分"能用"（Tools）与"已装载"。
func (a *Agent) RegisteredTools() []string {
	if a == nil {
		return nil
	}
	return append([]string(nil), a.registeredNames...)
}

// ToolsFor 返回指定会话**当前 active** 的工具名（排序）。
//
// 动态工具加载是按会话的：同一个 agent 可以同时服务多个会话，各自的 active 集合
// 互不影响。Tools() 是它的默认会话（""）版本。
func (a *Agent) ToolsFor(sessionID string) []string {
	if a == nil || a.activator == nil {
		return nil
	}
	names := a.activator.ActiveTools(sessionID)
	sort.Strings(names)
	return names
}

// ActiveTools 返回 sessionID 当前 **active** 的工具名（注册/激活顺序）。
//
// 与 ToolsFor 的区别：ToolsFor 返回排序后的副本（给设置界面用），ActiveTools 保持
// 请求里的真实顺序（给诊断与缓存断言用）。两者都实现 rig.ToolActivator。
func (a *Agent) ActiveTools(sessionID string) []string {
	if a == nil || a.activator == nil {
		return nil
	}
	return a.activator.ActiveTools(sessionID)
}

// ActivateTools 在 sessionID 里**纯增量**激活已注册的工具（触发者是宿主，会写一条
// tool.activated 事件）。它转发给底层的 rig.ToolActivator；未知名字进 unknown。
//
// 为什么只加不减：请求里的 tools 数组一旦变化，provider 的 prefix cache 就要重算；
// 追加式激活让已激活部分逐字节稳定。移除工具需要重建整个 tools 数组，因此不提供。
func (a *Agent) ActivateTools(sessionID string, names ...string) (activated, unknown []string) {
	if a == nil || a.activator == nil {
		return nil, names
	}
	before := make(map[string]struct{})
	for _, name := range a.activator.ActiveTools(sessionID) {
		before[name] = struct{}{}
	}
	activated, unknown = a.activator.ActivateTools(sessionID, names...)
	var added []string
	for _, name := range activated {
		if _, existed := before[name]; !existed {
			added = append(added, name)
		}
	}
	emitToolActivated(a.events, added, toolActivatedByHost)
	return activated, unknown
}

// toolNamesOf 提取工具名并排序（去重保持稳定）。
func toolNamesOf(tools []rig.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		names = append(names, t.Name())
	}
	sort.Strings(names)
	return names
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
	// 工具调用计数必须按 Run 独立（Agent 可被并发使用）：放在桥之外的包装里。
	var toolCalls atomic.Int64
	if inner := req.Emit; inner != nil {
		req.Emit = func(event Event) {
			if event.Type == rig.EventToolCall {
				toolCalls.Add(1)
			}
			inner(event)
		}
	}
	// 成本护栏：把"这一轮会不会超预算"交给 meter 判断，超了就取消本轮 Run。
	runCtx, costErr, cancelWatch := a.watchCost(ctx, &req)
	if cancelWatch != nil {
		defer cancelWatch()
	}
	result, runErr := a.agent.Run(runCtx, req)
	if costErr != nil && costErr.tripped {
		// 取消是守卫造成的：用可读的错误替换 context.Canceled，并保留部分结果。
		runErr = costErr.err()
	}
	if a.events != nil {
		payload := eventspkg.RunEnd{
			Turns:     result.Iterations,
			Messages:  len(result.Messages),
			ToolCalls: int(toolCalls.Load()),
		}
		if runErr != nil {
			payload.Err = a.events.redact(runErr.Error())
		}
		payload.PromptTokens = result.Usage.PromptTokens
		payload.CompletionTokens = result.Usage.CompletionTokens
		a.events.emit(eventspkg.KindRunEnd, payload)
	}
	return result, runErr
}

// costWatch 承载一次 Run 的花费守卫状态。
type costWatch struct {
	tripped bool
	spent   float64
	limit   float64
}

// err 返回超限时的用户可见错误。
func (w *costWatch) err() error {
	return fmt.Errorf("kit: 本次会话已花费 $%.2f，超过上限 $%.2f（可调大 MaxRunCostUSD 或新开一个会话）",
		w.spent, w.limit)
}

// watchCost 在配置了 MaxRunCostUSD 且挂了 Meter 时，把 Run 的 context 换成可取消
// 的，并在每次 usage 事件后检查累计花费；超限即取消，由 Run 收尾成可读错误。
//
// 它返回的 cancel 必须在 Run 结束后调用（同一次 Run 内持续有效）。
func (a *Agent) watchCost(ctx context.Context, req *RunRequest) (context.Context, *costWatch, context.CancelFunc) {
	if a.maxRunCost <= 0 || a.meter == nil || req.Emit == nil {
		return ctx, nil, nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	watch := &costWatch{limit: a.maxRunCost}
	next := req.Emit
	req.Emit = func(event Event) {
		if next != nil {
			next(event)
		}
		if watch.tripped || event.Type != rig.EventUsage {
			return
		}
		spent := a.meter.Session().CostUSD
		if spent >= watch.limit {
			watch.tripped = true
			watch.spent = spent
			cancel()
		}
	}
	return runCtx, watch, cancel
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
		// `.git` 走"数据面可写、控制面只读"的模型（index/objects/refs 可写，
		// config/hooks/info 只读）：整体保护会让 `git commit` 在沙箱里做不到，
		// 而整体放开等于把 hooks 这条命令执行面交给被约束进程。
		policy.AddGitProtection(root)
		policy.AddProtectedPaths(sandbox.WorkspaceNonGitMetadataPaths(root)...)
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

// --- 动态工具加载（lazy tool discovery） ------------------------------------

// alwaysActiveTools 是**无论配置如何**都在初始 active 集里的工具：
//
//   - tool_search：发现入口本身。没有它，模型无法激活任何东西。
//   - memory：用户的记忆。产品决定"记忆默认开"，它不是可选扩展。
//   - skill：技能清单。小 schema + 开工前必需，见上面的说明。
//
// 它们也不会出现在 tool_search 的清单里（清单只列 inactive 的工具）。
var alwaysActiveTools = map[string]bool{
	discovery.ToolSearch: true,
	"memory":             true,
	// skill：schema 只有 ~840 字节，而"当前有哪些技能可用"是**开工前**就该知道的
	// 信息——让模型先 tool_search 一次再发现技能，等于每次会话都多一次往返。
	"skill": true,
}

// Bool 返回 v 的指针。用于 kit.Config 里的三态开关（nil = 默认行为）。
//
//	off := kit.Bool(false) // kit.Config{ToolDiscovery: off}：回到 v0.20 的工具面
func Bool(v bool) *bool { return &v }

// coreActiveTools 是 discovery 开启时默认 active 的"核心 13"（另有 `tool_search`、
// `memory`、`skill` 三个常驻，见 alwaysActiveTools；零配置合计 16 个）。
//
// 选它们的标准：一次普通编码会话**几乎每轮都会用到**（读、写、编辑、搜索、跑
// 命令、待办、交付），因此它们的 schema 值得常驻在请求里。其余工具（web、jobs、
// terminal、schedule、mcp、lsp、worktree 等）按需激活。
var coreActiveTools = []string{
	"read_file", "write_file", "edit_file", "apply_patch",
	"list_dir", "glob", "grep", "search",
	"bash", "todo_write", "working_directory",
	"read_image", "str_replace_editor",
}

// toolActivatedByModel / toolActivatedByHost 是 events.ToolActivated.Trigger 的取值。
const (
	toolActivatedByModel = "model"
	toolActivatedByHost  = "host"
)

// initialActiveTools 计算传给 agent 的 InitialActiveTools。
//
// 三种情况：
//   - discovery 关闭 → nil（全部 active，v0.20 行为）；
//   - 宿主显式给了 InitialActiveTools → 它的副本（仍强制并入 alwaysActiveTools 中
//     实际注册了的那些：没有 tool_search 模型就无法自助激活，没有 memory 就违背
//     产品承诺）；
//   - 否则 → coreActiveTools + alwaysActiveTools 中实际注册了的那些。
//
// 未知名字（未注册）静默忽略：配置笔误不该让构造失败。
func initialActiveTools(cfg Config, toolSet []rig.Tool, discoveryEnabled bool) []string {
	if !discoveryEnabled {
		return nil
	}
	registered := make(map[string]struct{}, len(toolSet))
	for _, tool := range toolSet {
		if tool == nil {
			continue
		}
		registered[tool.Name()] = struct{}{}
	}
	chosen := make(map[string]struct{}, len(coreActiveTools)+len(alwaysActiveTools))
	if cfg.InitialActiveTools != nil {
		for _, name := range cfg.InitialActiveTools {
			chosen[strings.TrimSpace(name)] = struct{}{}
		}
	} else {
		for _, name := range coreActiveTools {
			chosen[name] = struct{}{}
		}
	}
	for name := range alwaysActiveTools {
		chosen[name] = struct{}{}
	}
	for _, name := range hostEnabledActiveTools(cfg) {
		chosen[name] = struct{}{}
	}
	out := make([]string, 0, len(chosen))
	for name := range chosen {
		if name == "" {
			continue
		}
		if _, ok := registered[name]; !ok {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// hostEnabledActiveTools 返回"宿主显式接线后应当立刻可用"的工具。
//
// 与 alwaysActiveTools 的区别：那些是产品默认（与配置无关），这些是宿主付了配置
// 成本的能力（接一个 Presenter 就该能交付文件）。它们同样不会出现在 tool_search
// 清单里（清单只列 inactive 的）。
func hostEnabledActiveTools(cfg Config) []string {
	var out []string
	if cfg.Presenter != nil {
		out = append(out, "present")
	}
	return out
}

// emitToolActivated 把一次激活写进事件流（trigger 区分模型自助与宿主直调）。
func emitToolActivated(bridge *eventBridge, names []string, trigger string) {
	if bridge == nil || len(names) == 0 {
		return
	}
	bridge.emit(eventspkg.KindToolActivated, eventspkg.ToolActivated{
		Tools:   append([]string(nil), names...),
		Trigger: trigger,
	})
}
