# 个人客户端审计 2H：长会话会不会坏

- 审计对象：`agent/`（主循环与压缩接线）、`session/compact`（窗口裁剪 / LLM 摘要）、`instructions/`（AGENTS.md 层级）、`events/`（事件流）、`gate/review.go`（自动审查门禁）
- 审计方式：**只读实现** + 新增 `audit_2h_*_test.go`（33 个用例，不改实现、未 commit）
- 时间：2026-11-18
- 核心问题：**一个每天开着、越聊越长的个人会话，会不会在某一天突然坏掉？**

## 0. 修复状态（2026-11-18 修复轮，本节在审计之后追加）

审计提出的 P1–P7 已全部修复，默认**全部打开**（不要让用户配置安全网）：

| 编号 | 问题 | 修复 | 落点 | 断言 |
| --- | --- | --- | --- | --- |
| P1 | 压缩失败 = 整轮对话失败 | 除 ctx 取消外一律降级：发 `EventError` 后带未压缩对话继续 | `agent/agent.go` `prepareContext` | `TestAudit2HCompactionErrorDegrades` |
| P2 | `Compact(..., budget)` 的 budget 被忽略 | 窗口建好后按 `boundary()` 整块丢弃 → 仍超则从旧到新截断正文（保留最后一条与用户消息）；`LLMCompactor` 先自适应降档工具结果 4→2→0 | `session/compact/window.go` `fitBudget`/`shrinkMessages`、`llm.go` | `TestAudit2HWindowCompactFitsBudgetWithHugeToolResult`、`TestAudit2HCompactNeverSplitsToolBlock`、`TestAudit2HLLMCompactAdaptivelyPrunesToBudget` |
| P3 | 抽风模型无兜底 | G1 连续 3 轮相同 → 注入提示；G2 连续 5 轮或同轮 ≥3 → `rig: repeated call blocked`；G3 kit 默认 200 轮 | `agent/agent.go` `trackRepetition`、`kit` 默认值 | `TestAudit2HRepeatedIdenticalToolCallsAreGuarded`、`TestAudit2HDefaultIterationCap` |
| P4 | 审查者/观察者 panic 崩进程 | goroutine 内 `recover` → 转成"审查失败"走既有回退/失败即拒绝；`AskGate` 的 Asker 与观察者同样隔离 | `gate/review.go`、`gate/ask.go` | `TestAudit2HReviewerPanicFailsClosed`、`TestAudit2HReviewerPanicUsesFallback`、`TestAudit2HObserverPanicIsIsolated`、`TestAudit2HAskerPanicIsIsolated` |
| P5 | `run.end` 三个数字不对 | `Turns`=模型轮数、新增 `Messages`=消息条数、桥按 Run 累计 `ToolCalls`；接通 `approval.request`/`approval.response`（AskGate 观察者 + ReviewGate 通知器） | `kit/kit.go`、`kit/wiring.go`、`events/events.go`、`gate/ask.go`、`gate/review.go` | `TestAudit2HRunEndPayloadFields`、`TestAudit2HApprovalIsVisibleInEvents`、`TestAudit2HAskObserverStages` |
| P6 | 孤立 `tool` 消息让会话永久 400 | `loadHistory` 后跑 `repairConversation()`（丢孤儿、合成 `[tool result unavailable]`） | `agent/repair.go` | `TestAudit2HOrphanToolMessageRepaired` |
| P7 | AGENTS.md 的 CRLF 进 prompt | `sanitizeText` 归一 CRLF 与孤立 CR | `instructions/imports.go` | `TestAudit2HCRLFNormalized` |

默认值（kit，零配置即生效）：`ContextBudget` = 模型上下文 × 0.6（下限 16k，未知窗口按 128k）；
`Compactor` = `WindowCompactor{KeepLast: 8, ToolResultKeepLast: 4}`；`MaxIterations` = 200（显式负数
= 真的不限）；压缩发生时事件流里有一条 `compacted N message(s) into a summary`；新增
`MaxRunCostUSD`（超限取消本轮并给出可读错误）。两条明确不做的（压缩持久化、零改动绕圈检测）已记入
`docs/audit/KNOWN-BOUNDARIES.md` 的 KN-19/KN-20。

