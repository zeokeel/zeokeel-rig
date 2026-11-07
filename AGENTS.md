# AGENTS.md — 在本仓库中工作的 AI 规则

本仓库是 **ZeoKeel Rig**（Go agent toolset SDK），当前 release v0.12.0。

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
- 权限模型：`read-only` / `workspace-write` / `danger-full-access` + `ask/auto/never` 审批；不要重新引入 OS 沙箱或外部运行时依赖（PTC 例外：只调用系统已安装的 Python/Node）。
- 发布按 `docs/RELEASE.md`：PR → CI → signed tag → release workflow → publish。

## 使用 SDK 时

- 快速路径：`kit.New`（`Config.Tools == nil` → 默认 `full.All` 81 个静态工具）。
- 完整路径：`full.All(AllOptions{Host, Runner, Searcher, SessionLog, MCPClients, LSP, ...})` 后传给 `agent.AgentSpec.Tools` 或 `kit.Config.Tools`。
- 默认 gate 只放行只读工具；写/执行需显式 `Gate`/`Approval`。
- `subagent`/`spawn_teammate` 是异步的：用 `wait_agent`/`team_wait`/`team_last` 收结果，完成通知会注入下一步。
- 后台 shell/terminal 共享同一个 `jobs.Manager`；`full.All` 已接好，手动组装要传 `WithBackgroundJobs`。
- `run_code` 只在 `kit.Config.PTC=true` 时出现，内层调用走同一 Gate。
