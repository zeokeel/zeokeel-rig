# ZeoKeel Rig 文档索引

本文解决一个问题：**面对一个 SDK 仓库，我该从哪篇文档开始读？** 它是全部文档的入口，
说明文档语言约定、按读者列出每篇文档，并提供一张"我想做 X → 先读哪篇"的路由表。
第一次接触 Rig 的读者从 [GETTING-STARTED.md](GETTING-STARTED.md) 开始；要改仓库代码
的读者先读 [../AGENTS.md](../AGENTS.md)，那是仓库工作规则与逐模块说明书。

## 文档语言

Rig 的正式文档只有中文：仓库根的项目文档、`docs/*.md`、`AGENTS.md`、
`CONTRIBUTING.md`、`SECURITY.md`、`COMPATIBILITY.md`、`SUPPORT.md` 都用中文写作。
[README.en.md](../README.en.md) 是英文入口（简短版 + 链接中文文档），
`CODE_OF_CONDUCT.md` 保持英文原文。

模型读的东西不翻译：工具 `Description`、JSON Schema、错误文案与门禁理由始终是英文，
`docs/ai/tools.json` 也按英文生成——那是模型可见的工具目录，改动它等于改接口。

## 文档一览

| 文档 | 内容 | 读者 |
| --- | --- | --- |
| [../README.md](../README.md) | 项目门面：定位、能力、快速开始、包清单 | 所有人 |
| [../README.en.md](../README.en.md) | 英文入口（简短，链接中文文档） | 英文读者 |
| [GETTING-STARTED.md](GETTING-STARTED.md) | 环境要求、安装、最小 agent、三种组装方式、审批与沙箱怎么接、会话持久化、常见任务配方、排障表 | 新手 / 集成者 |
| [ARCHITECTURE.md](ARCHITECTURE.md) | 分层图、一次 Run 的请求流、契约层、工具面装配、模块化守门、安全边界、持久化分层、扩展点、质量与发布 | 贡献者 / 集成者 |
| [TOOLS.md](TOOLS.md) | 工具目录（按类别）+ 四个计数口径 + 每个工具的权限与平台影响 | 集成者 / 贡献者 |
| [CONFIG.md](CONFIG.md) | `kit.Config` 全字段、默认值、优先级、环境变量 | 集成者 |
| [OWNERSHIP.md](OWNERSHIP.md) | 能力归属矩阵：哪一层负责什么（Rig / 宿主 / 待办）与三条判据 | 集成者 / 贡献者 |
| [SANDBOX-OS.md](SANDBOX-OS.md) | opt-in 的 `sandbox/osrunner`：平台后端、fail-closed 语义、可写授权与诊断工具 | 集成者 / 发布者 |
| [MEMORY.md](MEMORY.md) | 记忆/上下文数据库（VFS、分层、检索）与溢出（spill）的存储契约 | 集成者 |
| [sqlite.md](sqlite.md) | 会话持久化三档：NDJSON 目录 / SQLite 子模块 / `storage` 聚合契约 | 集成者 |
| [COVERAGE.md](COVERAGE.md) | 覆盖账本与 CI 门禁；新增工具要改哪些地方 | 贡献者 |
| [VERSIONING.md](VERSIONING.md) | 版本策略与兼容承诺 | 发布者 / 集成者 |
| [MIGRATION-v0.25.md](MIGRATION-v0.25.md) | 从 v0.24 升到 v0.25 的破坏性变更清单（旧 API / 新做法 / 备注）与 `cmd/rigmigrate` 扫描器 | 升级中的宿主集成者 |
| [RELEASE.md](RELEASE.md) | 发布流程：PR → CI → annotated tag（不需签名）→ release workflow 直接发布 | 发布者 |
| [AI-SDK.md](AI-SDK.md) | 给 AI 的 SDK 使用与组装指南（装配决策、常见陷阱） | AI |
| [ai/tools.json](ai/tools.json) | 机器可读的工具目录：每个工具的精确 JSON Schema | AI |
| [ai/tools.md](ai/tools.md) | 同一份工具目录的人读表格 | AI / 贡献者 |
| [ai/README.md](ai/README.md) | `docs/ai` 的生成方式与使用说明 | AI / 贡献者 |
| [../AGENTS.md](../AGENTS.md) | 仓库工作规则 + 逐模块说明；改代码前必须读 | 贡献者 / AI |
| [../CONTRIBUTING.md](../CONTRIBUTING.md) | 贡献流程与新增工具的完整清单 | 贡献者 |
| [../SECURITY.md](../SECURITY.md) | 漏洞报告渠道与威胁模型 | 贡献者 / 集成者 |
| [../COMPATIBILITY.md](../COMPATIBILITY.md) | release line 兼容矩阵 | 发布者 / 集成者 |
| [../SUPPORT.md](../SUPPORT.md) | 支持渠道与提问前该收集的信息 | 所有人 |
| [../CHANGELOG.md](../CHANGELOG.md) | 逐版本变更历史（包括"未发布"部分） | 所有人 |
| [../llms.txt](../llms.txt) | 给 LLM 的入口摘要 | AI |

