# AGENTS.md — 在 ZeoKeel Rig 仓库工作的 AI 规则

本文件同时是**仓库工作规则**和**模块说明书**。第一次进入本仓库的 AI 先读第 0–2 节；
要改某个模块，直接跳到第 3 节对应条目；要在宿主里使用 SDK，读第 5 节。
面向人类的项目介绍从 [README.md](README.md) 开始。

## 0. 仓库身份

| 项 | 值 |
|---|---|
| 项目 | ZeoKeel Rig —— 纯 Go 的 agent 工具集 SDK（个人 agent 客户端，单用户） |
| 模块 | `github.com/zeokeel/zeokeel-rig` |
| 当前 release | `v0.23.4`（`rig.Version` 必须与 tag 一致） |
| Go | 1.27+；构建与测试一律 `CGO_ENABLED=0` |
| 依赖 | 恰好一个：`mvdan.cc/sh/v3`（POSIX shell 解析器，仅 `sandbox` 策略层使用） |
| 包 | 69 个 Go 包（含 3 个 example、2 个 cmd、`internal/*`） |
| 许可 | MIT（v0.7.0 起） |
| 不提供 | UI、进程宿主、模块运行时/装配、账号鉴权、云端同步、密钥托管 |
| 文档语言 | 中文是唯一正式文档；模型可见的工具描述/Schema/错误文案保持英文，不翻译 |
| 未发布内容 | `storage`、`storagetest`、`memory` 公共契约、`rig.SpillStore`、`session/sqlite` 子模块（见 CHANGELOG「Unreleased」） |

## 1. 先读什么

| 你的任务 | 先读 |
|---|---|
| 在宿主里装配一个 agent | [docs/AI-SDK.md](docs/AI-SDK.md)、[docs/GETTING-STARTED.md](docs/GETTING-STARTED.md) |
| 查某个工具的精确 JSON Schema | [docs/ai/tools.json](docs/ai/tools.json)（由代码生成） |
| 增删工具、改工具面 | [docs/COVERAGE.md](docs/COVERAGE.md)、[docs/TOOLS.md](docs/TOOLS.md) |
| 改权限/审批/沙箱 | 本文件 §5、[docs/CONFIG.md](docs/CONFIG.md)、[docs/SANDBOX-OS.md](docs/SANDBOX-OS.md) |
| 改会话/持久化/记忆 | [docs/sqlite.md](docs/sqlite.md)、[docs/MEMORY.md](docs/MEMORY.md) |
| 改 CI/发布/版本 | [docs/RELEASE.md](docs/RELEASE.md)、[docs/VERSIONING.md](docs/VERSIONING.md)、[CONTRIBUTING.md](CONTRIBUTING.md) |
| 理解分层与请求流 | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) |
| 包级中文说明 | 对应目录的 `doc.go`，或 `go doc <pkg>` |

## 2. 模块地图

```
layer 0  契约        rig（模块根，stdlib-only，import 图的叶子）
layer 1  装配/循环   kit（唯一聚合层） · agent · tools（注册表） · tools/full
layer 2  能力        sandbox · gate · provider/* · tools/* · session/* · events ·
                     costmeter · modelprice · instructions · secrets · memory · storage
layer 3  实现        sandbox/osrunner · session/store · internal/* · session/sqlite（子模块）
layer 4  入口        cmd/rigdoc · cmd/rigdoctor · examples/*
```

三条被测试守住的模块化约束（`internal/rigdoc/modularity_test.go`）：

1. 叶子能力包不得 import `kit` 或 `tools/full`；
2. 根包 `rig` 只依赖标准库，且不 import 本仓库任何子包；
3. 只有 `kit` 同时装配 provider 与工具面。

`internal/rigdoc` 的 `leafCapabilities`（33 项）就是"必须能单独拿走使用"的名单；
新增可选能力包时把它加进去，否则这条承诺对该包没有守护。

## 3. 逐模块说明

### 3.1 契约层

