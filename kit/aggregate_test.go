package kit_test

// 聚合存储入口：宿主手里已经有一份 storage.Storage（一个库装会话消息/检查点/
// 记忆/溢出）时，不该被迫把它拆成五个字段分别传——那正是"聚合契约存在但用不上"
// 的样子。

import (
	"context"
	"errors"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/memory"
	"github.com/zeokeel/zeokeel-rig/session/store"
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

func (f *fakeAggregate) History() rig.History             { return nil }
func (f *fakeAggregate) Checkpoints() rig.CheckpointStore { return nil }
func (f *fakeAggregate) Memory() memory.Wiki              { return f.wiki }
func (f *fakeAggregate) Spill() rig.SpillStore            { return nil }
func (f *fakeAggregate) Close() error                     { f.closed = true; return nil }

var _ storage.Storage = (*fakeAggregate)(nil)

// TestKitAggregateStorageFeedsAccessors：聚合存储的记忆必须就是 memory 工具用的那份。
func TestKitAggregateStorageFeedsAccessors(t *testing.T) {
	aggregate := &fakeAggregate{wiki: &aggregateWiki{name: "from-aggregate"}}
	agent, err := kit.New(kit.Config{Model: "test-model", Root: t.TempDir(), Aggregate: aggregate})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	if err := agent.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !aggregate.closed {
		t.Fatal("Shutdown 没有关闭聚合存储")
	}
}

// TestKitAggregateSkipsUnsupportedCapabilities 钉住"不支持的能力不要采纳"。
//
// storage.Storage 的契约是"不支持就返回会报 ErrUnsupported 的诚实桩"，而不是返回
// nil。kit 如果只看"非 nil"，把 session/store.DirStorage 传进来就会：检查点保存
// 让每个 Run 立刻失败、memory 工具全动作报错、溢出被一个不支持 Store 的桩占位而
// 不再落盘。正确做法是逐个探测，不支持就跳过（保留 kit 的默认）。
func TestKitAggregateSkipsUnsupportedCapabilities(t *testing.T) {
	dir, err := store.NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	aggregate := store.NewDirStorage(dir)
	agent, err := kit.New(kit.Config{Model: "test-model", Root: t.TempDir(), Aggregate: aggregate})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent.Shutdown(context.Background()) }()

	// 记忆不该是"不支持"的桩：kit 应当保留自己的默认（文件 wiki）或 nil，
	// 而不是把不支持的桩装上。
	if wiki := agent.MemoryWiki(); wiki != nil {
		if _, err := wiki.Index(context.Background()); errors.Is(err, store.ErrUnsupported) {
			t.Fatal("MemoryWiki 采纳了不支持记忆的桩：memory 工具会全动作报错")
		}
	}
	// 检查点：不该因为采纳了不支持的桩而让 Run 失败。
	agent2, err := kit.New(kit.Config{
		Model:     "test-model",
		Root:      t.TempDir(),
		Aggregate: aggregate,
		Provider:  &leadAuditLoopProvider{},
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() { _ = agent2.Shutdown(context.Background()) }()
	if _, _, err := agent2.RunText(context.Background(), "s1", "hello", nil); err != nil {
		t.Fatalf("Run 因为采纳了不支持的检查点桩而失败: %v", err)
	}
}
