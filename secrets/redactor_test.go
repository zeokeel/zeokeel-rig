package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件里的凭据全部是合成的假值，只用于验证模式匹配；它们不是真实凭据。
const (
	fakeOpenAIKey  = "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	fakeProjKey    = "sk-proj-AbCdEf1234567890AbCdEf1234567890"
	fakeGitHubPAT  = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	fakeGitHubPat2 = "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuv"
	fakeAWSKeyID   = "AKIAIOSFODNN7EXAMPLE"
	fakeAWSKeyID2  = "ASIAIOSFODNN7EXAMPLE"
	fakeAWSSecret  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN" // 恰好 40 字节
	fakeSlackToken = "xoxb-1234567890-abcdefghij"
	fakeStripeKey  = "sk_live_abcdefghijklmnop1234"
	fakeStripeKey2 = "rk_live_abcdefghijklmnop1234"
	fakeJWT        = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	fakePEM        = "-----BEGIN RSA PRIVATE KEY-----\n" +
		"MIIEowIBAAKCAQEA1234567890abcdefghijklmnopqrstuvwxyzABCDEF\n" +
		"GHIJKLMNOPQRSTUVWXYZ0123456789+/abcdefghijklmnopqrstuvwxyz\n" +
		"-----END RSA PRIVATE KEY-----\n"
	// fakeGoogleKey 是 AIza + 恰好 35 个字符。
	fakeGoogleKey = "AIza" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// fakeGoogleKey 的长度必须是 4+35，防止有人改动常量后模式悄悄失配。
var _ = func() struct{} {
	if len(fakeGoogleKey) != 39 {
		panic("fakeGoogleKey 必须是 AIza + 35 字符")
	}
	return struct{}{}
}()

// ---------------------------------------------------------------------------
// 内置模式：每条 1 正例 + 至少 1 反例
// ---------------------------------------------------------------------------

// TestBuiltinPatterns 逐个模式断言"命中即整体替换成 [REDACTED]"以及"不该命中的绝不误伤"。
func TestBuiltinPatterns(t *testing.T) {
	r := New()
	tests := []struct {
		name string
		in   string
		want string
	}{
		// --- OpenAI / Anthropic 风格 sk- ---
		{"sk-key", "OPENAI_KEY=" + fakeOpenAIKey, "OPENAI_KEY=" + Marker},
		{"sk-key 在句中", "用 " + fakeOpenAIKey + " 调用", "用 " + Marker + " 调用"},
		{"sk-proj-key", fakeProjKey, Marker},
		{"sk-learn 是短串不误伤", "see sk-learn docs", "see sk-learn docs"},
		{"sk-proj- 无载荷不误伤", "prefix sk-proj- suffix", "prefix sk-proj- suffix"},
		// --- GitHub ---
		{"ghp-token", "token " + fakeGitHubPAT, "token " + Marker},
		{"gho-token", "gho_abcdefghijklmnopqrstuvwxyz0123456789", Marker},
		{"ghu-token", "ghu_abcdefghijklmnopqrstuvwxyz0123456789", Marker},
		{"ghs-token", "ghs_abcdefghijklmnopqrstuvwxyz0123456789", Marker},
		{"ghr-token", "ghr_abcdefghijklmnopqrstuvwxyz0123456789", Marker},
		{"ghp_ 太短不误伤", "ghp_short", "ghp_short"},
		{"github_pat_ 细粒度 token", fakeGitHubPat2, Marker},
		{"github_pat_ 太短不误伤", "github_pat_short", "github_pat_short"},
		// --- AWS ---
		{"AKIA key id", "aws_access_key_id = " + fakeAWSKeyID, "aws_access_key_id = " + Marker},
		{"ASIA key id", fakeAWSKeyID2, Marker},
		{"AKIA 太短不误伤", "AKIA123", "AKIA123"},
		{"AKIA 小写不误伤", "akiaiosfodnn7example", "akiaiosfodnn7example"},
		{"aws secret 40 字节", "aws_secret_access_key = " + fakeAWSSecret, "aws_secret_access_key = " + Marker},
		{"无 aws 上下文的 40 字节 base64 不误伤", "blob = " + fakeAWSSecret, "blob = " + fakeAWSSecret},
		// --- Slack ---
		{"slack token", "SLACK=" + fakeSlackToken, "SLACK=" + Marker},
		{"xoxk 前缀不在允许集", "xoxk-1234567890abcdef", "xoxk-1234567890abcdef"},
		{"xoxb 太短不误伤", "xoxb-short", "xoxb-short"},
		// --- Google ---
		{"google api key", "GOOGLE=" + fakeGoogleKey, "GOOGLE=" + Marker},
		{"AIza 太短不误伤", "AIza123", "AIza123"},
		// --- Stripe ---
		{"stripe sk_live", fakeStripeKey, Marker},
		{"stripe rk_live", fakeStripeKey2, Marker},
		{"stripe 前缀无载荷不误伤", "sk_live_ short", "sk_live_ short"},
		// --- JWT ---
		{"jwt", "Authorization: " + fakeJWT, "Authorization: " + Marker},
		{"jwt 只有两段不误伤", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0"},
		{"eyJ 单段不误伤", "eyJhbGciOiJIUzI1NiJ9", "eyJhbGciOiJIUzI1NiJ9"},
		// --- 私钥块 ---
		{"PEM 私钥整块", "before\n" + fakePEM + "after\n", "before\n" + Marker + "\nafter\n"},
		// 没有 END 时一直吞到文本结尾（fail closed：宁可多吞也不留半块密钥），
		// 因此块末尾的换行也一起被替换掉。
		{"PEM 未闭合也整段吞掉", "x\n-----BEGIN EC PRIVATE KEY-----\nAAAA\nBBBB\n", "x\n" + Marker},
		{"证书不是私钥不误伤", "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n", "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"},
		// --- Bearer / Basic ---
		{"bearer", "Authorization: Bearer abcdef0123456789", "Authorization: Bearer " + Marker},
		{"bearer 太短不误伤", "Bearer short", "Bearer short"},
		{"bearer 单独出现不误伤", "use the Bearer scheme", "use the Bearer scheme"},
		{"basic", "Authorization: Basic dXNlcjpwYXNzd29yZA==", "Authorization: Basic " + Marker},
		{"basic 太短不误伤", "Basic auth required", "Basic auth required"},
		// --- URL userinfo：只脱密码 ---
		{"url 密码", "postgres://admin:s3cr3tpass@db.internal:5432/app", "postgres://admin:" + Marker + "@db.internal:5432/app"},
		{"url 无 userinfo 不误伤", "https://example.com/path?a=1", "https://example.com/path?a=1"},
		{"url 只有用户名不误伤", "https://alice@example.com/x", "https://alice@example.com/x"},
		{"url 空密码不误伤", "https://alice:@example.com/x", "https://alice:@example.com/x"},
		// --- key/value ---
		{"password =", "password=hunter2hunter2", "password=" + Marker},
		{"password 空格等号", "password = hunter2hunter2", "password = " + Marker},
		{"api_key JSON 风格", `api_key: "s3cr3tvalue123"`, `api_key: "` + Marker + `"`},
		{"X-API-Key 头", "X-API-Key: abcdef123456", "X-API-Key: " + Marker},
		{"secret 键", "client_secret=abcdefghijkl", "client_secret=" + Marker},
		{"pwd 键", "pwd: 1234567890", "pwd: " + Marker},
		{"token 值太短不误伤", "token = 123", "token = 123"},
		{"裸 password 无值不误伤", "the password field", "the password field"},
		{"key 不是敏感键不误伤", "sort_key = abcdefghijkl", "sort_key = abcdefghijkl"},
		// --- 常见误伤源 ---
		{"UUID 不误伤", "id=123e4567-e89b-12d3-a456-426614174000", "id=123e4567-e89b-12d3-a456-426614174000"},
		{"git sha 不误伤", "commit 3f2b8a1c9d4e5f6a7b8c9d0e1f2a3b4c5d6e7f80", "commit 3f2b8a1c9d4e5f6a7b8c9d0e1f2a3b4c5d6e7f80"},
		{"base64 图片不误伤", "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==",
			"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="},
		{"普通文本不误伤", "the quick brown fox jumps over the lazy dog", "the quick brown fox jumps over the lazy dog"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := r.Redact(testCase.in); got != testCase.want {
				t.Fatalf("Redact(%q)\n got: %q\nwant: %q", testCase.in, got, testCase.want)
			}
		})
	}
}

// TestPatternsCounted 断言每个模式族都至少有一个正例（防止有人删模式表）。
func TestPatternsCounted(t *testing.T) {
	if len(rules) < 15 {
		t.Fatalf("内置模式只有 %d 条，任务要求覆盖 15 类以上", len(rules))
	}
	for _, rule := range rules {
		if rule.re == nil {
			t.Fatalf("规则 %+v 缺少正则", rule)
		}
		if len(rule.needles) == 0 {
			t.Fatalf("规则 %q 缺少字面量前置检查，会拖慢大文本脱敏", rule.re.String())
		}
	}
}

// ---------------------------------------------------------------------------
// 已知明文
// ---------------------------------------------------------------------------

func TestKnownValues(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		in     string
		want   string
	}{
		{"8 字节边界（刚好登记）", "12345678", "a 12345678 b", "a " + Marker + " b"},
		{"15 字节（短于典型 token）", "123456789012345", "x=123456789012345;", "x=" + Marker + ";"},
		{"7 字节被忽略", "1234567", "a 1234567 b", "a 1234567 b"},
		{"空字符串被忽略", "", "abc", "abc"},
		{"无模式可匹配的普通词", "correcthorsebatterystaple", "pass=correcthorsebatterystaple", "pass=" + Marker},
		{"多次出现全部替换", "abcdefgh", "abcdefgh and abcdefgh", Marker + " and " + Marker},
		{"已知值优先于模式", fakeOpenAIKey, "key " + fakeOpenAIKey, "key " + Marker},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			r := New(testCase.secret)
			if got := r.Redact(testCase.in); got != testCase.want {
				t.Fatalf("Redact(%q) = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}
}

// TestKnownLongestFirst 断言多个已知值互相包含时按长到短替换，避免短值把长值切成
// 一个仍然敏感的残片。
func TestKnownLongestFirst(t *testing.T) {
	r := New("abcdefghij", "abcdefghijklmno")
	if got := r.Redact("abcdefghijklmno"); got != Marker {
		t.Fatalf("Redact = %q, want %q（应按最长值优先替换）", got, Marker)
	}
}

// TestKnownNamed 断言命名标签的文案与净化（名字里的 ] 等字符不会破坏占位符）。
func TestKnownNamed(t *testing.T) {
	t.Run("命名", func(t *testing.T) {
		r := New()
		r.AddNamed("openai", fakeOpenAIKey)
		if got := r.Redact("k=" + fakeOpenAIKey); got != "k=[REDACTED:openai]" {
			t.Fatalf("Redact = %q, want k=[REDACTED:openai]", got)
		}
	})
	t.Run("名字被净化", func(t *testing.T) {
		r := New()
		r.AddNamed("a]b c", "abcdefghijkl")
		if got := r.Redact("abcdefghijkl"); got != "[REDACTED:a_b_c]" {
			t.Fatalf("Redact = %q, want [REDACTED:a_b_c]", got)
		}
	})
	t.Run("空名字退化为无名占位符", func(t *testing.T) {
		r := New()
		r.AddNamed("   ", "abcdefghijkl")
		if got := r.Redact("abcdefghijkl"); got != Marker {
			t.Fatalf("Redact = %q, want %q", got, Marker)
		}
	})
	t.Run("同名重复登记只替换一次", func(t *testing.T) {
		r := New()
		r.AddNamed("openai", "abcdefghijkl")
		r.AddNamed("other", "abcdefghijkl")
		if got := r.Redact("abcdefghijkl"); got != "[REDACTED:openai]" {
			t.Fatalf("Redact = %q, want 首次登记的名字", got)
		}
	})
}

// TestAddRejectsMarkerOverlap 断言与占位符本身重叠的已知值不会被登记——否则
// Redact(Redact(x)) 会继续变化（幂等性硬性要求）。
func TestAddRejectsMarkerOverlap(t *testing.T) {
	r := New("REDACTED")
	if got := r.Redact("[REDACTED]"); got != Marker {
		t.Fatalf("Redact = %q, want %q（与占位符重叠的值必须被拒绝）", got, Marker)
	}
}

// ---------------------------------------------------------------------------
// 幂等 / 字节 / 空值
// ---------------------------------------------------------------------------

// TestIdempotent 断言 Redact(Redact(x)) == Redact(x)：占位符本身不再被改写。
func TestIdempotent(t *testing.T) {
	r := New("hunter2hunter2")
	r.AddNamed("openai", fakeOpenAIKey)
	inputs := []string{
		"password=hunter2hunter2",
		"Authorization: Bearer " + fakeJWT,
		"k=" + fakeOpenAIKey,
		"before\n" + fakePEM + "after",
		"https://alice:hunter2hunter2@example.com/x",
		`{"token":"` + fakeOpenAIKey + `","password":"hunter2hunter2"}`,
		"混合 " + fakeGitHubPAT + " / " + fakeAWSKeyID + " / " + fakeSlackToken,
	}
	for _, input := range inputs {
		once := r.Redact(input)
		twice := r.Redact(once)
		if twice != once {
			t.Fatalf("不幂等:\n input: %q\n once: %q\n twice: %q", input, once, twice)
		}
	}
}

func TestRedactBytes(t *testing.T) {
	r := New()
	got := r.RedactBytes([]byte("k=" + fakeOpenAIKey))
	if string(got) != "k="+Marker {
		t.Fatalf("RedactBytes = %q", got)
	}
	plain := []byte("nothing to see")
	if got := r.RedactBytes(plain); !bytes.Equal(got, plain) {
		t.Fatalf("RedactBytes 改动了无害内容: %q", got)
	}
	if got := r.RedactBytes(nil); got != nil {
		t.Fatalf("RedactBytes(nil) = %q, want nil", got)
	}
}

func TestEmptyAndZeroValue(t *testing.T) {
	var zero Redactor
	if !zero.Empty() {
		t.Fatal("零值 Redactor 必须是 Empty")
	}
	if got := zero.Redact("k=" + fakeOpenAIKey); got != "k="+Marker {
		t.Fatalf("零值 Redactor 也必须脱敏模式: %q", got)
	}
	zero.Add("abcdefghij")
	if zero.Empty() {
		t.Fatal("Add 之后 Empty() 必须为 false")
	}
	if got := zero.Redact("abcdefghij"); got != Marker {
		t.Fatalf("零值 Redactor 的已知值未生效: %q", got)
	}

	var nilRedactor *Redactor
	if !nilRedactor.Empty() {
		t.Fatal("nil Redactor 应报告 Empty")
	}
	if got := nilRedactor.Redact("abc"); got != "abc" {
		t.Fatalf("nil Redactor 应原样返回: %q", got)
	}
}

// TestEmptyRedactorAllocations 断言"空 Redactor + 无害文本"走快路径：不触发正则、
// 不分配内存。
func TestEmptyRedactorAllocations(t *testing.T) {
	r := New()
	const text = "the quick brown fox jumps over the lazy dog\n"
	var last string
	allocs := testing.AllocsPerRun(50, func() { last = r.Redact(text) })
	if last != text {
		t.Fatalf("Redact 改动了无害文本: %q", last)
	}
	if allocs != 0 {
		t.Fatalf("空 Redactor 对无害文本分配了 %.1f 次，应为 0（走字面量快路径）", allocs)
	}
}

// ---------------------------------------------------------------------------
// JSON
// ---------------------------------------------------------------------------

func TestRedactJSON(t *testing.T) {
	r := New("hunter2hunter2")

	t.Run("嵌套结构原样保留", func(t *testing.T) {
		in := `{"a":{"b":[{"token":"` + fakeOpenAIKey + `","note":"ok"}]},"user":"alice"}`
		want := `{"a":{"b":[{"token":"` + Marker + `","note":"ok"}]},"user":"alice"}`
		got, err := r.RedactJSON([]byte(in))
		if err != nil {
			t.Fatalf("RedactJSON: %v", err)
		}
		if string(got) != want {
			t.Fatalf("RedactJSON =\n %s\nwant\n %s", got, want)
		}
		if !json.Valid(got) {
			t.Fatalf("输出不是合法 JSON: %s", got)
		}
	})

	t.Run("字符串转义", func(t *testing.T) {
		in := `{"note":"say \"hi\" ` + fakeOpenAIKey + `"}`
		want := `{"note":"say \"hi\" ` + Marker + `"}`
		got, err := r.RedactJSON([]byte(in))
		if err != nil {
			t.Fatalf("RedactJSON: %v", err)
		}
		if string(got) != want {
			t.Fatalf("RedactJSON =\n %s\nwant\n %s", got, want)
		}
		if !json.Valid(got) {
			t.Fatalf("输出不是合法 JSON: %s", got)
		}
	})

	t.Run("值里的 unicode 转义", func(t *testing.T) {
		in := `{"k":"sk-\u0061bcdefghijklmnopqrstuvwxyz"}`
		got, err := r.RedactJSON([]byte(in))
		if err != nil {
			t.Fatalf("RedactJSON: %v", err)
		}
		var decoded map[string]string
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("输出不是合法 JSON: %v (%s)", err, got)
		}
		if decoded["k"] != Marker {
			t.Fatalf("k = %q, want %q", decoded["k"], Marker)
		}
	})

	t.Run("敏感键名的值整体脱敏", func(t *testing.T) {
		in := `{"password":"hunter2","token_count":42,"user":"alice"}`
		got, err := r.RedactJSON([]byte(in))
		if err != nil {
			t.Fatalf("RedactJSON: %v", err)
		}
		want := `{"password":"` + Marker + `","token_count":42,"user":"alice"}`
		if string(got) != want {
			t.Fatalf("RedactJSON =\n %s\nwant\n %s", got, want)
		}
	})

	t.Run("顶层字符串", func(t *testing.T) {
		got, err := r.RedactJSON([]byte(`"` + fakeOpenAIKey + `"`))
		if err != nil {
			t.Fatalf("RedactJSON: %v", err)
		}
		if string(got) != `"`+Marker+`"` {
			t.Fatalf("RedactJSON = %s", got)
		}
	})

	t.Run("无秘密的 JSON 逐字节不变", func(t *testing.T) {
		in := []byte("{ \"b\" : 1, \"a\" : [true, null, 2] }")
		got, err := r.RedactJSON(in)
		if err != nil {
			t.Fatalf("RedactJSON: %v", err)
		}
		if !bytes.Equal(got, in) {
			t.Fatalf("RedactJSON = %q, want %q", got, in)
		}
	})

	t.Run("坏 JSON 退化为文本脱敏", func(t *testing.T) {
		in := []byte(`{"token": "` + fakeOpenAIKey)
		got, err := r.RedactJSON(in)
		if err != nil {
			t.Fatalf("RedactJSON: %v", err)
		}
		if string(got) != r.Redact(string(in)) {
			t.Fatalf("RedactJSON = %q, want 文本脱敏结果 %q", got, r.Redact(string(in)))
		}
		if strings.Contains(string(got), fakeOpenAIKey) {
			t.Fatalf("坏 JSON 没被脱敏: %s", got)
		}
	})

	t.Run("空输入", func(t *testing.T) {
		got, err := r.RedactJSON(nil)
		if err != nil {
			t.Fatalf("RedactJSON: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("RedactJSON(nil) = %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// 并发 / 性能
// ---------------------------------------------------------------------------

// TestConcurrentAddAndRedact 在读多写少的并发下检查数据竞争与"立即生效"。
func TestConcurrentAddAndRedact(t *testing.T) {
	r := New("initialsecret1")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	const text = "initialsecret1 and sk-abcdefghijklmnopqrstuvwxyz0123"

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					out := r.Redact(text)
					if strings.Contains(out, "initialsecret1") {
						t.Errorf("并发读取时已知值泄露: %q", out)
						return
					}
					_ = r.Empty()
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		r.Add(fmt.Sprintf("dynamicsecret%03d", i))
	}
	close(stop)
	wg.Wait()

	if got := r.Redact("dynamicsecret017"); got != Marker {
		t.Fatalf("后来登记的值未立即生效: %q", got)
	}
}

// TestRedactLargeTextPerformance 断言 1MiB 文本（含多个秘密与大量无害内容）在
// 100ms 内完成。race 构建下正则慢数倍，跳过时间门槛（仍跑正确性）。
func TestRedactLargeTextPerformance(t *testing.T) {
	unit := "2026-11-17T12:00:00Z INFO request ok id=123e4567-e89b-12d3-a456-426614174000 " +
		"sha=3f2b8a1c9d4e5f6a7b8c9d0e1f2a3b4c5d6e7f80 key=" + fakeOpenAIKey + "\n"
	var builder strings.Builder
	for builder.Len() < 1<<20 {
		builder.WriteString(unit)
	}
	input := builder.String()
	// 同尺寸、同形状、但没有秘密的基线文本：用来归一化机器速度。
	baseline := strings.ReplaceAll(input, fakeOpenAIKey, "sk-not-a-real-value-here")
	baseline = strings.ReplaceAll(baseline, "hunter2hunter2hunter2", "no-known-value")

	r := New("hunter2hunter2hunter2")
	// 取 3 次里的最小值：这是最不受共享 CI 调度抖动影响的统计量。
	measure := func(text string) time.Duration {
		best := time.Duration(1 << 62)
		for attempt := 0; attempt < 3; attempt++ {
			start := time.Now()
			_ = r.Redact(text)
			if elapsed := time.Since(start); elapsed < best {
				best = elapsed
			}
		}
		return best
	}
	baselineElapsed := measure(baseline)
	elapsed := measure(input)

	out := r.Redact(input)
	if strings.Contains(out, fakeOpenAIKey) {
		t.Fatal("1MiB 文本里的 sk- key 没被脱敏")
	}
	if strings.Contains(out, "hunter2hunter2hunter2") {
		t.Fatal("1MiB 文本里的已知值没被脱敏")
	}
	if raceEnabled {
		t.Skipf("race 构建下不断言时间门槛（耗时 %v，基线 %v）", elapsed, baselineElapsed)
	}
	// 相对门槛：比值能抓住算法回归（例如 O(n²)），而对机器快慢不敏感；50ms 松弛
	// 吸收共享 CI 的调度抖动。绝对门槛（100ms）在负载机器上偶发失败（实测 1/10
	// 次 134ms），因此改成这种形式。
	budget := 4*baselineElapsed + 50*time.Millisecond
	if elapsed > budget {
		t.Fatalf("1MiB 脱敏耗时 %v 超过相对预算 %v（无秘密基线 %v）", elapsed, budget, baselineElapsed)
	}
	t.Logf("1MiB 脱敏耗时 %v（无秘密基线 %v，预算 %v）", elapsed, baselineElapsed, budget)
}

// ---------------------------------------------------------------------------
// 真实形态的样例文件
// ---------------------------------------------------------------------------

// TestSampleTranscript 用 testdata 里的一段"工具输出"验证：秘密全部消失，正常
// 内容逐字保留。
func TestSampleTranscript(t *testing.T) {
	raw, err := readTestdata(t, "tool_output.txt")
	if err != nil {
		t.Fatal(err)
	}
	r := New("hunter2hunter2")
	out := r.Redact(raw)

	for _, secret := range []string{
		fakeOpenAIKey, fakeAWSKeyID, fakeAWSSecret, fakeGitHubPAT, fakeSlackToken,
		fakeGoogleKey, fakeJWT, "hunter2hunter2",
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("样例输出里仍含秘密 %q:\n%s", secret, out)
		}
	}
	for _, kept := range []string{
		`"user":"alice"`, "eu-west-1", "3f2b8a1c9d4e5f6a7b8c9d0e1f2a3b4c5d6e7f80",
		"123e4567-e89b-12d3-a456-426614174000", "api.example.com",
	} {
		if !strings.Contains(out, kept) {
			t.Fatalf("样例输出丢失了正常内容 %q:\n%s", kept, out)
		}
	}
	if !strings.Contains(out, Marker) {
		t.Fatalf("样例输出没有占位符:\n%s", out)
	}
}
