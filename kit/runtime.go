package kit

// Runtime：长生命周期宿主共享的进程级资源 + 每个 Agent 独占的会话状态。
//
// # 它解决什么问题
//
// `kit.New(cfg)` 每次都会重建**全部**东西：provider（含 http.Client 与连接池）、
// 后台任务管理器、终端会话管理器、工具实现、记忆 wiki、spill store……宿主每开一个
// 会话或子代理就要付一次全量代价，而且**状态不共享**：a1 起的后台任务 a2 看不到、
// `terminal_*` 句柄各玩各的一份。
//
// `Runtime` 把"一个进程里本来就只有一份"的东西提到外层，让 `NewAgent` 只重建
// "每个 Agent 必须自己一份"的东西。
//
// # 共享（Runtime 持有）
//
//   - provider（含它的 http.Client / 连接池；模型价表本身是 modelprice 的进程级表）；
//   - `*jobs.Manager`：后台 bash 任务跨 Agent 可见；
//   - `*terminal.Manager`：PTY 会话句柄跨 Agent 共享；
//   - 记忆 wiki（未显式注入时按工作区分区缓存，同一工作区的 Agent 共用一份）；
//   - spill store（`RuntimeConfig.Spill.Store` 实例）；
//   - 宿主能力通道（Searcher/SessionLog/Skills/TeamTasks/Scheduler/Reviewer/Runner/
//     MCP/Models/SandboxRunner）：它们本来就是进程级服务，Runtime 转发给每个 Agent。
//
// # 每 Agent 重建（`NewAgent` 自己构造）
//
// `agent.Agent`（ReAct 循环）、history、事件桥、`gate` 绑定（含 Asker/审批观察者）、
// `sandbox.Policy`（workspace roots、Mode、隐藏路径、可写根）、会话状态、工具集
// （工具实例绑定 Policy，所以必须每 Agent 一份；共享的是它们背后的管理器与存储）、
// activations、undo tracker、压缩预算、Meter。
//
// # 冲突字段
//
// 生命周期归 Runtime 的 `Config` 字段（`Provider`/`Jobs`/`BackgroundJobs`/
// `Storage`/`Aggregate`）在 `NewAgent` 里**报错**：静默覆盖会让宿主以为自己交出去
// 的句柄在被使用/被关闭。进程级能力字段（`SandboxRunner`/`Searcher`/…/`Spill`）
// 由 Runtime 的共享值**覆盖并告警**——它们没有独立生命周期，覆盖是安全的。
//
// # 调用方级围栏
//
// 共享的 `jobs.Manager` / `terminal.Manager` 不绑定任何一份策略：每个 Agent 在
// `NewAgent` 时把自己的 `(policy, runner)` 注册成一份 scope，`job_start`、
// `terminal_open`、`terminal_send` 执行时按**本次调用方**解析。因此 read-only 的
// Agent 不能借共享任务池在别人的工作区里写文件，workspace-write 的 Agent 也不会
// 被"第一个 read-only Agent"挡住。
//
// 池子可见、**写入按 owner 隔离**：`job_list`/`terminal_list` 跨 Agent 可见（共享池
// 的承诺），但 `terminal_send` 只接受**自己的**会话——它执行的命令跑在会话所有者的
// 进程与 runner 里，跨 owner 放行会让低权限 Agent 借别人的全权会话写盘。
//
// # Close 语义
//
// `Close` 是硬边界：它置 closed、**取消并等待**已创建 Agent 的在飞回合（聚合等待，
// 最多 `runtimeCloseWait`，超时返回点名"还有几个在飞回合"的可读错误），然后收尾
// 共享后台任务/PTY/spill。之后
// `NewAgent` 与已存在 Agent 的新 `Run` 都返回 `ErrRuntimeClosed`；被取消的在飞
// 回合也归一成 `ErrRuntimeClosed`。`Close` 幂等。
// Agent 自己的 `Shutdown` **不**关闭共享句柄：那是 Runtime 的责任。

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	eventspkg "github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
	"github.com/zeokeel/zeokeel-rig/tools/web"
)

// ErrRuntimeClosed 表示 Runtime 已经关闭：新的 Agent 建不出来，已存在的 Agent
// 也不能再跑回合。
//
// 它与 `ErrShuttingDown` 刻意分开：后者是**单个 Agent**的收尾，前者是**整个进程级
// 资源池**的收尾。宿主据此可以决定是"重开一个 Runtime"还是"别注销这个会话"。
var ErrRuntimeClosed = errors.New("kit: runtime is closed")

