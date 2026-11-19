# 工具覆盖账本与门禁

本文回答："这个 SDK 的工具面有没有悄悄少一个？新增工具要改哪些地方？CI 在哪一步拦住漂移？"读者是给仓库加/删工具的人，以及需要判断"这几个数字为什么不一样"的宿主开发者。工具本身的逐项说明在 [TOOLS.md](TOOLS.md)。

覆盖门禁的原则很简单：**任何静态工具都必须能被构造出来，任何构造出来的工具都必须在账本里**。前者防"新增了工具却忘了接线"，后者防"接线了却忘了登记"——两个方向都检查，才不会有工具静默地待在门禁之外。

## 1. 门禁由三部分组成

| 部分 | 位置 | 回答的问题 | 失败时 |
| --- | --- | --- | --- |
| 账本 | `tools/catalog/catalog.go` | 应该存在哪些工具名？ | — |
| 聚合器 | `tools/full/full.go` | 实际构造出哪些工具？ | — |
| 覆盖门禁 | `tools/full/catalog_test.go`（`TestToolCatalogCoverage`） | 账本与聚合器是否**双向**一致？ | CI 红，发布阻塞 |
| 现代面对照审计 | `tools/audit/`（`modern_test.go`） | 今天的工具面是否仍覆盖参考现代 agent 的工具面？ | CI 红 |
| 生成物新鲜度 | `go run ./cmd/rigdoc -check` | `docs/ai/*` 是否与代码一致？ | 本地/发布前自查（**不进 CI**） |

## 2. 账本里的列表

`tools/catalog` 有五个名字列表加一个动态模式：

| 列表 | 数量 | 含义 |
| --- | --- | --- |
| `Static` | **83** | 无头构建必须能在不依赖外部 server/UI 的情况下构造出的全部工具名。`tools/full` 的构建结果与它**双向**比对 |
| `Legacy` | **58** | 上一代 ZeoKeel agent 的确切工具面。删除其中一个旧名字是破坏性变更，必须在 `CHANGELOG.md` 公告 |
| `ServerBound` | **7** | 只有配置了外部 client 才存在的名字：`spill_read`、5 个 MCP 资源/提示工具、旧版合并工具 `lsp` |
| `Modern` | **62** | 当代编码 agent 应当暴露的工具面（超集式审计；名字可与 `Legacy` 重叠） |
| `DocumentedGaps` | **0** | 刻意不暴露为静态工具名的现代能力。子代理上下文工具 `structured_output` 是 `SubagentSpec.OutputSchema` 能力，不是独立静态名字 |
| `Dynamic` | `mcp__<server>__<tool>`（冲突时 `…__2`/`__3`） | 由每个 MCP server 自己的工具表在运行期生成的名字模式；清洗后撞名加确定性后缀 |

> **不要把 `tools/catalog` 的数字与 `tools/audit` 的数字混在一起。** `tools/audit` 是另一套对照表：`Reference` 63 个参考工具、`Mappings` 57 条映射、`DocumentedGaps` 6 个（2 个 kernel-bound + 4 个 host-bound）、`Modern` 55、`ServerBound` 10（含 `lsp` 与 `lsp_*` 分项）。其中参考面的 6 个 gap 与 `catalog.DocumentedGaps` 的 0 是两个不同的问题：前者是"参考 agent 有、Rig 不做"，后者是"Rig 静态名字层面刻意缺的"。

`tools/audit` 的 gap 理由与替代方案写在 `tools/audit/audit.go` 的 `DocumentedGaps` 条目里（`Reason` / `Alternative` 由 `TestReferenceAccounting` 强制非空）。早期的审计报告文档已从文档树删除，不要再引用它；审计的当前事实以这些表与测试输出为准。

## 3. 由哪些测试执行

### 3.1 覆盖门禁

```sh
go test ./tools/full -run TestToolCatalogCoverage -v
```

权威输出（实跑，逐字）：

```text
catalog complete: 83 static tools, 58 legacy names, 62 modern names, 0 documented gaps, 7 server-bound, dynamic [mcp__<server>__<tool>]
```

这一行是门禁打印的权威口径（数字来自 `catalog.Static`/`Legacy`/`Modern`/`DocumentedGaps`/`ServerBound`）。文档引用工具数量时应与它一致；**改数字前先改代码与账本**。CI 只跑代码门禁，不解析文档里的这行。

测试做的检查：

