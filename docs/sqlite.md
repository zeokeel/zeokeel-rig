# 会话持久化：内存 / NDJSON / SQLite

会话状态是"这一回合还能不能正确继续"的依据：消息、崩溃检查点、事件、记忆与溢出
结果都属于它。Rig 给三种形态，按宿主需要的能力递增；另外还有一个**未发布**的
`storage` 聚合契约，把"四类状态交给 SDK"这件事压成一个字段。本文读者是想给个人
agent 客户端选持久化方案、或准备从 NDJSON 迁到数据库的工程师。

三句话概括：

- `session.Memory` —— 进程内，零依赖，进程退出即丢；
- `session/store` —— 一个会话一个 NDJSON 文件，零依赖、可 `cat`、单实例写；
- `session/sqlite` —— 一个 `.db` 装全部（消息/事件/检查点/记忆/溢出/花费），
  **独立子模块**，跨会话检索与迁移，**尚未发布**。

## 1. 三种形态对照

| | `session.Memory` | `session/store` | `session/sqlite` |
| --- | --- | --- | --- |
| 落地 | 进程内存 | `<dir>/<id>.ndjson`，一行一条 `rig.Message` | 一个 SQLite 数据库文件 |
| 依赖 | 无 | 无（只用标准库） | `modernc.org/sqlite`（纯 Go、无 cgo），**独立 module** |
| 接口 | `rig.History`（+ `HistoryReplacer`） | `store.Store`、`rig.History`、`rig.HistoryReplacer` | `store.Store`（`*Store` 直接实现）；`rig.History`/`CheckpointStore`/`SessionLog` 由**视图**实现 |
| 检查点 | ✗ | ✗ | ✓（`rig_checkpoints`） |
| 长期记忆 | ✗（记忆不归会话管） | ✗ | ✓（`rig_memory_pages`） |
| 溢出结果 | ✗ | ✗（由 `SpillPolicy` 的目录后端管） | ✓（`rig_spill`） |
| 事件 | ✗ | ✗ | ✓（`rig_events`） |
| 花费 | ✗ | ✗ | ✓（`rig_costs`） |
| 跨会话查询 | ✗ | 只能 `List` 元数据 | ✓ 消息检索、事件检索、花费汇总 |
| 崩溃恢复 | ✗ | ✓ 追加写 + fsync；重启重新扫目录 | ✓ 事务 + WAL |
| 并发写 | 进程内并发安全 | **单目录单实例** | 进程内单连接串行写；多个进程共库靠 BUSY 重试，回合归属靠**租约** |
| 发布状态 | 已发布 | 已发布 | **未发布**（见 §7） |

选择建议：无头/测试用 `session.Memory`；单用户桌面产品要"关掉窗口再打开还能续"
用 `session/store`；要侧栏检索、事件轨迹、花费统计、或者本来就想把状态放进数据库，
用 `session/sqlite`（等它发布之后）。

## 2. `session.Memory`（进程内）

`session.NewMemory()` 返回一个按 `sessionID` 隔离的内存消息存储，实现
`rig.History`（`Load` 读取、`Append` 追加）与 `rig.HistoryReplacer`（`Replace`，
供上下文压缩落盘）。几条语义值得记住：

- `Load` 返回**深拷贝**（外层切片与每条消息的 `Content`/`ToolCalls` 都被复制），
  调用方改返回值不会污染已存历史；`Append` 存入前同样深拷贝。
- 一次 `Append` 是原子单位：并发读者只会看到"全部消息"或"一条都没有"。
- 未知会话的 `Load` 返回 `(nil, nil)`——这是 `rig.History` 契约，不是错误。
- 空 `sessionID` 是合法键；是否允许空会话由上层（`rig.RunRequest` 的约定）决定。

它是 `kit` 在没有任何持久化字段时的默认历史。

## 3. `session/store`（NDJSON 目录）

### 3.1 布局与索引

```
<dir>/<id>.ndjson    一行一条 rig.Message（追加写 + fsync）
```

目录用 0700 创建，会话文件用 0600。**没有索引文件**：`NewDir` 扫一遍目录，按文件名
（→ 会话 ID）与文件的 mtime/大小建**内存索引**，之后 `Create`/`Append`/`Replace`/
`Delete` 只维护内存索引。没有索引文件，就没有"索引与数据不一致"这一类问题，重启
也不需要迁移或重建逻辑——索引就是扫目录。

`Meta` 各字段在重启后的来源：

