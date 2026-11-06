# ZeoKeel Rig 现代工具面缺口审计（v0.8 · Round 3）

> **快照说明（v0.9.1）**：本报告是 v0.8 工具面审计的历史快照，计数与"same
> semantics"措辞以当时为准。v0.9.1 起：`ask_user` 为部分覆盖（缺少多问题/多选/
> 结构化回答），`present` 与 DSH 的"声明最终交付文件 + structured output"语义
> 不同；完整审计仍以 `tools/audit` 计数门禁为准（reference=63 mapped=57 gaps=6
> modern=53）。

> 审计角色：第三方视角。目标不是"再补几个工具"，而是把 **现代 agent 工具面 vs Rig 工具面** 的差距固化成
> 可重复运行的审计（`tools/audit/`），让"工具覆盖 100%"从说法变成 CI 门禁，并把剩下的每个缺口写清
> 归属（host / kernel）与替代。

## 0. TL;DR（Round 3）

- 对照对象：本机安装的 **DeepSeek Harness（`@deepseek-ai/dsh 0.2.1-rc.2`）** 的全部模型可见内置工具。
- 参考工具 **63 个**：v0.8 之后 **57 个已覆盖**，仅剩 **6 个 host/kernel-bound 缺口**（不再是工具面缺口）。
- Rig 需要的静态现代工具名（`Modern`）：**53 个**（v0.8 新增 `subagent_fork`），由 `tools/full.All` 全部产出，`Missing() == []`。
- Rig 当前静态工具：**74 个** = 53 个现代面必备 + 21 个兼容/超集工具；另有 **10 个 server-bound**（MCP/LSP）工具在配置外部服务后才存在。
- 本轮闭合 3 个缺口（全部由 v0.8 并行任务实现、审计侧只做映射）：
  1. `list_mcp_resource_templates` → **`mcp_list_resource_templates`**（server-bound，MCP `resources/templates/list`）
  2. `structured_output` → **`subagent` 的 OutputSchema 能力**（子代理上下文工具，不新增静态名）
  3. `subagent_fork` → **`subagent_fork`** 工具（`SubagentSpec.Fork` + `RunnerOptions.ParentSessionID` 真 fork，不再是 false-fork 近似）
- 剩余 6 项重新分类为 **kernel-bound（2）/ host-bound（4）**：`cordis_inspect_list`、`cordis_inspect_query`、
  `load_workspace_dependencies`、`plugin_manager`、`plugin_market`、`run_code`。它们不是"Rig 忘了做"，而是
  属于拥有模块运行时的产品（kernel）或进程宿主（host），SDK 不应越界实现。
- 本轮**仍不新增任何别名文件**：闭合项都通过新能力/新工具落位，不以同义名充数（§7）。

## 1. 审计对象与方法

| 项 | 值 |
|---|---|
| 参考实现 | `@deepseek-ai/dsh 0.2.1-rc.2`（`/usr/local/lib/node_modules/@deepseek-ai/dsh`，含 305 个 `@deepseek-ai/*` 包） |
| Rig 版本 | `github.com/zeokeel/zeokeel-rig`（本地 `v0.7.0-1-gcf61ab0` + v0.8 并行任务：MCP resource templates / Runner fork / structured output / `subagent_fork` 工具） |
| 参考工具提取 | 扫描全部工具包的 `defineTool({ name: ... })`、`ctx.tools.register(...)`、`toolName` 配置覆盖与常量定义（见附录 A） |
| Rig 侧真值 | `tools/full.All(full.AllOptions{})`（产品静态工具的唯一聚合点），由 `tools/audit/modern_test.go` 断言 |
| 门禁命令 | `go test ./tools/audit/... -count=1 -v` |
| 审计表 | `tools/audit/audit.go`：`Reference`（63）、`Mappings`（57）、`DocumentedGaps`（6，带 `Category`）、`Modern`（53）、`ServerBound`（10）、`Dynamic`、`ProductRequired`（2）、`Missing()`、`Check()` |

