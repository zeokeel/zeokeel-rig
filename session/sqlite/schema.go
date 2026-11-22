package sqlite

// coreVersion 是本构建能打开的 core schema 版本，存在数据库的 user_version 里。
//
// # 怎么加一条 schema 变更
//
//  1. 在 `migrations` 里**新开一段**（键 = 新版本号），把这次要做的 DDL 写进去；
//  2. 把这里的数字 +1，并保证 `migrations` 的键正好是 1..coreVersion（内部测试
//     `TestMigrationLedgerCoversEveryVersion` 会拦住漏一段或漏 bump）；
//  3. 新段里的每条语句都要写成幂等的：`CREATE ... IF NOT EXISTS`，或者
//     `ALTER TABLE ... ADD COLUMN`——后者由迁移执行器先探测列是否存在
//     （SQLite 的 ADD COLUMN 没有 IF NOT EXISTS，重跑会 `duplicate column name`）。
//
// ⚠️ **不要**把新列直接塞进旧段的 CREATE TABLE 里就当改完了。老库的表早就存在，
// `CREATE TABLE IF NOT EXISTS` 对它一条语句都不生效；那样升级会"成功"、版本号会
// 前进，但运行期一读新列就是 `no such column`。本次 v3 的 format_version 就是
// 第一个走 ALTER 的真实用例。
//
// 扩展（Extension）有自己的台账 `rig_schema_extensions`，所以宿主功能不需要知道
// 这个数字。
const coreVersion = 13

// searchIndexVersion 是建检索索引（虚拟表 + 触发器）的段号。
//
// 单独命名是因为它有一个别段没有的性质：没有 FTS5 的 SQLite 构建执行这段会报
// `no such module: fts5`，而迁移失败等于库打不开。执行器按这个段号把"索引语句"
// 过滤掉（见 migrate_v2.go 的 migrationStatements），检索降级为 substring，
// 库照常打开。
const searchIndexVersion = 13

