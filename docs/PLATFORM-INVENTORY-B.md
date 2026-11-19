# 平台化清点 B：`session/store` · `session/sqlite` · `session/memory` · `session/compact` · CI/发布面

> 配套文档：[PLATFORM.md](PLATFORM.md)（v2，唯一来源）。
> 本文件只清点**B 范围**（另一人负责 `rig`/`kit`/`agent`/`tools`）：
> `session/store/`、`session/sqlite/`（独立 module，含 `go.mod`/`go.sum`/全部测试）、
> `session/memory.go`、`session/compact/`、`.github/workflows/ci.yml` 的子模块与 consumer-smoke 步骤、
> `docs/sqlite.md`、`docs/MEMORY.md`、`docs/RELEASE.md`。
>
> **本文件只新建、不改任何 `.go`、不 commit。**

## 0. 核实方法与基线

- 基线 commit：`c536811`（`docs(platform): 重写为 v2`），工作树干净。
- 工具链：`go1.27.3 linux/amd64`，`CGO_ENABLED=0`。
- 本文件里**每一条**都来自下面四类证据之一，没有推断：
  1. 逐文件 `read`（带行号）+ `grep -n` 复核符号位置；
  2. `go doc -all ./session/store`、`go doc -all .`（`session/sqlite`）确认**导出面**（21 个 `*Store` 方法 + `Open`/`ImportDir`）；
  3. **编译探针**（`/tmp/probe-iface`）：验证 `docs/sqlite.md` 声称的接口实现关系，实测**4 条全假**（见 §10 的 `docs/sqlite.md:154-158` 那一行）；
  4. **真实复现** `sqlite-consumer-smoke`（见 §8 的 `ci.yml:321-382` 复现行）：用 CI 同一段脚本在临时 module 里跑，实测失败并拿到确切报错。
- 已确认的"不存在"（grep 全仓 0 命中，`.audit/` `.audit-scratch/` `.probe/` 等 gitignore 目录除外）：
  `storage` 包、`ALTER`、`wal_checkpoint`、`VACUUM`、`Chmod`/`0o600`（`session/sqlite` 内）、租约相关符号（`lease`/`租约`）；
  配额/`Prune` 在**本范围两包内**也是 0 命中（仓里只有 `tools/undo`、`internal/gitsnap` 有同名的 git 快照清理，与本议题无关）。

## 1. `store.Store` 的方法面与 `store.Dir` 的"只有目录实现有"的方法

新契约（PLATFORM §3）只有五个方法：`History() rig.History` / `Checkpoints() rig.CheckpointStore` /
`Memory() memory.Wiki` / `Spill() rig.SpillStore` / `Close() error`。**`store.Store` 的 7 个方法里只有
`Close` 进契约**，其余全是"写路径 + Meta 视图"，而 Meta 视图**不能**进 `storage`（会与 `session/store` 互相 import 成环）。

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/store/store.go:184-199` | `type Store interface` | 7 方法：`Create`/`Append`/`AppendBatch`/`Load`/`List`/`Delete`/`Close`，全部返回 `Meta`，参数带 `store.Filter` | **不改成** `storage.Storage`；保留为"目录后端自有接口"。聚合契约另建 `storage.Storage`（只读五方法） | 保留 | 中：`sqlite.Store` 为了满足它专门保留 `Append`/`Replace` 两个"返回 Meta"的方法，与新契约的写法并存，读者会以为两套都能当"存储契约"用 → 改后需复核（`contract.go:36` 的断言还要不要留） | `session/store/store_test.go:22`（`var _ store.Store`）、`session/sqlite/more_test.go:164`、`session/sqlite/contract.go:36` |
| `session/store/store.go:186` | `Store.Create` | 建空文件 + 入索引，返回 `Meta`（写路径） | 新契约无写方法（PLATFORM §5 步 4 才收编写路径）；仍在具体类型上 | 保留 | 低 | `store/store.go:496` 的 `Dir.Create` 实现；`store_test.go:82,643` |
| `session/store/store.go:188` | `Store.Append`（单条） | 一把 `rig.Message` | PLATFORM §3 明确"契约里一律用 `AppendBatch`，**不要**定义变参 `Append`" | deprecated 保留一版 | 中：`sqlite.Store.Append` 与 `store.Dir.Append` 都只是 `AppendBatch` 的一行转发，收编写路径后无人调用 → 改后需复核 | `sqlite/sessions.go:96`；`sqlite/fix20_test.go:53`（并发闭包内 `db.Append`） |
| `session/store/store.go:190` | `Store.AppendBatch` | 整批原子写 + Sync | 写路径的**规范名字**；`rig.History.Append` 与它同形（变参）→ 未来写契约沿用这个名字 | 保留 | 低 | `store_test.go:421`、`store/audit_store_test.go:338` |
| `session/store/store.go:192` | `Store.Load(ctx,id) (Meta, []rig.Message, error)` | 同时给"元数据 + 消息" | 消息读取走 `History() rig.History`（丢 Meta）；Meta 读取**必须留在具体类型**（见下一行） | 保留 | 中：与 `rig.History.Load` 同名不同签名，两个接口并存，`storage.Storage.History()` 只暴露前者 | `store_test.go:82,791`；`sqlite/sessions.go:176` |
| `session/store/store.go:194` | `Store.List(ctx, filter Filter)` | 侧栏列表 + Workspace/Since/Limit 过滤 | **不能进 `storage`**：`storage` 要引用 `store.Filter`/`store.Meta` 就成环（PLATFORM §3 "不要做的事"第 1 条） | 保留 | 中：新架构里"会话列表"没有契约位置，宿主必须拿具体类型 → 与"宿主只写业务"的宣言有落差，需在 PLATFORM 里显式列为"未收编" | `store_test.go:153`、`layout_test.go:155`、`sqlite/sessions.go:232` |
| `session/store/store.go:196` | `Store.Delete` | 删文件 + 删索引 | 同上，不进契约 | 保留 | 低 | `store_test.go:591`、`sqlite/sessions.go:284` |
| `session/store/store.go:198` | `Store.Close` / `Dir.Close` (`store.go:773-778`) | 只把 `closed` 置 true，**无 flush、无 fsync、无 checkpoint**（目录实现没有 WAL，这是对的） | 映射到 `storage.Storage.Close`；但语义要升级成"排空后再关"（PLATFORM §4-1 的 `Shutdown`） | 改写（仅语义/文档，方法签名不动） | 低（但**与 `sqlite.Close` 同名同义不同实现**，宿主会误以为都会有 checkpoint） | `store_test.go:692`（`TestClosedStoreRejectsEveryOperation`）；`sqlite/fix20_test.go:39` |
| `session/store/store.go:490` | `Dir.Dir() string` | 返回 root 绝对路径（备份/诊断用） | 不进契约；**唯一内部消费者是 `History.String()`**（`history.go:116`） | 保留 | **高**：除 `String()` 与测试外全仓 0 调用（`grep -rn '\.Dir()'` 只命中 `history.go:116`、`store/audit_store_test.go:34`、`store/history_test.go:40`，其余是 `memwiki.FileWiki.Dir()`）→ 改后需复核是否降为 `//go:build` 测试助手 | `store/audit_store_test.go:34`、`store/history_test.go:40` |
| `session/store/store.go:740-770` | `Dir.Replace(ctx,id,msgs) (Meta,error)` | 原子整窗替换（tmp+rename+fsync dir），压缩路径的地基 | 对外只经 `History.Replace`（`rig.HistoryReplacer`）暴露；具体类型方法保留给 kit 之外的调用方 | 保留 | 中：`storage.Storage.History()` 返回 `rig.History`，宿主若要 `Replace` 必须再断言 `rig.HistoryReplacer`（PLATFORM 未写这一步）→ 改后需复核 | `store/audit_store_test.go:49,558`；`store/history_test.go:100` |
| `session/store/history.go:47` | `Dir.History(ctx, id) *store.History` | 带 ctx + 默认会话 ID，返回**具体类型** | 与 `storage.Storage.History() rig.History`（无参、无默认 ID）**签名不兼容** → 用适配器 `storage.DirStorage{dir, defaultID}` 提供无参 `History()`；`Dir.History` 本身保留（`kit` 在用） | 保留 + 新增适配器 | 高：两个 `History` 方法并存（一个有 ctx/id、一个没有），并且 `store.History` 是导出具体类型 → 宿主很容易把 `DirStorage.History()` 的结果断言回 `*store.History`。建议适配器返回**未导出**包装类型 | `store/history_test.go:21-190`（6 个用例全部调 `s.History(ctx,…)`）；`kit/kit.go:762,773`；`kit/wiring_test.go:148` |
| `session/store/history.go:32-35` | `type History struct{store *Dir; id string}` | 导出结构 + 未导出字段；实现 `rig.History`/`rig.HistoryReplacer`（`history.go:38-41` 断言） | 保留；新契约只需 `rig.History`，`Replace` 由断言发现 | 保留 | 中：`History` 的 `store` 字段是 `*Dir`（具体类型），任何"多个存储实现"的说法到这里就断了 | `store/history_test.go:21,58,100,153,171,190` |
| `session/store/history.go:60` | `History.Load` 把 `ErrNotFound` 吞成 `(nil,nil)` | 契约要求 | 不变（`sqlite historyView.Load` 同语义，`views.go:44-50`） | 保留 | 低 | `store/history_test.go:21`；`sqlite/views_test.go:18` |
| `session/store/history.go:104-109` | `History.ensureSession` | `Append` 遇 `ErrNotFound` 时自动 `Create`（`history.go:84`） | 不变；但写契约收编后应与租约/多进程语义一起复核（自动建会话是"两个进程都建同一个 ID"的来源） | 保留 | 中 → 改后需复核 | `store/history_test.go:153` |
| `session/store/history.go:112-117` | `History.String()` | 唯一使用 `Dir.Dir()` 的地方 | 若 `Dir.Dir()` 降级，这里要改成缓存 root 或删掉 | 保留 | 中 | 无（仅日志/诊断） |

## 2. `store.Dir` 的元数据丢失：谁负责持久化 `Meta.Workspace` / `Messages` / `Title`

