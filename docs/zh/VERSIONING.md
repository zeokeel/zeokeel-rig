# 版本策略

ZeoKeel Rig 遵循[语义化版本（Semantic Versioning）](https://semver.org/) 与 Go 的 [module 版本编号](https://go.dev/ref/mod#versions)（语义导入版本）。本文说明 release 编号承诺什么、版本号存放在哪里，以及 tag 如何处理。

**当前状态：** pre-1.0，release line 为 `v0.15.x`（当前 `v0.15.0`，下一个小版本 `v0.16.0`）。模块路径是 `github.com/zeokeel/zeokeel-rig`，稳定契约面是模块根包 `rig`。在 `v1.0.0` 之前，小版本仍可能包含有文档记录的破坏性变更；消费方应锁定精确版本。

## 版本号存放在哪里

| 来源 | 位置 | 作用 |
| --- | --- | --- |
| Release tag | git tag `vX.Y.Z` | **权威来源。** `go get mod@vX.Y.Z` 解析的就是它，Go module proxy 与校验和数据库也记录它。 |
| `rig.Version` | 模块根 `version.go`（`const Version = "X.Y.Z"`） | 供诊断与支持请求使用的运行时版本号。必须等于去掉前导 `v` 的 tag。 |
| `CHANGELOG.md` | `## vX.Y.Z — YYYY-MM-DD` 小节 | 人类可读的发布说明；release 工作流会把该小节原样复制到 GitHub Release。 |
| `COMPATIBILITY.md` | release line 表格 | 每条 release line 的 Go 最低版本与依赖策略。 |
| `go.mod` | `go 1.27` | 模块的最低 Go 工具链。 |

tag 与常量被一起强制：

- `scripts/sync-version.sh` 从 `version.go` 读取 `const Version`，解析 tag（显式参数、`GITHUB_REF` 或 `git describe --tags --exact-match`），不一致时以非零退出。`make version` 运行它。
- `.github/workflows/release.yml` 在发布前运行 `scripts/sync-version.sh --require-tag`，因此与 `version.go` 不一致的 tag 无法产生 release。
- `version.go` **总是等于最新已发布 release tag**。日常开发中不要提前 bump：这个常量是 release 状态，不是开发标记。

## 语义导入版本

Go module 把 major 版本编码进 import path：

- **v0 与 v1** 使用裸路径 `github.com/zeokeel/zeokeel-rig`。模块根是 `package rig`，该包就是消费方导入的契约面。
- **v2 及以后** 使用 `github.com/zeokeel/zeokeel-rig/v2`（依此类推 `/v3`、…）。v1.0.0 之后的破坏性变更在新的 import path 上创建新的 major 版本。
- **绝不使用版本化子目录。** 本模块不会有 `v1/`、`v2/` 或 `contracts/v1` 这样的 generation 目录（v0.9 之前的 `contracts/v1` 包被移到模块根，正是为了遵循这条规则）。每个 major 版本是独立的 module path、有独立的 tag，而不是一个 module 里的文件夹。

在同一个 major 版本内，契约面以增量方式演进：

- 新包、工具、类型、函数、struct 字段与接口是增量变更（在结构体末尾添加字段并使用 keyed literal；消费方应始终使用 keyed literal）；
- 给已导出的接口增加方法是破坏性变更（会破坏外部实现），因此能力增长通过*可选*接口完成，由调用方 type-assert，而不是拓宽既有接口；
- 不改变签名也可能破坏性改变行为；非平凡的行为变更会记录在 `CHANGELOG.md`；
- 移除与重命名遵循[弃用策略](#弃用策略)。

### 什么算破坏性变更

- 删除或重命名已导出的标识符（包、类型、函数、方法、常量、变量），或改变其签名/类型。
- 删除或重命名已导出的 struct 字段；在导出布局有影响的结构体中间插入字段。
- 以合理消费方可能依赖的方式改变已文档化函数、选项或默认值的含义。
- 提高最低 Go 版本或增加 module 依赖。
- 让 sandbox 策略默认限制更宽松（安全相关；即使 API 未变也按破坏性处理）。

新增导出 API、新工具、新的可选配置字段或新的 backend 实现不属于破坏性变更。

## Pre-1.0 规则（`v0.x`）

由于模块 pre-1.0，语义化版本保证比 `v1+` 弱。本项目的具体规则：

| 版本位 | 允许内容 | 兼容承诺 |
| --- | --- | --- |
| `v0.Y.Z+1`（patch） | Bug 修复、文档、性能、安全修复；允许增量导出 API 但通常应等到 minor | 现有代码可编译且行为一致，除已被修正的 bug |
| `v0.Y+1.0`（minor） | 新功能、新工具、新包；**可能有破坏性契约变更** | 破坏性变更必须列在 `CHANGELOG.md` 的 `BREAKING` / `Removed` 下并附迁移说明 |
| `v1.0.0`（major） | 稳定化；首次兼容承诺 | 此后严格遵循语义化版本 |

消费方应：

- 在 `go.mod` 中锁定精确版本（`require github.com/zeokeel/zeokeel-rig v0.15.0`）并有意识地升级；
- 升级前阅读 `CHANGELOG.md` 的 `## Unreleased` / 对应 release 小节；
- 优先使用 keyed struct literal（`rig.AgentSpec{Model: ...}`），这样新增可选字段不会破坏代码；
- 把根契约面视为稳定集成点，不要依赖其他包中文档未承诺的内部细节。

v0.9 line 的具体情况：契约从 `contracts/v1` 移到模块根 `rig` 包，sandbox/网络加固记录在 [SECURITY.md](../../SECURITY.md) 和 [docs/ARCHITECTURE.md](ARCHITECTURE.md)。

## Post-1.0 规则

从 `v1.0.0` 起，严格遵循语义化版本：

- **Major**（`v2.0.0`）：破坏性变更；新的 import path `github.com/zeokeel/zeokeel-rig/v2`。上一个 major 在继任 major 发布后至少继续获得 **6 个月**的关键安全与回归修复（维护者可延长）。修复以旧 major 路径上的 `v1.x.y` tag 发布。
- **Minor**（`v1.Y.0`）：仅增量 API；无破坏性变更。
- **Patch**（`v1.Y.Z`）：向后兼容的修复、文档与安全修复。

新 major 绝不因外观原因创建，旧 major 绝不从仓库删除。

## 弃用策略

- 被弃用的 API 在其文档注释中用 Go 标准标记：`// Deprecated: use X instead.`
- 它会在 `CHANGELOG.md` 的 `Deprecated` 类别中列出并继续工作：
  - pre-1.0：移除前至少保留一个 minor release；
  - post-1.0：直到下一个 major 版本。
- 被弃用的代码在真正移除前仍保持测试覆盖。
- 移除是 `BREAKING`（pre-1.0），或落在新 major（post-1.0），并附迁移说明。

## Tag 不可变与撤回

已发布的 tag 是**不可变**的：

- 绝不要移动、删除或 force-push 已发布 tag。Go module proxy 与校验和数据库（`sum.golang.org`）缓存 tag 内容；移动 tag 会产生校验和不匹配，并破坏每个消费方现有的构建。
- Release tag 是 **annotated 且签名**的（`git tag -s vX.Y.Z -m "..."`，使用 `.github/allowed_signers` 中列出的 SSH 或 GPG 签名密钥）。release 工作流用 `git tag -v` 验证签名；未签名或无法验证的 tag 会在创建任何东西之前使发布失败。
- Release tag 必须指向 `main` 上的提交，且必须是携带 release `CHANGELOG.md` 小节与匹配 `version.go` 的提交。tag 只发布一次；release 工作流验证它，缺少 notes 小节时 fail closed。

如果某个 release 有问题：

1. 向前修复：在 `main` 上落修复并切新的 **patch** release（`vX.Y.Z+1`）。绝不要重新打 tag。
2. 如果 release 坏到消费方不能使用，在后续版本的 `go.mod` 中加入 [`retract`](https://go.dev/ref/mod#go-mod-file-retract) 指令；若问题与安全相关，发布 GitHub Security Advisory。撤回是给 `go` 命令的信号，不会重写历史。
3. 如果 push tag 后 GitHub Release 本身缺失或格式错误，重跑失败的 workflow job（见 [docs/RELEASE.md](RELEASE.md)）；tag 本身仍不可变。

分支名、commit hash、`main` 上的 `version.go` 与当前日期都不是 release 身份——只有 tag 是。

## 预发布版本

形如 `vX.Y.Z-rc.N`（或 `-beta.N`）的 tag 是合法的 Go module 版本，遵循相同的 tag 规则。`go get` 不会选择预发布版本，除非显式请求。release 工作流会为任何 `v*` tag 创建**草稿** GitHub Release，但不设置 `--prerelease`；请手动发布预发布 tag（`gh release edit --prerelease`）或先扩展工作流。

## Go 版本与依赖策略

- 模块要求 **Go 1.27**（`go.mod` 中的 `go 1.27` 指令）。提高最低版本会在 `CHANGELOG.md` 与 `COMPATIBILITY.md` 中公告；pre-1.0 期间这对旧工具链上的消费方属于兼容性相关变更。
- 模块有**恰好一个依赖**：`mvdan.cc/sh/v3`（`sandbox` 使用的 POSIX shell 解析器）。根 `rig` 包是只 import 标准库的叶子，CI 锁定实际构建的 import 集合。除非先修改本文档与 `CONTRIBUTING.md` 中的策略，否则不接受新依赖。
- `COMPATIBILITY.md` 记录每条 release line 的 Go 最低版本；`README.md` 展示当前安装 tag。

## Changelog 约定

`CHANGELOG.md` 遵循 [Keep a Changelog](https://keepachangelog.com/)，并且是 release notes 的唯一来源：

- 开发中的工作放在 `## Unreleased` 下，使用标准类别（`Added`、`Changed`、`Deprecated`、`Removed`、`Fixed`、`Security`）；
- 发布时用 `## vX.Y.Z — YYYY-MM-DD` 替换 `Unreleased`（日期前的 em dash 是项目风格；release 工作流按 `## vX.Y.Z` 前缀匹配）；
- 每个已发布版本恰好一个 release 小节，顶部恰好一个 `Unreleased` 小节；
- 破坏性变更标记为 `BREAKING`、commit type 带 `!`，并包含迁移说明；
- release 工作流会原样提取该小节；没有匹配小节的 release 会失败。

发布流程本身见 [docs/RELEASE.md](RELEASE.md)。