1. 用 `full.All(full.AllOptions{Root: t.TempDir(), SandboxGrants: true})` 构造完整静态工具集；返回空集直接失败。
2. 任何重名工具立即失败（`agent.New` 也会拒绝重名，这里提前拦住）。
3. `catalog.Check(names)`：`Static` 里每个名字都必须被构造（`Missing` 为空）。
4. 反向检查：构造出来的每个名字都必须在 `Static` 里（`Extra` 为空）。
5. `catalog.AuditModern(names)` 缺的名字必须出现在 `catalog.DocumentedGaps`。
6. `catalog.AuditLegacy(names)` 缺的名字必须出现在 `catalog.ServerBound`。
7. 打印上面那行 `catalog complete: …`。

注意门禁显式打开了 `SandboxGrants`：两个沙箱授权工具默认不在 `full.All(AllOptions{Root})` 里（81），但属于账本（83），所以门禁开选项后才能双向对齐。

### 3.2 现代面对照审计

```sh
go test ./tools/audit/... -v
```

权威输出（实跑，逐字）：

```text
modern tool surface covered: reference=63 mapped=57 gaps=6 modern=55 missing=0 (static tools=81)
audit tables: reference=63 mapped=57 gaps=6 modern=55 server-bound=10
```

它构建 `full.All(full.AllOptions{Root: t.TempDir()})`（不传 `SandboxGrants`，所以 `static tools=81`），断言：

- `TestModernToolSurface`：`audit.Modern` 的每个名字都能构造，且无重名；
- `TestReferenceAccounting`：每个参考工具要么有映射、要么是文档化 gap；映射目标必须是 `audit.Modern` 或 `audit.ServerBound`；`Modern` 有序不重复、不含 server-bound；`ProductRequired` 必须在 `Modern` 里；
- `TestDocumentedGapsAreIntentional`：gap 集合与类别划分锁定为 2 个 kernel-bound + 4 个 host-bound；
- `TestV08AuditCounts`：参考面计数锁定（63/57/6/55/10/2）；
- `TestMissing`：`Missing()` 助手本身的排序/空集语义。

新增或重命名工具时，如果它影响参考面覆盖，就必须同步改 `tools/audit` 的 `Mappings`/`DocumentedGaps` 与计数测试，而不是只改 `catalog.Static`。

### 3.3 生成物新鲜度

```sh
go run ./cmd/rigdoc -check
```

`cmd/rigdoc` 从真实构造器生成 [docs/ai/tools.json](ai/tools.json)（84 条 = 83 个静态工具 + opt-in `run_code`）与 [docs/ai/tools.md](ai/tools.md)。改了工具名、描述或 schema 而没重新生成时，`-check` 会失败——但这是**本地/发布前自查**，CI 不跑它。

## 4. 新增工具清单

少做任何一步都会让代码门禁红，或让文档与代码漂移（文档检查不进 CI，靠评审与发布前自查）。按顺序：

1. **实现包**：在 `tools/<name>/` 实现工具（只用标准库；宿主能力通过根包 `rig` 的接口）。模型的 `Description` / `Schema` / 错误文案是接口，用英文，不翻译。
2. **接线 `tools/full`**：在 `newRuntime`/`AllOptions` 里构造它。需要新宿主依赖就加一个 `AllOptions` 字段（并在零配置体检里有对应处理）；工作区级工具必须在有 `Root`/`Policy` 时才构造。
3. **登记 `catalog.Static`**：把名字加进 `tools/catalog/catalog.go` 的 `Static`。如果它来自旧版 agent，同时加进 `Legacy`；如果它需要外部 server/client，**不要**放 `Static`，放 `ServerBound`。
4. **更新预设表**：`tools/presets/presets.go` 的 `allToolNames` 是 `catalog.Static` 的手工副本（测试断言两者一致）。新工具默认进 `all`；是否进 `standard` 取决于它是不是兼容别名（别名加入 `compatAliasNames`），是否进 `personal` 取决于它是不是多智能体/编排类（加入 `personalExcluded`）。不要漏改——漏了工具在 `all` 预设里就不存在。
5. **写测试**：工具包自己的单测 + 覆盖门禁（`go test ./tools/full -run TestToolCatalogCoverage -v`）。若它补上了参考面的某个能力，同步 `tools/audit` 的 `Reference`/`Mappings`/`Modern`/`ServerBound`/`DocumentedGaps` 与计数锁。
6. **重新生成 AI 目录**：`go run ./cmd/rigdoc`，再 `go run ./cmd/rigdoc -check` 确认。
7. **改文档**：[TOOLS.md](TOOLS.md)（按类别加一行：包、availability、作用、关键参数/上限/权限）与本文（若口径或列表数量变了）。权限/配置相关再同步 [CONFIG.md](CONFIG.md)；新增可选能力包还要进 `internal/rigdoc` 的 `leafCapabilities`。
8. **写 `CHANGELOG.md`**：工具面变化是用户可见变更。
9. **跑全量门禁**：`gofmt`、`go build ./...`、`go vet ./...`、`go test ./...`（发布前 `-race`）、`make gate`。

