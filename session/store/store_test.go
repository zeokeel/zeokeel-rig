package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// Dir 必须满足冻结的 Store 接口。
var _ store.Store = (*store.Dir)(nil)

// fakeClock 让"创建时间/更新时间"在测试里完全确定，避免依赖真实时钟的粒度
// （Windows 上 SystemTime 粒度较粗会让"追加后更新"与"新建"落在同一刻）。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newStore 在真实临时目录上建一个 store，并在测试结束时关闭。
func newStore(t *testing.T, opts ...store.Option) (*store.Dir, string) {
	t.Helper()
	dir := t.TempDir()
	opened, err := store.NewDir(dir, opts...)
	if err != nil {
		t.Fatalf("NewDir(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened, dir
}

func mustCreate(t *testing.T, s *store.Dir, meta store.Meta) store.Meta {
	t.Helper()
	created, err := s.Create(context.Background(), meta)
	if err != nil {
		t.Fatalf("Create(%+v): %v", meta, err)
	}
	return created
}

func mustAppend(t *testing.T, s *store.Dir, id string, msgs ...rig.Message) store.Meta {
	t.Helper()
	updated, err := s.AppendBatch(context.Background(), id, msgs...)
	if err != nil {
		t.Fatalf("AppendBatch(%q): %v", id, err)
	}
	return updated
}

// pathOf 返回规范里约定的会话文件路径。
func pathOf(dir, id string) string {
	return filepath.Join(dir, "sessions", id+".ndjson")
}

func TestCreateAppendLoadRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	created := mustCreate(t, s, store.Meta{Title: "往返", Workspace: "/w", Model: "m1"})
	if created.ID == "" {
		t.Fatal("Create returned an empty ID")
	}
	if created.Messages != 0 || created.Bytes != 0 {
		t.Fatalf("fresh meta = %+v, want zero counters", created)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("fresh meta = %+v, want timestamps", created)
	}

	// 覆盖所有字段：content parts、tool_calls、reasoning、tool_call_id。
	messages := []rig.Message{
		{Role: rig.RoleSystem, Content: []rig.ContentPart{rig.TextPart("you are rig")}},
		{Role: rig.RoleUser, Content: []rig.ContentPart{rig.TextPart("看图"), rig.ImagePart("https://x/y.png", "high")}},
		{
			Role:      rig.RoleAssistant,
			Content:   []rig.ContentPart{rig.TextPart("我来调用工具")},
			Reasoning: "先查文件",
			ToolCalls: []rig.ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"a.txt"}`}},
		},
		{Role: rig.RoleTool, Content: []rig.ContentPart{rig.TextPart("内容")}, ToolCallID: "c1", Name: "read_file"},
	}
	updated := mustAppend(t, s, created.ID, messages...)
	if updated.Messages != len(messages) {
		t.Fatalf("Messages = %d, want %d", updated.Messages, len(messages))
	}
	if updated.Bytes <= 0 {
		t.Fatalf("Bytes = %d, want > 0", updated.Bytes)
	}
	if !updated.UpdatedAt.After(created.CreatedAt) && !updated.UpdatedAt.Equal(created.CreatedAt) {
		t.Fatalf("UpdatedAt = %v, want >= CreatedAt %v", updated.UpdatedAt, created.CreatedAt)
	}

	meta, loaded, err := s.Load(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta.ID != created.ID || meta.Title != "往返" || meta.Workspace != "/w" || meta.Model != "m1" {
		t.Fatalf("loaded meta = %+v", meta)
	}
	if meta.Messages != len(messages) {
		t.Fatalf("loaded Messages = %d, want %d", meta.Messages, len(messages))
	}
	if !reflect.DeepEqual(loaded, messages) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", loaded, messages)
	}

	// Load 返回副本：修改结果不得影响存储。
	loaded[0].Content[0].Text = "changed"
	_, again, err := s.Load(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Load again: %v", err)
	}
	if again[0].Content[0].Text != "you are rig" {
		t.Fatalf("stored message was mutated through Load result: %q", again[0].Content[0].Text)
	}
}

func TestLoadUnknownSessionReturnsErrNotFound(t *testing.T) {
	s, _ := newStore(t)
	if _, _, err := s.Load(context.Background(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Load(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := s.Append(context.Background(), "missing", rig.NewMessage(rig.RoleUser, "x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Append(missing) error = %v, want ErrNotFound", err)
	}
}

func TestListFilterAndOrder(t *testing.T) {
	clock := newFakeClock()
	s, _ := newStore(t, store.WithClock(clock.Now))

	first := mustCreate(t, s, store.Meta{Workspace: "/w1", Title: "a"})
	clock.Advance(time.Minute)
	second := mustCreate(t, s, store.Meta{Workspace: "/w1", Title: "b"})
	clock.Advance(time.Minute)
	third := mustCreate(t, s, store.Meta{Workspace: "/w2", Title: "c"})
	clock.Advance(time.Minute)
	// 追加让 first 成为最新（UpdatedAt = T0+3m），List 必须按 UpdatedAt 倒序。
	mustAppend(t, s, first.ID, rig.NewMessage(rig.RoleUser, "hi"))

	all, err := s.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := ids(all), []string{first.ID, third.ID, second.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List order = %v, want %v", got, want)
	}

	inW1, err := s.List(context.Background(), store.Filter{Workspace: "/w1"})
	if err != nil {
		t.Fatalf("List(workspace): %v", err)
	}
	if got, want := ids(inW1), []string{first.ID, second.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("workspace filter = %v, want %v", got, want)
	}

	since, err := s.List(context.Background(), store.Filter{Since: clock.Now().Add(-90 * time.Second)})
	if err != nil {
		t.Fatalf("List(since): %v", err)
	}
	if got, want := ids(since), []string{first.ID, third.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("since filter = %v, want %v", got, want)
	}

	limited, err := s.List(context.Background(), store.Filter{Limit: 2})
	if err != nil {
		t.Fatalf("List(limit): %v", err)
	}
	if got, want := ids(limited), []string{first.ID, third.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("limit = %v, want %v", got, want)
	}

	none, err := s.List(context.Background(), store.Filter{Workspace: "/nope"})
	if err != nil {
		t.Fatalf("List(no match): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("List(no match) = %v, want empty", ids(none))
	}
}

func TestReopenKeepsDataAndIndex(t *testing.T) {
	dir := t.TempDir()
	first, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meta := mustCreate(t, first, store.Meta{Title: "重启", Workspace: "/w", Model: "m"})
	messages := []rig.Message{
		rig.NewMessage(rig.RoleUser, "第一条"),
		rig.NewMessage(rig.RoleAssistant, "第二条"),
	}
	mustAppend(t, first, meta.ID, messages...)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("reopen NewDir: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	list, err := reopened.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	if len(list) != 1 || list[0].Messages != len(messages) || list[0].Title != "重启" {
		t.Fatalf("list after reopen = %+v", list)
	}
	gotMeta, gotMessages, err := reopened.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load after reopen: %v", err)
	}
	if gotMeta.Messages != len(messages) || !reflect.DeepEqual(gotMessages, messages) {
		t.Fatalf("messages after reopen = %#v (meta %+v)", gotMessages, gotMeta)
	}
	// 时间戳必须存活（index.json 持久化，而不是重新生成）。
	if !gotMeta.CreatedAt.Equal(meta.CreatedAt) {
		t.Fatalf("CreatedAt = %v, want %v", gotMeta.CreatedAt, meta.CreatedAt)
	}
}

// 崩溃模拟：最后一行只写了一半（没有结尾换行）。Load 必须成功，且条数只算完整的行。
func TestPartialTailLineIsIgnored(t *testing.T) {
	s, dir := newStore(t)
	meta := mustCreate(t, s, store.Meta{})
	complete := []rig.Message{
		rig.NewMessage(rig.RoleUser, "完整一"),
		rig.NewMessage(rig.RoleAssistant, "完整二"),
	}
	mustAppend(t, s, meta.ID, complete...)

	path := pathOf(dir, meta.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	partial := `{"role":"user","content":[{"type":"text","text":"半"`
	if err := os.WriteFile(path, append(before, partial...), 0o600); err != nil {
		t.Fatalf("write partial tail: %v", err)
	}

	loadedMeta, messages, err := s.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load with partial tail: %v", err)
	}
	if len(messages) != len(complete) {
		t.Fatalf("loaded %d messages, want %d (partial tail must be dropped)", len(messages), len(complete))
	}
	if loadedMeta.Messages != len(complete) {
		t.Fatalf("Meta.Messages = %d, want %d", loadedMeta.Messages, len(complete))
	}

	// List 也必须反映真实条数（通过文件大小与索引的差异触发校正）。
	list, err := s.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Messages != len(complete) {
		t.Fatalf("List after partial tail = %+v", list)
	}

	// 校正后的索引必须被持久化：重开 store 仍是真实条数。
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	list, err = reopened.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	if len(list) != 1 || list[0].Messages != len(complete) {
		t.Fatalf("List after reopen = %+v, want %d messages", list, len(complete))
	}
}

// 崩溃之后继续追加：半行必须先被裁掉，否则新消息会被粘到坏行后面。
func TestAppendAfterPartialTailTruncatesIt(t *testing.T) {
	s, dir := newStore(t)
	meta := mustCreate(t, s, store.Meta{})
	mustAppend(t, s, meta.ID, rig.NewMessage(rig.RoleUser, "one"))
	path := pathOf(dir, meta.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(path, append(before, `{"role":"user","cont`...), 0o600); err != nil {
		t.Fatalf("write partial: %v", err)
	}

	mustAppend(t, s, meta.ID, rig.NewMessage(rig.RoleAssistant, "two"))
	_, messages, err := s.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2: %#v", len(messages), messages)
	}
	if messages[1].Role != rig.RoleAssistant || messages[1].Text() != "two" {
		t.Fatalf("second message = %#v, want the freshly appended one", messages[1])
	}
}

func TestCorruptMiddleLineSkippedWithNotice(t *testing.T) {
	var notices []string
	var noticeMu sync.Mutex
	s, dir := newStore(t, store.WithNotice(func(format string, args ...any) {
		noticeMu.Lock()
		defer noticeMu.Unlock()
		notices = append(notices, fmt.Sprintf(format, args...))
	}))
	meta := mustCreate(t, s, store.Meta{})
	mustAppend(t, s,
		meta.ID,
		rig.NewMessage(rig.RoleUser, "keep-1"),
		rig.NewMessage(rig.RoleUser, "broken"),
		rig.NewMessage(rig.RoleUser, "keep-3"),
	)

	// 把中间那行换成非法 JSON（保持换行结构）。
	path := pathOf(dir, meta.ID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	if len(lines) < 3 {
		t.Fatalf("unexpected file shape: %q", raw)
	}
	lines[1] = "{ this is not json }\n"
	if err := os.WriteFile(path, []byte(strings.Join(lines[:len(lines)-1], "")), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	_, messages, err := s.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load with one corrupt line: %v", err)
	}
	if len(messages) != 2 || messages[0].Text() != "keep-1" || messages[1].Text() != "keep-3" {
		t.Fatalf("messages = %#v, want keep-1/keep-3", messages)
	}
	noticeMu.Lock()
	defer noticeMu.Unlock()
	if len(notices) == 0 {
		t.Fatal("Notice was never called for the corrupt line")
	}
	if !strings.Contains(strings.Join(notices, "\n"), "corrupt") && !strings.Contains(strings.Join(notices, "\n"), "跳过") {
		t.Fatalf("notices = %q, want a corruption notice", notices)
	}
}

func TestAllLinesCorruptReturnsErrCorrupt(t *testing.T) {
	s, dir := newStore(t)
	meta := mustCreate(t, s, store.Meta{})
	mustAppend(t, s, meta.ID, rig.NewMessage(rig.RoleUser, "one"))
	if err := os.WriteFile(pathOf(dir, meta.ID), []byte("{}\n{oops\n[1,2,3]\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Load(context.Background(), meta.ID); !errors.Is(err, store.ErrCorrupt) {
		t.Fatalf("Load(all corrupt) error = %v, want ErrCorrupt", err)
	}
}

func TestConcurrentAppendsSameSession(t *testing.T) {
	s, _ := newStore(t)
	meta := mustCreate(t, s, store.Meta{})

	const goroutines, perGoroutine = 20, 50
	var wait sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wait.Add(1)
		go func(g int) {
			defer wait.Done()
			for i := 0; i < perGoroutine; i++ {
				msg := rig.NewMessage(rig.RoleUser, fmt.Sprintf("g%02d-%03d", g, i))
				msg.Name = fmt.Sprintf("g%02d", g)
				if _, err := s.Append(context.Background(), meta.ID, msg); err != nil {
					errs <- fmt.Errorf("goroutine %d append %d: %w", g, i, err)
					return
				}
			}
		}(g)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}

	load := func() []rig.Message {
		t.Helper()
		_, messages, err := s.Load(context.Background(), meta.ID)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return messages
	}
	messages := load()
	if len(messages) != goroutines*perGoroutine {
		t.Fatalf("loaded %d messages, want %d", len(messages), goroutines*perGoroutine)
	}
	// 每个 goroutine 自己的消息必须保序（不能交错成乱序）。
	next := make(map[string]int, goroutines)
	for _, msg := range messages {
		want := fmt.Sprintf("%s-%03d", msg.Name, next[msg.Name])
		if msg.Text() != want {
			t.Fatalf("out of order for %s: got %q, want %q", msg.Name, msg.Text(), want)
		}
		next[msg.Name]++
	}
	for g := 0; g < goroutines; g++ {
		if next[fmt.Sprintf("g%02d", g)] != perGoroutine {
			t.Fatalf("goroutine g%d has %d messages, want %d", g, next[fmt.Sprintf("g%02d", g)], perGoroutine)
		}
	}
	// 两次读取顺序必须一致（文件顺序即顺序）。
	if second := load(); !reflect.DeepEqual(messages, second) {
		t.Fatal("two consecutive Loads returned different orders")
	}
}

func TestConcurrentCreateAndAppendKeepsIndexConsistent(t *testing.T) {
	s, _ := newStore(t)
	const sessions = 12
	var wait sync.WaitGroup
	ids := make([]string, sessions)
	for i := 0; i < sessions; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			created, err := s.Create(context.Background(), store.Meta{Workspace: fmt.Sprintf("/w%d", i)})
			if err != nil {
				t.Errorf("Create %d: %v", i, err)
				return
			}
			ids[i] = created.ID
			if _, err := s.AppendBatch(context.Background(), created.ID,
				rig.NewMessage(rig.RoleUser, "a"),
				rig.NewMessage(rig.RoleAssistant, "b"),
			); err != nil {
				t.Errorf("Append %d: %v", i, err)
			}
		}(i)
	}
	wait.Wait()

	list, err := s.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != sessions {
		t.Fatalf("List returned %d sessions, want %d", len(list), sessions)
	}
	for _, meta := range list {
		if meta.Messages != 2 {
			t.Fatalf("session %s Messages = %d, want 2", meta.ID, meta.Messages)
		}
	}
	seen := make(map[string]bool, sessions)
	for _, id := range ids {
		if id == "" || seen[id] {
			t.Fatalf("ids = %v, want %d distinct non-empty", ids, sessions)
		}
		seen[id] = true
	}
}

func TestIndexRebuildFromSessionFiles(t *testing.T) {
	dir := t.TempDir()
	first, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meta := mustCreate(t, first, store.Meta{Title: "", Workspace: "/w", Model: "m"})
	mustAppend(t, first, meta.ID,
		rig.NewMessage(rig.RoleUser, "重建索引时的首条用户消息"),
		rig.NewMessage(rig.RoleAssistant, "回答"),
	)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "index.json")); err != nil {
		t.Fatalf("remove index: %v", err)
	}

	rebuilt, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir after index removal: %v", err)
	}
	defer func() { _ = rebuilt.Close() }()
	list, err := rebuilt.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List after rebuild: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("rebuilt index = %+v, want 1 session", list)
	}
	if list[0].ID != meta.ID || list[0].Messages != 2 {
		t.Fatalf("rebuilt meta = %+v, want id %s with 2 messages", list[0], meta.ID)
	}
	if list[0].Title == "" {
		t.Fatal("rebuilt Title is empty, want the first user message summary")
	}
	_, messages, err := rebuilt.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load after rebuild: %v", err)
	}
	if len(messages) != 2 || messages[0].Text() != "重建索引时的首条用户消息" {
		t.Fatalf("messages after rebuild = %#v", messages)
	}
}

