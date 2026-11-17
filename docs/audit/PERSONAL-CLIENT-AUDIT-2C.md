# 个人客户端审计 2C：零配置 / 最少配置的真实体验

审计对象：`kit/`（默认装配）、`gate/`（三档默认门禁）、`agent/`（拒绝理由通道）、`tools/presets`（`personal` 预设）。

- **审计方式**：只读 + 新增 `audit_*_test.go`，不改实现；全部结论来自**真实 run**（脚本化 provider 驱动真实工具）与 gate 决策矩阵实测。
- **快照时间**：2026-11-18 01:20–01:40（`kit/`、`tools/full`、`tools/catalog`、`tools/presets` 在此期间**被其他代理持续修改**；本报告的数字均在 01:38 重新测量过）。
- **用例**：`gate/audit_personal2c_test.go`（4 条）、`agent/audit_personal2c_test.go`（2 条）、`kit/audit_personal2c_test.go`（15 条）、`tools/presets/audit_personal2c_test.go`（5 条）。
- **红/绿含义**：`TestAudit2CFinding_*` 是**当前不符合预期**的用例（红 = 发现），其余是钉住现状的 characterization（绿）。

---

## 0. 一句话结论

**"最少配置"（`Model` + `Provider` + `Root`）能跑通完整编码闭环——但只有在 `Approval: auto` 下；默认 `ask` 且没接 `Asker` 时 agent 是只读的，`write_file`/`apply_patch`/`go test` 全被拒。零配置（不传 `Root`）能装配，但静默把进程 cwd 当工作区。`personal` 预设的"体检"已经能把装配缺失的工具摘掉（39 个可用工具），但 `ask_user` 与 `web_fetch` 仍各有一个"必然失败"的洞。**

---

## 1. 零配置：`kit.New(kit.Config{Model: "m", Provider: p})`

| 问题 | 实测结论 |
|---|---|
| 会怎样？ | **成功装配**，`Root` 取 `DefaultRoot = "."`，策略根 = 进程 cwd（实测 `[/data/zeokeel-rig/kit]`）。 |
| 报错文案可读吗？ | 不报错，所以没有文案问题。真正会报错的是 `Root` 指向不存在的目录 / `Model` 为空（`Model` 为空返回包装 `rig.ErrModelRequired` 的可读错误）。 |
| 文档说了必须传 Root 吗？ | **没有说"必填"**：`Config.Root` 注明"为空表示 `DefaultRoot`（"."）"，kit 包文档的"一分钟上手"示例**显式传了 `Root: "."`**，容易让人以为必填。 |

**风险（P2）**：个人客户端从 `$HOME` 或 `/` 启动时，工作区根与可写面就是那个目录（保护路径只挡住 `.git`/`~/.ssh` 等少数位置）。建议在 `kit/doc.go` 的"默认值"一段把 `Root: "."` 的后果写成一句人话（"从哪个目录启动，agent 就能改哪个目录"），并在示例里保留显式 `Root`。

---

## 2. 最少配置：`kit.New(kit.Config{Model, Provider, Root})`

### 2.1 默认值逐条（实测）

| 项 | 默认 | 证据 |
|---|---|---|
| 工具集 | **personal 预设 → 注册 39 个工具**（名单 56 − 需显式开关 3 − 装配缺失 14） | `TestAudit2CMinimalConfigRegisteredToolCount`（逐个真实调用判定 `tool not found`） |
| 沙箱模式 | `workspace-write` | `TestAudit2CMinimalConfigDefaults` |
| 命令白名单 | 空（`WithAnyCommandAllowed(true)`，危险由 gate 判定） | `kit/kit.go:380` 附近注释 + 实测 `ls`/`go test` 可执行 |
| 审批 | `ask`；无 `Asker` 时退到 `PersonalAskWithoutAsker()` | 实测：`write_file` 被拒且理由为"没有可用的审批通道" |
| 保护路径 | **开**（`.git`/`.aws`/`.codex` + 家目录凭据） | 实测 `write_file .git/config` 被拒 |
| 会话 | 进程内 `session.Memory`；`Agent.SessionStore()` 返回 **nil** | 实测 |
| 事件 | `EventWriter` 为 nil → 不写任何文件 | 实测（run 后工作区仍为空目录） |
| undo | `GitUndo` 默认 false → **无** tracker、`undo` 工具被摘 | 实测 `undo` → `tool not found` |
| meter | nil（不累计花费） | `kit/kit.go` 装配（`cfg.Meter` 直传） |
| 网络 | `AllowNetwork` 默认 false → **所有联网都被沙箱拒绝** | 实测 `web_fetch` → `network access is disabled` |