**`rig`（模块根，`*.go`）**
稳定扩展面：`Message`/`Chunk`/`Usage`、`Provider`/`Stream`、`Tool`/`ToolCall`/`ToolResult`、
`Gate`/`Decision`、`History`/`HistoryReplacer`、`Agent`/`AgentSpec`/`RunRequest`/`RunResult`、
`Event`、以及宿主接口（`Asker`、`FilePresenter`、`PlanController`、`GoalStore`、
`memory.Wiki`、`Scheduler`、`SessionLog`、`Reviewer`、`SpillStore`、`CheckpointStore`、
`NoticeSource`、`Logger`、`Compactor`、`SubagentRunner`、`SkillStore`、`TeamTaskManager`）。
约束：**只能**用标准库、不能 import 本仓子包、不能加第三方依赖。任何改动都是兼容面改动。
`rig.Presenter`（`Present`）是已删除的 `present_artifact` 协议遗留，已废弃，新代码用 `FilePresenter`。

### 3.2 装配与循环

**`kit`** —— 唯一聚合层，一次 import 装配 provider + 工具集 + gate + history + agent。
入口：`kit.New(kit.Config) (*Agent, error)`、`Agent.Run`/`RunText`/`Shutdown`/`Preflight`/
`Tools`/`RegisteredTools`/`ActiveTools`/`ActivateTools`、`kit.Bool`、`Profiles`、`AvailableModels`。
职责边界：只做默认值与接线，不做业务逻辑；`Config.Tools` 非 nil 时 kit 不运行零配置体检、也不套默认预设（显式 `ToolPreset` 仍会按名字过滤）。
改默认值必须同步 [docs/CONFIG.md](docs/CONFIG.md) 与本文件 §5 的数字。

**`agent`** —— 流式 ReAct 循环：模型轮次、并行工具调用、审批、取消、事件；
`New(spec)`、`NewWithRedactor`、`NewRunner`（子代理 runner）、`ActivateTools`、
`DefaultMaxIterations=0`（不限）、重复调用守卫、上下文压缩接线、检查点保存。
默认轮数：agent 层 0（不限）；kit 层 200（`DefaultSafeMaxIterations`，成本护栏）。

**`tools`** —— 工具注册表：按名查找、确定性 specs、并发安全快照。不做工具实现。

**`tools/full`** —— 工具装配层：`All(AllOptions) []rig.Tool`（旧签名，丢弃后台句柄）
与 `NewRuntime(AllOptions) (Runtime, error)`（推荐，能 `Close` 后台任务）。
必须传 `Root` 或带 roots 的 `Policy`；否则 `apply_patch`/`search`/`undo` 不构造、
文件工具返回 `no roots configured`。

**`tools/catalog`** —— 工具账本：`Static`（83 个名字）、`Legacy`（58）、`Modern`（62）、
`ServerBound`（7）、`DocumentedGaps`（0）；`Check`/`AuditLegacy`/`AuditModern`。
新增/删除工具必须同时改账本，否则 `tools/full` 覆盖测试变红。

**`tools/presets`** —— 名字级工具集过滤：`minimal`(8)/`personal`(55)/`standard`(73)/`all`(83)。
只过滤名字，不动 prompt/权限；`personal` 去掉全部多智能体编排工具。

**`tools/audit`** —— 现代工具面对照审计（reference vs Rig），由测试强制。

### 3.3 权限与沙箱

**`sandbox`** —— 策略层（**DSH 对齐：只做可写根围栏**）：可写根与根内只读子路径
（`WritableRoot.ReadOnlySubpaths`）、只读模式、命令允许/拒绝名单、环境白名单、
网络与 URL 防护、输出/超时上限、运行期可写授权、
`Policy`/`WithMode`/`WithRoots`/`AddWritableRoot`/`SetDenialSink`。
**已停用**（导出符号保留、语义 no-op 并标 `Deprecated`）：保护路径
（`WithProtectedPaths`/`ProtectedPaths`，后者恒返回空）、拒绝 glob
（`WithDeniedGlobs`/`DeniedGlobs`，只记录不拦截）、策略层的命令风险分类
（`ClassifyCommand` 保留给宿主自用，策略层不再据此拦截）。
**不得** import 平台特定代码；**不得**依赖 `sandbox/osrunner`（方向相反）。