// migrations 把 core schema 按版本分段：键是"执行完这一段之后的 user_version"，
// 值是这一段要执行的 DDL。
//
// 为什么要分段而不是一个扁平列表（旧实现就是扁平列表）：一个扁平列表只能表达
// "把这些 CREATE IF NOT EXISTS 再跑一遍"，而它对**已经存在的表**加列无效。
// 分段之后每段只跑一次、版本号逐段前进，于是"给老表加列"第一次有了表达方式。
//
// 段 1 是基线（v1 时代的全部表），它在**任何**升级里都会重跑一遍：它的每条语句
// 都是 IF NOT EXISTS，重跑能自愈"表还在、索引被手工删掉"的库，而 v1 时代的库
// 本来就没有索引。其余段只在 `version > 当前 user_version` 时执行。
var migrations = map[int][]string{
	// v1：会话、消息、事件、花费、检查点。这四个面构成"一个库装全部"的最小集。
	1: {
		`CREATE TABLE IF NOT EXISTS rig_sessions (
			id            TEXT PRIMARY KEY,
			title         TEXT    NOT NULL DEFAULT '',
			workspace_id  TEXT    NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			bytes         INTEGER NOT NULL DEFAULT 0,
			-- revision orders two writes that landed in the same millisecond, so a
			-- list is deterministic instead of depending on id hashes.
			revision      INTEGER NOT NULL DEFAULT 0
		)`,
		// Newest first is the order every session list uses; without this index the
		// list is a full scan and a sort.
		`CREATE INDEX IF NOT EXISTS rig_sessions_updated_at ON rig_sessions(updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS rig_messages (
			session_id TEXT    NOT NULL REFERENCES rig_sessions(id) ON DELETE CASCADE,
			ordinal    INTEGER NOT NULL,
			payload    BLOB    NOT NULL
		)`,
		// The primary access path: one session's history in order. UNIQUE also makes
		// a retried append idempotent instead of duplicating a message.
		`CREATE UNIQUE INDEX IF NOT EXISTS rig_messages_session_ordinal ON rig_messages(session_id, ordinal)`,
		`CREATE TABLE IF NOT EXISTS rig_events (
			session_id TEXT    NOT NULL,
			seq        INTEGER NOT NULL,
			kind       TEXT    NOT NULL,
			at         INTEGER NOT NULL,
			payload    BLOB    NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS rig_events_session_seq ON rig_events(session_id, seq)`,
		// The trajectory and the search both filter by kind over a time window.
		`CREATE INDEX IF NOT EXISTS rig_events_type_at ON rig_events(kind, at)`,
		// Every settled request is one row: the meter is per run, the database is
		// the record that outlives it.
		`CREATE TABLE IF NOT EXISTS rig_costs (
			session_id        TEXT    NOT NULL,
			model             TEXT    NOT NULL DEFAULT '',
			prompt_tokens     INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens     INTEGER NOT NULL DEFAULT 0,
			cost_usd          REAL    NOT NULL DEFAULT 0,
			at                INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS rig_costs_session_at ON rig_costs(session_id, at)`,
		`CREATE INDEX IF NOT EXISTS rig_costs_at ON rig_costs(at)`,
		`CREATE TABLE IF NOT EXISTS rig_checkpoints (
			session_id TEXT PRIMARY KEY REFERENCES rig_sessions(id) ON DELETE CASCADE,
			payload    BLOB    NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	},
	// v2：记忆与溢出结果。这两张曾经"加了表却忘了 bump"，于是老库永远建不出来，
	// Memory()/Spill() 稳定报 no such table 且没有任何提示——分段的直接动机之一。
	2: {
		// Long-term notes: one row per page, the index is a read of the metadata
		// columns, and a search touches the body only to filter.
		`CREATE TABLE IF NOT EXISTS rig_memory_pages (
			name       TEXT PRIMARY KEY,
			title      TEXT    NOT NULL DEFAULT '',
			summary    TEXT    NOT NULL DEFAULT '',
			content    TEXT    NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL,
			bytes      INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS rig_memory_updated_at ON rig_memory_pages(updated_at DESC)`,
		// Oversized tool results the model only sees a handle for.
		//
		// seq 用 AUTOINCREMENT 而不是裸 rowid：GC 会删掉旧行，而 SQLite 在没指定
		// AUTOINCREMENT 时会复用被删掉的最大 rowid——新写入的溢出结果就会拿到一个比
		// 现存行更小的 seq，于是"按 seq 保留最新 N 条"会把刚写的那条当成最旧的删掉。
		`CREATE TABLE IF NOT EXISTS rig_spill (
			-- seq 只用来排序（写入毫秒可能相同）；id 才是模型看到的句柄。
			seq        INTEGER PRIMARY KEY AUTOINCREMENT,
			id         TEXT    NOT NULL UNIQUE,
			tool       TEXT    NOT NULL DEFAULT '',
			bytes      INTEGER NOT NULL DEFAULT 0,
			content    TEXT    NOT NULL,
			created_at INTEGER NOT NULL
		)`,
	},
	// v3：rig_sessions.format_version。
	//
	// 这一列记录**该会话的消息载荷编码版本**（不是表的版本）：今天 payload 就是
	// rig.Message 的 JSON，所以所有行都是 1。以后若改了消息编码（例如把内容切成
	// 外置块、或加一个二进制尾部），读取路径可以按这一列决定怎么解，而
	// `SELECT ... FROM rig_messages` 也不必一律尝试"新格式"——**旧会话仍然读得
	// 出来**。没有这一列时，唯一的选择是"整库一起升"或者"读失败就是读失败"。
	//
	// 这是第一条真正的 ALTER：老库的表已经存在，CREATE IF NOT EXISTS 对它无效。
	// 执行器会先探测列是否存在，所以重复执行不会 `duplicate column name`。
	3: {
		`ALTER TABLE rig_sessions ADD COLUMN format_version INTEGER NOT NULL DEFAULT 1`,
		// 会话租约（见 lease.go）：跨进程"谁正在跑这个会话的回合"。
		//
		// session_id 直接做主键——一个会话同一时刻只能有一个活着的持有人，这条
		// 约束由主键保证，而不是靠调用方守规矩。expires_at 是**墙钟**毫秒，过期
		// 即可被抢占（时钟依赖写在 lease.go 的包注释里）。
		//
		// 与 v3 同批：coreVersion 的维护在另一端，本表作为段 3 的一部分落地——
		// 它是一条新的 `CREATE TABLE IF NOT EXISTS`（不是给老表加列），所以对
		// "v2 老库升级到 v3"与"全新库"两种路径都成立。若要为它单开一段，把这三
		// 条语句挪到新段并把 coreVersion +1 即可（两处必须同批）。
		`CREATE TABLE IF NOT EXISTS rig_session_leases (
			session_id TEXT PRIMARY KEY,
			owner      TEXT    NOT NULL,
			acquired_at INTEGER NOT NULL,
			expires_at  INTEGER NOT NULL
		)`,
		// 清扫与诊断要按"哪些租约已经过期"扫描，而不是逐行读出来再比较。
		`CREATE INDEX IF NOT EXISTS rig_session_leases_expires_at ON rig_session_leases(expires_at)`,
	},
	// v4：导入指纹（见 migrate.go 的 ImportDir）。
	//
	// 迁移续跑要回答"库里这份历史还是不是源那份"。上一版在"条数相等"时只比最后一条，
	// 中间分叉（库 `m1,X,m3` vs 源 `m1,m2,m3`）发现不了；而每次重跑都把整份历史读进
	// 内存又太贵。这里在**导入成功时**记下源内容的 sha256，之后只查这一行。
	//
	// 为什么单独一张表而不是塞进 rig_sessions：指纹是"导入过程"的状态，不是会话自身的
	// 事实；放进会话行会让每次列会话都多带一列，也让"宿主自己改了历史"与"源变了"更难
	// 区分。session_id 做主键（一个会话只有一个导入来源），并随会话删除级联清掉——
	// 否则"删掉会话 → 用同一个 ID 重新导入不同内容"会读到过期指纹。
	4: {
		`CREATE TABLE IF NOT EXISTS rig_import_state (
			session_id    TEXT PRIMARY KEY REFERENCES rig_sessions(id) ON DELETE CASCADE,
			source_digest TEXT    NOT NULL,
			message_count INTEGER NOT NULL
		)`,
	},
	// v5：溢出结果的作用域（见 spill.go 的 GC）。
	//
	// 一个库里放着**所有**会话的溢出结果，而 agent 每存一条就按 SpillPolicy.MaxFiles
	// 调一次 GC。旧实现是全局裁剪（`DELETE ... WHERE id NOT IN (SELECT ... LIMIT ?)`），
	// 于是会话 A 的回收会删掉会话 B 的行——B 的 `ref:` 句柄从此读不回来，模型的工具
	// 输出永久丢失。文件后端早就按文件名里的作用域前缀分区了（agent/spill.go 的
	// pruneSpillFiles），这一列让数据库后端能做同样的事。
	//
	// scope 是**会话 ID**（rig.SpillScope 从 ctx 上取）。老数据落在空串这一桶里：
	// 空作用域的 GC 只回收空作用域的行，不会被当成"全库通配符"——那正好会退回上面
	// 那个 bug。
	//
	// 索引 (scope, seq DESC) 与 GC 的查询形状一一对应：按作用域过滤、按 seq 取最新
	// keep 条。没有它每次回收都是全表扫 + 排序。
	//
	// 给已有表加列必须走 ALTER（老库的表已经存在，段 1 的 CREATE TABLE IF NOT
	// EXISTS 对它一条语句都不生效），执行器会先探测列是否存在，所以重复执行安全。
	5: {
		`ALTER TABLE rig_spill ADD COLUMN scope TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS rig_spill_scope_seq ON rig_spill(scope, seq DESC)`,
	},
	// v6：消息的可检索文本（见 search.go 的 SearchMessages / searchTextFor）。
	//
	// 旧实现直接对 `CAST(payload AS TEXT)` 做 LIKE，而 payload 是 rig.Message 的
	// JSON：正文里的 `"`、`<`、`>`、`&` 与换行被转义成 `\"` / `\u003c` / `\u003e` /
	// `\u0026` / `\n`，于是"我写过这段话，却一条都搜不到"；tool_call 参数里的引号
	// 同样被转义。写入时把消息解码进这一列，查询就直接 LIKE 解码后的原文。
	//
	// 索引是**普通**索引而不是 FTS5：本模块的门禁是"零 cgo + 只有 sqlite 一个驱动"，
	// FTS5 会把 tokenizer、内容表与重建流程一起引入，个人库的量级用不上。
	//
	// 老行留空：查询对 `search_text = ''` 的行退回原 payload 兜底（升级不丢会话），
	// 一次性补齐走 BackfillSearchText。
	//
	// 注：台账守卫（TestMigrationLedgerKeepsAddColumnCapability）规定"一段只加一列"，
	// 所以 v6/v7/v8 各加一列，不要把它们合并进同一段。
	6: {
		`ALTER TABLE rig_messages ADD COLUMN search_text TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS rig_messages_search_text ON rig_messages(search_text)`,
	},
	// v7 / v8：花费的归属维度（见 cost.go 的 CostFilter / CostByWorkspace）。
	//
	// rig_costs 原来只记 session_id：宿主有多个工作区、一次 run 下面还挂着子代理时，
	// "这个工作区花了多少""这个 run 花了多少"既算不出、也归不准。两列都是空串 =
	// 未设置：老行、以及没有这两个概念的宿主都落进同一个空桶，聚合不会报错。
	//
	// 分开两段的原因同上：一段只加一列。
	7: {
		`ALTER TABLE rig_costs ADD COLUMN workspace_id TEXT NOT NULL DEFAULT ''`,
	},
	8: {
		`ALTER TABLE rig_costs ADD COLUMN run_id TEXT NOT NULL DEFAULT ''`,
	},
	// v9：删掉 v6 建的 rig_messages_search_text 索引。
	//
	// 它的唯一使用者是 `search_text LIKE '%x%'`（前导通配），SQLite 用不上普通
	// 索引（EXPLAIN 是 SCAN），留着只给每条消息写入加一次索引维护。等哪天检索换成
	// 前缀匹配或 FTS5 再加回来——那时索引形状也会不一样。
	9: {
		`DROP INDEX IF EXISTS rig_messages_search_text`,
	},
	// v10：记忆的归属工作区（见 memory.go 的 memoryStore）。
	//
	// `rig_memory_pages` 原来只有一张全局命名空间，而文件后端（internal/memwiki）
	// 是按 `<状态目录>/memory/<workspaceID>` 分区的。同一个库被多个仓库共用时，
	// A 仓的记忆会出现在 B 仓；这一列让数据库后端能做同样的事。
	//
	// 老行落在空串这一桶：空工作区的视图（既有行为）仍然读得到它们。
	//
	// 主键仍然是 name（历史段落不动、也不重建表），所以**物理**主键里会拼上工作区
	// 前缀才能让两个工作区各有一页同名页——拼接与拆解见 memory.go 的 storageName /
	// logicalMemoryName。索引按 (workspace_id, updated_at DESC) 建，与 Index 查询
	// 的形状一致（按工作区过滤、按更新时间倒序）。
	10: {
		`ALTER TABLE rig_memory_pages ADD COLUMN workspace_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS rig_memory_workspace_updated_at ON rig_memory_pages(workspace_id, updated_at DESC)`,
	},
	// v11：标题的"来源"标记。
	//
	// 自动标题（取第一条用户消息）只应在"用户没碰过标题"时生效。用
	// `message_count = 0` 当代理是错的：助手开场、或首条用户消息只有图片时，
	// 第一次追加拿不到标题但消息数已经 +1，之后真正的用户文本永远补不上标题。
	// 这一列在 UpdateMeta 显式设置/清空标题时置 1，条件改成 `title='' AND
	// title_cleared=0`。
	11: {
		`ALTER TABLE rig_sessions ADD COLUMN title_cleared INTEGER NOT NULL DEFAULT 0`,
	},
	// v12：会话的消息序号高水位。
	//
	// ordinal 是**对外游标**：`MessageHit.Ordinal` 与 `SearchQuery.AfterOrdinal` 都把它
	// 交给宿主，删除又刻意不重排（见 admin.go 的文件头）。但"不复用被删掉的序号"不能靠
	// `MAX(ordinal)+1` 保证——它只看现存行，删掉尾部或清空之后，下一条 `Append` 正好落回
	// 空洞，宿主手里的旧序号就静默指向另一条消息（与 spill 的 AUTOINCREMENT 注释记的是
	// 同一类复用 bug）。
	//
	// 这一列是每会话的单调高水位：写路径取 `max(next_ordinal, MAX(ordinal)+1)` 起算，
	// 落库后把 `next_ordinal` 推到 `该起点 + 本次写入条数`；`Replace` 只让它前进。
	// 老库的行由 `ADD COLUMN` 的默认 0 落地，升级提交后按 `MAX(ordinal)+1` 一次性回填
	// （见 sessions.go 的 backfillNextOrdinal，由 open.go 的 migrate 在提交后调用）——
	// 回填是一条裸 UPDATE，不在迁移段里，是因为迁移段只允许可无条件重放的 DDL 形状
	// （TestMigrationStatementsAreRepeatable）。
	12: {
		`ALTER TABLE rig_sessions ADD COLUMN next_ordinal INTEGER NOT NULL DEFAULT 0`,
	},
	// v13：检索质量——CJK 辅助列 + FTS5（trigram）索引（见 search_mode.go 与
	// open.go 的 searchIndexReady）。
	//
	// v6 的注释说"个人库的量级用不上 FTS5"，那是当时的判断；问题在于用户真的会搜
	// "磁盘"这种 2 字词，而 LIKE 全表扫既没有相关度、也没法在大库上保持可用。这一
	// 段把两条路径并存：默认仍是 substring（零值，行为不变），宿主显式要 SearchFTS
	// 才走索引。
	//
	// search_cjk 是**写入时由 Go 算好**的 unigram + bigram 词表（`#磁盘#` 形式）：
	// trigram tokenizer 只认 3 字符窗口，2 字 CJK 词自身撑不满一个窗口，没有这一列
	// 就是"静默零命中"。它必须是真的列——触发器不会算 bigram，只能照抄。
	//
	// 索引用 external content（content='rig_messages'）：正文仍只有一份，虚拟表
	// 只存倒排索引，写入路径也不需要在事务里维护两份内容。
	//
	// 老库的索引由 `INSERT INTO rig_messages_fts(rig_messages_fts) VALUES('rebuild')`
	// 回填——它不满足"可无条件重放的 DDL 形状"（裸 INSERT），所以**不**放在这一段，
	// 而是在迁移提交之后由 Open 调 BackfillSearchIndex（见 open.go）。
	//
	// 没有 FTS5 的构建上，这一段只执行那条 ALTER（列本身无害），虚拟表与触发器被
	// 执行器跳过（见 searchIndexVersion）。
	13: {
		`ALTER TABLE rig_messages ADD COLUMN search_cjk TEXT NOT NULL DEFAULT ''`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS rig_messages_fts USING fts5(
			search_text,
			search_cjk,
			content='rig_messages',
			content_rowid='rowid',
			tokenize='trigram'
		)`,
		// 三个触发器让索引跟着消息表走：写入路径（Append/AppendBatch/Replace/导入）
		// 因此**不必**知道 fts 存在，宿主直接改表也不会让索引悄悄过期。
		`CREATE TRIGGER IF NOT EXISTS rig_messages_fts_ai AFTER INSERT ON rig_messages BEGIN
			INSERT INTO rig_messages_fts(rowid, search_text, search_cjk)
			VALUES (new.rowid, new.search_text, new.search_cjk);
		END`,
		`CREATE TRIGGER IF NOT EXISTS rig_messages_fts_ad AFTER DELETE ON rig_messages BEGIN
			INSERT INTO rig_messages_fts(rig_messages_fts, rowid, search_text, search_cjk)
			VALUES ('delete', old.rowid, old.search_text, old.search_cjk);
		END`,
		`CREATE TRIGGER IF NOT EXISTS rig_messages_fts_au AFTER UPDATE ON rig_messages BEGIN
			INSERT INTO rig_messages_fts(rig_messages_fts, rowid, search_text, search_cjk)
			VALUES ('delete', old.rowid, old.search_text, old.search_cjk);
			INSERT INTO rig_messages_fts(rowid, search_text, search_cjk)
			VALUES (new.rowid, new.search_text, new.search_cjk);
		END`,
	},
}

// extensionLedger records which host extensions were applied, so a later Open
// runs only the new ones and an old database keeps working with a new binary.
const extensionLedger = `CREATE TABLE IF NOT EXISTS rig_schema_extensions (
	name       TEXT PRIMARY KEY,
	applied_at INTEGER NOT NULL
)`
