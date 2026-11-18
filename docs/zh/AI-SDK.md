# 给 AI 的 Rig SDK 使用指南

> 目标读者：直接使用或扩展 Rig 的 AI/人类工程师。
> 机器可读工具 Schema：[docs/ai/tools.json](../ai/tools.json)；人读表格：[docs/ai/tools.md](../ai/tools.md)。

## 1. 项目提供什么

ZeoKeel Rig 是**纯 Go 的 agent 工具集/SDK**：provider、ReAct 循环、工具、权限模式、审批 Gate、会话历史与编排工具。它不包含产品运行时、UI、进程宿主或模块装配。

- Go 1.27+；`CGO_ENABLED=0`；唯一依赖 `mvdan.cc/sh/v3`（仅 shell 策略解析）。
- **工具不会自动进入 agent loop**：必须显式传给 `agent.AgentSpec.Tools`，或使用 `kit.New`（默认 `tools/full.All`）。
- 当前 release **v0.23.4**。工具计数有三个口径，别混：**`kit.New(Config{})` 零配置默认暴露 37 个**（personal 预设 54 个再经"零配置体检"裁剪）；**`full.All(AllOptions{Root})` 构建 80 个**（`SandboxGrants: true` 时 82）；**账本（`catalog.Static` / `presets["all"]`）是 82**。另有 server-bound MCP/LSP、动态 `mcp__<server>__<tool>`、可选 `run_code`（PTC）。模型↔工具轮数默认不限制（显式 `MaxIterations > 0` 上限保留）。

## 2. 三种组装方式

### 2.1 最快：kit

```go
a, err := kit.New(kit.Config{
    Model: "deepseek-chat", Root: root,
    Mode: kit.ModeWorkspaceWrite, Approval: kit.ApprovalAsk,
    CommandAllowlist: []string{"git", "go", "ls", "cat"},
})
```

- `Config.Tools == nil` → 用 `full.All` 构造、再按 **personal 预设**过滤、最后过一遍**零配置体检**：零配置默认暴露 **39** 个工具。零配置体检（`kit.New` 的默认行为，容易被忽略）：personal 预设里凡是"没有宿主依赖就永远无法成功"的工具都会被**从默认集里移除**——缺 `Searcher` 移 `web_search`，缺 `SessionLog` 移 5 个 `session_*`，kit 没有注入入口的 `get_goal`/`create_goal`/`update_goal`/`plan_review`/`exit_plan_mode`/`subagent`/`subagent_fork` **恒移**，缺 `Asker` 移 `ask_user`（共 14 个）。原则是"默认工具集里每个工具都必须真的能用"。宿主每接一项依赖按项加回（`Asker` +1、`Searcher` +1、`SessionLog` +5、`SandboxGrants` +2、`PTC` +1、git 仓库里 `GitUndo` +1）。非 nil（包括空 slice）→ 完全按传入集合，预设与体检都不生效。
- ⚠️ 手工 `full.All(full.AllOptions{})`（不传 `Root`/`Policy`）只构造 **77** 个工具：`apply_patch`/`search`/`undo` 会被**静默跳过**，其余文件工具一律返回 `sandbox: denied: no roots configured`。**必须传 `Root` 或带 roots 的 `Policy`。**
- 手工组装 `full.All` 时，**写类工具必须拿到同一个 `Policy`**：`apply_patch` 需要 `patch.WithPolicy(policy)`，否则 read-only 模式、deny glob、保护区对它全部失效（`full.All` 内部已接好）。
- 默认 gate（`Approval: "ask"`）三层：只读工具与只读**命令**免问；`bash`/`pwsh` 里的危险命令直接拒绝（规则 id + 理由回给模型）；其余**问人**——问的是你注入的 `Config.Asker`。**没接 Asker 不会自动有人问**：kit 用 `gate.PersonalAskWithoutAsker()`，其余调用一律拒绝并告诉模型"没有可用的审批通道"。`"auto"` ≠ `AllowAll`（危险命令仍拒），`"never"` 全拒。详见 §3。
- `kit.New` 不暴露 Host/Runner/MCP/Searcher，需要这些能力的工具会返回“host 未配置”错误；完整能力用 2.2。