6 个原"现状固定"用例已改写为**断言新行为**（改名并保留原意）：`...ErrorAbortsTurn` →
`...ErrorDegrades`、`...RepeatedIdenticalToolCalls` → `...AreGuarded`、`...OrphanToolMessageInHistoryIsNotRepaired`
→ `...Repaired`、`...CannotGuaranteeBudget` → `...FitsBudget`、`...KnownGapCRLFNotNormalized` →
`...CRLFNormalized`、`...ObserverPanicPropagatesToCaller` → `...ObserverPanicIsIsolated`；
`...ReviewerPanicCrashesProcess`（子进程断言崩溃）改为 `...ReviewerPanicFailsClosed`（进程内断言 fail-closed）。

## 0.1 结论（一句话，审计当时）

**"能聊很久，但会在三个地方突然死"**：① 压缩器一旦出错，整轮对话直接失败（不是降级）；② 上下文预算不足时压缩**不保证**压到预算内，provider 照样 400；③ 模型抽风（无限重复同一调用）没有任何兜底，默认轮数不限 = 烧钱无上限。审查者的 panic 则是**进程级**故障（直接崩溃，连 fail-closed 都做不到）。

工具调用与结果的配对（最容易 400 的地方）在压缩路径上**是安全的**：`boundary()` 会把窗口切点向前扩到整块 `tool_calls` 的起点，续聊、摘要、多轮压缩都不产生孤立 `tool` 消息。这一段实现经得起压测。

## 1. 复现方式

```bash
export PATH=$PATH:/usr/local/go/bin
export GOCACHE=/data/zeokeel-rig/.audit-scratch/aud3h/gocache

# 审计用例（只跑新增的 33 个）
CGO_ENABLED=0 go test ./agent/ ./instructions/ ./gate/ ./kit/ -run 'TestAudit2H' -count=1 -v
```

新增测试文件（全部只读，未改任何实现）：

| 文件 | 用例数 | 覆盖 |
|---|---|---|
| `agent/audit_2h_longsession_test.go` | 13 | 20 万 token 会话压缩、工具配对、压缩失败/不持久化、重复调用、孤立 tool 消息、LLMCompactor 两条路径、预算不保证 |
| `instructions/audit_2h_prompt_test.go` | 10 | 5 层链顺序/预算、@import 链/循环/超深、超单文档上限、脏字节、根外 import |
| `gate/audit_2h_review_test.go` | 6 | 非法参数/超时/审查者错误/**审查者 panic**/观察者 panic/拒绝理由 |
| `kit/audit_2h_events_test.go` | 4 | 完整 Run 的 seq/kind/run.end 一致性、审批可见性、失败收尾、并发 seq |

实跑结果：**33/33 通过**（其中 6 个是"现状固定"用例：它们断言的行为是本次审计发现的问题，修好后会主动变红提醒更新本文）。

## 2. 最严重的三条

### P1：压缩失败 = 这一轮对话直接失败（没有降级）

**证据**：`TestAudit2HCompactionErrorAbortsTurn`。压缩器返回 `errors.New("summary provider down")` 时，`Run` 返回 `rig: compact context: summary provider down`，provider **一次都没被调用**，用户看到的是"agent 出错了"。

```go
// agent/agent.go:530-548
func (r *run) prepareContext() error {
	...
	compacted, err := r.a.compactor.Compact(...)
	if err != nil {
		return fmt.Errorf("rig: compact context: %w", err)   // ← 回合就此结束
	}
```

`LLMCompactor` 内部有 fallback（`TestAudit2HLLMCompactorFailureFallsBackToWindow` 实测：摘要模型 500 时成功回退到窗口裁剪，回合照常结束），所以**只有自定义 compactor、或 fallback 自己也失败时**才会踩到。但个人客户端最容易踩的恰恰是这一类：宿主自己写的 compactor（比如"按 token 精确裁剪"）、`ctx` 到期、磁盘满导致无法写摘要……一次瞬时故障就打断用户正在做的事。

