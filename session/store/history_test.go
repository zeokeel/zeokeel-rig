package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// History 适配器必须满足 agent 依赖的两份契约。
var (
	_ rig.History         = (*store.History)(nil)
	_ rig.HistoryReplacer = (*store.History)(nil)
	_ fmt.Stringer        = (*store.History)(nil)
)

func TestHistoryAppendLoad(t *testing.T) {
	s, _ := newStore(t)
	history := s.History(context.Background(), "s1")

	if messages, err := history.Load(context.Background(), "s1"); err != nil || messages != nil {
		t.Fatalf("Load(unknown) = (%#v, %v), want (nil, nil)", messages, err)
	}
	if err := history.Append(context.Background(), "s1", rig.NewMessage(rig.RoleUser, "从 History 写入")); err != nil {
		t.Fatalf("History.Append: %v", err)
	}
	messages, err := history.Load(context.Background(), "s1")
	if err != nil {
		t.Fatalf("History.Load: %v", err)
	}
	if len(messages) != 1 || messages[0].Text() != "从 History 写入" {
		t.Fatalf("messages = %#v", messages)
	}

	// 追加必须落盘：重开 store 之后仍能读到。
	dir := s.Dir()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	_, stored, err := reopened.Load(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Load after reopen: %v", err)
	}
	if len(stored) != 1 || stored[0].Text() != "从 History 写入" {
		t.Fatalf("stored = %#v, want the appended message", stored)
	}
}

func TestHistoryRoutesSessionIDs(t *testing.T) {
	s, _ := newStore(t)
	history := s.History(context.Background(), "default")

	// 同一个 History 实例必须按 sessionID 分流（agent 只持有一个 History，
	// 并把 RunRequest.SessionID 作为键传进来）。
	for _, id := range []string{"alpha", "beta", "alpha"} {
		if err := history.Append(context.Background(), id, rig.NewMessage(rig.RoleUser, "msg-"+id)); err != nil {
			t.Fatalf("Append(%s): %v", id, err)
		}
	}
	list, err := s.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %+v, want two sessions (alpha, beta)", list)
	}
	alpha, err := history.Load(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Load(alpha): %v", err)
	}
	if len(alpha) != 2 {
		t.Fatalf("alpha has %d messages, want 2", len(alpha))
	}
	beta, err := history.Load(context.Background(), "beta")
	if err != nil {
		t.Fatalf("Load(beta): %v", err)
	}
	if len(beta) != 1 || beta[0].Text() != "msg-beta" {
		t.Fatalf("beta = %#v", beta)
	}

	// 空 sessionID 回落到构造时给的默认 ID。
	if err := history.Append(context.Background(), "", rig.NewMessage(rig.RoleUser, "default-message")); err != nil {
		t.Fatalf("Append(empty id): %v", err)
	}
	if _, _, err := s.Load(context.Background(), "default"); err != nil {
		t.Fatalf("default session not created: %v", err)
	}
}

func TestHistoryReplace(t *testing.T) {
	s, _ := newStore(t)
	history := s.History(context.Background(), "s1")
	if err := history.Append(context.Background(), "s1",
		rig.NewMessage(rig.RoleUser, "old-1"),
		rig.NewMessage(rig.RoleAssistant, "old-2"),
		rig.NewMessage(rig.RoleUser, "old-3"),
	); err != nil {
		t.Fatalf("Append: %v", err)
	}

	compacted := []rig.Message{
		rig.NewMessage(rig.RoleSystem, "summary"),
		rig.NewMessage(rig.RoleUser, "old-3"),
	}
	if err := history.Replace(context.Background(), "s1", compacted); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	messages, err := history.Load(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Load after Replace: %v", err)
	}
	if !reflect.DeepEqual(messages, compacted) {
		t.Fatalf("messages after Replace = %#v, want %#v", messages, compacted)
	}
	meta, _, err := s.Load(context.Background(), "s1")
	if err != nil {
		t.Fatalf("store.Load after Replace: %v", err)
	}
	if meta.Messages != len(compacted) {
		t.Fatalf("meta.Messages = %d, want %d", meta.Messages, len(compacted))
	}

	// Replace 成空切片等价于清空会话，但会话本身仍然存在。
	if err := history.Replace(context.Background(), "s1", nil); err != nil {
		t.Fatalf("Replace(nil): %v", err)
	}
	messages, err = history.Load(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Load after clearing: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("messages after clearing = %#v, want empty", messages)
	}
	meta, _, err = s.Load(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Load meta after clearing: %v", err)
	}
	if meta.Messages != 0 || meta.Bytes != 0 {
		t.Fatalf("meta after clearing = %+v, want zero counters", meta)
	}
}

func TestHistoryAppendAutoCreatesSessionWithTitle(t *testing.T) {
	s, _ := newStore(t)
	history := s.History(context.Background(), "auto")
	if err := history.Append(context.Background(), "auto", rig.NewMessage(rig.RoleUser, "自动建会话")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	list, err := s.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List = %+v, want one auto-created session", list)
	}
	if list[0].ID != "auto" || list[0].Messages != 1 || list[0].Title != "自动建会话" {
		t.Fatalf("auto-created meta = %+v", list[0])
	}
}

func TestHistoryPropagatesStoreErrors(t *testing.T) {
	dir := t.TempDir()
	s, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	history := s.History(context.Background(), "s1")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err = history.Append(context.Background(), "s1", rig.NewMessage(rig.RoleUser, "x"))
	if !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Append on closed store = %v, want ErrClosed", err)
	}
	if _, loadErr := history.Load(context.Background(), "s1"); !errors.Is(loadErr, store.ErrClosed) {
		t.Fatalf("Load on closed store = %v, want ErrClosed", loadErr)
	}
}

func TestHistoryNestedAppendOrder(t *testing.T) {
	// History.Append 的 variadic 语义必须与 Memory 一致：一次调用是一批。
	s, _ := newStore(t)
	history := s.History(context.Background(), "s1")
	if err := history.Append(context.Background(), "s1",
		rig.NewMessage(rig.RoleUser, "1"),
		rig.NewMessage(rig.RoleAssistant, "2"),
		rig.NewMessage(rig.RoleUser, "3"),
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := history.Append(context.Background(), "s1"); err != nil {
		t.Fatalf("Append(no messages): %v", err)
	}
	messages, err := history.Load(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var texts []string
	for _, msg := range messages {
		texts = append(texts, msg.Text())
	}
	if want := []string{"1", "2", "3"}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("texts = %v, want %v", texts, want)
	}
}

// 让 fmt 在失败信息里可读（避免误报 unused import 的写法也便于将来扩展）。
var _ = fmt.Sprintf
