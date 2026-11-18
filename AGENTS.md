# AGENTS.md — 在本仓库中工作的 AI 规则

本仓库是 **ZeoKeel Rig**（Go agent toolset SDK），当前 release v0.23.3。

## 先读什么

1. `docs/zh/AI-SDK.md` — 给 AI 的 SDK 使用/组装指南（首选）
2. `docs/ai/tools.json` — 每个工具的精确 JSON Schema（AI 首选）
3. `README.md` / `docs/zh/` — 中文项目文档
4. 对应包的 `doc.go` / `go doc` — 包级中文说明

## 改动代码时

- 工具不是自动注册的：新工具必须同时更新 `tools/full.All`、`tools/catalog.Static`、相关测试、`docs/TOOLS.md`，并运行 `go run ./cmd/rigdoc` 重新生成 AI 目录。
- 模型可见的 `Description`/`Schema`/错误文案属于接口，修改需谨慎并更新快照/测试。
- 代码注释与包文档以中文为主；`go doc` 应可直接阅读。不要翻译模型可见的工具描述/JSON schema/错误字符串。
- 构建/测试必须 `CGO_ENABLED=0`；唯一依赖 `mvdan.cc/sh/v3`；根 `rig` 包必须保持 stdlib-only。
- 权限模型：`read-only` / `workspace-write` / `danger-full-access` + `ask/auto/never` 审批；核心策略层保持进程内围栏。OS 沙箱只以 **opt-in 的 `sandbox/osrunner` 子包**存在：纯 Go、无 cgo、零新增依赖、默认关闭，由宿主显式 `New` 并注入工具 `WithRunner`；不得把它变成默认行为，也不得让 `sandbox` 策略层依赖平台代码。外部运行时依赖仍禁止（PTC 例外：只调用系统已安装的 Python/Node；osrunner 的 macOS 分支只 exec 系统自带的 `sandbox-exec`）。
- 发布按 `docs/RELEASE.md`：PR → CI → signed tag → release workflow → publish。

## 使用 SDK 时

- 快速路径：`kit.New(Config{})` **零配置**默认注册 **39** 个工具（其中 **16** 个初始 active：核心 13 + `tool_search` + `memory` + `skill`——见下面的"动态工具加载"；接了 `Config.Presenter` 后 `present` 也回来，变成 40 注册 / 17 active）= personal 预设 **55** 个名字 − 没有 tracker 的 `undo`(1) − 零配置体检摘掉的 15 个（缺 `Searcher` 的 `web_search`、缺 `SessionLog` 的 5 个 `session_*`、kit 无注入入口的 `get_goal`/`create_goal`/`update_goal`/`plan_review`/`exit_plan_mode`、缺 `Asker` 的 `ask_user`、缺 `Presenter` 的 `present`、`AllowNetwork=false` 时的 `web_fetch`、非 Windows 平台的 `pwsh`）。宿主每接一项就按项加回：`Asker` +1（ask_user）、`Searcher` +1（web_search）、`SessionLog` +5（session_*）、`AllowNetwork` +1（web_fetch）、`PTC` +1、git 仓库里 `GitUndo` +1；**全部叠加 = 49**（`sandbox_status`/`request_sandbox_access` 自 v0.21 起**默认注册**——discoverable，不进初始 active 集；`SandboxGrants: kit.Bool(false)` 可关掉这 2 个）。（`subagent`/`subagent_fork` 已从 personal 预设移除：个人客户端不提供多智能体编排，需要时用 `standard`/`all` + `full.All(AllOptions{Runner: …})`。）**三个口径别混**：`full.All(Root)` 构造 **81** 个静态工具（`SandboxGrants: true` 时 **83**）；`catalog.Static` / `presets["all"]` = **83**。`full.All(full.AllOptions{})`（不传 `Root`/`Policy`）只有 **78** 个，`apply_patch`/`search`/`undo` 直接不构造、文件工具全部返回 `no roots configured`——**必须传 `Root` 或带 `Roots` 的 `Policy`**。
- 完整路径：`full.All(AllOptions{Host, Runner, Searcher, SessionLog, MCPClients, LSP, ...})` 后传给 `agent.AgentSpec.Tools` 或 `kit.Config.Tools`。
- **沙箱内的命令免审批**（`kit.Config.AutoAllowSandboxedCommands`，`nil` = 开，`kit.Bool(false)` 关）：接了 `SandboxRunner` 之后，`bash`/`pwsh` 的**非危险**命令不再弹窗——安全性由围栏保证，而不是由点击次数保证（否则用户会为少点几十次弹窗把审批整体关掉，连危险命令的拦截也丢了）。判据全部满足才免问：工具是 shell 类、runner 非 nil 且自报能强制、策略不是 `danger-full-access`、命令不是 `dangerous`。免问仍写一条 `approval.response`（decision=allow、reason=`sandboxed`）供 UI 显示与审计；危险命令与本开关无关，永远直接拒绝；文件工具（`write_file`/`edit_file`/`apply_patch`…）不经 OS runner，照旧问人。**headless 宿主受益最大**：没有 Asker 时，已沙箱化的命令现在能跑（此前一律"没有可用的审批通道"）。
- 默认 gate（`approval=ask`）是三层：① 只读工具与只读命令**免问**；② `bash`/`pwsh` 里的危险命令（`rm -rf /`、`mkfs`、`curl … | sh`）**直接拒绝**，规则 id 与中文理由作为工具错误回给模型；③ 其余（写文件、可变命令）**问人**——问的是宿主注入的 `kit.Config.Asker`（`rig.Asker`，单次询问默认 10 分钟超时）。**没接 Asker 时不会"自动有人问"**：kit 退到 `gate.PersonalAskWithoutAsker()`——只读照放、危险照拒、其余拒绝并明确告诉模型"没有可用的审批通道"。`Approval: "auto"` **不等于** `AllowAll`（危险命令仍拒）；`"never"` 全拒。要换策略用 `Config.Gate`（完全接管），并保证 `gate.CommandRiskGate` 在最外层。
- 模型↔工具轮数：`agent` 层默认不限制（`DefaultMaxIterations=0`）；**`kit` 默认 200 轮**
  （`kit.DefaultSafeMaxIterations`，成本护栏；显式负数=真的不限，>0=显式上限）。另有两道"抽风守卫"：
  连续 3 轮完全相同的工具调用注入提示、连续 5 轮或同轮 ≥3 个相同调用直接返回 `rig: repeated call blocked`。
  上下文压缩**默认开启**（模型上下文 × 0.6、下限 16k；默认 `WindowCompactor`），压缩失败会降级继续而不是中止回合。