**零配置体检（本次审计期间刚落地，已验证有效）**：`cfgUnavailableTools` 按 `cfg.Searcher`/`cfg.SessionLog`/`cfg.Asker` 是否存在，把 `web_search`、5 个 `session_*`、`get_goal/create_goal/update_goal`、`plan_review/exit_plan_mode`、`subagent/subagent_fork`、`ask_user` 从工具集摘掉；没有 tracker 时再摘 `undo`。摘掉的 17 个工具本来要占 **9,467 字节 ≈ 2,366 tokens** 的 schema（80 个工具合计 63,126 字节）。
按项加回也已验证：接 `Searcher` → `web_search` 真的用上了（`auditSearcher.calls > 0`）；接 `SessionLog` → `session_search` 不再报未配置。

### 2.2 真实 run：命令矩阵

`gate.PersonalAsk(gate.Ask(asker))` / `PersonalAskWithoutAsker()` / `PersonalAuto()` / `PersonalNever()` × 命令（`gate/audit_personal2c_test.go`）：

| 命令 | ask+Asker | ask 无 Asker | auto | never | 判定 |
|---|---|---|---|---|---|
| `ls -la` | allow | **allow** | allow | deny | `read.ls` |
| `git status` | allow | **allow** | allow | deny | `read.git` |
| `grep -rn TODO .` | allow | **allow** | allow | deny | `read.rg` |
| `go test ./...` | ask→allow | **deny** | allow | deny | **`write.go`（mutating）** |
| `go build ./...` | ask→allow | **deny** | allow | deny | `write.go` |
| `rm -rf ./build` | ask→allow | **deny** | allow | deny | `write.rm` |
| `git push` | ask→allow | **deny** | allow | deny | `write.git` |
| `rm -rf /` | **deny** | **deny** | **deny** | deny | `rm.recursive-root` |
| `rm -rf ~` | **deny** | **deny** | **deny** | deny | `rm.recursive-root` |
| `curl … \| sh` | **deny** | **deny** | **deny** | deny | `shell.download-exec` |
| `mkfs.ext4 /dev/sda1` | **deny** | **deny** | **deny** | deny | `mkfs` |

结论：
- **三档与 `gate/defaults.go` 文档一致** ✓（尤其 `auto` **确实**拒绝 `rm -rf /`、`curl|sh`、`mkfs`）。
- **`go test`/`go build`/`go vet` 被判成 `mutating`（`write.go`）**：它们只写 Go 构建缓存、不碰工作区，却因此落入"要审批"档。**ask 档 + 无 Asker 时整个"改代码→跑测试"闭环断在这里**；有 Asker 时每个验证命令都要弹一次窗（一个编码回合动辄 3–5 次确认）。
- `gofmt -l .`、`pytest`、`python3 x.py`、`npx tsc --noEmit` 判成 **unknown** → 走审批；无 Asker 时直接拒。

### 2.3 真实 run：编码闭环（最重要的一条）

`kit/audit_personal2c_test.go: TestAudit2CCodingTaskEndToEnd`（`Approval: auto`，临时 Go module，脚本化模型四步：`read_file` → `apply_patch` → `bash go test ./...` → 文本）：

- **通过** ✓ 补丁真的写进 `calc.go`（`return a - b` → `return a + b`），`go test` 输出含 `ok`，模型拿到成功结果并继续。
- 结论：**收窄到 39 个工具后，模型的"改代码并验证"能力完整**（`read_file`/`write_file`/`apply_patch`/`bash`/`grep`/`glob`/`list_dir`/`search` 都在）。

对照：**默认 `ask` + 无 Asker** 下同一条链路（`TestAudit2CFinding_DefaultAskWithoutAskerCannotEditOrTest`）：

```
write_file → rig: tool call denied: 当前没有可用的审批通道（宿主未提供 Asker）；请接上 Ask，或把审批模式改为 auto (do not repeat ...)
bash(go test ./...) → 同一条"没有审批通道"
```

即：**零配置的 agent 是只读的**，连自己的验证命令都跑不了。这与"个人编码助手"的直觉预期冲突，而 kit 文档的说法是"写文件与 shell 执行需要显式改用 ApprovalAuto"——技术上准确，但不显眼。

