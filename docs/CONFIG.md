# 配置参考（`kit.Config`）

本文解决什么问题：**Rig 没有配置文件，那宿主到底该怎么配一个 agent？** 读者是准备
把 Rig 接进自己产品的宿主开发者。读完本文你应该能：选对配置风格、知道每个字段的默认
值与坑、判断一次工具调用会被哪一层拦住、并在几种常见产品形态之间直接抄配方。

文中所有字段、常量、环境变量都对照 `kit/kit.go`、`kit/profiles.go`、
`kit/preflight.go`、`kit/escalation.go`、`gate/`、`sandbox/`、`tools/` 的当前
源码核对过；模型可见的英文文案（门禁理由、工具描述、错误串）不翻译、不改写。

## 1. SDK 是库：不读配置文件，没有全局设置

Rig 是**库**。它不读配置文件、不读 `RIG_*` 之类的配置约定、没有全局单例、没有
命令行 flag，也不探测配置目录。宿主从自己的来源（环境变量、flag、YAML、数据库、
UI 设置面板）加载配置，把普通 Go 值传进构造函数。

唯一的例外是**凭据与几个平台路径**：为了"装好就能用"，provider 会读一个环境
变量取 API key，记忆工具会读平台状态目录变量（见 §8）。除此之外，配置的权威
只有一个：你传给 `kit.New` 的那个结构体。

```go
a, err := kit.New(kit.Config{Model: "gpt-4o-mini", Root: "."})
if err != nil {
    return err // 装配期错误：模型缺失、根不存在、配置自相矛盾…
}
```

### 1.1 三种配置风格

| 风格 | 形状 | 用在哪 | 例子 |
| --- | --- | --- | --- |
| **Config struct + 构造函数** | `New(Config) (*T, error)` | 构造期一次性解析完选项；字段有名、好文档化、好传递 | `kit.New(kit.Config{...})`、`full.All(full.AllOptions{...})`、`openai.New(openai.Config{...})`、`builtin.NewWithPolicyOptions(...)` |
| **Functional options** | `New(opts ...Option)` + `WithX(...)` | 构造后不可变、但选项集合会长的对象（策略、管理器） | `sandbox.New(sandbox.WithRoots(...), sandbox.WithMode(...))`、`jobs.NewManager(jobs.WithPolicy(...), jobs.WithRunner(...))`、`terminal.NewManager(...)` |
| **静态 spec + 每次请求** | `New(Spec)` 之后 `Run(ctx, Request)` | 长生命周期运行时对象，逐轮行为与静态配置不同 | `agent.New(rig.AgentSpec{...})` + `Agent.Run(ctx, rig.RunRequest{...})`；`kit.Agent` 的 `Run`/`RunText` |

选择原则：

- 值**构造后不可变**、旋钮集合稳定 → Config struct。
- 值不可变、但旋钮**会不断追加** → functional options（加选项是 additive 的）。
- 旋钮**属于某一次运行**（模型覆盖、轮数上限、这次用哪个 Gate） → 放在请求上，
  别放在对象上。

三种风格的共同约定：

1. **构造函数校验并返回 error**（不 panic、不静默纠错）：模型缺失、根目录不存在、
   `Tools` 与 `SandboxRunner` 同时设置、`History` 与 `SessionStoreDir` 同时设置……
   都在 `kit.New` 里直接报错。
2. **option 不失败**：传给 `WithX` 的零值/负值通常意味着"用文档化的默认值"（见
   `sandbox` 的超时、输出上限），这样选项可以自由组合。
3. **零值有意义**：`kit.Config{Model: "…"}` 就是可用且刻意保守的 agent；
   `sandbox.New()` 则**什么都不允许**（安全相关的零值 fail closed）。
4. **构造后不可变**：`*sandbox.Policy`、`*openai.Provider`、`*jobs.Manager`
   可并发使用；不要把 `Policy` 交给工具后再改它（运行期放宽可写面请走
   `request_sandbox_access`，见 §5.5）。
5. **additive 演进**：新字段追加、零值保持旧行为，选项只加不改语义；字段/选项的
   移除先经过 `Deprecated` 阶段（见 [VERSIONING.md](VERSIONING.md)）。

## 2. `kit.New(kit.Config{...})` 全字段参考

**只有 `Model` 是必填**。其余字段的零值都有文档化默认值。下面按用途分组：每组
先给表，再单独说明该组里最容易踩的坑。

### 2.1 模型与 provider

| 字段 | 类型 | 默认值 | 说明与坑 |
| --- | --- | --- | --- |
| `Model` | `string` | **无（必填）** | 发送给 provider 的模型名。空串 → 返回包装 `rig.ErrModelRequired` 的错误。**即使传了自定义 `Provider` 也必须填**（agent 每轮都要带上模型名）。 |
| `Provider` | `kit.Provider`（`rig.Provider`） | `nil` | 非 nil 时**完全接管**：`BaseURL`、`APIKey`、`APIKeyEnv`、`ProviderKind` 全部被忽略。 |
| `ProviderKind` | `kit.ProviderKind` | `""` → 按 `openai` 处理 | 从零构造 provider 时用哪套协议：`openai` / `ollama` / `lmstudio` / `local`（`kit.BuildProvider` 里 switch，未知值报错）。 |
| `BaseURL` | `string` | `""` → provider 默认地址 | 只在 `Provider == nil` 时生效。网关用法：`BaseURL: "https://api.deepseek.com/v1"`。scheme 必须是 http/https（`openai.New` 校验）。 |
| `APIKey` | `string` | `""` → 读环境变量 | 非空时直接当 bearer token。 |
| `APIKeyEnv` | `string` | `""` → provider 自己的默认名 | openai/local 支持；**ollama 与 lmstudio 不读环境变量**，它们的 `APIKey` 只用于反代鉴权（空 = 不发 `Authorization`）。 |
| `ReasoningEffort` | `string` | `""`（不发该字段） | `""`/`low`/`medium`/`high`，**仅默认 OpenAI 兼容 provider 生效**；自定义 `Provider` 自行处理。 |
| `ModelContextTokens` | `int` | `0` → 按 128k 推导 | 只用于**推导** `ContextBudget`（窗口 × 0.6、下限 16k）。显式设置 `ContextBudget` 后本字段不参与计算。接本地模型时填真实窗口能让压缩阈值贴合。 |
| `SystemPrompt` | `string` | `""` | 每次模型请求前加上。`Instructions`/`Environment` 会拼在它后面。 |
| `Profile` | `string` | `""`（不应用档） | `careful` / `personal` / `local`；只填你**没显式设置**的 `Approval`、`ToolPreset`、`ProviderKind`、`MaxIterations`。未知档名报错。详见 §7。 |

