# 平台化改造 · 逐点代码清点（A 区：rig 根包 / kit / agent / session(不含 store,sqlite) / tools / events / costmeter / memory / memwiki / gate / rigdoc）

> 依据：`docs/PLATFORM.md`（v2）。本文件**只做清点**，不改任何 `.go`。
> 所有 `文件:行` 均用 `grep -n` / `sed -n` 实测核对（基线 commit `c536811`，
> `CGO_ENABLED=0 go build ./...` 绿）。凡与任务描述不符处，一律以实测为准并在
> §0.2 标注更正——**宁多勿漏，但绝不猜**。

---

## 0. 清点范围与方法

### 0.1 范围

| 在范围内 | 不在范围（另一人负责） |
|---|---|
| `*.go`（根包 `rig`） | `session/store` |
| `kit/` | `session/sqlite`（独立子模块） |
| `agent/` | |
| `session/`（**仅** `memory.go`、`compact/`） | |
| `tools/`（全部子包，重点 `tools/session`、`tools/memory`、`tools/builtin/spill.go`） | |
| `events/`、`costmeter/`、`memory/`、`internal/memwiki/` | |
| `internal/rigdoc/`、`gate/`（仅审批/存储相关） | |

`session/sqlite` 只在**契约取证**时引用（证明 `storage.Storage` 可实现），不计入本清单的处置行。

### 0.2 三处实测更正（任务描述与代码不一致，以代码为准）

| # | 任务描述 | 实测 |
|---|---|---|
| C1 | 「`kit.Agent` 的 `History()`」 | **不存在**。`kit.Agent` 的全部方法只有 `MemoryWiki/MemoryWikiError/Tools/RegisteredTools/ToolsFor/ActiveTools/ActivateTools/SessionStore/Policy/UndoTracker/Run/watchCost/CompactNow/RunText/EventError`（`kit/kit.go:952,963,1024,1033,1044,1057,1069,1105,1117,1134,1142,1215,1243,1274` + `kit/wiring.go:115`）。`kit` 里 `history` 只是**私有字段**（`kit/kit.go:922`），仅供 `CompactNow` 读（`kit/kit.go:1247,1251`）。 |
| C2 | 「三处互斥检查」（SessionStore/SessionStoreDir/History） | 只有 **2 个报错点**，覆盖 3 个两两组合：`kit/kit.go:757-760`（`SessionStore` vs `History`+`SessionStoreDir` 两个组合共用一条错误文案）与 `kit/kit.go:764-767`（`SessionStoreDir` vs `History`）。第三个组合没有独立代码点。 |
| C3 | 「`cfgUnavailableTools` 里 session_* 的摘除逻辑在注入 Storage 后**永不触发**」 | **不成立，除非 kit 零配置也自动建 Storage**。摘除条件是 `cfg.SessionLog == nil`（`kit/kit.go:983`）；`Storage` 是宿主注入字段，零配置下仍为 `nil`（宿主注入 `Storage` 后 `kit` 才回填 `cfg.SessionLog`），因此该分支**仍会触发**。真实风险是「双来源优先级」与「覆盖率下降」，不是永久不可达。详见 §2-S6 与 §14-②。 |

### 0.3 现状基线（引用 `PLATFORM.md` §1，已逐字段复核）

`kit.Config` 与存储相关的 9 个字段（行号为实测）：

| # | 字段 | 行 | 类型 |
|---|---|---|---|
| 1 | `Spill` | `kit/kit.go:199` | `*rig.SpillPolicy`（具体结构体指针） |
| 2 | `CheckpointStore` | `kit/kit.go:201` | `rig.CheckpointStore`（接口） |
| 3 | `History` | `kit/kit.go:231` | `History`（=`rig.History` 接口，别名见 `kit/aliases.go:37`） |
| 4 | `SessionStoreDir` | `kit/kit.go:236` | `string` |
| 5 | `SessionStore` | `kit/kit.go:241` | `*store.Dir`（**唯一具体类型**） |
| 6 | `SessionLog` | `kit/kit.go:254` | `rig.SessionLog`（接口） |
| 7 | `Meter` | `kit/kit.go:259` | `*costmeter.Meter`（具体结构体指针） |
| 8 | `Memory` | `kit/kit.go:268` | `memory.Wiki`（=`tools/memory.Wiki`=`memwiki.Wiki` 接口） |
| 9 | `EventWriter` | `kit/kit.go:307` | `io.Writer`（stdlib 接口） |

> 注意 §1 说「只有 `SessionStore` 是具体类型」：严格说 `Spill` 与 `Meter` 也是具体类型
> （`*rig.SpillPolicy`、`*costmeter.Meter`），只是它们不是「存储实现」而是「策略/累加器」。
> 本清单按「是否阻碍宿主换实现」来判定：**只有 `SessionStore` 阻碍**（它有 `*store.Dir` 的
> 方法集，宿主无法提供同形状实现），`Spill` 的 `Store` 子字段本身已是接口
> （`spill.go:107`），`Meter` 是纯内存计算、不落盘。

---

## 1. 根包 `rig`（stdlib-only 叶子）

> 铁律：根包**不得 import 本仓任何子包**（`internal/rigdoc/modularity_test.go:80-92`
> 的 `TestRootPackageStaysStdlibOnly` 硬拦）。因此 `storage.Storage` 引用
> `memory.Wiki` **不可能**放进根包——这是 `storage` 必须独立成包的第一性理由。

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `tool.go:53` | `rig.History` | `Load`+`Append(ctx,sid,...Message)` 两方法 | 不变（`storage.Storage.History()` 返回它） | 保留 | 无 | `contract_test.go` |
| `tool.go:62` | `rig.HistoryReplacer` | 可选 `Replace`，被 `kit.CompactNow` 类型断言（`kit/kit.go:1247`） | 不变 | 保留 | 若 `Storage.History()` 返回的视图不实现它，`CompactNow` 对 Storage 宿主**永远报错**——必须要求 sqlite `historyView` 的 `Replace`（`session/sqlite/views.go:59`）继续满足 | `kit/compact_test.go:34`、`session/memory_replace_test.go` |
| `spill.go:25` | `rig.SpillStore` | `Save/Load/GC` | 不变；成为 `storage.Storage.Spill()` 的返回类型 | 保留 | 无 | `agent/spill_store_test.go:29` |
| `spill.go:35` | `rig.ErrSpillNotFound` | 哨兵 | 不变 | 保留 | 无 | `tools/builtin/spill_test.go:66` |
| `context.go:85` | `rig.SpillPolicy` | `Dir/ThresholdBytes/MaxFiles/Store` 四字段 | **不改**。`Store` 可被 `storage.Storage.Spill()` 填充（内部合成），其余三字段是宿主旋钮、Storage **不覆盖** | 保留 | 若新代码只认 `Storage.Spill()` 而忽略 `SpillPolicy.MaxFiles`，则「回收上限」静默失效——`agent/spill.go:42-44` 的 GC 目前只认 policy | `agent/spill_store_test.go:52` |
| `context.go:111` | `rig.SpillSummaryPrefix` | 落盘前缀 | 不变（模型可见文案） | 保留 | 改文案会打破 `IsSpilledToolResult`（`context.go:114`）与回放 | `agent/spill_store_test.go` |
| `checkpoint.go:69` | `rig.Checkpoint` | 快照结构（`SessionID/TurnID/Phase/Messages/PriorMessages`…） | 不变；`storage.Storage.Checkpoints()` 的 `Save/Load` 载荷 | 保留 | 无 | `agent/checkpoint_test.go:117` |
| `checkpoint.go:162` | `rig.CheckpointStore` | `Load/Save/Clear` | 不变；成为 `Storage.Checkpoints()` 返回类型 | 保留 | 无 | `agent/checkpoint_integration_test.go:54,77,113` |
| `extensions.go:111` | `rig.SessionLog` | 只读 `Events/Search/Last` | 不变；`Storage` 需补一个 `SessionLog()` 返回**这个类型**（现状 `sqlite.Store.SessionLog()` 返回自有接口，见 `session/sqlite/views.go:19-32`） | 保留 | 若 Storage 只暴露写入方法（`AppendEvent`）而不暴露 `rig.SessionLog` 视图，5 个 `session_*` 工具**全部失源**（`tools/session/tools.go:48`） | `tools/session/tools_test.go:49` |
| `extensions.go:102` | `rig.SessionEvent` | 读模型（`Seq/SessionID/Type/Time/Payload`） | 不变 | 保留 | 无 | `tools/session/memory_test.go` |
| `extensions.go:61` | `rig.MemoryStore` | **扁平**旧记忆接口，`tools/memory.WikiFromStore`（`tools/memory/memory.go:108`）作只读迁移通道 | 建议 `deprecated 保留一版`：`storage.Memory()` 返回 `memory.Wiki` 后，扁平接口只剩迁移用途 | deprecated 保留一版 | 改后 `WikiFromStore`（`tools/memory/memory.go:108`）与 `storeWiki`（`114-200`）全仓**零非测试调用**（`memory_store` 已无注入入口：`full.AllOptions.Memory` `tools/full/full.go:110` 是唯一入口，kit 从不设置它）→ 疑似死代码 | 无（`tools/memory/memory_test.go` 未覆盖 `WikiFromStore`） |
| `agent.go:77` | `rig.Agent` | 只有 `Run` | 不变。`Shutdown(ctx) error` **不要**加进主接口：与 `ToolActivator`/`HistoryReplacer` 的既定做法一致（`agent.go:83-87` 明写「可选接口而不是 Agent 的方法」） | 保留 | 若硬塞进 `rig.Agent`，所有测试替身被迫跟进 | `contract_test.go` |
| `agent.go:105` | `rig.AgentSpec` 的 `History`（`:125`）、`Spill`（`:138`）、`CheckpointStore`（`:142`） | agent 的三处存储注入点 | 不变；`kit` 负责用 `storage.Storage` 的三个视图填它们 | 保留 | 无 | `agent/agent_test.go` |
| `version.go:3` | `rig.Version` | 版本号 | 改公共面后按 `docs/RELEASE.md` 递增 | 改写 | 不同步会让 `internal/rigdoc/rigdoc_test.go:73` 红 | `internal/rigdoc/rigdoc_test.go:58` |

**新增（根包）**：无。根包**不应该**新增任何存储符号——这是 stdlib-only 约束的直接结果。

---

## 2. `kit.Config` 的 9 个存储字段（逐字段）

> 读法：`cfg.X` 在 `kit.New` 内被读的行 → 传给谁 → 改接口后谁死。