// runtimeCloseWait 是 Close 等待在飞回合退出的上界。超过它就返回可读错误（共享
// 后台任务/PTY 仍然会被收尾），而不是无限挂住宿主的关窗流程。
const runtimeCloseWait = 5 * time.Second

// RuntimeConfig 配置 Runtime 共享的进程级资源。
//
// 与 `Config` 的分工：这里只放"一个进程里一份就够"的东西。每个会话/子代理的
// 差异（Model、SystemPrompt、Root、Mode、审批、History、预算……）留在 `Config`，
// 由 `Runtime.NewAgent` 接收。
type RuntimeConfig struct {
	// Provider 是共享的模型客户端（含它的 http.Client 与连接池）。非 nil 时
	// ProviderKind/BaseURL/APIKey/APIKeyEnv 被忽略。
	Provider Provider
	// ProviderKind 指定从零构造共享 provider 时用哪套协议（见 BuildProvider）。
	ProviderKind ProviderKind
	// Model 是共享 provider 的**默认**模型名（仅用于构造 provider；每个 Agent
	// 仍然使用自己 Config.Model）。
	Model string
	// BaseURL / APIKey / APIKeyEnv 是共享 provider 的接线参数。
	BaseURL   string
	APIKey    string
	APIKeyEnv string

	// SandboxRunner 是进程级的 OS 沙箱 runner，转发给每个 Agent。
	//
	// 注意它仍然与 `Config.Tools` 互斥（自建工具集不会经过 OS 沙箱包装）。
	SandboxRunner sandbox.Runner
	// SandboxRunnerConfig 是构造上面那份 runner 时用的 osrunner 配置（可选）。
	//
	// `DeriveRunner` 以它为基准派生新 runner（例如"这个子 Agent 要更严的包装
	// 命令"）；派生既不改动共享 runner，也不改动这里存的配置。
	SandboxRunnerConfig *osrunner.Config

	// Searcher / SessionLog / Skills / TeamTasks / Scheduler / Reviewer / Runner /
	// Models / MCPClients / MCPPrefix 是宿主已经拥有的进程级能力，原样转发给每个
	// Agent（与 Config 的同名字段同义）。
	Searcher   web.Searcher
	SessionLog rig.SessionLog
	Skills     rig.SkillStore
	TeamTasks  rig.TeamTaskManager
	Scheduler  rig.Scheduler
	Reviewer   rig.Reviewer
	Runner     rig.SubagentRunner
	Models     []string
	MCPClients []*mcp.Client
	MCPPrefix  string

	// Jobs 让宿主注入自己的后台任务管理器。非 nil 时所有 Agent 共用它，且
	// `Runtime.Close` 会关它。nil 表示由 Runtime 建一个共享任务池（之后所有
	// Agent 共用）。
	//
	// 任务池共享、**围栏不共享**：每次 job_start 都按调用方 Agent 的
	// policy/runner 执行（Runtime 在 NewAgent 时注册 owner→scope）。注入自己的
	// 管理器时需自行提供同样的按 owner 解析（jobs.WithScopeResolver），否则
	// 它只会用自己的默认策略。
	Jobs *jobs.Manager
	// Terminals 让宿主注入自己的终端会话管理器（同上：共享池、按调用方围栏、
	// 由 Runtime 关闭）。nil 表示由 Runtime 建一个。
	Terminals *terminal.Manager

	// Memory 是共享的记忆存储：非 nil 时所有 Agent 共用这一份（跨工作区）。
	// nil 表示按 Agent 的 Root 各建一份文件 wiki 并缓存（同一工作区复用）。
	Memory memory.Wiki

	// Spill 是共享的溢出存储策略。非 nil 时覆盖每个 Agent 自己的 Config.Spill；
	// `Dir` 为空时仍按各自的工作区解析成 `<Root>/.rig/spill`，共享的是 Store 实例。
	Spill *rig.SpillPolicy
}

