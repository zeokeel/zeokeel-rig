package store_test

// 对抗审计（外部视角）：语义契约、崩溃点、ID 边界、批量原子性、并发压测、History 契约。
//
// v0.20 起存储布局简化为"一个会话一个文件"（<dir>/<id>.ndjson，无 index.json、
// 无 sessions/ 子目录、无跨实例重载）。因此本文件删掉了所有针对索引文件的断言，
// 保留的每个用例都只断言**对外语义**——它们与具体布局无关。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// auditStore 打开一个独立的临时库，并返回它的根目录。
func auditStore(t *testing.T) (*store.Dir, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, st.Dir()
}

func userMsg(text string) rig.Message { return rig.NewMessage(rig.RoleUser, text) }

// sessionPathIn 返回新布局下的会话文件路径。
func sessionPathIn(root, id string) string { return filepath.Join(root, id+".ndjson") }

// someTime 是一个固定时刻。
func someTime() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

// F-SS1（major）：Replace 之后条数必须是"替换"而不是"累加"。
//
// 作者的自测断言了 meta.Messages，但它在断言之前先调了 Load——而 Load 会把索引
// 校正回真实值，于是永远看不到错误计数。这里直接看 List（只读内存索引）。
func TestAuditReplaceUpdatesMessageCount(t *testing.T) {
	st, _ := auditStore(t)
	ctx := context.Background()

	if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.AppendBatch(ctx, "s1",
		userMsg("m1"), userMsg("m2"), userMsg("m3"), userMsg("m4"), userMsg("m5")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	// 上下文压缩：窗口从 5 条缩到 2 条。
	if _, err := st.Replace(ctx, "s1", []rig.Message{
		rig.NewMessage(rig.RoleSystem, "summary"), userMsg("m5"),
	}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	// List 只读内存索引：它必须已经是 2（Replace 不能累加）。
	metas, err := st.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].Messages != 2 {
		t.Fatalf("List 报告 Messages = %+v, want 2", metas)
	}
	// Load 与 List 必须一致。
	meta, messages, err := st.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta.Messages != len(messages) || len(messages) != 2 {
		t.Fatalf("Load 之后 meta.Messages=%d len=%d, want 2/2", meta.Messages, len(messages))
	}
}

// F-SS1b（major，清空场景）：Replace(nil) 之后条数必须归零。
func TestAuditReplaceWithEmptyClearsCounters(t *testing.T) {
	st, _ := auditStore(t)
	ctx := context.Background()
	if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.AppendBatch(ctx, "s1", userMsg("a"), userMsg("b"), userMsg("c")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if _, err := st.Replace(ctx, "s1", nil); err != nil {
		t.Fatalf("Replace(nil): %v", err)
	}
	metas, err := st.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].Messages != 0 || metas[0].Bytes != 0 {
		t.Fatalf("清空后索引 = %+v, want messages:0 bytes:0", metas)
	}
}

// F-SS2（major）：写入侧接受"读取侧认定为损坏"的消息（无 role），
// 结果 Append 成功返回、Load 却读不回来 —— 静默丢数据。
func TestAuditAppendRejectsMessageWithoutRole(t *testing.T) {
	st, _ := auditStore(t)
	ctx := context.Background()
	if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 一条没有 role 的消息：Content 有内容，但 Role 是零值。
	bad := rig.Message{Content: []rig.ContentPart{rig.TextPart("我把内容写进去了，但角色丢了")}}
	meta, err := st.Append(ctx, "s1", bad)
	if err == nil {
		// 写入"成功"了：那它必须能被读回来，否则就是静默丢数据。
		_, messages, loadErr := st.Load(ctx, "s1")
		t.Fatalf("Append(无 role) 返回成功 meta=%+v（Messages=%d），Load 得到 %d 条、err=%v；"+
			"写侧必须拒绝这种记录，否则数据静默消失", meta, meta.Messages, len(messages), loadErr)
	}
	if !errors.Is(err, store.ErrInvalidMessage) {
		t.Fatalf("错误 = %v, want ErrInvalidMessage", err)
	}
	// 失败不得污染索引。
	metas, listErr := st.List(ctx, store.Filter{})
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(metas) != 1 || metas[0].Messages != 0 || metas[0].Bytes != 0 {
		t.Fatalf("被拒绝的批次污染了索引: %+v", metas)
	}
}

// F-SS3（minor）：数据文件被外部删除后，Append 不能"复活"它并写下错误计数。
func TestAuditAppendDoesNotResurrectDeletedFile(t *testing.T) {
	st, root := auditStore(t)
	ctx := context.Background()
	if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.AppendBatch(ctx, "s1", userMsg("a"), userMsg("b"), userMsg("c")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := os.Remove(sessionPathIn(root, "s1")); err != nil {
		t.Fatalf("删除数据文件: %v", err)
	}

	_, err := st.Append(ctx, "s1", userMsg("d"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("文件已消失时 Append 应返回 ErrNotFound，得到 %v", err)
	}
	if _, statErr := os.Stat(sessionPathIn(root, "s1")); !os.IsNotExist(statErr) {
		t.Fatalf("Append 复活了被删除的文件: %v", statErr)
	}
	metas, listErr := st.List(ctx, store.Filter{})
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(metas) != 0 {
		t.Fatalf("幽灵条目应被丢弃，List 仍返回 %+v", metas)
	}
}

// F-SS4：崩溃点矩阵——每一种都必须自愈（布局简化后，"索引不一致"整类消失，
// 只剩文件本身能出问题）。
func TestAuditCrashPointsSelfHeal(t *testing.T) {
	t.Run("空文件", func(t *testing.T) {
		st, root := auditStore(t)
		ctx := context.Background()
		if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sessionPathIn(root, "s1"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		meta, messages, err := st.Load(ctx, "s1")
		if err != nil {
			t.Fatalf("空文件应视为空会话: %v", err)
		}
		if len(messages) != 0 || meta.Messages != 0 {
			t.Fatalf("空文件应读出 0 条: %+v", meta)
		}
	})

	t.Run("只有半行", func(t *testing.T) {
		st, root := auditStore(t)
		ctx := context.Background()
		if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
			t.Fatal(err)
		}
		path := sessionPathIn(root, "s1")
		if err := os.WriteFile(path, []byte(`{"role":"user","content":[{"type":"te`), 0o600); err != nil {
			t.Fatal(err)
		}
		meta, messages, err := st.Load(ctx, "s1")
		if err != nil {
			t.Fatalf("半行应被丢弃而不是报错: %v", err)
		}
		if len(messages) != 0 || meta.Messages != 0 {
			t.Fatalf("半行不应被计入: %+v", meta)
		}
		// 后续 Append 必须先把半行裁掉。
		if _, err := st.Append(ctx, "s1", userMsg("after")); err != nil {
			t.Fatalf("Append: %v", err)
		}
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), "te\n") || !strings.HasSuffix(string(raw), "\n") {
			t.Fatalf("半行没有被裁掉: %q", raw)
		}
		if _, messages, err = st.Load(ctx, "s1"); err != nil || len(messages) != 1 {
			t.Fatalf("Append 之后应只有 1 条: %d, %v", len(messages), err)
		}
	})

	t.Run("行间空行", func(t *testing.T) {
		st, root := auditStore(t)
		ctx := context.Background()
		if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
			t.Fatal(err)
		}
		line, _ := json.Marshal(userMsg("keep"))
		body := "\n" + string(line) + "\n\n" + string(line) + "\n"
		if err := os.WriteFile(sessionPathIn(root, "s1"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		meta, messages, err := st.Load(ctx, "s1")
		if err != nil {
			t.Fatalf("空行应被跳过: %v", err)
		}
		if len(messages) != 2 || meta.Messages != 2 {
			t.Fatalf("空行场景应读出 2 条: %d / %+v", len(messages), meta)
		}
	})

	t.Run("重启后条数由 Load 校正", func(t *testing.T) {
		st, root := auditStore(t)
		ctx := context.Background()
		if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendBatch(ctx, "s1", userMsg("a"), userMsg("b")); err != nil {
			t.Fatal(err)
		}
		// 追加一条"已落盘但内存索引不知道"的记录（崩溃在提交前）。
		line, _ := json.Marshal(userMsg("c"))
		file, err := os.OpenFile(sessionPathIn(root, "s1"), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()

		reopened, err := store.NewDir(root)
		if err != nil {
			t.Fatalf("NewDir: %v", err)
		}
		defer func() { _ = reopened.Close() }()
		meta, messages, err := reopened.Load(ctx, "s1")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(messages) != 3 || meta.Messages != 3 {
			t.Fatalf("Load = %d 条 (meta %d), want 3（盘上的数据是唯一真相）", len(messages), meta.Messages)
		}
	})

	t.Run("文件里混入非消息JSON", func(t *testing.T) {
		st, root := auditStore(t)
		ctx := context.Background()
		if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
			t.Fatal(err)
		}
		line, _ := json.Marshal(userMsg("good"))
		body := `{"not":"a message"}` + "\n" + string(line) + "\n" + `[1,2,3]` + "\n" + `{}` + "\n"
		if err := os.WriteFile(sessionPathIn(root, "s1"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		meta, messages, err := st.Load(ctx, "s1")
		if err != nil {
			t.Fatalf("部分损坏不应整份失败: %v", err)
		}
		if len(messages) != 1 || meta.Messages != 1 {
			t.Fatalf("应只读出 1 条有效消息: %d / %+v", len(messages), meta)
		}
	})
}

// F-SS5：ID 边界矩阵。
func TestAuditIDBoundaries(t *testing.T) {
	st, root := auditStore(t)
	ctx := context.Background()

	rejected := []string{"../x", "a/b", `a\b`, ".hidden", ".", "..", "a\x00b", "a\nb", strings.Repeat("x", 129)}
	for _, id := range rejected {
		if _, err := st.Create(ctx, store.Meta{ID: id}); !errors.Is(err, store.ErrInvalidID) {
			t.Fatalf("Create(%q) = %v, want ErrInvalidID", id, err)
		}
	}

	accepted := []string{
		strings.Repeat("x", 128),            // 正好上限
		"全中文的会话-ID",                         // 非 ASCII → 哈希文件名
		"with space",                        // 空格 → 哈希
		"con", "nul", "aux", "com1", "LPT9", // Windows 设备名（见内部审计用例）
	}
	for _, id := range accepted {
		if _, err := st.Create(ctx, store.Meta{ID: id}); err != nil {
			t.Fatalf("Create(%q) = %v, want 成功", id, err)
		}
	}
	// 全部落在根目录里（扁平布局），且没有逃出 root。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || strings.ContainsAny(e.Name(), `/\`) {
			t.Fatalf("根目录里出现了非法条目: %q", e.Name())
		}
		if !strings.HasSuffix(e.Name(), ".ndjson") {
			t.Fatalf("根目录里出现了非会话文件: %q", e.Name())
		}
	}
	if len(entries) != len(accepted) {
		t.Fatalf("文件数 = %d, want %d", len(entries), len(accepted))
	}
}

// F-SS6：批量原子性 —— 批里有一条超限，整批不得落盘，索引不得变。
func TestAuditBatchAtomicityOnTooLarge(t *testing.T) {
	st, root := auditStore(t)
	ctx := context.Background()
	if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	big := userMsg(strings.Repeat("x", store.MaxMessageBytes))
	_, err := st.AppendBatch(ctx, "s1", userMsg("ok"), big, userMsg("ok2"))
	if !errors.Is(err, store.ErrMessageTooLarge) {
		t.Fatalf("AppendBatch = %v, want ErrMessageTooLarge", err)
	}
	raw, _ := os.ReadFile(sessionPathIn(root, "s1"))
	if len(raw) != 0 {
		t.Fatalf("超限批次写入了 %d 字节，违反'整批不落盘'", len(raw))
	}
	metas, listErr := st.List(ctx, store.Filter{})
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(metas) != 1 || metas[0].Messages != 0 || metas[0].Bytes != 0 {
		t.Fatalf("超限批次污染索引: %+v", metas)
	}
}

// F-SS7：并发混合压测（-race）：Create/Append/List 交错后，索引与文件必须一致，
// 且不出现重复条目、不留临时文件。
func TestAuditConcurrentMixedStress(t *testing.T) {
	st, root := auditStore(t)
	ctx := context.Background()

	const workers = 24
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 6; i++ {
				id := "s" + string(rune('a'+w%8))
				switch i % 3 {
				case 0:
					_, _ = st.Create(ctx, store.Meta{ID: id, Title: "t"})
				case 1:
					_, _ = st.Append(ctx, id, userMsg("m"))
				default:
					_, _ = st.List(ctx, store.Filter{})
				}
			}
		}(w)
	}
	wg.Wait()

	// 索引与文件一一对应，且计数等于文件里的有效行数（Load 校正后）。
	metas, err := st.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := map[string]bool{}
	for _, meta := range metas {
		if seen[meta.ID] {
			t.Fatalf("List 里出现重复条目 %q", meta.ID)
		}
		seen[meta.ID] = true
		if _, _, err := st.Load(ctx, meta.ID); err != nil {
			t.Fatalf("Load(%q): %v", meta.ID, err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("残留临时文件: %q", e.Name())
		}
		id := strings.TrimSuffix(e.Name(), ".ndjson")
		if !seen[id] {
			t.Fatalf("孤儿数据文件（内存索引里没有）: %q", e.Name())
		}
	}
}

// F-SS8：List 的排序/过滤稳定性（固定时钟 → 时间相等时按 ID 倒序）。
func TestAuditListOrderingAndFilter(t *testing.T) {
	dir := t.TempDir()
	fixed := someTime()
	st, err := store.NewDir(filepath.Join(dir, "s"), store.WithClock(func() time.Time { return fixed }))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if _, err := st.Create(ctx, store.Meta{ID: id, Workspace: "/w1"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	first, err := st.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	second, err := st.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("同一状态两次 List 结果不同（排序不确定）:\n%+v\n%+v", first, second)
	}
	if len(first) != 3 || first[0].ID != "c" || first[2].ID != "a" {
		t.Fatalf("时间相等时应按 ID 倒序: %+v", first)
	}
	if got, _ := st.List(ctx, store.Filter{Limit: 2}); len(got) != 2 {
		t.Fatalf("Limit=2 返回 %d 条", len(got))
	}
	if got, _ := st.List(ctx, store.Filter{Workspace: "/nope"}); len(got) != 0 {
		t.Fatalf("Workspace 过滤失效: %+v", got)
	}
	if got, _ := st.List(ctx, store.Filter{Since: fixed.Add(time.Hour)}); len(got) != 0 {
		t.Fatalf("Since 过滤失效: %+v", got)
	}
}

// F-SS9：Delete 之后用同一 ID 重建。
func TestAuditDeleteThenCreateSameID(t *testing.T) {
	st, _ := auditStore(t)
	ctx := context.Background()
	if _, err := st.Create(ctx, store.Meta{ID: "same"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, "same", userMsg("old")); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "same"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "same"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("重复 Delete = %v, want ErrNotFound", err)
	}
	if _, err := st.Create(ctx, store.Meta{ID: "same"}); err != nil {
		t.Fatalf("Delete 之后应能重建同一 ID: %v", err)
	}
	meta, messages, err := st.Load(ctx, "same")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 0 || meta.Messages != 0 {
		t.Fatalf("重建后的会话不应残留旧消息: %+v", meta)
	}
}

// F-SS10：History 契约 —— Load 必须返回调用方可随意修改的拷贝。
func TestAuditHistoryLoadReturnsIndependentSlice(t *testing.T) {
	st, _ := auditStore(t)
	ctx := context.Background()
	history := st.History(ctx, "h1")
	if err := history.Append(ctx, "h1", userMsg("original")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	first, err := history.Load(ctx, "h1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	first[0].Content[0].Text = "被调用方改掉了"
	_ = append(first, userMsg("多余")) // 只验证"改返回值不影响存储"

	second, err := history.Load(ctx, "h1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(second) != 1 || second[0].Text() != "original" {
		t.Fatalf("Load 返回了内部状态的别名: %#v", second)
	}
}

// F-SS11：History 的错误传播 + 关闭语义。
func TestAuditHistoryErrorPropagation(t *testing.T) {
	st, _ := auditStore(t)
	ctx := context.Background()
	history := st.History(ctx, "h1")
	if err := history.Append(ctx, "h1", userMsg("x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := history.Append(ctx, "h1", userMsg("y")); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 Append = %v, want ErrClosed", err)
	}
	if _, err := history.Load(ctx, "h1"); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 Load = %v, want ErrClosed", err)
	}
	if err := history.Replace(ctx, "h1", nil); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 Replace = %v, want ErrClosed", err)
	}
}

// F-SS12：目录异常（不存在/是文件）。
func TestAuditBadDirectory(t *testing.T) {
	t.Run("是文件", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "not-a-dir")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.NewDir(path); err == nil {
			t.Fatal("目录位置是普通文件时必须报错")
		}
	})
	t.Run("不存在则创建", func(t *testing.T) {
		root := t.TempDir()
		st, err := store.NewDir(filepath.Join(root, "a", "b", "c"))
		if err != nil {
			t.Fatalf("NewDir: %v", err)
		}
		defer func() { _ = st.Close() }()
	})
}

