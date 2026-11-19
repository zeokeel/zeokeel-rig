# 快速开始：把 Rig 接进你的应用

本文面向**第一次集成 ZeoKeel Rig 的人**：你已经决定在这个 Go 项目里嵌入一个 agent，
现在需要知道环境要求、装什么、三种组装方式怎么选、审批和沙箱怎么接、会话状态放哪，
以及出错时先看哪里。读完你应该能用三十行以内的代码跑起一个可用的 agent，并知道每
一个默认值来自哪里。

本文所有 API 名、字段名与默认值都以当前源码（release `v0.23.4`）为准；工具数量只引用
四个口径，见 [README.md](README.md) 的维护约定。分层设计与内部机制见
[ARCHITECTURE.md](ARCHITECTURE.md)。

## 1. 环境要求

| 项 | 要求 | 说明 |
| --- | --- | --- |
| Go | **1.27+** | `go.mod` 声明 `go 1.27`；本模块用到了该版本的标准库能力 |
| cgo | **不需要**，构建与测试一律 `CGO_ENABLED=0` | CI 里没有 cgo 构建路径 |
| 依赖 | **恰好一个**：`mvdan.cc/sh/v3` | POSIX shell 解析器，只被 `sandbox` 策略层用来判定命令；不引入数据库驱动、HTTP 框架或 agent 框架 |
| 根包依赖 | `rig` 只能依赖标准库 | 根包是导入图的叶子，任何宿主导入它都不会被拖进额外依赖 |
| 平台 | Linux 跑全量单测；**Windows 有阻塞的真机 CI job**；macOS 真机在独立 workflow；FreeBSD 只做交叉编译 | `bash`/`job_*`/`terminal_*` 需要 unix `/bin/sh`；Windows 上用 `pwsh`（ConPTY 不可用时回退 pipe）。交叉编译 + `GOOS` vet 覆盖 windows/darwin/freebsd |
| 网络 | 只有调用模型 API 时才需要 | `AllowNetwork` 默认 `false`，`web_fetch` 因此默认不注册 |
| 凭据 | 默认读环境变量 `OPENAI_API_KEY` | 也可以用 `Config.APIKey` 直接给，或换 `Config.APIKeyEnv` 指向别的变量 |

没有 API key 也能 `go build`：`kit.New` 构造 provider 时不需要 key，**第一次 `Run` 才会
返回 provider 错误**。本地模型（Ollama / LM Studio）默认不需要鉴权，见 §4 的
`ProviderKind`。

## 2. 安装

```sh
go get github.com/zeokeel/zeokeel-rig@v0.23.4
```

当前 release 是 **v0.23.4**，模块内的 `rig.Version` 与 tag 同步（发布流程会校验）。
仓库里自带三个可直接运行的程序，想先看效果可以不用写代码：

```sh
OPENAI_API_KEY=... go run ./examples/quickstart "列出当前目录并总结"
OPENAI_API_KEY=... go run ./examples/agent "list /tmp and summarize it"
go run ./examples/custom-tool          # 离线运行，不需要 key
```

## 3. 最小可用 agent

`kit` 是"一次 import"的装配层：provider、完整沙箱工具集、审批门禁、内存历史都在一次
`New` 里接好。下面这段与 [examples/quickstart/main.go](../examples/quickstart/main.go)
的调用序列一致（完整文件包含 `package`、`import` 与 `fail` 辅助函数；仓库根
`README.md` 的第一段 Go 代码块必须与该文件逐字节相同，可用 `make quickstart` 校验）：

```go
a, err := kit.New(kit.Config{
	Model: "gpt-4o-mini",
	// 接入 OpenAI 兼容网关时：
	// BaseURL: "https://api.deepseek.com/v1", APIKeyEnv: "DEEPSEEK_API_KEY",
})
if err != nil {
	fail(err)
}

ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
defer cancel()

_, result, err := a.RunText(ctx, "quickstart", prompt, func(event kit.Event) {
	if event.Type == kit.EventTextDelta {
		fmt.Print(event.Text)
	}
})
if err != nil {
	fail(err)
}
fmt.Printf("\nfinish=%s iterations=%d tokens=%d\n",
	result.FinishReason, result.Iterations, result.Usage.TotalTokens)
```

三个要点：

- `kit.New` 只强制要求 `Model`；其余字段的零值都有文档化默认（见 §4-A 与
  [CONFIG.md](CONFIG.md)）。
- `RunText(ctx, sessionID, prompt, emit)` 返回聚合后的助手文本、`kit.RunResult` 与
  error；`emit` 与 `RunRequest.Emit` 收到的是同一批 `kit.Event`。
- **事件是同步回调**：它在运行 goroutine 上被调用，不能阻塞。需要扇出（多个 UI 订阅
  者、写入 channel）时自己缓冲。

## 4. 三种组装方式

三条路的能力从少到多，代价也从少到多。先判断你需要哪一种：

| 方式 | 你得到什么 | 代价 |
| --- | --- | --- |
| A. `kit.New` | provider + 默认工具集 + 默认门禁 + 历史，一行配置 | 细节被默认值接管；`Config` 只暴露部分宿主能力 |
| B. `full.All`/`NewRuntime` + `agent.New` | 全部工具、宿主接口、MCP/LSP、子代理、自有 Gate/History | 需要自己装配策略、门禁、历史，并自己关后台句柄 |
| C. 按包取用 | 只要其中一项能力（例如只用 `sandbox` 做路径校验） | 模板代码要自己写；不享受任何默认值 |