`store.Dir` 的索引**只在内存里**（包文档 `store.go:22-23`、`store.go:259-260`），重启靠 `scan()` 重建，
`scan()` 只填 4 个字段（`store.go:340-345`）。逐个字段：

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/store/store.go:345`（`Meta` 字面量 `340-345`） | `Dir.scan` 构造的 `Meta` | 只赋 `ID`/`CreatedAt`/`UpdatedAt`(=mtime)/`Bytes`(=size) → **`Workspace`、`Model` 恒为空串**，`Messages` 恒为 0 | 需要"元数据落盘"。二选一：① 新增 sidecar `<dir>/<id>.meta.json`（唯一键 = 会话 ID，`fileStem` 复用）；② 在契约里把 `Workspace` 声明为**不持久化**（`Capabilities().WorkspacePersistent=false`），把分区口径统一交给 sqlite | 改写 | 高：如果不选①，"按工作区保留/统计"在目录后端永远不可实现，而 PLATFORM §8-1 已把它列为开放决策 | `store/store_test.go:207`（`TestReopenSeesSessionsFromFiles` 只断言 `Messages`/`Title`，**故意没断言 Workspace**——这就是缺口被测试放过的地方） |
| `session/store/store.go:153-156` | `Meta.Workspace` + 注释"只存在于内存索引里，重启后无法从文件恢复（留空）" | 文档化的已知丢失 | 同上；注释必须随实现一起改，否则成为"文档说丢、代码其实存了"的反向漂移 | 改写（注释） | 中 | 同上 |
| `session/store/store.go:157-158` | `Meta.Model` | 同样重启即丢，且 `sqlite` 侧**也从不写**（`sessions.go:81-83` 的 INSERT 没有 `model` 列，`rig_sessions` 表根本没有该列 → 见 `schema.go:24-35`） | 若要持久化，`rig_sessions` 需 **ALTER ADD COLUMN**（正好是 §4 的 ALTER 路径第一个真实用户）；否则归档为"不维护"，并在 `docs/sqlite.md:137-152` 保留说明 | 保留（按"不维护"归档）或 改写（加列） | 中 | `sqlite/fix20_test.go:274`（`TestMetaBytesAndModelAreDocumentedAsUnset` 把"恒为 0/空"钉死，改实现必须同时改它） |
| `session/store/store.go:161-165` | `Meta.Messages` | 重启后为 0，等第一次 `Load` 才补（`store.go:655-659` 的 `correctMeta`） | 不变（这是有意的启动成本取舍）；新契约里 `rig.History` 拿不到条数 → 侧栏若需要条数，必须 `List`（具体类型）或从 `Load` 自己数 | 保留 | 中：`rig.History` 契约里没有"条数"，宿主换成 `storage.Storage` 之后**条数信息彻底消失** → PLATFORM 需显式承认这个能力回退 | `store/store_test.go:207`、`layout_test.go:63` |
| `session/store/store.go:346-349` | `scan()` 里的 `if d.scanContents { meta.Title = d.scanTitle(id) }` | **默认开启**：`Title` 重启后**是能恢复的**（读开头 ≤256 KiB / ≤4 条，`store.go:365-434`）。只有 `WithScanContents(false)` 或扫描预算耗尽时才为空 | 保留；但要纠正 PLATFORM §8-1 的表述——"`Meta.Workspace` 重启即丢"正确，"`Title` 也丢"只有关掉扫描时成立 | 保留 | 低 | `store/scan_test.go:39,99`；`store/fix_titles_test.go:31,70` |
| `session/store/store.go:558` | `d.index[meta.ID] = meta`（`Create` 末尾） | 全仓**唯一**把调用方传入的 `Workspace`/`Model` 写进索引的地方（内存） | 若加 sidecar：这里要落盘；若不加：这里就是"数据进内存即止"的证据点 | 改写 / 保留 | 高 → 改后需复核 | `store/audit_store_test.go:420`、`fix_crossinstance_test.go:48` |
| `session/store/store.go:890-897` | `Dir.recoverIndex` 的 `Meta` 字面量 | 与 `scan()` 同样丢 `Workspace`/`Model`（跨实例兜底路径） | 与 `scan()` 共用同一个"从磁盘重建 Meta"的构造函数，避免两处各丢一次 | 改写 | 中 | `store/fix_crossinstance_test.go:17,48,82` |
| `session/store/store.go:819-838` | `Dir.commit` | `Append`/`Replace` 后只更新 `Messages`/`Bytes`/`UpdatedAt`/`Title`，不碰 `Workspace`/`Model` | 若持久化，这里要写回 sidecar（写放大：每批消息一次额外小写；可只在 Create/显式 SetMeta 时写） | 改写（或保持） | 低 | `store/audit_store_test.go:49` |
| `session/sqlite/sessions.go:116-118`（`INSERT ... VALUES(?, '', '', ?, ?)`） | `AppendBatch` 自动建会话时写死 `workspace_id=''` | 目录后端丢、sqlite 后端也丢（**自动建会话路径**），只有显式 `Create(meta{Workspace})` 才写对（`sessions.go:81-83`） | 保持"自动建会话不带业务字段"，但契约上必须写清："`Workspace` 只在 `Create` 时给"；否则宿主以为 `AppendBatch` 之后能查到工作区 | 保留 + 文档 | 中 | `sqlite/store_test.go:44`、`sqlite/more_test.go:22` |
| `session/sqlite/schema.go:24-35` | `rig_sessions` 表 | 有 `workspace_id` 列但**没有 `model` 列**，也没有"工作区"索引 | 若 `Model` 要持久化 → ALTER；若"按工作区保留/统计"要落地 → `CREATE INDEX ... (workspace_id, updated_at)` | 新增（ALTER + index） | 低 | `sqlite/open_test.go:61`（列清单断言 `SessionsColumns` 含 `title`） |

**结论（谁负责持久化）**：目录后端 = `Dir` 自己（需要新 sidecar 文件类型，或在契约里声明不持久化）；
sqlite 后端 = 数据库（`rig_sessions.workspace_id` 已有，缺 `model` 列与写入路径）。
**"重启后为 0"的准确位置是 `store.go:340-345`（Workspace/Model/Messages）与 `store.go:346`（Title 的开关）。**

## 3. `session/sqlite` 方法清单逐条对齐（21 个 `*Store` 方法，`go doc -all` 全量核对）

判定口径：**契约方法** = `storage.Storage` 直接声明；**具体类型独有** = 留在 `*sqlite.Store` 上、宿主可用但不在聚合契约里；
**死代码** = 新架构下没有调用方且没有明确保留理由。

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/sqlite/sessions.go:63` | `Store.Create` | 单 INSERT，`retryBusy` 包一层（`sessions.go:79`），唯一约束翻译成 `ErrExists`（`sessions.go:87-89`） | 具体类型独有（写路径）。PLATFORM §5 步 4 收编时再定契约 | 保留 | 低 | `sqlite/store_test.go:28`、`fix20_test.go:138`、`more_test.go:64` |
| `session/sqlite/sessions.go:96` | `Store.Append` | 一行转发 `AppendBatch` | 契约不用；`store.Store` 兼容用 | deprecated 保留一版 | 中：仓库内只有测试在用（`fix20_test.go:76` 的并发闭包）→ 改后需复核 | `sqlite/fix20_test.go:76` |
| `session/sqlite/sessions.go:102` | `Store.AppendBatch` | 单事务写消息 + 补会话行 + 标题 + `revision` 递增；**无 busy 重试**（`sessions.go:109` 直接 `BeginTx`） | 具体类型独有（写路径），但**必须加 `retryBusy`**（§5） | 改写 | 低 | `sqlite/store_test.go:44`、`fix20_test.go:297` |
| `session/sqlite/sessions.go:176` | `Store.Load` | 返回 `(Meta, []rig.Message, error)`；供 `historyView.Load`（`views.go:44`）与 `ImportDir`（`migrate.go:78`）使用 | 具体类型独有（Meta 版）；契约走 `History() rig.History` | 保留 | 低 | `sqlite/store_test.go:72`、`views_test.go:32` |
| `session/sqlite/sessions.go:232` | `Store.List` | 只读 `rig_sessions`，`ORDER BY updated_at DESC, revision DESC` | 具体类型独有（`store.Filter` 别名，不能进 storage） | 保留 | 低 | `sqlite/store_test.go:103` |
| `session/sqlite/sessions.go:284` | `Store.Delete` | 一个事务删 `rig_sessions` + `rig_events`；**不删 `rig_costs`**（外键只覆盖 messages/checkpoints） | 具体类型独有；配额/导出落地时复核是否要一并删 `rig_costs`、`rig_session_leases` | 改写（补删表） | 中：`rig_costs` 残留会让"删了会话但花费还在"（`CostTotals{SessionID:…}` 仍命中）→ 改后需复核 | `sqlite/store_test.go:131`、`fix20_test.go:198` |
| `session/sqlite/sessions.go:307` | `Store.Replace` | 单事务清 + 重写 + 计数覆盖；**无 busy 重试**（`sessions.go:311`） | 具体类型独有；`historyView.Replace`（`views.go:59`）背后的实现，压缩路径也走这里 | 改写（加 retryBusy） | 低 | `sqlite/views_test.go:50` |
| `session/sqlite/open.go:427` | `Store.Close` | `markClosed()` 后 `db.Close()`；**无 `wal_checkpoint`、无 `VACUUM`、无 chmod** | **契约方法**（`storage.Storage.Close`）→ 必须扩：`PRAGMA wal_checkpoint(TRUNCATE)` → 关句柄 → 三件套 chmod 兜底 | 改写 | 低（但改动直接决定"用户拷 .db 是否 malformed"，见 §6） | `sqlite/fix20_test.go:39`、`coverage_test.go:19`、`store_test.go:150` |
| `session/sqlite/open.go:336` | `Store.Schema` | `PRAGMA user_version` + `sqlite_master` + `pragma_table_info` + 扩展账本 | 具体类型独有；**`CanOpen` 复用它的三个内部查询**（`objects` 370 / `columns` 402） | 保留 | 低 | `sqlite/open_test.go:28`（`schemaOf` 助手）、`coverage_test.go:111` |
| `session/sqlite/open.go:327` | `Store.DB()` | 暴露 `*sql.DB`，关闭后返回 nil | 具体类型独有（宿主逃生口）。**必须保留**：`Export`/`Backup` 之外的宿主自建查询全靠它 | 保留 | 低 | `sqlite/coverage_test.go:95`（`TestDSNAndDBExposeTheHandle`） |
| `session/sqlite/open.go:321` | `Store.DSN()` | 返回带 pragma 的 DSN | 具体类型独有；`Backup`/`CanOpen` 需要拿 pragma 一致的第二条连接时会用到 | 保留 | **高**：全仓 0 非测试调用（只有 `coverage_test.go:97`）→ 改后需复核是否降为内部 `dsn` 字段 | `sqlite/coverage_test.go:97` |
| `session/sqlite/memory.go:19` | `Store.Memory() memory.Wiki` | 返回 `memoryStore` | **契约方法**，签名已对齐（PLATFORM §3 ✅） | 保留 | 低 | `sqlite/memory_test.go:22-283`（12 个用例） |
| `session/sqlite/spill.go:19` | `Store.Spill() rig.SpillStore` | 返回 `spillStore` | **契约方法**，签名已对齐（注意是 `Spill` 不是 `Spills`） | 保留 | 低 | `sqlite/spill_test.go:17,43`；`memory_test.go:338` |
| `session/sqlite/cost.go:63` | `Store.SaveCost` | 补会话行 + `retryBusy` 插 `rig_costs`（`cost.go:77`） | 具体类型独有（花费面，PLATFORM §0 判为"可选能力"） | 保留 | 低 | `sqlite/cost_test.go:17,73` |
| `session/sqlite/cost.go:90` | `Store.CostTotals` | 聚合查询 | 具体类型独有 | 保留 | 低 | `sqlite/cost_test.go:48`、`more_test.go:97` |
| `session/sqlite/cost.go:111` | `Store.CostByModel` | 按模型分组 | 具体类型独有 | 保留 | 低 | `sqlite/cost_test.go:48` |
| `session/sqlite/search.go:61` | `Store.SearchMessages` | 跨会话 `LIKE` + snippet | 具体类型独有（`store.Dir` 没有对应能力，是分叉点） | 保留 | 低 | `sqlite/search_test.go:15,44,60`、`sqlite/more_test.go:141` |
| `session/sqlite/search.go:177` | `Store.SearchEvents` | 按 kind/时间/payload | 具体类型独有 | 保留 | 低 | `sqlite/search_test.go:86`、`more_test.go:77` |
| `session/sqlite/views.go:26` | `Store.History() rig.History` | 返回 `historyView`（无 ctx/id） | **契约方法**，签名已对齐（PLATFORM §3 ✅） | 保留 | 低 | `sqlite/views_test.go:18,32,50` |
| `session/sqlite/views.go:29` | `Store.Checkpoints() rig.CheckpointStore` | 返回 `checkpointView` | **契约方法**，签名已对齐（PLATFORM §3 ✅） | 保留 | 低 | `sqlite/views_test.go:74,188` |
| `session/sqlite/views.go:32` | `Store.SessionLog() SessionLog` | 返回**包内自定义** `SessionLog` 接口（`views.go:19-23` = `rig.SessionLog` + `AppendEvent`） | **契约缺口**：方法必须改成返回 `rig.SessionLog`；`AppendEvent` 移到具体类型（`func (s *Store) AppendEvent(...)`）或新的写接口 | 改写 | 高（见下面的调用点全表） | `sqlite/views_test.go:105,150,170`、`coverage_test.go:34,76`、`search_test.go:90`、`more_test.go:80`、`fix20_test.go:108,162,201,211` |

