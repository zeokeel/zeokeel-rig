package sqlite

// Package sqlite stores everything an agent session writes — its messages, its
// event log, its interruption checkpoints and whatever else a feature needs to
// keep — in one SQLite database, instead of a directory of files.
//
// # The host owns the file
//
// The caller passes the database path; this package never invents one and never
// writes anywhere else. An SDK that picked its own storage location would put
// the host's data somewhere the host does not back up, migrate or clean up.
//
// # Why a database
//
// A directory of one file per session answers "load this session" and nothing
// else. A product also asks: which sessions are most recent, how many messages
// does each have, find the text I wrote last week, give me this turn's events by
// kind, and undo the last write. Those are queries, and a database answers them
// with an index instead of a directory scan.
//
// # What it is not
//
// The store holds no policy: it does not decide when to compact, which sessions
// to delete, or what a message means. It is a durable, concurrent, crash-safe
// place to put the values the rest of Rig already defines.
//
// # Extending the schema
//
// A host feature that needs its own table registers an Extension. Extensions are
// applied after the core schema, recorded in their own ledger, and applied only
// once; the core schema never changes because a host added something.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: the module keeps its cgo-free gate
)

// ErrNoPath is returned by Open when the host did not say where the database
// lives.
var ErrNoPath = errors.New("sqlite: Options.Path is required; the host chooses the database location")

// Options configures Open. The zero value is not openable: Path is required.
type Options struct {
	// Path is the database file the host chose. Its directory must exist; the
	// store creates the file, never the directories above it.
	Path string

	// Extensions are host schemas applied after the core tables, in the order
	// given. Each name is unique within one call and is recorded once applied.
	Extensions []Extension

	// BusyTimeout bounds how long a write waits for another connection's lock.
	// Zero uses DefaultBusyTimeout.
	BusyTimeout time.Duration
}

// DefaultBusyTimeout is how long a writer waits for the database lock when
// Options.BusyTimeout is unset. Two processes on one file is the normal case for
// a desktop product (a window and a background run), not an error.
const DefaultBusyTimeout = 5 * time.Second

// Extension is one host feature's schema: named statements applied once, in
// order, after the core schema.
type Extension struct {
	// Name identifies the extension in the ledger, conventionally
	// "<host>/<feature>". It must not be blank and must be unique per Open.
	Name string

	// Statements are executed once, in order, in one transaction. Write them
	// idempotently (CREATE TABLE IF NOT EXISTS …): a host that deletes the
	// ledger by hand must be able to reapply them.
	Statements []string
}

// Store is one open database.
type Store struct {
	db  *sql.DB
	dsn string
}

// Schema is what the database currently holds: the core version, the tables and
// indexes, and the columns of the sessions table. Hosts use it to assert that
// their extensions were applied; tests use it to pin the contract.
type Schema struct {
	Version         int
	Tables          []string
	Indexes         []string
	Extensions      []string
	SessionsColumns []string
}