// Runtime 持有一组进程级共享资源，并按需为每个会话/子代理构造 Agent。
//
// 它可安全并发使用：`NewAgent` 与 `Close` 互斥（同一把锁 + closed 标记）。
// `Run` 在入口检查 closed，在收尾时再读一次以把 Close 造成的取消归一成
// `ErrRuntimeClosed`；Close 等回合退出时**不持锁**，两者不会互卡。
type Runtime struct {
	mu     sync.Mutex
	closed bool

	cfg      RuntimeConfig
	provider Provider
	// jobs / terminals 是共享的后台句柄：任务/PTY 池跨 Agent 共用，策略与
	// runner 按调用方 owner 解析（见 scopes）。它们按需构造一次，之后所有
	// Agent 共用。
	jobs      *jobs.Manager
	terminals *terminal.Manager

	// memoryByRoot 缓存"按工作区分区"的自动记忆 wiki。显式注入 cfg.Memory 时
	// 不使用它。
	memoryByRoot map[string]memory.Wiki

	// scopes 是"调用方 owner → 该 Agent 的围栏"。共享的 jobs.Manager /
	// terminal.Manager 按这个表解析每次调用该用哪份策略与 runner，而不是把
	// 第一个 Agent 的策略绑死在管理器上。
	scopeMu sync.RWMutex
	scopes  map[string]agentScope
	// extraRoots 是 Runtime 级别追加的可写根（AddRoot/RemoveRoot）：新建 Agent 时
	// 追加在主根之后；RemoveRoot 还会撤销已存在 Agent（kit 自建策略）上的同一根。
	extraRoots []string
	// mcpNamed 是按名字登记的 MCP 客户端（AddMCPClient/RemoveMCPClient）。新建
	// Agent 时并入 Config.MCPClients；已存在 Agent 的工具面在构造期固定，只能留
	// 一条 notice。
	mcpNamed []namedMCPClient
	// agentSeq 给每个 Agent 生成唯一 owner 标签。
	agentSeq int
	// agents 是已创建的 Agent：Close 要取消并等待它们的在飞回合。
	agents []*Agent
	// runs 是**所有** Agent 在飞回合的聚合计数：Close 用一个 WaitGroup + 一个
	// time.After 等待全部回合退出，不给每个 Agent 各留一个永久阻塞的
	// goroutine（审计 D1 复审 N3）。
	runs sync.WaitGroup
	// inFlight 是当前在飞回合数，只用于超时文案（"还有几个回合没退"）。
	inFlight atomic.Int64
	// closeDone 在第一次 Close 收尾完成时关闭；并发的第二个 Close 等它。
	closeDone chan struct{}
}

// namedMCPClient 是一个按名字登记的 MCP 客户端（可被 RemoveMCPClient 撤销）。
type namedMCPClient struct {
	name   string
	client *mcp.Client
}

// agentScope 是一个 Agent 注册到共享管理器上的围栏快照。
type agentScope struct {
	policy *sandbox.Policy
	runner sandbox.Runner
}

// NewRuntime 构造共享资源池。
//
// Provider 为 nil 时按 ProviderKind/BaseURL/APIKey/APIKeyEnv/Model 构造一次
// （与 `kit.New` 的默认 provider 路径同源）；构造失败返回 provider 错误。
//
// 它不构造任何工具、不读工作区：那些是 `NewAgent` 的事。
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	r := &Runtime{
		cfg:          cfg,
		jobs:         cfg.Jobs,
		terminals:    cfg.Terminals,
		memoryByRoot: make(map[string]memory.Wiki),
		scopes:       make(map[string]agentScope),
		closeDone:    make(chan struct{}),
	}
	if cfg.Provider != nil {
		r.provider = cfg.Provider
		return r, nil
	}
	built, err := BuildProvider(cfg.ProviderKind, Config{
		Model:     cfg.Model,
		BaseURL:   cfg.BaseURL,
		APIKey:    cfg.APIKey,
		APIKeyEnv: cfg.APIKeyEnv,
	})
	if err != nil {
		return nil, fmt.Errorf("kit: runtime provider: %w", err)
	}
	r.provider = built
	return r, nil
}

// NewAgent 用一个 Agent 自己的 Config 装配一个 Agent，进程级资源取自 Runtime。
//
// 与共享资源冲突的 Config 字段按包文档里的约定处理：生命周期句柄
// （Provider/Jobs/BackgroundJobs/Storage/Aggregate）报错，可覆盖的能力字段被
// 共享值覆盖并告警。
//
// Runtime 已关闭时返回 `ErrRuntimeClosed`；构造失败时不泄漏任何共享句柄。
func (r *Runtime) NewAgent(cfg Config) (*Agent, error) {
	if r == nil {
		return nil, ErrRuntimeClosed
	}
	// 构造期整段持锁：Close 要么排在构造之前（这里直接报 ErrRuntimeClosed），
	// 要么排在构造之后（那时 Agent 已经拿到了完整的共享句柄）。
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrRuntimeClosed
	}
	// 每个 Agent 一个唯一 owner：共享的 jobs/terminal 管理器在每次调用时用它
	// 解析该 Agent 自己的策略与 runner（而不是第一个 Agent 的那份）。
	r.agentSeq++
	owner := fmt.Sprintf("agent-%d", r.agentSeq)
	agent, err := newAgent(cfg, r, owner)
	if err != nil {
		return nil, err
	}
	r.registerScope(owner, agentScope{policy: agent.policy, runner: agent.runner})
	r.agents = append(r.agents, agent)
	return agent, nil
}

