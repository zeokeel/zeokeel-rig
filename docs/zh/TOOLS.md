# 工具目录

上一代 ZeoKeel agent 暴露的每个工具现在都在本模块中，另有一些新增。本文件是覆盖门禁的“人读”一半；机器可读账本是 [`tools/catalog`](../../tools/catalog/catalog.go)，发布检查是 `go test ./tools/full`（`TestToolCatalogCoverage`）。

## 覆盖总览

| | 数量 |
| --- | --- |
| `tools/full.All()` 构建的静态工具 | **81** |
| 跟踪的 legacy 工具名（`catalog.Legacy`） | **61** |
| 跟踪的 modern 工具名（`catalog.Modern`） | **65** |
| 已记录缺口（`catalog.DocumentedGaps`） | 0（`structured_output` 是子 agent 能力） |
| server-bound 工具（需要 MCP/LSP 客户端） | 6 |
| 动态名称模式 | `mcp__<server>__<tool>` |

平台说明：`bash`、`jobs` 与 `terminal_*` 需要 unix `/bin/sh` 和 PTY；在 Windows 上它们返回清晰的配置错误（请用 `pwsh`）。其他工具平台无关。shell/job/terminal/worktree 工具遵循策略权限模式：`read-only` 禁用它们，`workspace-write` 把 cwd/参数限制在工作区并只允许 allowlist 命令，`danger-full-access` 移除这些围栏。LSP/MCP 服务器进程由宿主配置、宿主信任。

状态图例：**SDK** = 本模块随附；**server-bound** = 随附但仅在配置了服务器/客户端后才构造；**host interface** = 随附但由产品通过 `rig` 包接口注入 UI/store 实现（memory、scheduler 与 team task board 在 `full.All` 中有内存回退；其他 host 接口是可选的，没有默认实现）。

## 文件系统 / 编辑 / 图片

| 工具 | 包 | 说明 |
| --- | --- | --- |
| `read_file`、`write_file`、`edit_file` | `tools/builtin` | sandbox 策略路径、输出上限 |
| `list_dir`、`glob`、`grep` | `tools/builtin` | 隐藏被拒绝的 glob |
| `str_replace_editor` | `tools/editor` | view/create/str_replace/insert，带行号 |
| `read_image` | `tools/image` | magic-byte MIME，图片放在 `ToolResult.Parts` |
| `skill` | `tools/skill` | list/search/load 技能文档（`SKILL.md` 目录或注入的 `rig.SkillStore`） |

## Shell / 进程

| 工具 | 包 | 说明 |
| --- | --- | --- |
| `bash` | `tools/builtin` | 策略检查命令、env allowlist、超时；DSH 的 `description`/`run_in_background`/promotion + `sandbox_permissions` 在没有 executor 时被拒绝 |
| `pwsh` | `tools/builtin` | pwsh/powershell 探测、`-NoProfile -NonInteractive -Command`；与 bash 相同的 DSH 后台/description 面 |
| `terminal_open`、`terminal_send`、`terminal_read`、`terminal_signal`、`terminal_close`、`terminal_list` | `tools/terminal` | 真实 unix PTY（pipe 回退）、ANSI 剥离、Ctrl-C；DSH 的 `sessionId`/`text`/`submit`/`offset`/`count`/`type`/`name` 别名、run_in_background 发送、SIG* 信号 |
| `job_start`、`job_output`、`job_kill`、`job_list`、`job_wait` | `tools/jobs` | 后台进程；DSH 的 `wait`/`timeout_ms`/`reason` 参数、kind/label/status、完成通知 |

## Web / MCP / LSP

| 工具 | 包 | 说明 |
| --- | --- | --- |
| `web_fetch` | `tools/web` | HTTP(S)、重定向/大小上限、HTML→文本、URL 策略 |
| `web_search` | `tools/web` | 可插拔 `Searcher`；nil 后端报告配置错误 |
| `mcp_list_resources`、`mcp_read_resource`、`mcp_list_resource_templates`、`mcp_list_prompts`、`mcp_get_prompt` | `tools/mcp` | **server-bound**（MCP 客户端） |
| `lsp`（legacy action router） | `tools/lsp` | **server-bound**（LSP 客户端） |
| `lsp_hover`、`lsp_definition`、`lsp_diagnostics`、`lsp_symbols` | `tools/lsp` | 用 LSP 客户端构造 |
| `mcp__<server>__<tool>` | `tools/mcp` | **动态**，由每个服务器的 `tools/list` 生成 |

## 编排

