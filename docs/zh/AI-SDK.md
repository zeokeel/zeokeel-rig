# 给 AI 的 Rig SDK 使用指南

> 目标读者：直接使用或扩展 Rig 的 AI/人类工程师。
> 机器可读工具 Schema：[docs/ai/tools.json](../ai/tools.json)；人读表格：[docs/ai/tools.md](../ai/tools.md)。

## 1. 项目提供什么

ZeoKeel Rig 是**纯 Go 的 agent 工具集/SDK**：provider、ReAct 循环、工具、权限模式、审批 Gate、会话历史与编排工具。它不包含产品运行时、UI、进程宿主或模块装配。

- Go 1.27+；`CGO_ENABLED=0`；唯一依赖 `mvdan.cc/sh/v3`（仅 shell 策略解析）。
- **工具不会自动进入 agent loop**：必须显式传给 `agent.AgentSpec.Tools`，或使用 `kit.New`（默认 `tools/full.All`）。
- 当前 release **v0.13.1**：81 个静态工具 + server-bound MCP/LSP + 动态 `mcp__<server>__<tool>` + 可选 `run_code`（PTC）。

## 2. 三种组装方式

### 2.1 最快：kit

```go
a, err := kit.New(kit.Config{
    Model: "deepseek-chat", Root: root,
    Mode: kit.ModeWorkspaceWrite, Approval: kit.ApprovalAsk,
    CommandAllowlist: []string{"git", "go", "ls", "cat"},
})
```

- `Config.Tools == nil` → 默认 81 个静态工具；非 nil（包括空 slice）→ 完全按传入集合。
- 默认 gate 只放行 `read_file/list_dir/glob/grep`；写/执行需 `ApprovalAuto` 或自定义 `Gate`。
- `kit.New` 不暴露 Host/Runner/MCP/Searcher，需要这些能力的工具会返回“host 未配置”错误；完整能力用 2.2。

### 2.2 推荐：显式 `full.All` + 自定义 Tools

```go
policy := sandbox.New(
    sandbox.WithRoots(root),
    sandbox.WithMode(sandbox.ModeWorkspaceWrite),
    sandbox.WithCommandAllowlist("git", "go", "ls", "cat"),
)
host := hosttools.NewMemoryHost()
tools := full.All(full.AllOptions{
    Policy: policy, Root: root,
    Host: hosttools.Options{Asker: host, Presenter: host, FilePresenter: host, Plan: host, Goal: host, Interactive: true},
    Runner: runner, Searcher: searcher, SessionLog: session.NewMemory(),
    Settings: settings, Reviewer: reviewer,
    MCPClients: clients, LSP: lspClient, // 可选
})
a, err := agent.New(rig.AgentSpec{
    Provider: provider, Model: "deepseek-chat", Tools: tools,
    Gate: gate.AllowTools("read_file", "glob", "grep"),
})
```

### 2.3 子集或按包构造

```go
all := full.All(full.AllOptions{Policy: policy, Root: root, Runner: runner})
keep := map[string]bool{"read_file": true, "write_file": true, "bash": true, "job_output": true}
var subset []rig.Tool
for _, tool := range all { if keep[tool.Name()] { subset = append(subset, tool) } }
```

```go
bg := jobs.NewManager(jobs.WithPolicy(policy))
bash := builtin.NewWithPolicyOptions(policy, builtin.WithBackgroundJobs(bg))[6]
terms := terminal.NewManager(terminal.WithPolicy(policy), terminal.WithBackgroundJobs(bg))
```

> bash/pwsh/terminal 的后台执行与 jobs 完成通知必须共享同一个 `jobs.Manager`；`full.All` 已自动接线。手动组装需 `WithBackgroundJobs`。

### 2.4 可选 PTC

`kit.Config{PTC: true}` 追加 `run_code`：模型写 Python 3/Node.js，在**系统已安装运行时**中执行，通过 stdin/stdout 调用现有工具；每次内层调用都走同一 `rig.Gate`。不嵌入 JS 引擎。

## 3. 权限与审批

| 层 | 配置 | 作用 |
| --- | --- | --- |
| 权限模式 | `sandbox.Mode` / `kit.Config.Mode` | `read-only`（禁写/禁 shell）、`workspace-write`（默认：roots 内可写，仅 allowlist 命令）、`danger-full-access`（无 roots/allowlist fence，显式 denylist 仍生效） |
| 审批 | `kit.Config.Approval` / `rig.Gate` | `ask`（默认保守）、`auto`（全部放行）、`never`（全部拒绝）；自定义 Gate 可按工具审批 |

这些是工具实现层 fence，不是内核沙箱；不可信模型请把 host 放进容器/VM。

## 4. 工具能力摘要

完整 82 行见 [docs/ai/tools.md](../ai/tools.md)。选型：

| 类别 | 工具 | 用途 |
| --- | --- | --- |
| 文件/搜索 | `read_file`, `write_file`, `edit_file`, `str_replace_editor`, `list_dir`, `glob`, `grep`, `read_image` | 读写/定位代码、读图 |
| Shell/进程 | `bash`, `pwsh`, `job_*`, `terminal_*` | 一次性命令、后台任务、持久终端（DSH `run_in_background`/分页/SIG*） |
| Web | `web_fetch`, `web_search` | 抓取/搜索；`web_search` 支持 `queries[]`，需 `Searcher` |
| 会话/诊断 | `session_*`, `inspect`, `report` | 历史检索、统计、报告 |
| 记忆/状态 | `memory`, `todo_write`, `schedule_*`, `working_directory`, `enter/exit/list_worktrees`, `skill` | 计划、提醒、工作区、技能 |
| 编排 | `subagent`, `subagent_fork`, `spawn_teammate`, `list_agents`, `wait_agent`, `team_*`, `send_message`, `interrupt_agent`, `team_task_*`, `workflow`, `ralph` | 委派、团队、工作流；`subagent` 异步，结果用 `wait_agent`/`team_wait`/`team_last` |
| 目标/计划 | `goal`, `get_goal/create_goal/update_goal`, `plan_review`, `exit_plan_mode` | 长目标、计划审批 |
| 宿主交互 | `ask_user`, `present`, `present_artifact`, `settings_*` | 需要 host 接口 |
| 评审 | `code_review`, `auto_review` | 需要 `Reviewer` |
| MCP/LSP | `mcp__<server>__<tool>` 动态 + `mcp_*`；`lsp`/`lsp_*`（含 references/implementation） | 配置 client 后出现；HTTP 通知流由 `OnNotification` 接收 |
| PTC | `run_code` | `kit.Config.PTC` 开启 |