### 2.2 推荐：显式 `full.All` + 自定义 Tools

```go
policy := sandbox.New(
    sandbox.WithRoots(root),
    sandbox.WithMode(sandbox.ModeWorkspaceWrite),
    sandbox.WithCommandAllowlist("git", "go", "ls", "cat"),
)
host := hosttools.NewMemoryHost()
tools := full.All(full.AllOptions{
    Policy: policy, Root: root,
    Host: hosttools.Options{Asker: host, Presenter: host, FilePresenter: host, Plan: host, Goal: host, Interactive: true},
    Runner: runner, Searcher: searcher, SessionLog: session.NewMemory(),
    Settings: settings, Reviewer: reviewer,
    MCPClients: clients, LSP: lspClient, // 可选
})
a, err := agent.New(rig.AgentSpec{
    Provider: provider, Model: "deepseek-chat", Tools: tools,
    Gate: gate.PersonalAsk(gate.Ask(myAsker)), // 只读免问、危险拒绝、其余问人；
    // 没有 Ask 通道时用 gate.PersonalAskWithoutAsker()，不要用裸 AllowTools
    // （那会把 bash 整个放行，`rm -rf /` 也会被允许）
})
```

### 2.3 子集或按包构造

```go
all := full.All(full.AllOptions{Policy: policy, Root: root, Runner: runner})
keep := map[string]bool{"read_file": true, "write_file": true, "bash": true, "job_output": true}
var subset []rig.Tool
for _, tool := range all { if keep[tool.Name()] { subset = append(subset, tool) } }
```

```go
bg := jobs.NewManager(jobs.WithPolicy(policy))
bash := builtin.NewWithPolicyOptions(policy, builtin.WithBackgroundJobs(bg))[6]
terms := terminal.NewManager(terminal.WithPolicy(policy), terminal.WithBackgroundJobs(bg))
```

> bash/pwsh/terminal 的后台执行与 jobs 完成通知必须共享同一个 `jobs.Manager`；`full.All` 已自动接线。手动组装需 `WithBackgroundJobs`。

### 2.4 可选 PTC

`kit.Config{PTC: true}` 追加 `run_code`：模型写 Python 3/Node.js，在**系统已安装运行时**中执行，通过 stdin/stdout 调用现有工具；每次内层调用都走同一 `rig.Gate`。不嵌入 JS 引擎。

### 2.5 个人客户端默认配置（可直接抄）

个人 agent 客户端（单用户）的推荐装配。这里每个字段都有"不传会怎样"，先看清再删：

```go
a, err := kit.New(kit.Config{
    Model:    "deepseek-chat",   // 必填：发给 provider 的模型名
    Provider: p,                 // 不传：用 BaseURL/APIKey(APIKeyEnv) 构造 openai.Provider，
                                 //       没有 key 时第一次 Run 才报错
    Root:     workdir,           // 不传："."（进程 cwd）。强烈建议显式传工作区根
    Approval: kit.ApprovalAsk,   // 不传：等价于 "ask"（见下）
    Asker:    myAsker,           // 不传：ask 模式退化成"没有审批通道"——只读照放、
                                 //       危险命令拒绝、其余一律拒绝并告知模型
    Meter:    meter,             // 不传：不计花费（EventUsage 仍然照发，只是没人累加）
    SessionStoreDir: "~/.myagent/sessions", // 不传：内存历史，进程退出即丢
    EventWriter:     eventLog,   // 不传：没有 NDJSON 事件流（UI 拿不到流式事件）

    // 其余按需：
    // Mode: sandbox.ModeWorkspaceWrite,   // 不传：默认 workspace-write
    // GitUndo: true,                      // 不传：false（见下）
    // SandboxGrants: true,                // 不传：不追加 sandbox_status/request_sandbox_access
    // PTC: true,                          // 不传：不提供 run_code
})
```

`Approval: "ask"` 到底发生什么（三层，`gate.PersonalAsk`）：

