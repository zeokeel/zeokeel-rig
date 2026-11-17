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

## 四、默认值（`kit.New`）

`kit.New(Config{})` 零配置暴露 **37** 个工具，且**每一个都真能用**：缺 `Searcher`
就没有 `web_search`、缺 `SessionLog` 就没有 `session_*`、未联网就没有 `web_fetch`、
非 Windows 就没有 `pwsh`。宿主每接一项依赖按项加回（全叠加 49）。

默认值取舍都写在 `kit/doc.go` 与 `docs/zh/AI-SDK.md` §2.5 里，这里只列三个最容易
被误解的：

- `Approval: "auto"` **不等于** `AllowAll`——危险命令仍然被拒；
- 保护路径**默认开启**（`.git`、`~/.ssh` 等），它是**防误伤不是沙箱**；
- 输出脱敏**默认关闭**——数据是用户自己的；只有 `secrets.RedactForExport` 用于
  "用户主动导出日志"。