// AddRoot 追加一个 Runtime 级别的可写根：**之后新建**的 Agent 会拿到它（追加在
// 各自的工作区主根之后），已存在的 Agent 不被追溯修改。
//
// 为什么不让它追溯生效：给一个已经跑起来的 Agent 悄悄扩大可写面，是把审批过、
// 观察过的那份围栏换掉。要扩大已存在 Agent 的可写面，走 `request_sandbox_access`
// 的审批通道；要撤销用 `RemoveRoot`（收紧总是安全的，因此它即时生效）。
//
// path 必须存在且是目录；重复添加是幂等的。Runtime 已关闭时返回 ErrRuntimeClosed。
func (r *Runtime) AddRoot(path string) error {
	if r == nil {
		return ErrRuntimeClosed
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("kit: AddRoot requires a non-empty path")
	}
	abs, err := absoluteRoot(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("kit: runtime root %q: %w", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("kit: runtime root %q is not a directory", abs)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRuntimeClosed
	}
	if !slices.ContainsFunc(r.extraRoots, func(existing string) bool { return sandbox.PathEqual(existing, abs) }) {
		r.extraRoots = append(r.extraRoots, abs)
	}
	return nil
}

// RemoveRoot 撤销一个可写根：它先退出 Runtime 的追加根列表（之后新建的 Agent 不再
// 拥有它），再对**每个已存在的 Agent** 立即撤销同一个根。
//
// 只有 kit 自己构造的策略会被改动：宿主经 `Config.Policy` 传进来的策略归宿主所有。
// 主根不可撤销（它是相对路径的解析基准，见 `sandbox.Policy.RemoveWritableRoot`）。
// 既没有登记、也没有任何 Agent 拥有它时返回错误，让宿主能发现路径写错。
func (r *Runtime) RemoveRoot(path string) error {
	if r == nil {
		return ErrRuntimeClosed
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("kit: RemoveRoot requires a non-empty path")
	}
	abs, err := absoluteRoot(path)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrRuntimeClosed
	}
	registered := slices.ContainsFunc(r.extraRoots, func(existing string) bool { return sandbox.PathEqual(existing, abs) })
	if registered {
		kept := make([]string, 0, len(r.extraRoots))
		for _, existing := range r.extraRoots {
			if !sandbox.PathEqual(existing, abs) {
				kept = append(kept, existing)
			}
		}
		r.extraRoots = kept
	}
	agents := append([]*Agent(nil), r.agents...)
	r.mu.Unlock()

	revoked := false
	var errs []error
	for _, agent := range agents {
		if agent == nil || agent.policy == nil || !agent.policyOwned {
			continue
		}
		if err := agent.policy.RemoveWritableRoot(abs); err != nil {
			if errors.Is(err, sandbox.ErrWritableRootIsPrimary) {
				// 主根不能被撤销——**不能**当成"没有这根"跳过，否则 RemoveRoot 返回
				// nil 而该 Agent 仍可写（审计实测的静默成功）。
				errs = append(errs, fmt.Errorf("kit: runtime: root %q is the primary root of a live agent and cannot be removed: %w", abs, err))
				continue
			}
			if errors.Is(err, sandbox.ErrWritableRootNotFound) {
				// 这个 Agent 本来就没有这根：不是错误。
				continue
			}
			errs = append(errs, fmt.Errorf("kit: runtime: remove root %q: %w", abs, err))
			continue
		}
		revoked = true
	}
	if !registered && !revoked && len(errs) == 0 {
		return fmt.Errorf("kit: runtime: %q is not a writable root of this runtime", abs)
	}
	return errors.Join(errs...)
}

// AddMCPClient 按名字登记一个 MCP 客户端：**之后新建**的 Agent 工具面包含它的
// 工具/资源/提示工具（名字带 `MCPPrefix` + server 身份）。
//
// 已存在的 Agent 工具面在构造期固定（工具的 schema 与名字分配都发生在那一刻），
// 所以增删不会强改它们的 active 集，只往它们的事件流里写一条 notice。"这个会话
// 现在要用新 server" 的正确做法是新建一个 Agent（Runtime 已经共享 provider、后台
// 任务与 PTY，新建一个很便宜）。
func (r *Runtime) AddMCPClient(name string, client *mcp.Client) error {
	if r == nil {
		return ErrRuntimeClosed
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("kit: AddMCPClient requires a non-empty name")
	}
	if client == nil {
		return errors.New("kit: AddMCPClient requires a non-nil client")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrRuntimeClosed
	}
	for _, existing := range r.mcpNamed {
		if existing.name == name {
			r.mu.Unlock()
			return fmt.Errorf("kit: MCP client %q is already registered; RemoveMCPClient it first", name)
		}
	}
	r.mcpNamed = append(r.mcpNamed, namedMCPClient{name: name, client: client})
	agents := append([]*Agent(nil), r.agents...)
	r.mu.Unlock()

	notifyAgents(agents, fmt.Sprintf(
		"mcp client %q added: existing agents keep the tool surface they were built with; create a new agent to use its tools", name))
	return nil
}