### A. kit：零配置先跑起来

```go
a, err := kit.New(kit.Config{
	Model:     "deepseek-chat",
	BaseURL:   "https://api.deepseek.com/v1",
	APIKeyEnv: "DEEPSEEK_API_KEY",
	Root:      "/path/to/workspace",
	Asker:     myAsker, // 实现 rig.Asker，见 §5；没有 UI 时留空
})
```

接哪个模型由这几个字段决定：`ProviderKind` 选协议（空或 `openai` 走 OpenAI 兼容；
`ollama` 默认 `http://127.0.0.1:11434`、`lmstudio` 默认 `http://127.0.0.1:1234`、
`local` 是本地服务共用的基座）、`BaseURL` 覆盖地址、`APIKey`/`APIKeyEnv` 给凭据、
`ReasoningEffort` 传推理强度（只对默认 OpenAI 兼容 provider 生效）、`ModelContextTokens` 用于推导压缩预算（本地模型通常知道
自己的窗口，填上它比默认 128k 更准）。本地服务可用 `kit.AvailableModels(ctx, kind, cfg)`
列出可用模型。完整字段见 [CONFIG.md](CONFIG.md)。

零配置（`kit.New(kit.Config{Model: ...})`）具体得到什么：

| 项 | 零配置结果 |
| --- | --- |
| 工具面 | **39 个注册、16 个初始 active**（核心 13 + `tool_search` + `memory` + `skill`）；接上 `Config.Presenter` 后 **40 / 17** |
| 权限模式 | `Mode` 默认 `workspace-write`（可写工作区、不联网） |
| 审批 | `Approval` 默认 `ask`；**两档门禁**：只读工具免问，其余（写文件、`bash`/`pwsh`，**包括危险命令**）问人；默认不设超时 |
| 没有 Asker 时 | 其余调用被拒，理由文案是"当前没有可用的审批通道（宿主未提供 Asker）；请接上 Ask，或把审批模式改为 auto" |
| shell | 默认可用（`DisableShellAccess=false`，不限制命令名）；安全性由人工审批负责（**不做**命令风险分类），不靠程序白名单 |
| 轮数 | `MaxIterations` 未配置（0）时按 **200** 轮（`DefaultSafeMaxIterations`，成本护栏）；显式负数=真的不限，>0=显式上限 |
| 上下文 | 压缩默认开：预算 = 模型上下文 × 0.6、下限 16k；默认 `compact.WindowCompactor` |
| 动态工具加载 | 默认开：注册 ≠ 发给模型，模型用 `tool_search` 激活 |
| 保护路径 | **不再提供**（DSH 对齐）：kit 不追加 `.git`/`~/.ssh` 之类的写保护；要保护请自己构造 `sandbox.Policy` |
| 记忆 | 默认用按工作区分区的文件 wiki（目录 0700、页面 0600，不写进仓库） |
| `SandboxGrants` | 默认开：`sandbox_status` / `request_sandbox_access` 注册但不进初始 active 集；`kit.Bool(false)` 关闭 |
| `AutoAllowSandboxedCommands` | 默认开：挂了 OS runner 后，已沙箱化的 shell 命令免审批、不看命令文本（仍写 `approval.response`） |
| 默认关闭 | `PTC`、`AllowNetwork`、`GitUndo` |

四个权限档（`Config.PermissionPreset`）：`read-only` / `workspace-write`（默认）/
`danger-full-access` / `auto`（全权 + `ToolReviewer` 预审）。想一键切换"只读 / 可写 /
全权 / 自动预审"，用档位而不是自己拼 `Mode`+`Approval`。

"默认只读"的准确含义是**默认门禁只放行只读调用**，不是把策略设成 `read-only`：写文件、
可变命令这类调用会走到第 3 层（问人），没接 Asker 时就被拒绝。想让写操作直接可用，要么
接 `Asker`，要么显式改 `Approval`，不要靠"多点几次弹窗"绕过去（见 §5）。

工具面是这样算出来的（三个口径别混，详见 [TOOLS.md](TOOLS.md)）：

1. `full.NewRuntime(full.AllOptions{Policy, Root, …})` 构造完整静态工具集；
2. 按 `ToolPreset` 过滤（默认 `personal`，55 个名字；`standard` 73、`minimal` 8、`all` 83）；
3. **零配置体检**摘掉"当前组合里永远无法成功"的 15 个：缺 `Searcher` 的 `web_search`、
   缺 `SessionLog` 的 5 个 `session_*`、kit 没有注入入口的 `get_goal`/`create_goal`/
   `update_goal`/`plan_review`/`exit_plan_mode`、缺 `Presenter` 的 `present`、缺
   `Asker` 的 `ask_user`、`AllowNetwork=false` 时的 `web_fetch`、非 Windows 上的
   `pwsh`；另外没有 tracker 的 `undo` 也不进默认集。

