package team

// 本文件钉住 send_message → wait_agent 的**代次**语义：
// SendAsyncContext 只入队、排空是异步的，因此紧接着的 Wait 不能把上一轮结果当作
// 新消息的答复返回。
//
// 修复前 Wait 直接调 subagent.Wait，而排空 goroutine 还没把新消息送进去时，后者
// 立刻返回上一次的结果（"A"）——调用方以为新消息没生效。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// seqSubagent 是一个"结果按消息推进"的假子代理：Send 阻塞在 gate 上，放行后把
// 当前结果替换成对这条消息的答复；Wait 立即返回当前结果。
type seqSubagent struct {
	name string

	mu      sync.Mutex
	current rig.SubagentResult
	sends   []string

	gate     chan struct{}
	gateOnce sync.Once
	stopCh   chan struct{}
	// gates 按调用序号（从 1 开始）覆盖默认 gate，用于逐条消息控制时序。
	gates map[int]chan struct{}
}

func newSeqSubagent(name, first string) *seqSubagent {
	return &seqSubagent{
		name:    name,
		current: rig.SubagentResult{Name: name, Output: first, Iterations: 1},
		gate:    make(chan struct{}),
		stopCh:  make(chan struct{}),
	}
}

func (s *seqSubagent) Name() string { return s.name }

func (s *seqSubagent) Send(ctx context.Context, message string) (rig.SubagentResult, error) {
	s.mu.Lock()
	s.sends = append(s.sends, message)
	call := len(s.sends)
	gate := s.gate
	if specific, ok := s.gates[call]; ok && specific != nil {
		gate = specific
	}
	s.mu.Unlock()
	select {
	case <-gate:
	case <-s.stopCh:
		return rig.SubagentResult{}, errors.New("subagent was stopped")
	case <-ctx.Done():
		return rig.SubagentResult{}, ctx.Err()
	}
	result := rig.SubagentResult{Name: s.name, Output: "reply:" + message, Iterations: 1}
	s.mu.Lock()
	s.current = result
	s.mu.Unlock()
	return result, nil
}

func (s *seqSubagent) Wait(ctx context.Context) (rig.SubagentResult, error) {
	s.mu.Lock()
	result := s.current
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return rig.SubagentResult{}, err
	}
	return result, nil
}

func (s *seqSubagent) Stop() error {
	s.release()
	s.gateOnce.Do(func() { close(s.stopCh) })
	return nil
}

// release 放行阻塞中的 Send（测试驱动时序用）。
func (s *seqSubagent) release() {
	s.gateOnce.Do(func() { close(s.gate) })
}

type seqRunner struct {
	sub *seqSubagent
}

func (r *seqRunner) Start(_ context.Context, spec rig.SubagentSpec) (rig.Subagent, error) {
	r.sub = newSeqSubagent(spec.Name, "A")
	return r.sub, nil
}