### S1 `Spill`（`kit/kit.go:199`）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:199` | `Config.Spill *rig.SpillPolicy` | 读点 2 处：`kit.go:626`（`spillStoreOf` → `full.AllOptions.SpillStore`，供注册 `spill_read`）、`kit.go:873`（→ `rig.AgentSpec.Spill` → `agent.spill` `agent/agent.go:126`） | 保留字段（阈值/目录/上限是宿主旋钮），**新增**语义：`Storage != nil` 且 `Spill == nil` 时内部合成 `&rig.SpillPolicy{Store: st.Spill()}`；`Spill.Store` 与 `Storage` 同时设置时 **Storage 优先并告警**（沿用「注入即失效」口径 `kit.go:665-688`） | 保留 | `spillStoreOf`（`kit.go:1382-1387`）会退化成「第二来源」：两个入口都能决定 `spill_read` 是否注册，容易出现「注册了工具但 `agent.spill` 用另一个 Store」的双实例 | `agent/spill_store_test.go:29,52`、`tools/builtin/spill_test.go:39` |
| `kit/kit.go:1382-1387` | `spillStoreOf(*rig.SpillPolicy) rig.SpillStore` | 纯取值 + nil 保护 | 加一条 `Storage` 回落，或改写为 `func spillStoreOf(cfg Config) rig.SpillStore`（签名变更） | 改写 | 改签名后 `kit.go:626` 唯一调用点必须同步；签名里的 `policy` 参数若不再使用就是死参数 | 无直接测试 |

### S2 `CheckpointStore`（`kit/kit.go:201`）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:201` | `Config.CheckpointStore rig.CheckpointStore` | 读点 1 处：`kit.go:874` → `rig.AgentSpec.CheckpointStore` → `agent.checkpointStore`（`agent/agent.go:127,177`）→ `recoverCheckpoint`/`saveCheckpoint` | 保留（可选覆盖）。`Storage != nil` 且字段为 nil 时取 `st.Checkpoints()` | 保留 | 两者都非 nil 时若只取一个，另一个静默无效——**必须二选一并告警**，否则「宿主的检查点写到了自己的库、恢复却读另一个库」 | `agent/checkpoint_integration_test.go:54,77,113` |
| `checkpoint.go:162` 的 `Clear` | `rig.CheckpointStore.Clear` | 接口三方法之一 | 不变 | 保留 | `kit`/`agent` **从不调用 `Clear`**（全仓零命中）——`agent` 靠 Save(completed) 幂等（`agent/checkpoint.go:239-250`）。Storage 接管后若仍无人调用，`Clear` 在 sqlite 侧会成纯契约装饰 | `session/sqlite`（对方范围） |

### S3 `History`（`kit/kit.go:231`）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:231` | `Config.History History` | 读点 5 处：`kit.go:755`（`history := cfg.History`）、`:870`（`spec.History`）、`:897`（`Agent.history`）、`:1247`/`:1251`（`CompactNow`） | 保留为**最低层覆盖**；`Storage != nil` 时取 `st.History()`。优先级：`Storage` > `SessionStore` > `SessionStoreDir` > `History` > `session.NewMemory()` | 保留 | 语义重叠：`History` 与 `Storage.History()` 是同一能力的两条注入路径，**没有任何检查**阻止同时设置 → 静默选中一个 | `kit/wiring_test.go:180-190`（互斥）、`kit/compact_test.go:34` |
| `kit/aliases.go:37` | `kit.History = rig.History` | 门面别名 | 建议**新增** `kit.Storage` 别名（`= storage.Storage`），否则宿主用 kit 时还得二次 import | 新增 | 无 | `kit/aliases_test.go:12`（只测构造器，需补别名覆盖） |

### S4 `SessionStoreDir`（`kit/kit.go:236`）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:236` | `Config.SessionStoreDir string` | 读点 3 处：`:758`（互斥）、`:764`（判空）、`:768`（`store.NewDir`）；错误路径 `:769-771` | `deprecated 保留一版`：内部转成目录适配实现（`storage.NewDirStorage` 或 kit 内私有适配）；下一版删除 | deprecated 保留一版 | 删除后 `kit` 对 `session/store` 的 import（`kit/kit.go:29`）**整条消失** → `store.NewDir` 在发布面只剩宿主代码调用；`store.Dir` 的「零依赖入门路径」（`docs/MODULES.md` 定位）实际迁到 `storage.NewDirStorage` | `kit/wiring_test.go:111-160,180-190,799-812,819-849`、`kit/audit_wiring_test.go:349-450` |
| `kit/kit.go:764-774` | 目录打开内联逻辑 | `store.NewDir` + `History(ctx,"default")`（`session/store/history.go:47`） | 换成 Storage 注入；会话 id `"default"` 的硬编码随之消失 | 改写 | 硬编码 `"default"` 若被 Storage 路径沿用，会成为「所有宿主共享一个会话」的隐患（PLATFORM §8.1 未决） | 同上 |

### S5 `SessionStore`（`kit/kit.go:241`）—— 唯一具体类型

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:241` | `Config.SessionStore *store.Dir` | 读点 2 处：`:757`（互斥）、`:761`（赋给 `storeDir`）；另 `:902` 写入 `Agent.store` | 改为**接口**。`store.Store`（`session/store/store.go:184`）**没有** `History()` → 二选一：① kit 内私有组合接口 `interface{ store.Store; History(context.Context,string) rig.History }`；② 加 `Store.History` 方法（改对方包）。PLATFORM §5 步 2 明示「类型断言或加方法」 | 改写 | 改成接口后 `Agent.store`（`kit/kit.go:928`）**只被 `SessionStore()` 访问器读**（`:1109`），而该访问器全仓调用者只有 2 个测试（`kit/audit_personal2c_test.go:98`、`kit/audit_product_defaults_test.go:159`）→ 字段+访问器成对变死（宿主 UI 才是真消费者，无法在仓内证明） | `kit/audit_product_defaults_test.go:143`、`kit/audit_personal2c_test.go:87` |
| `kit/kit.go:1105-1110` | `(*Agent).SessionStore() *store.Dir` | 返回具体类型 | 返回接口（或**直接删除**，由 `Storage()` 取代） | 改写 | 若只需 List/Load，`Storage` 里根本没有「列表」方法（只读五方法不含 List/Load-sessions，PLATFORM §3）→ **删掉它会让「最近会话侧栏」失去入口**，必须先给 storage 补 `Sessions()` 的决策，否则这是功能性倒退 | 同上 |

### S6 `SessionLog`（`kit/kit.go:254`）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:254` | `Config.SessionLog rig.SessionLog` | 读点 3 处：`:621`（→ `full.AllOptions.SessionLog`）、`:672-673`（`Tools != nil` 时的「注入即失效」告警）、`:983-988`（`cfgUnavailableTools` 摘 5 个 `session_*`） | 保留（宿主自定义日志），`Storage != nil` 且为 nil 时取 `st.SessionLog()`（**要求 sqlite 侧改返回 `rig.SessionLog`**，`session/sqlite/views.go:32`） | 保留 | 见 §0.2-C3：摘除分支在零配置下仍触发，**不是**死代码；真实风险是「Storage 提供的 log」与「宿主提供的 log」并存时无告警（`:672` 的告警条件只认 `cfg.SessionLog`） | `kit/toolfilter_internal_test.go:136,165`（钉死 `+5`）、`kit/audit_personal2c_test.go:571` |

### S7 `Meter`（`kit/kit.go:259`）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:259` | `Config.Meter *costmeter.Meter` | 读点 4 处：`:707`（`newEventBridgeFor`）、`:900`（`Agent.meter`）、`:1151-1157`（`setMeter`）、`:1216`/`:1229`（`watchCost` 读 `Session().CostUSD`） | 保留为**内存累加器 + 实时 UI 数据源**；`Storage != nil` 时新增「每次 Run 结算写库」路径（`Storage.Costs()` 或 `SaveCost`） | 保留 | 若写库由 eventBridge 直接做（`kit/wiring.go:191`），`Meter` 与库会各算一份；若只在 Run 结束时写，中途 crash 丢花费（与 PLATFORM §0「花费是可选能力」一致，可接受，但必须在文档写明） | `kit/audit_costmeter_test.go:21,98,137`、`kit/audit_2h_defaults_test.go:159` |
| `kit/kit.go:1216` | `watchCost` 前置条件 `a.meter == nil` | 没 Meter 就不做花费护栏 | 若 Storage 提供花费但宿主没传 Meter，护栏**静默失效** → 必须改为「Storage 存在时由 kit 内部建 Meter」 | 改写 | 改后 `MaxRunCostUSD` 的语义从「需要 Meter」变成「需要 Meter 或 Storage」，`kit.go:192-197` 的文档必须同步 | `kit/audit_2h_defaults_test.go:159` |

### S8 `Memory`（`kit/kit.go:268`）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:268` | `Config.Memory memory.Wiki` | 读点 5 处：`:594`（`memoryWiki := cfg.Memory`）、`:623`（→ `full.AllOptions.Wiki`）、`:905`（`Agent.memoryWiki`）、`:952`、`:963` | 保留；`Storage != nil` 且为 nil 时取 `st.Memory()`，并**跳过** `:596-602` 的自动建文件 wiki | 保留 | 跳过自动回落分支 → `memoryWikiErr` 恒为 `nil` → `(*Agent).MemoryWikiError()`（`kit.go:963`）退化成一个只返回 nil 的访问器（**仅剩签名**），其 UI 提示价值归零 | `kit/memory_wiki_test.go:49,65,103,121` |
| `kit/kit.go:947-948` | `Agent.memoryWiki / memoryWikiErr` | 自动回落失败的可诊断状态 | 保留字段；Storage 路径下 `memoryWikiErr` 恒 nil | 保留 | 同上；反向地，`Storage.Memory()` 自身的错误**没有**落点（必须新增 `storageErr` 字段，否则失败被吞） | 同上 |

### S9 `EventWriter`（`kit/kit.go:307`）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:307` | `Config.EventWriter io.Writer` | 读点 1 处：`:707` → `newEventBridgeFor`（`kit/wiring.go:76-94`）→ `events.NewEmitter(w)` | `deprecated 保留一版`：降级为**可选 NDJSON 导出通道**；事件主路径改为写 `Storage` | deprecated 保留一版 | 见 §5-E3：只接 Storage 的宿主 `eventBridge.emitter == nil` → `(*Agent).EventError()`（`kit/wiring.go:115`）永远 nil | `kit/audit_2h_events_test.go:59,167,235,281`、`kit/wiring_test.go:471,537,561`、`kit/audit_wiring_test.go:90,148,180,206,278` |
| `kit/wiring.go:76-94` | `newEventBridgeFor / newEventBridge` | 两个构造器，`nil` writer 时返回 nil 桥 | 需新增第三个参数（`rig.SessionLog` 写侧或 `storage.Storage`）；`newEventBridge`（`:89`）几乎必将删除或只服务导出 | 改写 | `newEventBridge` 是 `newEventBridgeFor` 的退化分支，仅被同文件调用 + `kit/audit_wiring_internal_test.go:142,217` 直接构造 → 改签名会波及 internal 测试 | `kit/audit_wiring_internal_test.go:142,217` |

---

## 3. `kit` 的零配置逻辑

