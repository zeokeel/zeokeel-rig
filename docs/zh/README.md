# 中文文档索引

> 当前对应 release：**v0.16.2**（Go 1.27+，`CGO_ENABLED=0`，唯一依赖 `mvdan.cc/sh/v3`，81 个静态工具）。
> 中文文档是默认阅读版本；英文原文保留作为参考。若文档与代码冲突，以代码为准。

## 指南

| 文档 | 内容 |
| --- | --- |
| [AI-SDK.md](AI-SDK.md) | **给 AI 的 SDK 使用/组装指南**（首选入口）：三种组装方式、权限、host 接口、工具选型 |
| [TOOLS.md](TOOLS.md) | 完整工具目录：81 个静态工具、server-bound MCP/LSP、动态 `mcp__<server>__<tool>`、opt-in `run_code` |
| [CONFIG.md](CONFIG.md) | 配置约定与默认值、优先级、权限模式与审批策略、PTC、环境变量 |
| [ARCHITECTURE.md](ARCHITECTURE.md) | 分层、请求流、扩展契约、刻意缺失项、安全与版本策略 |
| [SECURITY.md](SECURITY.md) | 安全策略、支持版本、漏洞报告、威胁模型与供应链 |
| [COVERAGE.md](COVERAGE.md) | 工具覆盖账本/门禁，以及新增工具的完整检查清单 |
| [VERSIONING.md](VERSIONING.md) | 语义导入版本、破坏性变更定义、弃用与 tag 不可变策略 |
| [RELEASE.md](RELEASE.md) | 发布流程、签名 tag、release/publish 工作流与发布后复审 |
| [COMPATIBILITY.md](COMPATIBILITY.md) | 各 release line 的 Go 最低版本与依赖矩阵 |
| [SANDBOX-OS.md](../SANDBOX-OS.md) | 可选的纯 Go OS 沙箱 runner：平台矩阵、接入、fail-closed 与限制 |

## 其它入口

- 项目总览：[根目录中文 README](../../README.md)；英文：[README.en.md](../../README.en.md)。
- 机器可读工具 Schema：[docs/ai/tools.json](../ai/tools.json)；人读表格：[docs/ai/tools.md](../ai/tools.md)（由 `go run ./cmd/rigdoc` 生成，CI 用 `-check` 防漂移）。
- 英文原文：[docs/TOOLS.md](../TOOLS.md)、[docs/CONFIG.md](../CONFIG.md)、[docs/ARCHITECTURE.md](../ARCHITECTURE.md)、[SECURITY.md](../../SECURITY.md)、[docs/COVERAGE.md](../COVERAGE.md)、[docs/VERSIONING.md](../VERSIONING.md)、[docs/RELEASE.md](../RELEASE.md)、[COMPATIBILITY.md](../../COMPATIBILITY.md)。

## 当前计数（以 CI 门禁输出为准）

```text
catalog complete: 81 static tools, 61 legacy names, 65 modern names,
                  0 documented gaps, 6 server-bound, dynamic [mcp__<server>__<tool>]
audit: reference=63 mapped=57 gaps=6 modern=55 missing=0 (static tools=81)
audit tables: reference=63 mapped=57 gaps=6 modern=55 server-bound=10
```

## 维护约定

- 英文原文是唯一事实来源；改英文文档、工具数量或版本时，请同步更新本目录。
- 不要在本目录复制大段代码；链接到源码与英文原文，避免第二份事实来源漂移。
- 工具模型可见的 `Description`/`Schema`/错误文案属于接口，不翻译；中文文档只解释用法。
