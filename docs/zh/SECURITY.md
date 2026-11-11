# 安全策略

ZeoKeel Rig 是纯 Go、无 cgo 的 SDK（一个依赖：`mvdan.cc/sh/v3` POSIX shell 解析器，只被 sandbox 策略层使用），agent 循环与工具**运行在宿主进程内部**。本文说明哪些版本会收到安全修复、如何私下报告漏洞，以及内置沙箱策略保证什么、不保证什么。

## 支持的版本

安全修复只落在当前 minor line 的最新 patch 上。由于模块仍处于 pre-1.0，没有长期支持分支；消费方应锁定精确版本（见 [docs/VERSIONING.md](../VERSIONING.md)）并有意识地升级。

| 版本 | 支持状态 |
| --- | --- |
| `0.14.x` | ✅ 仅最新 patch |
| `0.13.x` | ❌ 请升级到 `0.14.x` |
| `0.12.x` | ❌ 请升级到 `0.14.x` |
| `≤ 0.10` | ❌ 不再支持 |

受支持 line 的 Go 最低版本是 **Go 1.27**（`go.mod` 中声明）；较旧工具链上的消费方应先升级 Go 再升级 Rig。

## 报告漏洞

**绝不要**为疑似漏洞开公开 issue、pull request 或 discussion。请使用 GitHub 私有漏洞报告：

1. 打开 <https://github.com/zeokeel/zeokeel-rig/security/advisories/new>（仓库 **Security** 标签页 → **Report a vulnerability**）。这是唯一的私密渠道；仓库已启用私有漏洞报告。
2. 如果你的账号无法使用该表单，开一个只说“I need a private security channel”的最小公开 issue 并提及 `@zeokeel-dev`。**不要**在公开 issue 中包含漏洞细节、复现步骤或凭证。
3. 尽量包含：
   - 受影响版本（`rig.Version` 或 release tag）与 `go version` 输出；
   - 操作系统；若涉及 sandbox，请给出策略配置（`sandbox.New(...)` 选项）以及是否安装了审批 gate；
   - 最小复现（Go 测试或小程序）与观察到的实际影响；
   - 任何建议的缓解措施。

请勿在报告中包含真实 API key 或私有数据。日志输出请脱敏。

## 你会得到什么

这是志愿者维护的开源项目；以下目标是尽力而为，不是商业 SLA（见 [SUPPORT.md](../../SUPPORT.md)）：

- **确认**收到报告：3 个工作日内。
- **初步评估**（严重程度、受影响版本、修复是否可行）：10 个工作日内。
- 对已确认问题提供修复与 GitHub Security Advisory（GHSA），随受支持 line 的下一个 patch release 发布。
- **协同披露**：目标是在收到报告后 90 天内发布 advisory（有修复时会更早）。在 advisory 发布前请勿公开披露，除非双方另有约定。
- 在 advisory 中致谢，除非你要求匿名。

