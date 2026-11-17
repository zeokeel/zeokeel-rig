package store_test

// 个人 agent 客户端：单进程、单实例、一个会话一个文件。
//
// 本文件钉住"简化后的存储布局"这一组新行为：
//   - 会话文件是 <dir>/<id>.ndjson，**没有** index.json、没有 sessions/ 子目录；
//   - 索引是 NewDir 时扫目录建出来的（按 mtime/大小），List 只读内存；
//   - resume：List 按 UpdatedAt 倒序（最近的在最前），启动时以文件 mtime 为准；
//   - 非会话条目（子目录、点文件、遗留的 sessions/）一律忽略。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// sessionFile 返回新布局下的会话文件路径。
func sessionFile(dir, id string) string {
	return filepath.Join(dir, id+".ndjson")
}

// writeSessionFile 直接按 NDJSON 写一个会话文件（绕过 store，模拟"上次运行留下的"）。
func writeSessionFile(t *testing.T, dir, id string, messages ...rig.Message) string {
	t.Helper()
	path := sessionFile(dir, id)
	var builder strings.Builder
	for _, message := range messages {
		line, err := json.Marshal(message)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		builder.Write(line)
		builder.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	return path
}

// listFiles 返回目录里的全部条目名（排序），用于断言"没有多余文件"。
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// NewDir 必须从已有文件重建索引：不读任何索引文件，一个会话一个文件。
func TestScanBuildsIndexFromSessionFiles(t *testing.T) {
	dir := t.TempDir()
	writeSessionFile(t, dir, "alpha",
		rig.NewMessage(rig.RoleUser, "第一个会话的第一条"),
		rig.NewMessage(rig.RoleAssistant, "回答"),
	)
	writeSessionFile(t, dir, "beta", rig.NewMessage(rig.RoleUser, "第二个会话"))

	st, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = st.Close() }()

	metas, err := st.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("List 扫到 %d 个会话，want 2: %+v", len(metas), metas)
	}
	ids := map[string]store.Meta{}
	for _, meta := range metas {
		ids[meta.ID] = meta
	}
	for _, id := range []string{"alpha", "beta"} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("List 里缺少会话 %q: %+v", id, metas)
		}
	}
	// 大小来自文件长度（扫目录就能拿到，不需要读内容）。
	info, err := os.Stat(sessionFile(dir, "alpha"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if ids["alpha"].Bytes != info.Size() {
		t.Fatalf("alpha.Bytes = %d, want 文件大小 %d", ids["alpha"].Bytes, info.Size())
	}

	// 内容必须能完整读出来（扫目录建索引不能影响 Load）。
	meta, messages, err := st.Load(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 || messages[0].Text() != "第一个会话的第一条" {
		t.Fatalf("Load 内容 = %#v", messages)
	}
	if meta.Messages != 2 || meta.Title == "" {
		t.Fatalf("Load 之后 meta 应被补全: %+v", meta)
	}
}

// 布局契约：只有 <id>.ndjson，没有 index.json、没有 sessions/ 子目录。
func TestLayoutHasNoIndexFileAndNoSessionsSubdir(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	ctx := context.Background()
	created, err := st.Create(ctx, store.Meta{Title: "布局"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.Append(ctx, created.ID, rig.NewMessage(rig.RoleUser, "hi")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := st.List(ctx, store.Filter{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	names := listFiles(t, dir)
	for _, name := range names {
		if name == "index.json" {
			t.Fatalf("出现了 index.json（新布局不应有索引文件）: %v", names)
		}
		if name == "sessions" {
			t.Fatalf("出现了 sessions/ 子目录（新布局是扁平目录）: %v", names)
		}
		if !strings.HasSuffix(name, ".ndjson") {
			t.Fatalf("目录里出现了非会话文件: %q（全部条目 %v）", name, names)
		}
	}
	if len(names) != 1 || names[0] != created.ID+".ndjson" {
		t.Fatalf("目录条目 = %v, want 只有 %q", names, created.ID+".ndjson")
	}
}

// resume：List 按 UpdatedAt 倒序；启动时以文件 mtime 为准。
func TestResumeOrdersByUpdatedAtNewestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// 三个会话，mtime 依次递增（都落在过去，避免与 Append 的真实时钟抢先后）：
	// old < middle < new。
	for index, id := range []string{"old", "middle", "new"} {
		writeSessionFile(t, dir, id, rig.NewMessage(rig.RoleUser, "m"))
		stamp := now.Add(time.Duration(index-3) * time.Hour)
		if err := os.Chtimes(sessionFile(dir, id), stamp, stamp); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	st, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = st.Close() }()

	metas, err := st.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	gotOrder := make([]string, 0, len(metas))
	for _, meta := range metas {
		gotOrder = append(gotOrder, meta.ID)
	}
	if strings.Join(gotOrder, ",") != "new,middle,old" {
		t.Fatalf("List 顺序 = %v, want [new middle old]（最近的在最前）", gotOrder)
	}
	// UpdatedAt 就是文件 mtime（不依赖任何索引文件）。
	newest := metas[0]
	info, err := os.Stat(sessionFile(dir, "new"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !newest.UpdatedAt.Equal(info.ModTime().UTC()) {
		t.Fatalf("UpdatedAt = %v, want 文件 mtime %v", newest.UpdatedAt, info.ModTime().UTC())
	}

	// Append 到最旧的会话：它必须立刻排到最前面（内存索引更新，不需要重开）。
	if _, err := st.Append(context.Background(), "old", rig.NewMessage(rig.RoleAssistant, "续写")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	metas, err = st.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if metas[0].ID != "old" {
		t.Fatalf("Append 之后顺序 = %+v, want \"old\" 在最前", metas)
	}
}

// 扫目录时必须跳过非会话条目：子目录、点文件、遗留的 sessions/、别的后缀。
func TestScanIgnoresNonSessionEntries(t *testing.T) {
	dir := t.TempDir()
	writeSessionFile(t, dir, "real", rig.NewMessage(rig.RoleUser, "hi"))
	// 各种噪音。
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	writeSessionFile(t, filepath.Join(dir, "sessions"), "legacy", rig.NewMessage(rig.RoleUser, "旧布局"))
	for _, name := range []string{".hidden.ndjson", "notes.txt", "index.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}

	st, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = st.Close() }()
	metas, err := st.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].ID != "real" {
		t.Fatalf("List = %+v, want 只有 \"real\"（其余都是噪音）", metas)
	}
}

// Delete 之后不得复活：文件消失、List 不含它、再 Append 是 ErrNotFound，
// 重新打开目录也看不见它。
func TestDeleteDoesNotResurrectOnRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	ctx := context.Background()
	created, err := st.Create(ctx, store.Meta{ID: "gone", Title: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.Append(ctx, created.ID, rig.NewMessage(rig.RoleUser, "bye")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := st.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(sessionFile(dir, "gone")); !os.IsNotExist(err) {
		t.Fatalf("删除后文件仍存在: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 重启：扫目录重建索引时它不能回来。
	reopened, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	metas, err := reopened.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 0 {
		t.Fatalf("删除的会话在重启后复活了: %+v", metas)
	}
	if _, _, err := reopened.Load(ctx, "gone"); err == nil {
		t.Fatal("Load(已删除) 成功了")
	}
}

// Create 之后立刻 List 就能看到它（内存索引），且不需要任何落盘索引。
func TestCreateIsVisibleInListWithoutIndexFile(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	created, err := st.Create(ctx, store.Meta{Title: "刚建的", Workspace: "/w"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	metas, err := st.List(ctx, store.Filter{Workspace: "/w"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].ID != created.ID || metas[0].Title != "刚建的" {
		t.Fatalf("List = %+v", metas)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); !os.IsNotExist(err) {
		t.Fatalf("List/Create 之后出现了 index.json: %v", err)
	}
}