### Z1 文件 wiki 默认路径

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:594-602` | `memoryWiki := cfg.Memory` + `memory.NewWorkspaceWiki("", root)` 自动建 | 零配置**会写盘**：`<stateDir>/memory/<workspaceID>`（`internal/memwiki/paths.go:97-111` → `:78-95` `DefaultStateDir`） | `Storage != nil` 时整段跳过；零配置（无 Storage）保留 | 保留（条件化） | 若不跳过，「Storage 的库」与「文件 wiki」两份记忆同时存在，模型看到第二份（`full.AllOptions.Wiki` 只收一个）——静默分叉 | `kit/memory_wiki_test.go:65`（钉死路径与跨实例可见）、`:103`（按工作区隔离） |
| `kit/kit.go:600` | `err` 分支回落到进程内 wiki | 装配失败不阻断 | 不变；但 Storage 路径失败时**没有**回落（见 S8 风险） | 保留 | `memoryWikiErr` 的语义需扩展为「自动回落失败 ∪ Storage 失败」 | `kit/memory_wiki_test.go:65` |

### Z2 `session.NewMemory()` 回落

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:775-777` | `if history == nil { history = session.NewMemory() }` | 零配置内存历史（`session/memory.go:77`） | 保留为最终兜底；但目标形态要求「内存聚合实现」（PLATFORM §5 步 2 明写**今天不存在、要新写**）→ 未来兜底应换成 `storage.NewMemoryStorage()` | 改写 | 若换成聚合内存实现，`session.NewMemory()` 在 kit 内**零调用** → `session.Memory` 只剩 `agent/subagent.go:228` 的 fork 用途（`seedForkHistory` 形参就是 `*session.Memory`） | `kit/audit_personal2c_test.go:87`（`SessionStore()==nil`）、`session/memory_test.go` |
| `session/memory.go:71-136` | `session.Memory`（`Load/Append/Replace/Len`） | 参考实现 | 保留 | 保留 | `Len`（`:132`）全仓非测试零调用（文档已注明「仅供测试与诊断」） | `session/memory_test.go` |
| `agent/subagent.go:228` | `seedForkHistory(ctx, target *session.Memory, ...)` | fork 种子**硬编码具体类型** | 这是与本次改造同类的「具体类型」问题，但不在 9 字段内；建议顺带记为技术债 | 保留 | 若 Storage 接管历史，fork 仍只写进程内 `*session.Memory` → 被 fork 的子会话**不持久化**（现状即如此，非本次引入） | `agent/subagent_test.go` |

### Z3 `withoutTool` / `cfgUnavailableTools` 的摘除清单

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:417-430` | `withoutTool(tools, name)` | 单名移除；`nil` 工具原样保留（留给 `agent.New` 报错） | 不变 | 保留 | 无 | `kit/toolfilter_internal_test.go` |
| `kit/kit.go:783-785` | 摘除循环 `for _, name := range cfgUnavailableTools(cfg)` | 应用摘除 | 不变的结构；**清单内容随注入点变化** | 保留 | 无 | `kit/toolfilter_internal_test.go:149`（钉死零配置 39 个） |
| `kit/kit.go:978-1018` | `cfgUnavailableTools(cfg) []string` | 摘除：`web_search`(980-982)、5 个 `session_*`(983-988)、`get_goal/create_goal/update_goal/plan_review/exit_plan_mode`(993-996)、`present`(997-1002)、`ask_user`(1003-1007)、`web_fetch`(1008-1012)、`pwsh`(1013-1016) | 需新增输入：`Storage != nil` 时 `session_*` 不再摘；新增 `spill_read` 的「可用性」判定（若 Storage 提供 Spill 则应从「不可用」转为「可用但 discoverable」） | 改写 | ① 5 个 `session_*` 分支**不会**永久不可达（见 §0.2-C3）；② 但若 kit 未来零配置也自动建内存 Storage，该分支就变成永久不可达 → 建议**保留分支、改写条件**为 `cfg.SessionLog == nil && cfg.Storage == nil`，让条件自身文档化 | `kit/toolfilter_internal_test.go:120-180`（39 个、`+5`） |
| `kit/kit.go:791` | `withoutTool(toolSet, "undo")` | 无 tracker 摘 undo | 不变 | 保留 | 无 | `kit/audit_personal2c_test.go:87` |
| `kit/kit.go:805` | `withoutTool(toolSet, discovery.ToolSearch)` | discovery 关时摘发现入口 | 不变 | 保留 | 无 | `kit/lazy_tools_test.go` |
| `kit/kit.go:1400-1405` | `coreActiveTools`（13 个） | 初始 active 集 | 不变。**不要**因为注入 Storage 就把 5 个 `session_*` 塞进常驻集——`internal/rigdoc/tokenbudget_test.go:306` 会因 schema 预算与 `TestEveryActiveToolIsWired` 变红 | 保留 | 无 | `internal/rigdoc/tokenbudget_test.go:133,189`、`internal/rigdoc/audit_4a_discovery_test.go:67` |
| `kit/kit.go:1488-1494` | `hostEnabledActiveTools(cfg)` | 目前只 `present` | 若未来把「接了 Storage ⇒ session_* 立刻可用」也写进来，需要同时改 tokenbudget 与 gate 白名单 | 保留 | 目前只返回 `present`，是**唯一**「宿主付了配置成本就立刻 active」的机制；不复用它会与既有产品口径不一致 | `internal/rigdoc/audit_4a_discovery_test.go:26` |

---

## 4. 互斥检查（现状 2 点 / 3 组合，见 §0.2-C2）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:757-760` | `if cfg.SessionStore != nil { if history != nil 或 cfg.SessionStoreDir != "" { … mutually exclusive } }` | 一条文案覆盖 2 个组合 | `SessionStore` 接口化后：若它被 `Storage` 取代，此检查**整个删除**；若二者共存，需扩成「Storage 与 SessionStore/SessionStoreDir/History **全部**互斥」四选一检查 | 改写 | 删除后 `History` + `SessionStore` 同时设置**不再报错**，会静默选中一个（顺序见 §2-S3 的优先级）→ 必须在 `Storage` 检查里补回等价断言 | `kit/wiring_test.go:180-190`、`kit/audit_wiring_test.go:349-360` |
| `kit/kit.go:764-767` | `SessionStoreDir` vs `History` 互斥 | 第二条文案 | 随 `SessionStoreDir` → deprecated，二者互斥语义转为「deprecated 字段与 Storage 互斥」 | deprecated 保留一版 | 若只加 `Storage` 检查而忘记旧字段，宿主可能同时写两个库（旧目录 + 新库）——**双写**比不写更糟 | 同上 |
| （不存在）`History` vs `History` | —— | 无第三条代码点 | 目标形态：单一 `Storage` 字段后，三处互斥检查**语义整体消失**（只剩「自定义 `Config.Tools` 时 Storage 不生效」这类横切告警） | 删除 | 全部删除后，`kit.go:229-241` 三段文档（约 13 行注释）也要一起改，否则注释承诺的报错不再发生（文档型死代码） | 无 |

---

