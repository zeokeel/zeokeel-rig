package kit

// Shutdown 的契约测试。
//
// 要守住的用户可见事实只有一句：**宿主关窗口时，正在跑的回合不能变成一次"失败"**。
// 现状是宿主只能直接关存储 → 回合在 Append 上拿到 ErrClosed → 事件流里出现一条
// 带错误的 run.end → 用户看到"回答到一半消失"（docs/PLATFORM.md §4-1）。
//
// 所以每个正向用例都配一条反向对照：先复现"直接关存储"的坏结果，再证明
// Shutdown 走的是"取消 → 等回合收尾 → 关后台任务 → 关存储"这条路。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// shutdownBuffer 是并发安全的事件落地目标（Run 在 goroutine 里写，测试在读）。
type shutdownBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *shutdownBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *shutdownBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// shutdownHistory 是可关闭的 History：Close 之后的所有写入返回 store.ErrClosed，
// 与 sqlite/store 两个真实后端一致。
type shutdownHistory struct {
	mu      sync.Mutex
	closed  bool
	byID    map[string][]rig.Message
	appends int
}

func newShutdownHistory() *shutdownHistory {
	return &shutdownHistory{byID: map[string][]rig.Message{}}
}

func (h *shutdownHistory) Load(_ context.Context, sessionID string) ([]rig.Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, store.ErrClosed
	}
	return append([]rig.Message(nil), h.byID[sessionID]...), nil
}

func (h *shutdownHistory) Append(_ context.Context, sessionID string, messages ...rig.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return store.ErrClosed
	}
	h.appends++
	h.byID[sessionID] = append(h.byID[sessionID], messages...)
	return nil
}

func (h *shutdownHistory) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
}

func (h *shutdownHistory) appendCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.appends
}

// shutdownFakeBackground 是可观测的后台任务句柄：只数 CloseAll 被调了几次。
type shutdownFakeBackground struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (b *shutdownFakeBackground) CloseAll() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return b.err
}

func (b *shutdownFakeBackground) closeCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// shutdownFakeStorage 记录自身被关闭的次数。
type shutdownFakeStorage struct {
	mu    sync.Mutex
	calls int
}

func (s *shutdownFakeStorage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return nil
}

func (s *shutdownFakeStorage) closeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// shutdownFlakyStorage 第一次 Close 失败、之后成功：用来钉住"失败的收尾能在下一次
// Shutdown 里重试，而不是被一次错误就标记成已完成"。
type shutdownFlakyStorage struct {
	mu    sync.Mutex
	calls int
}

func (s *shutdownFlakyStorage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 1 {
		return errors.New("boom: storage refused to close")
	}
	return nil
}

func (s *shutdownFlakyStorage) closeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// shutdownProbeTool 在调用时关掉历史——模拟"宿主直接 st.Close()"那一刀。
type shutdownProbeTool struct {
	history *shutdownHistory
}

func (t *shutdownProbeTool) Name() string           { return "shutdown_probe" }
func (t *shutdownProbeTool) Description() string    { return "close the backing store mid-turn" }
func (t *shutdownProbeTool) Schema() map[string]any { return map[string]any{"type": "object"} }

func (t *shutdownProbeTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	t.history.close()
	return rig.ToolResult{Content: "closed"}, nil
}

// shutdownScriptProvider 按剧本返回固定分片。
type shutdownScriptProvider struct {
	mu      sync.Mutex
	scripts [][]rig.Chunk
}

func (p *shutdownScriptProvider) Name() string { return "shutdown-script" }

func (p *shutdownScriptProvider) Stream(_ context.Context, _ rig.Request) (rig.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.scripts) == 0 {
		return nil, errors.New("shutdownScriptProvider: no script left")
	}
	script := p.scripts[0]
	p.scripts = p.scripts[1:]
	return &shutdownChunkStream{chunks: script}, nil
}

type shutdownChunkStream struct {
	chunks []rig.Chunk
	next   int
}

func (s *shutdownChunkStream) Recv() (rig.Chunk, error) {
	if s.next >= len(s.chunks) {
		return rig.Chunk{}, io.EOF
	}
	chunk := s.chunks[s.next]
	s.next++
	return chunk, nil
}

// shutdownBlockingProvider 的长回合：第一次 Stream 就阻塞到 ctx 被取消。
type shutdownBlockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *shutdownBlockingProvider) Name() string { return "shutdown-blocking" }

