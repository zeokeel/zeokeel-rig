# 兼容性（Go 版本、依赖、平台、release line）

本文解决什么问题：**我的工具链能不能编译它？它会不会给我带进一堆依赖？哪个平台上
它真的被测过？升级 release line 要注意什么？** 读者是准备把 Rig 拉进依赖图的宿主
开发者，以及要判断"这个版本能发吗"的维护者。

结论先行：**Go 1.27 起，唯一依赖 `mvdan.cc/sh/v3`，全程无 cgo**。依赖面从 v0.10.0
起就没有再变过；平台支持与"测过什么"见 §4。

## 1. 当前基线

| 项 | 值 | 来源 |
| --- | --- | --- |
| 模块路径 | `github.com/zeokeel/zeokeel-rig` | `go.mod` |
| 当前 release | `v0.26.1` | `version.go` 的 `rig.Version`（必须与 tag 一致） |
| Go 最低版本 | **1.27** | `go.mod` 的 `go 1.27` 指令 |
| Go 依赖 | **恰好一个**：`mvdan.cc/sh/v3 v3.14.1` | `go.mod` 的 `require` |
| cgo | **不需要**；构建与测试一律 `CGO_ENABLED=0` | CI 的 `cgo-free build and tests` |
| 许可证 | MIT（v0.7.0 起；v0.6.0 及更早 Apache-2.0） | `LICENSE` |
| Go 包数量 | 69（含 3 个 example、2 个 `cmd`、`internal/*`） | `go list ./...` |

### 1.1 唯一依赖是什么、为什么

`mvdan.cc/sh/v3` 是 **POSIX shell 解析器**，只被 `sandbox` 策略层使用：命令风险分级
（`ClassifyShellCommand`）需要真正读懂 shell 语法（管道、重定向、命令替换、`&&`
链），手写扫描器会在 `$(...)`、引号、注释上误判。它是纯 Go、无传递构建依赖，因此
不引入 cgo，也不改变"构建只要 Go 工具链"这条承诺。

除了它之外全部是标准库。这带来三条被 CI 门禁钉住的约束：

1. **根包 `rig` 只依赖标准库**，且不 import 本仓库任何子包（它是契约叶子）；
2. **叶子能力包不得 import `kit` 或 `tools/full`**（它们必须能被单独拿走使用）；
3. **只有 `kit` 同时装配 provider 与工具面**。

想要"编译期模块装配"这一类能力的产品自己接，Rig 不依赖也不推荐特定 runtime。

## 2. 按 release line 的矩阵

行只记录**依赖面与 Go 版本的变化**（这是本文的职责）；功能变化见
[CHANGELOG.md](CHANGELOG.md)。表里每行的 `go.mod` 都是从对应 tag 里读出来的。

| release line | Go 最低 | Go 依赖 | 该行发生的事 |
| --- | --- | --- | --- |
| **v0.26.x**（当前，`v0.26.1`） | 1.27 | `mvdan.cc/sh/v3 v3.14.1` | **当前行**：无新依赖、无 cgo。`v0.23.0` 是沙箱可用性大版本（工具链缓存可写、可写面统一为 `WritableRoot`、socket 屏蔽、沙箱内命令免审批），`v0.23.1`/`v0.23.2` 含**安全修复**（Windows 保护路径未生效、保护路径缺失改为 fail closed）——依赖面全程未变。**未发布（DSH 对齐）**：沙箱只做可写根围栏，保护路径与拒绝 glob 的 API 已**删除**（`WithHidden`/`ReadOnlySubpaths` 是替代），依赖面同样不变 |
| v0.18.x – v0.22.x | 1.27 | 同上 | `v0.18.0` 加了 stdlib-only 的 MCP OAuth 2.1 客户端（**无新依赖**）；`v0.22.0` 起 `sandbox_status`/`request_sandbox_access` 默认注册（仍是 discoverable），并引入逐命令提权——都是零新依赖 |
| v0.14.x – v0.17.x | 1.27 | 同上 | `v0.14.0` 引入 opt-in、默认关闭的纯 Go `sandbox/osrunner`（**无新依赖**、无 cgo） |
| v0.10.x – v0.13.x | 1.27 | `mvdan.cc/sh/v3 v3.14.1` | **`v0.10.0` 是唯一的依赖面变化**：用成熟 POSIX 解析器替换手写 shell 扫描器，并把 cgo-free 变成 CI 契约 |
| v0.9.x | 1.27 | 无 | 收敛到标准库；`contracts/v1` 已上提到模块根 |
| v0.2.x – v0.8.x | 1.27.3（这些 tag 里是补丁级固定） | 无 | `v0.2.0` 移除 `zeokeel-kernel` 依赖 |
| v0.1.x | 1.27.3（补丁级固定） | `github.com/zeokeel/zeokeel-kernel v0.1.1` | 最早的形态，**只有这一行有第二个依赖**，v0.2.0 起没有了 |

