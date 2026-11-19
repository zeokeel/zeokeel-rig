package store_test

// DirStorage（目录后端 → storage.Storage 的适配器）的行为测试。
//
// 契约级行为（会话往返、未知会话、列表顺序、删除、关闭语义、并发不丢写、能力位
// 一致性）全部由 storagetest 基线套件覆盖 —— 同一份用例将来跑在 sqlite 上。
// 本文件只补**目录后端特有**的部分：默认会话绑定、两个 History 入口一致、
// 三个"不支持"的访问器、Close 真的转发到了 Dir、以及落盘行数（套件只能断言
// 契约层面的条数，"文件里到底几行"是磁盘布局的事实）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/memory"
	"github.com/zeokeel/zeokeel-rig/session/store"
	"github.com/zeokeel/zeokeel-rig/storage"
	"github.com/zeokeel/zeokeel-rig/storagetest"
)

// dirCapabilities 是目录后端的能力位登记表。
//
// 三个 false 不是"暂时没测"，而是**实现层面没有**：磁盘布局只有一个会话一个
// NDJSON 文件，没有检查点对象、没有记忆 wiki、没有溢出结果。想支持就得换后端
// （session/sqlite），而不是在适配器里伪造。
var dirCapabilities = storagetest.Capabilities{
	"dir": {History: true, Checkpoints: false, Memory: false, Spill: false},
}

// newDirStorageAdapter 造一个绑定到临时目录的适配器。
func newDirStorageAdapter(t *testing.T) (store.DirStorage, *store.Dir, string) {
	t.Helper()
	dir, root := newStore(t)
	return store.NewDirStorage(dir), dir, root
}

// TestDirStorageBaseline 让目录后端跑一遍 storage 契约基线套件。
func TestDirStorageBaseline(t *testing.T) {
	dirCapabilities.Run(t, "dir", func(t *testing.T) storage.Storage {
		adapter, _, _ := newDirStorageAdapter(t)
		return adapter
	})
}

// TestDirStorageUnsupportedCapabilities 守住"不伪造能力"。
//
// 目录后端没有检查点/记忆/溢出。适配器返回的是**诚实的"不支持"桩**：句柄非 nil
// （storage.Storage 的接口文档禁止访问器返回 nil——那会让调用方在离原因很远的
// 地方 panic），但每个方法都返回包装了 ErrUnsupported 的错误，绝不静默成功。
//
// 这条与能力位是一对：能力位说 false，桩就必须报错；哪天有人把桩改成"空实现
// 静默接受"，能力位与行为的对应关系就断了，这里会红。
func TestDirStorageUnsupportedCapabilities(t *testing.T) {
	adapter, _, _ := newDirStorageAdapter(t)
	var s storage.Storage = adapter
	ctx := context.Background()

	caps, ok := dirCapabilities.Lookup("dir")
	if !ok {
		t.Fatal("dir 没有在 dirCapabilities 里登记")
	}
	if caps.Checkpoints || caps.Memory || caps.Spill {
		t.Fatalf("能力位 %+v 与实现的「不支持」不符：能力位必须描述实现，而不是愿望", caps)
	}

	// 三项"不支持"的访问器都必须返回可调用的句柄（非 nil），且每个方法都显式报错。
	assertUnsupported := func(t *testing.T, capName string, calls map[string]func() error) {
		t.Helper()
		for name, call := range calls {
			if err := call(); !errors.Is(err, store.ErrUnsupported) {
				t.Errorf("%s.%s() = %v，want errors.Is(err, store.ErrUnsupported)", capName, name, err)
			}
		}
	}

	if s.Checkpoints() == nil {
		t.Fatal("Checkpoints() 返回 nil：契约要求访问器不返回 nil，应返回不支持的桩")
	}
	assertUnsupported(t, "Checkpoints", map[string]func() error{
		"Load":  func() error { _, err := s.Checkpoints().Load(ctx, "any"); return err },
		"Save":  func() error { return s.Checkpoints().Save(ctx, "any", &rig.Checkpoint{}) },
		"Clear": func() error { return s.Checkpoints().Clear(ctx, "any") },
	})
	if s.Memory() == nil {
		t.Fatal("Memory() 返回 nil：契约要求访问器不返回 nil，应返回不支持的桩")
	}
	assertUnsupported(t, "Memory", map[string]func() error{
		"Index":  func() error { _, err := s.Memory().Index(ctx); return err },
		"Read":   func() error { _, err := s.Memory().Read(ctx, "any"); return err },
		"Write":  func() error { return s.Memory().Write(ctx, memory.Page{Name: "any", Summary: "x"}) },
		"Append": func() error { return s.Memory().Append(ctx, "any", "x") },
		"Delete": func() error { return s.Memory().Delete(ctx, "any") },
		"Search": func() error { _, err := s.Memory().Search(ctx, "x"); return err },
	})
	if s.Spill() == nil {
		t.Fatal("Spill() 返回 nil：契约要求访问器不返回 nil，应返回不支持的桩")
	}
	assertUnsupported(t, "Spill", map[string]func() error{
		"Save": func() error { _, err := s.Spill().Save(ctx, rig.ToolCall{}, "x"); return err },
		"Load": func() error { _, err := s.Spill().Load(ctx, "any"); return err },
		"GC":   func() error { _, err := s.Spill().GC(ctx, 1); return err },
	})
}

