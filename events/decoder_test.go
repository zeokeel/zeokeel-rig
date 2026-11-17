package events_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/events"
)

// 本文件是 Decoder/Replay 的行为测试：回放一致性、坏输入、版本闸门、未知 kind 透传。

// 写→读回：逐字段相等，payload 原始字节完全相同。
func TestRoundtripPreservesEveryField(t *testing.T) {
	var buf bytes.Buffer
	em := events.NewEmitter(&buf)

	want := []events.Envelope{
		{
			Time:    fixedBase,
			Kind:    events.KindRunStart,
			Payload: rawJSON(t, events.RunStart{Model: "deepseek-v4.1-flash", Provider: "deepseek", Workspace: "/w"}),
		},
		{
			Time:    fixedBase.Add(1500 * time.Microsecond),
			Kind:    events.KindToolResult,
			Payload: rawJSON(t, events.ToolResult{ID: "c1", Name: "bash", Content: "ok\n", DurationMS: 7}),
		},
		{
			Time: fixedBase.Add(2 * time.Second),
			Kind: "vendor.custom.kind",
			// 第三方扩展的未知 kind 也必须原样保留。
			Payload: rawJSON(t, struct {
				Extra string `json:"extra"`
			}{Extra: "保留"}),
		},
	}
	for _, env := range want {
		if err := em.EmitEnvelope(env); err != nil {
			t.Fatalf("EmitEnvelope: %v", err)
		}
	}

	dec := events.NewDecoder(bytes.NewReader(buf.Bytes()))
	for i, w := range want {
		got, err := dec.Next()
		if err != nil {
			t.Fatalf("第 %d 条 Next: %v", i+1, err)
		}
		if got.Version != events.Version {
			t.Fatalf("第 %d 条 version = %d, want %d", i+1, got.Version, events.Version)
		}
		if got.Seq != int64(i+1) {
			t.Fatalf("第 %d 条 seq = %d, want %d", i+1, got.Seq, i+1)
		}
		if !got.Time.Equal(w.Time) {
			t.Fatalf("第 %d 条 time = %v, want %v", i+1, got.Time, w.Time)
		}
		if got.Time.Location() != time.UTC {
			t.Fatalf("第 %d 条 time 时区 = %v, want UTC", i+1, got.Time.Location())
		}
		if got.Kind != w.Kind {
			t.Fatalf("第 %d 条 kind = %q, want %q", i+1, got.Kind, w.Kind)
		}
		if !bytes.Equal(got.Payload, w.Payload) {
			t.Fatalf("第 %d 条 payload = %q, want %q", i+1, got.Payload, w.Payload)
		}
	}
	if _, err := dec.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("流末 Next err = %v, want io.EOF", err)
	}
}

// 空行与 CRLF 必须被容忍。
func TestDecoderSkipsBlankLinesAndAcceptsCRLF(t *testing.T) {
	data := strings.Join([]string{
		"",
		`{"version":1,"seq":1,"time":"2026-11-17T12:00:00Z","kind":"notice"}`,
		"   ",
		"\t",
		`{"version":1,"seq":2,"time":"2026-11-17T12:00:01Z","kind":"error","payload":{"stage":"s","message":"m"}}`,
		"",
	}, "\r\n")

	got := readAll(t, []byte(data))
	if len(got) != 2 {
		t.Fatalf("解码出 %d 条, want 2", len(got))
	}
	if got[1].Seq != 2 || got[1].Kind != events.KindError {
		t.Fatalf("第二条 = %+v", got[1])
	}
}

// 坏 JSON、截断行都必须返回 ErrMalformed（而不是静默跳过或 panic）。
func TestDecoderRejectsBadAndTruncatedJSON(t *testing.T) {
	cases := map[string]string{
		"坏 JSON":   "{\"version\":1,\"seq\":1,\"kind\":}\n",
		"纯文本":      "hello world\n",
		"截断行(无换行)": `{"version":1,"seq":2,"kind":"notice"`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			dec := events.NewDecoder(strings.NewReader(data))
			_, err := dec.Next()
			if err == nil {
				t.Fatal("Next 成功了, want 错误")
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("Next err = io.EOF, want 解析错误")
			}
			if !errors.Is(err, events.ErrMalformed) {
				t.Fatalf("Next err = %v, want ErrMalformed", err)
			}
		})
	}
}

