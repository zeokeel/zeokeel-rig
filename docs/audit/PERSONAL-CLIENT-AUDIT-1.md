# 个人客户端对抗审计 · 第 1 轮

- **审计对象**：`/data/zeokeel-rig`（ZeoKeel Rig v0.19.0，Go 1.27，linux/amd64）
- **审计者**：`dsh-sandbox-audit`
- **范围**：全项目。**排除** `kit/`（Lead 正在改）、`internal/diffpreview/`、`gate/diff.go`、`tools/patch/` 的预览/摘要功能（我自己上一轮写的）
- **方法**：只读审计 + 新增 `audit_*_test.go`；每条结论都尽量给"可复现的实测方法"。不做实现修改
- **时间**：2026-11-18

## 一句话结论

三条"假默认值"里，**两条在 kit 的默认路径上仍然不成立**（`ApprovalAuto` 放行 `rm -rf /`、`ApprovalAsk` 不询问任何人），只有**保护路径**是真的；此外默认工具面 **84 个里有 28 个是多智能体编排**（33%），以及 3 处文档数字与代码不符。审批那两条 Lead 已在修，本文件给出可复现的验证方法与边界。

---

## 0. 结论摘要（按优先级）

| # | 级别 | 断言（一句话） | 状态 |
|---|---|---|:--:|
| A2 | **P0** | `kit` 的 `ApprovalAuto` 走 `gate.AllowAll()` → `rm -rf /` 被放行 | ❌ 不成立（Lead 在修） |
| A3 | **P0** | `kit` 的 `ApprovalAsk` 不问人 → 写/执行类工具静默拒绝 | ❌ 不成立（Lead 在修） |
| B1 | **P1** | 默认工具面 84 个里 28 个是多智能体编排（33%），与"个人客户端"定位不符 | ⚠️ 成立（过度设计） |
| B2 | P1 | 两个 legacy 别名（`spawn_team`/`close_team`）仍作为一等工具占 schema | ⚠️ 成立 |
| B3 | P1 | `report`（事件计数/工具与错误总数/事件类型分布）属合规/审计形态 | ⚠️ 成立 |
| D2 | P1 | AGENTS.md「写/执行需显式 Gate/Approval」措辞暗示"会来问你"，实际静默拒绝 | ⚠️ 措辞误导 |
| A1 | — | `gate.PersonalAuto()` 真的拦住 9 类危险命令、放行 5 类常规命令 | ✅ 成立 |
| A4 | — | 保护路径真的拦住 `write_file`/`edit_file`/`apply_patch`（含符号链接） | ✅ 成立 |
| A5 | P2 | `.GIT` 大小写变体在 darwin/windows 可绕过 `.git` 保护 | ⚠️ 平台风险 |
| A6 | P2 | 非 Windows 平台上 `.git\config` 被当作字面文件名创建 | ⚠️ 成立 |
| A7 | P2 | `..` 处理不一致：write_file 拒绝，apply_patch 归一化后照写 | ⚠️ 成立 |
| D1 | P2 | AGENTS.md「默认 84 / grants 86」→ 实际 **83 / 85** | ⚠️ 数字不符 |
| D3 | P2 | 「完整路径」示例 `full.All(AllOptions{...})` 零值 → 81 个工具且全部报 `no roots configured` | ⚠️ 文档不精确 |
| B4–B7 | P2 | `settings_get/set`、3 个重叠呈现工具、`list_subagent_models`、默认没走 preset | ⚠️ 观察项 |

---

## 1. 专项 A：「假默认值」逐条验证

### A1 — `ApprovalAuto` 下 `rm -rf /` 必须被拒？

| | |
|---|---|
| **断言** | `gate.PersonalAuto()`（= `CommandRiskGate(AllowAll())`）：危险命令直接拒绝，常规命令放行，不问人 |
| **实测方法** | `go test ./gate/ -run 'TestAuditPersonalAuto' -v`（新增 `gate/audit_defaults_test.go`）。9 条危险命令 + 5 条常规命令 |
| **实际结果** | 9/9 危险命令 → `DecisionDeny`；5/5 常规命令 → `DecisionAllow`；全程无 Go error |
| **结论** | ✅ **成立**（`gate/defaults.go:85`）。危险清单含 `rm -rf /`、`rm -rf ~`、`sudo rm -rf /`、`mkfs.ext4`、`dd if=/dev/zero of=/dev/sda`、`curl … \| sh`、`chmod -R 000 /`、`git push --force`、fork bomb |
| **最小修法** | 无（若 `PersonalAuto` 的配置有调整，这几条用例就是回归网） |