如果报告只涉及 [范围与威胁模型](#范围与威胁模型) 中列出的项目，我们可能以超出范围为由关闭，并解释原因。

## 范围与威胁模型

Rig 是**进程内库**，不是加固的多租户服务。宿主链接进自己二进制的代码是可信代码。内置 `sandbox` 策略约束内置工具可以触碰什么；它**不是 OS 沙箱**，也不是第三方工具的沙箱。

可选的 `sandbox/osrunner`（默认关闭）在 Linux/macOS/Windows 上提供 OS 层的**文件效果**约束（纯 Go 平台 runner）。它仍是同世界约束，不是容器/VM：读、网络与进程可见性不受约束，Windows 为 `partial`，macOS 依赖已弃用的 `sandbox-exec`。宿主通过 `osrunner.New` 显式启用，并用工具的 `WithRunner` 注入。

### 范围内

- **已发布代码中的沙箱策略绕过**：通过 `sandbox.ResolvePath` 的路径穿越/符号链接逃逸、glob 或 grep 的隐藏文件泄漏、命令 allowlist 绕过，以及 `tools/web` 中的 URL/SSRF 策略绕过。
- **命令执行**：`bash`/`pwsh`/terminal/job 工具执行了配置策略本应拒绝的东西。
- **网络策略**：`web_fetch` 在策略禁止时访问 loopback、private、link-local 或云 metadata 地址，包括通过重定向或 DNS rebinding。
- **不可信输入导致的崩溃**：模型输出、工具参数、SSE/JSON-RPC/LSP 帧（消息 header 在分配前以 64 KiB 为界，包括单个未终止 header 行）或 URL 触发的 panic 或无界资源使用。
- **凭证处理**：通过工具输出、命令环境变量或错误泄漏 API key 或环境变量。

### 安全加固

v0.9.1–v0.9.4 修正了 v0.9.0 过度宣称的 shell allowlist 边界；v0.11.0 增加了权限模式围栏。权威行为记录在包文档和 `docs/ARCHITECTURE.md` 中，简版如下：

- **Shell 命令 allowlist**（`sandbox.CheckShellCommand` + `CheckCommand`，供 `bash`、`pwsh`、`jobs` 和 `terminal_send` 使用）：
  - 默认拒绝未加引号的 shell 控制操作符（`;`、换行、`|`、`&`、重定向、子 shell）；
  - 命令替换 `$(...)` 与反引号替换**即使在双引号内**也被拒绝——`echo "$(id)"` 在 `/bin/sh` 中会执行，带引号的操作符并不是字面量；
  - 进程替换（`<(...)`、`>(...)`）被拒绝；
  - 前置 `VAR=value` 环境变量赋值与路径形式命令名（`./tool`、`/bin/tool`）被拒绝，因此 `PATH=.`、`LD_PRELOAD=...` 和工作区二进制不能借用 allowlist 中的 basename；
  - allowlist 中的命令名按裸名称与策略匹配。

  **续行也被建模。** 解析器在评估文本前移除（单引号外的）POSIX 反斜杠-换行续行，因此 `echo "$\<newline>(id)"` 会被当作 `/bin/sh` 拼行后执行的 `$(...)` 拒绝。同一个解析器为 allowlist 检查生成 argv[0]（bash、jobs 与 terminal_send 共用），因此校验与分发不会对引号产生分歧。

  `sandbox.WithAllowShellOperators()` 是显式退出开关，会让命令 allowlist 变成建议性检查（弱边界；请与审批 gate 组合使用）。把解释器（`sh`、`bash`、`env`、`xargs`、`python`、`eval` 等）加入 allowlist，仍然等于给该解释器任意代码执行能力：对不可信模型不要 allowlist 解释器。allowlist 是面向可配合模型的策略层，不是安全边界；审批 Gate 才是不可信模型的边界（模型不可信时请把整个宿主跑在容器/VM 中）。
- **`web_fetch` SSRF 防护**：fetcher 自行解析每个被 dial 的主机并连接到校验过的 IP，因此校验地址无法在校验与 dial 之间改变（DNS rebinding）。实现 `web.HostResolver` 的策略（如 `sandbox.Policy`）提供校验过的地址；其他所有非 nil 策略（包括 `web.PolicyFunc`）会得到 fetcher 侧防护，拒绝 loopback、private、link-local、CGNAT 与云 metadata 地址。`web.WithAllowPrivateHosts()` 是面向本地目标的显式 opt-out；nil 策略不安装任何防护。重定向的每一跳都会被校验。网络是否允许仍由宿主决定：`sandbox.WithNetwork(true)` 只允许公网 HTTP(S) 目的地。
- **权限模式（进程内围栏，不是 OS 沙箱）**：`sandbox.Mode` 提供 `read-only`（拒绝文件变更与 shell 执行）、`workspace-write`（默认；只在工作区根目录内写，shell 仅限命令 allowlist）和 `danger-full-access`（无根目录/allowlist 围栏；显式 denylist 仍生效）。`kit.Config.Approval` 增加 `ask`（默认 fail-safe 的只读 gate）、`auto`（放行所有调用）和 `never`（拒绝所有调用）。这些只约束随附的工具实现：敌对的进程内工具、被 allowlist 的解释器或交互式终端会话仍可以做宿主进程能做的任何事。可选的 `sandbox/osrunner` 把模式的**写/删**部分下沉到 OS（Linux bwrap/Landlock、macOS Seatbelt、Windows ACL），默认关闭；它不约束读、网络或进程可见性，Windows 分支为 `partial`。对不可信模型请把宿主跑在容器/VM 中——完整 OS 隔离是部署层，不是 SDK 特性。
- **`terminal_*`**：shell 二进制必须通过命令 allowlist，且每个 `terminal_send` 行都会经过与非交互 shell 相同的操作符/替换与命令检查（`cd` 等 shell builtin 也需要显式 allowlist）。交互式 shell 状态仍然不是安全边界：请使用审批 gate，并在不可信模型时把宿主跑在容器/VM 中。

### 超出范围

- 任何需要在进程内运行不可信代码的场景：宿主提供的自定义 `rig.Tool`、`rig.Provider`、`rig.Gate`、`rig.History`、searcher 或 skill 实现按设计都是可信代码。
- 审批 `Gate`：安装宽松 gate（或 nil，即放行一切）的宿主已选择允许这些调用。Gate 策略是宿主责任。
- OS、容器或文件系统配置错误；以 root 运行 Rig；工作区根目录可被不可信方写入；`GOMAXPROCS`/内核限制。
- 模型端点或代理本身、prompt injection 的后果（被允许的工具做了不希望的事）、计费/用量，或宿主环境中的 TLS 拦截。
- 宿主在 Rig 之外链接的代码（包括 `zeokeel-kernel` 产品）中的漏洞，以及缺少 TLS 证书固定。
- 未在受支持 line 上展示复现的旧版本报告。
- `sandbox/osrunner` 的读、网络、进程可见性或设备约束：它的契约只覆盖文件效果（写/删/创建），且 macOS 分支依赖 Apple 保留已弃用的 `sandbox-exec`。

## 供应链

- 模块有**恰好一个依赖**——`mvdan.cc/sh/v3`，只被 `sandbox` 使用的 POSIX shell 解析器；CI 锁定实际构建的 import 集合，并检查根 `rig` 包只 import 标准库。
- 所有工作流中的 GitHub Actions 都按完整 commit SHA 固定（Dependabot 更新 `.github/dependabot.yml` 中的 pin）；仓库也向 GitHub 请求 SHA 固定的 actions。
- Release 从 `main` 上的**签名 annotated tag** 由 `.github/workflows/release.yml` 切出。tag 构建通过 `workflow_call` 复用完整 CI 工作流（因此跑的正是合并门禁），对照 `.github/allowed_signers` 验证 tag 签名、验证 `version.go` 与 tag 一致且 tag 在 `main` 上，对已发布模块做干净环境 `go get`/`go mod verify` 冒烟，然后创建**草稿** GitHub Release。`contents: write` 只存在于 publish job；维护者在发布后复审后通过 `publish-release` 工作流发布草稿。见 [docs/RELEASE.md](../RELEASE.md)。
- Go module proxy 与 `sum.golang.org` 是已发布产物的完整性锚点：release 冒烟中的 `go mod verify` 会用校验和数据库校验下载的模块。
- 供应链问题请通过与其它漏洞相同的私密渠道报告。

## 行为准则

报告与后续沟通受我们的 [Code of Conduct](../../CODE_OF_CONDUCT.md) 约束。骚扰或行为问题请通过上述 GitHub 私有安全 advisory 表单报告，并标记为行为问题；不要在公开 issue 中发布细节。