- **动态工具加载默认开启**（`kit.Config.ToolDiscovery`，`nil` = 开）：初始只把**核心 13**
  （`read_file write_file edit_file apply_patch list_dir glob grep search bash todo_write
  working_directory read_image str_replace_editor`）+ **`tool_search`** + **`memory`** + **`skill`**
  发给模型（零配置 16 个；`skill` 只有 ~808 B schema，而"有哪些技能"是开工前就该知道的），
  其余工具"注册但不 active"；模型用 `tool_search`（`{}` 列清单 / `{"query":…}` 筛选 /
  `{"activate":[…]}` 加载）自助激活。常驻集合拆成两类：`memory`/`skill` **无条件常驻**，
  `tool_search` **随 `ToolDiscovery` 开关**（关掉发现时它不注册——不加载新工具了，发现
  入口也不该占位）。激活是**纯增量、按会话**：请求里的 `tools` 按**激活顺序**追加（初始集
  按注册顺序），新工具只出现在末尾——**激活到全集也不会重排**；**不激活时 tools 数组逐字
  节稳定**，激活那一轮 prefix cache 不命中、之后恢复。我们的 system prompt 不含工具描述，所以激活**不会**重建 prompt。
  关闭：`off := false; kit.Config{ToolDiscovery: &off}`（回到 v0.20 的"全部 active"）。
  `present` 由依赖门控：`kit.Config.Presenter`（`rig.FilePresenter`）为 nil 时**不注册**，
  接上之后保留并立刻进初始 active 集。
  自定义初始集合用 `kit.Config.InitialActiveTools`（`tool_search`/`memory`/`skill` 强制并入）；
  宿主也可直接 `agent.ActivateTools(sessionID, …)`（事件 `tool.activated` 的 trigger=host）。