| 工具 | 包 | 说明 |
| --- | --- | --- |
| `subagent`、`subagent_fork` | `tools/team`（DSH 面） | 异步受管子 agent：立即返回 target id，经 `rig.NoticeSource` 投递完成通知；`subagent_fork` 继承父会话历史（`RunnerOptions.ParentSessionID`）。旧同步构造器仍在 `tools/subagent`，但 `full.All` 接的是 `tools/team` 的异步版本 |
| `structured_output` | `agent`（子上下文工具） | 注入结构化子 agent；参数按 `SubagentSpec.OutputSchema` 校验 |
| `workflow` | `tools/workflow` | Go 原生声明式编排（顺序/并行，`{{prev}}`/`{{steps.<name>}}`）；无 JavaScript 引擎 |
| `ralph` | `tools/ralph` | 迭代子 agent 直到 `DONE`/轮数上限 |
| `team_spawn`、`team_list`、`team_send`、`team_wait`、`team_last`、`team_close`、`team_interrupt` | `tools/team` | 受管 subagent |
| `spawn_teammate`、`list_agents`、`wait_agent` | `tools/team` | 新增的 DSH 名称；`send_message` 非阻塞，`wait_agent` 等待下一次 team 变化 |
| `get_goal`、`create_goal`、`update_goal` | `tools/hosttools` | DSH revision-CAS goal 工具（保留 legacy `goal`） |
| `spawn_team`、`list_teams`、`team_status`、`close_team`、`wait_team`、`send_message`、`interrupt_agent`、`list_subagent_models` | `tools/team`（`legacy.go`） | 映射到 manager 的 legacy 工具名 |
| `team_task_create`、`team_task_list`、`team_task_get`、`team_task_update` | `tools/team`（`tasks.go`） | 基于 `rig.TeamTaskManager` 的共享任务板（内存默认实现） |
| `code_review`、`auto_review` | `tools/review` | `rig.Reviewer`（SubagentReviewer 适配器），输出 verdict/findings/blocking |

## 会话 / 诊断 / 报告

| 工具 | 包 | 说明 |
| --- | --- | --- |
| `session_search`、`session_trace`、`session_event_read`、`session_event_search`、`session_event_trace` | `tools/session` | 基于 `rig.SessionLog`；含内存日志 |
| `inspect` | `tools/inspect` | sessions/events/workspace/stats |
| `report` | `tools/report` | 在 sandbox 根目录内写 Markdown 报告 |

## Host 交互（接口）

| 工具 | 包 | 接口 |
| --- | --- | --- |
| `ask_user` | `tools/hosttools` | `rig.Asker`（`Interactive=false` → “no interactive UI”） |
| `get_goal`、`create_goal`、`update_goal` | `tools/hosttools` | DSH 三工具 goal 面，经常可选的 `hosttools.GoalRevisionStore` 实现 revision CAS；`goal` 作为 legacy 扩展保留 |
| `present` | `tools/hosttools` | DSH 兼容：把已有文件声明为交付物（`rig.FilePresenter`） |
| `present_artifact` | `tools/hosttools` | 渲染 artifact（`rig.Presenter`） |
| `plan_review`、`exit_plan_mode` | `tools/hosttools` | `rig.PlanController` |
| `goal` | `tools/hosttools` | `rig.GoalStore`（+ 可选的 `rig.GoalUpdater` 支持 complete/blocked/pause/resume/edit） |
| `settings_get`、`settings_set` | `tools/settings` | `rig.Settings` |
| `run_code`（opt-in） | `tools/ptc`，经 `kit.Config.PTC` | 模型编写的 Python 3/Node.js 程序在系统运行时中执行；内层调用走 Gate |

这些工具需要宿主提供对应实现：没有实现时它们返回可读的 “host did not configure” 工具错误（memory、scheduler 与 team task board 有内存回退；host-interface、settings、session、runner、reviewer 与 searcher 工具没有）。希望独立 agent 开箱即用时，使用 `hosttools.NewMemoryHost()`、`settings.Tools(nil)`、`session.NewMemory()`、一个 `rig.SubagentRunner`、一个 `rig.Reviewer` 和一个 `web.Searcher`。产品可以在不改变工具名或 schema 的情况下换入自己的 UI/store 实现。

## 记忆 / 调度 / worktree / 杂项

| 工具 | 包 | 说明 |
| --- | --- | --- |
| `memory` | `tools/memory` | 基于 `rig.MemoryStore` 的 add/list/search/remove/clear |
| `schedule_create`、`schedule_list`、`schedule_update`、`schedule_delete` | `tools/schedule` | 基于 `rig.Scheduler` 的 CRUD；cron 评估是宿主的职责 |
| `enter_worktree`、`exit_worktree`、`list_worktrees` | `tools/worktree` | git worktree，受沙箱检查，porcelain 解析 |
| `working_directory` | `tools/misc` | 受根目录检查的 cwd |
| `todo_write`、`todo` | `tools/misc` | 共享 store；`todo` 是 legacy 名称 |

## 构建注册表

```go
policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	sandbox.WithCommandAllowlist("ls", "git", "go"))
host := hosttools.NewMemoryHost()
devices := full.All(full.AllOptions{
	Policy: policy,
	Root:   root,
	Host: hosttools.Options{ // or drive a real UI
		Asker: host, Presenter: host, FilePresenter: host, Plan: host, Goal: host,
		Interactive: true,
	},
	Runner: agentRunner, // agent.NewRunner(...)
	Models: []string{"gpt-4o-mini"},
})
reg := tools.NewRegistry(devices...)
```

MCP 与 LSP 工具通过在 options 中传 `MCPClients` / `LSP` 加入（或直接调用它们的包）。

## 能力缺口（不是缺工具）

这些是路线图事项，不是缺失工具：MCP SSE 订阅/OAuth、PTY resize、Windows ConPTY、workflow 条件/循环、跨进程 subagent、会话日志持久化（host 负责）、技能仓库。覆盖门禁只跟踪工具面。

门禁机制与新增工具清单见 [COVERAGE.md](COVERAGE.md)。
