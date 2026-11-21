# Changelog

## Unreleased

### Added

- **记忆/上下文子系统 v2（默认）**：`memoryvfs` 公共契约（`mem://` VFS、L0/L1/L2、
  九类记忆、标签/链接/来源、会话/规则/快照/导入导出），文件后端
  `internal/memvfs`、检索 `internal/memsearch`、抽取 `internal/memextract`；
  BM25 + 可选向量/RRF/rerank/意图分析，会话捕获/提交/启发式抽取、相似度合并、
  TTL/衰减/配额/GC；纯 Go、stdlib-only、无新增依赖。`memory` 工具扩到 20 个动作；
  旧 `memory.Wiki` 保留兼容，首次打开自动迁移 `pages/*.md`。
- `kit.Config.MemoryStore/MemoryOptions/MemoryCapture/MemoryCommitTokens` 与
  `RuntimeConfig.MemoryStore/MemoryOptions`：默认 v2 后端、按工作区共享、Run 后
  捕获、阈值/压缩提交、Shutdown 冲刷。
- `kit.Config.MemoryRecall` / `MemoryRecallTokens`：Run 前自动召回相关记忆并作为
  system 消息注入本轮（默认开，预算 1200 token）；首轮注入 profile/preferences
  索引与 `<available-skills>`，并重放崩溃遗留 pending。
- `MemoryCaptureToolResults` / `MemorySkillCatalog`：可关闭 tool 结果捕获与 skills
  目录注入。
- `memoryvfs.ExtractorFunc` / `SummarizerFunc`：宿主 LLM 用一个函数接入
  写入侧（会话抽取/L0-L1 摘要）；读取侧仍保持 BM25，不引入 Embedder。
- `memoryvfs/sqlite` 子模块：SQLite 版 `memoryvfs.Store`，条目按
  `(workspace_id, uri)` 存储、URI 仍是文件夹式路径；召回按工作区取候选后做
  词法评分。主模块零新增依赖，宿主显式 opt-in。
- `internal/membench`：可复现 easy/hard 检索基准与分层 token 曲线。

- **`kit.Runtime`：长生命周期宿主的共享服务层**。`kit.New` 每次都会重建全部
  provider、后台任务管理器、PTY 池、记忆 wiki 与 spill store，宿主每开一个会话/子代理
  就付一次全量代价，状态也不共享（后台 job 跨会话不可见、`terminal_*` 句柄各一份）。
  新增并存的入口：
  - `kit.NewRuntime(kit.RuntimeConfig) (*Runtime, error)`：持有**进程级共享资源**
    （provider 含其 `http.Client`/连接池、`*jobs.Manager`、`*terminal.Manager`、
    按工作区缓存的记忆 wiki、spill store，以及 `Searcher`/`SessionLog`/`Skills`/
    `TeamTasks`/`Scheduler`/`Reviewer`/`Runner`/`MCPClients`/`Models`/`SandboxRunner`
    这些宿主能力通道）。
  - `(*Runtime).NewAgent(kit.Config) (*Agent, error)`：只为每个会话/子代理重建
    `agent.Agent`、history、gate 绑定、`sandbox.Policy`（roots/Mode/隐藏路径）、会话
    状态、工具实例、activations 与 undo。`Config` 里生命周期归 Runtime 的字段
    （`Provider`/`Jobs`/`BackgroundJobs`/`Storage`/`Aggregate`）**报错**；可覆盖的
    能力字段被共享值覆盖并 `slog.Warn`。
  - `(*Runtime).Close() error`：关共享后台任务、PTY 与 spill store（实现 `io.Closer`
    时），幂等。关闭后 `NewAgent` 与已存在 Agent 的 `Run` 都返回新增的
    `kit.ErrRuntimeClosed`；`Agent.Shutdown` 仍只收尾本 Agent，不碰共享句柄。
  - 并发安全：`NewAgent` 与 `Close` 互斥（锁 + closed 标记），多个 goroutine 可同时
    建 Agent / 读共享管理器。
  - `kit.New` **行为与告警不变**：它与 `Runtime.NewAgent` 共用同一份 `newAgent` 装配
    实现，只是自己构造并在 `Shutdown` 里收尾全部资源。
  - `tools/full` 新增 `AllOptions.BackgroundTerminals`（注入共享 `*terminal.Manager`），
    让 PTY 会话句柄能跨工具集/Agent 共享。
  - 测试：`TestRuntimeSharesBackgroundJobsAcrossAgents`、
    `TestRuntimeNewAgentUsesOwnPolicyAndSession`、`TestRuntimeCloseStopsNewAgents`、
    `TestRuntimeConcurrentNewAgent`（`kit/runtime_test.go`）。
- **`undo` 的非 git 后端（纯文件系统快照）**：`undo.NewFileTracker(root, undo.FileOptions{})`
  在**不是 git 仓库**的工作区里也能撤销回合——回合开始时把工作区里被改动/新增的文件复制
  到 `Dir/turns/<turn>/files/<rel>`（保留相对路径），清单 `Dir/turns/<turn>/manifest.json`
  记录权限位；清单与镜像分属两个命名空间，工作区里的 `manifest.json` 不会与清单撞名。
  回退时写回原文、删掉本回合新增的文件、恢复本回合删除的文件。超过 `FileOptions.MaxBytes`（默认 4 MiB）
  的文件只记"未快照"，undo 结果里以 warning 列出且不会被改动；符号链接等非常规文件
  不参与。`undo.Backend` 是两条后端的共同接口，`*undo.Tracker` 的方法签名不变，kit 与
  `undo` 工具不需要判断后端类型。
- **回合号跨进程恢复**：回合号持久化在 `Dir/state.json`（`{"turn":N}`，原子写）。
  `undo.NewFileTracker` 打开同一个 `Dir` 时会读回来继续；git 后端在新增的
  `undo.Options.Dir` 非空时同样持久化（kit 默认 `<Root>/.rig/undo`，并把 `.rig` 与
  **实际的 UndoDir 相对路径**加进忽略表，避免 `state.json` 自己变成一次可回退的
  "回合改动"）。`Dir` 为空时行为不变（回合号只活在进程内，重启后从 1 开始）。每次
  `Begin` 都会重读 `state.json` 并取 `max(内存, 磁盘)`：同一个 `Dir` 上的第二个实例
  不再撞号。
- **`kit.Config.UndoDir`**：非 git 工作区 + 设置了它时，kit 挂上 `FileTracker`，
  `undo` 工具因此出现；git 仓库里 `GitUndo` 仍然优先（`UndoDir` 只当回合号目录）。
  文件后端构造失败（例如 `UndoDir == Root`）时 `kit.New` **返回错误**，不再静默吞掉。
  `Config.Tools` 非 nil 时不改行为（与 `GitUndo` 一样列进"注入即失效"的告警）。
- **文件后端的安全与正确性补齐**：回退一律用 `Lstat` 判定类型、写入走"先删 + `O_EXCL`
  创建"，符号链接/目录/设备文件只以 warning 报告、不写穿（父目录是链接时同样拒绝）；
  只读（0444）目标可以回退并恢复清单里的权限位；文件被换成目录时给出准确原因且不删除
  目录内容；`x:y.txt` 这类 POSIX 合法文件名可以用于 path 过滤。

- **`kit.Runtime` 的多 root、MCP 客户端热增删与 runner 派生**（运行期变更面）：
  - `(*Runtime).AddRoot(path)` / `(*Runtime).RemoveRoot(path)`：多可写根。`AddRoot`
    追加一根、只影响**之后新建**的 Agent（重复添加幂等）；`RemoveRoot` 对**每个已存在
    的 Agent 立即撤销**，且对静态 `Config.Roots`/`Config.Root` **持久**——路径进入
    Runtime 的"已撤销"集合，之后用同一份 `Config` 新建的 Agent 不再拿到它（`AddRoot`
    同一条路径可加回来）。被撤销的根若在后续 `Config` 里是主根，`NewAgent` 返回可读
    错误而不是静默丢掉主根。撤销只改 kit 自建策略与 kit 追加到宿主策略上的根；主根
    不可撤销。
  - `kit.Config.Roots []string`：一次性声明多个可写根（**第一个是工作区主根**）；
    与 `Config.Root` 同时出现时以 `Roots` 为准并 `slog.Warn` 点名字段；每个根都必须
    存在且是目录——宿主显式传 `Config.Policy` 时 `Roots[1:]` 同样校验，并作为追加可写
    根声明进那份策略（不再静默丢弃）。
  - `(*Runtime).AddMCPClient(name, client)` / `(*Runtime).RemoveMCPClient(name)`：
    按名字登记 MCP 客户端，新建 Agent 的工具面随之包含/不含它的工具；已存在的 Agent
    工具面在构造期固定，只收一条事件流 `notice`（新增 `kit.ErrMCPClientNotFound`）。
    不关闭 client（连接生命周期归宿主）。
  - `(*Runtime).DeriveRunner(opts ...func(*osrunner.Config))`：以
    `RuntimeConfig.SandboxRunnerConfig` 为基准派生更严的 runner（副本上应用 opts），
    `SandboxRunnerConfig` 为 nil 时继承共享 runner 自己交回的配置
    （`interface{ RunnerConfig() osrunner.Config }`）；两者都拿不到就报错，不静默退回
    平台链。`Close` 之后返回 `kit.ErrRuntimeClosed`。共享 runner 不受影响。
  - `sandbox.Policy.RemoveWritableRoot(path) error`：撤销一个可写根（策略根/追加可写根/
    运行期授权都在范围内）。未知路径返回新增的 `sandbox.ErrWritableRootNotFound`；
    主根不可撤销，返回同时包装它的 `sandbox.ErrWritableRootIsPrimary`（调用方必须先判
    前者）。
  - MCP 工具不再被名字级预设过滤：预设只列内置工具名，而 MCP 工具名由服务器身份在
    运行期决定；之前默认 `personal` 预设会把宿主接进来的 MCP 工具静默裁掉。
    `MCPPrefix` 的规范化只有一份（新增 `mcp.NormalizePrefix`：去首尾空白、空串落
    `mcp__`、非 `__` 结尾补齐分隔符），工具名生成与按前缀过滤共用它——带空白的前缀
    不会再让这条修复原地复发。
  - 测试：`TestRuntimeAddRootAppliesToNewAgents`、`TestRuntimeRemoveRootRevokesExistingAgent`、
    `TestConfigRootsSupersedesRoot`、`TestRuntimeAddMCPClientExposesToolsToNewAgents`、
    `TestRuntimeRemoveMCPClientHidesToolsFromNewAgents`、`TestRuntimeDeriveRunnerDoesNotAffectExisting`
    （`kit/runtime_d2_test.go`）、`TestPolicyRemoveWritableRoot*`（`sandbox/writable_remove_test.go`）；
    复审补充：`TestRuntimeRemoveRootIsPersistentForNewAgents`、
    `TestDeriveRunnerInheritsSharedConfig`、`TestDeriveRunnerAfterCloseFails`、
    `TestConfigRootsAppendToExplicitPolicy`、`TestMCPPrefixWithWhitespaceKeepsToolsRegistered`
    （`kit/runtime_d2_fix_test.go`）、`TestNormalizePrefix*`（`tools/mcp/prefix_normalize_test.go`）。

### Removed

- **脱敏整体删除（破坏性）**：`secrets` 包（`Redactor`/`RedactForExport`/流式
  `Writer`/22 条内置模式）、`agent.NewWithRedactor`、`agent.RedactMessages`、
  `kit.Config.Redactor`、`RunnerOptions.Redactor`、`kit.NewEventSink` 的 redactor
  参数全部移除。工具结果、助手文本与推理、事件、错误、导出与日志都是**原文**——
  单用户客户端的取向是"不偷偷改写用户自己的数据"，而半吊子脱敏只会让人误以为已经
  清洗过。需要屏蔽凭据的宿主在自己的 UI/导出通道处理。
- `internal/rigdoc` 的"已删除包"守护加入 `secrets`，防止回归。
- **"保留符号但不干活"的 no-op API 全部删除（破坏性）**：上一轮 DSH 对齐把这些能力
  降级成"导出符号保留 + 语义 no-op + `Deprecated` 注释"，比删掉更糟——宿主编译通过、
  以为设了边界，运行时什么都没发生。现在它们**真的不存在**（回归由
  `internal/rigdoc` 的 `TestRemovedNoOpAPIsAreGone` 拦住）：

  | 已删除 | 替代做法 |
  | --- | --- |
  | `sandbox.WithProtectedPaths`、`(*Policy).AddProtectedPaths`、`(*Policy).ProtectedPaths`、`(*Policy).WriteDenyPaths`、`WithProtectMissingPaths`、`(*Policy).ProtectMissingPaths`、`sandbox.WorkspaceMetadataPaths`、`WorkspaceNonGitMetadataPaths` | 根内局部只读用可写根自己的 `sandbox.WritableRoot.ReadOnlySubpaths`（`WithReadOnlySubpaths`／`WithWritableRoot(root, readOnly...)`／`ProtectGitMetadata`） |
  | `sandbox.WithDeniedGlobs`、`(*Policy).DeniedGlobs` | `sandbox.WithHidden`／`Hidden`／`HiddenPatterns`（**参与判定**，读写与检索结果全拦）。`DeniedPath` 保留，但只反映根归属 |
  | `sandbox.ErrGrantProtected`；`ValidateWritableGrantPath` 去掉 `protected` 参数（现为 4 个参数） | 授权校验只看可写根、静态可写根与已授权项；要拒绝某个目录请把它排除在可写面之外 |
  | `kit.Config.DisableProtection`、`ProtectPaths`、`ProtectWorkspaceMetadata`、`kit.HomeSecretPaths` | 权限只由 `kit.Config.PermissionPreset`／`Mode`／`Approval`／`Policy` 表达；凭据保护属于部署层（容器/VM）或宿主自己的校验 |
  | `tools/full.AllOptions.ProtectWorkspaceMetadata` | 同上；`sandbox.Policy` 里不再有任何"保护"开关 |
  | `sandbox/osrunner` 的 `Profile.Protected`／`Profile.MissingProtected`、`ErrMissingProtected`、`RuntimeReport.UnprotectedPaths`／`UnprotectedWarning` | 没有保护路径就没有"缺失/未保护"诊断；OS 后端从不渲染保护规则 |
  | `Denial.ProtectedPaths`、`sandbox_status` 的 `pathProtection`／`unprotectedPaths`、`rigdoctor` 报告里的 `pathProtection`／`protected`／`missingProtected` | 拒绝归因仍给出模式、可写面、剩余预算与下一步；`sandbox_status` 用 `readOnlySubpaths` 表达仍然生效的根内只读 |

  兼容性：仓库在 pre-1.0，破坏性更新是允许的；迁移就是上表右列。`sandbox.ClassifyCommand`
  / `ClassifyShellCommand` **没有**删除——策略层不用它拦截，但 `gate.CommandRiskGate`
  与 `tools/builtin` 的逐命令提权是真的消费者。要"危险命令硬拦"用
  `gate.CommandRiskGate`，要"按权限档决定"用 `kit.PermissionPreset` + `Approval`。

### Added

- **`rig.ToolActivationStore`（C1）**：`tool_search` 的激活结果按会话持久化——`AgentSpec.ActivationStore`
  / `kit.Config.Activations`（内存实现 `kit.NewMemoryActivations()`），首次使用时惰性加载并合并
  （Run 前再确认一次），写失败只记日志不打断激活。
- **事件里的工具结果不再有损（E6）**：`events.ToolResult.Data` 携带 `rig.ToolResult.Data`
  （不可序列化时留空并补一条 notice，不打断运行）。

### Changed
- **`memory` 工具默认切到 v2 上下文后端**：动作从 6 个扩到 20 个
  （新增 `ls/tree/find/remember/forget/tag/commit/rules/stats/history/restore/mv/
  mkdir/snapshot`），旧 6 个动作语义保持；工具声明约 2.4 KB（旧 1083），细节继续
  交给运行时错误。`full.All` 无旧 Wiki/Memory 时也默认给 v2 动作面。
- 文档：`docs/MEMORY.md` 重写；`docs/CONFIG.md`、`docs/TOOLS.md`、
  `docs/ARCHITECTURE.md`、`AGENTS.md`、`docs/ai/*` 同步。

- **权限模式裁剪工具面（破坏性）**：`read-only` 档下 `kit` 不再注册纯写工具
  （`write_file`/`edit_file`/`apply_patch`/`str_replace_editor`）与 `undo`（它同样改
  工作区文件，且 `tools/undo` 不查策略），它们也不会出现在 `InitialActiveTools`、
  `Agent.Tools()` 与 `tool_search` 清单里——以前是"注册了、模型点开才被拒"，白烧
  一轮上下文与一次工具往返。`bash`/`pwsh`/`run_code` 只在**挂了 OS runner** 时保留：
  没有 runner 的 `read-only` 里策略拒绝所有命令（"shell commands are disabled"），
  留着它们是必然失败的死工具。零配置 `read-only` 因此是 35 注册 / 11 active
  （默认 `workspace-write` 仍是 40 / 16）。
  裁剪表在 `tools/presets`：`presets.ForMode(mode string, confined bool, names)` 与
  `presets.ModeFilter(mode string, confined bool)`（**破坏性签名变更**：档位从
  `sandbox.Mode` 改成字符串，`confined` 表示是否挂了 runner；`tools/presets` 因此
  不再 import `sandbox`，依赖面回到 77 个包、0 个 `mvdan.cc/sh`）。
  `workspace-write`/`danger-full-access` 与未知档位一律不裁剪。`Config.Tools` 非 nil
  （宿主自建工具集）时不裁剪，构造期告警里会写明"Mode 裁剪对自建工具集不生效"。
- **`Agent.PermissionPreset()` 与裁剪/围栏同源（破坏性）**：档位以前按 `cfg.Mode`
  推导，而工具面裁剪与围栏按 `policy.Mode()`，宿主显式传 `Policy` 时会"报一个档、
  跑另一个档"。现在统一以策略的生效模式为准（无 `Policy` 时它就是 `cfg.Mode`）：
  `Policy=read-only + Mode` 留空报 `read-only`，`Mode=read-only + Policy=workspace-write`
  报 `workspace-write`。
- **`job_start` / `terminal_open` 与 `bash` 同口径（read-only + runner）**：A4 已经让
  `bash`/`pwsh` 在挂着 OS runner 的 `read-only` 档里可用（写围栏由 runner 承担），但
  `tools/jobs` 的 `job_start` 与 `tools/terminal` 的 `terminal_open` 仍在策略层整体
  拒绝——同一条命令走 `bash` 能用、走 `job_start` 不能用。现在三者一致：`read-only`
  + 有 runner 时允许（进程仍经 `Runner.Confine` 包装，校验改用
  `sandbox.CheckConfinedCommand`，命令 allow/deny 名单与 workdir 围栏照旧；
  `job_start` 的命令文本解析路径与 `bash` 不同，`/bin/sh -c` 由
  `ParseShellCommand` 解析后校验同一个 argv）；没有 runner 时仍然拒绝，错误文案
  与 `bash` 一致地说明"缺的是 runner"以及两条出路。`terminal_send` 的逐行校验在同
  一组合里同样改用 confined 变体。
- **`session/sqlite` 的 `ImportRows` 改按 ordinal 对账**：`firstDivergence` 以前按
  切片位置比对，删过消息（库内留空洞）的会话再导入同一批就会被判 `Conflicts`，而且
  永久不可续导。现在对账键是"源 ordinal → 库内 ordinal"的**集合**比较：源里有、库里
  没有的位置按该 ordinal 补齐（`insertMessagesAtOrdinals`，高水位仍由 `nextOrdinal`
  推进到 > 最大值），库里多出来的位置保留、不算冲突；只有"同一个 ordinal 上内容
  不同"才记 `Conflicts`，且一条都不写。`ImportDir` 的续跑语义不受影响。

### Fixed

- **`read-only` 的可写面口径三处对齐**：`Preflight` 以前说"只能改 `<workspace>`（以及
  私有临时目录 scratch）"，但那一档工作区不可写。现在文案、`Policy.WritableRootsFor`
  与 `sandbox_status.writableRoots` 同口径：只有私有 scratch（没有 scratch 就明说
  "没有可写目录"）。
- **Windows `read-only` + scratch 真的可写了**：helper 侧 `read-only` 的 `--workspace`
  只做校验、不授权，于是 scratch 一直不可写，而 Profile/Preflight/status 都宣称可写。
  现在把 scratch 作为唯一工作区、以工作区写形状下发（= "除 scratch 外只读"）。
- **`withScratchEnv` 不再去重无关环境键**：`PATH=/a, PATH=/b` 在设置 scratch 后会丢掉
  `/b`；现在只有 `TMPDIR`/`TMP`/`TEMP` 被覆盖，其它键（含重复）原样保留。
- **`sandbox_status` 不再把建不出来的 scratch 说成可写**：`scratchError` 存在时
  `tempWritable.writable=false`、`writablePath` 为空，`writableRoots` 也不再列它（与
  osrunner profile 的 `Dropped` 一致）。
- `Policy.WritableRootsFor("")` 用策略自身模式（不再被当成 `read-only`）；
  `WithScratchDir` 绝对化失败时 `ScratchDir()` 返回空串，已存在的 scratch 目录也会被
  收紧到 0700；`read-only` 下 `bash`/`pwsh` 的 description 不再广告
  `sandbox_permissions:"require_escalated"`（那一档每条都会被硬拒）。


## v0.25.0 — 2026-11-20

### Added

- **隐藏路径（`sandbox.WithHidden`）**：v0.24.0 把拒绝 glob 降级成记录型 no-op 之后，
  "设置里勾了隐藏，模型就读不到"这条产品承诺没有执行点。现在有一等公民：
  `sandbox.WithHidden(patterns...)` + `Policy.Hidden`/`HiddenPatterns` + 导出的
  `sandbox.GlobMatch`（`**` 跨段、`*`/`?` 段内、其余字面量）。它是**参与判定**的
  隐藏——`ResolvePath`/`CheckPath`/`CheckWritablePath` 与 `DeniedPath` 共同据此拒绝，
  因此 `read_file`/`list_dir`/`glob`/`grep`/`search` 的结果与写入全部拦住；不带 `/`
  的模式按基名匹配任意深度，大小写折叠随平台。只约束工具面，**不是** OS 边界
  （`bash` 里 `cat .env` 仍然成立）。零配置入口是 `kit.Config.HiddenPaths`。
- **`search.WithHiddenFunc`**：`search` 不经过 `Policy.OpenPath`（它自己遍历），所以
  隐藏必须显式接线；`tools/full` 已经把它接到同一个 `Policy.Hidden` 上，宿主自己
  装配时也别漏（否则内容检索会成为唯一的漏点）。