// ErrMCPClientNotFound 表示 RemoveMCPClient 的名字没有登记过。
var ErrMCPClientNotFound = errors.New("kit: no MCP client registered under that name")

// RemoveMCPClient 撤销一个按名字登记的 MCP 客户端：之后新建的 Agent 工具面不再
// 包含它的工具；已存在的 Agent 同样只收 notice（见 AddMCPClient）。
//
// 它**不关闭** client：连接的生命周期归宿主（Run/NewAgent 也从不关闭宿主交来的
// 客户端），撤销登记只是不再把它接进新的 Agent。
func (r *Runtime) RemoveMCPClient(name string) error {
	if r == nil {
		return ErrRuntimeClosed
	}
	name = strings.TrimSpace(name)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrRuntimeClosed
	}
	kept := make([]namedMCPClient, 0, len(r.mcpNamed))
	found := false
	for _, existing := range r.mcpNamed {
		if existing.name == name {
			found = true
			continue
		}
		kept = append(kept, existing)
	}
	if !found {
		r.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrMCPClientNotFound, name)
	}
	r.mcpNamed = kept
	agents := append([]*Agent(nil), r.agents...)
	r.mu.Unlock()

	notifyAgents(agents, fmt.Sprintf(
		"mcp client %q removed: existing agents keep the tool surface they were built with; create a new agent to drop its tools", name))
	return nil
}

// DeriveRunner 基于 Runtime 的 runner 配置（RuntimeConfig.SandboxRunnerConfig）
// 派生一个新的 OS 沙箱 runner，供"这个 Agent 要更严的档位"使用。
//
// 共享 runner 不受影响：已存在的 Agent、之后新建的 Agent（没有显式
// `Config.SandboxRunner` 时）都继续用 `RuntimeConfig.SandboxRunner`。派生出来的
// runner 由调用方自己通过 `Config.SandboxRunner` 注入给某个 Agent（注意
// `Config.Tools` 与 runner 互斥）。
//
// opts 在基准配置的副本上依次应用（切片字段先深拷贝，避免改到 Runtime 存的配置）；
// 构造失败（例如自定义 runner 缺失败特征）原样返回 osrunner 的错误。
func (r *Runtime) DeriveRunner(opts ...func(*osrunner.Config)) (sandbox.Runner, error) {
	var base osrunner.Config
	if r != nil {
		r.mu.Lock()
		if r.cfg.SandboxRunnerConfig != nil {
			base = *r.cfg.SandboxRunnerConfig
		}
		r.mu.Unlock()
	}
	base.RunnerCommand = append([]string(nil), base.RunnerCommand...)
	base.RunnerFailureSignatures = append([]string(nil), base.RunnerFailureSignatures...)
	for _, opt := range opts {
		if opt != nil {
			opt(&base)
		}
	}
	runner, err := osrunner.New(base)
	if err != nil {
		return nil, fmt.Errorf("kit: derive runner: %w", err)
	}
	return runner, nil
}

// extraRootsLocked 返回 Runtime 追加的可写根副本。调用方必须持有 r.mu。
func (r *Runtime) extraRootsLocked() []string {
	return append([]string(nil), r.extraRoots...)
}

// sharedMCPClientsLocked 返回要接进新 Agent 的 MCP 客户端：RuntimeConfig.MCPClients
// 在前，按名字登记的热增客户端在后（按指针去重）。调用方必须持有 r.mu。
func (r *Runtime) sharedMCPClientsLocked() []*mcp.Client {
	out := make([]*mcp.Client, 0, len(r.cfg.MCPClients)+len(r.mcpNamed))
	seen := make(map[*mcp.Client]struct{}, cap(out))
	for _, client := range r.cfg.MCPClients {
		if client == nil {
			continue
		}
		if _, dup := seen[client]; dup {
			continue
		}
		seen[client] = struct{}{}
		out = append(out, client)
	}
	for _, named := range r.mcpNamed {
		if named.client == nil {
			continue
		}
		if _, dup := seen[named.client]; dup {
			continue
		}
		seen[named.client] = struct{}{}
		out = append(out, named.client)
	}
	return out
}