`kit.AvailableModels(ctx, kind, cfg)` 可以直接列某个 provider 的模型（本地
ollama/lmstudio 有 `/v1/models`；不支持列模型的 provider 返回 `(nil, false, nil)`）。

### 2.2 循环、上下文与成本

| 字段 | 类型 | 默认值 | 说明与坑 |
| --- | --- | --- | --- |
| `MaxIterations` | `int` | `0` → **200**（`kit.DefaultSafeMaxIterations`） | 模型↔工具轮数的**成本护栏**：`0` 不是"不限"，而是 200；**显式负数 = 真的不限**（内部归一化为 0 传给 agent）；`>0` 是显式上限。超限返回包装 `rig.ErrMaxIterations` 的错误。注意 `agent` 层默认是 0（不限），两道默认值不同。 |
| `ContextBudget` | `int` | `0` → 模型窗口 × 0.6，下限 16k | 估算的 prompt token 预算。`>0` 以配置为准；**`<0`（负数）表示关闭自动压缩**（`Compactor` 不会被装配）。 |
| `Compactor` | `kit.Compactor` | `nil` → `compact.WindowCompactor{KeepLast: 8, ToolResultKeepLast: 4}` | 仅在预算 `>0` 时装配默认值。想要摘要式压缩用 `compact.LLMCompactor`（每次压缩多花一次模型调用）。压缩失败会降级继续，不中止回合。 |
| `MaxRunCostUSD` | `float64` | `0`（不限制） | 单次 Run 的花费上限（美元）。**必须同时配 `Meter`** 才会生效（护栏靠 usage 事件驱动）；超限以可读错误收尾并取消本次 Run。与 `MaxIterations` 是两种护栏：轮数管"模型抽风"，花费管账单。 |
| `Meter` | `*costmeter.Meter` | `nil` | 例如 `costmeter.New(costmeter.WithBudget(2))`；UI 读 `Meter.Session().CostUSD`。挂上 Meter 后 kit 会自动开事件桥，因此不需要宿主额外传 `Emit`。 |
| `Spill` | `*rig.SpillPolicy` | `nil`（不落盘） | 超大工具结果落盘并把模型可见内容换成摘要；`Spill.Store` 非 nil 时会注册 `spill_read`（否则"结果太大已存起来"等于永久丢失）。 |
| `CheckpointStore` | `rig.CheckpointStore` | `nil`（关闭） | 回合边界写恢复检查点；`agent.RecoverInterruptedTurn` 负责修复被中断的回合（不重放副作用）。 |

### 2.3 工具面

| 字段 | 类型 | 默认值 | 说明与坑 |
| --- | --- | --- | --- |
| `Tools` | `[]kit.Tool` | `nil` → `tools/full.NewRuntime(...)` 的默认工具集 | **`nil` 与空切片含义不同**：`nil` = 用默认工具集；非 nil 的**空切片** = 一个工具都没有。非 nil 时预设与几个注入项的行为都会变（见下方"注入即失效"）。 |
| `ToolPreset` | `string` | `""`，且 `Tools == nil` 时等价 `personal` | `minimal`(8) / `personal`(55) / `standard`(73) / `all`(83)，按名字过滤。**`Tools != nil` 时 `""` 不过滤**（那是宿主自己的选择，不该被静默裁剪）；显式设置则照常过滤。未知预设报错。 |
| `ToolDiscovery` | `*bool`（三态） | `nil` → 开（`Tools == nil` 时）；`Tools != nil` 时 `nil` → 关 | 动态工具加载：初始只把核心 13 + `tool_search` + `memory` + `skill` 发给模型（零配置 16 个 active），其余注册但不 active，模型用 `tool_search` 自助激活。`Tools != nil` 默认关的理由：你自己挑的工具应该立刻能用。想开写 `kit.Bool(true)`。 |
| `InitialActiveTools` | `[]string` | `nil` → 核心 13（再加常驻的 `tool_search`/`memory`/`skill`，零配置共 16） | 覆盖初始 active 集。`tool_search`、`memory`、`skill` 会被**强制并入**（前提是它们真的注册了）；未知名字静默忽略；非 nil 的**空切片**表示初始一个都不 active（只剩 `tool_search` 也能工作）。 |
| `PTC` | `bool` | `false` | 追加 `run_code`（模型写的 Python 3 / Node.js 在系统已装运行时里跑，内层调用仍走同一个 Gate）。`run_code` 在预设过滤**之后**追加，因此不受 `ToolPreset` 影响。 |
| `SandboxGrants` | `*bool`（三态） | `nil` → **开** | 注册 `sandbox_status`（只读诊断）与 `request_sandbox_access`（会话级可写授权申请）。两者都是 discoverable、**不进**初始 active 集，注册不增加每回合常驻成本。`Tools != nil` 时不追加。门禁层面：`sandbox_status` 在只读白名单里，默认**免问放行**；`request_sandbox_access` 不在白名单，默认门禁**拒绝**它——放宽沙箱必须宿主显式配审批。关掉用 `kit.Bool(false)`。 |
| `DisableShellAccess` | `bool` | `false` | 与 `CommandAllowlist` 配合决定策略：`true` 且没有 allowlist 时，策略不设置"任意命令可用"，因此**所有** shell 命令被策略拒绝。 |
| `CommandAllowlist` | `[]string` | `nil` → 任意命令（`sandbox.WithAnyCommandAllowed(true)`） | 非空时只允许列出的命令基名（`/usr/bin/git` 匹配 `git`）；此时 `DisableShellAccess` 不再起作用（allowlist 优先）。 |

**为什么默认"不限制命令名"**：`workspace-write` 下如果连 `go test` 都跑不了，
"帮我跑测试"整个产品就不成立。危险性由 `gate.CommandRiskGate` 的命令分级 +
人工审批判定，而不是靠一张永远不全的程序白名单。

**注入即失效（`Tools != nil` 时）**：`Searcher`、`SessionLog`、`GitUndo`/
`UndoTracker`、`Asker`（的 `ask_user` 工具）、`SandboxGrants` 都只作用于 kit
自己构建的默认工具集。kit 不会因此报错（工具缺席是宿主的显式选择），但会打一条
`slog.Warn` 把这些被忽略的依赖列出来。**唯一的硬错误**是 `Tools` 与
`SandboxRunner` 同时设置：自建工具集不会经过 OS 沙箱包装，而那会让用户以为
"还在沙箱里"，属于"以为有保护其实没有"，必须构造期失败。

