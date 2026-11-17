package events_test

// 对抗审计：Emitter / Decoder 的失败路径与"文档承诺 vs 实现"。
//
// 这些用例默认都应该是"作者写的代码里已经成立"的断言；失败的用例就是审计发现。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/events"
)

// auditErrWriter 在第 n 次 Write 开始返回错误，用于验证"写失败后不再写字节"。
type auditErrWriter struct {
	mu    sync.Mutex
	limit int
	calls int
	bytes int
	fail  error
}

func (w *auditErrWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.fail != nil && w.calls > w.limit {
		return 0, w.fail
	}
	w.bytes += len(p)
	return len(p), nil
}

// F1（major）：文档承诺 NewEmitter(nil) "fail closed，不会 panic"，实际直接 panic。
func TestAuditNewEmitterNilWriterFailsClosed(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewEmitter(nil) 的写入 panic 了（文档承诺 fail closed 不 panic）: %v", r)
		}
	}()
	em := events.NewEmitter(nil)
	if err := em.Emit(events.KindNotice, nil); err == nil {
		t.Fatal("写向 nil writer 必须返回错误")
	}
	if err := em.Err(); err == nil {
		t.Fatal("nil writer 的失败必须被记为粘性错误（Err() 非 nil）")
	}
}

// F2（blocker 级风险）：payload 里带换行是否会破坏"一行一条"的线格式。
func TestAuditRawPayloadWithNewlineStaysOneLine(t *testing.T) {
	var buf bytes.Buffer
	em := events.NewEmitter(&buf)
	raw := json.RawMessage("{\n  \"role\": \"user\",\n  \"text\": \"a\\nb\"\n}")
	if err := em.EmitEnvelope(events.Envelope{Kind: events.KindMessage, Payload: raw}); err != nil {
		t.Fatalf("EmitEnvelope: %v", err)
	}
	if n := bytes.Count(buf.Bytes(), []byte("\n")); n != 1 {
		t.Fatalf("输出里出现了 %d 个换行（必须是 1 条事件 = 1 行）:\n%q", n, buf.Bytes())
	}
	var env events.Envelope
	if err := json.Unmarshal(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), &env); err != nil {
		t.Fatalf("输出不是单行合法 JSON: %v", err)
	}
	if !bytes.Contains(env.Payload, []byte(`a\nb`)) {
		t.Fatalf("payload 里的转义换行被破坏: %q", env.Payload)
	}
}