**`sandbox/osrunner`** —— **opt-in、默认关闭**的 OS 沙箱 runner：
Linux bwrap 或内置纯 Go Landlock 启动器、macOS `sandbox-exec`、Windows 受限令牌 + ACL；
纯 Go、无 cgo、零新增依赖。实现 `sandbox.Runner`，由工具 `WithRunner` 注入。
渲染结果与 DSH 的 sandbox-local **逐 token 一致**（`conformance_dsh_test.go` 钉死）：
read-only 只留 `/dev/null` 可写、workspace-write 再加 workspace 与私有 `/tmp`、
`danger-full-access` 透传（`Enforcement=none`）。
**不渲染**网络隔离（`--unshare-net`/seccomp）、宿主守护进程 socket 遮蔽、工具链缓存根、
`/var/tmp`、以及保护路径的只读规则——需要这些边界请用容器/VM 或在宿主侧叠加。
fail-closed：`Confine` 失败就不执行。只治理文件效果，网络/读/进程可见性/设备访问
都不在承诺内。不要把它变成默认行为，也不要把平台代码放进 `sandbox` 策略层。

**`gate`** —— 审批门禁（**DSH 对齐：只有两档**）：`PersonalAsk`/`PersonalAuto`/
`PersonalNever`、`PersonalAskWithoutAsker`、`Ask`、`Review`、`AllowThenAsk`、
`AllowSandboxedCommands`、`SandboxReadOnlyTools`、`DiffPrompt`/`SandboxGrantPrompt`。
`PersonalAuto` **就是** `AllowAll`（不问人、不分类命令）；`CommandRiskGate` 仍完整
可用，但**不再是任何默认预设的一部分**——要用就自己叠在最外层（`AllowThenAsk` 之外）。

### 3.4 工具包（`tools/*`）

**文件与编辑**
`builtin`（`read_file`/`write_file`/`edit_file`/`list_dir`/`glob`/`grep`/`bash`/`pwsh`；
`NewWithPolicyOptions` 是主入口，`WithRunner`/`WithEscalator`/`WithBackgroundJobs`/
`WithGrantTool` 是可选注入）、`editor`（`str_replace_editor`）、`image`（`read_image`）、
`patch`（`apply_patch`，必须 `patch.WithPolicy` 否则是沙箱旁路）、`search`（文件名模糊 +
BM25 + `@提及`）、`undo`（面向用户的回合撤销，git 隐藏快照；`IsGitRepo` 先判断）。

**shell 与进程**
`jobs`（`job_start/output/kill/list/wait`，环形缓冲、完成通知、owner 清理；`job_output`
支持分页续读——截断只推进到实际交付字节，并返回 `output_offset`/`next_offset`）、
`terminal`（`terminal_*`，unix PTY，Windows ConPTY，不可用时 pipe 回退）、
`builtin` 的 `bash`/`pwsh`。三者共享 `jobs.Manager`（`full` 已接好）。

**web**
`web`：`web_fetch`（HTTP(S)、重定向上限/大小上限、HTML→文本、SSRF 防护）与
`web_search`（可插拔 `web.Searcher`；没有 Searcher 时 kit 会摘掉它）。

**会话与诊断**
`session`（`session_*`，基于 `rig.SessionLog`）、`inspect`（sessions/events/workspace/stats）。

**宿主交互 / 计划 / 目标**
`hosttools`：`ask_user`、`present`（`rig.FilePresenter`）、`plan_review`、
`exit_plan_mode`、`goal`/`get_goal`/`create_goal`/`update_goal`，以及内存 Host 实现。
`present_artifact` 已删除，不要再加。

**记忆 / 调度 / worktree**
`memory`（`memory` 工具，基于 `memory.Wiki`）、`schedule`（`schedule_*`，`rig.Scheduler`）、
`worktree`（`enter_worktree`/`exit_worktree`/`list_worktrees`，受策略检查）。

**技能 / 发现 / 沙箱工具**
`skill`（`skill`：list/search/load，文件系统 `SKILL.md` 仓库）、
`discovery`（`tool_search`，动态工具加载入口）、
`sandboxgrant`（`request_sandbox_access`）、`sandboxstatus`（`sandbox_status`）。
`shelf` 是渐进式加载的**库原语**（宿主自定义工具名后使用），不是静态工具，不在账本。