宿主每接一项依赖就按项加回：`Asker` +1（`ask_user`）、`Searcher` +1（`web_search`）、
`SessionLog` +5（`session_*`）、`AllowNetwork` +1（`web_fetch`）、`Presenter` +1
（`present`）。另有独立开关带来的一次性加项：`PTC` 的 `run_code`（不在预设里）与 git
工作区里 `GitUndo` 的 `undo`。注册数随加回项单调增加；**初始 active 只在接上
`Presenter` 时变化**（`present` 进入初始集），`sandbox_status` /
`request_sandbox_access` 这类 discoverable 工具始终不进初始 active 集。

两个容易踩的边界：

- **`Config.Tools` 非 nil 时，kit 不运行零配置体检、也不套默认预设**：你传的工具集就是最终集合（显式写 `ToolPreset` 时仍会按名字过滤），
  kit 不会静默裁剪（但它会 warn 哪些依赖对自建集合不起作用）。
- **`Config.Tools` 与 `Config.SandboxRunner` 不能同时设置**：自建工具集不经过 OS 沙箱
  包装，同一份配置出现两种行为属于"以为有保护其实没有"，`kit.New` 直接报错。要在自建
  集合上挂 runner，请用 `full.All(AllOptions{SandboxRunner: …})`。

`kit.Agent` 除 `Run`/`RunText` 外还提供 `Shutdown`、`Preflight`、`Tools`、
`RegisteredTools`、`ActiveTools`、`ActivateTools`、`SessionStore`、`Policy`、
`UndoTracker`、`MemoryWiki`、`CompactNow`。`Profile` 字段（`careful`/`personal`/`local`）
只填你没显式设置的 `Approval`、`ToolPreset`、`ProviderKind`、`MaxIterations`。

### B. full.All + agent.New：完整能力

需要 MCP/LSP、宿主接口（`ask_user`/`present`/goal/plan）、子代理、自定义 Gate 或
CheckpointStore 时走这条路。两条硬要求：

1. **必须传 `Root` 或带 `Roots` 的 `Policy`**。`full.All(full.AllOptions{})` 只构造
   **78** 个工具：`apply_patch`/`search`/`undo` 不构造，其余文件工具一律返回
   `sandbox: denied: no roots configured`。
2. **用 `full.NewRuntime` 而不是 `full.All`**：前者把后台句柄（`jobs.Manager`、
   `terminal.Manager`）交出来，`Runtime.Close()` 能一次关掉；`All` 保持旧签名，句柄
   就地丢弃，宿主关窗口后子进程可能继续跑。

```go
policy := sandbox.New(
	sandbox.WithRoots(root),
	sandbox.WithMode(sandbox.ModeWorkspaceWrite),
)
runtime, err := full.NewRuntime(full.AllOptions{
	Policy: policy,
	Root:   root,
})
if err != nil {
	return err
}
defer runtime.Close()

prov, err := openai.New(openai.Config{DefaultModel: "gpt-4o-mini"})
if err != nil {
	return err
}
a, err := agent.New(rig.AgentSpec{
	Provider: prov,
	Model:    "gpt-4o-mini",
	Tools:    runtime.Tools,
	Gate:     gate.PersonalAskWithoutAsker(), // 有 UI 时换成 gate.PersonalAsk(gate.Ask(yourAsker))
	History:  session.NewMemory(),
})
```

`full.AllOptions` 最常用的字段：

| 字段 | 作用 |
| --- | --- |
| `Policy` | 文件/shell/terminal/任务工具共用的沙箱策略。nil 时按 `Root` 造一个空策略（无 shell、无网络） |
| `Root` | 工作区根：`apply_patch`/`search`/`undo`/`working_directory`/`inspect` 的边界 |
| `SandboxRunner` | OS 级 runner（`sandbox/osrunner`）；非 nil 时 shell、jobs、terminal、worktree 在 spawn 前经 `Confine` |
| `Escalator` | 逐命令提权审批通道（`builtin.Escalator`），见 §5 |
| `SandboxGrants` | 追加 `sandbox_status` / `request_sandbox_access`（默认 false；kit 里默认开） |
| `Host` | `hosttools.Options`：`Asker`/`FilePresenter`/`Plan`/`Goal`/`Interactive`，喂给 `ask_user`、`present`、plan 与 goal 工具 |
| `Asker` / `Presenter` | `Host.Asker` / `Host.FilePresenter` 的便捷入口；设了 `Asker` 会顺带把 `Interactive` 打开 |
| `Wiki` / `Memory` | 记忆存储：现行是 `memory.Wiki`，`Memory`（扁平 `rig.MemoryStore`）是历史字段 |
| `Scheduler` / `SessionLog` / `Searcher` | `schedule_*` / `session_*` / `web_search` 的后端；nil 时对应工具报配置错误或降级 |
| `MCPClients` / `LSP` | 额外的 MCP 工具/资源/提示与 `lsp_*` 工具（见 §8） |
| `Runner` | `spawn_teammate`/`wait_agent`/`team_*`/`subagent` 等编排工具的执行器；nil 时工具仍生成，调用报配置错误 |
| `Skill` / `TeamTasks` / `Reviewer` | 技能库、团队任务板、`code_review`/`auto_review` 的评审器 |
| `SpillStore` | 超大工具结果的存储；非 nil 时追加 `spill_read` 读回工具 |
| `BackgroundJobs` | 注入宿主自己的 `jobs.Manager`（与 bash 后台、`job_*`、terminal 共享） |
| `UndoTracker` | 回合快照跟踪器；nil 时 full 自建一个（没有快照，`undo` 调用会报错） |