映射规则（Round 3 不变）：

1. **同名 / 语义等价** → 直接映射（`bash` → `bash`，`session_search` → `session_search`）。
2. **一个 Rig 工具覆盖多个参考工具** → 多条映射指向同一 Rig 名（`create_goal` / `get_goal` / `update_goal` → `goal` 的 set/get/update action；`structured_output` → `subagent` 的 OutputSchema 能力）。
3. **参考工具名与 Rig 名不同但语义覆盖** → 映射并写 `Note`（`read`/`write`/`edit` → `read_file`/`write_file`/`edit_file`；`read_mcp_resource` → `mcp_read_resource`）。
4. **需要外部服务才有意义** → 归入 `ServerBound`，不作为静态门禁（MCP、legacy LSP）。
5. **SDK 边界之外的宿主能力** → `DocumentedGaps` + `Category`（`kernel-bound` / `host-bound`），必须给出原因与替代。

## 2. 结论摘要

| 指标 | v0.7（Round 2） | v0.8（Round 3） | 变化 |
|---|---|---|---|
| DSH 参考工具（`Reference`） | 63 | 63 | — |
| 已映射（`Mappings`） | 54 | **57** | +3 |
| 有意缺口（`DocumentedGaps`） | 9 | **6** | −3（全部为 host/kernel-bound） |
| kernel-bound / host-bound | — | **2 / 4** | 新分类 |
| Rig 现代面必备静态工具（`Modern`） | 52 | **53** | +1（`subagent_fork`） |
| Rig 实际静态工具（`tools/full.All`） | 73 | **74** | +1 |
| 缺失（`Missing`） | 0 | **0** | — |
| server-bound 工具（MCP/LSP，配置后可用） | 9 | **10** | +1（`mcp_list_resource_templates`） |
| 动态工具 | `mcp__<server>__<tool>` | 同左 | — |

审计测试输出（v0.8 合并后本机实测）：

```
modern tool surface covered: reference=63 mapped=57 gaps=6 modern=53 missing=0 (static tools=74)
audit tables: reference=63 mapped=57 gaps=6 modern=53 server-bound=10
gap categories: kernel-bound=2 host-bound=4
```

## 3. 全量映射表（63 个参考工具，Round 3）

