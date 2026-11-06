# ZeoKeel Rig 现代工具面缺口审计（v0.7 · Round 2）

> 审计角色：第三方视角。目标不是"再补几个工具"，而是把 **现代 agent 工具面 vs Rig 工具面** 的差距固化成
> 可重复运行的审计（`tools/audit/`），让"工具覆盖 100%"从说法变成 CI 门禁，并把每个有意的缺口写清原因与替代。

## 0. TL;DR

- 对照对象：本机安装的 **DeepSeek Harness（`@deepseek-ai/dsh 0.2.1-rc.2`）** 的全部模型可见内置工具。
- 提取结果：**63 个参考工具**；Rig 映射覆盖 **54 个**，有意缺口 **9 个**（DocumentedGaps）。
- Rig 需要保证的静态现代工具名（`Modern`）：**52 个**，由 `tools/full.All` 全部产出，`Missing() == []`。
- Rig 当前静态工具：**73 个** = 52 个现代面必备 + 21 个兼容/超集工具；另有 9 个 server-bound（MCP/LSP）工具在配置外部服务后才存在。
- 结论：**除 9 个已文档化缺口外，现代工具面已 100% 映射**。9 个缺口都不适合用"别名"掩盖：
  它们分别需要 fork 子代理 / 结构化子代理输出 / PTC 代码执行运行时 / MCP resource templates / host 内核信息 /
  插件市场与工作区依赖引导——属于 host 或 v2 契约能力，不是工具名问题。
- 本次**未新增任何别名文件**：逐项检查后，没有"成本低且语义诚实"的别名可补（详见 §7）。

## 1. 审计对象与方法

| 项 | 值 |
|---|---|
| 参考实现 | `@deepseek-ai/dsh 0.2.1-rc.2`（`/usr/local/lib/node_modules/@deepseek-ai/dsh`，含 305 个 `@deepseek-ai/*` 包） |
| Rig 版本 | `github.com/zeokeel/zeokeel-rig`（本地 `v0.6.0-2-gc5ac050` + v0.7 并行任务：skill / team_task_* / review / goal 生命周期） |
| 参考工具提取 | 扫描全部工具包的 `defineTool({ name: ... })`、`ctx.tools.register(...)`、`toolName` 配置覆盖与常量定义（见附录 A） |
| Rig 侧真值 | `tools/full.All(full.AllOptions{})`（产品静态工具的唯一聚合点），由 `tools/audit/modern_test.go` 断言 |
| 门禁命令 | `go test ./tools/audit/... -count=1 -v` |
| 审计表 | `tools/audit/audit.go`：`Reference`（63）、`Mappings`（54）、`DocumentedGaps`（9）、`Modern`（52）、`ServerBound`、`Dynamic`、`ProductRequired`、`Missing()`、`Check()` |

映射规则：

1. **同名 / 语义等价** → 直接映射（`bash` → `bash`，`session_search` → `session_search`）。
2. **一个 Rig 工具覆盖多个参考工具** → 多条映射指向同一 Rig 名（`create_goal` / `get_goal` / `update_goal` → `goal` 的 set/get/update action）。
3. **参考工具名与 Rig 名不同但语义覆盖** → 映射并写 `Note`（`read`/`write`/`edit` → `read_file`/`write_file`/`edit_file`；`read_mcp_resource` → `mcp_read_resource`）。
4. **需要外部服务才有意义** → 归入 `ServerBound`，不作为静态门禁（MCP、legacy LSP）。
5. **纯工具集 SDK 无法提供、且应由 host/product 承担** → `DocumentedGaps`，必须给出原因与替代。

## 2. 结论摘要

| 指标 | 数值 |
|---|---|
| DSH 参考工具（`Reference`） | 63 |
| 已映射（`Mappings`） | 54 |
| 有意缺口（`DocumentedGaps`） | 9 |
| Rig 现代面必备静态工具（`Modern`） | 52 |
| Rig 实际静态工具（`tools/full.All`） | 73 |
| 缺失（`Missing`） | **0** |
| server-bound 工具（MCP/LSP，配置后可用） | 9 |
| 动态工具 | `mcp__<server>__<tool>`（与 DSH 命名一致） |

