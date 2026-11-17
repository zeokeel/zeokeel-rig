package store_test

// S-P2 修复的回归用例（TDD）：跨实例的读/写/删要有磁盘兜底。
//
// 约束（写进文档）：同一目录仍然只推荐**一个写入者**；这些兜底是为了让"另一个窗口
// 看得见文件却读不出来"不再发生，不是为了支持并发写。

import (
	"context"
	"errors"
	"testing"

	"github.com/zeokeel/zeokeel-rig/session/store"
)

// B 实例必须能 Load A 实例新建的会话，且计数正确。
func TestFixCrossInstanceLoadSucceeds(t *testing.T) {
	dir := t.TempDir()
	first := openStore(t, dir)
	second := openStore(t, dir)

	meta, err := first.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := first.Append(context.Background(), meta.ID, user("hello")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	loadedMeta, messages, err := second.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("跨实例 Load 失败: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("消息数 = %d, want 3", len(messages))
	}
	if loadedMeta.Messages != 3 {
		t.Fatalf("Meta.Messages = %d, want 3", loadedMeta.Messages)
	}
	if loadedMeta.ID != meta.ID {
		t.Fatalf("Meta.ID = %q", loadedMeta.ID)
	}
}

// B 实例对 A 的会话追加：必须成功，且之后计数不能变成"1"。
func TestFixCrossInstanceAppendKeepsCount(t *testing.T) {
	dir := t.TempDir()
	first := openStore(t, dir)
	second := openStore(t, dir)

	meta, err := first.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := first.Append(context.Background(), meta.ID, user("one")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := first.Append(context.Background(), meta.ID, user("two")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	appended, err := second.Append(context.Background(), meta.ID, user("three"))
	if err != nil {
		t.Fatalf("跨实例 Append 失败: %v", err)
	}
	if appended.Messages != 3 {
		t.Fatalf("追加后的 Meta.Messages = %d, want 3（不能从 0 重新计数）", appended.Messages)
	}
	// 两个实例都必须读到 3 条。
	_, messages, err := first.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("first 读到 %d 条, want 3", len(messages))
	}
}

// B 实例删除 A 的会话：必须成功；之后两个实例都是 ErrNotFound，文件也没了。
func TestFixCrossInstanceDeleteSucceeds(t *testing.T) {
	dir := t.TempDir()
	first := openStore(t, dir)
	second := openStore(t, dir)

	meta, err := first.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := first.Append(context.Background(), meta.ID, user("x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := second.Delete(context.Background(), meta.ID); err != nil {
		t.Fatalf("跨实例 Delete 失败: %v", err)
	}
	if _, _, err := first.Load(context.Background(), meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("删除后 first.Load 错误 = %v, want ErrNotFound", err)
	}
	if _, _, err := second.Load(context.Background(), meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("删除后 second.Load 错误 = %v, want ErrNotFound", err)
	}
}

// 兜底不能把"真的不存在"变成"存在"。
func TestFixCrossInstanceMissingStaysNotFound(t *testing.T) {
	dir := t.TempDir()
	opened := openStore(t, dir)
	if _, _, err := opened.Load(context.Background(), "20260101T000000-deadbeef"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在的会话 Load 错误 = %v, want ErrNotFound", err)
	}
	if err := opened.Delete(context.Background(), "20260101T000000-deadbeef"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在的会话 Delete 错误 = %v, want ErrNotFound", err)
	}
}