1. `read_file`/`glob`/`grep` 这类只读工具、以及 `bash` 里的只读命令（`ls`、`git status`…）——**不问，直接执行**；
2. `rm -rf /`、`mkfs`、`curl … | sh` 这类危险命令，以及**往 git 控制面写**的命令（`.git/hooks/**`、`.git/config`、`.git/info/**`、`.git/modules/**/config`，规则 id `write.git.control`）——**直接拒绝**，把规则 id 与理由作为工具错误回给模型（让它别原样重试）；
3. 其余（`write_file`、可变命令…）——**问人**：调用你提供的 `Config.Asker`（`rig.Asker`，单次询问默认 10 分钟超时）。

**没接 `Asker` 时不会"自动有人问"**：kit 用 `gate.PersonalAskWithoutAsker()`，第 3 类一律拒绝并明确告诉模型"没有可用的审批通道，请让用户接上 Ask 或改用 auto"。`"auto"` **不等于** `AllowAll`（危险命令仍拒），`"never"` 全拒。

**提权是"逐命令、需人工批准、不改变策略"**：沙箱（`Config.SandboxRunner`）能拦住的只是路径与系统调用，而真实软件还会写**非路径资源**——Windows 的 HKCU 注册表（DPAPI 凭据）、macOS 的 keychain、系统级 socket。这些目标没有任何"可写目录"能救，所以 `bash`/`pwsh` 接受两个参数：

| 参数 | 作用 |
| --- | --- |
| `sandbox_permissions` | 目前唯一取值 `"require_escalated"`：让**这一条命令**不再包 OS 沙箱执行（取值由运行期校验，schema 里没有 enum 阶梯） |
| `justification` | 给审批人看的理由，至少 10 个字符；缺了直接报错 |

四条边界：① **必须由人批准**——`Asker` 为 nil 或 `Approval` 是 `auto`/`never` 时一律拒绝（自动档不能给自己提权）；② **只豁免"这一次执行"**，`Policy` 的可写面、保护路径、命令允许/禁止名单一律不变；③ **危险命令即使提权也拒绝**（`ClassifyCommand` 判 `RiskDangerous` 的直接挡回去）；④ **没有 OS runner 时提权没有意义**，直接报错。

所以模型必须在"沙箱挡住了正常需求"时才用它（写工作区外、访问宿主的非文件资源），不能用它绕过策略。工具描述只在提权**真的可用**时说明这件事（组合里没有提权通道、通道自报不可用、或没有 runner 时，那句提示不出现，避免指向一个必然失败的参数）；被拒绝那一刻的归因行也会给出同样的处置建议。

#### 两张可直接抄的配置（决定"能不能改代码并验证"）

```go
// ① ask + Asker：有人看着，写文件与可变命令弹窗确认（只读工具/只读命令免问）
a, err := kit.New(kit.Config{
    Model: "deepseek-chat", Provider: p, Root: workdir,
    Asker: myAsker, // 实现 rig.Asker：Ask(ctx, question, options) (answer, error)
})

// ② auto：不弹窗（危险命令仍被拒：rm -rf /、mkfs、curl … | sh 会直接 deny 并说明理由）
a, err := kit.New(kit.Config{
    Model: "deepseek-chat", Provider: p, Root: workdir,
    Approval: kit.ApprovalAuto,
})
```

| 你的 AI 要做什么 | ① ask + Asker | ② auto | 默认（ask，无 Asker） |
|---|---|---|---|
| `read_file` / `grep` / `glob` / `search` / `read_image` | ✅ 免问 | ✅ | ✅ |
| `ls`、`git status`（只读命令） | ✅ 免问 | ✅ | ✅ |
| `write_file` / `apply_patch` / `edit_file` | ✅ 弹窗确认 | ✅ | ❌ 拒绝（"没有可用的审批通道"） |
| `go test` / `go build` / `git commit`（可变命令） | ✅ 每条弹窗确认 | ✅ | ❌ 拒绝 |
| `rm -rf ./build`（可变命令） | ✅ 弹窗确认 | ✅ | ❌ 拒绝 |
| `rm -rf /`、`mkfs`、`curl … \| sh`（危险命令） | ❌ 直接拒绝 | ❌ 直接拒绝 | ❌ 直接拒绝 |

