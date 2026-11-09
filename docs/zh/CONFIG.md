# 配置约定

ZeoKeel Rig 是**库**：它不读配置文件、不保存全局设置，也不看 flag 或配置目录。宿主应用从自己的来源（环境变量、flag、YAML、数据库）加载配置，并把普通 Go 值传给构造函数。本文说明 SDK 使用的三种配置风格、默认值与优先级规则。

推荐的起点是单 import 门面：

```go
a, err := kit.New(kit.Config{Model: "gpt-4o-mini"})
```

`kit.New` 接好 provider、完整工具集、安全审批 gate、内存历史与 agent 循环，因此只需要“一个完整 agent”的宿主不必自己组装各部分。需要自定义组装的宿主仍可使用手工包。

## 三种风格

| 风格 | 形态 | 用途 | 示例 |
| --- | --- | --- | --- |
| **Config struct + 构造函数** | `New(Config) (*T, error)` | 选项在构造时一次性解析；命名清晰的 struct 比长选项列表更容易文档化与传递 | `openai.New(openai.Config{...})`、`full.All(full.AllOptions{...})`、`kit.New(kit.Config{...})`、`builtin.NewPwshWithConfig(builtin.PwshConfig{...})`、`inspect.New(inspect.Options{...})`、`report.New(report.Options{...})` |
| **函数式选项** | `New(opts ...Option)` + `WithX(...)` | 不可变策略/管理器对象，有文档化默认值且选项集会增长；新增选项不会破坏调用方 | `sandbox.New(sandbox.WithRoots(...), ...)`、`web.NewHTTPFetcher(web.WithPolicy(...), ...)`、`terminal.NewManager(terminal.WithPolicy(...), ...)`、`jobs.NewManager(jobs.WithPolicy(...), ...)`、`lsp`/`mcp` 客户端 builder |
| **静态 spec + 每调用 request** | `New(Spec)` 然后 `Run(ctx, Request)` | 行为在每次调用时不同的长生命周期运行时对象 | `agent.New(rig.AgentSpec{...})` + `agent.Run(ctx, rig.RunRequest{...})`、`agent.NewRunner(agent.RunnerOptions{...})`、`rig.SubagentSpec`、`kit.Agent.Run` / `kit.Agent.RunText` |

经验法则：

- 如果值在构造后**不可变**且配置项稳定，使用 config struct。
- 如果值**不可变但配置项会增长**（策略、管理器），使用函数式选项，使新增选项是增量的。
- 如果配置项是**每次操作**的（model 覆盖、迭代上限、审批 gate），放在 request 上，而不是对象上。

## 所有风格共享的约定

