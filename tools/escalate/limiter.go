package escalate

// 本文件是工具侧的并发额度与排队上限。
//
// Broker 侧有同名的 Policy.MaxConcurrent/MaxQueue，但那只有在工具自己拥有 Broker
// （NewWithBroker）时才生效；复用宿主已有 Client（New）时工具看不到对端策略。因此
// 工具自己再限一次流：既保证两种装配方式行为一致，也避免模型用"一次发很多工具
// 调用"把审批 UI 或宿主资源打爆——额度在**审批之前**获取，审批同样受它约束。

import (
	"context"
	"unicode/utf8"
)

// limiter 是"并发额度 + 有界排队"。
//
// 语义与 sandbox/escalate 的 Broker 一致：额度满时不立刻失败，而是排进有界队列；
// 队列也满了才回"繁忙"（可退避重试）。
type limiter struct {
	// sem 的容量是并发上限。
	sem chan struct{}
	// queue 的容量是排队上限；容量 0 表示不允许排队（额度满即繁忙）。
	queue chan struct{}
}

// newLimiter 构造一个 limiter；concurrency 与 queue 必须已归一化为正数/非负。
func newLimiter(concurrency, queue int) *limiter {
	return &limiter{
		sem:   make(chan struct{}, concurrency),
		queue: make(chan struct{}, queue),
	}
}

// acquire 获取一个执行槽位。
//
// 返回值 busy=true 表示"额度与排队都满"，调用方应回 busy 而不是等待；ok=false 且
// busy=false 表示 ctx 在排队期间被取消。成功返回时调用方必须 defer release()。
func (l *limiter) acquire(ctx context.Context) (ok, busy bool) {
	// 快路径：还有额度。
	select {
	case l.sem <- struct{}{}:
		return true, false
	default:
	}
	// 慢路径：先占排队名额（名额本身有界），再等额度。
	select {
	case l.queue <- struct{}{}:
	default:
		return false, true
	}
	select {
	case l.sem <- struct{}{}:
		<-l.queue
		return true, false
	case <-ctx.Done():
		<-l.queue
		return false, false
	}
}

// release 归还执行槽位。
func (l *limiter) release() {
	select {
	case <-l.sem:
	default:
	}
}

// utf8RuneStart 报告 b 是否是 UTF-8 字符的起始字节（截断时用来回退到字符边界）。
func utf8RuneStart(b byte) bool { return utf8.RuneStart(b) }