| # | DSH 内置工具 | 来源包 | Rig 映射 | 状态 | 备注 |
|---|---|---|---|---|---|
| 1 | `ask_user_question` | `dsh-tool-ask-user` | `ask_user` | ✅ 覆盖 | same semantics; Rig returns a readable error when no interactive UI is connected |
| 2 | `bash` | `dsh-tool-bash` | `bash` | ✅ 覆盖 | sandbox.Policy command allowlist + approval gate |
| 3 | `cordis_inspect_list` | `dsh-tool-cordis` | — | ⚠️ kernel-bound | Rig is a pure toolset SDK: it contains no module runtime or Cordis plugin tree, so there is no generic host registry to inspect. Module assembly and dependency injection belong to the product (ZeoKeel uses zeokeel-kernel). |
| 4 | `cordis_inspect_query` | `dsh-tool-cordis` | — | ⚠️ kernel-bound | Same as cordis_inspect_list: no kernel/plugin registry exists inside the SDK to query. |
| 5 | `create_goal` | `dsh-tool-goal` | `goal` | ✅ 覆盖 | goal action=set |
| 6 | `create_worktree` | `dsh-experimental-tool-worktree` | `enter_worktree` | ✅ 覆盖 | Rig additionally exposes exit_worktree and list_worktrees (superset of the DSH model-facing worktree surface) |
| 7 | `edit` | `dsh-tool-fs` | `edit_file` | ✅ 覆盖 | Rig also exposes str_replace_editor for structured edits |
| 8 | `exit_plan_mode` | `dsh-plan-mode` | `exit_plan_mode` | ✅ 覆盖 | Rig's plan_review submits the plan under review; exit_plan_mode applies approve/cancel/status |
| 9 | `get_goal` | `dsh-tool-goal` | `goal` | ✅ 覆盖 | goal action=get |
| 10 | `glob` | `dsh-tool-fs-search` | `glob` | ✅ 覆盖 | — |
| 11 | `grep` | `dsh-tool-fs-search` | `grep` | ✅ 覆盖 | — |
| 12 | `interrupt_agent` | `dsh-tool-subagent-control` | `interrupt_agent` | ✅ 覆盖 | legacy team alias for team_interrupt |
| 13 | `job_kill` | `dsh-tool-jobs` | `job_kill` | ✅ 覆盖 | — |
| 14 | `job_list` | `dsh-tool-jobs` | `job_list` | ✅ 覆盖 | — |
| 15 | `job_output` | `dsh-tool-jobs` | `job_output` | ✅ 覆盖 | — |
| 16 | `list_agents` | `dsh-tool-subagent-control` | `list_teams` | ✅ 覆盖 | also team_list; legacy list_teams stays registered |
| 17 | `list_mcp_resource_templates` | `dsh-mcp-resources` | `mcp_list_resource_templates` | ✅ 覆盖 | v0.8 server-bound: present only when an MCP client is configured, so excluded from the static Modern gate |
| 18 | `list_mcp_resources` | `dsh-mcp-resources` | `mcp_list_resources` | ✅ 覆盖 | server-bound: present only when an MCP client is configured, so excluded from the static Modern gate |
| 19 | `list_subagent_models` | `dsh-tool-subagent` | `list_subagent_models` | ✅ 覆盖 | legacy-name tool returning the configured model routes |
| 20 | `load_workspace_dependencies` | `dsh-tool-workspace-dependencies` | — | ⚠️ host-bound | Loading workspace dependency instructions is host/product boot policy (which files, which package manager, which trust boundary). Rig deliberately keeps boot policy out of the toolset. |
| 21 | `plugin_manager` | `dsh-plugin-manager` | — | ⚠️ host-bound | DSH's runtime plugin manager is part of its host runtime. Rig composes tools at compile time; there is no runtime plugin installer/loader to manage. |
| 22 | `plugin_market` | `dsh-plugin-market` | — | ⚠️ host-bound | A plugin marketplace is a product/UI feature backed by the runtime plugin manager; the SDK has neither. |
| 23 | `present` | `dsh-tool-present` | `present` | ✅ 覆盖 | — |
| 24 | `pwsh` | `dsh-tool-pwsh` | `pwsh` | ✅ 覆盖 | — |
| 25 | `ralph` | `dsh-tool-ralph` | `ralph` | ✅ 覆盖 | — |
| 26 | `read` | `dsh-tool-fs` | `read_file` | ✅ 覆盖 | — |
| 27 | `read_image` | `dsh-tool-fs` | `read_image` | ✅ 覆盖 | multimodal rig.ToolResult.Parts |
| 28 | `read_mcp_resource` | `dsh-mcp-resources` | `mcp_read_resource` | ✅ 覆盖 | server-bound, like mcp_list_resources |
| 29 | `run_code` | `dsh-ptc-runtime-node (PTC preset)` | — | ⚠️ host-bound | DSH's programmatic tool-calling (PTC) preset exposes a run_code tool that executes model-authored TypeScript against the tool API in an isolated runtime. Rig has no code-execution runtime and deliberately stays a plain Go toolset; adding one would be a new sandbox/runtime design decision, not a tool alias. |
| 30 | `schedule_create` | `dsh-tool-schedule` | `schedule_create` | ✅ 覆盖 | — |
| 31 | `schedule_delete` | `dsh-tool-schedule` | `schedule_delete` | ✅ 覆盖 | — |
| 32 | `schedule_list` | `dsh-tool-schedule` | `schedule_list` | ✅ 覆盖 | — |
| 33 | `schedule_update` | `dsh-tool-schedule` | `schedule_update` | ✅ 覆盖 | — |
| 34 | `send_message` | `dsh-tool-subagent-control` | `send_message` | ✅ 覆盖 | legacy team alias for team_send (target = id|name) |
| 35 | `session_event_read` | `dsh-tool-session-query` | `session_event_read` | ✅ 覆盖 | — |
| 36 | `session_event_search` | `dsh-tool-session-query` | `session_event_search` | ✅ 覆盖 | — |
| 37 | `session_event_trace` | `dsh-tool-session-query` | `session_event_trace` | ✅ 覆盖 | — |
| 38 | `session_search` | `dsh-tool-session-query` | `session_search` | ✅ 覆盖 | — |
| 39 | `session_trace` | `dsh-tool-session-query` | `session_trace` | ✅ 覆盖 | — |
| 40 | `skill` | `dsh-tool-skill` | `skill` | ✅ 覆盖 | — |
| 41 | `spawn_teammate` | `dsh-experimental-tool-agent-team` | `spawn_team` | ✅ 覆盖 | also team_spawn; legacy spawn_team stays registered |
| 42 | `str_replace_editor` | `dsh-tool-str-replace-editor` | `str_replace_editor` | ✅ 覆盖 | — |
| 43 | `structured_output` | `dsh-subagent (structured children)` | `subagent` | ✅ 覆盖 | v0.8 child-context tool: agent.NewRunner exposes structured_output while a SubagentSpec.OutputSchema is set and returns the captured object in SubagentResult.Structured; it is not a standalone static tool, so it does not add its own Modern name |
| 44 | `subagent` | `dsh-tool-subagent` | `subagent` | ✅ 覆盖 | fresh child context; DSH `subagent` (spawn provider) semantics |
| 45 | `subagent_fork` | `dsh-tool-subagent (dsh-base toolName override)` | `subagent_fork` | ✅ 覆盖 | v0.8 fork semantics: SubagentSpec.Fork seeds the child with a copy of the parent session's completed history (runner configured with RunnerOptions.ParentSessionID) |
| 46 | `team_task_create` | `dsh-experimental-tool-agent-team` | `team_task_create` | ✅ 覆盖 | — |
| 47 | `team_task_get` | `dsh-experimental-tool-agent-team` | `team_task_get` | ✅ 覆盖 | — |
| 48 | `team_task_list` | `dsh-experimental-tool-agent-team` | `team_task_list` | ✅ 覆盖 | — |
| 49 | `team_task_update` | `dsh-experimental-tool-agent-team` | `team_task_update` | ✅ 覆盖 | — |
| 50 | `terminal_close` | `dsh-tool-terminal` | `terminal_close` | ✅ 覆盖 | — |
| 51 | `terminal_list` | `dsh-tool-terminal` | `terminal_list` | ✅ 覆盖 | — |
| 52 | `terminal_open` | `dsh-tool-terminal` | `terminal_open` | ✅ 覆盖 | — |
| 53 | `terminal_read` | `dsh-tool-terminal` | `terminal_read` | ✅ 覆盖 | — |
| 54 | `terminal_send` | `dsh-tool-terminal` | `terminal_send` | ✅ 覆盖 | — |
| 55 | `terminal_signal` | `dsh-tool-terminal` | `terminal_signal` | ✅ 覆盖 | — |
| 56 | `todo_write` | `dsh-tool-todo` | `todo_write` | ✅ 覆盖 | legacy todo alias registered too |
| 57 | `update_goal` | `dsh-tool-goal` | `goal` | ✅ 覆盖 | goal action=update (complete/blocked/pause/resume/edit) when the GoalStore implements rig.GoalUpdater |
| 58 | `wait_agent` | `dsh-experimental-tool-agent-team` | `wait_team` | ✅ 覆盖 | also team_wait; legacy wait_team stays registered |
| 59 | `web_fetch` | `dsh-tool-web` | `web_fetch` | ✅ 覆盖 | — |
| 60 | `web_search` | `dsh-tool-web` | `web_search` | ✅ 覆盖 | — |
| 61 | `workflow` | `dsh-tool-workflow` | `workflow` | ✅ 覆盖 | — |
| 62 | `working_directory` | `dsh-tool-working-directory` | `working_directory` | ✅ 覆盖 | — |
| 63 | `write` | `dsh-tool-fs` | `write_file` | ✅ 覆盖 | — |

