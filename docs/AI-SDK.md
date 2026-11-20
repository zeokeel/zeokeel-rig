# 给 AI 的 Rig SDK 使用与组装指南

本文是**使用与组装**手册：你要在宿主程序里装配一个 agent，或者作为 AI 直接在这套
工具面上工作，读这一篇就够。它回答四件事：当前版本的工具面到底有多少（以及口径怎么
算）、三种组装方式各自给你什么、权限与沙箱真正拦住什么、以及改代码时必须同步哪些
地方。

机器可读的工具 Schema 在 [docs/ai/tools.json](ai/tools.json)（由 `cmd/rigdoc` 从
真实构造器生成），人读表格在 [docs/ai/tools.md](ai/tools.md)。**两者与本文冲突时，
以代码与 `tools.json` 为准。**

## 1. 当前版本与工具口径

当前 release **v0.23.4**（`rig.Version = "0.23.4"`，与 git tag 一致）。Go 1.27+，
构建与测试一律 `CGO_ENABLED=0`，唯一第三方依赖是 `mvdan.cc/sh/v3`（POSIX shell
解析器，只用于 `sandbox` 策略层）。

**工具数量必须带口径说**，四个口径的数字不同：

| 口径 | 数字 | 含义 |
| --- | --- | --- |
| `kit.New(kit.Config{})` 零配置**注册** | **40** | personal 预设 55 − 没有 tracker 的 `undo`(1) − 零配置体检摘掉的 14（`web_fetch` 自 2026-11 默认注册） |
| 同上**初始 active**（`ToolDiscovery` 默认开） | **16** | 核心 13 + `tool_search` + `memory` + `skill` |
| 接 `Config.Presenter` 后注册 / active | **41 / 17** | `present` 回来并立刻进 active |
| 全接线（Linux、git 仓库、`PTC: true`，personal 预设） | **50 注册 / 17 active** | 见下方公式 |
| `full.All(full.AllOptions{Root, Policy})` | **81** | 不含两个沙箱授权工具 |
| 同上 + `SandboxGrants: true` | **83** | |
| `full.All(full.AllOptions{})`（不传 `Root`/`Policy`） | **78** | `apply_patch`/`search`/`undo` 不构造 |
| `catalog.Static` / `presets["all"]` | **83** | 账本口径 |
| `presets["personal"]` / `["standard"]` / `["minimal"]` | **55 / 73 / 8** | |
| `catalog.Legacy` / `Modern` / `ServerBound` / `DocumentedGaps` | **58 / 62 / 7 / 0** | |
| `docs/ai/tools.json` 条目 | **84** | 83 个 static + opt-in 的 `run_code` |

**核心 13**（一次普通编码会话几乎每轮都用到，schema 常驻）：
`read_file`、`write_file`、`edit_file`、`apply_patch`、`list_dir`、`glob`、`grep`、
`search`、`bash`、`todo_write`、`working_directory`、`read_image`、
`str_replace_editor`。

**零配置体检摘掉的 15 个**（`kit/kit.go` 的 `cfgUnavailableTools`）——personal 预设
里"没有宿主依赖就永远无法成功"的工具：

| 缺什么 | 被摘掉的工具 |
| --- | --- |
| `Searcher` | `web_search` |
| `SessionLog` | 5 个 `session_*` |
| kit 没有注入入口 | `get_goal`、`create_goal`、`update_goal`、`plan_review`、`exit_plan_mode`（恒移） |
| `Presenter` | `present` |
| `Asker` | `ask_user` |
| `AllowNetwork=false` | `web_fetch` |
| 非 Windows 平台 | `pwsh` |

**全接线公式**：55 − 5（kit 无 GoalStore/PlanController 注入入口）− 1（非 Windows
无 `pwsh`）= 49；`PTC: true` 再 +1（`run_code`）= **50**。Windows 上把 `pwsh` 加回
= 51（不含 PTC 则 50）。`undo` 仅在 `GitUndo` 且工作区是 git 仓库时计入。

工具类别（`docs/ai/tools.json` 的 `category`，84 条）：orchestration 28、shell 13、
state 12、host 8、files 8、other 6、session 6、web 2、programmatic 1。

## 2. 三条硬事实（先记住，能省掉一半调试）

1. **工具不会自动进入 agent loop。** 必须显式传给 `agent.AgentSpec.Tools`，或者用
   `kit.New`（它自己调 `tools/full` 构造默认集）。`tools/catalog` 只是账本，
   `tools/presets` 只是名字过滤器，两者都不构造工具。
2. **模型可见的 `Description` / `Schema` / 错误文案属于接口。** 它们是英文、不进中文
   文档的翻译范围；改动要更新快照/测试。中文只用来解释用法。
3. **`full.All` 必须传 `Root` 或带 roots 的 `Policy`。** 全零的
   `full.All(full.AllOptions{})` 只构造 78 个工具：`apply_patch`/`search`/`undo`
   直接不构造，其余文件工具一律返回 `sandbox: denied: no roots configured`。

## 3. 三种组装方式

### 3.1 最快：`kit.New`

一次 import 装好 provider + 工具集 + 门禁 + 历史：

```go
a, err := kit.New(kit.Config{Model: "gpt-4o-mini", Root: workdir})
if err != nil {
    return err
}

// 一个 prompt → 聚合后的文本；emit 实时流出事件
_, result, err := a.RunText(ctx, "session-1", "列出这个仓库的文件并总结",
    func(ev kit.Event) {
        if ev.Type == kit.EventTextDelta {
            fmt.Print(ev.Text)
        }
    })
```

`(*kit.Agent)` 还提供 `Run`（`rig.RunRequest` 全功能）、`Shutdown`、`Preflight`、
`Tools`、`RegisteredTools`、`ActiveTools`、`ActivateTools`、`ToolsFor`、
`CompactNow`、`MemoryWiki`、`MemoryWikiError`、`SessionStore`、`Policy`、
`UndoTracker`。