**编排**
`team`（`spawn_teammate`/`list_agents`/`wait_agent`/`interrupt_agent`、`subagent`/
`subagent_fork` 的异步实现、`team_*` 旧别名、`team_task_*` 共享任务板）、
`subagent`（同步委派构造器，仍可用但 `full` 默认用 `team`）、
`Manager.SendAsyncContext(ctx, ref, msg)` 把消息投递到成员收件箱后立即返回，排空由成员自己的
goroutine 串行完成、不碰 `callMu`（旧 `SendAsync(ref, msg)` 保留为 Deprecated 包装）、
`workflow`（Go 原生声明式顺序/并行步骤，`{{prev}}`/`{{steps.name}}` 模板，无 JS 引擎）、
`ralph`（迭代子 agent 直到 DONE/上限）、`review`（`code_review`/`auto_review`，
`rig.Reviewer`；`SubagentReviewer` 可接到子 agent）。

**杂项**
`misc`：`working_directory`、`todo_write`（+ 旧别名 `todo`）。

**PTC**
`ptc`：`run_code`，只在 `kit.Config.PTC=true` 时出现；内层工具调用走同一个 Gate；
解释器进程也经 `Runner.Confine`（不是沙箱旁路）；只调用系统已安装的 Python 3/Node.js。

### 3.5 会话、存储与数据

**`session`** —— `session.Memory`：进程内 `History`，按 SessionID 隔离。
**`session/store`** —— 目录持久化：一会话一 NDJSON（`*store.Dir`、`History`、`List`/`Load`、
`DirStorage` 适配器）。单目录单实例写，不做跨进程锁。`AppendBatch`/`Load`/`Replace`/`Delete`
入口做归属校验（索引 stem、非常规文件、大小写折叠 + `os.SameFile`），两个 ID 落到同一文件时
返回 `store.ErrSessionIDConflict`；硬链接别名不在校验范围内。
**`session/compact`** —— 上下文压缩：`WindowCompactor`（默认，窗口/图片/tool 块裁剪）、
`LLMCompactor`（模型摘要 + 回退）。
**`storage`**（未发布）—— 「一个库装一份会话状态」的聚合契约：`History`/`Checkpoints`/
`Memory`/`Spill`/`Close`。分层铁律：不得 import `session/store`、不得被根包 import。
**`storagetest`**（未发布）—— 契约测试基线：`Factory` + `Capability`/`Capabilities` 能力位，
多实现共用。
**`memory`** —— 长期记忆契约 `Wiki`/`Page`/`PageMeta` 与规则函数。
**`session/sqlite`**（独立子模块）—— 同一份会话契约的 SQLite 实现：消息/事件/检查点/记忆/
溢出/花费入库，带索引、跨会话检索、NDJSON `ImportDir`、WAL、租约、BUSY 重试、0600。
主模块**永不** import 它；依赖门禁只允许根模块有一个解析器依赖。
**`internal/memwiki`** —— 记忆的磁盘/进程内实现（按工作区分区，0700/0600）。

### 3.6 provider

**`provider/openai`** —— OpenAI 兼容流式 provider（SSE、tool-call delta、reasoning、
usage、重试、`RequireCompleteStream` 可选）。
**`provider/local`** —— "OpenAI 兼容但不完全一样"的本地服务共享基座（`Config`/`Quirks`、
共享 SSE/重试、模型列表、`/v1/models`）。
**`provider/ollama`**（默认 `127.0.0.1:11434`）、**`provider/lmstudio`**
（默认 `127.0.0.1:1234`）：无鉴权默认、容忍缺失 `[DONE]`、可列模型。
新增 provider 时只改 provider 包 + `kit.ProviderKind` 接线，不要在工具层分叉。

### 3.7 横切能力

**`events`** —— 版本化 NDJSON 事件线格式（`Version=1`、`Envelope`、`Replay`、`Emitter`）；
agent 的 `rig.Event` 是进程内事件，不要混用。
**`costmeter`** —— 会话级 token/金额计量（纳美元整数）、预算告警、`Summary`/`Snapshot`。
**`modelprice`** —— 每百万 token 的美元价表（UI 估算，不是账单）。
**`instructions`** —— 发现并渲染 AGENTS.md 层级文档 + `<environment>` 块；
只应 Load 一次（prompt 前缀缓存）。
**`secrets`** —— **仅导出用**的凭据清洗（`RedactForExport`）；`kit` 运行时默认不脱敏。

### 3.8 internal

