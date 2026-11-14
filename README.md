# ZeoKeel Rig

[![CI](https://github.com/zeokeel/zeokeel-rig/actions/workflows/ci.yml/badge.svg)](https://github.com/zeokeel/zeokeel-rig/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zeokeel/zeokeel-rig.svg)](https://pkg.go.dev/github.com/zeokeel/zeokeel-rig)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/zeokeel/zeokeel-rig)](https://github.com/zeokeel/zeokeel-rig/releases)

> [English README](README.en.md) · 中文为默认文档；主要指南另有[中文翻译](docs/zh/README.md)，机器可读工具目录见 [docs/ai/tools.json](docs/ai/tools.json)。

**纯 Go 的 agent 工具集** —— 模型 provider、流式 ReAct 循环、工具、权限模式与审批 Gate、会话历史。只有一个很小的依赖：[`mvdan.cc/sh/v3`](https://pkg.go.dev/mvdan.cc/sh/v3)（POSIX shell 解析器，仅被 sandbox 策略层使用）；其余全部是 Go 标准库。无 cgo：构建与测试都在 `CGO_ENABLED=0` 下运行。

Rig 是从 [ZeoKeel](https://github.com/zeokeel) 抽出的可复用 agent 核心。它**不**包含模块运行时或进程装配：这一层由产品自己保留（ZeoKeel 使用 [zeokeel-kernel](https://github.com/zeokeel/zeokeel-kernel) 做编译期模块装配）。稳定的契约类型位于模块根包（`package rig`）。最短路径使用 [`kit`](kit) 门面：`kit.New` 以**一个 import** 接好 OpenAI 兼容 provider、完整沙箱工具集、安全默认 gate 与会话历史。需要自行组装二进制时，导入子包（`agent`、`tools`、`provider/openai`、`gate`、`session`）。

## 快速开始

下面代码就是 `examples/quickstart/main.go`：一个 import、一个构造函数。CI 在每次 push 时编译它，并校验本 README 中的代码块仍与文件一致，所以你复制的第一段代码总是能构建。

```go
// Command quickstart 是用 Rig SDK 构建的最小完整 agent：kit 门面在一次 import 中
// 装配好兼容 OpenAI 的 provider、完整的沙箱工具集、安全的只读审批门禁以及内存
// 历史。
//
// 与模型对话需要凭据：
//
//	OPENAI_API_KEY=... go run ./examples/quickstart "list the repo and summarize it"
//
// provider 默认使用 https://api.openai.com/v1。接入 OpenAI 兼容网关时设置
// Config.BaseURL，并用 Config.Model 选择模型。没有 API key 时程序仍可构建；
// 第一次运行会报告 provider 错误。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/kit"
)

func main() {
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "List the files in this directory and summarize them."
	}

	// 一次 import、一个构造函数：kit.New 默认使用完整工具集，沙箱限定在工作目录，
	// 禁用 shell 执行（没有命令允许列表），不联网，并且门禁只放行
	// read_file/list_dir/glob/grep。需要扩大 agent 能力时传入
	// Config.Gate/CommandAllowlist。
	a, err := kit.New(kit.Config{
		Model: "gpt-4o-mini",
		// 接入 OpenAI 兼容网关时：
		// BaseURL: "https://api.deepseek.com/v1", APIKeyEnv: "DEEPSEEK_API_KEY",
	})
	if err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// RunText 返回聚合后的助手文本；emit 会实时流出事件。
	_, result, err := a.RunText(ctx, "quickstart", prompt, func(event kit.Event) {
		if event.Type == kit.EventTextDelta {
			fmt.Print(event.Text)
		}
	})
	if err != nil {
		fail(err)
	}
	fmt.Println()
	fmt.Printf("finish=%s iterations=%d tokens=%d\n", result.FinishReason, result.Iterations, result.Usage.TotalTokens)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "quickstart:", err)
	os.Exit(1)
}
```

用任意 OpenAI 兼容端点加凭证运行：

```sh
OPENAI_API_KEY=... go run ./examples/quickstart "list the repo and summarize it"
```

## 安装

```sh
go get github.com/zeokeel/zeokeel-rig@v0.16.5
```

需要 Go 1.27+，且必须在关闭 cgo（`CGO_ENABLED=0`）下构建。模块恰好只有一个依赖——POSIX shell 解析器 `mvdan.cc/sh/v3`，仅被 `sandbox` 策略层导入；模块根包（`package rig`）保持仅标准库、是 import 图的叶子，并被需要契约的包导入（`sandbox` 策略库、`tools/audit` 与 `tools/catalog` 账本、内部 `tools/internal/background` 辅助包是无依赖叶子）。CI 强制依赖策略、无 cgo 构建与根包 stdlib 约束。

## 仓库布局

```
*.go (root)        package rig: stable extension surface (standard library only)
agent/             streaming ReAct loop, subagent runner/fork, structured output
provider/openai/   OpenAI-compatible streaming provider
sandbox/           agent sandbox policy (roots, commands, env, network, caps)
sandbox/osrunner/  opt-in pure-Go OS runners (bwrap/Landlock, Seatbelt, Windows ACL)
gate/              approval gates
kit/               one-import facade: New/Config/Agent/RunText + contract aliases
session/           in-memory History
tools/             tool registry (name lookup, specs, invoke)
  audit/           modern tool-surface audit gate (reference vs Rig)
  builtin/         read_file/write_file/edit_file/list_dir/glob/grep/bash (+pwsh)
  catalog/         coverage ledger: Static / Legacy / Modern / ServerBound
  editor/          str_replace_editor (view/create/str_replace/insert)
  full/            All(options): builds every static tool in one call
  hosttools/       ask_user/present/plan_review/exit_plan_mode/goal
  image/           read_image (multimodal ToolResult.Parts)
  inspect/         inspect (sessions/events/workspace/stats)
  internal/        internal toolutil helpers (not importable from outside the module)
  jobs/            job_start/output/kill/list/wait (background processes)
  lsp/             LSP stdio client + lsp_* tools
  mcp/             MCP stdio + Streamable HTTP client, tools/resources/prompts
  memory/          memory tool over rig.MemoryStore
  misc/            working_directory, todo_write (+ legacy todo)
  ralph/           ralph (iterate a child agent until DONE/limit)
  report/          report (markdown session report inside the sandbox root)
  review/          code_review / auto_review over rig.Reviewer
  schedule/        schedule_create/list/update/delete
  session/         session_search/trace/event_* over rig.SessionLog
  settings/        settings_get / settings_set
  skill/           skill (list/search/load) + filesystem SkillStore
  subagent/        subagent + subagent_fork (one in-process child agent)
  team/            team_* tools + team_task_* shared task board
  terminal/        terminal_open/send/read/signal/close/list (unix PTY, pipe fallback)
  web/             web_fetch + pluggable web_search
  workflow/        workflow tool (sequential/parallel subagent steps + templating)
  worktree/        enter_worktree / exit_worktree / list_worktrees
examples/          runnable examples: quickstart (kit), agent, custom-tool
docs/              ARCHITECTURE, TOOLS, COVERAGE, AUDIT-REPORT, CONFIG, VERSIONING, RELEASE
scripts/           sync-version.sh + legacy tool-surface audit
.github/           CI, tag-driven release workflow, issue/PR templates, dependabot
Makefile           developer tasks (make help / make ci / make fuzz / make bench)
```

> 此外：`cmd/rigdoc` 从真实构造器生成 [docs/ai/](docs/ai/) 的机器可读工具目录（CI 以 `-check` 防止漂移）；[docs/zh/](docs/zh/README.md) 是中文文档；[AGENTS.md](AGENTS.md) 与 [llms.txt](llms.txt) 是给 AI 使用者的入口。

## 包

| 包 | 说明 |
| --- | --- |
| `rig`（模块根） | 稳定扩展面：消息、流式 chunk、`Provider`、`Tool`、`Gate`、`History`、agent 事件、`Agent`/`AgentSpec`、host/store 接口。仅标准库；需要契约的包都会导入它。 |
| `agent` | 流式 ReAct 循环：模型轮次、并行工具调用、审批 gate、迭代上限、取消、同步事件流；subagent runner、fork 与结构化输出。 |
| `kit` | 单 import 门面：`kit.New(kit.Config{...})` 接好 provider + tools + gate + history + agent；`Agent.Run`/`Agent.RunText`（流式事件、返回聚合文本）；并 re-export 契约类型，使快速开始只需一个 import。 |
| `provider/openai` | OpenAI 兼容流式 provider（SSE、tool-call delta、reasoning、usage、重试）。 |
| `sandbox` | Agent 沙箱策略：允许的根目录、只读、命令允许/拒绝名单、环境变量允许名单、网络 + URL 防护、输出/超时上限、被拒绝的 glob。 |
| `sandbox/osrunner` | **可选、默认关闭**的纯 Go OS 沙箱 runner：Linux bwrap/Landlock、macOS `sandbox-exec`、Windows 受限令牌 + ACL；实现 `sandbox.Runner`，由工具 `WithRunner` 注入。见 [docs/SANDBOX-OS.md](docs/SANDBOX-OS.md)。 |
| `gate` | 审批 gate：`AllowAll`、`DenyAll`、`AllowTools(...)`、`Func(...)`；新增 `Ask(Asker)` 人工审批与 `Review(ToolReviewer)` 模型审前审批（可回退）。 |
| `session` | 内存中的会话 `History`（实现 `HistoryReplacer`）。 |
| `session/compact` | 上下文压缩：`WindowCompactor`（窗口/图片/tool 块裁剪）与 `LLMCompactor`（模型摘要 + 回退）。 |
| `tools` | 工具注册表：按名查找、确定性 specs、并发安全的快照 API。 |
| `tools/full` | `All(options)`：一次构建全部静态工具（想要完整注册表时从这里开始）。 |
| `tools/catalog` | 覆盖账本 + `Check`/`AuditLegacy`/`AuditModern`；发布门禁。 |
| `tools/audit` | 现代工具面审计（reference/mapped/gaps），由测试强制。 |
| `tools/presets` | 工具集预设：`minimal`/`standard`/`all` 过滤（不改 prompt/权限）。 |
| `tools/builtin` | `read_file`、`write_file`、`edit_file`、`list_dir`、`glob`、`grep`、`bash`、`pwsh` —— 经 `builtin.NewWithPolicy` 受沙箱约束。 |
| `tools/editor` | `str_replace_editor`：view/create/str_replace/insert。 |
| `tools/image` | `read_image`：magic-byte MIME，图片放在 `ToolResult.Parts`。 |
| `tools/terminal` | `terminal_*`：真实 unix PTY（pipe 回退）、ANSI 剥离、Ctrl-C、owner 作用域清理。 |
| `tools/jobs` | `job_*`：后台进程，带环形缓冲与退出码、完成通知、owner 作用域清理。 |
| `tools/web` | `web_fetch`（HTTP(S)、重定向上限/大小上限、HTML→文本）与 `web_search`（可插拔 `Searcher`）。 |
| `tools/mcp` | MCP stdio + Streamable HTTP 客户端；tools/resources/prompts/templates 都表示为 `rig.Tool`；HTTP 通知流（GET SSE、Last-Event-ID 恢复、session DELETE）。 |
| `tools/lsp` | LSP stdio 客户端与 `lsp_hover` / `lsp_definition` / `lsp_references` / `lsp_implementation` / `lsp_diagnostics` / `lsp_symbols`；UTF-16 位置校验。 |
| `tools/subagent` | `subagent`/`subagent_fork` 的旧同步构造器（`full.All` 现接 `tools/team` 的异步版本）。 |
| `tools/workflow` | 顺序/并行 subagent 步骤，支持 `{{prev}}`/`{{steps.name}}` 模板。 |
| `tools/team` | 受管 subagent（`team_*`）与 `team_task_*` 共享任务板。 |
| `tools/ralph` | 迭代子 agent 直到 `DONE` 或达到轮数上限。 |
| `tools/skill` | `skill`（list/search/load），基于文件系统 `SKILL.md` 仓库。 |
| `tools/review` | `code_review` / `auto_review`，基于 `rig.Reviewer`（含 `SubagentReviewer`）。 |
| `tools/hosttools` | host UI 接口与内存默认实现：`ask_user`、`present`、`plan_review`、`exit_plan_mode`、`goal`，以及 DSH 的 `get_goal`/`create_goal`/`update_goal`。 |
| `tools/settings` | `settings_get` / `settings_set`，基于 `rig.Settings`。 |
| `tools/memory` | `memory`，基于 `rig.MemoryStore`。 |
| `tools/schedule` | `schedule_*` CRUD，基于 `rig.Scheduler`。 |
| `tools/session` | `session_*` 工具，基于 `rig.SessionLog`。 |
| `tools/inspect`, `tools/report` | 诊断与 Markdown 会话报告。 |
| `tools/worktree` | Git worktree 工具，受沙箱检查。 |
| `tools/misc` | `working_directory`、`todo_write`（+ 旧版 `todo`）。 |
| `tools/ptc` | 可选的 `run_code`（PTC）：模型编写的 Python 3/Node.js 程序在**系统已安装运行时**中执行，内层工具调用走同一个审批 `Gate`；通过 `kit.Config.PTC` 启用。 |
| `examples/quickstart`, `examples/agent`, `examples/custom-tool` | 可运行示例（agent 类示例需要模型凭证）。 |

## 门禁（CI）

正确性是测试结果，不是声明。每次 push 和 pull request 都会运行：

| 门禁 | 强制内容 |
| --- | --- |
| `gofmt`、`build`、`vet` | 格式化与编译 |
| `test`、`test -race`、`-coverprofile` | 行为与数据竞争 |
| 覆盖率下限 | **总语句覆盖率 80%** 且**每个有测试的包函数覆盖率 ≥70%**（只有总数会掩盖测试不足的包） |
| `lint` | staticcheck v0.8.2 与 govulncheck v1.9.0 |
| 跨 OS | windows/darwin/freebsd 交叉编译**并**在 `GOOS` 下 `go vet`（连测试一起做编译检查）；单元测试套件本身只在 Linux 上跑（本仓库没有托管的 macOS/Windows runner） |
| `tools/catalog` + `tools/full` | `catalog.Static` 中每个工具都由 `full.All` 构建、每个构建出的工具都已登记、legacy 名称被构建或标记为 server-bound、无重复 |
| `tools/audit` | 现代 agent 工具面已映射，且没有未登记的缺口 |
| 根 `rig` 包 | 仅标准库**且**是 import 图的叶子（不导入任何 rig 子包） |
| 依赖策略 | **实际构建**的 import 集合恰好是 `mvdan.cc/sh/v3`（解析器）；模块根保持 stdlib-only |
| 无 cgo | 每个包都没有 `CgoFiles`；所有构建/测试都在 `CGO_ENABLED=0` 下运行（race detector 步骤是唯一使用 cgo 工具链的地方） |
| AI 工具目录 | `go run ./cmd/rigdoc -check` 校验 [docs/ai/](docs/ai/) 与真实工具构造器一致 |
| fuzz 冒烟 | 10 个 fuzz 目标（SSE/JSON-RPC/LSP 分帧、shell lexer、URL/path 策略）各跑 5s |
| benchmark 冒烟 | 所有 benchmark 跑一遍，抓 panic/回归 |
| README 快速开始 | README 代码块与 `examples/quickstart/main.go` 一致 |

由 tag 驱动的 `release` 工作流通过 `workflow_call` **复用同一份** CI 工作流，因此打 tag 时跑的正是合并门禁；它还会验证 tag 已签名（`.github/allowed_signers`）、`rig.Version` 与 tag 一致、被打 tag 的提交在 `main` 上，对已发布模块做干净环境的 `go get`/`go mod verify` 冒烟，并从 CHANGELOG 对应小节创建**草稿** GitHub Release。维护者完成发布后复审后，用 `publish-release` 工作流发布草稿（见 [docs/RELEASE.md](docs/RELEASE.md)）。发布 tag 是 annotated 且用 SSH 签名的；`main` 由必需 CI 检查和 `v*` tag ruleset 保护。

## 安全

内置工具执行的是**进程内**策略（不是 OS 沙箱）：文件访问用 `os.Root` 限定根目录；shell 工具默认拒绝控制操作符、命令/进程替换（即使在双引号内）、前置环境变量赋值与路径形式的命令名（可用 `sandbox.WithAllowShellOperators` 显式关闭该检查）；`web_fetch` 会自行解析并固定校验过的公网 IP，关闭 DNS rebinding 型 SSRF——对未实现 `web.HostResolver` 的策略还有 fetcher 侧防护（`web.WithAllowPrivateHosts` 是面向本地目标的显式开关）。`terminal_send` 的每一行都按 shell 命令检查，终端 shell 二进制必须通过 allowlist；shell 策略用同一个共享解析器建模 POSIX 引号、转义与反斜杠-换行续行，因此被续行拆开的命令替换仍会被拒绝。LSP 入站 header 在任何分配前先以 64 KiB 为界。审批 `Gate` 仍是不可逆操作的真实边界。权限模式（`read-only`、`workspace-write`、`danger-full-access`）与审批策略（`ask`/`auto`/`never`）对应 DeepSeek Harness 的模型：它们是工具实现之上的**进程内**围栏，**不是内核边界**。对不可信模型，请把宿主跑在容器或 VM 中并保持审批开启。全部无 cgo（`CGO_ENABLED=0`）。请私下报告漏洞——见 [SECURITY.md](SECURITY.md)。

## 扩展点

1. **Provider** —— 实现 `rig.Provider`（流式），或使用 `provider/openai` 对接任意 OpenAI 兼容网关。
2. **Tool** —— 实现 `rig.Tool`（`Name/Description/Schema/Invoke`）并注册（见 `examples/custom-tool`），或使用 `tools/full.All` 获得开箱即用的完整工具集。
3. **Gate** —— 实现 `rig.Gate` 来决定批准/拒绝工具调用。
4. **History** —— 实现 `rig.History` 以获得持久会话。
5. **Host 接口** —— 实现 `rig.Asker`/`Presenter`/`FilePresenter`/`PlanController`/`GoalStore`/`MemoryStore`/`Scheduler`/`SessionLog`/`Reviewer` 以替换内存默认实现。
6. **PTC** —— `kit.Config{PTC: true}` 会增加 `run_code` 工具：模型编写的 Python 3/Node.js 程序在系统已安装运行时中执行，并通过同一个审批 `Gate` 调用其他工具。不嵌入 JavaScript 引擎。`workflow` 工具仍是 Go 原生的声明式编排（无 JS）。

## 权限模式

Rig 采用与 DeepSeek Harness 相同的粗粒度权限模型：一个沙箱模式加一个审批策略。在 `kit.Config` 中一次性设置（或自行构造 `sandbox.New(sandbox.WithMode(...))` 与 `rig.Gate`）：

| 模式 | 文件变更 | Shell 命令 |
| --- | --- | --- |
| `read-only` | 拒绝 | 拒绝 |
| `workspace-write`（默认） | 仅限工作区根目录内 | 仅限 allowlist 中的命令 |
| `danger-full-access` | 不受限 | 不受限（显式 denylist 仍生效） |

审批策略（`kit.Config.Approval`，或你自己的 `rig.Gate`）：`ask`（默认：内置 gate 放行只读工具、拒绝其余工具；交互式询问需自行提供会提示用户的 Gate）、`auto`（放行所有调用）、`never`（拒绝所有调用）。这些是工具级围栏，不是内核边界：对不可信模型，请把宿主跑在容器/VM 中（例如 Docker，与 OpenHands 的做法一致）并保持审批开启。见 [docs/CONFIG.md](docs/CONFIG.md)。

## 可选 OS 沙箱（v0.14.0）

上面的权限模式是**进程内围栏**。v0.14.0 起新增 **opt-in、默认关闭**的 [`sandbox/osrunner`](docs/SANDBOX-OS.md)：它把要 spawn 的命令包装成平台原生受限形式，只治理文件效果（写入/删除/创建），让 shell/terminal/job/worktree 的写操作被 OS 限制在策略根与平台临时目录内。

| 平台 | 机制 | Enforcement |
| --- | --- | --- |
| Linux | bwrap，或自带的纯 Go Landlock 启动器 | full（老内核 ABI 为 partial） |
| macOS | 系统 `sandbox-exec`（Seatbelt/SBPL，已弃用） | full |
| Windows | Write-Restricted 受限令牌 + 目录 ACL | partial（只约束写/删） |
| 其他 | fail closed（`sandbox.ErrSandboxUnavailable`） | — |

```go
if osrunner.MaybeRunHelper(os.Args) { // Linux/Windows 的隐藏 helper
    return
}
runner, err := osrunner.New(osrunner.Config{})

// 推荐：一次接好默认工具集（bash/pwsh、jobs、terminal、worktree）
tools := full.All(full.AllOptions{Policy: policy, Root: root, SandboxRunner: runner})
// 或 kit 门面：kit.New(kit.Config{..., SandboxRunner: runner})

// 手动组装：builtin.WithRunner / jobs.WithRunner / terminal.WithRunner / worktree.WithRunner
```

`runner == nil` 时行为与 v0.13.x 完全一致；挂接 runner 却没有策略、或 `Confine` 失败时工具 **fail closed**，绝不裸跑。它仍不是容器/VM：网络、读、进程可见性不在承诺内。见 [docs/SANDBOX-OS.md](docs/SANDBOX-OS.md)。

## 与基于 kernel 的产品组合

装配、模块生命周期、依赖注入与预检被刻意**排除**在 Rig 之外。想要“从配置装配模块”的产品自己保留这一层；ZeoKeel 用 `zeokeel-kernel` 实现：

```
consumer product (e.g. ZeoKeel)
  kernel (compile-time module assembly) + product modules + UI
        │ imports
        ▼
ZeoKeel Rig  ← pure toolset (this repo; one parser dependency, cgo-free)
```

该拆分让每个 Rig 消费者免于引入自己可能不需要的模块运行时，同时产品仍能获得 kernel 的预检装配能力。

## 文档

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) —— 分层、请求流、契约、安全说明（[中文](docs/zh/ARCHITECTURE.md)）。
- [docs/TOOLS.md](docs/TOOLS.md) —— 完整工具目录（[中文](docs/zh/TOOLS.md)）。
- [docs/COVERAGE.md](docs/COVERAGE.md) —— 覆盖门禁如何工作、如何新增工具（[中文](docs/zh/COVERAGE.md)）。
- [docs/AUDIT-REPORT.md](docs/AUDIT-REPORT.md) —— 现代工具面审计（reference vs Rig；历史 v0.8/v0.9 快照，当前计数以 `tools/audit` 为准）。
- [docs/CONFIG.md](docs/CONFIG.md) —— 配置约定与默认值（Config struct vs options、model 优先级；[中文](docs/zh/CONFIG.md)）。
- [docs/SANDBOX-OS.md](docs/SANDBOX-OS.md) —— 可选 OS 沙箱 runner：平台矩阵、接入、fail-closed 与限制。
- [docs/VERSIONING.md](docs/VERSIONING.md) —— 语义导入版本与弃用策略（[中文](docs/zh/VERSIONING.md)）。
- [docs/RELEASE.md](docs/RELEASE.md) —— 发布清单与 tag/发布自动化（[中文](docs/zh/RELEASE.md)）。
- [docs/zh/README.md](docs/zh/README.md) —— 中文文档索引（含 [AI-SDK.md](docs/zh/AI-SDK.md) 与 SECURITY/COMPATIBILITY 的中文版）。
- [docs/ai/tools.json](docs/ai/tools.json) · [docs/ai/tools.md](docs/ai/tools.md) —— 机器可读 / 可读的工具 Schema 目录。
- [CONTRIBUTING.md](CONTRIBUTING.md) · [SECURITY.md](SECURITY.md) · [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) · [SUPPORT.md](SUPPORT.md)

## 平台支持

- **Linux**：所有包；完整单元测试套件在 CI 的 Linux 上运行。
- **macOS、FreeBSD**：CI 中交叉编译并做 `GOOS` vet；这些平台上的单元测试目前不在托管 CI 中运行（本仓库没有托管的 macOS/Windows runner —— 已披露的限制）。
- **Windows**：SDK（契约、`agent`、provider、文件/editor/image 工具、MCP/LSP 客户端、`pwsh`、session、store）会交叉编译并做 `GOOS` vet；`bash`、`jobs` 与 `terminal_*` 需要 unix `/bin/sh`，在 Windows 上会返回清晰的配置错误（请用 `pwsh` 工具；本版本未实现 Windows ConPTY）。Windows 运行时覆盖是已披露的缺口，不是声明。

OS 级隔离不是 SDK 的默认职责：模型不可信时，请把宿主进程跑在容器或 VM 中（Docker/Seatbelt/bubblewrap 是部署选择，与 OpenHands、Claude Code 的做法一致）。上文权限模式才是默认的进程内围栏；需要 OS 层文件效果约束时显式使用可选的 [`sandbox/osrunner`](docs/SANDBOX-OS.md)（默认关闭，Linux 可跑真实强制测试，macOS/Windows 只有 cross-compile + `GOOS` vet）。

非 Linux 的运行时覆盖是当前发布线已文档化的限制；强制执行的跨平台门禁是交叉编译加 `GOOS` vet。

## 兼容性与稳定性

- 模块根（`package rig`）是兼容面：同一 module major 版本内只做增量变更；v1.0.0 后的破坏性契约变更通过新的 module major 版本（`/v2` import path）发布，绝不用版本化子目录。
- v0.x 允许破坏性变更；`v1.0.0` 冻结 API。见 [CHANGELOG.md](CHANGELOG.md)。
- Go 1.27+；一个依赖（`mvdan.cc/sh/v3`，POSIX shell 解析器），无 cgo（`CGO_ENABLED=0`）。见 [COMPATIBILITY.md](COMPATIBILITY.md)。
- 发布 tag 为 annotated 且 SSH 签名；`sum.golang.org` 记录已发布模块的校验和（release 工作流会在干净消费者中跑 `go mod verify`）。
- `main` 要求 `test`+`lint` CI 检查、签名提交与线性历史；force-push 与删除被拒绝，`v*` tag ruleset 保护已发布 tag。这是单人维护项目：管理员保留紧急 bypass（`enforce_admins=false`），没有第二评审人，GitHub 提交 “Verified” 需要在账号上注册 SSH 签名公钥。`release` environment 是手动的 `workflow_dispatch` 步骤，不是必需评审人门禁。确切状态与剩余限制见 [docs/RELEASE.md](docs/RELEASE.md)。
- 许可证：从 v0.7.0 起为 MIT；v0.6.0 及更早的 tag 为 Apache-2.0。

## 路线图

- **v0.16.5**（当前 release）：把 Windows 沙箱并发 ACL 修复（v0.16.3）与测试 fake data race 修复（v0.16.4）cherry-pick 到 main 后发布；运行时代码与 v0.16.3 相同，测试与 CI 与 v0.16.4 相同。——跨进程命名 mutex + per-grant marker mutex + `%LOCALAPPDATA%\ZeoKeel\sandbox-acl\<pathHash>.json` 状态文件串行化并合并多 helper 的 workspace grant，cleanup 只撤销自己的 capability ACE，最后一个 grant 退出才恢复原始 descriptor；死 grant 自动清理，状态损坏 fail closed。无公共 API/模型可见变更。
- **v0.16.2**：Windows 子进程控制台窗口不再弹出——所有 `exec.Command` spawn 站点统一调用新增的 `internal/procattr.HideWindow`（Windows 设 `SysProcAttr.HideWindow`，其他平台 no-op），ConPTY 启动信息同时请求隐藏窗口；无公共 API/模型可见变更。
- **v0.16.1**：修复 Windows 沙箱 ACL 探测崩溃（`GetWindowsDirectoryW` 参数顺序写反导致 0xC0000005，所有命令工具/终端不可用）与 Windows `terminal_*` 早退（ConPTY 不可达）；`GetTempPath2W` 增加 `Find` 保护（Windows 10 回退 `GetTempPathW`）。无新增依赖、无新增工具、无破坏性变更。
- **v0.16.0**：Windows `terminal_*` 使用真正的 ConPTY（`CreatePseudoConsole` + `STARTUPINFOEX`/`PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE` + `CreateProcess`），以原生 PowerShell 作为交互 shell；ConPTY 不可用时自动回退到 pipe。
- **v0.15.1**：Windows 沙箱临时目录改用 Windows 自带 temp（`GetTempPath2W`/`GetTempPathW` + 用户 Known Folder 的 `LocalAppData\Temp`），不再依赖被裁剪的环境变量；`CommandEnv` 默认放行 Windows 非敏感定位变量（`TEMP`/`TMP`/`LOCALAPPDATA`/`SystemRoot` 等）；`job_start` 在 Windows 用 shell 基名校验允许列表。
- **v0.15.0**：Windows 命令执行修复——ACL runner 的私有临时目录不再退化到 `C:\WINDOWS`（`RIG_SANDBOX_TEMP`/`TEMP`/`TMP`/`%LOCALAPPDATA%\Temp` 多级回退），`write_file` 父目录与 `glob`/`grep` 目录遍历统一使用平台分隔符；`terminal_*` 在 Windows 默认 `powershell.exe`，`job_start` 在 Windows 通过 pwsh/powershell/cmd 后台运行。无破坏性变更。
- **v0.14.1**：v0.14.0 的 release lint 修复 + macOS Seatbelt 拒绝自检——把 Windows helper 常量移入 windows 构建标签文件（修复 Linux staticcheck U1000）；macOS 探测新增“只读 profile 下写工作区外临时文件必须被拒绝”的自检，否则 fail closed。
- **v0.14.0**：可选的纯 Go OS 沙箱 runner（additive，默认关闭）——
  新增 `sandbox.Runner` 接缝与 `sandbox/osrunner`：Linux bwrap + 自带 Landlock
  启动器、macOS `sandbox-exec`、Windows 受限令牌 + ACL（partial）；功能探测 +
  fail-closed；`tools/builtin`/`tools/jobs`/`tools/terminal`/`tools/worktree`
  新增 `WithRunner`。`runner == nil` 时行为不变。这不是容器/VM 级隔离：只治理
  文件效果。见 [docs/SANDBOX-OS.md](docs/SANDBOX-OS.md)。
- **v0.13.2**：Windows 命令 allowlist 平台语义补丁——
  `Policy.CheckCommand` 在 Windows 上忽略大小写并剥离 `.exe`（`pwsh` 与
  `pwsh.exe` 等价），修复 pwsh/powershell 工具命令被误判 not in the allowlist；
  非 Windows 行为不变。
- **v0.13.1**：OpenAI 兼容 provider 的流截断检测补丁——
  `RequireCompleteStream`（默认关闭，开启后截断流返回 `ErrIncompleteStream`，
  同时 wrap `io.ErrUnexpectedEOF`）；补齐 openai.Config/版本/doc 一致性。
- **v0.13.0**：补齐 SDK 功能缺口（additive）——上下文压缩
  （`session/compact` + `ContextBudget`/`Compactor`/`CompactNow`）、工具结果
  spill、checkpoint 中断恢复、`gate.Ask`/`gate.Review` 审批辅助、LSP
  references/implementation + UTF-16 校验、MCP 通知流、goal round driver、
  `tools/presets`、reasoning effort、terminal/jobs owner 清理。
- **v0.12.1**：文档与 AI 可读性 release（无代码行为变更）——
  中文为主的 README/docs/zh/41 个包中文包文档与注释、`AGENTS.md`/`llms.txt`、
  `docs/zh/AI-SDK.md`、由 `cmd/rigdoc` 生成的 `docs/ai/tools.json|md`，以及
  CI 的 AI 目录/文档计数一致性门禁。
- **v0.12.0**：DSH 能力同步 —— 异步 `subagent`/team 控制与完成通知（`rig.NoticeSource`）、类型化 `schedule_*` 选择器、revision-CAS 的 `get_goal`/`create_goal`/`update_goal`、后台 jobs/terminal/bash（`run_in_background`、promotion、通知）、DSH team 名称（`spawn_teammate`/`list_agents`/`wait_agent`）、可选的 `run_code` PTC（调用系统已安装的 Python/Node 运行时，`kit.Config.PTC`），以及 Go 原生声明式 `workflow`（无 JS 引擎）。81 个静态工具。破坏性变更：subagent 变异步、`present` 改为声明文件（`present_artifact` 保留旧的 artifact 工具）、bash/pwsh 非零退出码是结果。见 CHANGELOG。
- **v0.11.0**：范围纠正 —— 移除未发布的 OS 沙箱层，改为镜像 DeepSeek Harness 权限模型：`read-only` / `workspace-write` / `danger-full-access` 模式加 `ask` / `auto` / `never` 审批策略（进程内围栏；OS 隔离是容器/VM 的部署职责）。保留 POSIX 的 `mvdan.cc/sh` 解析器、无 cgo 契约与已完成的标准库现代化。未发布的 v0.10.1 tag 被取代；见 CHANGELOG。
- **v0.10.0**：OS 沙箱实验（v0.11.0 再移除）；shell 策略改用成熟的 `mvdan.cc/sh/v3` POSIX 解析器（取代手写扫描器；续行、引号与注释即 shell 自身语义）；工具集完成标准库现代化（`slices`、`maps`、`cmp`、内建 `min`/`max`、`errors.AsType`、`strconv`、`slices.Clone/Delete`、`sync.WaitGroup.Go`）；建立无 cgo 的 CI 契约（`CGO_ENABLED=0`）。破坏性变更：模块开始有一个依赖。见 CHANGELOG。
- **v0.9.4**：安全/正确性补丁 —— shell 策略建模 POSIX 续行（关闭 v0.9.3 的残留绕过）、`sandbox.ParseShellCommand` 成为唯一校验+argv 解析器（bash、jobs、terminal 共用）、单个未终止的 LSP header 在分配前受限、文档真话修复（平台/CI 声明、版本、host-interface 图例）与诚实的仓库保护限制。
- **v0.9.3**：发布工程补丁 —— release 工作流中的 tag 签名验证现在同时用 tag *name* 做 `git tag -v`（v0.9.2 ref 恢复修复的后续）。SDK 代码无变化；行为与 v0.9.2 相同。
- **v0.9.2**：发布工程补丁 —— 在签名验证前恢复 annotated tag 对象（SDK 代码无变化；行为与 v0.9.1 相同）。
- **v0.9.1**：安全/正确性热修 —— 拒绝带引号的命令替换与 env/path 劫持、fetcher 侧 SSRF 防护、LSP header 与工具扇出受限、LSP/terminal 路径与 allowlist 强制、取消/gate 错误时历史可重放、team spawn 死锁、发布复用完整 CI 门禁、签名 tag、草稿→发布流程、80%/70% 覆盖率下限、staticcheck+govulncheck、交叉编译 + `GOOS` vet（仅 Linux 运行时；本仓库没有托管 macOS/Windows runner）。
- **v0.9.0**：根包契约（`package rig`）、单 import 的 `kit` 门面、shell 操作符与 SSRF 加固、`tools/internal` 辅助层、CI fuzz/bench/覆盖率门禁、治理与 tag 驱动的发布自动化。
- **v0.8.0**：现代面审计关闭 —— MCP resource templates、`subagent_fork`、结构化子输出、74 个静态工具、`tools/audit` 的 `missing=0`。
- **v0.7.0**：现代工具面 —— `skill`、team 任务板（`team_task_*`）、完整 goal 生命周期（`goal action=update`）、代码评审（`code_review`/`auto_review`）、`tools/audit` + `catalog.Modern`；自此 tag 起为 MIT。
- **v0.6.0**：legacy agent 工具面补齐（host 工具、settings、memory、调度、worktree、session/inspect/report、ralph、旧名称）以及 `tools/catalog` 覆盖门禁。
- **v0.5.0**：真实 unix PTY terminal、MCP Streamable HTTP 传输、进程内编排（`agent.NewRunner`、`subagent`、`workflow`、`team_*`）。
- **Next**：MCP SSE 订阅/OAuth、workflow DSL（条件/循环）、Windows ConPTY、PTY resize、跨进程 subagent、压缩/SQLite 历史。

## License

MIT。见 [LICENSE](LICENSE)。