// Open opens (creating it when needed) the database at opts.Path and brings the
// schema up to date.
func Open(opts Options) (*Store, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, ErrNoPath
	}
	if err := validateExtensions(opts.Extensions); err != nil {
		return nil, err
	}
	timeout := opts.BusyTimeout
	if timeout <= 0 {
		timeout = DefaultBusyTimeout
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(" + fmt.Sprint(timeout.Milliseconds()) + ")" +
		"&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	// One writer at a time keeps the WAL from growing on concurrent runs; the
	// readers still run in parallel. Desktop traffic is small and bursty.
	db.SetMaxOpenConns(1)
	store := &Store{db: db, dsn: dsn}
	if err := store.migrate(context.Background(), opts.Extensions); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// validateExtensions rejects the shapes that would silently misfire: a blank or
// duplicated name, or an extension that would run nothing.
func validateExtensions(exts []Extension) error {
	seen := make(map[string]bool, len(exts))
	for _, ext := range exts {
		name := strings.TrimSpace(ext.Name)
		if name == "" {
			return errors.New("sqlite: extension name must not be blank")
		}
		if seen[name] {
			return fmt.Errorf("sqlite: duplicate extension %q", name)
		}
		seen[name] = true
		if len(ext.Statements) == 0 {
			return fmt.Errorf("sqlite: extension %q has no statements", name)
		}
	}
	return nil
}

// migrate applies the core schema and then every extension that the ledger does
// not already record.
func (s *Store) migrate(ctx context.Context, exts []Extension) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("sqlite: read schema version: %w", err)
	}
	if version > coreVersion {
		return fmt.Errorf("sqlite: database schema v%d is newer than this build's v%d", version, coreVersion)
	}
	if version < coreVersion {
		for _, stmt := range coreSchema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("sqlite: apply core schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, coreVersion)); err != nil {
			return fmt.Errorf("sqlite: record schema version: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, extensionLedger); err != nil {
		return fmt.Errorf("sqlite: create extension ledger: %w", err)
	}
	if len(exts) > 0 {
		applied := make(map[string]bool, len(exts))
		rows, err := tx.QueryContext(ctx, `SELECT name FROM rig_schema_extensions`)
		if err != nil {
			return fmt.Errorf("sqlite: read extension ledger: %w", err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return fmt.Errorf("sqlite: read extension ledger: %w", err)
			}
			applied[name] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("sqlite: read extension ledger: %w", err)
		}
		_ = rows.Close()
		for _, ext := range exts {
			name := strings.TrimSpace(ext.Name)
			if applied[name] {
				continue
			}
			for _, stmt := range ext.Statements {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("sqlite: apply extension %q: %w", name, err)
				}
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO rig_schema_extensions(name, applied_at) VALUES(?, ?)`,
				name, time.Now().UnixMilli()); err != nil {
				return fmt.Errorf("sqlite: record extension %q: %w", name, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit schema: %w", err)
	}
	return nil
}

// DSN returns the connection string this store opened, for a host that wants a
// second connection with the same pragmas.
func (s *Store) DSN() string { return s.dsn }

// DB returns the underlying handle for a host feature that needs a query the
// store does not offer. The handle is bounded to one writer at a time, so it is
// safe to use from several goroutines.
func (s *Store) DB() *sql.DB { return s.db }

// Schema reports what the database holds.
func (s *Store) Schema(ctx context.Context) (Schema, error) {
	var schema Schema
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schema.Version); err != nil {
		return Schema{}, fmt.Errorf("sqlite: read schema version: %w", err)
	}
	tables, indexes, err := s.objects(ctx)
	if err != nil {
		return Schema{}, err
	}
	schema.Tables, schema.Indexes = tables, indexes
	if columns, err := s.columns(ctx, "rig_sessions"); err == nil {
		schema.SessionsColumns = columns
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM rig_schema_extensions ORDER BY name`)
	if err == nil {
		for rows.Next() {
			var name string
			if scanErr := rows.Scan(&name); scanErr == nil {
				schema.Extensions = append(schema.Extensions, name)
			}
		}
		_ = rows.Close()
	}
	return schema, nil
}

// objects lists table and index names from the catalog.
func (s *Store) objects(ctx context.Context) (tables, indexes []string, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT type, name FROM sqlite_master WHERE type IN ('table','index') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: list schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return nil, nil, fmt.Errorf("sqlite: list schema: %w", err)
		}
		switch kind {
		case "table":
			tables = append(tables, name)
		case "index":
			indexes = append(indexes, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("sqlite: list schema: %w", err)
	}
	sort.Strings(tables)
	sort.Strings(indexes)
	return tables, indexes, nil
}

// columns lists one table's column names.
func (s *Store) columns(ctx context.Context, table string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("sqlite: columns of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite: columns of %s: %w", table, err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// Close releases the database handle. Calling it twice is safe.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	if err != nil {
		return fmt.Errorf("sqlite: close: %w", err)
	}
	return nil
}