- `subagent`/`spawn_teammate` 是异步的：用 `wait_agent`/`team_wait`/`team_last` 收结果，完成通知会注入下一步。
- 后台 shell/terminal 共享同一个 `jobs.Manager`；`full.All` 已接好，手动组装要传 `WithBackgroundJobs`。
- 沙箱可写面 = `Roots()` ∪ `AdditionalWritableRoots()` ∪ 平台临时目录 − `ProtectedPaths()`；运行期授权（`request_sandbox_access`）只能由审批通过后调用 `Policy.AddWritableRoot`，工具自身不审批。
- 可选能力层（都是零依赖、opt-in，由 `kit.Config` 一行接上）：`SessionStoreDir`（会话持久化 + 崩溃自愈）、`EventWriter`（NDJSON 事件流，见 `events`）、`Redactor`（**仅导出用**的凭据清洗，默认 nil=不脱敏，见 `secrets.RedactForExport`；`kit` 默认不再在工具边界脱敏）、`Instructions`/`Environment`（AGENTS.md 层级 + 环境块进系统提示词）、`GitUndo`（**默认 false**；开启后每个 Run 前用 git 打一份隐藏快照——`git stash create` 语义，不动 HEAD/分支/用户暂存区——`undo` 工具**只有在这时**才进 kit 的默认工具集；工作区不是 git 仓库时该开关被静默忽略、工具不出现，宿主可用 `undo.IsGitRepo` 提前判断）。`tools/undo` 的定位是**面向用户的"撤销按钮"**：用户在 UI 上点一下回到本回合开始前，模型只在用户明确要求撤销时才应调用它。旧的 `UndoTracker` 字段已废弃（保留仅为兼容自建 tracker 的宿主）。
- 高级装配：`sandbox.ClassifyCommand`/`gate.CommandRiskGate`（命令级风险，必须叠在 `AllowThenAsk` 之外）、`session/compact`（上下文压缩）。
- **产品定位是个人 agent 客户端 SDK（单用户）**。以下企业/多租户/自建服务形态的能力**已从发布面移除，不要重新加回**：`tools/escalate`（提权通道；要放宽权限直接用 `danger-full-access` 或审批）、`mcpserver` + `cmd/rigmcp`（把工具暴露成 MCP server；本产品是客户端，`tools/mcp` 是客户端能力，保留）、`internal/filewatch`（轮询冲突检测；单 agent 场景由 git 覆盖）、`sandbox/escalate`（沙箱提权协议本体 + 宿主 broker，含 `os/exec` 面；个人客户端放宽权限用 `danger-full-access` 或审批即可）。这四个包已**整包删除**，`internal/rigdoc` 的守护测试（`TestRemovedPackagesAreGone`）会拦住回归；`internal/procattr` 不受影响（`tools/{builtin,jobs,lsp,mcp,ptc,terminal,worktree}` 与 `sandbox/osrunner` 仍在用）。
- 另有四个**工具**（不是包）也已按同一口径移除，不要再加回：`report`（会话报告，合规/留痕形态）、`settings_get`/`settings_set`（设置读写；个人客户端的设置由宿主 UI 直接管理，不该让模型去改配置）、`present_artifact`（交付物面板；个人客户端用 `present` 即可）。
  > 若这几个名字仍出现在 `go test ./tools/full -run TestToolCatalogCoverage -v` 的输出或 `catalog.Static` 里，说明对应的删除**还没完成**。
- 保护路径默认开启（`kit.New` 自建策略时追加 workspace 元数据 + 主目录凭据位置），用 `kit.Config.DisableProtection=true` 关闭；它是防误伤不是沙箱。
- `run_code` 只在 `kit.Config.PTC=true` 时出现，内层调用走同一 Gate；挂接 `kit.Config.SandboxRunner` 时解释器进程同样经 `Runner.Confine` 包装（不是沙箱旁路）。
- 推荐门禁组合（`gate/defaults.go` 的三档，等价于 `CommandRiskGate(AllowThenAsk(SandboxReadOnlyTools, inner))`）：
  1. `gate.PersonalAsk(gate.Ask(host.Asker))` —— **个人客户端默认**：只读工具与只读命令免审批、危险命令直接拒绝（命中的规则 id 与中文理由会作为工具错误回到模型上下文，避免它原样重试）、其余交给人工审批（`WithAskPrompt(gate.SandboxGrantPrompt)` 仍可叠在 `Ask` 上）；
  2. `gate.PersonalAuto()` —— 不问人，但危险命令仍然被拒（**不等于**裸 `AllowAll()`）；`gate.PersonalNever()` —— 拒绝一切并说明原因。
  门禁顺序不能反：`CommandRiskGate` 必须在 `AllowThenAsk` **之外**（最外层），否则每条 bash 命令都会先弹窗；宿主没有 Asker 时用 `gate.PersonalAskWithoutAsker()`（理由文案是"没有可用的审批通道"）；**不要**用 `PersonalDefault(nil)`/`PersonalAsk(nil)`（现在返回 deny + "没有配置审批通道"理由、**不会中断 run**，但那样每次需要审批的调用都会被拒，属于配置疏漏），也不要用 `PersonalAsk(gate.DenyAll())`（文案会说 `approval=never`，与真实状态不符）。
  只读自动放行、放宽可写面必须人工确认。