`fsguard`（os.Root 之上的安全文件操作）、`gitsnap`（git 回合快照）、
`diffpreview`（纯函数统一 diff）、`memwiki`（记忆实现）、`procattr`（子进程属性，
Windows 隐藏控制台窗口）、`seccomp`（Linux 沙箱辅助）、`rigdoc`（AI 目录生成 + 模块化/
表面守护测试）、`tools/internal/{background,toolutil}`（后台任务与工具辅助）。

### 3.9 cmd 与 examples

`cmd/rigdoc`：从真实构造器生成 `docs/ai/tools.json|md`；`-check` 可本地自查漂移（CI 不查文档）。
`cmd/rigdoctor`：沙箱自检/诊断 CLI（渲染各平台后端 profile + 功能探测）。
`examples/quickstart`（kit 最小示例，README 第一段代码必须与它逐字节一致）、
`examples/agent`、`examples/custom-tool`。

## 4. 改代码时的硬规则

1. **新工具不是自动注册的**：实现包 → `tools/full` 接线 → `tools/catalog.Static` →
   `tools/presets` 名字表（需要时）→ 测试 → `go run ./cmd/rigdoc` → `docs/TOOLS.md`/
   `docs/COVERAGE.md` → `CHANGELOG.md`。代码侧少一处 CI 会红；文档侧靠评审与发布前自查。
2. **模型可见的 `Description`/`Schema`/错误文案属于接口**：改动要谨慎，更新快照/测试；
   不要翻译成中文。
3. **注释与包文档用中文**，`go doc` 应可直接阅读。
4. **依赖与构建**：`CGO_ENABLED=0`；唯一依赖 `mvdan.cc/sh/v3`；根包 stdlib-only；
   叶子能力包不得 import `kit`/`tools/full`。外部运行时依赖禁止（PTC 只调用系统已装
   Python/Node；osrunner 的 macOS 分支只 exec 系统 `sandbox-exec`）。
5. **权限模型**（DSH 对齐）：四个权限档 `read-only`/`workspace-write`/
   `danger-full-access`/`auto`（`kit.Config.PermissionPreset`），由 `Mode` + 两档审批
   `ask`/`never` 组成；沙箱只做可写根围栏，默认门禁不含命令风险分类。核心策略层保持
   进程内围栏。OS 沙箱只能是 opt-in 的 `sandbox/osrunner`，默认关闭、
   由宿主显式 `New` 并注入 `WithRunner`。
6. **发布**：按 [docs/RELEASE.md](docs/RELEASE.md)：PR → CI → 签名 annotated tag →
   release workflow → 草稿 → publish。`version.go` 只在发版时与 tag 一起变。
7. **文档同步**：改工具面/默认值/版本时，同批更新 README 中的数字、`docs/TOOLS.md`、
   `docs/CONFIG.md` 与本文件 §5。文档里不要出现内部审计/规划过程指代。

## 5. 使用 SDK 时的硬事实

### 5.1 工具面口径（只认这四个）

| 口径 | 数字 |
|---|---|
| `kit.New(Config{})` 零配置**注册** | **39**（其中 **16** 个初始 active） |
| 接 `Config.Presenter` | 40 注册 / 17 active |
| 全接线（Linux、git 仓库、PTC、`Presenter`） | 50 注册 / 17 active |
| `full.All(AllOptions{Root, Policy})` | **81**（`SandboxGrants: true` 时 **83**） |
| `full.All(full.AllOptions{})`（无 Root/Policy） | **78** |
| `catalog.Static` / `presets["all"]` | **83** |
| `presets`：personal / standard / minimal | **55 / 73 / 8** |
| `catalog`：Legacy / Modern / ServerBound | **58 / 62 / 7** |

- 零配置 39 = personal 55 − 无 tracker 的 `undo` − 体检摘掉的 15（缺 Searcher 的
  `web_search`、缺 SessionLog 的 5 个 `session_*`、kit 无注入入口的 5 个
  goal/plan 工具、缺 Presenter 的 `present`、缺 Asker 的 `ask_user`、
  `AllowNetwork=false` 的 `web_fetch`、非 Windows 的 `pwsh`）。