**零配置体检对所有工具集都执行**（这一点容易误读）：`kit.New` 会按
`kit.Config` 上的宿主能力字段摘掉"装配缺失、调用必然失败"的工具——
`Searcher == nil` 摘 `web_search`；`SessionLog == nil` 摘 5 个 `session_*`；
`Presenter == nil` 摘 `present`；`Asker == nil` 摘 `ask_user`；
`AllowNetwork == false` 摘 `web_fetch`；非 Windows 摘 `pwsh`；没有回合快照时摘
`undo`；此外 `get_goal`/`create_goal`/`update_goal`/`plan_review`/
`exit_plan_mode` 因为 kit 没有对应注入入口而**恒被摘掉**。
这些判据读的是 **`kit.Config`**，不是 `full.AllOptions`——所以自建工具集时，如果
能力只注进了 `full.AllOptions`（例如 `Searcher`、`Asker`），而要的工具仍被摘掉，
必须在 `kit.Config` 上同样填一遍。

### 2.4 权限与沙箱

| 字段 | 类型 | 默认值 | 说明与坑 |
| --- | --- | --- | --- |
| `Mode` | `kit.Mode`（`sandbox.Mode`） | `""` → `workspace-write` | `read-only` / `workspace-write` / `danger-full-access`；未知值构造期报错。`read-only` 拒绝文件修改**与** shell 执行；`danger-full-access` 移除根围栏与命令允许列表（显式 denylist 仍生效）。 |
| `Approval` | `kit.ApprovalMode` | `""` → `ask` | `ask` / `auto` / `never`；`Gate` 非 nil 时被忽略。三档语义见 §4。未知值报错。 |
| `Gate` | `kit.Gate`（`rig.Gate`） | `nil` → 由 `Approval` 构造 | 非 nil 时**始终优先**（完全接管）。自定义时务必保证 `gate.CommandRiskGate` 在**最外层**，否则每条命令都会先弹窗（§4.4）。 |
| `Policy` | `*sandbox.Policy` | `nil` → 由 `Root`/`Mode`/`AllowNetwork`/`CommandAllowlist` 构造 | 非 nil 时那些字段被忽略（决定权归策略），但 `Root` 仍用于定位根感知工具。**保护路径（§2.4 末尾）只在 `Policy == nil` 时由 kit 追加**。 |
| `Root` | `string` | `""` → `"."`（`kit.DefaultRoot`） | 工作区根。`Policy == nil` 时 kit 会 `os.Stat` 校验它存在且是目录（否则构造期报错）。 |
| `AllowNetwork` | `bool` | `false` | 同时做两件事：给策略 `sandbox.WithNetwork`，以及决定 `web_fetch` 是否注册。即使启用，策略仍拒绝 loopback/私有/链路本地/元数据等目标。 |
| `Sandbox` | `*bool`（三态） | `nil` | 只表达**意愿**，不探测平台：`nil` 尽力而为（有 runner 就用）；`true` = "我希望隔离"，但没有后端时**不报错**，只在 `Preflight` 里如实说明落差；`false` = 明确不要（此时再注入 `SandboxRunner` 是自相矛盾的配置，构造期报错）。 |
| `SandboxRunner` | `sandbox.Runner` | `nil` | OS 级受限执行器（`sandbox/osrunner`）。非 nil 时默认工具集的 `bash`/`pwsh`、jobs、terminal 与 worktree 在 spawn 前经 `Runner.Confine` 包装，**失败即拒绝执行**。**仅在 `Tools == nil` 时生效**；与 `Tools != nil` 同时出现是构造期错误。 |
| `SandboxUnavailableReason` | `string` | `""` | "为什么没有系统沙箱"的一句话（通常取 `osrunner.New` 的错误）。kit **不探测平台**（保持纯装配层），所以这句话只能由宿主填；`Preflight`/UI 用它给出完整答案。 |
| `AutoAllowSandboxedCommands` | `*bool`（三态） | `nil` → **开** | 已被 OS 沙箱包住的**非危险** shell 命令免审批；免问仍写一条 `approval.response`（decision=allow、reason=`sandboxed`）。`kit.Bool(false)` 回到"每条可变命令都问人"。判据与边界见 §4.5。 |
| `SandboxDenialSink` | `func(sandbox.Denial)` | `nil` | 接收每一次被识别的沙箱拒绝；挂在策略上，一次配置覆盖所有执行工具。回调必须并发安全。 |
| `DisableProtection` | `bool` | `false`（保护开） | **关闭默认保护路径的唯一入口**。`true` 时一个位置都不追加（`.git`、`~/.ssh`…）。 |
| `ProtectPaths` | `bool` | `false`，但**不改变任何行为** | 只是把"保护默认开"的意图写进配置里（给未来默认值变化留锚点）。设 `true` 与不设等价；它**无法**表达"关"，因为 Go 布尔零值是 false。 |
| `ProtectWorkspaceMetadata` | `bool` | `false`，**已废弃/无效** | 保留仅为源码兼容。设 `true` 没有额外效果（本来就开），设 `false` **不会**关闭保护。要关闭用 `DisableProtection: true`。 |

**默认保护路径**（`Policy == nil` 且 `DisableProtection == false`）：

- 每个策略根下的工作区元数据：`.git`（含 worktree 的 gitdir 指针，按"数据面
  可写、控制面只读"模型保护）、`.aws`、`.codex`——存在才登记；
- 用户主目录下**存在**的凭据位置：`~/.ssh`、`~/.aws`、`~/.config/gh`、`~/.netrc`
  （`kit.HomeSecretPaths()` 也导出给宿主拼自己的策略）。

这是**防误伤**，不是沙箱：它只约束文件工具的落点与（挂接 runner 时的）OS
profile，不改变进程权限。宿主显式提供 `Policy` 时保护策略归宿主所有，kit 不追加。

### 2.5 宿主能力注入

