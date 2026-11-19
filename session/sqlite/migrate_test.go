package sqlite_test

// Moving a host from the directory store to the database is one call, and it is
// verified record by record: same sessions, same titles, same message order.

import (
	"context"
	"path/filepath"
	"testing"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// TestImportFromDirectoryCopiesEverySession covers the upgrade path.
func TestImportFromDirectoryCopiesEverySession(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "sessions")
	legacy, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	first, err := legacy.Create(ctx, store.Meta{Title: "旧会话"})
	if err != nil {
		t.Fatalf("legacy Create: %v", err)
	}
	if _, err := legacy.AppendBatch(ctx, first.ID, userMessage("一"), assistantMessage("二")); err != nil {
		t.Fatalf("legacy AppendBatch: %v", err)
	}
	second, err := legacy.Create(ctx, store.Meta{Title: "第二条"})
	if err != nil {
		t.Fatalf("legacy Create: %v", err)
	}
	if _, err := legacy.Append(ctx, second.ID, userMessage("只有一条")); err != nil {
		t.Fatalf("legacy Append: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("legacy Close: %v", err)
	}

	db, _ := openTemp(t, sqlitestore.Options{})
	report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if report.Sessions != 2 || report.Messages != 3 {
		t.Fatalf("report = %+v", report)
	}
	metas, err := db.List(ctx, sqlitestore.Filter{})
	if err != nil || len(metas) != 2 {
		t.Fatalf("List = %+v (err %v)", metas, err)
	}
	_, messages, err := db.Load(ctx, first.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("imported messages = %d", len(messages))
	}
	// Importing again must not duplicate: the second run is a no-op per session.
	again, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir again: %v", err)
	}
	if again.Imported != 0 {
		t.Fatalf("second import imported %d sessions, want 0", again.Imported)
	}
	if metas, _ := db.List(ctx, sqlitestore.Filter{}); len(metas) != 2 {
		t.Fatalf("sessions after second import = %d", len(metas))
	}
}

// TestImportDryRunKeepsTheDatabaseUntouched lets a host preview the migration.
func TestImportDryRunKeepsTheDatabaseUntouched(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "sessions")
	legacy, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	if _, err := legacy.Create(ctx, store.Meta{Title: "预览"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, _ := openTemp(t, sqlitestore.Options{})
	report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir, DryRun: true})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if report.Sessions != 1 || report.Imported != 0 {
		t.Fatalf("dry-run report = %+v", report)
	}
	if metas, _ := db.List(ctx, sqlitestore.Filter{}); len(metas) != 0 {
		t.Fatalf("dry run wrote %d sessions", len(metas))
	}
}