**最小修法**（8 行，不动契约）：

```go
compacted, err := r.a.compactor.Compact(r.ctx, r.sessionID, cloneMessages(r.conversation), budget)
if err != nil {
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return ctxErr                    // 取消/超时照常上抛
	}
	r.emit(rig.Event{Type: rig.EventError, Err: fmt.Errorf("compaction failed, continuing without it: %w", err)})
	return nil                            // 降级：使用未压缩对话继续
}
```
理由：预算判断是**估算**（`EstimateTokens`），真正的裁判是 provider；带着未压缩对话继续，最坏情况是 provider 报"上下文超限"，那比"压缩器内部错误"更能指导用户（删掉长文件、开新会话）。

### P2：`Compact(..., budget)` 的 budget 参数被完全忽略 → 压缩不保证落到预算内

**证据**：`TestAudit2HCompactionCannotGuaranteeBudget`。预算 8000，4 条 ~6 万 token 的消息，`WindowCompactor{KeepLast: 4}` 压完仍估算 **180001 token**。`session/compact/window.go:41` 与 `llm.go` 的 `Compact` 都没有读取 `budget`。

对个人客户端的实际含义：**"最近 8 条"里贴一次大文件/一次大工具结果，压缩就失效**，provider 直接 400，而且用户完全不知道是压缩没生效——他会以为"这个客户端不能处理长文件"。

**最小修法**（在 `WindowCompactor` 里加 15 行，`LLMCompactor` 复用同一逻辑）：

```go
// 窗口建好之后，若仍超预算，从窗口头部整块丢弃（不拆 tool 块），直到放进预算。
for budget > 0 && rig.EstimateTokens(out) > budget && len(recent) > 1 {
	next := boundary(recent[1:], len(recent)-1)   // 已含 tool 块对齐
	if next == 0 { break }
	recent = recent[next:]
	out = append(append([]rig.Message{}, system...), append([]rig.Message{SummaryMessage(dropped+next, summary)}, recent...)...)
}
```
`LLMCompactor` 可先把 `ToolResultKeepLast` 自适应降档（4 → 2 → 0 → 继续裁窗口），因为工具结果通常是最占地方的部分。

### P3：模型抽风（重复同一工具调用）完全没有兜底，且默认轮数不限

**证据**：`TestAudit2HRepeatedIdenticalToolCalls`。模型连续 6 次请求**完全相同**的 `spin({"query":"same"})`，agent 原样执行 6 次、6 次把同样的结果贴回上下文，没有任何提示注入、没有去重、没有熔断；`agent.DefaultMaxIterations = 0`（`agent/agent.go:95`）意味着只要模型不停，就一直跑。

这是"无限预算"时代的真实风险，且**只有一次配置就能防住**：`MaxIterations`（`TestAudit2HMaxIterationsStopsRunawayLoop` 实测能停住，且停下后历史仍可重放）。但默认值是 0，个人客户端用户根本不知道要设。

**最小修法**：两件事，一件 10 行、一件一行。

```go
// (a) agent：连续 N 轮"同名同参"调用 → 注入一条提示（不拒绝，保留模型改主意的空间）
if sameCallStreak(calls, r.lastCallSignature) >= 3 {
	r.appendNotice("你已经连续 %d 次用相同参数调用 %s，结果没有变化。请换一种做法，或直接回答用户。")
}
// (b) kit：把安全网默认打开
kit.Config{ MaxIterations: 200 }   // 或 kit 内部在 0 时填 200
```
`200` 不是随口给的：一次有意义的复杂任务（读 20 个文件、跑 5 次测试、改 3 个文件）通常 30–60 轮；200 轮足够覆盖正常上限，而把最坏成本从上不封顶压到"约 200 × 20k prompt token ≈ 4M token"（按主流定价约 10–15 美元/次事故），宿主也能在事件流里看到 `run.end` 后做熔断。