| 字段 | 类型 | 默认值 | 说明与坑 |
| --- | --- | --- | --- |
| `Asker` | `rig.Asker` | `nil` | `approval=ask` 下真正去问人的通道（`Ask(ctx, question, options)`）。`nil` 时 kit 用 `gate.PersonalAskWithoutAsker()`：只读放行、危险拒绝、其余拒绝并**明确说明"没有可用的审批通道"**。接上它同时会注册 `ask_user` 工具。kit 给它的单次询问默认 10 分钟超时（`gate.WithAskTimeout`）。 |
| `Presenter` | `rig.FilePresenter` | `nil` | 让 `present` 能把交付文件交给宿主 UI。它同时是 `present` 的**门控**：`nil` 时 `present` 被摘掉；非 nil 时保留**并立刻进初始 active 集**（宿主已经付了配置成本，不该再让模型 `tool_search` 一次）。注意类型是 `rig.FilePresenter`（`PresentFiles`），不是已废弃的 `rig.Presenter`。 |
| `Searcher` | `web.Searcher` | `nil` | 提供 `web_search`；`nil` 时该工具被摘掉（而不是留一个永远报错的占位）。 |
| `SessionLog` | `rig.SessionLog` | `nil` | 提供 `session_search`/`session_trace`/`session_event_*`；`nil` 时被摘掉。 |
| `Memory` | `memory.Wiki` | `nil` → 自动建文件 wiki | 记忆工具的存储。`nil` 且 `Root` 非空时 kit 用 `<状态目录>/memory/<workspaceID>` 建按工作区分区的文件 wiki（目录 0700、页面 0600），**不写进仓库**。装配失败**不阻断** `kit.New`：回落到进程内实现，原因由 `Agent.MemoryWikiError()` 报出。想彻底关掉就是让工具集里没有 `memory`。 |

### 2.6 持久化、事件与收尾

| 字段 | 类型 | 默认值 | 说明与坑 |
| --- | --- | --- | --- |
| `History` | `kit.History` | `nil` → `session.NewMemory()` | 会话历史。与 `SessionStoreDir`/`SessionStore` **互斥**（同时设置构造期报错）。 |
| `SessionStoreDir` | `string` | `""` | 启用内置持久化：kit 打开一个 `session/store` 目录并挂成默认会话（一会话一 NDJSON，崩溃自愈）。要求 `History == nil` 与 `SessionStore == nil`。**同一目录只能由一个 store 实例写**（不做跨进程加锁）。 |
| `SessionStore` | `*store.Dir` | `nil` | 宿主自己持有的 store 实例（"最近会话"侧栏需要**同一个实例**做 `List`/`Load`）。与 `SessionStoreDir`/`History` 互斥。宿主传入的实例**不在** `Shutdown` 自动关闭范围内（要关就在 `Storage` 里登记一次）。 |
| `Storage` | `kit.Storage` | `nil` | `Shutdown` **最后**收尾的存储（只需要 `Close`；`*session/sqlite.Store` 直接满足）。 |
| `BackgroundJobs` | `[]kit.Background` | `nil` | `Shutdown` 要一并关掉的后台句柄（`jobs.Manager`/`terminal.Manager` 都满足）。只在宿主自建 `Tools` 时需要：默认工具集由 kit 经 `full.NewRuntime` 构造，两个管理器已被自动登记。 |
| `EventWriter` | `io.Writer` | `nil`（无事件流） | 把 agent 事件桥接成 `events.Envelope`（NDJSON）；一个 Run 一条流。写失败不影响 Run（错误被记成粘性状态，用 `Agent.EventError()` 读）。 |
| `Redactor` | `*secrets.Redactor` | `nil`（不脱敏） | **仅日志/导出用**：默认工具结果与模型文本明文直通（本产品是单用户个人客户端）。显式设置后用于事件流与 agent 入参脱敏，**不会**装饰工具返回内容。 |
| `GitUndo` | `bool` | `false` | 每个 Run 开始前用 git 打一份隐藏快照（`git stash create` 语义，不动 HEAD/分支/暂存区），并让 `undo` 工具出现。工作区不是 git 仓库时该开关被静默忽略（`undo` 不出现）。UI 可经 `Agent.UndoTracker()` 直接做"撤销按钮"。 |
| `UndoTracker` | `*undo.Tracker` | `nil` | 已废弃：优先用 `GitUndo`。保留是为了兼容既有宿主；与 `GitUndo` 同时设置时 **`UndoTracker` 优先**。 |
| `Instructions` | `*instructions.Set` | `nil` | AGENTS.md 层级文档进系统提示词（**只应 `Load` 一次**：system prompt 是请求前缀，任何字节变化都会打掉 provider 的 prompt 缓存）。 |
| `Environment` | `*instructions.Environment` | `nil` | `<environment>` 块（cwd/平台/日期/git）进系统提示词，拼在文档之前。 |

`Agent.Shutdown(ctx)` 的顺序是固定的，且每一步都不能换位置：**① 取消在飞 Run →
② 等它们退出（并 flush 事件）→ ③ 关后台任务（`BackgroundJobs`）→ ④ 关存储
（`Storage`）**。若第 ② 步等不到（ctx 超时），它**不关存储**（那正是 `ErrClosed`
的来源），但仍会关后台任务并返回错误，宿主可以再调一次。收尾是幂等的；`Shutdown`
之后的新 Run 直接返回 `ErrShuttingDown`。关停造成的取消**不算失败**（不会写进
`run_end.err`）。

## 3. 优先级：RunRequest → AgentSpec → Config → 默认

`kit.Agent.Run` 是薄封装：它给请求加撤销快照、事件、成本护栏，然后原样转给底层
`agent.Agent`。所以"谁覆盖谁"有两层含义——kit 字段决定 `AgentSpec`，`RunRequest`
在每次运行上再覆盖：

| 设置 | 优先级（从高到低） |
| --- | --- |
| 模型 | `rig.RunRequest.Model` → `rig.AgentSpec.Model`（kit 写入 `Config.Model`） → provider 的 `DefaultModel`（只在直接调 `Provider.Stream` 且 `Request.Model` 为空时用） |
| 轮数上限 | `rig.RunRequest.MaxIterations`（`>0` 生效，`<=0` 表示"用 agent 默认"） → `rig.AgentSpec.MaxIterations`（kit 写入：`0`→200、负数→不限、`>0` 显式） |
| 审批门禁 | `rig.RunRequest.Gate`（非 nil） → `rig.AgentSpec.Gate`（kit 写入 `Config.Gate`，或由 `Config.Approval` 构造） → `nil`（裸 `agent.New` 时等于全放行） |
| 会话/历史 | `rig.RunRequest.SessionID` 选择会话 → `Config.History` / `SessionStoreDir` / `SessionStore` 决定后端 → 默认 `session.NewMemory()` |
| 上下文预算与压缩 | 只有 `Config.ContextBudget` / `Config.Compactor`（没有 per-run 字段） |
| 工具面 | `Config.Tools` → 默认 `full.NewRuntime` → `Config.ToolPreset` 过滤 → 零配置体检裁剪 → `PTC` 追加 → 动态加载的 active 集 |
| 沙箱策略 | `Config.Policy` →（`nil` 时）由 `Root`/`Mode`/`AllowNetwork`/`CommandAllowlist` 构造 → 追加默认保护路径 |
| provider | `Config.Provider` →（`nil` 时）按 `Config.ProviderKind`（默认 `openai`）+ `BaseURL`/`APIKey`/`APIKeyEnv` 构造 |