---

## 3. 三档审批矩阵（工具 × 档位）

`gate/audit_personal2c_test.go: TestAudit2CApprovalMatrixTools` 实测（`read-only=true` 表示在 `gate.SandboxReadOnlyTools` 里）：

| 工具 | 只读白名单 | ask+Asker | ask 无 Asker | auto | never |
|---|---|---|---|---|---|
| `read_file` | ✅ | **不问，allow** | allow | allow | deny |
| `sandbox_status` | ✅ | 不问，allow | allow | allow | deny |
| `write_file` | ❌ | **问人**（asked=1） | **deny**（"没有审批通道"） | allow | deny |
| `apply_patch` | ❌ | 问人 | deny | allow | deny |
| `search` | ❌ | **问人** | **deny** | allow | deny |
| `read_image` | ❌ | **问人** | **deny** | allow | deny |

**两个具体问题**（你的问题清单里的那两条）：**ask 模式下 `write_file` 会不会问人？→ 会**（`asked=1`，`decision=allow/deny` 取决于审批人；拒绝时理由含"人工审批拒绝了这次 X 调用"）。**auto 模式下 `rm -rf /` 会不会真被拒？→ 会**（`deny` + `rm.recursive-root`）。

**新发现**：`SandboxReadOnlyTools` 只有 5 个名字（`read_file`/`list_dir`/`glob`/`grep`/`sandbox_status`），而 `search`、`read_image`、`session_*`、`inspect`、`memory`（读）、`working_directory`、`job_list`、`terminal_read` 这类**纯只读**的工具都要走审批 → ask 档里模型每做一次检索/看图都可能弹窗或（无 Asker 时）被拒。

---

## 4. 拒绝理由：模型读完能不能自己换方案？

危险命令（`auto` 档，`rm -rf /`）模型看到的**完整** tool message：

```
rig: tool call denied: 危险命令（命中 rm.recursive-root）：rm 递归删除指向根目录、家目录或工作区外路径；改用具体路径或更小的作用范围，或让用户在需要时自行执行（本工具不会代为执行） (do not repeat the same call unchanged; change the approach, explain the blocker to the user, or ask for permission)
```

人工审批拒绝（ask 档 + Asker 答 deny，`go test ./...`）：

```
rig: tool call denied: 人工审批拒绝了这次 bash 调用（审批人选择 deny 或没有给出有效答复） (do not repeat the same call unchanged; change the approach, explain the blocker to the user, or ask for permission)
```

**判断：合格。** 四要素齐全——① 是"审批拒绝"而不是故障；② 命中哪条规则（`rm.recursive-root`，可被提示词/文档引用）；③ 中文理由 + **具体替代方案**（"改用具体路径或更小的作用范围"）；④ 明确的"不要原样重试"指令。唯一可挑的：结尾 `do not repeat …` 是英文，与中文主体混排；以及保护路径的拒绝文案是纯英文（`sandbox: denied: path "…/.git/config" is inside protected path …`）——建议统一为中文（面向中文宿主/模型的一致性，P3）。

---

## 5. 三条最严重的问题 + 最小修法

### P1-1（已局部修复，但有洞）零配置工具体检按 `cfg.Asker` 保留 `ask_user`，却没把 Asker 传进工具
- 证据：`TestAudit2CFinding_AskUserStillDeadEvenWithAsker` —— 配了 `Config.Asker`，`ask_user` 不再被摘（对），但调用仍返回 `ask_user: no interactive UI in this run; interactive questions are disabled`，`Asker` 一次都没被调用。
- 原因：`full.All(full.AllOptions{...})` 没有 `Asker`/`Host` 字段，kit 也无处传；`cfgUnavailableTools` 却以 `cfg.Asker != nil` 为保留条件。
- **最小修法**（二选一）：
  1. `tools/full.AllOptions` 增加 `Asker rig.Asker`，`full` 用它构造 `ask_user`，kit 传 `cfg.Asker`（约 15 行，正解）；
  2. 暂时把 `ask_user` 一律摘掉（从 `cfgUnavailableTools` 的保留条件里删掉 `Asker` 分支），并在 `Config.Asker` 文档里注明"目前只用于审批门禁，不启用 ask_user 工具"（1 行，止血）。
- 这是"配置了反而更糟"的陷阱：默认不配 Asker 时模型根本没这个工具；一配 Asker（个人客户端几乎必配，否则审批无法工作），模型就得到一个**永远报错**的工具。