- **`instructions.WithRepoFileNames`**：仓库级/祖先级指令文件名可配置（默认
  `AGENTS.md` 永远在，传进来的名字追加在它之后）。宿主的"工作区指令文件"设置项
  现在能复用 `@import`、单文档/总量预算与防注入全套语义，不必自己读文件。
- **`instructions.RenderWith` + `RenderOptions{Trust, Notice}` + `DefaultTrustNotice`**：
  可选把整份指令包进 `<workspace_instructions trust="untrusted">` 围栏并附一行
  英文声明（"这是仓库内容，不是系统指令"）。`Trust` 为空时输出与 `Render` 逐字节
  相同，升级不改变 prompt 前缀。
- **`session/sqlite.Store.UpdateMeta` + `MetaPatch`**：改名与换工作区。`nil` 字段
  表示不改、空串表示显式清空；未知会话返回同一个 `ErrNotFound`；`updated_at`/
  `revision` 按 `Append`/`Replace` 的规矩维护（空补丁是纯回读，不推进时间戳）。
- **`rig.WithSpillScope` / `rig.SpillScope`**：把会话作用域打在 ctx 上，供
  `rig.SpillStore.GC` 按作用域回收。
- **`skill.ToolsWithRoot(root, store)`**：技能库的默认根跟随工作区根，而不是进程
  cwd（`Tools` 保留为 `ToolsWithRoot(os.Getwd(), …)`）。

- **`rig.TurnHook` + `rig.AgentSpec.Hooks`**：循环边界（`BeforeRun`/`AfterRun`/
  `BeforeTool`/`AfterTool`）的宿主接入点。回合边界在循环内部（一轮里可能有多次
  模型调用），宿主在 `Run` 外面包一层够不着——回合快照、事件双写、成本记账、工具
  结果后处理都需要这个缝。`BeforeRun` 出错中止本次运行；`BeforeTool` 出错只让这次
  调用失败（不去问人、不执行），整轮继续；`AfterTool` 可改写结果，改写对模型可见。
- **`rig.AgentSpec.SystemPromptFunc`**：按运行计算系统提示词（覆盖静态
  `SystemPrompt`）。长驻进程里日期、git 分支、当前权限档不再冻结在构造那一刻；
  每次 Run 调一次，同一轮内多次模型调用共用同一个值（前缀稳定）。返回空串表示
  本次没有系统提示词。
- **`kit.Config.Jobs` + `Agent.Jobs()`/`Terminals()`/`Tool(name)`**：宿主注入的
  `jobs.Manager` 现在**就是**工具在用的那一个（旧实现只把句柄登记给 `Shutdown`，
  工具在 kit 自建的管理器里跑，宿主手里的是空管理器）；`Agent.Tool(name)` 让宿主
  能按名字直接调用某个已注册工具。
- **`rig.ToolResult.Data`**：工具的可选结构化结果，只出现在
  `EventToolResult.Result` 里、不进模型消息。`jobs` 的五个工具已填
  （`job_output` 带 `output_offset`/`next_offset`/`output_truncated`，`job_list`
  带 `{"jobs": [...]}`），宿主不必再解析人读文本。
- **`jobs.JobSnapshot` + `Manager.Snapshot()`**：后台任务的宿主视图（id/命令/状态/
  退出码/起止时间/已产出字节），按开始时间稳定排序。
- **`mcp.SanitizeName` + `mcp.MigrateLegacyNames`**：清洗规则导出（宿主复刻
  `disabledTools` 白名单不再随规则漂移）；资源/提示工具的旧名映射（推不出 server
  的进 `needsUser`，未知名不静默丢）。
- **`session/sqlite`：检索解码列**：新增 `rig_messages.search_text`（正文 + 推理 +
  工具名 + tool_call 参数），`SearchMessages` 改走它。旧实现对原始 JSON 载荷做
  `LIKE`，正文里的 `"`、`<`、`>`、`&` 与换行**永远搜不到**；老行用原载荷兜底，
  `Store.BackfillSearchText(ctx)` 一次性补齐（幂等）；`SearchQuery.Offset` 支持
  跨会话翻页。
- **`session/sqlite`：成本维度**：`CostEntry`/`CostFilter` 增加 `WorkspaceID` 与
  `RunID`，新增 `CostBySession`/`CostByWorkspace` 聚合；删除会话连带删除它的花费行
  （没有外键，同一事务里显式清）。

- **`session/sqlite`：导入/导出对称**。`ImportRows(ctx, rows)` 是不依赖
  `store.Dir` 的通用导入（元数据 + 消息 + 可选事件/花费/检查点，幂等、冲突定位到
  条号）；`ExportOptions.IncludeEvents/IncludeCosts/IncludeCheckpoints` 让导出带上
  这些面（版本化 JSONL，各带头行）；会话标题/工作区写进 `sessions.meta.jsonl`
  清单，`CompareSessions` 把标题也纳入比较——往返不再丢标题。
- **`session/sqlite`：记忆按工作区分区**。`Options.Workspace` 非空时
  `Store.Memory()` 只读写该工作区（`rig_memory_pages.workspace_id`，物理主键拼前缀，
  两个工作区可各有一页同名页）；空 `Workspace` 保持"全部可见"。

- **`present` 的两种形态**：文件（`rig.FilePresenter`）与**内容产物**（`title`+`body`
  → `rig.Presenter`，宿主渲染成卡片/文档）。`rig.Presenter` 因此取消废弃——它与
  `FilePresenter` 互补，不是新旧替代；`present_artifact` 工具仍不提供。
- **goal/plan 注入入口**：`kit.Config.Goals`/`GoalRevisions`/`PlanController` 非 nil 时
  保留对应工具（旧版 `goal` 只需 `Goals`；`get_goal/create_goal/update_goal` 需要
  `GoalRevisions`，`hosttools.NewMemoryHost()` 两者都满足），不再无条件摘掉。
- **系统提示词区段**：`kit.Config.PromptSections` 按序渲染在 persona 之后、
  `<environment>` 与仓库指令之前（顺序即契约：不可信内容排最后）。
- **聚合存储入口**：`kit.Config.Aggregate`（`storage.Storage`）一次给出
  History/Checkpoints/Memory/Spill，显式字段优先，并自动进 `Shutdown`。
- **文件型溢出存储 `spillfile`**：`spillfile.New(dir)` 实现 `rig.SpillStore`（文件名带
  作用域前缀、`Save` 原子写、`Load` 拒路径穿越、`GC` 只回收同作用域、空作用域独立成桶）。
- **发布面自洽检查**：`scripts/check-submodule-release.sh` + `make submodule-tags-check`
  检查根 tag / 子模块 tag / `go.mod` 的 `require` 三者一致（发版前必跑）。

- **事件桥导出**：`kit.NewEventSink(w, redactor?)` 把 `rig.Event` 映射成 NDJSON
  `events.Envelope`（`spec.Emit = sink.Emit`，错误读 `sink.Err()`），不经过 kit 装配的
  宿主不必自己抄一份映射表。

### Changed

- **`apply_patch` 没有策略就不可用**（破坏性）：不传 `patch.WithPolicy` 的实例，
  每次调用返回 `apply_patch requires a sandbox policy: construct it with
  patch.WithPolicy(policy)`，**什么都不写**。旧行为是"退化成只做工作区根围栏"，也就
  是漏一行接线就静默绕过只读模式与根内只读子路径。`New` 的签名没有变。
- **溢出回收按会话作用域**（行为变更）：`rig.SpillStore.GC` 只回收与 ctx 同作用域的
  条目，`session/sqlite` 的 `rig_spill` 因此多了一列 `scope`（core schema v5）。空作用域
  **只**回收空作用域的行（不是全库通配符）：老数据留在这一桶里，仍可读，但不会再被
  按会话的回收清掉——长期驻留需要一次性迁移或宿主自己清理。

- **注入的后台管理器是空的**：`kit.Config.BackgroundJobs` 只登记关停、不注入工具，
  于是宿主拿到的 `jobs.Manager` 永远没有任务（UI 只能解析 `job_list` 文本）。现在用
  `kit.Config.Jobs` 注入，`Agent.Jobs()` 返回的就是工具在用的那个实例。
- **跨会话检索漏命中**：带引号/尖括号/`&`/换行的正文与 tool_call 参数搜不到（见上
  的 `search_text` 解码列）。
- **删会话留下孤儿成本行**：`rig_costs` 没有外键，旧 `Delete` 只删 sessions+events，
  `CostTotals` 仍会统计已删会话的花费。

- **v9 删掉的检索索引会被自愈路径重建**：`repairStatements` 漏了
  `DROP ... IF EXISTS`，第二次 `Open` 把 v6 的 `CREATE INDEX` 再跑一遍，索引又回来
  了（现在自愈按段重放三类幂等语句，测试覆盖 Open→Close→Open）。
- **显式改/清标题后自动标题不再覆盖**（补上一次修复的回归）：用
  `message_count = 0` 当"用户没碰过标题"的代理会漏掉"助手开场 / 首条用户消息只有
  图片"的会话——它们永远拿不到标题。改成 `rig_sessions.title_cleared` 标记列
  （core schema v11）。
- **围栏闭合标签的所有变体都被中和**：`</Workspace_Instructions>`、`</ x >` 前带
  空白的写法以前能穿过（只挡了小写与全大写两种）；现在大小写不敏感 + 允许空白。
- **`job_list` 的文本行与 `Data` 也改为单次快照**（上一轮只修了另外四个 job 工具）。
- **`AfterRun` 的 panic 默认也会有信号**：没有 `Logger` 时退回 `slog.Default()`，
  不再完全静默。
- **升级回填检索列改为分批**：按 `(session_id, ordinal)` 游标每批 500 行，避免老库
  首次打开把全部 payload 读进内存；失败走日志而不是 `_, _ =`。
- **`search_text` 补上 tool_call 的 `id`**（payload 兜底能搜到、解码列曾漏掉）。
- **`kit` 的"注入即失效"告警**：`Jobs` 实际生效却曾列进 ignored，`Models`/
  `MCPPrefix` 漏列。

### Fixed

- **回合钩子的收尾与隔离**：`AfterRun` 现在**一定**被调用（成功/失败/取消，以及
  `BeforeRun` 拒绝的那次——`RunResult` 为零值），宿主的回合不会永远开着；`AfterRun`
  里的 panic 被隔离成一条日志，不再让调用方丢掉已经算好的结果。`BeforeRun`/
  `BeforeTool` 的 panic 仍然向上抛（那是策略实现坏了）。
- **子 agent 不再继承父的 `SystemPromptFunc` 与 `Hooks`**：前者优先级最高，会把
  `SubagentSpec.SystemPrompt`、`SystemPromptAppend` 与 structured-output 指令全部
  盖掉；后者会让父会话的快照/记账为子 agent 的运行再触发一次。
- **`search` 的 mentions 模式遵守隐藏规则**：`@.env` 以前仍被确认存在并回显（files/
  text 已过滤）——现在按"缺失"处理；对应测试原来只断言"不含文件正文"，恒真。
- **`job_*` 的文本状态与 `Data` 同源**：`job_start`/`job_kill`/`job_wait`/`job_output`
  以前各自快照一次，命令极快结束时会出现"文本 running / Data exited"的矛盾视图。
- **`instructions.RenderWith` 的围栏闭合**：正文里含 `</workspace_instructions>` 时
  围栏会被提前闭合（声明掉到围栏外）——现在中和成字面量；`Trust` 非空但净化后为空
  （例如 `"!!!"`）时回退固定 token，而不是静默不加围栏。
- **`Config.Tools` 非 nil 时 `Config.Jobs` 不再被静默丢弃**：句柄仍可由
  `Agent.Jobs()` 读回并随 `Shutdown` 关闭（job_* 工具不出现，告警里会说明）。
- **`tools/full` 的 skill 根**：`AllOptions{Policy}`（不带 `Root`）形态下项目级技能
  不再静默消失——技能根与 patch/search/undo 用同一个 `workspaceRoot`。
- **`session/sqlite` 的 `Delete` 清掉该会话作用域的溢出行**：作用域化 GC 之后这些
  ≥16KiB 的正文再没有回收路径，会无界增长。
- **显式清空的会话标题不再被下一次追加恢复**：自动标题只在 `message_count = 0` 时
  生效，`UpdateMeta(Title: &"")` 的承诺因此成立。
- **升级打开时自动补齐解码列**：从 v6 之前的库升级的那一次 `Open` 会跑一次
  `BackfillSearchText`（此后不再扫表）；`searchTextFor` 补上 image URL 与
  `tool_call_id`，新行不再比老行的 payload 兜底搜得少。
- **`session/sqlite` 自愈路径补 `ADD COLUMN`**：`rig_messages` 被手工删掉后，段 1 用
  v1 形状重建表，后续段引用新列的索引会报 `no such column` 让 `Open` 永久失败；
  自愈现在也重跑 `ALTER TABLE ... ADD COLUMN`，且对"已经是最新版本"的库同样执行。
- **删掉 `rig_messages_search_text` 索引**（v9）：唯一使用者是前导通配的 `LIKE`，
  SQLite 用不上（EXPLAIN 是 SCAN），留着只给每条消息写入加一次索引维护。

- **`search` 在配置了隐藏之后仍能读到隐藏文件**：`tools/full` 装配时没有把策略判定
  接进 `search`（它自己遍历，不走 `Policy.OpenPath`），于是"隐藏 `.env`"之后
  `mode=text` 照样以 `path:line:content` 把内容吐出来。
- **`session/sqlite` 的溢出回收删掉别的会话的句柄**：`GC(keep)` 以前是全局裁剪，而
  agent 每存一条就调一次，于是会话 A 的回收让会话 B（或子 agent）手里的 `ref:` 失效
  ——句柄失效就是模型的工具结果永久丢失。现在与文件后端的按作用域分区行为一致，
  并补了 v4→v5 的迁移用例（老行 `scope=''`、不被误删）。
- **`tool_search` 的截断提示是中文**（`…还有 N 个，用 query 缩小范围`）：模型可见
  文案必须是英文，改为 `…and N more; use query to narrow the results`。
- **`skill` 的默认库忽略 `AllOptions.Root`**：守护进程 / GUI 这类 cwd 与工作区无关的
  宿主里，`<Root>/.zeokeel/skills` 被静默跳过（与 `tools/full` 文档不符）。

## v0.24.1 — 2026-11-20

### Changed
- **发布收敛为"推 tag"一步**：`release.yml` 在所有门禁通过后**直接发布** Release
  （不再产生草稿，`publish-release.yml` 已删除）；`v*` tag 不再要求签名
  （`.github/allowed_signers` 已删除）。发版动作就是 `git tag -a vX.Y.Z` +
  `git push origin vX.Y.Z`，随后 CI 自己发布。文档（RELEASE/VERSIONING/ARCHITECTURE/
  SECURITY/AGENTS）与各处的 `v0.23.4` 引用同步到当前版本。

## v0.24.0 — 2026-11-20

### Changed
- **MCP 资源/提示工具名带 server 身份**：`mcp.ResourceTools` 现在产出
  `mcp__<server>__list_resources` / `__read_resource` / `__list_resource_templates` /
  `__list_prompts` / `__get_prompt`（server 来自 `client.ServerName()`，清洗规则与普通
  工具一致），`mcp.ResourceToolName` / `mcp.ResourceToolNames` 可直接算出名字；
  `mcp.ServerTools.Resources` 让 `AggregateTools` 把资源工具与普通工具放进同一个名字
  空间，多连接（即使 `ServerName` 相同）也不会重名。**破坏性**：旧名
  `mcp_list_resources` 等**不再存在**——它们丢掉了 server 维度，配两个 server 时逐字
  重复。`tools/full` 因此删掉了装配层的改名兜底（`uniquifyToolNames`），直接使用
  `mcp.AggregateTools`。
- **轮数默认不再设上限**：`kit.DefaultSafeMaxIterations` 从 200 改为 0，`kit.Config`
  与 `kit.Profiles()` 都默认不限轮数（与 `agent` 层一致）；`MaxIterations > 0` 仍是
  显式上限，负数同样表示不限。轮数护栏属于人为边界，成本控制交给 `costmeter`
  （`Meter`/`MaxRunCostUSD`）与人工审批。

### Added
- **审计批次新增导出面**：`mcp.NewToolsChecked` / `mcp.AggregateTools` / `mcp.ServerTools` /
  `mcp.NameCollision` / `mcp.NameCollisionError`；`store.ErrSessionIDConflict`；
  `session/sqlite` 的 `ImportReport.Conflicts`；`jobs` 响应里的 `output_offset` / `next_offset` /
  `output_truncated` 三个字段；`team.Manager.SendAsyncContext`（旧 `SendAsync(ref, msg)` 保留为
  Deprecated 包装）。

- **DSH 对齐的权限预设（四档）**：`kit.Config.PermissionPreset` 一次性设置 `Mode` 与
  `Approval`，取值 `read-only` / `workspace-write` / `danger-full-access` / `auto`；
  `kit.Presets()` 返回名字、说明与两个旋钮，`Agent.PermissionPreset()` 报告**当前生效**
  的档位（旋钮值不匹配任何预设时为 `custom`，可展示但不可选）。显式设置的
  `Gate`/`Mode`/`Approval`（或 `Policy`）优先，预设不静默覆盖任何字段。
- **`auto` 档的自动预审 seam**：`kit.Config.ToolReviewer`（`gate.ToolReviewer`）。
  `auto` 档下每次待批调用先由审查模型判断——放行即执行，**明确拒绝或审查失败
  （超时/出错）都回退到人工 ask**；没有配 reviewer 时按纯 `ask` 运行，`Preflight`
  会说明这个降级。

- **`memory`（公共契约）与 `rig.SpillStore`**：把"记忆页"与"超大工具结果"的存储
  契约从 `internal/` 提到公共包，任何后端都能实现——`session/sqlite` 已经实现了
  两者（`Store.Memory()` / `Store.Spill()`）：记忆是一张 `rig_memory_pages` 表
  （索引只读四列元数据、检索命中标题/摘要/正文但只回元数据、覆盖写记录旧正文大小、
  append 写入 `## Log` 段），溢出结果是一张 `rig_spill` 表（句柄代替路径、
  `GC(keep)` 按写入顺序保留最新 N 条）。`SpillPolicy.Store` 为空时行为与之前完全一致。
- **`session/sqlite`（独立子模块）**：把会话持久化放进一个 SQLite 数据库——消息、
  事件、检查点、花费各一张表，索引在旁边；实现 `store.Store` + `rig.History` +
  `rig.CheckpointStore` + `rig.SessionLog`，并提供跨会话消息检索、事件检索、
  `ImportDir`（NDJSON 幂等迁移，支持 dry-run）与可扩展表结构（`Options.Extensions`，
  只应用一次并记账）。**数据库路径由宿主传入，本包不自己挑位置。** 放在独立
  module 里是因为主模块的依赖门禁只允许一个解析器依赖：想要 NDJSON 的宿主依赖面
  不变，想要数据库的宿主 `go get` 这一个子模块即可。文档见 `docs/sqlite.md`。

### Changed

- **联网工具默认可用（放宽防护，沙箱是唯一防护层）**：`kit.Config.AllowNetwork` 改为
  三态（`nil` = **开**，`kit.Bool(false)` 才关）。零配置下 `web_fetch` 不再被"零配置
  体检"摘掉（零配置注册 39 → **40**；接 `Config.Presenter` 后 41 / 17），配了
  `Config.Searcher` 时 `web_search` 照旧加入。网络本来就不在沙箱承诺内，能不能联网是
  宿主给了什么工具的问题；安全边界仍在沙箱（可写根围栏）+ 人工审批。
  `PTC` / `GitUndo` 不受影响——它们仍是额外能力的 opt-in，不是被限制的能力。

- **审批默认不再超时**（与 DSH 的 approval seam 一致）：`gate.Ask` 的默认
  `timeout` 是 0——人多久不回就等多久，只有取消本轮才撤回请求。需要上界时设
  `kit.Config.AskTimeout`（`>0`），超时以 `gate.ErrApprovalExpired` 收尾。
- **审批门禁从三层变两档**：`gate.PersonalDefault` / `PersonalAsk` 现在是
  `AllowThenAsk(SandboxReadOnlyTools, inner)`——只读工具免问 + 其余问人，
  **不再内置 `CommandRiskGate`**。`rm -rf /`、`mkfs`、`curl … | sh` 在 `ask` 档下与
  其它可变命令一样走人工审批；判据只有一条：谁能批准，谁就有权决定。
  `gate.CommandRiskGate` 与 `sandbox.ClassifyCommand` 仍然完整保留，宿主可自行组合
  （要用就必须叠在 `AllowThenAsk` 之外）。
- **`Approval: "auto"` 现在是全放行**：`gate.PersonalAuto()` == `gate.AllowAll()`，
  与 DSH 的 `approval=never` 同义（不问人、也不做命令分类）。需要危险命令硬拦的宿主
  请显式组合 `gate.CommandRiskGate`。
- **沙箱策略层只做可写根围栏**（DSH 对齐）：可写面由 `WritableRoot` +
  `WritableRoot.ReadOnlySubpaths` 一处决定；`read-only` 的正确语义是"只有 `/dev/null`
  可写"（不再有可写 `/tmp`），`workspace-write` 是"workspace + 私有 `/tmp`"，
  `danger-full-access` 透传（`Enforcement=none`）。
- **`sandbox/osrunner` 的渲染与 DSH sandbox-local 逐 token 一致**
  （`conformance_dsh_test.go` 钉死四个后端）：不再渲染 `--cap-drop ALL`、
  `--remount-ro /dev`、`--unshare-net`、seccomp 过滤器、宿主守护进程 socket 遮蔽、
  工具链缓存根、`/var/tmp`、SBPL 的 pty/fcntl/user-preference 规则、以及保护路径的
  只读规则。网络、读范围、进程可见性、设备访问都不在沙箱承诺内（部署层负责）。
- **已沙箱化命令免审批不再看命令文本**：`AutoAllowSandboxedCommands` 的判据只剩
  "shell 类工具 + runner 自报能强制 + 策略不是 danger-full-access"。

