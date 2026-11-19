package storagetest

import (
	"context"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// runCloseCases 守关闭语义。
//
// 这是"用户看到回答到一半消失"的根因所在：宿主在回合
// 还没收尾时关掉存储，若实现选择"静默成功"，agent 就会把一个写不进去的回合当成
// 正常回合继续跑下去。因此关闭之后**每个方法都必须报错**，而且**不许 panic**——
// panic 会把宿主进程一起带走，比报错严重得多。
func runCloseCases(t *testing.T, caps Capability, newStorage Factory) {
	t.Run("CloseIsIdempotent", func(t *testing.T) {
		s := newStore(t, newStorage) // Cleanup 还会再关一次
		if err := s.Close(); err != nil {
			t.Fatalf("第一次 Close: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("第二次 Close = %v，want nil：Close 必须可重复调用", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("第三次 Close = %v，want nil", err)
		}
	})

	t.Run("ClosedStorageRejectsAccess", func(t *testing.T) {
		s := newStore(t, newStorage)
		ctx := context.Background()
		// history 在关闭前取出来：关闭后仍然拿着它调用，才是最贴近宿主的路径
		// （宿主通常复用同一个 rig.History 句柄）。
		var history rig.History
		if caps.History {
			history = requireHistory(t, s, caps)
			// 关闭前先写一条：证明"这个实现本来是能读写的"，否则关闭后的报错可能
			// 只是因为它从来就不支持。
			if err := history.Append(ctx, sessionA, rig.NewMessage(rig.RoleUser, "关闭前")); err != nil {
				t.Fatalf("关闭前 Append: %v", err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		if history != nil {
			assertClosed(t, "History().Load", func() error {
				_, err := history.Load(ctx, sessionA)
				return err
			})
			assertClosed(t, "History().Append", func() error {
				return history.Append(ctx, sessionA, rig.NewMessage(rig.RoleUser, "关闭后"))
			})
		} else {
			t.Log("skip: backend does not support history（访问器未提供）")
		}
		// 关闭之后重新取访问器也不得 panic：契约方法没有 error 返回值，宿主拿到
		// 句柄后才可能在调用时报错，构造这一步不能崩。
		assertNoPanic(t, "关闭后 History()", func() { _ = s.History() })
		assertNoPanic(t, "关闭后 Checkpoints()", func() { _ = s.Checkpoints() })
		assertNoPanic(t, "关闭后 Memory()", func() { _ = s.Memory() })
		assertNoPanic(t, "关闭后 Spill()", func() { _ = s.Spill() })

		// 三个可选访问器：**不看能力位，看返回了什么**。
		//
		// 契约允许两种"不支持"的表达：返回 nil，或者返回一个每个方法都报错的桩
		// （storage.Storage 的接口文档要求后者——nil 会让调用方在离原因很远的地方
		// panic）。两种情况在本用例里的处理不同：有句柄就必须报错，没有就 skip 并
		// 说明原因。"不支持"桩恰好也会在关闭后报错，所以它自然通过。
		if checkpoints := s.Checkpoints(); checkpoints != nil {
			assertClosed(t, "Checkpoints().Load", func() error {
				_, err := checkpoints.Load(ctx, sessionA)
				return err
			})
			assertClosed(t, "Checkpoints().Save", func() error {
				return checkpoints.Save(ctx, sessionA, &rig.Checkpoint{
					SessionID: sessionA,
					TurnID:    "turn-closed",
					Phase:     rig.CheckpointPhaseRequested,
				})
			})
			assertClosed(t, "Checkpoints().Clear", func() error {
				return checkpoints.Clear(ctx, sessionA)
			})
		} else {
			t.Log("skip: backend does not support checkpoints（访问器返回 nil）")
		}
		if wiki := s.Memory(); wiki != nil {
			assertClosed(t, "Memory().Index", func() error {
				_, err := wiki.Index(ctx)
				return err
			})
			assertClosed(t, "Memory().Read", func() error {
				_, err := wiki.Read(ctx, "any-page")
				return err
			})
		} else {
			t.Log("skip: backend does not support memory（访问器返回 nil）")
		}
		if spill := s.Spill(); spill != nil {
			assertClosed(t, "Spill().Save", func() error {
				_, err := spill.Save(ctx, rig.ToolCall{ID: "c1", Name: "bash"}, "内容")
				return err
			})
			assertClosed(t, "Spill().Load", func() error {
				_, err := spill.Load(ctx, "any-ref")
				return err
			})
		} else {
			t.Log("skip: backend does not support spill（访问器返回 nil）")
		}
	})

	t.Run("CloseDuringWritesDoesNotPanic", func(t *testing.T) {
		if !caps.History {
			skipNoCapability(t, "history")
		}
		s := newStore(t, newStorage)
		history := requireHistory(t, s, caps)
		ctx := context.Background()

		var wait sync.WaitGroup
		for worker := 0; worker < 4; worker++ {
			wait.Add(1)
			go func(worker int) {
				defer wait.Done()
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("写入 goroutine %d panic 了（关闭与读写并发不得 panic）：%v", worker, r)
					}
				}()
				for i := 0; i < 50; i++ {
					// 关闭与写入竞争：成功或失败都可以（取决于时序），但**不许 panic**。
					// 竞态本身由 -race 兜住。
					_ = history.Append(ctx, sessionA, rig.NewMessage(rig.RoleUser, "写入进行中"))
				}
			}(worker)
		}
		// 让写入先跑起来，再在中间关掉。
		for i := 0; i < 5; i++ {
			_ = history.Append(ctx, sessionA, rig.NewMessage(rig.RoleUser, "抢占"))
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		wait.Wait()
	})
}

// assertClosed 断言"已关闭"后的调用返回明确错误，而不是 panic 或静默成功。
//
// 套件**不**要求具体哨兵错误：storage 层没有定义 ErrClosed，各实现的写法不同
// （store.ErrClosed / sqlite.ErrClosed …）。这里只守两条底线：
//
//  1. 不许 panic；
//  2. 必须返回非 nil 错误——nil 等于告诉调用方"写成功了"，那是数据丢失。
func assertClosed(t *testing.T, name string, call func() error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("关闭后调用 %s panic 了：%v（必须返回明确错误）", name, r)
		}
	}()
	if err := call(); err == nil {
		t.Errorf("关闭后调用 %s 返回 nil 错误：关闭必须让所有调用显式失败，"+
			"否则宿主会把「写不进去」当成「写成功了」", name)
	}
}

// assertNoPanic 断言调用不 panic。用于契约里没有 error 返回值的访问器。
func assertNoPanic(t *testing.T, name string, call func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("调用 %s panic 了：%v", name, r)
		}
	}()
	call()
}
