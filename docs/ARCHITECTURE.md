# 架构：Rig 是怎么组织的

本文面向**要改 Rig 或要判断"这个能力该不该放进 SDK"的人**。它回答四个问题：Rig 在
宿主应用里站在哪一层、一次 `Run` 到底发生了什么、哪些东西是稳定契约、以及哪些事
Rig 刻意不做。想先跑起来请看 [GETTING-STARTED.md](GETTING-STARTED.md)；工具清单与
计数口径见 [TOOLS.md](TOOLS.md)；`kit.Config` 的逐字段默认值见 [CONFIG.md](CONFIG.md)。

当前 release `v0.24.0`；模块 69 个 Go 包（含 3 个 example、2 个 `cmd`、`internal/*`），
唯一外部依赖 `mvdan.cc/sh/v3`（POSIX shell 解析器，只被 `sandbox` 策略层使用），
构建与测试一律 `CGO_ENABLED=0`。

## 1. 定位与分层

Rig 是**个人 agent 客户端 SDK（单用户）**：它提供 agent 运行时原语与工具面，宿主应用
提供外壳。定位写成一句话是——**Rig 拥有"模型 ↔ 工具"这条循环及其所有零件，宿主拥有
产品形态**。

```
┌─────────────────────────────────────────────────────────────────────┐
│ 宿主应用（UI · 进程宿主 · 账号鉴权 · 模块装配 · 云端同步 · 密钥托管） │
│   只依赖 rig 的契约类型，以及 kit / agent / tools 的装配入口          │
└───────────────────────────────┬─────────────────────────────────────┘
                                │ import
┌───────────────────────────────▼─────────────────────────────────────┐
│ layer 1  装配与循环                                                  │
│   kit（唯一聚合层：provider + 工具集 + 门禁 + 历史 + agent）           │
│   agent（流式 ReAct 循环） · tools（注册表） · tools/full（装配）      │
├─────────────────────────────────────────────────────────────────────┤
│ layer 2  能力包（可单独取用）                                        │
│   sandbox · gate · provider/{openai,local,ollama,lmstudio} ·          │
│   tools/* · session · session/store · session/compact · events ·      │
│   costmeter · modelprice · instructions · memory · storage            │
├─────────────────────────────────────────────────────────────────────┤
│ layer 3  实现细节                                                    │
│   sandbox/osrunner · session/sqlite（独立子模块） · internal/*         │
├─────────────────────────────────────────────────────────────────────┤
│ layer 0  契约层                                                      │
│   rig（模块根，只依赖标准库，导入图的叶子；所有层都可安全导入）        │
└─────────────────────────────────────────────────────────────────────┘
```

依赖只朝**更抽象**的方向走：装配层依赖能力层，能力层依赖契约；具体实现（layer 3）
依赖它要实现的契约（layer 2），而契约侧**不得**反向依赖实现。这不是审美问题：

- 契约层 stdlib-only、且不 import 本仓任何子包（CI 有测试守着），所以任何宿主、任何
  第三方 provider/tool/store 都能只依赖 `rig` 而不会被拖进整棵树。
- 能力包（layer 2）不依赖聚合层（layer 1），所以"我只想要 `tools/search`"是可行的，
  不需要先接受 `kit` 的默认值。
- 平台代码（`sandbox/osrunner`）在 layer 3，`sandbox` 策略层**不得**反向 import 它——
  默认构建完全不含任何平台特定执行逻辑；`storage` 契约也不得 import `session/store`
  （适配器放在 `session/store` 一侧）。

`cmd/rigdoc`、`cmd/rigdoctor`、`examples/*` 是入口，不属于库分层：它们可以依赖任意层，
也没有人依赖它们。

### Rig 拥有什么

- **循环**：流式 ReAct 循环、工具调用装配与并行执行、审批、取消、事件。
- **契约**：消息、流式分片、provider、tool、gate、history，以及全部宿主/存储接口。
- **工具面**：`tools/*` 下的具体工具与 `tools/full` 的装配、`tools/catalog` 账本、
  `tools/presets` 过滤。
- **策略与执行**：`sandbox` 进程内围栏、`gate` 审批组合、`sandbox/osrunner` 的
  opt-in OS 执行器。
- **数据面**：会话历史、上下文压缩、检查点、溢出结果、记忆/上下文数据库、事件流、计量。
- **provider 客户端**：OpenAI 兼容流式（含本地服务的 quirk 差异）。

### Rig 刻意不拥有什么

