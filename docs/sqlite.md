# SQLite 存储（`session/sqlite`）

`session/store` 用"一个会话一个 NDJSON 文件"落盘：零依赖、可 `cat`、单进程足够。
产品一旦要回答**跨会话**的问题——最近哪些会话、每条会话多少消息、上周我提过什么、
这回合都发生了哪些工具事件、这次发布花了多少钱——文件目录就只能扫。

`session/sqlite` 是同一个会话契约的数据库实现：一个文件、一张表一条消息、索引在
旁边。它**不改变** `session/store` 的默认行为，也不改变主模块的依赖面（见"为什么
是子模块"）。

## 一句话用法

```go
import sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"

db, err := sqlitestore.Open(sqlitestore.Options{Path: filepath.Join(dataDir, "sessions.db")})
if err != nil {
	return err
}
defer db.Close()

a, err := agent.New(rig.AgentSpec{
	Provider:        provider,
	History:         db.History(),     // rig.History（视图，见"与 rig 契约的关系"）
	CheckpointStore: db.Checkpoints(), // rig.CheckpointStore（视图）
})

log := db.SessionLog() // sqlite.SessionLog = rig.SessionLog + AppendEvent
if err := log.AppendEvent(ctx, ev); err != nil {
	return err
}
```

`rig.AgentSpec` **没有** `SessionLog` 字段（`agent.go:105-145`：只有 `History`、
`CheckpointStore`、`Spill`、`Compactor` 等），所以事件日志不通过 AgentSpec 交给
agent：它是宿主与工具层（`kit.Config.SessionLog`、`session_*`）自己消费的。上面三个
访问器拿到的是**视图**，而 `*sqlite.Store` 本身**不实现**它们背后的四个 rig 接口
（`History`/`HistoryReplacer`/`CheckpointStore`/`SessionLog`，都在根包）——原因见
文末"与 rig 契约的关系"。

**路径由宿主给。** 这个包永远不会自己挑位置：SDK 自己选目录，等于把用户数据放在
宿主不知道的地方——没人备份、没人清理、没人迁移。

## 存了什么

| 表 | 一行是什么 | 索引 |
|---|---|---|
| `rig_sessions` | 一个会话的元数据（标题、工作区、时间、消息数、revision） | `updated_at DESC`（列表顺序；`revision` 只参与同一毫秒内的先后比较，不进索引，`ORDER BY updated_at DESC, revision DESC` 的第二个键会让 SQLite 建一个临时 B-tree——会话量在个人客户端里是几百条量级，这点代价远小于为它维护第二个索引） |
| `rig_messages` | 一条消息（JSON 载荷 + 序号） | `(session_id, ordinal)` 唯一 |
| `rig_events` | 一条会话事件（类型、时间、载荷） | `(session_id, seq)` 唯一、`(kind, at)` |
| `rig_checkpoints` | 一个会话的中断检查点 | 主键 |
| `rig_costs` | 一次已结算请求的用量与金额 | `(session_id, at)`、`at` |
| `rig_memory_pages` | 一条长期记忆页（名字、标题、摘要、正文、更新时间、字节数）——`Memory()` 背后的那一张表，**一张**不是多张 | 主键 `name`、`updated_at DESC`（`Index` 按最近修改排序） |
| `rig_spill` | 一条溢出工具结果（模型只看到 `ref:` 句柄）：句柄、工具名、字节数、正文、时间 | 主键 `seq`（`AUTOINCREMENT`：GC 删行后不复用，否则"保留最新 N 条"会删掉刚写的那条）、`id` 唯一 |

`revision` 解决的是"同一毫秒两次写入"的顺序：没有它，列表顺序会随 id 的随机后缀
摆动。

## 扩展表结构

宿主功能要自己的表，就注册一个 `Extension`：名字唯一、语句幂等、只应用一次，记录
在自己的账本 `rig_schema_extensions` 里，核心 schema 永远不因为宿主加了什么而变。

```go
db, err := sqlitestore.Open(sqlitestore.Options{
	Path: dbPath,
	Extensions: []sqlitestore.Extension{{
		Name: "myapp/notes",
		Statements: []string{
			`CREATE TABLE IF NOT EXISTS myapp_notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS myapp_notes_body ON myapp_notes(body)`,
		},
	}},
})
```

`db.Schema(ctx)` 返回版本、表、索引、扩展账本与 `rig_sessions` 的列，宿主的启动自检
（"我的迁移跑没跑"）用它，不用自己查 `sqlite_master`。

