package secrets

// 本文件是误伤审计：脱敏工具在正常输出上"多杀"同样是缺陷——它会把用户看到的
// 内容改坏（配置文件、文档、日志）。这里固定"散文与普通标识符不得被改写"。

import (
	"strings"
	"testing"
)

// TestAuditUsability_ProseNotMasked 覆盖最容易被误伤的散文形态：Basic/Bearer 是
// 高频英文词，规则 `basic|bearer` + 一个 ≥8 字符的单词会直接把正常句子改烂。
func TestAuditUsability_ProseNotMasked(t *testing.T) {
	inputs := []string{
		"The basic configuration file is here.",
		"This is a basic authentication example.",
		"basic training starts on Monday",
		"The basic usage pattern is documented below.",
		"bearer tokenization is a thing in HTTP",
		"a bearer of good news",
		"The bearer authentication scheme",
		"Basic Configuration Reference",
	}
	for _, input := range inputs {
		if got := New().Redact(input); got != input {
			t.Fatalf("散文被误伤:\n入: %q\n出: %q", input, got)
		}
	}
	// 反向：真正的凭据仍然必须被替换（含数字/符号或足够长的随机串）。
	realInputs := map[string]string{
		"Authorization: Basic dXNlcjpwYXNzd29yZA==":              "dXNlcjpwYXNzd29yZA==",
		"Authorization: Basic dXNlcjpwYXNz:MTIzNA==":             "dXNlcjpwYXNz:MTIzNA==",
		"Authorization: Bearer abcdefghijklmnop1234":             "abcdefghijklmnop1234",
		"authorization: bearer eyJhbGciOiJIUzI1NiJ9.payload.sig": "eyJhbGciOiJIUzI1NiJ9",
	}
	for input, secret := range realInputs {
		if got := New().Redact(input); strings.Contains(got, secret) {
			t.Fatalf("真实凭据漏杀:\n入: %q\n出: %q", input, got)
		}
	}
}

// TestAuditUsability_IdentifiersNotMasked 覆盖普通技术产物：UUID、git sha、base64
// 图片、base32、长路径、正则字面量、赋值语句。
func TestAuditUsability_IdentifiersNotMasked(t *testing.T) {
	inputs := []string{
		"id=550e8400-e29b-41d4-a716-446655440000",
		"commit 3f786850e387550fdab836ed7e6dc881de23001b",
		"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==",
		"MZXW6YTBOI======",
		"/usr/local/lib/python3.11/site-packages/some_package/module.py",
		`re := regexp.MustCompile("password=[^ ]+")`,
		"token := getToken()",
		"token_count = 42",
		"password: none",
		"password policy: minimum length 12",
		"the secret sauce is patience",
		"api_key: ${API_KEY}",
		"see sk-learn docs for sk-learn usage",
		"timestamp=2026-11-17T20:46:30+08:00",
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	}
	for _, input := range inputs {
		if got := New().Redact(input); got != input {
			t.Fatalf("普通内容被误伤:\n入: %q\n出: %q", input, got)
		}
	}
}

// TestAuditUsability_IdempotentOnCorpus 固定幂等：Redact(Redact(x)) == Redact(x)，
// 包括带标签的占位符与已知明文替换之后的形态。
func TestAuditUsability_IdempotentOnCorpus(t *testing.T) {
	r := New("hunter2hunter2")
	corpus := []string{
		"Authorization: Bearer " + fakeOpenAIKey,
		"password=hunter2hunter2",
		"https://user:hunter2hunter2@example.com/x",
		`{"password":"hunter2hunter2","token":"` + fakeGitHubPAT + `"}`,
		fakePEM,
		"the basic configuration is fine",
		"a bare token=abcdefghijklmnop",
	}
	for _, input := range corpus {
		first := r.Redact(input)
		second := r.Redact(first)
		if second != first {
			t.Fatalf("不幂等:\n入: %q\n一: %q\n二: %q", input, first, second)
		}
	}
}

// TestAuditUsability_JSONKeysPreserved 固定 RedactJSON 只改值、键名与无秘密结构
// 逐字节不变（含数组、嵌套、转义、数字/布尔/null）。
func TestAuditUsability_JSONKeysPreserved(t *testing.T) {
	r := New()
	inputs := []string{
		`{"note":"x","list":[1,2,3],"nested":{"a":true,"b":null}}`,
		`{ "spaced" : [ 1 , 2 ] , "unicode" : "\u00e9" }`,
		`"top-level string"`,
		`123`,
		`null`,
	}
	for _, input := range inputs {
		output, err := r.RedactJSON([]byte(input))
		if err != nil {
			t.Fatalf("RedactJSON(%s): %v", input, err)
		}
		if string(output) != input {
			t.Fatalf("结构被改写:\n入: %s\n出: %s", input, output)
		}
	}
	// 敏感键的字符串值仍然必须被替换（结构保留 ≠ 不脱敏）。
	masked, err := r.RedactJSON([]byte(`{"password":"x","note":"ok"}`))
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	if strings.Contains(string(masked), `"x"`) || !strings.Contains(string(masked), `"note":"ok"`) {
		t.Fatalf("敏感键脱敏或结构保留失败: %s", masked)
	}
}