- **文档体系重构（中文单一事实来源）**：删除与代码脱节的英文/中文双轨镜像
  （`docs/zh/`）、历史审计快照（`docs/audit/`、`docs/AUDIT-REPORT.md`）与规划稿
  （`docs/PLATFORM*.md`、`docs/MODULES.md`）；`docs/*.md` 统一为中文正式文档。
  `README.md` 重写为项目门面（定位与边界、分组特性、安装、可编译的快速开始、
  核心概念、使用指南、配置速查、安全、平台、完整包清单、CI 门禁、文档索引）；
  新增 `docs/README.md`（索引）与 `docs/GETTING-STARTED.md`（安装/三种组装/排障）；
  `AGENTS.md` 重写为“仓库规则 + 按模块逐个说明”；`docs/AI-SDK.md` 从 `docs/zh/`
  移到 `docs/` 并更新全部计数与 API；`README.en.md` 缩为英文入口并指向中文文档。
  所有工具计数统一为四个口径（零配置 39/16、`full.All` 81/83、账本 83、预设
  55/73/8），并由 `internal/rigdoc` 的 `TestDocumentationCounts` 校验
  `docs/COVERAGE.md` 的权威行。
- **CI 聚焦代码**：主 CI 只保留代码门禁（gofmt/build/vet/test/race/覆盖率/fuzz/
  benchmark/依赖与 cgo 策略/跨平台编译/工具账本与审计/示例编译）。移除两项文档检查
  （`go run ./cmd/rigdoc -check`、README 与 `examples/quickstart` 的逐字节比对）与
  版本/tag 检查 job（`submodule release tags`）；`make ci` 同步只跑代码门禁。
  文档生成物、README/示例同步改由本地目标（`make ai-docs-check`、`make quickstart`）
  自查，版本与 tag 一致性由发布流程（`release.yml` 的 `verify`/`smoke`）校验。
- 文档中的平台与 CI 描述按实际工作流更正：Windows 有阻塞的真机
  `windows sandbox (real Windows)` job；macOS 真机在独立 `macos-sandbox.yml`
  （不阻塞）。

### Removed

- **保护路径**：`sandbox.WithProtectedPaths` / `AddProtectedPaths` / `ProtectedPaths`
  不再参与任何判定（`ProtectedPaths()` 恒返回空），导出符号保留并标 `Deprecated`；
  `kit` 不再追加工作区元数据与主目录凭据保护，`kit.Config.DisableProtection` /
  `ProtectPaths` / `ProtectWorkspaceMetadata` 退化为 no-op（保留仅为源码兼容）。
  仍然生效的"根内只读"只有 `WritableRoot.ReadOnlySubpaths`。
- **拒绝 glob**：`sandbox.WithDeniedGlobs` / `DeniedGlobs` 只记录、不拦截
  （`ResolvePath`/`CheckPath`/`CheckWrittenPath` 不再按它拒绝，文件工具也不再隐藏
  命中项）。
- **"硬链接守卫"作为策略层承诺**：本包从未实现 inode 级判定，文档里的这一条已删除；
  检索层的 `search.WithHardlinkGuard` 仍然保留，由那个包自己的开关控制。
- **OS runner 的网络隔离、socket 遮蔽与工具链缓存**：删除
  `sandbox/osrunner/seccomp_{linux,other}.go`、`daemon_sockets.go` 及对应证据测试；
  `ErrMissingProtected` 的"保护路径缺失即拒绝执行"路径随保护路径一起移除。
- **`cmd/rigdoctor` 的 `-protect` / `-deny` 开关**：两者不再影响任何强制逻辑。

### Fixed

- **审计 P1 批次（12 条，TDD 修复）**：
  - `str_replace_editor` 的 `create`/`str_replace`/`insert` 现在会拒绝落在
    `WritableRoot.ReadOnlySubpaths` 里的路径（此前只查 `ReadOnly()`，是唯一绕过该围栏的
    写工具；`view` 仍可读）。
  - `sandbox` 的运行期声明 API（`DeclareWritableRoot`/`AddReadOnlySubpaths`）加锁：
    并发读 + 声明此前会 `fatal error: concurrent map read and map write`。
  - `run_code` 的解释器改用环境白名单（`Policy.CommandEnv`），不再继承宿主全部环境变量。
  - MCP 工具名清洗后撞名不再让 `agent.New` 起不来：冲突项加确定性后缀 `__2`/`__3`，
    新增 `mcp.NewToolsChecked`（可诊断错误）与 `mcp.AggregateTools`（跨 server 去重）。
  - spill 按 `session` 作用域分区回收（文件名带 `spill.<scope>.` 前缀），不再跨会话互删；
    `kit` 默认目录改为 `<Root>/.rig/spill`（可读回）；`ctx` 取消即不写盘。
  - `job_output` 达到单次上限时改为**分页续读**：`consumed` 只推进到实际交付字节，
    响应带 `output_offset`/`next_offset`/`output_truncated`，尾巴不再永久丢失。
  - 前台 `bash`/`pwsh` 超时会杀掉**整个进程组**（POSIX），孙进程不再在工具返回后继续写。
  - `send_message` 在队友运行期间不再阻塞（投递走独立收件箱，不碰 `callMu`，且尊重 ctx）。
  - `tools/search` 的 CJK 分词改为 unigram + 相邻 bigram，中文查询不再系统性零命中。
  - `session/store` 增加会话文件归属校验（`store.ErrSessionIDConflict`），大小写/符号链接
    别名不再串线到别的会话文件。
  - `session/sqlite` 的 `ImportDir` 续跑只追加 `messages[N:]`，不再把整份历史重放一遍。
  - `provider/local` 对缺失 `index` 的 tool_call 改为按 `id` 归并（再按 `name`），
    无法归属时报错而不是静默把参数拼成非法 JSON。
- **`background.Job.Read` 的截断标记**：按偏移读之后它曾"静默短一截"；现在超过单次上限时
  补回 `[truncated: output exceeded N bytes; next offset: N]`。
- **`present` 工具拿到 Presenter**：此前 `kit` 把 `Config.Presenter` 只用于门控与初始
  active 集，没有传给 `full.AllOptions.Presenter`——`present` 被保留却每次调用都返回
  "the host did not provide a rig.FilePresenter implementation"。
- **压缩摘要链**：`WindowCompactor` / `LLMCompactor` 不再丢弃上一次的压缩摘要——
  本轮无需裁剪时原样保留，需要裁剪时把旧摘要正文与计数**并入**新摘要（例如
  `40 → 46`，而不是只报本轮的 6 条）。摘要不再在"窗口没超但整体超预算"时凭空消失。
- **检查点 `PriorMessages`**：压缩会用压缩器输出整体替换 conversation，旧实现可能把
  `PriorMessages` 写成大于 `len(Messages)`；现在按最终消息列表夹紧，满足
  `rig.Checkpoint.Validate` 的 `0 <= PriorMessages <= len(Messages)`。

- **`kit.Config.Tools` 不再被零配置体检误伤**：宿主传入自己的工具集时，kit 不运行
  `cfgUnavailableTools` 体检、也不按“没有回合快照”摘掉 `undo`——那些工具是宿主的
  选择，kit 无从判断其依赖。此前 `Tools` 非 nil 时仍会静默摘掉 `web_search`/
  `present`/`ask_user`/`web_fetch`/`undo` 等名字，与“自建工具集原样保留”的告警
  语义矛盾。新增回归用例 `TestAuditCustomToolsAreNotHealthChecked`。
- **LSP 写失败会把 client 标记为已终止**：服务端启动即崩溃时，向 stdin 写入可能在读循环
  观察到 EOF 之前就返回 EPIPE；此前 `request`/`notify` 直接返回 send 错误、`Err()` 仍是
  nil，调用方会把“初始化失败”误当成可重试错误。现在写失败统一经
  `transportFailure` 调用 `markClosed`（幂等、保留第一个归因），`TestServerCrashAtStartup`
  不再依赖时序。
- 修正与实现不符的包文档：`kit`（`SandboxGrants` 默认开、三层 `ask`、
  `CommandAllowlist` 为空时不限制命令名、`sandbox_status` 在只读白名单里）、
  `tools/full`（`NewRuntime` 是推荐入口、删除已移除的 settings/report）、
  `tools/catalog`（旧别名未移除）、`tools/terminal`（Windows ConPTY）、
  `rig.Presenter` 标为 Deprecated（`present_artifact` 已删除）、
  `agent` 的 active 顺序说明；测试与注释不再引用已删除的规划/审计文档。

## v0.23.4 — 2026-11-18

真机 macOS 验证从阻塞链路里拆出去（runner 侧故障，非我们的代码）。

- **macOS job 被系统性取消**：`started_at` → `completed_at` 恰好 15 分 02 秒且 **steps 为空**
  （一个步骤都没执行），而同一天早些时候同一个 job 96 秒跑绿。已用 `-timeout` 排除了
  "用例挂住"（真有挂住的用例，Go 会先打印全部 goroutine 栈并失败退出）。
- **被取消的 job 无法用 `continue-on-error` 中和**（取消不是失败），会连累整个 run 变红。
  因此把真机 macOS 验证搬到独立 workflow `macos-sandbox.yml`：它自己跑、自己红绿，
  不阻塞 main 与发布；runner 恢复稳定后可并回 `ci.yml` 并恢复为阻塞门禁。
- 保留 `-timeout 8m`：下次真挂住时会带着 goroutine 栈失败，而不是被静默取消。

## v0.23.3 — 2026-11-18

真机 macOS job 的 runner 选择修正（`v0.23.2` 的 tag run 因 macOS job 两次排队后被取消而
未生成 draft release）。

- **`macos-15-intel` → `macos-26`**：Intel 镜像资源更多（4 vCPU / 14 GB vs arm64 的
  3 vCPU / 7 GB），也确实跑绿过，但**排期容量不稳定**——同一个 commit 的 tag run 上连续两次
  排队后被取消，发布 gate 无法收敛。资源够用即可，可用性优先，回到主流 arm64 镜像；
  镜像版本仍然**固定**（不用 `macos-latest`，避免随退役/迁移漂移：macOS 14 已于 2026-11-02 退役）。

## v0.23.2 — 2026-11-18

安全修复版本：`v0.23.1` 之后又修掉一个"保护承诺失效"的缺陷，并让 CI 结论可复现。

### Fixed（保护承诺，F3）
- **保护路径"构造时不存在"不再被静默放行**：此前 `BuildProfile` 只渲染存在的保护项，
  不存在的仅进 `Profile.MissingProtected`（只报告不拦），于是在未被保护的子目录里
  `git init` 出来的 `.git` 在该次运行中**完全不受保护**——"工具说保护了 `.git`" 与事实
  不符。现在按后端能力分开处理：
  - **macOS（Seatbelt）**：直接把不存在的路径渲染成 `(deny file-write* (subpath …))`
    ——SBPL 按访问时求值，不需要拒绝执行；
  - **Linux bwrap/Landlock、Windows ACL**：`Confine` 拒绝执行并返回
    `osrunner.ErrMissingProtected`（`errors.Is(err, sandbox.ErrSandboxUnavailable)` 成立），
    错误里列出具体路径并说明"继续执行会让它们不受保护"；
  - 宿主可用新选项 `sandbox.WithProtectMissingPaths(false)`（**默认 true**）退回"只报告"。
- **区分"必须保护"与"尽力而为"**：只有**显式声明**的保护路径缺失才拒绝执行；git 控制面
  的 `config.worktree`/`info/`、缓存目录里的 `.npmrc` 等"将来可能出现"的子路径仍然只报告
  ——否则正常仓库里每条命令都会因为 fail-closed 跑不起来。
- **诊断**：`sandbox_status` 新增 `unprotectedPaths` 与 `unprotectedWarning`（"这些保护路径
  还不存在，本次运行不会被保护：…（初始化后重新运行即可）"）。
- **kit 默认保护改为"只登记当前存在的元数据"**：默认保护是防误伤，声明一个还不存在的
  位置既没有效果、又会让 runner fail-closed 卡死干净机器上的第一条命令；profile 每条命令
  重建，之后出现的 `.git`/`.aws` 会在下一条命令被自动纳入保护。

### Fixed（CI 可复现性）
- **发布 workflow 固定到 `ubuntu-24.04`**：GitHub 自 2026-10-19 起把 `ubuntu-latest` 迁移到
  Ubuntu 26，而发布 gate 的结论必须可复现——跑在哪个发行版上应当是显式选择，不能随 runner
  升级漂移（`release.yml` 的 verify/smoke/draft-release 与 `publish-release.yml`）。
- **三条用例改为自包含**：它们此前隐含依赖 runner 的 `$HOME`（断言"恰好存在哪些凭据文件"）
  或路径写法（8.3 短名 vs 长名），于是开发机绿、CI 红。现在断言涉及 ①主目录内容、
  ②用户缓存/临时目录位置、③平台后端能力、④路径写法时，一律在用例内自造环境
  （`t.Setenv` + `t.TempDir()`）或按后端能力 `t.Skipf`。

## v0.23.1 — 2026-11-18

真机 CI（真实 Windows + 真实 macOS）上线后抓到的一批缺陷，全部修完并让四个 job 同时绿。
**其中两条是安全相关**：

### Fixed（安全）
- **Windows 上保护路径从未被真正强制**：真机 `%TEMP%` 是 8.3 短名（`C:\Users\RUNNER~1\…`），
  而归属判定把规范化后的长名与会话传入的原始写法做**字符串**比对 → 匹配失败 →
  `Protected` 为空 → **deny ACE 一条都没加，`.git` 等保护路径一直可写**。
  修法：工作区列表统一规范化，"解析归属 / 派生 capability SID / 加 ACE / 匹配"四处看到
  同一种写法；并加一道诊断——解析出保护却一条都没归到工作区时，helper 明确打出
  `protection is NOT enforced`，**绝不静默**。
- **同一份策略里两种路径写法并存**：`normalizeProtectedPaths`（全局保护）只对"整条存在"的
  路径做解析，而 `canonicalSubpath`（根内只读）解析"已存在的最深祖先" → 同一个 `.git` 下
  出现 `/var/…` 与 `/private/var/…` 两种写法，同一目录被当成两个对象。
  两条路径统一为"存在的前缀解析、不存在的尾部原样"，并**先规范化 root 再拼接**。
- **`XDG_CACHE_HOME` 被当成替换而非追加**：设了它的用户 `~/.cache/pip`、`~/.cache/go-build`
  不在可写面 → 沙箱内只读。现在 `~/.cache` 与 `$XDG_CACHE_HOME` **两者都授权**。

### Fixed（可用性）
- **干净机器首次构建全挂**：`~/.cargo`/`~/.m2`/`~/Library/Developer` 等缓存根不存在时
  不被授权，而"可写"的语义**包含"可以创建"**。Seatbelt 直接渲染 `subpath`（按访问时求值）；
  bwrap/Landlock/Windows ACL 在 `Confine` 前 `MkdirAll(0o700)`（只建目录、失败不阻断）。
- **工作区在 `/dev/shm` 下变只读**（bwrap 一刀切跳过整个 `/dev` 子树）。
- **macOS 探测用裸 `true` 依赖 PATH** → 改绝对路径 `/usr/bin/true`（带 `/bin/sh -c 'exit 0'` 兜底）；
  `seatbeltExecutable` 此前硬编码系统路径、忽略可注入点（探测的可注入点形同虚设）。
- **worktree/submodule 在 Windows 上每条命令都跑不起来**：`.git` 是指针**文件**，而 helper
  对非目录 `--protect` fail closed。现在接受文件保护（按单个对象强制）。

### Added
- **沙箱一致性/极端测试套件**：70 项场景（路径形态、文件系统边界、命令边界、输出边界、
  并发、逃逸向量）**同一张表跑三个平台**，真机层由 `RIG_CONFORMANCE=1` 门控，`-v` 时打印
  能力矩阵统计（共 N 项 / 本平台适用 M / 真机执行 K / 跳过 J）。20 项标注为"已知边界"，
  失败信息里必带该标注，避免把边界当缺陷。
- **CI 增加真机 macOS job**（`macos-15-intel`：标准 runner 里资源最多，4 vCPU / 14 GB；
  macOS 15 上 `sandbox-exec` 确定存在）。此前 macOS 后端只有"渲染出的 SBPL 文本"断言与
  交叉编译，**真机从未跑过**。
- **`sandbox.Preflight` + `kit.Agent.Preflight()` + `rigdoctor sandbox`**：一句话回答
  "现在是否隔离、能不能联网、被挡住时点哪里"，四种状态各一句人话，并**禁止面向用户的
  句子出现内部术语**（`osrunner`/`workspace-write`/`bwrap`…共 11 个词，有用例守卫）。
  `kit.Config.Sandbox` 三态：nil = 尽力而为，**不因沙箱不可用而启动失败**。

## v0.23.0 — 2026-11-18

### 主线：沙箱从"能挡住"走到"能干活"

这一轮不再加隔离层，而是修掉"沙箱一开、正常开发全废"的那些断点，并把每处降级**如实上报**。
全部改动都有真机实测（Linux bwrap + Landlock ABI 4；macOS/Windows 为静态推导 + 交叉编译，
真机未跑的条目在各处显式标注）。

### Added

- **工具链缓存可写、其中的凭据文件只读**（三平台同一份函数）：`GOCACHE`/`GOMODCACHE`/
  `~/.npm`/pip/`~/.cargo`/`~/.m2`/`~/.gradle`/Xcode DerivedData 等缓存根进可写面，
  而 `~/.npmrc`、`~/.cargo/credentials.toml`、`~/.m2/settings.xml`、
  `%LOCALAPPDATA%\Microsoft\{Credentials,Vault,TokenBroker}` 等**保持只读**。
  实测：沙箱内 `go build` 成功（默认 `GOCACHE`，无额外 env 注入）。
- **`WritableRoot{Root, ReadOnlySubpaths}` 成为可写面的唯一模型**：四个后端与进程内围栏
  都从它派生，`.git` 因此可以"数据面可写、控制面只读"——实测 `git commit` 成功，同时
  `.git/config`、`.git/hooks/pre-commit` 返回 `Read-only file system`。
  顺带修掉一个真实分歧：`.git/config` 此前在进程内（write_file/patch）可写、在 OS 沙箱不可写。
- **宿主守护进程 socket 屏蔽 + seccomp 阻断 `connect(2)`**：read-only 模式下经宿主
  守护进程的 unix socket 读宿主文件系统是完整逃逸；当时 bwrap 把这些 socket 覆盖成
  `/dev/null`，并在策略禁止网络时装 seccomp 过滤器拦 `connect/bind/listen/accept`（含回环）。
  实测：守护进程客户端拿到内核 EPERM、`curl 127.0.0.1` 失败，而 `go build` 不受影响。
  `AllowNetwork=true` 时不装过滤器（否则本地 dev server 无法自测）。
- **沙箱内的命令免审批**（`kit.Config.AutoAllowSandboxedCommands`，默认开、
  `kit.Bool(false)` 关）：接了 `SandboxRunner` 之后，`bash`/`pwsh` 的非危险命令不再
  弹窗——沙箱与审批是两种独立的成本，围栏已经成立时再逐条要人点，用户会为了少点
  几十次弹窗把审批整体关掉（连危险命令的拦截一起丢）。免问判据（全部满足）：工具是
  shell 类、runner 非 nil 且自报能强制、策略不是 `danger-full-access`、命令不是
  `dangerous`。免问仍写一条 `approval.response`（decision=allow、reason=`sandboxed`）
  供 UI 显示与审计。危险命令永远直接拒绝；文件工具不经 OS runner，照旧问人。
  headless 宿主（没有 Asker）因此第一次能在沙箱里跑命令。
  实测："改 5 文件 + 3 次测试 + 提交" 的弹窗从 9 次降到 5 次。
- **提权提示**（`sandbox_permissions:"require_escalated"`）现在写进 `bash`/`pwsh` 的描述，
  但**只在真的可用时出现**：判据是"有提权通道 + 通道自报可用 + 有 OS runner +
  策略非 danger-full-access"，缺一项这句话就完全消失——避免指向一个必然失败的参数。
  提权阶梯不进 schema（取值由运行期校验）。

### Fixed

- **Windows 中文乱码**：根因是**输出侧代码页**（子进程按 936/GBK 写管道、我们按 UTF-8
  解码且零校验），**不是路径层**——路径全程 UTF-16，helper 也没有 ANSI 入口。
  修法两条腿：`pwsh` 在 Windows 上给脚本前置 `[Console]::OutputEncoding` +
  `$OutputEncoding` + `chcp.com 65001`（BOM-free、保存/恢复 `$LASTEXITCODE`、
  `param()`/`using`/`#requires` 脚本自动跳过、审批文案仍用用户原文）；后台任务的
  `job_output` 与 `cmd.exe` 分支同样处理。输出解码兜底：非法字节折叠成 1 个 U+FFFD 并告警，
  字节截断留下的半个字符静默丢弃。**不做 GBK 转码**（只允许一个依赖，猜错会把乱码变成
  "看起来正常"的错误文本）。已知限制：GBK 里有一批字节对恰好是合法 UTF-8
  （`一` 的 `D2 BB` 解码成 `һ`），这类输出无法检测——源头强制 UTF-8 才是修法。
- **跨平台 vet**：7 个 Linux-only 的 `osrunner` 测试文件补 `//go:build linux`
  —— 此前 `GOOS=windows go vet ./...` 编不过，CI 的 cross-compile and vet 步骤会红。
- **`sandbox_status` 可观测性**：新增 `toolchainCaches` / `networkCoverage` /
  `maskedSockets` / `missingToolchainCaches` / `privateTmpDisabled`，
  四个后端用 `sandbox.Reporter` 如实上报 `full`/`partial` 与"没被治理的部分"。

### 已知残余（下一轮）
- Landlock 的并集语义无法表达"根可写但子目录只读"，所以该后端把 `.git` 整根授予 rw
  （否则 `git commit` 不可用），控制面改由命令层规则 `write.git.control` 兜底；
  其 `RunnerLimits()` 逐字说明这条降级。元数据（chmod/chown/utimensat）不受 Landlock 治理。
- Windows ACL 后端不治理网络与设备命名空间，读不受限；WFP 需一次提权安装，不在桌面客户端范围。

## v0.22.0 — 2026-11-18

### 主线：沙箱挡住正常工具时，从"死路"变成"一次审批"

真实用户日志（Windows，钉钉 `dws` CLI）暴露两个死路：`~/.dws/.data.lock` 被拒、
`dws` 要写 **HKCU 注册表（DPAPI）** 被拒 —— 而模型没有任何合规逃生口，只能去猜
`DWS_DISABLE_KEYCHAIN=1` 这种偏方。这一版把两条路都补上，并且**不放宽任何策略**。

### Added

- **逐命令提权**（`bash`/`pwsh` 的 `sandbox_permissions: "require_escalated"`）：只豁免"这一次是否包 osrunner"，可写面/保护路径/命令风险判定/
  输出上限/cwd **全部照旧**。**危险命令即使提权也拒且不去问人**；`approval=auto`/
  `never` 拒绝（auto 不能自己给自己提权）；无 Asker 拒绝并指路；`justification`
  必填 ≥10 字符；审批故障 fail closed；允许后结果带
  `[sandbox: escalated …]` 审计说明；允许/拒绝都发 `sandbox.escalation` 事件。
