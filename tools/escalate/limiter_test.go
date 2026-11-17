package escalate

// 本文件是限流器与结果渲染的纯逻辑用例：它们不依赖连接，因此可以把"繁忙/排队/
// 取消"和"三态文案/截断边界"钉死。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// waitFor 轮询条件直到成立（避免用固定 sleep 制造 flaky 用例）。
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func TestLimiterAcquiresAndReleases(t *testing.T) {
	l := newLimiter(1, 1)
	ctx := context.Background()
	if ok, busy := l.acquire(ctx); !ok || busy {
		t.Fatalf("首个 acquire = (%v,%v)", ok, busy)
	}
	// 第二个请求：额度已满，占一个排队名额并等待。
	type outcome struct{ ok, busy bool }
	result := make(chan outcome, 1)
	go func() {
		ok, busy := l.acquire(ctx)
		result <- outcome{ok, busy}
	}()
	waitFor(t, "第二个请求进入排队", func() bool { return len(l.queue) == 1 })

	// 释放额度：排队者应当拿到它（而不是被 busy 掉）。
	l.release()
	select {
	case got := <-result:
		if !got.ok || got.busy {
			t.Fatalf("排队者在释放后应获得额度，实际 (%v,%v)", got.ok, got.busy)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("排队者没有拿到额度")
	}
	l.release()
	if len(l.queue) != 0 || len(l.sem) != 0 {
		t.Fatalf("额度/排队没有归还干净: sem=%d queue=%d", len(l.sem), len(l.queue))
	}
}

func TestLimiterBusyWhenQueueFull(t *testing.T) {
	l := newLimiter(1, 1)
	if ok, busy := l.acquire(context.Background()); !ok || busy {
		t.Fatalf("第一次 acquire = (%v,%v)", ok, busy)
	}
	defer l.release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = l.acquire(ctx) // 进入排队，撤销 ctx 后返回
	}()
	waitFor(t, "第二个请求进入排队", func() bool { return len(l.queue) == 1 })

	// 额度与排队都满：必须立刻 busy，而不是无限等。
	start := time.Now()
	ok, busy := l.acquire(context.Background())
	if ok || !busy {
		t.Fatalf("排队满时 acquire = (%v,%v), want (false,true)", ok, busy)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("busy 判定不该阻塞，耗时 %s", elapsed)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后排队者没有返回")
	}
}

func TestLimiterCancelWhileQueued(t *testing.T) {
	l := newLimiter(1, 1)
	if ok, busy := l.acquire(context.Background()); !ok || busy {
		t.Fatal("首个 acquire 应成功")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var acquired, busy bool
	go func() {
		defer close(done)
		acquired, busy = l.acquire(ctx)
	}()
	waitFor(t, "进入排队", func() bool { return len(l.queue) == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 acquire 没有返回")
	}
	if acquired || busy {
		t.Fatalf("取消后的 acquire = (%v,%v), want (false,false)", acquired, busy)
	}
	l.release()
	if got := len(l.queue); got != 0 {
		t.Fatalf("取消后排队名额没有归还: %d", got)
	}
}

func TestLimiterQueueZeroMeansNoWaiting(t *testing.T) {
	l := newLimiter(1, 0)
	if ok, busy := l.acquire(context.Background()); !ok || busy {
		t.Fatal("首个 acquire 应成功")
	}
	if _, busy := l.acquire(context.Background()); !busy {
		t.Fatal("queue 容量 0 时，额度满必须立刻 busy")
	}
	l.release()
}

func TestRenderSuccessFormat(t *testing.T) {
	got := renderSuccess(0, "out line\n", "", false, 0)
	want := "exit_code: 0\nstdout:\nout line\n\nstderr:\n" + emptyBodyNotice
	if got != want {
		t.Fatalf("renderSuccess =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderSuccessNonZeroExitAndStderr(t *testing.T) {
	got := renderSuccess(2, "", "boom", false, 0)
	if !strings.HasPrefix(got, "exit_code: 2\nstdout:\n"+emptyBodyNotice+"\nstderr:\nboom") {
		t.Fatalf("renderSuccess = %q", got)
	}
	if strings.Contains(got, truncatedNotice) {
		t.Fatalf("没截断却带截断标注: %q", got)
	}
}

func TestRenderSuccessTruncation(t *testing.T) {
	got := renderSuccess(0, strings.Repeat("a", 100), strings.Repeat("b", 100), false, 10)
	if !strings.Contains(got, truncatedNotice) {
		t.Fatalf("必须标注截断: %q", got)
	}
	if strings.Contains(got, strings.Repeat("a", 11)) {
		t.Fatalf("stdout 没有被截断: %q", got)
	}
	// broker 已经截断过（Truncated=true）时也要标注。
	got = renderSuccess(0, "x", "", true, 0)
	if !strings.Contains(got, truncatedNotice) {
		t.Fatalf("Truncated=true 必须标注: %q", got)
	}
}

func TestRenderSuccessTruncatesAtRuneBoundary(t *testing.T) {
	// "你好世界" 每个字 3 字节；限 4 字节时只能放下一个字，不能劈开第二个字。
	got := renderSuccess(0, "你好世界", "", false, 4)
	if !strings.Contains(got, "你") || strings.Contains(got, "好") {
		t.Fatalf("截断没有回退到字符边界: %q", got)
	}
	if !strings.Contains(got, truncatedNotice) {
		t.Fatalf("缺少截断标注: %q", got)
	}
}

func TestRenderDeniedAndFailedAndBusy(t *testing.T) {
	if got := renderDenied("命中禁止列表: git push"); got != DeniedPrefix+"命中禁止列表: git push]" {
		t.Fatalf("renderDenied = %q", got)
	}
	if got := renderDenied("   "); !strings.Contains(got, "no reason given") {
		t.Fatalf("空拒绝原因要有兜底文案: %q", got)
	}
	if got := renderFailed(errors.New("连接已关闭")); got != FailedPrefix+"连接已关闭]" {
		t.Fatalf("renderFailed = %q", got)
	}
	if got := renderFailed(nil); got != FailedPrefix+"unknown error]" {
		t.Fatalf("nil 错误要有兜底: %q", got)
	}
	busy := renderBusy("服务器繁忙：排队请求过多")
	if !strings.HasPrefix(busy, BusyPrefix) {
		t.Fatalf("renderBusy 前缀不对: %q", busy)
	}
	if !strings.Contains(busy, "retry") || !strings.Contains(busy, "not a policy denial") {
		t.Fatalf("busy 必须提示可重试且不是拒绝: %q", busy)
	}
}

func TestResultPrefixesAreDistinct(t *testing.T) {
	prefixes := map[string]string{
		"denied": DeniedPrefix,
		"failed": FailedPrefix,
		"busy":   BusyPrefix,
	}
	for name, prefix := range prefixes {
		for otherName, other := range prefixes {
			if name == otherName {
				continue
			}
			if strings.HasPrefix(prefix, other) || strings.HasPrefix(other, prefix) {
				t.Fatalf("%s 与 %s 的前缀互相包含，模型无法区分", name, otherName)
			}
		}
	}
	// 三态前缀都不能与成功状态的首行混淆。
	for _, prefix := range prefixes {
		if strings.HasPrefix("exit_code: 0", prefix) || strings.HasPrefix(prefix, "exit_code") {
			t.Fatalf("前缀 %q 与成功状态冲突", prefix)
		}
	}
}
