package kit_test

// 聚合存储入口：宿主手里已经有一份 storage.Storage（一个库装会话消息/检查点/
// 记忆/溢出）时，不该被迫把它拆成五个字段分别传——那正是"聚合契约存在但用不上"
// 的样子。

import (
	"context"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/memory"
	"github.com/zeokeel/zeokeel-rig/storage"
)

// aggregateWiki 是只为身份断言用的最小 wiki。
type aggregateWiki struct{ name string }

func (w *aggregateWiki) Index(context.Context) ([]memory.PageMeta, error) {
	return []memory.PageMeta{{Name: w.name, Title: w.name}}, nil
}
func (w *aggregateWiki) Read(context.Context, string) (memory.Page, error) {
	return memory.Page{}, memory.ErrNotFound
}
func (w *aggregateWiki) Write(context.Context, memory.Page) error { return nil }
func (w *aggregateWiki) Append(context.Context, string, string) error {
	return nil
}
func (w *aggregateWiki) Delete(context.Context, string) error { return nil }
func (w *aggregateWiki) Search(context.Context, string) ([]memory.PageMeta, error) {
	return nil, nil
}

// fakeAggregate 是 storage.Storage 的最小实现（只填 kit 会读的那几个访问器）。
type fakeAggregate struct {
	wiki   *aggregateWiki
	closed bool
}

func (f *fakeAggregate) History() rig.History               { return nil }
func (f *fakeAggregate) Checkpoints() rig.CheckpointStore   { return nil }
func (f *fakeAggregate) Memory() memory.Wiki                { return f.wiki }
func (f *fakeAggregate) Spill() rig.SpillStore              { return nil }
func (f *fakeAggregate) Close() error                       { f.closed = true; return nil }

var _ storage.Storage = (*fakeAggregate)(nil)

// TestKitAggregateStorageFeedsAccessors：聚合存储的记忆必须就是 memory 工具用的那份。
func TestKitAggregateStorageFeedsAccessors(t *testing.T) {
	aggregate := &fakeAggregate{wiki: &aggregateWiki{name: "from-aggregate"}}
	agent, err := kit.New(kit.Config{Model: "test-model", Root: t.TempDir(), Aggregate: aggregate})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	if got := agent.MemoryWiki(); got != aggregate.wiki {
		t.Fatalf("MemoryWiki 不是聚合存储里的那个（%p vs %p）", got, aggregate.wiki)
	}
	if err := agent.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !aggregate.closed {
		t.Fatal("Shutdown 没有关闭聚合存储")
	}
}

// TestKitExplicitMemoryWinsOverAggregate：显式字段优先，聚合只是默认值来源。
func TestKitExplicitMemoryWinsOverAggregate(t *testing.T) {
	explicit := &aggregateWiki{name: "explicit"}
	aggregate := &fakeAggregate{wiki: &aggregateWiki{name: "from-aggregate"}}
	agent, err := kit.New(kit.Config{
		Model:     "test-model",
		Root:      t.TempDir(),
		Aggregate: aggregate,
		Memory:    explicit,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()
	if got := agent.MemoryWiki(); got != explicit {
		t.Fatalf("显式 Memory 应当优先，实际拿到 %p", got)
	}
	_ = time.Now()
}