// Package sqlite_test drives the store's opening rules in the order a host meets
// them: a database path the host chooses, the schema it creates, the indexes the
// queries rely on, and the tables a host adds for its own features.
package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// openTemp opens a store in the test's own directory. Every test names its own
// path: the store never invents one.
func openTemp(t *testing.T, opts sqlitestore.Options) (*sqlitestore.Store, string) {
	t.Helper()
	opts.Path = filepath.Join(t.TempDir(), "rig.sqlite")
	store, err := sqlitestore.Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, opts.Path
}

// schemaOf reads the introspection view the store offers hosts.
func schemaOf(t *testing.T, store *sqlitestore.Store) sqlitestore.Schema {
	t.Helper()
	schema, err := store.Schema(context.Background())
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	return schema
}

func has(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// TestOpenRequiresAPath pins the one rule the API must never relax: the host says
// where the database lives. A default path would put an SDK's storage somewhere
// the host does not know about, where nothing backs it up and nothing cleans it.
func TestOpenRequiresAPath(t *testing.T) {
	if _, err := sqlitestore.Open(sqlitestore.Options{}); err == nil {
		t.Fatal("Open with no path: want an error, got none")
	}
	if _, err := sqlitestore.Open(sqlitestore.Options{Path: "   "}); err == nil {
		t.Fatal("Open with a blank path: want an error, got none")
	}
}

// TestOpenCreatesTheCoreSchema checks the tables and the indexes after one Open:
// the indexes are the reason a database beats a directory of files, so they are
// part of the contract rather than an implementation detail.
func TestOpenCreatesTheCoreSchema(t *testing.T) {
	store, _ := openTemp(t, sqlitestore.Options{})
	schema := schemaOf(t, store)
	for _, want := range []string{"rig_sessions", "rig_messages", "rig_events", "rig_checkpoints"} {
		if !has(schema.Tables, want) {
			t.Fatalf("table %q missing; tables = %v", want, schema.Tables)
		}
	}
	for _, want := range []string{
		"rig_sessions_updated_at",
		"rig_messages_session_ordinal",
		"rig_events_session_seq",
		"rig_events_type_at",
	} {
		if !has(schema.Indexes, want) {
			t.Fatalf("index %q missing; indexes = %v", want, schema.Indexes)
		}
	}
	if !has(schema.SessionsColumns, "title") {
		t.Fatalf("rig_sessions.title missing; columns = %v", schema.SessionsColumns)
	}
	if schema.Version <= 0 {
		t.Fatalf("schema version = %d, want > 0", schema.Version)
	}
}

// TestOpenIsIdempotent covers the ordinary restart: opening the same file again
// must not fail, must not lose rows, and must not change the schema version.
func TestOpenIsIdempotent(t *testing.T) {
	first, path := openTemp(t, sqlitestore.Options{})
	before := schemaOf(t, first).Version
	again, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = again.Close() }()
	if got := schemaOf(t, again).Version; got != before {
		t.Fatalf("schema version after reopen = %d, want %d", got, before)
	}
}

// TestExtensionsAddTheirOwnTables covers the extensibility the host asked for:
// a feature registers its schema next to the core tables, and the core leaves it
// alone on every later Open.
func TestExtensionsAddTheirOwnTables(t *testing.T) {
	ext := sqlitestore.Extension{
		Name: "example/notes",
		Statements: []string{
			`CREATE TABLE IF NOT EXISTS example_notes (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				body TEXT NOT NULL,
				created_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS example_notes_created_at ON example_notes(created_at)`,
		},
	}
	store, path := openTemp(t, sqlitestore.Options{Extensions: []sqlitestore.Extension{ext}})
	if !has(schemaOf(t, store).Tables, "example_notes") {
		t.Fatal("extension table example_notes missing")
	}
	again, err := sqlitestore.Open(sqlitestore.Options{
		Path: path,
		Extensions: []sqlitestore.Extension{ext, {
			Name:       "example/tags",
			Statements: []string{`CREATE TABLE IF NOT EXISTS example_tags (id INTEGER PRIMARY KEY)`},
		}},
	})
	if err != nil {
		t.Fatalf("reopen with extensions: %v", err)
	}
	defer func() { _ = again.Close() }()
	tables := schemaOf(t, again).Tables
	if !has(tables, "example_notes") || !has(tables, "example_tags") {
		t.Fatalf("extension tables after reopen = %v", tables)
	}
}

// TestExtensionsAreNamespacedAndUnique keeps two features from silently
// overwriting each other's schema.
func TestExtensionsAreNamespacedAndUnique(t *testing.T) {
	ext := sqlitestore.Extension{
		Name:       "example/notes",
		Statements: []string{`CREATE TABLE IF NOT EXISTS example_notes (id INTEGER PRIMARY KEY)`},
	}
	if _, err := sqlitestore.Open(sqlitestore.Options{
		Path:       filepath.Join(t.TempDir(), "dup.sqlite"),
		Extensions: []sqlitestore.Extension{ext, ext},
	}); err == nil {
		t.Fatal("duplicate extension names: want an error, got none")
	}
	if _, err := sqlitestore.Open(sqlitestore.Options{
		Path:       filepath.Join(t.TempDir(), "blank.sqlite"),
		Extensions: []sqlitestore.Extension{{}},
	}); err == nil {
		t.Fatal("extension with no name: want an error, got none")
	}
}