| 不做的事 | 为什么 | 谁负责 |
| --- | --- | --- |
| UI / 交互组件 | 每个产品形态不同；SDK 只给事件与审批回调 | 宿主 |
| 进程宿主、窗口生命周期、托盘 | 产品关心的是自己的进程模型 | 宿主 |
| 模块运行时 / 依赖注入容器 / 配置树 | 工具集不是 DI 框架；强加给每个宿主只会加重依赖 | 宿主（例如产品自己的装配层） |
| 账号鉴权、多租户、配额 | 产品定位是单用户个人客户端 | 宿主 |
| 云端同步、团队共享、密钥托管、凭据脱敏 | 同上 | 宿主 |
| 容器 / VM 级隔离编排 | 内核边界是部署问题，不是 SDK 能承诺的东西 | 部署环境 |
| 把工具暴露成 MCP **server** | 本产品是客户端；`tools/mcp` 是客户端能力 | 不提供 |

逐能力的归属（以及哪些"待办"该进 SDK）见 [OWNERSHIP.md](OWNERSHIP.md)——那张表比本节的
七行更细，且是判断"新需求放哪一层"的入口。

## 2. 一次 Run 的请求流

`agent.Agent.Run(ctx, rig.RunRequest)` 是唯一的执行入口。`kit.Agent.Run` 在其上包了
生命周期、事件桥与成本监视，但循环本身在 `agent` 包里。

```
caller ──rig.RunRequest──▶ agent.Run
   │
   ├─ loadHistory：History.Load + 修复孤立 tool 消息（坏历史会让 provider 永远 400）
   ├─ recoverCheckpoint：有检查点就修复被中断的回合（History 是事实来源）
   ├─ append input：RunRequest.Input 作为用户消息追加
   │
   └─ for iteration := 1..MaxIterations（0 = 不限）:
        ├─ ctx.Err()？→ 返回 ctx.Err()
        ├─ EventTurnStart
        ├─ prepareContext：估算 token 超预算 → Compactor 压缩（失败降级继续）
        ├─ checkpoint(requested)
        ├─ Provider.Stream(req)
        │    ├─ Delta        → EventTextDelta（逐 chunk）
        │    ├─ Reasoning    → EventReasoningDelta
        │    ├─ ToolCalls    → 按 Index 累积工具调用片段
        │    ├─ Usage        → EventUsage，累加 RunResult.Usage
        │    └─ FinishReason → 记录
        ├─ 装配 assistant 消息 → 追加历史 + EventMessage（空消息丢弃）
        ├─ 没有工具调用？→ EventTurnEnd + checkpoint(completed) → 返回
        ├─ toolRound（见下）
        └─ EventTurnEnd → 下一轮
```

`toolRound` 是一轮里最"厚"的部分，顺序不能换：

```
EventToolCall（逐个，按调用顺序）
   → 重复调用守卫（连续 3 轮相同：注入提示；连续 5 轮或同轮 ≥3 个相同：拒绝该调用）
   → 逐个调用审批（运行 goroutine 上，按调用顺序）：
        · 未知工具          → 工具错误 "rig: tool not found"
        · 已注册但未 active → 工具错误（提示用 tool_search 激活）
        · allow-always 命中 → 直接放行
        · Gate.Approve      → allow / allow-always（记入本次 run 的缓存）/ deny（带理由）
   → checkpoint(tool_pending)（失败即 fail-closed，不执行有副作用的工具）
   → 有界 worker 池并行执行（上限 8 个工具体）
   → 按调用顺序回填结果（EventToolResult + role=tool 消息）
   → 取走 NoticeSource 的通知、注入重复调用提示（作为 user 消息）
```

几条容易忽略但属于行为契约的事实：

- **审批串行、执行并行、回填按序**。审批在运行 goroutine 上逐个进行（UI 不会同时弹
  十个窗），执行用 goroutine 并行但有上限（`maxParallelToolCalls = 8`），结果、事件与
  历史都按调用顺序落位，因此历史可重放。
- **历史不可出现"没有回应的 tool_calls"**。未知工具、未激活、被拒绝、批次中途被取消
  ——每条 assistant 消息里的工具调用都会在函数返回前拿到一条 `role=tool` 结果（取消时
  是合成错误），否则 OpenAI 兼容端点会对这个会话的每次请求返回 400。
- **取消**：`ctx` 取消后循环返回 `ctx.Err()`；若取消发生在工具批次中，仍会先把该批次
  的历史补齐再返回。历史写入用 `context.WithoutCancel`，所以"取消"不会留下半条消息。
- **轮数上限**：`agent` 与 `kit` 两层默认都是 `0`（不限，`DefaultSafeMaxIterations = 0`）。
  轮数护栏属于人为边界，成本控制交给 `costmeter`（`Meter`/`MaxRunCostUSD`）与人工审批。
  显式设置 `MaxIterations > 0` 时，达到上限的那一轮工具仍会执行并记录，然后返回包装
  `rig.ErrMaxIterations` 的部分结果——工具调用与结果不能被拆到两次 Run 里。