### P1-2 `go test` / `go build` / `go vet` 被判成 `mutating`，默认 ask 档跑不了验证
- 证据：上表；以及 `TestAudit2CFinding_DefaultAskWithoutAskerCannotEditOrTest`（默认配置下 `go test ./...` 被拒，理由"没有审批通道"而非"危险"）。
- 影响：个人客户端的**核心循环**（改 → 测 → 修）在默认配置下不存在；有 Asker 时要为每条 `go test` 弹窗。
- **最小修法**（按性价比排序）：
  1. 在 `sandbox/command_risk.go` 的只读名单里加入"只写构建缓存的构建/测试类命令"：`go test|build|vet|list|env`、`cargo test|check|build`、`npm test|run build`、`pytest`、`tsc --noEmit`（判定为 `read-only`，理由：不改工作区文件）。风险点：`go test` 会执行测试代码（等价于跑任意代码）——但这在 auto 档本来就允许，ask 档的核心价值是"别打断人"，因此建议**只在 `auto` 档**把构建/测试类归为只读；
  2. 或在 kit 里提供一个 `Config.SafeCommands`（默认包含上述构建/测试命令），把它们加入 `CommandRiskGate` 的只读放行名单；
  3. 最低成本：`kit/doc.go` 的"一分钟上手"直接改成 `Approval: kit.ApprovalAuto` + 一句"个人客户端推荐 auto（危险命令仍会被拒）"，让默认路径可用。

### P1-3 `web_fetch` 永远失败（`AllowNetwork=false`），且 `web_search` 需要 Searcher 才出现 → 个人客户端默认**没有联网**
- 证据：`TestAudit2CFinding_WebFetchIsDeadByDefaultPolicy`（`web_fetch: URL blocked by policy: sandbox: denied: network access is disabled`）。
- 影响：39 个工具里有一个必然失败的联网工具；"帮我查一下…"这类最常见的个人助手请求开箱即挂。
- **最小修法**：
  1. `cfgUnavailableTools` 里加一条：`if !cfg.AllowNetwork { drop = append(drop, "web_fetch") }`（与 `web_search` 同等待遇，5 行，立刻消除"假能力"）；
  2. 同时把 `AllowNetwork` 的默认值变成 `true`（沙箱仍限制在只读 GET？——`web_fetch` 是宿主侧 HTTP，不经 shell），这需要产品判断；
  3. 折中：保留 `AllowNetwork=false` 作为默认，但在 `kit/doc.go` "默认值"里明写"默认不联网：`web_fetch` 与 `web_search` 都不可用，需要显式 `AllowNetwork: true` + `Searcher`"（文案补 2 行）。

### P2-4 `PersonalDefault(nil)` / `CommandRiskGate(nil)` 的配置错误会**中止整个 run**
- 证据：`agent/audit_personal2c_test.go: TestAudit2CFinding_GateGoErrorAbortsWholeRun` —— 需要审批但没有 inner 时 gate 返回 Go error，agent 记录一条 `role=tool` 的失败消息后**中止 run**，模型没有下一轮，宿主拿到"运行失败"。
- **最小修法**：`CommandRiskGate(nil)` 在"需要审批但没有 inner"时返回 `rig.DecisionDeny` + 说明文案（例如"当前门禁缺少 inner 审批层，已拒绝；请让宿主配置 gate.Ask 或改用 auto"）。同样 fail closed，但模型能自己换方案、对话不中断（改动约 10 行 + 更新现有用例期望）。

### P2-5 `personal` 预设的委派工具不成套
- 证据：`tools/presets/audit_personal2c_test.go: TestAudit2CFinding_DelegationToolsAreIncomplete` —— personal 有 `subagent`/`subagent_fork`，但没有 `wait_agent`/`list_agents`（AGENTS.md 自己写着"subagent 是异步的，用 wait_agent 收结果"）。
- 影响：用 `ToolPreset: "personal"` 且**给 full.All 传了 Runner** 的宿主（绕过 kit）会得到一个"能发不能收"的委派面。
- **最小修法**：在 `tools/presets/personalExcluded` 里**不要**排除 `wait_agent`/`list_agents`（或反过来把 `subagent*` 一起排掉）。二选一，别只留一半。