同名旋钮在各层含义一致；**更具体的一层赢**。`kit.RunRequest`/`RunResult`/
`kit.Event` 都是 `rig` 同名类型的别名，所以先 `kit.New`、日后下沉到
`agent.New` 不需要重写工具接线。

## 4. 权限模式 × 审批策略

两个正交的旋钮：

- **`Mode`（权限模式）**：工具实现层的进程内围栏——文件工具能写哪、shell 能不能跑。
- **`Approval`（审批策略）**：一次工具调用在**执行之前**该由谁点头。

### 4.1 模式

| `Mode` | 文件修改 | shell 命令 |
| --- | --- | --- |
| `sandbox.ModeReadOnly` | 拒绝 | 拒绝 |
| `sandbox.ModeWorkspaceWrite`（默认） | 仅在策略根内（∪ 追加可写根 ∪ 平台临时目录 − 保护路径） | 受命令允许列表/`WithAnyCommandAllowed` 约束 |
| `sandbox.ModeDangerFullAccess` | 无根围栏（显式 denylist 与保护路径仍生效） | 无允许列表围栏（显式 denylist 仍生效） |

两者都是**工具实现层的围栏，不是内核边界**：它们约束"配合的模型"，挡不住真的
去碰。要真正的文件效果边界得挂 `sandbox/osrunner`（见
[SANDBOX-OS.md](SANDBOX-OS.md)），要进程级隔离用容器/VM。

### 4.2 审批三档

| `Approval` | 实际门禁 | 语义 |
| --- | --- | --- |
| `ask`（默认） | 有 `Asker` → `gate.PersonalAsk(gate.AllowSandboxedCommands(…, gate.Ask(Asker)))`；无 `Asker` → `gate.PersonalAsk(DenyAllWithReason(DenyAllNoChannelReason))` | 三层：只读免问 → 危险命令直接拒绝 → 其余问人 / 拒绝并说明没有通道 |
| `auto` | `gate.PersonalAuto()` = `gate.CommandRiskGate(gate.AllowAll())` | **不问人，但不等于 `AllowAll`**：`rm -rf /`、`mkfs`、`curl … \| sh` 这类危险命令仍被直接拒绝 |
| `never` | `gate.PersonalNever()` = `gate.DenyAll()` | 拒绝一切调用，理由说明 `approval=never` |

矩阵（X 轴 `Mode`，Y 轴审批；"策略拒"表示即使门禁放行，工具实现层仍会拒绝）：

| | `ask` + 有 Asker | `ask` + 无 Asker | `auto` | `never` |
| --- | --- | --- | --- | --- |
| `read-only` | 只读免问；写/执行**问人**（且策略随后拒） | 只读放行；其余拒绝（"没有可用的审批通道"） | 只读直接跑；写/执行门禁放行但**策略拒** | 全拒 |
| `workspace-write` | 只读免问；危险拒；其余问人；已沙箱化命令免问 | 只读放行；危险拒；其余拒绝并说明无通道 | 不问人；危险仍拒；其余放行 | 全拒 |
| `danger-full-access` | 只读免问；危险拒；其余问人（**没有沙箱，因此没有"已沙箱化免问"**） | 同左但需要审批的调用被拒（无通道） | 不问人；危险仍拒；其余放行——**只应在容器/VM 里这么配** | 全拒 |

### 4.3 三层 `ask` 的判据

1. **只读免问**：`gate.SandboxReadOnlyTools` 白名单（`read_file`、`list_dir`、
   `glob`、`grep`、`read_image`、`search`、`inspect`、`sandbox_status`、
   `working_directory`、`tool_search`、`skill`、`memory`、`todo_write`、5 个
   `session_*` 等）与**只读命令**（`ls`、`git status`、`grep` …）直接放行。
   白名单的判据是"是否触及用户资源（文件/进程/网络）"，不是"是否只读"。
2. **危险命令直接拒绝**：`bash`/`pwsh` 的 `command` 经
   `sandbox.ClassifyShellCommandOnPlatform` 分级，`dangerous` 一律 `DecisionDeny`，
   命中规则 id 与中文理由通过可选接口 `DenialExplainer` 拼进工具错误回给模型
   （否则模型看不到原因会原样重试）。
3. **其余问人**：写文件、`apply_patch`、可变命令等交给 `Config.Asker`
   （`gate.Ask`，kit 给的单次等待默认 10 分钟；可选 `allow`/`allow-always`/`deny`）。

无法判定的命令（参数不是合法 JSON、缺 `command`、命令为空）**一律委托内层门禁**，
不猜。拒绝是工具错误（`DecisionDeny`）而不是 Go error，所以 run 继续、模型有下一轮。

### 4.4 自定义 Gate 与顺序

```go
import (
    "time"

    "github.com/zeokeel/zeokeel-rig/gate"
    "github.com/zeokeel/zeokeel-rig/kit"
)

g := gate.CommandRiskGate( // ① 必须在最外层
    gate.AllowThenAsk( // ② 只读工具免问
        gate.SandboxReadOnlyTools,
        gate.Ask(host.Asker, gate.WithAskTimeout(10*time.Minute)), // ③ 其余问人
    ),
)
a, err := kit.New(kit.Config{Model: "gpt-4o-mini", Root: root, Gate: g})
```

- **`CommandRiskGate` 必须在 `AllowThenAsk` 之外**：`AllowThenAsk` 只看工具名，
  一旦放行 `bash`，里面的 `ls` 与 `rm -rf /` 就无差别了；反过来把风险门禁放内层，
  则每条 bash 命令都会先弹一次窗才轮到风险判定。