## 3. 逐项结果（对照任务书 1–5）

### 3.1 压缩：**配对安全，但"能不能继续跑"取决于压缩器实现**

| 断言 | 结果 | 证据 |
|---|---|---|
| ① 压缩后仍能继续跑（下一轮请求合法） | ✅ 25 轮工具往返、每轮都压缩，全部请求合法，消息数不随轮数增长 | `TestAudit2HCompactionKeepsRequestValidAfterManyRounds` |
| ② 工具调用与结果配对不被截断 | ✅ 切点落在 tool 块中间时向前扩到 assistant `tool_calls`；不以 `tool` 开头；`LLMCompactor` 的**摘要请求**同样合法（它用"占位替换"而不是删除） | `TestAudit2HCompactionKeepsToolPairing`、`TestAudit2HCompactionBoundaryInsideToolBlock`、`TestAudit2HLLMCompactorPathsAreValid` |
| ③ 旧消息被替换、连续压缩不累积摘要 | ✅ 摘要只保留最新一条（`splitConversation` 丢弃旧摘要） | `TestAudit2HCompactionSummaryDoesNotAccumulate` |
| ④ 压缩失败时回合不受影响 | ❌ **回合直接失败**（P1） | `TestAudit2HCompactionErrorAbortsTurn` |
| 额外：200 条消息 / 50 万 token 的会话 | ✅ 压缩到 16 条以内、token 降到 1/4 以下，最新用户输入保留 | `TestAudit2H200Message005MTokenSession` |
| 额外：预算是否被真正尊重 | ❌ **budget 参数被忽略**（P2） | `TestAudit2HCompactionCannotGuaranteeBudget` |
| 额外：压缩结果是否写回 `History` | ⚠️ **不写回**（只在内存里），见 §4.2 | `TestAudit2HCompactionNotPersisted` |

关于任务书里担心的"孤立 `tool` 消息导致 provider 400"：**在压缩路径上不会发生**。真正会产生孤立 `tool` 消息的是**历史本身**——见 §4.1。

### 3.2 instructions：**预算与层级都正确，只有 CRLF 是脏点**

| 断言 | 结果 | 证据 |
|---|---|---|
| 5 层链（root → 3 ancestor → local）顺序正确 | ✅ root 在前、local 在后，来源标签 `root/ancestor/local` 正确 | `TestAudit2HFiveLevelChainOrderAndBudget` |
| 渲染字节数在预算内 | ✅ `DefaultTotalBytes=256 KiB` 生效；单文档 64 KiB 截断带 `…[truncated: N bytes omitted]` | `TestAudit2HOversizeDocumentTruncated` |
| 被丢弃的部分有提示 | ✅ 占位 `<!-- document skipped: total budget exceeded -->` + 末尾 `[instructions truncated: N document(s) skipped]`；且**优先保留最具体的 local** | `TestAudit2HTotalBudgetDropsLowestPriority` |
| `@import` 链 / 循环 / 超深 | ✅ 链正常展开；循环留下 `(cycle)` 注释不挂死；超 4 层留 `(max depth)` | `TestAudit2HImportChainExpands`、`TestAudit2HImportCycleDoesNotHang`、`TestAudit2HImportDepthLimit` |
| 非法 UTF-8 / NUL | ✅ 统一替换成 U+FFFD，渲染结果永远是合法 UTF-8 | `TestAudit2HDirtyBytesNeverReachPrompt`、`TestAudit2HRenderIsAlwaysValidUTF8` |
| 仓库内文档 import 根外文件 | ✅ 被拦（按"找不到"处理） | `TestAudit2HImportOutsideRepoRootIsBlocked` |
| CRLF | ⚠️ **原样进入 prompt**（`\r\n` 保留） | `TestAudit2HKnownGapCRLFNotNormalized` |

CRLF 是 minor：不影响正确性（JSON 合法、字节确定），但白占 token，且让"同一份文档在 Windows 上编辑过"和"没编辑过"产生不同 prompt（对 prompt 缓存/日志比对不友好）。**最小修法**：`instructions/imports.go:374 sanitizeText` 里加一行 `text = strings.ReplaceAll(text, "\r\n", "\n")`。