### 3.2 推荐（完整能力）：`full.NewRuntime`/`full.All` + `agent.New`

kit 不暴露 Host / 子 agent Runner / MCP / LSP 这些宿主能力（`Searcher`、`SessionLog`、`Presenter` 有独立字段）。需要它们就显式装配：

```go
policy := sandbox.New(
    sandbox.WithRoots(root),
    sandbox.WithMode(sandbox.ModeWorkspaceWrite),
    sandbox.WithCommandAllowlist("git", "go", "ls", "cat"),
)

rt, err := full.NewRuntime(full.AllOptions{
    Policy:   policy,
    Root:     root,
    Wiki:     memorytools.NewMemoryWiki(), // 记忆：nil 时用进程内实现
    Host:     hosttools.Options{Asker: host, FilePresenter: host, Interactive: true},
    Reviewer: reviewer,
})
if err != nil {
    return err
}
defer rt.Close() // 关掉 jobs.Manager 与 terminal.Manager（后台命令不能活过进程）

a, err := agent.New(rig.AgentSpec{
    Provider:        provider,
    Model:           "gpt-4o-mini",
    Tools:           rt.Tools,
    History:         session.NewMemory(),
    Gate:            gate.PersonalAsk(gate.Ask(host)), // 只读免问、危险拒绝、其余问人
    CheckpointStore: nil,                              // 需要崩溃恢复时接上
    MaxIterations:   0,                                // agent 层 0 = 不限
})
```

**为什么用 `NewRuntime` 而不是 `All`**：`All` 保持旧签名 `[]rig.Tool`，它**不交出**
后台句柄（`jobs.Manager` / `terminal.Manager` 创建后没有关闭路径）。`kit` 已经切到
`NewRuntime`；手写宿主也应当这样做。

手工装配时两条容易漏的接线：

- **写类工具必须拿到同一个 `Policy`**：`apply_patch` 要用
  `patch.New(root, patch.WithPolicy(policy))` 构造（`full` 内部已接好）。否则
  read-only 模式与可写根围栏对它全部失效——那是沙箱旁路。
- **后台执行要共享同一个 `jobs.Manager`**：`bash` 的后台命令、`job_*` 工具与
  `terminal_*` 必须看到同一个任务池，否则 `job_output` 读不到 `bash` 起的任务。

### 3.3 最小面：按包构造

只要几个工具时不要拿 83 个：

```go
import (
    "github.com/zeokeel/zeokeel-rig/tools"
    "github.com/zeokeel/zeokeel-rig/tools/builtin"
    "github.com/zeokeel/zeokeel-rig/tools/jobs"
)

policy := sandbox.New(sandbox.WithRoots(root))
bg := jobs.NewManager(jobs.WithPolicy(policy))

all := builtin.NewWithPolicyOptions(policy, builtin.WithBackgroundJobs(bg))
keep := map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "bash": true}
var subset []rig.Tool
for _, tool := range all {
    if keep[tool.Name()] {
        subset = append(subset, tool)
    }
}
reg := tools.NewRegistry(append(subset, jobs.Tools(bg)...)...)

a, err := agent.New(rig.AgentSpec{
    Provider: provider, Model: model, Tools: reg.Tools(), History: session.NewMemory(),
    Gate: gate.PersonalAsk(gate.Ask(host)),
})
defer bg.CloseAll()
```

### 3.4 零配置能力边界（默认配置到底能干什么）

`kit.New(kit.Config{Model: ...})` 是**可用但只读**的 agent：

- 默认 `Approval` = `ask`，而 `Asker` 为 nil → `gate.PersonalAskWithoutAsker()`：
  只读工具放行，**其余一律拒绝**并告诉模型"没有可用的审批通道"；
- `CommandAllowlist` 默认空、`DisableShellAccess` 默认 false → kit 给策略开
  `WithAnyCommandAllowed(true)`：命令的放行不靠程序白名单，而靠人工审批（所以默认
  配置下没有 Asker 时，可变命令仍然一律被拒）；
- `AllowNetwork` 默认 false → `web_fetch` 被摘掉；
- 没有 `Searcher` / `SessionLog` / `Presenter` / `Asker` → 对应工具不在集合里
  （见 §1 的 15 个）；
- 记忆默认开（工作区文件 wiki），动态加载默认开，上下文压缩默认开。

要"改代码 → 跑测试 → 修好"这条闭环，二选一：给 `Asker`（写操作与可变命令弹窗确认），
或 `PermissionPreset: "danger-full-access"`（全权且不问人，**就是** `AllowAll` 语义）。

## 4. 权限、审批与沙箱

### 4.1 权限档与审批

| 维度 | 配置 | 作用 |
| --- | --- | --- |
| 权限档 | `kit.Config.PermissionPreset` | 四档：`read-only` / `workspace-write`（默认）/ `danger-full-access` / `auto`；一次性设置 `Mode` + `Approval`，显式字段优先（档位变 `custom`） |
| 权限模式 | `sandbox.Mode` / `kit.Config.Mode` | `read-only`：只有 `/dev/null` 可写、禁 shell；`workspace-write`（默认）：workspace + 私有 `/tmp` 可写；`danger-full-access`：不包装（`Enforcement=none`） |
| 审批 | `kit.Config.Approval` / `rig.Gate` | **两档**：`ask`（默认，逐调用问人）/ `never`（全拒）；`auto` = 不问人、全放行；非 nil 的 `Config.Gate` 完全接管 |
| OS 沙箱 | `Config.SandboxRunner` | **opt-in**，默认 nil：由宿主 `osrunner.New` 后注入 |

默认 gate 是**两层**（`gate.PersonalAsk`）：

1. 只读工具 → 免问；
2. 其余（写文件、`bash`/`pwsh`、后台任务，**包括 `rm -rf /` 这类危险命令**）→ 问
   `Config.Asker`（`rig.Asker`）。**默认不设超时**：一直等，只有取消本轮才撤回；
   `Config.AskTimeout>0` 才有上界。