`rig.AgentSpec` 的关键字段：`Provider`、`Model`、`Tools`、`SystemPrompt`、`History`、
`Gate`、`MaxIterations`、`InitialActiveTools`、`ContextBudget`、`Compactor`、`Spill`、
`CheckpointStore`、`Logger`、采样选项（`Temperature`/`TopP`/`MaxTokens`）。

注意 `agent` 层的轮数默认值是 **0（不限）**，成本护栏在 `kit` 层（200 轮）。走 B 路线
时要自己决定 `MaxIterations` 或接 `costmeter`，否则模型在两种工具之间来回摇摆可以烧
很久。

手工组装到子包时还有两条容易漏的接线：`apply_patch` 必须拿到 `patch.WithPolicy(policy)`
（否则 read-only 模式与可写根围栏对它全部失效），shell 的后台执行必须接
`builtin.WithBackgroundJobs(manager)`（`full` 内部已经接好）。

### C. 按包取用：模块化承诺

模块化在这个仓库里的定义不是"代码分了包"，而是**每个能力都能被单独拿走用**。三条
守门测试保证它不会悄悄退化成"只能整套使用"（详见 [ARCHITECTURE.md](ARCHITECTURE.md) §5）：

1. 叶子能力包不得 import `kit` 或 `tools/full`；
2. 根包 `rig` 只依赖标准库，且不 import 本仓任何子包；
3. 只有 `kit` 同时装配 provider 与工具面。

"必须能单独使用"的名单是 `internal/rigdoc` 的 `leafCapabilities`（33 项），包含
`events`、`costmeter`、`modelprice`、`instructions`、`secrets`、`session`、
`session/store`、`session/compact`、`storage`、`storagetest`、`tools/{undo,patch,search,
builtin,jobs,terminal,mcp,lsp,ptc,worktree,skill,memory,subagent,catalog,presets}`、
`internal/{fsguard,gitsnap,diffpreview}`、`provider/{local,ollama,lmstudio,openai}`、
`sandbox`。新增可选能力包时要把它加进这张表，否则这条承诺对该包没有守护。

一个真·按包取用的例子（只用 `sandbox` 做路径校验 + 只用 `builtin` 的文件工具，连
agent 都不需要）：

```go
policy := sandbox.New(
	sandbox.WithRoots(root),
	sandbox.WithMode(sandbox.ModeReadOnly),
)
if err := policy.CheckPath(filepath.Join(root, "go.mod")); err != nil {
	return err // 路径越过可写面：检查失败即拒绝
}

reg := tools.NewRegistry(builtin.NewWithPolicy(policy)...)
result, err := reg.Invoke(context.Background(), rig.ToolCall{
	ID:        "call-1",
	Name:      "read_file",
	Arguments: `{"path":"go.mod"}`,
})
```

同样的方式可以只取 `session/store` 做 NDJSON 会话落盘、只取 `events` 写版本化事件流、
只取 `costmeter`+`modelprice` 算钱、只取 `provider/openai` 谈模型。

## 5. 审批怎么接

### Asker 接口

```go
type Asker interface {
	Ask(ctx context.Context, question string, options []string) (answer string, err error)
}
```

`options` 是 `gate` 提供的规范选项：`"allow"`（只放行本次）、`"allow-always"`（放行本次
并按**工具名**记住后续不再问）、`"deny"`。答案比较忽略大小写与首尾空白；**无法识别的
答案按拒绝处理**（fail-closed）。Asker 可能被并发调用，实现要线程安全。

接上有三种写法：

```go
// 1) 让 kit 组装默认两档门禁，Asker 只负责"问人"这一步
a, err := kit.New(kit.Config{Model: "gpt-4o-mini", Asker: myAsker})

// 2) 自己拿 gate.Ask（可以加超时、提示文案、观察者），交给完整装配路径
hostGate := gate.Ask(myAsker,
	gate.WithAskTimeout(10*time.Minute), // 默认不超时；这里显式给一个上界
	gate.WithAskPrompt(gate.SandboxGrantPrompt),
)

// 3) kit + 显式 Gate：完全接管门禁（要命令风险硬拦就自己叠最外层）
a, err = kit.New(kit.Config{Model: "gpt-4o-mini", Gate: gate.PersonalAsk(hostGate)})
```

`gate.Ask` 的行为：`allow` → 放行；`allow-always` → 放行并记住该工具名；`deny` 或无法
识别 → 拒绝（作为工具错误回给模型，不中止 run）；context 取消或 `WithAskTimeout` 到期 →
返回包装 `gate.ErrApprovalExpired` 与 `ctx.Err()` 的错误；Asker 自身报错 → 按审批基础设施
故障处理。`gate.WithAskObserver` 可以观察 `requested`/`decided` 两个阶段，kit 在接
`EventWriter` 时用它写 `approval.request`/`approval.response` 事件。

默认门禁（`Approval: "ask"`）是**两层**：