// TestDirStorageHistoryBindsDefaultSession 守默认会话绑定。
//
// 契约 History() 没有参数，所以适配器必须自己挑一个默认会话 ID：
// DefaultSessionID。空 sessionID 走默认，非空 sessionID 优先。
func TestDirStorageHistoryBindsDefaultSession(t *testing.T) {
	adapter, dir, _ := newDirStorageAdapter(t)
	history := adapter.History()
	ctx := context.Background()

	if err := history.Append(ctx, "", rig.NewMessage(rig.RoleUser, "绑定到默认会话")); err != nil {
		t.Fatalf("Append(空 sessionID): %v", err)
	}
	// 走具体类型的入口确认落点：默认会话就是 DefaultSessionID。
	_, messages, err := dir.Load(ctx, store.DefaultSessionID)
	if err != nil {
		t.Fatalf("Dir.Load(%q): %v", store.DefaultSessionID, err)
	}
	if len(messages) != 1 || messages[0].Text() != "绑定到默认会话" {
		t.Fatalf("默认会话内容 = %+v，want 1 条「绑定到默认会话」", messages)
	}

	// 显式 sessionID 优先于默认。
	if err := history.Append(ctx, "explicit", rig.NewMessage(rig.RoleUser, "显式会话")); err != nil {
		t.Fatalf("Append(explicit): %v", err)
	}
	if _, messages, err := dir.Load(ctx, "explicit"); err != nil || len(messages) != 1 {
		t.Fatalf("Dir.Load(explicit) = (%d 条, %v)，want (1, nil)", len(messages), err)
	}
	if _, messages, err := dir.Load(ctx, store.DefaultSessionID); err != nil || len(messages) != 1 {
		t.Fatalf("默认会话被显式会话污染：%d 条（err %v），want 1 条", len(messages), err)
	}
}

// TestDirStorageHistoryMatchesDirEntryPoint 守"同一个存储的两个 History 入口语义一致"。
//
// 适配器多出来的入口（无参 History()）与原有的 Dir.History(ctx, id) 必须等价，
// 否则"换接口"会变成"换语义"：同一个会话在两条路径上看到不同的东西。
func TestDirStorageHistoryMatchesDirEntryPoint(t *testing.T) {
	adapter, dir, _ := newDirStorageAdapter(t)
	ctx := context.Background()
	want := []rig.Message{
		rig.NewMessage(rig.RoleUser, "第一条"),
		rig.NewMessage(rig.RoleAssistant, "第二条"),
	}

	if err := adapter.History().Append(ctx, "shared", want...); err != nil {
		t.Fatalf("适配器 Append: %v", err)
	}

	fromAdapter, err := adapter.History().Load(ctx, "shared")
	if err != nil {
		t.Fatalf("适配器 Load: %v", err)
	}
	fromDir, err := dir.History(ctx, store.DefaultSessionID).Load(ctx, "shared")
	if err != nil {
		t.Fatalf("Dir.History Load: %v", err)
	}
	if len(fromAdapter) != len(fromDir) || len(fromAdapter) != len(want) {
		t.Fatalf("两个入口条数不同：适配器 %d 条，Dir.History %d 条，want %d 条",
			len(fromAdapter), len(fromDir), len(want))
	}
	for i := range want {
		if fromAdapter[i].Text() != fromDir[i].Text() {
			t.Fatalf("第 %d 条不同：适配器 %q，Dir.History %q", i, fromAdapter[i].Text(), fromDir[i].Text())
		}
	}
}

