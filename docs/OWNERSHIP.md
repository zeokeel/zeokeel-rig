# 能力归属矩阵：哪一层负责什么

本文解决什么问题：**加一个能力之前，先判断它该放在 Rig 还是宿主。** 读者是 SDK
维护者与接 SDK 的宿主开发者。读完你应该能回答"这个功能是谁的事"，而不是每次升级
都靠人读源码推断。

判据只有三条，满足任意一条才该进 SDK：

1. 宿主不写就无法兑现自己**已对用户做出的承诺**（安全、隐私、成本、可恢复）；
2. 两个以上宿主要重复实现同一段逻辑，且必须与 SDK 的执行点逐字节对齐；
3. 实现点在 SDK 内部（工具执行、结果回写、prompt 拼装、存储 schema），宿主没有钩子。

三条都不满足 → 宿主负责，SDK 不接。

图例：**Rig** = SDK 已经提供（含包与入口）；**宿主** = 明确由产品实现；
**待办** = 已判定该由 Rig 提供、尚未落地。

## 1. 循环与上下文

| 能力 | 归属 | 说明 |
| --- | --- | --- |
| ReAct 循环、并行工具、取消、重复调用守卫 | Rig | `agent` |
| 回合边界的钩子（快照、事件双写、成本、结果后处理） | Rig | `rig.TurnHook` + `AgentSpec.Hooks`（四个边界） |
| 系统提示词的**拼装** | Rig | `SystemPrompt` / `SystemPromptFunc`（按运行计算）；区段化还在**待办** |
| 提示词内容（产品文案、人格、纪律） | 宿主 | SDK 不写产品语气 |
| 指令文档发现与渲染（含 @import/预算/围栏） | Rig | `instructions.Load`、`WithRepoFileNames`、`RenderWith` |
| 决定"要不要信任仓库指令" | 宿主 | 用 `RenderOptions.Trust` 表达；默认不围栏 |
| 环境块（workspace/cwd/platform/日期/git） | Rig | `instructions.DetectEnvironment`；产品特有的"文件策略"一行由宿主追加 |
| 上下文压缩算法 | Rig | `session/compact`（窗口裁剪 / LLM 摘要 + 回退） |
| "什么时候压缩、给用户看什么横幅" | 宿主 | 事件与预算由宿主决定 |
| 模型元数据（上下文窗口/价目） | **待办** | 现在由宿主填 `ModelContextTokens`；注册表尚未提供 |

## 2. 工具面

| 能力 | 归属 | 说明 |
| --- | --- | --- |
| 内置工具实现（文件/shell/web/会话/记忆/编排…） | Rig | `tools/*`，账本见 `tools/catalog` |
| 工具集的**装配**（一次装好一个 agent） | Rig | `tools/full`（`All`/`NewRuntime`）、`kit` |
| 多工作区、动态 MCP 增删、按会话裁剪的装配 | 宿主 | 现阶段自建；`kit.Runtime` 在**待办** |
| 工具子集的命名策略（预设） | Rig | `tools/presets`（只过滤名字） |
| 权限模式 → 工具面裁剪 | **待办** | 现在只有审批白名单（`gate.SandboxReadOnlyTools`） |
| 工具结果的结构化数据 | Rig | `ToolResult.Data`（只进事件，不进模型） |
| 工具结果的人读文本 | Rig | `ToolResult.Content`（模型面） |
| 大结果的落盘/回收 | Rig | `SpillPolicy` + `rig.SpillStore`（按会话作用域回收） |
| 隐藏路径（对模型不可见） | Rig | `sandbox.WithHidden` / `kit.Config.HiddenPaths` / `search.WithHiddenFunc` |
| 技能库（`SKILL.md`）的发现与加载 | Rig | `tools/skill`（默认根跟随 `AllOptions.Root`） |
| 技能内容本身 | 宿主 | SDK 只提供读取与渲染 |

## 3. 权限、审批与沙箱

| 能力 | 归属 | 说明 |
| --- | --- | --- |
| 可写根围栏、根内只读子路径 | Rig | `sandbox.Policy`（进程内，不是内核边界） |
| OS 级执行器（bwrap/Landlock/sandbox-exec/ACL） | Rig（opt-in） | `sandbox/osrunner`，默认关闭、由宿主注入 |
| 审批组合（只读免问、问人、全放行） | Rig | `gate.PersonalAsk`/`PersonalAuto`/`Ask` |
| 审批 UI、队列、通知、超时策略 | 宿主 | SDK 只给 `rig.Asker` 与 `AskEvent` 观察点 |
| "记住这次允许"的持久化与作用域 | 宿主 | `gate.AskGate` 只有进程内、按工具名的记忆 |
| 危险命令规则表 | Rig（可选） | `sandbox.ClassifyCommand` / `gate.CommandRiskGate`，**不在默认预设里** |
| 逐命令提权 | Rig | `sandbox_permissions:"require_escalated"` + `Escalator`（只覆盖 bash/pwsh） |
| 运行期可写授权（会话级 grant） | Rig | `request_sandbox_access` + `Policy.AddWritableRoot` |
| 拒绝 glob / 保护路径（历史 API） | 无 | v0.24.0 起是记录型 no-op；用 `WithHidden` 或只读子路径 |
| 网络隔离、进程可见性、设备访问 | 部署层 | 沙箱不承诺；用容器/VM |