- 用 `gate.PersonalAsk*` 系列时顺序已经对；自己拼装才会踩这个坑。
- **不要**写 `gate.PersonalAsk(nil)` 或 `gate.PersonalAsk(gate.DenyAll())`：前者让
  每个需要审批的调用都被拒（配置疏漏），后者的文案会说 `approval=never`，与真实
  状态不符。没有 `Asker` 时的正确兜底是 `gate.PersonalAskWithoutAsker()`。
- `gate.Review(reviewer, gate.WithReviewFallback(gate.Ask(...)))` 可以先让
  reviewer 判、超时/出错再回落到人。

### 4.5 已沙箱化的命令免审批

判据（**全部满足**才免问，实现见 `gate.AllowSandboxedCommands`）：

1. 工具是 shell 类（`bash`/`pwsh`）；
2. `SandboxRunner != nil` 且自报能强制（`sandbox.RunnerStatus` 的 Enforcement
   不是 `none`）；
3. 策略非 nil 且模式不是 `danger-full-access`；
4. 命令**不是** dangerous（纵深防御，最外层早就拒了）；
5. 命令能解析出来。

免问仍写一条 `approval.response`（decision=allow、reason=`sandboxed`），UI 可以
显示"这条没问是因为已隔离"。文件工具（`write_file`/`edit_file`/`apply_patch`）
不经 OS runner，**照旧问人**。危险命令与本开关无关，永远直接拒绝。

它在组合里的位置是**固定**的：`CommandRiskGate`（最外层）→ `AllowThenAsk`（只读
免问）→ `AllowSandboxedCommands` → `Ask`（问人）。这样危险命令仍在外层被拒、只读
命令仍在 `AllowThenAsk` 放行，只有"可变但不危险、且会被 runner 包住"的那一类才走
免问路径。用 `Config.Approval` 时 kit 已经拼好；自己拼装时不要把这个顺序搞反。

这条默认值（`nil` = 开）的理由：沙箱与审批是两种独立的成本；用户会为了少点几十次
弹窗而把审批整体关掉，那时连危险命令的拦截也一起丢了。**headless 宿主受益最大**：
没有 Asker 时，已沙箱化的命令现在能跑（此前一律"没有可用的审批通道"）。

## 5. 逐命令提权（`sandbox_permissions`）

### 5.1 它是什么

OS 沙箱只能挡"路径与系统调用"，而真实软件还会写**非路径资源**（Windows 的 HKCU
注册表、macOS keychain、某些 CLI 的系统级 socket）——这些没有任何"路径授权"能
救。于是 `bash`/`pwsh` 接受两个参数：

| 参数 | 取值 | 说明 |
| --- | --- | --- |
| `sandbox_permissions` | `"require_escalated"`（**唯一**支持的值） | 请求"这一条命令在 OS 沙箱外跑" |
| `justification` | 字符串，**≥ 10 个字符**（字符数不是字节） | 为什么这条命令不能在沙箱内跑；会原样展示给审批人 |

### 5.2 边界（提权不放宽任何策略）

- **只在人工批准后**才跳过 OS runner 包装：`Escalator` 必须阻塞等人给结论；审批
  基础设施故障 → 拒绝执行，绝不默认放行。
- 策略可写面、保护路径、命令允许/禁止名单：**不变**。
- 危险命令判定：**不变**——提权请求撞到 dangerous 命令直接拒绝。
- `workdir` 校验、超时、输出上限：**不变**。
- `approval=auto`/`never` 档**一律拒绝**（auto 语义是"不问人"，而提权本质是一次
  人工判断）；没有 `Asker` 时同样拒绝，文案让用户去开审批或自己执行。
- 没有挂 OS runner 时也拒绝（命令本来就跑在沙箱外，提权没有意义，更不该变成
  绕过审批的口子）。
- 允许与拒绝都留痕：事件 `sandbox.escalation` + logger（唯一能让进程跑出沙箱的
  入口，必须可审计）。

### 5.3 宿主接入

`kit` 已经替你接好——传 `Asker` + `SandboxRunner` 即可：

```go
a, err := kit.New(kit.Config{
    Model:         "gpt-4o-mini",
    Root:          root,
    Asker:         host.Asker,          // 提权必须能问人
    SandboxRunner: runner,              // 没有 runner 就没有"跳出沙箱"这回事
    // Approval 保持默认（ask）；auto/never 下提权一律拒绝
})
```

手动装配时用 `full.AllOptions.Escalator`，或直接给 shell 工具挂：

```go
shell := builtin.NewWithPolicyOptions(policy,
    builtin.WithRunner(runner),
    builtin.WithEscalator(myEscalator), // 实现 builtin.Escalator
)
```

`builtin.EscalationRequest` 是审批 UI 的输入（也是审计记录）：`Tool`、`Command`、
`Argv`（含 shell 包装的完整 argv）、`Workdir`、`Justification`、`Mode`、
`Program`、`Risk`、`RuleID`。

### 5.4 什么时候模型会看到这两个参数

只有当组合里提权真的能走通时，`bash`/`pwsh` 的描述末尾才会追加提权提示：需要
`Escalator` 非 nil、`Runner` 非 nil、策略不是 `danger-full-access`、且通道自报
可用（kit 的 approver 在"没有 Asker"或 `approval=auto`/`never` 时自报不可用）。
不可用的组合里描述不会指向一个必然失败的参数，schema 也会如实说"本组合不支持"。

### 5.5 与"已删除的提权包"无关

已删除的 `tools/escalate` / `sandbox/escalate` 是**另一次尝试**（提权协议本体 +
宿主 broker + `os/exec` 面），整包移除且不得回归。本节讲的逐命令提权活在
`tools/builtin` 里：工具只声明"我要人批准这一条命令"，"问谁、能不能问、怎么记账"
由宿主（`kit` 的 `escalationApprover`）决定。**不要把两者混为一谈。**

沙箱可写面还有另一条**正交**的通道：`request_sandbox_access`
（`Config.SandboxGrants`，默认开）——它申请的是"某个目录变成可写根"，由宿主 Gate
审批后 `Policy.AddWritableRoot` 生效；工具自身不审批。可写面公式：

```text
Roots() ∪ AdditionalWritableRoots() ∪ 平台临时目录 − ProtectedPaths()
```

## 6. Preflight：现在到底安不安全

`Agent.Preflight()` 返回 `sandbox.PreflightReport`（一屏人话，UI 可直接展示）：

