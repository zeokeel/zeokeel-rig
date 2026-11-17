package secrets

import (
	"io"
	"strings"
	"testing"
)

// 本文件钉住第二轮审计里"刻意不修、只文档化"的两条边界，防止后人误以为漏了。

// TestRedactJSONNumericSecretUnderSensitiveKeyIsPreserved 钉住 L4 取舍：
// 敏感键下**数字型**的值不会被脱敏。
//
// 理由：RedactJSON 的契约是"结构保留，只改字符串值"。把数字改成字符串
// "[REDACTED]" 会破坏调用方的 JSON 类型契约（下游按 schema 解析会失败），
// 而改写数字为 0 之类同样改变语义。因此这里刻意保持原值，并在 doc.go 的
// "已知边界"里写明，配套建议是"数字型凭据请用 Add 登记到文本路径，
// 或让上游序列化成字符串"。
func TestRedactJSONNumericSecretUnderSensitiveKeyIsPreserved(t *testing.T) {
	r := New()
	payload := []byte(`{"pin":123456,"password":"hunter2hunter2","flag":true,"api_key":null}`)
	out, err := r.RedactJSON(payload)
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	got := string(out)
	// 字符串型秘密必须被抹掉。
	if strings.Contains(got, "hunter2hunter2") {
		t.Fatalf("字符串型密码泄露: %s", got)
	}
	// 数字/布尔/null 逐字节保留（这是刻意取舍，不是漏修）。
	for _, want := range []string{`"pin":123456`, `"flag":true`, `"api_key":null`} {
		if !strings.Contains(got, want) {
			t.Fatalf("非字符串值被改写（会破坏 JSON 契约）: want %s in %s", want, got)
		}
	}
}

// TestWriterOversizeSingleLineRedactsButDelays 钉住 MaxBuffered 的行为：
// 一条没有换行、远超 64 KiB 的单行会被**强制冲刷**（因此有延迟、不是实时），
// 但已经放出去的部分里绝不含完整凭据，未决缓冲也始终不超过 MaxBuffered。
func TestWriterOversizeSingleLineRedactsButDelays(t *testing.T) {
	secret := "sk-" + strings.Repeat("a", 48)
	var dst strings.Builder
	w := New(secret).Writer(&dst)

	// 一行 160 KiB：开头是秘密，后面全是填充（没有换行可借来判定边界）。
	line := secret + " " + strings.Repeat("x", 160<<10)
	if _, err := io.WriteString(w, line); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 延迟：超长单行不会在 Write 返回前全部吐出（尾部窗口被扣留）。
	if dst.Len() == 0 {
		t.Fatal("超长单行一个字都没输出：强制冲刷分支没有生效")
	}
	if dst.Len() >= len(line) {
		t.Fatalf("超长单行被完整输出（%d 字节），说明没有扣留尾部窗口", dst.Len())
	}
	// 不泄露：已输出的内容里不能出现秘密。
	if strings.Contains(dst.String(), secret) {
		t.Fatalf("强制冲刷泄露了凭据: %q", dst.String()[:80])
	}
	// 上界：未决缓冲不超过 MaxBuffered。
	if pending := w.(*redactingWriter).pending(); pending > MaxBuffered {
		t.Fatalf("未决缓冲 %d 超过上限 %d", pending, MaxBuffered)
	}

	// Flush 后剩余部分同样脱敏，且总量等于输入长度。
	if err := w.(Flusher).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	final := dst.String()
	if strings.Contains(final, secret) {
		t.Fatal("Flush 后仍然泄露凭据")
	}
	if !strings.Contains(final, "[REDACTED") {
		t.Fatal("输出里没有占位符，秘密可能被整段吞掉")
	}
	if len(final) != len(line)-(len(secret)-len("[REDACTED]")) {
		t.Fatalf("输出长度 %d 与输入 %d 不匹配（脱敏应只替换、不丢字节）", len(final), len(line))
	}
}
