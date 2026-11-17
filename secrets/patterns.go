package secrets

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// rule 是一条内置脱敏模式。
//
// 设计约束（流式处理依赖它们，改动前请先读 stream.go）：
//   - 所有模式都只在一行之内匹配，绝不跨 '\n'。这样流式层只要扣留"最后一个未
//     完成行"，就不可能把一处凭据切成两半。
//   - 每条规则都带 needles 字面量前置检查：文本里一个都没出现时直接跳过，既省
//     正则扫描，也是"空 Redactor + 无害文本零分配"快路径的实现方式。
//   - group 指向要被替换的子组（0 = 整个匹配），因此 Bearer/Basic/URL/key=value
//     这类模式可以保住上下文、只替换秘密本身。
type rule struct {
	// re 是模式本身。
	re *regexp.Regexp
	// needles 是任一对不上就跳过该规则的字面量。
	needles []string
	// fold 为真时 needles 按 ASCII 大小写不敏感比较（不分配内存）。
	fold bool
	// group 是要替换成占位符的子组；0 表示整个匹配。
	group int
	// keep 返回 true 表示该子匹配要原样保留：它可能已经是占位符（幂等性），也可能
	// 按值形态判定根本不像凭据/秘密（避免把 "basic configuration" 这类散文抹掉）。
	keep func(string) bool
}