| 字段 | 来源 | 备注 |
| --- | --- | --- |
| `ID` | 文件名 | 不安全字符的 ID 用**可逆**编码 `enc-<hex>` 落盘（超长 ID 退化成 `id-<sha256>`，不可逆）；`idFromStem` 能把原名还原出来，否则续传就断了 |
| `Title` | 读会话开头（默认开） | ≤ 256 KiB、最多前 4 条记录里找首条 user 消息，按 rune 截断到 120（`MaxTitleRunes`）。不需要标题时用 `WithScanContents(false)` 关掉（超大目录追启动速度） |
| `UpdatedAt` | 文件 mtime | `List` 按它倒序 = "最近会话在最前" |
| `Bytes` | 文件大小 | 含被跳过的损坏字节 |
| `Messages` | 0 | 扫目录不数条数（要读完整文件），等该会话第一次 `Load`/`Append` 补全 |
| `Workspace` / `Model` | 空 | **只存在于内存索引**，重启无法从文件恢复 |

### 3.2 接口

`store.Store` 的方法：`Create` / `Append` / `AppendBatch` / `Load` / `List` /
`Delete` / `Close`，外加 `Dir.Replace`（`History.Replace` 的底层：原子重写整个会话文件）。

- `List(ctx, store.Filter{Workspace, Since, Limit})` **只读内存索引**，不碰数据文件；
  顺序是 `UpdatedAt` 倒序、同时间按 ID 倒序（结果确定）。
- `Load` 返回 `(Meta, []rig.Message, error)`；会话不存在返回 `ErrNotFound`。
- 单条消息序列化后上限 **4 MiB**（`MaxMessageBytes`），超了返回
  `ErrMessageTooLarge` 而不是静默截断（截断会让历史回放时"少一半"）。
- 写侧与读侧一样严格：缺 `role` 的消息直接 `ErrInvalidMessage`——否则会出现
  "`Append` 成功、`Load` 读不回来"的静默丢数据。
- `WithNotice(func(format string, args ...any))` 是诊断通道：损坏行被跳过、文件缺失
  等**不致命**异常都通过它上报；回调在锁外调用，可以安全地回调 Store。

### 3.3 崩溃自愈

写入顺序本身就是协议：

1. `Append`/`AppendBatch` 把整批消息编码成 NDJSON 后**一次写入 + `Sync`**；
2. `Create` 用 `O_CREATE|O_EXCL` 建空文件，成功后才进内存索引；
3. `Replace`（上下文压缩）写临时文件 → `Sync` → `rename` → fsync 目录，读者只会
   看到旧的或新的完整内容。

读取时的容错规则（顺序重要）：尾部没有换行结尾的**半行丢弃**（视为未完成的写入）；
完整行里解析不出带 `role` 的 `rig.Message` 就**跳过**并上报 Notice；只要有一行成功
就返回成功；文件里有完整行但**全部**无法解析才返回 `ErrCorrupt`（让调用方显式决策，
而不是静默丢光会话）。追加之前会先把半行 `Truncate` 掉，否则新记录会粘在坏行后面
变成一条真正的坏行。

### 3.4 能力与限制

- **单目录单实例写**：`Dir` 不做跨进程/跨实例加锁。两个实例各有独立的内存索引，
  因此看不到对方新建/写入的会话——数据都在盘上（不会丢），但列表不完整。要"重启后
  继续"，先关掉旧实例再 `NewDir`。`Append`/`Load`/`Delete` 里有一条跨实例兜底
  （索引未命中时按磁盘补一条，`recoverIndex`），它只补索引、不加锁，**不构成**
  多写者支持。
- **目录会自愈，不会误判**：子目录、点文件、非 `.ndjson` 条目一律忽略；不是本包写的
  文件名直接跳过（文件仍在，不丢）。
- **关闭语义**：`Close` 只置关闭标志（没有 WAL、没有需要 flush 的缓冲），之后所有
  调用返回 `ErrClosed`，可重复调用。

### 3.5 接给 agent：`Dir.History`

```go
st, err := store.NewDir("/var/lib/myapp/sessions") // 0700
if err != nil {
    return err
}
defer st.Close()

history := st.History(context.Background(), "default") // 默认会话 ID
a, err := agent.New(rig.AgentSpec{Provider: provider, History: history})

// 侧栏：List + Load
metas, err := st.List(ctx, store.Filter{Limit: 20}) // 最近的在最前
meta, messages, err := st.Load(ctx, metas[0].ID)
```

`History` 的会话分流规则：**非空 `sessionID` 优先**，空则回落到构造时给的默认 ID。
所以一个 `History` 实例可以服务多个会话（`agent` 把 `RunRequest.SessionID` 原样传
进来）。`Load` 对未知会话返回 `(nil, nil)`；`Append` 在会话不存在时自动创建（标题
取首条 user 消息）——"崩了怎么续"只需要同一个 `sessionID`。

