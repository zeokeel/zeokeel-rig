package secrets

// 本文件是对抗审计（a5）的泄漏用例：目标是"让凭据穿过去"。所有测试都用合成凭据，
// 断言方式与作者一致：输出里不得出现秘密的任何 ≥3 字节前缀。
//
// 命名约定：
//   - TestAuditLeak_*          断言"必须不泄露"（发现缺陷时先红后修）。
//   - TestAuditKnownLimitation_* 固定"当前确实是泄漏"的已知边界（修复后应改成
//     NotContains），它们存在的意义是留下可执行的证据，而不是让缺陷隐形。

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// auditStream 把 chunks 依次写入一个 Writer 并 Flush，返回输出。
func auditStream(t *testing.T, r *Redactor, chunks ...string) string {
	t.Helper()
	var dst bytes.Buffer
	writer := r.Writer(&dst)
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	flusher, ok := writer.(Flusher)
	if !ok {
		t.Fatal("Writer 返回值必须实现 Flusher")
	}
	if err := flusher.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return dst.String()
}

// auditCredentials 是跨 chunk 攻击用的凭据样本（含前后缀上下文）。
func auditCredentials() map[string]string {
	pgp := "-----BEGIN PGP PRIVATE KEY BLOCK-----\n" +
		"lQOYBF7SECRETSECRETSECRETSECRETSECRETSECRETSECRET\n" +
		"=AbCd\n" +
		"-----END PGP PRIVATE KEY BLOCK-----\n"
	return map[string]string{
		"openai":     "prefix " + fakeOpenAIKey + " suffix\n",
		"proj":       fakeProjKey + "\n",
		"ghp":        "TOKEN=" + fakeGitHubPAT + "\n",
		"githubpat":  fakeGitHubPat2 + "\n",
		"aws-id":     "aws key id " + fakeAWSKeyID + "\n",
		"slack":      "slack 候选: " + fakeSlackToken + "\n",
		"stripe":     "stripe " + fakeStripeKey + "\n",
		"google":     "gkey " + fakeGoogleKey + "\n",
		"jwt":        "auth " + fakeJWT + "\n",
		"bearer":     "Authorization: Bearer " + fakeOpenAIKey + "\n",
		"basic":      "Authorization: Basic dXNlcjpwYXNzd29yZA==\n",
		"url-config": "https://user:hunter2hunter2@example.com/path\n",
		"key-value":  "password=hunter2hunter2\n",
		"pem":        fakePEM,
		"pgp":        pgp,
	}
}

// TestAuditLeak_ChunkBoundaryExhaustive 把每条凭据在每个字节位置切开（以及逐字节
// 写入），断言任何 ≥3 字节的前缀都不会出现在输出里。这是"跨 chunk 泄露"的穷举。
func TestAuditLeak_ChunkBoundaryExhaustive(t *testing.T) {
	for name, input := range auditCredentials() {
		input := input
		t.Run(name, func(t *testing.T) {
			secret := auditSecretOf(name)
			if secret == "" {
				t.Fatalf("测试夹具缺少 %q 的秘密样本", name)
			}
			// 1) 逐字节写入。
			if output := auditStream(t, New(), splitEveryByte(input)...); strings.Contains(output, secret[:3]) {
				t.Fatalf("逐字节写入泄露: %q\n输出: %q", secret[:3], output)
			}
			// 2) 每个切点各写一次。
			for cut := 1; cut < len(input); cut++ {
				output := auditStream(t, New(), input[:cut], input[cut:])
				assertNoFragment(t, output, secret)
			}
		})
	}
}

