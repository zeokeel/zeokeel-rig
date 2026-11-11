# ZeoKeel Rig —— 架构

> 配套：[模块根包 rig](../../doc.go)、[README](../../README.md)、[docs/AUDIT-REPORT.md](../AUDIT-REPORT.md)。
> 本设计来自促成 Rig 抽取的产品侧审计；可移植的摘要见审计报告。

## 1. 分层

```
consumer product (e.g. ZeoKeel desktop app)
  process host + kernel assembly + product modules + UI
        │ imports
        ▼
ZeoKeel Rig (this repo) — pure toolset, one parser dependency, cgo-free
  rig (module root) · agent · provider · tools · gate · session
```

Rig 拥有 **agent 运行时原语**；产品拥有**组装（composition）**。这是刻意的：模块运行时 / DI 容器 / 配置树不属于工具集，强行加给每个 Rig 消费者只会让依赖树更重而没有收益。例如 ZeoKeel 把 `zeokeel-kernel` 和它的 `assembly.json` 留在产品仓库。

## 2. 请求流

```
caller ──RunRequest──▶ agent.Run
                         │  load History, append input
                         ▼
              ┌─ round loop (≤ MaxIterations) ──────────────────┐
              │  Provider.Stream(req)                           │
              │    chunks → events (text/reasoning/tool deltas) │
              │    final → assistant Message (+ tool calls)     │
              │  no tool calls → done                           │
              │  tool calls: Gate.Approve each → execute        │
              │    (parallel, results in call order)            │
              │  append tool Messages → next round              │
              └─────────────────────────────────────────────────┘
                         │
                         ▼
                    RunResult + Events (synchronous callback)
```

线程模型：一次 run 的事件流是单 goroutine 的；并行工具执行内部使用 goroutine，并在下一轮前 join。run 通过其 `context.Context` 取消并返回 `ctx.Err()`。`Emit` 同步调用；需要扇出的消费者应自行缓冲。

## 3. 扩展契约（模块根，`package rig`）

| 接口 | 实现它可以增加 |
| --- | --- |
| `Provider` / `Stream` | 一个模型厂商（或包装另一个 API） |
| `Tool` | 模型可调用的一个能力 |
| `Gate` | 审批策略（按调用 allow/deny/allow-always） |
| `History` | 持久或自定义的会话存储 |
| `Logger` / `Settings` | 接入宿主的日志/设置 |
| `SubagentRunner` / host 接口 | 自定义子 agent 运行时，或注入 UI/store（`Asker`、`Presenter`/`FilePresenter`、`PlanController`、`GoalStore`、`MemoryStore`、`Scheduler`、`SessionLog`、`Reviewer`…） |

消息是 wire-shaped（角色、content parts、tool calls、tool-result 消息），因此 provider 可以把它们映射到任何 chat-completions 风格的 API。Rig 从不在会话中保存 provider SDK 类型。

## 4. 刻意不包含的内容

- 模块注册表 / 编译期装配 / 预检 / 回滚（产品关注点——ZeoKeel 用 kernel 实现）；
- UI 工具包、进程宿主、配置文件、数据库驱动；
- 运行时插件加载、HMR、容器/内核级沙箱。`sandbox` 包实施的是**进程内权限策略**（`read-only` / `workspace-write` / `danger-full-access` 三种模式加一个审批 gate）；完整 OS 级隔离是部署关注点：不可信模型请把宿主跑在容器/VM 中。可选的、默认关闭的 `sandbox/osrunner` 把模式的**写/删**部分下沉到 OS（bwrap/Landlock、Seatbelt、Windows ACL），供不想要容器的宿主使用。

## 5. 安全与信任

Rig 运行在产品的二进制中；每个工具和 provider 都是你编译进来并信任的代码。内置工具实施：