### 3.6 适配聚合契约：`store.DirStorage`

`store.NewDirStorage(dir)` 把目录后端适配成 `storage.Storage`（见 §5）。它诚实地只
提供一项能力：

| 访问器 | 结果 |
| --- | --- |
| `History()` | 绑定 `store.DefaultSessionID`（`"default"`）的 `rig.History`，同时转发 `Replace` |
| `Checkpoints()` | 桩：每个方法返回包了 `store.ErrUnsupported` 的错误 |
| `Memory()` | 同上的桩 |
| `Spill()` | 同上的桩 |
| `Close()` | 转发 `Dir.Close`（幂等） |

三个"不支持"**不返回 nil**（`storage.Storage` 的接口文档要求访问器不返回 nil），也
不伪造能力：宿主用 `errors.Is(err, store.ErrUnsupported)` 就知道"这项能力得换后端
（`session/sqlite`）"，而不是把它当存储故障重试。零值 `DirStorage` 不是 panic 陷阱
（`History()` 返回一个每个方法都报配置错误的句柄，`Close()` 返回 nil）。

## 4. `session/sqlite`（独立子模块，未发布）

### 4.1 定位与打开

模块路径 `github.com/zeokeel/zeokeel-rig/session/sqlite`，自带 `go.mod`。放在独立
module 里是因为主模块的依赖门禁只允许一个解析器依赖（`mvdan.cc/sh/v3`）：想要
NDJSON 的宿主依赖面一个字节都不变，想用数据库的宿主单独引这一个子模块。

```go
import sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"

db, err := sqlitestore.Open(sqlitestore.Options{
    Path: filepath.Join(dataDir, "sessions.db"), // 必填：路径由宿主给
})
if err != nil {
    return err
}
defer db.Close() // 先 wal_checkpoint(TRUNCATE)，再关连接池
```

- **路径由宿主给**：`Options.Path` 为空返回 `ErrNoPath`。SDK 自己挑存储位置，等于把
  用户数据塞进宿主不备份、不迁移、不清理的地方。库文件所在的**目录必须已存在**
  （本包建文件，不建上层目录）。
- 打开之后立刻把 `.db` / `-wal` / `-shm` 三件套收紧到 **0600**（Windows 上没有
  POSIX 权限位，跳过而不是假装设置了）。收紧失败就让 `Open` 失败——库里有会话正文，
  "打不开"远比"打开了一个同机其他账户能读的文件"好。
- 连接串固定 `journal_mode(WAL)`、`foreign_keys(1)`、`synchronous(NORMAL)`、
  `busy_timeout(默认 5000ms)`；`SetMaxOpenConns(1)`：同一时刻只允许一个写者，避免
  并发写把 WAL 撑大（桌面流量小且突发，串行写不是瓶颈）。

### 4.2 存了什么

| 表 | 一行是什么 | 索引 |
| --- | --- | --- |
| `rig_sessions` | 会话元数据（标题、工作区、时间、消息数、字节数、`revision`） | `updated_at DESC` |
| `rig_messages` | 一条消息（JSON 载荷 + 序号） | `(session_id, ordinal)` 唯一 |
| `rig_events` | 一条会话事件（种类、时间、载荷） | `(session_id, seq)` 唯一、`(kind, at)` |
| `rig_checkpoints` | 一个会话的中断检查点 | 主键 `session_id` |
| `rig_costs` | 一次已结算请求的用量与金额 | `(session_id, at)`、`at` |
| `rig_memory_pages` | 一条长期记忆页 | 主键 `name`、`updated_at DESC` |
| `rig_spill` | 一条溢出工具结果 | `seq`（`AUTOINCREMENT`）主键、`id` 唯一 |
| `rig_schema_extensions` | 宿主扩展的账本（`name`、`applied_at`） | — |

`revision` 解决"同一毫秒两次写入"的顺序：没有它，列表顺序会随 id 的随机后缀摆动。
`rig_messages` 的 `(session_id, ordinal)` 唯一索引既是读取路径，也让重试的追加变成
幂等而不是重复一条消息。

**两个字段的现状**（不要把 0 当真实统计）：

- `Meta.Bytes` 只在 `Create` 时照抄调用方给的值，`Append`/`Replace` **不维护**它；
  经默认路径（`AppendBatch` 自动建会话）创建的会话读回来是 0。`length(payload)` 与
  宿主观测的磁盘占用（页对齐 + WAL）本来就不一致，给一个"看起来精确但都不对"的数字
  更容易误导。