- **事件是同步回调**：`Emit` 在运行 goroutine 上被调用，不能阻塞；需要扇出就自己缓冲。
  `EventTurnStart`/`EventTurnEnd` 包裹每个模型轮次，`Iteration` 从 1 开始。
- **压缩失败不中止运行**：预算只是估算，真正的裁判是 provider；压缩器出错时发一条
  `EventError` 提示并带未压缩对话继续——最坏情况是 provider 报上下文超限，那比"压缩器
  内部错误"更能指导用户。压缩成功会发 `EventContextCompacted`（带前后条数）。

## 3. 契约层：根包 `rig` 的接口

根包只有接口、值类型与少量纯函数；所有实现都在子包或宿主里。**它的形状就是 SDK 的
兼容面**：改这里等于改所有宿主的编译期依赖。

| 接口 | 谁实现 | 缺省 / 零值行为 |
| --- | --- | --- |
| `Provider`（`Stream`） | `provider/openai`、`provider/{local,ollama,lmstudio}`、第三方 | `agent.New` 拒绝 nil（`rig.ErrProviderRequired`）；空模型名拒绝（`rig.ErrModelRequired`） |
| `Tool` / `ToolResult` | `tools/*` 各包、宿主自定义 | nil 条目、空名字、重名都会让 `agent.New` 在构造期失败；`Tools` 为空表示纯对话 |
| `Gate` / `Decision` | `gate.*` 组合、宿主 | `AgentSpec.Gate` 为 nil 表示全部放行；`kit` 永远填一个（默认两档 `PersonalAsk`） |
| `History` / `HistoryReplacer` | `session.Memory`、`session/store`、`session/sqlite`（未发布）、宿主 | nil 或空 `SessionID` → 不持久化，但 `RunResult.Messages` 仍含本次全部消息 |
| `Agent` / `ToolActivator` | `agent.Agent`；`ActivateTools` 是可选扩展接口 | 宿主可以自己实现 `Agent`（只有一个方法）；未实现 `ToolActivator` 时动态加载不可用 |
| `Asker` | 宿主 UI | `kit`：退到 `gate.PersonalAskWithoutAsker()`（只读放行、其余拒绝并说明"没有可用的审批通道"）；`full` 的 `ask_user` 返回"no interactive UI" |
| `FilePresenter` | 宿主 UI | `kit` 摘掉 `present`；`full` 里 `present` 每次返回"宿主没有提供 FilePresenter" |
| `Presenter`（已废弃） | 兼容旧宿主 | 已废弃的 `present_artifact` 协议遗留，新代码用 `FilePresenter` |
| `PlanController` | 宿主 | `kit` 摘掉 `plan_review`/`exit_plan_mode`；`full` 中调用报"未配置"工具错误 |
| `GoalStore` / `GoalUpdater` | 宿主 | `kit` 摘掉目标三件套；`full` 中调用报"未配置" |
| `memoryvfs.Store` / `memory.Wiki` | `internal/memvfs`（文件/进程内）、`internal/memwiki`（旧 wiki）、`session/sqlite`（未发布）、宿主 | `kit` 按工作区自动开 v2 文件后端（`<state>/memory/<workspaceID>/v2`）；打开失败回落旧 wiki；`full` 用进程内实现 |
| `SkillStore` | `tools/skill` 的文件系统实现、宿主 | `full` 默认文件系统存储（`$ZEOKEEL_SKILLS`、`<root>/.zeokeel/skills`、`~/.zeokeel/skills`） |
| `Scheduler` | 宿主；`full` 有内存兜底 | 内存调度器 |
| `SessionLog` | 宿主（`session/store`、`session/sqlite`（未发布）可实现） | `kit` 摘掉 5 个 `session_*`；`full` 里这些工具降级/报配置错误 |
| `Reviewer` | 宿主、`review.SubagentReviewer` | `full` 里 `code_review`/`auto_review` 报配置错误 |
| `SpillStore` | 宿主、`session/sqlite`（未发布） | nil → 不注册 `spill_read`；`SpillPolicy` 为 nil 时只按工具自身限制截断 |
| `CheckpointStore` | 宿主、`session/sqlite`（未发布） | nil → 不写检查点、不恢复中断回合 |
| `NoticeSource`（工具的可选接口） | `jobs` 等 | 每个工具轮次结束后取 `PendingNotices()`，作为 user 消息注入下一轮 |
| `Logger` | 宿主 | nil = 丢弃诊断输出 |
| `Compactor` | `session/compact`（`WindowCompactor` / `LLMCompactor`）、宿主 | `agent` 层 nil = 不压缩；`kit` 默认 `WindowCompactor` |
| `SubagentRunner` | `agent.NewRunner`、第三方运行时 | nil → 编排工具调用报配置错误 |
| `TeamTaskManager` | 内存实现、宿主数据库 | `full` 用内存任务板 |
| `Settings` | 宿主 | 极简 KV 契约，仍在根包导出以兼容既有宿主；SDK 内没有强制消费者，模型可见的设置类工具已移除（设置应由宿主 UI 管） |