- **非路径资源拒绝归因**：`sandbox.Denial` 新增 `Kind`（`path`/`registry`/
  `credential-store`）与 `Resource` + 17 条识别正则。非路径类**不渲染可写面**
  （申请目录对它无效），处置顺序与路径类相反：**先提权、再重定向**。
- `sandbox_status` / `request_sandbox_access` **默认注册**（discoverable，初始 active
  仍是 16）；`SandboxGrants: kit.Bool(false)` 可关。
- Windows 冒烟测试 5 条（`windows_smoke_test.go`，走真实产品路径；本机 Linux 未执行，
  仅交叉编译 + vet）。

### Fixed

- **denial 建议指向不存在的工具**：`NextSteps()` 此前无条件让模型调
  `request_sandbox_access`，而零配置下它根本不存在（日志里模型因此压根没试申请，
  直接去猜环境变量）。现在按实际装配生成三档建议：有 grant / 只有提权 / 都没有。
- **交互式命令被缓冲**：`pwsh` 描述原先没提醒，日志里模型用 `| Out-String` 把输出
  缓冲住，OAuth 等待 91 秒期间**看不到任何进度**。现补一句（+111 字节/处）：交互式
  用 `terminal_*`、长任务用 `job_*`。

## v0.21.0 — 2026-11-18

### 主线：默认只发 16 个工具，其余按需加载

零配置 `tools` 体积 **39.1 KB → 16.0 KB（≈3,995 tokens，−59%）**，能力面不变
（83 个静态工具都还在，只是不再一次全发）。三条缓存契约都有实测与守门测试：
不激活时 `tools` **逐字节稳定**、激活**只在末尾追加**（**激活到全集也不重排**）、
激活**不重建 system prompt**（我们的 prompt 不含工具描述）。诚实的代价：激活
那一轮 prefix cache 不命中，之后恢复。

### Added

- **动态工具加载（lazy tool discovery）**：`kit.New` 现在默认只把**核心 14** +
  `tool_search` + `memory` 发给模型（零配置 16 个 active / 38 个已注册），其余工具
  注册但不 active，模型用 `tool_search`（`{}` 列清单 / `{"query":…}` 筛选 /
  `{"activate":[…]}` 加载）按需激活。激活是**纯增量、按会话**：请求里的 `tools`
  数组只在末尾追加，不激活时逐字节稳定（prefix cache 友好）；`kit` 的 system prompt
  不含工具描述，所以激活**不重建 prompt**。实测零配置 `tools` 体积 39.1KB → 17.2KB
  （−56%）。
  - `agent.AgentSpec.InitialActiveTools` + `rig.ToolActivator`
    （`ActivateTools`/`ActiveTools`，按会话、幂等、只加不减）；
  - 未激活的工具**不可执行**：派发前拒绝，错误文案
    `rig: tool "x" is registered but not active in this session; activate it with tool_search first`；
  - 新增叶子包 `tools/discovery`（工具 `tool_search`，schema 322 字节）；
  - 新增事件 `tool.activated`（payload `events.ToolActivated{Tools, Trigger}`，
    Trigger ∈ `model`/`host`）；
  - `kit.Config.ToolDiscovery *bool`（`nil`=开，`kit.Bool(false)`=回到 v0.20 的
    "全部 active"）、`kit.Config.InitialActiveTools`、`kit.Agent.RegisteredTools()`；
    `kit.Agent.Tools()` 语义改为"当前 **active** 的工具"。
  - 自建工具集（`Config.Tools`）默认**不**开发现：宿主自己挑的工具就该能用。
  - **`skill` 并入常驻集**：schema 只有 808 字节，而"当前有哪些技能可用"是开工前就该
    知道的信息；它永不出现在 `tool_search` 清单里。
  - **`present` 改为按依赖门控**：新增 `kit.Config.Presenter`（`rig.FilePresenter`），
    nil 时 `present` 不注册（零配置没有 presenter，留着必然是死工具），接上后保留并
    立刻进初始 active 集。零配置因此是 **37 注册 / 16 active**，接了 Presenter 是
    **38 注册 / 17 active**。
  - **新增成本与缓存守门测试**（`internal/rigdoc/tokenbudget_test.go`）：零配置 active
    集 tools JSON ≤ 19 KB、常驻单工具 schema ≤ 2.5 KB、三条缓存不变量跨包复验、
    active 集逐个探针"可调用"。

### Fixed

- **零配置默认审批档（`approval=ask` + 无 Asker）下，元数据/本地状态工具被误拦**：
  `tool_search`、`skill`、`memory`、`todo_write` 没进 `gate.SandboxReadOnlyTools`，
  于是模型第一次 `tool_search` 就被"没有可用的审批通道"拒绝——动态工具加载在默认
  配置下**完全不可用**（而无 Asker 的宿主根本没有合规路径激活任何工具）。
  免问白名单的判据现在写清楚了：**"是否触及用户资源（文件 / 进程 / 网络）"，不是
  "是否只读"**——`memory`/`todo_write` 写的是我们自己的 app 状态，属于"写但不触及
  用户资源"；修改用户文件、执行命令、长期改变行为（`schedule_*`）与用户可见交付
  （`present`）仍然一律问人。新增四条零配置默认档回归用例（含显式的免问/被拒矩阵）。

### Changed

- `kit.Agent.Tools()` 从"全部注册"改为"当前 active"；要全部注册名用
  `kit.Agent.RegisteredTools()`。
- 账本：`catalog.Static` / `presets["all"]` 82 → **83**（+`tool_search`），
  `full.All(Root)` 80 → **81**。

## v0.20.1 — 2026-11-18

### Fixed

- **`secrets.New` 之后 `state` 永不为 nil**：`New()` 不预置快照，`state.Load()` 在首次
  `Add` 之前返回 nil；内部读路径都用 `if current != nil` 兜住了，但这条"有时 nil
  有时非 nil"的不变量本身是坑——并发用例直接读内部快照时踩了 nil deref（v0.20.0
  的 tag run 因此在 `coverage floors` 步骤 panic）。现在 `New()` 预置一次空快照，
  把不变量变成无条件的，同一路径上任何新加的读代码都不会再踩空。

> 发布说明：`v0.20.0` 的 tag 因上述 panic 未通过 release gates，**未创建 draft
> release**（该 tag 受仓库规则保护、不可删除）。请使用 `v0.20.1`。

## v0.20.0 — 2026-11-18

**定位变更（BREAKING 默认值）：个人 agent 客户端 SDK（单用户）。**

这一版的主线不是加功能，而是把产品从"为开发这个 SDK 的场景设计的 SDK"改回
"一个人和一个 agent 结对工作"的 SDK：**删掉 50 个文件 / 15,488 行企业形态能力**，
**工具面从 84 收敛到零配置 37 个且每个都真能用**，并修掉五轮审计发现的三类
"骗人的默认值"。

零配置 `kit.New(Config{Model, Provider, Root})` 的行为变了（详见
`docs/MODULES.md` 与 `AGENTS.md`）：
- `ApprovalAuto` **不再等于** `AllowAll`——危险命令（`rm -rf /`、`mkfs`、`curl|sh`）
  仍然被拒绝；
- `ApprovalAsk` 现在真的**问人**（`Config.Asker`）；没接 Asker 时是"没有审批通道"的
  拒绝，而不是静默 deny；
- 保护路径（`.git`、`~/.ssh`、`~/.aws` 等）**默认开启**且真的拦写入；
- shell **默认可执行**（原来 workspace-write 下一条命令都跑不了），危险性由命令
  风险判定 + 审批判断；
- 上下文压缩与"抽风守卫"**默认开启**，`MaxIterations` 默认 200；
- 工具集默认走 `personal` 预设，缺宿主依赖的工具会被**摘掉**而不是留在 prompt 里
  报错。

### Added

- **命令级风险判定**（`sandbox.ClassifyCommand`/`ClassifyShellCommand` + `gate.CommandRiskGate`）：把命令解析成 argv 后判定 read-only / mutating / dangerous / unknown，只读命令免审批、危险命令直接拒绝、其余委托人工审批；覆盖管道取最严、`xargs`/`find -exec`/`bash -c`/`sudo` 递归、重定向写系统目录、`curl|sh`、Windows 危险命令。329 条表驱动用例 + fuzz。
- **版本化事件协议**（`events`）：`Envelope`（version/seq/time/kind/payload）+ NDJSON `Emitter`/`Decoder`/`Replay`，14 个稳定 kind 与 payload 类型；字节级 golden、并发 seq 稠密、粘性写错误、超长行与未知 kind 的明确语义。
- **会话持久化**（`session/store`）：`Store` 接口 + 基于目录的 NDJSON 实现（`index.json` + `sessions/<id>.ndjson`），崩溃安全（先写数据再原子更新索引、半行裁剪、索引重建、自愈校正），并提供 `rig.History` 适配；`kit.Config.SessionStoreDir` 一行接上。
- **apply_patch 工具**（`tools/patch`）：一次原子应用多文件补丁，支持统一 diff 与 `*** Begin Patch` 两种格式，三级上下文容错匹配、歧义即拒绝、路径三条硬规则、CRLF/权限/无尾换行保真、dry-run。
- **指令层级与环境上下文**（`instructions`）：AGENTS.md 向上发现（user/root/ancestor/local 四级）、`@import` 递归（围栏内不展开、循环/深度拦截、默认限制在仓库根内）、总量预算、`Set.Render`；`Environment`（workspace/cwd/平台/日期/git）只读 `.git/HEAD`、单行化防注入；`kit.Config.Instructions/Environment` 直接拼进系统提示词。
- **输出脱敏**（`secrets`）：`Redactor` 支持已知明文（含命名标签）与 22 类内置凭据模式、JSON 键级脱敏、流式 `Writer`/`Reader`（跨 chunk 不泄露、必须 Flush）；`kit.Config.Redactor` 一次包装覆盖所有工具输出。
- **模糊 + BM25 检索**（`tools/search`）：文件名模糊打分、内容 BM25（k1=1.2/b=0.75，多词 AND）、`@path` 提及解析；输出契约稳定、忽略 VCS/二进制/超大文件、超时可控。
- **沙箱执行提权协议**（`sandbox/escalate`，**内部实验，未在产品面暴露**）：沙箱内请求宿主执行的 NDJSON 协议 + 宿主侧 Broker（argv 前缀 Allow/Deny/Approval、目录与符号链接校验、环境白名单、防重放、并发额度、超时、输出截断、审计）；`--flag=value` 绕过与孙进程拖死都已被测试钉死。它没有对应的模型可调用工具，也不在 `tools/catalog`/`tools/presets`/`tools/full` 里。
- **回合撤销**（`tools/undo`）：快照 + 全有或全无回退（前像 + 逆操作栈），只回退被改动文件、保留权限、二进制/超预算只报告不猜；`Tracker` 按回合保留最近 N 份，`kit.Config.UndoTracker` 在每次 Run 前自动快照。

- **沙箱运行期可写授权（path-scoped write grant）**：`sandbox.Policy.AddWritableRoot`、`ValidateWritableGrantPath`、`Grant`、`WithGrantBudget`；新增工具 `sandbox_status`（只读策略视图）与 `request_sandbox_access`（模型为某个工具自己的凭据/缓存目录申请会话级可写根，审批由 `rig.Gate` 决定，默认拒绝）。默认工具集不变，二者在 `full.AllOptions.SandboxGrants` / `kit.Config.SandboxGrants` 下构造。
- **策略可写面配置**：`WithWritableRoots`、`WithProtectedPaths`、`WithSlashTmp`、`WithTmpdirEnv`；`Policy.AdditionalWritableRoots()` / `ProtectedPaths()` / `DroppedWritableRoots()`。
- **结构化沙箱拒绝**：`sandbox.Denial` / `DetectDenial`（提取被拒路径、操作、模式、可写面与下一步），并接入 bash/pwsh、jobs、terminal 与 worktree 的结果视图。

### Changed

- **`provider/openai` 复用 `provider/local` 的协议实现**：openai 从 982 行降到 428 行
  （−56%），协议实现从两份变一份。差异改由 `local.Quirks{RequireDone,
  StrictFinishReasons, StrictDeltas}` 表达（缺 `[DONE]` 报 `ErrIncompleteStream`、
  finish_reason 原样透传、不认 `message` 回退/旧式 `function_call`/对象型
  `arguments`）。**既有 30 个用例一行未改**；新增 8 条跨 provider 契约用例把
  "严格 vs 宽松"固化成契约而不是巧合。
- **模块化变成被测试锁住的约束**：新增 `internal/rigdoc/modularity_test.go`（31 个叶子
  能力包不得 import `kit`/`tools/full`；根包 `rig` 仅标准库；只有 `kit` 同时装配
  provider 与工具面）与 `docs/MODULES.md`（模块清单、两种使用方式、按场景挑模块）。
- `sandbox/osrunner` 的 profile 由 `BuildProfile` 单一真源派生（bwrap/Landlock/Seatbelt/Windows ACL 不再各自解释策略）；`EnforcementNone` 表示 `danger-full-access` 透传；Landlock 恒报 `partial`（无法治理 chmod/chown 等元数据）。
- 沙箱拒绝归因不再要求退出码非零：只要输出命中方言就追加（CLI 常把落盘失败包成自己的文案后仍退 0）。归因文案从单行"疑似"升级为结构化块（`[sandbox: denied <op> to <path> …]` + 可写面 + 处置顺序）。
- bwrap profile：read-only 追加 `--remount-ro /dev`（`/dev/*` 不再可写）、`--bind` 覆盖 `$TMPDIR` 等非 `/tmp` 临时根、`--unshare-net`、`--cap-drop ALL`；workspace-write 支持 `ProtectedPaths` 的 `--ro-bind` 覆盖。

### Removed

为对齐**个人 agent 客户端 SDK（单用户）**的定位，移除以下四块企业/多租户/自建服务形态的能力（共 50 个文件 / 15,488 行）：

- **`tools/escalate` + `kit.Config.Escalation`**：把提权协议包装成模型可调用工具（`request_command_escalation`）的产品通道。个人客户端放宽权限用 `danger-full-access` 或审批即可，不需要"把单条命令送回宿主执行"的旁路；该通道出过 P0 级 RCE。
- **`mcpserver` + `cmd/rigmcp`**：把 `rig.Tool` 暴露成 MCP server 的能力。产品本身是 MCP **客户端**，`tools/mcp`（stdio/Streamable HTTP、OAuth 2.1）不受影响。
- **`internal/filewatch`**：轮询式文件监听与 `Changed`/`ChangedPaths`/`DigestFile` 冲突检测助手。单 agent 场景由 git 自身覆盖，轮询 watcher 只带来扫描开销与"两次扫描之间创建又删除"的假阴性。
- **`sandbox/escalate`（提权协议整包）**：沙箱内请求宿主执行的 NDJSON 协议 + 宿主侧 Broker（`broker.go` 含 `os/exec`）整目录删除，共 15 个文件 / 4,421 行。产品面本来就没有引用（`tools/escalate` 已删、`kit.Config.Escalation` 已删），留着只增加攻击面与维护成本；个人客户端需要放宽权限时直接用 `danger-full-access` 或审批通道。"以后要"时重写一份比维护死代码便宜。`internal/procattr` 保留（`tools/{builtin,jobs,lsp,mcp,ptc,terminal,worktree}` 与 `sandbox/osrunner` 仍在用）。

守护：`internal/rigdoc/surface_test.go` 断言这四个包（含 `sandbox/escalate`）、相关工具名与 `kit.Config.Escalation` 都不会回到发布面。

### 已知结构债（不阻塞发布，记录在此以免被遗忘）

- **`tools/mcp` 里有两个 SSE 解析器**（`http_transport.go` 的行级解析与
  `notifications.go` 的完整帧解析），与 `provider/local` 的 `SSEData` 是同一协议的
  第三份实现。计划 v0.20.1 抽 `internal/sse` 三处共用——`provider/openai` 本轮已经
  收敛，这是同类风险点的最后一处。
- **工具名账本有三处来源**（`tools/catalog.Static`、`tools/presets`、
  `full.All`），靠互锁测试保持一致。长期看更适合"能力注册表"式装配（每个模块自己
  声明需要什么/提供什么），但这会改动 `full.AllOptions` 的形态，留到 v0.21 评估。

第二轮审计清理的**过度设计残留**（合规/运维形态，个人客户端不需要第三方审计，也不该让模型改自己的设置）：

- **`report` 工具（整包删除）**：事件计数 / 工具调用与错误总数 / 事件类型分布。没有第三方要审计的读者，模型看到它只会被诱导去做"汇报"，而不是完成任务。诊断仍可用 `inspect`（sessions/events/workspace/stats）。
- **`settings_get` / `settings_set`（整包删除）**：把应用外壳的配置读写暴露成模型工具，等于把"应用设置"和"模型工具面"混在一起；设置应由 UI 直接改。宿主若需要配置存储，用自己的内部 API，不要再包成工具。
- **`present_artifact`（整工具删除）**：`present`（声明交付文件，`rig.FilePresenter`）已经是 DSH 兼容的现代名字；`present_artifact` 只是旧产物渲染入口，`standard` 预设此前就已排除它，`all` 里保留它会让模型看到两个近义工具。`rig.Presenter` 接口保留（宿主可继续实现），SDK 不再暴露对应工具。

计数变化：`catalog.Static` 86→82、`catalog.Legacy` 61→58、`catalog.Modern` 65→62、`tools/full.All`（带 Root）84→80、`presets`：standard 76→72、all 86→82、personal 60→56。

### Deprecated

### Fixed

- **`run_code`（PTC）此前是 OS 沙箱旁路**：`tools/ptc` 直接 `exec` 解释器，未接 `Runner`。现在 `ptc.Config` 接受 `Runner`+`Policy`（`kit.Config.PTC` 自动透传），解释器进程与 `bash` 同样经 `Confine` 包装；`Runner` 非 nil 而 `Policy` 为 nil 时 fail closed。程序临时文件改放策略根（`bwrap` 的 workspace-write 会挂私有 tmpfs `/tmp`，宿主 `/tmp` 里的脚本在沙箱内不存在），并在失败结果里追加结构化沙箱归因。

### Security

- **修 terminal 后台发送的状态竞态**：`job_kill` 会让会话退出，而 `backgroundOutcome` 先读 `exited` 标志就把这次终止误报成 `completed (session exited: -1)`；现在取消优先于"已退出"，测试在 CPU 压满时可稳定复现、修复后 40 轮全绿。

- **修 Windows 并发死锁（真机 CI 发现）**：per-workspace 命名 mutex 的所有权是按**线程**的，而 Go 的 goroutine 会在 OS 线程间迁移，`ReleaseMutex` 落到别的线程上就返回 `ERROR_NOT_OWNER`，锁永不释放——4 个并发 helper 时整步卡满 10 分钟。现在临界区用 `runtime.LockOSThread` 把 goroutine 钉在同一线程上，并把等待改成有上限（30s）：异常时得到一条明确错误，而不是永久挂起。

- **Windows 测试可移植性**：沙箱后端自身的纯逻辑测试现在能在 Windows 上通过（POSIX 形状的断言按平台跳过；ACL 状态 fixture 用带盘符的绝对路径）；所有真机强制类用例统一由 `RIG_WINDOWS_SANDBOX_TEST=1` 门控，CI 的 `windows-sandbox` job 因此把"纯逻辑"与"真机强制"分成两个明确步骤。`untrustedExecutableReason` 在 Windows 上不再做 POSIX 权限位判断（Go 的 FileMode 在 Windows 上不反映 ACL），只保留跨平台的"落在工作目录内"检查。

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

## v0.19.0 — 2026-11-17

默认行为变更发布：模型↔工具轮数默认不限制（unlimited by default），显式上限能力保留。

### Changed

- **BREAKING（默认行为）：模型↔工具轮数默认不限制（unlimited by default）**。宿主反馈：个人使用场景默认不应限制工具轮数。`rig.AgentSpec.MaxIterations` <= 0 与 `kit.Config.MaxIterations` <= 0 不再回退到 20 轮兜底，而是表示**不限制轮数**（内部以 0 表示 unbounded，循环仅在显式上限 >0 时计数判定）；`rig.RunRequest.MaxIterations` >0 仍按次覆盖，<=0 回退到 agent 默认（spec 未配置上限时即不限制）。显式 >0 的上限能力保留：超限仍执行最后一个允许轮次的工具并追加结果，随后返回部分 `RunResult` 与包装 `rig.ErrMaxIterations` 的错误（`errors.Is` 兼容）。子 agent（`rig.SubagentSpec.MaxIterations` = 0）继承父 agent 默认，因此同样默认不限制；`tools/ralph` 自身的 `max_rounds` 轮次预算不变。
  - **迁移注记**：依赖旧 20 轮默认兜底的宿主需显式设置上限（如 `rig.AgentSpec{MaxIterations: 20}`、`kit.Config{MaxIterations: 20}` 或按次 `rig.RunRequest{MaxIterations: N}`）；此前用大哨兵值（如 1_000_000）模拟 unlimited 的宿主可直接改回零值/省略该字段。
- `agent.DefaultMaxIterations` 与 `kit.DefaultMaxIterations` 常量重定义为 **0（unlimited）**，不再是 20。引用这两个常量的代码无需改动即可编译，但其语义随默认行为变更（BREAKING 的一部分，迁移注记同上）。

### 验证

- TDD red→green：`test(agent)` 红提交（6 个失败测试钉住默认 unlimited 与常量语义，4 个显式上限对照通过）→ `feat(agent)!` 绿提交。默认 spec/默认 RunRequest/零值 `kit.Config` 下 25 轮工具循环 + 收尾轮（26 轮 > 旧默认 20）正常完成；显式 cap=3 时第 4 轮仍返回 wrap `rig.ErrMaxIterations` 的错误（spec 层与 RunRequest 层各一组对照）。
- 本地门禁：`gofmt`（tracked 文件）、`go build ./...`、`go vet ./...`、`go test ./... -count=1`（`CGO_ENABLED=0`）、`go test ./... -race`、coverage floors（总 statement 覆盖 83.2%，每包 ≥70% 达标）、`go run ./cmd/rigdoc -check`、依赖策略/根包 stdlib-only/无 cgo 检查、windows/amd64 + darwin/arm64 + freebsd/amd64 交叉编译 + `go vet`、README quickstart 漂移、examples、catalog/audit 门禁、fuzz smoke（5s/target）与 benchmark smoke 全部通过；staticcheck/govulncheck 由 CI 执行。
- 兼容性：BREAKING 默认行为变更（迁移注记见上）；无新依赖、无工具数量变化（81 个静态工具）、模型可见工具描述/JSON Schema 不变；`tools/ralph` 的 `max_rounds` 与 `GoalRoundDriver` 的轮次预算是独立机制，不受影响。