// notifyAgents 给一组已存在的 Agent 各写一条事件流 notice（没开事件流时 no-op）。
//
// 它是"工具面被运行期变更影响"的唯一可见通道：既不静默吞掉，也不假装能给已存在
// 的 Agent 换工具集。
func notifyAgents(agents []*Agent, message string) {
	for _, agent := range agents {
		if agent == nil || agent.events == nil {
			continue
		}
		agent.events.emit(eventspkg.KindNotice, noticePayload{Message: message})
	}
}

// absoluteRoot 把路径规范成绝对路径（清理并尽可能解析符号链接）。它不要求路径
// 存在：撤销一个已经被删掉的目录同样是合法操作。
func absoluteRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("kit: root %q: %w", path, err)
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(real)
	}
	return abs, nil
}

// registerScope 把某个 owner 的围栏登记进共享管理器的解析表。
func (r *Runtime) registerScope(owner string, scope agentScope) {
	r.scopeMu.Lock()
	defer r.scopeMu.Unlock()
	if r.scopes == nil {
		r.scopes = make(map[string]agentScope)
	}
	r.scopes[owner] = scope
}

// scopeForOwner 是共享管理器的 resolver：没有登记的 owner（例如宿主直接用
// 管理器 API）返回 ok=false，管理器退回自己的默认值。
func (r *Runtime) scopeForOwner(owner string) (agentScope, bool) {
	r.scopeMu.RLock()
	defer r.scopeMu.RUnlock()
	scope, ok := r.scopes[owner]
	return scope, ok
}

// Close 关闭共享资源：先取消并等待在飞回合（最多 runtimeCloseWait），再收尾
// 后台任务（杀进程）、终端会话（杀 PTY）、spill store（实现 io.Closer 时）。
// 返回聚合错误；可安全重复调用（第二次是 no-op）。
//
// 等待期间**不持锁**：在飞回合收尾时会读 closed 标记（例如把 context.Canceled
// 归一成 ErrRuntimeClosed），持锁等待会互相卡死。已置位的 closed 保证并发的
// NewAgent / 新 Run 被拒。并发的第二个 Close 会等第一个收尾完成后返回 nil。
//
// 关闭之后 `NewAgent` 与已存在 Agent 的新 `Run` 都返回 `ErrRuntimeClosed`；
// 被 Close 取消的在飞回合同样归一成 `ErrRuntimeClosed`。Agent 自己的 `Shutdown`
// 不会重复关闭这些共享句柄。
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		done := r.closeDone
		r.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	r.closed = true
	done := r.closeDone
	agents := append([]*Agent(nil), r.agents...)
	backgroundJobs, terminals, spill := r.jobs, r.terminals, r.cfg.Spill
	r.mu.Unlock()

	var errs []error
	// 1. 取消所有已创建 Agent 的在飞回合。共享后台任务/PTY 马上要被收尾，
	//    让回合先退出，而不是在半路撞上被关掉的句柄（审计 D1 #3）。
	for _, agent := range agents {
		if agent != nil && agent.life != nil {
			agent.life.cancel()
		}
	}
	// 2. 等它们退到边界：单个聚合 WaitGroup ＋ 一个超时选择。上界是
	//    runtimeCloseWait；超时返回可读错误（点名还有几个在飞回合与出口），不
	//    无限挂住宿主的关窗流程。
	//
	//    超时只留**一个**等待 goroutine（回合退出后它自己结束），而不是给每个
	//    Agent 各起一个永久阻塞的等待者（审计 D1 复审 N3）。
	waited := make(chan struct{})
	go func() {
		r.runs.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(runtimeCloseWait):
		errs = append(errs, fmt.Errorf(
			"kit: runtime close timed out waiting for %d in-flight run(s); call Agent.Shutdown then retry",
			r.inFlight.Load()))
	}
	// 3. 收尾共享句柄。
	if backgroundJobs != nil {
		if err := backgroundJobs.CloseAll(); err != nil {
			errs = append(errs, fmt.Errorf("kit: runtime: close background jobs: %w", err))
		}
	}
	if terminals != nil {
		if err := terminals.CloseAll(); err != nil {
			errs = append(errs, fmt.Errorf("kit: runtime: close terminal sessions: %w", err))
		}
	}
	if store := spillStoreOf(spill); store != nil {
		// 溢出存储没有统一的 Close 契约（见 rig.SpillStore）：实现了 io.Closer
		// 才收尾，否则交给宿主自己关。
		if closer, ok := store.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				errs = append(errs, fmt.Errorf("kit: runtime: close spill store: %w", err))
			}
		}
	}
	if done != nil {
		close(done)
	}
	return errors.Join(errs...)
}