### 3.1 `SessionLog() SessionLog` 返回类型不一致 —— **全部调用点**（`grep -rn 'SessionLog()'`，已剔除 gitignore 的 `.audit-scratch/` 同源拷贝）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/sqlite/views.go:19-23` | `type SessionLog interface` | 包内接口 = `rig.SessionLog` + `AppendEvent` | **删除**这个接口；`AppendEvent` 提到 `*Store` 上（`func (s *Store) AppendEvent(ctx, rig.SessionEvent) error`），`eventView` 只留读方法 | 删除 | 低（导出类型删除属 BREAKING，需 CHANGELOG） | — |
| `session/sqlite/views.go:32` | `func (s *Store) SessionLog() SessionLog` | 返回自定接口 | `func (s *Store) SessionLog() rig.SessionLog` | 改写 | 中：改名后"写入"入口不再能从 `SessionLog()` 上拿到 → 所有写事件调用点必须改 | 同上 |
| `session/sqlite/views.go:135` | `var _ SessionLog = eventView{}` | 编译断言 | 改成 `var _ rig.SessionLog = eventView{}` | 改写 | 低 | — |
| `session/sqlite/views.go:139` | `(e eventView) AppendEvent` | 写在读视图上 | 迁到 `*Store.AppendEvent`（同文件），或保留 `eventView.AppendEvent` 但**不导出** | 改写 | 低 | `views_test.go:150` |
| `session/sqlite/views_test.go:108` | `log := db.SessionLog()` 后 `log.AppendEvent(...)` | 依赖自定接口 | 改成 `db.AppendEvent(...)`；`log` 只用于 `Events/Search/Last` | 改写 | — | `views_test.go:105` |
| `session/sqlite/views_test.go:153` | `log := db.SessionLog(); log.AppendEvent(...)` | 同上 | 同上 | 改写 | — | `views_test.go:150` |
| `session/sqlite/views_test.go:173` | `log.AppendEvent(...)`（两个会话） | 同上 | 同上 | 改写 | — | `views_test.go:170` |
| `session/sqlite/coverage_test.go:34` | `db.SessionLog().AppendEvent(ctx, …)` 断言 `ErrClosed` | 链式调用，改类型后**编译不过** | `db.AppendEvent(...)`；并补一条 `db.SessionLog().Events(...)` 的 `ErrClosed` 断言 | 改写 | — | `coverage_test.go:19` |
| `session/sqlite/coverage_test.go:76` | `log := db.SessionLog()` 后 `log.AppendEvent` 两次（空 session / 空 type） | 同上 | `db.AppendEvent` | 改写 | — | `coverage_test.go:74` |
| `session/sqlite/search_test.go:90` | `log := db.SessionLog()` → `log.AppendEvent` 循环 | 同上 | `db.AppendEvent` | 改写 | — | `search_test.go:86` |
| `session/sqlite/more_test.go:80` | `log := db.SessionLog()` → `log.AppendEvent` ×2 | 同上 | `db.AppendEvent` | 改写 | — | `more_test.go:77` |
| `session/sqlite/fix20_test.go:108` | `db.SessionLog().AppendEvent(...)`（Close 之后断言） | 同上 | `db.AppendEvent(...)` | 改写 | — | `fix20_test.go:39` |
| `session/sqlite/fix20_test.go:162` | `log := db.SessionLog()` → `log.AppendEvent` 循环 | 同上 | `db.AppendEvent` | 改写 | — | `fix20_test.go:159` |
| `session/sqlite/fix20_test.go:201,211` | `db.SessionLog().AppendEvent` / `.Search` | 链式 | `db.AppendEvent` / `db.SessionLog().Search` | 改写 | — | `fix20_test.go:198` |
| `docs/sqlite.md:26` | `SessionLog: db.SessionLog(), // rig.SessionLog` | 注释声称"就是 `rig.SessionLog`"，**今天不成立**（见 §10） | 改成 `db.SessionLog()`（改后为真），并补一段"写事件用 `db.AppendEvent`" | 改写（文档） | — | — |

> 顺序含义：这一条**必须先做**——`SessionLog` 返回类型不先改，后面给 `storage.Storage` 加
> `SessionLog`（或任何用到事件写入的地方）都会编译不过/语义分叉（见 §12）。

## 4. 迁移与版本：`coreVersion` / `migrate` / `Schema` / `Options.Extensions` / `CanOpen` / ALTER

**实测：全仓 `grep -rn "ALTER" --include=*.go session/sqlite session/store` → 0 命中。
`coreSchema` 里 14 条语句（7 表 + 7 索引）全是 `CREATE TABLE/INDEX IF NOT EXISTS`（`schema.go:23-100`）。**

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/sqlite/schema.go:15` | `const coreVersion = 2` | 本构建能打开的 core 版本；变更 schema 必须手工 +1（同文件 5-14 行的护栏注释） | 本次要 **+1 到 3**（租约表 + `model` 列/工作区索引），并且**第一次真正用上 ALTER** | 改写 | 中：`coreVersion` 是"手工同步"的常量，没有测试能证明 `coreSchema` 与它一致 → 建议加一条"schema 指纹/语句数"守卫测试 | `sqlite/memory_test.go:283`（`TestMemoryUpgradeFromV1KeepsMemoryAndSpillUsable` 是它的回归） |
| `session/sqlite/schema.go:23-100` | `var coreSchema []string` | 12 条幂等 DDL；无版本化分段（没有 `v2Statements` 这种结构） | 拆成"按版本分段"的 `[]migrationStep{version, statements}`，每条步进要么 DDL 要么 `ALTER` | 改写 | 高：不拆段就没法表达"v2→v3 加列"，只能靠"再全量跑一遍 IF NOT EXISTS"（对已有表加列**无效**） | `sqlite/open_test.go:61`（`TestOpenCreatesTheCoreSchema` 断言表/索引/列） |
| `session/sqlite/open.go:237-307` | `(*Store).migrate` | 一个事务里：读 `user_version` → 若 `< coreVersion` 跑全量 `coreSchema` → 写 `user_version` → 建扩展账本 → 应用未记录的扩展 | 拆成 `migrate` 内部按步进执行；`CanOpen` 前置预检；只读打开时不跑任何 DDL | 改写 | 中 | `sqlite/open_test.go:89`（`TestOpenIsIdempotent`） |
| `session/sqlite/open.go:248-250` | `PRAGMA user_version` 读取 | 在写事务里读 | 抽出 `probeVersion(ctx, db) (int, error)`，供 `CanOpen` 与 `Schema` 共用 | 新增（抽函数） | 低 | `sqlite/open_test.go:89` |
| `session/sqlite/open.go:251-253` | `if version > coreVersion { return fmt.Errorf("database schema v%d is newer than this build's v%d") }` | **回滚即砖头**：新版本写的库，旧版本连打开都不行 → 也就无法 `Export`（PLATFORM §4-3） | 改成 `CanOpen` 的三态结论：`Upgrade`/`BackupFirst`/`ReadOnly`；`Open` 默认仍拒绝（安全），但提供 `Options.ReadOnly` 与 `Options.AllowDowngradeReadOnly` | 改写 | 中：把"报错"改成"三选一"后，原来只测"报错字符串"的地方要改 | 无直接测试 → **缺测试，需新增** `canopen_test.go` |
| `session/sqlite/open.go:254-263` | `if version < coreVersion { 跑 coreSchema; 写 user_version }` | 老库只做"建缺失的表"，**加列静默失败**（PLATFORM §4-3 的原话） | 换成"逐版本步进 + ALTER"；每步成功后才写 `user_version` | 改写 | 高 | `sqlite/memory_test.go:283`（v1 库 → 当前版本） |
| `session/sqlite/open.go:264-302` | 扩展账本 `rig_schema_extensions` + 应用逻辑 | 宿主 schema 独立台账，只跑一次 | 保留；但 `CanOpen` 必须能报告"待应用的扩展"（不执行），否则宿主无法预检 | 保留 + 新增（报告） | 低 | `sqlite/open_test.go:105,140`、`coverage_test.go:111` |
| `session/sqlite/open.go:78-86` | `type Extension struct{Name; Statements}` | 只有名字 + 语句；没有"版本" | 保留（宿主自管版本）；文档要写清"扩展没有 ALTER 路径，宿主自己负责" | 保留 | 低 | `sqlite/open_test.go:105` |
| `session/sqlite/open.go:218-234` | `validateExtensions` | 拒空名/重名/无语句 | 保留；`CanOpen` 也要在预检阶段调用它（否则"能打开但不能应用"要到 Apply 时才发现） | 保留 + 复用 | 低 | `sqlite/open_test.go:140` |
| `session/sqlite/open.go:185-214` | `func Open(opts Options)` | 拼 DSN（`197-198`）→ `sql.Open` → `SetMaxOpenConns(1)`（`205`）→ `migrate` | 加：① `probeVersion` 预检（失败即返回可判定错误）；② `Options.ReadOnly`；③ 三件套 chmod（§6）；④ `Options.BackupBeforeMigrate` | 改写 | 中 | `sqlite/open_test.go:49,61,89`、`coverage_test.go:124` |
| `session/sqlite/open.go:57-68` | `type Options struct{Path; Extensions; BusyTimeout}` | 3 个字段 | 新增 `ReadOnly bool`、`BackupBeforeMigrate string`（或 `Backup func(path string) error`）、`LeaseTTL time.Duration` | 新增（字段） | 低 | 同上 |
| `session/sqlite/open.go:336-367` | `(*Store).Schema` | 报告 version/tables/indexes/extensions/columns | 保留；新增 `CanOpen(path) (Verdict, error)` **不经过 `*Store`**（宿主在决定"是否打开"时还没有 Store） | 保留 + 新增 | 低 | `sqlite/open_test.go:28` |
| `session/sqlite/open.go:370-399` / `402-421` | `objects` / `columns` | 两个内部查询 | `CanOpen` 复用（报告"缺哪些列"）；改成接受 `*sql.DB` 参数而非 `*Store`，避免"必须先 Open 才能预检"的自锁 | 改写（签名） | 中 | `sqlite/open_test.go:61` |

**`CanOpen` 要动哪些函数（最小集）**：`Open`（`open.go:185`）、`migrate`（`237`）、`validateExtensions`（`218`）、
`objects`/`columns`（`370`/`402` 改签名）、`Schema`（`336`，复用 `probeVersion`），**并新增**
`CanOpen`、`Verdict`、`probeVersion`、`migrationSteps`（`schema.go`）。

## 5. 并发与多进程

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/sqlite/open.go:205` | `db.SetMaxOpenConns(1)` | 单连接串行化写，避免 WAL 无限增长；读者也**被迫串行** | 保留（写者唯一），但要在"多进程租约"下复核：连接池大小 1 会让长事务堵住读路径；`Backup`（`VACUUM INTO`）也在同一连接上排队 | 保留 + 复核 | 中 | `sqlite/coverage_test.go:47`（`TestTwoStoresOnOneFile`） |
| `session/sqlite/open.go:197-198` | DSN：`busy_timeout` / `journal_mode(WAL)` / `foreign_keys(1)` / `synchronous(NORMAL)` | 字符串拼接 | 保留；`Backup`/`CanOpen` 需要同一个拼装函数（抽 `buildDSN(path, timeout, readOnly)`） | 改写（抽函数） | 低 | `sqlite/coverage_test.go:97` |
| `session/sqlite/open.go:54` | `var ErrBusy` | 哨兵，注释"另一个连接/进程正握着写锁" | 保留；租约冲突建议**另开** `ErrLeaseHeld`（语义不同：一个是锁超时、一个是会话被别人持有） | 保留 | 低 | `sqlite/busy_internal_test.go:13` |
| `session/sqlite/busy.go:28-37` | `isBusy` | 按文案匹配 `database is locked`/`sqlite_busy`（驱动不导出错误码） | 保留；文案匹配是长期脆弱点，建议同时用 `modernc.org/sqlite` 的 `*sqlite.Error` 码（若可得）兜一层 | 保留 + 复核 | 中 | `sqlite/busy_internal_test.go:13` |
| `session/sqlite/busy.go:52-77` | `retryBusy` | 3 次、指数退避、总预算 2s、`ctx` 优先 | 保留；**接入点只有两个**（下两行），热路径没有 | 保留 | 低 | `sqlite/busy_internal_test.go:13` |
| `session/sqlite/sessions.go:79-85` | `retryBusy` 包 `Create` 的 INSERT | 有重试 | 保留 | 保留 | 低 | `sqlite/fix20_test.go:138` |
| `session/sqlite/cost.go:77-83` | `retryBusy` 包 `SaveCost` 的 INSERT | 有重试 | 保留 | 保留 | 低 | `sqlite/cost_test.go:17` |
| `session/sqlite/sessions.go:109-112` | `AppendBatch` 的 `BeginTx` | **无重试**（PLATFORM §4-2 点名的热路径） | 用 `retryBusy` 包住整个事务（注意：`retryBusy` 收 `func() error`，事务失败必须整体重开，不能重试单条语句） | 改写 | 高 | `sqlite/store_test.go:44`、`fix20_test.go:297` |
| `session/sqlite/sessions.go:311-315` | `Replace` 的 `BeginTx` | 无重试 | 同上 | 改写 | 高 | `sqlite/views_test.go:50` |
| `session/sqlite/views.go:106-117` | `checkpointView.Save` 的两条 `ExecContext`（先补会话行再 upsert） | 无重试、**非事务**（两条语句可能只落一条） | 包 `retryBusy`；两条语句合一个事务 | 改写 | 高 | `sqlite/views_test.go:74` |
| `session/sqlite/views.go:157-160` | `eventView.AppendEvent` 的 INSERT（`seq = MAX(seq)+1` 子查询） | 无重试；`MAX+1` 在并发写者下靠唯一索引兜底（失败即返回错误，不重算） | 包 `retryBusy`，并在唯一冲突时重算 seq | 改写 | 中 | `sqlite/views_test.go:150` |
| `session/sqlite/spill.go:35-39` / `64-71` | `spillStore.Save` / `GC` | 无重试 | 包 `retryBusy`（`Save` 在 agent 热路径上，见 `agent/spill.go:43`） | 改写 | 中 | `sqlite/spill_test.go:17,43` |
| `session/sqlite/memory.go:61-110` / `121-164` | `memoryStore.Write` / `Append` | `Append` 已经有"先写一次拿锁"的事务技巧（`memory.go:132-135`），但无 busy 重试 | 包 `retryBusy`；`Write` 的读-改-写要进事务（今天 `Write` 是**无事务**的 `SELECT bytes` + `INSERT … ON CONFLICT`） | 改写 | 高 | `sqlite/memory_test.go:212`（并发原子性） |
| `session/sqlite/sessions.go:284-303` | `Store.Delete` 事务 | 无重试 | 包 `retryBusy` | 改写 | 低 | `sqlite/fix20_test.go:198` |