## 4.5 上下文、恢复与预设（长任务）

- **自动压缩**：设置 `kit.Config.ContextBudget`（估算 token）与 `Compactor`；
  常用 `session/compact.WindowCompactor`（确定、无模型）或 `LLMCompactor`
  （模型摘要，失败自动回退）。超预算时 agent 在模型调用前压缩内存对话。
- **手动压缩**：`(*kit.Agent).CompactNow(ctx, sessionID)`，需要 History 实现
  `rig.HistoryReplacer`（默认 `session.Memory` 已实现）。
- **工具结果落盘**：`kit.Config.Spill = &rig.SpillPolicy{Dir, ThresholdBytes, MaxFiles}`；
  超过阈值的工具结果写入文件，模型看到 `[tool result spilled to file]` + path，
  避免每轮重发。
- **中断恢复**：`kit.Config.CheckpointStore`；agent 在请求前/工具执行前/正常结束
  写检查点，`agent.RecoverInterruptedTurn` 在下次启动补“结果未知、未重放”的
  tool 结果与 assistant 标记，绝不重放副作用。
- **工具预设**：`kit.Config.ToolPreset` = `minimal`/`standard`/`all`；仅过滤工具
  集，不改变系统 prompt 与权限。
- **推理强度**：`kit.Config.ReasoningEffort` = `low`/`medium`/`high`（默认
  OpenAI 兼容 provider）；`rig.Request.Extra["reasoning_effort"]` 可按次覆盖。
- **审批辅助**：`gate.Ask(rig.Asker)`（allow/allow-always/deny）与
  `gate.Review(gate.ToolReviewer, ...)`（模型审前审批，失败/超时回退人工）。

## 5. 工具调用与结果

参数是 JSON 对象；必填字段见 `docs/ai/tools.json` 的 `required`。结果：

```go
type ToolResult struct {
    Content string
    Parts   []ContentPart // 图片等附件
    IsError bool
}
```

- `bash/pwsh` 非零退出是 `Content` + `[exit code: N]`、`IsError=false`（DSH 语义）；超时/无法启动才 `IsError=true`。
- 路径相对 root/policy roots 解析，越界拒绝。
- `tools.NewRegistry(...).Specs()` 可生成模型可见工具列表。

## 6. 宿主接口与条件能力

| 接口/配置 | 支撑工具 |
| --- | --- |
| `rig.Gate` | 全部工具（含 `run_code` 内层） |
| `rig.Asker` | `ask_user` |
| `rig.Presenter` / `rig.FilePresenter` | `present_artifact` / `present` |
| `rig.PlanController` | `plan_review` / `exit_plan_mode` |
| `rig.GoalStore`（可选 `hosttools.GoalRevisionStore`） | `goal` / 三个 goal 工具 |
| `rig.SubagentRunner` | `subagent`/`subagent_fork`/`workflow`/`ralph`/`team_*` |
| `rig.SessionLog` | `session_*`/`inspect`/`report` |
| `rig.Scheduler`/`MemoryStore`/`Settings`/`Reviewer` | schedule/memory/settings/review |
| `web.Searcher` | `web_search` |
| `AllOptions.MCPClients` / `LSP` | MCP/LSP 工具 |

默认 `full.All` 会构造需要接口的工具；未配置时调用返回可读错误，AI 应检查可用性/向用户确认。

## 7. AI 工作规则

1. 先定组装方式：快速用 `kit.New`；完整能力用 `full.All` + 显式依赖。
2. 不假设工具已注册；缺工具时给用户明确错误，不要伪造调用。
3. 遵守权限：默认只读 gate；写/执行需显式放行；`danger-full-access` 仅在可信/容器环境。
4. 异步工具用 `wait_agent`/`team_wait`/`team_last` 收结果；完成通知会作为 user 消息进入下一步。
5. 长任务用 `job_*`/terminal；后台 bash 返回 job id。
6. 只操作允许路径；不要路径逃逸或让解释器绕过 allowlist。
7. PTC 只在 `kit.Config.PTC=true` 时存在。
8. 不改模型可见的描述/Schema/错误文案。

## 8. 文件地图

| 路径 | 内容 |
| --- | --- |
| `agent/` | ReAct 循环、工具执行、通知注入 |
| `kit/` | 一站式组装（默认 full.All、权限/审批、PTC） |
| `sandbox/` | 权限模式、路径/命令策略、mvdan shell 解析 |
| `tools/full/` | 全部静态工具与后台/通知接线 |
| `tools/<name>/` | 各工具包（含中文包文档） |
| `cmd/rigdoc/` | 生成 AI 工具目录 |
| `docs/ai/tools.json` | 机器可读 Schema（AI 首选） |
| `AGENTS.md` | 仓库内 AI 规则 |

## 9. 重新生成

```sh
go run ./cmd/rigdoc
go run ./cmd/rigdoc -check
```