结论：**默认配置是"只读 agent"**；要"改代码并验证"必须给 `Asker`（要人确认）或 `ApprovalAuto`（不问人，但危险命令仍拒）。

三个默认值背后的取舍：

- **为什么默认不脱敏（`Redactor: nil`）**：个人客户端的定位是单用户、数据都是用户自己的；工具边界脱敏会让模型拿不到真实内容（`~/.aws/config` 里的 key 名字、日志里的哈希都会被糊掉），却挡不住用户自己粘贴。真正需要脱敏的是**导出/上报日志**那一条路径（`secrets.RedactForExport`）。
- **为什么保护路径默认开（`DisableProtection` 才是出口）**：个人客户端最容易的误伤是让 agent 改坏 `.git`（含 worktree gitdir 指针）、`~/.ssh`、`~/.aws`、`~/.config/gh`、`~/.netrc`。这些位置即使落在可写根内也强制只读。它是**防误伤不是沙箱**——不改变进程权限，也不阻止用户自己用编辑器改。真要让 agent 改 `.git` 就显式 `DisableProtection: true`。
- **为什么 `GitUndo` 默认关**：快照不是免费的（每回合要哈希变化的文件），而且只对 git 工作区有意义。默认关 = 零成本；开启后每个 Run 前打一份隐藏快照（`git stash create` 语义，不动 HEAD/分支/暂存区），`undo` 工具才进默认工具集；工作区不是 git 仓库时该开关被**静默忽略**（工具不出现），宿主可用 `undo.IsGitRepo` 提前判断再显示"撤销"按钮。

### 2.6 花费展示契约（金额怎么给用户看）

`kit.Config.Meter` 接上 `costmeter.Meter` 之后，`meter.Session()` 返回的 `Summary` 就是
UI 的 JSON 契约（`cost_usd` / `estimated` / `price_table_updated` / `unpriced_models` /
`by_model` / `models`）。展示时遵守六条，否则用户会觉得"这个数字在骗我"：

1. **`cost_usd` 永远是估算**：`estimated == true` 时数字前加 `≈`（内置价目表全部标了
   `estimated`，因为价格会过时）。百分比/图形化同样要按"估算"表述。
2. **`unpriced_models` 非空时必须提示**"部分模型未计价"：这些调用成本记 0，总额是
   **下界**，不是真相。不要把它渲染成"这次很便宜"。
3. **`estimated=true` 有两种成因**（表内估算价 / 有未定价模型），所以**判断"是否漏算"
   只能看 `unpriced_models` 是否为空**。只看 `estimated` 会把"全部已计价但价格是估算"
   误判成"有模型没算钱"。
4. **本地模型显示"本地·免费"而不是 `$0.00`**：Ollama/vLLM/LM Studio 这类本地推理是
   `priced == true && cost_usd == 0 && estimated == false`——0 不是估算，是事实。
   渲染成 `$0.00` 会让人以为"这个云模型免费"。
5. **版本明细读 `models`（逐行也有 `estimated`），模型排行读 `by_model`**：前者按调用时
   的原始名（`gpt-4o-2024-08-06`、`openai/gpt-4o`）各一行，后者归并到规范名
   （`gpt-4o`）。逐行标 `≈` 用 `models[i].estimated`，不要用总额的 `estimated` 反推。
6. **显示"价目表更新于 `price_table_updated`"**：这是用户判断"这数字可信度"的唯一线索。
   价目表过时是本包已知风险（见 `modelprice` 包文档的"待核实"清单）。

持久化用 `Snapshot.CostNanoUSD`（整数纳美元），**不要**存 `cost_usd` 浮点：`Snapshot →
Restore` 逐纳元往返一致（有测试钉住），换算成 float 再存会引入 1e-9 级漂移。

计量的三个已知边界（写进你的"关于"页比藏着好）：

- 单次成本 < 1 纳美元时（自定义 `WithPricer` 的极低价），逐次四舍五入会丢；表内价目
  不会触发（最便宜也是 50 纳元/token）；