- `Meta.Model` **没有对应的列**，恒为空。会话中途可以换模型；要"这个会话用过哪些
  模型"，查 `rig_costs`（`CostByModel`）。

### 4.3 视图：给 agent 用的四个面

`*sqlite.Store` 实现的是 `store.Store`（`var _ store.Store = (*Store)(nil)`）。根包
`rig` 的四个接口由**视图**实现，`*Store` 本身不实现它们：

| 访问器 | 返回类型 | 谁实现接口 |
| --- | --- | --- |
| `db.History()` | `rig.History` | 内部 `historyView`（同时实现 `rig.HistoryReplacer`） |
| `db.Checkpoints()` | `rig.CheckpointStore` | 内部 `checkpointView` |
| `db.SessionLog()` | `sqlite.SessionLog` = `rig.SessionLog` + `AppendEvent` | 内部 `eventView` |
| `db.Memory()` | `memory.Wiki` | 内部 `memoryStore` |
| `db.Spill()` | `rig.SpillStore` | 内部 `spillStore` |

所以写法上要这样：

```go
var bad rig.History = db           // 编译不过：*sqlite.Store 不是 rig.History
var h rig.History = db.History()   // 正确：访问器返回视图

a, err := agent.New(rig.AgentSpec{
    Provider:        provider,
    History:         db.History(),
    CheckpointStore: db.Checkpoints(), // 崩溃恢复：没有它就没有跨进程续跑
})

// 事件日志不归 agent.AgentSpec 管（它没有 SessionLog 字段）：
// 宿主与工具层（kit.Config.SessionLog、session_*）自己消费。
log := db.SessionLog()
err = log.AppendEvent(ctx, rig.SessionEvent{ /* … */ })
```

`rig.AgentSpec` 只有 `History`、`CheckpointStore`、`Spill`、`Compactor` 等字段；
`SessionLog` 是宿主/工具层的能力（`kit.Config.SessionLog`）。

### 4.4 跨会话检索

```go
hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{
    Text:      "发布清单",
    SessionID: "",        // 空 = 所有会话
    Limit:     20,        // 0 = DefaultSearchLimit（50）
})
events, err := db.SearchEvents(ctx, sqlitestore.EventQuery{
    Type:  "tool.call",
    Since: weekAgo,
})
```

- `SearchMessages` 跨会话按正文做大小写不敏感的 `LIKE`，`%`/`_`/`\` 按**字面量**
  匹配（`ESCAPE '\'`）；命中按会话最近更新时间倒序、再按 `session_id`、`ordinal`。
  每行是 `MessageHit{SessionID, Title, Ordinal, Role, Time, Snippet}`，`Snippet`
  是命中位置两侧各 48 个**字符**的上下文（两侧被截断时加 `…`）。
- `AfterOrdinal` 用于续页，只在**单个会话**内有意义（消息按会话读），请与
  `SessionID` 一起用。
- `SearchEvents` 按 `SessionID` / `Type` / `Since` / `Until` / `Text` 组合过滤，
  返回 `EventHit{SessionID, Seq, Type, Time, Payload}`，`Limit` 同样是 50。
- 两者都只读，`Limit` 是上限而不是分页游标（消息面用 `AfterOrdinal`，事件面用
  `Since` 收窄时间窗）。

### 4.5 花费

```go
err = db.SaveCost(ctx, sessionID, sqlitestore.CostEntryFromSummary(model, meter.Session()))
totals, err := db.CostTotals(ctx, sqlitestore.CostFilter{SessionID: sessionID})
byModel, err := db.CostByModel(ctx, sqlitestore.CostFilter{Since: monthStart})
```

内存里的 `costmeter.Meter` 回答"这一轮到现在花了多少"；数据库回答"上周一共花了
多少"——进程退出之后还答得出。`CostEntry.CostUSD` 为 0 表示价目表里没有这个模型，
这一行**仍然存下来**（用量数据不会因此丢失）。

### 4.6 扩展表结构

```go
db, err := sqlitestore.Open(sqlitestore.Options{
    Path: dbPath,
    Extensions: []sqlitestore.Extension{{
        Name: "myapp/notes", // 约定 "<宿主>/<特性>"，非空且本次 Open 内唯一
        Statements: []string{
            `CREATE TABLE IF NOT EXISTS myapp_notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)`,
            `CREATE INDEX IF NOT EXISTS myapp_notes_body ON myapp_notes(body)`,
        },
    }},
})
```

- 扩展在核心 schema 之后、在**一个事务**里按顺序执行，成功后记进独立账本
  `rig_schema_extensions`，**只执行一次**（同一个名字之后再 Open 会跳过）。