### 5.1 会话租约：要新增什么

现状：**零租约代码**（`grep -rni "lease\|租约"` 在 `session/sqlite`、`session/store` 0 命中），
而 `agent` 每个 Run 开头会 `RecoverInterruptedTurn`（PLATFORM §4-2 所述），第二个进程会把别人**正在跑**的回合当崩溃残留。

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/sqlite/schema.go:23-100`（新表） | `rig_session_leases` | 不存在 | `(session_id TEXT PRIMARY KEY, owner TEXT NOT NULL, acquired_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, heartbeat_at INTEGER NOT NULL)`；加索引 `expires_at` 供清扫 | 新增 | 低 | 新增 `lease_test.go` |
| 新文件 `session/sqlite/lease.go`（建议） | `AcquireLease(ctx, sessionID, owner, ttl) error` | 不存在 | `INSERT … ON CONFLICT DO UPDATE … WHERE expires_at < now`（原子抢锁），抢不到返回 `ErrLeaseHeld`（内含当前 owner，便于诊断） | 新增 | 低 | 新增 |
| 同上 | `RenewLease(ctx, sessionID, owner, ttl) error` | 不存在 | 续租点：agent 每轮 / 每次 `AppendBatch` 前；`WHERE owner = ?`（被抢走则不续，返回 `ErrLeaseHeld`） | 新增 | 低 | 新增 |
| 同上 | `ReleaseLease(ctx, sessionID, owner) error` | 不存在 | `Shutdown` 与 Run 结束调用；幂等 | 新增 | 低 | 新增 |
| 同上 | `LeaseOwner(ctx, sessionID) (owner string, expires time.Time, err error)` | 不存在 | 给 `RecoverInterruptedTurn` 判"这是活着的回合还是残留"（**跨 owner 判断的接缝在 `agent`，不在本包**） | 新增 | 中：只加不接就是纯死代码 → 改后需复核 | 新增 |
| `session/sqlite/sessions.go:102`（`AppendBatch` 事务内） | 租约校验点 | 无 | 事务内 `SELECT owner, expires_at FROM rig_session_leases WHERE session_id=?`，不属于自己且未过期 → 返回 `ErrLeaseHeld`；**不自动抢租约**（避免把"读历史"变成"拿锁"） | 新增（校验） | 中：与 §5 的 `retryBusy` 交叉——重试时租约状态可能已变，必须在每次重试里重新校验 | 新增 |
| `session/sqlite/views.go:91`（`checkpointView.Save`） | 第二个"必须持租约才能写"的点 | 无 | 同上校验（检查点是崩溃恢复的核心，不能由两个进程同时写） | 新增（校验） | 中 | 新增 |
| `session/store/`（整包） | 租约 | 目录后端**明确不支持多实例**（包文档 `store.go:42-46`） | 不改：契约里用能力位表达（`Capabilities().MultiProcess=false`），目录后端**永不**实现租约 | 保留 | 中：能力位若只写在文档里，宿主不会检查 → 需要 `storagetest` 的基线套件强制两者都声明 | `store/audit2_store_test.go:374`、`store/fix_crossinstance_test.go`（这 4 个用例本质是"单写者"文档测试） |

## 6. `Close` 与备份、文件权限 0600

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/sqlite/open.go:427-439` | `(*Store).Close` | `markClosed()` → `db.Close()`。**没有 `wal_checkpoint`**（全仓 `grep wal_checkpoint` 0 命中）→ 用户拷 `.db` 漏 `-wal` 得到 malformed（PLATFORM §4-4） | `PRAGMA wal_checkpoint(TRUNCATE)`（在 `db.Close()` **之前**、单独取一条连接）→ `db.Close()` → 三件套 chmod 兜底 → 幂等返回 | 改写 | 低（改动是"用户可感知的正确性"） | `sqlite/fix20_test.go:39`（Close 与并发方法共存）、`coverage_test.go:19` |
| `session/sqlite/open.go:96-164` | `dbHandle`（原子指针外壳） | `closed` 与 `ptr` 分开记，Close 后仍转发以退化成 `sql: database is closed` | 保留；但 checkpoint 必须在 `markClosed()` **之前**执行，否则拿不到句柄 → 顺序要显式写进注释 | 保留 | 中：`markClosed` 一旦先跑，checkpoint 就静默跳过 → 必须在同一处断言"先 checkpoint 再 markClosed" | `sqlite/fix20_test.go:39` |
| 新文件 `session/sqlite/backup.go`（建议） | `(*Store).Backup(ctx, destPath) error` | 不存在（全仓 `VACUUM` 0 命中） | `VACUUM INTO ?`（SQLite ≥3.27，modernc 支持；目标文件必须不存在）→ 完成后 `os.Chmod(dest, 0o600)`；返回 `destPath` 的字节数便于 UI 展示 | 新增 | 低 | 新增 `backup_test.go`（含"备份可独立打开且消息条数一致"，即真正的 `VACUUM INTO` 往返） |
| 同上 | `Backup` 的挂点 | — | ① 宿主显式调用；② `Options.BackupBeforeMigrate`（迁移前自动备份，PLATFORM §4-3 的"先备份"动作）；③ `Shutdown` 内可选 | 新增 | 中：三处挂点若只做①，PLATFORM 的"先备份再升级"就没有落地路径 | 新增 |
| `session/sqlite/open.go:199-207` | `sql.Open` + `migrate` 之后 | 没有任何 chmod（`grep -rn "0o600\|Chmod" session/sqlite` → 0 命中，**这是实测**） | 在 `migrate` 成功后对三件套 `os.Chmod(path, 0o600)` / `path+"-wal"` / `path+"-shm"`；`-wal`/`-shm` 在首个写事务前可能不存在 → **不存在就跳过，不报错**；`Close` 与 `Backup` 里再兜一次 | 新增 | 中：只在 `Open` 时 chmod，`-wal` 后来被重建（WAL 被 checkpoint 删除后重新创建）会退回 umask → 需要"每次写后/turn 后"的兜底点，或在 `Close` 里必做 | 新增 `perm_test.go`（断言 `mode&0o077 == 0`，`runtime.GOOS == "windows"` 跳过） |
| 新文件（可并入 `backup.go`） | `chmodStateFiles(path) error` | 不存在 | 统一处理 `-wal`/`-shm` 的可选存在 + Windows 跳过 | 新增 | 低 | 同上 |
| `session/store/`（目录后端） | 文件权限 | 会话文件已经 `0o600`（`store.go:537` 的 `os.OpenFile(..., 0o600)`、`records.go:125`），目录 `0o700`（`store.go:307`） | 无需改；**可作为 sqlite 侧的对齐基准**（"记忆 0700/0600 而会话库按 umask"的落差就在 `sqlite` 这一侧） | 保留 | 低 | `store/layout_test.go`（未断言权限）→ 建议补一条目录/文件权限断言，作为"两个后端口径一致"的守卫 |

## 7. `Export` / `Import`：`migrate.go` 逐函数 + `Export` 全新面