审计测试输出（本机实测）：

```
modern tool surface covered: reference=63 mapped=54 gaps=9 modern=52 missing=0 (static tools=73)
audit tables: reference=63 mapped=54 gaps=9 modern=52 server-bound=9
```

## 3. 全量映射表（63 个参考工具）

| # | DSH 内置工具 | 来源包 | Rig 映射 | 状态 | 备注 |
|---|---|---|---|---|---|
| 1 | `ask_user_question` | `dsh-tool-ask-user` | `ask_user` | ✅ 覆盖 | same semantics; Rig returns a readable error when no interactive UI is connected |
| 2 | `bash` | `dsh-tool-bash` | `bash` | ✅ 覆盖 | sandbox.Policy command allowlist + approval gate |
| 3 | `cordis_inspect_list` | `dsh-tool-cordis` | — | ⚠️ 缺口 | Rig is a pure toolset SDK: it contains no module runtime or Cordis plugin tree, so there is no generic host registry to inspect. Module assembly and dependency injection belong to the product (ZeoKeel uses zeokeel-kernel). |
| 4 | `cordis_inspect_query` | `dsh-tool-cordis` | — | ⚠️ 缺口 | Same as cordis_inspect_list: no kernel/plugin registry exists inside the SDK to query. |
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
| 17 | `list_mcp_resource_templates` | `dsh-mcp-resources` | — | ⚠️ 缺口 | Rig's MCP client lists resources and prompts and reads resources/prompts, but does not yet expose resources/templates/list or expand template URIs. |
| 18 | `list_mcp_resources` | `dsh-mcp-resources` | `mcp_list_resources` | ✅ 覆盖 | server-bound: present only when an MCP client is configured, so excluded from the static Modern gate |
| 19 | `list_subagent_models` | `dsh-tool-subagent` | `list_subagent_models` | ✅ 覆盖 | legacy-name tool returning the configured model routes |
| 20 | `load_workspace_dependencies` | `dsh-tool-workspace-dependencies` | — | ⚠️ 缺口 | Loading workspace dependency instructions is host/product boot policy (which files, which package manager, which trust boundary). Rig deliberately keeps boot policy out of the toolset. |
| 21 | `plugin_manager` | `dsh-plugin-manager` | — | ⚠️ 缺口 | DSH's runtime plugin manager is part of its host runtime. Rig composes tools at compile time; there is no runtime plugin installer/loader to manage. |
| 22 | `plugin_market` | `dsh-plugin-market` | — | ⚠️ 缺口 | A plugin marketplace is a product/UI feature backed by the runtime plugin manager; the SDK has neither. |
| 23 | `present` | `dsh-tool-present` | `present` | ✅ 覆盖 | — |
| 24 | `pwsh` | `dsh-tool-pwsh` | `pwsh` | ✅ 覆盖 | — |
| 25 | `ralph` | `dsh-tool-ralph` | `ralph` | ✅ 覆盖 | — |
| 26 | `read` | `dsh-tool-fs` | `read_file` | ✅ 覆盖 | — |
| 27 | `read_image` | `dsh-tool-fs` | `read_image` | ✅ 覆盖 | multimodal v1.ToolResult.Parts |
| 28 | `read_mcp_resource` | `dsh-mcp-resources` | `mcp_read_resource` | ✅ 覆盖 | server-bound, like mcp_list_resources |
| 29 | `run_code` | `dsh-ptc-runtime-node (PTC preset)` | — | ⚠️ 缺口 | DSH's programmatic tool-calling (PTC) preset exposes a run_code tool that executes model-authored TypeScript against the tool API in an isolated runtime. Rig has no code-execution runtime and deliberately stays a plain Go toolset; adding one would be a new sandbox/runtime design decision, not a tool alias. |
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
| 43 | `structured_output` | `dsh-subagent (structured children)` | — | ⚠️ 缺口 | The tool exists only on children started with an output schema (DSH subagent structured output): the child calls structured_output with arguments matching the requested schema and the parent receives a typed result. v1.SubagentSpec/SubagentResult are text-only in the frozen v1 contract, so the tool has no channel to return structured data. |
| 44 | `subagent` | `dsh-tool-subagent` | `subagent` | ✅ 覆盖 | fresh child context; DSH `subagent` (spawn provider) semantics |
| 45 | `subagent_fork` | `dsh-tool-subagent (dsh-base toolName override)` | — | ⚠️ 缺口 | v1.SubagentRunner has no fork/context-inheritance API: Rig's agent.NewRunner builds children with a fresh session history and a filtered tool set, so a child seeded with the parent's completed turns cannot be expressed. Approximating it with SystemPromptAppend + a fresh history would not inherit the conversation and would mislead the model. |
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
| 57 | `update_goal` | `dsh-tool-goal` | `goal` | ✅ 覆盖 | goal action=update (complete/blocked/pause/resume/edit) when the GoalStore implements v1.GoalUpdater |
| 58 | `wait_agent` | `dsh-experimental-tool-agent-team` | `wait_team` | ✅ 覆盖 | also team_wait; legacy wait_team stays registered |
| 59 | `web_fetch` | `dsh-tool-web` | `web_fetch` | ✅ 覆盖 | — |
| 60 | `web_search` | `dsh-tool-web` | `web_search` | ✅ 覆盖 | — |
| 61 | `workflow` | `dsh-tool-workflow` | `workflow` | ✅ 覆盖 | — |
| 62 | `working_directory` | `dsh-tool-working-directory` | `working_directory` | ✅ 覆盖 | — |
| 63 | `write` | `dsh-tool-fs` | `write_file` | ✅ 覆盖 | — |