读法：**从 v0.10.0 起依赖面就没再变过**——`go.mod` 里永远只有一行 `require`。升级
release line 时要读的是 CHANGELOG 的行为变化与迁移说明，不是依赖变化。

## 3. 子模块 `session/sqlite`：不污染主模块

`session/sqlite` 是**独立 Go module**（自己的 `go.mod`、自己的 tag 前缀
`session/sqlite/vX.Y.Z`），把会话持久化放进一个 SQLite 数据库。它的依赖面明显更宽：

| 子模块 `go.mod` | 值 |
| --- | --- |
| module | `github.com/zeokeel/zeokeel-rig/session/sqlite` |
| Go | 1.27 |
| 直接依赖 | `github.com/zeokeel/zeokeel-rig`（根模块）+ `modernc.org/sqlite v1.62.0` |
| 间接依赖 | `modernc.org/libc`、`modernc.org/memory`、`modernc.org/mathutil`、`golang.org/x/sys` 等 |
| cgo | 不需要（`modernc.org/sqlite` 是纯 Go 实现） |

**为什么不放进主模块**：主模块的依赖门禁把编译进来的 import 钉死成一个解析器依赖。
SQLite 驱动虽然也是纯 Go，但依赖树很大；放进独立 module 之后：

- 只要 NDJSON 持久化（`session/store`）的宿主，依赖面**一个字节都没变**；
- 想要数据库的宿主 `go get github.com/zeokeel/zeokeel-rig/session/sqlite` 即可；
- 主模块的"唯一依赖 + cgo-free"门禁照常通过。

**主模块永不 import 它**（子模块单向依赖根模块，反向没有边），因此"装 Rig 会不会
带进 SQLite"的答案永远是"不会，除非你显式 `go get` 这个子模块"。

### 3.1 它尚未随正式 release 发布

截至本文写作时：**根模块最高 tag 是 `v0.24.0`，`session/sqlite/v*` 一个 tag 都没有**，
而子模块的 `go.mod` 里 `require github.com/zeokeel/zeokeel-rig v0.24.0` 指向一个从未
打过 tag 的版本（它编译需要主模块下一版才有的 `memory` 公共包）。同时文件里还留着
`replace github.com/zeokeel/zeokeel-rig => ../..`——那是**本仓开发**用的手段，只在
本仓工作树里生效（Go 只应用主 module 的 replace，下游 `go get` 时它是死声明）。

所以正确的描述是：**子模块已在 HEAD 源码里，但要等下一次发布的根 tag 与
`session/sqlite/vX.Y.Z` 一起打出后才对下游可用**。发布顺序是"先打根 tag，再打子模块
tag"，细节见 [docs/sqlite.md](docs/sqlite.md) 与 [docs/RELEASE.md](docs/RELEASE.md)。

CI 里由 `sqlite-consumer smoke`（**阻塞**）盯着代码/打包面：在临时 module 里
`go get` 本子模块并 `go build`，模拟真实宿主（无 `go.work`、无 `replace`）。

`require`、根 tag 与子模块 tag 三者是否自洽**不是 CI 检查**：发版前由发版人自查，
`release.yml` 的 `verify` 与 `published-module smoke` 会再次校验。

## 4. 平台支持与测试覆盖

| 平台 | CI 里的作为 | 覆盖程度 |
| --- | --- | --- |
| **Linux**（`ubuntu-24.04`） | `test` job：`gofmt`、build、vet、**全量 `go test`**、`-race`、覆盖率、fuzz 冒烟、benchmark 冒烟、工具账本/审计、依赖策略、cgo-free、根包 stdlib-only | **唯一跑全量测试与 `-race` 的平台** |
| **Windows**（`windows-latest`） | `windows sandbox (real Windows)` job（**阻塞**）：构建 + 沙箱后端纯逻辑测试 + **真机 ACL / 受限令牌 / Job 进程树**运行期测试 | 本仓**不做 Wine 假验证**（Wine 只模拟 API 形状，不强制 restricting SID / 完整性级别 / ACL 继承，跑出"通过"是假阳性） |
| **macOS** | 独立的 `macos-sandbox.yml` workflow（`macos-26`，push 命中 `sandbox/**`、`internal/procattr/**`、`tools/builtin/**` 或手动触发）：真机跑 `sandbox/osrunner` 的 Seatbelt 测试 | **真机验证存在，但不阻塞 main 与发布**（runner 侧曾被系统性取消；恢复稳定后可并回 `ci.yml` 并升级为阻塞门禁） |
| **Windows / macOS / FreeBSD**（交叉） | `test` job 里的 `cross-compile and vet`：对 `windows/amd64`、`darwin/arm64`、`freebsd/amd64` 做 `go build ./...` 与 `go vet ./...` | 保证"能编译、能过 vet"，不代表运行时行为已验证 |
| 其他平台 | 无 CI | 编译层面可能可用；`sandbox/osrunner` 在这些平台上 **fail closed**（`sandbox.ErrSandboxUnavailable`），不假装隔离 |