- 全接线 50 = 55 − 5（goal/plan 恒缺注入入口）− 1（非 Windows 无 `pwsh`）+ 1（PTC
  的 `run_code`）；`undo` 仅在 `GitUndo` 且工作区是 git 仓库时计入。
- **核心 13**：`read_file write_file edit_file apply_patch list_dir glob grep search
  bash todo_write working_directory read_image str_replace_editor`；
  初始 active 16 = 核心 13 + `tool_search` + `memory` + `skill`。

### 5.2 默认值（`kit.New`）

- 只有 `Model` 必填；`Root` 默认 `.`；`Mode` 默认 `workspace-write`；
  `Approval` 默认 `ask`；`PermissionPreset` 默认不选（`""`）；`DisableShellAccess=false`
  且 `CommandAllowlist` 为空时策略**不限制命令名**（`WithAnyCommandAllowed(true)`，
  安全性靠人工审批）；不联网。
- `MaxIterations`：0 → 200（`DefaultSafeMaxIterations`）；负数 → 真的不限；>0 → 上限。
- 上下文压缩默认开（模型窗口 × 0.6、下限 16k；`WindowCompactor`），失败降级继续。
- 动态工具加载默认开（`ToolDiscovery` nil=开）；`InitialActiveTools` 可覆盖，
  `tool_search`/`memory`/`skill` 强制并入。
- `SandboxGrants` 默认**开**（注册两个工具，但不进初始 active 集；
  `kit.Bool(false)` 关闭）。
- `AutoAllowSandboxedCommands` 默认**开**（nil）：已沙箱化的 shell 命令免问
  （围栏已生效时**不看命令文本**），仍写 `approval.response`（reason=sandboxed）。
- `AskTimeout` 默认 `0` = **审批不超时、一直等**，只有取消本轮才撤回；`>0` 才设预算。
- **kit 不再追加任何保护路径**（`DisableProtection`/`ProtectPaths`/`ProtectWorkspaceMetadata`
  已废弃、不再生效）；需要保护请自己构造 `sandbox.Policy` 并 `AddProtectedPaths`
  （该 API 现在也只是记录，不再拦截——见 §3.3）。
- memory 工具默认启用（`<状态目录>/memory/<workspaceID>` 文件 wiki，0700/0600，
  不写进仓库）；`GitUndo`、`PTC`、`AllowNetwork` 默认关。
- `Spill` 默认关；一旦设置且 `SpillPolicy.Dir` 为空，kit 解析为 `<Root>/.rig/spill`
  （工作区内、可写根内、模型能按 `path:` 读回；建议宿主把 `.rig/` 加进 `.gitignore`）。
  裸 `agent.New` 不给 `Dir` 时回落到 `<系统临时目录>/zeokeel-rig-spill`（能落盘但读不回）。

### 5.3 审批与沙箱

- **权限四档**（`Config.PermissionPreset`，`kit.Presets()`）：`read-only`
  （只读 + ask）、`workspace-write`（工作区可写 + ask，默认）、`danger-full-access`
  （全权 + auto）、`auto`（全权 + ask + `ToolReviewer` 预审）。显式设置的
  `Gate`/`Mode`/`Approval`（或 `Policy`）优先，此时档位退化为 `custom`。
- `auto` 档的预审：reviewer 放行即执行；**明确拒绝或审查失败都回退到人工 ask**；
  没配 `ToolReviewer` 时按纯 ask 跑，`Preflight` 会说明这个降级。
- **审批两档**：`ask` = 只读工具免问 → 其余（**包括 `rm -rf /`**）问 `Config.Asker`；
  **默认不超时**（一直等），`AskTimeout>0` 才有上界；无 Asker 时
  `gate.PersonalAskWithoutAsker()`：只读放行、其余拒绝并说明"没有可用的审批通道"。
  `never` = 全拒。`Approval: auto` = `PersonalAuto()` = **`AllowAll()`（全放行）**。
- 默认门禁**不做**命令风险分类，也**不硬拒危险命令**；`CommandRiskGate` 仍可用，
  但要自己显式叠在最外层（`AllowThenAsk` 之外）。
- 不要用 `PersonalDefault(nil)`/`PersonalAsk(nil)`/`PersonalAsk(gate.DenyAll())`：
  文案会说 `approval=never`，与真实状态不符。