### 3.3 events：**seq 与收尾可靠，但 `run.end` 的三个数字有两个是错的/空的**

| 断言 | 结果 | 证据 |
|---|---|---|
| seq 从 1 开始、连续、并发不重号 | ✅ 8 goroutine × 25 条 = 200 条，seq 严格 1..200 | `TestAudit2HConcurrentEmitKeepsSeqContiguous` |
| kind 全部合法 | ✅ 12 种已知 kind 之外没有出现 | `TestAudit2HEventSequenceIsConsistent` |
| `run.start` 在首、`run.end` 在尾且各一条 | ✅ | 同上 |
| `tool.call` / `tool.result` 按 id 配对 | ✅ | 同上 |
| `run.end` 的 tokens 与 `RunResult` 一致 | ✅ prompt/completion 逐字相等 | 同上 |
| `run.end` 的 `turns` 与 `RunResult` 一致 | ⚠️ **语义不符**：`Turns = len(result.Messages)`（`kit/kit.go:665`），实测 `turns=4` 而模型轮数是 2 | 同上 |
| `run.end` 的 `tool_calls` | ⚠️ **恒为 0**：字段存在，桥从不填充 | 同上 |
| 审批过程可见 | ⚠️ **`approval.request` / `approval.response` 从未发出**：一次被 deny 的审批在事件流里只有 `tool.result(is_error=true)` | `TestAudit2HApprovalIsInvisibleInEvents` |
| 失败时事件流收尾完整 | ✅ `error` + 带 `err` 的 `run.end`，seq 仍连续 | `TestAudit2HEventErrorIsRecordedAndRunEnded` |

**最小修法**（`kit/kit.go:665` + `kit/wiring.go`）：

```go
payload := eventspkg.RunEnd{
	Turns:     result.Iterations,   // 模型轮数，与文档一致
	Messages:  len(result.Messages), // 新增字段，老的 Turns 语义可以保留一个版本
	ToolCalls: toolCallCount,        // 桥在 EventToolCall 上累加
}
```
审批可见性是个人客户端的**功能性缺口**：UI 要显示"正在等你确认 `bash rm -rf build`"，只能靠事件流。`gate/review.go` 已有 `WithReviewObserver`，`AskGate` 也有审批回调位；缺的是把它们接到 `events.KindApprovalRequest/Response` 的一条线（约 30 行，位于 `kit/wiring.go`）。

### 3.4 gate/review.go：**fail-closed 做得对，但 panic 会带走整个进程**

| 场景 | 契约要求 | 实测 | 证据 |
|---|---|---|---|
| 参数不是合法 JSON | 失败即拒绝 / 回退 | ✅ `DecisionDeny`，且不调用审查者（不返回 Go error） | `TestAudit2HReviewInvalidArgsFailsClosed` |
| 审查超时 | 失败即拒绝 / 回退，且不卡住 | ✅ 20ms 超时立即返回 Deny（审查者故意无视 ctx） | `TestAudit2HReviewTimeoutFailsClosed` |
| 审查者返回 error | 回退到人工门禁 | ✅ 回退门禁被调用 1 次并放行 | `TestAudit2HReviewerErrorUsesFallback` |
| 审查拒绝 | 理由要能到模型 | ✅ `ExplainDenial` 返回审查理由 | `TestAudit2HReviewDenyReasonIsExposed` |
| **审查者 panic** | 应 fail closed | ❌ **整个进程崩溃**（子进程非零退出，stderr 有 panic 栈） | `TestAudit2HReviewerPanicCrashesProcess` |
| **观察者 panic** | 应隔离 | ❌ 沿 `Approve` 传回调用方（会中断 run） | `TestAudit2HObserverPanicPropagatesToCaller` |

审查者在**独立 goroutine** 里跑（`gate/review.go:305`），Go 的 goroutine panic 无法被父 goroutine recover——宿主代码里一个 nil map 写入就能让用户的整个会话崩掉，且发生在"正在等审查结论"这种最不该崩的时刻。