// F3（minor）：单行超限后，emitter 必须仍然可用且不占号。
func TestAuditLineTooLongKeepsEmitterUsable(t *testing.T) {
	var buf bytes.Buffer
	em := events.NewEmitter(&buf)
	if err := em.Emit(events.KindNotice, events.MsgDelta{Text: "ok"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	huge := events.MsgDelta{Text: strings.Repeat("x", events.MaxLineBytes+16)}
	err := em.Emit(events.KindMessage, huge)
	if !errors.Is(err, events.ErrLineTooLong) {
		t.Fatalf("超长 payload 应返回 ErrLineTooLong，得到 %v", err)
	}
	if em.Err() != nil {
		t.Fatalf("超长不是写失败，Err() 必须为 nil，得到 %v", em.Err())
	}
	if got := em.Seq(); got != 1 {
		t.Fatalf("超长事件不得占号：Seq() = %d, want 1", got)
	}
	if err := em.Emit(events.KindNotice, nil); err != nil {
		t.Fatalf("超长之后必须还能继续写: %v", err)
	}
	if got := em.Seq(); got != 2 {
		t.Fatalf("Seq() = %d, want 2（不得跳号）", got)
	}
}

// F4（major）：编码失败不置坏、不占号、Err() 仍为 nil。
func TestAuditEncodeFailureDoesNotPoisonOrConsumeSeq(t *testing.T) {
	var buf bytes.Buffer
	em := events.NewEmitter(&buf)
	if err := em.Emit(events.KindNotice, nil); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	bad := json.RawMessage(`{"unterminated": `)
	if err := em.Emit(events.KindMessage, bad); err == nil {
		t.Fatal("非法 payload 必须返回编码错误")
	}
	if em.Err() != nil {
		t.Fatalf("编码失败不是写失败: Err() = %v", em.Err())
	}
	if got := em.Seq(); got != 1 {
		t.Fatalf("编码失败不得占号: Seq() = %d, want 1", got)
	}
	if err := em.Emit(events.KindNotice, nil); err != nil {
		t.Fatalf("编码失败后必须还能写: %v", err)
	}
	if got := em.Seq(); got != 2 {
		t.Fatalf("Seq() = %d, want 2", got)
	}
}

// F5（major）：写失败后必须"不再写任何字节"，且 Err() 返回同一个错误实例。
func TestAuditStickyWriteErrorStopsAllWrites(t *testing.T) {
	boom := errors.New("磁盘满了")
	w := &auditErrWriter{limit: 1, fail: boom}
	em := events.NewEmitter(w)
	if err := em.Emit(events.KindNotice, nil); err != nil {
		t.Fatalf("第一条应写成功: %v", err)
	}
	err := em.Emit(events.KindNotice, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("第二条应返回底层错误，得到 %v", err)
	}
	for i := 0; i < 5; i++ {
		if got := em.Emit(events.KindNotice, nil); !errors.Is(got, boom) {
			t.Fatalf("第 %d 次重试应返回同一错误，得到 %v", i, got)
		}
		if got, want := em.Err(), boom; got != want {
			t.Fatalf("Err() 必须是同一个错误实例: %v vs %v", got, want)
		}
	}
	w.mu.Lock()
	calls, written := w.calls, w.bytes
	w.mu.Unlock()
	if calls != 2 {
		t.Fatalf("写失败后不应再调用底层 Write：calls = %d, want 2", calls)
	}
	if got := em.Seq(); got != 1 {
		t.Fatalf("失败的条目不占号: Seq() = %d, want 1", got)
	}
	if written != 65 { // 仅第一条事件写入了字节
		t.Logf("已写字节 = %d（仅用于观察，不作断言）", written)
	}
}

// F6（blocker 风险）：200 goroutine 并发，seq 严格递增无重复，且每一行都能解析。
func TestAuditConcurrentSeqStrictlyIncreasing(t *testing.T) {
	var buf bytes.Buffer
	// bytes.Buffer 本身不是并发安全的，用 lockedWriter 把并发安全交给 Emitter 的
	// "单次 Write" 承诺来验证。
	em := events.NewEmitter(&lockedWriter{w: &buf})

	const goroutines, perG = 200, 5
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perG)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < perG; i++ {
				size := r.Intn(4096)
				payload := events.MsgDelta{Role: "assistant", Text: strings.Repeat("y", size)}
				if err := em.Emit(events.KindMessageDelta, payload); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发 Emit 失败: %v", err)
	}

	var seqs []int64
	if err := events.Replay(&lockedReader{b: &buf}, func(env events.Envelope) error {
		seqs = append(seqs, env.Seq)
		return nil
	}); err != nil {
		t.Fatalf("并发写出的事件流无法回放（说明有行交织/损坏）: %v", err)
	}
	if want := goroutines * perG; len(seqs) != want {
		t.Fatalf("事件数 = %d, want %d", len(seqs), want)
	}
	seen := make(map[int64]bool, len(seqs))
	for i, s := range seqs {
		if seen[s] {
			t.Fatalf("seq %d 重复（第 %d 条）", s, i)
		}
		seen[s] = true
		if s != int64(i+1) {
			t.Fatalf("seq 不是严格递增: 第 %d 条的 seq = %d", i, s)
		}
	}
}

// lockedWriter / lockedReader 让 bytes.Buffer 在 Emitter 之外也安全（读侧与写侧分离）。
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type lockedReader struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (l *lockedReader) Read(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Read(p)
}

// F7：Decoder 的版本语义（缺失/0/未来版本一律 ErrUnsupportedVersion，且粘性）。
func TestAuditDecoderVersionSemantics(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"缺失 version", `{"seq":1,"kind":"notice"}`},
		{"version=0", `{"version":0,"seq":1,"kind":"notice"}`},
		{"未来版本", fmt.Sprintf(`{"version":%d,"seq":1,"kind":"notice"}`, events.Version+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := events.NewDecoder(strings.NewReader(tc.line + "\n"))
			_, err := dec.Next()
			if !errors.Is(err, events.ErrUnsupportedVersion) {
				t.Fatalf("Next() = %v, want ErrUnsupportedVersion", err)
			}
			// 粘性：不能退化成 io.EOF，否则调用方会以为"正常结束"。
			if _, err2 := dec.Next(); !errors.Is(err2, events.ErrUnsupportedVersion) {
				t.Fatalf("第二次 Next() = %v, want 同一 ErrUnsupportedVersion", err2)
			}
		})
	}
}