1. **工具级白名单**（`gate.AllowThenAsk` + `gate.SandboxReadOnlyTools`）：`read_file`、
   `glob`、`grep` 这类只读工具免问；
2. **人工审批**：其余一切（写文件、`bash`/`pwsh`、后台任务，**包括 `rm -rf /` 这类
   危险命令**）交给你的 Asker。**默认不设超时**：一直等，只有取消本轮才撤回；要上界
   设 `Config.AskTimeout`，或自己用 `gate.WithAskTimeout`。

默认**不含** `gate.CommandRiskGate`（DSH 对齐：命令风险分类不进默认门禁）。要"命中即
拒"，显式把它叠在 `AllowThenAsk` **之外**（最外层），否则每条 `bash` 都会先弹一次窗。

### 没有 Asker 时会发生什么

kit 会退到 `gate.PersonalAskWithoutAsker()`：只读照放、其余拒绝并明确告诉
模型"当前没有可用的审批通道（宿主未提供 Asker）；请接上 Ask，或把审批模式改为 auto"。
这是"拒绝但给理由"，run 继续，模型可以换办法。

三种**不要**用的写法：

- `gate.PersonalDefault(nil)` / `gate.PersonalAsk(nil)`：不会中断 run，但每次需要审批的
  调用都必然被拒——那是配置疏漏，不是可用状态。
- `gate.PersonalAsk(gate.DenyAll())`：文案会说 `approval=never`，与真实状态不符，会误导
  模型。
- 裸 `gate.AllowAll()`：`bash` 里的 `rm -rf /` 也会被放行。

### auto / never 不等于"什么都放行"

- `Approval: "auto"` = `gate.PersonalAuto()` = **`AllowAll()`**：不问人，也不分类命令
  （`rm -rf /` 会执行）。围栏仍由 `Mode` 表达；要危险命令硬拦请显式叠
  `gate.CommandRiskGate`。
- `Approval: "never"` = `gate.PersonalNever()`：全部拒绝并说明 `approval=never`。
- 挂了 `SandboxRunner` 之后，`AutoAllowSandboxedCommands`（默认开）会让**已沙箱化的**
  shell 命令免问（**不看命令文本**），判据：工具是 shell 类、runner 非 nil 且自报能
  强制、策略不是 `danger-full-access`。文件工具不经 OS runner，照旧问人。

### 逐命令提权（`require_escalated`）

这是"沙箱挡住了正常需求"的一条出口（另一条是 `request_sandbox_access` 申请可写面），
**只是对执行隔离的一次豁免，不是对策略的豁免**：

- 模型在 `bash`/`pwsh` 调用里传 `sandbox_permissions: "require_escalated"` 与
  `justification`（**至少 10 个字符**）；
- 只有**人工批准**后，这一条命令才不再被 OS runner 包装；`Policy` 的可写面、
  命令允许/禁止名单、workdir、超时、输出上限全部不变；
- `approval=auto` 与 `never` 档一律拒绝（提权本质是人工判断）；
- 宿主通过 `builtin.WithEscalator` 或 `full.AllOptions.Escalator` 注入
  `builtin.Escalator`（`Escalate(ctx, EscalationRequest) (bool, error)`）；kit 用
  `Config.Asker` + 审批档位自动实现它，只在 `ask` 档且 Asker 非 nil 时可用；
- 允许与拒绝都会写 `sandbox.escalation` 事件（接 `EventWriter` 时）。

不要把逐命令提权和已删除的 `tools/escalate` 包混为一谈：那个包（整包提权通道）已经
删除，要放宽权限请用 `danger-full-access` 或审批。

## 6. 沙箱怎么开

核心策略层（`sandbox`）永远是**进程内围栏**：它拦住越界路径、只读模式下的写、被禁的
命令与 URL。要更强的边界就注入 OS 级 runner（`sandbox/osrunner`，opt-in、纯 Go、无
cgo、零新增依赖）。

```go
runner, runnerErr := osrunner.New(osrunner.Config{})
cfg := kit.Config{
	Model: "gpt-4o-mini",
	Root:  root,
}
if runnerErr != nil {
	// 环境问题不该让应用打不开：把原因交给 Preflight 解释，不要静默裸跑
	cfg.SandboxUnavailableReason = runnerErr.Error()
} else {
	cfg.SandboxRunner = runner
}
a, err := kit.New(cfg)
if err != nil {
	return err
}
report := a.Preflight()
if !report.Enforced {
	// 只有进程内围栏：把 report.Summaries / Warnings / NextSteps 如实展示给用户
}
```

`osrunner.New` 按平台选后端：Linux 依次试 bubblewrap、内置纯 Go Landlock；macOS 用系统
自带 `sandbox-exec`（Seatbelt）；Windows 用受限令牌 + ACL。没有任何可用后端时返回
`sandbox.ErrSandboxUnavailable`，调用方必须拒绝执行（fail-closed），不能返回未受限的
argv 裸跑。

`Preflight()` 返回 `sandbox.PreflightReport`：