| 字段 | 含义 |
| --- | --- |
| `Mode` | 实际生效的权限模式 |
| `Enforced` | 有没有**真的** OS 围栏（`false` = 只有进程内校验） |
| `Backend` | 人类可读的后端名（`bubblewrap` / `Landlock` / `Seatbelt` / `Windows ACL`），没有则为空 |
| `Enforcement` | 后端对"文件效果"承诺的实际覆盖程度：`full` / `partial` / `none` |
| `Network` | `off` / `loopback` / `full` / `not-governed` |
| `Summaries` | 按重要性排序的一两句结论（"已隔离 / 部分隔离 / 未隔离"） |
| `Warnings` | "你需要知道但不会挡住你"的事实（例如读不受限） |
| `NextSteps` | 被挡住时该点哪里 |

`PreflightReport.Text()` 把它们拼成多行文本（含"需要知道"/"下一步"小标题），
命令行与 UI 共用同一份措辞——与工具里"被拒绝"的建议同源，因此 UI 与模型不会各说
一套。

`SandboxUnavailableReason` 的用法：kit **不探测平台**（保持纯装配层）。宿主在
`osrunner.New` 失败时把错误填进这个字段，`Preflight` 就会在"这台机器上没有可用的
系统沙箱"后面补一句"原因：…"，而不是只有半句结论。若实际有 runner，这句话会被
忽略（`Agent.Preflight` 里清空），避免出现"有沙箱却报告没有"的矛盾。

`Sandbox: kit.Bool(true)` 但组合里没有后端时，`Preflight` 会在最前面追加一句落差
说明（"你要求了系统沙箱，但当前组合里没有可用的后端；应用照常运行，只是没有系统
级围栏"）——**不会**让 `kit.New` 失败。装不上沙箱不该等于应用打不开。

## 7. Profile：一行拿到一组合理默认

`Profile` 只填你**没显式设置**的四个字段（`Approval`、`ToolPreset`、
`ProviderKind`、`MaxIterations`），其余一律不动：

| 字段 | `careful` | `personal`（推荐） | `local` |
| --- | --- | --- | --- |
| `Approval` | `ask` | `ask` | `auto` |
| `ToolPreset` | `personal` | `personal` | `personal` |
| `ProviderKind` | `openai` | `openai` | `ollama` |
| `MaxIterations` | `200` | `200` | `200` |
| 定位（`Profile.Note`） | "每一步都问我：只读操作免问，其余写/执行都要我确认" | "推荐：只读免问、危险命令直接拒绝、其余问我" | "本地模型：不问人，但危险命令仍被拒绝；默认不联网" |

注意两点：

- `careful` 与 `personal` 在**取值上完全相同**——差别只在说明文案（`careful` 存在
  是给设置界面一个"最保守"的选项名）。想更严格请显式写配置：`Mode: read-only`
  或 `gate.PersonalNever()`。
- `local` 把 `Approval` 设为 `auto`（不问人）并把 provider 换成 ollama；**危险命令
  仍被拒**。它不涉及联网开关——`AllowNetwork` 默认就是关，保持默认即可。

`kit.Profiles()` 返回整张表，`kit.LookupProfile(name)` 按名取（空名返回
`(Profile{}, false)`）。档名不合法时 `kit.New` 直接报错并列出三个合法值。

## 8. 环境变量

Rig 自己读取的环境变量只有下面这些（逐个 grep 核对过，没有"约定俗成"的
`RIG_*` 配置变量）：

| 变量 | 谁读 | 作用 |
| --- | --- | --- |
| `OPENAI_API_KEY` | `provider/openai`（`DefaultAPIKeyEnv`） | `APIKey` 为空且 `APIKeyEnv` 未指定时的 API key |
| `APIKeyEnv` 指定的名字 | `provider/openai`、`provider/local` | 自定义凭据变量名（例如 `DEEPSEEK_API_KEY`） |
| `LOCAL_API_KEY` | `provider/local`（`DefaultAPIKeyEnv`） | 自建 OpenAI 兼容端点的 key；刻意**不**复用 `OPENAI_API_KEY`（把托管 key 发给 `127.0.0.1` 是凭据泄漏） |
| `ZEOKEEL_SKILLS` | `tools/skill`（`DefaultDirs`） | 冒号（`os.PathListSeparator`）分隔的额外技能目录；另外两个默认目录是 `<root>/.zeokeel/skills` 与 `~/.zeokeel/skills` |
| `XDG_STATE_HOME` | `internal/memwiki.DefaultStateDir` | 记忆 wiki 的状态目录基址（实际落在 `<该值>/rig/memory/<workspaceID>`） |
| `LOCALAPPDATA` | `internal/memwiki`、`sandbox/osrunner` | Windows 上的状态目录基址（`%LOCALAPPDATA%\rig`）与工具链缓存根 |
| `APPDATA` | `sandbox/osrunner`（Windows） | 工具链缓存根（例如 `%APPDATA%` 下的包管理器缓存）；不含凭据文件 |
| `HOME` / `USERPROFILE` | `kit.HomeSecretPaths`、`sandbox/osrunner` | 主目录：保护路径（`~/.ssh`…）与工具链缓存根的推导；取不到就不猜 |
| `TMPDIR` / `TEMP` / `TMP` | `sandbox`（策略的可写集）、`sandbox/osrunner` | 平台临时目录：`$TMPDIR`（`os.TempDir()`）是否进入可写集受 `sandbox.WithTmpdirEnv` 控制；Windows 上还是私有临时目录的候选基址 |
| `RIG_SANDBOX_TEMP` | `sandbox/osrunner`（Windows） | 私有临时目录基址的**显式覆盖**（受限环境下指定可写目录）；优先级高于系统 temp 机制（`os.TempDir()` 与 `GetTempPath` 的回退链） |
| `SystemRoot` / `WINDIR` | `sandbox/osrunner`（Windows）、`sandbox` | 系统目录：落在其下的临时基址会被剔除（普通用户不可写） |
| `SystemDrive` | `sandbox/grant.go` | Windows 上校验可写授权路径的盘符 |
| `COMSPEC` | `sandbox/osrunner`（Windows） | 功能探测用的 shell（缺省 `cmd.exe`） |
| `RIGDOCTOR_ASKER` | `cmd/rigdoctor` | 诊断 CLI 的"有没有审批通道"假设（`yes`/`no`）；库本身不读 |
| `GOOS` | `cmd/rigdoctor` | 仅用于报告里的 `platform` 字段（注意它读的是**环境变量** `GOOS` 而不是 `runtime.GOOS`：没设置时报告里显示 `host`；纯诊断字段，不影响任何行为） |

