package store_test

// S-P1 修复的回归用例（TDD）：首条消息超长时，标题提取要继续看后续记录。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/session/store"
)

// collectNotices 返回一个 Notice 收集器。
func collectNotices() (func(format string, args ...any), func() []string) {
	var mu sync.Mutex
	var notices []string
	return func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			notices = append(notices, fmt.Sprintf(format, args...))
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), notices...)
		}
}

// 首条 300 KiB、第二条可读 → 标题必须来自第二条。
func TestFixTitleSkipsOversizeRecord(t *testing.T) {
	dir := t.TempDir()
	notice, snapshot := collectNotices()
	opened, err := store.NewDir(dir, store.WithNotice(notice))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meta, err := opened.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	huge := strings.Repeat("x", 300<<10)
	if _, err := opened.Append(context.Background(), meta.ID, user(huge)); err != nil {
		t.Fatalf("Append huge: %v", err)
	}
	if _, err := opened.Append(context.Background(), meta.ID, user("readable title")); err != nil {
		t.Fatalf("Append title: %v", err)
	}
	_ = opened.Close()

	reopened, err := store.NewDir(dir, store.WithNotice(notice))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	listed, err := reopened.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("会话数 = %d", len(listed))
	}
	if listed[0].Title != "readable title" {
		t.Fatalf("Title = %q, want %q（超长首条应被跳过，继续看后续记录）", listed[0].Title, "readable title")
	}
	t.Logf("notices: %v", snapshot())
}

// 前 4 条全是超长 → 标题为空，且只上报一次 Notice。
func TestFixTitleAllOversizeGivesUpOnce(t *testing.T) {
	dir := t.TempDir()
	notice, snapshot := collectNotices()
	opened, err := store.NewDir(dir, store.WithNotice(notice))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meta, err := opened.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	huge := strings.Repeat("y", 300<<10)
	for i := 0; i < 4; i++ {
		if _, err := opened.Append(context.Background(), meta.ID, user(fmt.Sprintf("%d-%s", i, huge))); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	_ = opened.Close()

	reopened, err := store.NewDir(dir, store.WithNotice(notice))
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	listed, err := reopened.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Title != "" {
		t.Fatalf("4 条全超长时 Title 应为空，得到 %q", listed[0].Title)
	}
	// 只针对"标题扫描"这一件事统计 Notice：允许其它类别的通知存在，但本会话的超长
	// 上报必须恰好一次。
	oversize := 0
	for _, item := range snapshot() {
		if strings.Contains(item, "超过") && strings.Contains(item, "标题") {
			oversize++
		}
	}
	if oversize != 1 {
		t.Fatalf("超长导致的标题 Notice 次数 = %d, want 1（全部通知：%v）", oversize, snapshot())
	}
}