// beginRun 把一次 Run 计入"在飞回合"，并拒绝 Close 之后的新回合。
//
// 它与 closed 检查在**同一把锁**上完成：Close 置位 closed 之后不可能再有 Add
// 落在 Wait 开始之后（`sync.WaitGroup` 不允许计数归零后并发 Add）。Close 也已经
// 在置位前取消了所有 Agent 的根 context，所以这里放行的回合会立刻带着取消退出。
func (r *Runtime) beginRun() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRuntimeClosed
	}
	r.runs.Add(1)
	r.inFlight.Add(1)
	return nil
}

// endRun 把一个回合从在飞集合里摘掉。它必须与 beginRun 成对调用。
func (r *Runtime) endRun() {
	if r == nil {
		return
	}
	r.inFlight.Add(-1)
	r.runs.Done()
}

// isClosed 报告 Close 是否已经开始（Agent.Run 用它把取消归一成
// ErrRuntimeClosed）。
func (r *Runtime) isClosed() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// jobsLockedFor 返回共享的后台任务管理器。调用方必须持有 r.mu。
//
// 管理器**不绑定**任何一份策略：每次 job_start 都按调用方 owner 解析该 Agent
// 的 policy/runner（见 jobs.WithScopeResolver）。任务池跨 Agent 共享，围栏不
// 共享——否则 read-only 的 Agent 能在别人的工作区里起写任务（审计 D1 #1）。
func (r *Runtime) jobsLockedFor() *jobs.Manager {
	if r.jobs != nil {
		return r.jobs
	}
	r.jobs = jobs.NewManager(jobs.WithScopeResolver(func(owner string) (jobs.JobScope, bool) {
		scope, ok := r.scopeForOwner(owner)
		if !ok {
			return jobs.JobScope{}, false
		}
		return jobs.JobScope{Policy: scope.policy, Runner: scope.runner}, true
	}))
	return r.jobs
}

// terminalsLockedFor 返回共享的终端会话管理器（语义同 jobsLockedFor）。
// 调用方必须持有 r.mu。
func (r *Runtime) terminalsLockedFor(background *jobs.Manager) *terminal.Manager {
	if r.terminals != nil {
		return r.terminals
	}
	r.terminals = terminal.NewManager(
		terminal.WithBackgroundJobs(background),
		terminal.WithScopeResolver(func(owner string) (terminal.Scope, bool) {
			scope, ok := r.scopeForOwner(owner)
			if !ok {
				return terminal.Scope{}, false
			}
			return terminal.Scope{Policy: scope.policy, Runner: scope.runner}, true
		}),
	)
	return r.terminals
}

// memoryLockedFor 返回按工作区共享的记忆 wiki。调用方必须持有 r.mu。
//
// 显式注入的 cfg.Memory 优先（跨工作区共享一份）；否则按 root 缓存一份
// `memory.NewWorkspaceWiki`。建不出来时返回 nil，让 Agent 的常规路径重试并记录
// 可诊断错误（与 `kit.New` 的行为一致：记忆不可用不该让 Agent 建不出来）。
func (r *Runtime) memoryLockedFor(root string) memory.Wiki {
	if r.cfg.Memory != nil {
		return r.cfg.Memory
	}
	if root == "" {
		return nil
	}
	if wiki, ok := r.memoryByRoot[root]; ok {
		return wiki
	}
	wiki, err := memory.NewWorkspaceWiki("", root)
	if err != nil {
		return nil
	}
	r.memoryByRoot[root] = wiki
	return wiki
}

