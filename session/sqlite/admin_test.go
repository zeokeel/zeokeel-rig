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
	if !meta.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("updated_at 没有推进：%v（原 %v）", meta.UpdatedAt, created.UpdatedAt)
	}
	if !meta.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("created_at 被删除改掉了：%v（原 %v）", meta.CreatedAt, created.CreatedAt)
	}
	if after := rawSessionRevision(t, path, created.ID); after != before+1 {
		t.Fatalf("revision = %d，want %d（删除要像 Append 一样 +1）", after, before+1)
	}

	// 幂等空删：不推进 revision（否则一次误点会把会话顶到列表最前面）。
	stable := rawSessionRevision(t, path, created.ID)
	if _, err := store.DeleteMessages(ctx, created.ID, []int{0}); err != nil {
		t.Fatalf("空删 DeleteMessages: %v", err)
	}
	if got := rawSessionRevision(t, path, created.ID); got != stable {
		t.Fatalf("空删推进了 revision：%d，want %d", got, stable)
	}
	if _, err := store.TruncateMessages(ctx, created.ID, 10); err != nil {
		t.Fatalf("空截断 TruncateMessages: %v", err)
	}
	if got := rawSessionRevision(t, path, created.ID); got != stable {
		t.Fatalf("空截断推进了 revision：%d，want %d", got, stable)
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
		{Model: "m1", PromptTokens: 100, CompletionTokens: 10, CostUSD: 0.5},
		{Model: "m2", PromptTokens: 50, CompletionTokens: 5, CostUSD: 0.25},
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

// TestSessionAdminUnknownSessionReportsNotFound：三个新方法对未知会话返回与 Load
// 一致的哨兵。
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