// rules 是内置模式表，顺序即应用顺序。更具体/更危险的模式排在前面：
// 私钥块最先（块内是 base64 噪声），随后是各家固定前缀，最后才是 key=value 这类
// 上下文模式。
var rules = []rule{
	// 0) PEM 私钥块：BEGIN 到 END 整块替换。没有 END（被截断/流式一半）时吃到
	// 文本结尾，宁可多吞一点也不留下半块密钥。
	{
		// `(?: BLOCK)?` 覆盖 PGP 私钥块（`-----BEGIN PGP PRIVATE KEY BLOCK-----`）：
		// 漏掉它会让整份私钥原样输出（审计发现，见 audit_leak_test.go）。
		re:      regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z)`),
		needles: []string{"-----BEGIN"},
	},
	// 1) GitHub 细粒度 token。
	{
		re:      regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
		needles: []string{"github_pat_"},
	},
	// 2) GitHub 经典 token：ghp_/gho_/ghu_/ghs_/ghr_。
	{
		re:      regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`),
		needles: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"},
	},
	// 3) AWS access key id（长期 AKIA / 临时 ASIA）。
	{
		re:      regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`),
		needles: []string{"AKIA", "ASIA"},
	},
	// 4) Slack token。
	{
		re:      regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
		needles: []string{"xox"},
	},
	// 5) Google API key（AIza + 35 字符）。
	{
		re:      regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),
		needles: []string{"AIza"},
	},
	// 6) Stripe 密钥（live/test 的 sk_/rk_）。
	{
		re:      regexp.MustCompile(`(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`),
		needles: []string{"sk_live_", "rk_live_", "sk_test_", "rk_test_"},
	},
	// 7) OpenAI 项目级 key（比通用 sk- 更具体，排在前面）。
	{
		re:      regexp.MustCompile(`sk-proj-[A-Za-z0-9_\-]{20,}`),
		needles: []string{"sk-proj-"},
	},
	// 8) 通用 sk- 风格 key。20 字符下限用来放过 sk-learn 之类的短词。
	{
		re:      regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`),
		needles: []string{"sk-"},
	},
	// 9) JWT：eyJ 打头的三段 base64url。
	{
		re:      regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`),
		needles: []string{"eyJ"},
	},
	// 10) AWS secret access key：40 字节 base64ish，且同一行上下文里有 aws/amazon。
	// 只替换 40 字节的值，键名与分隔符原样保留。
	{
		re:      regexp.MustCompile(`(?i)(?:aws|amazon)[^\n]{0,40}?[=:][ \t]*["']?([A-Za-z0-9/+=]{40})(?:[^A-Za-z0-9/+=]|\z)`),
		needles: []string{"aws", "amazon"},
		fold:    true,
		group:   1,
	},
	// 11) Authorization: Bearer <token>：保留 scheme 与空白，只替换 token。
	{
		re:      regexp.MustCompile(`(?i)(\bbearer[ \t]+)([A-Za-z0-9._~+/=\-]{8,})`),
		needles: []string{"bearer"},
		fold:    true,
		group:   2,
		// "bearer tokenization" 是普通英文：只有字面量长得像凭据时才替换。
		keep: keepNonCredentialValue(bearerPlainWordFloor),
	},
	// 12) Authorization: Basic <base64>：同上。
	{
		re:      regexp.MustCompile(`(?i)(\bbasic[ \t]+)([A-Za-z0-9+/]{8,}={0,2})`),
		needles: []string{"basic"},
		fold:    true,
		group:   2,
		// "basic configuration"/"basic authentication" 是普通英文，不能整词抹掉；
		// Basic 的值有确定结构（base64 的 "user:password"），按结构判定即可。
		keep: func(value string) bool { return !looksLikeBasicCredential(value) },
	},
	// 13) URL userinfo：scheme://user:pass@host → 只把 pass 换成占位符。
	{
		re:      regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)([^/\s:@]{1,64}):([^/\s@]{1,256})@`),
		needles: []string{"://"},
		group:   3,
		keep:    isMarker,
	},
	// 14) GitLab personal access token：glpat- + 20 位以上。
	{
		re:      regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`),
		needles: []string{"glpat-"},
	},
	// 15) Hugging Face token：hf_ + 30 位以上字母数字。
	{
		re:      regexp.MustCompile(`hf_[A-Za-z0-9]{30,}`),
		needles: []string{"hf_"},
	},
	// 16) npm access token：npm_ + 36 位以上。
	{
		re:      regexp.MustCompile(`npm_[A-Za-z0-9]{36,}`),
		needles: []string{"npm_"},
	},
	// 17) PyPI token：pypi- 前缀的载荷很长（实测 50 位以上），短串不误伤。
	{
		re:      regexp.MustCompile(`pypi-[A-Za-z0-9_-]{50,}`),
		needles: []string{"pypi-"},
	},
	// 18) DigitalOcean personal access token：dop_v1_ + 恰好 64 位小写十六进制。
	{
		re:      regexp.MustCompile(`dop_v1_[a-f0-9]{64}`),
		needles: []string{"dop_v1_"},
	},
	// 19) SendGrid API key：SG.<base64url 段>.<base64url 段>，两段都不少于 16 位。
	{
		re:      regexp.MustCompile(`SG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`),
		needles: []string{"SG."},
	},
	// 20) AWS 临时凭据的 session token：只有 100 位以上的 base64ish 值才替换，
	// 因此 `aws_session_token: none` 这类占位符不会被抹掉。只替换 group 1（值），
	// 键名与分隔符原样保留。
	//
	// 注意这里用 `[ \t]*` 而不是 `\s*`：内置模式必须**单行**匹配——流式层靠这一点
	// 保证 token 不会被行边界切开，跨行匹配在流式路径里等于漏杀。
	{
		re:      regexp.MustCompile(`(?i)aws_session_token['"]?[ \t]*[=:][ \t]*["']?([A-Za-z0-9/+=]{100,})`),
		needles: []string{"aws_session_token"},
		fold:    true,
		group:   1,
	},
	// 21) key=value / key: "value"：只替换值。值必须 ≥8 个非空白字符，避免把
	// "token: 123" 这类无关内容也抹掉。
	//
	// 键名刻意不加 \b：client_secret、X-API-Key、db_password 这类复合键名的边界是
	// '_'/'-'（词字符），加了 \b 反而会全部漏判。
	{
		re:      regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key)["']?[ \t]*[=:][ \t]*["']?([^\s"']{8,})`),
		needles: []string{"password", "passwd", "pwd", "secret", "token", "api_key", "api-key", "apikey"},
		fold:    true,
		group:   2,
		keep:    keepNonSecretValue,
	},
}

// bearerPlainWordFloor 是"纯字母值"允许的最长长度：超过它的纯字母串更可能是随机
// 凭据而不是英文单词（Bearer 的值没有可用结构，只能靠这条启发式）。
const bearerPlainWordFloor = 24

// looksLikeBasicCredential 报告值是否是 base64 编码的 "user:password"。
//
// Basic 认证的值有确定结构：去掉填充后解码，结果必须全是可打印 ASCII 且含 ':'。
// 英文单词（"configuration"、"training"）要么不是合法 base64，要么解码出不可打印
// 字节，因此不会被误判——这条判据把 "basic configuration" 这类散文完全排除。
func looksLikeBasicCredential(value string) bool {
	decoded, ok := decodeBase64Loose(value)
	if !ok {
		return false
	}
	hasColon := false
	for _, char := range decoded {
		if char == ':' {
			hasColon = true
			continue
		}
		if char < 0x20 || char > 0x7e {
			return false
		}
	}
	return hasColon
}

// decodeBase64Loose 解码可能缺少填充的 base64（Basic 头通常带 '='，但不保证）。
func decodeBase64Loose(value string) ([]byte, bool) {
	if value == "" {
		return nil, false
	}
	unpadded := strings.TrimRight(value, "=")
	if remainder := len(unpadded) % 4; remainder == 1 {
		return nil, false
	} else if remainder > 0 {
		unpadded += strings.Repeat("=", 4-remainder)
	}
	decoded, err := base64.StdEncoding.DecodeString(unpadded)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// looksLikeCredentialValue 判断一个候选值是否值得当作凭据脱敏。
//
// 真实凭据几乎总含数字或 +/=._~- 之一，而英文单词不是；因此"纯字母 + 较短"的
// 候选按普通文本处理。这条判据只用于 Bearer/Basic 这类"关键字 + 任意词"的宽松
// 模式——它把 "basic configuration"、"bearer tokenization" 这类散文从误伤里救回来，
// 同时不会漏掉任何含数字/符号的真实 token。
func looksLikeCredentialValue(value string, plainWordFloor int) bool {
	if len(value) >= plainWordFloor {
		return true
	}
	for i := 0; i < len(value); i++ {
		char := value[i]
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' {
			continue
		}
		return true
	}
	return false
}

// keepNonCredentialValue 返回规则的 keep 谓词：不像凭据的值原样保留。
func keepNonCredentialValue(plainWordFloor int) func(string) bool {
	return func(value string) bool { return !looksLikeCredentialValue(value, plainWordFloor) }
}

// keepNonSecretValue 是 key=value 规则的 keep 谓词：占位符（幂等）与模板引用
// （`${API_KEY}`、`$API_KEY`）都原样保留——后者不含秘密，抹掉只会破坏配置模板。
func keepNonSecretValue(value string) bool {
	return isMarker(value) || isTemplateReference(value)
}

// isTemplateReference 报告值是否是变量引用而不是字面量秘密。
func isTemplateReference(value string) bool {
	if len(value) < 2 || value[0] != '$' {
		return false
	}
	body := value[1:]
	if body[0] == '{' {
		if body[len(body)-1] != '}' {
			return false
		}
		body = body[1 : len(body)-1]
	}
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		char := body[i]
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char == '_':
		case char >= '0' && char <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// isMarker 报告 s 是否已经是占位符。用于保证幂等：Redact(Redact(x)) == Redact(x)。
func isMarker(s string) bool { return strings.HasPrefix(s, markerRedacted) }

// apply 对 s 应用一条规则；没有匹配时原样返回（不分配）。
func (r rule) apply(s string) string {
	if !containsAny(s, r.needles, r.fold) {
		return s
	}
	locations := r.re.FindAllStringSubmatchIndex(s, -1)
	if locations == nil {
		return s
	}
	var builder strings.Builder
	builder.Grow(len(s))
	last := 0
	changed := false
	for _, location := range locations {
		index := 2 * r.group
		if index+1 >= len(location) || location[index] < 0 {
			continue
		}
		start, end := location[index], location[index+1]
		if start < last {
			continue
		}
		if r.keep != nil && r.keep(s[start:end]) {
			continue
		}
		builder.WriteString(s[last:start])
		builder.WriteString(markerRedacted)
		last = end
		changed = true
	}
	if !changed {
		return s
	}
	builder.WriteString(s[last:])
	return builder.String()
}

// containsAny 报告 s 是否包含 needles 中的任意一个；fold 为真时按 ASCII 大小写
// 不敏感比较。两者都不分配内存。
func containsAny(s string, needles []string, fold bool) bool {
	for _, needle := range needles {
		if fold {
			if containsFold(s, needle) {
				return true
			}
			continue
		}
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// containsFold 是 ASCII 大小写不敏感的 strings.Contains，且不分配内存。
func containsFold(s, substr string) bool {
	if substr == "" {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	first := lowerASCII(substr[0])
	for i := 0; i+len(substr) <= len(s); i++ {
		if lowerASCII(s[i]) != first {
			continue
		}
		matched := true
		for j := 1; j < len(substr); j++ {
			if lowerASCII(s[i+j]) != lowerASCII(substr[j]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// lowerASCII 把 ASCII 大写字母转小写；非 ASCII 字节原样返回（凭据都是 ASCII）。
func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
