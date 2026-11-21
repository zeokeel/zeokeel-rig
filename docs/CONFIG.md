# 配置参考（`kit.Config`）

本文解决什么问题：**Rig 没有配置文件，那宿主到底该怎么配一个 agent？** 读者是准备
把 Rig 接进自己产品的宿主开发者。读完本文你应该能：选对配置风格、知道每个字段的默认
值与坑、判断一次工具调用会被哪一层拦住、并在几种常见产品形态之间直接抄配方。

文中所有字段、常量、环境变量都对照 `kit/kit.go`、`kit/profiles.go`、
`kit/preflight.go`、`kit/escalation.go`、`gate/`、`sandbox/`、`tools/` 的当前
源码核对过；模型可见的英文文案（门禁理由、工具描述、错误串）不翻译、不改写。

> 长生命周期宿主（桌面客户端、守护进程、子代理编排）不要每次 `kit.New`：用
> `kit.NewRuntime(kit.RuntimeConfig)` 共享 provider、后台任务与 PTY 池，再用
> `NewAgent` 为每个会话建轻量 Agent。共享/独占清单、冲突字段与 `Close` 语义见
> [AI-SDK.md §3.2](AI-SDK.md#32-长生命周期宿主kitnewruntime)。

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
| `MaxIterations` | `int` | `0` → **不限**（与 `agent` 层一致） | 模型↔工具轮数。默认不给总闸：轮数护栏是人为边界，会挡住模型本可完成的自主任务；成本控制请用 `Meter`/`MaxRunCostUSD` 与人工审批。`>0` 是**显式**上限（超限返回包装 `rig.ErrMaxIterations` 的错误）；负数同样表示不限。 |
| `ContextBudget` | `int` | `0` → 模型窗口 × 0.6，下限 16k | 估算的 prompt token 预算。`>0` 以配置为准；**`<0`（负数）表示关闭自动压缩**（`Compactor` 不会被装配）。 |
| `Compactor` | `kit.Compactor` | `nil` → `compact.WindowCompactor{KeepLast: 8, ToolResultKeepLast: 4}` | 仅在预算 `>0` 时装配默认值。想要摘要式压缩用 `compact.LLMCompactor`（每次压缩多花一次模型调用）。压缩失败会降级继续，不中止回合。 |
| `MaxRunCostUSD` | `float64` | `0`（不限制） | 单次 Run 的花费上限（美元）。**必须同时配 `Meter`** 才会生效（护栏靠 usage 事件驱动）；超限以可读错误收尾并取消本次 Run。与 `MaxIterations` 是两种护栏：轮数管"模型抽风"，花费管账单。 |
| `Meter` | `*costmeter.Meter` | `nil` | 例如 `costmeter.New(costmeter.WithBudget(2))`；UI 读 `Meter.Session().CostUSD`。挂上 Meter 后 kit 会自动开事件桥，因此不需要宿主额外传 `Emit`。 |
| `Spill` | `*rig.SpillPolicy` | `nil`（不落盘） | 超大工具结果落盘并把模型可见内容换成摘要；`Spill.Store` 非 nil 时会注册 `spill_read`（否则"结果太大已存起来"等于永久丢失）。`Dir` 为空时解析为 `<Root>/.rig/spill`（per-workspace、在可写根内、模型可按 `path:` 读回；建议把 `.rig/` 加进 `.gitignore`）；裸 `agent.New` 不补默认值。 |
| `CheckpointStore` | `rig.CheckpointStore` | `nil`（关闭） | 回合边界写恢复检查点；`agent.RecoverInterruptedTurn` 负责修复被中断的回合（不重放副作用）。 |

### 2.3 工具面

| 字段 | 类型 | 默认值 | 说明与坑 |
| --- | --- | --- | --- |
| `Tools` | `[]kit.Tool` | `nil` → `tools/full.NewRuntime(...)` 的默认工具集 | **`nil` 与空切片含义不同**：`nil` = 用默认工具集；非 nil 的**空切片** = 一个工具都没有。非 nil 时预设与几个注入项的行为都会变（见下方"注入即失效"）。 |
| `ToolPreset` | `string` | `""`，且 `Tools == nil` 时等价 `personal` | `minimal`(8) / `personal`(55) / `standard`(73) / `all`(83)，按名字过滤。**`Tools != nil` 时 `""` 不过滤**（那是宿主自己的选择，不该被静默裁剪）；显式设置则照常过滤。未知预设报错。 |
| `ToolDiscovery` | `*bool`（三态） | `nil` → 开（`Tools == nil` 时）；`Tools != nil` 时 `nil` → 关 | 动态工具加载：初始只把核心 13 + `tool_search` + `memory` + `skill` 发给模型（零配置 16 个 active），其余注册但不 active，模型用 `tool_search` 自助激活。`Tools != nil` 默认关的理由：你自己挑的工具应该立刻能用。想开写 `kit.Bool(true)`。 |
| `InitialActiveTools` | `[]string` | `nil` → 核心 13（再加常驻的 `tool_search`/`memory`/`skill`，零配置共 16） | 覆盖初始 active 集。`tool_search`、`memory`、`skill` 会被**强制并入**（前提是它们真的注册了）；未知名字静默忽略；非 nil 的**空切片**表示初始一个都不 active（只剩 `tool_search` 也能工作）。 |
| `PTC` | `bool` | `false` | 追加 `run_code`（模型写的 Python 3 / Node.js 在系统已装运行时里跑，内层调用仍走同一个 Gate）。`run_code` 在预设过滤**之后**追加，因此不受 `ToolPreset` 影响。解释器**默认原样继承**宿主环境（`PYTHONPATH`/`VIRTUAL_ENV`/自定义键都透得进去，与 bash 一致）；只有策略显式 `WithEnvAllowlist(...)` 时才按名单过滤。 |
| `SandboxGrants` | `*bool`（三态） | `nil` → **开** | 注册 `sandbox_status`（只读诊断）与 `request_sandbox_access`（会话级可写授权申请）。两者都是 discoverable、**不进**初始 active 集，注册不增加每回合常驻成本。`Tools != nil` 时不追加。门禁层面：`sandbox_status` 在只读白名单里，默认**免问放行**；`request_sandbox_access` 不在白名单，`ask` 档下**会问你**（`auto` 档直接放行）——所以放宽可写面始终有人在场或已显式选了不问人的档。关掉用 `kit.Bool(false)`。 |
| `DisableShellAccess` | `bool` | `false` | 与 `CommandAllowlist` 配合决定策略：`true` 且没有 allowlist 时，策略不设置"任意命令可用"，因此**所有** shell 命令被策略拒绝。 |
| `CommandAllowlist` | `[]string` | `nil` → 任意命令（`sandbox.WithAnyCommandAllowed(true)`） | 非空时只允许列出的命令基名（`/usr/bin/git` 匹配 `git`）；此时 `DisableShellAccess` 不再起作用（allowlist 优先）。 |
| `ScratchDir` | `string` | `""` | 私有可写临时目录：非空时注入命令环境的 `TMPDIR`/`TMP`/`TEMP`，并作为 OS runner 的可写根（read-only 档下编译器等工具终于有地方写临时文件）。目录不存在会自动创建（0700）。空串 = 不注入（既有行为）。 |
| `HiddenPaths` | `[]string` | `nil` → 不隐藏 | 对模型隐藏的路径模式（`sandbox.WithHidden`）：命中它的文件与目录在 `read_file`/`list_dir`/`glob`/`grep`/`search` 里都看不到，写工具也拒绝（含 `apply_patch`）。模式相对工作区根书写（`**/.env`、`secrets/**`），不含 `/` 的模式按基名匹配任意深度（`id_rsa`）。**只约束工具面**：`bash` 里 `cat .env` 仍然成立，真正的隔离在 `sandbox/osrunner` 或部署层；`Policy` 非 nil 时本字段被忽略。 |

**为什么默认"不限制命令名"**：`workspace-write` 下如果连 `go test` 都跑不了，
"帮我跑测试"整个产品就不成立。危险性由**人工审批**判定（`ask` 档下每条可变命令
都会问人），而不是靠一张永远不全的程序白名单；默认门禁**不做**命令级风险分类。

**注入即失效（`Tools != nil` 时）**：`Searcher`、`SessionLog`、`GitUndo`/
`UndoDir`/`UndoTracker`、`Asker`（的 `ask_user` 工具）、`SandboxGrants` 都只作用于 kit
自己构建的默认工具集。kit 不会因此报错（工具缺席是宿主的显式选择），但会打一条
`slog.Warn` 把这些被忽略的依赖列出来。**唯一的硬错误**是 `Tools` 与
`SandboxRunner` 同时设置：自建工具集不会经过 OS 沙箱包装，而那会让用户以为
"还在沙箱里"，属于"以为有保护其实没有"，必须构造期失败。

**零配置体检对所有工具集都执行**（这一点容易误读）：`kit.New` 会按
`kit.Config` 上的宿主能力字段摘掉"装配缺失、调用必然失败"的工具——
`Searcher == nil` 摘 `web_search`；`SessionLog == nil` 摘 5 个 `session_*`；
`Presenter == nil` 摘 `present`；`Asker == nil` 摘 `ask_user`；
`AllowNetwork` 显式为 `false` 时摘 `web_fetch`（默认 nil=开，不摘）；非 Windows 摘 `pwsh`；没有回合快照时摘
`undo`；此外 `get_goal`/`create_goal`/`update_goal`/`plan_review`/
`exit_plan_mode` 因为 kit 没有对应注入入口而**恒被摘掉**。
这些判据读的是 **`kit.Config`**，不是 `full.AllOptions`——所以自建工具集时，如果
能力只注进了 `full.AllOptions`（例如 `Searcher`、`Asker`），而要的工具仍被摘掉，
必须在 `kit.Config` 上同样填一遍。

### 2.4 权限与沙箱

| 字段 | 类型 | 默认值 | 说明与坑 |
| --- | --- | --- | --- |
| `Mode` | `kit.Mode`（`sandbox.Mode`） | `""` → `workspace-write` | `read-only` / `workspace-write` / `danger-full-access`；未知值构造期报错。`read-only` 拒绝文件修改**与** shell 执行，**并且会裁剪工具面**：`write_file`/`edit_file`/`apply_patch`/`str_replace_editor` 与 `undo` 不再注册；`bash`/`pwsh`/`run_code` 只在挂了 `SandboxRunner` 时保留（未挂 runner 时策略拒绝所有命令，留着它们只会白烧一轮，因此一并裁掉）。被裁的工具也进不了 `InitialActiveTools`、`Agent.Tools()` 与 `tool_search` 清单；`Config.Tools` 非 nil 时不裁剪。`danger-full-access` 移除根围栏与命令允许列表（显式 denylist 仍生效）。 |
| `Approval` | `kit.ApprovalMode` | `""` → `ask` | `ask` / `auto` / `never`；`Gate` 非 nil 时被忽略。`ask` = 只读免问 + 其余问 Asker（无 Asker 则 fail-closed 拒绝）；`auto` = 不问人、全部放行（与 DSH 的 danger-full-access 档等价）；`never` = 全拒。要硬拦危险命令请显式叠加 `gate.CommandRiskGate`（默认预设不再使用）。未知值报错。 |
| `PermissionPreset` | `string` | `""`（不选） | DSH 四档之一：`read-only`/`workspace-write`/`danger-full-access`/`auto`（见 `kit.Presets()`）。**一次性设置 `Mode` 与 `Approval`**；显式设置的 `Gate`/`Mode`/`Approval`（或 `Policy`）优先，此时实际档位退化为 `custom`（不静默覆盖，只记 warning）。比 `Profile` 更具体，先落地。未知值构造期报错。 |
| `ToolReviewer` | `gate.ToolReviewer` | `nil` | 仅 `auto` 档使用：自动预审器。放行即执行；拒绝或审查失败（超时/出错）回退到人工 ask；没有 `Asker` 时回退到"没有审批通道"的拒绝。没配时 auto 退化为纯 ask（`Preflight` 会说明）。 |
| `AskTimeout` | `time.Duration` | `0` = 一直等 | 审批等待的内部上界。与 DSH 一致：**默认不超时**，只有调用方取消轮次才会撤回请求；`>0` 时超时以 `gate.ErrApprovalExpired` 收尾。与 `auto` 的审查器超时（`gate.DefaultToolReviewTimeout`，15s）无关。 |
| `Gate` | `kit.Gate`（`rig.Gate`） | `nil` → 由 `Approval` 构造 | 非 nil 时**始终优先**（完全接管，且 `PermissionPreset` 退化为 `custom`）。默认组合里**没有** `CommandRiskGate`；要命令风险硬拦需自己把它叠在**最外层**（§4.4）。 |
| `Policy` | `*sandbox.Policy` | `nil` → 由 `Root`/`Mode`/`AllowNetwork`/`CommandAllowlist` 构造 | 非 nil 时那些字段被忽略（决定权归策略），但 `Root` 仍用于定位根感知工具。kit 不追加任何写面约束（见 §2.4 末尾）。 |
| `Root` | `string` | `""` → `"."`（`kit.DefaultRoot`） | 工作区根。`Policy == nil` 时 kit 会 `os.Stat` 校验它存在且是目录（否则构造期报错）。 |
| `AllowNetwork` | `*bool`（三态） | `nil` → **开** | 同时做两件事：给策略 `sandbox.WithNetwork`，以及决定 `web_fetch` 是否注册。`kit.Bool(false)` 才关闭（策略禁止联网 + 摘掉 `web_fetch`）。默认开：网络不在沙箱承诺内，防护交给沙箱围栏与审批。即使开启，策略仍拒绝 loopback/私有/链路本地/元数据等目标（SSRF 防护，不是联网开关）。 |
| `Sandbox` | `*bool`（三态） | `nil` | 只表达**意愿**，不探测平台：`nil` 尽力而为（有 runner 就用）；`true` = "我希望隔离"，但没有后端时**不报错**，只在 `Preflight` 里如实说明落差；`false` = 明确不要（此时再注入 `SandboxRunner` 是自相矛盾的配置，构造期报错）。 |
| `SandboxRunner` | `sandbox.Runner` | `nil` | OS 级受限执行器（`sandbox/osrunner`）。非 nil 时默认工具集的 `bash`/`pwsh`、jobs、terminal 与 worktree 在 spawn 前经 `Runner.Confine` 包装，**失败即拒绝执行**。**仅在 `Tools == nil` 时生效**；与 `Tools != nil` 同时出现是构造期错误。 |
| `SandboxUnavailableReason` | `string` | `""` | "为什么没有系统沙箱"的一句话（通常取 `osrunner.New` 的错误）。kit **不探测平台**（保持纯装配层），所以这句话只能由宿主填；`Preflight`/UI 用它给出完整答案。 |
| `AutoAllowSandboxedCommands` | `*bool`（三态） | `nil` → **开** | 已被 OS 沙箱包住的 shell 命令免审批（**不看命令文本**）；免问仍写一条 `approval.response`（decision=allow、reason=`sandboxed`）。`kit.Bool(false)` 回到"每条可变命令都问人"。判据与边界见 §4.5。 |
| `SandboxDenialSink` | `func(sandbox.Denial)` | `nil` | 接收每一次被识别的沙箱拒绝；挂在策略上，一次配置覆盖所有执行工具。回调必须并发安全。 |
**没有"保护路径"这类装配**：kit 不追加任何写面约束（工作区元数据与主目录凭据位置
都不加），策略层也没有这一层——`DisableProtection`/`ProtectPaths`/
`ProtectWorkspaceMetadata` 与 `kit.HomeSecretPaths()` 已删除（破坏性变更与替代做法
见 [CHANGELOG](../CHANGELOG.md) 的 Unreleased → Removed）。

要"根内局部只读"，自己构造策略并声明可写根的只读子路径：

```go
p := sandbox.New(
    sandbox.WithRoots(root),
    sandbox.WithMode(sandbox.ModeWorkspaceWrite),
    sandbox.WithReadOnlySubpaths(root, ".git/config", ".git/hooks"),
)
agent, err := kit.New(kit.Config{Model: "...", Root: root, Policy: p})
```

要让模型"看不见"某些路径，用 `Config.HiddenPaths`（真的参与判定，见 §2.4）。

### 2.5 宿主能力注入

| 字段 | 类型 | 默认值 | 说明与坑 |
| --- | --- | --- | --- |
| `Asker` | `rig.Asker` | `nil` | `approval=ask` 下真正去问人的通道（`Ask(ctx, question, options)`）。`nil` 时 kit 用 `gate.PersonalAskWithoutAsker()`：只读放行、危险拒绝、其余拒绝并**明确说明"没有可用的审批通道"**。接上它同时会注册 `ask_user` 工具。kit **默认不给**询问设超时（与 DSH 的 approval seam 一致：人多久不回就等多久，只有调用方取消轮次才撤回请求）；要上界用 `AskTimeout`。 |
| `Presenter` | `rig.FilePresenter` | `nil` | 让 `present` 能把交付文件交给宿主 UI。它同时是 `present` 的**门控**：`nil` 时 `present` 被摘掉；非 nil 时保留**并立刻进初始 active 集**（宿主已经付了配置成本，不该再让模型 `tool_search` 一次）。注意类型是 `rig.FilePresenter`（`PresentFiles`），不是已废弃的 `rig.Presenter`。 |
| `Searcher` | `web.Searcher` | `nil` | 提供 `web_search`；`nil` 时该工具被摘掉（而不是留一个永远报错的占位）。 |
| `SessionLog` | `rig.SessionLog` | `nil` | 提供 `session_search`/`session_trace`/`session_event_*`；`nil` 时被摘掉。 |
| `Memory` | `memory.Wiki` | `nil` → 由 v2 后端替代 | **旧版** wiki 存储，仅为兼容保留。与 `MemoryStore` 同时设置时 `MemoryStore` 优先；只设 `Memory` 时 `memory` 工具仍走旧语义。 |
| `MemoryStore` | `memoryvfs.Store` | `nil` → 自动建 SQLite 后端 | 记忆/上下文后端。`nil` 且 `Root` 非空时 kit 用 `<状态目录>/memory/<workspaceID>/memory.db` 建按工作区分区的 SQLite 库（0700，**不写进仓库**）；打不开直接返回错误，不回落旧 wiki/进程内。`ToolDiscovery` 仍把 `memory` 当常驻工具。 |
| `MemoryOptions` | `[]memoryvfs.Option` | `nil` | 传给自动打开的 v2 后端：`WithEmbedder`/`WithReranker`/`WithSummarizer`/`WithExtractor`/`WithIntentAnalyzer`/配额/衰减。默认全部走确定性实现，**不联网、不调模型**。 |
| `MemoryCapture` | `*bool`（三态） | `nil` → 开（有 v2 后端且工具集里有 `memory` 时） | Run 结束后把 user/assistant 消息写进会话 pending 队列。`kit.Bool(false)` 关闭自动捕获；工具显式写入不受影响。 |
| `MemoryRecall` | `*bool`（三态） | `nil` → 开（有 v2 后端且工具集里有 `memory` 时） | 每个 Run 开始前用最后一条用户输入检索相关记忆，作为一条 system 消息注入本轮。`kit.Bool(false)` 关闭自动召回，只靠模型显式调用 `memory`。 |
| `MemoryRecallTokens` | `int` | `0` → 1200 | 自动召回注入的 token 预算（超过就少注入几条）。 |
| `MemoryCaptureToolResults` | `*bool`（三态） | `nil` → 开 | 是否把 tool 结果也写进 pending；`false` 只捕获 user/assistant。 |
| `MemorySkillCatalog` | `*bool`（三态） | `nil` → 开 | 会话首轮是否注入 `<available-skills>` 目录（来自 `mem://skills`）。 |
| `MemoryCommitTokens` | `int` | `0` → 4000 | pending token 超过它时自动 commit（抽取 + 归档）；上下文压缩时也会强制 commit。 |

### 2.6 持久化、事件与收尾

| 字段 | 类型 | 默认值 | 说明与坑 |
| --- | --- | --- | --- |
| `History` | `kit.History` | `nil` → `session.NewMemory()` | 会话历史。与 `SessionStoreDir`/`SessionStore` **互斥**（同时设置构造期报错）。 |
| `SessionStoreDir` | `string` | `""` | 启用内置持久化：kit 打开一个 `session/store` 目录并挂成默认会话（一会话一 NDJSON，崩溃自愈）。要求 `History == nil` 与 `SessionStore == nil`。**同一目录只能由一个 store 实例写**（不做跨进程加锁）。 |
| `SessionStore` | `*store.Dir` | `nil` | 宿主自己持有的 store 实例（"最近会话"侧栏需要**同一个实例**做 `List`/`Load`）。与 `SessionStoreDir`/`History` 互斥。宿主传入的实例**不在** `Shutdown` 自动关闭范围内（要关就在 `Storage` 里登记一次）。 |
| `Storage` | `kit.Storage` | `nil` | `Shutdown` **最后**收尾的存储（只需要 `Close`；`*session/sqlite.Store` 直接满足）。 |
| `BackgroundJobs` | `[]kit.Background` | `nil` | `Shutdown` 要一并关掉的后台句柄（`jobs.Manager`/`terminal.Manager` 都满足）。只在宿主自建 `Tools` 时需要：默认工具集由 kit 经 `full.NewRuntime` 构造，两个管理器已被自动登记。 |
| `Aggregate` | `storage.Storage` | `nil` | 一次给出 `History`/`Checkpoints`/`Memory`/`Spill` 的聚合存储；展开时**显式设置的字段优先**，并自动登记进 `Shutdown`。 |
| `Activations` | `rig.ToolActivationStore` | `nil` | `tool_search` 的激活结果按会话持久化（`Load`/`Save`）；首次使用该会话的 active 集合时惰性加载并与 `InitialActiveTools` 合并（构造期还没有 SessionID），Run 前再确认一次。内存实现见 `kit.NewMemoryActivations()`。 |
| `PromptSections` | `[]kit.PromptSection` | `nil` | 宿主追加的系统提示词区段，按序渲染在 `SystemPrompt` 之后、`<environment>` 与仓库指令之前。 |
| `ArtifactPresenter` | `rig.Presenter` | `nil` | `present` 的内容产物形态（title+body → 宿主渲染）。没有它时该形态返回可读错误，文件形态不受影响。 |
| `Goals` / `GoalRevisions` | `rig.GoalStore` / `hosttools.GoalRevisionStore` | `nil` | 目标工具的实现。只给 `Goals` 时保留旧版 `goal`；`get_goal/create_goal/update_goal` 需要 `GoalRevisions`（`hosttools.NewMemoryHost()` 两者都满足）。 |
| `PlanController` | `rig.PlanController` | `nil` | `plan_review`/`exit_plan_mode` 的实现；为 nil 时这两个工具被摘掉。 |
| `EventWriter` | `io.Writer` | `nil`（无事件流） | 把 agent 事件桥接成 `events.Envelope`（NDJSON）；一个 Run 一条流。写失败不影响 Run（错误被记成粘性状态，用 `Agent.EventError()` 读）。 |
| `GitUndo` | `bool` | `false` | 每个 Run 开始前用 git 打一份隐藏快照（`git stash create` 语义，不动 HEAD/分支/暂存区），并让 `undo` 工具出现。工作区不是 git 仓库时该开关被静默忽略（要撤销请用 `UndoDir`）。UI 可经 `Agent.UndoTracker()` 直接做"撤销按钮"。`read-only` 档下 `undo` 被工具面裁剪，本开关不会让工具出现（快照仍照打）。 |
| `UndoDir` | `string` | `""` | 回合快照目录。工作区**不是** git 仓库且本字段非空时，kit 挂上纯文件系统快照后端（`undo.NewFileTracker`），`undo` 工具因此出现：每个 Run 开始前把工作区里的普通文件复制到 `<UndoDir>/turns/<turn>/files/<rel>`（清单 `manifest.json` 在回合目录里，与镜像分开命名空间），回退时写回原文、删掉本回合新增的文件、恢复本回合删除的文件；回退不跟随符号链接（类型变化只报 warning），只读目标也能回退。单文件超过 4 MiB 只记"未快照"并在结果里列出（不改动它）；符号链接等非常规文件不参与；`.gitignore` 不生效（没有 git）。回合号持久化在 `<UndoDir>/state.json`，**重启后继续递增**。文件后端构造失败（例如本字段等于 `Root`）时 `kit.New` 返回错误。git 仓库里 `GitUndo` 仍然优先，此时本字段只当 git 后端的回合号目录用（默认 `<Root>/.rig/undo`；没开 `GitUndo` 时 git 仓库不挂撤销），并会被自动加进忽略表。 |
| `UndoTracker` | `*undo.Tracker` | `nil` | 已废弃：优先用 `GitUndo`/`UndoDir`。保留是为了兼容既有宿主；与 `GitUndo` 同时设置时 **`UndoTracker` 优先**。 |
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
| 轮数上限 | `rig.RunRequest.MaxIterations`（`>0` 生效，`<=0` 表示"用 agent 默认"） → `rig.AgentSpec.MaxIterations`（kit 写入：`0`→不限、负数→不限、`>0` 显式） |
| 审批门禁 | `rig.RunRequest.Gate`（非 nil） → `rig.AgentSpec.Gate`（kit 写入 `Config.Gate`，或由 `Config.Approval` 构造） → `nil`（裸 `agent.New` 时等于全放行） |
| 会话/历史 | `rig.RunRequest.SessionID` 选择会话 → `Config.History` / `SessionStoreDir` / `SessionStore` 决定后端 → 默认 `session.NewMemory()` |
| 上下文预算与压缩 | 只有 `Config.ContextBudget` / `Config.Compactor`（没有 per-run 字段） |
| 工具面 | `Config.Tools` → 默认 `full.NewRuntime` → `Config.ToolPreset` 过滤 → 零配置体检裁剪 → `PTC` 追加 → 动态加载的 active 集 |
| 沙箱策略 | `Config.Policy` →（`nil` 时）由 `Root`/`Mode`/`AllowNetwork`/`CommandAllowlist` 构造 → 不追加任何写面约束 |
| provider | `Config.Provider` →（`nil` 时）按 `Config.ProviderKind`（默认 `openai`）+ `BaseURL`/`APIKey`/`APIKeyEnv` 构造 |

同名旋钮在各层含义一致；**更具体的一层赢**。`kit.RunRequest`/`RunResult`/
`kit.Event` 都是 `rig` 同名类型的别名，所以先 `kit.New`、日后下沉到
`agent.New` 不需要重写工具接线。

## 4. 权限模式 × 审批策略

两个正交的旋钮：

- **`Mode`（权限模式）**：工具实现层的进程内围栏——文件工具能写哪、shell 能不能跑。
- **`Approval`（审批策略）**：一次工具调用在**执行之前**该由谁点头。

### 4.1 模式（DSH 对齐：只做可写根围栏）

| `Mode` | 文件修改 | shell 命令 |
| --- | --- | --- |
| `sandbox.ModeReadOnly` | 拒绝；唯一可写的位置是 `/dev/null`（**没有**可写 `/tmp`） | 拒绝 |
| `sandbox.ModeWorkspaceWrite`（默认） | 仅 workspace ∪ 显式追加的可写根 ∪ **私有** `/tmp`（`$TMPDIR`） | 受命令允许列表/`WithAnyCommandAllowed` 约束 |
| `sandbox.ModeDangerFullAccess` | 不包装：OS runner 的 `Enforcement` 是 `none`，进程能写哪就能写哪 | 无允许列表围栏（显式 denylist 仍生效） |

三个模式就是 DSH 权限模型的三档，策略层**只强制可写根围栏**，外加 `..`／绝对
路径／符号链接的包含性检查。挂 `sandbox/osrunner` 时四个后端渲染出的可写面是：
`--ro-bind / / --dev /dev`（只读，只有 `/dev/null` 可写）／再加 `--tmpfs /tmp` +
`--bind <workspace>`（workspace-write，workspace 与私有 tmp）。详见
[SANDBOX-OS.md](SANDBOX-OS.md)。

**以下两块历史加固已删除**（破坏性变更，替代做法见 [CHANGELOG](../CHANGELOG.md)
的 Unreleased → Removed）：保护路径（`WithProtectedPaths`/`AddProtectedPaths`/
`ProtectedPaths`/`WriteDenyPaths`）与拒绝 glob（`WithDeniedGlobs`/`DeniedGlobs`）。
"根内只读"只有一种表达：`WritableRoot.ReadOnlySubpaths`（例如 `.git/config`、
`.git/hooks`）。

策略层的**命令风险分类**（`sandbox.ClassifyCommand`/`ClassifyShellCommand`）仍在，
但策略层不用它拦截：真实消费者是 `gate.CommandRiskGate` 与 `tools/builtin` 的逐命令
提权。

**想让模型"看不到"某些路径**，用 `Config.HiddenPaths`（`sandbox.WithHidden`）：它是
**参与判定**的隐藏（读、列目录、`glob`/`grep`/`search`、写入都拒绝）。它只约束工具面，
不改变 shell 行为。

两者都是**工具实现层的围栏，不是内核边界**：它们约束"配合的模型"，挡不住真的
去碰。要真正的文件效果边界得挂 `sandbox/osrunner`（见
[SANDBOX-OS.md](SANDBOX-OS.md)），要进程级隔离用容器/VM。**网络、读范围、进程
可见性与设备访问都不在沙箱的承诺范围内**（由部署层负责）。

### 4.1.1 四个权限档（`PermissionPreset`）

| 档位 | `Mode` | `Approval` | 一句话 |
| --- | --- | --- | --- |
| `read-only` | `read-only` | `ask` | 不能改文件、不能执行命令；写工具与 `undo` **不注册**，未挂 runner 时 `bash`/`pwsh`/`run_code` 也不注册（工具面裁剪）；需要审批的调用会问你 |
| `workspace-write` | `workspace-write` | `ask` | 只让它改工作区：读免问，写文件与可变命令问你 |
| `danger-full-access` | `danger-full-access` | `auto` | 全权且**不问人**（`Enforcement=none`） |
| `auto` | `danger-full-access` | `ask` | 全权 + 先由 `ToolReviewer` 自动预审，拿不准再问你 |

`custom` 是"当前旋钮值不匹配任何预设"的推导状态：可以被展示，但不能作为选择目标
（`kit.Config.PermissionPreset` 不接受它）。`auto` 档没配 `ToolReviewer` 时退化为
纯 `ask`，`Preflight` 会把这个降级写明。

### 4.2 审批两档

与 DSH 一致，**审批只有两档**：`ask`（逐调用问人）与 `never`（全拒）。不存在
"危险命令硬拦"这一档：`rm -rf /` 在 `ask` 档下与其它可变调用一样走人工审批，由人
决定；在 `never` 档下与其它一切调用一样被拒。

| `Approval` | 实际门禁 | 语义 |
| --- | --- | --- |
| `ask`（默认） | 有 `Asker` → `gate.PersonalAsk(gate.AllowSandboxedCommands(…, gate.Ask(Asker)))`；无 `Asker` → `gate.PersonalAsk(DenyAllWithReason(DenyAllNoChannelReason))` | **两层**：只读工具免问 → 其余（写文件、bash/pwsh、后台任务…**包括危险命令**）问人；没有审批通道时 fail-closed 拒绝并说明理由 |
| `auto` | `gate.PersonalAuto()` = `gate.AllowAll()` | **全放行、不问人**：`rm -rf /`、`mkfs`、`curl … \| sh` 都会执行。要保护就换 `ask`，或自己显式叠 `gate.CommandRiskGate` |
| `never` | `gate.PersonalNever()` = `gate.DenyAll()` | 拒绝一切调用，理由说明 `approval=never` |

`CommandRiskGate` 仍然完整保留（宿主可自行组合），只是**不再是任何默认预设的一部分**：
判据只有一条——谁能批准，谁就有权决定。

矩阵（X 轴 `Mode`，Y 轴审批；"策略拒"表示即使门禁放行，工具实现层仍会拒绝）：

| | `ask` + 有 Asker | `ask` + 无 Asker | `auto` | `never` |
| --- | --- | --- | --- | --- |
| `read-only` | 只读免问；写/执行**问人**（且策略随后拒） | 只读放行；其余拒绝（"没有可用的审批通道"） | 只读直接跑；写/执行门禁放行但**策略拒** | 全拒 |
| `workspace-write` | 只读免问；其余问人；已沙箱化命令免问 | 只读放行；其余拒绝并说明无通道 | 不问人、全部放行；写仍受可写面围栏 | 全拒 |
| `danger-full-access` | 只读免问；其余问人（**没有沙箱，因此没有"已沙箱化免问"**） | 同左但需要审批的调用被拒（无通道） | 不问人、全部放行——**只应在容器/VM 里这么配** | 全拒 |

### 4.3 两层 `ask` 的判据

1. **只读免问**：`gate.SandboxReadOnlyTools` 白名单（`read_file`、`list_dir`、
   `glob`、`grep`、`read_image`、`search`、`inspect`、`sandbox_status`、
   `working_directory`、`tool_search`、`skill`、`memory`、`todo_write`、5 个
   `session_*` 等）直接放行。白名单的判据是"是否触及用户资源（文件/进程/网络）"，
   不是"是否只读"。**注意免问按工具名判定**：`bash`/`pwsh`/`job_start`/`terminal_send`
   这些能执行任意命令的工具永远不在免问名单里，它们的每一次调用都会问人。
2. **其余问人**：写文件、`apply_patch`、可变命令、**以及 `rm -rf /` 这类危险命令**
   都交给 `Config.Asker`（`gate.Ask`；**默认不设超时，一直等**，只有取消本轮才撤回；
   `AskTimeout` >0 才有上界；可选 `allow`/`allow-always`/`deny`）。没有 `Asker` 时
   fail-closed 拒绝并给出可读理由（"没有可用的审批通道"）。

危险命令**不再被审批层硬拒**：命令风险分类（`sandbox.ClassifyCommand`）仍可被宿主
拿去做自己的判断，但默认门禁不看它。要在审批之外再叠一层硬拦，显式组合
`gate.CommandRiskGate`（见 §4.4）。

拒绝是工具错误（`DecisionDeny`）而不是 Go error，所以 run 继续、模型有下一轮。

### 4.4 自定义 Gate 与顺序

默认情况**不需要**自定义 Gate：`Approval: ask` 已经拼好
`AllowThenAsk(SandboxReadOnlyTools, AllowSandboxedCommands(Ask))`。以下是"我自己
要再叠一层命令风险硬拦"的写法（这是 opt-in，不是默认）：

```go
import (
    "github.com/zeokeel/zeokeel-rig/gate"
    "github.com/zeokeel/zeokeel-rig/kit"
)

g := gate.CommandRiskGate( // ① 自选的最外层：危险命令直接拒（默认预设不含这一层）
    gate.PersonalAsk( // ② 只读工具免问 + 已沙箱化命令免问
        gate.Ask(host.Asker), // ③ 其余问人（默认不设超时）
    ),
)
a, err := kit.New(kit.Config{Model: "gpt-4o-mini", Root: root, Gate: g})
```

- **`CommandRiskGate` 若要用，必须在 `AllowThenAsk` 之外**：`AllowThenAsk` 只看
  工具名，一旦放行 `bash`，里面的 `ls` 与 `rm -rf /` 就无差别了；反过来把风险门禁
  放内层，则每条 bash 命令都会先弹一次窗才轮到风险判定。
- 默认预设**不使用**它：规则表的覆盖率靠不住（换一个工具名或写法就漏判），而且
  "直接拒绝"让人在真正需要时没有出路。需要它时显式组合。
- **不要**写 `gate.PersonalAsk(nil)` 或 `gate.PersonalAsk(gate.DenyAll())`：前者让
  每个需要审批的调用都被拒（配置疏漏），后者的文案会说 `approval=never`，与真实
  状态不符。没有 `Asker` 时的正确兜底是 `gate.PersonalAskWithoutAsker()`。
- `gate.Review(reviewer, gate.WithReviewFallback(gate.Ask(...)))` 可以先让
  reviewer 判、超时/出错再回落到人；`PermissionPreset: "auto"` 用的是它能接受的
  加强版（reviewer **明确拒绝**也回落到人，见 `kit` 里的 `autoReviewGate`）。

### 4.5 已沙箱化的命令免审批

判据（**全部满足**才免问，实现见 `gate.AllowSandboxedCommands`）：

1. 工具是 shell 类（`bash`/`pwsh`）；
2. `SandboxRunner != nil` 且自报能强制（`sandbox.RunnerStatus` 的 Enforcement
   不是 `none`）；
3. 策略非 nil 且模式不是 `danger-full-access`。

**不再看命令文本**：围栏已生效时，命令内容不是免问的门槛（DSH 对齐后取消了命令
风险分类这一层判据）。

免问仍写一条 `approval.response`（decision=allow、reason=`sandboxed`），UI 可以
显示"这条没问是因为已隔离"。文件工具（`write_file`/`edit_file`/`apply_patch`）
不经 OS runner，**照旧问人**。

它在组合里的位置是**固定**的：`AllowThenAsk`（只读免问）→
`AllowSandboxedCommands` → `Ask`（问人）；只有"可变但会被 runner 包住"的那一类
才走免问路径。用 `Config.Approval` 时 kit 已经拼好；自己拼装时不要把这个顺序搞反。

这条默认值（`nil` = 开）的理由：沙箱与审批是两种独立的成本；用户会为了少点几十次
弹窗而把审批整体关掉（`approval=auto` → 全放行）。**headless 宿主受益最大**：
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
- 策略可写面、命令允许/禁止名单：**不变**。
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
审批后 `Policy.AddWritableRoot` 生效；工具自身不审批。可写面公式（策略层没有
"保护路径"这一项）：

```text
Roots() ∪ AdditionalWritableRoots() ∪ 平台临时目录（私有 /tmp）
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
| `MaxIterations` | `0`（不限） | `0`（不限） | `0`（不限） |
| 定位（`Profile.Note`） | "每一步都问我：只读操作免问，其余写/执行都要我确认" | "推荐：只读免问、其余问我" | "本地模型：不问人、全部放行；默认不联网" |

注意两点：

- `careful` 与 `personal` 在**取值上完全相同**——差别只在说明文案（`careful` 存在
  是给设置界面一个"最保守"的选项名）。想更严格请显式写配置：`Mode: read-only`
  或 `gate.PersonalNever()`。
- `local` 把 `Approval` 设为 `auto`（**全放行、不问人**）并把 provider 换成 ollama。
  它不涉及联网开关——`AllowNetwork` 默认就是**开**（三态 nil=开），与档位无关；
  `auto` 档的围栏仍由 `Mode`（默认 `workspace-write`）表达。

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
| `XDG_STATE_HOME` | `internal/memwiki.DefaultStateDir` | 记忆的状态目录基址（实际落在 `<该值>/rig/memory/<workspaceID>/memory.db`；旧 wiki 在同级的 `pages/`） |
| `LOCALAPPDATA` | `internal/memwiki`、`sandbox/osrunner` | Windows 上的状态目录基址（`%LOCALAPPDATA%\rig`）与工具链缓存根 |
| `APPDATA` | `sandbox/osrunner`（Windows） | 工具链缓存根（例如 `%APPDATA%` 下的包管理器缓存）；不含凭据文件 |
| `HOME` / `USERPROFILE` | `sandbox/osrunner`、`kit` | 主目录：私有临时目录的基址来自它；取不到就不猜 |
| `TMPDIR` / `TEMP` / `TMP` | `sandbox`（策略的可写集）、`sandbox/osrunner` | 平台临时目录：`$TMPDIR`（`os.TempDir()`）是否进入可写集受 `sandbox.WithTmpdirEnv` 控制；Windows 上还是私有临时目录的候选基址 |
| `RIG_SANDBOX_TEMP` | `sandbox/osrunner`（Windows） | 私有临时目录基址的**显式覆盖**（受限环境下指定可写目录）；优先级高于系统 temp 机制（`os.TempDir()` 与 `GetTempPath` 的回退链） |
| `SystemRoot` / `WINDIR` | `sandbox/osrunner`（Windows）、`sandbox` | 系统目录：落在其下的临时基址会被剔除（普通用户不可写） |
| `SystemDrive` | `sandbox/grant.go` | Windows 上校验可写授权路径的盘符 |
| `COMSPEC` | `sandbox/osrunner`（Windows） | 功能探测用的 shell（缺省 `cmd.exe`） |
| `RIGDOCTOR_ASKER` | `cmd/rigdoctor` | 诊断 CLI 的"有没有审批通道"假设（`yes`/`no`）；库本身不读 |
| `GOOS` | `cmd/rigdoctor` | 仅用于报告里的 `platform` 字段（注意它读的是**环境变量** `GOOS` 而不是 `runtime.GOOS`：没设置时报告里显示 `host`；纯诊断字段，不影响任何行为） |

另外两个与"读取"无关但常被问到的事实：

- **子进程继承的环境**默认是**原样继承**：`Policy.CommandEnv` 在未显式
  `sandbox.WithEnvAllowlist(...)` 时把宿主环境整个交给子进程（`bash`/`pwsh`/`jobs`/
  `terminal`/`run_code` 一致），因此 `PYTHONPATH`/`VIRTUAL_ENV`/`GOPATH`/自定义键与
  **凭据**都可见。这是 DSH 对齐后的决定（防护由沙箱/容器负责，见 SECURITY.md
  「凭据」）。要收紧就显式配置：`sandbox.WithEnvAllowlist(sandbox.DefaultEnvAllowlist()...)`
  会退回"只保留 PATH/HOME/LANG 与 Windows 的 TEMP/TMP/TMPDIR/LOCALAPPDATA/APPDATA/
  USERPROFILE/SystemRoot/SystemDrive/WINDIR/COMSPEC/PATHEXT/USERNAME/USERDOMAIN/OS"
  这一份推荐清单；`WithEnvAllowlist()` 传空则命令以空环境运行。
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
    // 可选：挂上 OS 沙箱后，已被围栏化的 shell 命令不再逐条弹窗
    // SandboxRunner: runner,
})
```

### 9.3 无人值守（CI、批处理）

```go
a, err := kit.New(kit.Config{
    Model:    "gpt-4o-mini",
    Root:     "/work/repo",
    Approval: kit.ApprovalAuto, // 不问人、全部放行（DSH 的 danger-full-access 档）
    // 只有容器/VM 里才值得放开围栏：
    // Mode: kit.ModeDangerFullAccess,
})
```

`auto` **就是** `AllowAll`：`rm -rf /`、`mkfs`、`curl … | sh` 都会执行——这是
"不问人"的准确含义，也正是 DSH 里 `approval=never` 的效果。要保留拦截请用 `ask`
档（由人决定），或自己叠 `Gate: gate.CommandRiskGate(gate.AllowAll())`。

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

### 9.6 回合撤销（GitUndo / UndoDir）

```go
a, err := kit.New(kit.Config{
    Model:   "gpt-4o-mini",
    Root:    root,
    GitUndo: true, // git 工作区：隐藏提交快照（不是仓库时被静默忽略）
})
preview, err := a.UndoTracker().Changes(0, "") // 预览
applied, err := a.UndoTracker().Revert(0)     // 撤销最近一回合
```

工作区不是 git 仓库时用 `UndoDir` 挂纯文件系统快照，契约相同（同样是 `Changes`/
`Revert`，`undo` 工具因此出现）：

```go
a, err := kit.New(kit.Config{
    Model:   "gpt-4o-mini",
    Root:    root, // 非 git 目录
    UndoDir: filepath.Join(root, ".rig", "undo"),
})
```

两条后端都由 `undo.Tracker` 统一编排回合号，`UndoDir`/`Options.Dir` 里的
`state.json` 让回合号**跨进程继续**（重启后从上次的号码往下数，不会回到 1）。

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