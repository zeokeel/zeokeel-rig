package sqlite

// coreVersion 是本构建能打开的 core schema 版本，存在数据库的 user_version 里。
//
// ⚠️ 护栏：**任何 coreSchema 的变更都必须同时把这里的数字 +1**。
//
// 原因：`migrate` 只在 `user_version < coreVersion` 时才执行 coreSchema，而每条
// 语句都是 `CREATE TABLE IF NOT EXISTS`。如果新增了表却忘记 bump，已经存在的库
// 看到的 `user_version` 就等于 coreVersion，于是**永远不会**建新表——打开后
// `Memory()`/`Spill()` 会一直报 `no such table`，而且没有任何错误提示。
// （这不是假设：`rig_memory_pages` / `rig_spill` 就曾这样漏 bump 过一次。）
//
// 扩展（Extension）有自己的台账 `rig_schema_extensions`，所以宿主功能不需要知道
// 这个数字。
const coreVersion = 2

// coreSchema is applied in one transaction on first Open (and on Open after a
// downgrade-free upgrade). Each statement is idempotent, so an interrupted
// first run is repaired by the next one.
//
// Sizes and time: timestamps are unix milliseconds, payloads are JSON-encoded
// rig values, and every table that a query filters on has an index beside it.
var coreSchema = []string{
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
	`CREATE TABLE IF NOT EXISTS rig_checkpoints (
		session_id TEXT PRIMARY KEY REFERENCES rig_sessions(id) ON DELETE CASCADE,
		payload    BLOB    NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
}

// extensionLedger records which host extensions were applied, so a later Open
// runs only the new ones and an old database keeps working with a new binary.
const extensionLedger = `CREATE TABLE IF NOT EXISTS rig_schema_extensions (
	name       TEXT PRIMARY KEY,
	applied_at INTEGER NOT NULL
)`