- 峰谷/地域/批量价只能取单档（表内统一取较高档，`Note` 里写明）；
- `RunRequest.Model` 覆盖 `Config.Model` 时成本记到**实际模型**，`by_model` 会出现两个键
  ——这是正确行为，不是重复计数。

## 3. 权限与审批

| 层 | 配置 | 作用 |
| --- | --- | --- |
| 权限模式 | `sandbox.Mode` / `kit.Config.Mode` | `read-only`（禁写/禁 shell）、`workspace-write`（默认：roots 内可写，仅 allowlist 命令）、`danger-full-access`（无 roots/allowlist fence，显式 denylist 仍生效） |
| 审批 | `kit.Config.Approval` / `rig.Gate` | `ask`（默认）：只读工具/只读命令免问、危险命令直接拒绝、其余问 `Asker`（没接 `Asker` → 其余一律拒绝并说明"没有可用的审批通道"）；`auto`：不问人但**危险命令仍拒**（≠ `AllowAll`）；`never`：全拒。自定义 `Gate` 完全接管 |

这些是工具实现层 fence，不是内核沙箱；不可信模型请把 host 放进容器/VM。

## 4. 工具能力摘要

完整表格（83 条 = `full.All(Root, SandboxGrants)` 的 82 个 + `run_code`）见 [docs/ai/tools.md](../ai/tools.md)。选型：

| 类别 | 工具 | 用途 |
| --- | --- | --- |
| 文件/搜索 | `read_file`, `write_file`, `edit_file`, `str_replace_editor`, `list_dir`, `glob`, `grep`, `read_image` | 读写/定位代码、读图 |
| Shell/进程 | `bash`, `pwsh`, `job_*`, `terminal_*` | 一次性命令、后台任务、持久终端（`run_in_background`/分页/SIG*） |
| Web | `web_fetch`, `web_search` | 抓取/搜索；`web_search` 支持 `queries[]`，需 `Searcher` |
| 会话/诊断 | `session_*`, `inspect` | 历史检索、统计 |
| 记忆/状态 | `memory`, `todo_write`, `schedule_*`, `working_directory`, `enter/exit/list_worktrees`, `skill` | 计划、提醒、工作区、技能 |
| 编排 | `subagent`, `subagent_fork`, `spawn_teammate`, `list_agents`, `wait_agent`, `team_*`, `send_message`, `interrupt_agent`, `team_task_*`, `workflow`, `ralph` | 委派、团队、工作流；`subagent` 异步，结果用 `wait_agent`/`team_wait`/`team_last` |
| 目标/计划 | `goal`, `get_goal/create_goal/update_goal`, `plan_review`, `exit_plan_mode` | 长目标、计划审批 |
| 宿主交互 | `ask_user`, `present` | 需要 host 接口 |
| 评审 | `code_review`, `auto_review` | 需要 `Reviewer` |
| MCP/LSP | `mcp__<server>__<tool>` 动态 + `mcp_*`；`lsp`/`lsp_*`（含 references/implementation） | 配置 client 后出现；HTTP 通知流由 `OnNotification` 接收 |
| PTC | `run_code` | `kit.Config.PTC` 开启 |

## 4.5 上下文、恢复与预设（长任务）

- **自动压缩（默认开启）**：`kit` 默认按"模型上下文 × 0.6（下限 16k）"推导 `ContextBudget`，
  并默认使用 `compact.WindowCompactor`；想换摘要式压缩时显式设置 `compact.LLMCompactor`
  （每次压缩多一次模型调用）。压缩发生时会发一条 `compacted N message(s)` 事件；
  常用 `session/compact.WindowCompactor`（确定、无模型）或 `LLMCompactor`
  （模型摘要，失败自动回退）。超预算时 agent 在模型调用前压缩内存对话。
- **手动压缩**：`(*kit.Agent).CompactNow(ctx, sessionID)`，需要 History 实现
  `rig.HistoryReplacer`（默认 `session.Memory` 已实现）。
- **工具结果落盘**：`kit.Config.Spill = &rig.SpillPolicy{Dir, ThresholdBytes, MaxFiles}`；
  超过阈值的工具结果写入文件，模型看到 `[tool result spilled to file]` + path，
  避免每轮重发。
