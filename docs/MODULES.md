# 模块清单（ZeoKeel Rig）

本 SDK 的定位是**模块化 AI SDK，给个人 agent 产品使用**。这里的"模块化"有一条硬性
定义，并且被 `internal/rigdoc/modularity_test.go` 强制：

> **每个能力包都能被单独拿走使用**——它们都不 import 聚合层（`kit`）或工具装配层
> （`tools/full`）。你想只用 `tools/patch` 或只用 `provider/ollama`，就 `go get` 后
> 直接 import，不会被迫拖进整棵依赖树。

三条被测试守住的约束：

| 约束 | 为什么 | 守门测试 |
|---|---|---|
| 叶子能力包不得 import `kit` / `tools/full` | 否则"只用一项"就变成"整套都要" | `TestLeafCapabilitiesDoNotImportAggregators` |
| 根包 `rig` 只依赖标准库，且不 import 本仓子包 | 它是所有宿主与模块共享的最小契约层 | `TestRootPackageStaysStdlibOnly` |
| 只有 `kit` 同时装配 provider 与工具面 | 新增 provider/工具时只改一处 | `TestKitIsTheOnlyAggregator` |

---

## 一、两种使用方式

**A. 一行接入（推荐给应用）**

```go
a, err := kit.New(kit.Config{
    Model:   "gpt-4o",
    Root:    ".",
    Profile: kit.ProfilePersonal, // careful / personal / local
})
```

`kit` 是唯一知道"怎么把所有模块拼起来"的包；它负责默认值、装配、以及把各个可选
能力接到 agent 上。

**B. 按需组装（推荐给要控制的宿主）**

```go
policy := sandbox.New(sandbox.WithRoots("."))
tools  := []rig.Tool{
    builtin.ReadFile(policy),
    patch.New(".", patch.WithPolicy(policy)),
    search.New(search.Options{Root: "."}),
}
a := agent.New(agent.AgentSpec{Provider: provider, Tools: tools, Gate: gate.PersonalAuto()})
```

> ⚠️ `full.All(full.AllOptions{})`（不传 `Root`/`Policy`）只会构造 77 个工具，
> `apply_patch`/`search`/`undo` 不存在、文件工具全部返回 `no roots configured`——
> **必须传 `Root` 或带 `Roots` 的 `Policy`**。

---

## 二、模块清单

"独立可用"= 该包可以脱离 `kit` 直接使用（已由测试保证）。

| 模块 | 一句话 | 独立可用 |
|---|---|---|
| `rig`（根包） | 契约层：`Provider`/`Tool`/`Gate`/`Message`/`Usage` 等类型，仅依赖标准库 | — |
| `agent` | 主循环：模型↔工具轮数、压缩、抽风守卫、历史修复 | ✅ |
| `kit` | 聚合层：默认值、装配、各可选能力的接线 | 它是聚合层 |
| `sandbox` | 策略层：可写根、保护路径、deny glob、命令与网络开关 | ✅ |
| `sandbox/osrunner` | OS 沙箱（bwrap/Landlock/Seatbelt/Windows ACL），**opt-in** | ✅ |
| `gate` | 审批门禁：`PersonalAsk/Auto/Never` 三档、命令风险判定、拒绝理由回模型 | ✅ |
| `tools/builtin` | 文件工具（read/write/edit/list/glob/grep/read_image） | ✅ |
| `tools/patch` | `apply_patch`：一次原子应用多文件补丁 + `dry_run` diff 预览 | ✅ |
| `tools/search` | 模糊文件名 + BM25 内容 + `@提及` | ✅ |
| `tools/undo` | 面向用户的"撤销按钮"（git 隐藏快照） | ✅ |
| `tools/jobs` / `tools/terminal` | 后台进程 / 有状态 shell 会话 | ✅ |
| `tools/mcp` / `tools/lsp` | MCP **客户端** / LSP 客户端 | ✅ |
| `tools/ptc` | `run_code`（调用系统已装的 Python/Node） | ✅ |
| `tools/skill` / `tools/memory` / `tools/subagent` / `tools/worktree` | 技能 / 记忆 / 同步委派 / git worktree | ✅ |
| `tools/catalog` / `tools/presets` | 工具账本 / 工具集预设（minimal·personal·standard·all） | ✅ |
| `provider/openai` | 任意 OpenAI chat-completions 兼容端点 | ✅ |
| `provider/local` | "OpenAI 兼容但不完全一样"的共享基座（`Quirks` + 共享重试/SSE） | ✅ |
| `provider/ollama` / `provider/lmstudio` | 本地模型（默认无鉴权、容忍缺失 `[DONE]`、模型列表） | ✅ |
| `events` | 版本化事件流（NDJSON），供 UI/日志/回放 | ✅ |
| `costmeter` + `modelprice` | 会话级花费计量（纳美元整数）与价目表 | ✅ |
| `instructions` | AGENTS.md 层级 + `@import` + 环境块进系统提示词 | ✅ |
| `secrets` | **仅导出用**的凭据清洗（运行时默认不脱敏） | ✅ |
| `session` / `session/store` / `session/compact` | 内存会话 / 落盘会话（一会话一 NDJSON）/ 上下文压缩 | ✅ |
| `internal/fsguard` | 受限于一棵目录树的安全文件操作（`os.Root` 之上） | 仅同模块 |
| `internal/gitsnap` | 基于 git 的回合快照 | 仅同模块 |
| `internal/diffpreview` | 统一 diff 预览（纯函数、无 IO） | 仅同模块 |

