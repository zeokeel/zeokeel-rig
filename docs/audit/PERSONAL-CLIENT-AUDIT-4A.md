# 个人客户端对抗审计 · 第 4A 轮：动态工具加载（lazy tool discovery）

- **对象**：`agent/`（active 集合 / `ActivateTools` / 未激活拒绝 / `ToolSessionID`）、`tools/discovery/`（`tool_search`）、`kit/`（`ToolDiscovery`/`InitialActiveTools`/初始 16 个/`present` 门控/`skill` 常驻）、`events/`（`tool.activated`）
- **审计者**：`dsh-sandbox-audit`｜**方式**：只读 + 新增 `audit_discovery_test.go`（agent 包）与 `audit_4a_discovery_test.go`（rigdoc 包），**未改实现**
- **环境**：linux/amd64，Go 1.27，`CGO_ENABLED=0`；仓库处于多人并发编辑状态（见"时间线"一节）
- **审计目标**：**不偏离设计**——承诺 ↔ 实现逐条对照，不是找 bug

## 一句话结论

**7 条承诺成立、3 条有偏离**。其中一条是**实测到的 P0**（零配置下 `tool_search` 被审批门禁拒绝 → 21 个可发现工具永远激活不了），在我写完用例后 4 分钟内被另一位作者修掉（改的正是我准备建议的那一行）；仍**未修**的是同源的 `memory`/`todo_write` 被拒、以及一个**会让 prefix cache 整体崩掉**的"激活到全集"捷径。

---

## 0. 逐条结论

| # | 承诺 | 结论 | 证据 |
|---|---|:--:|---|
| 1 | 不激活时 `Request.Tools` 逐字节稳定 | ✅ **成立** | 5 轮 sha256 `400942272ff23e01…` 恒定、81 字节；同时 `Messages` 每轮都在变（证明量对了对象） |
| 2 | 激活只追加（前缀逐字节相同） | ✅ **成立**，且**比文档更强** | 注册 `[read_file write_file grep]`、初始 active `grep`；激活 `read_file` 后为 `[grep, read_file]`（末尾追加），54→38 字节且旧数组是新数组前缀；再跑 2 轮稳定 |
| 3 | 永不移除（空/重复/未知） | ✅ **成立** | 三种 no-op 后 `tools` JSON、sha256、字节数**完全不变**；未知只进 `unknown`；`ActiveTools` 不变 |
| 4 | 未激活不可调用、不过 gate | ✅ **成立** | counting gate = **0 次**、工具执行 = **0 次**、文案：`rig: tool "write_file" is registered but not active in this session; activate it with tool_search first` |
| 5 | 按会话隔离、切回不丢 | ✅ **成立** | s1→`read_file,write_file`、s2→`read_file,grep` 交替 4 次各自稳定；切回 s1 与首次**逐字节相同**；新会话 s3 不继承 |
| 6 | `present` 依赖门控 | ✅ **成立** | 零配置：`present` **不注册**、不在 active；接 `Presenter`：注册且**立刻 active**（16 → 17） |
| 7 | `skill` 常驻（含关闭 discovery） | ⚠️ **部分偏离** | 零配置 16 个含 `skill`/`memory`/`tool_search` ✅；`ToolDiscovery=false` 时 `skill`/`memory` 在、**`tool_search` 不注册**（实现合理，承诺措辞过宽） |
| 8 | `tool_search` 清单语义 | ✅ **成立** | 不列 `tool_search`/`memory`/`skill` 与已 active 的工具；只列 inactive；`query` 过滤有效；45 个可发现工具时输出 **41 行**（40 条 + 1 行"用 query 缩小范围"）；`activate` 返回 `activated:` + "从下一条消息起可用"，**下一轮** tools 末尾出现且仍在末尾 |
| 9 | token 与守卫 | ✅ **成立**（守卫真的会咬） | 零配置 active `tools` JSON = **17164 字节 ≈ 4291 tokens**（预算 19456 字节，余量 2292）；三个 rigdoc 守卫存在且**人为制造失败后确实变红**（输出见 §3） |
| 10 | 文档与实现一致 | ⚠️ **有偏离** | `docs/MODULES.md:147`「按**注册顺序**过滤」与实现相反、且与同文件 198 行自相矛盾；`kit.go` 注释「核心 14」实为 13；AGENTS.md 的 37/16 实测一致 |

---

## 1. 最严重的三条

### ① P0（已修）零配置下 `tool_search` 被审批门禁拒绝 → 整条链路失效

