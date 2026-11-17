package events_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/events"
)

// 本文件是 Emitter 的行为测试：线格式稳定性、payload 省略、并发单调性、
// 以及写失败后的"已坏"状态。

// fixedBase 是 golden/回放测试使用的固定时刻基准（UTC），保证字节可复现。
var fixedBase = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// rawJSON 把值编码成 payload 原始 JSON。它与 Emitter 内部使用同一个编码器配置
// （不做 HTML 转义），因此断言可以逐字节比较。
func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("编码 payload: %v", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// readAll 把缓冲区里的事件全部解码出来。
func readAll(t *testing.T, data []byte) []events.Envelope {
	t.Helper()
	var out []events.Envelope
	if err := events.Replay(bytes.NewReader(data), func(env events.Envelope) error {
		out = append(out, env)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return out
}

// 同一 payload 两次序列化必须逐字节相同（线格式确定性）。
func TestEmitDeterministicBytes(t *testing.T) {
	payload := events.ToolResult{ID: "call-1", Name: "read_file", Content: "a<b&c", DurationMS: 12}

	var first, second bytes.Buffer
	for _, buf := range []*bytes.Buffer{&first, &second} {
		em := events.NewEmitter(buf)
		if err := em.EmitEnvelope(events.Envelope{
			Time:    fixedBase,
			Kind:    events.KindToolResult,
			Payload: rawJSON(t, payload),
		}); err != nil {
			t.Fatalf("EmitEnvelope: %v", err)
		}
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("同一输入两次序列化不一致:\n  %q\n  %q", first.Bytes(), second.Bytes())
	}
	// payload 中的 < & 不能被 HTML 转义（线格式面向第三方前端/日志）。
	if !bytes.Contains(first.Bytes(), []byte("a<b&c")) {
		t.Fatalf("payload 被 HTML 转义: %q", first.Bytes())
	}
}

// Emit 的 seq 从 1 开始单调递增，Time 一律为 UTC。
func TestEmitAssignsSeqAndUTC(t *testing.T) {
	var buf bytes.Buffer
	em := events.NewEmitter(&buf)
	if got := em.Seq(); got != 0 {
		t.Fatalf("初始 Seq() = %d, want 0", got)
	}
	for i := 0; i < 3; i++ {
		if err := em.Emit(events.KindNotice, events.MsgDelta{Role: "assistant", Text: "x"}); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	if got := em.Seq(); got != 3 {
		t.Fatalf("Seq() = %d, want 3", got)
	}

	// 用一个非 UTC 时区的时刻调用 EmitEnvelope，落盘必须是 UTC。
	loc := time.FixedZone("CST", 8*3600)
	if err := em.EmitEnvelope(events.Envelope{
		Time: fixedBase.In(loc),
		Kind: events.KindNotice,
	}); err != nil {
		t.Fatalf("EmitEnvelope: %v", err)
	}
	got := readAll(t, buf.Bytes())
	if len(got) != 4 {
		t.Fatalf("解码出 %d 条, want 4", len(got))
	}
	for i, env := range got {
		if env.Version != events.Version {
			t.Fatalf("第 %d 条 version = %d, want %d", i+1, env.Version, events.Version)
		}
		if env.Seq != int64(i+1) {
			t.Fatalf("第 %d 条 seq = %d, want %d", i+1, env.Seq, i+1)
		}
		if env.Time.Location() != time.UTC {
			t.Fatalf("第 %d 条 time 时区 = %v, want UTC", i+1, env.Time.Location())
		}
	}
	if !got[3].Time.Equal(fixedBase) {
		t.Fatalf("时刻被改动: %v, want %v", got[3].Time, fixedBase)
	}
}

// payload 为 nil 时必须省略 payload 字段（omitempty）。
func TestEmitNilPayloadOmitsField(t *testing.T) {
	var buf bytes.Buffer
	em := events.NewEmitter(&buf)
	if err := em.Emit(events.KindNotice, nil); err != nil {
		t.Fatalf("Emit(nil): %v", err)
	}
	line := buf.String()
	if bytes.Contains([]byte(line), []byte(`"payload"`)) {
		t.Fatalf("nil payload 仍写出 payload 字段: %q", line)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Fatalf("行必须以换行结束: %q", line)
	}
	if bytes.Count(buf.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("一条事件必须恰好一行: %q", line)
	}
}

// 并发 Emit：seq 严格递增且无重复/空洞，每行完整可解析。
func TestEmitConcurrentSeqIsDenseAndLinesIntact(t *testing.T) {
	const goroutines, perGoroutine = 100, 100

	var buf bytes.Buffer
	em := events.NewEmitter(&buf)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := em.Emit(events.KindMessageDelta, events.MsgDelta{Role: "assistant", Text: "增量"}); err != nil {
					t.Errorf("Emit: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := em.Seq(); got != goroutines*perGoroutine {
		t.Fatalf("Seq() = %d, want %d", got, goroutines*perGoroutine)
	}
	got := readAll(t, buf.Bytes())
	if len(got) != goroutines*perGoroutine {
		t.Fatalf("解码出 %d 条, want %d", len(got), goroutines*perGoroutine)
	}
	seen := make(map[int64]struct{}, len(got))
	for _, env := range got {
		if _, dup := seen[env.Seq]; dup {
			t.Fatalf("seq %d 重复", env.Seq)
		}
		seen[env.Seq] = struct{}{}
	}
	for seq := int64(1); seq <= int64(len(got)); seq++ {
		if _, ok := seen[seq]; !ok {
			t.Fatalf("seq %d 空洞", seq)
		}
	}
}

// failWriter 在第 N 次写入后开始失败，用于验证"已坏"状态。
type failWriter struct {
	err   error
	calls int
	after int
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.after {
		return 0, w.err
	}
	return len(p), nil
}

// 写失败后 Err() 必须保持同一个错误，后续 Emit 直接返回该错误且不再写。
func TestEmitterStickyWriteError(t *testing.T) {
	boom := errors.New("磁盘满了")
	w := &failWriter{err: boom, after: 1}
	em := events.NewEmitter(w)

	if err := em.Emit(events.KindNotice, nil); err != nil {
		t.Fatalf("第一次 Emit: %v", err)
	}
	err := em.Emit(events.KindNotice, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("第二次 Emit err = %v, want %v", err, boom)
	}
	if !errors.Is(em.Err(), boom) {
		t.Fatalf("Err() = %v, want %v", em.Err(), boom)
	}
	if got := em.Seq(); got != 1 {
		t.Fatalf("失败后 Seq() = %d, want 1（只统计成功写入）", got)
	}

	calls := w.calls
	if err := em.Emit(events.KindNotice, nil); !errors.Is(err, boom) {
		t.Fatalf("第三次 Emit err = %v, want 粘性 %v", err, boom)
	}
	if w.calls != calls {
		t.Fatalf("已坏之后仍在写: calls %d -> %d", calls, w.calls)
	}
	if em.Err() != err {
		t.Fatalf("Err() 返回了新错误实例: %v", em.Err())
	}
}

// 超长事件必须在写之前被拒绝，且不污染输出。
func TestEmitRejectsOversizeLine(t *testing.T) {
	var buf bytes.Buffer
	em := events.NewEmitter(&buf)
	big := events.Message{Role: "assistant", Text: string(bytes.Repeat([]byte("x"), events.MaxLineBytes))}
	err := em.Emit(events.KindMessage, big)
	if !errors.Is(err, events.ErrLineTooLong) {
		t.Fatalf("Emit(超大) err = %v, want ErrLineTooLong", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("超长事件被写出 %d 字节, want 0", buf.Len())
	}
	if got := em.Seq(); got != 0 {
		t.Fatalf("失败后 Seq() = %d, want 0", got)
	}
	// 超长不是写失败：emitter 不应进入"已坏"状态。
	if err := em.Emit(events.KindNotice, nil); err != nil {
		t.Fatalf("超长之后的小事件 Emit: %v", err)
	}
	if em.Err() != nil {
		t.Fatalf("Err() = %v, want nil（超长不是写失败）", em.Err())
	}
}

// 空 writer（把事件直接丢弃）也要正常工作。
func TestEmitterToDiscard(t *testing.T) {
	em := events.NewEmitter(io.Discard)
	if err := em.Emit(events.KindRunStart, events.RunStart{Model: "m"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if em.Err() != nil {
		t.Fatalf("Err() = %v, want nil", em.Err())
	}
}

// 编码失败（payload 无法序列化、或调用方给的 raw payload 非法）不是写失败：
// 不置坏、不占号，修好输入后仍可继续写。
func TestEmitterEncodingErrorsAreNotSticky(t *testing.T) {
	var buf bytes.Buffer
	em := events.NewEmitter(&buf)

	if err := em.Emit(events.KindNotice, func() {}); err == nil {
		t.Fatal("Emit(func) 成功了, want 编码错误")
	}
	if err := em.EmitEnvelope(events.Envelope{Kind: events.KindNotice, Payload: json.RawMessage("{bad")}); err == nil {
		t.Fatal("EmitEnvelope(非法 raw payload) 成功了, want 编码错误")
	}
	if em.Err() != nil {
		t.Fatalf("Err() = %v, want nil（编码失败不是写失败）", em.Err())
	}
	if got := em.Seq(); got != 0 {
		t.Fatalf("Seq() = %d, want 0（失败不占号）", got)
	}

	if err := em.Emit(events.KindNotice, nil); err != nil {
		t.Fatalf("修好后 Emit: %v", err)
	}
	got := readAll(t, buf.Bytes())
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("解码 = %+v, want 一条 seq=1", got)
	}
}