// 超长行返回 ErrLineTooLong。
func TestDecoderRejectsOversizeLine(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"version":1,"seq":1,"kind":"notice","payload":{"message":"`)
	buf.Write(bytes.Repeat([]byte("x"), events.MaxLineBytes+8))
	buf.WriteString("\"}}\n")

	dec := events.NewDecoder(bytes.NewReader(buf.Bytes()))
	_, err := dec.Next()
	if !errors.Is(err, events.ErrLineTooLong) {
		t.Fatalf("Next err = %v, want ErrLineTooLong", err)
	}
}

// 版本闸门：更大的版本必须明确拒绝，而不是猜测字段。
func TestDecoderRejectsNewerVersion(t *testing.T) {
	for _, line := range []string{
		`{"version":2,"seq":1,"kind":"notice"}`,
		`{"version":0,"seq":1,"kind":"notice"}`,
		`{"seq":1,"kind":"notice"}`,
	} {
		dec := events.NewDecoder(strings.NewReader(line + "\n"))
		_, err := dec.Next()
		if !errors.Is(err, events.ErrUnsupportedVersion) {
			t.Fatalf("%q: Next err = %v, want ErrUnsupportedVersion", line, err)
		}
	}

	// 同版本必须通过。
	dec := events.NewDecoder(strings.NewReader(`{"version":1,"seq":9,"kind":"notice"}`))
	env, err := dec.Next()
	if err != nil {
		t.Fatalf("版本 1 Next: %v", err)
	}
	if env.Seq != 9 {
		t.Fatalf("seq = %d, want 9", env.Seq)
	}
}

// Replay：未知 kind 继续透传；fn 的错误原样返回并中断。
func TestReplayUnknownKindAndCallbackError(t *testing.T) {
	data := strings.Join([]string{
		`{"version":1,"seq":1,"time":"2026-11-17T12:00:00Z","kind":"vendor.future","payload":{"a":1}}`,
		`{"version":1,"seq":2,"time":"2026-11-17T12:00:01Z","kind":"notice"}`,
		`{"version":1,"seq":3,"time":"2026-11-17T12:00:02Z","kind":"notice"}`,
	}, "\n") + "\n"

	var kinds []string
	if err := events.Replay(strings.NewReader(data), func(env events.Envelope) error {
		kinds = append(kinds, env.Kind)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(kinds) != 3 || kinds[0] != "vendor.future" {
		t.Fatalf("kinds = %v", kinds)
	}

	stop := errors.New("调用方要求停止")
	seen := 0
	err := events.Replay(strings.NewReader(data), func(env events.Envelope) error {
		seen++
		if seen == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Replay err = %v, want %v", err, stop)
	}
	if seen != 2 {
		t.Fatalf("回调被调用 %d 次, want 2（第二行就应中断）", seen)
	}

	// 解码错误同样中断并返回。
	err = events.Replay(strings.NewReader("not json\n"), func(events.Envelope) error { return nil })
	if !errors.Is(err, events.ErrMalformed) {
		t.Fatalf("Replay(坏 JSON) err = %v, want ErrMalformed", err)
	}
}

// 空输入：Replay 直接返回 nil，Decoder 直接返回 io.EOF。
func TestEmptyStream(t *testing.T) {
	if err := events.Replay(strings.NewReader(""), func(events.Envelope) error {
		t.Fatal("空流不应调用回调")
		return nil
	}); err != nil {
		t.Fatalf("Replay(\"\") = %v, want nil", err)
	}
	dec := events.NewDecoder(strings.NewReader("\n\n\n"))
	if _, err := dec.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next = %v, want io.EOF", err)
	}
}

// 版本常量与 kind 字符串属于线格式契约，必须逐字面量钉住。
func TestWireContractConstants(t *testing.T) {
	if events.Version != 1 {
		t.Fatalf("Version = %d, want 1（不兼容变更必须 +1）", events.Version)
	}
	if events.MaxLineBytes != 1<<20 {
		t.Fatalf("MaxLineBytes = %d, want 1MiB", events.MaxLineBytes)
	}
	kinds := map[string]string{
		events.KindRunStart:         "run.start",
		events.KindRunEnd:           "run.end",
		events.KindMessage:          "message",
		events.KindMessageDelta:     "message.delta",
		events.KindReasoning:        "reasoning.delta",
		events.KindToolCall:         "tool.call",
		events.KindToolResult:       "tool.result",
		events.KindApprovalRequest:  "approval.request",
		events.KindApprovalResponse: "approval.response",
		events.KindDiff:             "diff",
		events.KindNotice:           "notice",
		events.KindError:            "error",
	}
	for got, want := range kinds {
		if got != want {
			t.Fatalf("kind 常量 = %q, want %q（线格式契约）", got, want)
		}
	}
	if len(kinds) != 12 {
		t.Fatalf("kind 常量数量 = %d, want 12", len(kinds))
	}
}

// 终止性错误必须可重复观察：第二次 Next 仍返回同一类错误，绝不静默变成 io.EOF
// （否则"读到一半坏了"会被误判为"流正常结束"）。
func TestDecoderTerminalErrorsAreSticky(t *testing.T) {
	t.Run("超长行", func(t *testing.T) {
		var buf bytes.Buffer
		buf.Write(bytes.Repeat([]byte("x"), events.MaxLineBytes+2))
		buf.WriteString("\n")
		dec := events.NewDecoder(bytes.NewReader(buf.Bytes()))
		for i := 0; i < 2; i++ {
			if _, err := dec.Next(); !errors.Is(err, events.ErrLineTooLong) {
				t.Fatalf("第 %d 次 Next err = %v, want ErrLineTooLong", i+1, err)
			}
		}
	})
	t.Run("版本不兼容", func(t *testing.T) {
		dec := events.NewDecoder(strings.NewReader(`{"version":2,"seq":1,"kind":"notice"}` + "\n"))
		for i := 0; i < 2; i++ {
			if _, err := dec.Next(); !errors.Is(err, events.ErrUnsupportedVersion) {
				t.Fatalf("第 %d 次 Next err = %v, want ErrUnsupportedVersion", i+1, err)
			}
		}
	})
}

// payload 里出现未识别的字段时，原始字节必须完整保留（转发/回放不丢信息）。
func TestDecoderPreservesUnknownPayloadFields(t *testing.T) {
	line := `{"version":1,"seq":1,"time":"2026-11-17T12:00:00Z","kind":"tool.result","payload":{"id":"c1","name":"bash","future_field":[1,2,{"a":null}]},"extra_top":"ignored"}` + "\n"
	dec := events.NewDecoder(strings.NewReader(line))
	env, err := dec.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	want := `{"id":"c1","name":"bash","future_field":[1,2,{"a":null}]}`
	if string(env.Payload) != want {
		t.Fatalf("payload = %q, want %q", env.Payload, want)
	}
}