// auditSecretOf 返回某条样本里"绝不该出现在输出里"的那个凭据本体。
func auditSecretOf(name string) string {
	switch name {
	case "openai", "bearer":
		return fakeOpenAIKey
	case "proj":
		return fakeProjKey
	case "ghp":
		return fakeGitHubPAT
	case "githubpat":
		return fakeGitHubPat2
	case "aws-id":
		return fakeAWSKeyID
	case "slack":
		return fakeSlackToken
	case "stripe":
		return fakeStripeKey
	case "google":
		return fakeGoogleKey
	case "jwt":
		return fakeJWT
	case "basic":
		return "dXNlcjpwYXNzd29yZA=="
	case "url-config":
		return "hunter2hunter2"
	case "key-value":
		return "hunter2hunter2"
	case "pem":
		return "MIIEowIBAAKCAQEA1234567890abcdefghijklmnopqrstuvwxyzABCDEF"
	case "pgp":
		return "lQOYBF7SECRETSECRETSECRET"
	}
	return ""
}

func splitEveryByte(input string) []string {
	chunks := make([]string, 0, len(input))
	for index := 0; index < len(input); index++ {
		chunks = append(chunks, input[index:index+1])
	}
	return chunks
}

// TestAuditLeak_PGPPrivateKeyBlock 是本次审计最重要的发现：PGP 私钥块的 BEGIN/END
// 标记与 PEM 的 `[A-Z0-9 ]*PRIVATE KEY-----` 形状不匹配，整块原样输出。
func TestAuditLeak_PGPPrivateKeyBlock(t *testing.T) {
	const body = "lQOYBF7SECRETSECRETSECRETSECRETSECRETSECRETSECRET"
	complete := "-----BEGIN PGP PRIVATE KEY BLOCK-----\n" + body + "\n-----END PGP PRIVATE KEY BLOCK-----\n"
	truncated := "-----BEGIN PGP PRIVATE KEY BLOCK-----\n" + body + "\n"

	for label, input := range map[string]string{"complete": complete, "truncated": truncated} {
		input := input
		t.Run("plain-"+label, func(t *testing.T) {
			output := New().Redact(input)
			if strings.Contains(output, "SECRET") {
				t.Fatalf("PGP 私钥明文泄露:\n入: %q\n出: %q", input, output)
			}
			if !strings.Contains(output, Marker) {
				t.Fatalf("PGP 私钥块没有被替换成占位符: %q", output)
			}
		})
		t.Run("stream-"+label, func(t *testing.T) {
			output := auditStream(t, New(), splitEveryByte(input)...)
			if strings.Contains(output, "SECRET") {
				t.Fatalf("流式路径 PGP 私钥明文泄露:\n出: %q", output)
			}
		})
	}
}

// TestAuditLeak_PEMVariants 覆盖 PEM 的各种变体：不同密钥类型、END 前有空行、
// 两个块相邻、BEGIN 行有前后缀、CRLF 行尾。
func TestAuditLeak_PEMVariants(t *testing.T) {
	body := "MIIESECRETSECRETSECRETSECRETSECRETSECRETSECRET"
	cases := map[string]string{
		"rsa":       "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----\n",
		"ec":        "-----BEGIN EC PRIVATE KEY-----\n" + body + "\n-----END EC PRIVATE KEY-----\n",
		"openssh":   "-----BEGIN OPENSSH PRIVATE KEY-----\n" + body + "\n-----END OPENSSH PRIVATE KEY-----\n",
		"dsa":       "-----BEGIN DSA PRIVATE KEY-----\n" + body + "\n-----END DSA PRIVATE KEY-----\n",
		"encrypted": "-----BEGIN ENCRYPTED PRIVATE KEY-----\n" + body + "\n-----END ENCRYPTED PRIVATE KEY-----\n",
		"blankline": "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n\n\n-----END RSA PRIVATE KEY-----\n",
		"adjacent": "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----\n" +
			"-----BEGIN EC PRIVATE KEY-----\n" + body + "\n-----END EC PRIVATE KEY-----\n",
		"prefix": "note: -----BEGIN RSA PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----\n",
		"crlf":   "-----BEGIN RSA PRIVATE KEY-----\r\n" + body + "\r\n-----END RSA PRIVATE KEY-----\r\n",
		"no-end": "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n",
	}
	for name, input := range cases {
		input := input
		t.Run(name, func(t *testing.T) {
			output := New().Redact(input)
			if strings.Contains(output, "SECRET") {
				t.Fatalf("私钥明文泄露:\n出: %q", output)
			}
			if !strings.Contains(output, Marker) {
				t.Fatalf("私钥块没有被替换: %q", output)
			}
			streamed := auditStream(t, New(), splitEveryByte(input)...)
			if strings.Contains(streamed, "SECRET") {
				t.Fatalf("流式路径私钥明文泄露:\n出: %q", streamed)
			}
		})
	}
}