没有 `Asker` 时不会"自动有人问"：kit 用 `gate.PersonalAskWithoutAsker()`——只读
放行、其余拒绝并说明原因。`Approval: "auto"` = `gate.PersonalAuto()` = **`AllowAll()`**；
`"never"` = `gate.PersonalNever()`，全拒。

默认门禁**不含** `gate.CommandRiskGate`（命令风险分类不再参与默认审批）。宿主若要
"命中即拒"，自己把它叠在 `gate.AllowThenAsk` / `gate.Ask` **之外**（最外层）。

### 4.2 沙箱内命令免审批

`Config.AutoAllowSandboxedCommands`（`*bool`，**nil = 开**）：接了 `SandboxRunner`
之后，`bash`/`pwsh` 的命令不再弹窗（**不看命令文本**）。判据：工具是 shell 类、
runner 非 nil 且自报能强制、策略不是 `danger-full-access`。免问仍写一条
`approval.response`（decision=allow、reason=`sandboxed`）供 UI 与审计；文件工具不经
OS runner，照旧问人。

关闭：`kit.Bool(false)`。headless 宿主受益最大——没有 Asker 时，已沙箱化的命令现在
能跑（此前一律"没有可用的审批通道"）。

### 4.3 保护路径（已不再提供）

**DSH 对齐后本 SDK 不提供保护路径**：`kit.New` 不追加 `.git`/`.aws`/`~/.ssh`/`~/.netrc`
之类的写保护，`sandbox.ProtectedPaths()` 恒返回空，`WithProtectedPaths` /
`AddProtectedPaths` / 拒绝 glob 都只是记录（no-op，标 `Deprecated`）；
`DisableProtection` / `ProtectPaths` / `ProtectWorkspaceMetadata` 同样不再生效。

仍然生效的"根内只读"只有 `sandbox.WritableRoot.ReadOnlySubpaths`（例如
`.git/config`、`.git/hooks`，仅 bwrap/Seatbelt/ACL 后端）。需要保护凭据的宿主请自己
构造 `sandbox.Policy` 并在宿主侧校验，或把敏感文件移出可读面。

### 4.4 沙箱可写面

```
read-only         只有 /dev/null 可写（没有可写 /tmp）
workspace-write   Roots() ∪ AdditionalWritableRoots() ∪ 私有 /tmp（$TMPDIR）
danger-full-access 不包装（Enforcement = none）
```

运行期授权只有一条路径：`request_sandbox_access` 工具提出申请 → 宿主 `Gate` 审批 →
`Policy.AddWritableRoot`。工具自身**不审批**：`ask` 档会问你、`auto` 档放行、无审批
通道则拒绝（预算默认 5 次，`sandbox.WithGrantBudget` 可调）。`sandbox_status` 是只读
诊断（模式、可写面、根内只读子路径、已授权路径、剩余预算）。

想知道"现在到底有没有 OS 级隔离、为什么没有"，用 `(*kit.Agent).Preflight()`——它
返回 `sandbox.PreflightReport`（Summaries / Warnings / NextSteps），措辞与工具里的
拒绝建议同源，UI 与模型不会各说一套。

### 4.5 逐命令提权（`require_escalated`）

沙箱能拦住的只是路径与系统调用；真实软件还会写**非路径资源**（Windows 的 HKCU
注册表、macOS keychain、系统级 socket）。为此 `bash`/`pwsh` 接受两个参数：

| 参数 | 作用 |
| --- | --- |
| `sandbox_permissions` | 目前唯一取值 `"require_escalated"`：让**这一条命令**不被 OS runner 包装 |
| `justification` | 给审批人看的理由，**≥ 10 个字符**；缺了直接报错 |

三条边界：① 必须**人工批准**（`Asker` 为 nil、或 `Approval` 是 `auto`/`never` 时
一律拒绝——自动档不能给自己提权）；② 只豁免这一次执行，`Policy` 可写面与命令
允许/禁止名单一律不变；③ 没有 OS runner 时提权没有意义，直接报错。

注入方式：`builtin.WithEscalator(...)` / `full.AllOptions.Escalator`（kit 按
`Approval` + `Asker` 自动决定是否可用）。**它不是**已整包删除的 `tools/escalate`——
那个包不要加回来。

## 5. 动态工具加载与请求缓存契约

默认开启（`Config.ToolDiscovery`，`nil` = 开）。初始只把默认 active 集（零配置 16 个）
发给模型，其余"注册但不 active"；模型用 `tool_search` 自助激活。

| 参数 | 语义 |
| --- | --- |
| `{}` | 列出所有未激活的工具（名字 + 一行说明） |
| `{"query": "…"}` | 按关键词筛选 |
| `{"activate": ["glob", "web_fetch"]}` | 加载这些工具，本轮起可调用 |

**缓存契约**（这是动态加载能省钱的唯一原因，破坏它等于白做）：

- 激活是**纯增量、按会话、只追加**：请求里的 `tools` 按激活顺序追加（初始集按注册
  顺序），激活到全集也不会重排；
- **不激活时 `tools` 数组逐字节稳定**，所以 provider 的 prefix cache 一直命中；
  激活的那一轮 prefix 不命中，之后恢复；
- system prompt **不含**工具描述，所以激活**不会**重建 prompt；
- 关闭：`off := false; kit.Config{ToolDiscovery: &off}` → 回到"全部注册工具一开始
  就 active"。

相关 API：

- `Config.InitialActiveTools` 自定义初始 active 集（`tool_search`/`memory`/`skill`
  强制并入；非 nil 的空切片表示初始一个都不 active）；
- `(*agent.Agent).ActivateTools(sessionID, names...)` / `(*kit.Agent).ActivateTools` 宿主侧
  激活（幂等，返回 `activated` 与 `unknown`）；
- 事件 `tool.activated`（`events.KindToolActivated`），payload
  `events.ToolActivated{Tools, Trigger}`，`Trigger` 取 `"model"`（模型经
  `tool_search`）或 `"host"`（宿主直调）。