### 7.1 现有 `ImportDir`（目录 → 库）逐函数

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/sqlite/migrate.go:18-23` | `type ImportOptions{Dir; DryRun}` | 两字段 | 保留；新增 `Overwrite bool`（Export→Import 往返时"同名会话已存在"必须有明确策略） | 改写（加字段） | 低 | `sqlite/migrate_test.go:74` |
| `session/sqlite/migrate.go:26-37` | `type ImportReport{Sessions;Messages;Imported;Skipped;Failed}` | 报告五字段 | 保留；新增 `BackedUp string`（若接了"先备份"）；`Failed []string` 建议改成结构化（`ID + error`），现在只有字符串 | 改写 | 低 | `sqlite/migrate_test.go:16` |
| `session/sqlite/migrate.go:40-58` | `ImportDir` 入口 + 前置校验（`target == nil`、`Dir` 必填、`os.Stat` 必须已是目录） | 拒绝"目录不存在"（避免 `NewDir` 顺手建空目录） | 保留；`target` 改成**接口**（`*Store` → 最小写接口），否则 `Export` 反向、内存实现导入都要具体类型 | 改写（参数类型） | 中 | `sqlite/more_test.go:109`（`TestImportMissingDirectoryReportsAnError`） |
| `session/sqlite/migrate.go:59-62` | `store.NewDir(dir)` + `List` | 直接依赖 `session/store` 具体类型 | 可保留（子模块本来就 import 根 module）；若要"从任意 `store.Store` 导入"，改成接口参数 | 保留 | 中 | `sqlite/migrate_test.go:16` |
| `session/sqlite/migrate.go:64` | `sort.SliceStable(... CreatedAt.Before ...)` | 最旧先导，与源目录排序一致 | 保留 | 保留 | 低 | 同上 |
| `session/sqlite/migrate.go:70-75` | `existing map[string]int`（ID → 库内消息数） | 续跑判据的数据源 | 保留；`List` 若失败被静默忽略（`if current, err := …; err == nil`）→ **改写成必须报错**（现在会静默重导） | 改写 | 中：这条 `err == nil` 是"静默降级"，与 PLATFORM "不静默"的原则冲突 | 同上 |
| `session/sqlite/migrate.go:76-87` | 主循环：`source.Load` + `report.Messages += len` + `have >= len → Skipped` | 按消息数判"已完整" | 保留（这是文档化的幂等判据） | 保留 | 低 | `sqlite/fix20_test.go:223` |
| `session/sqlite/migrate.go:88-90` | `if opts.DryRun { continue }` | 空跑只统计 | 保留 | 保留 | 低 | `sqlite/migrate_test.go:74` |
| `session/sqlite/migrate.go:91-103` | 空壳/半截会话：不 Create，直接 `AppendBatch` 续导 | 已修过一次（"实测 0/2"注释） | 保留 | 保留 | 低 | `sqlite/fix20_test.go:223` |
| `session/sqlite/migrate.go:104-121` | `target.Create(meta{ID,Title,Workspace,CreatedAt,UpdatedAt})` + `AppendBatch` | 逐会话 `Create` | 保留；`Messages`/`Bytes` 不传（由 `AppendBatch` 累加）→ 与 §2 一致 | 保留 | 低 | `sqlite/migrate_test.go:16` |

### 7.2 `Export`（全新）

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| 新文件 `session/sqlite/export.go`（建议） | `ExportOptions{Dir; Sessions []string; Since time.Time; DryRun bool}` | 不存在 | 与 `ImportOptions` 对称；`Sessions` 空 = 全导；`DryRun` 只算不写 | 新增 | 低 | 新增 `export_test.go` |
| 同上 | `ExportReport{Sessions; Messages; Exported; Skipped; Failed}` | 不存在 | 与 `ImportReport` 字段对齐（便于宿主把两个报告混着显示） | 新增 | 低 | 新增 |
| 同上 | `func ExportToDir(ctx, source, opts) (ExportReport, error)` | 不存在 | 用 `source.List` + `source.Load` 读出，用 `store.Dir.Create` + `AppendBatch` 写出（`store.Dir` 已具备这两个能力，无需新写文件格式） | 新增 | 低 | 新增 |
| 同上 | `source` 参数类型 | — | **接口**（`List`/`Load` 两方法的最小接口），不要写成 `*Store`：否则内存实现/未来实现都不能导出 | 新增 | 中：写成 `*Store` 会重演 `kit.Config.SessionStore *store.Dir` 的具体类型病 | 新增 |
| 同上（写入侧） | 目标目录已有同名会话 | 不存在 | 明确策略：`Overwrite`（`Replace`）或 `Skip`（进 `Skipped`）；**默认 Skip 并报告** | 新增 | 中 | 新增 |
| 同上（校验） | 往返校验 `VerifyRoundTrip(ctx, src, dst)` | 不存在 | 逐会话比对：`Title`、`len(Messages)`、逐条 `Role/Text`、`CreatedAt`（秒级）；**不比对 `Workspace`/`Model`/`Bytes`**（§2 的已知丢失，要求先修 §2 或显式豁免） | 新增 | 高：不写豁免清单的话，目录后端往返必然"失败"，导致门禁永远红 | 新增 `roundtrip_test.go` |
| 同上 | 事件/花费/检查点是否导出 | 不存在 | **第一版只导会话与消息**；事件/花费/检查点在 `Report` 里显式标 `NotExported`（避免"导出成功"的错觉） | 新增 | 中 | 新增 |
| `session/sqlite/migrate.go:3-5`（文件头注释） | 包注释"把已有宿主从目录存储搬到数据库" | 单向叙述 | 改成双向（`Import`/`Export` 一组），并说明往返校验的边界 | 改写 | 低 | — |

## 8. CI 与发布面

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/sqlite/go.mod:6` | `require github.com/zeokeel/zeokeel-rig v0.24.0` | 要求一个**不存在的版本**（`git tag -l 'v0.24*'` → 空；根 tag 最高 `v0.23.4`） | 随本次发布 bump 到实际要发的 `vX.Y.Z`；新增 `storage` 包后**必须**一起 bump（否则下游拿不到 `storage`） | 改写 | **高**：`v0.24.0` 从未发布 → 下游 `go get` 必然解析失败（见第 6 行的实测） | — |
| `session/sqlite/go.mod:22` | `replace github.com/zeokeel/zeokeel-rig => ../..` | 本地开发用 replace，**提交在仓库里** | 删除；本地用 `go.work`（`docs/sqlite.md:130-133` 已经这么建议，但 go.mod 里还留着 replace → 文档与文件自相矛盾） | 删除 | **高**：留着 replace 会掩盖"require 指向的版本不存在"这一事实——这就是它一直没被发现的原因 | — |
| `session/sqlite/go.mod`（整文件）/ `go.sum` | 子模块 module 身份 | 无任何 `session/sqlite/vX` tag（`git tag -l 'session/sqlite/*'` → 空） | 发布时补 `session/sqlite/vX.Y.Z`（`docs/RELEASE.md:33` 的步骤），并让"漏打 tag"在 CI 里红 | 新增（流程） | 高 | — |
| `.github/workflows/ci.yml:83-102` | 子模块门禁步骤（`test (session/sqlite submodule)`） | `go work init ../.. && go work use .` → build/vet → 交叉编译 → `go mod tidy -diff` → 覆盖率下限 | 保留结构；`go.work` 的临时生成方式可以留（**实测可用**：`go work init ../..` + `go work use .` 后 `go list ./...` 只列子模块），但删掉 replace 后这一步不再是"必需"（require 指向真实 tag） | 保留 | 中：删除 replace 后，本地/CI 都必须靠 go.work 或真实 tag，任一处漏配就是"本地绿、CI 红" | — |
| `.github/workflows/ci.yml:90-93` | 交叉编译 `GOOS=darwin/windows go build ./...` | 只有两个平台 | 保留（纯 Go 驱动，三平台可编；`tidy` 漏 go.sum 条目只有交叉编译能暴露——这个理由仍然成立） | 保留 | 低 | — |
| `.github/workflows/ci.yml:94-99` | `go mod tidy -diff`（workspace 模式） | 在 go.work 下跑，会同时整理两个 module | 保留；注意 workspace 模式下 tidy 的行为依赖 Go 版本（Go 1.27），bump 工具链时要复核 | 保留 | 中 | — |
| `.github/workflows/ci.yml:104-114` | 子模块 `-race`（`CGO_ENABLED=1`） | 临时 go.work + race | 保留；新增的租约/备份/导出测试必须在 `-race` 下过（并发抢锁用例） | 保留 | 低 | — |
| `.github/workflows/ci.yml:305-382` | `sqlite-consumer-smoke` job | 用 `file://` 模块代理伪造 `v0.24.0` 的子模块 zip，在临时 module 里 `go get` + `go build` | **必须改**：① `stage` 也要打包**根 module**（`stage()` 里已有 `dir == "."` 分支，但**从未被调用**——`ci.yml:337-343` 是死分支，`ci.yml:347` 只 stage 了子模块）；② 第三方依赖（`modernc.org/*`）不可能从 `file://` 代理拿到 → `GOPROXY` 必须写成 `file://$proxy,https://proxy.golang.org`（或直接跑 `sqlite-consumer-smoke` 前 `go mod download` 预热 GOMODCACHE）；③ 版本号必须与真实 tag 一致（现在写死 `v0.24.0`，`ci.yml:323`） | 改写 | **高（已实测失败）** | 见下行实测 |
| `.github/workflows/ci.yml:321-382`（复现） | — | **实测**：用同一段脚本（临时 file:// 代理 + 空 GOMODCACHE）跑 `go get github.com/zeokeel/zeokeel-rig/session/sqlite@v0.24.0`，失败：`reading file:///…/proxy/github.com/zeokeel/zeokeel-rig/@v/v0.24.0.zip: no such file or directory`（根模块）+ `modernc.org/sqlite@v1.62.0: … no such file or directory`（第三方） | 这条 job **今天就是红的**（或从未真正跑过）；本架构变更前必须先修好它，否则"子模块能被宿主用起来"的唯一证据是假的 | 改写 | 高 | — |
| `.github/workflows/ci.yml:337-343` | `stage()` 的 `if [ "$dir" = "." ]`（根模块打包：排除 `.git`/`session`/`go.work`） | **死代码**：全脚本只调用了一次 `stage … session/sqlite`（`ci.yml:347`） | 要么调用它打包根模块（推荐，配合 ② 的 GOPROXY 修正），要么删除该分支 | 改写 / 删除 | **高**（典型"看起来覆盖了、其实没跑过"） | — |
| `.github/workflows/release.yml` | 发布 tag 自动化 | 只处理根 module 的 `vX.Y.Z`（`grep sqlite` → 0 命中） | 不改（`docs/RELEASE.md:40` 已声明子模块 tag 是手工步骤）；但**新增 storage 包后**，"先发根、再发子模块"的顺序变成硬约束（子模块要 `go get` 到根 module 的新版本） | 保留 | 中 | — |
| `docs/RELEASE.md:16-41` | 子模块 tag 步骤 | 手工步骤写得完整（`go mod edit -require` → 主模块先发 → 子模块 tag） | 保留；新增两条：① 删除 `replace ../..` 后要在**干净环境**验证（`GOPROXY=off` 编不过就说明 go.work 泄漏）；② 本次发布必须同时 bump `storage` 相关 require | 改写 | 中 | — |
| `docs/RELEASE.md:43` | 列表项 `2.` **重复编号**（第 16 行也是 `2.`） | markdown 编号错乱（渲染成 1,2,2,3） | 改成 `3.`/`4.` | 改写 | 低 | — |
| `scripts/coverage-floor.sh`（`ci.yml:102` 调用） | 子模块覆盖率下限 | 每包下限 | 新增 `storage`/`storagetest` 后，覆盖率口径要包含新包（否则新包 0 覆盖也"绿"） | 保留 + 复核 | 中 | — |

## 9. 测试面：逐文件判定

图例：**基线** = 可原样搬进 `storagetest`（两个实现都该满足）；**后端特有** = 留在本包；
**失去意义/必须重写** = 架构变更后断言不再成立或编译不过。