// F8：CRLF / 无尾换行 / 空行 / 未知 kind / 未知字段。
func TestAuditDecoderLineEndingsAndUnknowns(t *testing.T) {
	body := strings.Join([]string{
		`{"version":1,"seq":1,"kind":"notice"}` + "\r\n",
		"\r\n",
		`{"version":1,"seq":2,"kind":"brand.new.kind","payload":{"x":1},"future":true}` + "\r\n",
		`{"version":1,"seq":3,"kind":"notice"}`, // 无尾换行
	}, "")
	var got []events.Envelope
	if err := events.Replay(strings.NewReader(body), func(env events.Envelope) error {
		got = append(got, env)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("事件数 = %d, want 3（空行应跳过）", len(got))
	}
	if got[1].Kind != "brand.new.kind" || string(got[1].Payload) != `{"x":1}` {
		t.Fatalf("未知 kind 必须原样透传: %+v", got[1])
	}
}

// F9：超长行必须终止解析且粘性（不能退化成 EOF，也不能悄悄跳过）。
func TestAuditDecoderOversizeLineIsSticky(t *testing.T) {
	long := `{"version":1,"seq":1,"kind":"notice","payload":"` + strings.Repeat("z", events.MaxLineBytes) + `"}`
	dec := events.NewDecoder(strings.NewReader(long + "\n" + `{"version":1,"seq":2,"kind":"notice"}` + "\n"))
	if _, err := dec.Next(); !errors.Is(err, events.ErrLineTooLong) {
		t.Fatalf("Next() = %v, want ErrLineTooLong", err)
	}
	if _, err := dec.Next(); !errors.Is(err, events.ErrLineTooLong) {
		t.Fatalf("第二次 Next() = %v, want 同一 ErrLineTooLong", err)
	}
}

// F10：正好 1MiB 的行必须能过（边界正确性），1MiB+1 必须拒。
func TestAuditDecoderLineLengthBoundary(t *testing.T) {
	base := `{"version":1,"seq":1,"kind":"notice","payload":""}`
	pad := events.MaxLineBytes - len(base)
	if pad < 0 {
		t.Fatalf("基准行已经超过上限: %d", len(base))
	}
	exact := base[:len(base)-2] + strings.Repeat("p", pad) + `"}`
	if len(exact) != events.MaxLineBytes {
		t.Fatalf("构造错误: %d", len(exact))
	}
	dec := events.NewDecoder(strings.NewReader(exact + "\n"))
	if _, err := dec.Next(); err != nil {
		t.Fatalf("正好 %d 字节的行应接受: %v", events.MaxLineBytes, err)
	}

	over := exact[:len(exact)-2] + "p" + `"}`
	dec = events.NewDecoder(strings.NewReader(over + "\n"))
	if _, err := dec.Next(); !errors.Is(err, events.ErrLineTooLong) {
		t.Fatalf("超过 1 字节必须拒绝，得到 %v", err)
	}
}

// F11：畸形但可解析的 payload（不是 object、是标量、是 null）不应让解码器崩。
func TestAuditDecoderNonObjectPayload(t *testing.T) {
	lines := []string{
		`{"version":1,"seq":1,"kind":"notice","payload":42}`,
		`{"version":1,"seq":2,"kind":"notice","payload":"just a string"}`,
		`{"version":1,"seq":3,"kind":"notice","payload":null}`,
		`{"version":1,"seq":4,"kind":"notice","payload":[1,2,3]}`,
	}
	for i, line := range lines {
		dec := events.NewDecoder(strings.NewReader(line + "\n"))
		env, err := dec.Next()
		if err != nil {
			t.Fatalf("第 %d 行不应让解码器失败: %v", i, err)
		}
		if len(env.Payload) == 0 {
			t.Fatalf("第 %d 行的 payload 应原样保留", i)
		}
	}
}

// F12：坏 JSON 之后解码器仍可继续（作者文档只承诺"返回 ErrMalformed"，这里钉住
// "可恢复"这一行为，避免以后被改成粘性而破坏长流容错）。
func TestAuditDecoderMalformedLineIsRecoverable(t *testing.T) {
	body := `{"version":1,"seq":1,` + "\n" + `{"version":1,"seq":2,"kind":"notice"}` + "\n"
	dec := events.NewDecoder(strings.NewReader(body))
	if _, err := dec.Next(); !errors.Is(err, events.ErrMalformed) {
		t.Fatalf("坏行应返回 ErrMalformed，得到 %v", err)
	}
	env, err := dec.Next()
	if err != nil {
		t.Fatalf("坏行之后应能继续解析: %v", err)
	}
	if env.Seq != 2 {
		t.Fatalf("seq = %d, want 2", env.Seq)
	}
}

// F13：Replay 的 nil 回调必须报错而不是 panic；nil reader 不应 panic。
func TestAuditReplayNilCallback(t *testing.T) {
	if err := events.Replay(strings.NewReader(""), nil); err == nil {
		t.Fatal("nil 回调必须报错")
	}
}

// F13b（nit，钉住现状）：NewDecoder(nil).Next() 与 stdlib 语义一致地 panic
// （bufio 对 nil Reader 不做保护）。文档没有承诺 fail closed，所以这里不修改实现，
// 只用用例把现状钉住：将来若要改成返回错误，请同步更新本用例与文档。
func TestAuditDecoderNilReaderPanicsLikeStdlib(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("现状是 panic；若实现已改成 fail closed，请更新本用例与 doc")
		}
	}()
	_, _ = events.NewDecoder(nil).Next()
}