几条契约级不变量（写在包的 `doc.go` 里，也由测试守着）：

- `Tool` 实现必须**并发安全**：同一批工具调用会并行执行。
- `History` 实现必须并发安全；`Append` 以一次调用为原子单位。
- `Provider` 可被并发调用；返回的 `Stream` 归调用方所有，`Recv` 同一时刻只能有一个
  goroutine 调用，返回错误或 `io.EOF` 时释放资源。
- `Gate` 在运行 goroutine 上、工具执行之前被调用，可以阻塞等待用户输入。
- 消息是**线格式形状**（角色、内容分片、工具调用、工具结果），provider 负责翻译成
  各家 API；SDK 从不在会话里存 provider 的 SDK 类型。

## 4. 工具面装配

工具不会自动进入 agent 循环。一条工具从"实现"到"模型能用"要穿过四道关：

```
实现（tools/<pkg>）
  → 装配：tools/full.NewRuntime(AllOptions{…}).Tools        // 81 个（+SandboxGrants 83）
  → 过滤：tools/presets（personal 55 / standard 73 / minimal 8 / all 83）
  → 体检：kit 的零配置体检（摘掉当前组合里永远无法成功的工具）
  → 注册：AgentSpec.Tools；其中只有 active 集合会出现在请求的 tools 数组里
```

### 4.1 catalog 账本

`tools/catalog` 是发布账本：`Static`（83 个静态工具名）、`Legacy`（58 个旧版名字）、
`Modern`（62 个现代工具面的对照名单）、`ServerBound`（7 个需要 MCP/LSP 客户端的名字）、
`DocumentedGaps`（0）。它是覆盖测试与 `docs/ai/tools.json` 的事实来源：增删工具必须
同时改账本，否则 `go test ./tools/full -run TestToolCatalogCoverage` 变红。

`tools/full` 是**唯一聚合点**：它把各工具包的实现装配成 `[]rig.Tool`。两个入口：

- `full.NewRuntime(AllOptions) (Runtime, error)` —— **推荐**。除了工具集，还交出
  `Jobs()` / `Terminals()` 两个后台句柄与 `Close()`；`kit` 走这条。
- `full.All(AllOptions) []rig.Tool` —— 旧签名，内部就是 `NewRuntime` 之后丢掉句柄。
  需要关后台任务的宿主不要用它。

`AllOptions` 里 `Policy`/`Root` 决定"根感知工具"是否构造：没有 `Root` 也没有带
`Roots` 的 `Policy` 时只有 **78** 个工具（`apply_patch`/`search`/`undo` 不构造，文件
工具一律 `sandbox: denied: no roots configured`）；传了 `Root` 或 `Policy` 是 **81**，
再加 `SandboxGrants: true` 是 **83**。

### 4.2 presets 只按名字过滤

`tools/presets` 不做系统提示词、拓扑或权限：它只是"名字集合"。因此同一个 `personal`
可以叠在任意策略与门禁上。`personal`（kit 默认，55 个）去掉全部多智能体编排工具；
`standard`（73）去掉 10 个兼容旧名；`all`（83）是账本原样；`minimal`（8）是六个
`terminal_*` 加 `read_image`、`working_directory`。

### 4.3 零配置体检

原则一句话：**默认工具集里每个工具都必须真的能用**。留下一个"永远失败"的工具只会
诱导模型反复重试（既烧钱又让人看见一串错误）。因此 `kit.New` 会按宿主依赖摘掉：

- 缺 `Searcher` → `web_search`；缺 `SessionLog` → 5 个 `session_*`；
- `kit` 没有注入入口 → `get_goal`/`create_goal`/`update_goal`/`plan_review`/
  `exit_plan_mode`；
- 缺 `Presenter` → `present`；缺 `Asker` → `ask_user`；
- `AllowNetwork=false` → `web_fetch`；非 Windows → `pwsh`。

以上是那 **15 个**；`undo` 是独立的一条（没有回合快照就不暴露），不计入 15。

结果就是零配置 **40 注册 / 16 初始 active**（核心 13 + `tool_search` + `memory` +
`skill`）。宿主每接一项依赖就按项加回。

### 4.4 动态工具加载与三条缓存契约

默认 `ToolDiscovery` 开启：注册表里的工具**注册但不 active**，初始只把核心工具与
常驻入口发给模型；模型用 `tool_search`（`{}` 列清单、`{"query":…}` 筛选、
`{"activate":[…]}` 加载）自助激活。这套设计成立的前提是三条缓存契约：

