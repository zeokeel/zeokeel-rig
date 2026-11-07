# 覆盖门禁 —— “有没有工具漏了？”

SDK 维护一份机器可读的完整 agent 工具面记录，一旦有东西消失就让 CI 失败。这是每个 release 都要跑的流程。

## 组成

| 组成 | 位置 | 作用 |
| --- | --- | --- |
| 账本 | `tools/catalog/catalog.go` | `Static`（必须可构造）、`Legacy`（旧 agent 的精确工具面）、`ServerBound`（需要 MCP/LSP 客户端）、`Dynamic`（运行时名称模式） |
| 聚合器 | `tools/full/full.go` | `full.All(options)` 构建每个静态工具 |
| 门禁 | `tools/full/catalog_test.go` | `TestToolCatalogCoverage` 断言：每个 `Static` 名称都存在、每个 legacy 名称要么被构建要么被显式标记为 server-bound、没有重复名称 |
| 现代面审计 | `tools/audit/` | `Reference`（DSH 内置工具）、`Mappings`、`DocumentedGaps`（host/kernel-bound）、`Modern`、`ServerBound`、`Dynamic`；门禁：`tools/audit/modern_test.go` |
| CI | `.github/workflows/ci.yml` | 在常规 test/race 步骤之后显式运行覆盖门禁 |

本地运行：

```bash
go test ./tools/full -run TestToolCatalogCoverage -count=1 -v
# catalog complete: 81 static tools, 61 legacy names, 65 modern names, 0 documented gaps, 6 server-bound, dynamic [mcp__<server>__<tool>]
```

## 账本列表

- **Static** —— 81 个无需外部进程即可由 headless SDK 构建的工具：文件系统/editor/image、shell/terminal/jobs、web、session/inspect/report、带内存默认实现的 host 接口工具、memory/schedule/worktree/skill、编排（含 subagent_fork）与 misc。
- **Legacy** —— 从上一代 ZeoKeel agent 提取的 61 个工具名（`old/internal/tools` 加上运行时的 `settings_get`/`settings_set`）。它们为兼容而保留；移除其中任何一个都是破坏性变更，必须在 `CHANGELOG.md` 中公告（当前 release line 没有计划移除）。若有名称消失且未列入 `ServerBound`，审计失败。
- **ServerBound** —— 6 个仅在配置了 MCP 服务器或语言服务器时才存在的工具（`mcp_list_resources`、`mcp_read_resource`、`mcp_list_resource_templates`、`mcp_list_prompts`、`mcp_get_prompt`、旧版 `lsp`）。它们的构造器与测试在 `tools/mcp` / `tools/lsp`。
- **Modern** —— 65 个当前一代 coding agent 预期暴露的工具名（DSH 风格工具面）。`catalog.AuditModern` 在同一个测试里运行。
- **DocumentedGaps** —— catalog 层面的缺口：当前为零。子 agent 上下文工具 `structured_output` 以能力形式提供（`SubagentSpec.OutputSchema`），不是独立静态名称。现代面审计保留自己的 6 个 host/kernel-bound 缺口，记录在 `docs/AUDIT-REPORT.md`（历史快照；当前计数以测试输出为准）。
- **Dynamic** —— `mcp__<server>__<tool>` 名称在运行时由每个服务器自己的工具列表生成。

## 现代面审计（`tools/audit`）

catalog 门禁回答“是否有 legacy/static 工具消失”。现代面审计回答互补的问题：“今天的 Rig 工具面是否仍覆盖参考现代 agent（DeepSeek Harness）？”它从 v0.7 引入，每次 release 都重跑：

- `Reference` —— 63 个模型可见内置工具，提取自安装的 `@deepseek-ai/dsh 0.2.1-rc.2`（工具包 + `toolName` 配置覆盖 + 常量注册工具；提取脚本见 `docs/AUDIT-REPORT.md`）。
- `Mappings` —— 63 个参考工具中有 57 个映射到覆盖它们的 Rig 工具/能力（一个 Rig 工具可覆盖多个参考工具，例如 `goal` 覆盖 `create_goal`/`get_goal`/`update_goal`）。
- `Modern` —— 审计要求的 55 个静态 Rig 名称；`Missing()` 必须为空。
- `ServerBound` —— 10 个仅在配置外部客户端时存在的 Rig MCP/LSP 名称（含 v0.8 的 `mcp_list_resource_templates`），因此不针对静态构建断言。
- `DocumentedGaps` —— 6 个剩余能力，每个带机器可读的 `Category`：`kernel-bound`（2 个：`cordis_inspect_list`、`cordis_inspect_query`，需要一个拥有模块运行时的产品）或 `host-bound`（4 个：`load_workspace_dependencies`、`plugin_manager`、`plugin_market`、`run_code`，属于进程/产品关注点）。`run_code` 现在作为 **opt-in kit 扩展**提供（`kit.Config.PTC`，使用系统安装的 Python/Node 运行时，内层调用由 Gate 检查），但刻意不是 `full.All` 静态工具，因此审计缺口仍归类为 host-bound。完整原因与替代方案见 `docs/AUDIT-REPORT.md`；缺口集合与 2/4 分类由测试锁定，不能静默漂移。
- `Dynamic` —— `mcp__<server>__<tool>`，与参考命名一致。

```bash
go test ./tools/audit/... -count=1 -v
# modern tool surface covered: reference=63 mapped=57 gaps=6 modern=55 missing=0 (static tools=81)
# audit tables: reference=63 mapped=57 gaps=6 modern=55 server-bound=10
```

## 新增工具（检查清单）

1. 在 `tools/<name>` 包中实现它（仅标准库；host 能力使用根 `rig` 包接口）。
2. 把它接入 `tools/full/full.go`（`All`）——否则门禁失败。
3. 把名称加入 `catalog.Static`；若来自旧 agent，同时加入 `catalog.Legacy`（或说明为何是 server-bound）。
4. 在工具包中加测试；运行 `gofmt`、`go vet ./...`、`go test ./... -count=1 -race`，然后跑覆盖门禁。
5. 若该工具补齐了现代面能力，更新 `tools/audit`（`Reference`/`Mappings`/`Modern`/`ServerBound`/`DocumentedGaps`）、其计数门禁以及 `docs/AUDIT-REPORT.md`。
6. 更新 `docs/TOOLS.md` 与 `CHANGELOG.md`；运行 `go run ./cmd/rigdoc` 重新生成 `docs/ai/`；打 tag 发布。

## 可复现的 legacy 审计

打包脚本会把旧 agent 与账本做 diff（测试夹具工具被排除，因为它们只存在于 `_test.go` 文件）：

```bash
./scripts/audit-legacy-tools.sh /path/to/old-agent-repo
# legacy tools scanned: 58
# catalog legacy names: 61
# catalog additions (Rig-only tools, expected): exit_plan_mode, plan_review, web_search
# audit: legacy tool surface complete
```

已知测试夹具工具名（`big`、`binder`、`block`、`clock`、`echo`、`side_effect`）从不是真实工具，会被扫描过滤；如果旧仓库新增真实工具，脚本会失败并给出要添加的名称。

## 审计更新的 legacy 面

如果更早的产品线出现另一个工具，提取名称并 diff：

```bash
# old repo
grep -rhA1 'Name() string' old/internal/tools/*.go | grep -oE '"[a-z_0-9]+"' | tr -d '"' | sort -u
```

与 `catalog.Legacy` 比较（`TestToolCatalogCoverage` 会打印任何缺失且非 server-bound 的名称）。