// TestAuditLeak_JSONSensitiveContainer 覆盖敏感键的值是数组/对象时的泄漏：
// 当前实现只把数组的第一个字符串元素替换掉，其余元素与嵌套对象里的值原样输出。
func TestAuditLeak_JSONSensitiveContainer(t *testing.T) {
	r := New()
	cases := []string{
		`{"api_key":["s3cr3t1value","s3cr3t2value"]}`,
		`{"password":["p1secretvalue","p2secretvalue"]}`,
		`{"api_key":{"v":"s3cr3t1value"}}`,
		`{"nested":{"secret":["s3cr3t1value","s3cr3t2value"]}}`,
		`{"list":[{"token":"s3cr3t1value"},{"token":"s3cr3t2value"}]}`,
	}
	for _, input := range cases {
		input := input
		t.Run(input, func(t *testing.T) {
			output, err := r.RedactJSON([]byte(input))
			if err != nil {
				t.Fatalf("RedactJSON: %v", err)
			}
			if strings.Contains(string(output), "s3cr3t") || strings.Contains(string(output), "p1secret") {
				t.Fatalf("敏感键容器里的明文泄露: %s", output)
			}
			if !json.Valid(output) {
				t.Fatalf("输出不是合法 JSON: %s", output)
			}
		})
	}
}

// TestAuditLeak_JSONSensitiveKeyNames 覆盖敏感键名合集：文本路径认为 pwd 是敏感键，
// JSON 路径却不认，导致 {"pwd": "..."} 原样输出。
func TestAuditLeak_JSONSensitiveKeyNames(t *testing.T) {
	r := New()
	for _, key := range []string{"password", "passwd", "pwd", "pass", "passcode", "api_key", "apiKey", "API-KEY", "secret", "client_secret", "access_token", "X-Auth-Token", "credentials", "private_key"} {
		input := `{"` + key + `":"s3cr3t1value"}`
		output, err := r.RedactJSON([]byte(input))
		if err != nil {
			t.Fatalf("RedactJSON(%s): %v", key, err)
		}
		if strings.Contains(string(output), "s3cr3t1value") {
			t.Fatalf("敏感键 %q 的值泄露: %s", key, output)
		}
	}
	// 反例：这些键名不该被整值脱敏。
	for _, key := range []string{"token_count", "author", "note", "username", "passenger"} {
		input := `{"` + key + `":"s3cr3t1value"}`
		output, err := r.RedactJSON([]byte(input))
		if err != nil {
			t.Fatalf("RedactJSON(%s): %v", key, err)
		}
		if !strings.Contains(string(output), "s3cr3t1value") {
			t.Fatalf("键 %q 被误判成敏感键: %s", key, output)
		}
	}
}