## 4. DocumentedGaps（9 项，已文档化的有意缺口）

| # | DSH 工具 | 为什么 Rig 不做 | 替代方案 |
|---|---|---|---|
| 1 | `cordis_inspect_list` | Rig 是纯工具集：库里没有模块运行时/Cordis 插件树，没有通用 host 注册表可枚举。装配与 DI 属于产品层（ZeoKeel 用 zeokeel-kernel）。 | `inspect`（sessions/stats/workspace）覆盖 agent 可见状态；拥有模块运行时的产品自建 host 检查工具。 |
| 2 | `cordis_inspect_query` | 同上：SDK 内没有 kernel/plugin registry 可查询。 | `inspect` + 产品侧 host 工具。 |
| 3 | `list_mcp_resource_templates` | Rig 的 MCP client 支持 resources/list、resources/read、prompts/list、prompts/get，但尚未实现 resources/templates/list 与模板 URI 展开。 | `mcp_list_resources`（很多 server 同时暴露具体资源）+ `mcp_list_prompts`；产品可在 tools/mcp Client 之上加模板工具。 |
| 4 | `load_workspace_dependencies` | 工作区依赖引导是 host/产品启动策略（哪些文件、哪个包管理器、信任边界）；Rig 刻意不把启动策略塞进工具集。 | `read_file` 依赖文件 + `bash` 安装；产品可封装成任务专用工具。 |
| 5 | `plugin_manager` | DSH 的运行时插件管理属于其 host runtime；Rig 是编译期组装，没有运行期插件安装器/loader 可管理。 | 编进产品的 Go 模块（`tools/full` 是聚合点）或 MCP server 提供进程外能力。 |
| 6 | `plugin_market` | 插件市场是产品/UI 功能，依赖运行时插件管理器；SDK 两者都没有。 | 产品侧功能；外部能力清单可用 `web_fetch` 读取，可运行能力走 MCP 或编译期模块。 |
| 7 | `run_code` | DSH 的 PTC（programmatic tool calling）preset 暴露 `run_code`：在隔离运行时执行模型编写的 TypeScript 并调用工具。Rig 没有代码执行运行时，也刻意保持纯 Go 工具集；新增它等于引入新的 sandbox/runtime 设计，不是工具别名能补的。 | `workflow`（多步编排 + `{{prev}}`/`{{steps.name}}` 模板）与 `bash`/`terminal` 脚本；需要 PTC 的产品可为自己的可信运行时加 `run_code`。 |
| 8 | `structured_output` | 该工具只存在于"带 output schema 的子代理"上下文：子代理按 schema 调 `structured_output`，父代理收到结构化结果。冻结的 v1 `SubagentSpec`/`SubagentResult` 只有文本通道，没有返回结构化数据的通道。 | 在 prompt 中要求子代理输出 JSON 文本并解析；v2 `SubagentSpec` 可加 output schema 通道。 |
| 9 | `subagent_fork` | `v1.SubagentRunner` 没有 fork/会话继承 API：`agent.NewRunner` 用全新 history + 过滤后的工具集构造子代理，"继承父会话已完成轮次"无法表达；用 `SystemPromptAppend` + fresh history 近似并不继承会话，会误导模型。 | `subagent` + 自包含 prompt（父代理把所需上下文摘录进 prompt）。未来 v2 `SubagentRunner` fork provider 可闭合。 |