// F14：Emit 的 Time 必须是 UTC 且零值取当前时刻。
func TestAuditEmitterTimeNormalization(t *testing.T) {
	var buf bytes.Buffer
	em := events.NewEmitter(&buf)
	loc := time.FixedZone("UTC+8", 8*3600)
	local := time.Date(2026, 1, 2, 3, 4, 5, 0, loc)
	if err := em.EmitEnvelope(events.Envelope{Time: local, Kind: events.KindNotice}); err != nil {
		t.Fatalf("EmitEnvelope: %v", err)
	}
	var env events.Envelope
	if err := json.Unmarshal(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), &env); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if env.Time.Location() != time.UTC {
		t.Fatalf("Time 未规范成 UTC: %v", env.Time)
	}
	if !env.Time.Equal(local) {
		t.Fatalf("规范化改变了时刻: %v vs %v", env.Time, local)
	}
}

// jsonEqual 判断两段 JSON 是否语义相等（数字按字面比较，不转 float64）。
func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	dec := func(s string) any {
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatalf("decode %q: %v", s, err)
		}
		return v
	}
	return reflect.DeepEqual(dec(a), dec(b))
}

// F18（fuzz）：任意字节流都不能让 Decoder panic，返回的错误必须是已声明哨兵之一
// （或 io.EOF / 底层 IO 错误）。
func FuzzAuditDecoderNeverPanics(f *testing.F) {
	f.Add([]byte(`{"version":1,"seq":1,"kind":"notice"}` + "\n"))
	f.Add([]byte(`{"version":1,"seq":1,"kind":"notice","payload":{"a":1}}`))
	f.Add([]byte("{\"version\":1,\"seq\"\n\n\r\n"))
	f.Add([]byte(`{"version":2}`))
	f.Add([]byte{0xff, 0xfe, '\n'})
	f.Fuzz(func(t *testing.T, data []byte) {
		dec := events.NewDecoder(bytes.NewReader(data))
		for i := 0; i < 64; i++ {
			env, err := dec.Next()
			if err != nil {
				switch {
				case errors.Is(err, io.EOF),
					errors.Is(err, events.ErrMalformed),
					errors.Is(err, events.ErrUnsupportedVersion),
					errors.Is(err, events.ErrLineTooLong):
					return
				default:
					t.Fatalf("未声明的错误类别: %v", err)
				}
			}
			if env.Version != events.Version {
				t.Fatalf("成功返回的事件版本必须是 %d，得到 %d", events.Version, env.Version)
			}
			if env.Time.Location() != time.UTC {
				t.Fatalf("Time 必须是 UTC: %v", env.Time)
			}
		}
	})
}