1. **不激活时逐字节稳定**：请求里的 `tools` 数组在会话内不会因为别的原因变化；
2. **只追加**：激活是纯增量。`agent` 维护的是**有序** active 列表，新工具按**注册
   顺序**追加在末尾，已激活部分不重排——所以"激活到全集"也不会让前缀失效；
3. **不重建 system prompt**：Rig 的系统提示词**不含工具描述**，工具 schema 只出现在
   请求的 `tools` 字段里。因此激活工具不会导致 prompt 前缀变化，最多让激活那一轮
   provider 的 prefix cache 不命中，之后恢复。

常驻集合拆成两类：`memory` 与 `skill` **无条件常驻**（用户记忆是产品承诺；`skill` 的
schema 只有几百字节，而"有哪些技能"是开工前就该知道的信息）；`tool_search` **随
`ToolDiscovery` 开关**——关掉发现时它也不注册。`present` 由 `Config.Presenter` 门控，
接上后保留并立刻进入初始 active 集。宿主可用 `InitialActiveTools` 自定义初始集
（`tool_search`/`memory`/`skill` 会被强制并入），或直接 `Agent.ActivateTools` 从宿主侧
激活（事件 `tool.activated` 的 `trigger=host`）。

## 5. 模块化：三条守门测试

"模块化"在这个仓库里有可执行的判据，不是口号。`internal/rigdoc/modularity_test.go`
用三条测试守住它：

| 测试 | 断言 | 破坏后的后果 |
| --- | --- | --- |
| `TestLeafCapabilitiesDoNotImportAggregators` | `leafCapabilities` 名单里的包不得 import `kit` 或 `tools/full` | 只想用一个能力的宿主被迫拖进整棵树 |
| `TestRootPackageStaysStdlibOnly` | 根包 `rig` 只依赖标准库、且不 import 本仓任何子包 | 整个 SDK 的依赖面随契约层膨胀 |
| `TestKitIsTheOnlyAggregator` | 除 `kit`/`cmd/*`/`examples/*` 外，没有包同时 import `tools/full` 与 `provider/*` | 新增一个 provider 或一组工具就要改多处 |

`leafCapabilities`（34 项）就是"必须能单独拿走使用"的清单：`events`、`costmeter`、
`modelprice`、`instructions`、`session`、`session/store`、`session/compact`、
`storage`、`storagetest`、`memoryvfs`、`spillfile`、`tools/{undo,patch,search,builtin,jobs,terminal,mcp,lsp,ptc,
worktree,skill,memory,subagent,catalog,presets}`、`internal/{fsguard,gitsnap,
diffpreview}`、`provider/{local,ollama,lmstudio,openai}`、`sandbox`。

它的意义是**把承诺变成可执行的检查**：新增一个可选能力包时，把它加进这张表，这条
"能单独使用"的承诺才对该包生效；不加就等于允许它偷偷依赖聚合层。注意这张表包含
`tools/presets` 与 `tools/catalog`——账本与预设也必须能在没有 `kit` 的情况下使用。

## 6. 权限与安全边界

Rig 的安全模型分三层，**每层的强度不同，不要互相冒充**（DSH 对齐后只有
6.1 与 6.3 在默认路径上生效，6.2 是**可选**叠加）：

### 6.1 进程内围栏（默认，永远在）

`sandbox.Policy` 在进程内检查每一条路径与命令。DSH 对齐后它**只做可写根围栏**：

- 模式：`read-only` / `workspace-write`（默认）/ `danger-full-access`；
- 可写面 = `Roots()` ∪ `AdditionalWritableRoots()` ∪ 平台临时目录（私有 `/tmp`）；
- 根内只读：`WritableRoot.ReadOnlySubpaths`（例如 `.git/config`、`.git/hooks`）——
  这是"根可写但某子树只读"的**唯一**表达；
- 命令允许/禁止名单（`WithCommandAllowlist`；默认不限制命令名，`DisableShellAccess`
  可关掉 shell）、环境白名单、网络与 URL 策略（即使开网也拒绝 loopback/私有/链路
  本地/元数据目标）、输出与超时上限；
- **已删除**（破坏性变更，见 [CHANGELOG](../CHANGELOG.md)）：保护路径
  （`WithProtectedPaths`/`ProtectedPaths`/`WriteDenyPaths`）与拒绝 glob
  （`WithDeniedGlobs`/`DeniedGlobs`）。kit 也不再追加 `.git`/`~/.ssh`/`~/.aws`。
  策略层的命令风险分类留在 `sandbox`，但策略层不用它拦截（真实消费者是
  `gate.CommandRiskGate` 与逐命令提权）。要让模型看不见敏感文件用
  `sandbox.WithHidden`，要真隔离用容器/VM；`sandbox.Policy` 只约束**配合的模型**，
  不改变进程权限。