## 5. `kit.Agent` 上与存储有关的字段/方法

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/kit.go:928` | `Agent.store *store.Dir` | 只被 `SessionStore()` 读（`:1109`） | 改接口或删除 | 改写 | **典型死字段**：`SessionStore` 接口化后若宿主改用 `Storage()`，此字段 + 访问器只剩测试调用（`kit/audit_personal2c_test.go:98`、`kit/audit_product_defaults_test.go:159`）。这与任务里给的例子一致：「改接口后字段无人读，仅剩赋值」——**赋值点只有 `kit.go:902`** | 同上 |
| `kit/kit.go:922` | `Agent.history rig.History` | 只被 `CompactNow` 读（`:1247,1251`），因为 `agent.Agent` 内部也持有一份（`agent/agent.go:119`） | 保留（`CompactNow` 需要它做 `HistoryReplacer` 断言） | 保留 | 两份 history 引用必须指向同一实例；若 Storage 路径下 `spec.History` 与 `Agent.history` 取自不同来源，`CompactNow` 压缩的是**另一份**历史 → 静默无效 | `kit/compact_test.go:34,82,98` |
| `kit/kit.go:924` | `Agent.events *eventBridge` | 运行期事件出口 | 保留；需能写 Storage | 改写 | 见 §5-E3 | `kit/audit_2h_events_test.go` |
| `kit/kit.go:926` | `Agent.meter *costmeter.Meter` | 花费护栏读数 | 保留 | 保留 | 无 | `kit/audit_2h_defaults_test.go:159` |
| `kit/kit.go:947-948` | `Agent.memoryWiki / memoryWikiErr` | 记忆后端 + 回落原因 | 保留（见 §2-S8） | 保留 | `memoryWikiErr` 在 Storage 路径恒 nil → `MemoryWikiError()` 死化 | `kit/memory_wiki_test.go:65` |
| `kit/kit.go:952-957` | `(*Agent).MemoryWiki() memory.Wiki` | 取记忆后端 | 建议改名为 `Storage()` 之外的**保留**访问器（导出/预置页面仍需） | 保留 | 若宿主改读 `Storage.Memory()`，此访问器只剩测试（`kit/memory_wiki_test.go:26,127`） | `kit/memory_wiki_test.go:121` |
| `kit/kit.go:963-968` | `(*Agent).MemoryWikiError() error` | 回落诊断 | 保留 | 保留 | Storage 路径恒 nil（见 §2-S8） | `kit/memory_wiki_test.go:70` |
| `kit/kit.go:1105-1110` | `(*Agent).SessionStore() *store.Dir` | 侧栏入口 | 改接口 / 删除 / 由 `Storage()` 取代 | 改写 | 删除即失去「最近会话列表」入口（见 §2-S5） | `kit/audit_product_defaults_test.go:159` |
| `kit/wiring.go:115-120` | `(*Agent).EventError() error` | 返回 `emitter.Err()` | 语义需扩展为「事件落库失败」或明确降级为「仅 NDJSON 通道」 | 改写 | 只接 Storage 时 `emitter==nil` → 恒 nil → **死访问器** | `kit/wiring_test.go:561` |
| `kit/kit.go:1243-1269` | `(*Agent).CompactNow(ctx, sid)` | 依赖 `Config.History` 实现 `rig.HistoryReplacer` | 依赖 `Storage.History()` 的实现满足 `HistoryReplacer` | 保留 | 若 Storage 视图不实现 `Replace`，此方法对 Storage 宿主**永远返回错误**（`kit.go:1249`）——必须在 `storage` 契约层加断言 | `kit/compact_test.go:34,82,98` |
| `kit/kit.go:1142-1196` | `(*Agent).Run` | 事件桥 + 花费护栏 + undo 快照 | 新增：会话租约获取/释放（若租约归 kit）、Run 结束时结算花费写库 | 改写 | 租约若放在 `agent.Agent` 而不在 kit，则自建 `agent.New` 的宿主绕过租约 → 「多进程」修复只覆盖 kit 路径 | `kit/audit_2h_events_test.go`、`kit/wiring_test.go:819-849` |
| `kit/kit.go:918-949` | `type Agent struct` | 15 个字段 | 需新增：`storage storage.Storage`（或 `st`）与 `storageErr error`；并新增 `Shutdown` 所需的 `jobs`/`terminals` 句柄（见 §6-S10） | 新增 | 不新增 `storage` 字段就无法实现 `Shutdown`（拿不到 Close 目标） | —— |

**`kit.Agent` 目前没有 `Close`/`Shutdown`**（实锤：全仓 `Shutdown` 只出现在 `tools/lsp`，`func (a *Agent) Close` 零命中）。

---

## 6. `agent/` 包

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `agent/agent.go:119` | `Agent.history rig.History` | `spec.History`（`:169`） | 不变；`kit` 用 `Storage.History()` 填 | 保留 | 无 | `agent/agent_test.go` |
| `agent/agent.go:126` | `Agent.spill *rig.SpillPolicy` | `spec.Spill`（`:176`）；使用点 `:944-953` | 不变 | 保留 | 无 | `agent/spill_store_test.go:29,52` |
| `agent/agent.go:127` | `Agent.checkpointStore rig.CheckpointStore` | `spec.CheckpointStore`（`:177`） | 不变；`kit` 用 `Storage.Checkpoints()` 填 | 保留 | 无 | `agent/checkpoint_integration_test.go` |
| `agent/agent.go:404` | `run.persist = a.history != nil && req.SessionID != ""` | 落盘总开关 | 不变 | 保留 | 若 Storage 接管但 `spec.History` 仍为 nil，`persist` 恒 false → 一片静默不落盘 | `agent/agent_test.go` |
| `agent/agent.go:412-416` | 调用点 `r.recoverCheckpoint()`（TurnID 赋值后、输入追加前） | 每个 Run 开头无条件恢复 | 需在恢复**之前**加租约检查：拿不到租约（同会话另一进程在跑）→ 跳过恢复且不写检查点，否则第二个进程会把第一个的活跃回合当崩溃残留 | 改写 | 顺序脆弱：若租约检查放在 `recoverCheckpoint` 之后，合成消息已写入历史，无法回退 → **必须把租约放在 `:408` 的 `loadHistory` 之前**（否则读到的历史已被污染） | `agent/checkpoint_integration_test.go:77` |
| `agent/checkpoint.go:126` | `RecoverInterruptedTurn(ctx, store CheckpointLoader, sessionID)` | 只读、确定性；`store nil`/空 session → `(nil,nil)`（`:134-136`）；`Load` 出错视为「无检查点」（`:137-145`） | 不变（纯函数，**不要**为租约改它） | 保留 | 依赖形状：只要求 `Load` 返回 `*rig.Checkpoint`（`checkpoint.go:78-81`），因此 `Storage.Checkpoints()` 满足即可，**不需要** `CheckpointStore.Save`；但它返回的 `RepairedCheckpoint`（`:111`）必须由调用方 `Save`（`agent/agent.go:687-692`）→ 只读契约不够，写侧必须存在 | `agent/checkpoint_test.go:117` |
| `agent/checkpoint.go:85` | `var _ CheckpointLoader = (rig.CheckpointStore)(nil)` | 编译期断言 | 建议**新增** `var _ agent.CheckpointLoader = storage.Storage{}`（在 storage 侧或测试侧），保证聚合契约的检查点视图兼容 | 新增 | 没有这条断言，`Storage.Checkpoints()` 的返回类型退回具体视图时不会被发现 | —— |
| `agent/agent.go:699-715` | `saveCheckpoint` + 三处调用（`:444` requested / `:478` completed / `:865` tool_pending） | fail-closed（`:713-714`） | 不变 | 保留 | 若 Storage 的 `Save` 没有 busy 重试，多进程下这里会把「database is locked」变成**回合失败**（PLATFORM §4-2 明确指出热路径 `AppendBatch` 无重试） | `agent/checkpoint_integration_test.go:113` |
| `agent/spill.go:23-79` | `spillToolResult` | `Store != nil` 走句柄（`:33-48`），否则文件（`:49-78`） | 不变 | 保留 | 文件分支在「宿主接 Storage」后依然被零配置用到（`Spill` 可为 nil），**不是死代码**；但 `Dir` 默认落系统临时目录（`:51`）与 0600 权限议题（PLATFORM §4-4）无关，属另一个未修项 | `agent/spill_store_test.go:52` |
| `agent/spill.go:42-44` | `policy.Store.GC(ctx, MaxFiles)` | Save 后按上限回收 | 不变；Storage 的 `Spill()` 必须实现 `GC` | 保留 | 若 Storage 的 Spill 视图 `GC` 是 no-op，`MaxFiles` 静默失效（注释已警告同一问题） | `agent/spill_store_test.go` |
| `agent/subagent.go:54,79,136-147,228-240` | `ParentHistory` / `parentHistory` / `seedForkHistory` | fork 用父历史 + 进程内 `*session.Memory` | 不改（与 9 字段无关），但会受「History 换实现」影响 | 保留 | `seedForkHistory` 的形参是 `*session.Memory`（具体类型）→ 父历史换实现后子会话历史仍在内存；这条技术债与 `SessionStore *store.Dir` 同源，建议一并登记 | `agent/subagent_test.go` |
| **S10** `kit` 缺 `Shutdown` | `(*kit.Agent).Shutdown(ctx) error` | **不存在** | `新增`。必须依次：取消在飞 Run → 排空事件 → `storage.Close()`（其内 `wal_checkpoint(TRUNCATE)`）→ 关闭后台任务 | 新增 | 顺序错则「回答到一半消失」（PLATFORM §4-1 原文） | 需新增 |
| **S10a** 后台任务句柄 | `tools/jobs.Manager` | `full.All` 内部 `jobs.NewManager`（`tools/full/full.go:181`）后**丢弃**；`kit` 不 import `tools/jobs`（0 命中） | 必须把 manager 暴露出来：`full.AllOptions.BackgroundJobs *jobs.Manager`（注入）或新增 `full.NewRuntime(opts) (tools []rig.Tool, rt Runtime)` | 新增 | `jobs.Manager.CloseAll`（`tools/jobs/jobs.go:232`）目前**只有测试调用**（`tools/builtin/*_test.go`、`tools/terminal/pty_test.go`）→ 生产路径从未关闭过任何后台进程 | `tools/jobs/jobs_test.go`（CloseAll 语义） |
| **S10b** 终端句柄 | `tools/terminal.Manager` | 同样在 `tools/full/full.go:235` 创建后丢弃 | 同 S10a，一并暴露 `Terminals` 或统一进 `Runtime` | 新增 | `terminal.Manager.CloseAll`（`tools/terminal/terminal.go:316`）同样只有测试调用 | `tools/terminal/pty_test.go:27,61,280` |
| **S10c** 租约契约 | —— | 全仓 `lease`/`Lease` 0 命中；无任何跨进程互斥 | `新增`（建议 `storage.LeaseAcquirer` 可选接口 + agent 侧薄封装；sqlite 侧实现） | 新增 | 契约放 `agent` 会导致 `storage` 反向依赖 agent（分层倒置）；放根包则根包无法引用 `storage` 类型——**只能放 `storage`** | 需新增 |

---

## 7. `tools/session_*` 五个工具：构造与注册路径

注册链路（实测，三跳）：

```
kit.New
  ├─ kit.go:621  full.AllOptions.SessionLog = cfg.SessionLog
  └─ kit.go:983-988  cfg.SessionLog == nil → 摘掉 5 个名字
tools/full/full.go:277   session.Tools(opts.SessionLog)      ← 无条件构造 5 个
tools/full/full.go:278   inspect.New(inspect.Options{Log: opts.SessionLog})
tools/session/tools.go:48  Tools(log) → searchTool/traceTool/{4 more}
gate/sandbox.go:59-60    5 个名字在只读白名单（免审批）
```

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `tools/session/tools.go:48-56` | `Tools(log rig.SessionLog) []rig.Tool` | 允许 `nil` log，工具仍构造、调用时报「no session log」（`:376` `requireLog`） | 保留。取数来源改为 `Storage.SessionLog()` | 保留 | 与 kit 的摘除机制**重复**：`nil` 降级（本行）与 `cfgUnavailableTools` 摘除（`kit.go:983`）是两套「缺失处理」，全量工具集（`full.All` 直接调用）会看到「注册但必然报错」的 5 个工具 | `tools/session/tools_test.go:49,215` |
| `tools/session/tools.go:16-22` | 5 个工具名常量 | 稳定模型可见标识 | 不变 | 保留 | 改名 = 不兼容 | `tools/session/tools_test.go:49` |
| `tools/session/tools.go:59-72` | `sessionTool`（含 `log` 字段） | 每工具持一份 `rig.SessionLog` | 不变 | 保留 | 无 | 同上 |
| `tools/session/tools.go:76/127/186/240/294` | `searchTool/traceTool/eventReadTool/eventSearchTool/eventTraceTool` | 5 个构造 + `run` 闭包 | 不变 | 保留 | 无 | `tools/session/tools_test.go:96,122,142,170,178` |
| `tools/session/tools.go:428-451` | `resolveSession`（按 seq 跨会话解析） | 依赖 `Search`/`Last` | 不变 | 保留 | 若 Storage 的 log 视图不实现 `Search`，`session_event_trace` 的省略-session 分支失效（`rig.SessionLog` 三方法都必须有） | `tools/session/tools_test.go:178` |
| `tools/session/memory.go:21-186` | `MemoryLog`（进程内 `rig.SessionLog` + `Sessions` 扩展） | 零依赖参考实现 | `deprecated 保留一版`：Storage 接管后它只剩「无头/测试」用途 | deprecated 保留一版 | 全仓非测试调用 **0**（只用 `tools/session/*_test.go`、`tools/inspect/inspect_test.go:54-55`）→ 改后需复核是否整型删除。**但它同时是 `tools/inspect.SessionLister` 的唯一实现**（`tools/inspect/inspect.go:53-55`、`tools/session/memory.go:63`） | `tools/session/memory_test.go:14,61,96,124,142,160` |
| `tools/session/doc.go:1-32` | 包文档 | 描述 `Tools(log)` 与降级 | 需补一句「有 Storage 时由存储提供 log」 | 改写 | 不同步 = 文档型死代码 | —— |
| `tools/catalog/catalog.go:50-51` | `Static` 中的 5 个名字 | 静态账本 | 不变（工具不消失） | 保留 | 若把 5 个工具改成「Storage 专属」，需要移到 `ServerBound`（`:82-92`），否则 `TestToolCatalogCoverage`（`tools/full/catalog_test.go:20`）双向比对失败 | `tools/full/catalog_test.go:20` |
| `tools/presets/presets.go:70-71` | personal 预设含 5 个名字 | 预设名单 | 不变 | 保留 | 若 5 个工具改成默认构造，`kit/toolfilter_internal_test.go:149` 的 **39** 与 `AGENTS.md` 的「39/55/49」三处数字都要改 | `kit/toolfilter_internal_test.go:149`、`internal/rigdoc/rigdoc_test.go:58` |

**结论（回应任务第 6 点）**：注入 Storage 后这 5 个工具**不会**变成死工具——它们是模型可见的检索入口，
Storage 只改「取数来源」。真正会变死的是 `tools/session.MemoryLog`（进程内实现），以及
`Tools(log)` 的 `nil` 降级分支（与 kit 摘除重复的那一套）。

---

## 8. `tools/memory`、`tools/builtin/spill.go`、`events.Emitter`

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `tools/memory/memory.go:22-29` | `ToolName` / `DefaultMaxOutputBytes` / `DefaultMaxReadBytes` | 模型可见常量 | 不变 | 保留 | 无 | `tools/memory/memory_test.go:49` |
| `tools/memory/memory.go:32-39` | 类型别名 `Page/PageMeta/Wiki = memwiki.*` | **别名**（`=`） | **必须继续保持别名**（见 §10） | 保留 | 若改成 `type Wiki interface{…}` 定义，`memory.Wiki` 与 `tools/memory.Wiki`、`memwiki.Wiki` 三者**互不兼容**，`kit.Config.Memory` 与 `storage.Storage.Memory()` 无法互通 | `tools/memory/memory_test.go`（全包） |
| `tools/memory/memory.go:42-58` | `NewFileWiki` / `NewMemoryWiki` / `NewWorkspaceWiki` | 三个构造器 | 保留；`NewWorkspaceWiki("", root)` 是 kit 零配置路径（`kit/kit.go:597`） | 保留 | 若 `storage` 自建内存实现，`NewMemoryWiki` 在 kit 内零调用（`full.go:368` 仍用） | `kit/memory_wiki_test.go:65` |
| `tools/memory/memory.go:90-106` | `Tools(wiki Wiki, opts...)` | 构造 `memory` 工具 | 不变 | 保留 | 无 | `tools/memory/memory_test.go` |
| `tools/memory/memory.go:108-200` | `WikiFromStore` + `storeWiki` | 扁平 `rig.MemoryStore` → wiki 的只读桥 | `deprecated 保留一版` | deprecated 保留一版 | 全仓零非测试调用（唯一入口 `full.AllOptions.Memory` `tools/full/full.go:110` 从不被 kit 设置）→ **疑似死代码**，改后需复核 | 无 |
| `tools/full/full.go:359-369` | `resolveMemoryWiki(opts)` | `Wiki` 优先，其次 `Memory`，否则 `NewMemoryWiki()` | 新增优先项：`opts.Storage` 的 `Memory()`；否则零配置会拿**进程内** wiki（与 kit 的文件 wiki 不一致！） | 改写 | 现状已有一个坑：`full.All` 直接调用时零配置 = 内存 wiki，而 kit 零配置 = 文件 wiki——「同一句话两个后端」，Storage 注入是修掉它的机会 | `internal/rigdoc/tokenbudget_test.go:306`（探针 memory） |
| `tools/builtin/spill.go:24` | `ToolSpillRead = "spill_read"` | 工具名 | 不变 | 保留 | 无 | `tools/builtin/spill_test.go:39` |
| `tools/builtin/spill.go:40-45` | `NewSpillRead(store rig.SpillStore) rig.Tool` | `store == nil` → 返回 nil（据此不注册） | 不变 | 保留 | 无 | `tools/builtin/spill_test.go:39` |
| `tools/full/full.go:260-262` | `if tool := builtin.NewSpillRead(opts.SpillStore); tool != nil` | **唯一**注册点，仅看 `AllOptions.SpillStore` | 必须新增：`opts.Storage != nil` 时也注册（`Storage.Spill()`） | 改写 | 若只在 `kit` 里合成 `SpillPolicy.Store` 而 `full.All` 不看 `Storage`，则「直接用 `full.All` 的宿主」用 Storage 时**没有** `spill_read` → 溢出结果永久丢失（本文件头注释 `:3-11` 正是这个警告） | `tools/full/catalog_test.go:18-19` |
| `tools/builtin/spill.go`（gate 侧缺口） | `spill_read` 不在 `gate.SandboxReadOnlyTools` | 白名单 `gate/sandbox.go:42-64`**不含** `spill_read` | Storage 自动开 Spill 后，模型可激活 `spill_read`，但默认 ask 档会**拒绝**它 → 新的「注册了但不可用」死工具 | 新增（补进白名单） | 现在没暴露是因为它从不进初始 active 集；一旦 Storage 让它常见，`internal/rigdoc/tokenbudget_test.go:306` 的同类断言（若把 `spill_read` 纳入 active）会红 | 需新增 |
| `events/emitter.go:39-46` | `events.Emitter` | NDJSON 串行写、seq 严格递增、写坏即粘性失败 | 保留（导出通道） | 保留 | 不是死代码，但若 Storage 成为唯一必需通道，`Emitter` 的 `Seq()`（`:131`）/`Err()`（`:138`）语义要重新定位为「导出通道状态」 | `events/emitter_test.go`、`events/audit_ndjson_test.go` |
| `events/emitter.go:50-57` | `NewEmitter(w io.Writer)` | `nil` writer → 全写失败（fail closed） | 不变 | 保留 | 无 | `events/emitter_test.go` |
| `kit/wiring.go:93` | `newEventBridge` 内 `events.NewEmitter(w)` | 唯一生产构造点 | 保留 + 新增 Storage 分支 | 改写 | 见 §2-S9 | `kit/audit_wiring_internal_test.go:142` |
| `kit/wiring.go:139-207` | `emitEvent`（21 个 kind 的映射） | 1 个事件 → 1 条 NDJSON | 目标：1 个事件 → 1 条 `rig.SessionEvent`（`Storage` 写侧）**并**可选 NDJSON | 改写 | 若两路都写而 seq 各自独立，`session_trace`（按 seq 排序）与 NDJSON 的 seq **不一致** → 排障时对不上号 | `kit/audit_2h_events_test.go:59`（seq 连续性） |
| `kit/wiring.go:115-120` | `(*Agent).EventError()` | 返回 emitter 首错 | 需重新定义（「事件写库失败算不算 turn error」= PLATFORM §8.4 **未决**） | 改写 | 决策未定就改会返工；建议先落 §8.4 再动手 | `kit/wiring_test.go:537,561` |
| `events/decoder.go` | `events.Decoder` | NDJSON 读回 | 保留（仍是导出格式的读侧） | 保留 | 不存在「反向」读库的等价物：Storage 的读侧是 `rig.SessionLog`，两者 payload 形状不同（`events.Envelope` vs `rig.SessionEvent`）→ 需要一份映射说明 | `events/decoder_test.go` |

---

## 9. `costmeter`：`Add/Session/Snapshot/Restore` 与将来的 `Storage.Costs()`

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `costmeter/meter.go:274` | `(*Meter).Add(model, usage)` | **唯一**写路径：`kit/wiring.go:191`（`case rig.EventUsage`） | 保留（内存累加）；新增「结算后写库」 | 保留 | 若写库改成「每次 Add 都写」，`Meter` 与库双写且可能不一致（库写失败时 Meter 已加） | `costmeter/meter_test.go`、`kit/audit_costmeter_test.go:21,98` |
| `costmeter/meter.go:387` | `(*Meter).Session() Summary` | 读点：`kit/kit.go:1229`（花费护栏）+ 宿主 UI | 保留 | 保留 | 无 | `kit/audit_2h_defaults_test.go:159` |
| `costmeter/meter.go:517` | `(*Meter).Snapshot() Snapshot` | **全仓非测试调用 0** | 目标是与 `Storage` 的持久化对接（注释 `:513-516` 明写「导出可持久化状态」） | deprecated 保留一版 | **已死**：`sqlite` 侧走的是 `CostEntryFromSummary`（`session/sqlite/cost.go:28`）+ `SaveCost`（`:63`），**不用** `Snapshot`。若 Storage 也用 SaveCost 路线，`Snapshot/Restore` 永久死；若改用 Snapshot 路线，`sqlite.SaveCost/CostTotals/CostByModel` 变死。这是**二选一的架构判断**，必须先定 | `costmeter/meter_test.go:252,293,670` |
| `costmeter/meter.go:552` | `(*Meter).Restore(Snapshot) error` | **全仓非测试调用 0** | 同上 | deprecated 保留一版 | 同上 | 同上 |
| `costmeter/meter.go:83` | `Snapshot` 结构（`CostNanoUSD` 等） | 持久化格式 | 若 Storage 走 Summary 路线，此结构失去消费者 | deprecated 保留一版 | 同上 | 同上 |
| `costmeter/meter.go:16` | `SnapshotVersion` | 快照版本 | 同上 | 保留 | 版本常量随结构一起死 | `costmeter/meter_test.go:299` |
| `costmeter/meter.go:452/462/475/488/501` | `Warned/Exceeded/Remaining/Budget/Reset` | 预算信号 | 保留（配额「只做机制」= PLATFORM §5 步 5，默认关） | 保留 | `Reset`/`Remaining`/`Budget` 在仓内只有自身测试调用（kit 只用 `Session().CostUSD`）→ 宿主 API，不能按仓内调用数判死 | `costmeter/warn_budget_test.go` |
| （对方范围，仅作对照）`session/sqlite/cost.go:63,90,111` | `SaveCost/CostTotals/CostByModel` | **零非测试调用者** | 目标：成为 `Storage.Costs()` 的实现 | 保留 | 目前是「表建好了没人写」——`docs/PLATFORM.md` §4-2 提到的「谁在写库」正是这个问题 | `session/sqlite/cost_test.go` |
| `costmeter/doc.go:1-40+` | 包文档 | 明写「持久化用 Snapshot.CostNanoUSD」 | 若改走 `CostEntry.CostUSD`（float64），**必须**改这段文档，否则精度承诺（「不要换成微美元」`doc.go:26`）与实现冲突 | 改写 | 文档与实现不一致 = 后续改动的错误向导 | —— |

**回答「谁在写库」**：**现在没有任何代码写花费库**。唯一写路径是 `Meter.Add`（纯内存），
`SaveCost` 在发布面零调用。

---

## 10. `memory/` 与 `internal/memwiki/`

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `memory/memory.go:16/19/24` | `Page` / `PageMeta` / `Wiki` **类型别名** | `= memwiki.*` | **保持别名**（`Storage.Memory()` 返回 `memory.Wiki`） | 保留 | 改成 `type` 定义会同时破坏：`session/sqlite/memory.go:19`（返回 `memory.Wiki`）、`tools/memory` 的别名、以及**模块外宿主的实现**（外部包只能通过 `memory.Page` 命名 `memwiki.Page`） | `memory/memory_test.go`、`session/sqlite/memory_test.go` |
| `memory/memory.go:27-50` | 上限常量与 4 个哨兵 | 规则唯一来源 | 不变 | 保留 | 无 | `memory/memory_test.go` |
| `memory/memory.go:56-78` | 7 个规则函数（`ValidateName`…`ClampText`） | 转发 memwiki | 不变 | 保留 | 无 | 同上 |
| `memory/`（导入方向） | **谁 import 根 `memory`** | 只有 `session/sqlite/memory.go:15`（+ 3 个测试）。**`kit` 不 import 它**——`kit` 用的是 `tools/memory`（`kit/kit.go:32`） | `storage` 必须 import **根 `memory`**（不是 `tools/memory`，否则 `storage` 依赖「工具层」违反分层）；`kit` 将同时 import `storage` 与 `tools/memory` → **`memory` 这个标识符在 `kit` 里已被 `tools/memory` 占用** → 必须起别名（如 `memstorage "…/memory"` 或统一改用 `storage.Storage.Memory()` 而不在 kit 里写类型名） | 新增（别名策略） | 别名写错 = 编译错；更隐蔽的是「`kit` 里 `memory.Wiki` 指的是 `tools/memory.Wiki`」这一既成事实若被新代码误读为根 `memory.Wiki`，**因为两者是同一别名而不会报错**，但 import 语义会误导后来者 | —— |
| `internal/memwiki/wiki.go:102-117` | `memwiki.Wiki`（6 方法） | 语义唯一来源 | 不变 | 保留 | 无 | `internal/memwiki/wiki_test.go` |
| `internal/memwiki/paths.go:78-95` | `DefaultStateDir()` | `$XDG_STATE_HOME/rig` → `~/.local/state/rig` | 不变；但「Storage 接管后零配置不再写盘」意味着此路径只在无 Storage 时用 | 保留 | 若 Storage 接管后 kit 不再调用，`DefaultStateDir` 只剩 `tools/memory.NewWorkspaceWiki` 的调用链（`tools/memory/memory.go:52`）→ 仍活 | `internal/memwiki/wiki_test.go` |
| `internal/memwiki/paths.go:97-111` | `WikiDir(stateDir, root)` | `<stateDir>/memory/<workspaceID>` | 不变 | 保留 | 见 §0.2 的 PLATFORM 更正：工作区分区口径三处不一致（PLATFORM §8.1 未决）——`store.Meta.Workspace` 重启即丢、sqlite 自动建会话写死 `workspace_id=''`、记忆按工作区目录 → **Storage 聚合后这三者必须在同一个分区键上**，否则「按工作区保留/统计」永远做不出来 | `kit/memory_wiki_test.go:103` |
| `internal/memwiki/file.go:26-64` | `FileWiki` / `NewFileWiki` | 落盘实现（0700/0600） | 保留 | 保留 | 无 | `internal/memwiki/wiki_test.go` |
| `internal/memwiki/memory.go:12-21` | `MemoryWiki` / `NewMemoryWiki` | 进程内实现 | 保留 | 保留 | 若 `storage` 新写「内存聚合实现」（PLATFORM §5 步 2），`MemoryWiki` 仍被它复用（`Memory()` 返回它）→ 不会死 | `internal/memwiki/wiki_test.go:293` |
| `internal/memwiki/api.go:11-25` | 5 个导出规则函数 | 给「包外后端」用 | 不变 | 保留 | 无 | `internal/memwiki/api_test.go` |

**导入方向结论**：`storage` → `memory` → `internal/memwiki`（单向，无环）。
禁止：`internal/memwiki` → `storage`；`memory` → `storage`（会让根契约层反向依赖聚合层）；
`storage` → `session/store`（见 §11 的 `NewDirStorage` 冲突）。

---

## 11. `internal/rigdoc` 守护清单 + `docs/ai`

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `internal/rigdoc/modularity_test.go:30-62` | `leafCapabilities`（31 个包） | 不含 `storage`、不含 `memory`、不含 `internal/memwiki` | **必须新增 `"storage"`**（PLATFORM §2-3 硬要求）。建议同时评估是否把 `"memory"` 加入（它现在是公开契约包却不在名单里 → 现有名单本身有漏项） | 新增 | 不加则「新包可被单独拿走使用」这条模块承诺对 `storage` 无守护；加入后 `storage` 一旦 import `kit`/`tools/full` 立刻红——这正是我们想要的边界 | `internal/rigdoc/modularity_test.go:65` |
| `internal/rigdoc/modularity_test.go:79-92` | `TestRootPackageStaysStdlibOnly` | 根包只允许 stdlib，且禁止 `modulePath+"/…"` | 不变。**它正是「聚合契约不能放根包」的执行者** | 保留 | 若有人把 `Storage` 塞进根包，会立刻红（PLATFORM §2-1 已实测「加一行 import 即红」） | 同文件 |
| `internal/rigdoc/modularity_test.go:96-116` | `TestKitIsTheOnlyAggregator` | 只有 kit 能同时 import `tools/full` + `provider/*` | 不变 | 保留 | `storage` 若 import `tools/full` 会先被叶子约束拦（需先加进 `leafCapabilities`） | 同文件 |
| `internal/rigdoc/surface_test.go:70-123` | `TestRemovedPackagesAreGone`（4 个已删包） | 目录 + import 双向扫描 | 不变；`storage` 是新包，**不**进这份「已移除」清单 | 保留 | 别把 `storage` 误加进 `removedPackages` | 同文件 |
| `internal/rigdoc/surface_test.go:165-174` | `TestKitConfigHasNoEscalation`（反射扫 `kit.Config` 字段名含 `escalat`） | 产品面守护 | 不变 | 保留 | 新增 `Config.Storage` 不会触发；但若字段名取 `StorageEscalation` 之类会被误伤 | 同文件 |
| `internal/rigdoc/rigdoc.go:39-56` | `BuildCatalog()` | `full.All(AllOptions{Policy, Root:".", SandboxGrants:true})` + `ptc.New` | **只要 `full.All` 的工具名集合不变，`docs/ai/tools.json` 与 `tools.md` 不变**。一旦 `storage != nil` 时新增/移动工具（例如把 `spill_read` 变成默认构造），必须重新 `go run ./cmd/rigdoc` | 保留（条件重生成） | `-check` 会红（`rigdoc.go:109-124`），这是好事；风险在于「有人手工改 `tools.md`」 | `internal/rigdoc/rigdoc_test.go:39` |
| `internal/rigdoc/rigdoc_test.go:58-102` | `TestDocumentationCounts` | 断言 `docs/TOOLS.md` 的静态工具数 == `len(full.All(...))`，且 `docs/COVERAGE.md`/`docs/zh/COVERAGE.md`/`CHANGELOG.md` 同步 | 若工具数变化（`spill_read` 上移、或 storage 带来新工具），**5 个文档**要同步 | 保留（条件改文档） | 数字漂移是最常见的「红在别人机器上」 | 同文件 |
| `internal/rigdoc/tokenbudget_test.go:306-400` | `TestEveryActiveToolIsWired` | 逐个探针调用 active 工具，禁止 "not configured"；并要求只读工具在默认档免问 | 若把 `session_*`/`spill_read` 纳入初始 active，**必须**同时改 `gate.SandboxReadOnlyTools`（`gate/sandbox.go:42`），否则红 | 保留（条件扩展） | `deadToolPatterns` 会捕获「接了 Storage 但没接线」的新死工具——这是本次改造最有价值的既有护栏 | 同文件 |
| `internal/rigdoc/tokenbudget_test.go:133,189` | `TestActiveToolsetTokenBudget` / `TestCoreToolSchemaBudget` | active 集 schema token 预算 | 不变（不动 coreActiveTools 就不变） | 保留 | 加常驻工具会直接超预算 | 同文件 |
| `docs/MODULES.md:81`（`memory` 行）、`:82`（`session*` 行）、`:83`（`session/sqlite` 行） | 文档模块清单 | 已有 `memory`/`session`/`session/store`/`session/sqlite` 行 | **必须新增 `storage` 一行**（PLATFORM §2-3 硬要求） | 新增 | 不加则 `docs/MODULES.md` 的「三条被测试守住的约束」表格与实际不符 | —— |

**新增文件建议**：见 §14-③。

---

## 12. `gate/`（只列与审批/存储相关）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `gate/sandbox.go:42-64` | `SandboxReadOnlyTools` | 24 个只读工具名；含 5 个 `session_*`（`:59-60`），**不含** `spill_read` | 若 Storage 让 `spill_read` 变成常见工具，必须补进名单 | 新增（补名） | 不补 → 「注册了但默认档必被拒」的新死工具（`internal/rigdoc/tokenbudget_test.go:392-396` 的文案正是这条教训） | `gate/audit_personal2c_test.go:308` |
| `gate/ask.go:297-356` | `AskGate.AllowAlways` / `ClearAllowAlways` / `AllowAlwaysTools` / `IsAllowAlways` | **进程内** map，不落盘 | 不改（审批决定不是会话状态；PLATFORM 未把审批纳入 Storage） | 保留 | 跨进程租约上线后，「allow-always」在多进程间不共享——同一工具在两个进程各问一次。可接受，但要在文档写明 | `gate/ask_test.go` |
| `agent/agent.go:405,516-518,832-853` | `run.allowAlways`（本次 Run 内） | Run 级缓存 | 不变 | 保留 | 无 | `agent/denial_test.go` |
| `kit/wiring.go:187-197` | `case rig.EventUsage` → `meter.Add` + `KindNotice` | 花费与用量进事件流 | Storage 写入侧要复用**同一处**（否则两处各算一次） | 改写 | 若新增一条独立写库路径而这里不动，用量会写两次或零次 | `kit/audit_costmeter_test.go:21` |

---

## 13. 测试清点（改哪些测试、哪些是「钉子」）

> 「钉子」= 改行为必红、必须同步改的断言。它们比代码更早暴露口径不一致。

| 位置（文件:行） | 符号 / 断言 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `kit/toolfilter_internal_test.go:149` | `base != 39` | 钉死零配置 **39** 个工具 | 工具数一变必须改；`AGENTS.md` 的「39/55/49」也要改 | 改写 | 数字不改 → 红；改了不复核 → 文档撒谎 | 本行 |
| `kit/toolfilter_internal_test.go:165` | `+ SessionLog 应 +5` | 钉死 5 个 `session_*` | Storage 注入路径要新增一条「+5」或改成「Storage 时 +5」 | 改写 | 同上 | 本行 |
| `kit/audit_wiring_test.go:349-450` | `TestAuditSessionStoreConfig`（三条契约）+ 单实例跨实例可见性 | 钉死 `SessionStoreDir` 行为 | `SessionStoreDir` deprecated 后保留；新增 `Storage` 版同构用例 | 改写 | 只测旧路径 → 新路径无覆盖 | 本行 |
| `kit/wiring_test.go:180-190` | History 与 SessionStoreDir 互斥报错 | 钉死互斥语义 | 三处互斥改为「Storage 四选一」，断言文案要更新 | 改写 | 同上 | 本行 |
| `kit/wiring_test.go:819-849` | 「同一 SessionStoreDir 上先 New 的 agent 看不到后建的会话」 | 已报告的接线缺陷 | Storage/单实例约束下**要么修、要么写成文档** | 改写 | 这是「单目录单实例」的取证用例，删掉就失去唯一证据 | 本行 |
| `kit/audit_personal2c_test.go:98` | `Agent.SessionStore() != nil` | 零配置无会话存储 | 改成 `Agent.Storage() == nil`（若访问器更名） | 改写 | `SessionStore()` 删除后此断言编译不过 | 本行 |
| `kit/audit_product_defaults_test.go:143-160` | `SessionStore() == st` 实例同一性 | 钉死「宿主传入的实例必须原样返回」 | Storage 取代后要保留「同一实例」语义（否则侧栏列表不全） | 改写 | 语义丢失 → 宿主侧栏不完整 | 本行 |
| `kit/memory_wiki_test.go:65,103,121` | 零配置文件 wiki 路径 / 按工作区隔离 / 注入优先 | 钉死零配置**写盘** | Storage 注入时跳过自动建 wiki；零配置仍写盘（PLATFORM §6 已更正旧说法） | 改写 | 若改成「零配置不写盘」，这 3 个用例全红 | 本行 |
| `kit/compact_test.go:34,82,98` | `CompactNow`（成功 / 空历史 / 非 Replacer） | 钉死 `HistoryReplacer` 断言 | Storage 视图必须满足 `HistoryReplacer` | 保留 | 见 §5 | 本行 |
| `kit/audit_2h_events_test.go:59,167,235,281` | seq 连续 / 审批可见 / 写失败 / 并发 | 钉死 NDJSON 语义 | 事件入库后这些断言的对象变成「NDJSON 导出通道」；需新增「入库 seq 连续」同构用例 | 改写 | 换了写入端但沿用旧断言 → 测的和跑的不是一条路 | 本行 |
| `kit/wiring_test.go:537,561` | 写失败不打断 Run / 无 EventWriter 时 `EventError()==nil` | 钉死「尽力而为」 | 需扩展到「入库失败是否 turn error」（PLATFORM §8.4 未决） | 改写 | 语义未定就改 = 返工 | 本行 |
| `kit/audit_2h_defaults_test.go:159` | `MaxRunCostUSD` 超限取消 | 钉死花费护栏依赖 Meter | 若允许「Storage 提供花费」，护栏前置条件变化 | 改写 | 见 §2-S7 | 本行 |
| `agent/checkpoint_integration_test.go:54,77,113` | 写检查点 / 恢复中断工具调用 / Save 失败 fail-closed | 钉死检查点形状（`Messages`+`PriorMessages`+`Phase`） | Storage 的 `Checkpoints()` 必须逐字段兼容 | 保留 | 形状漂移 → 恢复合成消息错位 | 本行 |
| `agent/spill_store_test.go:29,52` | Store 走句柄 / 无 Store 走文件 | 钉死两条分支 | 两条都要留（Storage 不能取代文件分支） | 保留 | 删文件分支 → 零配置溢出全丢 | 本行 |
| `tools/builtin/spill_test.go:39-93` | `spill_read` 构造/读回/未知句柄/缺参/截断 | 5 个用例 | `full.All` 增加 `Storage` 注册后需补「不传 Store 也不传 Storage 时不注册」 | 改写 | 漏测 → 零配置多出一个必然失败的工具 | 本行 |
| `tools/session/tools_test.go:49,215` | 目录覆盖 / `nil` log 降级 | 钉死「稳定工具目录」 | `nil` 降级与 kit 摘除重复（见 §7），需二选一 | 改写 | 两套降级都留着 → 行为不可预测 | 本行 |
| `tools/session/memory_test.go:14-184` | `MemoryLog` 六个用例 | 钉死进程内日志 | 若 `MemoryLog` deprecated，这些用例随之 deprecated | deprecated 保留一版 | 删实现留测试 = 编译不过 | 本行 |
| `internal/rigdoc/modularity_test.go:65` | 叶子约束 | 名单驱动 | 加 `"storage"` 后自动覆盖 | 新增 | 见 §11 | 本行 |
| `internal/rigdoc/rigdoc_test.go:39,58` | 生成物最新 / 文档计数 | 钉死 `docs/ai` 与 5 处数字 | 工具面变化时同步 | 保留 | 见 §11 | 本行 |
| `costmeter/meter_test.go:252,293,670` | Snapshot/Restore 往返与脏数据拒绝 | 唯一覆盖 `Snapshot/Restore` 的地方 | 若弃用，测试一并 deprecated | deprecated 保留一版 | 见 §9 | 本行 |

---

## 14. 三张汇总表

### ① 必删清单（改名/改接口后**确定**无人引用）

| # | 位置 | 符号 | 删除理由（可判定性） |
|---|---|---|---|
| 1 | `kit/kit.go:764-774` | `SessionStoreDir` 分支（目录打开内联逻辑） | `SessionStoreDir` 转 deprecated/删除后，这段 `store.NewDir`+`History(ctx,"default")` 无其他引用（`kit` 内 `store.NewDir` 仅此一处） |
| 2 | `kit/kit.go:29` | `import ".../session/store"` | 同上；删除后 `kit` 不再直接依赖 `session/store`（改由 `storage` 适配器承担） |
| 3 | `kit/kit.go:757-760` + `764-767` | 两条互斥报错（及 `kit.go:229-241` 约 13 行字段文档） | 单一 `Storage` 字段落地后，「三选一」语义不存在；不删则**永远不可达**（唯一可达路径是旧字段，旧字段已删） |
| 4 | `kit/kit.go:922` + `:897` 赋值 + `:1105-1110` | `Agent.history` 之外的 `Agent.store` 字段与 `SessionStore()` | 若以 `Storage()` 取代：唯一读取者是测试（`audit_personal2c_test.go:98`、`audit_product_defaults_test.go:159`），非测试消费在仓内不可证 → **需先确认宿主侧栏的替代入口**，确认后删 |
| 5 | `kit/wiring.go:89-94` | `newEventBridge(w, redactor)`（无 meter 的退化构造器） | 事件主路径改为 Storage 后，只剩 `newEventBridgeFor` 需要；本函数仅同文件 + `kit/audit_wiring_internal_test.go:142,217` 引用，改测试后可删 |
| 6 | `kit/kit.go:672-674` | 「`SessionLog` 注入即失效」告警分支 | `Storage` 与 `SessionLog` 合并后该组合不再存在（需同步 `kit/audit_config_conflict_test.go:86` 断言） |
| 7 | `tools/memory/memory.go:108-200` | `WikiFromStore` + `storeWiki`（6 方法） | 全仓**零非测试调用**，唯一入口 `full.AllOptions.Memory`（`tools/full/full.go:110`）从不被 kit 设置；`rig.MemoryStore` 一旦 deprecated 即可删 |
| 8 | `tools/full/full.go:359-369` 的 `Memory` 分支 | `resolveMemoryWiki` 里 `opts.Memory` 回落 | 同 7 |
| 9 | `agent/checkpoint.go` 的 `rig.CheckpointStore.Clear` 使用假设（非代码删除，而是**契约瘦身**） | `Clear` | `kit`/`agent` 零调用（靠 Save(completed) 幂等）。**不建议本轮删接口**（宿主可能已实现），但应在文档标注「Rig 自身不会调用」 |

> 说明：本清单刻意不含「改后才确定」的项 —— 那些进 ②。

### ② 疑似死代码（改了才确定，标注**改后需复核**）

| # | 位置 | 符号 | 为什么会死 / 为什么现在不定 |
|---|---|---|---|
| 1 | `kit/kit.go:928,1105-1110` | `Agent.store` + `SessionStore()` | 改接口后字段无人读、仅剩赋值；但宿主「最近会话侧栏」是仓外消费者，`storage.Storage`（只读五方法）**没有 List/Load-sessions** → 删之前必须先给 `storage` 补列表能力的决策 |
| 2 | `kit/kit.go:963-968` | `(*Agent).MemoryWikiError()` | Storage 路径跳过自动建 wiki ⇒ `memoryWikiErr` 恒 nil ⇒ 访问器恒返回 nil（仅剩签名）。**但反向风险**：Storage 自身错误需要新落点，否则失败被吞 |
| 3 | `kit/wiring.go:115-120` | `(*Agent).EventError()` | 只接 Storage 时 `emitter == nil` ⇒ 恒 nil。**改后需复核**：要么扩展为「存储写错」，要么明确「仅导出通道」 |
| 4 | `kit/kit.go:983-988` | `cfgUnavailableTools` 的 5 个 `session_*` 摘除 | **更正任务描述**（§0.2-C3）：零配置 `Storage == nil` 时**仍触发**，不是永久不可达。只有当 kit 零配置也自动建内存 Storage 时才真正死掉。改后需复核条件是否写成 `cfg.SessionLog == nil && cfg.Storage == nil` |
| 5 | `tools/session/memory.go:21-186` | `MemoryLog` | 非测试调用 0；但它是 `tools/inspect.SessionLister` 的唯一实现（`tools/inspect/inspect.go:53`）→ 删它会让 `inspect action=sessions` 失去枚举能力。改后需复核 |
| 6 | `tools/session/tools.go:376-383` | `requireLog` 的 `nil` 降级分支 | kit 走 `cfgUnavailableTools` 摘除、`full.All` 走 nil 降级 —— 两套并存。Storage 注入后 kit 路径永不进入 nil 分支，但 `full.All` 直连路径仍会 |
| 7 | `costmeter/meter.go:517,552` + `:83` | `Snapshot/Restore/ModelSnapshot` | 全仓非测试调用 0，且 sqlite 侧走 `CostEntryFromSummary`+`SaveCost` 另一条路线。**二选一**：走 SaveCost → 本组死；走 Snapshot → `sqlite.SaveCost/CostTotals/CostByModel` 死 |
| 8 | `costmeter/meter.go:475,488,501` | `Remaining/Budget/Reset` | 仓内只有自身测试；属宿主 API，不能按调用数判死。改后需复核（配额机制落地时可能被重新用到） |
| 9 | `agent/spill.go:49-78` | 文件落盘分支 | Storage 接管后**不会**死（零配置 `Spill==nil` 仍走这里）——列在此处只为防止有人「顺手删掉」 |
| 10 | `tools/builtin/spill.go` + `gate/sandbox.go:42` | `spill_read` 不在只读白名单 | Storage 自动开 Spill 后它会变成「可激活但默认必被拒」的死工具。改后需复核并补名 |
| 11 | `session/memory.go:132` | `(*Memory).Len()` | 文档已注明「仅测试与诊断」；若换成聚合内存实现需复核是否保留 |
| 12 | `docs/ai/tools.json` / `docs/ai/tools.md` | 生成物 | 工具面不变则不变；一变就 stale（`rigdoc -check` 红）。改后需复核是否重新生成 |

### ③ 需要新增的文件

| # | 新文件 | 内容 | 为什么必须新建 |
|---|---|---|---|
| 1 | `storage/doc.go` | 包文档：边界（§0 判据）、只读五方法起步、可选能力如何演进、与 `session/store`/`session/sqlite` 的关系 | 新公开包必须有中文包文档（仓规） |
| 2 | `storage/storage.go` | `type Storage interface { History() rig.History; Checkpoints() rig.CheckpointStore; Memory() memory.Wiki; Spill() rig.SpillStore; Close() error }` | 聚合契约**不能**放根包（stdlib-only 硬拦，`modularity_test.go:80`） |
| 3 | `storage/capabilities.go` | `Capabilities()` 结构 + 可选扩展接口（`SessionLogSource`、`CostSource`、`SessionLister`、`LeaseAcquirer`） | PLATFORM §5 步 0 要求 `Capabilities()`；且「可选能力」必须有**显式可选接口**而不是往主接口塞方法（否则第二个实现永远不达标） |
| 4 | `storage/memory.go`（或 `storage/memorystorage.go`） | 内存聚合实现（PLATFORM §5 步 2 明写「今天不存在，要新写」） | 零依赖宿主/测试的第二实现；`storagetest` 需要两个实现才能证明契约不偏 |
| 5 | `storage/lease.go` | 会话租约契约（`Acquire/Bump/Release` 或 `WithLease(ctx, sid, fn)`） | 多进程修复的契约层；放 `agent` 会倒置依赖（agent 不能反向依赖 storage 的具体类型） |
| 6 | `storage/storagetest/`（`storagetest.go` + `cases.go`） | stdlib-only 工厂 + 契约基线用例（两实现共用） | PLATFORM §5 步 0：只加测试、先立基线，否则「多实现分叉即红」是假的 |
| 7 | `storage/dirstorage.go` **或** `session/store/storage.go` | `NewDirStorage(*store.Dir) Storage` 适配器 | PLATFORM §3 明确要求；**但**放 `storage` 会让 layer 1 依赖 layer 2（违反 §2 分层表）。建议改放 `session/store`（`store` 不 import `storage`，无环），或独立 `storage/dirstore`。**这是本清单唯一需要架构决策的冲突项** |
| 8 | `kit/storage.go` | kit 侧的 Storage 装配胶水：9 字段优先级、EventWriter 降级告警、`Storage()` 访问器 | `kit.go` 已 1505 行，装配逻辑再塞进去不可维护 |
| 9 | `kit/shutdown.go` | `(*Agent).Shutdown(ctx) error` 与句柄收集（jobs/terminals/storage） | PLATFORM §4-1；顺序（取消 run → 排空事件 → checkpoint → close）必须集中一处 |
| 10 | `tools/full/runtime.go` | `NewRuntime(opts) (Runtime, []rig.Tool)`，`Runtime` 持 `Jobs *jobs.Manager`、`Terminals *terminal.Manager`、`Storage` | `full.All` 丢弃 manager（`full.go:181,235`）是 `Shutdown` 不可能的根因 |
| 11 | `agent/lease.go` | Run 级租约薄封装（在 `runTurn` 早于 `loadHistory` 获取） | 见 §6 的顺序风险 |
| 12 | `docs/PLATFORM-INVENTORY-B.md`（对方） | —— | 与本文对应的 store/sqlite 清单 |

---

## 15. 规模估算

| 类别 | 文件数 | 符号数 | 说明 |
|---|---|---|---|
| **新增** | 11–13（含 `storage` 6–7 个、`kit` 2 个、`tools/full` 1 个、`agent` 1 个、`storage/storagetest` 1–2 个） | 约 14 个类型/接口 + 7 个函数（`Shutdown`/`NewRuntime`/`NewDirStorage`/`NewMemoryStorage`/lease 3 个） | `storage` 是全新包，估 600–900 行（含文档） |
| **改写** | 12（`kit/kit.go`、`kit/wiring.go`、`kit/aliases.go`、`agent/agent.go`、`agent/checkpoint.go`、`tools/full/full.go`、`tools/memory/memory.go`、`gate/sandbox.go`、`internal/rigdoc/modularity_test.go`、`kit/aliases_test.go`、`costmeter/doc.go`、`memory/` 无） | 约 22 个符号 | `kit/kit.go` 单文件约 10 处（9 字段 + 互斥 + Agent 字面量） |
| **删除** | 6（`kit/kit.go`、`kit/wiring.go`、`tools/memory/memory.go`、`tools/full/full.go`、`kit/audit_config_conflict_test.go` 断言、`kit/aliases.go` 无） | 约 9 个符号/分支 | 见 §14-① |
| **deprecated 保留一版** | 8（`kit/kit.go`、`kit/audit_wiring_test.go`、`tools/session/memory.go`、`tools/memory/memory.go`、`costmeter/meter.go`、`costmeter/meter_test.go`、`rig/extensions.go`、`kit/audit_2h_events_test.go`） | 约 12 个符号 | 每次只破坏一处（PLATFORM §5 发版节奏） |
| **测试改动** | 17 个测试文件 | 约 25 个用例 | 其中 **8 个是钉子**（改行为必红）：39 计数、+5、互斥、SessionStore 实例性、零配置 wiki 路径、seq 连续、检查点形状、`docs/ai` 计数 |
| **文档** | 7（`docs/MODULES.md`、`docs/PLATFORM.md` 不变、`docs/ai/tools.json`、`docs/ai/tools.md`、`docs/TOOLS.md`、`docs/COVERAGE.md`、`docs/zh/COVERAGE.md`、`CHANGELOG.md`） | —— | 数字类文档必须与工具面同批改 |
| **合计** | 约 55 个文件触点 | 约 85 行清单项 / 约 60 个符号 | 按 PLATFORM §5 的四次小版本切分，每次 10–15 个文件 |

---

## 16. 顺序依赖（不按此顺序会编译不过或返工）

| 序 | 必须先做 | 否则 | 证据 |
|---|---|---|---|
| **D1** | 新建 `storage` 包（接口 + 类型别名引用根 `memory`） | `kit` 无处可 import | 根包 stdlib-only（`modularity_test.go:80-92`） |
| **D2** | `session/sqlite` 的 `SessionLog()` 改返回 `rig.SessionLog`（**对方范围**） | `var _ storage.Storage = (*sqlite.Store)(nil)` 编译不过（PLATFORM §3 缺口表第 1 行） | `session/sqlite/views.go:19-32` |
| **D3** | `storage` 先只落**只读五方法** | 塞 `Sessions()/Costs()` 会立刻撞 PLATFORM §3 的三个「已否决」项（环、缺方法、同名不同签名） | `docs/PLATFORM.md:80-88` |
| **D4** | `storage.NewDirStorage` 的**归属**先定 | 放 `storage` ⇒ layer 1 依赖 layer 2（违反 §2 分层表）；放 `session/store` ⇒ 需对方配合 | §14-③-7 |
| **D5** | `kit.Config.SessionStore` 接口化（步 2）**之后**才能删 `SessionStoreDir` | 宿主无迁移路径，直接破坏（PLATFORM §5 的「四次小版本」理由） | `docs/PLATFORM.md:117` |
| **D6** | `Shutdown` / `wal_checkpoint` / `Backup`（步 3）在写路径收编（步 4）**之前** | 写路径收编后事件/花费都进库，此时没有关机收尾 = 「回答到一半消失」+ WAL 未 checkpoint（PLATFORM §4-1） | `docs/PLATFORM.md:112-113` |
| **D7** | 先定「事件写失败算不算 turn error」（PLATFORM §8.4 **未决**） | `EventError()`（`kit/wiring.go:115`）与 `emitEvent`（`:139`）的语义会改两遍 | `docs/PLATFORM.md:147` |
| **D8** | 先定花费持久化走 `Snapshot` 还是 `CostEntry` | `costmeter.Snapshot/Restore` 与 `sqlite.SaveCost` **必有一组**变死代码，且 `costmeter/doc.go:26` 的精度承诺与 `CostEntry.CostUSD`（float64）冲突 | `costmeter/meter.go:517,552`、`session/sqlite/cost.go:28,63` |
| **D9** | `full.All` 接受 `Storage`（或 `Runtime`）**之前**不要改 `tools/session`/`spill_read` 的注册逻辑 | 工具层改了但装配层没改 ⇒ 两处口径不一致，`TestToolCatalogCoverage` 与 `TestEveryActiveToolIsWired` 双红 | `tools/full/full.go:260,277` |
| **D10** | 租约在 `runTurn` 的 **`loadHistory` 之前**（`agent/agent.go:408` 之前）获取 | 放在 `recoverCheckpoint`（`:413`）之后 ⇒ 第二个进程已把合成消息写进历史，无法回退 | `agent/agent.go:408-417` |
| **D11** | `leafCapabilities` 加 `"storage"` 与 `docs/MODULES.md` 加一行，与 `storage` 包**同批**提交 | `go test ./internal/rigdoc` 绿但模块表漏项（PLATFORM §2-3 要求）；反之先加名字后建包会红 | `internal/rigdoc/modularity_test.go:30`、`docs/MODULES.md:81` |
| **D12** | `go run ./cmd/rigdoc` 与数字类文档放在**最后一批** | 任何工具面变动都会让 `rigdoc -check` 与 `TestDocumentationCounts` 红，中途跑等于噪音 | `internal/rigdoc/rigdoc.go:109`、`rigdoc_test.go:58` |

---

## 17. 附：一句话结论（给评审）

1. `Storage` **只能**是新包：根包 stdlib-only 已实测硬拦（`modularity_test.go:80-92`）。
2. 9 个字段里**只有** `SessionStore` 真正阻碍换实现；`Spill`/`Meter` 是「策略/累加器」，
   `History`/`CheckpointStore`/`SessionLog`/`Memory`/`EventWriter` 已是接口或 stdlib 接口。
3. 三处互斥检查实际是**两点代码**；改单一字段后语义整体消失，**不能只删代码不删注释**。
4. `kit.Agent` 没有 `History()`、没有 `Close`/`Shutdown`；`jobs.Manager` 与 `terminal.Manager`
   在 `full.All` 里创建后被丢弃，`CloseAll` 只有测试调用 —— 这是 `app.Shutdown()` 的**唯一硬阻塞**。
5. 5 个 `session_*` **不会**变死工具（它们是模型可见入口），会变死的是 `tools/session.MemoryLog`
   与 `WikiFromStore`。
6. `costmeter.Snapshot/Restore` 现在就是死代码（仓内零调用），而 `sqlite.SaveCost` 是
   「表建好了没人写」——**花费持久化路线必须先决策，再动手**。