// F19（fuzz）：EmitEnvelope 成功时，输出必须恰好一行、且能解码回同样的外壳
// （payload 字节原样、kind/seq/time 一致）。
func FuzzAuditEmitRoundTrip(f *testing.F) {
	f.Add(`{"a":1}`)
	f.Add(`"str"`)
	f.Add(`[1,2,3]`)
	f.Add(`null`)
	f.Fuzz(func(t *testing.T, payload string) {
		if !json.Valid([]byte(payload)) {
			return
		}
		var buf bytes.Buffer
		em := events.NewEmitter(&buf)
		env := events.Envelope{
			Time:    time.Unix(0, 1_700_000_000_123_456_789).UTC(),
			Kind:    "audit.kind",
			Payload: json.RawMessage(payload),
		}
		if err := em.EmitEnvelope(env); err != nil {
			if !errors.Is(err, events.ErrLineTooLong) {
				t.Fatalf("合法 payload 只可能因超长失败，得到 %v", err)
			}
			return
		}
		out := buf.Bytes()
		if bytes.Count(out, []byte("\n")) != 1 || !bytes.HasSuffix(out, []byte("\n")) {
			t.Fatalf("输出不是恰好一行: %q", out)
		}
		var back events.Envelope
		if err := json.Unmarshal(bytes.TrimSuffix(out, []byte("\n")), &back); err != nil {
			t.Fatalf("输出无法解码: %v (%q)", err, out)
		}
		if back.Kind != env.Kind || back.Seq != 1 || !back.Time.Equal(env.Time) {
			t.Fatalf("外壳字段不一致: %+v vs %+v", back, env)
		}
		// stdlib 会对 RawMessage 做规范化（压缩空白 + 转义 U+2028/2029），
		// 因此这里断言的是 JSON 语义相等，而不是字面字节相等（见 F20）。
		if !jsonEqual(t, payload, string(back.Payload)) {
			t.Fatalf("payload 语义不一致: 输入 %q 回读 %q", payload, back.Payload)
		}
	})
}

// F20（minor，文档 vs 实现）：payload 并不是逐字节原样写出。
//
// stdlib 的 encoding/json 在编码 RawMessage 时会压缩空白并把 U+2028/U+2029 转义
// （Go 1.27 实测；对象 payload 里的这两个字符同样被转义）。作者原注释写的是
// "原样写出（只压缩空白）"，与实现不符——本用例把真实行为钉住，注释已同步修正。
func TestAuditPayloadNormalizationIsStdlibShaped(t *testing.T) {
	var buf bytes.Buffer
	em := events.NewEmitter(&buf)
	raw := json.RawMessage("{\n  \"a\": \"\u2029\"\n}")
	if err := em.EmitEnvelope(events.Envelope{Kind: events.KindMessage, Payload: raw}); err != nil {
		t.Fatalf("EmitEnvelope: %v", err)
	}
	line := buf.String()
	if !strings.Contains(line, `\u2029`) {
		t.Fatalf("期望 U+2029 被转义成 \\u2029（stdlib 行为），实际: %q", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("payload 里的空白没有被压缩成单行: %q", line)
	}
	var back events.Envelope
	if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &back); err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if !jsonEqual(t, string(raw), string(back.Payload)) {
		t.Fatalf("规范化改变了 JSON 语义: %q vs %q", raw, back.Payload)
	}
}
