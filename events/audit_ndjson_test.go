package events_test

// 审计（只读）：NDJSON 事件流能不能被"任意前端 + 任意版本"安全消费。
//
// 关注点：① 每一行都是合法 JSON（中文/emoji/换行/引号/U+2028 都不能破坏行边界）；
// ② 未知 kind 的前向兼容（透传 payload）；未知 version 必须拒绝；
// ③ 10 万事件写入吞吐与单行超限的原子性。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/events"
)

// TestAuditNDJSONEveryLineIsValidJSONWithUnicode 用最难编码的文本打一遍：中文、
// emoji、换行、制表符、引号、反斜杠、U+2028、HTML 尖括号。
func TestAuditNDJSONEveryLineIsValidJSONWithUnicode(t *testing.T) {
	texts := []string{
		"中文内容，包含全角标点。",
		"emoji: 🚀🧪✅ 组合字符 👨‍👩‍👧‍👦",
		"换行\n第二行\r\n第三行\r第四行",
		`引号 " 与反斜杠 \ 与 \n 字面量`,
		"行分隔符 U+2028: \u2028 段分隔符 U+2029: \u2029",
		"<script>alert(1)</script> & a<b&c",
		"tab\there\u0000nul",
		"",
	}
	var buffer bytes.Buffer
	emitter := events.NewEmitter(&buffer)
	for _, text := range texts {
		if err := emitter.Emit(events.KindMessage, events.Message{Role: "assistant", Text: text}); err != nil {
			t.Fatalf("Emit(%q): %v", text, err)
		}
	}
	blob := buffer.Bytes()
	// 行边界：每条事件恰好一行（内部换行必须被转义）。
	if got := strings.Count(string(blob), "\n"); got != len(texts) {
		t.Fatalf("物理行数 = %d, want %d（说明有 payload 里的换行没被转义）", got, len(texts))
	}
	scanner := bufio.NewScanner(bytes.NewReader(blob))
	scanner.Buffer(make([]byte, 0, 64*1024), events.MaxLineBytes+1)
	index := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if !json.Valid(line) {
			t.Fatalf("第 %d 行不是合法 JSON: %q", index+1, line)
		}
		var env events.Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			t.Fatalf("第 %d 行无法解码: %v", index+1, err)
		}
		if env.Kind != events.KindMessage {
			t.Fatalf("第 %d 行 kind = %q", index+1, env.Kind)
		}
		var payload events.Message
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("第 %d 行 payload 无法解码: %v", index+1, err)
		}
		if payload.Text != texts[index] {
			t.Fatalf("第 %d 行文本往返不一致:\n got %q\nwant %q", index+1, payload.Text, texts[index])
		}
		index++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if index != len(texts) {
		t.Fatalf("解出 %d 行, want %d", index, len(texts))
	}
}