**最小修法**（3 行，`runReview` 里）：

```go
go func() {
	defer func() {
		if rec := recover(); rec != nil {
			results <- result{err: fmt.Errorf("gate: reviewer panicked: %v", rec)}
		}
	}()
	review, err := reviewer.ReviewTool(reviewCtx, request)
	results <- result{review: review, err: err}
}()
```
这样 panic 会走已有的"审查失败"通道 → 回退人工 / fail-closed，正好符合文档承诺。观察者建议同样处理（或在文档里明确"观察者 panic 由宿主负责"，但那与"永不中断 run"的承诺冲突）。

## 4. 另外两个会被用户真实撞到的坑

### 4.1 历史里的孤立 `tool` 消息不会被修复 → 直接 400

`TestAudit2HOrphanToolMessageInHistoryIsNotRepaired`：历史里有一条 `role=tool`、`ToolCallID="missing"` 的消息（上次崩溃、跨端同步、手动编辑都可能产生），agent 原样作为第一条消息发给 provider。OpenAI 兼容端点会回 `messages with role 'tool' must be a response to a preceding message with 'tool_calls'`——用户看到一条看不懂的 400，而且**这个会话从此再也打不开**（每轮都复现）。

`agent` 的 checkpoint 恢复路径会合成缺失的工具结果，但**普通历史加载不会**。**最小修法**：在 `loadHistory` 之后加一个 `repairConversation(messages)`（20 行）：

```go
// 1) 丢弃没有对应 assistant tool_calls 的 tool 消息；
// 2) 为没有结果的 tool_calls 合成 `[tool result unavailable: the session was interrupted]`；
// 3) 丢弃空 assistant 消息与连续重复的 user 消息。
```

### 4.2 自动压缩不写回历史 → 每轮重新摘要

`TestAudit2HCompactionNotPersisted`：压缩只改内存里的 `r.conversation`，`History` 仍是全量。用 `LLMCompactor` 时，**每一轮**都要再发一次摘要请求（20s 超时、按 token 计费、且模型可能给出不同摘要 → 前缀不稳定，prompt 缓存失效）。

对"每天开着"的个人客户端，这是最贵的一条隐性成本：一个 20 万 token 的会话，每轮都要为"重新理解历史"付一次钱。

**最小修法**：给 `AgentSpec` 加一个 `PersistCompaction bool`（kit 侧 `Config.CompactPersist`），压缩成功且 `History` 实现 `rig.HistoryReplacer` 时写回；同时把被替换掉的原始消息追加到一个"归档"文件（个人客户端要能回看原始对话，不能只有摘要）。或者更轻的版本：**memorize per-session 摘要**（`map[sessionID]summary`），至少同一次进程内不重复摘要。

## 5. "长会话 / 抽风模型"的兜底建议（产品建议）

按"默认值应该是安全的"这一原则，给出一组可落地的默认值。**核心观点：个人客户端不该把安全网交给用户配置**。

### 5.1 长会话：三层防线，前两层必须默认打开

| 层 | 做什么 | 默认值建议 | 现在的状态 |
|---|---|---|---|
| L1 预算 | `ContextBudget` 按模型上下文自动推（例如 `model.context_length × 0.6`），而不是让宿主填一个魔数 | 自动：`window×0.6`，最少 16k | ❌ 默认 0 = 不压缩 |
| L2 压缩 | `WindowCompactor{KeepLast: 8}` + `PruneToolResults(4)` 作为**默认** compactor；LLM 摘要是"更好但更贵"的升级项 | 开 | ❌ 默认 nil |
| L3 兜底 | 压缩失败 → 降级继续（P1）；压不进预算 → 继续裁（P2）；provider 报"上下文超限" → **捕获该错误、强制压到预算的 50% 后重试一次**，仍失败才把可读的错误交给用户 | 开 | ❌ 无 |