`spill_read` 是 discoverable 的代表：它只在宿主注入 `SpillStore` 时注册，且不进初始
active 集——"真的需要时才用"的工具不该占常驻预算。

## 6. 常用装配配方

### 6.1 `kit.New`：全字段

`Config` 只有 `Model` 必填；其余零值都有文档化默认。全字段清单（按用途分组，类型与
默认值以代码为准）：

| 字段 | 类型 | 不传时 |
| --- | --- | --- |
| `Profile` | `string` | 空。`careful`/`personal`/`local`，只填未显式设置的 `Approval`/`ToolPreset`/`ProviderKind`/`MaxIterations` |
| `ProviderKind` | `kit.ProviderKind` | `""` ≈ openai；可选 `kit.ProviderOllama`/`ProviderLMStudio`/`ProviderLocal` |
| `Model` | `string` | **必填** |
| `SystemPrompt` | `string` | 空（每次请求前不加东西） |
| `MaxIterations` | `int` | 0 → **不限**（`DefaultSafeMaxIterations = 0`，与 agent 层一致）；>0 = 显式上限；负数 = 不限 |
| `ContextBudget` | `int` | 0 → 模型窗口 × 0.6、下限 16k；负数 = 关自动压缩 |
| `Compactor` | `rig.Compactor` | 默认 `compact.WindowCompactor` |
| `ModelContextTokens` | `int` | 0 → 按 128k 推导预算 |
| `ReasoningEffort` | `string` | 空；`low`/`medium`/`high`（仅默认 OpenAI 兼容 provider） |
| `BaseURL` / `APIKey` / `APIKeyEnv` | `string` | OpenAI 默认地址 / 从 `OPENAI_API_KEY` 读 |
| `Provider` | `kit.Provider` | nil → 用上面三个字段构造 openai 兼容 provider |
| `MaxRunCostUSD` | `float64` | 0 = 不限制；>0 时超限以明确错误收尾并取消 Run |
| `Meter` | `*costmeter.Meter` | nil = 不计花费（`EventUsage` 照发） |
| `Tools` | `[]kit.Tool` | nil → 默认工具集；**非 nil 的空切片 = 不用任何工具** |
| `ToolPreset` | `string` | 空 ≈ `personal`（仅在 kit 自建工具集时；宿主传 `Tools` 时需显式才过滤） |
| `ToolDiscovery` | `*bool` | nil = 开 |
| `InitialActiveTools` | `[]string` | nil = 核心 13 + 三个常驻 |
| `PTC` | `bool` | false（`run_code` 只在 true 时出现） |
| `SandboxGrants` | `*bool` | **nil = 开**（注册 `sandbox_status`/`request_sandbox_access`，都不进初始 active） |
| `DisableShellAccess` | `bool` | false |
| `CommandAllowlist` | `[]string` | 空且 `DisableShellAccess=false` → 策略允许任意命令（由风险分类 + 审批裁定）；非空 → 只允许这些命令基名；`DisableShellAccess=true` 且空 → 全部拒绝 |
| `Mode` | `sandbox.Mode` | `workspace-write` |
| `Approval` | `kit.ApprovalMode` | `ask` |
| `Gate` | `kit.Gate` | nil → 按 `Approval` 构造；非 nil 完全接管 |
| `Policy` | `*sandbox.Policy` | nil → 按 `Root`/`Mode`/`AllowNetwork`/`CommandAllowlist` 构造 |
| `Root` | `string` | `"."` |
| `AllowNetwork` | `bool` | false |
| `Sandbox` | `*bool` | nil = 尽力而为（有 runner 就用） |
| `SandboxRunner` | `sandbox.Runner` | nil = 只有进程内围栏 |
| `SandboxUnavailableReason` | `string` | 空（给 Preflight/UI 的一句话） |
| `SandboxDenialSink` | `func(sandbox.Denial)` | nil = 不审计 |
| `AutoAllowSandboxedCommands` | `*bool` | nil = 开 |
| `ProtectPaths` / `ProtectWorkspaceMetadata` | `bool` | 无效/已废弃：保护恒开，关闭只有 `DisableProtection` |
| `DisableProtection` | `bool` | false |
| `Asker` | `rig.Asker` | nil → `PersonalAskWithoutAsker` |
| `Presenter` | `rig.FilePresenter` | nil → `present` 被摘掉；非 nil → 保留并进初始 active |
| `Searcher` | `web.Searcher` | nil → `web_search` 被摘掉 |
| `SessionLog` | `rig.SessionLog` | nil → 5 个 `session_*` 被摘掉 |
| `Memory` | `memory.Wiki` | nil 且 `Root` 非空 → 自动建工作区文件 wiki |
| `History` | `kit.History` | nil → `session.NewMemory()`（或 `SessionStoreDir` 下的默认会话） |
| `SessionStoreDir` | `string` | 空；非空时开一个 `session/store` 目录（要求 `History` 为 nil） |
| `SessionStore` | `*store.Dir` | nil；宿主自己持有实例（与上两者互斥） |
| `Storage` | `kit.Storage` | nil；`Shutdown` 最后收尾它（`*sqlite.Store` 直接满足，子模块**未发布**） |
| `BackgroundJobs` | `[]kit.Background` | nil；只在自建 `Tools` 时需要 |
| `CheckpointStore` | `rig.CheckpointStore` | nil = 关闭检查点 |
| `Spill` | `*rig.SpillPolicy` | nil = 只按工具自身上限截断 |
| `kit.NewEventSink(w)` | `*kit.EventSink` | 不经过 kit 装配时用同一份 `rig.Event` → NDJSON 映射：`spec.Emit = sink.Emit`，写失败累积到 `sink.Err()`。 |
| `EventWriter` | `io.Writer` | nil = 没有 NDJSON 事件流 |
| `GitUndo` | `bool` | false；true 时每回合 git 快照，`undo` 才进工具集 |
| `UndoTracker` | `*undo.Tracker` | nil；已废弃（优先 `GitUndo`） |
| `Instructions` | `*instructions.Set` | nil；AGENTS.md 层级进 system prompt（只 Load 一次） |
| `Environment` | `*instructions.Environment` | nil；`<environment>` 块进 system prompt |