## v0.18.1 — 2026-11-17

审计修复发布（来源：2026-11 ZeoKeel 多角色安全/可用性审计（RG-03/RG-05）；仅 rig 侧这两项修复，审批/提权语义与 full→passthrough 设计不变）。

### Fixed

- **RG-05（P3）bash/pwsh 工具描述随 runner 注入动态化**：未挂接 runner 时逐字保留原「This tool is not an OS sandbox: the approval gate is the boundary.」边界句；经 `builtin.WithRunner`/`PwshConfig.Runner` 挂接 OS runner 后改写为准确表述——OS 沙箱按策略约束文件效果（`danger-full-access` 原样透传、不包装），审批 gate 是其上的另一层。消除「宿主注入 runner + shell 自动放行」组合下模型每轮读到的描述失真。默认 `rigdoc` AI 目录（无 runner 组合）不变。
- **RG-03（P2）shell/jobs 工具结果消费 `ConfinedCommand.DenialSignatures`/`Enforcement`（沙箱拒绝归因）**：命令失败（非零退出或超时）且合并输出按大小写不敏感子串匹配到 runner 拒绝方言（bwrap `read-only file system`、Landlock `permission denied`、Seatbelt `operation not permitted`、Windows ACL `access is denied`/`拒绝访问`）时，在结果文本末尾追加结构化归因行 `[sandbox: suspected OS sandbox denial (matched "…"; policy mode …, enforcement …); …]`，让模型/用户能区分「OS 沙箱拒绝」与「命令自身失败」。覆盖 bash/pwsh 前台、超时提升与后台路径；`jobs.Manager` 记录自身 Confine 或生产者 `ProcessSpec` 携带的方言（内部新增 `DenialSignatures`/`Enforcement` 字段），并在失败任务的所有结果视图（`job_output`/`job_wait`/`job_kill`）渲染归因行。成功路径、不匹配失败、被模型杀死的任务与无 runner 组合逐字节不变；归因是「疑似」提示（方言子串也可能来自命令自身的普通错误），不改变退出码语义，也不提供提权通道（rig 拒绝 per-call escalation 是设计）。共享渲染助手为内部包 `tools/internal/toolutil.SandboxDenialNote`（非公共契约）。

### 验证

- TDD red→green：`test(tools)` 红提交（10 个失败测试钉住两态描述与四平台方言归因 + 对照组）→ `fix(tools)` 绿提交。
- 本地门禁：`gofmt`（tracked 文件）、`go build ./...`、`go vet ./...`、`go test ./... -count=1`（`CGO_ENABLED=0`）、`go test ./... -race`、coverage floors（总 statement 覆盖 83.2%，每包函数覆盖达标）、`go run ./cmd/rigdoc -check`、windows/darwin/freebsd 交叉编译 + `go vet` 全部通过；staticcheck/govulncheck 由 CI 执行。
- 兼容性：无公共 API 变更、无新依赖、无工具数量变化；模型可见文本仅上述两处变化（挂接 runner 组合的描述、失败结果的归因行）。

## v0.18.0 — 2026-11-16

### Added

- **`mcp.NewOAuthClient` / `mcp.OAuthConfig`**：MCP Authorization（传输层）OAuth 2.1
  客户端。覆盖 401 + `WWW-Authenticate` 解析、RFC 9728 Protected Resource Metadata、
  RFC 8414 / OIDC Discovery 的 AS metadata（issuer 与 S256 严格校验）、预注册 client_id
  与 RFC 7591 动态注册、PKCE(S256)+state+RFC 9207 `iss`、127.0.0.1 回环回调、授权码换
  令牌、刷新（单飞 + 轮换安全）、撤销；`TokenStore`/`OpenBrowser` 由宿主注入，rig 不读
  文件、不依赖 UI。令牌记录为版本化 JSON，错误按稳定 `OAuthErrorCode` 分类。
- **`mcp.Config.OAuth` / `mcp.HTTPConfig.OAuth`**：把授权客户端接入 Streamable HTTP
  连接；`oauth.HTTPClient()` 为每个请求注入 Bearer，401 时刷新并重试一次，403
  `insufficient_scope` 记录为可重试的授权错误。
- `tools/mcp/oauth*_test.go`：fake AS + fake MCP 协议测试（发现回退、PKCE 拒绝、issuer
  不匹配、state/iss 校验、DCR 重试、刷新/轮换/失效、撤销、单飞刷新、持久化与重启恢复）。

### Changed

- `tools/mcp/doc.go`：补充 OAuth 两阶段授权用法、TokenStore/OpenBrowser 注入与安全边界；
  README/docs 的 MCP 能力与路线图同步（OAuth 从 “Next” 移除）。

## v0.17.1 — 2026-11-15

多会话 Goal Revision 支持与 DSH 轮次对齐（汇总未发布的 v0.17.0 改动），并包含 v0.16.5 的 Windows 沙盒并发 ACL 修复。

### Added

- **`hosttools.Options.GoalRevision`**：显式注入 `GoalRevisionStore`；DSH `get_goal`/`create_goal`/`update_goal` 优先使用它。旧 `Options.Goal` 与旧版 `goal` 工具保持兼容，旧宿主零改动。
- **`GoalRevisionStore.CurrentGoal(ctx)`**：读取当前目标时携带工具调用 ctx，多会话宿主可返回调用方会话自己的目标。**接口签名变更**：直接实现 `GoalRevisionStore` 的宿主需要加 `context.Context` 参数；`hosttools.NewMemoryHost` 已同步。

### Changed

- **`agent.GoalRoundDriver` 默认/硬上限对齐 DSH**：`DefaultGoalRounds` 6→256、`MaxGoalRounds` 12→256；`SetMaxRounds` 超限钳制行为不变。产品仍可用 `ContinuationPrompt` 注入 objective/round。

### 包含

- v0.16.5：Windows 沙盒并发 ACL 修复（跨进程命名 mutex + per-grant marker + 状态文件，cleanup 只撤销自己的 capability ACE，崩溃 grant 自愈）与测试 fake data race 修复。

### 验证

- `gofmt`、`go build ./...`、`go vet ./...`、`go test ./... -count=1`；
- `go test -race ./sandbox/osrunner`、`GOOS=windows GOARCH=amd64 go build ./...`；
- 兼容性：`tools/hosttools` 旧 API（`Options.Goal` + 旧版 `goal` 工具）保持不变；仅自定义 `GoalRevisionStore` 实现需要加 ctx。

## v0.16.5 — 2026-11-15

维护性发布：把 v0.16.3（Windows 沙箱并发 ACL 修复）与 v0.16.4（测试 fake 的 data race 修复）cherry-pick 到 main 后发布，使 release workflow 的 “tag 必须在 main 上” 校验通过。

### Changed

- 运行时代码与 v0.16.3 相同；测试与 CI 与 v0.16.4 相同；无公共 API/工具/helper 协议变化。
- v0.16.3/v0.16.4 的 tag 指向独立维护分支，release workflow 因 tag 不在 main 上未通过；本版是推荐使用的模块版本（`go get github.com/zeokeel/zeokeel-rig@v0.16.5`）。

## v0.16.4 — 2026-11-15

CI/测试修复：v0.16.3 的 release test 在新增的 `fakeACLBackend` marker map 上存在 data race（仅测试代码，运行时代码与 v0.16.3 完全一致）。

### Fixed

- `fakeACLBackend` 的 `CreateMarker`/`MarkerAlive`/`CloseMarker` 用独立的 `markerMu` 保护 `markerAlive` map；`go test -race ./sandbox/osrunner` 通过，release CI 不再因该 race 失败。
- 无运行时行为/公共 API 变化；Windows 实机并发回归仍需 `RIG_WINDOWS_SANDBOX_TEST=1`。

## v0.16.3 — 2026-11-15

Windows 沙箱并发执行不再因 ACL grant/restore 互相覆盖而失败。

### Fixed

- **Windows 并发命令的执行域 ACL 竞争**：旧实现里每个 sandbox helper 读取并保存
  workspace 目录的整份 DACL，退出时连同自己的临时 capability ACE 一起恢复整份
  DACL。多个 helper 并发时，后退出者用自己进入时的快照覆盖前者的授权/恢复，导致
  另一个仍在运行（或新启动）的命令写 workspace 失败。
- 修复：按 workspace 路径哈希引入跨进程命名 mutex
  （`Local\ZeoKeelSandboxACL-<sha256(path)>`）串行化 ACL 状态变更；每个 grant 持有
  自己的命名 marker mutex（`Local\ZeoKeelSandboxGrant-<pathHash>-<sidHash>`），
  helper 崩溃时句柄自动消失，下一个 helper 视其为死 grant 并清理其 capability ACE。
- 状态文件 `%LOCALAPPDATA%\ZeoKeel\sandbox-acl\<pathHash>.json`（不可用退回
  `%TEMP%`）记录原始 self-relative security descriptor（base64）、
  denyAdded/labelApplied 与活跃 grants；mutex 内 load/purge/apply/save，temp+rename
  原子写。首个 grant 保存原始 descriptor、加 Everyone `FILE_DELETE_CHILD` deny 与
  Low 完整性标签（各仅一次，标签失败仍降级为只依赖受限令牌）；cleanup 只撤销自己的
  capability ACE，最后一个 grant 退出时才恢复原始 descriptor 并删除状态文件。
- 状态文件损坏/版本未知时 fail closed（helper 不 spawn 未受限进程，不改写安全
  描述符）；restore 失败保留状态文件供后续 helper 重试，绝不放宽授权。
- read-only 模式仍然完全不修改 workspace ACL；受限令牌、restricting SIDs、Low
  完整性、私有临时目录、helper argv/结果格式与错误契约均不变，产品侧无需改动。

### 验证

- 新增 Linux 可跑的纯状态机单测（并发 A/B grant、A 退出、B 退出 restore、死 grant
  purge、崩溃残留恢复、损坏状态 fail closed、label 降级、命名/哈希/状态文件校验）。
- Windows 真实并发回归测试（4 个 helper 同时在同一 workspace 授权并写文件）需要
  `RIG_WINDOWS_SANDBOX_TEST=1` 才运行；Linux 跳过。
- `go test ./sandbox/osrunner`、`go test ./...`、`GOOS=windows GOARCH=amd64 go
  build ./...`、`GOOS=windows GOARCH=amd64 go vet ./...` 通过。

## v0.16.2 — 2026-11-14

Windows 上执行工具时不再弹出控制台窗口。

### Fixed

- **Windows 每次工具调用弹黑窗**：桌面宿主是 GUI 子系统进程，`exec.Command`
  启动的控制台子进程（`bash`/`pwsh`/`job_start`/`terminal_*`、MCP/LSP/`run_code`
  服务、沙箱 helper）在控制台继承失效时（例如 Windows Terminal 接管默认终端）
  会各自新建一个可见控制台窗口。新增 `internal/procattr.HideWindow`：Windows 上
  给子进程设置 `SysProcAttr.HideWindow`（`STARTF_USESHOWWINDOW` + `SW_HIDE`），
  其他平台为空操作；`tools/builtin`、`tools/jobs`、`tools/terminal`、`tools/mcp`、
  `tools/lsp`、`tools/ptc`、`tools/worktree` 与 `sandbox/osrunner` 的所有 spawn
  站点统一调用，并保留调用方已设置的 `Token`/`CreationFlags`。ConPTY 的
  `STARTUPINFOEX` 同时请求隐藏初始窗口。
- 无新增依赖、无新增工具、无公共 API/模型可见变更；宿主无需改代码。

### 验证

- `gofmt`、`go build ./...`、`go vet ./...`、`go test ./... -count=1`；
- `internal/procattr` 覆盖测试：任何 `exec.Command/CommandContext` 未调用
  `HideWindow` 都会失败（Linux CI 即可拦住回归）；
- 覆盖率门禁、`go run ./cmd/rigdoc -check`、staticcheck；
- `GOOS=windows/amd64`、`darwin/arm64`、`freebsd/amd64` 交叉 build + vet；
  Windows 上的实际窗口行为建议真机复核一次。

## v0.16.1 — 2026-11-12

Windows 沙箱 ACL 探测崩溃与 Windows `terminal_*` 被早退禁用的修复。

### Fixed

- **Windows ACL 探测崩溃（0xC0000005，导致沙箱不可用）**：`winWindowsDirectory`
  把 `GetWindowsDirectoryW` 的 `(lpBuffer, uSize)` 参数传反（照抄了
  `GetTempPath2W(BufferLength, Buffer)` 的顺序），Win32 把容量 260（0x104）当成
  输出缓冲区指针并写入，probe helper 以 `exit status 2` 崩溃。沙箱 runner 因此
  fail closed：Windows 上所有命令工具（`pwsh`/`bash`/`job_start`/`terminal_*`）
  都返回 `sandbox runner cannot confine the command: os sandbox unavailable`。
  现已改为正确参数顺序，并新增 Windows 运行时回归测试
  `TestWindowsRuntimeKnownFolders`。
- **Windows 10 / Server 2019 上的 `GetTempPath2W`**：该导出只存在于 Windows 11 /
  Server 2022+；`syscall.LazyProc.Call` 找不到过程会 panic（`mustFind`），旧系统
  上 ACL 探测会以同样的 `exit status 2` 崩溃。`winGetTempPath` 现在先
  `Find` 再 `Call`，缺失时回退 `GetTempPathW`。
- **Windows `terminal_*` 早退使 ConPTY 不可达**：v0.16.0 在 `Manager.open()`
  仍保留 `runtime.GOOS == "windows"` 早退，返回 "Windows ConPTY support is not
  implemented in this release"，ConPTY 实现是死代码。删除早退后 Windows 走
  ConPTY（`ptySupported()` 探测），旧系统或创建失败时自动回退 pipe。

### 验证

- Linux：`gofmt`、`go build ./...`、`go vet ./...`、`go test ./...`；
- `GOOS=windows` amd64/arm64 交叉编译与 `go vet`（Windows 运行时测试仍只交叉
  编译，仓库暂无 Windows runner；建议后续补 `windows-latest` CI job）；
- `make ci` 与 clean-room `go get` 由 tag 工作流执行。

## v0.16.0 — 2026-11-11

Windows 终端使用真正的 ConPTY，调用原生命令行。

### Added

- **Windows `terminal_*` 走 ConPTY**：用 Win32 `CreatePseudoConsole` +
  `STARTUPINFOEX`/`PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE` + `CreateProcess` 启动
  原生 PowerShell；输入/输出经由 ConPTY 管道，Ctrl-C 以 0x03 写入主端。
  Windows 10 1809+ 可用；ConPTY 不可用（旧系统或创建失败）时 manager 自动
  回退到 pipe 传输并给出 warning。
- **原生 shell 定位兜底**：PATH 缺失时用 Win32 `GetSystemDirectoryW` 定位
  `System32\WindowsPowerShell\v1.0\powershell.exe` / `cmd.exe`。
- **进程抽象**：终端会话改用 `process`/`ptyEnd` 接口，Unix PTY、Windows
  ConPTY 与 pipe 三种传输共享 wait/signal/close 逻辑；PTY 关闭幂等。

### 验证

- Linux 全量 `go test ./...`、`go vet ./...`、`GOOS=windows` 交叉编译；Windows
  交互行为由消费方复验（仓库无 Windows runner）。`make ci` 全量。

## v0.15.1 — 2026-11-11

Windows 沙箱临时目录的根因修复：改用 Windows 自带 temp，并放行定位它所需的非敏感环境变量。

### Fixed

- **helper 环境被裁剪导致候选为空**：命令的 `CommandEnv` 默认只放行
  `PATH`/`HOME`/`LANG`，Windows helper 因此看不到 `TEMP`/`TMP`/
  `LOCALAPPDATA`/`USERPROFILE`/`SystemRoot`；`os.TempDir()` 又回退到
  `C:\WINDOWS`，导致错误信息 `tried` 为空。现在 helper 的临时目录按 Windows
  自己的机制选择：`RIG_SANDBOX_TEMP` → `GetTempPath2W`/`GetTempPathW` →
  用户 Known Folder 的 `LocalAppData\Temp` → `os.TempDir()`；用 Win32
  `GetWindowsDirectoryW` 过滤 Windows 目录（即使 `SystemRoot` 也被裁剪）。
  不再把私有 temp 放进工作区。
- **默认环境变量允许列表**：`sandbox.Policy.CommandEnv` 默认放行 Windows 的
  `TEMP`/`TMP`/`TMPDIR`/`LOCALAPPDATA`/`APPDATA`/`USERPROFILE`/`SystemRoot`/
  `SystemDrive`/`WINDIR`/`COMSPEC`/`PATHEXT`/`USERNAME`/`USERDOMAIN`/`OS`
  （均非敏感）；API key/token/凭据仍会被丢弃。
- **`job_start` Windows 命令校验**：策略按 shell 的裸基名（如 `pwsh.exe`）
  校验允许列表，仍用 `LookPath` 返回的绝对路径启动，修复“命令名不能是路径”。

### 验证

- `go test ./...`、`go vet ./...`、`GOOS=windows` 交叉编译；`make ci` 全量。

## v0.15.0 — 2026-11-11

Windows 命令执行修复 + Windows 文件/终端/后台任务支持；无破坏性变更。

### Fixed

- **Windows ACL runner 临时目录**：私有临时目录原用 `os.TempDir()`；Windows 在
  `TEMP/TMP` 未设置时回退到 `C:\WINDOWS`，普通用户不可写，helper 在授权前
  以 `windows-acl-run: create private temp directory ...` / 退出码 127 失败，
  所有命令都无法执行。现在按 `RIG_SANDBOX_TEMP`、`TEMP`/`TMP`、
  `%LOCALAPPDATA%\Temp`、`os.UserCacheDir()\Temp`、`%USERPROFILE%\AppData\Local\Temp`、
  `os.TempDir()` 的顺序选第一个可写候选，并剔除 SystemRoot/WINDIR 之下的路径；
  功能探测与真实执行共用同一套选择逻辑。受限环境可用 `RIG_SANDBOX_TEMP`
  显式指定可写目录。
- **Windows 路径分隔符**：`write_file` 的父目录创建改用 `filepath.Dir`（原来
  `path.Dir` 不识别 `\`，嵌套路径不建目录）；`glob`/`grep` 传给
  `os.Root.FS()` 的路径统一 `filepath.ToSlash`（Windows 的 `fs.FS` 拒绝反斜杠）。

### Added

- **`terminal_*` Windows**：默认 shell 从硬编码 `/bin/sh` 改为按平台选择
  （Windows 为 `powershell.exe`）；ConPTY 仍不实现，但 pipe transport 可用。
- **`job_start` Windows**：后台任务在 Windows 上通过 `pwsh`/`powershell`
  （最后 `cmd.exe`）执行，策略命令允许列表仍生效。

### 验证

- Linux 全量 `go test ./...`、`go vet ./...`、`GOOS=windows` 交叉编译；
  -race 与 Windows 侧由消费方复验（仓库无 Windows runner）。

## v0.14.1 — 2026-11-11

v0.14.0 的 release lint 修复 + macOS Seatbelt 拒绝自检；无破坏性变更。

### Added

- **Seatbelt 拒绝自检（denial self-test）**：macOS runner 的功能探测在
  `sandbox-exec -p <profile> -- true` 之后，再用只读 profile 尝试写工作区外的临时
  文件。写入成功说明 Seatbelt 没有按预期拒绝 `file-write*`（“API 还在但行为静默
  变弱”），此时 `New` 失败并返回 `sandbox.ErrSandboxUnavailable`，绝不把不可信的
  runner 交给工具。纯逻辑测试用假 `sandbox-exec` 覆盖了“行为正常 / 未强制 / 无拒绝
  地退出”三种情形。

### Fixed

- 把 Windows helper 的失败前缀/退出码常量移到 `//go:build windows` 文件：
  v0.14.0 的这两个常量在 Linux 构建下被 staticcheck 判为 U1000（unused），导致
  release 门禁失败；无行为变更。

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。


- 见上：Seatbelt denial self-test。

## v0.14.0 — 2026-11-11

可选的纯 Go OS 沙箱 runner（additive，默认关闭）；无破坏性变更。v0.11.0 曾移除
未发布的 OS 沙箱实验；本版以 **opt-in 子包** 的形式重新引入，核心策略层保持
进程内围栏，默认行为不变。

### Added

- **`sandbox.Runner` 接缝**：`Confine(argv, policy)` 返回 `sandbox.ConfinedCommand`
  （受限 argv、`Enforcement`、`DenialSignatures`）；`sandbox.ErrSandboxUnavailable`
  表示本机无法强制请求的模式，调用方必须 fail closed。
- **`sandbox/osrunner`（可选、纯 Go、无 cgo、零新增依赖）**：
  - Linux：bwrap 优先，回退自带的 Landlock 启动器（raw syscall 444/445/446，
    ABI 1–5 协商，`PR_SET_NO_NEW_PRIVS` 后 `exec`）；
  - macOS：系统 `sandbox-exec`（Seatbelt/SBPL）；
  - Windows：Write-Restricted 受限令牌 + 目录 ACL 的隐藏 helper（`partial`：
    只约束写/删）；
  - 功能探测 + fail-closed；`Config.RunnerCommand` 自定义 runner；
  - 宿主在 main 开始调用 `osrunner.MaybeRunHelper`（Linux/Windows 需要）。
- **工具接入**：`tools/builtin`（bash/pwsh）、`tools/jobs`、`tools/terminal`、
  `tools/worktree` 新增 `WithRunner`；`runner == nil` 时行为与 v0.13.x 完全一致。
- 文档：[docs/SANDBOX-OS.md](docs/SANDBOX-OS.md)。

### Changed

- `AGENTS.md` 放宽定位：OS 沙箱以 opt-in 的 `sandbox/osrunner` 子包存在，纯 Go、
  默认关闭；核心 `sandbox` 策略层仍不得依赖平台代码。
- README/SECURITY/ARCHITECTURE/COMPATIBILITY 更新 OS runner 的边界说明。

### Removed

为对齐**个人 agent 客户端 SDK（单用户）**的定位，移除以下四块企业/多租户/自建服务形态的能力（共 50 个文件 / 15,488 行）：

