package escalate_test

// 对抗审计（续）：排队上限（Policy.MaxQueue）。
//
// 背景：MaxConcurrent 只限制"同时在执行"的数量，排队请求原先无上限，恶意沙箱
// 可以靠刷请求把"已批准"的命令无限期推迟（head-of-line blocking）。这里固定
// 新语义：排队数超过 MaxQueue 立刻拒绝，让客户端退避重试，而不是挂到超时。

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

// queuePolicy 返回一个"并发 1、排队可配"的策略。
func queuePolicy(t *testing.T, maxQueue int) escalate.Policy {
	t.Helper()
	return escalate.Policy{
		Allow:         [][]string{{"echo"}},
		Roots:         []string{t.TempDir()},
		MaxConcurrent: 1,
		MaxQueue:      maxQueue,
		MaxTimeout:    10 * time.Second,
	}
}

// blockingRunner 返回一个"进门就阻塞到 release 关闭"的 runner，并暴露当前在执行的
// 数量（用于确认请求真的已经占住/排上）。
func blockingRunner(release <-chan struct{}) (func(context.Context, escalate.Request) (escalate.Response, error), *atomic.Int32) {
	var running atomic.Int32
	runner := func(context.Context, escalate.Request) (escalate.Response, error) {
		running.Add(1)
		defer running.Add(-1)
		<-release
		return escalate.Response{Stdout: "ok"}, nil
	}
	return runner, &running
}

// waitUntil 在 3 秒内等待条件成立。
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAuditQueueFullRejectsImmediately 验证：并发额度满 + 排队满之后，下一个请求
// 立刻被拒（Denied + QueueFullReason），而不是排队甚至超时；已有请求不受影响。
func TestAuditQueueFullRejectsImmediately(t *testing.T) {
	policy := queuePolicy(t, 2)
	root := policy.Roots[0]
	release := make(chan struct{})
	runner, running := blockingRunner(release)
	client := startBroker(t, policy, escalate.WithRunner(runner))
	ctx, cancel := execCtx(t)
	defer cancel()

	// 第 1 个请求占住唯一的执行槽位。
	var firstDone sync.WaitGroup
	firstDone.Add(1)
	go func() {
		defer firstDone.Done()
		_, _ = client.Exec(ctx, escalate.Request{ID: "q-0", Argv: []string{"echo", "0"}, Dir: root, Reason: "占槽位"})
	}()
	waitUntil(t, "第一个请求进入执行", func() bool { return running.Load() == 1 })

	// 再塞满排队额度（MaxQueue=2）。
	var queued sync.WaitGroup
	queued.Add(2)
	for i := 1; i <= 2; i++ {
		go func(i int) {
			defer queued.Done()
			_, _ = client.Exec(ctx, escalate.Request{ID: "q-" + itoa(i), Argv: []string{"echo", "q"}, Dir: root, Reason: "排队"})
		}(i)
	}
	// 给它们时间真正排到 sem 上（runner 仍然阻塞，槽位不会释放）。
	time.Sleep(200 * time.Millisecond)
	if running.Load() != 1 {
		t.Fatalf("在执行的请求数 = %d，期望 1", running.Load())
	}

	// 第 4 个请求：排队已满 → 立刻被拒。
	start := time.Now()
	resp, err := client.Exec(ctx, escalate.Request{ID: "q-overflow", Argv: []string{"echo", "x"}, Dir: root, Reason: "溢出"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("溢出请求返回了 Go 错误（应当是 Denied 响应）: %v", err)
	}
	if !resp.Denied {
		t.Fatalf("排队已满却没有被拒: %+v", resp)
	}
	if !strings.Contains(resp.DenyReason, "服务器繁忙") {
		t.Fatalf("拒绝原因不是「繁忙」: %q", resp.DenyReason)
	}
	if elapsed > time.Second {
		t.Fatalf("排队满时被拒耗时 %s，应当立刻返回（不能拖到超时）", elapsed)
	}
	if resp.ExitCode != escalate.DeniedExitCode {
		t.Fatalf("exitCode = %d，期望 %d", resp.ExitCode, escalate.DeniedExitCode)
	}

	// 放行：已有请求（1 个执行 + 2 个排队）全部正常完成，不受拒绝影响。
	close(release)
	firstDone.Wait()
	queued.Wait()
}

// TestAuditQueueUnlimitedRestoresOldBehavior 验证 MaxQueue=-1 恢复"无限排队"。
func TestAuditQueueUnlimitedRestoresOldBehavior(t *testing.T) {
	policy := queuePolicy(t, -1)
	root := policy.Roots[0]
	release := make(chan struct{})
	runner, running := blockingRunner(release)
	client := startBroker(t, policy, escalate.WithRunner(runner))
	ctx, cancel := execCtx(t)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = client.Exec(ctx, escalate.Request{ID: "u-0", Argv: []string{"echo", "0"}, Dir: root, Reason: "占槽位"})
	}()
	waitUntil(t, "第一个请求进入执行", func() bool { return running.Load() == 1 })

	// 远超默认排队长度的请求全部只能排队，不能收到"繁忙"。
	const extra = 80
	results := make(chan escalate.Response, extra)
	for i := 0; i < extra; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Exec(ctx, escalate.Request{
				ID: "u-" + itoa(i+1), Argv: []string{"echo", "q"}, Dir: root, Reason: "无限排队",
			})
			if err != nil {
				resp = escalate.Response{Denied: true, DenyReason: "go error: " + err.Error()}
			}
			results <- resp
		}(i)
	}
	time.Sleep(300 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	for resp := range results {
		if resp.Denied && strings.Contains(resp.DenyReason, "服务器繁忙") {
			t.Fatal("MaxQueue=-1 时不应出现「繁忙」拒绝（旧行为是无上限排队）")
		}
	}
}