### 6.2 命令风险判定（可选，默认不在门禁里）

`gate.CommandRiskGate` 用 `sandbox.ClassifyCommand` 把 `bash`/`pwsh` 的 `command`
参数分成 `read-only` / `mutating` / `dangerous` / `unknown`。**它不再是默认预设的
一部分**（DSH 对齐：判据只有一条——谁能批准，谁就有权决定）：

- 显式组合时：`read-only` → 免审批；`dangerous` → 直接拒绝，命中的规则 id（如
  `rm.recursive-root`）与中文理由通过 `DenialExplainer` 拼进工具错误；
  `mutating`/`unknown` → 交给内层（人工审批）。
- 若要它生效，必须叠在工具级白名单之外：`AllowThenAsk` 只看工具名，一旦放行
  `bash`，`ls` 与 `rm -rf /` 就无差别了。
- 为什么默认不用：判定完全依赖进程内的规则表——漏掉一种写法（例如命令走
  `job_start` 而不是 `bash`）就等于放行，多判一种写法就是误拒；而"直接拒绝"让人在
  真正需要时没有出路。真正要挡不可信模型，需要下面的 OS 执行器或容器/VM。

### 6.3 OS 执行器与容器/VM（opt-in / 部署）

- `sandbox/osrunner` 把策略承诺的**文件效果**下沉到操作系统：Linux 用 bubblewrap 或
  内置纯 Go Landlock，macOS 用系统 `sandbox-exec`（Seatbelt），Windows 用受限令牌 +
  ACL。纯 Go、无 cgo、零新增依赖，默认关闭，由宿主 `osrunner.New` 后经
  `Config.SandboxRunner`（kit）或 `AllOptions.SandboxRunner`（full）注入。
- **fail-closed**：`Confine` 失败就拒绝执行，绝不返回未受限 argv 裸跑；没有可用后端时
  返回 `sandbox.ErrSandboxUnavailable`。
- 覆盖程度是**运行时事实**：`Enforcement` 有 `full` / `partial` / `none`，Windows ACL
  与较老 Landlock 是 `partial`（读不受限、网络可能不隔离等）。`Preflight()` 与
  `sandbox_status` 会把这些缺口如实告诉模型与用户，而不是让"没报错"被读成"隔离了"。
- 容器/VM 是**唯一的内核边界**。Rig 不编排容器，也不假装 `sandbox` 是内核沙箱；
  不可信模型请把宿主整体跑进容器/VM。

### 6.4 审批、提权与运行期授权

- 默认门禁组合是 `AllowThenAsk(SandboxReadOnlyTools, inner)`（两档：只读工具免问 +
  其余问人），`inner` 通常是 `gate.AllowSandboxedCommands(..., gate.Ask(host.Asker))`。
  **默认不含** `CommandRiskGate`；要用就显式叠最外层。
- `Approval: "ask"`（默认）= 上述组合，**默认不超时**（一直等，只有取消本轮才撤回；
  `Config.AskTimeout>0` 才有上界）。`Approval: "auto"` = `gate.PersonalAuto()` =
  `AllowAll()`：**不问人、全放行**；`"never"` 全拒。
- 四个产品级档位（`Config.PermissionPreset`）：`read-only` / `workspace-write` /
  `danger-full-access`（全权 + auto）/ `auto`（全权 + ask + `ToolReviewer` 预审）。
  显式设置的 `Gate`/`Mode`/`Approval`（或 `Policy`）优先，档位退化为 `custom`。
- 挂了 OS runner 之后，`AutoAllowSandboxedCommands`（默认开）让**已沙箱化**的 shell
  命令免问（判据不看命令文本；仍写 `approval.response`，`reason=sandboxed`）；文件
  工具不经 runner，照旧问人。理由是安全应由围栏保证，而不是由点击次数保证。
- **逐命令提权**：`sandbox_permissions: "require_escalated"` + `justification`
  （≥10 字符），只在**人工批准**后让那一条命令不被 OS runner 包装。策略、workdir、
  超时、输出上限一律不变；`auto`/`never` 档拒绝；允许与拒绝都写 `sandbox.escalation`
  事件。它是对**执行隔离**的豁免，不是对策略的豁免。
- **运行期可写授权**：`request_sandbox_access` 让模型为"某个工具自己的凭据/缓存目录"
  申请会话级可写根，审批由宿主的 Gate 决定（`ask` 档问你、`auto` 档放行、无通道则
  拒绝），批准后调 `Policy.AddWritableRoot` 立即对所有工具生效；`sandbox_status` 是
  只读诊断。工具自身不审批——这是刻意的人工边界。

更多实现细节（各平台 profile、缓存白名单、拒绝归因、诊断 CLI
`cmd/rigdoctor`）见 [SANDBOX-OS.md](SANDBOX-OS.md)；威胁模型与已知边界见
[SECURITY.md](../SECURITY.md)。

