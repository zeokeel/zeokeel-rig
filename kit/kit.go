package kit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
	"github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/session/compact"
	"github.com/zeokeel/zeokeel-rig/session/store"
	"github.com/zeokeel/zeokeel-rig/storage"
	"github.com/zeokeel/zeokeel-rig/tools/discovery"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/hosttools"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
	"github.com/zeokeel/zeokeel-rig/tools/presets"
	"github.com/zeokeel/zeokeel-rig/tools/ptc"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxgrant"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxstatus"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
	"github.com/zeokeel/zeokeel-rig/tools/undo"
	"github.com/zeokeel/zeokeel-rig/tools/web"
)

// DefaultRoot 是 Config.Root 为空时使用的工作区根目录。
const DefaultRoot = "."

// DefaultMaxIterations 是 Config.MaxIterations 为零或负数时使用的模型轮数上限
// 默认值：0，表示不限制轮数（unlimited）。它与 agent.DefaultMaxIterations 相同。
const DefaultMaxIterations = agent.DefaultMaxIterations

// DefaultSafeMaxIterations 是 kit 在 Config.MaxIterations 未配置（0）时采用的
// 轮数默认值：**0，不限制轮数**。kit 不再替宿主设置轮数总闸——轮数护栏属于人为
// 边界，会挡住模型本来能完成的自主任务；成本控制交给 costmeter 的预算
// （Config.Meter / MaxRunCostUSD）与人工审批，而不是截断模型的思考轮次。
//
// 需要上限时显式设置 Config.MaxIterations（>0 为显式上限；负数同样是"不限"）。
// 该常量保留导出只为兼容既有引用，语义已改为"默认不限"。
const DefaultSafeMaxIterations = 0

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
	// ApprovalAsk 是默认值：**只读免问 + 其余问人**。门禁为
	// gate.PersonalAsk(...)：gate.SandboxReadOnlyTools 里的只读工具（read_file、
	// glob、grep…）免审批；其余一切（写文件、bash/pwsh、后台任务…）交给
	// Config.Asker 人工审批；没有 Asker 时 fail-closed 拒绝并说明"没有可用的
	// 审批通道"（而不是假装 approval=never）。
	//
	// 它**不**做命令级风险分类：bash 里的 `ls` 与 `rm -rf /` 一样都要问人。
	ApprovalAsk ApprovalMode = "ask"
	// ApprovalAuto 是"不问人"预设：门禁为 gate.PersonalAuto()，也就是
	// **全部放行**（AllowAll，与 DSH 的 `approval=never` 同义：审批环节不再拦住
	// 任何调用）。它与 PermissionPreset=danger-full-access 是同一组合：
	// 全权由 Mode 表达，本字段只决定问不问人。
	//
	// 想保留"危险命令硬拦"这一层，必须**显式**叠加 gate.CommandRiskGate
	// （默认预设已不再使用它——规则表的覆盖率靠不住）：
	//
	//	Gate: gate.CommandRiskGate(gate.PersonalAuto())
	//
	// Deprecated 提示：CommandRiskGate 仍然完整可用，但不再是任何默认预设的一部分。
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
//     WithNetwork(AllowNetwork，默认开), WithCommandAllowlist(CommandAllowlist...))。
//     CommandAllowlist 为空且 DisableShellAccess=false 时策略允许任意程序
//     （WithAnyCommandAllowed）；默认门禁**不做**命令风险分类，能否执行由人工审批
//     决定（要硬拦就显式叠加 gate.CommandRiskGate）。
//   - Tools：使用该 policy 与 Root 的 tools/full.NewRuntime（完整内置工具集，
//     包含受沙箱约束的文件工具与 shell），再经 ToolPreset（默认 personal）过滤和
//     零配置体检裁剪；没有宿主依赖的工具会被摘掉，而不是留一个必然报错的占位。
//   - Gate：由 Approval 决定。"ask"（默认）是两层门禁：只读工具免问；其余
//     （写文件、bash/pwsh，**包括 `rm -rf /`**）交给 Config.Asker 人工审批，默认
//     **不超时**（一直等，只有取消本轮才撤回；无 Asker 时 fail-closed 拒绝并说明
//     "没有可用的审批通道"）。"auto" 不问人、全部放行（PersonalAuto == AllowAll，
//     与 DSH 的 danger-full-access 档等价）；"never" 拒绝所有调用。非 nil 的
//     Config.Gate 始终优先。默认不做命令风险分类，要硬拦就显式叠加
//     gate.CommandRiskGate。
//   - History：session.NewMemory()（内存实现，按 SessionID 隔离）。
//   - PermissionPreset：默认不选；可用四档（read-only / workspace-write /
//     danger-full-access / auto，见 Presets()）一次性设置 Mode 与 Approval。
//     显式设置的 Gate/Mode/Approval（或 Policy）优先，此时实际档位是 custom。
//   - Root："."（构造时进程的工作目录）。
//   - MaxIterations：0 → DefaultSafeMaxIterations（0，不限）；>0 是显式上限；
//     负数同样表示不限制轮数。
type Config struct {
	// Profile 是"配置档"名字（careful / personal / local，见 Profiles()）。
	// 它只填**你没显式设置**的字段：Approval、ToolPreset、ProviderKind、
	// MaxIterations。个人客户端推荐 "personal"（只读免问、其余问人，含危险命令）。
	// 要选沙箱档位用 PermissionPreset（它比 Profile 更具体，先落地）。
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
	// 未设置（0）表示**不限制轮数**（与 agent 层一致）：轮数护栏是人为边界，会挡住
	// 模型本可完成的自主任务。>0 是显式上限（超限返回包装 rig.ErrMaxIterations 的
	// 错误）；负数同样表示不限制。想控制成本请接 costmeter（Meter/MaxRunCostUSD）
	// 或依赖人工审批，而不是限制轮数。
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
	// AutoAllowSandboxedCommands 让"已经被 OS 沙箱包住"的 shell 命令**免审批**。
	//
	//	nil（默认）    → 开：满足判据的 bash/pwsh 调用不再弹窗（但仍写
	//	                 approval.response 事件，reason=sandboxed，便于 UI 显示与审计）；
	//	kit.Bool(false) → 关：回到"每条可变命令都问人"的行为。
	//
	// 判据（全部满足才免问，见 gate.AllowSandboxedCommands）：工具是 bash/pwsh、
	// SandboxRunner 非 nil 且自报能强制、策略不是 danger-full-access。**不看命令
	// 文本**——围栏已生效时命令内容不再参与免问判定（默认门禁也不做命令风险分类）。
	// 文件工具（write_file/edit_file/apply_patch…）不经 OS runner，因此不受影响，
	// 照旧问人。
	//
	// 理由：沙箱与审批是两种独立的成本。命令已被围栏限制到"只能写工作区、不能连网"
	// 时再逐条要人点，用户会为了少点几十次弹窗而把审批整体关掉——那时连该有的审批
	// 一起丢了。安全性应当由围栏保证，而不是由点击次数保证。
	AutoAllowSandboxedCommands *bool
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
	// Activations 持久化**按会话**的完整 active 工具集合（激活/取消激活都跨进程
	// 保留）。nil 表示只在进程内记 active 集。
	//
	// 它直接透传给 agent 的 rig.AgentSpec.ActivationStore：kit 加载某个 SessionID
	// 的 active 集合时用 store 里的记录整体替换 InitialActiveTools（记录是权威
	// 全集，因此取消激活也能跨重启生效），ActivateTools / DeactivateTools 之后写回。
	// SessionID 为空（临时会话）时完全跳过持久化。
	//
	// 无头宿主可以先用 kit.NewMemoryActivations() 起个进程内实现；要跨重启保留就
	// 自己实现这个接口（落文件/数据库都行）。注意它只在 ToolDiscovery 开启时才有
	// 意义：discovery 关闭时全部工具都 active，没有"已激活"可记。
	Activations rig.ToolActivationStore
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
	// ExtraTools 追加到（默认或自建的）工具集之后：宿主模块/插件注入业务工具
	// 的入口，不需要放弃 kit 的默认工具、发现与体检。它们不经过 ToolPreset 与
	// Mode 裁剪（宿主自己的选择），参与 discovery 的初始 active 计算。
	ExtraTools []Tool
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
	// gate.PersonalAskWithoutAsker()：只读放行、其余拒绝并说明"没有可用的审批通道"
	// ——而不是静默拒绝或假装 approval=never。
	//
	// 询问默认**不超时**：与 DSH 的 approval seam 一致，人多久不回就等多久，只有
	// 调用方取消轮次（ctx 结束）才会撤回请求。要给"一直不回"设上界，用 AskTimeout。
	Asker rig.Asker
	// AskTimeout 给"等待人工审批"设一个内部上界。0（默认）= **一直等**，与
	// gate.Ask 的默认一致，也与 DSH 一致：审批默认不超时，只有调用方取消轮次才会
	// 撤回请求。>0 时等待超时会以 gate.ErrApprovalExpired 收尾（Run 返回该错误）。
	//
	// auto 档的审查器超时（gate.DefaultToolReviewTimeout，15s）与本字段无关：
	// 那是"自动预审"的超时，超时后回退到人工 ask，而 ask 仍按本字段（默认无限）
	// 等待。
	AskTimeout time.Duration

	// Approvals 是审批记忆的持久化后端：用户在审批里选 allow-always 之后，规则写进
	// 它，下次遇到同一个目标（同工具同参数 / 同一条命令 / 同一个路径）直接放行、
	// 不再打扰。为 nil（默认）= 不记忆，行为与以前完全一致。
	//
	// 记忆**只免掉提问**，不放宽任何围栏：被记住的调用照样经过沙箱策略与工具自身的
	// 检查（read-only 下记住的命令仍然会被拒绝）。读写失败只记日志、不打断回合；
	// 进程内实现在 NewMemoryApprovals，要跨重启保留就自己实现 rig.ApprovalStore。
	//
	// 它只挂在 kit **自己拼的**门禁上。显式传了 Config.Gate 时不自动挂（那里面可能
	// 叠了 CommandRiskGate 之类的硬策略，记忆命中会绕过它）；要记忆就自己用
	// gate.WithApprovalMemory 决定包在哪一层。approval=never 时不挂。
	Approvals rig.ApprovalStore
	// ApprovalScope 决定记住的规则生效范围：rig.ApprovalScopeSession（默认，
	// 空值同义）= 只在写入它的会话里生效；rig.ApprovalScopeGlobal = 全局生效。
	// 其它值在构造期报错（写错了不该被静默当成 session）。
	ApprovalScope string
	// ApprovalPromptChain 是模型/审批人看到的审批文案链：按顺序拼接非空段。
	// 示例（先看 diff 再问是否允许）：
	//
	//	ApprovalPromptChain: []gate.Prompt{
	//		gate.DiffPrompt(gate.WithDiffRoot(root)),
	//		gate.SandboxGrantPrompt,
	//	},
	//
	// 为空（默认）时只用现有的一条：gate.SandboxGrantPrompt（写类工具回退到
	// AskGate 的默认文案），因此默认行为不变。Preflight 会如实报告当前用的是
	// 哪条链。
	ApprovalPromptChain []gate.Prompt

	// Searcher 提供 web_search。为 nil 时 web_search 会被**从工具集里摘掉**
	// （而不是留一个永远报 "no Searcher configured" 的工具占着 system prompt）。
	Searcher web.Searcher

	// SessionLog 提供 session_search / session_trace / session_event_*。
	// 为 nil 时这些工具会被摘掉。
	SessionLog rig.SessionLog

	// 以下六项是"宿主已经有的能力"，原样转交给工具集（`full.AllOptions` 的同名
	// 字段）。以前它们只有 `full.AllOptions` 有、kit 没有，于是接 MCP/技能库/共享
	// 任务板/调度器/评审器的宿主只能 `Config.Tools = full.All(...)` 自建装配，而
	// 那条路径会跳过 `full.NewRuntime`（丢懒加载、零配置体检与 grant 工具）。
	//
	// `Config.Tools` 非 nil 时它们同样不生效（工具是宿主自己的），并在告警里列出。

	// Skills 为 skill 工具提供技能存储；nil 表示默认文件系统存储
	// （$ZEOKEEL_SKILLS、<root>/.zeokeel/skills、~/.zeokeel/skills）。
	Skills rig.SkillStore
	// TeamTasks 为 team_task_* 提供共享任务板；nil 表示进程内实现。
	TeamTasks rig.TeamTaskManager
	// Scheduler 为 schedule_* 提供调度器；nil 表示进程内实现（重启即丢）。
	Scheduler rig.Scheduler
	// Reviewer 为 code_review / auto_review 提供评审器；nil 时这两个工具报告
	// "没有配置评审器"。
	Reviewer rig.Reviewer
	// Runner 为 subagent/workflow/ralph/team 提供执行器；nil 时仍生成工具，
	// 调用时报配置错误。
	Runner rig.SubagentRunner
	// Models 是遗留 list_subagent_models 工具报告的模型列表。
	Models []string
	// MCPClients 是已连接的 MCP 客户端：每个服务器贡献它的工具/资源/提示
	// （工具名形如 `mcp__<server>__<tool>`）。连接生命周期归宿主。
	MCPClients []*mcp.Client
	// MCPPrefix 覆盖 MCP 工具名前缀（默认 "mcp__"）。
	MCPPrefix string

	// ArtifactPresenter 交付 present 的**内容产物**形态（title + body → 宿主渲染），
	// 与 FilePresenter（present 的文件形态）互补。任一非 nil 时 present 不会被摘掉。
	ArtifactPresenter rig.Presenter
	// Goals 为**旧版** goal 工具提供目标状态（rig.GoalStore：Set/Get/Clear）。
	// 新版三件套（get_goal/create_goal/update_goal）需要 GoalRevisions。
	Goals rig.GoalStore
	// GoalRevisions 为 get_goal/create_goal/update_goal 提供带修订号的目标存储
	// （hosttools.GoalRevisionStore）。hosttools.NewMemoryHost() 同时满足它与 Goals。
	GoalRevisions hosttools.GoalRevisionStore
	// PlanController 为 plan_review / exit_plan_mode 提供 plan 模式控制器。
	PlanController rig.PlanController

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

	// DisableShellAccess 关闭 shell（bash/pwsh）执行：开启后不设置"允许任意程序"，
	// 且没有显式 CommandAllowlist 时所有命令都会被策略拒绝。默认 false（可用）。
	DisableShellAccess bool
	// Instructions 非空时把 AGENTS.md 层级文档拼进系统提示词末尾（在 Environment
	// 之前）。只应 Load 一次：system prompt 是请求前缀，任何字节变化都会打掉
	// provider 的 prompt 缓存。
	Instructions *instructions.Set
	// Environment 非空时把 <environment> 块拼进系统提示词（cwd/平台/日期/git）。
	Environment *instructions.Environment
	// ScratchDir 是 read-only / workspace-write 档下的**私有可写临时目录**：
	// 非空时注入到 `TMPDIR/TMP/TEMP`（`sandbox.WithScratchDir`），并作为 OS runner
	// 的可写根之一。read-only 档没有它时编译器等工具无处写临时文件（"批准了也跑
	// 不动"）。空串表示不注入（既有行为）。
	ScratchDir string

	// Hooks 是 rig.TurnHook 的透传：BeforeRun/AfterRun/BeforeTool/AfterTool。
	// kit 自己不在这些边界做隐式工作，宿主用它接快照、成本区间、事件双写与
	// 工具结果后处理。
	Hooks []rig.TurnHook
	// PromptStages/RequestStages/ResponseStages/TurnStages/CompactionStages 是
	// rig 的按运行阶段（见 rig/stages.go）：可改系统提示词、模型请求、模型响应、
	// 回合边界与压缩输入。按 Order 稳定排序后传入 agent。
	PromptStages     []rig.PromptStage
	RequestStages    []rig.RequestStage
	ResponseStages   []rig.ResponseStage
	TurnStages       []rig.TurnStage
	CompactionStages []rig.CompactionStage

	// PromptSections 是宿主追加的系统提示词区段，按顺序渲染在 SystemPrompt 之后、
	// <environment> 与仓库指令（Instructions）之前。空 Body 被忽略。
	//
	// 它解决"宿主想插一段自己的规则，但不想替掉整份 prompt"：产品语气、隐私声明、
	// 当前权限档的说明都属于这一层。要完全控制整份提示词（例如按运行重算），
	// 用 agent 层的 rig.AgentSpec.SystemPromptFunc。
	PromptSections []PromptSection
	// GitUndo 打开"每回合自动快照 + 撤销按钮"：每个 Run 开始前用 git 拍一份隐藏快照
	// （`git stash create` 语义，不动 HEAD），用户可以通过 undo 工具/UI 回退该回合的
	// 文件改动。默认 **false**：快照不是免费的（每回合要哈希变化的文件），而且它只对
	// git 工作区有意义。
	//
	// 工作区不是 git 仓库时这个开关会被静默忽略（要撤销请设置 UndoDir，走文件快照）。
	//
	// 与 UndoTracker 同时设置时 **UndoTracker 优先**（宿主自己给了 tracker 就用它，
	// GitUndo 不再单独建一个）。
	GitUndo bool

	// UndoDir 是非 git 工作区里的"回合快照目录"：工作区**不是** git 仓库、且本字段
	// 非空时，kit 挂上纯文件系统的快照后端（undo.NewFileTracker），`undo` 工具因此
	// 出现。快照内容与回合号都在这个目录里（回合号持久化在 Dir/state.json，重启后
	// 继续递增；镜像在 `Dir/turns/<turn>/files/`，清单与它是两个命名空间）。建议用
	// 工作区外的目录，或用 `<Root>/.rig/undo` 并把它加进 .gitignore。
	//
	// 文件后端构造失败（例如本字段等于 Root）时 **kit.New 返回错误**，不再静默把
	// undo 摘掉；本字段落在 Root 内时，kit 会把它（相对 Root 的实际路径）加进 git
	// 后端的忽略表，避免回合号状态文件被当成一次回合改动。
	//
	// git 仓库里 **GitUndo 仍然优先**：开了 GitUndo 时本字段只作为 git 后端的回合号
	// 持久化目录（默认 `<Root>/.rig/undo`）；没开 GitUndo 时 git 仓库不挂撤销（要撤销
	// 请用 GitUndo，而不是文件快照）。
	UndoDir string

	// UndoTracker 是宿主自建的回合快照跟踪器。
	//
	// 已废弃：优先用 GitUndo/UndoDir。保留它是为了兼容既有宿主（同一个 tracker 可以
	// 被多个 agent 共享，也能自定义 Options）。
	UndoTracker *undo.Tracker
	// EventWriter 非空时把 agent 事件桥接成 events.Envelope（NDJSON）写入它。
	// 一个 Run 一条流；Emitter 由 kit 内部创建，seq 在同一 writer 上连续。
	EventWriter io.Writer

	// Storage 是 Agent.Shutdown 最后收尾的存储（聚合契约见 storage 包）。
	//
	// 现在只需要 Close；`*session/sqlite.Store` 直接满足（它的 Close 内部先做
	// `PRAGMA wal_checkpoint(TRUNCATE)`，所以关闭之后的 .db 是完整可拷的）。
	// nil 表示没有外部存储要关。
	//
	// 宿主**自己传入**的 SessionStore 不在自动关闭范围内（同一目录单实例，其它
	// 组件可能还在用它）；要把它也收尾就在本字段里登记一次。
	Storage Storage

	// Aggregate 是「一个库装全部会话状态」的聚合契约（storage.Storage：History/
	// Checkpoints/Memory/Spill/Close）。非 nil 时 kit 从它取这四个访问器填进对应
	// 单字段（**显式设置的字段优先**），并把它登记进 Shutdown 收尾。
	Aggregate storage.Storage

	// Jobs 是宿主传入的后台任务管理器：非 nil 时 kit 用它（bash 的后台任务、job_*
	// 工具与 terminal 共用这一个），并把同一个实例经 Agent.Jobs() 交还给宿主——
	// 宿主画"后台任务"列表时读的就是工具在用的那一份，不必解析 job_list 的人读文本。
	//
	// 为 nil 时 kit 自建一个（`Agent.Jobs()` 仍然能拿到）。想额外登记别的
	// io.Closer/CloseAll 句柄，用 BackgroundJobs。
	Jobs *jobs.Manager
	// BackgroundJobs 是 Shutdown 要关掉的后台句柄（jobs.Manager / terminal.Manager
	// 都满足 kit.Background）。
	//
	// 只在宿主自建 Config.Tools 时才需要：那种情况下 kit 看不到工具背后的管理器。
	// 默认工具集由 kit 自己经 full.NewRuntime 构造，两个管理器会被自动登记。
	BackgroundJobs []Background

	// Root 是沙箱策略与根感知工具使用的工作区根目录。为空表示 DefaultRoot
	// （"."）。
	Root string
	// Roots 一次性声明多可写根：**第一个是工作区主根**（相对路径的解析基准、
	// 根感知工具的报告根），其余是同一个 Agent 可写也可读的额外根。
	//
	// 它与 Root 同时出现时以 Roots 为准并告警：两个字段都表达"工作区"，静默取其
	// 一会让宿主读不出自己配了什么。运行期增删用 Runtime.AddRoot/RemoveRoot。
	Roots []string
	// Policy 完全覆盖沙箱策略。非 nil 时，Root 仍用于定位根感知的报告类工具，但
	// Mode、AllowNetwork 与 CommandAllowlist 被忽略（这些决定归传入的策略所有）。
	Policy *sandbox.Policy
	// Sandbox 三态开关，决定要不要用系统级沙箱（默认 nil = 尽力而为）：
	//
	//	nil  有 runner 就用，没有就退回应用层校验，状态在 Preflight 里如实告知；
	//	true 希望启用；不可用时**不报错**（应用照常打开，只是 Preflight 会说明）；
	//	false 明确不要；此时不要再注入 SandboxRunner（那是自相矛盾的配置，会报错）。
	//
	// 任何环境问题（没装 bubblewrap、sandbox-exec 不可用）都**不会**让 New 失败：
	// 用户视角里"装不上沙箱"不该等于"应用打不开"。
	Sandbox *bool
	// SandboxUnavailableReason 是"为什么没有系统沙箱"给用户看的一句话（例如
	// osrunner.New 返回的错误）。kit 自己**不探测平台**（保持纯装配层），所以
	// 宿主在创建 runner 失败时把它填进来，Preflight/UI 就能给出完整答案而不是
	// 只有半句"这台机器上没有可用的系统沙箱"。
	SandboxUnavailableReason string
	// SandboxRunner 是可选的 OS 沙箱 runner（见 sandbox/osrunner）。非 nil 时
	// 默认工具集的 bash/pwsh、jobs、terminal 与 worktree 会在 spawn 前经
	// Runner.Confine 包装；失败即拒绝执行。nil 保持默认行为。仅在 Tools 为 nil
	// （使用默认 full.All 工具集）时生效。
	SandboxRunner sandbox.Runner
	// SandboxDenialSink 接收每一次被识别的沙箱拒绝（结构化 sandbox.Denial）。
	// 它挂在 policy 上，一次配置覆盖所有执行工具；回调必须并发安全。
	SandboxDenialSink func(sandbox.Denial)
	// SandboxGrants 在 Tools 为 nil（使用默认工具集）时**注册**
	// request_sandbox_access 与 sandbox_status 两个工具：前者让模型为"某个工具
	// 自己的凭据/缓存目录"申请会话级可写授权，批准后立即对所有工具生效；后者是
	// 只读诊断工具，报告当前可写面、已批准的授权与剩余预算。授权次数受
	// sandbox.WithGrantBudget 限制（默认 sandbox.DefaultGrantBudget，5 次）。
	// Tools 非 nil 时不追加（自定义工具集由宿主自行装配）。
	//
	// 安全默认：注册不等于放行。内置门禁只自动放行只读工具：sandbox_status 是只读
	// 诊断，在 gate.SandboxReadOnlyTools 里，免问；request_sandbox_access **不在**
	// 白名单里，默认配置下会被拒绝并回传给模型。放宽沙箱必须由宿主显式配置审批
	// （Approval 换为 ApprovalAuto、或提供 Ask/Review/自定义 Gate）——这是刻意的
	// 人工边界。
	//
	// 与 ToolPreset 并用没有问题：两个名字都已登记进 tools/catalog.Static 与
	// presets 的名字表，预设不会把它们过滤掉。
	//
	// **默认开**（nil = 开）：它们是 discoverable 的（不进初始 active 集），所以
	// 注册不增加每个回合的常驻成本，却让"沙箱挡住了我需要的东西"有一条**真实存在**
	// 的出口（否则拒绝建议会把模型指向一个不存在的工具——真实用户日志里正是如此）。
	// 关掉用 kit.Bool(false)。
	SandboxGrants *bool
	// PermissionPreset 一次性设置 Mode 与 Approval（DSH 的 permission-presets
	// 等价物）。可选值：read-only / workspace-write / danger-full-access / auto，
	// 语义见 Presets()。
	//
	// 优先级：**显式设置的 Gate/Mode/Approval（以及显式 Policy）优先**。任意一个
	// 被显式设置时本字段不写任何旋钮，实际档位退化为 PermissionPresetCustom
	// （"custom"：可以展示，但不能作为选择目标），并记一条 warning——绝不静默
	// 覆盖宿主的选择。预设比 Profile 更具体，所以它先于 Profile 落地。
	//
	// auto 档需要同时设置 ToolReviewer；没配时按 approval=ask 运行，Preflight 会
	// 说明这个降级。
	PermissionPreset string
	// Mode 在 Policy 为 nil 时选择权限模式：sandbox.ModeWorkspaceWrite（默认）、
	// sandbox.ModeReadOnly 或 sandbox.ModeDangerFullAccess。只读模式拒绝文件修改
	// 与 shell 执行；danger-full-access 移除工作区/允许列表围栏，只应与显式或
	// 自动审批搭配。
	//
	// 与 PermissionPreset 同时设置时本字段优先（预设退化为 custom）。
	Mode sandbox.Mode
	// ToolReviewer 是 auto 档的自动预审器（见 gate.ToolReviewer）：每次待批调用先
	// 由它判断，放行即执行；它拒绝或审查失败（超时/出错）时回退到人工 ask。没有
	// Asker 时回退到"没有审批通道"的拒绝（approval=never 下即为终局拒绝）。
	//
	// 只有生效档位是 auto（PermissionPreset="auto"，或 (danger-full-access, ask)
	// 的组合且本字段非 nil）时才被使用；其余档位忽略它。
	ToolReviewer gate.ToolReviewer
	// PTC 启用模型侧的 run_code 工具：模型编写的程序在系统已安装的 Python 3 或
	// Node.js 运行时中执行，并通过已配置的审批门禁调用其他工具。默认关闭；内层
	// 调用与普通工具调用一样走同一个 Gate。
	PTC bool
	// Approval 在 Gate 为 nil 时选择内置门禁如何处理工具调用：
	//
	//	ApprovalAsk（默认）  只读免问 + 其余问人：gate.SandboxReadOnlyTools 免审批，
	//	                    其余（写文件、bash/pwsh…）交给 Config.Asker；没有 Asker
	//	                    时 fail-closed 拒绝并说明"没有可用的审批通道"。
	//	ApprovalAuto         不问人、全部放行（gate.PersonalAuto() == AllowAll），
	//	                    与 PermissionPreset=danger-full-access 是同一组合。
	//	ApprovalNever        拒绝所有调用。
	//
	// 需要"危险命令硬拦"这一层时显式叠加 gate.CommandRiskGate；默认预设不再使用
	// 它（命令规则表的覆盖率靠不住，能不能批准应当由人决定）。
	//
	// 与 PermissionPreset 同时设置时本字段优先（预设退化为 custom）。
	Approval ApprovalMode
	// AllowNetwork 在 Policy 为 nil 时启用对外网络访问，同时决定零配置体检是否
	// 保留 `web_fetch`（配了 Searcher 时 `web_search` 也一并在）。
	//
	//	nil（默认）        → **开**：网络与 `web_fetch` 默认可用。网络本来就不在
	//	                     沙箱承诺内（见 AGENTS §5.3），能不能联网是"宿主给了
	//	                     什么工具"的问题，不该由 kit 悄悄替用户关掉；防护交给
	//	                     沙箱（可写根围栏）与审批。
	//	kit.Bool(false)    → 关：策略禁止联网，`web_fetch` 被摘掉（留着一个每次
	//	                     都被拒的联网工具只会诱导模型重试）。
	//	kit.Bool(true)     → 显式开（与 nil 等价）。
	//
	// 即使开启，`sandbox.Policy` 仍会拒绝 loopback/私有/链路本地/云元数据等目标：
	// 这是 SSRF 防护，不是"联网开关"。
	//
	// 三态写法与 ToolDiscovery/SandboxGrants/AutoAllowSandboxedCommands 一致。
	AllowNetwork *bool
	// CommandAllowlist 在 Policy 为 nil 时允许列出的 shell 命令基名。默认为空：
	// 此时若 DisableShellAccess=false，策略不限制命令名（WithAnyCommandAllowed）；
	// 若 DisableShellAccess=true，shell 执行被拒绝。
	CommandAllowlist []string
	// HiddenPaths 是对模型隐藏的路径模式（见 sandbox.WithHidden）：命中它的文件
	// 与目录在 read_file / list_dir / glob / grep / search 里都看不到，也不能写。
	//
	// 模式相对工作区根书写（`**/.env`、`secrets/**`），不含 "/" 的模式按基名匹配
	// 任意深度（`id_rsa`）。它只约束工具面，不是文件系统权限——shell 里 `cat .env`
	// 仍然成立，真正的隔离在 runner/部署层。
	//
	// Policy 非 nil 时本字段被忽略（策略由调用方拥有）。
	HiddenPaths []string
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

// effectiveMCPPrefix 返回生效的 MCP 工具名前缀：规范化规则**只有一份**，在 mcp 包里
// （trim 空白 + 空串落默认值 + 补齐 `__` 后缀分隔符）。工具名生成（full.All →
// mcp.AggregateTools）与这里的前缀匹配（splitServerBoundTools）共用它，否则带空白的
// 前缀会让服务端绑定工具一个都摘不出来，personal 预设又会静默裁掉 MCP 工具。
func effectiveMCPPrefix(prefix string) string {
	return mcp.NormalizePrefix(prefix)
}

// splitServerBoundTools 按 MCP 前缀把服务端绑定工具从内置工具里摘出来（保序）。
// 前缀为空时不做切分（调用方传的是 effectiveMCPPrefix 的结果，不会是空串）。
func splitServerBoundTools(tools []rig.Tool, prefix string) (serverBound, builtin []rig.Tool) {
	if prefix == "" {
		return nil, tools
	}
	for _, tool := range tools {
		if tool == nil {
			builtin = append(builtin, tool)
			continue
		}
		if strings.HasPrefix(tool.Name(), prefix) {
			serverBound = append(serverBound, tool)
			continue
		}
		builtin = append(builtin, tool)
	}
	return serverBound, builtin
}

// declaredRoots 把 `Config.Roots` 规整成"去掉空白与空项"的列表。空串不是有效根
// （`Config.Root` 为空时另有 `DefaultRoot` 兜底），所以这里直接丢掉。路径规范化
// 交给 `sandbox.WithRoots`：它负责绝对化、解析符号链接并丢弃打不开的根。
func declaredRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if trimmed := strings.TrimSpace(root); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
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

// undoIgnoreRules 返回 kit 给 undo 后端的忽略表。
//
// 规则由**实际的** UndoDir 推出（相对 Root 的路径），而不是写死 `.rig`：宿主把
// UndoDir 设成 Root 内别的目录时，那个目录里的 state.json 同样会变成一次"回合改动"，
// 被 undo 当成可回退的文件（Revert 会把回合号写回旧值）。`.rig` 始终保留：kit 自己
// 的 spill 默认也在 `<Root>/.rig/spill`。UndoDir 在 Root 外时不追加任何规则。
func undoIgnoreRules(root, undoDir string) []string {
	rules := []string{".rig"}
	trimmed := strings.TrimSpace(undoDir)
	if trimmed == "" {
		return rules
	}
	absRoot, rootErr := filepath.Abs(root)
	absDir, dirErr := filepath.Abs(trimmed)
	if rootErr != nil || dirErr != nil {
		return rules
	}
	rel, err := filepath.Rel(filepath.Clean(absRoot), filepath.Clean(absDir))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rules
	}
	rule := filepath.ToSlash(rel)
	if rule == ".rig" || strings.HasPrefix(rule, ".rig/") {
		return rules
	}
	return append(rules, rule)
}