## 4. DocumentedGaps（6 项，全部 host/kernel-bound）

Round 3 把剩余缺口从"工具面缺口"重新分类为**宿主归属问题**，`Gap.Category` 固化为机器可读字段：

- `kernel-bound`：需要"拥有模块运行时的产品"（ZeoKeel kernel / 装配层）才能提供。
- `host-bound`：属于进程/产品宿主（启动策略、运行时插件管理、代码执行运行时）。

| # | DSH 工具 | 归属 | 为什么 SDK 不做 | 替代 |
|---|---|---|---|---|
| 1 | `cordis_inspect_list` | `kernel-bound` | Rig 是纯工具集：库里没有模块运行时/Cordis 插件树，没有通用 host 注册表可枚举。装配与 DI 属于产品层（ZeoKeel 用 zeokeel-kernel）。 | `inspect`（sessions/stats/workspace）覆盖 agent 可见状态；拥有模块运行时的产品自建 host 检查工具。 |
| 2 | `cordis_inspect_query` | `kernel-bound` | 同上：SDK 内没有 kernel/plugin registry 可查询。 | `inspect` + 产品侧 host 工具。 |
| 3 | `load_workspace_dependencies` | `host-bound` | 工作区依赖引导是 host/产品启动策略（哪些文件、哪个包管理器、信任边界）；Rig 刻意不把启动策略塞进工具集。 | `read_file` 依赖文件 + `bash` 安装；产品可封装成任务专用工具。 |
| 4 | `plugin_manager` | `host-bound` | DSH 的运行时插件管理属于其 host runtime；Rig 是编译期组装，没有运行期插件安装器/loader 可管理。 | 编进产品的 Go 模块（`tools/full` 是聚合点）或 MCP server 提供进程外能力。 |
| 5 | `plugin_market` | `host-bound` | 插件市场是产品/UI 功能，依赖运行时插件管理器；SDK 两者都没有。 | 产品侧功能；外部能力清单可用 `web_fetch` 读取，可运行能力走 MCP 或编译期模块。 |
| 6 | `run_code` | `host-bound` | DSH 的 PTC preset 暴露 `run_code`：在隔离运行时执行模型编写的 TypeScript 并调用工具。Rig 没有代码执行运行时，也刻意保持纯 Go 工具集；新增它等于引入新的 sandbox/runtime 设计。 | `workflow`（多步编排 + `{{prev}}`/`{{steps.name}}` 模板）与 `bash`/`terminal` 脚本；需要 PTC 的产品为自有可信运行时加 `run_code`。 |

