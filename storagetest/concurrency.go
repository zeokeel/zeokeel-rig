package storagetest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// runConcurrencyCases 是套件里最值钱的一条：并发追加不许丢写。
//
// 它对应的是一类真实缺陷——分片锁用错（读在锁外）、"读-改-写"没用同一把锁、
// 或者 Append 直接覆盖整个窗口。这类 bug 在单线程用例里完全看不见：单条往返、
// 顺序、未知会话全都绿，只有"8 个 goroutine 同时写同一个会话"才会暴露。
//
// 断言的是**条数相等且每条恰好出现一次**，而不是"条数够多"：丢一条同时重复
// 一条也能凑出相同条数，那同样是数据损坏。
func runConcurrencyCases(t *testing.T, caps Capability, newStorage Factory) {
	t.Run("ConcurrentAppendKeepsEveryMessage", func(t *testing.T) {
		if !caps.History {
			skipNoCapability(t, "history")
		}
		s := newStore(t, newStorage)
		history := requireHistory(t, s, caps)
		ctx := context.Background()

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
					message.Name = fmt.Sprintf("g%02d", worker)
					if err := history.Append(ctx, sessionA, message); err != nil {
						errs <- fmt.Errorf("goroutine %d 第 %d 条: %w", worker, i, err)
						return
					}
				}
			}(worker)
		}
		wait.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("并发 Append 报错: %v", err)
		}

		messages, err := history.Load(ctx, sessionA)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := goroutines * perGoroutine
		if len(messages) != want {
			t.Fatalf("并发追加后读到 %d 条，want %d 条：有写入被丢了（这正是 Append 丢写那类 bug）",
				len(messages), want)
		}

		// 每条恰好出现一次，且同一 goroutine 内部的顺序保持不变。
		seen := make(map[string]int, want)
		next := make(map[string]int, goroutines)
		for _, message := range messages {
			text := message.Text()
			seen[text]++
			wantText := fmt.Sprintf("%s-%03d", message.Name, next[message.Name])
			if text != wantText {
				t.Fatalf("%s 的消息乱序或丢失：读到 %q，按顺序应是 %q", message.Name, text, wantText)
			}
			next[message.Name]++
		}
		for text, count := range seen {
			if count != 1 {
				t.Fatalf("消息 %q 出现了 %d 次：并发写不能重复", text, count)
			}
		}
		for worker := 0; worker < goroutines; worker++ {
			name := fmt.Sprintf("g%02d", worker)
			if next[name] != perGoroutine {
				t.Fatalf("%s 只读到 %d 条，want %d 条", name, next[name], perGoroutine)
			}
		}
	})
}