- **中断恢复**：`kit.Config.CheckpointStore`；agent 在请求前/工具执行前/正常结束
  写检查点，`agent.RecoverInterruptedTurn` 在下次启动补“结果未知、未重放”的
  tool 结果与 assistant 标记，绝不重放副作用。
- **工具预设**：`kit.Config.ToolPreset` = `minimal`/`standard`/`all`；仅过滤工具
  集，不改变系统 prompt 与权限。
- **推理强度**：`kit.Config.ReasoningEffort` = `low`/`medium`/`high`（默认
  OpenAI 兼容 provider）；`rig.Request.Extra["reasoning_effort"]` 可按次覆盖。
- **审批辅助**：`gate.Ask(rig.Asker)`（allow/allow-always/deny）与
  `gate.Review(gate.ToolReviewer, ...)`（模型审前审批，失败/超时回退人工）。

## 5. 工具调用与结果

参数是 JSON 对象；必填字段见 `docs/ai/tools.json` 的 `required`。结果：

```go
type ToolResult struct {
    Content string
    Parts   []ContentPart // 图片等附件
    IsError bool
}
```

- `bash/pwsh` 非零退出是 `Content` + `[exit code: N]`、`IsError=false`（线协议语义）；超时/无法启动才 `IsError=true`。
- 路径相对 root/policy roots 解析，越界拒绝。
- `tools.NewRegistry(...).Specs()` 可生成模型可见工具列表。

## 6. 宿主接口与条件能力

| 接口/配置 | 支撑工具 |
| --- | --- |
| `rig.Gate` | 全部工具（含 `run_code` 内层） |
| `rig.Asker` | `ask_user` |
| `rig.FilePresenter` | `present` |
| `rig.PlanController` | `plan_review` / `exit_plan_mode` |
| `rig.GoalStore`（可选 `hosttools.GoalRevisionStore`） | `goal` / 三个 goal 工具 |
| `rig.SubagentRunner` | `subagent`/`subagent_fork`/`workflow`/`ralph`/`team_*` |
| `rig.SessionLog` | `session_*`/`inspect`/`report` |
| `rig.Scheduler`/`MemoryStore`/`Settings`/`Reviewer` | schedule/memory/settings/review |
| `web.Searcher` | `web_search` |
| `AllOptions.MCPClients` / `LSP` | MCP/LSP 工具 |

默认 `full.All` 会构造需要接口的工具；未配置时调用返回可读错误，AI 应检查可用性/向用户确认。

## 7. AI 工作规则

1. 先定组装方式：快速用 `kit.New`；完整能力用 `full.All` + 显式依赖。
2. 不假设工具已注册；缺工具时给用户明确错误，不要伪造调用。
3. 遵守权限：默认只读 gate；写/执行需显式放行；`danger-full-access` 仅在可信/容器环境。
4. 异步工具用 `wait_agent`/`team_wait`/`team_last` 收结果；完成通知会作为 user 消息进入下一步。
5. 长任务用 `job_*`/terminal；后台 bash 返回 job id。
6. 只操作允许路径；不要路径逃逸或让解释器绕过 allowlist。
7. PTC 只在 `kit.Config.PTC=true` 时存在。
8. 不改模型可见的描述/Schema/错误文案。

## 8. 文件地图

| 路径 | 内容 |
| --- | --- |
| `agent/` | ReAct 循环、工具执行、通知注入 |
| `kit/` | 一站式组装（默认 full.All、权限/审批、PTC） |
| `sandbox/` | 权限模式、路径/命令策略、mvdan shell 解析 |
| `tools/full/` | 全部静态工具与后台/通知接线 |
| `tools/<name>/` | 各工具包（含中文包文档） |
| `cmd/rigdoc/` | 生成 AI 工具目录 |
| `cmd/rigdoctor/` | 沙箱诊断：渲染各平台 profile、探测 runner、展示每个模式的实际可写面 |
| `docs/ai/tools.json` | 机器可读 Schema（AI 首选） |
| `AGENTS.md` | 仓库内 AI 规则 |

## 9. 重新生成

```sh
go run ./cmd/rigdoc
go run ./cmd/rigdoc -check
```