// TestAuditNDJSONUnknownKindForwardCompatible 断言未知 kind 会被透传（前向兼容），
// 而未知 version 会被拒绝（不允许"猜着读"）。
func TestAuditNDJSONUnknownKindForwardCompatible(t *testing.T) {
	var buffer bytes.Buffer
	emitter := events.NewEmitter(&buffer)
	// 直接写一个"未来版本新增的 kind"，payload 是任意 JSON。
	future := json.RawMessage(`{"new_field":[1,2,3],"nested":{"x":"中文"}}`)
	if err := emitter.EmitEnvelope(events.Envelope{Kind: "future.thing", Payload: future}); err != nil {
		t.Fatalf("EmitEnvelope: %v", err)
	}
	if err := emitter.Emit(events.KindNotice, map[string]string{"msg": "after future"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var kinds []string
	var payloads []string
	if err := events.Replay(bytes.NewReader(buffer.Bytes()), func(env events.Envelope) error {
		kinds = append(kinds, env.Kind)
		payloads = append(payloads, string(env.Payload))
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(kinds) != 2 || kinds[0] != "future.thing" || kinds[1] != events.KindNotice {
		t.Fatalf("Replay kinds = %v", kinds)
	}
	// payload 原样保留（键序可能被规范化，因此按解析后的值比较）。
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payloads[0]), &decoded); err != nil {
		t.Fatalf("未知 kind 的 payload 被破坏: %v（%q）", err, payloads[0])
	}
	if _, ok := decoded["new_field"]; !ok {
		t.Fatalf("未知 kind 的 payload 丢了字段: %v", decoded)
	}

	// 未知 version：必须报 ErrUnsupportedVersion，且后续 Next 反复返回同一错误。
	higher := []byte(`{"version":99,"seq":1,"time":"2026-11-18T00:00:00Z","kind":"future.thing"}` + "\n")
	decoder := events.NewDecoder(bytes.NewReader(higher))
	if _, err := decoder.Next(); err == nil || !strings.Contains(err.Error(), "版本") {
		t.Fatalf("更高 version 应当被拒绝，实际 err=%v", err)
	}
	_, firstErr := decoder.Next()
	_, secondErr := decoder.Next()
	if firstErr == nil || secondErr == nil || firstErr.Error() != secondErr.Error() {
		t.Fatalf("未知 version 的错误不是粘性的: %v / %v", firstErr, secondErr)
	}
}

// TestAuditNDJSONOversizedLineIsRejectedAtomically 断言超过单行上限的事件被拒绝，
// 且不写出任何字节（不破坏行边界）。
func TestAuditNDJSONOversizedLineIsRejectedAtomically(t *testing.T) {
	var buffer bytes.Buffer
	emitter := events.NewEmitter(&buffer)
	if err := emitter.Emit(events.KindNotice, map[string]string{"msg": "small"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	before := buffer.Len()
	huge := strings.Repeat("x", events.MaxLineBytes)
	if err := emitter.Emit(events.KindNotice, map[string]string{"msg": huge}); err == nil {
		t.Fatal("超长事件应当被拒绝")
	}
	if buffer.Len() != before {
		t.Fatalf("超长事件写出了 %d 字节（应当 0）", buffer.Len()-before)
	}
	if emitter.Seq() != 1 {
		t.Fatalf("Seq = %d, want 1（失败的事件不占号）", emitter.Seq())
	}
	// emitter 仍然可用。
	if err := emitter.Emit(events.KindNotice, map[string]string{"msg": "after"}); err != nil {
		t.Fatalf("超长事件之后 emitter 应当仍可用: %v", err)
	}
	if emitter.Seq() != 2 {
		t.Fatalf("Seq = %d, want 2", emitter.Seq())
	}
}

// TestAuditNDJSONThroughput100k 写 10 万事件并解码回来，报告吞吐。
func TestAuditNDJSONThroughput100k(t *testing.T) {
	const count = 100_000
	// 写：直连 bytes.Buffer（不做磁盘 IO，纯编码+序列化成本）。
	var buffer bytes.Buffer
	emitter := events.NewEmitter(&buffer)
	start := time.Now()
	for i := 0; i < count; i++ {
		if err := emitter.Emit(events.KindToolResult, events.ToolResult{
			ID: "call_1", Name: "read_file", Content: "内容包含中文与 emoji 🚀，长度适中。",
		}); err != nil {
			t.Fatalf("Emit #%d: %v", i, err)
		}
	}
	writeElapsed := time.Since(start)
	if emitter.Seq() != count {
		t.Fatalf("Seq = %d, want %d", emitter.Seq(), count)
	}

	// 读：完整解码一遍。
	start = time.Now()
	var decoded int
	if err := events.Replay(bytes.NewReader(buffer.Bytes()), func(events.Envelope) error {
		decoded++
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	readElapsed := time.Since(start)
	if decoded != count {
		t.Fatalf("解出 %d 条, want %d", decoded, count)
	}
	t.Logf("10 万事件：写 %v（%.0f ns/条，%.1f 万条/秒），读 %v（%.0f ns/条）",
		writeElapsed, float64(writeElapsed.Nanoseconds())/count, 1e4/float64(writeElapsed.Microseconds())*1e6/1e4,
		readElapsed, float64(readElapsed.Nanoseconds())/count)
	if auditRaceEnabled {
		// -race 下编码/解码慢 5–12 倍，只记录不判定（门槛是非 race 构建的产品指标）。
		t.Logf("-race 构建：跳过吞吐门槛判定（写 %v / 读 %v）", writeElapsed, readElapsed)
		return
	}
	if writeElapsed > 3*time.Second {
		t.Fatalf("写入 10 万事件耗时 %v（>3s），吞吐不足", writeElapsed)
	}
	if readElapsed > 3*time.Second {
		t.Fatalf("解码 10 万事件耗时 %v（>3s）", readElapsed)
	}
}

// TestAuditNDJSONTruncatedTailIsMalformed 断言截断的最后一行报 ErrMalformed 而不是
// 被当成合法事件（否则 UI 会显示半条记录）。
func TestAuditNDJSONTruncatedTailIsMalformed(t *testing.T) {
	var buffer bytes.Buffer
	emitter := events.NewEmitter(&buffer)
	if err := emitter.Emit(events.KindMessage, events.Message{Role: "assistant", Text: "完整的一条"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	good := buffer.Len()
	// 追加半行（模拟写入过程中崩溃）。
	buffer.WriteString(`{"version":1,"seq":2,"kind":"message","payload":{"role":"assist`)
	var count int
	err := events.Replay(bytes.NewReader(buffer.Bytes()), func(events.Envelope) error {
		count++
		return nil
	})
	if err == nil {
		t.Fatal("截断的行应当报错")
	}
	if count != 1 {
		t.Fatalf("截断前应当解出 1 条，实际 %d", count)
	}
	if !strings.Contains(err.Error(), "JSON") && !strings.Contains(err.Error(), "合法") {
		t.Fatalf("错误应当提示 JSON 非法: %v", err)
	}
	t.Logf("截断行错误（良好）：%v（完整行 %d 字节）", err, good)
}

// TestAuditNDJSONEmitterSeqAndTimeStable 断言 seq 严格递增、时间归一化到 UTC。
func TestAuditNDJSONEmitterSeqAndTimeStable(t *testing.T) {
	var buffer bytes.Buffer
	emitter := events.NewEmitter(&buffer)
	for i := 0; i < 5; i++ {
		if err := emitter.Emit(events.KindNotice, nil); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	location := time.FixedZone("UTC+8", 8*3600)
	emitter2 := events.NewEmitter(io.Discard)
	if err := emitter2.EmitEnvelope(events.Envelope{Kind: events.KindNotice, Time: time.Date(2026, 11, 18, 9, 0, 0, 0, location)}); err != nil {
		t.Fatalf("EmitEnvelope: %v", err)
	}

	var seqs []int64
	if err := events.Replay(bytes.NewReader(buffer.Bytes()), func(env events.Envelope) error {
		seqs = append(seqs, env.Seq)
		if env.Time.Location() != time.UTC {
			t.Fatalf("时间没有归一化到 UTC: %v", env.Time)
		}
		if env.Version != events.Version {
			t.Fatalf("version = %d", env.Version)
		}
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("seq[%d] = %d, want %d", i, seq, i+1)
		}
	}
}
