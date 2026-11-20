package sqlite_test

// 会话管理面：按序号删消息、按"保留最新 N 条"截断、会话体检统计、清检查点。
//
// 这组方法的共同承诺：**一个事务**、幂等、未知会话用 Load 的同一个哨兵
// （errors.Is(err, ErrNotFound)）；删除只改消息表，**不重排 ordinal**，所以历史
// 里留下的空洞是稳定的——宿主手里已有的序号不会因为一次删除而指向别的消息。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// rawMessageOrdinals 直接读库里的 ordinal 列：Load 只返回消息内容，而"不重排"
// 这条契约只能看列。
func rawMessageOrdinals(t *testing.T, path, id string) []int {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.Query(`SELECT ordinal FROM rig_messages WHERE session_id = ? ORDER BY ordinal`, id)
	if err != nil {
		t.Fatalf("read ordinals of %s: %v", id, err)
	}
	defer func() { _ = rows.Close() }()
	var ordinals []int
	for rows.Next() {
		var ordinal int
		if err := rows.Scan(&ordinal); err != nil {
			t.Fatalf("scan ordinal: %v", err)
		}
		ordinals = append(ordinals, ordinal)
	}
	return ordinals
}

// rawSessionUpdatedAt 直接读库里的 `updated_at`（毫秒）：要钉住"删除前后严格变大"，
// 必须绕开 Meta 的时间值，否则拿 Create 时那个"2 小时前"的基线比，断言恒真。
func rawSessionUpdatedAt(t *testing.T, path, id string) int64 {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var updatedAt int64
	if err := raw.QueryRow(`SELECT updated_at FROM rig_sessions WHERE id = ?`, id).Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at of %s: %v", id, err)
	}
	return updatedAt
}

// rawNextOrdinal 直接读会话行的 ordinal 高水位。
func rawNextOrdinal(t *testing.T, path, id string) int64 {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var next int64
	if err := raw.QueryRow(`SELECT next_ordinal FROM rig_sessions WHERE id = ?`, id).Scan(&next); err != nil {
		t.Fatalf("read next_ordinal of %s: %v", id, err)
	}
	return next
}

// TestDeleteMessagesRemovesOnlyGivenOrdinals：只删给定的序号；不存在与重复的序号
// 幂等跳过，返回值是**真正删掉的行数**。
func TestDeleteMessagesRemovesOnlyGivenOrdinals(t *testing.T) {
	store, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if _, err := store.AppendBatch(ctx, "s-del",
		userMessage("m0"), assistantMessage("m1"), userMessage("m2"), assistantMessage("m3"),
	); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	removed, err := store.DeleteMessages(ctx, "s-del", []int{0, 2, 2, 99})
	if err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	if removed != 2 {
		t.Fatalf("删掉 %d 行，want 2（重复 2 与不存在的 99 不计）", removed)
	}

	meta, messages, err := store.Load(ctx, "s-del")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 || messages[0].Text() != "m1" || messages[1].Text() != "m3" {
		t.Fatalf("剩余消息 = %q/%q，want m1/m3", messages[0].Text(), messages[1].Text())
	}
	if meta.Messages != 2 {
		t.Fatalf("message_count = %d，want 2", meta.Messages)
	}

	// 幂等：同一批序号再来一次，一行都不该删、也不该报错。
	again, err := store.DeleteMessages(ctx, "s-del", []int{0, 2})
	if err != nil {
		t.Fatalf("第二次 DeleteMessages: %v", err)
	}
	if again != 0 {
		t.Fatalf("第二次删掉 %d 行，want 0", again)
	}
}