## 5. Rig 超集：21 个不属于现代面映射的静态工具

Rig 静态工具 74 个 > Modern 53 个，多出的 21 个是**兼容旧 agent 的工具名**与**Rig 主动扩展**，不是缺口：

- `close_team`
- `exit_worktree`
- `inspect`
- `job_start`
- `job_wait`
- `list_dir`
- `list_worktrees`
- `memory`
- `plan_review`
- `report`
- `settings_get`
- `settings_set`
- `team_close`
- `team_interrupt`
- `team_last`
- `team_list`
- `team_send`
- `team_spawn`
- `team_status`
- `team_wait`
- `todo`

分组说明：

- **旧名兼容（61 个 legacy 名门禁，catalog 保障）**：`spawn_team`/`list_teams`/`team_status`/`close_team`/`wait_team`、`send_message`/`interrupt_agent`/`list_subagent_models`、`todo`、`plan_review`、`inspect`/`report`、`memory`/`settings_get`/`settings_set` 等，保留给既有 prompt/集成使用。
- **Rig 扩展**：`job_start`/`job_wait`（DSH 只有 job_output、job_kill、job_list）、`exit_worktree`/`list_worktrees`（本机 DSH 0.2.1-rc.2 的模型面只有 `create_worktree`，见 §8）、`exit_plan_mode` 的配套 `plan_review`。
- **团队工具双入口**：`team_*`（新语义）与 `team_list` 等 legacy 名并存；审计只要求覆盖 DSH 语义的其中一套。