// TestWaitAfterSendReturnsNewResult：成员当前结果是 "A"，SendAsync 一条要求答复 "B"
// 的消息后立刻 Wait，必须拿到新结果而不是 "A"。
func TestWaitAfterSendReturnsNewResult(t *testing.T) {
	runner := &seqRunner{}
	m := NewManager(runner)
	defer m.CloseAll()
	if _, err := m.Spawn(context.Background(), rig.SubagentSpec{Name: "mate", Prompt: "work"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	sub := runner.sub
	// 与工具层一致：先取一次首轮结果（watchCompletion 在后台做同一件事），
	// 这样成员的"当前结果"就是 A（否则用例没有测到目标）。
	if first, err := m.Wait(context.Background(), "mate"); err != nil || first.Output != "A" {
		t.Fatalf("首轮 Wait = %q err=%v，want A", first.Output, err)
	}
	if last, ok, err := m.Last("mate"); err != nil || !ok || last.Output != "A" {
		t.Fatalf("初始状态不对: Last ok=%v err=%v", ok, err)
	}

	if err := m.SendAsyncContext(context.Background(), "mate", "please answer B"); err != nil {
		t.Fatalf("SendAsyncContext: %v", err)
	}

	results := make(chan rig.SubagentResult, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := m.Wait(context.Background(), "mate")
		if err != nil {
			errs <- err
			return
		}
		results <- result
	}()

	// 给（修复前的）Wait 足够时间提前返回旧结果 A。
	time.Sleep(100 * time.Millisecond)
	sub.release()

	select {
	case err := <-errs:
		t.Fatalf("Wait: %v", err)
	case result := <-results:
		if result.Output != "reply:please answer B" {
			t.Fatalf("Wait 返回 %q，want 新消息的答复（修复前会返回旧结果 \"A\"）", result.Output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait 超时：排空完成后仍没有返回")
	}
	// Wait 之后 Last 也必须是新结果。
	if last, ok, err := m.Last("mate"); err != nil || !ok || last.Output != "reply:please answer B" {
		t.Fatalf("Last = %+v ok=%v err=%v，want 新结果", last, ok, err)
	}
}

// TestWaitWithEmptyInboxReturnsImmediately：没有待处理消息时 Wait 行为不变（立即
// 返回当前结果，不因为"代次"等待）。
func TestWaitWithEmptyInboxReturnsImmediately(t *testing.T) {
	runner := &seqRunner{}
	m := NewManager(runner)
	defer m.CloseAll()
	if _, err := m.Spawn(context.Background(), rig.SubagentSpec{Name: "mate", Prompt: "work"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	start := time.Now()
	result, err := m.Wait(context.Background(), "mate")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Output != "A" {
		t.Fatalf("Wait = %q，want 当前结果 A", result.Output)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("空队列的 Wait 阻塞了 %v，应立返回", elapsed)
	}
}

// TestWaitAfterSendHonoursContext：排空未完成时 Wait 仍受 ctx 约束（不会挂死）。
func TestWaitAfterSendHonoursContext(t *testing.T) {
	runner := &seqRunner{}
	m := NewManager(runner)
	defer m.CloseAll()
	if _, err := m.Spawn(context.Background(), rig.SubagentSpec{Name: "mate", Prompt: "work"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := m.SendAsyncContext(context.Background(), "mate", "slow"); err != nil {
		t.Fatalf("SendAsyncContext: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := m.Wait(ctx, "mate"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait err = %v，want DeadlineExceeded", err)
	}
}

// setGates 为第 n 次 Send 指定阻塞通道（nil 表示用默认 gate）。
func (s *seqSubagent) setGates(gates map[int]chan struct{}) {
	s.mu.Lock()
	s.gates = gates
	s.mu.Unlock()
}

func (s *seqSubagent) sendCountLocked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sends)
}

// waitForSend 轮询到第 n 次 Send 已进入（即排空 goroutine 已经拿到这条消息）。
func (s *seqSubagent) waitForSend(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.sendCountLocked() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("第 %d 次 Send 没有进入（已进入 %d 次）", n, s.sendCountLocked())
}

// TestWaitCoversMessagesQueuedDuringWait：Wait 已在等第一条消息时又有新消息入队，
// Wait 必须收敛到**新消息**的答复，而不是停在调用时刻的那一代。
func TestWaitCoversMessagesQueuedDuringWait(t *testing.T) {
	runner := &seqRunner{}
	m := NewManager(runner)
	defer m.CloseAll()
	if _, err := m.Spawn(context.Background(), rig.SubagentSpec{Name: "mate", Prompt: "work"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	sub := runner.sub
	if first, err := m.Wait(context.Background(), "mate"); err != nil || first.Output != "A" {
		t.Fatalf("首轮 Wait = %q err=%v，want A", first.Output, err)
	}
	gate1 := make(chan struct{})
	gate2 := make(chan struct{})
	sub.setGates(map[int]chan struct{}{1: gate1, 2: gate2})

	// 第一条消息：排空 goroutine 会卡在 Send #1 上。
	if err := m.SendAsyncContext(context.Background(), "mate", "first"); err != nil {
		t.Fatalf("SendAsync(first): %v", err)
	}
	sub.waitForSend(t, 1)

	// Wait 开始等（此刻 target = 1）。
	results := make(chan rig.SubagentResult, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := m.Wait(context.Background(), "mate")
		if err != nil {
			errs <- err
			return
		}
		results <- result
	}()
	time.Sleep(50 * time.Millisecond)

	// Wait 等待期间再入队一条：它也必须被这次 Wait 覆盖。
	if err := m.SendAsyncContext(context.Background(), "mate", "second"); err != nil {
		t.Fatalf("SendAsync(second): %v", err)
	}

	// 放行第一条：旧实现此时会立刻返回 "reply:first"（只等到 target=1 那一代）。
	close(gate1)
	time.Sleep(150 * time.Millisecond)
	close(gate2)

	select {
	case err := <-errs:
		t.Fatalf("Wait: %v", err)
	case result := <-results:
		if result.Output != "reply:second" {
			t.Fatalf("Wait 返回 %q，want 新入队消息的答复 \"reply:second\"（只等调用时刻的代次会漏掉它）", result.Output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait 超时：收敛后仍没有返回")
	}
}

// TestWaitFollowsStreamingMessages：Wait 期间消息源源不断入队时，Wait 必须一路收敛到
// **最后一条**（第 N 条）的答复——收敛只受调用方 ctx 约束，没有人为轮数上界。
func TestWaitFollowsStreamingMessages(t *testing.T) {
	const messages = 20
	runner := &seqRunner{}
	m := NewManager(runner)
	defer m.CloseAll()
	if _, err := m.Spawn(context.Background(), rig.SubagentSpec{Name: "mate", Prompt: "work"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	sub := runner.sub
	if first, err := m.Wait(context.Background(), "mate"); err != nil || first.Output != "A" {
		t.Fatalf("首轮 Wait = %q err=%v，want A", first.Output, err)
	}
	// 每条消息一个 gate：第 n 次 Send 卡在 gates[n]。
	gates := make(map[int]chan struct{}, messages)
	for i := 1; i <= messages; i++ {
		gates[i] = make(chan struct{})
	}
	sub.setGates(gates)

	if err := m.SendAsyncContext(context.Background(), "mate", "msg-1"); err != nil {
		t.Fatalf("SendAsync(msg-1): %v", err)
	}
	sub.waitForSend(t, 1)

	results := make(chan rig.SubagentResult, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := m.Wait(context.Background(), "mate")
		if err != nil {
			errs <- err
			return
		}
		results <- result
	}()
	time.Sleep(30 * time.Millisecond) // 让 Wait 先进入等待

	for i := 1; i <= messages; i++ {
		if i < messages {
			// Wait 已在等待时入队下一条：它必须被这次 Wait 覆盖。
			if err := m.SendAsyncContext(context.Background(), "mate", fmt.Sprintf("msg-%d", i+1)); err != nil {
				t.Fatalf("SendAsync(msg-%d): %v", i+1, err)
			}
		}
		close(gates[i])
		if i < messages {
			// 等排空 goroutine 拿到下一条：说明第 i 条已应用，Wait 会推进到下一轮。
			sub.waitForSend(t, i+1)
		}
	}

	select {
	case err := <-errs:
		t.Fatalf("Wait: %v", err)
	case result := <-results:
		if result.Output != fmt.Sprintf("reply:msg-%d", messages) {
			t.Fatalf("Wait 返回 %q，want 第 %d 条消息的答复（有轮数上界的实现会停在中间某条）",
				result.Output, messages)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait 超时：没有收敛到最后一条消息")
	}
}

// TestWaitUnboundedRespectsContext：收敛没有轮数上界，但等待仍严格受调用方 ctx 约束——
// 消息持续涌入时 200ms 的 deadline 必须让 Wait 返回 DeadlineExceeded，而不是提前
// 收敛到某条中间消息（"无上界"不等于"失控"）。
func TestWaitUnboundedRespectsContext(t *testing.T) {
	runner := &seqRunner{}
	m := NewManager(runner)
	defer m.CloseAll()
	if _, err := m.Spawn(context.Background(), rig.SubagentSpec{Name: "mate", Prompt: "work"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	sub := runner.sub
	if first, err := m.Wait(context.Background(), "mate"); err != nil || first.Output != "A" {
		t.Fatalf("首轮 Wait = %q err=%v，want A", first.Output, err)
	}
	// 前 1 条消息卡住，保证 Wait 的代次快照 > 0（否则走"空队列立即返回"的原路径）；
	// 之后的消息逐条放行，让 appliedSeq 持续前进。
	const gatesCount = 400
	gates := make(map[int]chan struct{}, gatesCount)
	for i := 1; i <= gatesCount; i++ {
		gates[i] = make(chan struct{})
	}
	sub.setGates(gates)
	if err := m.SendAsyncContext(context.Background(), "mate", "msg-1"); err != nil {
		t.Fatalf("SendAsync(msg-1): %v", err)
	}
	sub.waitForSend(t, 1)

	stop := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	// 入队速度远快于放行速度：queuedSeq 始终领先 appliedSeq，收敛永不发生。
	go func() {
		defer workers.Done()
		for i := 2; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = m.SendAsyncContext(context.Background(), "mate", fmt.Sprintf("msg-%d", i))
			time.Sleep(200 * time.Microsecond)
		}
	}()
	go func() {
		defer workers.Done()
		for i := 1; i <= gatesCount; i++ {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			close(gates[i])
		}
	}()
	defer func() {
		close(stop)
		workers.Wait()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	result, err := m.Wait(ctx, "mate")
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = (%q, %v)，want DeadlineExceeded（无上界不等于失控）", result.Output, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ctx 到期后过了 %v 才返回", elapsed)
	}
}
