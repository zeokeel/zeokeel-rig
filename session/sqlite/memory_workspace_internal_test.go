package sqlite

// 内部用例：工作区分区落到**哪一列**、以及"老库升级出这一列"这两件事只能从
// 表结构层面验证（外部用例只能看到视图行为）。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/memory"
)

// TestMemoryWriteStoresWorkspaceColumn 钉住"写入落到 workspace 列"，并且逻辑页名
// 与物理主键分开：同名页在两个工作区能共存，但对外仍然是 "build"。
func TestMemoryWriteStoresWorkspaceColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws.sqlite")
	store, err := Open(Options{Path: path, Workspace: "repo-a"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.Memory().Write(ctx, memory.Page{Name: "build", Summary: "A 的摘要"}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var workspace, physical string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT workspace_id, name FROM rig_memory_pages WHERE workspace_id = ?`, "repo-a").
		Scan(&workspace, &physical); err != nil {
		t.Fatalf("读回 workspace_id: %v（迁移段没登记这一列时这里是 no such column）", err)
	}
	if workspace != "repo-a" {
		t.Fatalf("workspace_id = %q，want repo-a", workspace)
	}
	if physical == "build" {
		t.Fatalf("物理主键仍是裸页名：两个工作区的同名页会撞主键")
	}
	if !strings.HasPrefix(physical, "repo-a") {
		t.Fatalf("物理主键 = %q，没有带上工作区作用域", physical)
	}
	page, err := store.Memory().Read(ctx, "build")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if page.Name != "build" {
		t.Fatalf("Read 回来的页名 = %q，want 逻辑名 build", page.Name)
	}

	// 空工作区（老行为）写下的行必须落在空串这一桶，且能被工作区视图之外的读路径读到。
	legacy, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open(空工作区): %v", err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	if err := legacy.Memory().Write(ctx, memory.Page{Name: "old", Summary: "老行为"}); err != nil {
		t.Fatalf("Write(空工作区): %v", err)
	}
	var bucket string
	if err := legacy.DB().QueryRowContext(ctx,
		`SELECT workspace_id FROM rig_memory_pages WHERE name = 'old'`).Scan(&bucket); err != nil {
		t.Fatalf("读回老行: %v", err)
	}
	if bucket != "" {
		t.Fatalf("空工作区写下的行 workspace_id = %q，want 空串", bucket)
	}
}

// TestMemoryUpgradeAddsWorkspaceColumn：v9 老库（rig_memory_pages 没有 workspace_id）
// 打开之后必须补出这一列，并且老行仍然可读——这是"新迁移段 + expectedAlters 登记"
// 的实证。
func TestMemoryUpgradeAddsWorkspaceColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.sqlite")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// v9 的 rig_memory_pages：没有 workspace_id，里面已经有一行老数据。
	for _, statement := range []string{
		`CREATE TABLE rig_memory_pages (
			name TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, bytes INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO rig_memory_pages(name, title, summary, content, updated_at, bytes)
		 VALUES('legacy', '老页', '升级前就有', '老正文', 1700000000000, 9)`,
		`PRAGMA user_version = 9`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			_ = raw.Close()
			t.Fatalf("建 v9 库失败（%s）: %v", statement, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open(升级): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	schema, err := store.Schema(ctx)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	var hasColumn bool
	if columns, err := store.columns(ctx, "rig_memory_pages"); err == nil {
		for _, name := range columns {
			if name == "workspace_id" {
				hasColumn = true
			}
		}
	}
	if !hasColumn {
		t.Fatalf("升级后 rig_memory_pages 仍没有 workspace_id（版本 %d）", schema.Version)
	}
	page, err := store.Memory().Read(ctx, "legacy")
	if err != nil {
		t.Fatalf("升级后 Read(老行): %v", err)
	}
	if page.Content != "老正文" || page.Summary != "升级前就有" {
		t.Fatalf("老行 = %+v", page)
	}
}