补充：**不存在 `docs/AUDIT-REPORT.md`**。历史审计报告已删除；审计理由写在 `tools/audit/audit.go` 的表与 `DocumentedGaps.Reason` 里，由测试守住。文档统一用中文写在 `docs/` 下。

## 5. 代码门禁与 CI 位置

主 CI 在 `.github/workflows/ci.yml`（job `test`），**只验证代码**。与本账本相关的两个步骤：

| 步骤 | 作用 |
| --- | --- |
| `tool catalog coverage` | `go test ./tools/full -run TestToolCatalogCoverage -count=1 -v` |
| `modern tool surface audit` | `go test ./tools/audit -count=1 -v` |

同一 job 里还有 `gofmt`、`build`、`vet`、`test`、`test (race)`、覆盖率下限（总 80% + 有测试的包函数 ≥70%）、fuzz 冒烟、benchmark 冒烟、依赖策略、无 cgo、根包 stdlib-only、跨 OS 编译 + vet 等代码步骤。文档生成物（`rigdoc -check`）、README/示例同步与版本/tag 校验**不在 CI 里**；发布 workflow（`.github/workflows/release.yml`）复用同一套代码门禁，并另行校验版本与 tag。发布流程见 [RELEASE.md](RELEASE.md)。

本地命令（`Makefile`）：

```sh
make gate            # 两个覆盖门禁（CI 同款）
make ai-docs         # 重新生成 docs/ai
make ai-docs-check   # 只检查是否最新（本地/发布前）
```

## 6. 常见坑

| 坑 | 症状 | 正确做法 |
| --- | --- | --- |
| 重复工具名 | `duplicate tool name "x" in full.All`；`agent.New` 也会拒绝 | 一个名字只能有一个构造点；调用方需要合并多个来源时自己做去重（例如 `kit` 的 `appendUniqueTools`），别靠调用顺序碰运气 |
| server-bound 误登记 | 把它放进 `Static`，门禁报 `Missing`；或放进 `Dynamic` 却当静态 | 需要外部 client 的名字放 `ServerBound`；只在运行时生成的名字模式放 `Dynamic` |
| 忘记 `rigdoc` | 发布前 `rigdoc -check` 失败，`docs/ai/*` 仍是旧内容（CI 不检查） | 改完工具名/描述/schema 立刻 `go run ./cmd/rigdoc` |
| 数字口径混用 | 文档里 78/81/83/39 互相打架 | 每个数字都标口径：零配置注册 39、`full.All` 81（+grants 83）、账本 83、预设 55/73/83/8、`tools.json` 84 |
| 忘记预设表 | 工具在 `full.All` 里有，`presets["all"]` 里没有 | 同步 `tools/presets` 的 `allToolNames`；预设测试会断言它与 `catalog.Static` 一致 |
| 忘了 `SandboxGrants` | 新授权工具在门禁里 `Missing`（门禁开选项仍缺）或在默认集里多出来 | 授权类工具在 `AllOptions.SandboxGrants` 下构造，并登记进 `Static` |
| 忘了 `Root`/`Policy` | `full.All(full.AllOptions{})` 只有 78 个，`apply_patch`/`search`/`undo` 静默缺席 | 组装时传 `Root` 或带 `Roots` 的 `Policy` |
| 改模型可见文案 | 快照/测试漂移，且中文文档与模型实际看到的不一致 | `Description`/`Schema`/错误字符串保持英文；文档只解释，不翻译 |
| 复活已删除名字 | `report`、`settings_get`、`settings_set`、`present_artifact` 出现在账本/预设/文档里 | 这些工具已按产品定位移除，不要再加回；`internal/rigdoc` 的守护测试会拦已删除包 |
| 把 `run_code` 当静态工具 | 账本数量变成 84 | 它是 opt-in（`kit.Config.PTC`），只出现在 `tools.json` 的第 84 条，不进 `catalog.Static` |

## 相关文档

- [TOOLS.md](TOOLS.md) — 工具目录（按类别）、计数口径、动态加载、权限/平台影响
- [docs/ai/tools.json](ai/tools.json) / [docs/ai/tools.md](ai/tools.md) — 生成的机器可读目录
- [CONFIG.md](CONFIG.md) — `kit.Config` 字段与默认值
- [RELEASE.md](RELEASE.md) — 发布流程与门禁
- [ARCHITECTURE.md](ARCHITECTURE.md) — 分层与模块边界