一个覆盖**全部字段**的示例（不用的字段显式写 `nil`/零值并注明默认；真实宿主按需删减）：

```go
a, err := kit.New(kit.Config{
    // —— 模型 / provider ——
    Profile:            "personal",   // 只填未显式设置的 Approval/ToolPreset/ProviderKind/MaxIterations
    ProviderKind:       kit.ProviderOpenAI,
    Model:              "deepseek-chat", // 必填
    SystemPrompt:       "You are a concise coding assistant.",
    MaxIterations:      0,            // 0 = 不限；>0 = 显式上限；负数 = 不限
    ContextBudget:      0,            // 0 = 自动推导（窗口 × 0.6，下限 16k）；负数 = 关压缩
    Compactor:          nil,          // nil = compact.WindowCompactor
    Presenter:          host,         // rig.FilePresenter；nil 时 present 被摘掉
    AutoAllowSandboxedCommands: kit.Bool(true), // nil 也是开
    ToolDiscovery:      nil,          // nil = 开
    InitialActiveTools: nil,          // nil = 核心 13 + tool_search/memory/skill
    ModelContextTokens: 128_000,
    MaxRunCostUSD:      2.5,          // >0 时超限报错并取消本次 Run
    Spill:              &rig.SpillPolicy{Dir: spillDir, ThresholdBytes: 16 << 10, MaxFiles: 200},
    CheckpointStore:    checkpoints,  // rig.CheckpointStore；nil = 关闭
    ReasoningEffort:    "medium",     // "" / "low" / "medium" / "high"
    BaseURL:            "https://api.deepseek.com/v1",
    APIKey:             "",           // 空则从 APIKeyEnv 读
    APIKeyEnv:          "DEEPSEEK_API_KEY",
    Provider:           nil,          // 非 nil 时忽略 BaseURL/APIKey/APIKeyEnv

    // —— 工具面 ——
    Tools:              nil,          // nil = 默认工具集；非 nil 的空切片 = 不用工具
    ToolPreset:         "personal",
    Gate:               nil,          // nil = 按 Approval 构造；非 nil 完全接管
    History:            nil,          // 与 SessionStoreDir/SessionStore 互斥
    SessionStoreDir:    "/var/lib/myapp/sessions",
    SessionStore:       nil,          // 宿主自己持有 store.Dir（与上一行互斥）

    // —— 宿主能力 ——
    Asker:              host,         // rig.Asker
    Searcher:           searcher,     // web.Searcher
    SessionLog:         sessionLog,   // rig.SessionLog
    Meter:              costmeter.New(costmeter.WithBudget(2.5)),
    Memory:             nil,          // nil 且 Root 非空 → 工作区文件 wiki
    DisableShellAccess: false,
    Instructions:       instructionsSet, // *instructions.Set（只 Load 一次）
    Environment:        envBlock,     // *instructions.Environment
    GitUndo:            false,        // true 时每回合 git 快照，undo 才进工具集
    UndoTracker:        nil,          // 已废弃：优先 GitUndo
    EventWriter:        eventLog,     // io.Writer（NDJSON 事件流）
    Storage:            db,           // Shutdown 最后收尾（*sqlite.Store，未发布）
    BackgroundJobs:     nil,          // 只在自建 Tools 时需要

    // —— 权限 / 沙箱 ——
    Root:               workdir,
    Policy:             nil,          // 非 nil 时完全接管（Mode/AllowNetwork/Allowlist 失效）
    Sandbox:            nil,          // 三态意愿开关：nil 尽力而为 / true 想要 / false 明确不要
    SandboxUnavailableReason: "",     // 给 Preflight/UI 的一句话
    SandboxRunner:      runner,       // osrunner.New 的结果（opt-in），见 §6.8
    ProtectPaths:       true,         // 显式锚点：保护默认开，设 false 也关不掉
    ProtectWorkspaceMetadata: false,  // Deprecated，无任何效果
    DisableProtection:  false,        // 唯一关闭保护的入口
    SandboxDenialSink:  nil,          // func(sandbox.Denial)
    SandboxGrants:      kit.Bool(true), // nil = 开（两个授权/诊断工具，不进初始 active）
    Mode:               kit.ModeWorkspaceWrite,
    PTC:                false,        // true 才追加 run_code
    Approval:           kit.ApprovalAsk,
    AllowNetwork:       true,         // 才有 web_fetch（web_search 另需 Searcher）
    CommandAllowlist:   []string{"go", "git", "ls", "cat", "rg"},
})
defer a.Shutdown(ctx) // 宿主关窗口时的唯一正确收尾入口
```

### 6.2 `full.All` / `full.NewRuntime`：`AllOptions` 全字段