### A2 —【P0】`kit` 的 `ApprovalAuto` 真的拦得住 `rm -rf /` 吗？

| | |
|---|---|
| **断言** | 「`ApprovalAuto` 下 `rm -rf /` 必须被拒」 |
| **实测方法** | ① 代码路径：`kit/kit.go:399-400` → `case ApprovalAuto: gateImpl = gate.AllowAll()`；② 行为对照：`go test ./gate/ -run 'TestAuditAllowAllDoesNotBlockDangerousCommands' -v` |
| **实际结果** | `gate.AllowAll()` 对 `bash {"command":"rm -rf /"}` 返回 **`DecisionAllow`**；同一调用在 `gate.PersonalAuto()` 下返回 `DecisionDeny`。`kit` 的 auto 分支用的是**前者** |
| **结论** | ❌ **不成立**。`gate.PersonalAuto()` 是对的，但 kit 的 auto 档没接它（`PersonalAuto` 在 kit 里**零引用**） |
| **最小修法** | `kit/kit.go:399-400` 改成 `gateImpl = gate.PersonalAuto()`；`ApprovalNever`（`:401-402`）改成 `gate.PersonalNever()`（文案才会说 approval=never） |

### A3 —【P0】`ApprovalAsk` 在 `Asker != nil` 时真的会问人吗？

| | |
|---|---|
| **断言** | 「`ApprovalAsk` 会询问用户」（AGENTS.md 与 `ApprovalMode` 的命名都暗示这一点） |
| **实测方法** | ① 代码路径：`kit/kit.go:403-411`，`default:` 分支 → `gate.AllowTools(read_file, list_dir, glob, grep)`，**全程没有 `gate.Ask`**；② 行为实测：`go test ./.audit-scratch/t11/probe -run TestDefaultGateDecisions`（等价构造 kit 用的那个 gate） |
| **实际结果** | `read_file` → `allow`；`write_file` / `apply_patch` / `bash` → **`deny`**（且没有 Go error、**没有任何提问**）。`gate.PersonalAsk(gate.Ask(真 Asker))` 本身是对的（`TestAuditPersonalAskActuallyAsksAHuman` 证明它真的调了 Asker 1 次） |
| **结论** | ❌ **不成立**：kit 的 ask 档从不询问，用户看到的是"工具被拒绝"而不是"要不要允许"。**产品缺的正是这一环**（上一轮报告的 D 节） |
| **最小修法** | `kit` 在 `approval==ask && cfg.Asker != nil` 时构造 `gate.PersonalAsk(gate.Ask(cfg.Asker, opts...))`；`Asker == nil` 时用 `gate.PersonalAskWithoutAsker()`（已存在，文案准确：`gate/gate.go:251`） |

### A4 — 保护路径真的拦住三个写工具吗？（含符号链接 / `../` / 绝对路径）

| | |
|---|---|
| **断言** | `.git` 这类元数据/凭据目录即使在工作区内也保持只读 |
| **实测方法** | `go test ./tools/builtin/ -run 'TestAuditWriteFileRefusesProtectedPaths\|TestAuditEditFileRefusesProtectedPaths\|TestAuditWriteFileRefusesProtectedPathViaSymlink' -v`；`go test ./tools/patch/ -run 'TestAuditApplyPatch' -v` |
| **实际结果** | write_file/edit_file/apply_patch 对 `.git/config`（直接、深层、绝对路径）全部拒绝；`alias -> .git` 再写 `alias/config` 被拒（**含二级跳转** `a -> b -> .git`）；apply_patch 的 Add/Update/Delete/Move 四条路径全拒；失败时**全有或全无**（同一补丁里的 `ok.txt` 也没被改）；`../` 逃逸被拒；保护路径之外的普通写入照常成功 |
| **结论** | ✅ **成立**（`sandbox/path.go:294-310` `CheckWritablePath` + `resolveForProtection` 先解真实落点再判保护；`protectedAncestorOf` 取最长前缀） |
| **最小修法** | 无。建议把这两组用例保留为回归网 |

### A5 —【P2 平台风险】`.GIT` 大小写变体