// New 根据 cfg 装配一个可直接运行的 Agent。
//
// cfg.Model 为空时返回包装 rig.ErrModelRequired 的错误；无法构造默认 provider 时
// 返回 provider/配置错误；自定义 Tool 集合非法（nil 工具、空名字或重名）时返回
// 校验错误。kit 自行构造沙箱策略时，工作区根目录不存在同样是错误。
//
// New 是"一次性宿主"的入口：它自己构造并在 Agent.Shutdown 里收尾全部资源。
// 要在多个会话/子代理之间共享 provider、后台任务与 PTY 池，用 Runtime（见
// runtime.go）；两条路径共用同一份装配代码，行为逐字节一致。
func New(cfg Config) (*Agent, error) {
	return newAgent(cfg, nil, "")
}

// newAgent 是 New 与 Runtime.NewAgent 共用的装配实现。
//
// rt 为 nil 表示"一次性装配"：所有资源都由本 Agent 构造并在 Shutdown 里收尾。
// rt 非 nil 时调用方必须持有 rt.mu，进程级资源（provider、jobs.Manager、
// terminal.Manager、记忆 wiki、spill store、宿主能力通道）取自 rt，且**不**登记
// 进本 Agent 的收尾清单——它们的生命周期归 Runtime。
// owner 是 Runtime 给这个 Agent 分配的调用方标识（`kit.New` 为空）：共享的
// jobs/terminal 管理器据此解析本 Agent 的围栏。
func newAgent(cfg Config, rt *Runtime, owner string) (*Agent, error) {
	// 聚合存储先展开成单字段（显式设置的优先）：后面所有路径都只认单字段，不必
	// 在每个分支里再判断"要不要看 Aggregate"。原始 Aggregate 字段是否出现要
	// 先记下来：展开之后它就被搬进了 Storage（审计 D1 #5）。
	hadAggregate := cfg.Aggregate != nil
	applyAggregateStorage(&cfg)
	// 权限预设比"配置档"更具体，所以先落地：它一次性设置 Mode 与 Approval，且只在
	// 宿主**没有**显式设置 Gate/Mode/Approval（也没有传 Policy）时生效——显式设置
	// 优先，此时实际档位退化为 custom，kit 不静默覆盖任何字段。
	presetResolution, err := applyPermissionPreset(&cfg)
	if err != nil {
		return nil, err
	}
	// 配置档随后落地：档只填宿主没显式设置、预设也没写的字段。
	if err := applyProfile(&cfg); err != nil {
		return nil, err
	}

	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("kit: %w", rig.ErrModelRequired)
	}
	// 审批记忆作用域只认两个值：写错了当场报错，而不是静默按 session 跑（那会
	// 让"怎么换会话还要问"变成一次无法解释的行为）。
	approvalScope, err := normalizeApprovalScope(cfg.ApprovalScope)
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(cfg.Root)
	roots := declaredRoots(cfg.Roots)
	if len(roots) > 0 {
		// Roots 比 Root 更具体（一次声明整份可写面），所以它优先；但静默忽略
		// Root 会让宿主以为"两个都在用"，必须告警并点名两个字段。
		if root != "" {
			slog.Warn("kit: 同时设置了 Config.Root 与 Config.Roots，以 Config.Roots 为准（第一个是工作区主根）",
				"Config.Root", root, "Config.Roots", len(roots))
		}
		root = roots[0]
	} else {
		if root == "" {
			root = DefaultRoot
		}
		roots = []string{root}
	}
	// 静态声明的追加根（不含主根）：显式 Config.Policy 时由 kit 追加进宿主的策略。
	declaredExtras := append([]string(nil), roots[1:]...)
	// Runtime 级别追加的可写根（Runtime.AddRoot）：只影响构造期之后新建的 Agent，
	// 已存在的 Agent 由 Runtime.RemoveRoot 单独撤销。
	var runtimeRoots []string
	if rt != nil {
		runtimeRoots = rt.extraRootsLocked()
		roots = append(roots, runtimeRoots...)
		// 撤销对静态根同样持久：同一份 Config 再建 Agent 时被撤销的根不复活。
		filtered, err := rt.filterRevokedRootsLocked(roots)
		if err != nil {
			return nil, err
		}
		roots = filtered
		if len(declaredExtras) > 0 {
			if kept, err := rt.filterRevokedRootsLocked(append([]string{root}, declaredExtras...)); err != nil {
				return nil, err
			} else {
				declaredExtras = append([]string(nil), kept[1:]...)
			}
		}
	}
	// 共享资源先落地：provider、记忆 wiki、spill、宿主能力通道由 Runtime 持有。
	// 与共享资源冲突的生命周期字段在这里直接报错（不静默覆盖宿主的句柄）。
	if rt != nil {
		if err := rt.applySharedLocked(&cfg, root, hadAggregate); err != nil {
			return nil, err
		}
	}
	// 注入即失效（审计 A1）：自建工具集**不会**经过 OS 沙箱包装，但 SandboxRunner
	// 仍会被 PTC 使用——同一份配置出现两种行为，而用户以为"还在沙箱里"。
	// 这是"以为有保护其实没有"，必须构造期报错，不能只告警。
	if cfg.Tools != nil && cfg.SandboxRunner != nil {
		return nil, errors.New("kit: Config.Tools 与 Config.SandboxRunner 不能同时设置——" +
			"自建工具集不会经过 OS 沙箱包装；请在 full.All(AllOptions{SandboxRunner: …}) 里挂 runner，或不要传 Tools")
	}
	// 沙箱三态：只决定"用不用注入的 runner"，绝不因为"本机没有沙箱"而失败。
	// 解析结果直接写回 cfg，后续所有使用者（工具集、PTC、状态工具）口径一致。
	runner, sandboxExplicit, err := resolveSandbox(cfg)
	if err != nil {
		return nil, err
	}
	cfg.SandboxRunner = runner
	// <=0 表示不限制轮数（unbounded）；负值归一化为 0。未配置时不再补默认总闸：
	// 轮数护栏是人为边界，成本控制交给 costmeter 与人工审批（见 DefaultSafeMaxIterations）。
	maxIterations := cfg.MaxIterations
	if maxIterations == 0 {
		maxIterations = DefaultSafeMaxIterations // 现在等于 0（不限）
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
	if undoTracker == nil && (cfg.GitUndo || strings.TrimSpace(cfg.UndoDir) != "") {
		// git 工作区优先用 git 隐藏快照；不是仓库时，只有宿主明确了 UndoDir 才挂
		// 文件系统快照（否则安静地不提供撤销，而不是每回合往事件流里打一条失败）。
		isRepo := false
		if yes, err := undo.IsGitRepo(context.Background(), root); err == nil {
			isRepo = yes
		}
		undoDir := strings.TrimSpace(cfg.UndoDir)
		switch {
		case cfg.GitUndo && isRepo:
			gitDir := undoDir
			if gitDir == "" {
				gitDir = filepath.Join(root, ".rig", "undo")
			}
			// 回合号写在 gitDir/state.json 里：把**实际的** UndoDir（相对 Root 时）
			// 加进忽略表，否则它自己会变成一次"回合改动"，被 undo 当成可回退的文件
			// （Revert 会把回合号写回旧值）。`.rig` 始终保留：kit 自己的 spill 也用它。
			undoTracker = undo.NewTracker(root, undo.Options{Dir: gitDir, Ignore: undoIgnoreRules(root, gitDir)})
		case !isRepo && undoDir != "":
			tracker, trackerErr := undo.NewFileTracker(root, undo.FileOptions{Dir: undoDir})
			if trackerErr != nil {
				// 配置错误（例如 UndoDir 就是 Root）不能静默吞掉：宿主会以为撤销开着。
				return nil, fmt.Errorf("kit: undo: cannot create the file snapshot tracker for UndoDir %q: %w", undoDir, trackerErr)
			}
			undoTracker = tracker
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
	// 生效档位名：显式选择的预设 > (Mode, Approval[, ToolReviewer]) 反查 > custom。
	// 它在门禁组合与 Agent.PermissionPreset() 之间共用同一份推导，避免"报告一个档、
	// 实际跑另一个档"。档位在策略构造之后按 **policy.Mode()** 推导（A6 #3：与围栏、
	// 工具面裁剪同源；cfg.Mode 只在没有显式 Policy 时才是真档位）。

	sandboxGrants := cfg.SandboxGrants == nil || *cfg.SandboxGrants
	// 联网同样三态：nil = 开（默认），显式 false 才关。口径写回 cfg，后续所有
	// 使用者（策略、体检）共用同一个值。
	allowNetwork := cfg.AllowNetwork == nil || *cfg.AllowNetwork

	policy := cfg.Policy
	// policyOwned 记录策略是不是 kit 自己建的：Runtime.RemoveRoot 只撤销 kit 自己
	// 拥有的策略（宿主传进来的 Policy 归宿主所有，kit 不替它做决定）。
	policyOwned := cfg.Policy == nil
	// kitAppendedRoots 是 kit 追加到**宿主策略**上的可写根（Config.Roots[1:]）。
	// 策略本身仍归宿主，但这几根是 kit 声明的，所以 RemoveRoot 可以撤销它们。
	var kitAppendedRoots []string
	if policy == nil {
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("kit: workspace root %q: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("kit: workspace root %q is not a directory", root)
		}
		for _, extra := range roots[1:] {
			// 追加根同样 fail-closed：不存在就构造期报错，而不是让策略悄悄少一根。
			extraInfo, err := os.Stat(extra)
			if err != nil {
				return nil, fmt.Errorf("kit: root %q: %w", extra, err)
			}
			if !extraInfo.IsDir() {
				return nil, fmt.Errorf("kit: root %q is not a directory", extra)
			}
		}
		// 联网默认**开**（nil = 开）；只有显式 kit.Bool(false) 才关。
		opts := []sandbox.Option{
			sandbox.WithRoots(roots...),
			sandbox.WithMode(mode),
			sandbox.WithNetwork(allowNetwork),
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
		if len(cfg.HiddenPaths) > 0 {
			// "别让模型看到这些"是产品级配置，不该逼宿主自己构造策略。
			opts = append(opts, sandbox.WithHidden(cfg.HiddenPaths...))
		}
		if cfg.ScratchDir != "" {
			opts = append(opts, sandbox.WithScratchDir(cfg.ScratchDir))
		}
		policy = sandbox.New(opts...)
	} else {
		// 宿主显式 Policy + Config.Roots：Roots[1:] **不能**静默丢弃（文档承诺"每个
		// 根都必须存在且是目录，否则构造期报错"，静默丢掉让该承诺在该组合下失效）。
		// 它们作为追加可写根声明进宿主的策略，并记进 kitAppendedRoots 以便 RemoveRoot
		// 能撤销 kit 声明的这一部分。
		for _, extra := range declaredExtras {
			extraInfo, err := os.Stat(extra)
			if err != nil {
				return nil, fmt.Errorf("kit: root %q: %w", extra, err)
			}
			if !extraInfo.IsDir() {
				return nil, fmt.Errorf("kit: root %q is not a directory", extra)
			}
			// 记录**规范化**路径：Runtime.RemoveRoot 用绝对+解析符号链接的路径比较，
			// 存原始声明串（可能是相对路径）会让撤销匹配不上。
			resolved, err := absoluteRoot(extra)
			if err != nil {
				return nil, err
			}
			policy.DeclareWritableRoot(resolved)
			kitAppendedRoots = append(kitAppendedRoots, resolved)
		}
		if len(runtimeRoots) > 0 {
			// Runtime 级别的追加根（Runtime.AddRoot）同样声明进这份宿主策略：AddRoot
			// 对**已存在**的 Agent 就是直接改这份策略，新建的 Agent 必须拿到同一个根，
			// 否则"给这个 Runtime 加根"的承诺会随 Agent 的创建时间不同而不同。代价
			// 同样是会修改宿主传入的对象，所以留一条告警。
			for _, extra := range runtimeRoots {
				policy.DeclareWritableRoot(extra)
				if !slices.ContainsFunc(kitAppendedRoots, func(existing string) bool {
					return sandbox.PathEqual(existing, extra)
				}) {
					kitAppendedRoots = append(kitAppendedRoots, extra)
				}
			}
			slog.Warn("kit: 新建的 Agent 把 Runtime.AddRoot 的根声明进了宿主经 Config.Policy 传入的策略（策略归宿主所有，kit 会修改它）；不想要这个行为就不要在 Runtime 级别加根",
				"runtimeRoots", len(runtimeRoots))
		}
	}

	if cfg.SandboxDenialSink != nil {
		policy.SetDenialSink(cfg.SandboxDenialSink)
	}
	// 档位只有一个来源：策略的生效模式。显式传 Policy 时策略才真正围栏，cfg.Mode
	// 只在没有 Policy 时被用来构造策略；两者不一致时以策略为准，否则 UI 会报一个
	// 档、裁剪与围栏跑另一个档（审计 A6 #3）。
	effectivePreset := derivePermissionPreset(cfg, presetResolution, policy.Mode(), approval)
	// 权限只由 Mode/PermissionPreset 与显式 Policy 表达：kit **不**追加任何保护
	// 路径（策略层也没有这个概念）。根内局部只读由调用方自己用可写根的
	// ReadOnlySubpaths 表达。
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
	// 共享后台句柄：Runtime 形态下 jobs.Manager / terminal.Manager 是**跨 Agent**
	// 的一份（首次构造时用这份策略与 runner 建出来），所以它们不进本 Agent 的
	// 收尾清单——那是 Runtime.Close 的责任。
	var sharedTerminals *terminal.Manager
	if rt != nil {
		cfg.Jobs = rt.jobsLockedFor()
		sharedTerminals = rt.terminalsLockedFor(cfg.Jobs)
	}
	// 后台句柄：宿主显式登记的（Config.BackgroundJobs）＋ kit 自己经 full.NewRuntime
	// 拿到的（jobs.Manager / terminal.Manager）。旧的 full.All 会把这两个管理器就地
	// 丢掉，于是生产路径上没有任何 CloseAll 调用；NewRuntime 把句柄交出来，
	// Shutdown 才能统一收尾。
	var background []Background
	background = append(background, cfg.BackgroundJobs...)
	var (
		agentJobs      *jobs.Manager
		agentTerminals *terminal.Manager
	)
	if toolSet == nil {
		runtime, err := full.NewRuntime(full.AllOptions{
			// Owner 让 job_start / terminal_open / terminal_send 按**本 Agent**
			// 的围栏执行（共享管理器不再绑死第一个 Agent 的策略）。
			Owner:         owner,
			Policy:        policy,
			Root:          root,
			SandboxRunner: cfg.SandboxRunner,
			UndoTracker:   undoTracker,
			Searcher:      cfg.Searcher,
			SessionLog:    cfg.SessionLog,
			Asker:         cfg.Asker,
			// Presenter 是 present 工具的宿主实现。漏传会让 present 被保留（因为
			// Config.Presenter 非 nil 会跳过体检）却不工作：每次调用都返回
			// "the host did not provide a rig.FilePresenter implementation"。
			Presenter:      cfg.Presenter,
			Wiki:           memoryWiki,
			SandboxGrants:  sandboxGrants,
			Escalator:      approver,
			SpillStore:     spillStoreOf(cfg.Spill),
			BackgroundJobs: cfg.Jobs,
			Skill:          cfg.Skills,
			TeamTasks:      cfg.TeamTasks,
			Scheduler:      cfg.Scheduler,
			Reviewer:       cfg.Reviewer,
			Runner:         cfg.Runner,
			Models:         cfg.Models,
			MCPClients:     cfg.MCPClients,
			// 交给 mcp 的必须是**已规范化**的前缀：下面 splitServerBoundTools 用的是
			// 同一个值，两侧不能各算一遍（审计 D2 #6）。
			MCPPrefix: effectiveMCPPrefix(cfg.MCPPrefix),
			Artifact:  cfg.ArtifactPresenter,
			Host: hosttools.Options{
				Root:         root,
				Goal:         cfg.Goals,
				GoalRevision: cfg.GoalRevisions,
				Plan:         cfg.PlanController,
			},
			// 共享的 PTY 池：Runtime 形态下终端会话句柄跨 Agent 可见。
			BackgroundTerminals: sharedTerminals,
		})
		if err != nil {
			return nil, fmt.Errorf("kit: tools: %w", err)
		}
		toolSet = runtime.Tools
		// 两个管理器都要收进 Shutdown：宿主关窗口时后台 bash 与 PTY 会话必须一起死。
		// 同时把它们留在 Agent 上：宿主画后台任务/终端列表时读的必须是工具在用的
		// 那一份（否则注入与自建两条路径都会让宿主拿到"空的管理器"）。
		if rtJobs := runtime.Jobs(); rtJobs != nil {
			agentJobs = rtJobs
			if rt == nil {
				// 一次性装配：句柄归本 Agent，Shutdown 要关它。
				background = append(background, rtJobs)
			}
		}
		if rtTerminals := runtime.Terminals(); rtTerminals != nil {
			agentTerminals = rtTerminals
			if rt == nil {
				background = append(background, rtTerminals)
			}
		}
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
				statusOpts = append(statusOpts,
					sandboxstatus.WithRunnerInfo(
						func() (string, sandbox.Enforcement) {
							name, enforcement, _ := sandbox.RunnerStatus(runner)
							return name, enforcement
						}),
					// 运行时诊断：缓存白名单生效了没有、网络断没断、哪些宿主
					// 守护进程 socket 被屏蔽——审计时不必再靠 go build 试错。
					sandboxstatus.WithRuntimeView(func() sandboxstatus.RuntimeView {
						report := osrunner.DescribeRuntime(policy, runner)
						return sandboxstatus.RuntimeView{
							ToolchainCaches:    report.ToolchainCaches,
							MissingCaches:      report.MissingToolchainCaches,
							MaskedSockets:      report.MaskedSockets,
							NetworkCoverage:    report.NetworkCoverage,
							PrivateTmpDisabled: report.PrivateTmpDisabledReason,
						}
					}))
			}
			toolSet = appendUniqueTools(toolSet,
				sandboxgrant.New(policy),
				sandboxstatus.New(policy, statusOpts...),
			)
		}
	}
	// 自建工具集（Config.Tools 非 nil）时 kit 不构造作业管理器，但宿主注入的那个
	// 仍然要"被记住"：Agent.Jobs() 要能读回同一个实例，Shutdown 也要关它——否则
	// 宿主以为交出去了，实际既读不到也关不掉。
	if agentJobs == nil && cfg.Jobs != nil {
		agentJobs = cfg.Jobs
		// Runtime 形态下这个句柄是共享的：登记进 Shutdown 会让一个 Agent 的
		// Shutdown 杀掉别的 Agent 正在用的后台任务。
		if rt == nil {
			background = append(background, cfg.Jobs)
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
		if cfg.GitUndo || cfg.UndoTracker != nil || strings.TrimSpace(cfg.UndoDir) != "" {
			ignored = append(ignored, "GitUndo/UndoDir/UndoTracker（回合快照照打，但自建工具集里没有 undo 工具）")
		}
		if cfg.Asker != nil {
			ignored = append(ignored, "Asker 的 ask_user 工具不会出现（审批门禁仍会使用 Asker）")
		}
		if cfg.SandboxGrants != nil && *cfg.SandboxGrants {
			ignored = append(ignored, "SandboxGrants（sandbox_status/request_sandbox_access 不会出现）")
		}
		if cfg.Activations != nil {
			// 自建工具集默认不开 discovery（见下面的 discoveryEnabled），全部工具
			// 一开始就 active，没有"已激活"需要持久化：store 会透传给 agent，但既
			// 不 Load 也不 Save。
			ignored = append(ignored, "Activations（自建工具集默认不开 discovery：全部工具 active，没有激活集可持久化；显式开 discovery 时才生效）")
		}
		if policy.Mode() == sandbox.ModeReadOnly {
			// 自建工具集不做 Mode 裁剪：宿主自己挑的工具不能被 kit 按档位静默
			// 摘掉，但"read-only 档下 write_file 还在"必须说清楚是刻意的。
			ignored = append(ignored, "Mode 裁剪（read-only 档下纯写工具/undo/无 runner 的 shell 工具不注册）对自建工具集不生效")
		}
		if cfg.Skills != nil || cfg.TeamTasks != nil || cfg.Scheduler != nil ||
			cfg.Reviewer != nil || cfg.Runner != nil || len(cfg.MCPClients) > 0 || len(cfg.Models) > 0 || cfg.MCPPrefix != "" {
			ignored = append(ignored, "Skills/TeamTasks/Scheduler/Reviewer/Runner/MCPClients/MCPPrefix/Models（工具是宿主自建的，这些注入不生效；Jobs 是例外，句柄仍会被登记）")
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
	// MCP/服务端绑定工具**不**属于名字级预设：预设只列内置工具名，而 MCP 工具的
	// 名字由服务器身份在运行期决定。把它们按前缀摘出来、过滤之后原样放回，否则
	// 宿主显式接进来的 MCP 客户端会被默认预设静默丢掉（工具面看起来根本没有它）。
	var serverBoundTools []rig.Tool
	if cfg.Tools == nil && len(cfg.MCPClients) > 0 {
		serverBoundTools, toolSet = splitServerBoundTools(toolSet, effectiveMCPPrefix(cfg.MCPPrefix))
	}
	if preset != "" {
		filtered, err := presets.Filter(preset, toolSet)
		if err != nil {
			return nil, fmt.Errorf("kit: %w", err)
		}
		toolSet = filtered
	}
	toolSet = append(toolSet, serverBoundTools...)
	// 模块/插件注入的工具：放在预设与模式裁剪之后，语义是"宿主明确追加"。
	if len(cfg.ExtraTools) > 0 {
		toolSet = appendUniqueTools(toolSet, cfg.ExtraTools...)
	}

	// 权限模式再裁一次工具面（A6）：read-only 档下纯写工具与 undo **不注册**，而不是
	// "注册了、模型点开才被拒"——后者白烧一轮上下文与一次工具往返。裁剪表在
	// tools/presets（ModeFilter），这里只接线，不判断哪些工具算写工具。
	// 档位取**策略**的生效模式而不是 cfg.Mode：宿主显式传 Policy 时策略才真正
	// 围栏，两者可能不同（预设只在没传 Policy 时才写 cfg.Mode）。
	// shell 类工具（bash/pwsh/run_code）只在挂了 OS runner 时才保留：没有 runner 的
	// read-only 里策略拒绝所有命令，留着它们是必然失败的死工具（A6 #2）。
	// Config.Tools 非 nil 时不动：那是宿主的选择（见上面的告警）。
	keepTool := presets.ModeFilter(string(policy.Mode()), cfg.SandboxRunner != nil)
	if cfg.Tools == nil {
		trimmed := make([]rig.Tool, 0, len(toolSet))
		for _, tool := range toolSet {
			// nil 工具留给 agent.New 报校验错误，不在这里吃掉。
			if tool == nil || keepTool(tool.Name()) {
				trimmed = append(trimmed, tool)
			}
		}
		toolSet = trimmed
	}

	// 事件桥要在门禁之前构造：审批观察者（approval.request/response）需要它，
	// 否则"正在等用户确认"这类状态进不了事件流，UI 无从显示。
	bridge := newEventBridgeFor(cfg.EventWriter, cfg.Meter, cfg.Model)
	// 事件桥晚于工具集构造：把提权通道的审计出口补上（允许与拒绝都要留痕）。
	approver.bridge = bridge

	// 审批记忆是否真的挂在这份装配上（宿主自定义 Gate、approval=never 时不挂）。
	// 它在门禁构造之前就要定下来：挂了记忆时必须关掉 AskGate 自带的"按工具名
	// allow-always"，否则点一次 write_file a.txt 会把同工具任意参数、任意会话一起
	// 放行（见 gate.WithoutToolMemory）。
	memStore := approvalMemoryStore(cfg.Approvals, cfg.Gate == nil, approval)

	gateImpl := cfg.Gate
	// askInner 是最内层的审批门禁（问人，或"没有通道"的拒绝兜底）：审批记忆按 Run
	// 直接贴在它上面，"问过没有"只由记忆判定。gateLayer 以它为参数重建完整门禁
	// （PersonalAsk + 只读/已沙箱化免问 + auto 档的 review 包装），三者都只在 kit
	// 自己的 ask 装配下非 nil。
	var (
		askInner  rig.Gate
		gateLayer func(rig.Gate) rig.Gate
	)
	if gateImpl == nil {
		// buildAsk 把"最内层审批门禁"组装成 ask 档的完整门禁：只读工具免问 +
		// 已沙箱化命令免问 + 其余问人。抽成函数是因为每个 Run 都要把审批记忆换成
		// 那一轮的会话快照，而外层组合必须与构造期完全一致（同一个 AskGate 实例）。
		buildAsk := func(inner rig.Gate) rig.Gate {
			return gate.PersonalAsk(sandboxedAllowance(cfg.AutoAllowSandboxedCommands, cfg.SandboxRunner, policy, bridge, inner))
		}
		switch approval {
		case ApprovalAuto:
			// 自动模式 = gate.PersonalAuto() = **AllowAll()**：不问人、不分类命令，
			// 全部放行。它对应 DSH 的 danger-full-access 档（全权）。要保留命令级
			// 拦截，宿主必须显式叠加 gate.CommandRiskGate。
			gateImpl = gate.PersonalAuto()
		case ApprovalNever:
			gateImpl = gate.PersonalNever()
		default:
			// 审批模式：只读工具免问；其余（写文件、bash/pwsh，含危险命令）交给
			// 人工。宿主没接 Asker 时用"没有审批通道"的兜底，而不是假装
			// approval=never（那会让模型误以为用户主动禁止了一切）。
			if cfg.Asker != nil {
				// 审批默认**不超时**（与 DSH 的 approval seam 一致：人多久不回就
				// 一直挂着，只有调用方取消轮次才会撤回请求）。显式设置
				// Config.AskTimeout>0 时才给 gate.Ask 一个内部截止时间。
				opts := []gate.AskOption{
					// 默认一条（SandboxGrantPrompt，现有行为）；宿主配了
					// Config.ApprovalPromptChain 时按配置顺序拼接。
					gate.WithAskPrompt(approvalPrompt(cfg.ApprovalPromptChain)),
				}
				if cfg.AskTimeout > 0 {
					opts = append(opts, gate.WithAskTimeout(cfg.AskTimeout))
				}
				if memStore != nil {
					// 记忆层负责"问过没有"：AskGate 不再按工具名 allow-always。
					opts = append(opts, gate.WithoutToolMemory())
				}
				if bridge != nil {
					opts = append(opts, gate.WithAskObserver(func(event gate.AskEvent) {
						bridge.emitApproval(event)
					}))
				}
				askInner = gate.Ask(cfg.Asker, opts...)
			} else {
				// 没有审批通道时，"已沙箱化"尤其重要：否则 headless 宿主连 `mkdir`
				// 都跑不了。围栏成立就放行（并留痕），不成立才走"没有审批通道"的拒绝。
				askInner = gate.DenyAllWithReason(gate.DenyAllNoChannelReason)
			}
			gateLayer = buildAsk
			gateImpl = buildAsk(askInner)
		}
		// DSH 的 auto 档：先自动预审，reviewer 放行即执行、拒绝或审查失败都回到
		// 人工 ask。没有 ToolReviewer 时退化为纯 ask（Preflight 会说明这个降级）。
		if effectivePreset == PermissionPresetAuto && cfg.ToolReviewer != nil {
			gateImpl = autoReviewGate(cfg.ToolReviewer, gateImpl)
			if gateLayer != nil {
				// 每轮重建时也要把 review 包装套回去：记忆在最内层，外层的预审
				// 不能被绕过。
				askLayer := gateLayer
				gateLayer = func(inner rig.Gate) rig.Gate {
					return autoReviewGate(cfg.ToolReviewer, askLayer(inner))
				}
			}
		}
	}

	// run_code 与 bash 同档：没有 runner 的 read-only 里它同样必定被策略拒绝，
	// 因此与其它 shell 类工具共用同一个谓词（Config.Tools 非 nil 时不裁）。
	if cfg.PTC && (cfg.Tools != nil || keepTool("run_code")) {
		// 内层工具调用的门禁在**每次 Run** 时从 context 取（run_code 是构造期
		// 创建的，拿不到 RunRequest）：这样内层与外层走同一条规则源，审批记忆与
		// 本轮 memo 对内层调用同样生效。ctx 上没有本轮门禁时（宿主经 Agent.Tool
		// 直接调用工具）回退到构造期这条。
		toolSet = append(toolSet, ptc.New(ptc.Config{
			Tools:  slices.Clone(toolSet),
			Gate:   gateImpl,
			Root:   root,
			Runner: cfg.SandboxRunner,
			Policy: policy,
			GateSource: func(ctx context.Context) rig.Gate {
				if current, ok := runGateFrom(ctx); ok {
					return current
				}
				return gateImpl
			},
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

	// 零配置体检**只作用于 kit 自己构建的默认工具集**：宿主传了 Config.Tools，
	// 那些工具是宿主的选择，kit 无从判断它们背后的依赖，也不该按自己的装配状态
	// 静默摘掉（否则"我明明传了这个工具"会变成一次无法解释的缺席）。
	// 默认集里，personal 预设有 15 个工具在 kit 默认装配下**永远无法成功**
	// （缺 Searcher / SessionLog / GoalStore 与 PlanController 注入入口 /
	// Presenter / Asker / 网络授权，或非 Windows 平台的 pwsh），它们只会占
	// system prompt 并诱导模型重试。原则：默认工具集里**每个工具都必须真的能用**。
	// 具体名单见 cfgUnavailableTools。
	if cfg.Tools == nil {
		for _, name := range cfgUnavailableTools(cfg) {
			toolSet = withoutTool(toolSet, name)
		}
		if undoTracker == nil {
			// 没有回合快照就不该把 undo 暴露给模型：它只会返回"没有可用快照"，
			// 白白浪费一个回合。宿主配置 GitUndo 或 UndoTracker 后，full.All 会自动
			// 挂载它。
			toolSet = withoutTool(toolSet, "undo")
		}
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
					// 工具自己声明"不可发现"（rig.DiscoverableTool）时不进清单：宿主
					// 仍可用 ActivateTools 按名字激活，模型也能直接调用已激活的它。
					if !rig.IsToolDiscoverable(tool) {
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
	systemPrompt := assembleSystemPrompt(cfg.SystemPrompt, cfg.Environment, cfg.Instructions, promptRoot, cfg.PromptSections...)

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
		Spill:              spillPolicyFor(cfg.Spill, promptRoot),
		CheckpointStore:    cfg.CheckpointStore,
		ActivationStore:    cfg.Activations,
		Hooks:              cfg.Hooks,
		PromptStages:       cfg.PromptStages,
		RequestStages:      cfg.RequestStages,
		ResponseStages:     cfg.ResponseStages,
		TurnStages:         cfg.TurnStages,
		CompactionStages:   cfg.CompactionStages,
	}
	underlying, err2 := agent.New(spec)
	if err2 != nil {
		return nil, fmt.Errorf("kit: %w", err2)
	}
	// 把动态加载能力接上：tool_search 的回调与 Agent.ActivateTools 都走它。
	if candidate, ok := underlying.(rig.ToolActivator); ok {
		toolActivator = candidate
		activeTool = func(sessionID string) []string { return candidate.ActiveTools(sessionID) }
	}
	// 生命周期：在飞回合的取消、后台句柄与存储的关闭都挂在这里（见 shutdown.go）。
	life := newLifecycle()
	life.background = background
	if cfg.Storage != nil {
		life.stores = append(life.stores, cfg.Storage)
	}
	// kit 只关**自己打开的** store.Dir；宿主传入的 Config.SessionStore 归宿主
	// （同一目录单实例，其它组件可能还在用它）。
	if storeDir != nil && cfg.SessionStoreDir != "" {
		life.stores = append(life.stores, storeDir)
	}
	return &Agent{
		agent:                    underlying,
		compactor:                compactor,
		contextBudget:            contextBudget,
		history:                  history,
		undo:                     undoTracker,
		events:                   bridge,
		meter:                    cfg.Meter,
		model:                    cfg.Model,
		store:                    storeDir,
		maxRunCost:               cfg.MaxRunCostUSD,
		gate:                     gateImpl,
		approvals:                memStore,
		askInner:                 askInner,
		gateLayer:                gateLayer,
		approvalScope:            approvalScope,
		approvalPrompts:          append([]gate.Prompt(nil), cfg.ApprovalPromptChain...),
		policy:                   policy,
		policyOwned:              policyOwned,
		kitAppendedRoots:         kitAppendedRoots,
		memoryWiki:               memoryWiki,
		memoryWikiErr:            memoryWikiErr,
		activator:                toolActivator,
		registeredNames:          toolNamesOf(toolSet),
		runner:                   cfg.SandboxRunner,
		askerAvailable:           cfg.Asker != nil && approval == ApprovalAsk,
		permissionPreset:         effectivePreset,
		autoDegraded:             effectivePreset == PermissionPresetAuto && cfg.ToolReviewer == nil,
		sandboxRequested:         sandboxExplicit,
		sandboxUnavailableReason: cfg.SandboxUnavailableReason,
		life:                     life,
		eventWriter:              cfg.EventWriter,
		jobs:                     agentJobs,
		terminals:                agentTerminals,
		toolByName:               toolMapOf(toolSet),
		// 非 nil 表示本 Agent 的进程级资源归一个 Runtime：Runtime.Close 之后
		// Run 直接拒绝（见 Run 入口的 ErrRuntimeClosed 检查）。
		runtime: rt,
		owner:   owner,
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
	// policyOwned 表示 policy 是 kit 构造的（而不是宿主经 Config.Policy 交来的）：
	// Runtime.RemoveRoot 只撤销它自己的策略。
	policyOwned bool
	// kitAppendedRoots 是 kit 追加到**宿主策略**上的可写根（显式 Config.Policy 时的
	// Config.Roots[1:]）。Runtime.RemoveRoot 只撤销这一部分，策略的其余部分归宿主。
	kitAppendedRoots []string
	meter            *costmeter.Meter
	model            string
	store            *store.Dir
	// maxRunCost > 0 时对每次 Run 做花费护栏（需要 Meter）。
	maxRunCost float64
	// gate 是构造期决定的门禁（Config.Gate 或由 Approval 拼出来的组合）。每次 Run
	// 会临时在它外面叠审批记忆与本轮去重（见 approvals.go 的 gatewayForRun）。
	gate rig.Gate
	// approvals / approvalScope 是审批记忆的 store 与作用域（nil store = 不记忆）。
	approvals     rig.ApprovalStore
	approvalScope string
	// askInner / gateLayer 是 kit 自建 ask 装配的最内层审批门禁与它的外层组合。
	// 审批记忆按 Run 直接贴在 askInner 上（记忆在最内层），再用 gateLayer 重建外层，
	// 因此"问过没有"只有一个判定者，AskGate 只回答"要不要问"。非 ask 装配
	// （auto/never/宿主自定义 Gate）时两者为 nil。
	askInner  rig.Gate
	gateLayer func(rig.Gate) rig.Gate
	// approvalPrompts 是 Config.ApprovalPromptChain 的原值（nil = 默认链），
	// 只用于 Preflight 如实报告"当前用的是哪条链"。
	approvalPrompts []gate.Prompt
	// activator 是底层 agent 的动态工具加载能力（rig.ToolActivator）；discovery
	// 关闭时底层 agent 仍然实现它（InitialActiveTools=nil 时全部 active），因此
	// 这里总是非 nil。
	activator rig.ToolActivator
	// registeredNames 是**全部注册**的工具名（排序），与 active 集合无关。
	registeredNames []string
	// runner / askerAvailable / sandboxRequested 是预检（Preflight）需要的三项事实：
	// 实际生效的 runner、当前组合里能不能问人、用户有没有明确要求隔离。
	runner           sandbox.Runner
	askerAvailable   bool
	sandboxRequested bool
	// sandboxUnavailableReason 是"为什么没有沙箱"的可选说明（来自
	// Config.SandboxUnavailableReason；kit 不探测平台）。
	sandboxUnavailableReason string
	// permissionPreset 是构造时推导出的生效权限档位名（四档之一或 "custom"），
	// 由 PermissionPreset() 暴露给设置界面。
	permissionPreset string
	// autoDegraded 报告"选了 auto 但没配 ToolReviewer"：此时按 approval=ask 运行，
	// Preflight 会说明这个降级。
	autoDegraded bool
	// memoryWiki 是 memory 工具实际使用的存储；memoryWikiErr 记录"自动建文件 wiki
	// 失败后回落到进程内实现"的原因（可诊断，不静默）。
	memoryWiki    memory.Wiki
	memoryWikiErr error
	// life 是运行期收尾状态（在飞回合计数、可取消的根 context、要关的后台句柄与
	// 存储）。Shutdown 的全部逻辑在 shutdown.go；这里只持有它。
	life *lifecycle
	// eventWriter 是 Config.EventWriter 的原值：Shutdown 需要它来判断能否 Flush
	// （事件桥的 emitter 只认 io.Writer，不暴露底层 writer）。
	eventWriter io.Writer
	// jobs / terminals 是**工具实际在用**的后台句柄（注入的或 kit 自建的）。宿主
	// 画列表/做自动化时读它们，不要解析 job_list 的人读文本。
	jobs      *jobs.Manager
	terminals *terminal.Manager
	// toolByName 是按名字取工具的快照（构造后不变）：宿主偶尔需要直接调用某个
	// 工具（例如"从 UI 里重跑一次 search"），不必自己再拼一遍注册表。
	toolByName map[string]rig.Tool
	// runtime 是本 Agent 的进程级资源共享方（kit.New 装配时为 nil）。Runtime.Close
	// 之后它的共享后台任务/PTY 已经收尾，本 Agent 的 Run 必须拒绝。
	runtime *Runtime
	// owner 是本 Agent 在共享资源池里的调用方标识（Runtime.NewAgent 分配，
	// kit.New 装配时为空）。宿主直接调用共享管理器时必须把它带上，见 Owner()。
	owner string
}

// MemoryWiki 返回 memory 工具实际使用的存储（宿主可用来预置/导出页面）。
func (a *Agent) MemoryWiki() memory.Wiki {
	if a == nil {
		return nil
	}
	return a.memoryWiki
}

// Owner 返回本 Agent 在共享资源池里的调用方标识（`Runtime.NewAgent` 分配，形如
// `agent-1`；`kit.New` 装配时为空字符串）。
//
// 它是宿主**直接调用**共享管理器时的必填项：`Agent.Jobs().StartProcess` 要把
// `ProcessSpec.Owner` 填成这个值，管理器才知道该用哪份 policy/runner 围住任务；
// 不填会 fail closed（"this manager is owned by a kit.Runtime; pass the owning
// agent's id"），而不是裸跑。
func (a *Agent) Owner() string {
	if a == nil {
		return ""
	}
	return a.owner
}

// Jobs 返回**工具实际在用**的后台任务管理器（注入的 Config.Jobs，或 kit 自建的
// 那一个）；没有工具集时为 nil。
//
// 它是宿主画"后台任务"列表的数据源：任务状态、退出码、输出进度都在这里，不需要
// 解析 job_list/job_output 的人读文本（那种文本对模型友好，对机器脆弱）。
func (a *Agent) Jobs() *jobs.Manager {
	if a == nil {
		return nil
	}
	return a.jobs
}

// Terminals 返回工具实际在用的终端管理器；没有装配 terminal 工具时为 nil。
func (a *Agent) Terminals() *terminal.Manager {
	if a == nil {
		return nil
	}
	return a.terminals
}

// Tool 按名字取一个已注册的工具（副本句柄；工具的 Invoke 由实现自行保证并发安全）。
// 宿主用它做"直接调用某个工具"这类动作（例如 UI 里重跑一次 search）。
func (a *Agent) Tool(name string) (rig.Tool, bool) {
	if a == nil {
		return nil, false
	}
	tool, ok := a.toolByName[name]
	return tool, ok
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
	// goal/plan 工具需要宿主注入目标存储与 plan 控制器；没有就不该留在工具面上
	// （否则模型点开一个只会报 "not configured" 的工具）。
	// subagent/subagent_fork 已经不在 personal 预设里（个人客户端不提供多智能体
	// 编排），这里不必再列。
	if cfg.Goals == nil {
		drop = append(drop, "goal")
	}
	if cfg.GoalRevisions == nil {
		// 新版三件套需要修订号存储；只给 GoalStore 时它们会报
		// "does not implement GoalRevisionStore"。
		if _, ok := cfg.Goals.(hosttools.GoalRevisionStore); !ok {
			drop = append(drop, "get_goal", "create_goal", "update_goal")
		}
	}
	if cfg.PlanController == nil {
		drop = append(drop, "plan_review", "exit_plan_mode")
	}
	if cfg.Presenter == nil && cfg.ArtifactPresenter == nil {
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
	if cfg.AllowNetwork != nil && !*cfg.AllowNetwork {
		// 只有**显式**关掉网络时才摘掉 web_fetch：策略会拒绝每一次请求
		// （network access is disabled），留着一个"永远失败"的联网工具只会让模型
		// 以为它能上网。默认（nil = 开）保留它。
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
// 追加式激活让已激活部分逐字节稳定。要移除工具用 DeactivateTools（它会明确地
// 重建前缀，因此是宿主的独立动作）。
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

// DeactivateTools 在 sessionID 里**移除**已注册的工具（触发者是宿主，例如设置界面
// 上的"关掉这个工具"开关，会写一条 tool.deactivated 事件）。它转发给底层的
// rig.ToolDeactivator，语义与 agent.Agent.DeactivateTools 一致：
//
//   - 返回真正被移除的名字（按 active 顺序）；
//   - `tool_search`/`memory`/`skill` 不可移除，返回可读错误且整批不动；
//   - 未注册/未激活的名字静默忽略；配了 Config.Activations 时同步落盘。
//
// 这与 ActivateTools 的"只追加"契约相反：请求里的 tools 数组会变短，provider 的
// prefix cache 从被移除那一项起失效。因此它只接受宿主主动调用，模型侧没有对应工具。
func (a *Agent) DeactivateTools(sessionID string, names ...string) (removed []string, err error) {
	if a == nil || a.activator == nil {
		return nil, nil
	}
	deactivator, ok := a.activator.(rig.ToolDeactivator)
	if !ok {
		return nil, errors.New("kit: the underlying agent does not support tool deactivation")
	}
	removed, err = deactivator.DeactivateTools(sessionID, names...)
	if err != nil {
		return nil, err
	}
	// 事件流必须与 active 集合同步：只写 tool.activated 不写取消，会让按事件流镜像
	// 工具面的宿主一直显示"这个工具还开着"。
	emitToolDeactivated(a.events, removed, toolDeactivatedByHost)
	return removed, nil
}

// toolNamesOf 提取工具名并排序（去重保持稳定）。
// toolMapOf 建 name → tool 的映射（调用方保证名字唯一，重复时后者覆盖）。
func toolMapOf(tools []rig.Tool) map[string]rig.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		out[tool.Name()] = tool
	}
	return out
}

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

// PermissionPreset 返回当前**生效**的权限档位名：read-only / workspace-write /
// danger-full-access / auto，推不出来时返回 PermissionPresetCustom（"custom"）。
//
// 推导规则（与 New 里门禁组合用的是同一份结果，不会出现"报告一个档、实际跑另一个"）：
//  1. 宿主自己提供了 Config.Gate → custom（门禁不归 kit 组合，推不出档位）；
//  2. 宿主显式选了 Config.PermissionPreset 且没有被 Gate/Mode/Approval/Policy
//     覆盖 → 就是那个预设名（auto 缺 ToolReviewer 时仍报 auto：选择是明确的，
//     降级行为由 Preflight 说明）；
//  3. 否则按 (Mode, Approval) 反查预设表；auto 额外要求 ToolReviewer 非 nil
//     （"全权 + ask 但没有审查者"是自定义组合，不是 auto）；
//  4. 都不匹配 → custom。
//
// 因此零配置 agent（workspace-write + ask）返回 "workspace-write"；显式把 Mode 改成
// read-only 而没动 Approval 的 agent 返回 "read-only"。
func (a *Agent) PermissionPreset() string {
	if a == nil || a.permissionPreset == "" {
		return PermissionPresetCustom
	}
	return a.permissionPreset
}

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
	// Runtime 收尾是硬边界：共享后台任务与 PTY 池已经关掉，本 Agent 再跑只会
	// 得到"半个世界"。在入口一次判断，不给调用方留悬念；同时把本次回合计入
	// Runtime 的在飞集合，Close 才能一次性等到它们退出（而不是按 Agent 各起一个
	// 永久阻塞的等待 goroutine）。
	if a.runtime != nil {
		if err := a.runtime.beginRun(); err != nil {
			return RunResult{}, err
		}
		defer a.runtime.endRun()
	}
	// 生命周期入口：把本次 Run 挂到 Shutdown 的取消/等待链上。Shutdown 之后的新
	// 回合直接拒绝（ErrShuttingDown），而不是让它撞上一个正在关闭的存储。
	runCtx, finish, lifeErr := a.life.begin(ctx)
	if lifeErr != nil {
		return RunResult{}, lifeErr
	}
	defer finish()
	ctx = runCtx

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
	// 审批记忆（按 SessionID 作用域）与本轮去重只在本次 Run 内生效：memo 随 Run
	// 结束丢弃，所以下一次 Run 仍按记忆规则判断（见 approvals.go）。
	req.Gate = a.gatewayForRun(req)
	// 本次 Run 的门禁同时挂到 context 上：工具（特别是 run_code 的内层调用）
	// 只拿得到 context，必须用它，而不是构造期快照。
	result, runErr := a.agent.Run(withRunGate(runCtx, req.Gate), req)
	// Runtime.Close 的取消对整个 Runtime 生效：把它归一成 ErrRuntimeClosed，
	// 与"Close 之后新回合被拒"同一个用户可见结论（否则在飞回合会显示成一次
	// 神秘的 context canceled）。
	if a.runtime != nil && a.runtime.isClosed() && errors.Is(runErr, context.Canceled) {
		runErr = ErrRuntimeClosed
	}
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
			// 关停不是失败：宿主主动 Shutdown 造成的取消不写进 err，否则 UI 会把一次
			// 正常退出显示成"回合失败"。已经产生的部分结果照常返回。
			if !a.shutdownCancellation(runErr) {
				payload.Err = runErr.Error()
			}
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

// applyAggregateStorage 把 storage.Storage 的访问器填进对应的单字段。
//
// 显式设置的字段优先（聚合只是默认值来源）；`Storage`（只要求 Close）只在没设置
// 时接管，保证聚合存储一定会被 Shutdown 收尾。
func applyAggregateStorage(cfg *Config) {
	if cfg.Aggregate == nil || isNilValue(cfg.Aggregate) {
		return
	}
	aggregate := cfg.Aggregate
	ctx := context.Background()
	// storage.Storage 的契约是"不支持的能力返回会报 ErrUnsupported 的**诚实桩**"，
	// 不是返回 nil。只看"非 nil"会把桩当能力采纳：检查点保存让每个 Run 立刻失败、
	// memory 工具全动作报错、溢出被一个不支持的 Store 占位而不再落盘。所以逐个探测。
	if cfg.History == nil && cfg.SessionStore == nil && cfg.SessionStoreDir == "" {
		cfg.History = aggregate.History()
	}
	if cfg.CheckpointStore == nil {
		if candidate := aggregate.Checkpoints(); candidate != nil {
			if _, err := candidate.Load(ctx, "__capability_probe__"); !errors.Is(err, store.ErrUnsupported) {
				cfg.CheckpointStore = candidate
			}
		}
	}
	if cfg.Memory == nil {
		if candidate := aggregate.Memory(); candidate != nil {
			if _, err := candidate.Read(ctx, "__capability_probe__"); !errors.Is(err, store.ErrUnsupported) {
				cfg.Memory = candidate
			}
		}
	}
	if cfg.Spill == nil {
		if candidate := aggregate.Spill(); candidate != nil {
			if _, err := candidate.Load(ctx, "__capability_probe__"); !errors.Is(err, store.ErrUnsupported) {
				cfg.Spill = &rig.SpillPolicy{Store: candidate}
			}
		}
	}
	if cfg.Storage == nil {
		cfg.Storage = aggregate
	}
}

// isNilValue 判断接口里装的是不是 typed nil（`var p *T; var i I = p`）。
//
// Go 的接口比较对这种情况给出"不等于 nil"，于是 `if aggregate == nil` 放过它，
// 后面调用方法就 panic。所有可注入的接口都要过这一关。
func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func, reflect.Chan:
		return reflected.IsNil()
	default:
		return false
	}
}

// spillStoreOf 取出溢出存储；未配置时返回 nil（full.All 据此不注册 spill_read）。
func spillStoreOf(policy *rig.SpillPolicy) rig.SpillStore {
	if policy == nil {
		return nil
	}
	return policy.Store
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

// toolDeactivatedByHost 是 events.ToolDeactivated.Trigger 的取值：取消激活只有宿主
// 能触发（模型侧没有对应工具），因此只有一个取值。
const toolDeactivatedByHost = "host"

// sandboxedAllowance 按 Config.AutoAllowSandboxedCommands 决定是否给 inner 套上
// "已沙箱化即免问"这一层。默认开（nil），kit.Bool(false) 关。
func sandboxedAllowance(enabled *bool, runner sandbox.Runner, policy *sandbox.Policy, bridge *eventBridge, inner rig.Gate) rig.Gate {
	if enabled != nil && !*enabled {
		return inner
	}
	opts := gate.SandboxedOptions{
		Runner: runner,
		Policy: policy,
		Observer: func(event gate.AskEvent) {
			// 免问也必须留痕：UI 据此显示"这条没问是因为已隔离"，审计可复查。
			if bridge != nil {
				bridge.emitApproval(event)
			}
		},
	}
	return gate.AllowSandboxedCommands(opts, inner)
}

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

// spillPolicyFor 把宿主给的 SpillPolicy 解析成"agent 真能用的那一份"。
//
// 唯一的默认值是目录：`Dir` 为空时用 `<workspace>/.rig/spill`。为什么不是系统临时
// 目录（旧行为）：那是全局位置，所有工作区/会话的溢出结果混在一起，而且不在工作区
// 可写根内；模型拿到的 `path:` 是否能读回取决于读围栏的细节，不是承诺。工作区内的
// 目录则天然 per-workspace（独立回收、用户可自行查看/清理），并且一定在可写根内。
//
// 只在 Dir 为空时补默认值：宿主显式给了目录就完全照它的来。
func spillPolicyFor(policy *rig.SpillPolicy, workspace string) *rig.SpillPolicy {
	if policy == nil {
		return nil
	}
	if strings.TrimSpace(policy.Dir) != "" {
		return policy
	}
	resolved := *policy
	resolved.Dir = filepath.Join(workspace, ".rig", "spill")
	return &resolved
}

// hostEnabledActiveTools 返回"宿主显式接线后应当立刻可用"的工具。
//
// 与 alwaysActiveTools 的区别：那些是产品默认（与配置无关），这些是宿主付了配置
// 成本的能力（接一个 Presenter 就该能交付文件）。它们同样不会出现在 tool_search
// 清单里（清单只列 inactive 的）。
func hostEnabledActiveTools(cfg Config) []string {
	var out []string
	// 两种形态都算"宿主接上了 present"：只给内容产物 presenter 时它同样应当直接
	// 可用（否则模型要多花一轮 tool_search，而摘除条件那边已经是对称的）。
	if cfg.Presenter != nil || cfg.ArtifactPresenter != nil {
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

// emitToolDeactivated 把一次取消激活写进事件流（与 tool.activated 对称，payload
// 同结构；trigger 目前恒为 host）。
func emitToolDeactivated(bridge *eventBridge, names []string, trigger string) {
	if bridge == nil || len(names) == 0 {
		return
	}
	bridge.emit(eventspkg.KindToolDeactivated, eventspkg.ToolDeactivated{
		Tools:   append([]string(nil), names...),
		Trigger: trigger,
	})
}