### 9.1 `session/store/*_test.go`

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/store/store_test.go:22` | `var _ store.Store = (*store.Dir)(nil)` | 编译断言 | 若 `Store` 接口不变 → 保留；若 `History` 进接口 → 必改（`Dir` 的 `History` 带 ctx/id，不满足） | 改写 | 中 | — |
| `session/store/store_test.go:82,143,153,421,480,566,591,616,643,692,723` | `TestCreateAppendLoadRoundTrip` / `LoadUnknownSessionReturnsErrNotFound` / `ListFilterAndOrder` / `ConcurrentAppendsSameSession` / `ConcurrentCreateAndAppendKeepsIndexConsistent` / `MessageTooLargeIsRejected` / `DeleteRemovesSessionAndFile` / `CreateGeneratesSortableIDs` / `CreateWithExplicitIDAndDuplicate` / `ClosedStoreRejectsEveryOperation` / `CancelledContextIsReported` | 契约级行为（不含文件布局） | 搬进 `storagetest` 基线套件，两个实现各跑一遍 | 改写（搬迁） | 低 | 与 `session/sqlite/store_test.go:28-150` **几乎是同一批用例**（重复实现）→ 这正是 `storagetest` 的证据 |
| `session/store/store_test.go:207,263,335,527,746,788,816,862` | `TestReopenSeesSessionsFromFiles` / `TestPartialTailLineIsIgnored` / `TestAppendAfterPartialTailTruncatesIt` / `TestScanFindsHandWrittenSessionFiles` / `TestSessionFileHasOneRecordPerLine` / `TestExternallyDeletedFileIsNotResurrected` / `TestUnusualIDUsesEncodedFileName` / `TestExtraLineOnDiskIsPickedUpByLoad` | NDJSON 布局/半行/文件名编码 | 保留在 `session/store`（后端特有） | 保留 | 低 | — |
| `session/store/layout_test.go:63,116,155,209,242,285` | 6 个"布局"用例（无 index.json / 无 sessions/ 子目录 / mtime 排序 / 重启删不掉） | 目录布局契约 | 保留；**新增**一条"sidecar 元数据文件"用例（若 §2 选方案①） | 保留 | 低 | — |
| `session/store/scan_test.go:39,99,130,148,173,197,221,245,333` | `TestScanContents*` 8 + `BenchmarkNewDir100Sessions` | 扫目录取标题/关闭扫描/预算 | 保留（这是 `Capabilities().StartupTitle=true` 的实现细节）；`245` 的启动成本用例可降级为 benchmark | 保留 | 低 | — |
| `session/store/audit_internal_test.go:20,55,71` | `checkID`/`fileStem` 单射/`trimPartialTail` | 纯内部函数 | 保留 | 保留 | 低 | — |
| `session/store/audit_store_test.go:489,513` | `TestAuditHistoryLoadReturnsIndependentSlice` / `TestAuditHistoryErrorPropagation` | `Dir.History` 适配器 | 搬进 `storagetest`（`History()` 契约）；若 `History` 改成适配器提供，则改为调 `DirStorage.History()` | 改写（搬迁） | 中 | — |
| `session/store/audit_store_test.go:169`（`TestAuditCrashPointsSelfHeal`）、`:558`（`TestAuditReplaceVersusConcurrentAppend`） | 崩溃点自愈 / Replace 与 Append 竞争 | 目录后端特有 | 保留（`Replace` 语义两实现都要有，但"崩溃点"是文件语义）→ 前者留、后者搬 `storagetest` | 保留 / 改写 | 低 | — |
| `session/store/audit2_store_test.go:374,420` + `fix_crossinstance_test.go:17,48,82,106` | `TestAuditTwoInstancesOnSameDir` / `TestAuditCrossInstanceLoad` / `TestFixCrossInstance*` 4 个 | **"同目录多实例"** 行为（跨实例补索引） | **失去意义**：新架构明确要"多进程租约"（PLATFORM §4-2），而目录后端声明不支持多实例 → 改成能力位测试：`Capabilities().MultiProcess == false` 时这些用例只验证"文档所述的降级行为"，且不再是基线套件的一部分 | 改写 | 高：这批用例现在在**鼓励**一个架构上要消灭的行为 | — |
| `session/store/audit2_store_test.go:43,105,174,233,281,338` | 并发不同会话/半行/计数/标题 | 混合 | 条目级计数/半行 → 留；标题取首条 user → 搬 `storagetest`（`History` 语义） | 保留 / 改写 | 低 | — |
| `session/store/fix_titles_test.go:31,70` | `scanTitle` 超长记录跳过/预算耗尽 | 扫目录特有 | 保留 | 保留 | 低 | — |
| `session/store/history_test.go:21,58,100,153,171,190` | 6 个 `Dir.History` 用例 | 直接调 `s.History(ctx,"s1")` | 若保留 `Dir.History` → 原样；若改为适配器 → 全部改调 `storage.NewDirStorage(s,"s1").History()`；同时**新增**一条"同一个存储两种 History 入口语义一致"的用例 | 改写 | 中 | — |
| `session/store/audit2_perf_test.go:15` | `TestAuditPerfLoadAfter10kMessages` | 性能门槛 | 保留（改成 `testing.Short()` 跳过） | 保留 | 低 | — |

### 9.2 `session/sqlite/*_test.go`

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/sqlite/store_test.go:28,44,72,95,103,131,150` | 7 个方法级用例 | 与 `session/store/store_test.go` 重复的"契约级"行为 | **搬进 `storagetest` 基线**，本文件基本清空（只留 SQLite 特有的字节/权限断言） | 改写（搬迁） | 低 | 与 §9.1 第一组一一对应 |
| `session/sqlite/views_test.go:18,32,50` | `History` 视图 3 例 | `History()` 契约 | 搬 `storagetest`（`History()` 是契约方法） | 改写（搬迁） | 低 | — |
| `session/sqlite/views_test.go:74,188` | `TestCheckpointRoundTrip` / `TestDeletedSessionLeavesNoCheckpoint` | `Checkpoints()` 契约 | 搬 `storagetest`（`Checkpoints()` 是契约方法）；`188` 依赖"删会话级联删检查点"，需在基线里声明为能力 | 改写（搬迁） | 低 | — |
| `session/sqlite/views_test.go:105,150,170` | SessionLog 三例（`AppendEvent` 链式调用） | 依赖自定 `SessionLog` 接口 | **必须重写**（§3.1 调用点表）；重写后 `Events/Search/Last` 部分可进 `storagetest`（若把 `SessionLog()` 加进契约的话） | 改写 | 高 | — |
| `session/sqlite/search_test.go:15,44,60,86`（含 `:90` 的 `log.AppendEvent`） | 4 例 | SQLite 独有检索 | 保留（`SearchMessages`/`SearchEvents` 不在契约里）；`86` 的造数方式要按 §3.1 改 | 保留 + 局部改写 | 低 | — |
| `session/sqlite/cost_test.go:17,48,73,102` | 4 例 | 花费面 | 保留（不在契约内） | 保留 | 低 | — |
| `session/sqlite/memory_test.go:22,45,69,101,125,150,172,185,212,250,283` | 11 例（`memory.Wiki` 后端的**契约一致性**） | 与文件后端 `internal/memwiki` 有对应的行为断言 | 拆分：`22,45,69,101,125,150,172,185,250` 里"契约规则"部分 → `storagetest`（或 `memorytest`，PLATFORM 未规划则需要新建一个共享套件）；`212`（并发原子性）、`283`（v1 升级）留本包 | 改写（部分搬迁） | 中：`memory` 有两个后端但**没有共享测试套件**，是"多实现分叉即红"没做到的另一个例子 | `283` 同时是 `coreVersion` 回归 |
| `session/sqlite/memory_internal_test.go:14,29` | `firstLine` UTF-8/首行 | 内部实现 | 保留 | 保留 | 低 | — |
| `session/sqlite/spill_test.go:17,43` | `Save/Load`、`GC(keep)` | `rig.SpillStore` 契约 | 搬 `storagetest`（`Spill()` 是契约方法）；`43` 的"按 seq 保留最新"是 SQLite 实现细节，留一条在本包 | 改写（部分搬迁） | 低 | — |
| `session/sqlite/open_test.go:49,61,89,105,140` | `Path` 必填 / 核心 schema / 幂等 / 扩展 / 扩展校验 | `Open` 面 | 保留；`61` 的表/索引/列清单要随 `coreVersion` bump 更新（新增租约表）；新增 `CanOpen` 三态用例、`ReadOnly` 用例 | 改写 | 中 | — |
| `session/sqlite/migrate_test.go:16,74` | `ImportFromDirectory` / `DryRun` | 单向导入 | 保留 + 新增 `Export`/往返校验用例（§7.2）；`DryRun` 用例可原样保留 | 改写（扩充） | 低 | — |
| `session/sqlite/more_test.go:157`（`:164` 的 `var _ store.Store`） | `TestContractAliasesMatch` | 断言 `ErrNotFound`/`ErrClosed` 是 `store` 的别名 + `Store` 实现 `store.Store` | **必须重写**：别名是否保留取决于是否删 `contract.go` 的别名（建议保留 `Err*` 别名、删 `Meta`/`Filter` 别名的"展示性"依赖）；`var _ store.Store` 与新契约断言 `var _ storage.Storage = (*Store)(nil)` 并存 | 改写 | 中 | — |
| `session/sqlite/more_test.go:22,37,51,64,77,97,109,121,141` | 其余 9 例（自动建会话/标题截断/时间戳/事件边界/花费窗口/缺目录/snippet/单会话检索） | 后端行为 | 保留（`22,37,51` 属契约级 → 可搬基线） | 保留 / 部分搬迁 | 低 | — |
| `session/sqlite/fix20_test.go:39`（`:108` SessionLog 断言）、`:117`、`:138`、`:159`、`:198`、`:223`、`:274`、`:297` | 8 例 | 历史修复回归 | `39` Close 用例要**扩**：新增"Close 前 wal_checkpoint 生效"（`-wal` 文件消失或大小归零）；`108` 按 §3.1 改；`223` Import 续导保留；`274`（`Meta.Bytes/Model` 恒 0）**若 §2 决定持久化 Model 就必须改** | 改写 | 中 | — |
| `session/sqlite/coverage_test.go:19`（`:34,76` SessionLog）、`:47`、`:74`、`:86`、`:95`、`:111`、`:124` | 7 例 | 覆盖面补丁 | `19`/`74` 按 §3.1 改；`47`（`TestTwoStoresOnOneFile`）升级为**租约用例**（两个 `*Store` 抢同一个会话 → 第二个必须拿到 `ErrLeaseHeld`）；`124` 扩成"Open 后文件权限 0600" | 改写 | 中 | — |
| `session/sqlite/busy_internal_test.go:13` | `TestBusyRetryBehaviour` | 只测 `retryBusy` 本身 | 保留；**新增**"`AppendBatch`/`Replace`/`SaveCheckpoint`/`SaveCost` 都会重试"的接入点用例（今天只有 `Create`/`SaveCost` 接上） | 改写（扩充） | 高：不补这条，§5 的重试改写没有任何门禁 | — |

### 9.3 归属结论

- `session/sqlite/fix20_test.go`：**留本包**（历史修复回归 + 实现细节），但 `:39`/`:108`/`:274` 三处必须改。
- `session/sqlite/memory_test.go`：**拆分**——契约部分进共享套件，`212`/`283` 留本包。
- `session/sqlite/spill_test.go`：**拆分**——`17` 进共享套件，`43` 留本包。
- `session/store/history_test.go`：**留本包**（`Dir.History` 的 ctx/id 语义是本包特有），但要在适配器落地后补一条"两入口一致"。

## 10. 文档面

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `docs/sqlite.md:11-28` | "一句话用法" | `defer db.Close()` + `SessionLog: db.SessionLog()` | 改成 `app.Shutdown(ctx)`（不是 `st.Close()`，PLATFORM §1）+ `Storage: st`；`SessionLog` 行按 §3.1 改注释 | 改写 | 中：这份文档是宿主第一入口，`defer db.Close()` 正是 PLATFORM §4-1 要消灭的写法 | — |
| `docs/sqlite.md:33-44` | "存了什么"表 | 5 张表（`rig_sessions`/`messages`/`events`/`checkpoints`/`costs`），**漏了 `rig_memory_pages` 与 `rig_spill`**（`schema.go:72-94` 明明有） | 补齐 7 张表 + 新增 `rig_session_leases`；索引列补 `rig_memory_updated_at` | 改写 | 中：文档漏表会让宿主以为 `Memory()`/`Spill()` 没有落库 | `session/sqlite/open_test.go:61` |
| `docs/sqlite.md:67-76` | "并发与崩溃" | 讲 `busy_timeout`/有限重试/事务/外键 | 必须补：① 多进程**租约**与 `ErrLeaseHeld`；② `SetMaxOpenConns(1)` 的读也被串行的代价；③ `Close` 现在会 `wal_checkpoint` | 改写 | 中：这段今天会让人以为"多进程已经安全" | `coverage_test.go:47` |
| `docs/sqlite.md:78-86` | "从 NDJSON 迁移" | 只有 `ImportDir` | 补 `Export`/往返校验；补"先备份（`Options.BackupBeforeMigrate`）再升级"的推荐流程 | 改写 | 低 | `migrate_test.go` |
| `docs/sqlite.md:98-119` | "为什么是子模块" + CI 步骤清单 | 列出 `CGO_ENABLED=0`/交叉编译/`-race`/`tidy -diff`/下游冒烟；`:119` 写的是 **`published-module smoke`**，而 CI 里实际 job 名是 **`sqlite-consumer-smoke`**（`ci.yml:305-306`） | 名字改对；补"consumer smoke 需要 stage 根模块 + 允许网络拉第三方依赖"（§8 实测） | 改写 | 中：文档指向一个不存在的步骤名 → 读者按名字找不到门禁 | — |
| `docs/sqlite.md:121-135` | "版本与本地开发" | 说"不要用 `replace ../..`，改用 go.work" | **与 `go.mod:22` 直接矛盾**（replace 就在文件里）；删除 replace 后这段才成立；`go.work` 步骤可保留（实测可用） | 改写 | **高**：文档与文件互相打脸，且这个矛盾正是 §8 第 1 行 bug 的温床 | — |
| `docs/sqlite.md:137-152` | "Meta.Bytes 与 Meta.Model 恒为 0/空" | 解释为何不维护 | 若 §2 决定加 `model` 列 → 整节重写；若保持 → 保留并把 `Bytes` 的口径与"目录后端用文件大小"对比说明 | 改写 / 保留 | 中 | `fix20_test.go:274` |
| `docs/sqlite.md:154-158` | "与目录实现的契约一致" | 声称 `sqlite.Store` 实现了 `store.Store`、**`rig.History`、`rig.HistoryReplacer`、`rig.CheckpointStore`、`rig.SessionLog`** | **实测 4 条全假**（编译探针）：`Append` 签名不符、`Replace` 返回 `(Meta,error)`、缺 `Clear`、缺 `Events`。只能通过 `History()`/`Checkpoints()`/`SessionLog()` 视图拿到 | 改写 | **高**：这是文档里的**事实错误**，会直接误导宿主写 `var h rig.History = st` | — |
| `docs/MEMORY.md:9` | 能力/实现表 | `memory.Wiki` 两个实现（`internal/memwiki` 文件 / `session/sqlite` 表）；`rig.SpillStore` 两个实现 | 补一行"聚合契约 `storage.Storage.Memory()/Spill()`"，并说明 `internal/memwiki` 是 `internal/`（宿主不可用）→ 唯一可用的进程内 wiki 入口问题（PLATFORM §6 "内存聚合实现要新写"） | 改写 | 中 | — |
| `docs/MEMORY.md:20-21` | `wiki := store.Memory() // session/sqlite` | 用变量名 `store` 当 `*sqlite.Store`，与包名 `session/store` **同名** | 改成 `wiki := app.Storage().Memory()` 或 `wiki := st.Memory()`（变量名 `st`），避免读者误以为有 `store.Memory()` 这个包级函数 | 改写 | 中 | — |
| `docs/MEMORY.md:53-55` | `Spill: &rig.SpillPolicy{Store: store.Spill(), MaxFiles: 200}` | 同上命名 | 同上；并补"租约/多进程下 spill 同库"的说明 | 改写 | 低 | — |
| `docs/MEMORY.md:59-66` | `Save/Load/GC` + 回收语义 | "文件后端每次写后按 `MaxFiles` 修剪；库后端每次 `Save` 后 `GC(MaxFiles)`（`agent/spill.go:43`）" | 保留（这条**已核实为真**）；新增"配额机制默认关（PLATFORM §5 步 5）：`GC` 由 agent 触发，SDK 不自动删" | 保留 | 低 | `session/sqlite/spill_test.go:43` |
| `docs/RELEASE.md:16-41` | 子模块 tag 流程 | 正确但缺两件事 | 补：① 删 replace 后必须在**干净 GOPATH** 验证子模块；② 新增 `storage` 包属于根 module → 子模块 `require` 必须 bump 到含它的版本（否则 `storage` 对子模块不可见） | 改写 | 中 | — |
| `docs/RELEASE.md:43,59-68` | 编号重复 + "release 跑哪些门禁"列表 | 列表第 2 项出现两次；门禁清单里**没提**子模块门禁与 consumer smoke | 修编号；把 `test (session/sqlite submodule)`、`sqlite-consumer-smoke` 写进清单（否则"tag 跑的门禁 == merge 跑的门禁"这句话对子模块不成立） | 改写 | 中 | — |

## 11. `session/memory.go` 与 `session/compact/`（变更面小，但仍是本范围）

这两个包**不碰存储后端**，所以架构变更对它们是"签名/陈述层"的，不是实现层的。
清点结论：**`compact` 基本不动**（它只依赖 `rig.Compactor`/`rig.Message`），
`session/memory.go` 需要一次**文档纠错 + 一个"聚合形态内存实现"的缺口登记**。

| 位置（文件:行） | 符号 | 现状 | 变更后 | 处置 | 死代码风险 | 关联测试 |
|---|---|---|---|---|---|---|
| `session/memory.go:4-5` | 包文档"持久化实现（SQLite/JSONL 等）位于 SDK 之外" | **已不成立**：`session/store`（NDJSON）与 `session/sqlite` 都在 SDK 内 | 改成"SDK 提供目录与 SQLite 两个参考实现；宿主可只依赖 `rig.History` 契约" | 改写 | 中：这句错话会让宿主以为"SDK 不管持久化"，与 PLATFORM 全篇矛盾 | `session/memory_test.go`（整文件） |
| `session/memory.go:71-74` | `type Memory struct{mu; sessions map[string][]rig.Message}` | 内存 `rig.History` | 保留；但要注意它是**唯一**零依赖 History 实现，PLATFORM §6 "聚合形态的内存实现要新写"指的不是它 | 保留 | 低 | `memory_test.go:16-249`（10 例）、`memory_replace_test.go:11` |
| `session/memory.go:84,99,115` | `Load` / `Append` / `Replace` | 深拷贝、原子批、实现 `rig.HistoryReplacer` | 保留（`Replace` 是 `HistoryReplacer` 的第二个实现，证明该契约跨实现可用） | 保留 | 低 | `memory_replace_test.go:11` |
| `session/memory.go:132` | `Len(sessionID)` | 测试/诊断用 | 保留；**注意它与"契约里没有条数"的缺口形成对照**（§2 第 4 行）：内存实现能免费给条数，两个持久化实现给不了 | 保留 | 中：`Len` 全仓无生产调用 → 改后需复核 | `memory_test.go`（多处） |
| `session/memory.go`（缺） | `Close` / `Checkpoints` / `Memory` / `Spill` | **没有**：`session.Memory` 只有 History 一面 | 若要做"内存版 `storage.Storage`"（PLATFORM §6 明确要新写），需要新类型（建议 `storage.MemoryStorage` 或 `session.NewMemoryStorage()`）**组合** `session.Memory` + `memwiki.MemoryWiki` + 内存 Checkpoint/Spill（这两个今天**不存在**） | 新增（不在本包内实现，登记缺口） | 高：先写 `storagetest` 基线再实现，否则"内存实现"会以 `storagetest` 未覆盖的形状出现 | — |
| `session/compact/window.go:35-47` | `WindowCompactor` / `Compact` | 确定性裁剪，`rig.Compactor`（`context.go:18-20`） | 不动 | 保留 | 低 | `compact/compact_test.go:19,46,67,83` |
| `session/compact/llm.go:29-42` | `LLMCompactor` / `Compact` | 模型摘要 + 降级 | 不动 | 保留 | 低 | `compact_test.go:124,143,159` |
| `session/compact/window.go:76,82,96` | `SummaryMessage` / `CompactionSummaryContent` / `IsCompactionSummary` | 摘要消息的形状（可被持久化后识别） | 不动；但**持久化后的摘要消息必须能跨重启识别**——这依赖 `Replace` 把消息原样存下（两实现都满足：`store.Replace` `store.go:740`、`sqlite.Replace` `sessions.go:307`） | 保留 | 低 | `compact_test.go:19` |
| `session/compact/window.go:205-231` | `CloneMessages` / `cloneMessage` | 导出深拷贝助手 | 与 `session/memory.go:150` 的未导出 `cloneMessages` **重复实现** | 保留 | 中：两份深拷贝行为若漂移，`-race` 抓不到（一个拷了 `Content` 没拷 `ToolCalls` 就会漏）→ 改后需复核是否收敛成一处 | —（无交叉断言） |
| `session/compact/`（整包） | 与持久化的接缝 | 包文档 `window.go:5` 明说"是否把压缩结果持久化替换历史由宿主决定"；实际由 `kit.Agent.CompactNow` 经 `rig.HistoryReplacer` 调 `Replace`（`kit/kit.go:1265`） | 不动；契约变更时只需保证 `storage.Storage.History()` 的结果**也能被断言成 `rig.HistoryReplacer`**（否则压缩在新架构下静默失效） | 保留 + 复核 | **高**：`History()` 返回 `rig.History`，压缩路径要 `HistoryReplacer`；若适配器返回的类型没实现 `Replace`，压缩"看上去成功、实际没落盘" | `compact/compact_test.go`（不覆盖 `Replace` 接缝）；`kit/wiring_test.go:148` |

## 12. 顺序依赖（不按这个顺序改，编译不过或语义分叉）

1. **`SessionLog` 返回类型**（`views.go:19-23,32,135,139` + 13 个调用点）：**第一步**。
   理由：它是唯一"改了就会让 13 处编译失败"的公共签名变更；任何先做 `storage.Storage` 注入的尝试都会撞上
   `*sqlite.Store` 不满足 `rig.SessionLog` 的编译期检查（若 `storage` 里断言 `SessionLog() rig.SessionLog` 就直接红）。
2. **删 `session/sqlite/go.mod:22` 的 `replace` + 修正 `require`（`go.mod:6`）+ 修 consumer smoke**：
   必须在"新增 `storage` 包并让子模块 import 它"**之前**完成。否则子模块的 require 指向的版本里没有 `storage`，
   本地靠 replace 编过、下游 `go get` 直接失败（§8 已实测同类失败）。
3. **建 `storage` 包 + `storage.DirStorage` 适配器**（新增文件，另一人主责但本包要提供 `Dir.History` 不动）：
   适配器**必须放在 `storage` 包**（PLATFORM §3 的断言 `var _ storage.Storage = storage.DirStorage{}`），
   于是 `storage` import `session/store`。**推论（必须写进 PLATFORM）**：`session/store` **不能**反向 import `storage`，
   所以 `Dir` 上不能有 `func (d *Dir) Storage() storage.Storage`（否则成环）。二者只能选一个方向。
4. **`storagetest` + 基线套件搬迁**：先有基线，再改 `sqlite` 的 `coreVersion`/`Close`/重试——否则"多实现分叉即红"仍然不存在，
   §6/§5 的重写没有回归网。PLATFORM §5 步 0 已经这么排，本清点确认这个顺序是**必需**的（不是偏好）。
5. **`coreVersion=3` + 分段迁移 + ALTER**：必须在 `Backup`（§6）之后、`CanOpen` 的"先备份"动作之前打通，
   否则 `Options.BackupBeforeMigrate` 没有落点。顺序：`migrationSteps` → `probeVersion` → `CanOpen` → `Backup` → `Options.BackupBeforeMigrate`。
6. **租约**（新表在 `coreVersion=3` 里）→ 然后才能改 `AppendBatch`/`SaveCheckpoint` 的校验。
   若先改校验、后建表 → 每次写都 `no such table`（和 `rig_memory_pages` 漏 bump 那次同型）。
7. **`retryBusy` 全覆盖**（§5）放在租约之后：重试会重跑事务，租约校验必须在每次重试里重新求值。
8. **0600 chmod** 放在 `Close` 的 checkpoint 之后一起做（同一处顺序敏感逻辑），并在 `Open` 后兜一次。
9. **`Export`** 最后做：它依赖 `Import` 的幂等语义 + §2 的字段持久化决策（否则往返校验必然失败）。
10. **文档面**（§10）在代码落地后一次性改；`docs/sqlite.md:154-158` 的事实错误可以**现在**独立修（不依赖任何代码）。

## 13. 规模估算

| 维度 | 估算 | 依据 |
|---|---|---|
| 涉及文件（非测试） | **12 个改 + 6 个新增** | 改：`store.go`/`history.go`/`records.go`（少量）、`open.go`/`sessions.go`/`views.go`/`memory.go`/`spill.go`/`cost.go`/`schema.go`/`contract.go`、`go.mod`；新增：`storage/storage.go`、`storage/dir.go`、`sqlite/lease.go`、`sqlite/backup.go`、`sqlite/export.go`、`sqlite/canopen.go` |
| 涉及测试文件 | **18 个**（`session/store` 10 + `session/sqlite` 12 中的 10 改 4 新） | §9 两张表 |
| 符号（方法+函数） | **≈ 58 个**：`store` 侧 18（`Dir` 9 + `History` 5 + 内部 4）、`sqlite` 侧 21 方法 + 12 内部、新增约 14（`CanOpen`/`Verdict`/`probeVersion`/`migrationSteps`/`Backup`/`chmodStateFiles`/`AcquireLease`/`RenewLease`/`ReleaseLease`/`LeaseOwner`/`ExportToDir`/`VerifyRoundTrip`/`buildDSN`/`DirStorage`） | `go doc -all` 全量 + §1/§3/§5/§6/§7 行数 |
| 预计删除行 | **≈ 140-220 行** | `SessionLog` 接口与 `eventView` 拆分（−10）、`contract.go` 别名 5 个（−12，若删）、`replace`（−1）、各测试的 `log.AppendEvent` 改写（−20 净）、`store.Append`/`sqlite.Append` 若 deprecated 后清理（−8）、`ci.yml` 死分支（−7）、重复的基线用例搬走后（`session/store/store_test.go` −约 300 行、`session/sqlite/store_test.go` −约 130 行，但这部分是**搬迁**不是净删） |
| 净新增行 | **≈ 700-1000 行** | 租约 ~180、Backup ~90、CanOpen/迁移分段 ~220、Export/往返 ~200、storagetest ~300、chmod/retry ~40 |
| BREAKING 项 | **3** | ① `sqlite.SessionLog` 接口删除 + `SessionLog()` 返回类型改；② `store.Append` 建议 deprecated；③ 子模块 module 版本（`v0.24.0` → 实际版本）与"目录后端不支持多实例"从"隐式"变"显式契约" |

## 汇总 ① 必删清单

| 位置（文件:行） | 符号 | 理由 |
|---|---|---|
| `session/sqlite/go.mod:22` | `replace github.com/zeokeel/zeokeel-rig => ../..` | 掩盖 require 指向未发布版本；下游 `go get` 必失败（§8 已实测） |
| `session/sqlite/views.go:19-23` | `type SessionLog interface` | 与 `rig.SessionLog` 重复定义，且是"返回类型不一致"的根源 |
| `.github/workflows/ci.yml:337-343` | `stage()` 里的 `dir == "."` 分支 | 从未被调用（死分支）；要么调用要么删 |
| `session/sqlite/sessions.go:96-98` | `Store.Append`（**建议** deprecated 而非硬删） | PLATFORM §3 明确"契约里一律用 `AppendBatch`"；仓库内仅测试使用 |
| `session/store/store.go:188` | `Store.Append`（同上，deprecated 一版） | 同上 |
| `session/sqlite/contract.go:11,14` | `type Meta = store.Meta` / `type Filter = store.Filter`（**建议删**） | 别名把 `storage`→`store` 的类型耦合伪装成"契约一致"；保留 `Err*` 别名即可 |
| `session/store/audit2_perf_test.go:15`（`TestAuditPerfLoadAfter10kMessages`） | 若接入 `-short` 门禁则改 build tag | 10k 消息用例在 `-race` 下会拖慢子模块/根门禁 |

## 汇总 ② 疑似死代码（**改后需复核**）

| 位置（文件:行） | 符号 | 判定线索 | 复核动作 |
|---|---|---|---|
| `session/sqlite/open.go:321` | `Store.DSN()` | 全仓 0 非测试调用（仅 `coverage_test.go:97`） | 改后若 `buildDSN` 内部化，降为未导出 |
| `session/store/store.go:490` | `Dir.Dir()` | 仅 `history.go:116` 的 `String()` + 2 个测试 | 若 `History.String()` 改成缓存 root 则可删 |
| `session/sqlite/sessions.go:96` | `Store.Append` | 仅测试使用（`fix20_test.go:53,82,281,306,319`） | 与 `store.Append` 一起 deprecated |
| `session/store/history.go:112` | `History.String()` | 无调用方（仅接口满足） | 保留或删，二者都不影响编译 |
| `session/sqlite/open.go:370-399` | `objects`（在 `Schema` 之外的唯一调用者是 `Schema` 自己） | 只 1 个调用点 | 与 `CanOpen` 复用后再判 |
| `session/sqlite/migrate.go:70-75` | `existing` 里 `if current, err := target.List(...); err == nil` | 错误被静默忽略 | 改成显式报错（不是删，但**必须改**） |
| `session/sqlite/contract.go:36` | `var _ store.Store = (*Store)(nil)` | 若删 `contract.go` 的别名，此断言要挪到 `storagetest` 或换成 `storage.Storage` 断言 | 与 §3 的 `storage.Storage` 断言**并列**保留一个版本 |
| `.audit-scratch/sqlcheck/*`（gitignore） | `session/sqlite` 的同源拷贝（含 `views.go:32`、`fix20_test.go:108` 等） | 不在 git 索引里，但会污染 `grep`/本地构建 | 改动 sqlite 后同步删除或忽略；**不要**把它当第二个实现 |

## 汇总 ③ 需要新增的文件（含 `storagetest` 划分建议）

| 新增文件 | 内容 | 主责 | 为什么必须单独一个文件/包 |
|---|---|---|---|
| `storage/storage.go` | `type Storage interface{History/Checkpoints/Memory/Spill/Close}` + `Capabilities` 结构 | 另一人（契约层） | 铁律：不能在根 `rig` 包（`TestRootPackageStaysStdlibOnly`） |
| `storage/dir.go` | `DirStorage{dir *store.Dir; defaultID string}` + `NewDirStorage` + `var _ Storage = DirStorage{}` | 另一人 | PLATFORM §3 明确要适配器；**放这里**意味着 `storage`→`session/store` 单向依赖（§12 第 3 条） |
| `storagetest/factory.go` | `Factory interface{New(t *testing.T) Storage; Capabilities() Capabilities}` + 两个默认工厂（目录/内存占位） | **本范围** | 只有它能让"多实现分叉即红"成真；PLATFORM §5 步 0 |
| `storagetest/capabilities.go` | `Capabilities{MultiProcess, WorkspacePersistent, TitleAtStartup, Transactional, Search, Cost, Backup bool}` | **本范围** | §2/§5 的"目录后端不支持多实例"必须有个可执行的声明处 |
| `storagetest/history.go` | 基线：`Load` 未知会话 `(nil,nil)`、`Append` 原子性、`Replace` 语义、并发同会话、空批 no-op | **本范围** | 搬 `store_test.go` + `sqlite/store_test.go` + `views_test.go:18,32,50` 的公共部分 |
| `storagetest/checkpoints.go` | 基线：往返、缺省 `(nil,nil)`、`Clear` 幂等、删会话级联 | **本范围** | 搬到 `views_test.go:74,188` |
| `storagetest/spill.go` | 基线：`Save/Load/GC(keep)`、`ErrSpillNotFound` | **本范围** | 搬 `spill_test.go:17` |
| `storagetest/memory.go` | 基线：`memory.Wiki` 6 条契约规则（名称校验、摘要必填、Append 不自建、改写留痕、Index 全量、Search 排序） | **本范围（建议；PLATFORM 未规划）** | `memory` 今天有两个后端却**没有**共享套件（§9.2 的发现） |
| `storagetest/close.go` | 基线：`Close` 幂等、关闭后每个契约方法返回 `ErrClosed`、`Close` 与读写并发无 `-race` | **本范围** | 搬 `coverage_test.go:19` + `fix20_test.go:39` 的公共部分 |
| `session/sqlite/lease.go` | `AcquireLease/RenewLease/ReleaseLease/LeaseOwner` + `ErrLeaseHeld` | **本范围** | §5.1 |
| `session/sqlite/backup.go` | `Backup(ctx,dest)`（`VACUUM INTO`）+ `chmodStateFiles` | **本范围** | §6 |
| `session/sqlite/canopen.go` | `CanOpen(path) (Verdict, error)` + `probeVersion` + `Verdict{Upgrade, BackupFirst, ReadOnly}` | **本范围** | §4 |
| `session/sqlite/export.go` | `ExportOptions/ExportReport/ExportToDir/VerifyRoundTrip` | **本范围** | §7.2 |
| `session/sqlite/lease_test.go` / `backup_test.go` / `canopen_test.go` / `export_test.go` / `perm_test.go` | 对应新增面的门禁 | **本范围** | 每个新能力必须有独立回归；`perm_test.go` 在 Windows 上跳过 |

---

### 一句话结论

`sqlite` 侧的**只读五方法已经全部具备**（`History()` `views.go:26` / `Checkpoints()` `:29` / `Memory()` `memory.go:19` / `Spill()` `spill.go:19` / `Close()` `open.go:427`），
真正要动的是**五个"不满足或不安全"的地方**：`SessionLog()` 返回类型（13 个调用点）、`Close` 缺 `wal_checkpoint`、
写路径缺 busy 重试与租约、迁移缺 ALTER/`CanOpen`、子模块的 `replace`+`require`+consumer smoke 三个一起假绿。
目录后端则要**明确降级**：`Workspace`/`Model` 不持久化、不支持多实例——把这两件事写成 `Capabilities()` 而不是写在注释里。