### P3-6 `SandboxReadOnlyTools` 对 39 个工具来说太窄
- `search`/`read_image`/`session_*`/`inspect`/`memory`/`working_directory`/`job_list`/`terminal_read` 都是只读语义却要审批。
- **最小修法**：把纯读工具加进 `gate.SandboxReadOnlyTools`（`search`、`read_image`、`session_search`、`session_trace`、`session_event_read`、`inspect`、`working_directory`、`job_list`、`terminal_read`、`terminal_list`）。注意 `memory` 有写动作（add/remove），应保留在审批侧或按 action 细分。

---

## 6. `personal` 预设：该加 / 该减

**口径先对齐**（三个数别混）：`presets["personal"]` = **56 个名字**；`presets["standard"]` = 72；`presets["all"]`/`catalog.Static` = 82；`full.All(Root)` 构造 **80**；**kit 默认实际注册 39 个**（56 − 3 个需显式开关 − 14 个装配缺失）。你清单里的"58"是上一版（当时还有 `report`/`settings_get`/`settings_set`/`present_artifact`）。

**该加（3 个）**
| 名字 | 理由 |
|---|---|
| `wait_agent` | 与 `subagent`/`subagent_fork` 配套的"收结果"通道；只留发起不留收回 = 模型发完就断线。 |
| `list_agents` | 同上，"看子代理状态"是个人客户端多任务并行时的最低可观测性。 |
| `web_search`（条件性） | 名字已在 personal 里；真正的缺口是 kit 侧没有 `Searcher` 注入路径（现在有 `Config.Searcher` 了，属装配问题不是预设问题）。 |

**该减（0 个必减；1 个建议减）**
| 名字 | 理由 |
|---|---|
| `pwsh` | 非 Windows 平台上必然失败（`pwsh` 未安装）。跨平台 SDK 保留无害，但个人客户端在 macOS/Linux 上会多一个"永远报错"的工具——建议**按平台过滤**（`runtime.GOOS != "windows"` 时摘掉），而不是从预设里删。 |
| （其余 55 个） | 都属于"个人 = 一个人 + 一个 agent"的合理能力面。`code_review`/`auto_review` 是可用的（实测需参数，不报装配错误），`inspect`/`session_event_*` 是排障面，保留。 |

**反过来：14 个"装配缺失"的工具不该从预设里删**——它们属于 `full.All` 的完整装配面（宿主传了 Host/Runner/Searcher/SessionLog 就该有）。正确做法是**kit 的 `cfgUnavailableTools` 过滤**（已实现），而不是把名字从 preset 里拿掉。

**被误删检查（你点名的 6 个）**：`todo_write` ✅ 在、`ask_user` ✅ 在（但见 P1-1）、`present` ✅ 在、`skill` ✅ 在、`memory` ✅ 在、`web_fetch` ✅ 在（但见 P1-3）。**没有误删**。

---

## 7. 用 `personal`/39 工具能不能完成"改代码并验证"（结论）

**能**，前提是 `Approval: auto`（或接上 `Asker`）。实测四步闭环全部成功：`read_file` → `apply_patch`（真的改了文件）→ `bash go test ./...`（真的跑了、输出 `ok`）→ 模型总结。缺的从来不是工具，而是默认审批档位把写/执行全部挡住了。

---

## 8. 修复记录（本次审计的直接产出）