| 字段 | 类型 | 作用 |
| --- | --- | --- |
| `Policy` | `*sandbox.Policy` | 文件/shell/terminal/worktree 工具的策略；nil = 以 `Root` 为根的空策略（不联网、无 allowlist） |
| `Root` | `string` | `working_directory`/`inspect` 报告的 workspace 根 |
| `SandboxRunner` | `sandbox.Runner` | 注入 `bash`/`pwsh`/`jobs`/`terminal`/`worktree`（spawn 前 `Confine`，失败即拒） |
| `Escalator` | `builtin.Escalator` | 逐命令提权通道（`require_escalated`） |
| `SpillStore` | `rig.SpillStore` | 非 nil 时追加 `spill_read` |
| `SandboxGrants` | `bool` | 追加 `sandbox_status` + `request_sandbox_access` |
| `BackgroundJobs` | `*jobs.Manager` | 注入宿主自己的任务池（`full` 不改写它的 policy/runner） |
| `SandboxGrantObserver` | `func(rig.ToolCall, sandbox.Grant, error)` | 授权申请审计钩子 |
| `UndoTracker` | `*undo.Tracker` | 回合快照跟踪器（nil 时 `full` 自建） |
| `SearchMaxResults` / `SearchTimeout` | `int` / `time.Duration` | `search` 的默认上限/超时 |
| `PatchMaxBytes` | `int` | `apply_patch` 接受的上限（默认 1 MiB） |
| `ProtectWorkspaceMetadata` | `bool` | 为每个策略根追加工作区元数据保护（默认关，见 §4.3 的 kit 默认） |
| `SandboxDenialSink` | `func(sandbox.Denial)` | 结构化拒绝审计 |
| `Host` | `hosttools.Options` | `ask_user`/`present`/`plan_review`/`exit_plan_mode`/goal 四件的宿主实现 |
| `Presenter` | `rig.FilePresenter` | `present` 的便捷入口（等价 `Host.FilePresenter`） |
| `Asker` | `rig.Asker` | `ask_user` 的便捷入口（等价 `Host.Asker` + `Interactive=true`） |
| `Memory` | `rig.MemoryStore` | **历史字段**：扁平记忆（只读迁移通道，见 docs/MEMORY.md） |
| `Wiki` | `memory.Wiki` | 渐进式 wiki 记忆（与 `Memory` 同时设置时优先） |
| `Scheduler` | `rig.Scheduler` | `schedule_*`（nil = 内存调度器） |
| `SessionLog` | `rig.SessionLog` | `session_*`/`inspect` |
| `Searcher` | `web.Searcher` | `web_search` |
| `LSP` / `LSPPathToURI` / `LSPPathPolicy` | `*lsp.Client` / `func(string) (string, error)` / `lsp.PathPolicy` | `lsp_*` 与旧 `lsp` |
| `MCPClients` / `MCPPrefix` | `[]*mcp.Client` / `string` | MCP 工具（前缀默认 `"mcp__"`） |
| `Runner` | `rig.SubagentRunner` | `subagent`/`workflow`/`ralph`/`team_*`（nil 时工具仍在，调用报配置错误） |
| `Models` | `[]string` | 遗留 `list_subagent_models` 报告的模型列表 |
| `Skill` | `rig.SkillStore` | `skill`（nil = 文件系统 SKILL.md 仓库） |
| `TeamTasks` | `rig.TeamTaskManager` | `team_task_*`（nil = 内存任务板） |
| `Reviewer` | `rig.Reviewer` | `code_review`/`auto_review` |

### 6.3 `agent.New(rig.AgentSpec)`

| 字段 | 语义 |
| --- | --- |
| `Model` | 默认模型名 |
| `SystemPrompt` | 每次运行前加到请求最前面 |
| `MaxIterations` | **<= 0 表示不限**（默认），`>0` 超限返回包了 `ErrMaxIterations` 的错误 |
| `Tools` | 暴露给模型的工具 |
| `InitialActiveTools` | 会话开始时"可调用"的工具名；nil = 全部可调用（v0.20 行为）；只影响请求与派发，不影响注册 |
| `Provider` | 模型后端（`rig.Provider`） |
| `History` | 对话存储；nil = 关闭持久化 |
| `Gate` | 审批门禁；**nil = 全部放行**（与 kit 的默认相反，注意） |
| `Temperature` / `TopP` / `MaxTokens` | 采样选项（零值=未设置） |
| `ContextBudget` / `Compactor` | 超预算时压缩；`Compactor` nil = 不压缩 |
| `Spill` | `*rig.SpillPolicy` |
| `CheckpointStore` | 回合边界检查点；nil = 关闭 |
| `Logger` | `rig.Logger`（`Printf`），nil = 丢弃 |

agent 层还自带三道护栏（kit 沿用）：连续 3 轮**完全相同**的工具调用注入提示；连续 5
轮或同轮 ≥3 个相同调用直接返回 `rig: repeated call blocked`；上下文压缩失败降级继续
而不是中止回合。

### 6.4 宿主接口实现

`full.AllOptions.Host`（`hosttools.Options`）与两个便捷字段背后就是这些接口：

| 接口 | 方法 | 支撑的工具 |
| --- | --- | --- |
| `rig.Asker` | `Ask(ctx, question string, options []string) (string, error)` | `ask_user`，以及 `gate.Ask` 的审批通道 |
| `rig.FilePresenter` | `PresentFiles(ctx, files []PresentedFile) error` | `present` |
| `rig.PlanController` | `State()` / `Review(ctx, content)` / `Exit(ctx, action)` | `plan_review` / `exit_plan_mode` |
| `rig.GoalStore` | `Set` / `Get` / `Clear` | 旧 `goal` 工具 |
| `hosttools.GoalRevisionStore` | 修订版目标（`get_goal`/`create_goal`/`update_goal`） | 三个 goal 工具 |
| `rig.SessionLog` | `Events` / `Search` / `Last` | `session_*`、`inspect` |
| `rig.Scheduler` | `Create`/`List`/`Update`/`Delete` | `schedule_*` |
| `rig.SkillStore` | `List` / `Read` | `skill` |
| `rig.SubagentRunner` | 子 agent 执行器 | 编排一族 |
| `rig.Reviewer` | 评审器 | `code_review` / `auto_review` |
| `web.Searcher` | 搜索后端 | `web_search` |

一个最小宿主（同时实现 Asker 与 FilePresenter）：

```go
type host struct{ ui *UI }

func (h host) Ask(ctx context.Context, question string, options []string) (string, error) {
    return h.ui.Prompt(ctx, question, options) // 默认不设超时：等到用户回答或本轮取消
}

func (h host) PresentFiles(ctx context.Context, files []rig.PresentedFile) error {
    h.ui.OpenFiles(files) // 每个 PresentedFile 有 Path 与 Description
    return nil
}
```

`hosttools.Options.Interactive=false` 时 `ask_user` 不调 `Asker`，直接返回
"no interactive UI"——无头运行/评测/批处理应当用这个。