| | |
|---|---|
| **承诺** | 动态加载默认开启（`ToolDiscovery` nil = 开），模型用 `tool_search` 自助激活 |
| **实测方法** | 零配置 `kit.New{Model, Provider, Root}`，脚本让模型调用 `tool_search {}`（`internal/rigdoc/audit_4a_discovery_test.go`） |
| **实际结果（11:39 实测）** | `rig: tool call denied: 当前没有可用的审批通道（宿主未提供 Asker）；请接上 Ask，或把审批模式改为 auto`。根因：零配置 gate = `PersonalAskWithoutAsker()` = `AllowThenAsk(SandboxReadOnlyTools, …)`，而 `gate.SandboxReadOnlyTools` 里没有 `tool_search` → 注册表 **37 个工具里 21 个 discoverable 永远激活不了** |
| **结论** | ❌ 偏离（严重）：默认配置下"发现入口"本身被拒，动态加载直接失效 |
| **时间线** | 11:39 我实测到拒绝；11:43 `gate/sandbox.go` 已被另一位作者改成把 `tool_search`、`skill` 加入白名单（注释里写的理由与我一致）；11:46 我的用例复跑已通过（`terminal_open …` 清单正常返回）。**用例保留为回归守卫**（修好后自动走正向分支） |
| **最小修法** | 已完成。若要更稳，建议在 `kit` 里把 `alwaysActiveTools` 一并**并入** `PersonalAsk*` 的只读白名单，而不是依赖 `gate.SandboxReadOnlyTools` 维护同一份名单（两处一致才不会再次漏） |

### ② P1（**仍未修**）`memory` 与 `todo_write` 在零配置下不可调用

| | |
|---|---|
| **实测方法** | 对零配置 active 的 16 个工具逐个发一次最小合法调用，看 gate 决定 |
| **实际结果** | **放行 10、拒绝 6**：`[apply_patch write_file edit_file todo_write memory str_replace_editor]`。其中 4 个写工具被拒是**设计使然**（无审批通道时写操作拒绝，AGENTS.md 写明）；但 `memory` 与 `todo_write` 是**进程内状态**，不碰文件、不执行命令 |
| **结论** | ❌ 偏离（P1）：① AGENTS.md 承诺「`memory`：记忆默认开，**它不是可选扩展**」——零配置下它每次都返回"没有可用的审批通道"；② `todo_write` 是"核心 13"之一（"几乎每轮都会用到"），同样不可用；③ **三个常驻工具里只有 `skill`/`tool_search` 修好了，`memory` 被漏掉** |
| **最小修法** | 把 `"memory"`、`"todo_write"` 加入 `gate.SandboxReadOnlyTools`（二者都是纯进程内、可回滚的状态；`todo_write` 甚至不写盘）。或者在 kit 侧把 `alwaysActiveTools` + core 集里的"非文件/非执行"工具并入 allowlist，并加一条断言：**初始 active 集里除写/执行类之外的工具，在零配置 gate 下必须放行** |

### ③ P1（**仍未修**）「激活到全集」的捷径会把 tools 数组整体重排 → 缓存全崩

| | |
|---|---|
| **承诺** | 激活只追加："新工具只在数组**末尾**出现"、"激活那一轮 prefix cache 不命中、之后恢复" |
| **实测方法** | 注册 `[alpha beta gamma]`，初始 active 只有 `gamma`；依次激活 `alpha`、`beta`，观察每轮请求的 tools 顺序 |
| **实际结果** | 第 0 轮 `gamma` → 第 1 轮 `gamma,alpha` → 第 2 轮（激活 beta 后 active 覆盖全部）**`alpha,beta,gamma`**：数组被**整体重排**，不是追加 |
| **根因** | `agent/agent.go` `activeSpecs` 的捷径：`if len(set.order) == len(a.toolSpecs) { return a.toolSpecs }` —— 返回**注册顺序**的数组，丢掉了逐次追加的 order |
| **结论** | ❌ 偏离（P1）：这一轮的 cache 不是"崩一项"，而是**整个 tools 前缀全对不上**。虽然只在"激活到刚好覆盖全部注册工具"的那一刻发生一次，但它是**最坏的一种缓存失效**，且与承诺的"纯增量"直接矛盾 |
| **最小修法** | 捷径加一个顺序判等即可：<br>`if len(set.order) == len(a.toolSpecs) && slices.Equal(set.order, a.toolOrder) { return a.toolSpecs }`<br>（初始 active 集天然是注册顺序，所以这个捷径对"初始就全 active"仍成立；只有"逐次激活凑齐全集"才会走到构造分支，代价是一次分配，可接受。） |

---

## 2. 什么时候缓存会被**意外**打破（除上述第 ③ 条）