- 语句请写成幂等的：宿主手工删掉账本之后必须能重新应用。
- 核心 schema 不会因为宿主加了东西而改变；`db.Schema(ctx)` 返回当前版本、表、索引、
  已应用的扩展名与 `rig_sessions` 的列——宿主启动自检"我的迁移跑没跑"用它，不用自己
  查 `sqlite_master`。
- 核心 schema 版本在 `user_version` 里（当前 `coreVersion = 3`），按段迁移：每段执行
  完才写版本号。库比本程序新时 `Open` 拒绝（旧二进制写新库会丢字段），此时先用
  `sqlitestore.CanOpen(path)` 拿到可执行的下一步（`create` / `open` / `backup-first` /
  `upgrade-build` / `read-only`），而不是只看一条"太新"的错误。

### 4.7 关闭、备份与导出

- **`Close`**：先 `PRAGMA wal_checkpoint(TRUNCATE)`（把已提交的帧并回主库并截断
  WAL），再关连接池。没有这一步，用户"只拷 `.db`"拿到的是缺最近提交的旧快照，拷贝
  落在写入中途时甚至 malformed。checkpoint 失败**不阻断**关闭（数据还在 `-wal` 里，
  下次打开会重放），但错误会返回：宿主需要知道"这个库现在拷不得"。`Close` 可重复
  调用，之后所有导出方法返回 `ErrClosed`（不 panic）。
- **`Backup(ctx, path)`**：用 `VACUUM INTO` 写一份**一致快照**，可以在库正常运行、
  有写入时调用；目标文件**必须不存在**（覆盖等于悄悄毁掉上一份备份 →
  `ErrBackupTargetExists`），目录必须已存在。产物是单个自包含文件。
- **`Export(ctx, dir)` / `ExportToDir(ctx, opts)`**：把库写回 NDJSON 目录（`ImportDir`
  能直接读回来）。`Export` 对同名会话原子覆盖，任一会话失败即整体返回错误；需要
  "跳过并报告"用 `ExportToDir` + `ExportOptions.Overwrite=false`。
  **导出不是完整备份**：本版只导出会话与消息，事件/花费/检查点不导出
  （`ExportReport.NotExported` 会如实列出）。
- **`VerifyExport(ctx, dir)`**：把导出目录重新导入一个全新临时库，逐会话比对消息
  条数与字节，返回 `RoundTripReport{Sessions, Messages, Bytes, Mismatches}`——
  升级/迁移之后跑一次，回答"这份备份到底能不能还原"。

### 4.8 并发、BUSY 与单实例

- **写路径有限重试**：`busy_timeout` 到点后驱动返回 `SQLITE_BUSY`，写路径再做 3 次
  指数退避（15 ms 起，总等待不超过 2 s），仍拿不到锁就返回可用
  `errors.Is(err, sqlitestore.ErrBusy)` 判定的错误。重试包住**整个事务**（而不是
  单条语句）：锁冲突时必须重开事务、重新读一次，否则重试用的是过期快照。
- **一次调用一个事务**：`AppendBatch` 要么全写要么全不写；`Replace` 不会让读者看到
  半个窗口；`Delete` 把"删会话 + 删事件"放进同一事务（分开执行时崩溃会留下孤儿事件，
  会话以同一 ID 重建时旧事件会"复活"）。
- **提交后回读用 `context.WithoutCancel`**：调用方的 ctx 可能在这几微秒里被取消，
  那样会返回"取消错误"而数据其实已落库——宿主看到错误后重试，就会把同一批消息写
  两遍。
- **单实例与租约**：同一个库可以被两个进程打开（桌面产品里"一个窗口 + 一个后台任务"
  是常态），所以"这个会话现在由谁在跑回合"必须有**库内**的答案：

  ```go
  err = db.AcquireLease(ctx, sessionID, owner, ttl) // ErrLeaseHeld：别人持有且未过期
  err = db.RenewLease(ctx, sessionID, owner, ttl)
  err = db.ReleaseLease(ctx, sessionID, owner)
  lease, err := db.LeaseOwner(ctx, sessionID)       // ErrNoLease：从未获取/已释放
  ```

  语义：一个会话同一时刻只有一个 owner（`session_id` 主键 + 一条
  `INSERT … ON CONFLICT DO UPDATE … WHERE`，读-判断-写是原子的）；`expires_at` 到期后
  任何进程都能抢占；同一个 owner 重复 `Acquire` 是幂等续租；`RenewLease`/
  `ReleaseLease` 都带 `owner = ?`（被抢走之后老进程既续不了、也删不掉新持有者的行）。
  租约**不要求会话行已存在**（Run 可能在第一条消息之前就取租约），所以这张表没有外键。
  为什么需要它：agent 每个 Run 开头都会把"有检查点、没有结束标记"的回合判定为崩溃
  残留并补一条合成消息；第二个进程会把第一个进程**正在跑**的回合当成残留，写坏用户
  可见的历史且不可回退。
  **时钟依赖**：判断用墙钟毫秒。同机没问题；共享网络文件系统上时钟不同步会出问题
  （快的提前抢占活着的回合，慢的长时间拿不到），系统时间回拨也可能让过期租约继续
  有效。TTL 要选得让"一次正常回合"远小于它，长回合里配合 `RenewLease`。