L3 的最后一条尤其重要：现在 provider 的"context length exceeded"会原样冒泡成一条英文 400，用户不知道该怎么办。个人客户端应该把它翻译成"这次对话太长了，我已经压缩过历史；如果还不行，请开一个新会话继续"。

另外建议把**压缩摘要可见化**：事件流里已经有 `notice`，加一条 `KindNotice{Message:"compacted N messages into a summary"}`，用户才知道"我的历史没丢，只是被折叠了"——这比静默替换更符合个人客户端的心智。

### 5.2 抽风模型：三道闸，全部默认打开

| 闸 | 触发 | 动作 | 成本 |
|---|---|---|---|
| G1 重复调用提示 | 同名 + 同参数连续 3 次 | 注入一条 user/system notice："你已经用相同参数调用 X 三次，结果没变。换一种做法或直接回答。" | 让它自己跳出循环，**不打断** |
| G2 重复调用熔断 | 同一签名连续 5 次，或同一轮内相同调用 ≥ 3 次 | 直接对该调用返回工具错误 `rig: repeated call blocked`（模型可见，且计入历史） | 停止无效消耗，但保留继续推进的可能 |
| G3 轮数上限 | `MaxIterations` | **默认 200**（不是 ∞，也不是 20） | 最坏成本从 ∞ 降到 ~4M token |

三者的顺序是"提示 → 熔断 → 总闸"：先在语义层帮助模型，再在工具层拦住重复，最后用硬上限兜住不可预见的循环（例如模型在两个工具间来回摇摆，签名每次不同）。

补充两条与"烧钱"直接相关的建议：

1. **`run.end` 必须能算出花费**。现在 `run.end` 有 tokens，但 `turns`/`tool_calls` 是错的/空的（§3.3），宿主要自己数。修好这三个字段，UI 才能在每个回合后显示"这一轮 ≈$0.02"——这是个人用户唯一能实感到的"预算护栏"。
2. **单次 Run 的成本上限**：`costmeter.Meter` 已经能记账，建议在 `AgentSpec` 里加 `MaxRunCostUSD float64`，超过就以 `ErrBudgetExceeded` 收尾（Rig 已有预算概念，但只到会话级/需宿主自己查）。比起"轮数上限"，用户更能理解"这次最多花 2 块钱"。

### 5.3 一句话版本

> 默认配置应该是：**上下文自动压缩 + 压缩失败降级 + 重复调用提示与熔断 + 200 轮硬上限 + 每轮显示花费**。现在这五项里，默认打开的只有"无"，其余都要宿主自己接。

## 6. 未覆盖 / 已知限制

- `events` 包的 emitter/decoder 单元性质（seq、超长行、截断行、版本、并发）在前几轮审计里已覆盖（`events/audit_events_test.go`、`audit_ndjson_test.go`），本轮**不重复**，只在 Run 级别验证。
- 本轮没有审计 `session/store`（持久化）与 checkpoint 恢复的**长会话**表现，只验证了 agent 内部的配对约束；`agent` 自带的恢复用例已覆盖"崩溃后合成缺失结果"。
- `instructions` 的 FIFO/特殊文件拒绝、符号链接逃逸已有用例（`fence_test.go`、`audit_fifo_*`），未重复。
- 审计期间仓库处于多人并行改动状态，`kit` 的可编译窗口很短：
  - 中途 `kit` 因 `rig.Searcher`、`tools/undo` 因 `Prune` 编译失败（别人的在途改动），恢复后我补跑了 kit 级 4 个用例，**4/4 通过**（输出见 §3.3 的实测值：`turns=4` vs `Iterations=2`、`tool_calls=0`、无 `approval.*`）；
  - 随后 `kit/audit_personal2c_test.go`（另一位审计者的文件）一度被其所有者改动到半成品状态（`rig.SessionEvent.Kind` 字段变更、`fullAllForAudit` 未定义），`kit_test` 整体无法编译；该文件落地后**已最终复跑**：33/33 全绿。
  - 若你复跑时 `kit_test` 编译失败，请先确认不是别人在途改动（`git status kit/`），与本文的用例无关。