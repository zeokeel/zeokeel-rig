# ZeoKeel Rig

[![CI](https://github.com/zeokeel/zeokeel-rig/actions/workflows/ci.yml/badge.svg)](https://github.com/zeokeel/zeokeel-rig/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zeokeel/zeokeel-rig.svg)](https://pkg.go.dev/github.com/zeokeel/zeokeel-rig)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/zeokeel/zeokeel-rig)](https://github.com/zeokeel/zeokeel-rig/releases)

> [English README](README.en.md) · 中文是唯一正式文档语言；机器可读工具目录见 [docs/ai/tools.json](docs/ai/tools.json)。

**个人 agent 客户端的纯 Go SDK。** 一次 import 拿到：兼容 OpenAI 的 provider、流式
ReAct 循环、一套带权限围栏的工具、DSH 对齐的四档权限预设与两档审批、会话历史与持久化、NDJSON 事件流、
成本计量，以及可选的编排工具。当前 release **v0.23.4**，要求 **Go 1.27+**，
构建与测试一律 `CGO_ENABLED=0`，依赖恰好一个（`mvdan.cc/sh/v3`，仅 `sandbox` 策略层
使用的 POSIX shell 解析器）。

## 这是什么

Rig 是你写一个桌面 / CLI / 本地 agent 客户端时，放在 `main()` 后面的那一层：

- **单用户、单进程**。会话、记忆、花费、快照都是"这台机器上这个用户的"；
  没有账号体系，也没有多租户隔离。
- **契约优先**。模块根包 `rig` 只依赖标准库，可以只拿走一个工具、只拿走 `gate`、
  只拿走 `provider`，不必拖进整棵子树。
- **模型可见文案是接口**。工具的 `Description` / JSON Schema / 错误字符串保持英文，
  中文文档只解释用法，不改写它们。

## 这不是什么

| 不做 | 说明 |
| --- | --- |
| UI | 没有 TUI、GUI、聊天前端组件；`present` / `ask_user` 通过你实现的宿主接口回调。 |
| 进程宿主 | 不托管模型进程，不做守护、托盘、自动更新、崩溃上报。 |
| 模块装配 | 不做编译期模块注入或插件运行时；那是产品自己保留的一层。 |
| 账号鉴权 | 没有登录、配额、云端同步、密钥托管。 |
| 内核级沙箱 | 内建围栏是**进程内**策略校验（见[安全](#安全)）；内核边界是 opt-in 的 `sandbox/osrunner`。 |

## 特性

**agent 循环**

- 流式 ReAct：文本/推理增量、并行工具调用、取消与超时、可插拔 `Provider`。
- 轮数护栏：`kit` 默认 **200 轮**（`kit.DefaultSafeMaxIterations`，成本总闸）；
  `agent` 层默认不限（`MaxIterations=0`），显式负数 = 真的不限，`>0` = 显式上限。
- 抽风守卫：连续 3 轮完全相同的工具调用注入提示；连续 5 轮或同轮 ≥3 个相同调用
  直接返回 `rig: repeated call blocked`。
- 上下文压缩**默认开启**：模型窗口 × 0.6、下限 16k，默认 `WindowCompactor`
  （保留最近 4 条工具结果原文）；压缩失败降级继续，而不是中止回合。
- 回合检查点（`CheckpointStore`）与被中断回合的恢复。

**工具面**（口径必须分清，详见 [docs/TOOLS.md](docs/TOOLS.md)）

| 口径 | 数字 |
| --- | --- |
| `kit.New(kit.Config{})` 零配置**注册** | **40**，其中 **16** 个初始 active |
| 同上、接 `Config.Presenter` | 41 注册 / 17 active |
| 全接线（Linux、git 仓库、`PTC`） | 50 注册 / 17 active |
| `full.All(full.AllOptions{Root, Policy})` | **81**（`SandboxGrants: true` 时 **83**） |
| `full.All(full.AllOptions{})`（无 `Root`/`Policy`） | **78** |
| 账本 `catalog.Static` / `presets["all"]` | **83** |
| 预设 personal / standard / minimal | **55 / 73 / 8** |
| 账本 Legacy / Modern / ServerBound / DocumentedGaps | **58 / 62 / 7 / 0** |

`docs/ai/tools.json` 有 **85** 条 = 83 个静态工具 + opt-in 的 `run_code` + 未构造的
`spill_read`；按类别是 orchestration 28、shell 13、state 12、host 8、files 8、
session 6、other 7、web 2、programmatic 1。

- **动态工具加载默认开启**：初始只把**核心 13**（`read_file write_file edit_file
  apply_patch list_dir glob grep search bash todo_write working_directory read_image
  str_replace_editor`）+ `tool_search` + `memory` + `skill` 发给模型（零配置 16 个），
  其余工具注册但不 active，模型用 `tool_search` 自助激活。
- 工具按用途分组：文件与编辑、shell 与进程（`bash`/`pwsh`、`job_*`、`terminal_*`）、
  检索（`search`/`glob`/`grep`）、web（`web_fetch`/`web_search`）、会话与诊断、
  记忆与调度、worktree、编排、PTC。

**权限与审批**

- **四个权限档**（`Config.PermissionPreset`，DSH 对齐）：`read-only` /
  `workspace-write`（默认）/ `danger-full-access` / `auto`（全权 + 初审模型预审）
  （见[权限模式与审批](#权限模式与审批)）。
- **两档审批**：`ask`（默认，逐调用问人）/ `never`（全拒）；`Approval: auto` 是
  "不问人、全放行"。`ask` 不做命令级风险分类——`rm -rf /` 与普通写操作一样问你。
- 不接 `Asker` 时不会"自动有人问"：退化为 `gate.PersonalAskWithoutAsker()`，
  只读照放、其余拒绝并明确告诉模型"没有可用的审批通道"。
- 审批**默认不超时**（一直等，只有取消本轮才撤回）；要上界设 `AskTimeout`。
- 逐命令提权：`sandbox_permissions:"require_escalated"` + `justification`，
  只在**人工批准**后让这一条命令不被 OS runner 包装，策略不变。
- **没有"保护路径"这类装配**：kit 不追加 `.git`/`~/.ssh`/`~/.aws` 之类的保护项，
  策略层也没有这一层（相关导出符号已删除）。要根内局部只读，自己构造
  `sandbox.Policy` 并声明可写根的 `ReadOnlySubpaths`；要"按模式看住路径"用
  `sandbox.WithHidden`（它真的参与判定）。

**沙箱**

- 进程内围栏（默认）：**只做可写根围栏**——可写根与根内只读子路径、只读模式、
  命令允许/拒绝名单、环境白名单、网络开关、输出与超时上限、运行期可写授权、
  结构化拒绝审计（`sandbox.Denial`）。
- `sandbox/osrunner`（**opt-in、默认关闭**）：Linux bubblewrap 或纯 Go Landlock 启动器、
  macOS `sandbox-exec`、Windows 受限令牌 + ACL；纯 Go、无 cgo、零新增依赖、fail-closed。
- 沙箱内命令免审批（`AutoAllowSandboxedCommands`，默认开）：已围栏化的 shell 命令
  不再弹窗（围栏生效时不看命令文本），但仍写 `approval.response`
  （reason=`sandboxed`）供 UI 与审计。

**会话与持久化**

- 内存历史（默认）、目录 NDJSON（`session/store`：一会话一文件、崩溃自愈）、
  SQLite（`session/sqlite` 独立子模块，**未发布**）、聚合契约 `storage`（**未发布**）。
- `History` / `SessionStoreDir` / `SessionStore` 三者互斥；`Shutdown(ctx)` 统一收尾
  在飞 Run、事件、`Storage` 与后台任务。

**编排**

- `team`：`spawn_teammate` / `list_agents` / `wait_agent` / `interrupt_agent`、异步
  `subagent` / `subagent_fork`、`team_task_*` 共享任务板。
- `workflow`：Go 原生声明式顺序/并行步骤（`{{prev}}` / `{{steps.name}}` 模板，无 JS 引擎）。
- `ralph`：迭代子 agent 直到 `DONE` 或触上限；`review`：`code_review` / `auto_review`。

**provider**

- `provider/openai`（SSE、tool-call delta、reasoning、usage、重试）、
  `provider/local`（OpenAI 兼容但"不完全一样"的共享基座）、
  `provider/ollama`（默认 `http://127.0.0.1:11434`）、
  `provider/lmstudio`（默认 `http://127.0.0.1:1234`）。

**可观测性**

- `events`：版本化 NDJSON 事件线（`Envelope` v1、`Emitter`、`Replay`），一个 Run 一条流。
- `costmeter`：会话级 token/金额计量（纳美元整数）、预算告警、`Summary`/`Snapshot` 快照；
  `modelprice` 提供每百万 token 价目表（估算用，不是账单）。
- 审批事件（`approval.request` / `approval.response`）、沙箱拒绝审计、`Preflight()` 自检。

## 环境要求与安装

- **Go 1.27+**（`go.mod` 声明 `go 1.27`）。
- **`CGO_ENABLED=0`**：构建与测试都在无 cgo 下运行。
- **一个依赖**：`mvdan.cc/sh/v3`（POSIX shell 解析器，仅用于命令级策略判定）。
- 根包 `rig` **只依赖标准库**，可以单独拿走。

```sh
export CGO_ENABLED=0
go get github.com/zeokeel/zeokeel-rig@v0.26.0
```

在本仓库内开发时不要 `go get`，直接：

```sh
export PATH=$PATH:/usr/local/go/bin   # 若 Go 不在 PATH
export CGO_ENABLED=0
go build ./...
go test ./...
```

`session/sqlite` 是**独立子模块**（自带 `go.mod`，引入 `modernc.org/sqlite`），主模块
永不 import 它；它随下一版本发布，现阶段按[未发布](#接下来)看待，见
[docs/sqlite.md](docs/sqlite.md)。

## 快速开始

下面这段代码就是 [`examples/quickstart/main.go`](examples/quickstart/main.go) 的原文：
一次 import、一个构造函数。CI 只编译示例（代码门禁）；README 与示例是否同步由本地
`make quickstart` 检查，改了一边记得改另一边。

```go
// Command quickstart 是用 Rig SDK 构建的最小完整 agent：kit 门面在一次 import 中
// 装配好兼容 OpenAI 的 provider、带沙箱策略的完整工具集、两档审批门禁（只读免问、
// 其余问人）以及内存历史。
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

	// 一次 import、一个构造函数：kit.New 默认使用完整工具集（再经 personal 预设与
	// 零配置体检裁剪），沙箱只做**可写根围栏**（默认限定在工作目录、不联网）。
	// 策略层不设命令白名单（空 CommandAllowlist 时允许任意程序，否则 go test 这类
	// 命令根本跑不了），所以边界是审批而不是命令分类：approval=ask（默认）只有
	// 两档——只读工具免问，其余调用（写文件、bash/pwsh……）一律问 Config.Asker；
	// 默认不超时（一直等），没有 Asker 时 fail-closed 拒绝并说明"没有可用的审批
	// 通道"。approval=never 全拒，approval=auto 全放行。
	// 默认**不做**命令风险分类，也**不硬拒**危险命令；要这一层就用 Gate 字段显式
	// 叠加 gate.CommandRiskGate。
	// 想一次定好权限面用 PermissionPreset（read-only / workspace-write /
	// danger-full-access / auto）；需要扩大能力时传入 Config.Asker、Approval 或
	// CommandAllowlist。
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

运行它：

```sh
export OPENAI_API_KEY=sk-...
export CGO_ENABLED=0
go run ./examples/quickstart "列出这个目录里的文件并总结"
```

- 接 OpenAI 兼容网关：填 `Config.BaseURL`（例如 `https://api.deepseek.com/v1`）、
  `Config.APIKeyEnv`（默认 `OPENAI_API_KEY`）与 `Config.Model`。
- 零配置的 `kit.New` 是**可用但只读**：`Approval` 默认 `ask` 且没有 `Asker`，
  写文件与所有非只读命令都会因为"没有可用的审批通道"被拒绝（只读工具照放）。
  策略层**不设命令白名单**（空 `CommandAllowlist` 时允许任意程序，否则 `go test`
  这类命令根本跑不了），安全性由人工审批负责（**不做**命令风险分类）；默认也不
  联网。要跑"改代码 → 跑测试 → 修好"的闭环，见
  [接上 Asker](#接上-asker做人工审批) 或 [不需要交互](#不需要交互approvalauto)。
- 没有 API key 时程序仍可构建，第一次 `Run` 才报 provider 错误。

## 核心概念

### 三种组装方式

| 方式 | 适合 | 入口 |
| --- | --- | --- |
| **`kit`** | 大多数个人客户端；默认值与安全姿态已经为你选好 | `kit.New(kit.Config{})` |
| **`full.All` + `agent`** | 需要 Host/Runner/Searcher/MCP/LSP/评审器等完整能力 | `full.NewRuntime(full.AllOptions{...})` → `agent.New(rig.AgentSpec{...})` |
| **按包** | 只要一个工具、一个 gate、一个 provider，或自己写循环 | `tools.NewRegistry(...)`、`builtin.*`、`gate.*` |

三者共享同一套类型（`kit` 里的类型都是根包别名，见 `kit/aliases.go`），所以可以混用：
先用 `kit` 起步，需要更多能力时把值原样搬进 `agent.AgentSpec`。

`kit` 是薄门面而不是框架：它只做默认值与接线，不做业务逻辑。`Config.Tools` 非 nil 时，
你给的就是最终工具集——"零配置体检"不再运行，预设也只在**显式**设置 `ToolPreset` 时才过滤
（空值不会被静默裁剪，那是宿主自己的选择）。

### 工具注册与动态加载

工具**不会自动进入 agent loop**：必须显式交给 `agent.AgentSpec.Tools`，或使用
`kit.New`（默认走 `full.All`，再按预设与"零配置体检"裁剪）。

零配置注册 **40** 个是怎么来的：personal 预设 **55** 个名字 − 没有 tracker 的 `undo`(1)
− 零配置体检摘掉的 14 个（39→40 是因为 `AllowNetwork` 三态、默认开，`web_fetch`
不再被摘）。被摘掉的都是"没有宿主依赖就永远无法成功"的工具——留着一个必然报错的
工具只会诱导模型重试：

| 被摘掉 | 补回来的条件 |
| --- | --- |
| `web_search` | 接 `Config.Searcher`（+1） |
| 5 个 `session_*` | 接 `Config.SessionLog`（+5） |
| `get_goal` / `create_goal` / `update_goal` / `plan_review` / `exit_plan_mode` | `kit` 无注入入口，恒移（用 `full.All` 的 `Host`） |
| `present` | 接 `Config.Presenter`（+1 注册，且**立刻**进初始 active 集 → 17） |
| `ask_user` | 接 `Config.Asker`（+1） |
| `pwsh` | 仅在 Windows 注册 |

`web_fetch` 现在**默认注册**（`AllowNetwork` 默认开）；显式 `kit.Bool(false)` 才会把它
连同联网能力一起关掉——网络不在沙箱承诺内，防护交给沙箱围栏与审批。

全部叠加（Linux、git 仓库、`PTC`）= **50 注册 / 17 active**。注意 `kit` 里没有
`subagent` / `spawn_teammate`：个人客户端默认不提供多智能体编排，需要时用
`standard`/`all` 预设 + `full.All(AllOptions{Runner: ...})`。

**动态加载**让"注册 40 个"不等于"每回合都发给模型 40 个"：`Config.ToolDiscovery`
（`nil` = 开）开启时，初始只发核心 13 + `tool_search` + `memory` + `skill`。模型用
`tool_search` 自助激活：

| 参数 | 作用 |
| --- | --- |
| `{}` | 列出全部可激活工具 |
| `{"query":"git"}` | 按名字/描述筛选 |
| `{"activate":["apply_patch"]}` | 激活并立刻可调用 |

激活是**纯增量、按会话**的：请求里的 `tools` 按激活顺序追加，新工具只出现在末尾，
不重排、不重建 system prompt（system prompt 里不含工具描述）。不激活时 `tools` 数组
逐字节稳定，provider 的 prefix cache 一直命中。关闭它用
`off := false; kit.Config{ToolDiscovery: &off}`（回到"全部 active"）；自定义初始集合用
`Config.InitialActiveTools`（`tool_search`/`memory`/`skill` 强制并入）。

> ⚠️ 手工组装时**必须**给 `Root` 或带 roots 的 `Policy`：
> `full.All(full.AllOptions{})` 只有 **78** 个工具，`apply_patch`/`search`/`undo`
> 直接不构造，文件工具全部返回 `no roots configured`。

### 权限模式与审批

权限模式（`sandbox.Mode`）决定"能碰什么"，审批（`kit.ApprovalMode`）决定"谁来点
同意"。两者独立，DSH 对齐后**沙箱只做可写根围栏**：

| 模式 | 含义 |
| --- | --- |
| `read-only` | 拒绝文件修改与 shell 执行；唯一可写的位置是 `/dev/null`（**没有**可写 `/tmp`）。 |
| `workspace-write`（默认） | 可写面 = `Roots()` ∪ `AdditionalWritableRoots()` ∪ **私有** `/tmp`（`$TMPDIR`）。 |
| `danger-full-access` | 不包装：OS runner 的 `Enforcement` 是 `none`，只应与显式/自动审批搭配。 |

四个产品级档位（`Config.PermissionPreset`）就是把上面两个旋钮捆好：

| 档位 | 组合 | 一句话 |
| --- | --- | --- |
| `read-only` | 只读 + `ask` | 不能改文件、不能执行命令，需要审批的调用问你 |
| `workspace-write`（默认） | 工作区可写 + `ask` | 读免问，写与可变命令问你 |
| `danger-full-access` | 全权 + `auto` | 全权且不问人 |
| `auto` | 全权 + `ask` + `ToolReviewer` | 全权，先由初审模型判断，拿不准再问你 |

`ask`（默认，等价 `gate.PersonalAsk`）只有两层：

1. **只读工具免问**——`read_file` / `glob` / `grep` / `sandbox_status` 之类直接执行；
2. **其余问人**——写文件、`bash`/`pwsh`、后台任务，**包括 `rm -rf /` 这类危险命令**，
   都走你注入的 `Config.Asker`（`rig.Asker`）。**默认不设超时**：人多久不回就等多久，
   只有取消本轮才撤回；要上界设 `Config.AskTimeout`。

**没接 `Asker` 时不会"自动有人问"**：`kit` 退到 `gate.PersonalAskWithoutAsker()`，
需要审批的调用一律拒绝并明确告诉模型"没有可用的审批通道"。`Approval: "auto"` **就是**
`AllowAll`（全放行，与 DSH 的 `approval=never` 同义）；`"never"` 全拒。

默认门禁**不做**命令风险分类，也**不硬拒危险命令**——谁能批准，谁就有权决定。
`gate.CommandRiskGate` 仍然完整可用，但要自己显式叠在**最外层**：

```go
g := gate.CommandRiskGate(gate.PersonalAsk(gate.Ask(myAsker)))
```

它不再是任何默认预设的一部分（规则表的覆盖率靠不住）。

### 沙箱

两层，别混：

| 层 | 是什么 | 何时用 |
| --- | --- | --- |
| 进程内围栏（默认） | 策略层校验：路径解析（可写根围栏）、命令允许/拒绝名单、网络、超时、输出上限。约束的是**配合的模型**，不改变进程权限。 | 总是开启。 |
| `sandbox/osrunner`（opt-in） | OS 级围栏 runner：Linux bubblewrap/Landlock、macOS `sandbox-exec`、Windows 受限令牌 + ACL。`Confine` 失败即拒绝执行（fail-closed）。 | `runner, err := osrunner.New(...)` 后注入 `kit.Config.SandboxRunner` 或 `full.AllOptions.SandboxRunner`。 |

关键行为：

- **沙箱内的命令免审批**（`AutoAllowSandboxedCommands`，`nil` = 开）。判据全部满足才
  免问：工具是 shell 类、runner 非 nil 且自报能强制、策略不是 `danger-full-access`。
  **不看命令文本**。免问仍写一条 `approval.response`（decision=allow、
  reason=`sandboxed`）；文件工具不经 OS runner，照旧问人。
- **逐命令提权**（`require_escalated`）：沙箱能挡的是路径与 syscall，而真实软件还会写
  注册表、keychain 这类"不是路径"的资源。`bash`/`pwsh` 接受
  `sandbox_permissions:"require_escalated"` + `justification`（≥10 字符），
  **仅在人工批准后**让这一条命令不被 OS runner 包装；策略、workdir、超时、
  输出上限一律不变；`auto`/`never` 档拒绝。宿主通过 `builtin.WithEscalator` 或
  `full.AllOptions.Escalator` 注入。
  （它和早年删除的 `sandbox/escalate` 提权协议包不是一回事：那个包已整包移除，
  回归会被守护测试拦住。）
- **运行期授权**：`sandbox_status`（只读诊断）与 `request_sandbox_access`（申请会话级
  可写授权）默认**注册但不 active**；授权预算默认 `sandbox.DefaultGrantBudget` = 5，
  审批由宿主 Gate 决定：`ask` 档下会问你，`auto` 档直接放行。
- `Preflight()` 回答"现在到底有没有 OS 级隔离、可写面是什么、为什么没有"。

### 会话与持久化

| 选择 | 形态 | 状态 |
| --- | --- | --- |
| `History`（默认 nil → `session.NewMemory()`） | 进程内，按 `SessionID` 隔离 | 可用 |
| `SessionStoreDir` | 一会话一 NDJSON 文件，可 `cat`，崩溃自愈 | 可用 |
| `SessionStore`（`*store.Dir`） | 宿主自己持有单实例，供"最近会话"侧栏 `List`/`Load` | 可用 |
| `storage` 聚合契约 | `History`/`Checkpoints`/`Memory`/`Spill`/`Close` 一次交给 SDK | **未发布** |
| `session/sqlite` | 同一份契约的 SQLite 实现（独立子模块） | **未发布** |

- 三者互斥：`History`、`SessionStoreDir`、`SessionStore` 同时设置会报错；同一目录只能由
  **一个** store 实例写（不做跨进程锁）。
- `session/store` 是 NDJSON，零依赖，单进程足够；一旦要回答"跨会话"的问题（上周我提过
  什么、这次发布花了多少钱、这回合有哪些工具事件），就该换 SQLite——见
  [docs/sqlite.md](docs/sqlite.md)。
- 关停一律走 `Agent.Shutdown(ctx)`：依次收尾在飞 Run → 事件 → `Storage` → 后台任务。

## 使用指南

下面的片段都用真实 API，写之前按 `doc.go` 与源码核对过。**宁可少而准**——需要完整
字段列表时看 [docs/CONFIG.md](docs/CONFIG.md)。

### 接上 Asker：做人工审批

`rig.Asker` 只有一个方法，你在自己的 UI 里实现它即可：

```go
type uiAsker struct{ /* 你的窗口/终端句柄 */ }

// Ask 返回用户的选择。options 是门禁给的候选（例如 allow / allow-always / deny）。
func (a *uiAsker) Ask(ctx context.Context, question string, options []string) (string, error) {
	return a.prompt(ctx, question, options) // 你自己的人机交互
}

asker := &uiAsker{}
a, err := kit.New(kit.Config{
	Model:    "gpt-4o-mini",
	Root:     workdir,
	Asker:    asker,
	Approval: kit.ApprovalAsk, // 默认值，写出来是为了让姿态显式
	// 想换提示文案 / 超时：
	Gate: gate.PersonalAsk(gate.Ask(asker, gate.WithAskTimeout(10*time.Minute))),
})
if err != nil {
	return err
}
```

`ask_user` 工具也依赖同一个 `Asker`——不接它，这个工具不会注册。

### 不需要交互：`ApprovalAuto`

```go
a, err := kit.New(kit.Config{
	Model:    "gpt-4o-mini",
	Root:     workdir,
	Approval: kit.ApprovalAuto, // 不问人、全放行（DSH 的 danger-full-access 档）
	Mode:     sandbox.ModeWorkspaceWrite, // 围栏仍由 Mode 表达
})
```

`ApprovalAuto` 就是 `gate.PersonalAuto()`，也就是 `AllowAll()`：`rm -rf /`、`mkfs`、
`curl … | sh` 都会执行——这是"不问人"的准确含义。要保留拦截请用 `ask` 档（由人
决定），或自己叠 `Gate: gate.CommandRiskGate(gate.AllowAll())`。围栏（能写哪）仍由
`Mode` 决定，请先读[安全](#安全)。

### `full.All` 手动装配

需要 `Host`/`Runner`/`Searcher`/MCP/LSP 时用这条路。后台句柄要用 `NewRuntime` 取回来，
否则后台命令会比进程活得久：

```go
policy := sandbox.New(
	sandbox.WithRoots(root),
	sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	sandbox.WithCommandAllowlist("git", "go", "ls", "cat"),
)

prov, err := openai.New(openai.Config{DefaultModel: "gpt-4o-mini"})
if err != nil {
	return err
}

rt, err := full.NewRuntime(full.AllOptions{
	Root:       root,
	Policy:     policy,
	SessionLog: sessionLog, // 有它才会出现 session_* 工具
	Searcher:   searcher,   // 有它才会出现 web_search
})
if err != nil {
	return err
}
defer rt.Close() // 关掉后台 bash 与 PTY 会话

a, err := agent.New(rig.AgentSpec{
	Provider: prov,
	Model:    "gpt-4o-mini",
	Tools:    rt.Tools,
	Gate:     gate.PersonalAsk(gate.Ask(asker)),
	History:  session.NewMemory(),
})
```

写类工具必须拿到**同一个** `Policy`：`apply_patch` 需要 `patch.WithPolicy(policy)`，
否则只读模式对它失效（`full.All` 内部已接好，手动拼装时要自己接）。

### 事件流与 NDJSON 回放

```go
log, err := os.Create(filepath.Join(dataDir, "events.ndjson"))
if err != nil {
	return err
}
defer log.Close()

a, err := kit.New(kit.Config{
	Model:       "gpt-4o-mini",
	Root:        workdir,
	EventWriter: log, // 一个 Run 一条流，seq 在同一 writer 上连续
})
```

回放（`events` 是版本化线格式，未知 `Kind` 必须透传）：

```go
f, err := os.Open(filepath.Join(dataDir, "events.ndjson"))
if err != nil {
	return err
}
defer f.Close()

err = events.Replay(f, func(env events.Envelope) error {
	fmt.Printf("[%d] %s @%s\n", env.Seq, env.Kind, env.Time.Format(time.RFC3339))
	return nil
})
```

进程内事件是另一套：`Agent.Run` / `RunText` 的 `emit func(kit.Event)` 回调，类型是
`kit.EventTextDelta` / `EventToolCall` / `EventToolResult` / `EventUsage` 等。别把两者混用。

### 成本计量

```go
meter := costmeter.New(costmeter.WithBudget(2.00)) // 2 美元预算告警
a, err := kit.New(kit.Config{
	Model:         "gpt-4o-mini",
	Root:          workdir,
	Meter:         meter,
	MaxRunCostUSD: 2.00, // kit 侧硬上限：超限以明确错误收尾并取消本次 Run
})
if err != nil {
	return err
}

// 跑完之后：
s := meter.Session()
fmt.Printf("$%.4f / %d requests / %d tokens（估算=%v）\n",
	s.CostUSD, s.Requests, s.PromptTokens+s.CompletionTokens, s.Estimated)
```

金额内部用**纳美元整数**累计，`Summary.CostUSD` 只是展示用浮点；`Summary.UnpricedModels`
非空说明有模型没查到价目（价目表会过时，`modelprice` 是估算不是账单）。`MaxRunCostUSD`
与 `MaxIterations` 是两种护栏：一个管账单，一个管模型抽风。

### 子代理与团队

`spawn_teammate` / `subagent` 是**异步**的：发出去之后用 `wait_agent` / `team_task_*`
收结果，完成通知会注入下一步。要给它们一个执行器：

```go
// 1) 子 agent 复用父的 provider / gate；MaxDepth=1 表示不再套娃。
childRunner, err := agent.NewRunner(rig.AgentSpec{
	Provider: prov,
	Model:    "gpt-4o-mini",
	Gate:     gate.PersonalAsk(gate.Ask(asker)),
}, agent.RunnerOptions{MaxDepth: 1, MaxConcurrent: 2})
if err != nil {
	return err
}

// 2) 把 runner 交给 full：spawn_teammate / subagent / workflow / ralph 一并出现。
rt, err := full.NewRuntime(full.AllOptions{Root: root, Policy: policy, Runner: childRunner})
if err != nil {
	return err
}
defer rt.Close()
```

`Runner` 为 nil 时这些工具仍会构造，但调用会返回可读的配置错误——不会 panic。
子 agent 的工具集由传给 `NewRunner` 的 spec 决定（默认会剔除编排类工具，防止无限套娃）。

### MCP 与 LSP 客户端

Rig 是**客户端**：把外部 MCP server 的工具接进来，把 LSP 的语义能力给模型。

```go
mcpClient, err := mcp.Start(ctx, mcp.Config{
	Command: "npx",
	Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", root},
})
if err != nil {
	return err
}
defer mcpClient.Close()

lspClient, err := lsp.Start(ctx, lsp.Config{
	Command:    "gopls",
	LanguageID: "go",
	Dir:        root,
})
if err != nil {
	return err
}
defer lspClient.Close()

rt, err := full.NewRuntime(full.AllOptions{
	Root:       root,
	Policy:     policy,
	MCPClients: []*mcp.Client{mcpClient},
	MCPPrefix:  "mcp__", // 默认值；工具名是 mcp__<server>__<tool>
	LSP:        lspClient,
})
```

MCP 工具名随 server 动态出现，所以它们不在静态账本（`catalog.Static`）里——这正是
`catalog.ServerBound` 存在的理由。

### 持久化与关停

```go
a, err := kit.New(kit.Config{
	Model:           "gpt-4o-mini",
	Root:            workdir,
	SessionStoreDir: filepath.Join(dataDir, "sessions"), // 一会话一 NDJSON
	Storage:         externalStore,                      // 可选：Shutdown 最后收尾
})
if err != nil {
	return err
}
defer a.Shutdown(context.Background())

// 宿主自己列"最近会话"时必须用同一个实例（同一目录单实例写）：
sessions := a.SessionStore()
metas, err := sessions.List(ctx, store.Filter{Limit: 20})
for _, m := range metas {
	fmt.Println(m.ID, m.Title, m.UpdatedAt)
}
```

`Shutdown` 依次收尾在飞 Run、事件写入、`Storage`、后台任务。宿主自己传入的
`SessionStore` **不在**自动关闭范围内（同一目录单实例，别的组件可能还在用）；
要一起收尾就在 `Config.Storage` 里登记一次。

## 配置速查

`kit.Config` 只有 `Model` 必填，其余零值都有文档化默认。完整字段、优先级与生效规则见
[docs/CONFIG.md](docs/CONFIG.md)。

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `Model` | —（必填） | 发给 provider 的模型名。 |
| `Profile` | `""` | `careful`/`personal`/`local`，只填你没显式设置的 `Approval`/`ToolPreset`/`ProviderKind`/`MaxIterations`。 |
| `Provider` / `ProviderKind` | `nil` / `openai` | 自定义 provider，或从零构造时用 `openai`/`ollama`/`lmstudio`/`local`。 |
| `BaseURL` / `APIKey` / `APIKeyEnv` | 空 / 空 / `OPENAI_API_KEY` | OpenAI 兼容网关。 |
| `Root` | `"."` | 工作区根，策略与根感知工具都用它。 |
| `Mode` | `workspace-write` | 权限模式（`read-only`/`workspace-write`/`danger-full-access`）。 |
| `Approval` | `ask` | 审批档（`ask` 问人 / `auto` 全放行 / `never` 全拒）。 |
| `PermissionPreset` | `""`（不选） | 四档预设：`read-only`/`workspace-write`/`danger-full-access`/`auto`；一次性设置 `Mode`+`Approval`。 |
| `ToolReviewer` | `nil` | `auto` 档的初审模型：放行即执行，拒绝或失败回退到问人。 |
| `AskTimeout` | `0`（一直等） | 审批等待上界；`0` = 不超时，只有取消本轮才撤回。 |
| `Gate` | `nil` | 非 nil 时完全接管审批（`PermissionPreset` 退化为 `custom`）。 |
| `Asker` | `nil` | 非 nil 才有"问人"通道，也才会注册 `ask_user`。 |
| `ToolPreset` | `personal` | `minimal`/`personal`/`standard`/`all`。 |
| `ToolDiscovery` | `nil`＝开 | 动态工具加载；`kit.Bool(false)` 关闭。 |
| `InitialActiveTools` | `nil`（核心集） | 覆盖初始 active 集；`tool_search`/`memory`/`skill` 强制并入。 |
| `MaxIterations` | `0` → 200 | 负数 = 真的不限，`>0` = 显式上限。 |
| `ContextBudget` | `0` → 窗口×0.6，下限 16k | 负数关闭自动压缩。 |
| `Compactor` | `WindowCompactor` | 需要更好摘要时换 `compact.LLMCompactor`（多一次模型调用）。 |
| `MaxRunCostUSD` | `0`（不限） | 单次会话花费硬上限，超限取消本次 Run。 |
| `Meter` | `nil` | 接 `costmeter.Meter` 才有会话级金额统计。 |
| `History` / `SessionStoreDir` / `SessionStore` | `nil`（内存） | 三者互斥。 |
| `EventWriter` | `nil` | 非 nil 时写 NDJSON 事件流。 |
| `Instructions` / `Environment` | `nil` | AGENTS.md 层级 + `<environment>` 块进 system prompt（只 Load 一次，否则打掉 prompt 缓存）。 |
| `SandboxRunner` | `nil` | 非 nil 时用 OS 沙箱；失败即拒绝执行。 |
| `AutoAllowSandboxedCommands` | `nil`＝开 | 已围栏化的 shell 命令免审批（不看命令文本）。 |
| `SandboxGrants` | `nil`＝开 | 注册 `sandbox_status`/`request_sandbox_access`（不进初始 active 集）。 |
| `AllowNetwork` / `CommandAllowlist` | `nil`＝**开** / 空 | 网络开关（三态：`nil`=开，`kit.Bool(false)` 才关，关掉同时摘 `web_fetch`）；`CommandAllowlist` 为空时不限制命令名（`DisableShellAccess=false`），安全性由人工审批判定（不做命令风险分类）。 |
| `GitUndo` | `false` | 每回合前打 git 隐藏快照（`git stash create` 语义），`undo` 工具因此出现。 |
| `PTC` | `false` | 追加 `run_code`：模型写 Python/Node，内层调用走同一个 Gate。 |
| `Presenter` / `Searcher` / `SessionLog` / `Memory` | `nil` | 宿主能力注入；缺一项就摘掉对应工具（`Memory` 为 nil 时默认用按工作区分区的文件 wiki）。 |
| `Spill` / `CheckpointStore` | `nil` | 超大工具结果落盘 / 回合检查点。 |
| `Storage` / `BackgroundJobs` | `nil` | `Shutdown` 要收尾的存储与后台句柄。 |

## 安全

**边界先说清楚**：Rig 默认的围栏是**进程内策略层**，不是内核沙箱。它能约束一个
"配合的模型"不去碰不该碰的路径、不跑不该跑的命令，但**挡不住**恶意代码本身——
那是容器或 VM 的职责。

- **不可信模型 / 不可信输入必须放进容器或 VM。** 可以再叠加 `sandbox/osrunner` 作为
  第二层，但别把它当成"可以跑任意代码"的许可证：它只治理文件效果，不承诺网络、
  读权限或进程可见性。
- **沙箱只围栏"写哪里"**：`read-only` 只留 `/dev/null` 可写、`workspace-write` =
  workspace ∪ 追加可写根 ∪ 私有 `/tmp`、`danger-full-access` 不包装。**网络、读范围、
  进程可见性与设备访问都不在沙箱承诺内**——需要这些边界请用容器/VM。
- **危险命令不再被硬拒**（DSH 对齐）：`rm -rf /`、`mkfs`、`curl … | sh` 在 `ask` 档
  下**问你**，在 `auto` 档下**直接执行**。要保留硬拦，显式叠 `gate.CommandRiskGate`。
- **没有"保护路径"这一层**：kit 不追加 `.git`/`~/.ssh`/`~/.aws` 之类的写保护，
  `WithProtectedPaths`/`ProtectedPaths`/`WithDeniedGlobs` 等已删除（破坏性变更见
  CHANGELOG）。要根内局部只读用可写根的 `ReadOnlySubpaths`；要让模型看不见某些
  路径用 `sandbox.WithHidden`。
- **权限放宽必须有人确认**（`ask` 档）：只读工具自动放行，扩大可写面
  （`request_sandbox_access`）与逐命令提权都经过 Gate；`auto` 档不经人，这是它的
  定义——选它等于把决定权交给模型。
- **`danger-full-access` 是移除围栏**：它去掉工作区与允许列表，只应与显式或自动审批
  搭配，并在 UI 上明确告知用户。
- **凭据**：SDK **不做任何脱敏**——运行时与导出路径都不改写工具输出、模型文本、
  事件或错误（数据都是用户自己的）。要屏蔽凭据的宿主在自己的 UI/导出通道处理。
- **漏洞报告**：不要开公开 issue/PR。走 GitHub 私密通道
  <https://github.com/zeokeel/zeokeel-rig/security/advisories/new>，细节见
  [SECURITY.md](SECURITY.md)。

## 平台支持

| 平台 | 保障 |
| --- | --- |
| **Linux** | CI 唯一跑完整测试矩阵的平台：全部包 + 单测 + `-race` + 覆盖率。 |
| **macOS** | 交叉编译 + `GOOS=darwin` vet；真机沙箱验证在独立 workflow `macos-sandbox.yml`（自己跑、自己红绿，**不阻塞** main 与发布）。 |
| **FreeBSD** | 交叉编译 + `GOOS=freebsd` vet。 |
| **Windows** | 交叉编译 + vet，另有阻塞门禁 `windows-sandbox`（windows-latest，跑真机 ACL / 受限令牌 / Job 进程树）。 |

Windows 的两点差异要摆在显眼处：

- `bash`、`jobs`、`terminal_*` 需要 unix 风格的 `/bin/sh`。Windows 上请用 **`pwsh`**
  工具（已实现 ConPTY；ConPTY 不可用时回退 pipe）。
- 文件语义、ACL 与信号处理与 unix 不同；跨平台宿主应把"哪些工具存在"当作运行期
  查询（`Agent.Tools()` / `ActiveTools(sessionID)`），不要写死。

## 项目结构

69 个 Go 包（含 3 个 example、2 个 cmd 与 `internal/*`），按依赖层次组织：

```
rig（模块根，stdlib-only 契约：Message/Chunk/Usage、Provider/Stream、Tool/Gate、
     History、Agent/RunRequest/RunResult、Event、宿主接口）

agent                 流式 ReAct 循环（轮次、并行工具调用、审批、取消、事件、检查点）
kit                   唯一聚合层：provider + 工具面 + gate + history 一次装配
gate                  审批门禁：PersonalAsk/PersonalAuto/PersonalNever、Ask、Review、
                      AllowThenAsk、AllowSandboxedCommands、CommandRiskGate

sandbox               策略层（可写根围栏、隐藏路径、命令名单、网络、超时）
sandbox/osrunner      opt-in OS runner：bwrap/Landlock、sandbox-exec、Windows ACL

session               进程内 History；session/store 目录 NDJSON；session/compact 压缩
session/sqlite        SQLite 实现（独立子模块，未发布）
storage, storagetest  聚合契约与契约测试基线（未发布）
memory                长期记忆契约 Wiki/Page/PageMeta
instructions          AGENTS.md 层级发现 + 环境块

events                版本化 NDJSON 事件线（Envelope/Emitter/Replay/Decoder）
costmeter             会话级 token/金额计量（纳美元整数、预算告警）
modelprice            每百万 token 价目表

provider/openai       SSE、tool-call delta、reasoning、usage、重试
provider/local        本地 OpenAI 兼容服务的共享基座（Quirks/SSE/重试）
provider/ollama       默认 127.0.0.1:11434
provider/lmstudio     默认 127.0.0.1:1234

tools                 工具注册表（按名查找、确定性 specs、并发安全快照）
tools/full            工具装配：All / NewRuntime（推荐，能关后台句柄）
tools/catalog         工具账本 Static(83)/Legacy(58)/Modern(62)/ServerBound(7)
tools/presets         名字级预设 minimal(8)/personal(55)/standard(73)/all(83)
tools/audit           现代工具面对照审计

tools/builtin         read_file/write_file/edit_file/list_dir/glob/grep/bash/pwsh
tools/editor          str_replace_editor
tools/image           read_image
tools/patch           apply_patch
tools/search          文件名模糊 + BM25 + @提及
tools/undo            面向用户的回合撤销（git 隐藏快照）
tools/jobs            job_start/output/kill/list/wait
tools/terminal        terminal_*（unix PTY / Windows ConPTY / pipe 回退）
tools/web             web_fetch、web_search
tools/session         session_*（基于 rig.SessionLog）
tools/inspect         sessions/events/workspace/stats
tools/memory          memory 工具（基于 memory.Wiki）
tools/skill           skill 工具（list/search/load）
tools/schedule        schedule_*
tools/worktree        enter_worktree/exit_worktree/list_worktrees
tools/misc            working_directory、todo_write
tools/hosttools       ask_user、present、plan_review、exit_plan_mode、goal 系列
tools/discovery       tool_search（动态工具加载入口）
tools/sandboxgrant    request_sandbox_access
tools/sandboxstatus   sandbox_status
tools/shelf           渐进式加载原语（库，不是静态工具）
tools/team            异步编排：spawn_teammate/wait_agent/team_task_*
tools/subagent        同步委派构造器
tools/workflow        Go 原生声明式步骤（无 JS 引擎）
tools/ralph           迭代子 agent 直到 DONE/上限
tools/review          code_review/auto_review
tools/mcp             MCP 客户端（stdio / Streamable HTTP）
tools/lsp             LSP 客户端
tools/ptc             run_code（opt-in）

internal/fsguard      os.Root 之上的安全文件操作
internal/gitsnap      git 回合快照
internal/diffpreview  纯函数统一 diff
internal/memwiki      记忆的磁盘/进程内实现
internal/procattr     子进程属性（Windows 隐藏控制台窗口）
internal/seccomp      Linux 沙箱辅助
internal/rigdoc       AI 目录生成 + 模块化/表面守护测试

cmd/rigdoc            从真实构造器生成 docs/ai（-check 可本地自查漂移）
cmd/rigdoctor         沙箱自检/诊断 CLI
examples/quickstart   kit 最小示例（本 README 第一段代码的来源）
examples/agent        按包组装的最小 agent
examples/custom-tool  最小的第三方工具
```

三条被测试守住的模块化约束：叶子能力包不得 import `kit`/`tools/full`；根包 `rig`
只依赖标准库且不 import 本仓任何子包；只有 `kit` 同时装配 provider 与工具面。

## 质量门禁

CI（`.github/workflows/ci.yml`）与 `make ci` 是同一套东西，缺一项就不合并：

| 门禁 | 方式 |
| --- | --- |
| 格式 | `gofmt -l` 必须无输出（`make fmt`） |
| 编译 / 静态检查 | `go build ./...`、`go vet ./...` |
| 单测 / 竞态 | `go test ./...`、`go test -race ./...` |
| 覆盖率 | 总覆盖率 **≥80%**，有测试的包函数 **≥70%**（`make cover-floor`） |
| lint / 漏洞 | `staticcheck` v0.8.2、`govulncheck` v1.9.0 |
| 跨平台 | windows/darwin/freebsd 交叉编译 + `GOOS` vet |
| 工具账本 | `tools/full` 覆盖测试、`tools/catalog` 审计、`tools/audit` 现代面审计 |
| 依赖与构建面 | 依赖策略、根包 stdlib-only、`CGO_ENABLED=0` 构建与测试 |
| 模糊与性能 | fuzz 冒烟（10 个目标 × 5s）、benchmark 冒烟 |
| 示例 | `go build ./examples/...`（示例必须可编译） |

**CI 只验证代码**：文档是否最新（`make ai-docs-check`）、README 与示例是否同步
（`make quickstart`）、版本与 tag 是否一致（`make version`）都是独立目标，不进入
CI，也不进入 `make ci`。版本校验属于发布流程，见 [docs/RELEASE.md](docs/RELEASE.md)。

## 版本与兼容

- **pre-1.0**：`v0.23.x`。minor 版本之间仍可能有**有文档的**破坏性变更；正式契约面是
  模块根包 `rig`，消费者应固定精确版本。
- `rig.Version` 必须与 release tag 一致（`scripts/sync-version.sh` 在 CI 里校验）。
- 许可：**MIT**（v0.7.0 起；更早版本是 Apache-2.0）。
- 版本策略见 [docs/VERSIONING.md](docs/VERSIONING.md)，发行线矩阵见
  [COMPATIBILITY.md](COMPATIBILITY.md)，历史变更见 [CHANGELOG.md](CHANGELOG.md)。

### 接下来

CHANGELOG 的「Unreleased」正在攒下一版，三件事值得提前知道：

- **`memory` 变成公共契约**，并新增 `rig.SpillStore`——记忆页与超大工具结果都能换后端。
- **`session/sqlite` 独立子模块**：同一份会话契约的数据库实现（消息/事件/检查点/记忆/
  溢出/花费），带跨会话检索与 NDJSON `ImportDir` 迁移；它随下一版本发布，现在按未发布看待。
- **`storage` / `storagetest`**：一个值装一份会话状态的聚合契约，以及多实现共用的契约
  测试基线。

详细条目与日期一律看 [CHANGELOG.md](CHANGELOG.md)——本文档不重抄历史。

## 文档索引

| 文档 | 内容 |
| --- | --- |
| [docs/README.md](docs/README.md) | 文档总索引与推荐阅读顺序 |
| [docs/GETTING-STARTED.md](docs/GETTING-STARTED.md) | 安装、三种组装、常见任务、排障 |
| [docs/AI-SDK.md](docs/AI-SDK.md) | 给 AI 的 SDK 使用/组装指南（搭配 [docs/ai/tools.json](docs/ai/tools.json)） |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 分层、请求流、契约、扩展点、设计边界 |
| [docs/TOOLS.md](docs/TOOLS.md) | 工具目录（按类别）+ 口径 + 权限/平台影响 |
| [docs/CONFIG.md](docs/CONFIG.md) | `kit.Config` 全字段、默认值、优先级、环境变量 |
| [docs/SANDBOX-OS.md](docs/SANDBOX-OS.md) | opt-in OS runner：平台后端、注入方式、失败语义 |
| [docs/MEMORY.md](docs/MEMORY.md) | 记忆 wiki 与 spill |
| [docs/sqlite.md](docs/sqlite.md) | 会话持久化：NDJSON / SQLite / `storage` 契约 |
| [docs/COVERAGE.md](docs/COVERAGE.md) | 覆盖账本与门禁（含新增工具清单） |
| [docs/VERSIONING.md](docs/VERSIONING.md) | 版本策略 |
| [docs/RELEASE.md](docs/RELEASE.md) | 发布流程 |
| [docs/ai/](docs/ai/) | 机器可读工具目录：`tools.json` / `tools.md` / `README.md` |

包级说明看各目录的 `doc.go`，或直接 `go doc github.com/zeokeel/zeokeel-rig/kit`。

## 贡献与许可

- 先读 [CONTRIBUTING.md](CONTRIBUTING.md)：新增工具不是"写一个 `Tool` 就完事"——
  实现包 → `tools/full` 接线 → `tools/catalog.Static` → 预设名字表 → 测试 →
  `go run ./cmd/rigdoc` → `docs/TOOLS.md`/`docs/COVERAGE.md` → `CHANGELOG.md`，
  少一处 CI 就会红。
- 模型可见的 `Description` / Schema / 错误文案属于接口：改动要谨慎并同步快照，**不要翻译**。
- 代码注释与包文档以中文为主；`go doc` 应可直接阅读。
- 安全漏洞请走私密通道（见[安全](#安全)），不要开公开 issue。
- 行为准则见 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)，支持渠道见 [SUPPORT.md](SUPPORT.md)。
- 本项目以 [MIT](LICENSE) 许可发布。