### 4.9 从 NDJSON 迁移：`ImportDir`

```go
preview, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: oldDir, DryRun: true})
// DryRun 只读只数、不写库：宿主先给用户看"将导入 N 个会话、M 条消息"
report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: oldDir})
```

- **幂等**：续跑判据是"**库内消息数 ≥ 源消息数**"才跳过，而不是"会话 ID 已存在"——
  上一次导入可能在 Create 成功、AppendBatch 失败处中断，库里留下一个空壳会话；只看
  ID 会把它永久跳过，源里的消息永远进不来。不满就按 `ordinal` 续追加。
- **不中断整次迁移**：读不出来的会话进 `report.Failed`（带原因），其余照常导入；
  `report.Skipped` 列出已完整的会话；`report.Sessions`/`Messages` 是"发现"的数量
  （试运行也计入），`report.Imported` 是本次真正写入的会话数。
- 源目录不存在时直接报错（`store.NewDir` 会把空目录建出来，然后"什么都没导入"）。
- 导入按 `CreatedAt` 从旧到新，让库里的排序与源目录一致。
- 反向出口是 `Export`/`ExportToDir`（§4.7），`VerifyExport` 可以校验往返。

## 5. `storage` 聚合契约（**未发布**）

包 `storage` 把"会话历史、检查点、记忆、溢出结果"四类状态压成**一个**注入点：
宿主只给一个值，就能把会话状态一次交给 SDK。

```go
type Storage interface {
    History() rig.History                 // 必需：会话消息读写（layer 2 实现）
    Checkpoints() rig.CheckpointStore     // 必需：崩溃恢复的唯一依据
    Memory() memory.Wiki                  // 可选能力，但两个现成后端都已有
    Spill() rig.SpillStore                // 必需：Save/Load/GC 必须同属一个后端
    Close() error                         // 必需：生命周期最后一环
}
```

### 5.1 实现者必须保证

- 四个访问器**并发安全**，并**原样返回**注入的同一个实例（侧栏、诊断与"我传进去的
  那个后端就是被用的那个"这一直觉都依赖它）。
- 访问器**不返回 nil**：某类能力不存在时返回一个每个方法都报"不支持"的**桩**。
  nil 会让调用方在离真正原因很远的地方 panic。
- `Close` 幂等，并把后端的真实错误透出（例如 WAL 没 checkpoint、连接没关干净）。
- 调用顺序是硬要求：**先取消/收尾在飞回合与后台任务、排空事件，再 `Close`**。反过来
  做会让运行中的回合拿到"已关闭"错误，把一次正常回答判成失败（用户看到"回答到一半
  消失"）。关闭之后访问器仍返回句柄，句柄自身的方法返回"已关闭"类错误。

契约刻意只声明**五个访问器**：同名不同签名的方法（`Load` 出现 4 次、`Save`/`Append`
各 3 次、`Search`/`Delete` 各 2 次）无法在同一个接口里共存，所以不能靠嵌入已有接口
拼出来。

### 5.2 分层铁律

```
layer 0  根包 rig                                  stdlib only，最小契约叶子
layer 1  storage（本包）                           stdlib + 本仓 rig / memory，聚合契约
layer 2  session/store、session、internal/memwiki  零依赖实现
layer 3  子模块 session/sqlite                     modernc.org/sqlite，参考实现
layer 4  宿主                                      只写业务逻辑
```

由此推出两条由测试守住的约束：

- `storage` **不得** import `session/store` 或 `session/sqlite`（layer 1 依赖
  layer 2 会成环，并把只想拿契约的宿主拖进整棵子树）；
- `storage` **不得**被根包 `rig` 引用（否则根包立刻违反 stdlib-only）。

所以"把 `*store.Dir` 包成本契约"的适配器**不在这个包里**，而是在
`session/store`（`DirStorage`）：依赖方向只能是
`session/store ──import──▶ storage ──import──▶ rig / memory`。

### 5.3 可选能力走独立接口