## 6. 已闭合的 v0.8 缺口（v0.7 的 3 个缺口如何关闭）

| v0.7 缺口 | v0.8 闭合方式 | 审计处理 |
|---|---|---|
| `list_mcp_resource_templates` | `tools/mcp` 新增 `Client.ListResourceTemplates(ctx)`（分页 cursor、按 uriTemplate 去重）+ `mcp_list_resource_templates` 工具，仍 server-bound | 加入 `Mappings`；加入 `ServerBound`（不进入静态 `Modern`），映射条数 +1 |
| `structured_output` | 冻结的 v1 契约增加 `SubagentSpec.OutputSchema` / `SubagentResult.Structured`；`agent.NewRunner` 在子代理上下文追加 `structured_output` 工具并按 schema 校验，捕获结构化结果（含文本 JSON 回退） | 映射到 `subagent` 能力并注明"子代理上下文工具，无独立静态名"，不新增 `Modern` |
| `subagent_fork` | 冻结的 v1 契约增加 `SubagentSpec.Fork`；`RunnerOptions.ParentSessionID`/`ParentHistory`；`tools/subagent` 新增 `subagent_fork` 工具（`fork` 参数/独立工具），子代理以父会话已完成 history 的副本为种子 | 映射到新静态工具 `subagent_fork`，`Modern` +1 |

> 结论：v0.8 之后**不再有"工具面"缺口**；剩 6 项是 SDK 边界外的宿主能力，属于产品/kernel 任务。

## 7. 小缺口实现决策（为什么仍不新增别名文件）

| 候选 / 原则 | 决策 | 理由 |
|---|---|---|
| `subagent_fork` | v0.8 由真实能力闭合（`SubagentSpec.Fork`），**不是**别名 | v0.7 的判断保留：无会话继承通道时宁可不做；v0.8 契约与 Runner 到位后才加真工具。 |
| `structured_output` | 作为 `subagent` 上下文工具实现（`output_schema` 参数），不单列静态工具 | 该工具只存在于"带 output schema 的子代理"上下文，独立静态名没有意义；审计映射到 `subagent` 能力。 |
| `create_goal` / `get_goal` / `update_goal` 别名 | 不做 | 单个 `goal` 工具的 set/get/update action 已完整表达三种语义；别名只增加工具面噪声。 |
| `list_agents` / `wait_agent` / `spawn_teammate` 别名 | 不做 | `list_teams`/`team_list`、`wait_team`/`team_wait`、`spawn_team`/`team_spawn` 已覆盖；再加 DSH 名会形成第三套同义名。 |
| `read`/`write`/`edit` 别名 | 不做 | `read_file`/`write_file`/`edit_file` + `str_replace_editor` 已覆盖；通用短名与 `bash` 内命令混淆且无收益。 |
| `run_code` 别名 | 不做 | 需要运行时，不是别名问题（§4）。 |
| 通用原则 | **先补能力，再谈命名** | v0.7→v0.8 的 3 个闭合项全部走"契约/Runner 能力 + 真实工具"，审计不靠别名粉饰覆盖。 |

