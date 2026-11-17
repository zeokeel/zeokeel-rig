package escalate_test

// 沙箱内 client 的用例：ID 自动分配、并发、关闭、取消、服务端消失。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

func TestClientAssignsIDWhenEmpty(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)

	var mu sync.Mutex
	var seenID string
	client := startBroker(t, policy, escalate.WithAuditor(func(req escalate.Request, _ escalate.Response) {
		mu.Lock()
		seenID = req.ID
		mu.Unlock()
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{
		Argv: []string{"echo", "x"}, Dir: root, Reason: "client 应自动补 ID",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Denied {
		t.Fatalf("不应被拒绝: %+v", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenID == "" {
		t.Fatal("client 必须为未指定 ID 的请求自动生成 ID")
	}
	if resp.ID != seenID {
		t.Fatalf("响应 ID %q 与请求 ID %q 不一致", resp.ID, seenID)
	}
}

func TestClientConcurrentExec(t *testing.T) {
	policy, root := testPolicy(t)
	client := startBroker(t, policy, escalate.WithRunner(func(_ context.Context, req escalate.Request) (escalate.Response, error) {
		time.Sleep(10 * time.Millisecond)
		return escalate.Response{ExitCode: 0, Stdout: req.ID}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	const n = 6
	var wg sync.WaitGroup
	out := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Exec(ctx, escalate.Request{
				ID: fmt.Sprintf("client-%d", i), Argv: []string{"echo", "x"}, Dir: root, Reason: "并发 client",
			})
			out[i], errs[i] = resp.Stdout, err
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个请求失败: %v", i, errs[i])
		}
		if out[i] != fmt.Sprintf("client-%d", i) {
			t.Fatalf("第 %d 个响应串线: %q", i, out[i])
		}
	}
}

func TestClientDuplicateIDRejectedLocally(t *testing.T) {
	policy, root := testPolicy(t)
	release := make(chan struct{})
	client := startBroker(t, policy, escalate.WithRunner(func(_ context.Context, _ escalate.Request) (escalate.Response, error) {
		<-release
		return escalate.Response{ExitCode: 0}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = client.Exec(ctx, escalate.Request{ID: "dup", Argv: []string{"echo", "x"}, Dir: root, Reason: "占位"})
	}()
	// 等第一个请求进入在途状态，再用同一个 ID 发第二条。
	time.Sleep(50 * time.Millisecond)
	if _, err := client.Exec(ctx, escalate.Request{ID: "dup", Argv: []string{"echo", "x"}, Dir: root, Reason: "重复"}); err == nil {
		t.Fatal("同一 client 内重复 ID 必须在本地拒绝（否则响应会串线）")
	}
	close(release)
	wg.Wait()
}

func TestClientCloseUnblocksPending(t *testing.T) {
	policy, root := testPolicy(t)
	client := startBroker(t, policy, escalate.WithRunner(func(ctx context.Context, _ escalate.Request) (escalate.Response, error) {
		<-ctx.Done()
		return escalate.Response{ExitCode: -1}, ctx.Err()
	}))

	errCh := make(chan error, 1)
	go func() {
		_, err := client.Exec(context.Background(), escalate.Request{
			ID: "pending", Argv: []string{"echo", "x"}, Dir: root, Reason: "在途中关闭",
		})
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, escalate.ErrClosed) {
			t.Fatalf("在途请求应得到 ErrClosed，得到 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close 未解除在途请求的阻塞")
	}
}

func TestClientExecAfterCloseReturnsErrClosed(t *testing.T) {
	policy, root := testPolicy(t)
	client := startBroker(t, policy)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := client.Exec(context.Background(), escalate.Request{
		ID: "after-close", Argv: []string{"echo", "x"}, Dir: root, Reason: "关闭之后",
	})
	if !errors.Is(err, escalate.ErrClosed) {
		t.Fatalf("关闭后 Exec 应得到 ErrClosed，得到 %v", err)
	}
	// Close 幂等。
	if err := client.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
}

func TestClientExecContextCancel(t *testing.T) {
	policy, root := testPolicy(t)
	client := startBroker(t, policy, escalate.WithRunner(func(ctx context.Context, _ escalate.Request) (escalate.Response, error) {
		<-ctx.Done()
		return escalate.Response{ExitCode: -1}, ctx.Err()
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := client.Exec(ctx, escalate.Request{
		ID: "cancel", Argv: []string{"echo", "x"}, Dir: root, Reason: "客户端侧取消",
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回 context.DeadlineExceeded，得到 %v", err)
	}
}

func TestClientServerDisconnectReturnsErrClosed(t *testing.T) {
	policy, root := testPolicy(t)
	broker, err := escalate.NewBroker(policy)
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	server, clientConn := pipePair(t)
	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { errCh <- broker.Serve(ctx, server) }()

	client := escalate.NewClient(clientConn)
	execCtx, execCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer execCancel()
	if _, err := client.Exec(execCtx, escalate.Request{
		ID: "before-drop", Argv: []string{"echo", "x"}, Dir: root, Reason: "先确认在线",
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	// 掐断服务端。
	_ = server.Close()
	<-errCh

	_, err = client.Exec(execCtx, escalate.Request{
		ID: "after-drop", Argv: []string{"echo", "x"}, Dir: root, Reason: "服务端已消失",
	})
	if !errors.Is(err, escalate.ErrClosed) {
		t.Fatalf("服务端断开后 Exec 应得到 ErrClosed，得到 %v", err)
	}
	_ = client.Close()
}