1. **构造函数做校验并返回 error。** `openai.New` 拒绝非法 base URL；`agent.New` 拒绝 nil provider 或空 model（wrap `rig.ErrProviderRequired` / `rig.ErrModelRequired`）以及重复的工具名；`kit.New` 返回 error 而不是 panic。
2. **选项不会失败；它们回退到文档化默认值。** 给 `WithX` 选项传零/负 duration 或 size 会保留默认值（见下面的表格），这让选项可组合。
3. **零值有意义且有文档。** `openai.Config{}` 可用，`full.AllOptions{}` 可用，`sandbox.New()` 刻意拒绝一切。安全相关的零值 fail closed。
4. **配置在构造后不可变。** `*sandbox.Policy`、`*openai.Provider`、`*terminal.Manager` 与 `*jobs.Manager` 可安全并发使用；把 `Policy` 交给工具后不要修改它。
5. **增量演进。** 新字段追加，零值保持旧行为；选项只新增、不改变用途；字段和选项只通过[弃用策略](VERSIONING.md#弃用策略)移除。
6. **同名同义。** 多处出现的设置（`Model`、`MaxIterations`、`Gate`）含义一致；更具体的层优先（见[优先级](#优先级)）。
7. **没有隐藏全局状态。** 构造函数不读文件或包级可变变量。SDK 自身只读下面[环境变量](#环境变量)列出的变量。

## 优先级

设置按从具体到通用解析：

| 设置 | 优先级（高者在前） |
| --- | --- |
| Model | `rig.RunRequest.Model` → `rig.AgentSpec.Model`（agent 总是发送非空 model）→ `openai.Config.DefaultModel`（仅在直接调用 `Provider.Stream` 且 `Request.Model` 为空时） |
| Max iterations | `rig.RunRequest.MaxIterations`（> 0）→ `rig.AgentSpec.MaxIterations`（> 0）→ `agent.DefaultMaxIterations` = 20 |
| 审批 gate | `rig.RunRequest.Gate`（非 nil）→ `rig.AgentSpec.Gate` → nil，表示*全部放行* |
| History / session | `rig.RunRequest.SessionID`（空 = 全新历史，不持久化）→ `rig.AgentSpec.History`（nil = 不持久化） |
| Temperature / TopP / MaxTokens | 仅在 `rig.AgentSpec`（没有 per-run 采样字段） |
| Sandbox 策略 | 传给工具构造函数的 `*sandbox.Policy`；没有 policy 全局 |
| Provider | `kit.Config.Provider` 覆盖 → 由 kit 字段构造的 `openai.New(openai.Config{...})` |

`kit.Agent`（门面）委托给由 `kit.Config` 构造的普通 `agent.Agent`，因此同样的优先级适用；`kit.Config` 中未设置的字段回退到下面的门面默认值。

## 默认值参考

### `openai.Config`（provider）

`openai.New(openai.Config{})` 是合法的。零值含义：

| 字段 | 零值行为 | 默认 |
| --- | --- | --- |
| `BaseURL` | 使用 SDK 默认端点 | `https://api.openai.com/v1`（`openai.DefaultBaseURL`）；行尾斜杠被去掉；scheme 必须是 `http`/`https` |
| `APIKey` | 从环境读 `APIKeyEnv`（或 `OPENAI_API_KEY`） | 无——解析出的 key 为空时不发送 `Authorization` 头（本地服务） |
| `APIKeyEnv` | 读 `OPENAI_API_KEY` | `openai.DefaultAPIKeyEnv` = `OPENAI_API_KEY` |
| `DefaultModel` | `Stream` 要求 `Request.Model` | 空；两者都未设置时 `Stream` 返回 wrap `rig.ErrModelRequired` 的 error |
| `Headers` | 不额外加头 | nil |
| `HTTPClient` | SDK 客户端，60s response-header 超时；request context 约束流式 body | nil |
| `MaxRetries` | 使用 SDK 默认 | `openai.DefaultMaxRetries` = 2；负值禁用重试 |
| `RetryBackoff` | 使用 SDK 默认 | `openai.DefaultRetryBackoff` = 500ms，每次翻倍；负值 = 立即重试 |
| `IncludeUsage` | 始终开启（兼容字段） | Rig 总是发送 `stream_options.include_usage` |
| `DefaultReasoningEffort` | 不发送 `reasoning_effort` | 空；归一化为 `low`/`medium`/`high`，非法值在 `New` 报错；`Request.Extra` 可覆盖 |
| `RequireCompleteStream` | 无 `finish_reason`/`[DONE]` 的干净 EOF 视为正常结束 | `false`；开启后截断流返回 `openai.ErrIncompleteStream`（同时 wrap `io.ErrUnexpectedEOF`） |

可重试失败是 HTTP 408/425/429/5xx 与传输错误，会遵循 `Retry-After`（延迟上限 30s）。流建立之后的失败不重试。

### `sandbox.New()`（策略）

`sandbox.New()` 不带选项时**什么都不允许**——没有文件访问、没有 shell、没有网络。选项（全部可选，按顺序应用）：

| 选项 | 零值/默认行为 | 说明 |
| --- | --- | --- |
| `WithRoots(roots...)` | 无 root：每个路径都被拒绝 | 第一个 root 是相对路径的主 root；无参数移除访问；打不开的 root 被丢弃（fail closed） |
| `WithReadOnly(bool)` | `false`（若 root 与工具允许则可写） | `true` 同时禁用 shell 命令，因为 shell 可以写 |
| `WithMode(sandbox.Mode)` | `ModeWorkspaceWrite` | `read-only` 拒绝文件变更与 shell 执行；`workspace-write` 把写限制在 roots、命令限制在 allowlist；`danger-full-access` 移除 root/allowlist 围栏（显式 denylist 仍生效）。未知 mode fail closed |
| `WithCommandAllowlist(commands...)` | 无 allowlist：**shell 执行禁用**（在 `danger-full-access` 下被忽略） | 按 `argv[0]` 的 basename 匹配；`/usr/bin/git` 匹配 `"git"`；Windows 上同时忽略大小写与 `.exe` 后缀（`"pwsh"` 与 `"pwsh.exe"` 等价） |
| `WithCommandDenylist(commands...)` | 空 | 匹配方式同 allowlist（Windows 忽略大小写与 `.exe`）；deny 优先于 allow |
| `WithEnvAllowlist(names...)` | `PATH`、`HOME`、`LANG` | 不带名字调用时命令以空环境运行 |
| `WithNetwork(bool)` | `false` | `true` 仍只允许公网 HTTP(S)；loopback/private/link-local/metadata 被拒绝 |
| `WithMaxOutputBytes(int)` | `sandbox.DefaultMaxOutputBytes` = 32 KiB | 非正值保留默认 |
| `WithTimeout(duration)` | `sandbox.DefaultTimeout` = 30s | 非正值保留默认；per-call 超时不会超过它 |
| `WithDeniedGlobs(patterns...)` | 无 | 始终拒绝的 glob，例如 `**/.env`、`**/*.pem` |
| `WithAllowShellOperators()`（v0.9） | 设置 allowlist 时操作符被拒绝 | opt-in、弱边界；仅 allowlist 命令的管道/重定向需要；请与审批 gate 组合 |

policy 还强制 `..`/NUL 拒绝、符号链接逃逸检查与 `os.Root` I/O 约束。它是**进程内策略，不是 OS 沙箱**；见 [SECURITY.md](../../SECURITY.md)。

### `agent.New` / `rig.AgentSpec`

| 字段 | 零值行为 | 默认/说明 |
| --- | --- | --- |
| `Provider` | **必需** | `agent.New` 返回 wrap `rig.ErrProviderRequired` 的 error |
| `Model` | **必需** | 为空时返回 wrap `rig.ErrModelRequired` 的 error |
| `SystemPrompt` | 无 system 消息 | 被 trim；每次请求前置，从不持久化 |
| `MaxIterations` | 使用默认 | `agent.DefaultMaxIterations` = 20 |
| `Tools` | 无工具 | nil/空表示模型不能调用任何东西 |
| `Gate` | 全部放行 | 对不可信模型危险；见下文 `gate` |
| `History` | 不持久化 | nil；此时 `RunRequest.SessionID` 与存储无关 |
| `Temperature`、`TopP` | 未设置 | nil 指针；provider/端点默认生效 |
| `MaxTokens` | 未设置 | 0 |
| `Logger` | 丢弃 | nil；只有 `Printf` 风格诊断 |

当 provider、tools 与 history 安全时，`agent.New` 可安全并发 `Run`；循环不保存可变状态。

### `rig.RunRequest`（每一轮）

| 字段 | 零值行为 |
| --- | --- |
| `SessionID` | `""` = 全新历史；本次 run 不 load 也不 persist |
| `Input` | nil = 续跑且不添加 user 消息 |
| `Model` | `""` = 使用 `AgentSpec` 的 model |
| `MaxIterations` | 0 = agent 默认（除非 spec 设置，否则为 20） |
| `Gate` | nil = 使用 agent 的 gate（若也为 nil：全部放行） |
| `Emit` | nil = 丢弃事件；事件在 run goroutine 上同步发出 |

### `full.AllOptions`（完整工具集）

`full.All(full.AllOptions{})` 构建每个静态工具。每个依赖都是可选的，并降级为文档化回退，而不是 nil panic：

| 选项 | 零值行为 |
| --- | --- |
| `Policy` | `sandbox.New(sandbox.WithRoots(opts.Root))`——带工作区 root（见注释）且无 shell/网络的策略 |
| `Root` | `""`；`working_directory`/inspect/report 报告的工作区 root，也用于默认 policy；注意空 root 会归一化为*无 root*（文件访问被拒绝）——请传绝对路径 |
| `Settings` | nil：`settings_get`/`settings_set` 返回可读的 “host did not configure a store” 工具错误 |
| `Host`（`hosttools.Options`） | nil 字段：对应工具（`ask_user`、`present`、`present_artifact`、`plan_review`、`exit_plan_mode`、`goal`/`get_goal`/`create_goal`/`update_goal`）返回可读的 “host did not configure” 工具错误；使用 `hosttools.NewMemoryHost()` 获得确定的内存行为（`FilePresenter` 支撑 DSH 风格 `present`，`GoalStore`/`GoalRevisionStore` 支撑 goal 工具） |
| `Memory` | nil：全新内存 store |
| `Scheduler` | nil：全新内存 scheduler |
| `SessionLog` | nil：`session_*`/inspect/report 工具降级为可读错误 |
| `Searcher` | nil：`web_search` 被调用时报告配置错误 |
| `LSP`、`LSPPathToURI` | nil：不构建 LSP 工具 |
| `LSPPathPolicy` | nil：若设置了 `Policy` 则从其推导（`Policy.CheckPath`），否则不受限 |
| `MCPClients` | nil：不构建 MCP 工具（`mcp__<server>__<tool>` 是动态的） |
| `MCPPrefix` | `mcp__` |
| `Runner` | nil：编排工具（`subagent`、`workflow`、`ralph`、team 工具）仍存在，但被调用时报告配置错误 |
| `Models` | nil/空：`list_subagent_models` 解释 subagent 继承父 model |
| `Skill` | nil：文件系统 store，根为 `$ZEOKEEL_SKILLS`、`<root>/.zeokeel/skills`、`~/.zeokeel/skills` |
| `TeamTasks` | nil：全新内存任务板 |
| `Reviewer` | nil：`code_review`/`auto_review` 报告配置错误 |

使用 `tools.NewRegistry(full.All(opts)...)` 得到面向模型的注册表；`NewRegistry` 在 nil/重名时 panic（编译期编程错误），而 `Registry.Register` 为运行时组装返回 error。

### `kit.Config`（推荐门面）

`kit.New(kit.Config{...})` 返回 `*kit.Agent`，接好 provider、tools、gate、history 与循环。所有字段可选；常见默认值：

| 字段 | 零值行为 | 默认 |
| --- | --- | --- |
| `Model` | **必需** | `kit.New` 在为空时返回 wrap `rig.ErrModelRequired` 的 error（即使提供了自定义 `Provider`，agent 仍需要默认 model） |
| `SystemPrompt` | 无 system 消息 | `""` |
| `BaseURL` | provider 默认 | `https://api.openai.com/v1`（设置 `Provider` 时忽略） |
| `APIKey` / `APIKeyEnv` | provider 行为 | `APIKeyEnv` = `OPENAI_API_KEY`（设置 `Provider` 时忽略） |
| `Provider` | 构建 OpenAI 兼容 provider | nil = 内置 `openai` provider |
| `Tools` | 构建完整工具集 | nil = `full.All(...)`；非 nil 空 slice 表示无工具 |
| `Gate` | 由 `Approval` 构建（非 nil 宿主 gate 总是优先） | `ApprovalAsk` → `gate.AllowTools("read_file", "list_dir", "glob", "grep")`；`ApprovalAuto` → `AllowAll`；`ApprovalNever` → `DenyAll` |
| `History` | 内存历史 | `session.NewMemory()` |
| `Root` | `kit.DefaultRoot` = `"."` | 当 `Policy` 为 nil 时，`kit.New` 校验 root 存在且是目录 |
| `Policy` | 由 Root/Mode/AllowNetwork/CommandAllowlist 构建 policy | nil = 构建的 policy；设置后 Mode、AllowNetwork 与 CommandAllowlist 被忽略，且 root 不由 kit 校验 |
| `Mode` | `sandbox.ModeWorkspaceWrite` | 权限围栏：`read-only` 拒绝文件变更与 shell 执行；`workspace-write` 把写限制在 roots、命令限制在 allowlist；`danger-full-access` 移除这些围栏（显式 denylist 仍生效）。未知 mode 被 `kit.New` 拒绝 |
| `Approval` | `ApprovalAsk`（fail-safe） | `ask` 只放行只读工具集、拒绝其余；`auto` 放行所有调用；`never` 拒绝所有调用。`Gate` 非 nil 时忽略 |
| `PTC` | 关闭 | `true` 追加 opt-in 的 `run_code` 工具（模型编写的 Python 3/Node.js 在系统运行时中执行；每个内层调用都走同一个 gate） |
| `AllowNetwork` | 网络禁用 | `false`（开启后也只允许公网 HTTP(S)） |
| `CommandAllowlist` | shell 禁用 | nil（空） |
| `MaxIterations` | 循环默认 | `kit.DefaultMaxIterations` = 20 |
| `ContextBudget` | 不自动压缩 | 估算 prompt token 预算；需同时设置 `Compactor` |
| `Compactor` | nil | `session/compact.WindowCompactor` 或 `LLMCompactor` |
| `Spill` | nil | `rig.SpillPolicy`；大工具结果落盘并以摘要替换 |
| `CheckpointStore` | nil | 可选中断恢复检查点 |
| `ToolPreset` | 不过滤 | `minimal`、`standard` 或 `all`（`tools/presets`） |
| `ReasoningEffort` | 空 | `low`/`medium`/`high`（仅默认 OpenAI 兼容 provider） |

门面是增量的（字段追加）并包装同一批契约类型（`kit.RunRequest`、`kit.RunResult`、`kit.Event`、`kit.Version` 是 `rig` 对应类型的别名），因此宿主可以从 `kit.New` 起步，之后降级到 `agent.New` 而不必重写工具接线。确切的校验信息与字段见包文档（`go doc github.com/zeokeel/zeokeel-rig/kit.Config`）。

### `gate` 包

gate 决定工具调用是否可以运行。nil（未配置 gate）表示**全部放行**，只适合完全可信的工具集。

| 构造函数 | 行为 |
| --- | --- |
| `gate.AllowAll()` | 放行一切 |
| `gate.DenyAll()` | 拒绝一切 |
| `gate.AllowTools(names...)` | 只放行指定名字的工具，其余拒绝 |
| `gate.Func(fn)` | 宿主回调（例如 UI 审批提示）；在 run goroutine 上调用 |
| `nil` | 等同于 `AllowAll` |

被拒绝的调用会作为工具错误（`DecisionDeny`）报告给模型；除非宿主的 gate 返回 error，否则 run 继续。`DecisionAllowAlways` 仅在本次 run 的剩余时间里对相同的（tool, arguments）调用跳过 gate。

### 会话历史

`session.NewMemory()` 是内存 `rig.History`；nil 表示不持久化。实现 `rig.History`（并发安全）可做持久存储；agent 按 `SessionID` load/append。`kit.New` 默认使用 `session.NewMemory()`。

### `web_fetch` / `web_search`（`tools/web`）

| 设置 | 默认 |
| --- | --- |
| 超时（整次调用，含所有重定向） | `web.DefaultTimeout` = 30s |
| 响应体上限 | `web.DefaultMaxBytes` = 2 MiB |
| 输出字符上限 | `web.DefaultMaxChars` = 20000（单次调用上限 `web.MaxCharsLimit` = 200000） |
| 重定向链 | `web.MaxRedirects` = 5 跳 |
| User-Agent | `web.DefaultUserAgent` = `"ZeoKeel-Rig/" + rig.Version`（当前为 `ZeoKeel-Rig/0.12.0`） |
| 搜索结果数 | `web.DefaultSearchLimit` = 5（最大 20） |
| URL 策略 | `nil` 放行所有 URL——**不可信模型请始终传 `sandbox.Policy`** |
| 私有地址 | 没有 `web.HostResolver` 的非 nil 策略会得到 fetcher 侧防护，拒绝 loopback/private/link-local/CGNAT/metadata IP；`web.WithAllowPrivateHosts()` 是显式 opt-out |

`web.NewFetcher(policy)` 是简单形式；`web.NewHTTPFetcher(opts...)` 接受 `WithPolicy`、`WithAllowPrivateHosts`、`WithTimeout`、`WithMaxBytes`、`WithMaxChars`、`WithUserAgent` 与 `WithTransport`。零/负选项值保留默认值。`sandbox.Policy` 路径在策略内校验地址（`web.HostResolver`）；fetcher 侧防护覆盖 `web.PolicyFunc` 与自定义 `CheckURL` 策略。

## 程序化工具调用（PTC）

`kit.Config{PTC: true}` 追加 DSH 风格的 `run_code` 工具。模型编写的程序运行在**系统安装**的 Python 3 或 Node.js 运行时中（在 PATH 上探测；自行构造工具时用 `ptc.Config.Python`/`Node` 固定）。程序调用 `tools.<name>(...)`，每个内层调用都像普通工具调用一样经过同一个 `rig.Gate`。不嵌入 JavaScript 引擎；这是基于 stdin/stdout 的子进程桥。`workflow` 独立存在，仍是 Go 原生的声明式编排器（顺序/并行步骤），不是 JS 运行时。

## 上下文管理、恢复与预设

- `kit.Config.ContextBudget` + `Compactor`：超预算时在模型调用前压缩内存对话；
  `session/compact.WindowCompactor` 保留 system 与最近窗口、卸载旧图片并保持
  tool_calls/tool 结果成块；`LLMCompactor` 先写模型摘要，失败回退窗口压缩。
- `kit.Agent.CompactNow(ctx, sessionID)`：手动压缩并持久化；要求 History 实现
  `rig.HistoryReplacer`（默认 `session.Memory` 已实现）。
- `kit.Config.Spill`：超过阈值的工具结果落盘，模型只看到摘要 + 路径。
- `kit.Config.CheckpointStore`：回合边界保存检查点；`agent.RecoverInterruptedTurn`
  为中断回合补“结果未知、未重放”的工具结果与 assistant 标记，绝不重放副作用。
- `kit.Config.ToolPreset`：`minimal`（8）/`standard`（70）/`all`（81），只过滤
  工具集，不改变系统 prompt 或权限策略。
- `kit.Config.ReasoningEffort`：`low`/`medium`/`high`，传给默认
  `provider/openai` 的顶层 `reasoning_effort`；`Request.Extra` 可按次覆盖。

审批辅助（`gate`）：

```go
g := gate.Review(reviewer,
    gate.WithReviewTimeout(15*time.Second),
    gate.WithReviewFallback(gate.Ask(host.Asker, gate.WithAskTimeout(10*time.Minute))),
    gate.WithReviewAllowlist("read_file", "list_dir", "glob", "grep"))
a, err := kit.New(kit.Config{Model: "gpt-4o-mini", Gate: g})
```

`gate.Ask` 把 `rig.Asker` 变成 allow/allow-always/deny 审批；`gate.Review` 先问
可插拔 `ToolReviewer`，错误/超时回退到人工 Gate。

## 权限模式与审批

Rig 遵循 DeepSeek Harness 权限模型：一个粗粒度 sandbox 模式加一个审批策略。这些模式是随附工具上的进程内围栏，不是内核边界——不可信模型请把宿主跑在容器/VM 中。

| 模式 | 文件变更 | Shell 命令 |
| --- | --- | --- |
| `sandbox.ModeReadOnly` | 拒绝 | 拒绝 |
| `sandbox.ModeWorkspaceWrite`（默认） | 仅工作区 roots 内 | 仅命令 allowlist |
| `sandbox.ModeDangerFullAccess` | 不受限（显式 denied globs 仍生效） | 不受限（显式 denylist 仍生效） |

使用 `kit` 时直接设置两个开关：

```go
a, err := kit.New(kit.Config{
    Model:            "deepseek-chat",
    Root:             root,
    Mode:             kit.ModeWorkspaceWrite, // or kit.ModeReadOnly / kit.ModeDangerFullAccess
    Approval:         kit.ApprovalAsk,        // or kit.ApprovalAuto / kit.ApprovalNever
    CommandAllowlist: []string{"git", "go", "ls", "cat"},
})
```

`ApprovalAsk` 是 fail-safe 默认值：内置 gate 只放行只读工具集、拒绝其余。提供 `kit.Config.Gate`（`rig.Gate`）来提示用户或应用自定义规则；`ApprovalAuto` 放行所有调用，`ApprovalNever` 拒绝所有调用。非 nil 的 `Gate` 总是优先于 `Approval`。自行构建 `sandbox.Policy` 时，使用相同取值调用 `sandbox.WithMode`。这些选择只约束工具实现；OS 隔离属于部署层（容器/VM）。

## 环境变量

SDK 自身只读这些变量：

| 变量 | 读取者 | 含义 |
| --- | --- | --- |
| `OPENAI_API_KEY` | `provider/openai` | `Config.APIKey` 为空且 `Config.APIKeyEnv` 未设置时使用的 API key |
| `ZEOKEEL_SKILLS` | `tools/skill` | 冒号分隔（`os.PathListSeparator`）的额外技能目录 |
| `HOME` | `tools/skill` | 展开 `~/.zeokeel/skills` |
| `PATH`、`HOME`、`LANG` | `sandbox` shell 策略 | 启用 shell 执行时命令继承的默认环境 allowlist |

provider **不**读 `OPENAI_BASE_URL`（或类似变量）：请显式传 `openai.Config.BaseURL`。没有配置文件，没有 `RIG_*` 变量约定，也没有全局 flag；配置加载由宿主负责。

## 宿主中的推荐分层

1. **代码默认值** —— 零值 / `kit.Config` 默认值；安全相关处 fail closed。
2. **环境变量** —— provider 凭证经 `APIKeyEnv`，技能路径经 `ZEOKEEL_SKILLS`；绝不要提交 key。
3. **宿主配置文件/flag** —— model 名、base URL、roots、命令 allowlist、迭代上限、gate 策略；宿主自行加载并填入 Config struct。
4. **每次请求** —— `rig.RunRequest` 覆盖项（model、迭代上限、gate、`Emit`）以及 `kit.RunRequest`/`RunText` 参数。

## 配置与版本

Config struct 与选项集是公共 API 的一部分。它们遵循 [docs/VERSIONING.md](VERSIONING.md) 中的增量规则：新字段追加且零值保持旧行为，选项只新增，移除先弃用。消费方应使用 **keyed struct literal**（`openai.Config{BaseURL: ...}`），绝不要依赖位置 literal，这样新增可选字段不会破坏代码。