> 结论：本轮审计**没有实现型缺口**。闭合项由并行任务实现，审计侧只更新映射与文档，不改既有工具语义。

## 8. 值得注意的发现（Round 3）

1. **v0.8 是"能力闭合"而非"命名对齐"**：3 个缺口分别落在 MCP 客户端、agent Runner、subagent 工具三个层，
   审计的 `Mappings`/`Modern`/`ServerBound` 三张表分别吸收，说明审计粒度是"能力"而不是"名字数"。
2. **server-bound 边界保持干净**：`mcp_list_resource_templates` 只在配置 MCP client 后存在，因此进 `ServerBound`
   而不是静态 `Modern`；否则审计会在无外部服务的构建上误报。
3. **子代理上下文工具是审计的新类别**：`structured_output` 没有独立静态名，但确实是参考面能力；审计用
   "映射到 `subagent` 能力 + Note"表达，而不是硬塞进 `Modern` 或塞进缺口。
4. **host/kernel-bound 分类让缺口可答辩**：6 项全部给出 `Category`、原因、替代；`kernel-bound` 2 项归产品
   （zeokeel-kernel 装配层），`host-bound` 4 项归进程/产品。审计测试锁定 2/4 分布，防止未来"缺口挪来挪去"。
5. **参考清单修正（v0.7 发现，仍有效）**：本机 DSH 实际模型可见内置工具 63 个（含 `exit_plan_mode`、`structured_output`、
   `run_code`、`plugin_manager`/`plugin_market`、`cordis_inspect_*`、`load_workspace_dependencies` 等任务书初始清单未列全的项）；
   审计表以实测为准。
6. **catalog 与 audit 分工**：`tools/catalog` 是旧名/静态聚合门禁，`tools/audit` 是现代面映射门禁；v0.8 集成时
   `catalog.Modern`/`DocumentedGaps` 与 `docs/COVERAGE.md` 需同步到本报告口径（本报告与 `tools/audit` 是审计真值）。

## 9. 复现与门禁

```sh
cd /path/to/zeokeel-rig
export PATH=/usr/local/go/bin:$PATH GOPROXY=off GOSUMDB=off GOFLAGS=-mod=mod

# 1) 审计门禁（唯一需要的命令）
go test ./tools/audit/... -count=1 -v
# 期望：PASS，日志含
#   modern tool surface covered: reference=63 mapped=57 gaps=6 modern=53 missing=0 (static tools=74)
#   audit tables: reference=63 mapped=57 gaps=6 modern=53 server-bound=10
#   TestDocumentedGapsAreIntentional（6 项、kernel-bound=2/host-bound=4）
#   TestV08AuditCounts（Reference=63/Mappings=57/Gaps=6/Modern=53/ServerBound=10）

# 2) 全仓回归
go build ./... && go vet ./... && go test ./... -count=1
gofmt -l tools/audit   # 无输出
```

变更流程（保持"可重复"）：

1. DSH 升级 / 新增参考工具 → 更新 `Reference`（附录 A 脚本重新提取）。
2. 接一个新 Rig 工具或能力 → 更新 `Mappings`/`Modern`/`ServerBound`/`ProductRequired`，并同步 `TestV08AuditCounts` 与报告数字。
3. 决定不做某能力 → 归入 `DocumentedGaps` 并选择 `Category`（kernel-bound / host-bound），写 reason + alternative；缺口集合由测试锁定。
4. `go test ./tools/audit/...` 红 = 工具面回退或接线漏了，禁止绕过。

## 10. 下一轮建议（Lead review）

