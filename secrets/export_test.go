package secrets_test

// RedactForExport 的契约：导出口径与 Redactor 完全一致，但每次独立、无状态。

import (
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/secrets"
)

// TestRedactForExportCoversPatternsAndKnown 覆盖两类判定：内置模式 + 调用方给的
// 已知明文（包括不符合任何内置模式的自定义 token）。
func TestRedactForExportCoversPatternsAndKnown(t *testing.T) {
	const known = "internal-token-9f3a2b7c1d"
	text := strings.Join([]string{
		"openai: sk-abcdefghijklmnopqrstuvwxyz012345",
		"github: ghp_0123456789abcdefghijklmnopqrstuvwx",
		"internal: " + known,
		"plain prose that must survive: hello world",
	}, "\n")

	cleaned := secrets.RedactForExport(text, known)
	for _, leaked := range []string{"sk-abcdefghijklmnopqrstuvwxyz012345", "ghp_0123456789abcdefghijklmnopqrstuvwx", known} {
		if strings.Contains(cleaned, leaked) {
			t.Fatalf("导出文本仍然包含 %q:\n%s", leaked, cleaned)
		}
	}
	if !strings.Contains(cleaned, "[REDACTED") {
		t.Fatalf("没有出现占位符:\n%s", cleaned)
	}
	if !strings.Contains(cleaned, "plain prose that must survive: hello world") {
		t.Fatalf("普通散文被误杀:\n%s", cleaned)
	}
}

// TestRedactForExportIsStatelessAcrossCalls 断言 known 不会泄漏到下一次调用
// （每次新建一次性 Redactor，而不是往全局实例里 Add）。
func TestRedactForExportIsStatelessAcrossCalls(t *testing.T) {
	const known = "session-only-secret-value"
	if !strings.Contains(secrets.RedactForExport("x "+known, known), "[REDACTED") {
		t.Fatal("带 known 的调用没有替换")
	}
	// 第二次不传 known：同一个值必须保持原文（未被上一次登记污染）。
	if got := secrets.RedactForExport("x " + known); strings.Contains(got, "[REDACTED") {
		t.Fatalf("known 泄漏到了下一次调用: %q", got)
	}
}

// TestRedactForExportEmpty 覆盖空输入与空 known。
func TestRedactForExportEmpty(t *testing.T) {
	if got := secrets.RedactForExport(""); got != "" {
		t.Fatalf("空文本 = %q", got)
	}
	if got := secrets.RedactForExport("nothing secret here"); got != "nothing secret here" {
		t.Fatalf("无秘密文本被改写: %q", got)
	}
}

// TestRedactForExportIdempotent 断言重复导出不会叠加占位符。
func TestRedactForExportIdempotent(t *testing.T) {
	text := "token sk-abcdefghijklmnopqrstuvwxyz012345 end"
	once := secrets.RedactForExport(text)
	twice := secrets.RedactForExport(once)
	if once != twice {
		t.Fatalf("不幂等:\n once=%q\ntwice=%q", once, twice)
	}
}

// TestRedactForExportConcurrent 断言并发调用互不干扰（无共享状态）。
func TestRedactForExportConcurrent(t *testing.T) {
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			known := "concurrent-secret-value"
			got := secrets.RedactForExport("a "+known+" b", known)
			if strings.Contains(got, known) {
				t.Errorf("goroutine %d: 未替换", i)
			}
		}(i)
	}
	wait.Wait()
}