// applySharedLocked 把 Runtime 的共享值填进 cfg。调用方必须持有 r.mu。
//
// 两类字段的处理不同（见包文档）：生命周期句柄报错，能力字段覆盖并告警。
func (r *Runtime) applySharedLocked(cfg *Config, root string, hadAggregate bool) error {
	// 生命周期归 Runtime：这些句柄的关闭责任在 Runtime.Close，Config 里再给
	// 一个会让"谁负责关"变成两说。每条文案都点出**具体字段**与它的出口。
	switch {
	case cfg.Provider != nil:
		return errors.New("kit: Config.Provider is owned by the Runtime; set RuntimeConfig.Provider instead")
	case cfg.Jobs != nil:
		return errors.New("kit: Config.Jobs is owned by the Runtime (shared by every agent); set RuntimeConfig.Jobs instead")
	case len(cfg.BackgroundJobs) > 0:
		return errors.New("kit: Config.BackgroundJobs is owned by the Runtime; register those handles outside the agent")
	case hadAggregate:
		// 必须在 cfg.Storage 判断之前：applyAggregateStorage 会把 Aggregate 展开
		// 进 Storage，否则这里只会报出一个宿主从没设过的字段（审计 D1 #5）。
		return errors.New("kit: Config.Aggregate is per-agent session state; RuntimeConfig has no shared aggregate store — remove Config.Aggregate")
	case cfg.Storage != nil:
		return errors.New("kit: Config.Storage is per-agent session state; RuntimeConfig has no shared storage — remove Config.Storage")
	}

	var overridden []string
	override := func(name string, shared bool) {
		if shared {
			overridden = append(overridden, name)
		}
	}
	// 共享 runner 只在 Agent **没有显式表达意愿**时注入：Config.Sandbox=false
	// 是明确的 opt-out（不报错、不注入），否则 Runtime 会一边声称"能力字段
	// 覆盖 + 告警"，一边把这种合法配置硬拒（审计 D1 #4）。
	switch {
	case cfg.Sandbox != nil && !*cfg.Sandbox:
		// 明确不要：保留 nil，围栏回到进程内策略（Preflight 会如实报告）。
	case r.cfg.SandboxRunner != nil:
		override("SandboxRunner", cfg.SandboxRunner != nil)
		cfg.SandboxRunner = r.cfg.SandboxRunner
	}
	override("Searcher", cfg.Searcher != nil && r.cfg.Searcher != nil)
	if r.cfg.Searcher != nil {
		cfg.Searcher = r.cfg.Searcher
	}
	override("SessionLog", cfg.SessionLog != nil && r.cfg.SessionLog != nil)
	if r.cfg.SessionLog != nil {
		cfg.SessionLog = r.cfg.SessionLog
	}
	override("Skills", cfg.Skills != nil && r.cfg.Skills != nil)
	if r.cfg.Skills != nil {
		cfg.Skills = r.cfg.Skills
	}
	override("TeamTasks", cfg.TeamTasks != nil && r.cfg.TeamTasks != nil)
	if r.cfg.TeamTasks != nil {
		cfg.TeamTasks = r.cfg.TeamTasks
	}
	override("Scheduler", cfg.Scheduler != nil && r.cfg.Scheduler != nil)
	if r.cfg.Scheduler != nil {
		cfg.Scheduler = r.cfg.Scheduler
	}
	override("Reviewer", cfg.Reviewer != nil && r.cfg.Reviewer != nil)
	if r.cfg.Reviewer != nil {
		cfg.Reviewer = r.cfg.Reviewer
	}
	override("Runner", cfg.Runner != nil && r.cfg.Runner != nil)
	if r.cfg.Runner != nil {
		cfg.Runner = r.cfg.Runner
	}
	override("Models", len(cfg.Models) > 0 && len(r.cfg.Models) > 0)
	if len(r.cfg.Models) > 0 {
		cfg.Models = r.cfg.Models
	}
	// MCP：RuntimeConfig.MCPClients 与按名字登记的客户端（AddMCPClient）合并后
	// 一起转发——热增的客户端必须和构造期给的那批同等对待，否则"给新 Agent 加
	// server"在工具层看起来是两套能力。
	sharedMCP := r.sharedMCPClientsLocked()
	override("MCPClients", len(cfg.MCPClients) > 0 && len(sharedMCP) > 0)
	if len(sharedMCP) > 0 {
		cfg.MCPClients = sharedMCP
	}
	override("MCPPrefix", cfg.MCPPrefix != "" && r.cfg.MCPPrefix != "")
	if r.cfg.MCPPrefix != "" {
		cfg.MCPPrefix = r.cfg.MCPPrefix
	}
	override("Spill", cfg.Spill != nil && r.cfg.Spill != nil)
	if r.cfg.Spill != nil {
		cfg.Spill = r.cfg.Spill
	}
	// 记忆：Runtime 显式注入时覆盖；否则按工作区共享自动 wiki（Config.Memory
	// 显式设置时保留宿主自己的那份）。
	if r.cfg.Memory != nil {
		override("Memory", cfg.Memory != nil)
		cfg.Memory = r.cfg.Memory
	} else if cfg.Memory == nil {
		cfg.Memory = r.memoryLockedFor(root)
	}
	cfg.Provider = r.provider

	if len(overridden) > 0 {
		// 覆盖必须留痕：宿主以为自己在按 Agent 配的能力，实际用的是 Runtime 的。
		slog.Warn("kit: Runtime 覆盖了这些 Config 字段（进程级能力归 Runtime 所有）",
			"fields", strings.Join(overridden, ", "))
	}
	return nil
}