- **`tools/escalate` + `kit.Config.Escalation`**：把提权协议包装成模型可调用工具（`request_command_escalation`）的产品通道。个人客户端放宽权限用 `danger-full-access` 或审批即可，不需要"把单条命令送回宿主执行"的旁路；该通道出过 P0 级 RCE。
- **`mcpserver` + `cmd/rigmcp`**：把 `rig.Tool` 暴露成 MCP server 的能力。产品本身是 MCP **客户端**，`tools/mcp`（stdio/Streamable HTTP、OAuth 2.1）不受影响。
- **`internal/filewatch`**：轮询式文件监听与 `Changed`/`ChangedPaths`/`DigestFile` 冲突检测助手。单 agent 场景由 git 自身覆盖，轮询 watcher 只带来扫描开销与"两次扫描之间创建又删除"的假阴性。
- **`sandbox/escalate`（提权协议整包）**：沙箱内请求宿主执行的 NDJSON 协议 + 宿主侧 Broker（`broker.go` 含 `os/exec`）整目录删除，共 15 个文件 / 4,421 行。产品面本来就没有引用（`tools/escalate` 已删、`kit.Config.Escalation` 已删），留着只增加攻击面与维护成本；个人客户端需要放宽权限时直接用 `danger-full-access` 或审批通道。"以后要"时重写一份比维护死代码便宜。`internal/procattr` 保留（`tools/{builtin,jobs,lsp,mcp,ptc,terminal,worktree}` 与 `sandbox/osrunner` 仍在用）。

守护：`internal/rigdoc/surface_test.go` 断言这四个包（含 `sandbox/escalate`）、相关工具名与 `kit.Config.Escalation` 都不会回到发布面。

### 已知结构债（不阻塞发布，记录在此以免被遗忘）

- **`tools/mcp` 里有两个 SSE 解析器**（`http_transport.go` 的行级解析与
  `notifications.go` 的完整帧解析），与 `provider/local` 的 `SSEData` 是同一协议的
  第三份实现。计划 v0.20.1 抽 `internal/sse` 三处共用——`provider/openai` 本轮已经
  收敛，这是同类风险点的最后一处。
- **工具名账本有三处来源**（`tools/catalog.Static`、`tools/presets`、
  `full.All`），靠互锁测试保持一致。长期看更适合"能力注册表"式装配（每个模块自己
  声明需要什么/提供什么），但这会改动 `full.AllOptions` 的形态，留到 v0.21 评估。

### Deprecated

### Fixed

- 稳定 `tools/jobs` 的 awaited-settlement 测试：改用短生命周期但非瞬时的命令，避免
  job 在 waiter 注册前结算造成的 `-race` 抖动（测试修复，无行为变更）。

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

- OS runner 只治理文件效果（写/删/创建），不约束读、网络、进程可见性或设备；
  Windows 为 `partial`；macOS 依赖已弃用的 `sandbox-exec`；Linux 老内核 ABI 为
  `partial`。默认关闭，未显式启用时没有任何行为变化。

## v0.13.2 — 2026-11-09

Windows 命令 allowlist 的平台语义补丁；无破坏性变更。

### Added

### Changed

### Removed

为对齐**个人 agent 客户端 SDK（单用户）**的定位，移除以下四块企业/多租户/自建服务形态的能力（共 50 个文件 / 15,488 行）：

- **`tools/escalate` + `kit.Config.Escalation`**：把提权协议包装成模型可调用工具（`request_command_escalation`）的产品通道。个人客户端放宽权限用 `danger-full-access` 或审批即可，不需要"把单条命令送回宿主执行"的旁路；该通道出过 P0 级 RCE。
- **`mcpserver` + `cmd/rigmcp`**：把 `rig.Tool` 暴露成 MCP server 的能力。产品本身是 MCP **客户端**，`tools/mcp`（stdio/Streamable HTTP、OAuth 2.1）不受影响。
- **`internal/filewatch`**：轮询式文件监听与 `Changed`/`ChangedPaths`/`DigestFile` 冲突检测助手。单 agent 场景由 git 自身覆盖，轮询 watcher 只带来扫描开销与"两次扫描之间创建又删除"的假阴性。
- **`sandbox/escalate`（提权协议整包）**：沙箱内请求宿主执行的 NDJSON 协议 + 宿主侧 Broker（`broker.go` 含 `os/exec`）整目录删除，共 15 个文件 / 4,421 行。产品面本来就没有引用（`tools/escalate` 已删、`kit.Config.Escalation` 已删），留着只增加攻击面与维护成本；个人客户端需要放宽权限时直接用 `danger-full-access` 或审批通道。"以后要"时重写一份比维护死代码便宜。`internal/procattr` 保留（`tools/{builtin,jobs,lsp,mcp,ptc,terminal,worktree}` 与 `sandbox/osrunner` 仍在用）。

守护：`internal/rigdoc/surface_test.go` 断言这四个包（含 `sandbox/escalate`）、相关工具名与 `kit.Config.Escalation` 都不会回到发布面。

### 已知结构债（不阻塞发布，记录在此以免被遗忘）

- **`tools/mcp` 里有两个 SSE 解析器**（`http_transport.go` 的行级解析与
  `notifications.go` 的完整帧解析），与 `provider/local` 的 `SSEData` 是同一协议的
  第三份实现。计划 v0.20.1 抽 `internal/sse` 三处共用——`provider/openai` 本轮已经
  收敛，这是同类风险点的最后一处。
- **工具名账本有三处来源**（`tools/catalog.Static`、`tools/presets`、
  `full.All`），靠互锁测试保持一致。长期看更适合"能力注册表"式装配（每个模块自己
  声明需要什么/提供什么），但这会改动 `full.AllOptions` 的形态，留到 v0.21 评估。

### Deprecated

### Fixed

- **Windows 命令 allowlist 忽略 `.exe` 与大小写**：`Policy.CheckCommand` 现在按
  平台语义比较命令名——Windows 上 `"pwsh"`/`"pwsh.exe"`（及大小写变体）与
  `argv[0]` 的基名双向等价，`WithCommandAllowlist("pwsh", "powershell")` 不会再
  把工具传入的 `pwsh.exe`/`powershell.exe` 误判为 not in the allowlist；denylist
  使用同样的语义。非 Windows 平台行为不变（不剥 `.exe`、大小写敏感）。平台判断
  参数化为 `normalizeCommandName`/`commandNameMatches` 以便在任意宿主上测试两个
  分支。

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

## v0.13.1 — 2026-11-08

OpenAI 兼容 provider 的流截断检测补丁，并修复文档/版本一致性问题；无破坏性
变更。

### Added

- **流完整性校验**：`provider/openai.Config.RequireCompleteStream`。开启后，若事件流在
  `finish_reason` 与 `[DONE]` 都未出现前就关闭，`Recv` 返回新的
  `openai.ErrIncompleteStream`（回答可能被截断）；默认关闭，保持对直接关闭
  连接、不发送终止事件的 OpenAI 兼容服务器的兼容性。

### Changed

### Removed

为对齐**个人 agent 客户端 SDK（单用户）**的定位，移除以下四块企业/多租户/自建服务形态的能力（共 50 个文件 / 15,488 行）：

- **`tools/escalate` + `kit.Config.Escalation`**：把提权协议包装成模型可调用工具（`request_command_escalation`）的产品通道。个人客户端放宽权限用 `danger-full-access` 或审批即可，不需要"把单条命令送回宿主执行"的旁路；该通道出过 P0 级 RCE。
- **`mcpserver` + `cmd/rigmcp`**：把 `rig.Tool` 暴露成 MCP server 的能力。产品本身是 MCP **客户端**，`tools/mcp`（stdio/Streamable HTTP、OAuth 2.1）不受影响。
- **`internal/filewatch`**：轮询式文件监听与 `Changed`/`ChangedPaths`/`DigestFile` 冲突检测助手。单 agent 场景由 git 自身覆盖，轮询 watcher 只带来扫描开销与"两次扫描之间创建又删除"的假阴性。
- **`sandbox/escalate`（提权协议整包）**：沙箱内请求宿主执行的 NDJSON 协议 + 宿主侧 Broker（`broker.go` 含 `os/exec`）整目录删除，共 15 个文件 / 4,421 行。产品面本来就没有引用（`tools/escalate` 已删、`kit.Config.Escalation` 已删），留着只增加攻击面与维护成本；个人客户端需要放宽权限时直接用 `danger-full-access` 或审批通道。"以后要"时重写一份比维护死代码便宜。`internal/procattr` 保留（`tools/{builtin,jobs,lsp,mcp,ptc,terminal,worktree}` 与 `sandbox/osrunner` 仍在用）。

守护：`internal/rigdoc/surface_test.go` 断言这四个包（含 `sandbox/escalate`）、相关工具名与 `kit.Config.Escalation` 都不会回到发布面。

### 已知结构债（不阻塞发布，记录在此以免被遗忘）

- **`tools/mcp` 里有两个 SSE 解析器**（`http_transport.go` 的行级解析与
  `notifications.go` 的完整帧解析），与 `provider/local` 的 `SSEData` 是同一协议的
  第三份实现。计划 v0.20.1 抽 `internal/sse` 三处共用——`provider/openai` 本轮已经
  收敛，这是同类风险点的最后一处。
- **工具名账本有三处来源**（`tools/catalog.Static`、`tools/presets`、
  `full.All`），靠互锁测试保持一致。长期看更适合"能力注册表"式装配（每个模块自己
  声明需要什么/提供什么），但这会改动 `full.AllOptions` 的形态，留到 v0.21 评估。

### Deprecated

### Fixed

- `openai.ErrIncompleteStream` 现在同时 wrap `io.ErrUnexpectedEOF`，调用方可用
  标准 `errors.Is` 判断截断流。
- 补测“部分内容已返回后再返回 `ErrIncompleteStream`”的契约；`FuzzStreamDecode`
  同时覆盖 `RequireCompleteStream=false/true` 两种模式。
- `docs/CONFIG.md` / `docs/zh/CONFIG.md` 的 `openai.Config` 默认值表补齐
  `DefaultReasoningEffort` 与 `RequireCompleteStream`，说明截断流不会被自动重试。
- 修正 `docs/VERSIONING.md` / `docs/zh/VERSIONING.md` 的 release line/current
  version（`v0.13.x` / `v0.13.1`）。

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

## v0.13.0 — 2026-11-07

补齐 `02-sdk-gaps` 审计中的 SDK 缺失能力（additive，无破坏性变更）。

### Added

- **上下文压缩**：根包 `rig.Compactor`/`EstimateTokens`/`SpillPolicy`/
  `CheckpointStore`/`HistoryReplacer`；新增 `session/compact`：
  `WindowCompactor`（system 保留、最近窗口、旧图片卸载、tool_calls/tool 结果
  成块）与 `LLMCompactor`（模型摘要 + 超时/失败回退）。`agent.AgentSpec` 与
  `kit.Config` 增加 `ContextBudget`/`Compactor`；`kit.Agent.CompactNow` 支持
  手动压缩并持久化。
- **工具结果落盘（spill）**：`rig.SpillPolicy` + `kit.Config.Spill`；超过阈值
  的 tool 结果写入文件，模型看到稳定摘要与路径。
- **中断恢复 checkpoint**：根包 `Checkpoint`/`CheckpointPhase`/`CheckpointStore`；
  `agent.RecoverInterruptedTurn` 为中断回合补“结果未知、未重放”的 tool 结果与
  assistant 标记；agent 在模型请求前、工具执行前、正常结束写检查点（fail-closed）。
- **审批 Gate 辅助**：`gate.Ask(rig.Asker)`（allow/allow-always/deny、
  `ErrApprovalExpired`）与 `gate.Review(ToolReviewer)`（模型审前审批、超时/
  错误回退到人工/基础 gate、allowlist、observer）。
- **LSP references/implementation**：`lsp_references`/`lsp_implementation` 工具、
  legacy `lsp` action、`UTF16Len`/`ValidatePosition` 位置校验；LSP 服务端能力
  四态解析，未声明能力时返回可读错误。
- **MCP 通知流**：HTTP GET SSE、`OnNotification`/`OnNotificationError`、
  `Last-Event-ID` 断线恢复、`Close` 时 session DELETE；无处理器时不发起 GET。
- **Goal round driver**：`agent.GoalRoundDriver`（每会话预算、active-only 继续、
  prompt 生成，线程安全）。
- **工具集预设**：`tools/presets`（minimal/standard/all）+ `kit.Config.ToolPreset`。
- **Reasoning effort**：`provider/openai.Config.DefaultReasoningEffort` 与
  `kit.Config.ReasoningEffort`（low/medium/high；`Request.Extra` 可覆盖）。
- **owner 作用域清理**：`terminal.WithOwner`/`CloseAllOwned`、`jobs.WithOwner`/
  `CloseAllOwned`，owner 显示在 session/job 列表。

### Changed

- `gate` 包文档修正“内置门禁都是无状态的”表述（新增 Ask/Review gate）。
- `tools/lsp` `Tools()` 4→6；legacy `lsp` action 4→6（配置 LSP client 时）。
  静态 `full.All` 仍为 **81** 个工具（LSP 工具仍 server-bound）。
- 文档同步：README、docs/TOOLS、docs/CONFIG、docs/zh、AI-SDK、CHANGELOG。

### Notes

- `gate.Review` 的拒绝原因通过 `ReviewOutcome`/observer 暴露；agent 当前工具错误
  文本仍是固定 deny 文案（未新增 reason 通道）。
- 压缩只作用于内存对话；持久化替换需要 History 实现 `HistoryReplacer`
  （`session.Memory` 已实现）。
- 已知限制：`_test.go` 注释仍保留英文；模型可见 Description/Schema/错误文案
  保持英文。

### Fixed

- **`run_code`（PTC）此前是 OS 沙箱旁路**：`tools/ptc` 直接 `exec` 解释器，未接 `Runner`。现在 `ptc.Config` 接受 `Runner`+`Policy`（`kit.Config.PTC` 自动透传），解释器进程与 `bash` 同样经 `Confine` 包装；`Runner` 非 nil 而 `Policy` 为 nil 时 fail closed。程序临时文件改放策略根（`bwrap` 的 workspace-write 会挂私有 tmpfs `/tmp`，宿主 `/tmp` 里的脚本在沙箱内不存在），并在失败结果里追加结构化沙箱归因。

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

## v0.12.1 — 2026-11-07

文档与 AI 可读性 release；**无代码行为变更**。

### Added

- 中文优先文档：`README.md` 改为中文主文档（`README.en.md` 保留英文），新增
  `docs/zh/` 全量指南（TOOLS/CONFIG/ARCHITECTURE/SECURITY/COVERAGE/
  VERSIONING/RELEASE/COMPATIBILITY）与中文索引。
- AI 使用入口：`AGENTS.md`、`llms.txt`、`docs/zh/AI-SDK.md`（组装方式、工具
  用法/能力、宿主依赖、权限与常见陷阱）。
- 机器可读工具目录：`cmd/rigdoc` 从真实 `tools/full.All()` 生成
  `docs/ai/tools.json` 与 `docs/ai/tools.md`（82 条：81 静态 + opt-in
  `run_code`，含 JSON Schema、分类、DSH 对应、可用性）。
- CI 门禁：`go run ./cmd/rigdoc -check` 防止 AI 目录漂移；`cmd/rigdoc` 测试
  同时校验文档计数（81 static / 65 catalog Modern / 55 audit Modern /
  v0.12.1）。
- 中文包文档与注释：全部 41 个包的包文档为中文 `doc.go`/包注释，公开 API 与
  内部注释中文化（仅注释变化，AST/测试验证无代码/字符串变更）。

### Changed

- 文档一致性修复：81 静态工具/65 catalog Modern/55 audit Modern、v0.12.x 支持
  线、唯一依赖 `mvdan.cc/sh/v3`、`CGO_ENABLED=0`、无 OS 沙箱声明；历史
  `docs/AUDIT-REPORT.md` 标注为 v0.8 快照；README/en/docs/zh 交叉链接与
  quickstart 代码块同步。
- 英文旧文档加中文优先 banner，指向 `README.md` 与 `docs/zh/`。

### Notes

- **模型可见的工具 Description/JSON Schema/错误文案保持英文**，因为它们是
  模型接口与 DSH 对齐的一部分；中文解释放在包文档与 `docs/zh/`。
- `_test.go` 注释不在 pkg.go.dev 渲染范围内，本批未强制中文化。
- 文档与代码冲突时以代码为准；中文文档为默认阅读版本，英文原文保留参考。


## v0.12.0 — 2026-11-07

DSH capability sync (second batch): asynchronous subagents/team control, typed
scheduling, revision-CAS goals, background jobs/terminal/bash, and opt-in PTC.

### Added

- **Async subagents and DSH team names** (`tools/team`): `subagent` and
  `subagent_fork` now use the async team manager: they return a target id
  immediately and deliver a completion notice through the new
  `rig.NoticeSource` capability, which the agent appends as a user message at
  the next step. Added `spawn_teammate`, `list_agents` and `wait_agent` (next
  team change; `noProgress` when nothing can progress); `send_message` remains
  non-blocking; `team_*` and legacy names remain as extensions.
- **Typed scheduling** (`tools/schedule`): `at`, `after_seconds`,
  `every_seconds`, `daily`, `weekly` and `cron` selectors with time-zone and
  cron canonicalization, `title`/`name` handling, and legacy free-form
  `schedule` strings kept; canonical stored grammar is documented in the
  package.
- **DSH goal tools** (`tools/hosttools`): `get_goal`, `create_goal`,
  `update_goal` with exact id+revision compare-and-set, actions
  edit/pause/resume/complete/blocked and `blocked_reason`; implemented over the
  optional `hosttools.GoalRevisionStore` (the in-memory host implements it).
  The legacy `goal` tool remains.
- **Background execution** (`tools/jobs`, `tools/builtin`, `tools/terminal`):
  new internal `tools/internal/background` producer contract; jobs support
  process and terminal-send kinds, DSH statuses/notices, `job_output` wait/
  `timeout_ms`, `job_kill` reason and kind/label listing. `bash`/`pwsh` accept
  `description`, `run_in_background` and foreground-timeout promotion;
  `terminal_*` accept DSH `sessionId`/`text`/`submit`/`offset`/`count`/`type`/
  `name`, paginated reads, SIG* signals and background sends. `full.All` wires
  the jobs manager into the shell and terminal tools and drains job completion
  notices through `rig.NoticeSource`.
- **PTC** (`tools/ptc` + `kit.Config.PTC`): opt-in `run_code` tool that executes
  model-authored Python 3/Node.js in a **system-installed runtime** (subprocess
  bridge over stdin/stdout, no embedded JS engine), exposing existing tools as
  `tools.<name>` with every inner call checked by the configured gate.

### Changed

- `tools/full.All` now builds **81 static tools** (previously 75): async
  subagent/team names and the three goal tools. `run_code` is an opt-in kit
  extension, not a static tool.
- `present` (first batch) remains DSH-compatible file declaration;
  `present_artifact` remains the artifact extension.
- `workflow` remains the Go-native declarative orchestrator (sequential/parallel
  steps, templating); DSH's JavaScript workflow/PTC program is intentionally not
  embedded. PTC instead executes user code in an installed runtime.
- Docs/catalog/audit mappings updated (static 81, catalog Modern 65, audit
  Modern 55; reference=63/mapped=57/gaps=6 unchanged).

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

- PTC inner tool calls pass through the same `rig.Gate` as ordinary calls; the
  host can restrict which tools are visible via `ptc.Config.Tools`.
- `sandbox_permissions`/`justification` are accepted on bash/pwsh/terminal
  schemas but always rejected with DSH's "no sandboxing executor" error: Rig has
  no per-call escalation executor.

### Notes / remaining gaps

- Still DSH-host-/kernel-specific: `load_workspace_dependencies`,
  `cordis_inspect_*`, `plugin_manager`, `plugin_market`.
- Not implemented from DSH details: output spill files (ring-only), owner-scoped
  terminal backend registry, DSH goal authority/driver rules (direct-human,
  autonomous-round threshold), DSH cron "no future occurrence" rejection and
  strict `additionalProperties` rejection; `run_code` uses Python/Node system
  runtimes instead of DSH's embedded PTC runtime.
- `run_code` is an opt-in kit extension rather than a `full.All` static tool.

## v0.11.0 — 2026-11-07

Scope correction. The OS-sandbox experiment is removed; Rig now mirrors the
DeepSeek Harness permission model: coarse sandbox modes plus an approval policy.
OS isolation is a deployment concern (container/VM), not an SDK feature.

### Breaking

- **Removed the opt-in OS-sandbox API**: `sandbox.NewOSExec`,
  `sandbox.OSExecOptions`, `sandbox.OSExec`, `sandbox.ErrOSExecUnavailable`,
  `sandbox.WithOSExec`, `(*sandbox.Policy).OSExec`, `(*sandbox.Policy).CommandContext`
  and `kit.Config.OSExec`. The `v0.10.1` tag (native Landlock/seccomp experiment)
  was never released and is superseded by this release; consumers of `v0.10.0`
  that attached an OS sandbox should switch to a permission Mode + approval and
  run the host in a container/VM for untrusted models.
- Shell/git/terminal processes are again plain `os/exec` calls; the PTY transport
  no longer has a sandbox fallback branch.

### Added

- **Permission modes** (`sandbox.Mode`, `sandbox.WithMode`):
  `ModeReadOnly` (deny file mutations and shell execution),
  `ModeWorkspaceWrite` (default: writes only under the workspace roots, shell only
  via the allowlist) and `ModeDangerFullAccess` (no root/allowlist fence; explicit
  denylist still applies). Unknown modes fail closed.
- **Approval policy** in `kit`: `ApprovalAsk` (default fail-safe gate: read-only
  tools allowed, the rest denied), `ApprovalAuto` (allow every call) and
  `ApprovalNever` (deny every call), plus `kit.Config.Mode`/`kit.Config.Approval`
  and `kit.Mode*`/`kit.Approval*` aliases. A non-nil `Config.Gate` always wins.
- Tests for mode semantics (read-only, workspace-write, danger-full-access and
  invalid-mode fail-closed) and kit mode/approval validation.

### Changed

- README/SECURITY/ARCHITECTURE/CONFIG/TOOLS now document the permission model and
  state plainly that these are in-process fences, not a kernel boundary; the
  removed OS-sandbox sections are replaced by "run the host in a container/VM".
- Kept from v0.10.0: `mvdan.cc/sh/v3` POSIX parser (single validation+argv
  parser for bash/pwsh/jobs/terminal), the cgo-free contract (`CGO_ENABLED=0`),
  and the completed standard-library modernization.

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

- The project no longer claims an OS-level sandbox. Permission modes constrain
  the shipped tool implementations; the approval Gate remains the boundary for
  irreversible actions, and untrusted models require container/VM isolation.

### Notes for consumers

- `v0.10.0` OS-sandbox users: remove `sandbox.WithOSExec` / `kit.Config.OSExec`
  and choose a `Mode` + `Approval`; for strong isolation run the host under a
  VM (as OpenHands does) and keep approvals enabled.