另外两个与"读取"无关但常被问到的事实：

- **子进程继承的环境**由策略的 `defaultEnvAllowlist` 决定：默认
  `PATH`、`HOME`、`LANG`，以及 Windows 的 `TEMP`/`TMP`/`TMPDIR`/`LOCALAPPDATA`/
  `APPDATA`/`USERPROFILE`/`SystemRoot`/`SystemDrive`/`WINDIR`/`COMSPEC`/
  `PATHEXT`/`USERNAME`/`USERDOMAIN`/`OS`。API key、token 等其余变量在命令启动前被
  丢弃（`sandbox.WithEnvAllowlist` 可改）。
- `osrunner` 用 `exec.LookPath` 找 `bwrap`（Linux）与 `sandbox-exec`（macOS 的
  回退路径），所以 `PATH` 间接影响**沙箱本体能否被找到**；macOS 优先固定
  `/usr/bin/sandbox-exec`，并拒绝 group/world 可写的候选。

## 9. 常见配置配方

下面每段都是最小可用配置（把 `host` 换成宿主自己的对象；真实代码里 `a`/`err` 当然
要用起来，示例为省行数没有逐个处理）。

### 9.1 只读客户端（问答、代码解释、审计）

```go
a, err := kit.New(kit.Config{
    Model: "gpt-4o-mini",
    Root:  root,
    Mode:  kit.ModeReadOnly, // 写文件与 shell 全部拒绝
})
```

零配置本身也是这个姿态（`ask` + 无 `Asker` → 只读放行、其余拒绝）。

### 9.2 有人审批的编码 agent（推荐的产品默认）

```go
a, err := kit.New(kit.Config{
    Model:    "deepseek-chat",
    Root:     root,
    Asker:    host.Asker,     // 弹窗审批：allow / allow-always / deny
    Approval: kit.ApprovalAsk, // 默认值，写出来更清楚
    // 可选：挂上 OS 沙箱后，非危险的 shell 命令不再逐条弹窗
    // SandboxRunner: runner,
})
```

### 9.3 无人值守（CI、批处理）

```go
a, err := kit.New(kit.Config{
    Model:    "gpt-4o-mini",
    Root:     "/work/repo",
    Approval: kit.ApprovalAuto, // 不问人；危险命令仍被拒
    // 只有容器/VM 里才值得放开围栏：
    // Mode: kit.ModeDangerFullAccess,
})
```

`auto` 不是 `AllowAll`：`rm -rf /`、`mkfs`、`curl … | sh` 仍被直接拒绝，理由会回到
模型上下文，让它换做法。要真的什么都不拦，得自己写 `Gate: gate.AllowAll()` ——
那意味着放弃"危险命令"这一层。

### 9.4 本地模型（ollama / LM Studio）

```go
a, err := kit.New(kit.Config{
    Model:        "qwen2.5-coder:7b",
    Root:         root,
    ProviderKind: kit.ProviderOllama, // 默认 http://127.0.0.1:11434
    Profile:      "local",            // = 上面 + Approval:auto
})

// LM Studio 同理：ProviderKind: kit.ProviderLMStudio（默认 http://127.0.0.1:1234）
// 先看看本机有什么模型：
// models, listed, err := kit.AvailableModels(ctx, kit.ProviderOllama, kit.Config{})
```

本地服务默认无鉴权，因此不需要 key；接反代要鉴权时填 `APIKey`。

### 9.5 持久化会话

```go
a, err := kit.New(kit.Config{
    Model:           "gpt-4o-mini",
    Root:            root,
    SessionStoreDir: filepath.Join(stateDir, "sessions"),
})
// 侧栏用 kit 建的那个实例（同一目录单实例）：
store := a.SessionStore()
```

`History`、`SessionStoreDir`、`SessionStore` **三者互斥**，同时设置会构造期报错。
同一目录不要开两个 store 实例（不做跨进程/跨实例加锁，两边会互相覆盖索引）。

### 9.6 回合撤销（GitUndo）

```go
a, err := kit.New(kit.Config{
    Model:   "gpt-4o-mini",
    Root:    root,
    GitUndo: true, // 工作区不是 git 仓库时被静默忽略，undo 工具也不出现
})
preview, err := a.UndoTracker().Changes(0, "") // 预览
applied, err := a.UndoTracker().Revert(0)     // 撤销最近一回合
```

`undo` 是**面向用户的按钮**：模型只在用户明确要求撤销时才应调用它。

### 9.7 成本上限

```go
meter := costmeter.New(costmeter.WithBudget(2.0)) // $2 预算（告警线默认在 80%）
a, err := kit.New(kit.Config{
    Model:         "gpt-4o-mini",
    Root:          root,
    Meter:         meter,
    MaxRunCostUSD: 2.0, // 必须同时配 Meter，否则护栏不生效
})
```

超限时本次 Run 被取消并返回可读错误（"本次会话已花费 $X，超过上限 $Y"），已经产生
的部分结果照常返回。轮数护栏（`MaxIterations`）与花费护栏是两件事，建议都配。

### 9.8 关闭 shell

```go
a, err := kit.New(kit.Config{
    Model:              "gpt-4o-mini",
    Root:               root,
    DisableShellAccess: true, // 没有 allowlist 时所有命令被策略拒绝
})
```

只想放开几个命令时反过来用白名单（此时 `DisableShellAccess` 不再起作用）：

```go
a, err := kit.New(kit.Config{
    Model:            "gpt-4o-mini",
    Root:             root,
    CommandAllowlist: []string{"git", "go", "ls", "cat"},
})
```

## 10. 相关文档

- [TOOLS.md](TOOLS.md) —— 工具目录、口径（零配置 / `full.All` / 账本 / 预设）、
  每个工具的权限与平台影响
- [SANDBOX-OS.md](SANDBOX-OS.md) —— opt-in 的 OS 沙箱 runner、fail-closed 语义、
  `rigdoctor` 诊断
- [ARCHITECTURE.md](ARCHITECTURE.md) —— 分层、请求流、契约与扩展点
- [VERSIONING.md](VERSIONING.md) —— 配置结构体与选项的兼容性承诺
- [AI-SDK.md](AI-SDK.md) —— 给 AI 的 SDK 组装指南
- [GETTING-STARTED.md](GETTING-STARTED.md) —— 安装、三种组装方式、排障
- [README.md](../README.md) —— 项目门面与最小示例