- 逐命令提权：`sandbox_permissions:"require_escalated"` + `justification`（≥10 字符），
  只在**人工批准**后让这一条命令不被 OS runner 包装；策略、workdir、超时、输出上限
  不变；`auto`/`never` 档拒绝。宿主用 `builtin.WithEscalator` /
  `full.AllOptions.Escalator` 注入。它**不是**已删除的 `tools/escalate` 包。
- 沙箱只做可写根围栏：`read-only` 只留 `/dev/null` 可写（无 `/tmp`）、
  `workspace-write` = workspace ∪ 追加可写根 ∪ 私有 `/tmp`（`$TMPDIR`）、
  `danger-full-access` 不包装（`Enforcement=none`）。**没有** `− ProtectedPaths()`
  这一项；运行期授权只能由审批通过后 `Policy.AddWritableRoot`。
- 网络、读范围、进程可见性、设备访问**都不在沙箱承诺内**（部署层负责）。
- `Preflight()` 回答"现在有没有 OS 级隔离、可写面是什么、为什么没有"。

### 5.4 编排与后台

- `subagent`/`spawn_teammate` 是**异步**的：用 `wait_agent`/`team_wait`/`team_last`
  收结果，完成通知会注入下一步。
- 后台 shell/terminal 共享同一个 `jobs.Manager`；`full.All`/`NewRuntime` 已接好，
  手动组装要传 `WithBackgroundJobs`。
- 会话持久化：`History` / `SessionStoreDir` / `SessionStore` 三者互斥；
  `Storage` + `BackgroundJobs` 由 `Agent.Shutdown(ctx)` 收尾。

### 5.5 可选能力层（零依赖、opt-in）

`SessionStoreDir`（NDJSON 持久化 + 崩溃自愈）、`SessionStore`、`Storage`、
`Spill`（+`spill_read`；默认目录 `<Root>/.rig/spill`，回收按 session 作用域分区）、`CheckpointStore`、`EventWriter`（NDJSON）、`Redactor`
（仅导出用，默认 nil）、`Instructions`/`Environment`、`GitUndo`（默认 false）、
`Meter`/`MaxRunCostUSD`、`SandboxRunner`、`PTC`。

## 6. 不要重新加回的能力

**已整包删除**（`internal/rigdoc` 的 `TestRemovedPackagesAreGone` 会拦回归）：

- `tools/escalate`：旧的提权通道包。要放宽权限用 `danger-full-access`、审批，或
  `tools/builtin` 的逐命令提权（人工批准、策略不变）。
- `mcpserver` + `cmd/rigmcp`：把工具暴露成 MCP server。本产品是客户端；
  `tools/mcp` 是客户端能力，保留。
- `internal/filewatch`：轮询冲突检测（单 agent 场景由 git 覆盖）。
- `sandbox/escalate`：沙箱提权协议本体 + 宿主 broker。

**已删除的工具名**（不要出现在 `catalog.Static`/预设/文档里）：
`report`、`settings_get`、`settings_set`、`present_artifact`。

## 7. 验证命令

**CI 只验证代码**（`make ci` 与 `.github/workflows/ci.yml` 同款）：

```sh
export PATH=$PATH:/usr/local/go/bin   # 本环境 Go 不在默认 PATH
export CGO_ENABLED=0

gofmt -l $(git ls-files '*.go')        # 必须无输出
go build ./...
go vet ./...
go test ./...                          # 全量；发布前跑 -race
go test ./tools/full -run TestToolCatalogCoverage -v   # 账本口径
make ci                                # 本地代码门禁
```

文档与发布状态的检查**不进 CI**，按需单独跑：

```sh
go run ./cmd/rigdoc -check             # docs/ai 是否最新
make quickstart                        # README 快速开始与示例逐字节一致
make version                           # version.go 与 release tag 一致（发布流程用）
```

## 8. 文档约定

- 中文正式文档；一篇文档一个 H1；开头写"解决什么问题/读者是谁"，结尾给"相关文档"。
- 数字必须标口径（零配置 / `full.All` / 账本 / 预设），且与 §5.1 一致。
- 工具名、字段名、错误文案用代码格式；不翻译模型可见字符串。
- 不写内部审计轮次、计划稿编号、已删除文档的引用。
- 文档索引：[docs/README.md](docs/README.md)。