## 5. Rig 超集：21 个不属于现代面映射的静态工具

Rig 静态工具 73 个 > Modern 52 个，多出的 21 个是**兼容旧 agent 的工具名**与**Rig 主动扩展**，不是缺口：

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

## 6. Server-bound 与动态工具

`ServerBound`（配置对应客户端后才存在，因此不在静态门禁中）：

- MCP：`mcp_list_resources`、`mcp_read_resource`、`mcp_list_prompts`、`mcp_get_prompt`（覆盖 `list_mcp_resources`、`read_mcp_resource`；模板见缺口 3）。
- LSP：`lsp`（legacy 单工具）+ `lsp_hover`/`lsp_definition`/`lsp_diagnostics`/`lsp_symbols`。

动态工具（`Dynamic`）：

| 参考模式 | Rig 模式 | 说明 |
|---|---|---|
| `mcp__<server>__<tool>` | `mcp__<server>__<tool>` | DSH 为每个 MCP server tool 动态注册；Rig `tools/mcp.Tools/NewTools` 使用相同命名方案（`tools/mcp.DefaultPrefix`）。 |

## 7. 小缺口实现决策（为什么没有新增别名文件）

逐项检查后结论：**没有"成本低且语义诚实"的别名可补**，因此本任务只新增审计包与文档，没有改任何既有 tools 包。

| 候选 | 决策 | 理由 |
|---|---|---|
| `create_goal` / `get_goal` / `update_goal` 别名 | 不做 | 单个 `goal` 工具的 set/get/update action 已完整表达三种语义；DSH 拆成 3 个工具是它的 UI/审批设计选择，别名只增加工具面噪声，不增加能力。映射表已标注 action 对应关系。 |
| `list_agents` / `wait_agent` 别名 | 不做 | `list_teams`/`team_list`、`wait_team`/`team_wait`、`spawn_team`/`team_spawn` 已覆盖；再加 `list_agents` 会与项目现有命名体系（team_*/legacy）产生第三套同义名，反而增加模型选择成本。 |
| `spawn_teammate` 别名 | 不做 | 同 `list_agents`：映射到 `spawn_team`/`team_spawn`；DSH 的 team 是 experimental bundle，Rig 的 team 语义一致。 |
| `subagent_fork` 别名 | 不做 | 见缺口 9：v1 Runner 无会话继承，别名只能用 fresh context 伪装 fork，会主动误导模型；宁缺毋滥，等 v2 契约。 |
| `read`/`write`/`edit` 别名 | 不做 | Rig 已有 `read_file`/`write_file`/`edit_file` + `str_replace_editor`；通用短名会与 `bash` 内命令混淆且无收益。 |
| `run_code` / `structured_output` | 不做 | 需要新运行时/新契约通道，不是别名问题。 |

> 结论：本轮审计没有"实现型缺口"；真正的差距都在 §4，需要契约/运行时演进，已全部文档化。

## 8. 值得注意的发现

1. **参考清单修正**：本机 DSH 0.2.1-rc.2 实际模型可见内置工具是 **63** 个。除任务书给的清单外，还发现
   `exit_plan_mode`（`dsh-plan-mode`，常量注册，非字面 `defineTool name:`）、`structured_output`（`dsh-subagent` 结构化子代理）、
   `run_code`（PTC preset）、`list_mcp_resource_templates`、`plugin_manager`/`plugin_market`（DSH 可选覆盖层）、
   `cordis_inspect_list/query`、`load_workspace_dependencies`。审计表以实测为准。