- **文件工具** —— 根目录沙箱（`os.Root`/`OpenPath`），拒绝路径穿越与符号链接逃逸；
- **shell 工具** —— 命令允许/拒绝名单（在 `danger-full-access` 下不再限制）、受限环境变量、输出上限与超时；共享同一个 POSIX 感知解析器（`sandbox.ParseShellCommand`），在拒绝未加引号的控制操作符、命令/进程替换（`$(...)`、反引号、`<(...)`、`>(...)`，**即使在双引号内**）、前置环境变量赋值（`PATH=.`、`LD_PRELOAD=...`）和路径形式命令名之前，先按 `/bin/sh` 的方式处理引号、转义与反斜杠-换行续行。因此 allowlist 中的第一个命令无法串联或夹带第二个命令，包括通过被 `/bin/sh` 拼接成 `"$(id)"` 的 `"$\<newline>(id)"` 这类写法。`sandbox.WithAllowShellOperators` 是显式退出开关，会使命令 allowlist 退化为建议性检查。allowlist 是面向可配合模型的策略层，不是 OS 边界；把解释器（`sh`、`env`、`xargs`、`python` 等）加入 allowlist 就等于有意允许任意执行，审批 Gate 仍是边界（不可信宿主请跑在容器/VM 中）；
- **LSP** —— 入站消息体上限 16 MiB，header 在任何分配前先以 64 KiB 字节预算读取（单个未终止的 header 行会被拒绝而不是被缓冲）；LSP 工具接受可选的路径策略，`tools/full` 会从 sandbox 策略推导；
- **`web_fetch`** —— 仅 HTTP(S)、重定向/大小上限与 URL 策略。fetcher 总是自行解析被 dial 的主机并固定校验过的 IP，关闭 `CheckURL` 无 DNS 契约留下的 DNS rebinding 缺口。实现 `web.HostResolver` 的策略（如 `sandbox.Policy`）提供已校验地址；其他任何非 nil 策略会得到 fetcher 侧防护，拒绝 loopback/private/link-local/CGNAT/metadata 地址。`web.WithAllowPrivateHosts` 是面向本地目标的显式 opt-out。
- **可选 OS runner**（`sandbox/osrunner`，默认关闭）—— 实现 `sandbox.Runner`，把 `bash`/`pwsh`/`job_start`/`terminal_*`/`git worktree` 的 argv 包成平台原生文件效果围栏：Linux bwrap 或纯 Go Landlock 启动器、macOS `sandbox-exec`、Windows Write-Restricted 令牌 + ACL helper。失败即拒绝（`sandbox.ErrSandboxUnavailable`），不属于默认装配；见 [SANDBOX-OS.md](../SANDBOX-OS.md)。

残余限制：这是进程内策略，不是内核/容器边界。shell 命令与工具以当前用户运行；审批 `Gate` 仍是调用方对不可逆操作的政策边界，忽略策略的工具可以做进程能做的任何事。`terminal_*` 工具会启动交互式 shell：shell 二进制必须通过 allowlist，且 `terminal_send` 的每一行都按非交互 shell 命令检查，但交互式 shell 状态（变量、被 allowlist 的 builtin）与会话内启动的程序不会被进一步限制——任何被 allowlist 的解释器/命令同样如此。请使用 Gate，并在模型不可信时把宿主跑在容器/VM 中。可选的 `sandbox/osrunner` 只在 OS 层收窄文件效果（写/删/创建），不约束读、网络或进程可见性，Windows 分支为 `partial`；它补充而不是替代 Gate/容器建议。

根 `rig` 包从不 import 其他 rig 包，且仅使用标准库（CI 同时强制这两点，以及依赖策略与无 cgo 构建）。

平台说明：`bash`、`jobs` 与 `terminal_*` 需要 unix `/bin/sh`/PTY，本 release 仅支持 Unix；在 Windows 上它们返回清晰的配置错误，`pwsh` 工具是受支持的 shell 入口。CI 在 Linux 上运行完整单元测试，并对 windows/darwin/freebsd 交叉编译 + `GOOS` vet；本仓库没有托管 macOS/Windows runner，因此这些平台的运行时覆盖是已披露限制（见 README.md 的“平台支持”）。`sandbox/osrunner` 同理：Linux 分支有真实强制测试，macOS/Windows 分支只做 cross-compile + `GOOS` vet，运行时测试按平台标签执行。

## 6. 版本策略

- `v0.x`：API 可能在小版本间变化；同一条 release line 内以增量为主，弃用会提前公告。
- `v1.0.0`：根 `rig` 包冻结；破坏性变更通过新的 module major 版本（`github.com/zeokeel/zeokeel-rig/v2`）发布，绝不用版本化子目录。
- 模块恰好有一个依赖（`mvdan.cc/sh/v3`，POSIX shell 解析器，仅被 `sandbox` 导入）；根 `rig` 包保持仅标准库，没有依赖矩阵。