| # | 发现 | 处置 | 落点 | 验收用例 |
|---|---|---|---|---|
| P1-1 | 配了 `Asker` 后 `ask_user` 仍报 "no interactive UI" | **已修**：`tools/full.AllOptions` 新增 `Asker rig.Asker`（同时把 `Host.Interactive` 打开），`kit` 传 `cfg.Asker` | `tools/full/full.go`、`kit/kit.go` | `kit/.../TestAudit2CAskUserReachesAsker`（Asker 被调用 1 次 + 问题文本透传 + 回答回到模型） |
| P1-3 | `web_fetch` 在 `AllowNetwork=false` 时必然失败却仍在工具集里 | **已修**：不联网就摘掉 | `kit/kit.go`（`cfgUnavailableTools`） | `TestAudit2CWebFetchFollowsAllowNetwork` + `TestAudit2CToolFilterDropsWebFetchWithoutNetwork` |
| P1-4 | 委派工具"能发不能收" | **已修（方案：一起排掉）**：`personal` 预设移除 `subagent`/`subagent_fork`；`cfgUnavailableTools` 不再列它们；`presets` 包文档写明用 `standard`/`all` + `full.All(Runner)` | `tools/presets/presets.go`、`kit/kit.go` | `TestAudit2CPersonalDropsDelegationEntirely` + `TestAudit2CToolFilterNoLongerListsSubagentTools` |
| P2 | `pwsh` 在非 Windows 上必然失败 | **已修**：按 `runtime.GOOS`（可注入 `goosForToolFilter`）摘掉 | `kit/kit.go` | `TestAudit2CToolFilterDropsPwshOutsideWindows`（linux/darwin/freebsd 摘、windows 留）+ 外部 `pwsh` → `tool not found` |
| P3 | `SandboxReadOnlyTools` 只有 5 个名字，纯读工具也要审批 | **已修**：补齐 `search`/`read_image`/`session_*`/`inspect`/`working_directory`/`job_list`/`job_output`/`job_wait`/`terminal_read`/`terminal_list`（写/执行类仍走审批） | `gate/sandbox.go` | `gate/.../TestAudit2CReadOnlyToolsDoNotAsk`（含"写工具不得进白名单"的反向断言） |
| P1-2 | `go test`/`go build` 判成 `mutating`，默认 ask 无 Asker 时编码闭环不存在 | **不改分类（正确）**，改为**文档**：`kit/doc.go` 的"一分钟上手"写明默认只读 + 两种改法；`docs/zh/AI-SDK.md` §2.5 给出两张可抄配置 + 能力对照表 | `kit/doc.go`、`docs/zh/AI-SDK.md`、`AGENTS.md` | `TestAudit2CFinding_DefaultAskWithoutAskerCannotEditOrTest`（钉住"默认只读"这一事实）+ `TestAudit2CCodingTaskEndToEnd`（auto 下闭环通过） |
| （附带）| `PersonalDefault(nil)` 配置错误中止整个 run | 由另一处改动修掉：现在返回 `DecisionDeny` + "没有配置审批通道"理由，对话继续 | `gate/gate.go` | `agent/.../TestAudit2CNoApprovalChannelDeniesWithoutAborting`（模型被调用 2 次、run 不失败） |

**修复后的工具数（发布说明用）**：
`personal` 预设名单 **54**；`kit.New(Config{})` 零配置 **37**；逐项加回：`Asker` +1（38）、`Searcher` +1（38）、`SessionLog` +5（42）、`AllowNetwork` +1（38）、`SandboxGrants` +2（39）、`PTC` +1（38）、`GitUndo`（git 仓库）+1（38）；**全部叠加 49**。

## 附：审计用例清单（红 = 发现）

| 包 | 用例 | 状态 |
|---|---|---|
| gate | `TestAudit2CApprovalMatrixTools` / `...Commands` / `...AskModeAsksInsteadOfSilentlyDenying` / `...AutoStillDeniesDangerous` / `...PersonalDefaultNilInnerFailsClosed` | 绿 |
| agent | `TestAudit2CAskDenialReasonReachesModel` | 绿 |
| agent | `TestAudit2CFinding_GateGoErrorAbortsWholeRun` | **红（P2-4）** |
| kit | `TestAudit2CZeroConfigDefaultsRootToCwd` / `...MinimalConfigDefaults` / `...RealRunCommandMatrix` / `...CodingTaskEndToEnd` / `...DangerousRejectionMessageIsActionable` / `...ProtectedPathWriteIsDenied` / `...EventsDefaultOff` / `...DefaultToolsetContextCost` / `...MinimalConfigRegisteredToolCount` / `...ZeroConfigToolPruning` / `...ReopenWithSearcherAndSessionLog` | 绿 |
| kit | `TestAudit2CFinding_DefaultAskWithoutAskerCannotEditOrTest` | 绿（钉住现状：默认 ask 无法编辑/测试 = P1-2 的表现） |
| kit | `TestAudit2CFinding_AskUserStillDeadEvenWithAsker` | **红（P1-1）** |
| kit | `TestAudit2CFinding_WebFetchIsDeadByDefaultPolicy` | **红（P1-3）** |
| presets | `TestAudit2CPresetHierarchy` / `...PersonalKeepsEssentials` / `...RemovedToolsStayRemoved` / `...FilterContract` | 绿 |
| presets | `TestAudit2CFinding_DelegationToolsAreIncomplete` | **红（P2-5）** |

复现：

```bash
export PATH=$PATH:/usr/local/go/bin
export GOCACHE=/data/zeokeel-rig/.audit-scratch/aud3c/gocache
go test ./gate/ ./agent/ ./kit/ ./tools/presets/ -run 'TestAudit2C' -count=1 -v
```