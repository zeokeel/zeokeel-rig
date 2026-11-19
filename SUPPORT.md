# 支持

本文说明遇到问题该去哪里问、提问时带什么信息、以及能期待什么样的响应。读者是使用 SDK 或在本仓库上做集成的人。Rig 由志愿者维护，支持是**社区尽力而为**，没有商业支持、没有响应时限 SLA、也没有私有支持渠道。

## 去哪问

仓库的支持入口是 GitHub Issues（`.github/` 下的模板与 `config.yml` 的联系链接都以它为中心；没有单独开启 Discussions）：

| 你要做的事 | 渠道 |
| --- | --- |
| 报告 SDK 的缺陷 | [GitHub Issues → Bug report](https://github.com/zeokeel/zeokeel-rig/issues/new?template=bug_report.yml) |
| 提功能或 API 需求 | [GitHub Issues → Feature request](https://github.com/zeokeel/zeokeel-rig/issues/new?template=feature_request.yml) |
| 用法问题（"怎么做 X"） | 仍然走 Issues：用 Bug report 模板，标题前加 `[question]` |
| 报告安全漏洞 | **只能走私密渠道**，见 [SECURITY.md](SECURITY.md)；绝不要开公开 issue |
| 行为准则相关投诉 | 走私密渠道，并标注为 conduct concern，见 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) |
| 行政事务联系维护者 | 开 issue 并 @`zeokeel-dev` |

Issue 模板本身会要求版本、Go 版本、平台与最小复现；`config.yml` 里也把安全、支持、贡献三条链接放在新建 issue 页面上，所以直接选模板比手写空白 issue 更省事（本仓库关闭了空白 issue）。

提问前请先自查这些地方，多数问题在里面已有答案：

- [README.md](README.md) — 安装、快速开始、组装方式；
- [docs/CONFIG.md](docs/CONFIG.md) — `kit.Config` 全字段、默认值、优先级；
- [docs/TOOLS.md](docs/TOOLS.md)、[docs/COVERAGE.md](docs/COVERAGE.md) — 工具目录、工具面口径数字；
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — 分层、请求流、设计边界；
- [docs/SANDBOX-OS.md](docs/SANDBOX-OS.md) — OS 沙箱 runner 的接入与平台限制；
- [docs/sqlite.md](docs/sqlite.md) — 会话持久化（NDJSON / SQLite / 存储契约）；
- [docs/VERSIONING.md](docs/VERSIONING.md)、[COMPATIBILITY.md](COMPATIBILITY.md) — 版本与兼容性；
- [CHANGELOG.md](CHANGELOG.md) — 两个版本之间到底改了什么；
- 包文档：<https://pkg.go.dev/github.com/zeokeel/zeokeel-rig>（每个包都有中文 `doc.go`，`go doc <包>` 也能看）。

## 提问模板

一个**最小、自包含**的复现是最有价值的东西。请附上：

1. **版本**：`rig.Version` 的值，或 `go.mod` 里的 release tag。
2. **平台**：操作系统与架构（例如 `linux/amd64`、`darwin/arm64`、`windows/amd64`）。
3. **Go 版本**：`go version` 的完整输出。
4. **最小复现**：能独立跑起来的最小 Go 程序或测试（不要贴整个项目）。
5. **期望 vs 实际**：你以为会发生什么，实际发生了什么（含完整错误文本或 panic）。
6. **日志**：相关片段；**先删掉 API key、token 与私有数据**。
7. **沙箱/权限相关**（涉及工具执行、文件访问、命令被拒时）：`sandbox.Mode`、`kit.Config.Approval`、可写根、`AllowNetwork`、是否注入了 `Gate`/`Asker`/`SandboxRunner`。
8. **如果你改过默认值**：`ToolPreset`、`InitialActiveTools`、`Config.Tools`、自定义 `Gate`——这些都会改变行为，先说清楚。

可直接复制的骨架：

```text
### 版本
rig.Version: 0.23.4 / go.mod: github.com/zeokeel/zeokeel-rig v0.23.4

### 平台与工具链
go version go1.27.3 linux/amd64

### 最小复现
<能独立运行的最小程序或测试；构造工具调用/策略的那几行要完整>

### 期望
<应该发生什么>

### 实际
<实际发生了什么；完整错误文本>

### 沙箱/权限配置
Mode: workspace-write
Approval: ask
Asker: 有 / 无
SandboxRunner: osrunner / 无
Roots: ...

### 日志
<脱敏后的相关输出>
```

## 响应预期

- 维护者是志愿者，处理顺序按影响面与可复现性排，不按提交时间；
- 没有响应时限承诺；使用问题的答复可能很慢；
- 没有私有支持、没有优先级支持、没有商业支持合同；
- 中文或英文提问都可以，用你能把问题说清楚的语言；
- 一个带上失败测试的 PR 通常比 issue 更快解决问题（见 [CONTRIBUTING.md](CONTRIBUTING.md)）。

## 支持的版本

只有当前 release line 的最新 patch 会收到修复：当前是 **`v0.23.x`**，最新发布 `v0.23.4`。更早的版本不保证修复，请先升级再提问。模块处于 pre-1.0，请在 `go.mod` 里 pin 精确版本并刻意升级；细节见 [SECURITY.md](SECURITY.md#支持的版本) 与 [docs/VERSIONING.md](docs/VERSIONING.md)。

## 安全问题

**不要**把漏洞写进公开 issue、PR 或 discussion。唯一渠道是 GitHub 私密漏洞报告，报告内容与响应预期见 [SECURITY.md](SECURITY.md#如何私下报告)。

## 我们不能帮什么

- 调试你的模型端点、网关、代理或 provider 凭据问题（`provider/openai` 只按 OpenAI 兼容协议发请求，端点侧的问题不在这里的范围）；
- 为基于 Rig 的产品做超出公开 issue 范围的设计评审；
- 私有、优先或付费支持——项目不提供；
- 宿主主动关掉安全机制后的行为（例如注入 `nil` 或放行一切的 `Gate`、把模式开到 `danger-full-access`）；
- 声称沙箱提供了它明确不承诺的隔离（读、网络、进程可见性）——这些边界写着 [docs/SANDBOX-OS.md](docs/SANDBOX-OS.md) 和 [SECURITY.md](SECURITY.md#威胁模型) 里。

## 贡献

修 bug、补文档、加测试都欢迎，流程与本地门禁见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 相关文档

- [SECURITY.md](SECURITY.md) — 漏洞报告渠道、支持版本、威胁模型
- [CONTRIBUTING.md](CONTRIBUTING.md) — 开发环境、代码规则、PR 要求
- [docs/VERSIONING.md](docs/VERSIONING.md) — 版本策略与升级方式
- [docs/CONFIG.md](docs/CONFIG.md) — 配置项与默认值
- [docs/TOOLS.md](docs/TOOLS.md)、[docs/COVERAGE.md](docs/COVERAGE.md) — 工具目录与口径
- [CHANGELOG.md](CHANGELOG.md) — 版本变更记录