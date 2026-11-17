package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// errNilWriter 是 NewEmitter(nil) 的粘性写错误。
//
// 文档承诺"w 为 nil 时所有写入都会失败（fail closed，不会 panic）"：nil 的
// io.Writer 接口被调用时会 panic，因此必须在写入前显式拦下。
var errNilWriter = errors.New("events: emitter 的 writer 为 nil")

// maxRetainedBuffer 是 emitter 允许长期占用的编码缓冲上限。
//
// 一行合法事件最多 MaxLineBytes 字节（含换行 MaxLineBytes+1），所以比它更大的
// 缓冲永远不会被有效复用；一条被拒绝的巨型 payload 不应把内存永久钉在 emitter 上。
const maxRetainedBuffer = MaxLineBytes + 1

// Emitter 把事件串行写成 NDJSON（一行一个 Envelope），并发安全。
//
// 语义：
//   - Emit / EmitEnvelope 在同一个互斥区内完成"取号 + 编码 + 单次 Write"，
//     因此并发调用时 seq 严格递增、每行完整（不会交织）。
//   - Version 总被写成 Version；Seq 由 emitter 赋值（调用方给的值会被覆盖）；
//     Time 为零值时取当前时刻，并一律规范成 UTC。
//   - 写失败后进入"已坏"状态：后续调用直接返回同一个错误，不再写字节，
//     Err() 也返回该错误。失败的那一条不占号，Seq() 只统计成功写出的条数。
//   - 单行超过 MaxLineBytes 时返回 ErrLineTooLong 且**不写任何字节**；这不是
//     写失败，emitter 仍然可用。
//
// 本类型不做缓冲（不包 bufio）：调用方需要批量写时自己套 bufio.Writer，但要
// 注意 Emitter 保证的"单行原子"只到自己的 Write 调用为止。
type Emitter struct {
	mu  sync.Mutex
	w   io.Writer
	buf bytes.Buffer
	enc *json.Encoder
	seq int64
	err error
}

// NewEmitter 返回写向 w 的 emitter。w 为 nil 时所有写入都会失败（fail closed，
// 不会 panic）。
func NewEmitter(w io.Writer) *Emitter {
	e := &Emitter{w: w}
	e.enc = json.NewEncoder(&e.buf)
	// 关闭 HTML 转义：这是面向第三方前端与日志的线格式，"a<b&c" 应保持可读，
	// 而不是变成 \u003cb\u0026c。
	e.enc.SetEscapeHTML(false)
	return e
}

// Emit 编码一条事件。payload 为 nil 时省略 payload 字段；其他值用 encoding/json
// 编码（不做 HTML 转义，因此与 json.Marshal 在含 <>& 时字节不同）。
func (e *Emitter) Emit(kind string, payload any) error {
	var raw json.RawMessage
	if payload != nil {
		b, err := marshalPayload(payload)
		if err != nil {
			return err
		}
		raw = b
	}
	return e.EmitEnvelope(Envelope{Kind: kind, Payload: raw})
}

// EmitEnvelope 写出一条完整外壳。Version/Seq 由 emitter 赋值，Time 为零值时取
// 当前时刻；Payload 若是调用方给的原始 JSON，会交由 encoding/json 规范化后写出：
// 压缩空白、并用 \u2028/\u2029 转义行/段分隔符（stdlib 对 RawMessage 也如此，
// 见 audit_events_test.go 的 F20）。除这几种规范化外，payload 的 JSON 字节不变。
func (e *Emitter) EmitEnvelope(env Envelope) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	if e.w == nil {
		e.err = errNilWriter
		return e.err
	}

	env.Version = Version
	env.Seq = e.seq + 1
	if env.Time.IsZero() {
		env.Time = time.Now()
	}
	env.Time = normalizeTime(env.Time)

	e.buf.Reset()
	if err := e.enc.Encode(env); err != nil {
		// 编码失败（例如 Payload 不是合法 JSON）不是写失败：不置坏，调用方
		// 修好输入后仍可继续写。
		return fmt.Errorf("events: 编码事件: %w", err)
	}
	line := e.buf.Bytes()
	if len(line) > MaxLineBytes+1 { // +1 是行尾换行
		e.releaseBuffer()
		return fmt.Errorf("%w: %d 字节", ErrLineTooLong, len(line)-1)
	}
	n, err := e.w.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		e.err = err
		return err
	}
	e.seq = env.Seq
	return nil
}

// releaseBuffer 丢弃过大的编码缓冲（连同绑定它的一次性编码器）。
//
// 只在缓冲超过 maxRetainedBuffer 时发生：正常行永远不会触发，因此不影响稳态性能。
func (e *Emitter) releaseBuffer() {
	if e.buf.Cap() <= maxRetainedBuffer {
		return
	}
	e.buf = bytes.Buffer{}
	e.enc = json.NewEncoder(&e.buf)
	e.enc.SetEscapeHTML(false)
}

// Seq 返回已成功写出的最后一条事件的序号（未写过时为 0）。
func (e *Emitter) Seq() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seq
}

// Err 返回导致 emitter 进入"已坏"状态的写错误；健康时返回 nil。
func (e *Emitter) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// marshalPayload 编码 payload，返回不含行尾换行的紧凑 JSON。
//
// 用编码器而不是 json.Marshal 是为了关掉 HTML 转义，使线格式对文本友好且与
// 快照稳定一致。
func marshalPayload(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("events: 编码 payload: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