2. **worktree 是 Rig 超集**：DSH 0.2.1-rc.2 的模型面只注册 `create_worktree`；`exit_worktree`/`list_worktrees` 是 Rig 的扩展
   （任务书里"create/exit/list_worktrees → enter/exit/list_worktrees"的前两项映射成立，后两项按 Rig 超集记录，不算参考工具）。
3. **goal 是 1 vs 3**：DSH 用 `create_goal`/`get_goal`/`update_goal` 三个工具；Rig 用单 `goal` 工具的 action。能力等价，审计按 action 映射。
4. **动态 MCP 命名已对齐**：这是"工具面 100%"里容易被忽略的一类；Rig 与 DSH 都是 `mcp__<server>__<tool>`，天然对齐。
5. **catalog 与 audit 分工**：`tools/catalog` 是"旧名 100%"门禁（66 静态/61 旧名），`tools/audit` 是"现代面映射"门禁；
   两者互补，新工具落地要同时过。
6. **并行任务接线状态**：审计编写期间 v0.7 并行任务（skill、team_task_*、code_review/auto_review、goal 生命周期）
   已落地并接入 `tools/full.All`（73 静态工具）。首次本地跑审计时 `Missing()` 正是这 7 个名字，接线后归零——
   audit 表就是按这个顺序工作的。

## 9. 复现与门禁

```sh
cd /data/zeokeel-rig
export PATH=/usr/local/go/bin:$PATH GOPROXY=off GOSUMDB=off GOFLAGS=-mod=mod

# 1) 审计门禁（唯一需要的命令）
go test ./tools/audit/... -count=1 -v
# 期望：PASS，日志含
#   modern tool surface covered: reference=63 mapped=54 gaps=9 modern=52 missing=0 (static tools=73)
#   audit tables: reference=63 mapped=54 gaps=9 modern=52 server-bound=9

# 2) 全仓回归
go build ./... && go vet ./... && go test ./... -count=1
gofmt -l tools/audit   # 无输出
```

变更流程（保持"可重复"）：

1. DSH 升级 / 新增参考工具 → 更新 `Reference`（附录 A 脚本重新提取）。
2. 接一个新 Rig 工具或别名 → 更新 `Mappings`/`Modern`/`ProductRequired`，跑审计；缺名字会精确列出。
3. 决定不做某能力 → 加 `DocumentedGaps`（必须写 reason + alternative），`TestDocumentedGapsAreIntentional` 会锁定缺口集合，防止静默漂移。
4. `go test ./tools/audit/...` 红 = 工具面回退或接线漏了，禁止绕过。

## 10. 下一轮建议（Lead review）

| 优先级 | 建议 | 成本 | 收益 |
|---|---|---|---|
| P0 | 把 `tools/audit` 加入 CI/发布门禁（与 catalog 并列），`docs/TOOLS.md` 增加"现代面映射"章节链接到本报告 | 0.5 人日 | 让 100% 覆盖可持续 |
| P1 | `tools/mcp` 增加 `resources/templates/list` 支持 + `mcp_list_resource_templates` 工具（server-bound，闭合缺口 3） | 1-2 人日 | MCP 生态完整度 |
| P2 | v2 `SubagentSpec{OutputSchema, Fork}` 契约 + agent Runner fork provider，随后补 `subagent_fork`/`structured_output`（闭合缺口 8、9） | 3-5 人日 | 多代理编排对齐现代面 |
| P3 | 产品侧（ZeoKeel）提供 `cordis_inspect_*` 等价 host 工具；`load_workspace_dependencies` 做成产品工具（闭合缺口 1、2、4） | 产品任务 | 把 host 能力留在 host |
| P4 | `run_code`：除非产品明确需要 PTC，否则保持不做；若要做需独立 sandbox/运行时设计（缺口 7） | 大 | 不做是诚实选择 |
| P5 | 别名策略保持现状：不为 DSH 同义名新增别名；未来若第三方集成方明确要求，再以"deprecated 别名 + 文档"方式增量添加 | — | 控制工具面噪声 |

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