CI 的阻塞 job 是：`test`、`windows sandbox (real Windows)`、`lint`
（staticcheck + govulncheck）、`sqlite-consumer smoke`。CI **只验证代码**；文档与
版本/tag 一致性不在 CI 里。

### 4.1 工具层的平台差异（会影响你的产品行为）

- `bash` 需要 unix `/bin/sh`；**Windows 上它仍会注册，但每次调用返回可读错误并提示
  改用 `pwsh`**。反向的摘除只发生在另一边：`kit` 在**非 Windows** 平台把 `pwsh` 从
  工具集里摘掉（它在那里必然报"找不到命令"）。
- `job_start`（后台任务）用**平台 shell** 起进程：Unix 是 `/bin/sh -c`，Windows 优先
  `pwsh`、再退到 Windows PowerShell、最后 `cmd.exe`。所以后台任务在 Windows 上可用，
  而 `bash` 工具本身不可用（见上一条）。
- `terminal_*` 在 unix 用 PTY；Windows 用 ConPTY，ConPTY 不可用时**回退到 pipe**
  （工具仍可用，但交互式全屏程序可能表现不同）。
- 本地 provider 的默认地址与能力一致（`ollama` 默认 `127.0.0.1:11434`、`lmstudio`
  默认 `127.0.0.1:1234`），与平台无关。

## 5. 构建与依赖约束（承诺部分）

- **构建与测试必须 `CGO_ENABLED=0`。** 唯一例外是 `go test -race`（Go 工具链本身
  需要 cgo 才能启用竞态检测），它属于开发/CI 工具，不属于库的构建方式。
- **不给消费者带进第二个依赖。** 新增依赖需要先改
  [VERSIONING.md](docs/VERSIONING.md) 与本文件里的政策，再进代码。
- **根包 stdlib-only**、**叶子能力包不 import `kit`/`tools/full`**、**只有 `kit`
  同时装配 provider 与工具面**——三条都由测试守住，不是口头约定。
- **不依赖外部运行时**：`run_code`（PTC）只调用系统已安装的 Python 3 / Node.js；
  `osrunner` 的 macOS 分支只 exec 系统自带的 `sandbox-exec`。

## 6. 兼容性承诺

完整的版本策略见 [docs/VERSIONING.md](docs/VERSIONING.md)，这里只说与"能不能升级"
相关的四条：

1. **`v0.x` 期间 minor 版本可以带破坏性变更**（会在 CHANGELOG 里标 `BREAKING` 并给
   迁移说明）。生产宿主应**固定精确版本**并主动升级，不要依赖 `v0` 的语义化假设。
2. **模块根包 `rig` 是稳定契约面**，在同一个大版本内**additive** 演进：新字段追加
   在末尾、零值保持旧行为、选项只加不改语义、移除先经过 `Deprecated`。
3. `v1.0.0` 之后的破坏性变更会走**新的 import 路径**
   （`github.com/zeokeel/zeokeel-rig/v2`），**不会**在模块里建 `v2/` 目录。
4. 消费方请一律用**带键的结构体字面量**（`kit.Config{Model: …}`、`openai.Config{BaseURL: …}`），
   不要用位置字面量——这样新增可选字段永远不会打断编译。

## 7. 相关文档

- [docs/VERSIONING.md](docs/VERSIONING.md) —— 语义化版本、tag 不可变性、废弃政策
- [docs/RELEASE.md](docs/RELEASE.md) —— 发布流程（含子模块 tag 顺序）
- [docs/sqlite.md](docs/sqlite.md) —— `session/sqlite` 子模块的能力与发布状态
- [docs/SANDBOX-OS.md](docs/SANDBOX-OS.md) —— 各平台 OS 沙箱后端与 fail-closed 语义
- [README.md](README.md) —— 安装方式与最小示例