| | |
|---|---|
| **断言** | 保护路径在**大小写不敏感**的文件系统（macOS APFS 默认、Windows NTFS 默认）上同样生效 |
| **实测方法** | `go test ./tools/builtin/ -run TestAuditProtectedPathIsCaseSensitive -v`；代码：`sandbox/path.go:179` |
| **实际结果** | `pathWithin` 是纯字符串前缀比较（`strings.HasPrefix(root+sep)`），**不折叠大小写**。Linux 上 `.GIT` 是另一个目录（用例里表现为"创建成功"）；在 darwin/windows 上 `.GIT/config` 指向同一个 `.git/config`，但保护判定不会命中。对比：`sandbox/grant.go:283` 的 `samePath` 在 Windows 上**是**折叠大小写的——说明这类差异本仓库已有先例，只是保护路径没做 |
| **结论** | ⚠️ **平台相关漏洞面**（本机 Linux 无法执行 darwin/windows 分支，按代码路径判定） |
| **最小修法** | `sandbox/path.go` 的 `pathWithin` 在 `runtime.GOOS == "windows"`（以及 darwin，若产品保证 macOS 支持）上改为 `strings.EqualFold` 前缀比较；或在 `protectedAncestorOf` 里对两侧先折叠 |

### A6 —【P2】非 Windows 平台上 `.git\config` 会成功，但写的是另一个文件

| | |
|---|---|
| **实测方法** | `go test ./tools/builtin/ -run TestAuditBackslashPathCreatesLiteralFile -v` |
| **实际结果** | 在 Linux 上 `write_file {"path":".git\\config"}` **成功**，落盘文件的名字字面上是 `.git\config`（保护文件 `.git/config` 未变）。模型以为写的是 `.git/config` |
| **结论** | ⚠️ 不是越权（碰不到保护文件），但会误导模型并在工作区里留怪文件名；Windows 上该路径会正常命中保护 |
| **最小修法** | 非 Windows 平台在路径校验里拒绝含 `\` 的路径（或至少 result 里明说"按字面文件名创建"） |

### A7 —【P2 一致性】`..` 的处理两个写工具不一致

| | |
|---|---|
| **实测方法** | `go test ./tools/builtin/ -run TestAuditParentComponentAnywhereIsRejected -v`；`go test ./tools/patch/ -run TestAuditApplyPatchCleansDotDotInsteadOfRejecting -v` |
| **实际结果** | `write_file ".git/../outside.txt"` → 拒绝：`sandbox: denied: path ".git/../outside.txt" contains a parent-directory component`（`sandbox/path.go:40` 的 `hasDotDotComponent` 是**任何** `..` 都拒，不只逃逸）。`apply_patch` 的 Codex 解析器先 `filepath.Clean`：`.git/../escape.txt` → `escape.txt` 并被**创建**；`sub/../ok.txt` → `ok.txt` → 报"already exists" |
| **结论** | ⚠️ 无越权（Clean 后仍在根内且后续仍经策略 + `os.Root`），但同一输入两个工具给出相反结论 |
| **最小修法** | apply_patch 在解析阶段对含 `..` 的路径报同样的 `parent-directory component`（与 write_file 一致），或反过来把 write_file 放宽——**建议统一为拒绝** |

### A8 — 逃逸类路径（对照组，全部通过）

`../escape.txt`、`src/../../escape.txt` 在 write_file 与 apply_patch 上都被拒；工作区外没有被创建任何文件。✅

---

## 2. 专项 B：过度设计残留（发布面里的"服务形态"能力）

**实测方法**：`go test ./.audit-scratch/t11/probe -run TestCategorize -v`（对 `full.All(full.AllOptions{Policy, Root})` 的真实输出分类，即 kit 默认工具集）。

```
total=84
多智能体编排        28  close_team enter_worktree exit_worktree interrupt_agent list_agents
                        list_subagent_models list_teams list_worktrees ralph spawn_team
                        spawn_teammate subagent subagent_fork team_close team_interrupt
                        team_last team_list team_send team_spawn team_status team_task_create
                        team_task_get team_task_list team_task_update team_wait wait_agent
                        wait_team workflow