| 字段 | 含义 |
| --- | --- |
| `Mode` | 实际生效的权限模式 |
| `Enforced` | 有没有**真的** OS 围栏（false = 只有进程内校验） |
| `Backend` / `Enforcement` | 后端名与覆盖程度（`full`/`partial`/`none`）；`partial` 的具体缺口由 `sandbox_status` 与 `RunnerLimits` 说清 |
| `Network` | `off` / `loopback` / `full` / `not-governed` |
| `Summaries` / `Warnings` / `NextSteps` | 人话结论、需要知道但不会挡住你的事实、被挡住时点哪里 |

两个注入点接的是同一种东西（`sandbox.Runner`，`osrunner.New` 的返回值直接满足）：

- 走 kit：`Config.SandboxRunner`，只在 `Config.Tools` 为 nil（默认工具集）时生效；
  两者同时设会报错（见 §4-A）；
- 走完整装配：`full.AllOptions.SandboxRunner`，在构造工具集时一起给，工具从构造那一刻
  就绑定这个 runner。

三条配置事实：

- `Config.Sandbox` 是三态：`nil`（默认尽力而为）、`true`（明确希望启用，不可用也不报错）、
  `false`（明确不要；此时再注入 `SandboxRunner` 是自相矛盾，`kit.New` 报错）。
- 挂了 runner 后，`bash`/`pwsh`、`jobs`、`terminal`、`worktree` 与 `run_code` 的解释器
  进程在 spawn 前都会经 `Runner.Confine` 包装；`Confine` 失败即拒绝执行。文件工具
  （`write_file`/`edit_file`/`apply_patch`…）不走 runner，仍由进程内策略把关。
- 沙箱可写面 = `Roots()` ∪ `AdditionalWritableRoots()` ∪ 平台临时目录 −
  `ProtectedPaths()`。运行期放宽只能由审批通过后 `Policy.AddWritableRoot` 完成
  （`request_sandbox_access` 工具就是这样：工具自身不审批，审批由宿主的 Gate 决定，
  默认 Gate 会拒绝它；预算默认 5 次）。

OS runner 只治理**文件效果**，不承诺读隔离、进程可见性或完整网络隔离；要内核级边界请
把宿主跑在容器/VM 里，见 [ARCHITECTURE.md](ARCHITECTURE.md) §6 与
[SANDBOX-OS.md](SANDBOX-OS.md)。

## 7. 会话与持久化

会话状态有四档，选一个即可：

| 方式 | 形态 | 适合 |
| --- | --- | --- |
| 默认（什么都不传） | `session.NewMemory()`，进程内、按 `SessionID` 隔离 | 单次会话、测试 |
| `Config.SessionStoreDir` | 一会话一个 NDJSON，目录即库；启动时扫描索引、崩溃自愈 | 桌面应用默认 |
| `Config.SessionStore` | 宿主自己持有 `*store.Dir`（与 `SessionStoreDir`/`History` 互斥） | "最近会话"侧栏要用**同一个实例** `List`/`Load` |
| `Config.History` | 自己实现 `rig.History`（`Load`/`Append`；可选 `HistoryReplacer`） | 已有数据库/远端同步 |

```go
a, err := kit.New(kit.Config{
	Model:           "gpt-4o-mini",
	SessionStoreDir: dir, // 单目录单实例：store 不做跨进程加锁
})
if err != nil {
	return err
}
defer a.Shutdown(ctx) // 收尾在飞 Run、事件、Storage、后台任务

sessions := a.SessionStore() // kit 打开的这个 *store.Dir；未启用持久化时为 nil
_ = sessions                // 用它做 List/Load，不要自己再 NewDir 同一目录
```

`History`、`SessionStoreDir`、`SessionStore` **三者互斥**，同时设置会报错。
`store.Dir` 不做跨进程/跨实例锁：同一目录只能由一个实例写，重启时先释放前一个实例。

`Agent.Shutdown(ctx)` 的收尾顺序是：取消并等待在飞 Run → 冲刷事件 → 关闭 `Storage` →
关闭后台任务。`Config.Storage` 只需要一个 `Close() error`（`*session/store.Dir`、
`*session/sqlite.Store` 都满足）；宿主**自己传入**的 `SessionStore` 不在自动关闭范围内，
要一起收尾就在 `Storage` 里登记一次。`Config.BackgroundJobs` 只在自建工具集时需要——
默认工具集由 `full.NewRuntime` 构造，两个管理器已被自动登记。

还有两层可选状态：

- `Config.CheckpointStore`：回合边界（模型请求前、有副作用的工具执行前、回合结束）写
  恢复检查点，崩溃后下一轮自动修复中断回合；
- `Config.Spill`：超过阈值（默认 16 KiB）的工具结果落盘或写入 `SpillStore`，模型只看到
  摘要与句柄；接上 `SpillStore` 时 `full` 会注册 `spill_read` 读回工具。

> **未发布内容**：`session/sqlite`（独立子模块，`go.mod` 要求 `v0.24.0`）、`storage`/
> `storagetest` 聚合契约、`memory` 公共契约与 `rig.SpillStore` 已在 HEAD 源码中，但
> **不在 v0.23.4 的发布面**，随下一版本发布。因此现在**不能**直接 `go get` 那个子模块
> ——它依赖的 `v0.24.0` 主模块尚未发布；需要 SQLite 会话持久化请等下一版本，或按
> [sqlite.md](sqlite.md) 的说明从源码侧接入。

## 8. 常见任务配方

### 8.1 流式打印

