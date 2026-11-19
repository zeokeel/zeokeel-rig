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

agent, err := agent.New(rig.AgentSpec{
	Provider:        provider,
	History:         db.History(),        // rig.History
	CheckpointStore: db.Checkpoints(),    // rig.CheckpointStore
	SessionLog:      db.SessionLog(),     // rig.SessionLog
})
```

**路径由宿主给。** 这个包永远不会自己挑位置：SDK 自己选目录，等于把用户数据放在
宿主不知道的地方——没人备份、没人清理、没人迁移。

## 存了什么

| 表 | 一行是什么 | 索引 |
|---|---|---|
| `rig_sessions` | 一个会话的元数据（标题、工作区、时间、消息数、revision） | `updated_at DESC, revision DESC`（列表顺序） |
| `rig_messages` | 一条消息（JSON 载荷 + 序号） | `(session_id, ordinal)` 唯一 |
| `rig_events` | 一条会话事件（类型、时间、载荷） | `(session_id, seq)` 唯一、`(kind, at)` |
| `rig_checkpoints` | 一个会话的中断检查点 | 主键 |
| `rig_costs` | 一次已结算请求的用量与金额 | `(session_id, at)`、`at` |

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
  第二个写者等待而不是失败；
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

子模块在 CI 里单独跑一遍 build/vet/test/race/coverage，见 `.github/workflows/ci.yml`
的 `test (session/sqlite submodule)` 步骤。

## 与目录实现的契约一致

`sqlite.Store` 实现了 `store.Store`、`rig.History`、`rig.HistoryReplacer`、
`rig.CheckpointStore` 和 `rig.SessionLog`；`sqlite.Meta`/`Filter`/`ErrNotFound` 等
都是 `session/store` 的别名，所以切换实现只改构造行，`errors.Is` 也照常工作。