判据是一句话："这份数据丢了，agent 这个回合还能不能正确继续？"

- **不能** → 必需：会话/消息、检查点、溢出结果。
- **能，只是体验差** → 可选：事件、花费、记忆、检索。

可选能力**不进主接口**（否则第二个实现就永远不达标），而是做成独立小接口让实现方按需
满足、宿主按需类型断言。目前**尚未**进入契约的三处缺口（有明确原因，不要自行补）：

| 缺口 | 原因 |
| --- | --- |
| `SessionLog` | 各实现自有接口的方法集与 `rig.SessionLog` 不一致（`sqlite.SessionLog` 多一个 `AppendEvent`，返回类型也不同），需先统一 |
| 事件写入 | "事件写失败算不算 turn error"的语义未定；目前只有 `kit.Config.EventWriter`（NDJSON 导出通道） |
| 追加签名 | 两个实现都只有单条 `Append`，契约里一律用已有的 `AppendBatch(ctx, id, ...rig.Message)`，不要再定义一个变参 `Append` |

`storagetest` 里的 `SessionAdmin` 是这种"可选能力"的范式：`ListSessions` /
`DeleteSession` 两个方法，实现了就参与对应用例，没实现就 skip 并打印原因——不需要
import `storagetest`（结构化接口自动匹配）。

### 5.4 `storagetest` 基线套件（**未发布**）

同一份契约有两个实现时，"两边各写一遍测试"必然分叉；`storagetest` 把阈值、边界与
坑固定在套件里，实现只能选择"支持/不支持"（能力位），**不能**选择"少测一条"。

```go
func TestStorageBaseline(t *testing.T) {
    dirCaps := storagetest.Capability{History: true}
    storagetest.Run(t, "dir", func(t *testing.T) storage.Storage {
        d, err := store.NewDir(t.TempDir())
        if err != nil {
            t.Fatalf("NewDir: %v", err)
        }
        t.Cleanup(func() { _ = d.Close() })
        return store.NewDirStorage(d)
    }, dirCaps)
}
```

- `Factory` 每次用例都返回一份**全新的、空的**实现（用例之间不共享状态），返回 nil
  判失败。
- `Capability{History, Checkpoints, Memory, Spill}` 是能力位：**声明 true 就必须真的
  返回可用的实现**，否则视为撒谎直接判失败；声明 false 的用例会 skip 并打印
  `skip: backend does not support <能力>`。
- `Capabilities`（`map[string]Capability`）是"实现名 → 能力位"的登记表；
  `Capabilities.Run` 对未登记的实现名直接失败——"忘了登记"和"确实不支持"必须能区分。
- 只依赖标准库与本仓 `rig`/`storage`/`memory` 契约包，因此**仓外的宿主**也能 import。

## 6. kit 接线

### 6.1 三个互斥字段

| 字段 | 行为 |
| --- | --- |
| `SessionStoreDir string` | kit 打开一个 `session/store` 目录，并把 `History` 挂成其中的默认会话（会话 ID `"default"`）。要求 `History` 为 nil |
| `SessionStore *store.Dir` | 宿主自己持有实例（"最近会话"侧栏需要用**同一个实例** `List`/`Load`）。与 `SessionStoreDir`/`History` 互斥 |
| `History History` | 完全自定义（例如 `session.Memory` 或 `sqlite.Store.History()`） |

三者同时设置会得到明确的配置错误：
`kit: SessionStore, History and SessionStoreDir are mutually exclusive`。
三者都不设时用 `session.NewMemory()`。

**单实例警告**：`store.Dir` 不做跨进程/跨实例加锁，同一目录只能由**一个** store 实例
写——两个实例同时写同一会话会互相覆盖索引。重启时先释放前一个实例。用
`Agent.SessionStore()` 取 kit 建的那个实例（不要再 `NewDir` 同一个目录）。
`session/sqlite` 没有这个限制：同一个库可以被多个进程打开（写路径用 BUSY 重试兜底），跨进程的回合归属用**租约**表达（§4.8）。

### 6.2 `Storage` 与 `BackgroundJobs`

- `Config.Storage` 只需要 `Close()`（`kit.Storage`）。宿主传 `*session/sqlite.Store`
  即可——它的 `Close` 内部先做 `PRAGMA wal_checkpoint(TRUNCATE)`，所以关闭之后的
  `.db` 是完整可拷的。
- kit **只关自己打开的** `store.Dir`（即 `SessionStoreDir`）；宿主传入的
  `Config.SessionStore` 归宿主（同一目录单实例，其它组件可能还在用它）——要收尾就在
  `Config.Storage` 里登记一次。