// TestDeleteMessagesKeepsOrdinalsStable：删除**不重排**序号，之后的追加也不会
// 复用空洞。
func TestDeleteMessagesKeepsOrdinalsStable(t *testing.T) {
	store, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if _, err := store.AppendBatch(ctx, "s-stable",
		userMessage("m0"), assistantMessage("m1"), userMessage("m2"), assistantMessage("m3"),
	); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if _, err := store.DeleteMessages(ctx, "s-stable", []int{1}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	if got := rawMessageOrdinals(t, path, "s-stable"); fmt.Sprint(got) != "[0 2 3]" {
		t.Fatalf("删除后的序号 = %v，want [0 2 3]（不重排）", got)
	}

	// 追加走 MAX(ordinal)+1：空洞不会被重新占用。
	if _, err := store.Append(ctx, "s-stable", userMessage("m4")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := rawMessageOrdinals(t, path, "s-stable"); fmt.Sprint(got) != "[0 2 3 4]" {
		t.Fatalf("追加后的序号 = %v，want [0 2 3 4]（不复用空洞）", got)
	}
}

// TestAppendDoesNotReuseOrdinalAfterTruncate：删掉**尾部**消息之后再追加，新消息
// 不能落回被删掉的序号。
//
// ordinal 是宿主手里已经拿到的游标（`MessageHit.Ordinal`、`SearchQuery.AfterOrdinal`）：
// 一旦被复用，旧序号会静默指向另一条消息。`MAX(ordinal)+1` 只看现存行，删掉当前最大值
// 之后它正好落回空洞，所以高水位必须记在会话行上（`rig_sessions.next_ordinal`）。
func TestAppendDoesNotReuseOrdinalAfterTruncate(t *testing.T) {
	store, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if _, err := store.AppendBatch(ctx, "s-reuse",
		userMessage("m0"), assistantMessage("m1"), userMessage("m2")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if _, err := store.DeleteMessages(ctx, "s-reuse", []int{1, 2}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	if got := rawMessageOrdinals(t, path, "s-reuse"); fmt.Sprint(got) != "[0]" {
		t.Fatalf("删除后的序号 = %v，want [0]", got)
	}

	if _, err := store.Append(ctx, "s-reuse", userMessage("m3")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := rawMessageOrdinals(t, path, "s-reuse"); fmt.Sprint(got) != "[0 3]" {
		t.Fatalf("追加后的序号 = %v，want [0 3]（不复用被删掉的 1/2）", got)
	}
	if _, messages, err := store.Load(ctx, "s-reuse"); err != nil || len(messages) != 2 || messages[1].Text() != "m3" {
		t.Fatalf("Load = %d 条（err %v），want 2 条且末条是 m3", len(messages), err)
	}
}

// TestAppendDoesNotReuseOrdinalAfterTruncateAll：清空历史之后再追加，序号必须接着
// 原来的高水位走，而不是从 0 重新开始。
func TestAppendDoesNotReuseOrdinalAfterTruncateAll(t *testing.T) {
	store, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if _, err := store.AppendBatch(ctx, "s-reuse-all",
		userMessage("m0"), assistantMessage("m1"), userMessage("m2")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	removed, err := store.TruncateMessages(ctx, "s-reuse-all", 0)
	if err != nil || removed != 3 {
		t.Fatalf("TruncateMessages(0) = %d, %v，want 3, nil", removed, err)
	}
	if got := rawMessageOrdinals(t, path, "s-reuse-all"); len(got) != 0 {
		t.Fatalf("清空后的序号 = %v，want 空", got)
	}

	if _, err := store.Append(ctx, "s-reuse-all", userMessage("m3")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := rawMessageOrdinals(t, path, "s-reuse-all"); fmt.Sprint(got) != "[3]" {
		t.Fatalf("清空后追加的序号 = %v，want [3]（不能回到 0）", got)
	}
}

// TestReplaceDoesNotRewindNextOrdinal：压缩（Replace）把历史重排成 0..n-1 之后，
// 高水位只能前进——被替换掉的旧序号不能被之后的消息复用。
func TestReplaceDoesNotRewindNextOrdinal(t *testing.T) {
	store, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if _, err := store.AppendBatch(ctx, "s-replace",
		userMessage("m0"), assistantMessage("m1"), userMessage("m2")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if _, err := store.Replace(ctx, "s-replace", []rig.Message{assistantMessage("摘要")}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := rawMessageOrdinals(t, path, "s-replace"); fmt.Sprint(got) != "[0]" {
		t.Fatalf("Replace 之后的序号 = %v，want [0]（历史按 0..n-1 重排）", got)
	}
	if _, err := store.Append(ctx, "s-replace", userMessage("m3")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := rawMessageOrdinals(t, path, "s-replace"); fmt.Sprint(got) != "[0 3]" {
		t.Fatalf("Replace 之后追加的序号 = %v，want [0 3]（高水位只前进）", got)
	}
}

// TestDeleteMessagesHandlesManyOrdinals：一次删一大批不能撞上 SQLite 的变量上限
// （旧实现每个 ordinal 一个 `?`，超过驱动上限整条语句报 too many SQL variables）。
// 姊妹方法 TruncateMessages 用子查询没有这个问题，两条路径的能力必须一致。
func TestDeleteMessagesHandlesManyOrdinals(t *testing.T) {
	store, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	const total = 33000
	msgs := make([]rig.Message, total)
	for index := range msgs {
		msgs[index] = rig.NewMessage(rig.RoleUser, fmt.Sprintf("m%d", index))
	}
	if _, err := store.AppendBatch(ctx, "s-many", msgs...); err != nil {
		t.Fatalf("AppendBatch(%d 条): %v", total, err)
	}
	ordinals := make([]int, 0, total-1)
	for index := 0; index < total-1; index++ {
		ordinals = append(ordinals, index)
	}
	removed, err := store.DeleteMessages(ctx, "s-many", ordinals)
	if err != nil {
		t.Fatalf("DeleteMessages(%d 个序号): %v", len(ordinals), err)
	}
	if removed != total-1 {
		t.Fatalf("删掉 %d 行，want %d", removed, total-1)
	}
	meta, messages, err := store.Load(ctx, "s-many")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 1 || meta.Messages != 1 {
		t.Fatalf("剩余 %d 条 / count %d，want 1/1", len(messages), meta.Messages)
	}
	if want := fmt.Sprintf("m%d", total-1); messages[0].Text() != want {
		t.Fatalf("剩下的消息 = %q，want %q", messages[0].Text(), want)
	}
}

// TestTruncateMessagesKeepsNewest：只保留最新 keep 条；keep<=0 删空；
// keep 大于总数时什么都不删。
func TestTruncateMessagesKeepsNewest(t *testing.T) {
	store, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if _, err := store.AppendBatch(ctx, "s-trunc",
		userMessage("m0"), assistantMessage("m1"), userMessage("m2"),
		assistantMessage("m3"), userMessage("m4"),
	); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	removed, err := store.TruncateMessages(ctx, "s-trunc", 2)
	if err != nil {
		t.Fatalf("TruncateMessages(2): %v", err)
	}
	if removed != 3 {
		t.Fatalf("删掉 %d 行，want 3", removed)
	}
	meta, messages, err := store.Load(ctx, "s-trunc")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 || messages[0].Text() != "m3" || messages[1].Text() != "m4" {
		t.Fatalf("保留的消息 = %q/%q，want m3/m4", messages[0].Text(), messages[1].Text())
	}
	if meta.Messages != 2 {
		t.Fatalf("message_count = %d，want 2", meta.Messages)
	}
	if got := rawMessageOrdinals(t, path, "s-trunc"); fmt.Sprint(got) != "[3 4]" {
		t.Fatalf("保留的序号 = %v，want [3 4]", got)
	}

	// keep 大于总数：一行都不删。
	if removed, err := store.TruncateMessages(ctx, "s-trunc", 10); err != nil || removed != 0 {
		t.Fatalf("TruncateMessages(10) = %d, %v，want 0, nil", removed, err)
	}

	// keep=0：删空。
	removed, err = store.TruncateMessages(ctx, "s-trunc", 0)
	if err != nil {
		t.Fatalf("TruncateMessages(0): %v", err)
	}
	if removed != 2 {
		t.Fatalf("keep=0 删掉 %d 行，want 2", removed)
	}
	meta, messages, err = store.Load(ctx, "s-trunc")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 0 || meta.Messages != 0 {
		t.Fatalf("keep=0 之后 = %d 条消息 / count %d，want 0/0", len(messages), meta.Messages)
	}
}

// TestSessionAdminUpdatesMetaOnDelete：删除消息是一次写，与 Append 一样维护
// updated_at / revision / message_count；没有删到行时两者都不推进。
func TestSessionAdminUpdatesMetaOnDelete(t *testing.T) {
	store, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	createdAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)
	created, err := store.Create(ctx, sqlitestore.Meta{Title: "删消息", CreatedAt: createdAt, UpdatedAt: createdAt})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.AppendBatch(ctx, created.ID,
		userMessage("m0"), assistantMessage("m1"), userMessage("m2")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	before := rawSessionRevision(t, path, created.ID)
	// 基线必须是**追加之后**的 updated_at：created.UpdatedAt 是 2 小时前的值，
	// 而 AppendBatch 早已把 updated_at 推到 now，拿它比较会让断言恒真（实现不再
	// 推进 updated_at 也成立）。毫秒级时间戳要留出至少一毫秒才能分开两次写。
	beforeUpdatedAt := rawSessionUpdatedAt(t, path, created.ID)
	time.Sleep(3 * time.Millisecond)

	if _, err := store.DeleteMessages(ctx, created.ID, []int{0}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	meta, _, err := store.Load(ctx, created.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta.Messages != 2 {
		t.Fatalf("message_count = %d，want 2", meta.Messages)
	}
	if got := rawSessionUpdatedAt(t, path, created.ID); got <= beforeUpdatedAt {
		t.Fatalf("updated_at 没有推进：%d，want > %d", got, beforeUpdatedAt)
	}
	if !meta.UpdatedAt.Equal(time.UnixMilli(rawSessionUpdatedAt(t, path, created.ID)).UTC()) {
		t.Fatalf("Load 的 updated_at = %v，与库内 %d 不一致", meta.UpdatedAt, rawSessionUpdatedAt(t, path, created.ID))
	}
	if !meta.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("created_at 被删除改掉了：%v（原 %v）", meta.CreatedAt, created.CreatedAt)
	}
	if after := rawSessionRevision(t, path, created.ID); after != before+1 {
		t.Fatalf("revision = %d，want %d（删除要像 Append 一样 +1）", after, before+1)
	}

	// 幂等空删：revision 与 updated_at 都不推进（否则一次误点会把会话顶到列表最前面）。
	stable := rawSessionRevision(t, path, created.ID)
	stableUpdatedAt := rawSessionUpdatedAt(t, path, created.ID)
	time.Sleep(3 * time.Millisecond)
	if _, err := store.DeleteMessages(ctx, created.ID, []int{0}); err != nil {
		t.Fatalf("空删 DeleteMessages: %v", err)
	}
	if got := rawSessionRevision(t, path, created.ID); got != stable {
		t.Fatalf("空删推进了 revision：%d，want %d", got, stable)
	}
	if got := rawSessionUpdatedAt(t, path, created.ID); got != stableUpdatedAt {
		t.Fatalf("空删推进了 updated_at：%d，want %d", got, stableUpdatedAt)
	}
	time.Sleep(3 * time.Millisecond)
	if _, err := store.TruncateMessages(ctx, created.ID, 10); err != nil {
		t.Fatalf("空截断 TruncateMessages: %v", err)
	}
	if got := rawSessionRevision(t, path, created.ID); got != stable {
		t.Fatalf("空截断推进了 revision：%d，want %d", got, stable)
	}
	if got := rawSessionUpdatedAt(t, path, created.ID); got != stableUpdatedAt {
		t.Fatalf("空截断推进了 updated_at：%d，want %d", got, stableUpdatedAt)
	}
}

// TestStatsReportsCountsAndTotals：会话体检把三张表的条数与用量合计、载荷字节、
// 检查点存在性一次报齐。
func TestStatsReportsCountsAndTotals(t *testing.T) {
	store, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := store.Create(ctx, sqlitestore.Meta{Title: "体检"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := userMessage("第一条")
	second := assistantMessage("第二条")
	if _, err := store.AppendBatch(ctx, created.ID, first, second); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := store.SessionLog().AppendEvent(ctx, rig.SessionEvent{
		SessionID: created.ID, Type: "turn", Payload: json.RawMessage(`{"n":1}`),
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	for _, entry := range []sqlitestore.CostEntry{
		{Model: "m1", PromptTokens: 100, CompletionTokens: 10, CachedTokens: 60, CostUSD: 0.5},
		{Model: "m2", PromptTokens: 50, CompletionTokens: 5, CachedTokens: 40, CostUSD: 0.25},
	} {
		if err := store.SaveCost(ctx, created.ID, entry); err != nil {
			t.Fatalf("SaveCost: %v", err)
		}
	}
	if err := store.Checkpoints().Save(ctx, created.ID, &rig.Checkpoint{SessionID: created.ID, TurnID: "t1"}); err != nil {
		t.Fatalf("Save checkpoint: %v", err)
	}

	stats, err := store.Stats(ctx, created.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Messages != 2 || stats.Events != 1 || stats.Costs != 2 {
		t.Fatalf("条数 = messages %d / events %d / costs %d，want 2/1/2", stats.Messages, stats.Events, stats.Costs)
	}
	if stats.PromptTokens != 150 || stats.CompletionTokens != 15 {
		t.Fatalf("token 合计 = %d/%d，want 150/15", stats.PromptTokens, stats.CompletionTokens)
	}
	if math.Abs(stats.CostUSD-0.75) > 1e-9 {
		t.Fatalf("cost_usd 合计 = %v，want 0.75", stats.CostUSD)
	}
	// cached_tokens 是 prompt 的子集，但它是独立的一个用量面：Stats 必须与包内
	// 其余合计（CostTotals / SessionCost / 导出 faces）同一口径，否则宿主没法用
	// 体检数对账缓存命中。
	totals, err := store.CostTotals(ctx, sqlitestore.CostFilter{SessionID: created.ID})
	if err != nil {
		t.Fatalf("CostTotals: %v", err)
	}
	if stats.CachedTokens != 100 || stats.CachedTokens != totals.CachedTokens {
		t.Fatalf("cached_tokens 合计 = %d（CostTotals %d），want 100（两者一致）",
			stats.CachedTokens, totals.CachedTokens)
	}
	firstPayload, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	secondPayload, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if want := int64(len(firstPayload) + len(secondPayload)); stats.Bytes != want {
		t.Fatalf("payload 字节 = %d，want %d", stats.Bytes, want)
	}
	if !stats.Checkpoint {
		t.Fatal("Checkpoint = false，want true")
	}
	if stats.CreatedAt.IsZero() || stats.UpdatedAt.IsZero() {
		t.Fatalf("时间戳缺失：%+v", stats)
	}
	if !stats.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("created_at = %v，want %v", stats.CreatedAt, created.CreatedAt)
	}

	// 清掉检查点之后，统计只剩消息/事件/花费。
	if _, err := store.DeleteCheckpoint(ctx, created.ID); err != nil {
		t.Fatalf("DeleteCheckpoint: %v", err)
	}
	stats, err = store.Stats(ctx, created.ID)
	if err != nil {
		t.Fatalf("Stats(第二次): %v", err)
	}
	if stats.Checkpoint {
		t.Fatal("DeleteCheckpoint 之后 Checkpoint = true")
	}
	if stats.Messages != 2 || stats.Events != 1 || stats.Costs != 2 {
		t.Fatalf("清检查点影响了其余计数：%+v", stats)
	}
}

// TestDeleteCheckpoint：删掉返回 true，再删返回 false（没有则 false, nil）。
func TestDeleteCheckpoint(t *testing.T) {
	store, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := store.Create(ctx, sqlitestore.Meta{Title: "检查点"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Checkpoints().Save(ctx, created.ID, &rig.Checkpoint{SessionID: created.ID, TurnID: "t1"}); err != nil {
		t.Fatalf("Save checkpoint: %v", err)
	}
	deleted, err := store.DeleteCheckpoint(ctx, created.ID)
	if err != nil {
		t.Fatalf("DeleteCheckpoint: %v", err)
	}
	if !deleted {
		t.Fatal("有检查点却返回 false")
	}
	if checkpoint, err := store.SessionCheckpoint(ctx, created.ID); err != nil || checkpoint != nil {
		t.Fatalf("删除后 SessionCheckpoint = %v, %v，want nil, nil", checkpoint, err)
	}
	deleted, err = store.DeleteCheckpoint(ctx, created.ID)
	if err != nil {
		t.Fatalf("第二次 DeleteCheckpoint: %v", err)
	}
	if deleted {
		t.Fatal("没有检查点却返回 true")
	}
}

// TestSessionAdminUnknownSessionReportsNotFound：三个删除/体检方法对未知会话返回与
// Load 一致的哨兵；DeleteCheckpoint 不同——它是纯幂等删除，未知会话返回 false, nil。
func TestSessionAdminUnknownSessionReportsNotFound(t *testing.T) {
	store, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if _, _, err := store.Load(ctx, "no-such-session"); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("Load(未知会话) = %v，want ErrNotFound", err)
	}
	if _, err := store.DeleteMessages(ctx, "no-such-session", []int{0}); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("DeleteMessages(未知会话) = %v，want ErrNotFound", err)
	}
	if _, err := store.TruncateMessages(ctx, "no-such-session", 1); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("TruncateMessages(未知会话) = %v，want ErrNotFound", err)
	}
	if _, err := store.Stats(ctx, "no-such-session"); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("Stats(未知会话) = %v，want ErrNotFound", err)
	}
	// 删除是幂等的，但未知会话仍然要先确认存在：空序号列表也不例外。
	if _, err := store.DeleteMessages(ctx, "no-such-session", nil); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("DeleteMessages(未知会话, 空序号) = %v，want ErrNotFound", err)
	}
	// DeleteCheckpoint 是幂等删除：会话不存在不报错，只是没删到行。若有人把它改成
	// "先查会话、不存在报 ErrNotFound"，这里会红。
	if deleted, err := store.DeleteCheckpoint(ctx, "no-such-session"); err != nil || deleted {
		t.Fatalf("DeleteCheckpoint(未知会话) = %v, %v，want false, nil", deleted, err)
	}
}

// TestSessionAdminOnClosedStore：关闭之后四个方法都返回 ErrClosed，而不是 panic。
func TestSessionAdminOnClosedStore(t *testing.T) {
	store, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := store.DeleteMessages(ctx, "s", []int{0}); !errors.Is(err, sqlitestore.ErrClosed) {
		t.Fatalf("关闭后 DeleteMessages = %v，want ErrClosed", err)
	}
	if _, err := store.TruncateMessages(ctx, "s", 0); !errors.Is(err, sqlitestore.ErrClosed) {
		t.Fatalf("关闭后 TruncateMessages = %v，want ErrClosed", err)
	}
	if _, err := store.Stats(ctx, "s"); !errors.Is(err, sqlitestore.ErrClosed) {
		t.Fatalf("关闭后 Stats = %v，want ErrClosed", err)
	}
	if _, err := store.DeleteCheckpoint(ctx, "s"); !errors.Is(err, sqlitestore.ErrClosed) {
		t.Fatalf("关闭后 DeleteCheckpoint = %v，want ErrClosed", err)
	}
}

// TestUpgradeBackfillsNextOrdinal：老库（v12 之前）的会话行没有高水位，升级时必须
// 按 `MAX(ordinal)+1` 回填，否则"删掉尾部消息 → 追加"会复用宿主已经拿到的序号。
//
// v1 库里那条会话只有一条 ordinal 0 的消息：回填后高水位是 1；清空再追加必须拿到 1
// （不修的话 `MAX(ordinal)+1` 在空表上算出 0）。
func TestUpgradeBackfillsNextOrdinal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v11.sqlite")
	buildV1Database(t, path)
	if columns := rawSessionColumns(t, path); has(columns, "next_ordinal") {
		t.Fatalf("v1 库不该有 next_ordinal，columns = %v", columns)
	}

	store, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open(v1 → 最新): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	if got := rawNextOrdinal(t, path, "v1-session"); got != 1 {
		t.Fatalf("升级后的高水位 = %d，want 1（MAX(ordinal)+1 回填）", got)
	}
	if _, err := store.TruncateMessages(ctx, "v1-session", 0); err != nil {
		t.Fatalf("TruncateMessages: %v", err)
	}
	if _, err := store.Append(ctx, "v1-session", userMessage("升级后追加")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := rawMessageOrdinals(t, path, "v1-session"); fmt.Sprint(got) != "[1]" {
		t.Fatalf("升级后追加的序号 = %v，want [1]（不复用 0）", got)
	}
}

// TestOpenAlwaysBackfillsNextOrdinal 钉住"回填不能绑在版本门上"。
//
// 迁移段只写 user_version；回填是提交后的 UPDATE。若把回填写成 `version < 12`，
// 回填失败后 user_version 已经是 12，**下一次打开会静默跳过回填**，序号继续复用；
// `user_version=0` 但核心表已存在的老库也会漏掉。
func TestOpenAlwaysBackfillsNextOrdinal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backfill-next.db")
	ctx := context.Background()

	first, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := first.Append(ctx, "s-next", rig.NewMessage(rig.RoleUser, "一")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 模拟"列已加、值没回填、user_version 已是 12"的中间态（回填失败后的库）。
	if _, err := first.DB().ExecContext(ctx, `UPDATE rig_sessions SET next_ordinal = 0`); err != nil {
		t.Fatalf("reset next_ordinal: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }()
	var next int
	if err := second.DB().QueryRowContext(ctx,
		`SELECT next_ordinal FROM rig_sessions WHERE id = 's-next'`).Scan(&next); err != nil {
		t.Fatalf("read next_ordinal: %v", err)
	}
	if next == 0 {
		t.Fatal("重新打开没有回填 next_ordinal（清空尾部后 Append 会复用序号）")
	}
}
