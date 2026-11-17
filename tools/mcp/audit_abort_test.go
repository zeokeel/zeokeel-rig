package mcp

// 审计回归（F2）：传输被 abort 之后，后续请求必须返回**可识别错误**，不能 panic。
//
// 历史：abort() 把 t.pending 置为 nil 以释放等待者，但只设置 abortErr、不设置
// closed；Request() 在锁内只检查 closed，随后 `t.pending[msg.ID] = response` 直接
// 写 nil map → panic: assignment to entry in nil map。服务端崩溃（本包的 "crash"
// 场景）就能触发：进程退出 → abort → 下一次任意请求 panic。
//
// 这些用例用 recover 把 panic 转成可读的测试失败（否则测试进程直接崩）。

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestRequestAfterAbortReturnsError 证明服务端崩溃后，Ping 返回错误而不是 panic。
func TestRequestAfterAbortReturnsError(t *testing.T) {
	client := startClient(t, "default", nil)
	if _, err := client.CallTool(context.Background(), "crash", nil); err == nil {
		t.Fatal("CallTool(crash) returned no error; the fake server should have exited")
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		err := client.Ping(context.Background())
		if err == nil {
			t.Fatal("Ping after a server crash returned nil error")
		}
	}()
	if recovered != nil {
		t.Fatalf("request after transport abort panicked: %v (want a returned error)", recovered)
	}
}

// TestConcurrentRequestsAfterAbortAllFail 证明 abort 之后并发 10 个请求全部返回
// 错误、没有一个 panic（原来的实现会在 nil map 上 panic）。
func TestConcurrentRequestsAfterAbortAllFail(t *testing.T) {
	client := startClient(t, "default", nil)
	if _, err := client.CallTool(context.Background(), "crash", nil); err == nil {
		t.Fatal("CallTool(crash) returned no error; the fake server should have exited")
	}

	const workers = 10
	var wait sync.WaitGroup
	errs := make([]error, workers)
	panics := make([]any, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			defer func() { panics[slot] = recover() }()
			// 交错调用不同方法，覆盖 Request 的两个入口参数形状。
			if slot%2 == 0 {
				errs[slot] = client.Ping(context.Background())
				return
			}
			_, errs[slot] = client.ListTools(context.Background())
		}(index)
	}
	wait.Wait()

	for slot := 0; slot < workers; slot++ {
		if panics[slot] != nil {
			t.Fatalf("worker %d panicked after transport abort: %v", slot, panics[slot])
		}
		if errs[slot] == nil {
			t.Fatalf("worker %d returned nil error after transport abort", slot)
		}
		if !errors.Is(errs[slot], ErrClosed) && !errors.Is(errs[slot], context.Canceled) {
			// 允许进程退出的具体错误，但必须是"可识别"的错误：至少不能是空值。
			t.Logf("worker %d error (acceptable): %v", slot, errs[slot])
		}
	}
}