// TestAuditLeak_JSONEscapedAndStructure 覆盖 JSON 转义、嵌套、坏 JSON 与结构保留。
func TestAuditLeak_JSONEscapedAndStructure(t *testing.T) {
	r := New()
	// \u002d 是 '-'：解码后仍是 sk-proj- 形态，必须被脱敏。
	escaped := `{"k":"sk\u002dproj-AbCdEf1234567890AbCdEf1234567890"}`
	output, err := r.RedactJSON([]byte(escaped))
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	if strings.Contains(string(output), "AbCdEf1234567890") {
		t.Fatalf("unicode 转义后的 key 泄露: %s", output)
	}
	// 坏 JSON：退化为文本脱敏，且不得抛出错误。
	broken := []byte(`{"token": "` + fakeOpenAIKey)
	output, err = r.RedactJSON(broken)
	if err != nil {
		t.Fatalf("RedactJSON(坏 JSON): %v", err)
	}
	if strings.Contains(string(output), fakeOpenAIKey) {
		t.Fatalf("坏 JSON 没被脱敏: %s", output)
	}
	// 无秘密的 JSON 必须逐字节不变（含数组/嵌套/转义）。
	for _, input := range []string{
		`{"a":[1,true,null,"x"],"b":{"c":"d"},"e":"\u00e9"}`,
		`{ "b" : 1, "a" : [true, null, 2] }`,
		`[]`,
		`{}`,
	} {
		got, err := r.RedactJSON([]byte(input))
		if err != nil {
			t.Fatalf("RedactJSON(%s): %v", input, err)
		}
		if string(got) != input {
			t.Fatalf("无秘密 JSON 被改写:\n入: %s\n出: %s", input, got)
		}
	}
}

// TestAuditLeak_KnownTextEdges 覆盖已知明文的边角：正则元字符、互为子串、超长、
// 占位符重叠、登记后立即生效。
func TestAuditLeak_KnownTextEdges(t *testing.T) {
	t.Run("regex-metacharacters-are-literal", func(t *testing.T) {
		r := New(".*(abc)[0-9]+$")
		if got := r.Redact("x .*(abc)[0-9]+$ y"); got != "x "+Marker+" y" {
			t.Fatalf("redact = %q", got)
		}
	})
	t.Run("substring-of-another-known", func(t *testing.T) {
		r := New("abcdefgh", "abcdefghijklmn")
		if got := r.Redact("v=abcdefghijklmn"); got != "v="+Marker {
			t.Fatalf("redact = %q, want the longest known to win", got)
		}
	})
	t.Run("long-known-nonstreaming", func(t *testing.T) {
		secret := strings.Repeat("ABCDEFGHIJ", 10000) // 100 KiB
		r := New(secret)
		if got := r.Redact("a " + secret + " b"); strings.Contains(got, "ABCDEFGHIJ") {
			t.Fatalf("超长 known 未被替换: %q", got[:60])
		}
	})
	t.Run("marker-overlap-rejected", func(t *testing.T) {
		r := New("[REDACTED]", "REDACTED", "ACTED")
		if !r.Empty() {
			// "ACTED" 不在占位符里，但 "REDACTED" 在；Empty 只反映"有没有登记"。
			// 这里只断言占位符本身不会被再次替换。
			_ = r
		}
		if got := r.Redact("[REDACTED]"); got != "[REDACTED]" {
			t.Fatalf("占位符被破坏: %q", got)
		}
	})
	t.Run("add-takes-effect-immediately", func(t *testing.T) {
		r := New()
		secret := "custom-secret-value-1234"
		if got := r.Redact(secret); got != secret {
			t.Fatalf("登记前就被替换: %q", got)
		}
		r.Add(secret)
		if got := r.Redact("x " + secret + " y"); strings.Contains(got, secret) {
			t.Fatalf("登记后仍泄露: %q", got)
		}
	})
}