## 7. 持久化分层

状态有四类：会话消息、恢复检查点、记忆、溢出结果。它们共用一套分层：

| 层 | 实现 | 状态 |
| --- | --- | --- |
| 内存 | `session.Memory`（`rig.History`） | 已发布，默认 |
| 目录（NDJSON） | `session/store.Dir`：一会话一文件、启动扫描索引、崩溃自愈、`Replace` 原子替换；`DirStorage` 适配器把它包装成聚合契约（检查点/记忆/溢出返回"不支持"） | 已发布；单目录**单实例**写，不做跨进程锁 |
| SQLite | `session/sqlite`：消息、事件、检查点、记忆、溢出、花费各一张表，带索引、跨会话检索、NDJSON `ImportDir`、WAL、租约、BUSY 重试、0600 | **未发布**：独立子模块（`go.mod` 要求 `v0.24.0`），随下一版本发布 |
| 聚合契约 | `storage.Storage`：`History`/`Checkpoints`/`Memory`/`Spill`/`Close`；`storagetest` 是共享的契约基线 | **未发布**（HEAD 源码中）；访问器不返回 nil，`Close` 幂等 |

装配侧的三条互斥关系：`Config.History`、`Config.SessionStoreDir`、`Config.SessionStore`
同时只能用一个；`Agent.Shutdown(ctx)` 依次收尾在飞 Run、事件、`Config.Storage`、
后台任务。检查点（`rig.CheckpointStore`）写在三个 barrier 上——模型请求前
（`requested`）、有副作用的工具执行前（`tool_pending`）、回合正常结束（`completed`）
——保存失败即把当前回合视为失败（fail-closed），不要在无恢复记录时继续执行副作用。
溢出（`rig.SpillPolicy` + 可选 `rig.SpillStore`）把超大工具结果换成摘要与句柄，接上
Store 时 `full` 会注册 `spill_read` 读回工具——没有读回路径的"已存起来"等于永久丢失。

会话持久化的选型与迁移见 [sqlite.md](sqlite.md)；记忆/上下文数据库与 spill 策略见
[MEMORY.md](MEMORY.md)。

## 8. 扩展点

扩展不需要改循环：实现契约、在装配层注册即可。

| 想扩展 | 实现 | 接在哪 |
| --- | --- | --- |
| 新模型厂商 | `rig.Provider`（`Name` + `Stream` 返回 `rig.Stream`） | `AgentSpec.Provider` / `kit.Config.Provider`；`kit.ProviderKind` 加接线 |
| 单个新能力 | `rig.Tool`（`Name`/`Description`/`Schema`/`Invoke`，并发安全） | `AgentSpec.Tools` 或 `kit.Config.Tools`；内置工具还要进账本 |
| 审批策略 | `rig.Gate`（可再实现 `DenialExplainer` 给拒绝理由） | `AgentSpec.Gate` / `kit.Config.Gate`；默认两档 `PersonalAsk`（`CommandRiskGate` 只在显式叠加时用，且放最外层） |
| 会话存储 | `rig.History`（可选 `HistoryReplacer`） | `Config.History` / `SessionStore` |
| 宿主能力 | `Asker`、`FilePresenter`、`PlanController`、`GoalStore`+`GoalUpdater`、`SessionLog`、`Reviewer`、`Scheduler`、`SkillStore`、`TeamTaskManager` | `kit.Config` 对应字段，或 `full.AllOptions.Host` |
| 状态后端 | `memoryvfs.Store`、`memory.Wiki`、`rig.SpillStore`、`rig.CheckpointStore`、`storage.Storage` | `Config.MemoryStore`/`Memory`/`Spill`/`CheckpointStore`/`Storage` |
| 观测 | `Logger`、`Compactor`、`EventWriter`、`Meter` | 对应 `kit.Config` 字段 |
| 子 agent 运行时 | `rig.SubagentRunner` | `full.AllOptions.Runner`；默认实现 `agent.NewRunner` |
| 模型自己写程序调工具 | `tools/ptc`（`run_code`） | `kit.Config.PTC = true`；内层调用走同一个 Gate，解释器进程同样经 `Runner.Confine` |
| 声明式多步编排 | `tools/workflow` | 由模型调用；Go 原生顺序/并行步骤，`{{prev}}`/`{{steps.name}}` 模板，没有 JS 引擎 |
| 渐进式加载自定义工具 | `tools/shelf`（库原语，不是静态工具） | 宿主自定义工具名后使用 |

几个边界值得记住：

- **PTC 只调用系统已安装的 Python 3 / Node.js**，不自带运行时，也不引入新依赖。
- **`tools/shelf` 不在账本里**：它是给宿主复用的加载原语（list/search/load），不是
  一个模型可见的静态工具。