// TestAuditQueueFullIsAudited 验证"繁忙"拒绝也走审计（宿主需要统计容量问题）。
func TestAuditQueueFullIsAudited(t *testing.T) {
	policy := queuePolicy(t, 1)
	root := policy.Roots[0]
	release := make(chan struct{})
	runner, running := blockingRunner(release)

	var mu sync.Mutex
	var busy int
	auditor := func(_ escalate.Request, resp escalate.Response) {
		if resp.Denied && strings.Contains(resp.DenyReason, "服务器繁忙") {
			mu.Lock()
			busy++
			mu.Unlock()
		}
	}
	client := startBroker(t, policy, escalate.WithRunner(runner), escalate.WithAuditor(auditor))
	ctx, cancel := execCtx(t)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = client.Exec(ctx, escalate.Request{ID: "a-0", Argv: []string{"echo", "0"}, Dir: root, Reason: "占槽位"})
	}()
	waitUntil(t, "第一个请求进入执行", func() bool { return running.Load() == 1 })
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = client.Exec(ctx, escalate.Request{ID: "a-1", Argv: []string{"echo", "1"}, Dir: root, Reason: "排队"})
	}()
	time.Sleep(200 * time.Millisecond)

	if _, err := client.Exec(ctx, escalate.Request{ID: "a-2", Argv: []string{"echo", "2"}, Dir: root, Reason: "溢出"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	mu.Lock()
	got := busy
	mu.Unlock()
	if got == 0 {
		t.Fatal("繁忙拒绝没有进审计")
	}
	close(release)
	wg.Wait()
}

// TestAuditQueueDefaultIs64 验证"MaxQueue=0 时默认上限 64"是真实生效的：
// 1 个占槽位 + 80 个排队 → 至少 (80-64)=16 个被"繁忙"拒绝。
func TestAuditQueueDefaultIs64(t *testing.T) {
	policy := queuePolicy(t, 0) // 0 = 默认
	root := policy.Roots[0]
	release := make(chan struct{})
	runner, running := blockingRunner(release)
	client := startBroker(t, policy, escalate.WithRunner(runner))
	ctx, cancel := execCtx(t)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = client.Exec(ctx, escalate.Request{ID: "d-0", Argv: []string{"echo", "0"}, Dir: root, Reason: "占槽位"})
	}()
	waitUntil(t, "第一个请求进入执行", func() bool { return running.Load() == 1 })

	const extra = 80
	var busy atomic.Int32
	for i := 1; i <= extra; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Exec(ctx, escalate.Request{
				ID: "d-" + itoa(i), Argv: []string{"echo", "q"}, Dir: root, Reason: "默认队列",
			})
			if err == nil && resp.Denied && strings.Contains(resp.DenyReason, "服务器繁忙") {
				busy.Add(1)
			}
		}(i)
	}
	time.Sleep(500 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := busy.Load(); got < extra-escalate.DefaultMaxQueue-4 {
		t.Fatalf("繁忙拒绝数 = %d，默认排队上限 64 似乎没生效（期望 >= %d）", got, extra-escalate.DefaultMaxQueue-4)
	}
}

// TestAuditQueueConfigValidation 覆盖 MaxQueue 的配置校验。
func TestAuditQueueConfigValidation(t *testing.T) {
	root := t.TempDir()
	base := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}
	cases := []struct {
		name    string
		queue   int
		wantErr bool
	}{
		{"0 = 默认 64", 0, false},
		{"-1 = 不限", -1, false},
		{"正常上限", 8, false},
		{"-2 非法", -2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := base
			policy.MaxQueue = tc.queue
			if _, err := escalate.NewBroker(policy); (err != nil) != tc.wantErr {
				t.Fatalf("MaxQueue=%d err=%v, wantErr=%v", tc.queue, err, tc.wantErr)
			}
		})
	}
	if escalate.DefaultMaxQueue != 64 {
		t.Fatalf("DefaultMaxQueue = %d，期望 64", escalate.DefaultMaxQueue)
	}
	if !strings.Contains(escalate.QueueFullReason, "繁忙") {
		t.Fatalf("QueueFullReason = %q，应当可读且包含「繁忙」", escalate.QueueFullReason)
	}
}

// itoa 是 strconv.Itoa 的测试简写（避免为了一个用例引入依赖）。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