func (p *shutdownBlockingProvider) Stream(ctx context.Context, _ rig.Request) (rig.Stream, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// shutdownRunEnds 解析 NDJSON 事件流里的 run.end payload。
func shutdownRunEnds(t *testing.T, ndjson string) []events.RunEnd {
	t.Helper()
	var ends []events.RunEnd
	for _, line := range strings.Split(strings.TrimSpace(ndjson), "\n") {
		if line == "" {
			continue
		}
		var env events.Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("事件行不是合法 JSON: %v (%q)", err, line)
		}
		if env.Kind != events.KindRunEnd {
			continue
		}
		var end events.RunEnd
		if len(env.Payload) > 0 {
			if err := json.Unmarshal(env.Payload, &end); err != nil {
				t.Fatalf("run.end payload: %v", err)
			}
		}
		ends = append(ends, end)
	}
	return ends
}

// shutdownContext 返回带超时的 ctx，避免关停 bug 把测试挂死。
func shutdownContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func shutdownUserMessage(text string) rig.Message {
	return rig.Message{Role: rig.RoleUser, Content: []rig.ContentPart{rig.TextPart(text)}}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestShutdownCancelsRunningTurn：起一个长回合 → Shutdown → 回合被取消，且事件流
// 里**没有**一次"回合失败"。
func TestShutdownCancelsRunningTurn(t *testing.T) {
	t.Run("反向对照：回合中途关存储会把回答变成失败", func(t *testing.T) {
		history := newShutdownHistory()
		provider := &shutdownScriptProvider{scripts: [][]rig.Chunk{
			{{ToolCalls: []rig.ToolCallDelta{{Index: 0, ID: "c1", Name: "shutdown_probe", Arguments: "{}"}}}, {FinishReason: "tool_calls"}},
			{{Delta: "done"}, {FinishReason: "stop"}},
		}}
		a, err := New(Config{
			Model:    "m",
			Provider: provider,
			Root:     t.TempDir(),
			History:  history,
			Gate:     gate.AllowAll(),
			Tools:    []rig.Tool{&shutdownProbeTool{history: history}},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, runErr := a.Run(shutdownContext(t), RunRequest{SessionID: "s", Input: []rig.Message{shutdownUserMessage("hi")}})
		if !errors.Is(runErr, store.ErrClosed) {
			t.Fatalf("run error = %v, want store.ErrClosed（这条对照用例要复现的正是「回答到一半消失」）", runErr)
		}
		if history.appendCount() == 0 {
			t.Fatal("前置条件不成立：回合在关闭之前一条都没写")
		}
	})

	t.Run("Shutdown 取消在飞回合且不报失败", func(t *testing.T) {
		var sink shutdownBuffer
		provider := &shutdownBlockingProvider{started: make(chan struct{})}
		history := newShutdownHistory()
		a, err := New(Config{
			Model:       "m",
			Provider:    provider,
			Root:        t.TempDir(),
			History:     history,
			EventWriter: &sink,
			Tools:       []rig.Tool{&shutdownProbeTool{history: history}},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		type runOutcome struct {
			result RunResult
			err    error
		}
		done := make(chan runOutcome, 1)
		go func() {
			result, runErr := a.Run(context.Background(), RunRequest{
				SessionID: "s",
				Input:     []rig.Message{shutdownUserMessage("写一篇长文")},
			})
			done <- runOutcome{result: result, err: runErr}
		}()

		select {
		case <-provider.started:
		case <-time.After(5 * time.Second):
			t.Fatal("回合没有开始（provider.Stream 从未被调用）")
		}

		if err := a.Shutdown(shutdownContext(t)); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}

		var outcome runOutcome
		select {
		case outcome = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Shutdown 之后回合仍未返回")
		}
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("回合错误 = %v, want context.Canceled（关停是取消，不是存储失败）", outcome.err)
		}

		// 事件流：run.end 必须存在、且不带错误（否则 UI 会把一次正常关停显示成失败）。
		ndjson := sink.String()
		ends := shutdownRunEnds(t, ndjson)
		if len(ends) != 1 {
			t.Fatalf("run.end 条数 = %d, want 1；事件流:\n%s", len(ends), ndjson)
		}
		if ends[0].Err != "" {
			t.Fatalf("run.end 带错误 %q：关停被当成回合失败", ends[0].Err)
		}
		// 哨兵兜底：任何事件里都不能漏出 ErrClosed 型字样。
		if strings.Contains(strings.ToLower(ndjson), "closed") {
			t.Fatalf("事件流里出现了 closed：\n%s", ndjson)
		}
	})
}

// TestShutdownClosesBackgroundJobs：Shutdown 必须关掉后台任务句柄。
func TestShutdownClosesBackgroundJobs(t *testing.T) {
	background := &shutdownFakeBackground{}
	a, err := New(Config{
		Model:          "m",
		Provider:       &shutdownScriptProvider{},
		Root:           t.TempDir(),
		Tools:          []rig.Tool{&shutdownProbeTool{history: newShutdownHistory()}},
		BackgroundJobs: []Background{background},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Shutdown(shutdownContext(t)); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := background.closeCalls(); got != 1 {
		t.Fatalf("CloseAll 调用次数 = %d, want 1", got)
	}
}

// TestShutdownIsIdempotent：连续两次不报错，且收尾动作只发生一次。
func TestShutdownIsIdempotent(t *testing.T) {
	background := &shutdownFakeBackground{}
	storage := &shutdownFakeStorage{}
	a, err := New(Config{
		Model:          "m",
		Provider:       &shutdownScriptProvider{},
		Root:           t.TempDir(),
		Tools:          []rig.Tool{&shutdownProbeTool{history: newShutdownHistory()}},
		BackgroundJobs: []Background{background},
		Storage:        storage,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := shutdownContext(t)
	if err := a.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := a.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if got := background.closeCalls(); got != 1 {
		t.Fatalf("CloseAll 调用次数 = %d, want 1（重复关停必须幂等）", got)
	}
	if got := storage.closeCalls(); got != 1 {
		t.Fatalf("Storage.Close 调用次数 = %d, want 1", got)
	}

	// 关停之后的 Run 必须可判定地失败，而不是让模型去用一个已死的存储。
	if _, err := a.Run(ctx, RunRequest{SessionID: "s", Input: []rig.Message{shutdownUserMessage("hi")}}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Shutdown 之后 Run 的错误 = %v, want ErrShuttingDown", err)
	}

	// nil Agent 上调用也必须安全（宿主清理代码常在 defer 里跑）。
	var nilAgent *Agent
	if err := nilAgent.Shutdown(ctx); err != nil {
		t.Fatalf("nil Agent Shutdown: %v", err)
	}
}

// TestShutdownRetriesAfterCleanupFailure：收尾失败不能被标记成"已完成"——下一次
// Shutdown 必须重试那个还在开着的存储（否则宿主的重试只是一句空话）。
func TestShutdownRetriesAfterCleanupFailure(t *testing.T) {
	storage := &shutdownFlakyStorage{}
	a, err := New(Config{
		Model:    "m",
		Provider: &shutdownScriptProvider{},
		Root:     t.TempDir(),
		Tools:    []rig.Tool{&shutdownProbeTool{history: newShutdownHistory()}},
		Storage:  storage,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := shutdownContext(t)
	firstErr := a.Shutdown(ctx)
	if firstErr == nil {
		t.Fatal("第一次 Shutdown 应当把存储的关闭错误报出来")
	}
	if !strings.Contains(firstErr.Error(), "boom") {
		t.Fatalf("第一次 Shutdown 错误 = %v, 期望包含底层错误", firstErr)
	}
	if err := a.Shutdown(ctx); err != nil {
		t.Fatalf("重试 Shutdown: %v", err)
	}
	if got := storage.closeCalls(); got != 2 {
		t.Fatalf("Storage.Close 调用次数 = %d, want 2（第一次失败后必须重试）", got)
	}
	// 关停状态不因为一次失败的回滚：新的回合仍然必须被拒。
	if _, err := a.Run(ctx, RunRequest{SessionID: "s", Input: []rig.Message{shutdownUserMessage("hi")}}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("失败重试之后 Run 的错误 = %v, want ErrShuttingDown", err)
	}
}

// TestNewRegistersFullRuntimeBackgroundJobs 是"硬阻塞"的守卫：full.All 过去把
// jobs.Manager / terminal.Manager 创建后丢掉，生产路径上没有任何 CloseAll 调用。
// kit 现在必须把 full.NewRuntime 交出的两个句柄登记进 Shutdown。
func TestNewRegistersFullRuntimeBackgroundJobs(t *testing.T) {
	a, err := New(Config{
		Model:    "m",
		Provider: &shutdownScriptProvider{},
		Root:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.life == nil {
		t.Fatal("kit.New 没有建立生命周期状态：Shutdown 将无处取消/无句柄可关")
	}
	if got := len(a.life.background); got < 2 {
		t.Fatalf("登记的后台句柄 = %d, want ≥2（jobs.Manager + terminal.Manager）", got)
	}
	if err := a.Shutdown(shutdownContext(t)); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
