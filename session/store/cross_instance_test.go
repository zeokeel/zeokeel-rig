package store_test

// 跨实例可见性（同一目录上的两个 store.Dir）：自愈式重载。
//
// 背景：kit 的接线审计发现同目录两个实例互相看不见对方建的会话（Append 报
// ErrNotFound）。加跨进程文件锁会把合法用法（进程重启后新实例接管）一起挡掉，
// 因此这里用"内存未命中 → 按节流策略从磁盘重载一次 → 重试"。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// openPair 在同一个目录上开两个互相独立的实例（模拟"另一个进程/另一个 kit"）。
func openPair(t *testing.T) (*store.Dir, *store.Dir, string) {
	t.Helper()
	dir := t.TempDir()
	first, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir(first): %v", err)
	}
	second, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir(second): %v", err)
	}
	t.Cleanup(func() {
		_ = first.Close()
		_ = second.Close()
	})
	return first, second, dir
}

func TestCrossInstanceSessionIsVisible(t *testing.T) {
	first, second, _ := openPair(t)
	ctx := context.Background()

	created, err := first.Create(ctx, store.Meta{Title: "跨实例", Workspace: "/w"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// second 在 NewDir 时就加载过索引（此时还没有这个会话）：Load 必须能自愈。
	meta, messages, err := second.Load(ctx, created.ID)
	if err != nil {
		t.Fatalf("second.Load: %v", err)
	}
	if meta.ID != created.ID || len(messages) != 0 {
		t.Fatalf("second.Load = (%+v, %d messages)", meta, len(messages))
	}

	// second 追加一条：另一实例刚 Create 的会话必须能正常写。
	if _, err := second.Append(ctx, created.ID, rig.NewMessage(rig.RoleUser, "from-second")); err != nil {
		t.Fatalf("second.Append: %v", err)
	}
	// 回到 first：它自己创建的会话必须看到第二条（重载 + 校正）。
	firstMeta, firstMessages, err := first.Load(ctx, created.ID)
	if err != nil {
		t.Fatalf("first.Load: %v", err)
	}
	if len(firstMessages) != 1 || firstMessages[0].Text() != "from-second" {
		t.Fatalf("first.Load messages = %#v", firstMessages)
	}
	if firstMeta.Messages != 1 {
		t.Fatalf("first.Load meta.Messages = %d, want 1", firstMeta.Messages)
	}

	// second 的 List 也必须看到（虽然它没有创建这个会话）。
	list, err := second.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("second.List: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("second.List = %+v, want the session created by first", list)
	}
}

func TestCrossInstanceAppendToSameSessionKeepsAllRecords(t *testing.T) {
	first, second, dir := openPair(t)
	ctx := context.Background()

	created, err := first.Create(ctx, store.Meta{Title: "shared"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 让两边都先看见这个会话，之后交替追加。
	if _, err := second.Append(ctx, created.ID, rig.NewMessage(rig.RoleUser, "warmup-second")); err != nil {
		t.Fatalf("second warmup append: %v", err)
	}
	if _, err := first.Append(ctx, created.ID, rig.NewMessage(rig.RoleUser, "warmup-first")); err != nil {
		t.Fatalf("first warmup append: %v", err)
	}

	const rounds = 3
	type participant struct {
		label    string
		instance *store.Dir
	}
	participants := []participant{{"first", first}, {"second", second}}
	var wait sync.WaitGroup
	errs := make(chan error, len(participants))
	for _, entry := range participants {
		wait.Add(1)
		go func(entry participant) {
			defer wait.Done()
			for index := 0; index < rounds; index++ {
				_, appendErr := entry.instance.Append(ctx, created.ID,
					rig.NewMessage(rig.RoleUser, fmt.Sprintf("%s-%d", entry.label, index)))
				if appendErr != nil {
					errs <- fmt.Errorf("%s append %d: %w", entry.label, index, appendErr)
					return
				}
			}
		}(entry)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent cross-instance append: %v", err)
	}

	// 文件层面：一条记录一整行，条数 = 2 条 warmup + 2×rounds。
	raw, err := os.ReadFile(filepath.Join(dir, "sessions", created.ID+".ndjson"))
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 2+2*rounds {
		t.Fatalf("session file has %d lines, want %d:\n%s", len(lines), 2+2*rounds, raw)
	}
	for index, line := range lines {
		var message rig.Message
		if unmarshalErr := json.Unmarshal([]byte(line), &message); unmarshalErr != nil {
			t.Fatalf("line %d is not a rig.Message: %v (%q)", index+1, unmarshalErr, line)
		}
	}

	// 逻辑层面：任一实例 Load 都必须看到全部记录（索引可能 last-writer-wins，
	// 但数据都在盘上，Load 的重算会自愈条数）。
	for name, instance := range map[string]*store.Dir{"first": first, "second": second} {
		_, messages, loadErr := instance.Load(ctx, created.ID)
		if loadErr != nil {
			t.Fatalf("%s.Load: %v", name, loadErr)
		}
		if len(messages) != 2+2*rounds {
			t.Fatalf("%s.Load returned %d messages, want %d", name, len(messages), 2+2*rounds)
		}
	}
}

// 另一实例刚 Create、索引还没落地（或索引被 last-writer-wins 覆盖掉）时，
// 只有会话文件存在：重载必须"收养"它。
func TestCrossInstanceAdoptsFileWithoutIndexEntry(t *testing.T) {
	first, second, dir := openPair(t)
	ctx := context.Background()

	created, err := first.Create(ctx, store.Meta{Title: "orphan"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := first.Append(ctx, created.ID, rig.NewMessage(rig.RoleUser, "写在文件里的")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 手工清空索引，模拟"索引条目丢了但这个会话真的存在"。
	indexPath := filepath.Join(dir, "index.json")
	if err := os.WriteFile(indexPath, []byte(`{"version":1,"sessions":[]}`), 0o600); err != nil {
		t.Fatalf("rewrite index: %v", err)
	}

	// second 的内存索引里没有它；重载后必须从文件收养并正常追加。
	if _, err := second.Append(ctx, created.ID, rig.NewMessage(rig.RoleAssistant, "追加成功")); err != nil {
		t.Fatalf("second.Append on an orphan session: %v", err)
	}
	list, err := second.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("second.List: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID || list[0].Messages != 2 {
		t.Fatalf("second.List after adoption = %+v, want the orphan with 2 messages", list)
	}
}

// 两个实例各自 Create 不同会话：索引是 last-writer-wins，但**不能丢会话** ——
// 另一个实例的 List/Load 必须通过"收养孤儿文件"把它们找回来。
func TestCrossInstanceIndexLastWriterWinsSelfHeals(t *testing.T) {
	first, second, _ := openPair(t)
	ctx := context.Background()

	firstMeta, err := first.Create(ctx, store.Meta{Title: "first-session"})
	if err != nil {
		t.Fatalf("first.Create: %v", err)
	}
	// second 的索引里还是空的；它 Create 时会用自己的内存索引覆盖 index.json。
	secondMeta, err := second.Create(ctx, store.Meta{Title: "second-session"})
	if err != nil {
		t.Fatalf("second.Create: %v", err)
	}

	// 两边都必须最终看到两个会话（谁的索引赢了都行，文件都在）。
	for name, instance := range map[string]*store.Dir{"first": first, "second": second} {
		list, listErr := instance.List(ctx, store.Filter{})
		if listErr != nil {
			t.Fatalf("%s.List: %v", name, listErr)
		}
		seen := map[string]bool{}
		for _, meta := range list {
			seen[meta.ID] = true
		}
		if !seen[firstMeta.ID] || !seen[secondMeta.ID] {
			t.Fatalf("%s.List = %+v, want both sessions (self-heal via orphan adoption)", name, list)
		}
	}

	// 两个会话都必须可读可写。
	if _, _, err := first.Load(ctx, secondMeta.ID); err != nil {
		t.Fatalf("first.Load(second's session): %v", err)
	}
	if _, err := second.Append(ctx, firstMeta.ID, rig.NewMessage(rig.RoleUser, "still writable")); err != nil {
		t.Fatalf("second.Append(first's session): %v", err)
	}
}

// 反复查同一个不存在的会话：重载必须被节流（不能每次 stat/读盘）。
// 观察点是 store.ReloadCountForTest（导出给测试的计数器，见 store.go）。
func TestCrossInstanceRepeatedMissIsThrottled(t *testing.T) {
	first, second, _ := openPair(t)
	ctx := context.Background()
	if _, err := first.Create(ctx, store.Meta{Title: "seed"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for attempt := 0; attempt < 500; attempt++ {
		if _, _, err := second.Load(ctx, "does-not-exist"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("attempt %d: Load error = %v, want ErrNotFound", attempt, err)
		}
	}
	if reloads := store.ReloadCountForTest(second); reloads > 1 {
		t.Fatalf("throttle did not work: %d reloads for 500 misses on the same id", reloads)
	}

	// 不同 id 各自有一次机会（不能被别人的节流饿死）。
	if _, _, err := second.Load(ctx, "another-missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Load(another-missing) = %v, want ErrNotFound", err)
	}
	if reloads := store.ReloadCountForTest(second); reloads != 2 {
		t.Fatalf("reloads = %d, want 2 (one per distinct id)", reloads)
	}
}

// 单实例的常规路径**不允许**触发自愈重载（否则每次 Append 都可能读盘）。
// 只允许一次 stat 级别的"索引戳"检查——reloads 计数器必须保持 0。
func TestSingleInstanceNeverReloads(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	meta := mustCreate(t, s, store.Meta{Title: "solo", Workspace: "/w"})
	mustAppend(t, s, meta.ID, rig.NewMessage(rig.RoleUser, "one"))

	if _, _, err := s.Load(ctx, meta.ID); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := s.List(ctx, store.Filter{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	mustAppend(t, s, meta.ID, rig.NewMessage(rig.RoleAssistant, "two"))
	if err := s.Delete(ctx, meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if reloads := store.ReloadCountForTest(s); reloads != 0 {
		t.Fatalf("single-instance path triggered %d self-heal reloads, want 0", reloads)
	}
}