// TestDirStorageHistoryHidesConcreteType 守适配器的返回类型。
//
// 如果 History() 直接返回 *store.History，宿主就会写下 `history.(*store.History)`
// 之类的断言，那么"换存储实现"这件事在这些地方立刻崩掉。适配器必须返回未导出的
// 包装，同时不丢能力（Replace 仍可用）。
func TestDirStorageHistoryHidesConcreteType(t *testing.T) {
	adapter, _, _ := newDirStorageAdapter(t)
	history := adapter.History()
	if _, ok := history.(*store.History); ok {
		t.Fatal("History() 返回了 *store.History：适配器必须返回未导出包装，避免宿主断言具体类型")
	}
	replacer, ok := history.(rig.HistoryReplacer)
	if !ok {
		t.Fatal("History() 丢了 rig.HistoryReplacer：上下文压缩的持久化路径不能因为换接口而消失")
	}

	ctx := context.Background()
	if err := history.Append(ctx, "replace-me", rig.NewMessage(rig.RoleUser, "原始")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := replacer.Replace(ctx, "replace-me", []rig.Message{rig.NewMessage(rig.RoleUser, "压缩后")}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	messages, err := history.Load(ctx, "replace-me")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 1 || messages[0].Text() != "压缩后" {
		t.Fatalf("Replace 后 = %+v，want 只剩「压缩后」一条", messages)
	}
}

// TestDirStorageCloseForwardsToDir 守 Close 真的转发到了底层 Dir。
//
// 适配器自己再维护一个 closed 标志是常见的重复状态来源：两个标志不同步，就会出现
// "适配器认为关了、Dir 还在写"或者反过来。这里直接检查底层 Dir 的行为。
func TestDirStorageCloseForwardsToDir(t *testing.T) {
	adapter, dir, _ := newDirStorageAdapter(t)
	ctx := context.Background()

	if err := adapter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 底层 Dir 必须已经关闭（转发，而不是适配器自己记一笔）。
	if _, _, err := dir.Load(ctx, "any"); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("适配器 Close 后 Dir.Load = %v，want store.ErrClosed（Close 必须转发到 Dir）", err)
	}
	// 重复关闭不报错。
	if err := adapter.Close(); err != nil {
		t.Fatalf("第二次 Close = %v，want nil", err)
	}
	// 经由适配器的读写也必须报 ErrClosed。
	if _, err := adapter.History().Load(ctx, "any"); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 History().Load = %v，want store.ErrClosed", err)
	}
}

// TestDirStorageZeroValueReturnsErrors 守零值不 panic。
//
// 适配器是宿主直接接触的类型，零值（或 NewDirStorage(nil)）应该得到一条能读懂的
// 错误，而不是段错误。这也是"关闭后不 panic"同一条底线的延伸。
func TestDirStorageZeroValueReturnsErrors(t *testing.T) {
	for name, adapter := range map[string]store.DirStorage{
		"zero":    {},
		"nil-dir": store.NewDirStorage(nil),
	} {
		t.Run(name, func(t *testing.T) {
			var s storage.Storage = adapter
			if err := s.Close(); err != nil {
				t.Fatalf("Close = %v，want nil（没有资源可关）", err)
			}
			if _, err := s.History().Load(context.Background(), "any"); err == nil {
				t.Fatal("未绑定 Dir 的 History().Load 返回 nil 错误：必须报错，而不是静默返回空历史")
			}
			if err := s.History().Append(context.Background(), "any", rig.NewMessage(rig.RoleUser, "x")); err == nil {
				t.Fatal("未绑定 Dir 的 History().Append 返回 nil 错误")
			}
			if _, err := adapter.ListSessions(context.Background()); err == nil {
				t.Fatal("未绑定 Dir 的 ListSessions 返回 nil 错误")
			}
			if err := adapter.DeleteSession(context.Background(), "any"); err == nil {
				t.Fatal("未绑定 Dir 的 DeleteSession 返回 nil 错误")
			}
		})
	}
}

// TestDirStorageConcurrentAppendLandsEveryLine 是"Append 丢写"的文件级门禁。
//
// 基线套件已经在契约层面断言"条数 == 调用次数"；这里再断言**磁盘上的行数**也是
// 同样多。两条一起才钉死"内存索引说 200 条、文件里只有 150 行"这类悄悄丢数据：
// 重启后索引从文件重建，少掉的那部分就永远消失了。
func TestDirStorageConcurrentAppendLandsEveryLine(t *testing.T) {
	adapter, _, root := newDirStorageAdapter(t)
	history := adapter.History()
	ctx := context.Background()
	const sessionID = "concurrent-lines"

	const (
		goroutines   = 8
		perGoroutine = 25
	)
	var wait sync.WaitGroup
	errs := make(chan error, goroutines)
	for worker := 0; worker < goroutines; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for i := 0; i < perGoroutine; i++ {
				message := rig.NewMessage(rig.RoleUser, fmt.Sprintf("g%02d-%03d", worker, i))
				if err := history.Append(ctx, sessionID, message); err != nil {
					errs <- fmt.Errorf("goroutine %d 第 %d 条: %w", worker, i, err)
					return
				}
			}
		}(worker)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发 Append: %v", err)
	}

	want := goroutines * perGoroutine
	messages, err := history.Load(ctx, sessionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != want {
		t.Fatalf("Load 返回 %d 条，want %d 条", len(messages), want)
	}

	content, err := os.ReadFile(pathOf(root, sessionID))
	if err != nil {
		t.Fatalf("读取会话文件: %v", err)
	}
	if lines := strings.Count(string(content), "\n"); lines != want {
		t.Fatalf("文件里有 %d 行，want %d 行：并发写入丢了 %d 行（索引与文件已经不一致）",
			lines, want, want-lines)
	}
	if !strings.HasSuffix(string(content), "\n") {
		t.Fatal("文件末尾缺少换行：最后一条记录没写完")
	}
}
