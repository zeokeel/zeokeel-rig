package sqlite

// coreVersion is the schema version this build of the store knows how to open.
// It is stored in the database's user_version and bumped only when the core
// tables change; extensions keep their own ledger, so a host feature never has
// to know this number.
const coreVersion = 1

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