- **自定义 provider 时不要碰工具层**：provider 只负责把线格式消息翻译成厂商 API，
  任何"这个工具怎么跑"的判断都属于工具/策略层。
- **不要重新加回已删除的包与工具**：`tools/escalate`、`sandbox/escalate`、`mcpserver`
  （+`cmd/rigmcp`）、`internal/filewatch`，以及 `report`、`settings_get`、
  `settings_set`、`present_artifact`。`internal/rigdoc` 的守护测试会拦住回归；
  历史与理由见 [AGENTS.md](../AGENTS.md) 与 [CHANGELOG.md](../CHANGELOG.md)。

## 9. 质量与发布

### 9.1 CI 门禁

一次 PR 要过这些检查。阻塞 job 是 `test`（Linux 全量单测）、`windows sandbox (real
Windows)`（真机 ACL/受限令牌/Job 进程树）、`lint`、`sqlite-consumer smoke`。
CI **只验证代码**：文档生成物、README/示例同步与版本/tag 一致性都不在 CI 里
（版本与 tag 属于发布流程）。

- `gofmt`、`go build ./...`、`go vet ./...`、`go test ./...`（发布前另跑 `-race`）；
- 覆盖率：总覆盖率 80%，有测试的包函数覆盖率 ≥70%；
- `staticcheck`、`govulncheck`；
- 交叉编译 + `GOOS` vet 覆盖 windows/amd64、darwin/arm64、freebsd/amd64；
- **Windows 是真机验证，不是 Wine**：受限令牌、Low 完整性、ACL 继承这些语义 Wine
  只模拟 API 形状、不真正强制，所以在 Wine 上"通过"是假阳性；该 job 跑在
  `windows-latest` 上，故意不固定版本，好第一时间看到 Windows 语义漂移；
- macOS 真机沙箱验证在独立 workflow `macos-sandbox.yml`（`macos-26`，push main 命中
  `sandbox/**`、`internal/procattr/**`、`tools/builtin/**`、自身文件或手动触发），
  **不阻塞** main 与发布，tag 不触发；FreeBSD 只有交叉编译 + `GOOS=freebsd` vet；
- 工具面：`tools/full` 的账本覆盖测试、`tools/audit` 的现代面对照、`catalog.Static`
  与 `presets` 的一致性；
- 模块化守门：叶子包不 import 聚合层、根包 stdlib-only、只有 `kit` 同时装配
  provider + 工具面；
- 依赖策略与"无 cgo"：唯一依赖 `mvdan.cc/sh/v3`；
- fuzz 冒烟（10 个目标 × 5 秒）与 benchmark 冒烟。

CI **只验证代码**。文档与发布状态是本地/发布流程的检查，不进 CI：
`go run ./cmd/rigdoc -check`（`docs/ai/*` 与真实构造器一致）、
`make quickstart`（README 第一段 Go 代码与 `examples/quickstart/main.go` 逐字节一致）、
`make version`（`version.go` 与 release tag 一致）。

本地一次性跑齐：`make ci`；`make deps` 只跑依赖门禁。

### 9.2 rigdoc：工具目录由代码生成

`cmd/rigdoc` 从**真实的构造器**生成 `docs/ai/`：`tools.json` 是每个工具的精确 JSON
Schema（85 条 = 83 静态 + opt-in `run_code` + 未构造的 `spill_read`），`tools.md` 是同一份数据的人读表格。
模型可见的 `Description`/`Schema` 属于接口，所以"文档写的"与"模型看到的"必须同源；
`-check` 在 CI 里阻止两者漂移。

### 9.3 发布流程（摘要）

PR → CI 绿 → 打 **annotated tag**（不要求签名）→ release workflow（复用 CI）→ 直接发布 Release
Release。`scripts/sync-version.sh` 校验 `version.go` 的
`rig.Version` 与 tag 一致；`session/sqlite` 作为独立子模块有各自的发布标签门禁。
完整步骤、失败处置与 checklist 见 [RELEASE.md](RELEASE.md)，版本策略与兼容承诺见
[VERSIONING.md](VERSIONING.md)、[COMPATIBILITY.md](../COMPATIBILITY.md)。

## 相关文档

- [README.md](README.md)：文档索引与维护约定
- [GETTING-STARTED.md](GETTING-STARTED.md)：安装、三种组装方式、审批与沙箱、排障
- [TOOLS.md](TOOLS.md)：工具目录与计数口径
- [CONFIG.md](CONFIG.md)：`kit.Config` 全字段
- [SANDBOX-OS.md](SANDBOX-OS.md)：OS 级沙箱
- [sqlite.md](sqlite.md) / [MEMORY.md](MEMORY.md)：持久化与记忆
- [AGENTS.md](../AGENTS.md)：仓库规则与逐模块说明