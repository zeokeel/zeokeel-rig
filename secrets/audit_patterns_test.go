package secrets

// 本文件覆盖审计补充的厂商前缀规则（GitLab / Hugging Face / npm / PyPI /
// DigitalOcean / SendGrid）与 AWS session token。每条规则都有正例（命中即整体替换，
// 流式逐字节切块也一样）与反例（形状相近但太短/前缀不对/是占位符，必须原样输出）。

import (
	"strings"
	"testing"
)

func TestAuditPatternsProviderTokens(t *testing.T) {
	gitlab := "glpat-" + "abcdEFGH1234_567890-x"                                            // 22 位载荷
	huggingFace := "hf_" + "abcdefghijklmnopqrstuvwxyz012345"                               // 32 位
	npmToken := "npm_" + "abcdefghijklmnopqrstuvwxyz0123456789ABCD"                         // 40 位
	pypiToken := "pypi-" + strings.Repeat("AgEIcHlwaS5vcmc", 5)[:60]                        // 60 位
	digitalOcean := "dop_v1_" + strings.Repeat("0123456789abcdef", 4)                       // 64 位 hex
	sendgrid := "SG." + strings.Repeat("Ab1_-xyz", 2) + "." + strings.Repeat("Cd2_-XYZ", 2) // 16+16 位
	sessionToken := strings.Repeat("AbC9+/", 30)                                            // 180 位 base64ish

	positives := []struct {
		name   string
		input  string
		secret string
	}{
		{"gitlab-pat", "token " + gitlab + " end", gitlab},
		{"gitlab-pat 行首", gitlab + "\n", gitlab},
		{"huggingface", "hf key: " + huggingFace, huggingFace},
		{"npm", "//registry.npmjs.org/:_authToken=" + npmToken, npmToken},
		{"pypi", "pypi " + pypiToken, pypiToken},
		{"digitalocean", "do " + digitalOcean, digitalOcean},
		{"sendgrid", "sg " + sendgrid, sendgrid},
		{"aws-session-token", `aws_session_token = "` + sessionToken + `"`, sessionToken},
		{"aws-session-token 冒号", "aws_session_token: " + sessionToken, sessionToken},
		{"aws-session-token 大写键名", "AWS_SESSION_TOKEN=" + sessionToken, sessionToken},
	}
	for _, testCase := range positives {
		testCase := testCase
		t.Run("正例/"+testCase.name, func(t *testing.T) {
			got := New().Redact(testCase.input)
			if strings.Contains(got, testCase.secret) {
				t.Fatalf("明文泄露:\n入: %q\n出: %q", testCase.input, got)
			}
			if !strings.Contains(got, Marker) {
				t.Fatalf("没有替换成占位符: %q", got)
			}
			// 流式路径逐字节切块：同一份输入不得泄露。
			streamed := auditStream(t, New(), splitEveryByte(testCase.input)...)
			if strings.Contains(streamed, testCase.secret) {
				t.Fatalf("流式路径泄露:\n出: %q", streamed)
			}
		})
	}

	negatives := []string{
		// 太短：载荷长度不足。
		"glpat-abcdEFGH12",
		"hf_abcdefghijklmnopqrst",
		"npm_abcdefghijklmnopqrst",
		"pypi-" + strings.Repeat("A", 20),
		"dop_v1_0123456789abcdef",        // 16 位 hex，太短
		"SG.abc.def",                     // 两段都太短
		"SG.abcdefghijklmnop.qrstuvwxyz", // 第二段太短
		// 前缀不对：形状相近但不是这家厂商的格式。
		"glpat_abcdEFGH1234567890_xy",       // 下划线而非连字符
		"GLPAT-abcdEFGH1234567890_xy",       // 大小写不符
		"dop_v1_" + strings.Repeat("z", 64), // 64 位但不是十六进制
		"dop_v2_" + strings.Repeat("0123456789abcdef", 4),
		// 占位符/引用：不该被当成秘密。
		"aws_session_token: none",
		"aws_session_token = ${AWS_SESSION_TOKEN}",
		"aws_session_token_count = 42",
		"aws_session_tokens = " + strings.Repeat("A", 120), // 键名不同
		// 普通文本。
		"gitlab pat tokens look like glpat-... in docs",
		"the hf_ prefix is documented here",
		"run npm_install for the npm_ tooling",
	}
	for _, input := range negatives {
		if got := New().Redact(input); got != input {
			t.Fatalf("反例被误伤:\n入: %q\n出: %q", input, got)
		}
	}
}

// TestAuditPatternsAWSSecretKeyUnaffected 固定"新增 AWS session token 规则没有打乱
// 既有的 AWS secret access key 规则"：40 位值仍然只替换值、保留键名。
func TestAuditPatternsAWSSecretKeyUnaffected(t *testing.T) {
	input := "aws_secret_access_key = " + fakeAWSSecret
	got := New().Redact(input)
	if !strings.HasPrefix(got, "aws_secret_access_key = ") {
		t.Fatalf("键名被改写: %q", got)
	}
	if strings.Contains(got, fakeAWSSecret) {
		t.Fatalf("AWS secret 泄露: %q", got)
	}
}