```go
_, result, err := a.RunText(ctx, "s-1", prompt, func(ev kit.Event) {
	if ev.Type == kit.EventTextDelta {
		fmt.Print(ev.Text)
	}
})
```

需要更多事件时用 `RunRequest.Emit`：`EventTurnStart`/`EventTurnEnd`（每个模型轮次）、
`EventTextDelta`/`EventReasoningDelta`、`EventToolCall`/`EventToolResult`、`EventUsage`、
`EventMessage`、`EventError`、`EventContextCompacted`（上下文被折叠时带压缩前后条数）。

### 8.2 读文件

`read_file`、`list_dir`、`glob`、`grep` 都在默认初始 active 集里，模型可以直接用（路径
相对于 `Root`，越过可写面会被拒绝）。如果宿主自己要用工具读，不必经过模型：

```go
reg := tools.NewRegistry(builtin.NewWithPolicy(policy)...)
result, err := reg.Invoke(ctx, rig.ToolCall{
	Name:      "read_file",
	Arguments: `{"path":"go.mod"}`,
})
```

### 8.3 自定义 Tool

实现 `rig.Tool` 的四个方法即可（完整可运行示例见
[examples/custom-tool/main.go](../examples/custom-tool/main.go)；下面的 `Invoke` 用
`strings.Fields` 代替示例里的手写分词，只为缩短篇幅）：

```go
type wordCount struct{}

func (wordCount) Name() string        { return "word_count" }
func (wordCount) Description() string { return "Count the words in a text." }
func (wordCount) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "text to count"},
		},
		"required":             []any{"text"},
		"additionalProperties": false,
	}
}
func (wordCount) Invoke(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return rig.ToolResult{Content: "invalid arguments: " + err.Error(), IsError: true}, nil
	}
	return rig.ToolResult{Content: fmt.Sprintf("%d words", len(strings.Fields(args.Text)))}, nil
}
```

挂进 agent 有两条路：`agent.AgentSpec.Tools` 里 append 它（B 路线），或
`kit.Config.Tools`（A 路线，注意此时 kit 不跑零配置体检、也不套默认预设）。新增**内置**工具还要改
账本与文档，见 [COVERAGE.md](COVERAGE.md)。

### 8.4 MCP / LSP 客户端

```go
// MCP：stdio 启动一个 server，把它暴露的工具并进完整工具集
client, err := mcp.Start(ctx, mcp.Config{
	Command: "npx",
	Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", root},
})
if err != nil {
	return err
}
defer client.Close()

toolSet := full.All(full.AllOptions{
	Root:       root,
	Policy:     sandbox.New(sandbox.WithRoots(root)),
	MCPClients: []*mcp.Client{client}, // 远端 server 用 mcp.StartHTTP
})
```

```go
// LSP：接一个语言服务器，得到 lsp_* 工具
client, err := lsp.Start(ctx, lsp.Config{
	Command:    "gopls",
	LanguageID: "go",
	Dir:        root,
})
if err != nil {
	return err
}
defer client.Close()

toolSet := full.All(full.AllOptions{
	Root:   root,
	Policy: sandbox.New(sandbox.WithRoots(root)),
	LSP:    client,
})
```

MCP 工具默认带 `mcp__<server>__<tool>` 前缀（`AllOptions.MCPPrefix` 可改）。上面两段得到的
`toolSet` 交给 `agent.New` 的 `Tools`（要能关后台句柄就用 `full.NewRuntime` 的
`runtime.Tools`）；它们与其它工具一样要过审批门禁。

### 8.5 事件流（NDJSON）

```go
f, err := os.Create("run.ndjson")
if err != nil {
	return err
}
defer f.Close()

a, err := kit.New(kit.Config{
	Model:       "gpt-4o-mini",
	EventWriter: f, // 一个 Run 一条流；seq 在同一 writer 上连续
})
if err != nil {
	return err
}
defer a.Shutdown(ctx) // 关停时冲刷事件
```

线格式是版本化的（`events.Version = 1`）：`events.Envelope` + `events.NewDecoder` 可以
重放与校验。`rig.Event` 是进程内事件、`events.Envelope` 是落盘事件，两者不要混用。
`kind` 覆盖 `run.start`/`run.end`、`message`/`message.delta`、`reasoning.delta`、
`tool.call`/`tool.result`、`tool.activated`、`approval.request`/`approval.response`、
`sandbox.escalation`、`diff`、`notice`、`error`；用量以 `notice` 行写入，同时累加进
`Meter`（如果接了）。

### 8.6 成本计量

```go
meter := costmeter.New()
a, err := kit.New(kit.Config{
	Model:         "gpt-4o-mini",
	Meter:         meter, // kit 按 EventUsage 累计
	MaxRunCostUSD: 0.5,   // 超过上限以明确错误结束并取消本次 Run
})
if err != nil {
	return err
}
defer a.Shutdown(ctx)

// ... 正常使用 a.Run / a.RunText ...

summary := meter.Session()
fmt.Println(summary.CostUSD, summary.Estimated, summary.UnpricedModels)
```

`Summary` 里 `Estimated=true` 表示金额是估算（价目表可能过时，或含未定价模型）；要判断
"是否漏算"看 `UnpricedModels` 是否为空。价目表来自 `modelprice`，它是 UI 估算，不是账单。