## 并发与崩溃

- WAL + `busy_timeout`（默认 5s，可配）：一个窗口和一次后台运行同时写是正常情况，
  第二个写者**在 busy_timeout 之内**等待而不是立刻失败；超时之后驱动返回
  `SQLITE_BUSY`，写路径再做有限重试（3 次指数退避、总等待不超过 2s），仍拿不到锁就
  返回可用 `errors.Is(err, sqlite.ErrBusy)` 判定的错误；
- 每次写入是一个事务：`AppendBatch` 要么全写要么全不写，`Replace`（压缩）不会让
  读者看到半个窗口；
- 外键级联：删会话就是删它的消息、检查点（事件按会话显式删）；
- 中断检查点可以先于第一条消息存在（回合在首次 append 前就失败也要能续跑）。

## 从 NDJSON 迁移

```go
report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: oldDir, DryRun: true})
// DryRun 只读不写，宿主先给用户看预览
report, err = sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: oldDir})
```

导入按会话幂等：已经入库的会话被跳过，所以中断后重跑不会重复。

## 检索

```go
hits, _ := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "发布清单", Limit: 20})
events, _ := db.SearchEvents(ctx, sqlitestore.EventQuery{Type: "tool.call", Since: weekAgo})
```

消息检索大小写不敏感、`%`/`_` 按字面匹配（用户输入里的下划线就是下划线），命中带
上下文片段；事件可按类型/时间/载荷同时过滤。

## 为什么是子模块

主模块的门禁把编译进来的 import 钉死成一个解析器依赖（`mvdan.cc/sh/v3`），并要求
cgo-free。SQLite 驱动（`modernc.org/sqlite`，纯 Go、无 cgo）虽然不是 cgo，但依赖树
很大。把它放进**独立 module** 后：

- 只想要 NDJSON 的宿主，依赖面一个字节都没变；
- 想要数据库的宿主 `go get github.com/zeokeel/zeokeel-rig/session/sqlite` 即可；
- 主模块的 `make deps` / `cgo-free` 门禁照常通过。

子模块在 CI 里单独跑一遍：

- `CGO_ENABLED=0 go build/vet/test`（Linux）；
- **交叉编译**：`GOOS=darwin` 与 `GOOS=windows` 的 `go build ./...`（纯 Go 驱动意味着
  三个平台都能编；`go mod tidy` 漏掉某个间接依赖的 `go.sum` 条目时，只有交叉编译会
  报错，所以这条不能省）；
- `-race`（`CGO_ENABLED=1`）；
- `go mod tidy -diff`（不允许提交未 tidy 的 go.mod/go.sum）；
- **下游消费冒烟**：临时 module 里 `go get` 本子模块并 `go build`，模拟真实宿主。

见 `.github/workflows/ci.yml` 的 `test (session/sqlite submodule)` 步骤（`:71`，race 版在
`:104`）与 `sqlite-consumer-smoke` job（`:305`）。

## 版本与本地开发

子模块是**独立 module**，因此需要**自己的 tag**（`session/sqlite/vX.Y.Z`，见
`docs/RELEASE.md`）。`session/sqlite/go.mod` 里的
`require github.com/zeokeel/zeokeel-rig vX.Y.Z` 指向它编译所需的主模块版本。

**现状（两处"本地开发态"，都必须在发布时清掉）**：

- `session/sqlite/go.mod:6` 写着 `require github.com/zeokeel/zeokeel-rig v0.24.0`，而
  `v0.24.0` **从未打过 tag**（`git tag -l 'v0.24*'` 为空；根 tag 最高 `v0.23.4`）——
  下游 `go get` 必然解析失败。**发布时随 bump 改成实际要发的 `vX.Y.Z`**：先打根 tag，
  再打 `session/sqlite/vX.Y.Z`。`session/sqlite/*` 目前**一个 tag 都没有**
  （`git tag -l 'session/sqlite/*'` 为空）。
- `session/sqlite/go.mod:22` 还留着 `replace github.com/zeokeel/zeokeel-rig => ../..`。
  本地能编过正是靠它，但它是**临时手段**：留着 replace 的 `go.mod` 不能发版，也不能
  当成"子模块已能被宿主消费"的证据。

所以"**不要用 `replace ../..`**"这句话描述的是**发布面**，而**不是今天的文件**——
两者目前不一致，以 `go.mod` 为准，改动落在发布那一步。本地开发继续用仓库根的
`go.work`（它不进发布面）：