- `v0.10.1` was tagged during development but never published; its release notes
  are intentionally superseded by this section.

## v0.10.1 — unreleased (superseded by v0.11.0)

Native, dependency-free Linux OS sandbox (no external binary) and completion of
the standard-library modernization across code and tests.

### Added

- **Native Linux OS sandbox backend `landlock` (default).** `sandbox.NewOSExec`
  now re-execs the host binary with a hidden marker; a small `init()` runner
  applies Landlock write confinement (workspace + `WritableDirs` + `/tmp` +
  `/dev/null`/`tty`) and, when `AllowNetwork` is false, a seccomp filter denying
  `socket(AF_INET|AF_INET6)` and `io_uring_setup`, then `execve`s the real
  command. Pure Go standard library: **no bwrap/system binary, no cgo, no new
  module dependency**. The PTY terminal transport now works under this backend
  (previously it fell back to pipe).
- Integration tests: native filesystem confinement, seccomp network denial (with
  `AllowNetwork` enabled as the control), and PTY-under-sandbox. The real-bwrap
  integration tests remain for the optional backend.
- `Backend: "bwrap"` stays available; auto mode falls back to bwrap only when
  Landlock is unavailable and bwrap exists, otherwise it fails closed with
  `sandbox.ErrOSExecUnavailable`.

### Changed

- `sandbox.OSExec` Linux default backend is now `landlock` (`Backend()` reports
  it). No API removal; `bwrap` is opt-in/fallback.
- Completed standard-library modernization across packages and tests:
  `sort`→`slices` (30 hand-written sort lines deleted), `append([]T(nil),…)`→
  `slices.Clone` (54), `errors.As`→`errors.AsType` (14), `os.IsNotExist`→
  `errors.Is(err, fs.ErrNotExist)` (11), hand-rolled `itoa` removed (3),
  `fmt.Sprintf("%d")`→`strconv.Itoa/FormatInt` (5), `HasPrefix`+`TrimPrefix`→
  `strings.CutPrefix` (7), manual clamps→builtin `min` (3), `bytes.Index`→
  `bytes.Cut`, `slices.Delete/Contains`, `maps.Keys`+`slices.Sorted`,
  `sync.WaitGroup.Go`, `atomic.Int64`. No mechanical stdlib-replacement pattern
  remains in non-generated code.
- Docs/README/SECURITY/ARCHITECTURE/CONFIG updated: Linux needs no installed
  sandbox helper (Landlock ABI>=1, Linux 5.13+; network filter linux/amd64+arm64);
  macOS keeps the system-provided `sandbox-exec` (a pure-Go Seatbelt backend would
  require cgo, which is forbidden); Windows uses the native restricted token +
  built-in `icacls` low-integrity labels (network not isolated).

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

- Removes the bubblewrap install/PATH dependency from the default Linux sandbox
  path (smaller install and supply-chain surface); Landlock fails closed when the
  kernel does not support it.
- seccomp also blocks `io_uring_setup`, closing the "create a socket without the
  socket syscall" bypass.
- Landlock/seccomp are applied before `execve` in a re-exec'd copy of the host
  binary; restrictions are inherited across exec and cannot be dropped.

### Notes for operators

- Linux: kernel 5.13+ with Landlock enabled; on other architectures or kernels
  without Landlock, use `Backend: "bwrap"` or leave network enabled (Landlock-only
  mode needs no seccomp arch support).
- macOS: `sandbox-exec` is part of macOS; pure-Go native Seatbelt is not possible
  without cgo. Windows: filesystem/privilege confinement only, no network
  isolation (documented).

## v0.10.0 — 2026-11-07

Adds an opt-in OS-level process sandbox, replaces the hand-written shell scanner
with the established `mvdan.cc/sh/v3` POSIX parser, modernizes the toolset onto
the Go standard library, and makes cgo-free builds a CI contract.

### Breaking

- **One module dependency.** Rig now requires `mvdan.cc/sh/v3 v3.14.1` (pure Go,
  no transitive build dependencies) for shell parsing. `go list -m all` on a
  consumer shows this module plus that dependency; the module root stays
  standard-library-only and every build remains cgo-free (`CGO_ENABLED=0`).
- **Shell policy behavior.** The parser now models the shell's complete POSIX
  grammar, so `$((...))` arithmetic expansion is allowed when it contains no
  command substitution (previously rejected wholesale), comments are handled like
  the shell, and process substitution is reported as a POSIX parse error. The
  `ParseShellCommand`/`CheckShellCommand` signatures are unchanged; argv[0] may
  now be quoted (`'git' status` works) as long as it is a static literal.
- **Legacy constructors stay outside the sandbox.** `builtin.NewShell`,
  `jobs.NewManager()` and `terminal.NewManager()` without `WithPolicy` still spawn
  without policy or OS confinement (unchanged, documented); use the policy-aware
  constructors with `sandbox.WithOSExec`.

### Added

- **`sandbox.NewOSExec` / `sandbox.WithOSExec` / `kit.Config.OSExec`**: opt-in
  OS-level process sandbox for bash, pwsh, jobs, terminal and worktree git.
  - Linux: bubblewrap (`bwrap`), unprivileged; writable roots bind-mounted,
    read-only `/`, private `/tmp`, unshared PID/net/user namespaces, network off
    unless `AllowNetwork`.
  - macOS: `sandbox-exec` Seatbelt profile; writes limited to workspace/writable
    dirs + temp, network denied unless allowed.
  - Windows: restricted token + low integrity + `icacls` labels on writable
    roots; privileges dropped, writes confined to the workspace. **Network is not
    isolated on Windows** (use a container/VM for untrusted models).
  - Other platforms: fail closed with `sandbox.ErrOSExecUnavailable`.
  - All backends are cgo-free: system helper via `os/exec` on Linux/macOS, the
    standard library restricted-token `SysProcAttr` on Windows.
- `sandbox.ParseShellCommand` is now the single validation+argv parser shared by
  bash, pwsh, jobs and terminal (the three duplicated splitters are gone).
- `FuzzParseShellCommand` remains the parser fuzz target; new integration tests
  execute real `bwrap` sandboxed commands (writes inside/outside, network
  namespace) and construct the full 74-tool surface with an OS sandbox attached.
- `kit.Config.OSExec` (cgo-free convenience wiring; must be attached via
  `sandbox.WithOSExec` when a custom `Config.Policy` is used).

### Changed

- Shell parsing: the ~300-line hand-written lexer/continuation scanner is replaced
  by `mvdan.cc/sh/v3` AST validation plus a small POSIX line-continuation
  pre-pass; quoting, escapes, comments and continuation semantics come from the
  maintained parser.
- **Standard-library modernization across tools**: `sort` → `slices.Sort` /
  `slices.SortFunc` / `slices.SortStableFunc` (with `cmp.Compare`/`cmp.Or`),
  `append([]T(nil), …)` → `slices.Clone`, manual caps → builtin `min`/`max`,
  `sort.Ints`-dead-work removal, `slices.Contains`/`slices.Delete`,
  `maps.Keys` + `slices.Sorted`, `strconv.FormatInt` instead of hand-rolled itoa,
  `errors.Is(…, fs.ErrNotExist)`, `cmp.Or` in the provider, `sync.WaitGroup.Go`
  and `atomic.Int64` in the agent/workflow fan-out.
- CI: workflow-wide `CGO_ENABLED=0`; new **cgo-free** gate (asserts no package has
  `CgoFiles` and runs the suite with cgo disabled) and **dependency policy** gate
  (built imports are exactly the parser); the race step is the only cgo tooling
  use and is documented as such. Release workflow also runs cgo-free.
- `Makefile`: `CGO_ENABLED ?= 0` exported for all gates; `make race` opts into
  cgo tooling explicitly; `make deps` now enforces the dependency policy.
- `docs/CONFIG.md` documents `sandbox.NewOSExec` and Windows' network limitation;
  README/ARCHITECTURE/SECURITY/TOOLS/COMPATIBILITY/CONTRIBUTING/PR template were
  updated for the dependency, OS sandbox and cgo-free contract.

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

- **OS-level confinement (opt-in)** for every process-spawning model-facing tool;
  the in-process policy and approval Gate stay the policy/approval layers.
- The POSIX shell parser is now a maintained, fuzz-tested library
  (`mvdan.cc/sh/v3`) rather than a hand-rolled scanner; line continuations are
  still normalized to the shell's own behavior before AST validation.
- Supply chain: one pure-Go dependency, no cgo, `govulncheck` covers it, module
  checksums remain in `go.sum`/`sum.golang.org`.

### Docs

- README: dependency/cgo/OS-sandbox sections, platform support matrix for the
  sandbox backends, CI gate table, release history.
- `docs/ARCHITECTURE.md`: OS-sandbox layer and cgo-free contract; intention list
  updated (no longer claims OS sandboxing is absent).
- `docs/RELEASE.md`: release gates include the dependency-policy/cgo-free gates.
- `docs/VERSIONING.md`: current release line `v0.10.x`.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.10.0` is a drop-in upgrade for
  library consumers; the extra module is a pure-Go parser with no cgo or
  transitive build dependencies. To use the OS sandbox, construct
  `sandbox.NewOSExec` and attach it via `sandbox.WithOSExec` (or
  `kit.Config.OSExec`); install `bubblewrap` on Linux. Windows sandboxing confines
  filesystem writes/privileges but not the network.

## v0.9.4 — 2026-11-07

Security and correctness patch for the shell policy and LSP framing, plus
documentation-truth and release-gate corrections. No breaking changes: the
module path and root contract are unchanged (`rig.Version` reports `0.9.4`).

### Added

- `sandbox.ParseShellCommand(command string) ([]string, error)`: the single
  validation + word-splitting parser used by the built-in `bash`, `jobs` and
  `terminal_send` tools. `CheckShellCommand` remains as the validation-only
  API and delegates to the same parser.
- Fuzz target `FuzzParseShellCommand` (registered in CI) covering quotes,
  escapes, line continuations, comments and substitution constructs.

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

- **POSIX line continuations are modeled by the shell policy.** v0.9.1 rejected
  quoted `$(...)`/backticks but `/bin/sh` removes backslash-newline before
  tokenizing, so `echo "$\<newline>(id)"` (and the `${x:-$\<newline>(id)}`
  variant) still reached `/bin/sh` as `"$(id)"` and executed `id` with an
  allowlist of `echo`. The parser now strips backslash-newline outside single
  quotes with the shell's quote rules and validates the joined text; the
  regression corpus covers quoted, unquoted and parameter-expansion variants.
- **One parser for validation and argv[0].** The previous three copies of a
  quote-unaware splitter are gone; a divergence between the validator and the
  allowlist check can no longer be introduced per tool.
- **LSP headers are bounded before allocation, including one unterminated
  line.** v0.9.1 capped multi-line headers, but `bufio.Reader.ReadString`
  allocated the whole line before the cap applied (a 96 MiB stream without a
  newline grew the heap by ~92 MiB in the re-audit). The reader now uses
  `bufio.Reader.ReadLine` under a hard 64 KiB budget and a 128-line cap.

### Changed

- Shell comments that begin a word are handled like the shell. `$((...))`
  arithmetic expansion and parentheses inside `${...}` remain rejected
  conservatively because they may contain command substitution; pass
  `sandbox.WithAllowShellOperators` when a host needs full shell syntax and
  accepts the advisory allowlist.

### Docs

- Removed the "Windows/macOS test matrix" claim from README, `docs/RELEASE.md`
  and the release workflow comment: CI runs the unit suite on Linux and
  cross-compiles + `GOOS`-vets windows/darwin/freebsd (hosted macOS/Windows
  runners are unavailable for this repository).
- Updated `docs/VERSIONING.md` current release line, `docs/TOOLS.md`
  host-interface legend, `docs/ARCHITECTURE.md` shell/LSP security sections,
  `tools/lsp/doc.go` limits, and the README CI/security/platform text.
- Documented the repository-protection limits honestly (single maintainer,
  `enforce_admins=false`, GitHub commit verification needing
  `admin:ssh_signing_key`, release environment without a required reviewer);
  see the new "Repository protection status" section in `docs/RELEASE.md`.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.9.4` is a drop-in upgrade from
  v0.9.3. Scripts that relied on `echo "$((1+2))"` passing the allowlist policy
  need to quote the expansion result or compute it outside the shell; this is
  the conservative documented behavior since v0.9.1.

## v0.9.3 — 2026-11-07

Release-engineering patch; the SDK code is identical to v0.9.1/v0.9.2.

### Fixed