### 8.7 子代理

```go
runner, err := agent.NewRunner(parentSpec, agent.RunnerOptions{
	MaxDepth:      2, // 允许的子 agent 层数；负值禁用
	MaxConcurrent: 3, // 同时存活的子 agent 上限
})
if err != nil {
	return err
}

toolSet := full.All(full.AllOptions{
	Policy: policy,
	Root:   root,
	Runner: runner,
})
```

`agent.NewRunner` 复用父 agent 的 provider/模型/门禁/采样选项，给子 agent 过滤后的工具集
（默认排除编排工具，避免无限递归）与**全新**的历史。子 agent 自己永远不碰父会话。
`subagent`/`spawn_teammate` 是**异步**的：用 `wait_agent`/`team_*` 收结果，完成通知会
注入下一步。

注意 `personal` 预设**不含**任何多智能体编排工具（个人客户端是"一个人和一个 agent
结对"）；要用它们请把 `ToolPreset` 换成 `standard`/`all` 并注入 `Runner`。

## 9. 排障

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `rig: model is required` | `Config.Model` / `AgentSpec.Model` 为空 | 填模型名；`RunRequest.Model` 只能覆盖，不能替代 |
| provider 返回 401 / 缺少凭据 | 默认读 `OPENAI_API_KEY`；网关通常要换 key 与地址 | 设 `APIKeyEnv`/`APIKey`/`BaseURL`；本地 Ollama（`127.0.0.1:11434`）与 LM Studio（`127.0.0.1:1234`）默认无鉴权 |
| 模型说"当前没有可用的审批通道" | `Approval=ask` 但没有 `Asker` | 接 `Config.Asker`；无 UI 的批处理场景可改 `Approval: "auto"`（全放行，不再问人） |
| `sandbox: denied: no roots configured` | 手工 `full.All(full.AllOptions{})` 没给 `Root`/`Policy` | 传 `Root` 或带 `Roots` 的 `Policy`；此时只有 78 个工具 |
| `kit: Config.Tools 与 Config.SandboxRunner 不能同时设置` | 自建工具集不会被 OS runner 包装 | 用 `full.All(AllOptions{SandboxRunner: …})` 组装，再传给 `Config.Tools` |
| Windows 上 `bash`/`job_*`/`terminal_*` 失败 | 这些工具需要 unix `/bin/sh` | Windows 用 `pwsh`（ConPTY，不可用时回退 pipe）；跨平台脚本别假设 `bash` 存在 |
| `web_fetch` 总是被拒 | `AllowNetwork` 默认 `false` | 显式打开；即使打开，沙箱仍拒绝 loopback/私有/链路本地/元数据目标 |
| 工具明明注册了，模型说没这个工具 | 动态工具加载：注册 ≠ active | 模型用 `tool_search` 的 `{"activate":[…]}` 激活；宿主可用 `Agent.ActivateTools`；或 `ToolDiscovery: kit.Bool(false)` 关掉发现 |
| 工具不在注册表里 | 被零配置体检摘掉了（缺 Searcher/SessionLog/Presenter/Asker…） | 接上对应依赖，或用 `Config.Tools` 自建集合 |
| 模型反复调同一个工具 | "抽风"或工具结果让它误以为没生效 | 连续 3 轮完全相同会注入提示；连续 5 轮或同轮 ≥3 个相同调用直接返回 `rig: repeated call blocked`。检查工具是否真的成功、参数是否变了 |
| 上下文超限 / 历史被折叠 | 默认预算 = 模型窗口 × 0.6、下限 16k | 观察 `EventContextCompacted`（带压缩前后条数）；压缩失败会降级继续而不是中止。要关自动压缩给负数 `ContextBudget`；小窗口模型请填 `ModelContextTokens` |
| 关窗口后后台任务还在跑 | 后台句柄没人关 | 用 `full.NewRuntime` + `Runtime.Close()`，或 `kit.New` + `Agent.Shutdown(ctx)` |
| macOS 上跑得通但没隔离 | 没装/没选到 OS runner | 看 `Preflight()` / `sandbox_status`；`Enforced=false` 说明只有进程内围栏 |

## 10. 下一步

- 工具清单、计数口径与每个工具的权限/平台影响：[TOOLS.md](TOOLS.md)
- `kit.Config` 每个字段的默认值、优先级与环境变量：[CONFIG.md](CONFIG.md)
- 分层、请求流、契约层与扩展点：[ARCHITECTURE.md](ARCHITECTURE.md)
- 给 AI 的装配指南（含更多陷阱与决策表）：[AI-SDK.md](AI-SDK.md)
- OS 级沙箱细节：[SANDBOX-OS.md](SANDBOX-OS.md)
- 记忆与溢出：[MEMORY.md](MEMORY.md)
- 会话持久化与迁移：[sqlite.md](sqlite.md)

## 相关文档

- [README.md](README.md)：文档索引与维护约定
- [ARCHITECTURE.md](ARCHITECTURE.md)：内部设计与设计边界
- [TOOLS.md](TOOLS.md)：工具目录
- [CONFIG.md](CONFIG.md)：配置全字段
- [AI-SDK.md](AI-SDK.md)：AI 视角的组装指南