## 4. 数据面

| 能力 | 归属 | 说明 |
| --- | --- | --- |
| 会话/消息/事件/检查点/花费/记忆/溢出的存储契约 | Rig | 根包接口 + `session/store`（NDJSON）+ `session/sqlite`（子模块） |
| 会话改名/换工作区 | Rig | `session/sqlite.UpdateMeta` + `MetaPatch` |
| 跨会话检索 | Rig | `SearchMessages`（解码列）+ `BackfillSearchText` + `Offset` |
| 花费落库与维度聚合 | Rig | `SaveCost` / `CostBySession` / `CostByWorkspace` |
| 统计口径（日/模型/工作区怎么算、UI 怎么展示） | 宿主 | SDK 只给原子聚合，不给产品口径 |
| 从"宿主自己的库"迁移 | **待办** | 现在只有 NDJSON `ImportDir` |
| 记忆的**模型**（页 vs 条目） | Rig + 宿主 | SDK 给 `memory.Wiki`；条目型语义与迁移属宿主 |
| 记忆的展示与编辑面板 | 宿主 | |
| 事件线格式（可搬运的 NDJSON） | Rig | `events`（`Envelope`/`Emitter`/`Replay`） |
| 导出、备份、审计报告 | Rig + 宿主 | `Export`/`VerifyExport` 给数据；报告形态属宿主 |

## 5. 编排与宿主交互

| 能力 | 归属 | 说明 |
| --- | --- | --- |
| 子代理/团队/工作流/ralph 的工具与调度 | Rig | `tools/{team,subagent,workflow,ralph}` |
| 团队的运行态读模型（running/idle/done） | 宿主 | SDK 只报可用性；宿主按事件折叠 |
| `ask_user`、`plan_review`、`exit_plan_mode` | Rig（工具）+ 宿主（实现） | `rig.Asker`/`PlanController` |
| `present`（声明已有文件） | Rig（工具）+ 宿主（渲染） | `rig.FilePresenter` |
| 内容产物（title+body → 卡片/文档） | 宿主 | `rig.Presenter` 已废弃；契约恢复在**待办** |
| goal 三件套（工具） | Rig | `tools/hosttools`：`goal`/`get_goal`/`create_goal`/`update_goal` |
| goal 的持久化与修订冲突 | 宿主 | `rig.GoalStore`/`GoalUpdater` 契约在 SDK |
| 定时任务（工具） | Rig | `tools/schedule` |
| 定时任务的存储与触发器 | 宿主 | `rig.Scheduler` 契约在 SDK |
| 后台任务（bash 后台、PTY） | Rig | `tools/jobs`；`Manager.Snapshot()` 给宿主视图 |
| 后台任务在 UI 里的呈现 | 宿主 | 读 `Snapshot()`，不要解析文本 |

## 6. 观测与产品形态

| 能力 | 归属 | 说明 |
| --- | --- | --- |
| 进程内事件（`rig.Event`） | Rig | 循环的观察窗口 |
| 事件落成 NDJSON（`events.Envelope`） | Rig + 宿主 | `EventWriter` 由宿主注入 |
| 成本计量（单次 Run 预算/告警） | Rig | `costmeter` |
| 价目表（内置估算） | Rig | `modelprice`（不是账单） |
| 价目覆盖/自定义 | 宿主 | 通过 `costmeter.WithPricer` 注入 |
| 凭据脱敏（运行时/导出） | 宿主 | SDK 不提供：任何一层都不改写文本（见 SECURITY.md「凭据」） |
| UI、i18n、快照、窗口生命周期 | 宿主 | SDK 不做 |
| 账号、多租户、配额、云同步 | 宿主 | 单用户个人客户端的定位 |

## 7. 怎么用这张表

- 提新需求时先问：宿主自己写会不会**重复**且**必须与 SDK 对齐**？会 → 进 SDK。
- 一个能力"SDK 有但拿不到"（只在 `kit` 的内部路径里）算 bug，不算归属问题。
- 标 **待办** 的条目是已判定该进 SDK、尚未落地；落地时同步本表与
  [ARCHITECTURE.md](ARCHITECTURE.md) 的"刻意不拥有什么"一节。

## 相关文档

- [ARCHITECTURE.md](ARCHITECTURE.md) —— 分层与请求流
- [CONFIG.md](CONFIG.md) —— `kit.Config` 逐字段默认值
- [TOOLS.md](TOOLS.md) / [COVERAGE.md](COVERAGE.md) —— 工具面与计数口径
- [SANDBOX-OS.md](SANDBOX-OS.md) —— 沙箱边界
- [sqlite.md](sqlite.md) —— 数据面实现细节