- `Config.BackgroundJobs` 只在宿主**自建** `Config.Tools` 时才需要：默认工具集由 kit
  经 `full.NewRuntime` 构造，`jobs.Manager` 与 `terminal.Manager` 会被自动登记。

### 6.3 `Agent.Shutdown(ctx)` 的收尾顺序

顺序不能换位置：

1. **取消在飞回合**（一次 `cancel` 覆盖所有 Run 的 provider 流与工具调用）；
2. **等它们退出**（= 排空回合驱动的 `Append` 与事件写入；`ctx` 用来限制等待时间）；
3. **关后台任务**（`jobs.Manager` / `terminal.Manager` 的 `CloseAll`）——后台命令与
   PTY 会话不能活过宿主进程；
4. **排空事件写入**（flush 缓冲 writer）；
5. **关存储**（`sqlite.Store.Close` 负责 WAL checkpoint）。

两条边界：

- 第 2 步等不到回合退出（`ctx` 超时）时**不关存储**——那正是 `ErrClosed` 的来源；
  但仍关后台任务并返回错误，宿主可以再调一次 `Shutdown` 收尾。
- 收尾完成的再次调用是 no-op；`Shutdown` 之后的新 `Run` 直接返回
  `kit.ErrShuttingDown`（不是存储错误：它是宿主的主动动作，调用方可以据此静默丢弃
  用户新输入，而不是弹错误）。

```go
a, err := kit.New(kit.Config{
    Model:           "gpt-4o-mini",
    SessionStoreDir: "/var/lib/myapp/sessions", // 一会话一 NDJSON
    Storage:         db,                        // 例如 *sqlite.Store：Shutdown 时关它
    BackgroundJobs:  []kit.Background{bg},      // 仅在自建 Config.Tools 时需要
})
if err != nil {
    return err
}
defer a.Shutdown(ctx) // 宿主关窗口时的唯一正确收尾入口
```

## 7. 发布状态（如实）

**当前正式 release 是 v0.23.4。下面这些都在 CHANGELOG 的「Unreleased」里，v0.23.4
的发布面里没有它们：**

- `storage`、`storagetest`（聚合契约与基线套件）；
- `memory` 公共契约（`memory.Wiki` 等）；
- `rig.SpillStore` 与 `memory.Wiki` 在根包/工具层的接线；
- `session/sqlite` 子模块。

`session/sqlite` 还多一条发布工程问题：它的 `go.mod` 写着

```
require github.com/zeokeel/zeokeel-rig v0.24.0
replace github.com/zeokeel/zeokeel-rig => ../..
```

- `v0.24.0` **尚未打 tag**（根 tag 最高是 `v0.23.4`），而本模块 import 的根模块
  `memory` 包在 v0.23.4 里还不存在（实测 v0.23.4 下 `go build` 会报
  `module … does not contain package …/memory`）。所以下游 `go get` 这个子模块
  **必然解析失败**。
- `replace ../..` 只服务本仓开发（Go 只应用主 module 的 replace，子模块被 `go get`
  时这条声明是死的）。它掩盖不了"require 指向的版本不存在"。
- 子模块自己也**一个 tag 都没有**（`session/sqlite/vX.Y.Z` 还没有）。

因此正确说法是：**`session/sqlite` 需随下一个版本一起发布**——先打根 tag，再打
`session/sqlite/vX.Y.Z`，并把 `require` 改成实际要发的版本。在此之前：

- 不要把它写成"`go get` 即可用"；
- 不要用 `replace` 的现状反推"已经能被宿主消费"（CI 的 `sqlite-consumer smoke`
  盯着这条；`require`/tag 自洽由发版流程校验，不在 CI 里）。

本仓开发仍然可以直接用工作树构建（`replace ../..`，或仓库根的 `go.work`——后者不进
发布面）。

## 8. 相关文档

- [docs/MEMORY.md](MEMORY.md) —— 记忆 wiki 契约与 spill / `spill_read`
- [docs/CONFIG.md](CONFIG.md) —— `kit.Config` 的 `History`/`SessionStoreDir`/
  `SessionStore`/`Storage`/`BackgroundJobs`/`CheckpointStore`/`Spill` 字段
- [docs/ARCHITECTURE.md](ARCHITECTURE.md) —— 分层、请求流与扩展点
- [docs/RELEASE.md](RELEASE.md) —— 根 tag 与子模块 tag 的发布流程
- [CHANGELOG.md](../CHANGELOG.md) —— 「Unreleased」里的准确清单
- [`session/store`](../session/store)、[`session/sqlite`](../session/sqlite)、
  [`storage`](../storage) 的包文档（`go doc`）