| 优先级 | 建议 | 成本 | 收益 |
|---|---|---|---|
| P0 | 把 `tools/audit` 加入 CI/发布门禁（与 catalog 并列）；`docs/TOOLS.md`/`docs/COVERAGE.md` 的现代面口径统一到本报告 | 0.5 人日 | 让 100% 覆盖可持续 |
| P1 | ~~MCP resource templates~~ v0.8 已闭合（`mcp_list_resource_templates`）；后续可加模板 URI 展开辅助 | — / 小 | MCP 生态完整度 |
| P2 | ~~fork / structured_output~~ v0.8 已闭合；后续观察 `SubagentSpec.Fork` 在嵌套深度与 tool filter 上的语义 | — / 小 | 多代理编排对齐 |
| P3 | 产品侧（ZeoKeel）提供 `cordis_inspect_*` 等价 host 工具；`load_workspace_dependencies` 做成产品工具（闭合 kernel-bound 2 + host-bound 1） | 产品任务 | 把宿主能力留在宿主 |
| P4 | `run_code`：除非产品明确需要 PTC，否则保持不做；要做需独立 sandbox/运行时设计 | 大 | 不做是诚实选择 |
| P5 | 别名策略保持现状：不为 DSH 同义名新增别名；第三方集成确有需求时再做 deprecated 别名 | — | 控制工具面噪声 |
| P6 | 每次 DSH 升级跑附录 A 脚本重提 `Reference`，审计差异作为 release checklist 一步 | 0.5 人日/次 | 防参考面漂移 |

## 附录 A：DSH 工具提取脚本（可复现）

```python
# 在参考 DSH 安装目录运行：提取所有模型可见内置工具名
import glob, os, re
root = "/usr/local/lib/node_modules/@deepseek-ai/dsh/node_modules/@deepseek-ai"
found = {}
for pkg in sorted(glob.glob(root + "/dsh-*")):
    name = os.path.basename(pkg)
    if "client-ui" in name or "typert" in name or "web-app" in name:
        continue
    for js in glob.glob(pkg + "/lib/**/*.js", recursive=True):
        text = open(js, encoding="utf-8", errors="ignore").read()
        for m in re.finditer(r"defineTool\([{(]", text):
            chunk = text[m.start():m.start() + 600]
            nm = re.search(r"name:\s*['\"]([a-z][a-z0-9_]{2,40})['\"]", chunk)
            if nm:
                found.setdefault(nm.group(1), set()).add(name)
        for m in re.finditer(r"toolName:\s*(?:z\.string\(\)\.default\()?['\"]([a-z][a-z0-9_]{2,40})['\"]", text):
            found.setdefault(m.group(1), set()).add(name)
print("\n".join(sorted(found)))
```

需人工补的三类（脚本无法自动提取，已固定在 `audit.go`）：

1. 常量注册：`dsh-plan-mode` 的 `EXIT_PLAN_MODE = "exit_plan_mode"`；`dsh-subagent` 的 `STRUCTURED_OUTPUT_TOOL = "structured_output"`；`dsh-workflow-ptc` 的 `PTC_TOOL_NAME = "run_code"`。
2. 配置覆盖：`dsh-base/cordis.patch.yml` 的 `toolName: subagent_fork`（同一 `dsh-tool-subagent` 包注册的第二个实例）。
3. 动态工具：`dsh-mcp-client` 的 `mcp__<server>__<tool>`（Rig 同名方案，不静态断言）。

## 附录 B：v0.7 → v0.8 审计变更记录

| 项 | v0.7 Round 2 | v0.8 Round 3 |
|---|---|---|
| Mappings | 54 | 57（+`list_mcp_resource_templates`、+`structured_output`、+`subagent_fork`） |
| DocumentedGaps | 9（混合：工具面 + host/kernel） | 6（全部 host/kernel-bound，带 `Category`） |
| Modern | 52 | 53（+`subagent_fork`；`structured_output` 不单列） |
| ServerBound | 9 | 10（+`mcp_list_resource_templates`） |
| 静态工具 | 73 | 74 |
| 结论 | 现代面映射 100%，9 缺口待契约/宿主 | 工具面缺口 0，6 项宿主能力归位，3 项能力闭合 |