- **Tag signature verification, follow-up.** v0.9.2 restored the annotated tag
  object but passed the full ref (`refs/tags/vX.Y.Z`) to `git tag -v`, which
  resolves names under `refs/tags/` itself and therefore failed with *"tag not
  found"*. v0.9.3 passes the plain tag name after restoring and checking the tag
  object, so the release workflow verifies the SSH signature against
  `.github/allowed_signers` and proceeds to the clean-room smoke and draft
  release.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.9.3` behaves exactly like v0.9.1;
  this patch only changes how the GitHub Release is produced.

## v0.9.2 — 2026-11-07

Release-engineering patch; the SDK code is identical to v0.9.1.

### Fixed

- **Release workflow signature verification.** `actions/checkout` rewrites
  `refs/tags/<tag>` to the commit SHA for tag events, so the verify job saw a
  lightweight tag and failed with *"cannot verify a non-tag object of type
  commit"*; this blocked creation of the v0.9.1 GitHub Release (the tag and the
  module itself were already published and immutable). The workflow now
  force-fetches the annotated tag object from the remote, requires it to be of
  type `tag`, and only then runs `git tag -v` against
  `.github/allowed_signers`.

### Notes for consumers

- `go get github.com/zeokeel/zeokeel-rig@v0.9.2` behaves exactly like v0.9.1;
  this patch only changes how the GitHub Release is produced.

## v0.9.1 — 2026-11-07

Security and correctness hotfix for the v0.9 line, plus release-engineering and
documentation-truth fixes. No breaking changes: `rig.Version` reports `0.9.1`,
the module path and the root contract surface are unchanged.

### Security

- **拒绝判定接入退出码**：新增 `sandbox.DetectDenialWithExit`（抑制 2/126/127 的误报、识别 `128+SIGSYS` 的漏报），bash/pwsh/jobs/worktree/run_code 都传退出码；归因摘要行带上 `exit N` / `signal SIGSYS`。
- **沙箱拒绝审计钩子**：`sandbox.WithDenialSink` / `Policy.SetDenialSink` + `full.AllOptions.SandboxDenialSink` / `kit.Config.SandboxDenialSink`，一次配置覆盖所有执行工具，拿到结构化 `Denial`（模式、enforcement、路径、退出码/信号、可写面）。
- **`deniedGlobs` 的 OS 层缺口可见化**：新增 `Policy.DeniedGlobs()` 与 `rigdoctor -deny`，配置了 glob 但未挂 OS 强制时会明确告警（进程内策略只约束内置工具，沙箱里的 shell 仍可读到匹配文件）。
- **工作区元数据保护（opt-in）**：`full.AllOptions.ProtectWorkspaceMetadata` / `kit.Config.ProtectWorkspaceMetadata` 为每个根追加 `.git`（worktree 时含 `gitdir:` 指针目标）、`.aws`、`.codex`，防止被约束的 agent 改写自己的守卫与凭据；默认关闭以避免改变 git 工作流。
- **沙箱本体防劫持 + 探测分类**：bwrap / sandbox-exec 候选若 group/world 可写、目录世界可写或落在工作目录内一律拒收；bwrap 探测失败会分类为"未安装 / 用户命名空间被禁 / WSL1 / profile 被拒"并进入错误文本。
- **Windows read-only 完整性**：read-only 现在真的把令牌降到 Low 完整性（`windowsGrantShape` 早就这么声明，但 helper 的 read-only 分支此前从不调用）——no-write-up 让写任何 Medium 对象都失败，与"只读"一致；降级失败时打印诊断并退回只依赖 restricting 交集，绝不放宽写授权。
- **Windows 进程树**：子进程改在 kill-on-close Job 中运行（`CREATE_SUSPENDED → Assign → Resume`），顶层命令退出后先收树再恢复 per-run 形状，脱离的后代不再在脚下失去授权；Job 不可用时 fail closed；看门狗超时返回固定退出码 124 并输出可识别文案。

- **Windows 真机验证进入 CI**：新增 `windows-sandbox` job（`windows-latest`，初始 `continue-on-error`），跑 `RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntime`。Wine **不能**用于验证该后端（实测 restricting SID/Low 完整性/ACL 传播均不生效，`scripts/wine-probe/` 保留可复现的负结果与 `docs/SANDBOX-OS.md` §10 的实测表）。

- **Seatbelt 写围栏补洞**：显式 `(deny system-fcntl (fcntl-command 80 110))`（F_MAKECOMPRESSED/F_TRANSFEREXTENTS 可通过只读描述符改文件）与 `(deny user-preference-write)`（CFPreferences 经 cfprefsd 代写 `~/Library/Preferences`）；pty 收窄为 `/dev/ptmx` + `/dev/ttys[0-9]+`，不再整目录放行 `/dev/pts`；`sandbox-exec` 固定取 `/usr/bin/sandbox-exec` 并拒绝 group/world 可写候选。
- **Windows 掩码修正**：`winProtectDenyMask` 由错误的 `0x00010076` 改为文档所述的 `0x00010156`（此前漏掉 `FILE_WRITE_ATTRIBUTES`，保护路径仍可改属性/时间戳；又多拒了 `FILE_EXECUTE`）；`winGrantMask` 补 `READ_CONTROL`（GENERIC_WRITE 会映射到该位，缺失会导致"用 GENERIC_WRITE 打开文件"被整条拒绝）；新增字面量锚点测试防止常量自比。
- **保护路径不再是 fail-open**：`ProtectedPaths` 保留尚不存在的项（此前被静默丢弃，会话中途 `git init` 的 `.git` 无人保护），Seatbelt 对缺失项照常下规则，其余后端把缺失项放进 `Profile.MissingProtected` 并由 `rigdoctor` 报警。
- **系统临时目录补 `/var/tmp`**（macOS 另含 `/private/var/tmp`），作为构建工具的 scratch 目录；仍受 `WithSlashTmp` 控制。

- **Windows ACL 后端**：保护路径（`ProtectedPaths`）现在真正强制（保护目录及其既有子树逐对象 deny ACE，读仍允许；非法/不存在的 protect 值 fail closed）。同时修复三个缺陷：部分应用失败会留下永久 deny ACE/Low 标签残留（改为单次 `SetNamedSecurityInfoW` 合并 DACL+LABEL，并在改动前先落盘原始描述符、失败即同锁回滚）；每次执行 3~5 次全树 ACL 传播（改用由规范化路径派生的确定性 capability SID + 精确 ACE 幂等跳过，能力 ACE 与 protect deny ACE 作为缓存资产复用）；`unsafe.Offsetof` 多级 selector 在真机上给出内层偏移导致运行时测试必失败、并让 `winLayoutSupported` 依赖未定义语义（改为显式相加）。

- **Shell allowlists are now enforced against quoted command substitution.**
  v0.9.0 rejected unquoted `$(...)`/backticks but allowed them inside double
  quotes, where `/bin/sh` still executes them: `echo "$(id)"` bypassed an
  allowlist that did not include `id`. `sandbox.CheckShellCommand` now rejects
  command substitution `$(...)`, backticks and process substitution `<(...)`/
  `>(...)` anywhere except single quotes, and the check is applied by `bash`,
  `pwsh`, `jobs` and `terminal_send`.
- **Environment-assignment and path-form command hijacks are rejected.**
  `PATH=. tool`, `LD_PRELOAD=... tool` and `./tool`/`/path/tool` could make a
  model-provided command resolve to a workspace binary while the allowlist only
  saw an allow-listed basename. `sandbox.CheckCommand` now rejects leading
  `VAR=value` assignments and path-form command names: allowlisted commands are
  bare names. Documented caveat: allowlisting an interpreter (`sh`, `env`,
  `xargs`, `python`, `eval`, ...) still yields arbitrary code execution.
- **`web_fetch` SSRF guard covers policies without `web.HostResolver`.**
  `web.PolicyFunc`/custom `CheckURL` policies previously fell back to the
  standard dialer, so a public name resolving to loopback was dialed. The
  fetcher now resolves and validates every dialed host itself and rejects
  loopback/private/link-local/CGNAT/cloud-metadata addresses for every non-nil
  policy; `web.WithAllowPrivateHosts()` is the explicit opt-out for local
  targets (used by the tests and by hosts fetching local mirrors).
- **LSP message headers are bounded** (64 KiB) before allocation, closing the
  unbounded `ReadMIMEHeader` allocation (a hostile server could make the client
  allocate hundreds of MiB before the 16 MiB message cap applied).
- **LSP tools gained an installable path policy.** `lsp.WithPathPolicy` (wired by
  `tools/full.All` from the sandbox policy) validates model-provided paths before
  the tool reads a file, so `lsp_*` can no longer read arbitrary files outside
  the sandbox and leak them through language-server diagnostics.
- **`terminal_*` is bounded consistently with its documentation.** The shell
  binary must pass the command allowlist and each `terminal_send` line runs the
  same operator/substitution and command checks as the non-interactive shell.
- **Agent tool fan-out is bounded.** One model round now executes at most 8 tool
  bodies in parallel (`maxParallelToolCalls`) instead of one goroutine per call;
  a response with thousands of tool calls can no longer fan out thousands of
  concurrent executions.
- **Approval gates see normalized tool names.** A leading space in a
  model-provided tool name no longer slips past name-matching gates or the
  allow-always cache; dispatch, events, history and the gate all use the
  trimmed name.

### Fixed

- **Cancelled and gate-error tool rounds keep history replayable.** A run
  cancelled mid-tool, cancelled in the approval gate, or aborted by a gate error
  used to persist an assistant `tool_calls` message with no `role=tool` answers;
  the next request on that session was rejected (HTTP 400) forever. Every tool
  call is now answered with a tool result (synthetic cancellation/denial results
  where needed), and history writes use a non-cancelled context.
- **`team_spawn` no longer deadlocks at the runner concurrency limit.** The team
  manager held its lock while `SubagentRunner.Start` waited for capacity, so the
  fifth spawn (runner limit 4, member limit 8) blocked `team_list`/`team_wait`/
  `team_close` and every other team tool forever. Spawn reserves a slot, releases
  the lock and starts the child with the caller's context; the member limit also
  counts in-flight spawns.
- CI race fixed in the new coverage gate and lint job versions are pinned.

### Changed

- **Release workflow reuses the full CI gate set.** `.github/workflows/ci.yml` is
  now a reusable workflow (`workflow_call`) and `.github/workflows/release.yml`
  calls it, so the tag build runs exactly the merge gates (gofmt, build, vet,
  tests, race, coverage floors, fuzz, benchmarks, catalog/audit gates,
  zero-dependency and root-leaf policy, README drift, cross-compile + `GOOS`
  vet, examples, staticcheck, govulncheck).
- **Signed tags, tag-on-main check and clean-room smoke before a draft release.**
  The release workflow verifies the annotated tag's SSH signature against
  `.github/allowed_signers`, verifies the tagged commit is on `main`, runs
  `scripts/sync-version.sh --require-tag`, does a clean-room `go get`+
  `go mod verify` of the published module, and creates a **draft** GitHub
  Release. `publish-release.yml` (manual dispatch, `confirm_reviewed=yes`)
  publishes the draft after the post-release re-audit. `contents: write` is
  scoped to the publish job; all actions are pinned by commit SHA.
- **Coverage is gated per package, not only in total.** CI and `make ci` enforce
  80% total statement coverage *and* >=70% function coverage for every package
  with tests (`scripts/coverage-floor.sh`), so a large well-tested package cannot
  mask an under-tested one.
- **CI gained a `lint` job (staticcheck v0.8.2, govulncheck v1.9.0)** and
  cross-compile now also runs `go vet` with `GOOS` set (windows/darwin/freebsd),
  which compile-checks the tests too; `gofmt`/`make fmt` scan tracked Go files
  only. Hosted macOS/Windows runners are unavailable for this repository, so
  non-Linux runtime coverage is a documented limitation (README "Platform
  support"), not a claim.
- **Unix-only tools fail clearly on Windows**: `bash`, `jobs` and `terminal_*`
  report a configuration error instead of a missing-`/bin/sh` crash; use the
  `pwsh` tool. Platform support is documented in README/ARCHITECTURE/TOOLS.
- `report` closes its `os.Root` handle so the report file is not left locked.
- **Documentation truth pass.** Corrected the v0.9.0 shell/web_fetch hardening
  claims, the `terminal_*` security text, the `tools/full` and `docs/TOOLS.md`
  "in-memory defaults" overclaim, the CONTRIBUTING coverage contradiction, the
  RELEASE/VERSIONING process text, the `doc.go` import-graph claim, the
  `examples/custom-tool` package reference, the COMPATIBILITY Go-min rows and
  the AUDIT-REPORT snapshot note. Non-routable `dev@zeokeel.local` contacts were
  replaced with the private GitHub advisory channel (the only private channel).
- **Retroactive GitHub Releases** were created for v0.1.0–v0.8.0 from their
  CHANGELOG sections, so every published tag now has release notes. Tags are
  immutable and were not moved.

### Removed

为对齐**个人 agent 客户端 SDK（单用户）**的定位，移除以下四块企业/多租户/自建服务形态的能力（共 50 个文件 / 15,488 行）：

- **`tools/escalate` + `kit.Config.Escalation`**：把提权协议包装成模型可调用工具（`request_command_escalation`）的产品通道。个人客户端放宽权限用 `danger-full-access` 或审批即可，不需要"把单条命令送回宿主执行"的旁路；该通道出过 P0 级 RCE。
- **`mcpserver` + `cmd/rigmcp`**：把 `rig.Tool` 暴露成 MCP server 的能力。产品本身是 MCP **客户端**，`tools/mcp`（stdio/Streamable HTTP、OAuth 2.1）不受影响。
- **`internal/filewatch`**：轮询式文件监听与 `Changed`/`ChangedPaths`/`DigestFile` 冲突检测助手。单 agent 场景由 git 自身覆盖，轮询 watcher 只带来扫描开销与"两次扫描之间创建又删除"的假阴性。
- **`sandbox/escalate`（提权协议整包）**：沙箱内请求宿主执行的 NDJSON 协议 + 宿主侧 Broker（`broker.go` 含 `os/exec`）整目录删除，共 15 个文件 / 4,421 行。产品面本来就没有引用（`tools/escalate` 已删、`kit.Config.Escalation` 已删），留着只增加攻击面与维护成本；个人客户端需要放宽权限时直接用 `danger-full-access` 或审批通道。"以后要"时重写一份比维护死代码便宜。`internal/procattr` 保留（`tools/{builtin,jobs,lsp,mcp,ptc,terminal,worktree}` 与 `sandbox/osrunner` 仍在用）。

守护：`internal/rigdoc/surface_test.go` 断言这四个包（含 `sandbox/escalate`）、相关工具名与 `kit.Config.Escalation` 都不会回到发布面。

### 已知结构债（不阻塞发布，记录在此以免被遗忘）

- **`tools/mcp` 里有两个 SSE 解析器**（`http_transport.go` 的行级解析与
  `notifications.go` 的完整帧解析），与 `provider/local` 的 `SSEData` 是同一协议的
  第三份实现。计划 v0.20.1 抽 `internal/sse` 三处共用——`provider/openai` 本轮已经
  收敛，这是同类风险点的最后一处。
- **工具名账本有三处来源**（`tools/catalog.Static`、`tools/presets`、
  `full.All`），靠互锁测试保持一致。长期看更适合"能力注册表"式装配（每个模块自己
  声明需要什么/提供什么），但这会改动 `full.AllOptions` 的形态，留到 v0.21 评估。

### Deprecated

- The 61 legacy tool-name aliases tracked by `catalog.Legacy` (for example
  `spawn_team`/`list_teams`/`wait_team`/`close_team`, `send_message`,
  `interrupt_agent`, `list_subagent_models`, `todo`) are **deprecated as of
  v0.9.1** in favor of the modern names (`team_spawn`, `team_list`, `team_wait`,
  `team_close`, `team_send`, `team_interrupt`, `todo_write`, ...). They keep
  working for at least one minor release (v0.10.0) per the deprecation policy in
  [docs/VERSIONING.md](docs/VERSIONING.md); removal is planned for v0.10.0.

## v0.9.0 — 2026-11-06

Root-package contracts, a one-import `kit` facade, security hardening, and the
engineering shell around the SDK (CI gates, fuzzing, governance, release automation).

### Breaking

- The contract package moved from `contracts/v1` to the module root: the stable
  extension surface is now `package rig` at `github.com/zeokeel/zeokeel-rig`.
  Migrate imports (`v1 "…/contracts/v1"` → `"github.com/zeokeel/zeokeel-rig"`) and
  references (`v1.Tool` → `rig.Tool`). Versioning is module-level: after v1.0.0 a
  breaking contract change gets a new major version at the import path (`.../v2`),
  never a versioned subdirectory.
- Unused sentinels `rig.ErrDenied` and `rig.ErrNoStream` were removed; the four
  remaining sentinels are all returned by the implementation.

### Added

- `kit`: one-import facade — `kit.New(kit.Config{Model: ...})` wires an
  OpenAI-compatible provider, `full.All` tools, a safe read-only default gate and
  in-memory history; `Agent.Run`/`Agent.RunText` (aggregated text + event stream);
  contract aliases and `kit.Version`.
- `rig.Version`: the module version is queryable from code and checked against the
  release tag (`scripts/sync-version.sh`).
- `tools/internal/toolutil`: shared helper layer for tool packages (JSON args,
  results, JSON-Schema builders, UTF-8 truncation). Tool packages went from 131
  helper definitions / 864 lines to 11 shared implementations.
- Fuzz targets (9) and benchmarks (15) for the SSE, MCP, LSP, URL and path parsers
  and the registry/agent hot paths; CI runs a 5s-per-target fuzz smoke and a
  benchmark smoke.
- Governance: `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`,
  `SUPPORT.md`, `CODEOWNERS`, issue/PR templates, dependabot, `.editorconfig`,
  `.gitattributes` and a `Makefile` that mirrors CI.
- Release automation: tag-driven `.github/workflows/release.yml` verifies the tag
  against `rig.Version`, re-runs the gates, extracts the matching CHANGELOG
  section and creates the GitHub Release; `scripts/sync-version.sh` enforces the
  invariant locally and in CI.
- Docs: `docs/VERSIONING.md`, `docs/RELEASE.md`, `docs/CONFIG.md`; the README was
  rewritten around the one-import quickstart and a complete package list.

### Changed

- Security hardening (still an in-process policy; the approval `Gate` remains the
  boundary):
  - shell tools now reject unquoted shell control operators (`;`, `|`, `&&`, `$()`,
    backticks, redirects, newlines), so an allow-listed first command cannot chain
    another program. `sandbox.WithAllowShellOperators` is the documented opt-out
    that makes the allowlist advisory-only; the same check is wired into the jobs
    and pwsh tools. Terminal sessions document that the allowlist does not bound
    commands typed into an interactive shell.
  - `web_fetch` resolves and validates every dialed host itself and pins the
    validated public IP when the policy implements `web.HostResolver` (as
    `sandbox.Policy` does), closing the DNS-rebinding SSRF gap; redirects are
    covered too.
- `go.mod` declares `go 1.27` instead of a patch-pinned `1.27.3`.
- README quickstart is now `examples/quickstart` (one import) and CI diffs the
  README block against it so it cannot rot.
- `docs/ARCHITECTURE.md` documents the enforced sandbox behavior and residual
  limits; the stale `tool` package name and removed assembly references are gone.
- MCP client version now follows `rig.Version` instead of a stale `0.3.0`.
- CI: gofmt, modern-surface audit, root-package stdlib/leaf gate, coverage floor
  (80%), fuzz smoke, benchmark smoke, cross-compilation (windows/darwin/freebsd)
  and the README drift check; the job gained `permissions: contents: read`,
  concurrency and a timeout.

### Fixed

- CI: the "contracts must stay standard-library only" step compared the module's
  own import path with `grep '\.'` and failed on every run since v0.1.0; it now
  filters `go list -deps` by the `Standard` field and excludes the package itself
  (the root package after the move).
- Coverage gate: `TestToolCatalogCoverage` now also fails when `full.All` builds a
  tool missing from `catalog.Static` (nine v0.7/v0.8 tools had been outside the
  ledger; 74 static names = 74 built tools).
- `sandbox.CheckPath` no longer trims whitespace from an already-resolved path, so
  legal file names ending in a space agree with `ResolvePath` (found by fuzzing).
- `agent` documents model precedence (`RunRequest.Model` > `AgentSpec.Model`; the
  provider default applies only when calling the provider directly).
- README/docs: duplicate "(this tag)" roadmap entries, partial layout/package
  tables and the `OPENAI_BASE_URL` misinformation are fixed.

### Removed

- Local `.probe/` development scripts are untracked and ignored; the legacy audit
  script requires the old-repo path instead of defaulting to a maintainer machine.
- Dead `rig.ErrDenied` / `rig.ErrNoStream` sentinels (see Breaking).

## v0.8.0 — 2026-11-06

Audit-gap close: remote MCP templates, subagent fork and structured output.

### Added

- `tools/mcp`: `ListResourceTemplates` / `mcp_list_resource_templates`
  (resources/templates/list with cursor pagination, uriTemplate dedupe;
  server-bound like the other MCP tools).
- `agent.NewRunner`: `RunnerOptions.ParentSessionID` (+ optional ParentHistory)
  for fork; `SubagentSpec.Fork` seeds a child with a copy of the parent session
  history, `SubagentSpec.OutputSchema` gives the child a `structured_output`
  tool validated against a JSON-Schema subset and returns
  `SubagentResult.Structured`.
- `tools/subagent`: `fork` and `output_schema` parameters; new `subagent_fork`
  tool (fork forced on).
- `contracts/v1`: `SubagentSpec.Fork`, `SubagentSpec.OutputSchema`,
  `SubagentResult.Structured` (additive).
- `tools/audit` + `docs/AUDIT-REPORT.md`: three gaps closed; the remaining 6 are
  reclassified as host/kernel-bound (cordis inspection, plugin manager/market,
  workspace dependencies, PTC run_code).
- `tools/catalog` + `tools/full.All`: 74 static tools, 62 modern names, no
  catalog gaps.

## v0.7.0 — 2026-11-06

Modern-surface gap close.

### Added

- `tools/skill`: `skill` tool (list/search/load) with a filesystem `SkillStore`
  (`*/SKILL.md`, top-level `*.md`, `$ZEOKEEL_SKILLS`,
  `<root>/.zeokeel/skills`, `~/.zeokeel/skills`).
- `tools/team` (`tasks.go`): shared task board - `team_task_create`,
  `team_task_list`, `team_task_get`, `team_task_update` over
  `v1.TeamTaskManager` with pending/in_progress/completed states, owner claim,
  blocked-by dependency cycles and an in-memory default manager.
- `tools/hosttools`: `goal action=update` with complete/blocked/pause/resume/edit
  via the optional `v1.GoalUpdater`; the in-memory Host implements the state
  machine (blocked reason retained).
- `tools/review`: `code_review` and `auto_review` over `v1.Reviewer`, plus
  `SubagentReviewer` (child-agent reviewer with `VERDICT:`/`FILE:LINE` parsing).
- `tools/audit`: modern tool-surface audit (`Modern`/`DocumentedGaps`,
  `Missing`) and `docs/AUDIT-REPORT.md`.
- `contracts/v1`: `GoalUpdater`/`GoalUpdateAction`, `TeamTask`,
  `TeamTaskFilter`, `TeamTaskUpdate`, `TeamTaskManager`.
- `tools/catalog`: `Modern`, `DocumentedGaps`, `AuditModern`; the coverage test
  now checks static + legacy + modern names.

### Changed

- `tools/full.All`: new `Skill`, `TeamTasks`, `Reviewer` options; the full
  registry is now 73 static tools.
- Relicensed from Apache-2.0 to MIT (`LICENSE`, `README.md`).

## v0.6.0 — 2026-11-06

The legacy agent tool surface is complete, with a coverage gate that keeps it
complete.

### Added

- `tools/hosttools`: `ask_user`, `present`, `plan_review`, `exit_plan_mode`,
  `goal` built on the new `contracts/v1` host interfaces (Asker/Presenter/
  PlanController/GoalStore) with an in-memory default `Host`.
- `tools/settings`: `settings_get` / `settings_set` over `v1.Settings`.
- `tools/memory`: `memory` tool (add/list/search/remove/clear) with an
  in-memory `v1.MemoryStore` default.
- `tools/schedule`: `schedule_create/list/update/delete` with an in-memory
  `v1.Scheduler` default (cron evaluation stays in the host).
- `tools/worktree`: `enter_worktree` / `exit_worktree` / `list_worktrees`
  (git worktree, sandbox-checked, `git worktree list --porcelain` parsing).
- `tools/session`: `session_search`, `session_trace`, `session_event_read`,
  `session_event_search`, `session_event_trace` over `v1.SessionLog`, plus an
  in-memory log implementation.
- `tools/inspect` (`inspect`) and `tools/report` (markdown session report
  written inside the sandbox root).
- `tools/ralph`: iterate a child agent via `v1.SubagentRunner` until DONE or
  the round limit.
- `tools/team`: legacy tool names (`spawn_team`, `list_teams`, `team_status`,
  `close_team`, `wait_team`, `send_message`, `interrupt_agent`,
  `list_subagent_models`) mapped onto the existing manager.
- `tools/terminal`: `terminal_list` (sessions with transport/cwd/state).
- `tools/lsp`: legacy single `lsp` tool with action routing.
- `tools/misc`: legacy `todo` alias sharing the `todo_write` store.
- `tools/full`: `All(options)` aggregating every static tool.
- `tools/catalog`: coverage ledger (`Static`, `Legacy`, `ServerBound`,
  `Dynamic`), `Check` and `AuditLegacy`; `tools/full` runs them as
  `TestToolCatalogCoverage` so a missing legacy tool fails CI.
- `contracts/v1/extensions.go`: host interfaces and store contracts
  (Asker, Presenter, PlanController, GoalStore, MemoryStore, SkillStore,
  Scheduler, SessionLog, Reviewer + types).

### Notes

- 66 static tools, 61 legacy names, 5 server-bound (MCP resources/prompts and
  the legacy `lsp` tool), dynamic `mcp__<server>__<tool>` names.
- Zero dependencies, standard library only.

## v0.5.0 — 2026-11-06

Real terminal, remote MCP and in-process orchestration. Still zero dependencies.

### Added

- `tools/terminal`: real unix PTY transport (`/dev/ptmx` + ioctl, setsid/setctty,
  interactive shell); `terminal_send` uses a quiet period instead of a marker, ANSI
  is stripped by default, Ctrl-C is delivered as `0x03`; pipe transport remains as a
  fallback (Windows/other platforms) and keeps its previous semantics.
- `tools/mcp`: Streamable HTTP transport — POST JSON-RPC, `Mcp-Session-Id`,
  `MCP-Protocol-Version`, JSON or SSE responses, session-expiry errors, concurrency
  and cancellation; `Config{Transport,URL,Headers,HTTPClient}` and `StartHTTP`.
  The existing stdio transport is unchanged.
- `contracts/v1/subagent.go`: `SubagentSpec`, `SubagentResult`, `Subagent`,
  `SubagentRunner` (additive).
- `agent.NewRunner`: `v1.SubagentRunner` with depth (`MaxDepth`) and concurrency
  (`MaxConcurrent`) limits, tool exclusion (`ExcludedTools`, defaults include the
  orchestration tools) and per-child isolated history.
- `tools/subagent`: `subagent` tool (one child agent, blocking result).
- `tools/workflow`: `workflow` tool — sequential/parallel subagent steps with
  `{{prev}}` / `{{steps.<name>}}` templating.
- `tools/team`: managed subagents — `team_spawn/team_list/team_send/team_wait/
  team_last/team_close/team_interrupt` with id/name resolution, member limits and
  lifecycle state machine.

### Changed

- `agent.DefaultExcludedTools` now lists the shipped orchestration tool names
  (`subagent`, `workflow`, `team*`) so child agents cannot recurse by default.

## v0.4.0 — 2026-11-06

Complete agent tool surface, still zero dependencies.

### Added

- `tools/editor`: `str_replace_editor` — view/create/str_replace/insert with line
  numbers, sandbox-root checks, read-only enforcement and output caps.
- `tools/image`: `read_image` — magic-byte MIME detection (PNG/JPEG/GIF/WEBP/BMP)
  returning the image in the new `contracts/v1.ToolResult.Parts` (the agent delivers
  attachments as a follow-up user message).
- `tools/terminal`: stateful shell sessions — `terminal_open/send/read/signal/close`
  over a persistent pipe-based shell (marker-terminated commands; not a PTY yet).
- `tools/jobs`: background processes — `job_start/output/kill/list/wait` with ring
  buffered output, exit codes, signals and cleanup.
- `tools/mcp`: resources/prompts support — `ListResources/ReadResource/ListPrompts/
  GetPrompt` and the `mcp_list_resources` / `mcp_read_resource` / `mcp_list_prompts` /
  `mcp_get_prompt` tools.
- `tools/builtin`: `pwsh` tool (pwsh/powershell detection, `-NoProfile
  -NonInteractive -Command`, policy-aware).
- `contracts/v1.ToolResult.Parts` for multimodal tool attachments (additive).

### Changed

- Repository layout: tool packages moved from `tool/` to `tools/` (package `tools`),
  with implementations in `tools/{builtin,editor,image,terminal,jobs,web,mcp,lsp,misc}`.
- `tools.Registry.Unregister(name)` for dynamically composed tool sets.

## v0.3.0 — 2026-11-06

Complete agent tool surface (still zero dependencies).

### Added

- `sandbox`: policy layer for agent tools — allowed roots (symlink-safe `os.Root`
  resolution), read-only mode, command allow/deny lists, environment allowlist,
  network switch with private-address URL guard, output/timeout caps, denied globs.
- `tools/builtin`: `NewWithPolicy(*sandbox.Policy)` runs the full built-in toolset under
  the policy (existing `NewFS`/`NewShell`/`NewBuiltins` keep working).
- `tools/web`: `web_fetch` (HTTP(S), redirect/size caps, HTML→text) and `web_search`
  (pluggable `Searcher`, no built-in vendor).
- `tools/mcp`: standard-library MCP client over stdio JSON-RPC 2.0 — initialize
  handshake, paginated tools/list, tools/call (text/image/audio/resource), ping,
  concurrent calls, cancellation, graceful close — plus `Tools()` adapting MCP tools
  into Rig tools (`mcp__<server>__<tool>`).
- `tools/lsp`: standard-library LSP client (Content-Length framing, initialize,
  hover/definition/documentSymbols/diagnostics) and `lsp_*` tools.
- `tools/misc`: `working_directory` (root-checked cwd) and in-memory `todo_write`.
- `docs/TOOLS.md`: full old-agent tool catalog mapped to Rig / product / roadmap.

## v0.2.0 — 2026-11-06

**Breaking: Rig is now a pure toolset with zero dependencies.**

### Removed (moved back to the ZeoKeel product repo)

- `assembly`, `host`, `kernelx`, `keys`, `modules/appinfo` — compile-time module
  assembly, preflight/rollback, headless host and the appinfo module now live in the
  product that uses the kernel; Rig no longer depends on `zeokeel-kernel`.
- `examples/minimal`, `examples/custom-module` (module-assembly examples).

### Added

- `examples/agent` — provider + built-in tools + gate + history + streaming loop.
- `examples/custom-tool` — a third-party `Tool` implementation, registry and invoke.

### Changed

- `go.mod` has no `require` entries; `go list -m all` shows only this module.

## v0.1.0 — 2026-11-06

First public tag of the headless agent SDK.

### Added

- `contracts/v1`: messages/content parts, tool calls, streaming chunks, `Provider`,
  `Tool`, `Gate`, `History`, agent events, `Agent`/`AgentSpec`, sentinel errors.
  Standard library only; the API compatibility generation.
- `agent`: eino-free streaming ReAct loop — model rounds, parallel tool calls,
  approval gate, iteration cap, cancellation, synchronous event stream, history
  persistence.
- `provider/openai`: OpenAI-compatible streaming provider — SSE parsing, tool-call
  deltas, reasoning content, usage, 429/5xx retries, context cancellation.
- `tools` + `tools/builtin`: registry and seven built-in tools (`read_file`,
  `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`) with root sandbox,
  output caps and timeouts.
- `gate`: `AllowAll`, `DenyAll`, `AllowTools`, `Func`.
- `session`: in-memory `History`.
- `assembly` + `modules/appinfo`: compile-time registry, embedded headless/minimal
  assemblies, kernel preflight/rollback, build-identity module.
- `host`: headless runner with flags, settings file, logger, signals and graceful
  shutdown.
- `kernelx` (public): kernel descriptors/service keys/registry/effect helpers for module authors;
  `keys`: shared service keys (`AppInfoKey`, `SettingsKey`, `LoggerKey`).
- `examples/minimal`, `examples/custom-module`.