// TestAuditKnownLimitation_StreamingKnownMultiline 记录已知限制：Writer 按行输出，
// 因此"含换行的已知明文"在流式路径不会被字面量替换（非流式 Redact 正常）。
func TestAuditKnownLimitation_StreamingKnownMultiline(t *testing.T) {
	secret := "line-one part\nline-two part"
	r := New(secret)
	if got := r.Redact("x " + secret + " y"); strings.Contains(got, "line-one part") {
		t.Fatalf("非流式应替换多行 known: %q", got)
	}
	// 已知限制：Writer 只扣留"最后一个未完成行"。当 known 内部含换行、且写入边界恰好
	// 落在那个换行上时，前半段会作为"完整行"先被输出（此时它已不在同一块里，无法匹配）。
	output := auditStream(t, r, "x line-one part\n", "line-two part y\n")
	if !strings.Contains(output, "line-one part") {
		t.Fatalf("行为变化：流式路径现在能替换跨块的多行 known 了（请把本用例改成 NotContains）: %q", output)
	}
}

// TestAuditKnownLimitation_OverMaxBufferedSecret 记录已知限制：超过 MaxBuffered
// （64 KiB）的凭据在流式路径会被强制冲刷切开（非流式没有这个问题）。
func TestAuditKnownLimitation_OverMaxBufferedSecret(t *testing.T) {
	secret := strings.Repeat("Z", 70<<10)
	r := New(secret)
	if got := r.Redact(secret); strings.Contains(got, "ZZZ") {
		t.Fatalf("非流式应替换超长 known")
	}
	var dst bytes.Buffer
	writer := r.Writer(&dst)
	for offset := 0; offset < len(secret); offset += 4096 {
		end := min(offset+4096, len(secret))
		if _, err := writer.Write([]byte(secret[offset:end])); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := writer.(Flusher).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if strings.Contains(dst.String(), strings.Repeat("Z", 32)) {
		t.Logf("已知限制确认：>%d 字节的 known 在流式路径泄露了 %d 字节", MaxBuffered, dst.Len())
	}
}

// TestAuditKnownLimitation_YAMLBlockValue 记录已知限制：key 与其值不在同一行时
// （YAML 的 `password:` + 缩进值）不匹配——所有内置模式都是单行规则。
func TestAuditKnownLimitation_YAMLBlockValue(t *testing.T) {
	input := "password:\n  hunter2hunter2\n"
	output := New().Redact(input)
	if !strings.Contains(output, "hunter2hunter2") {
		t.Fatalf("行为变化：YAML 跨行值现在被脱敏了（请把本用例改成 NotContains）: %q", output)
	}
	// 同一行时正常。
	if got := New().Redact("password: hunter2hunter2\n"); strings.Contains(got, "hunter2hunter2") {
		t.Fatalf("同行 key=value 应被脱敏: %q", got)
	}
}

// TestAuditKnownLimitation_PatternGaps 记录模式表覆盖缺口：这些真实存在的凭据前缀
// 目前一条规则都不命中（不做高熵检测的代价）。
func TestAuditKnownLimitation_PatternGaps(t *testing.T) {
	cases := map[string]string{
		"gitlab-pat":     "glpat-abcdefghijklmnopqrst",
		"huggingface":    "hf_abcdefghijklmnopqrstuvwxyz012345",
		"pypi":           "pypi-AgEIcHlwaS5vcmcCJGFiY2RlZmdoaWprbG1ub3A",
		"digitalocean":   "dop_v1_" + strings.Repeat("0123456789abcdef", 4),
		"google-oauth":   "ya29.a0AfH6SMBxSECRETSECRETSECRET",
		"sendgrid":       "SG.abcdefghijklmnopqrstuv.abcdefghijklmnopqrstuvwxyz0123456789ABCDE",
		"twilio":         "SK" + strings.Repeat("0123456789abcdef", 2),
		"azure-sas":      "sv=2022-11-02&ss=b&srt=sco&sp=rl&se=2030-01-01&sig=SECRETSIGNATURE",
		"npm-token-only": "npm_abcdefghijklmnopqrstuvwxyz012345",
	}
	leaked := 0
	for name, input := range cases {
		if output := New().Redact(input); strings.Contains(output, "SECRET") || output == input {
			leaked++
			t.Logf("覆盖缺口：%s 未匹配任何规则（输出 == 输入）", name)
		}
	}
	if leaked == 0 {
		t.Fatal("行为变化：所有缺口样本现在都被覆盖了（请更新报告 §D）")
	}
}

// TestAuditLeak_JSONEscapedKeyName 覆盖"键名本身被转义"的绕过尝试：
// `{"pass\u0077ord":"..."}` 解码后是 password，必须按敏感键处理。
func TestAuditLeak_JSONEscapedKeyName(t *testing.T) {
	r := New()
	inputs := []string{
		`{"pass\u0077ord":"hunter2hunter2"}`,
		`{"tok\u0065n":"s3cr3t1value"}`,
		`{"api\u005fkey":"s3cr3t1value"}`,
	}
	for _, input := range inputs {
		output, err := r.RedactJSON([]byte(input))
		if err != nil {
			t.Fatalf("RedactJSON(%s): %v", input, err)
		}
		if strings.Contains(string(output), "hunter2hunter2") || strings.Contains(string(output), "s3cr3t1value") {
			t.Fatalf("转义键名的值泄露: %s", output)
		}
		if !json.Valid(output) {
			t.Fatalf("输出不是合法 JSON: %s", output)
		}
	}
}

// TestAuditLeak_JSONNullAndNumberPayload 覆盖值里的 NUL 转义、数字/布尔与超大载荷。
func TestAuditLeak_JSONNullAndNumberPayload(t *testing.T) {
	r := New()
	withNUL := []byte(`{"note":"a\u0000b","password":"hunter2hunter2","n":42}`)
	output, err := r.RedactJSON(withNUL)
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	if !json.Valid(output) {
		t.Fatalf("输出不是合法 JSON: %s", output)
	}
	if strings.Contains(string(output), "hunter2hunter2") {
		t.Fatalf("NUL 载荷里的密码泄露: %s", output)
	}
	if !strings.Contains(string(output), `"n":42`) || !strings.Contains(string(output), `\u0000`) {
		t.Fatalf("结构/转义被改写: %s", output)
	}
	// 超大 JSON：100k 个键值对，确认不 panic 且敏感键被脱敏。
	builder := strings.Builder{}
	builder.WriteString(`{"items":[`)
	for i := 0; i < 20000; i++ {
		if i > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(`{"id":1,"note":"x"}`)
	}
	builder.WriteString(`],"password":"hunter2hunter2"}`)
	large, err := r.RedactJSON([]byte(builder.String()))
	if err != nil {
		t.Fatalf("RedactJSON(large): %v", err)
	}
	if strings.Contains(string(large), "hunter2hunter2") {
		t.Fatal("超大 JSON 里的密码泄露")
	}
	if !json.Valid(large) {
		t.Fatal("超大 JSON 输出不合法")
	}
}

// TestAuditKnownLimitation_EncodingVariants 记录"编码变体"这条已知边界：URL 编码、
// base64 包装、零宽字符插字都不在模式表覆盖范围内（不做高熵检测的代价）。
func TestAuditKnownLimitation_EncodingVariants(t *testing.T) {
	cases := map[string]string{
		"url-encoded":      "token=sk%2Dproj%2DAbCdEf1234567890AbCdEf1234567890",
		"base64-wrapped":   "token= c2stcHJvai1BYkNkRWYxMjM0NTY3ODkwQWJDZEVmMTIzNDU2Nzg5MA==",
		"zero-width":       "sk-proj-\u200bAbCdEf1234567890AbCdEf1234567890",
		"space-separated":  "sk-proj- AbCdEf1234567890 AbCdEf1234567890",
		"split-by-escapes": "sk\\x2dproj-AbCdEf1234567890AbCdEf1234567890",
	}
	for name, input := range cases {
		if output := New().Redact(input); output == input {
			t.Logf("编码变体未覆盖：%s（输出 == 输入）", name)
		}
	}
}
