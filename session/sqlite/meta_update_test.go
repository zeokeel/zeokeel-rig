package sqlite_test

// 会话元数据的更新入口（改名、换工作区）。
//
// `rig_sessions` 一直有 `title` / `workspace_id` 两列，却没有更新它们的方法：宿主
// 只能在 UI 里改个名字，却没地方落库。这里钉住最小补丁语义——nil 字段不改、空串
// 是**显式清空**、未知会话复用 Load/meta 的哨兵错误、updated_at 与 revision 照
// Append/Replace 的规矩维护（列表按 updated_at 倒序，revision 用来在同毫秒写入之间
// 保序）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// textPtr 返回一个指向 v 的字符串指针（MetaPatch 的字段是 nil = 不改）。
func textPtr(v string) *string { return &v }

// TestUpdateMetaRenamesSession：改名之后 Load 与 List 都要看到新标题。
func TestUpdateMetaRenamesSession(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{Title: "自动生成的标题", Workspace: "ws-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	renamed, err := db.UpdateMeta(ctx, created.ID, sqlitestore.MetaPatch{Title: textPtr("用户起的名字")})
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if renamed.ID != created.ID || renamed.Title != "用户起的名字" {
		t.Fatalf("UpdateMeta 返回 %+v，want id=%s title=用户起的名字", renamed, created.ID)
	}
	// 只改标题：其余字段不动。
	if renamed.Workspace != "ws-1" {
		t.Fatalf("只改标题却动了工作区：%q", renamed.Workspace)
	}

	loaded, _, err := db.Load(ctx, created.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Title != "用户起的名字" {
		t.Fatalf("Load 后标题 = %q，want 用户起的名字", loaded.Title)
	}

	metas, err := db.List(ctx, sqlitestore.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, meta := range metas {
		if meta.ID == created.ID {
			found = true
			if meta.Title != "用户起的名字" {
				t.Fatalf("List 里的标题 = %q，want 用户起的名字", meta.Title)
			}
		}
	}
	if !found {
		t.Fatalf("List 里没有会话 %s", created.ID)
	}
}

// TestUpdateMetaClearsTitleWithEmptyString：`Title: ptr("")` 是**清空**，不是"不改"。
func TestUpdateMetaClearsTitleWithEmptyString(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{Title: "要清掉的标题"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleared, err := db.UpdateMeta(ctx, created.ID, sqlitestore.MetaPatch{Title: textPtr("")})
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if cleared.Title != "" {
		t.Fatalf("标题 = %q，want 空串", cleared.Title)
	}
	loaded, _, err := db.Load(ctx, created.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Title != "" {
		t.Fatalf("落库后的标题 = %q，want 空串", loaded.Title)
	}
}

// TestUpdateMetaWorkspaceOnlyKeepsTitle：只给 WorkspaceID 时标题必须原样保留。
func TestUpdateMetaWorkspaceOnlyKeepsTitle(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{Title: "保留的标题", Workspace: "ws-old"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	updated, err := db.UpdateMeta(ctx, created.ID, sqlitestore.MetaPatch{WorkspaceID: textPtr("ws-new")})
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if updated.Workspace != "ws-new" {
		t.Fatalf("工作区 = %q，want ws-new", updated.Workspace)
	}
	if updated.Title != "保留的标题" {
		t.Fatalf("只改工作区却动了标题：%q", updated.Title)
	}
	// 按工作区过滤能命中新值、不再命中旧值。
	metas, err := db.List(ctx, sqlitestore.Filter{Workspace: "ws-new"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].ID != created.ID {
		t.Fatalf("按 ws-new 过滤结果 = %+v，want 只有 %s", metas, created.ID)
	}
}

// TestUpdateMetaUnknownSessionReportsNotFound：未知会话复用 Load/meta 的哨兵错误。
func TestUpdateMetaUnknownSessionReportsNotFound(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if _, err := db.UpdateMeta(ctx, "no-such-session", sqlitestore.MetaPatch{Title: textPtr("x")}); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("UpdateMeta(未知会话) = %v，want ErrNotFound", err)
	}
	// 空补丁也必须先确认会话存在，否则宿主会以为改成功了。
	if _, err := db.UpdateMeta(ctx, "no-such-session", sqlitestore.MetaPatch{}); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("UpdateMeta(未知会话, 空补丁) = %v，want ErrNotFound", err)
	}
}

// TestUpdateMetaIsIdempotent：同一份补丁重复应用两次不报错、结果一致。
func TestUpdateMetaIsIdempotent(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{Title: "旧"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	patch := sqlitestore.MetaPatch{Title: textPtr("新"), WorkspaceID: textPtr("ws-2")}
	first, err := db.UpdateMeta(ctx, created.ID, patch)
	if err != nil {
		t.Fatalf("第一次 UpdateMeta: %v", err)
	}
	second, err := db.UpdateMeta(ctx, created.ID, patch)
	if err != nil {
		t.Fatalf("第二次 UpdateMeta: %v", err)
	}
	if first.Title != second.Title || first.Workspace != second.Workspace {
		t.Fatalf("重复应用补丁结果不一致：%+v vs %+v", first, second)
	}
	metas, err := db.List(ctx, sqlitestore.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].Title != "新" {
		t.Fatalf("重复改名后 List = %+v，want 一条标题为 新 的会话", metas)
	}
}

// TestUpdateMetaMaintainsUpdatedAtAndRevision：改名是一次写，与 Append/Replace 一样
// 要推进 updated_at 与 revision（列表按 updated_at 倒序，revision 用来保序）。
func TestUpdateMetaMaintainsUpdatedAtAndRevision(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	createdAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)
	created, err := db.Create(ctx, sqlitestore.Meta{Title: "旧", CreatedAt: createdAt, UpdatedAt: createdAt})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := rawSessionRevision(t, path, created.ID)

	updated, err := db.UpdateMeta(ctx, created.ID, sqlitestore.MetaPatch{Title: textPtr("新")})
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("updated_at 没有推进：%v（原 %v）", updated.UpdatedAt, created.UpdatedAt)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("created_at 被改名改掉了：%v（原 %v）", updated.CreatedAt, created.CreatedAt)
	}
	after := rawSessionRevision(t, path, created.ID)
	if after != before+1 {
		t.Fatalf("revision = %d，want %d（改名要像 Append 一样 +1）", after, before+1)
	}
}

// TestUpdateMetaEmptyPatchChangesNothing：空补丁只回读，不动时间戳。
func TestUpdateMetaEmptyPatchChangesNothing(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	createdAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	created, err := db.Create(ctx, sqlitestore.Meta{Title: "不动", Workspace: "ws-1", CreatedAt: createdAt, UpdatedAt: createdAt})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := rawSessionRevision(t, path, created.ID)
	unchanged, err := db.UpdateMeta(ctx, created.ID, sqlitestore.MetaPatch{})
	if err != nil {
		t.Fatalf("UpdateMeta(空补丁): %v", err)
	}
	if unchanged.Title != "不动" || unchanged.Workspace != "ws-1" {
		t.Fatalf("空补丁改动了字段：%+v", unchanged)
	}
	if !unchanged.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("空补丁推进了 updated_at：%v（原 %v）", unchanged.UpdatedAt, created.UpdatedAt)
	}
	if got := rawSessionRevision(t, path, created.ID); got != before {
		t.Fatalf("空补丁推进了 revision：%d，want %d", got, before)
	}
}

// rawSessionRevision 读一行的 revision（Meta 不暴露它，顺序护栏只能看列）。
func rawSessionRevision(t *testing.T, path, id string) int64 {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var revision int64
	if err := raw.QueryRow(
		fmt.Sprintf(`SELECT revision FROM rig_sessions WHERE id = ?`), id).Scan(&revision); err != nil {
		t.Fatalf("read revision of %s: %v", id, err)
	}
	return revision
}