// F-SS16：Replace 与并发 Append 交错后，计数必须等于文件里的真实条数
// （两者由同一把分片锁串行化，任何交错都不允许留下 ErrCorrupt 或错计数）。
func TestAuditReplaceVersusConcurrentAppend(t *testing.T) {
	st, _ := auditStore(t)
	ctx := context.Background()
	if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.AppendBatch(ctx, "s1", userMsg("seed")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				_, _ = st.Append(ctx, "s1", userMsg("concurrent"))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 8; j++ {
			_, _ = st.Replace(ctx, "s1", []rig.Message{userMsg("window")})
		}
	}()
	wg.Wait()

	// 文件必须始终可解析（Replace 是 rename，Append 是单行追加）。
	meta, messages, err := st.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("交错之后 Load 失败（文件被写坏）: %v", err)
	}
	if meta.Messages != len(messages) {
		t.Fatalf("索引条数 %d 与实际条数 %d 不一致", meta.Messages, len(messages))
	}
	metas, err := st.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].Messages != len(messages) {
		t.Fatalf("List 与实际条数不一致: %+v vs %d", metas, len(messages))
	}
	if metas[0].Bytes <= 0 {
		t.Fatalf("索引 Bytes = %d，应为正数", metas[0].Bytes)
	}
}

// F-SS17（nit）：根目录本身是符号链接时，文件写进链接目标（本包信任自己的目录）。
func TestAuditSymlinkedRootIsFollowed(t *testing.T) {
	parent := t.TempDir()
	realDir := filepath.Join(parent, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	st, err := store.NewDir(link)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(realDir, "s1.ndjson")); err != nil {
		t.Fatalf("数据文件应落在符号链接目标里: %v", err)
	}
}

// F-SS18（minor）：只有空行的会话文件被误判成"整份损坏"。
func TestAuditBlankOnlyFileIsEmptyNotCorrupt(t *testing.T) {
	st, root := auditStore(t)
	ctx := context.Background()
	if _, err := st.Create(ctx, store.Meta{ID: "s1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(sessionPathIn(root, "s1"), []byte("\n\n\n"), 0o600); err != nil {
		t.Fatalf("写入: %v", err)
	}
	meta, messages, err := st.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("只有空行的文件应等价于空会话，得到 %v", err)
	}
	if len(messages) != 0 || meta.Messages != 0 {
		t.Fatalf("应读出 0 条: %d / %+v", len(messages), meta)
	}
	// 真正的"全是坏记录"仍然必须报 ErrCorrupt（不要把容错扩大成永久静默）。
	bad, _ := json.Marshal(rig.Message{Content: []rig.ContentPart{rig.TextPart("no role")}})
	if err := os.WriteFile(sessionPathIn(root, "s1"), append(bad, '\n'), 0o600); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if _, _, err := st.Load(ctx, "s1"); !errors.Is(err, store.ErrCorrupt) {
		t.Fatalf("全是坏记录时 = %v, want ErrCorrupt", err)
	}
}