> `internal/*` 只能被本模块 import。如果宿主的 UI 是**独立模块**，请不要依赖它们
> 的数据类型——用 `kit`/`events`/`costmeter` 暴露的公开类型（例如
> `costmeter.Summary` 的 `cost_usd`）。

---

## 三、按场景挑选模块

| 你要做的东西 | 需要的模块 | 不需要 |
|---|---|---|
| 纯对话客户端（无工具） | `rig` + `agent` + 一个 `provider` | 其余全部 |
| 编码 agent（读/改/跑） | `sandbox` + `tools/builtin` + `tools/patch` + `tools/search` + `tools/jobs` + `gate` | `provider/ollama`、`events`、`costmeter` |
| 带 UI 的桌面客户端 | 上面 + `events`（流式渲染）+ `costmeter`（花费显示）+ `session/store`（会话列表/续传） | — |
| 只想要 diff 预览 | `internal/diffpreview` 是 internal 的；宿主用 `gate.DiffPrompt` 或 `tools/patch` 的 `dry_run` | 其余全部 |
| 只想接本地模型 | `provider/ollama` 或 `provider/lmstudio`（`kit.ProviderKind` + `kit.AvailableModels` 可做到"启动即用"） | 云 provider |

---

## 四、动态工具加载与缓存

工具多了以后，每个工具的 JSON Schema 都会进入**每一次**请求的 `tools` 数组：既花钱，
又稀释模型的注意力。动态工具加载（lazy tool discovery）的做法是——默认只发常用的
那一小撮，其余工具**注册但不 active**，模型需要时自己把它们加载进来。

### Pi（pi.dev）的分层，以及我们抄了什么

Pi 把这件事拆成两层，**runtime/provider 负责缓存正确性，扩展只负责"发现 + 激活
策略"**。它的关键机制是：

1. 初始只激活一个 `lazy` 网关 + 基础工具 + `alwaysActive` 集；
2. 其余工具"已注册但 inactive"，通过 `lazy({})`（列状态）/`lazy({search})` /
   `lazy({activate})` 发现与激活；
3. **激活是纯增量**：`setActiveTools(current + additions)`，**从不移除**——移除会
   重建 system prompt 并使 prefix cache 失效；
4. 有 native deferred loading 的模型把新增工具定义放在**工具结果位置**加载；没有的
   provider 只能重发 active tools，因此**不承诺激活那一轮的完整 cache 命中**；
5. 激活状态写进会话 branch（resume/fork/compaction 后恢复）；
6. 带 prompt metadata 的工具可能触发 system prompt rebuild（缓存杀手）；
7. active 集没变时标 `no-op`，不重复调用。

我们抄的是 **1/2/3/5/7**，并且把第 4 条的"诚实代价"写清楚（见下）。

### 我们的实现

- **初始 active 集** = 核心 13（`read_file write_file edit_file apply_patch list_dir
  glob grep search bash todo_write working_directory read_image str_replace_editor`）
  + 三个常驻能力：`tool_search`（发现入口）、`memory`（记忆默认开）、`skill`（技能
  清单，schema 只有 808 字节，而"有哪些技能"是开工前就该知道的）——只取**实际注册**
  的那些；零配置下就是 **16 个**；
- `present` **不再常驻**：它需要宿主的 `rig.FilePresenter`，所以由依赖门控——
  `kit.Config.Presenter` 为 nil 时被零配置体检摘掉（留着一个必然报错的交付工具只
  会诱导模型重试），接上之后保留**并立刻进初始 active 集**（宿主付了配置成本）；

- 其余工具仍然进入注册表（`full.All` 账本不变），但既不出现在请求里，被模型调用时
  会得到 `rig: tool "x" is registered but not active in this session; activate it
  with tool_search first`（不执行、不过审批门禁）；
- 模型用 `tool_search`：
  - `{}` → 列出未激活工具（默认最多 40 条，超出提示"用 query 缩小范围"）；
  - `{"query":"pdf"}` → 按名字/描述筛选；
  - `{"activate":["web_fetch"]}` → 激活，返回 `activated` / `unknown`；
- 激活是**纯增量**且**按会话**：`ActivateTools(sessionID, names...)` 只加不减，
  请求里的 `tools` 按**注册顺序**过滤，因此新工具只在数组**末尾**出现；
- **system prompt 里没有工具描述**（`kit/wiring.go` 的 `assembleSystemPrompt` 只拼
  base + environment + AGENTS.md），所以激活**不会**重建 prompt——这是我们的天然
  优势，Pi 的第 6 条风险在我们这里不存在；