### 6.5 MCP 与 LSP

```go
// MCP：stdio 或 Streamable HTTP
client, err := mcp.Start(ctx, mcp.Config{Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", root}})
if err != nil {
    return err
}
defer client.Close()

rt, err := full.NewRuntime(full.AllOptions{
    Policy: policy, Root: root,
    MCPClients: []*mcp.Client{client}, // 工具名：mcp__<server>__<tool>
})

// LSP：静态工具（无 client 时不出现）
lspClient, err := lsp.Start(ctx, lsp.Config{Command: "gopls"})
if err != nil {
    return err
}
defer lspClient.Close()
if err := lspClient.Initialize(ctx); err != nil {
    return err
}
// full.AllOptions{LSP: lspClient}: lsp_hover/lsp_definition/lsp_references/
// lsp_implementation/lsp_diagnostics/lsp_symbols
```

MCP 工具是**动态**的（`mcp__<server>__<tool>`），因此它们不进 `catalog.Static`；账本
把它们记在 `catalog.ServerBound`（7 个：`spill_read` + 5 个 MCP 面 + 旧 `lsp`）。
LSP 的路径校验可以用 `LSPPathPolicy` 收窄（nil 时从 `Policy.CheckPath` 推导）。

### 6.6 持久化

| 目标 | 配置 |
| --- | --- |
| 关窗口还能续（NDJSON） | `kit.Config{SessionStoreDir: "/var/lib/app/sessions"}`。同一目录**单实例**写 |
| 自己持有 store 实例（侧栏需要 `List`/`Load`） | `kit.Config{SessionStore: st}` 或之后 `a.SessionStore()` |
| 完全自定义历史 | `kit.Config{History: h}`（三者互斥） |
| 崩溃恢复 | `CheckpointStore`（session/sqlite 的 `Checkpoints()`，子模块**未发布**） |
| 超大工具结果 | `Spill: &rig.SpillPolicy{...}` |
| 收尾数据库 | `Storage: st`（`Shutdown` 最后关它；`sqlite.Close` 先 WAL checkpoint，子模块**未发布**） |

`History` / `SessionStoreDir` / `SessionStore` 三者互斥，同时设置会得到明确的配置
错误。`Shutdown` 的顺序是：取消在飞回合 → 等它们退出 → 关后台任务 → 排空事件 →
关存储；第 2 步等不到回合退出时**不关存储**（那正是"回答到一半消失"的来源）。细节
见 [docs/sqlite.md](sqlite.md)。

### 6.7 事件与成本

```go
// NDJSON 事件流：一个 Run 一条流，seq 在同一 writer 上连续
f, _ := os.Create("events.ndjson")
a, _ := kit.New(kit.Config{Model: model, EventWriter: f})

// 事后按版本化格式重放（读回同一个文件）
rf, _ := os.Open("events.ndjson")
if err := events.Replay(rf, func(env events.Envelope) error { return nil }); err != nil { … }

// 成本：Meter + 花费上限
meter := costmeter.New(costmeter.WithBudget(5), costmeter.WithWarnAt(0.8))
a, _ = kit.New(kit.Config{Model: model, Meter: meter, MaxRunCostUSD: 5})
summary := meter.Session() // costmeter.Summary：Requests/PromptTokens/CostUSD/Estimated/…
```

`kit.Event`（进程内事件，`RunRequest.Emit` / `RunText` 的 `emit` 回调）与
`events.Envelope`（NDJSON 线格式）不是一回事，别混用：前者是 agent 循环的观察点，
后者是给日志/回放用的版本化格式。

花费展示有一条硬规则：`cost_usd` **永远是估算**；`Estimated` 为真可能来自"表内价是
估算值"或"有未定价模型"，判断"是否漏算"只看 `UnpricedModels` 是否为空。持久化用
`Snapshot.CostNanoUSD`（整数纳美元），不要存 `cost_usd` 浮点。

### 6.8 OS 沙箱 runner（opt-in）

```go
if osrunner.MaybeRunHelper(os.Args) { // 必须在 main 最开始调用一次
    return
}
runner, err := osrunner.New(osrunner.Config{})
if err != nil {
    // 没有可用后端：不报错也能开应用，把原因交给 Preflight
    cfg.SandboxUnavailableReason = err.Error()
}
cfg.SandboxRunner = runner // nil 时自动跳过（Config.Sandbox=true 也不会凭空多出围栏）
```

`Config.Sandbox` 是三态意愿开关，`SandboxRunner` 才是实际能力；`Sandbox=false` 与
非 nil 的 `SandboxRunner` 同时设置会**报错**（自相矛盾的配置）。平台矩阵、fail-closed
语义与限制见 [docs/SANDBOX-OS.md](SANDBOX-OS.md)。

## 7. AI 工作规则（在本仓库改代码时）

### 7.1 加/改/删一个工具：少一处 CI 就红

1. 实现包（`tools/<name>`，带中文 `doc.go`）；
2. `tools/full` 接线（`AllOptions` 加依赖字段，`full.All`/`NewRuntime` 里构造）；
3. `tools/catalog.Static` 账本加名字（以及 `Legacy`/`Modern`/`ServerBound` 的归类）；
4. `tools/presets` 的名字表（需要进预设时）；
5. 测试（`tools/full` 的 `TestToolCatalogCoverage`、`tools/audit`、包内单测）；
6. `go run ./cmd/rigdoc` 重新生成 `docs/ai/tools.json`、`docs/ai/tools.md`
   （发布前可用 `-check` 自查漂移，CI 不查文档）；
7. `docs/TOOLS.md` / `docs/COVERAGE.md` 与 `CHANGELOG.md`。

删除工具时同一份清单反向执行；已删除的 `report`、`settings_get`、`settings_set`、
`present_artifact` **不要**加回来。

### 7.2 代码规则

- **模型可见的 `Description`/`Schema`/错误文案不翻译**：它们是接口，改动要更新快照与
  测试。