func TestMessageTooLargeIsRejected(t *testing.T) {
	s, _ := newStore(t)
	meta := mustCreate(t, s, store.Meta{})
	mustAppend(t, s, meta.ID, rig.NewMessage(rig.RoleUser, "small"))

	huge := rig.NewMessage(rig.RoleUser, strings.Repeat("x", store.MaxMessageBytes+1))
	if _, err := s.Append(context.Background(), meta.ID, huge); !errors.Is(err, store.ErrMessageTooLarge) {
		t.Fatalf("Append(huge) error = %v, want ErrMessageTooLarge", err)
	}
	// 批量里只要有一条超限，整批都不落盘。
	if _, err := s.AppendBatch(context.Background(), meta.ID,
		rig.NewMessage(rig.RoleUser, "ok"),
		huge,
	); !errors.Is(err, store.ErrMessageTooLarge) {
		t.Fatalf("AppendBatch(huge) error = %v, want ErrMessageTooLarge", err)
	}
	_, messages, err := s.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 1 || messages[0].Text() != "small" {
		t.Fatalf("messages after rejected appends = %#v, want only the small one", messages)
	}
}

func TestDeleteRemovesSessionAndFile(t *testing.T) {
	s, dir := newStore(t)
	meta := mustCreate(t, s, store.Meta{})
	mustAppend(t, s, meta.ID, rig.NewMessage(rig.RoleUser, "bye"))
	if err := s.Delete(context.Background(), meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list, err := s.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List after Delete = %+v, want empty", list)
	}
	if _, _, err := s.Load(context.Background(), meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Load after Delete = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(pathOf(dir, meta.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session file still exists (err=%v)", err)
	}
	if err := s.Delete(context.Background(), meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestCreateGeneratesSortableIDs(t *testing.T) {
	s, _ := newStore(t)
	pattern := regexp.MustCompile(`^(\d{8}T\d{6})-[0-9a-f]{8}$`)
	seen := make(map[string]bool)
	var previousStamp string
	for i := 0; i < 20; i++ {
		meta := mustCreate(t, s, store.Meta{})
		match := pattern.FindStringSubmatch(meta.ID)
		if match == nil {
			t.Fatalf("ID = %q, want 20261117T201500-<8 hex>", meta.ID)
		}
		if strings.ContainsAny(meta.ID, `/\`) {
			t.Fatalf("ID %q contains a path separator", meta.ID)
		}
		if seen[meta.ID] {
			t.Fatalf("duplicate ID %q", meta.ID)
		}
		seen[meta.ID] = true
		// 时间戳前缀必须单调不减（同一秒内用随机后缀打破平局，此时不保证字典序，
		// List 的顺序由 UpdatedAt 决定，不依赖 ID）。
		if previousStamp != "" && match[1] < previousStamp {
			t.Fatalf("ID timestamp prefix went backwards: %q after %q", match[1], previousStamp)
		}
		previousStamp = match[1]
	}
}

func TestCreateWithExplicitIDAndDuplicate(t *testing.T) {
	s, _ := newStore(t)
	first := mustCreate(t, s, store.Meta{ID: "custom-id"})
	if first.ID != "custom-id" {
		t.Fatalf("ID = %q, want custom-id", first.ID)
	}
	if _, err := s.Create(context.Background(), store.Meta{ID: "custom-id"}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate Create error = %v, want ErrExists", err)
	}
	// 路径穿越形式的 ID 必须被拒绝，且不能在磁盘上留下任何文件。
	if _, err := s.Create(context.Background(), store.Meta{ID: "../escape"}); err == nil {
		t.Fatal("Create(../escape) succeeded, want an error")
	}
}

func TestTitleSummarisesFirstUserMessage(t *testing.T) {
	clock := newFakeClock()
	s, _ := newStore(t, store.WithClock(clock.Now))
	meta := mustCreate(t, s, store.Meta{})

	long := strings.Repeat("很长的中文标题", 40) // 280 个 rune
	updated := mustAppend(t, s, meta.ID,
		rig.NewMessage(rig.RoleSystem, "system 消息不参与标题"),
		rig.NewMessage(rig.RoleUser, long),
	)
	if updated.Title == "" {
		t.Fatal("Title is empty, want a summary of the first user message")
	}
	if runes := []rune(updated.Title); len(runes) > 120 {
		t.Fatalf("Title has %d runes, want <= 120", len(runes))
	}
	if !strings.HasPrefix(updated.Title, "很长的中文标题") {
		t.Fatalf("Title = %q, want the user message prefix", updated.Title)
	}

	// 后续用户消息不得覆盖已有标题。
	again := mustAppend(t, s, meta.ID, rig.NewMessage(rig.RoleUser, "第二条用户消息"))
	if again.Title != updated.Title {
		t.Fatalf("Title changed to %q, want %q", again.Title, updated.Title)
	}

	// 空白折叠。
	other := mustCreate(t, s, store.Meta{})
	collapsed := mustAppend(t, s, other.ID, rig.NewMessage(rig.RoleUser, "  多行\n\n标题   "))
	if collapsed.Title != "多行 标题" {
		t.Fatalf("Title = %q, want %q", collapsed.Title, "多行 标题")
	}
}

func TestClosedStoreRejectsEveryOperation(t *testing.T) {
	dir := t.TempDir()
	s, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meta := mustCreate(t, s, store.Meta{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	ctx := context.Background()
	if _, err := s.Create(ctx, store.Meta{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Create after Close = %v, want ErrClosed", err)
	}
	if _, err := s.Append(ctx, meta.ID, rig.NewMessage(rig.RoleUser, "x")); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Append after Close = %v, want ErrClosed", err)
	}
	if _, _, err := s.Load(ctx, meta.ID); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Load after Close = %v, want ErrClosed", err)
	}
	if _, err := s.List(ctx, store.Filter{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("List after Close = %v, want ErrClosed", err)
	}
	if err := s.Delete(ctx, meta.ID); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Delete after Close = %v, want ErrClosed", err)
	}
}

func TestCancelledContextIsReported(t *testing.T) {
	s, _ := newStore(t)
	meta := mustCreate(t, s, store.Meta{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Load(ctx, meta.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load(cancelled) error = %v, want context.Canceled", err)
	}
	if _, err := s.Append(ctx, meta.ID, rig.NewMessage(rig.RoleUser, "x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Append(cancelled) error = %v, want context.Canceled", err)
	}
}

// ids 提取一页 Meta 的 ID 序列，便于断言顺序。
func ids(metas []store.Meta) []string {
	out := make([]string, len(metas))
	for i, meta := range metas {
		out[i] = meta.ID
	}
	return out
}

// 逐行解析会话文件，确认没有交错写入产生的坏行（并发测试的辅助断言）。
func TestSessionFileHasOneRecordPerLine(t *testing.T) {
	s, dir := newStore(t)
	meta := mustCreate(t, s, store.Meta{})
	var wait sync.WaitGroup
	for g := 0; g < 8; g++ {
		wait.Add(1)
		go func(g int) {
			defer wait.Done()
			for i := 0; i < 10; i++ {
				if _, err := s.Append(context.Background(), meta.ID, rig.NewMessage(rig.RoleUser, fmt.Sprintf("%d-%d", g, i))); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(g)
	}
	wait.Wait()

	raw, err := os.ReadFile(pathOf(dir, meta.ID))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("file does not end with a newline: %q", raw)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 80 {
		t.Fatalf("file has %d lines, want 80", len(lines))
	}
	for i, line := range lines {
		var msg rig.Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("line %d is not a rig.Message: %v (%q)", i+1, err, line)
		}
		if msg.Text() == "" {
			t.Fatalf("line %d has empty text: %q", i+1, line)
		}
	}
}

func TestCorruptIndexIsRebuiltWithNotice(t *testing.T) {
	var notices []string
	var noticeMu sync.Mutex
	dir := t.TempDir()
	first, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meta := mustCreate(t, first, store.Meta{})
	mustAppend(t, first, meta.ID, rig.NewMessage(rig.RoleUser, "活着"))
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{ not an index"), 0o600); err != nil {
		t.Fatalf("corrupt index: %v", err)
	}

	rebuilt, err := store.NewDir(dir, store.WithNotice(func(format string, args ...any) {
		noticeMu.Lock()
		defer noticeMu.Unlock()
		notices = append(notices, fmt.Sprintf(format, args...))
	}))
	if err != nil {
		t.Fatalf("NewDir with corrupt index: %v", err)
	}
	defer func() { _ = rebuilt.Close() }()
	list, err := rebuilt.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != meta.ID || list[0].Messages != 1 {
		t.Fatalf("rebuilt list = %+v", list)
	}
	noticeMu.Lock()
	defer noticeMu.Unlock()
	if len(notices) == 0 {
		t.Fatal("Notice was never called when rebuilding a corrupt index")
	}
}

func TestGhostIndexEntryIsDropped(t *testing.T) {
	s, dir := newStore(t)
	meta := mustCreate(t, s, store.Meta{})
	mustAppend(t, s, meta.ID, rig.NewMessage(rig.RoleUser, "x"))
	// 模拟"文件被外部删除 / Delete 中途崩溃"：索引还在，数据没了。
	if err := os.Remove(pathOf(dir, meta.ID)); err != nil {
		t.Fatalf("remove session file: %v", err)
	}
	list, err := s.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List = %+v, want the ghost entry dropped", list)
	}
	if _, _, err := s.Load(context.Background(), meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Load = %v, want ErrNotFound", err)
	}
}

// 非 ASCII / 带空格的 ID 走哈希文件名，仍然要能完整往返。
func TestUnusualIDUsesHashedFileName(t *testing.T) {
	s, dir := newStore(t)
	meta := mustCreate(t, s, store.Meta{ID: "会话 1:测试"})
	mustAppend(t, s, meta.ID, rig.NewMessage(rig.RoleUser, "哈希文件名"))

	entries, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("sessions dir has %d entries, want 1", len(entries))
	}
	name := entries[0].Name()
	if strings.ContainsAny(name, ` :`) {
		t.Fatalf("file name %q contains unsafe characters", name)
	}
	if !strings.HasPrefix(name, "id-") {
		t.Fatalf("file name %q, want the hashed form id-<hex>.ndjson", name)
	}
	gotMeta, messages, err := s.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotMeta.ID != meta.ID || len(messages) != 1 || messages[0].Text() != "哈希文件名" {
		t.Fatalf("round trip = (%+v, %#v)", gotMeta, messages)
	}
}

// 崩溃在"数据已落盘、索引未更新"之间：索引偏小，但 Load/List 必须自愈到真实条数，
// 并把校正结果写回索引（再开一次也不需要重新扫文件）。
func TestCrashBetweenFileAndIndexIsHealed(t *testing.T) {
	dir := t.TempDir()
	writer, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meta := mustCreate(t, writer, store.Meta{})
	mustAppend(t, writer, meta.ID, rig.NewMessage(rig.RoleUser, "已计入索引"))
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	line, err := json.Marshal(rig.NewMessage(rig.RoleUser, "盘上多出来的那条"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	file, err := os.OpenFile(pathOf(dir, meta.ID), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open session file: %v", err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		t.Fatalf("append line: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	gotMeta, messages, err := reopened.Load(context.Background(), meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 || gotMeta.Messages != 2 {
		t.Fatalf("Load = %d messages (meta %d), want 2", len(messages), gotMeta.Messages)
	}
	list, err := reopened.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Messages != 2 {
		t.Fatalf("List = %+v, want Messages=2 after self-heal", list)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 校正已写回索引：第三次打开时索引本身就该是 2。
	third, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("reopen again: %v", err)
	}
	defer func() { _ = third.Close() }()
	list, err = third.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List again: %v", err)
	}
	if len(list) != 1 || list[0].Messages != 2 {
		t.Fatalf("List after persisted correction = %+v", list)
	}
}