| 场景 | 会破吗 | 说明 |
|---|:--:|---|
| 不激活、连续多轮 | ❌ 不会 | 实测 5 轮 sha256 恒定（承诺 1） |
| 激活一个"注册得比已 active 更早"的工具 | ❌ 不会 | 实测顺序是 `[grep, read_file]`——**追加**而非按注册顺序插入（承诺 2）。这一点与 `docs/MODULES.md:147` 的写法**相反**，见 §4 |
| 重复激活 / 空激活 / 未知名字 | ❌ 不会 | 三种 no-op 后字节完全不变（承诺 3） |
| 激活到"恰好覆盖全部注册工具" | ✅ **会**（第 ③ 条） | 捷径返回注册顺序 → 整体重排 |
| 两个会话各自激活 | ❌ 不会 | 每会话独立 order；实测交替 4 轮各自稳定、切回不丢（承诺 5） |
| 宿主直接调 `agent.ActivateTools` | ❌ 不会 | 与模型激活走同一条路径（同一把 `activeMu`、同一个 append 语义） |
| 注册表在运行期变化（如 PTC/undo 追加、Presenter 接入） | 单轮 | 注册表在 `kit.New` 时定型，运行期不变；跨进程/改配置重建 agent 时会与上一进程的最后一轮不同（**已知且文档化的限制**："跨进程 resume 回到初始集合"） |
| 会话恢复 / fork | 单次 | 同上：激活状态只在内存里，resume 后回到初始 16 个 |
| `initialActiveTools` 里的 `sort.Strings(out)` | ❌ 不会 | agent 按 `toolOrder` 重排初始集，所以初始顺序 = 注册顺序（跨进程稳定） |

> 结论：现在的设计在"正常使用"下确实保住了前缀；**唯一的结构性缺口是"激活到全集"的捷径**。建议修完后补一条 invariant 测试：*任意激活序列下，第 n+1 轮的 tools 必须是第 n 轮的前缀加若干新项*（我的 `TestAudit4AActivatingEverythingReordersTheArray` 就是这条，修好后它会自动从 SKIP 转 PASS）。

---

## 3. 守卫是否为空壳？（第 9 项，含人为制造失败的红/绿输出）

三个守卫都在 `internal/rigdoc/tokenbudget_test.go`：`TestActiveToolsetTokenBudget`（19 KB 预算）、`TestCoreToolSchemaBudget`（单 schema 2.5 KB + 白名单）、`TestActivationAppendOnlyInvariant`（用真实零配置 agent 复验三条缓存不变量）、外加 `TestEveryActiveToolIsWired`（每个 active 工具都真能用）。

**人为制造失败**（临时把两个预算常量改小，验证后**已还原**——`git diff internal/rigdoc/tokenbudget_test.go` 里这两行现在是上下文而非改动）：

```
--- FAIL: TestActiveToolsetTokenBudget (0.00s)
    tokenbudget_test.go:154: 零配置 active 集：16 个工具，tools JSON = 17164 字节 ≈ 4291 tokens
    tokenbudget_test.go:166: 零配置 active 集 tools JSON = 17164 字节（≈4291 tokens），超过上限 1 字节。最胖的 5 个：
        memory                  2267 字节
        apply_patch             2190 字节
        str_replace_editor      2068 字节

--- FAIL: TestCoreToolSchemaBudget (0.00s)
    tokenbudget_test.go:196: 常驻工具 "read_file" 的 schema = 682 字节（≈170 tokens），超过上限 100 字节。
```

还原后两个测试恢复绿。**结论：不是空壳**，失败信息足够定位（直接点名最胖的工具/超限字节）。

**一个守卫盲区**（P1，见 ①②）：`TestEveryActiveToolIsWired` 只验证"工具被正确装配"（调用不返回 `not configured`），**不验证"默认 gate 下可调用"**。所以 16 个 active 工具里 6 个被 gate 拒绝时，它**仍然是绿的**。建议在该测试里加一条：对零配置 gate 逐个 `Approve` 一次，断言"非写/非执行类工具必须放行"。

---

## 4. 文档与实现（第 10 项）