文件/命令/网络等核心  18
后台进程/终端        11
计划/提醒            10
会话/记忆             6
呈现                  3  present present_artifact report
审查                  3  auto_review code_review plan_review
设置                  2  settings_get settings_set
```

| # | 级别 | 发现 | 为什么不该留在个人客户端产品面 | 最小修法 |
|---|---|---|---|---|
| **B1** | P1 | **28/84（33%）是团队/多智能体编排**：`team_*`（11）、`team_task_*`（4）、`subagent`/`subagent_fork`/`spawn_teammate`、`wait_agent`/`interrupt_agent`/`list_agents`/`list_teams`、`enter_worktree`/`exit_worktree`/`list_worktrees`、`list_subagent_models`、`workflow`、`ralph` | 这些是"一台机器上跑多个 agent、给它们分派任务板、每个 agent 一个 git worktree"的**多实例 + 编排**形态——正是 Lead 列的"多实例/自建服务"类别。对单用户客户端：① 每个工具都要进 system prompt（84 个 schema 是实打实的 token 与决策噪声）；② worktree 工具的副作用（建分支、改 `HEAD`）个人用户很难理解；③ 团队任务板没有第二个成员就没有意义 | 默认集用 `presets.standard`（73）或为个人客户端新增 `presets.personal`（约 40：核心 18 + jobs/terminal 11 + 计划 10，去掉编排/团队/worktree/settings/report）；编排能力保留为**显式 opt-in**（`full.AllOptions.MultiAgent` 或独立包），不进默认 |
| **B2** | P1 | **legacy 别名当一等工具**：`spawn_team`（Description 自称 "Legacy alias of team_spawn"）、`close_team`（"Legacy alias of team_close"） | 新产品面的每个工具都要在模型上下文里占一个 schema；把历史别名当新 API 发布，等于让新用户继承旧包袱。而且**两个名字做同一件事**会让模型随机选一个，行为不可预测 | 从 `full.All` 移除两个别名（保留函数给老宿主显式挂） |
| **B3** | P1 | `report`：写一份 session 报告——"event counts, tool-call and error totals, event-type breakdown, recent events" | 这是**合规/审计/运维**产物。个人客户端里"让模型自己写一份带错误统计的报告"既不是高频需求，也把事件总览（潜在敏感信息）交给模型 | 移出默认集；需要时由宿主显式挂（或干脆改成 UI 侧能力） |
| **B4** | P2 | `settings_get`/`settings_set`：模型可见的 KV 存储，"the host decides where settings persist" | 应用外壳的配置读写不应该暴露成模型工具：它让模型能读/改宿主配置，且"host decides"意味着语义未定——典型的"服务形态"残留 | 移出默认集（宿主需要时显式挂） |
| **B5** | P2 | 三个重叠的呈现工具：`present` / `present_artifact` / `report` | 同一件事（把东西给人看）三种 API，模型选择困难、维护成本 ×3 | 合并为一个 `present`（artifact 作为其形态之一） |
| **B6** | P2 | `list_subagent_models`（"the model names the host allows subagents to use"） | 只有存在子代理时才有意义；单用户客户端默认没有子代理 | 随 B1 一起移出默认集 |
| **B7** | P2 | `presets` 已提供 `minimal=8 / standard=73 / all=84`，但 **kit 默认是 all**，AGENTS.md 也没有推荐 preset | 已经写好的"收窄"能力没人用；默认仍是最大面 | `kit.Config.ToolPreset` 默认改为 `standard`（或新 `personal`），并在 AGENTS.md 里给出推荐 |

> 数量小结：仅 B1+B2+B3+B4+B6 就能把默认工具面从 **84** 降到 **约 53**，减少 31 个 schema 与对应的选择歧义；若要"个人客户端最小可用集"，`presets.minimal`（8 个）已经存在，建议在这两者之间加一档。

---

## 3. 专项 C：默认配置体检（`kit.New(Config{Model: "x"})` 什么都不配）

**实测方法**：`kit/kit.go` 逐行读 + 等价行为探针（`go test ./.audit-scratch/t11/probe -v`）。`kit` 正在被 Lead 修改，故只列**实际代码路径**，不评估其结构。

| 项 | 默认值 / 行为 | 代码位置 | 实测 |
|---|---|---|---|
| `Root` | `"."`（进程 CWD） | `kit/kit.go:296-299` | — |
| `Mode` | `workspace-write` | `kit/kit.go:327-330` | — |
| `Network` | 关闭（`AllowNetwork` 零值 false） | `kit/kit.go:355` | — |
| `Policy` | 自建：`sandbox.New(WithRoots(root), WithMode, WithNetwork, WithCommandAllowlist)` | `kit/kit.go:344-357` | — |
| **保护路径** | **开**：每个根下 `.git`（含 worktree gitdir 指针）/`.aws`/`.codex` + 主目录 `~/.ssh`、`~/.aws`、`~/.config/gh`、`~/.netrc`（仅存在者）；`DisableProtection` 可关 | `kit/kit.go:358-360`、`:612`、`:624-632` | 见 A4 |
| **工具集** | `full.All(Policy, Root)` = **84**，再因 `undo` 无 tracker 被移除 → **实际 83** | `kit/kit.go:361-383`、`:439-445` | `full.All(Policy+Root)=84`；`withoutTool("undo")` → 83 |
| `undo` | **不进默认集**（`GitUndo` 默认 false；开了也要求是 git 仓库） | `kit/kit.go:300-309`、`:444` | ✅ 与 AGENTS.md 一致 |
| **Gate** | `ApprovalAsk`（默认）→ `gate.AllowTools("read_file","list_dir","glob","grep")`：**只读 4 个放行，其余静默拒绝，不问人** | `kit/kit.go:403-411` | A3 实测：write/apply_patch/bash → deny |
| `History` | `session.NewMemory()` | `kit/kit.go:436-441` | — |
| `SessionStoreDir` | 空 → **持久化关闭** | `kit/kit.go:424-434` | — |
| `EventWriter` | nil → **事件流关闭** | `kit/kit.go:153-155` | — |
| `Redactor` | nil → **不脱敏** | `kit/kit.go:130-139`、`:462-467` | — |
| `GitUndo` | false → 快照关闭 | `kit/kit.go:140-153` | — |
| `costmeter` | **未接线**：`costmeter` 包存在，但 `kit`/`full`/`agent` 都没有引用 | `grep -rn costmeter kit/ tools/full/ agent/` → 无结果 | — |
| `MaxIterations` | 0 = **不限轮数** | `kit/kit.go:301`、`:456` | — |
| `Provider` | 默认 OpenAI 兼容 provider，构造**不需要** API key（首次 Run 才报错） | `kit/kit.go:312-330` | — |

**一句话**：零配置的 `kit.New` = 单根 `workspace-write` + 保护路径开 + 83 个工具 + **只读 4 个可用、其余静默拒绝** + 内存会话 + 无持久化/无事件/无脱敏/无撤销。安全侧是保守的（好），但"能干什么"极窄（新用户第一次运行就会发现 `write_file` 被拒且没人告诉他为什么）。

**建议**（留给 Lead 决定）：`costmeter` 要么接进 kit（个人用户确实想知道花了多少钱），要么删掉——现在它是死代码。

---

## 4. 专项 D：文档与实现不一致

**实测方法**：逐句抽取 `AGENTS.md`、`docs/TOOLS.md`、`gate/defaults.go` 注释里的"数量/默认值/推荐组合"断言，与代码或探针结果对照。

| # | 级别 | 文档原话 | 实际 | 最小修法 |
|---|---|---|---|---|
| **D1** | P2 | `AGENTS.md:23`「默认 `full.All` **84** 个静态工具；开启 `SandboxGrants` 后为 **86**」 | `full.All(Policy+Root)` = 84/86 ✅，**但 kit 默认把 `undo` 摘掉**（`kit/kit.go:444`）→ 用户拿到的实际是 **83/85** | 改成"84 个（`GitUndo` 关闭时 kit 会摘掉 `undo`，实际 83）" |
| **D2** | P1 | `AGENTS.md`「默认 gate 只放行只读工具；**写/执行需显式 `Gate`/`Approval`**」 | 措辞让人以为"会来征求同意"；实际 `ApprovalAsk` 是**静默拒绝**，且 `Approval: auto` 是 **AllowAll**（A2/A3） | 与 kit 修复一起改文案：「默认只读放行、其余**拒绝**；要询问请传 `Asker`（kit 会构造 `PersonalAsk`）」 |
| **D3** | P2 | `AGENTS.md:25`「完整路径：`full.All(AllOptions{Host, Runner, …})` 后传给 `agent.AgentSpec.Tools`」 | 零值 `full.All(AllOptions{})` → **81** 个工具（少 `apply_patch`/`search`/`undo`，见 `tools/full/full.go:163-190` 的 `workspaceRoot != ""` 条件），且文件工具全部报 `sandbox: denied: no roots configured` | 在文档里点明：**必须传 `Root` 或带 `Roots` 的 `Policy`**，否则工作区级工具不存在 |
| **D4** | P2 | `docs/TOOLS.md:16`「Static tools built by `tools/full.All()` = **84**」 | 84 只在"给了 Root/Policy"时成立，零值 81 | 加一个脚注，或把表头改成"84（需要 Root 或带 Roots 的 Policy；零值 81）" |
| D5 | — | `AGENTS.md`「模型↔工具轮数默认不限制」「`Redactor` 默认 nil=不脱敏」「`GitUndo` 默认 false」「保护路径默认开启」「`tools/escalate`/`mcpserver`/`internal/filewatch`/`sandbox/escalate` 已删除且有守护测试」 | 全部 ✅ 与代码一致（`kit/kit.go:301`、`:462-467`、`:140`、`:612`；`internal/rigdoc/surface_test.go`） | 无 |

---

## 5. 本轮新增的审计用例

只读审计，未改任何实现。新增 3 个文件：

| 文件 | 用例 | 覆盖 |
|---|---|---|
| `gate/audit_defaults_test.go` | `TestAuditPersonalAutoDeniesDangerousCommands`（9 条危险命令）、`…AllowsBenignCommands`（5 条）、`…AllowAllDoesNotBlockDangerousCommands`、`TestAuditPersonalNeverDeniesWithReason`、`TestAuditPersonalAskActuallyAsksAHuman`、`…DeniesDangerousCommandsWithoutAsking`、`…SkipsReadOnly`、`…WithoutAskerExplainsNoChannel` | 专项 A（A1/A2/A3） |
| `tools/builtin/audit_protection_test.go` | `TestAuditWriteFileRefusesProtectedPaths`（5 种路径）、`TestAuditEditFileRefusesProtectedPaths`、`…RefusesProtectedPathViaSymlink`（含二级跳转）、`…StillWritesNormalPaths`、`…RefusesParentEscape`、`TestAuditBackslashPathCreatesLiteralFile`、`TestAuditParentComponentAnywhereIsRejected`、`TestAuditProtectedPathIsCaseSensitive` | 专项 A（A4/A5/A6/A7/A8） |
| `tools/patch/audit2_protection_ops_test.go` | `TestAuditApplyPatchAddAndUpdateProtectedFile`（3 例）、`…IsAllOrNothingWithProtectedPath`、`…CleansDotDotInsteadOfRejecting` | 专项 A（A4/A7） |

跑法：

```bash
export PATH=$PATH:/usr/local/go/bin
CGO_ENABLED=0 go test ./gate/ ./tools/builtin/ ./tools/patch/ -run 'TestAudit' -count=1 -v
```

全部用例当前**通过**（它们断言的是"实际行为"）；A5/A6/A7 这三条以 `t.Logf` 记录"⚠️"结论而不是断言失败，避免把审计发现变成 CI 红灯。

---

## 6. 本轮**未**验证（诚实边界）

| 项 | 原因 |
|---|---|
| darwin/windows 上的大小写变体绕过（A5）、`\` 分隔符、`bash`/PTY 的平台错误文案 | 本机 linux/amd64，只能按代码路径判定；已交叉编译通过，但**没有**在真机跑 |
| `kit` 内部结构与她正在做的收口 | 按 Lead 要求排除；A2/A3 只做"行为等价性验证"，不评估其实现 |
| `sandbox/osrunner` 的 mac/landlock 行为 | 需要平台权限与真实内核特性，本轮未运行 |
| 我上一轮的 `diffpreview`/`gate/diff.go`/`tools/patch` 预览 | 按 Lead 要求排除（另有独立验收） |
| `agent` 循环在门禁返回 Go error 时的行为 | 只做了代码阅读（`PersonalDefault(nil)` 的文档断言），未构造端到端反例 |

---

## 附：给 Lead 的修复顺序建议

1. **A2 + A3（kit 审批两档）** —— 你已在做；改完请用 `gate/audit_defaults_test.go` 的三条用例当验收（把 `TestAuditAllowAllDoesNotBlockDangerousCommands` 的注释条件反过来即可）。
2. **B1 + B2（工具面收窄）** —— 影响最大、风险最低（纯删减），且能立刻降低 prompt 成本与模型选择歧义。
3. **A5（大小写绕过）** —— 一行 `EqualFold`，先把 darwin 堵上；Windows 顺带。
4. **A6/A7（路径一致性）** —— 统一为"拒绝含 `\` 与含 `..` 的路径"，两个写工具一致。
5. **B3/B4/B5/B6** —— 逐条决定"移出默认集"还是"保留但默认不挂"。
6. **D1–D4（文档数字/措辞）** —— 与上面一起改，避免下次审计再对不上。