- 会话恢复：dynamic 状态随 agent 内存里的按会话集合存在；跨进程 resume 时回到初始
  集合（已激活的工具会在下一次 `tool_search` 里重新出现）——这是有意的简化，因为
  把激活集合写进历史会污染 prefix。

### 诚实的代价（要写进文档，而不是藏起来）

- **激活那一轮 prefix cache 不命中**：`tools` 数组变了，缓存前缀必须重算。之后恢复
  稳定（已激活部分逐字节不变）——不激活就不会有这次重算。
- 初始不 active 的工具**多一次往返**：模型要先 `tool_search` 再调用。对"偶尔用一次"
  的工具（web、jobs、terminal、mcp）这笔账是划算的；对高频工具不划算，所以要放进
  核心 14。
- 我们**不做** native deferred loading：所有 provider 都会在激活后重发完整 active
  tools 数组。要做需要 provider 侧支持"在工具结果位置带工具定义"，超出当前范围。

### 自定义工具面与渐进式装配

宿主要么微调默认档，要么完全自己装配。三种典型写法：

```go
// A. 只要自己的初始集（其余照旧 discoverable；tool_search/memory/skill 强制并入）
a, _ := kit.New(kit.Config{
    Model: m, Provider: p, Root: ".",
    InitialActiveTools: []string{"read_file", "bash", "memory", "tool_search", "skill", "my_lint"},
})

// B. 关掉发现、全量 active（v0.20 行为）
off := false
a, _ := kit.New(kit.Config{Model: m, Provider: p, Root: ".", ToolDiscovery: &off})

// C. 完全自己装配（不用 kit）
tools := append(full.All(full.AllOptions{Root: "."}), progressive.New(...)...)
a, _ := agent.New(agent.AgentSpec{
    Model: m, Provider: p, Tools: tools,
    InitialActiveTools: []string{"read_file", "bash", "memory", "tool_search", "skill", "my_lint"},
})
```

注意 B 里 `ToolDiscovery: &off`（`kit.Bool(false)`）——`Config.ToolDiscovery` 用
`*bool` 是因为它的零值必须表示"开启"。另外 `Config.Tools`（自建工具集）**默认不开
发现**：宿主自己挑出来的工具就该能用；要在自建集上也开发现，显式 `kit.Bool(true)`。

### 三条缓存契约（宿主自查清单）

1. **不激活时 `Request.Tools` 逐字节稳定**——连续多轮完全相同（`internal/rigdoc`
   的 `TestActivationAppendOnlyInvariant` 与 `kit/lazy_tools_test.go` 双重复验）。
2. **激活只追加**——新工具出现在数组**末尾**，旧数组是新的前缀。这条是无条件的：
   每个会话维护**有序 active 列表**，激活 = append（而不是"按注册顺序重新过滤"，
   后者会把晚激活的、注册更早的工具插到中间，让前缀失效）。
3. **激活不重建 system prompt**——因为本 SDK 的 system prompt **不含工具描述**
   （`kit` 的 `assembleSystemPrompt` 只拼 base + environment + AGENTS.md）。

> ⚠️ 第 3 条是**你**的责任：如果你在自己的宿主里往 system prompt 里塞工具描述
> （例如为了"让模型更懂工具"），那么激活工具时你必须重建 prompt——那一刻 1、2 两条
> 保住的缓存全部失效。要么别塞，要么把工具描述放在 `tools` 数组里（我们就是这样）。

### 怎么关

```go
off := false
kit.New(kit.Config{Model: "m", ToolDiscovery: &off})  // 回到 v0.20：全部工具一开始就 active
```

`kit.Config.InitialActiveTools` 可以自定义初始集合（`tool_search` 与 `memory` 会被
强制并入）。宿主侧还可以直接调 `agent.ActivateTools(sessionID, names...)`——触发者
在事件流里记为 `host`（模型自助激活记为 `model`）。

---

## 五、默认值（`kit.New`）

`kit.New(Config{})` 零配置**注册** 37 个工具，其中 **16** 个初始 active（核心 13 + `tool_search` + `memory` + `skill`，其余由模型用 `tool_search` 按需加载——见第四节），且**每一个都真能用**：缺 `Searcher`
就没有 `web_search`、缺 `SessionLog` 就没有 `session_*`、未联网就没有 `web_fetch`、
非 Windows 就没有 `pwsh`。宿主每接一项依赖按项加回（全叠加 50）。

默认值取舍都写在 `kit/doc.go` 与 `docs/zh/AI-SDK.md` §2.5 里，这里只列三个最容易
被误解的：

- `Approval: "auto"` **不等于** `AllowAll`——危险命令仍然被拒；
- 保护路径**默认开启**（`.git`、`~/.ssh` 等），它是**防误伤不是沙箱**；
- 输出脱敏**默认关闭**——数据是用户自己的；只有 `secrets.RedactForExport` 用于
  "用户主动导出日志"。