```bash
cd <repo>
go work init . ./session/sqlite   # 已存在则跳过；go.work 已被 .gitignore 忽略
```

`go.work` 只影响本机构建；子模块的 `go.mod` 最终必须"指向真实版本"，且不含 replace。

## Meta.Bytes 与 Meta.Model 恒为 0 / 空

`store.Meta` 有 `Bytes`（会话占用字节数）和 `Model`（会话主模型）两个字段。本实现
**当前不维护它们**：`Create` 不接收字节数、`Append`/`Replace` 也不累加，因此读回来
的值恒为 `0` 与 `""`。

为什么不强行填：

- `Bytes` 需要精确统计消息序列化后的体积，而 SQLite 的 `length(payload)` 与宿主观测
  的"磁盘占用"（页对齐 + WAL）本来就不一致；给一个"看起来精确但都不对"的数字，比
  明确留空更容易误导。
- `Model` 在一次会话里可以变（用户中途换模型），把它压成会话级单值会丢信息；需要
  "这个会话用过哪些模型"的宿主应当查 `rig_costs`（`CostByModel`）。

如果宿主依赖这两个字段做 UI，请按"0/空 = 未统计"处理；将来若开始维护，会在
CHANGELOG 里写明。

## 与 rig 契约的关系（视图 ≠ 接口实现）

目录后端那条契约 **`store.Store`** 它是真实现的（`contract.go:36`：
`var _ store.Store = (*Store)(nil)`）。但根包 `rig` 的四个接口（`History`/`HistoryReplacer`/
`CheckpointStore`/`SessionLog`，定义见 `tool.go:53,62`、`checkpoint.go:162`、
`extensions.go:111`）它**一个都不实现**——它们由 `History()`/`Checkpoints()`/
`SessionLog()` 返回的**视图**实现，`*Store` 只是这些视图的构造入口（`views.go:26/29/32`）：

| 接口（都在根包 `rig`） | `*sqlite.Store` 为什么不满足（编译探针实测） | 谁满足 |
|---|---|---|
| `History` | `Append` 是 `(ctx, id string, msg rig.Message) (Meta, error)`——单条、返回 `Meta`；契约要 `(ctx, id string, messages ...rig.Message) error`（`tool.go:53-60`） | `historyView`（`views.go:38` 断言） |
| `HistoryReplacer` | `Replace` 返回 `(Meta, error)`；契约要 `error`（`tool.go:62-64`） | `historyView`（`views.go:39` 断言） |
| `CheckpointStore` | 方法集里**没有任何检查点方法**：`Load`/`Save`/`Clear` 全在视图上（`checkpoint.go:162-170`）。探针报 `missing method Clear` | `checkpointView`（`views.go:67` 断言） |
| `SessionLog` | 缺 `Events`：`Events`/`Search`/`Last` 只在 `eventView` 上（`extensions.go:111-118`）。探针报 `missing method Events` | `eventView`（`var _ SessionLog = eventView{}`） |

**视图 ≠ 接口实现**，写法上就是这一行：

```go
var h rig.History = db            // 编译不过：*sqlite.Store 不是 rig.History
var h rig.History = db.History()  // 正确：db.History() 返回 rig.History
```

四个视图都是**未导出类型**，宿主只应通过 `History()`/`Checkpoints()`/`SessionLog()`
取——这也是聚合契约的形状：它只要求 `History() rig.History` 这样的**访问器**，不要
求 `*sqlite.Store` 自己变成 `History`/`SessionLog`。

`db.SessionLog()` 的**声明返回类型**是包内的 `sqlite.SessionLog`（`views.go:19-23` =
`rig.SessionLog` + `AppendEvent`）。它的方法集是超集，所以

```go
var log rig.SessionLog = db.SessionLog() // 成立：接口→接口再赋值只看方法集包含
```

但**方法签名**仍不匹配任何声明 `SessionLog() rig.SessionLog` 的契约（探针：
`have SessionLog() sqlite.SessionLog`／`want SessionLog() rig.SessionLog`）——契约若
把 `SessionLog()` 写进去，就必须先改返回类型（`docs/PLATFORM.md` §3 缺口表第 1 行）。

`sqlite.Meta`/`Filter`/`ErrNotFound` 等都是 `session/store` 的**类型别名与同一个错误
值**（`contract.go:11/14/20`），所以切换实现只改构造行，`errors.Is` 也照常工作。