| # | 文档原话 | 实际 | 是否偏离 | 最小修法 |
|---|---|---|:--:|---|
| 1 | `docs/MODULES.md:147`「请求里的 `tools` 按**注册顺序**过滤，因此新工具只在数组**末尾**出现」 | 实现按**激活顺序**追加（`activeSet.order`）；实测 `[grep, read_file]`。**按注册顺序过滤会把 read_file 插到 grep 前面**，那正是要避免的缓存失效 | ❌ 偏离（且与同文件 198 行「有序 active 列表，激活 = append（而不是按注册顺序重新过滤）」自相矛盾） | 把 147 行改成：「请求里的 `tools` 按**激活顺序**排列（初始集用注册顺序），新工具只**追加**在末尾」 |
| 2 | `kit/kit.go` 注释「`coreActiveTools` 是 discovery 开启时默认 active 的**核心 14**」 | 实际 13 个 | ❌ nit | 改成"核心 13" |
| 3 | `docs/MODULES.md`「零配置下就是 **16 个**」 | 实测 16 ✅ | ✅ | — |
| 4 | `docs/MODULES.md` / AGENTS.md「`skill` 的 schema 只有 **808 字节**」 | 实测 808 ✅（`memory` 1083、`tool_search` 554） | ✅ | — |
| 5 | AGENTS.md「零配置注册 **37** / active **16** / 全部叠加 **50**；接 Presenter 后 38 / 17」 | 实测 37 / 16 ✅；接 Presenter 后 **17 active** ✅；50 = 37 + 1(Asker) + 1(Searcher) + 5(SessionLog) + 1(AllowNetwork) + 1(Presenter) + 2(Grants) + 1(PTC) + 1(GitUndo) 算术成立（未逐项实测：需要 8 个宿主依赖的假实现） | ✅ | — |
| 6 | MODULES.md「**激活那一轮 prefix cache 不命中**」的诚实说明 | **仍在**（§"诚实的代价"），与实现一致 | ✅ | — |
| 7 | MODULES.md「system prompt 里没有工具描述，所以激活不会重建 prompt」 | `kit/wiring.go` 的 `assembleSystemPrompt` 只拼 base + environment + AGENTS.md ✅ | ✅ | — |

---

## 5. 本轮新增用例

| 文件 | 用例 |
|---|---|
| `agent/audit_discovery_test.go` | `TestAudit4AToolsAreByteStableWithoutActivation`（5 轮 sha256）、`TestAudit4AActivationAppendsAtEndRegardlessOfRegistrationOrder`、`TestAudit4ANeverRemovesAnything`、`TestAudit4AInactiveToolSkipsGateAndExecution`（gate=0 次）、`TestAudit4ASessionIsolationAndSwitchBack`、`TestAudit4AActivatingEverythingReordersTheArray`（**已知缺陷 → SKIP**，修好自动转 PASS） |
| `internal/rigdoc/audit_4a_discovery_test.go` | `TestAudit4APresentIsGatedByPresenter`、`TestAudit4AResidentToolsAlwaysPresent`、`TestAudit4AToolSearchListAndActivate`、`TestAudit4AToolSearchTruncatesAt40`、`TestAudit4AToolSearchIsDeniedByDefaultGate`（P0 证据 + 回归守卫） |

```bash
export PATH=$PATH:/usr/local/go/bin
CGO_ENABLED=0 GOCACHE=… go test ./agent/ ./internal/rigdoc/ -run 'TestAudit4A' -count=1 -v
```
两包全量当前均 `ok`（`TestAudit4AActivatingEverythingReordersTheArray` 以 SKIP 形式记录缺陷，不把主干 CI 变红）。

---

## 6. 未验证 / 诚实边界

| 项 | 原因 |
|---|---|
| `events.tool.activated` 的字段与 trigger=host/model 的区分 | 只读了 `kit` 侧 `emitToolActivated`；未在事件流上断言（本轮范围外，且 `events` 的写入路径需要 EventWriter 装配） |
| "全部叠加 = 50" | 算术一致，但要实测需为 Asker/Searcher/SessionLog/PTR Runner 等 8 个依赖各写假实现，超出本轮 |
| 跨进程 resume 的激活状态 | 文档明说是"回到初始集合"，实现也没有持久化，无矛盾；未做真实进程重启实验 |
| `TestActivationAppendOnlyInvariant` 的"会不会红" | 它依赖真实零配置 agent，无法在不改实现的前提下构造失败；我用自己第 2/3 条用例（同一条语义、不同断言）证明了实现正确，但**无法证明它本身会红**——这是本轮唯一没做成的"证明守卫不是空壳" |
| 另一代理正在改的 `tools/shelf`、`tools/skill` | 按要求未触碰、未评估；若它们改变了工具注册名，本报告的工具名清单可能已过期 |

---

## 7. 修复清单（按优先级）

1. **P1**：`memory`、`todo_write` 加入 `gate.SandboxReadOnlyTools`（或 kit 侧统一并入 `alwaysActiveTools`），并给 `TestEveryActiveToolIsWired` 补"默认 gate 必须放行非写类 active 工具"的断言。
2. **P1**：`activeSpecs` 的捷径加 `slices.Equal(set.order, a.toolOrder)` 判等，消除"激活到全集"的重排。
3. **P2**：`docs/MODULES.md:147` 改为"激活顺序"，与同文件 198 行统一。
4. **P2**：`kit/kit.go` 注释「核心 14」→「核心 13」。
5. **P3**：承诺 7 的措辞改为「`skill`/`memory` 常驻；`tool_search` 随 `ToolDiscovery` 开关」。