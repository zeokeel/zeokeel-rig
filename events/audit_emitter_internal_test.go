package events

// 对抗审计（包内）：Emitter 内部缓冲的内存保留上限。
//
// 放在包内是因为要直接看 e.buf 的容量——外部测试只能靠 runtime.MemStats 猜，容易 flaky。

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// F15（minor）：一条被拒绝的巨型 payload 不应把编码缓冲永久留在 emitter 上。
//
// 未修复时：Emit 一个 8MiB payload 会先在内部缓冲里编码完整一行，再判定超长返回
// ErrLineTooLong——缓冲容量被保留到 emitter 结束，长生命周期 emitter（会话日志）
// 就因为一次巨型消息永久多占 8MiB。
func TestAuditHugeRejectedPayloadDoesNotRetainBuffer(t *testing.T) {
	em := NewEmitter(&bytes.Buffer{})
	huge := MsgDelta{Text: strings.Repeat("x", 8<<20)}
	err := em.Emit(KindMessage, huge)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("Emit(8MiB) = %v, want ErrLineTooLong", err)
	}
	if got := em.buf.Cap(); got > maxRetainedBuffer {
		t.Fatalf("被拒绝的巨型 payload 仍占着 %d 字节编码缓冲（上限 %d）", got, maxRetainedBuffer)
	}
	// 修复后 emitter 仍必须可用，且 seq 不跳号。
	if err := em.Emit(KindNotice, nil); err != nil {
		t.Fatalf("超长之后必须还能写: %v", err)
	}
	if got := em.Seq(); got != 1 {
		t.Fatalf("Seq() = %d, want 1", got)
	}
}

// F16：正常（合法）行不应触发缓冲释放，稳态不产生额外分配。
func TestAuditNormalLinesKeepBuffer(t *testing.T) {
	em := NewEmitter(&bytes.Buffer{})
	for i := 0; i < 3; i++ {
		if err := em.Emit(KindNotice, MsgDelta{Text: "ok"}); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	if em.buf.Cap() == 0 {
		t.Fatal("正常写入后缓冲被清空，说明 releaseBuffer 的阈值算错了")
	}
	if em.buf.Cap() > maxRetainedBuffer {
		t.Fatalf("正常行的缓冲不应超过上限: %d", em.buf.Cap())
	}
}

// F17：nil writer 的粘性错误必须是同一个实例，且后续调用不再尝试写。
func TestAuditNilWriterIsSticky(t *testing.T) {
	em := NewEmitter(nil)
	first := em.Emit(KindNotice, nil)
	if !errors.Is(first, errNilWriter) {
		t.Fatalf("Emit = %v, want errNilWriter", first)
	}
	second := em.Emit(KindNotice, nil)
	if second != first {
		t.Fatalf("nil writer 的错误必须粘性且同实例: %v vs %v", first, second)
	}
	if err := em.Err(); err != first {
		t.Fatalf("Err() = %v, want %v", err, first)
	}
	if got := em.Seq(); got != 0 {
		t.Fatalf("Seq() = %d, want 0", got)
	}
}