- **`CGO_ENABLED=0`**；唯一依赖 `mvdan.cc/sh/v3`；**根包 `rig` 必须 stdlib-only**，
  且不 import 本仓库任何子包。
- 叶子能力包不得 import `kit` 或 `tools/full`；只有 `kit` 同时装配 provider 与工具面。
- 注释与包文档中文；`go doc` 应能直接读。
- 权限模型与 OS 沙箱的边界见 §4：核心策略层保持进程内围栏，OS 沙箱只能是 opt-in 的
  `sandbox/osrunner`。
- 已整包删除、不要加回：`tools/escalate`、`mcpserver` + `cmd/rigmcp`、
  `internal/filewatch`、`sandbox/escalate`（`internal/rigdoc` 的
  `TestRemovedPackagesAreGone` 会拦）。

### 7.3 验证命令

```sh
export PATH=$PATH:/usr/local/go/bin
export CGO_ENABLED=0

gofmt -l $(git ls-files '*.go')      # 必须无输出
go build ./...
go vet ./...
go test ./...                        # 发布前再加 -race
go test ./tools/full -run TestToolCatalogCoverage -v   # 账本口径
make ci                              # 本地代码门禁（CI 同款）
```

文档与版本检查不进 CI，按需单独跑：`go run ./cmd/rigdoc -check`（docs/ai）、
`make quickstart`（README 与示例）、`make version`（发布）。

## 8. 工具选择速查

按"你要做什么"选工具——全部 84 条的精确 Schema 见
[docs/ai/tools.json](ai/tools.json)，账本口径与权限影响见 [docs/TOOLS.md](TOOLS.md)。

| 你要做什么 | 用哪个 | 备注 |
| --- | --- | --- |
| 读一个文件 | `read_file` | 大文件用 `offset`/`limit`；图片用 `read_image` |
| 新建或整体重写文件 | `write_file` | 覆盖写；需要先读一遍 |
| 改几行 | `edit_file` / `str_replace_editor` | 精确字符串替换；冲突时重读再改 |
| 跨文件/批量改动 | `apply_patch` | 支持 unified diff 与 Codex patch；原子应用 |
| 看目录 / 找文件名 | `list_dir` / `glob` | `glob` 按模式匹配路径 |
| 搜内容 | `grep` | 正则、按文件过滤；结构化检索用 `search`（文件名模糊 + BM25 + `@提及`） |
| 跑一条命令 | `bash`（Windows: `pwsh`） | 非零退出是结果不是错误（带 `[exit code: N]`） |
| 跑长命令 / 轮询 | `job_start` → `job_output`/`job_wait` → `job_kill`；`job_list` | 与 `bash` 的 `run_in_background` 共享同一个 `jobs.Manager` |
| 需要交互式 shell / 常驻会话 | `terminal_open` / `terminal_send` / `terminal_read` / `terminal_signal` / `terminal_close` | Unix PTY；Windows ConPTY，不可用时回退 pipe |
| 抓网页 / 搜索 | `web_fetch` / `web_search` | `web_fetch` 需 `AllowNetwork`；`web_search` 需宿主注入 `Searcher` |
| 记长期笔记 | `memory` | 常驻（初始 active）；先 `index` 再 `read` |
| 列可用技能 / 读技能 | `skill` | 常驻；从 `SKILL.md` 仓库加载 |
| 找工具（按需加载） | `tool_search` | 常驻入口；`{}` 列表 / `query` 筛选 / `activate` 加载 |
| 委派子任务 | `spawn_teammate` → `wait_agent`（旧名 `team_wait`/`team_last`）；同步版 `subagent` | **异步**：完成通知会注入下一步 |
| 多 agent 协作 | `list_agents`、`send_message`、`interrupt_agent`、`team_task_*` | 共享任务板 + 消息 |
| 声明式流程 / 迭代 | `workflow` / `ralph` | `workflow` 是 Go 原生步骤，无 JS 引擎 |
| 回读溢出结果 | `spill_read` | server-bound：需宿主注入 `SpillStore` |
| 申请可写授权 / 查沙箱 | `request_sandbox_access` / `sandbox_status` | 默认注册但不 active；授权需 Gate 批准 |
| 计划 / 提问 / 交付 | `plan_review`/`exit_plan_mode`、`ask_user`、`present` | 分别需要 `PlanController`、`Asker`、`FilePresenter` |
| 写代码跑 Python/Node | `run_code` | 只在 `kit.Config.PTC=true` 时存在 |

被拒绝时先看错误里的规则 id 与理由：那是 `gate` 给模型的可执行反馈，**不要原样重试**
同一条调用；需要放宽权限就说明需求、由宿主调整 `Mode`/`Approval`，或用
`sandbox_permissions: "require_escalated"` + `justification` 请求人工批准（§4.5）。

## 9. 相关文档

- [docs/GETTING-STARTED.md](GETTING-STARTED.md) —— 安装、最小示例、常见任务与排障
- [docs/CONFIG.md](CONFIG.md) —— `kit.Config` 全字段、默认值与优先级
- [docs/TOOLS.md](TOOLS.md) —— 工具目录（按类别、口径、权限/平台影响）
- [docs/COVERAGE.md](COVERAGE.md) —— 账本、覆盖门禁与新增工具清单
- [docs/ARCHITECTURE.md](ARCHITECTURE.md) —— 分层、请求流、扩展点与设计边界
- [docs/SANDBOX-OS.md](SANDBOX-OS.md) —— opt-in OS runner 的平台矩阵与限制
- [docs/MEMORY.md](MEMORY.md) —— 记忆 wiki 与溢出结果
- [docs/sqlite.md](sqlite.md) —— 会话持久化三形态与 `storage` 聚合契约
- [docs/ai/tools.json](ai/tools.json) / [docs/ai/tools.md](ai/tools.md) —— 机器可读 / 人读的工具目录
- [AGENTS.md](../AGENTS.md) —— 本仓库 AI 工作规则与逐模块说明