## 按任务找文档

| 我想… | 先读 | 再读 |
| --- | --- | --- |
| **接一个模型**（云端 API 或本地服务） | [CONFIG.md](CONFIG.md) 的 provider 相关字段（`ProviderKind`、`BaseURL`、`APIKey`、`APIKeyEnv`、`ReasoningEffort`、`ModelContextTokens`） | [GETTING-STARTED.md](GETTING-STARTED.md) §1、§4；`go doc github.com/zeokeel/zeokeel-rig/provider/openai` |
| **加一个工具** | [../AGENTS.md](../AGENTS.md) §4（新工具不是自动注册的） | [COVERAGE.md](COVERAGE.md)、[TOOLS.md](TOOLS.md)、[ai/tools.json](ai/tools.json) |
| **开沙箱**（OS 级围栏） | [SANDBOX-OS.md](SANDBOX-OS.md) | [GETTING-STARTED.md](GETTING-STARTED.md) §6、[CONFIG.md](CONFIG.md) 的 `Sandbox*` 字段 |
| **持久化**（会话/记忆/溢出） | [sqlite.md](sqlite.md) | [MEMORY.md](MEMORY.md)、[GETTING-STARTED.md](GETTING-STARTED.md) §7 |
| **升级到 v0.25**（破坏性变更） | [MIGRATION-v0.25.md](MIGRATION-v0.25.md) | [../CHANGELOG.md](../CHANGELOG.md)、[COMPATIBILITY.md](../COMPATIBILITY.md) |
| **发布**（出一个新版本） | [RELEASE.md](RELEASE.md) | [VERSIONING.md](VERSIONING.md)、[COMPATIBILITY.md](../COMPATIBILITY.md)、[COVERAGE.md](COVERAGE.md) |
| 让 AI 帮我写集成代码 | [AI-SDK.md](AI-SDK.md) | [ai/tools.json](ai/tools.json) |
| 理解内部设计与请求流 | [ARCHITECTURE.md](ARCHITECTURE.md) | 对应包的 `doc.go` / `go doc` |

## 维护约定

**数字口径以测试输出为准。** 工具数量有四个口径，写文档时必须标明是哪一种：

| 口径 | 数字 |
| --- | --- |
| `kit.New(kit.Config{})` 零配置**注册** / **初始 active** | **40 / 16**（接 `Config.Presenter` 后 **41 / 17**） |
| `full.All(full.AllOptions{Root, Policy})` | **81**（`SandboxGrants: true` 时 **83**；空 `AllOptions{}` 只有 **78**） |
| 账本 `catalog.Static` / `presets["all"]` | **83** |
| 预设 `personal` / `standard` / `minimal` | **55 / 73 / 8** |

发布前用下面的命令复算，出现不一致先改测试里的账本，再改文档：

```sh
go test ./tools/full -run TestToolCatalogCoverage -v
go test ./internal/rigdoc -run TestDocumentationCounts -v
go run ./cmd/rigdoc -check
```

**改工具面必须同步 `docs/TOOLS.md` 与 `rigdoc`。** 一次工具面改动至少涉及：实现包 →
`tools/full` 接线 → `tools/catalog.Static` →（需要时）`tools/presets` 名字表 → 测试 →
`go run ./cmd/rigdoc`（重新生成 `docs/ai/tools.json` 与 `docs/ai/tools.md`）→
[docs/TOOLS.md](TOOLS.md) 与 [docs/COVERAGE.md](COVERAGE.md) → `CHANGELOG.md`。少一处
代码门禁（`tools/full` 覆盖测试、`tools/audit`）就会红；文档部分靠评审与发布前自查。详见 [COVERAGE.md](COVERAGE.md)。

其它约定：

- 一篇文档只用一个 H1；开头写"解决什么问题 / 读者是谁"，结尾给"相关文档"。
- 工具名、字段名、错误文案用代码格式（反引号或 Go 代码块）；不翻译模型可见字符串。
- 不引用已删除的文档，也不写内部评审/规划过程；历史变更写在 [../CHANGELOG.md](../CHANGELOG.md)。
- 示例代码以 `examples/` 下能编译的程序为准：`examples/quickstart`、`examples/agent`、
  `examples/custom-tool`。

## 相关文档

- 仓库根项目介绍：[README.md](../README.md)
- 仓库 AI 规则与模块地图：[AGENTS.md](../AGENTS.md)
- 第一次集成：[GETTING-STARTED.md](GETTING-STARTED.md)
- 分层与请求流：[ARCHITECTURE.md](ARCHITECTURE.md)