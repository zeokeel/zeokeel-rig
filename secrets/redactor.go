package secrets

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	// Marker 是未命名凭据的替换文案。
	Marker = "[REDACTED]"

	// markerRedacted 是内部用的同值常量：命名占位符以它为前缀，因此 isMarker
	// 同时覆盖 [REDACTED] 与 [REDACTED:NAME]。
	markerRedacted = "[REDACTED]"
	// markerNamedPrefix / markerNamedSuffix 组成 [REDACTED:name]。
	markerNamedPrefix = "[REDACTED:"
	markerNamedSuffix = "]"

	// minKnownLen 是登记已知明文的最小字节数。太短的值（如 "true"、"1234"）会在
	// 正常文本里大面积误伤，收益却几乎为零。
	minKnownLen = 8
	// maxNameLen 是占位符里标签的最大长度（净化后按字节截断）。
	maxNameLen = 32
)

// knownSecret 是一条已登记的明文及其替换文案。
type knownSecret struct {
	secret      string
	replacement string
}

// snapshot 是已知明文集合的不可变快照。Add 用写时复制替换整份快照，因此 Redact
// 的读路径不需要持锁。
type snapshot struct {
	// secrets 按长度降序排列：长值先替换，避免短值把长值切出一个仍然敏感的残片。
	secrets []knownSecret
}

// Redactor 做凭据脱敏，并发安全，零值可用。
//
// 零值（或 New() 不带参数）只做内置模式脱敏；登记已知明文后，字面量匹配优先于
// 模式匹配，因此宿主正在使用的凭据即使不符合任何内置模式（例如内部服务的自定义
// token）也能被替换。
type Redactor struct {
	// mu 只保护写入路径（Add/AddNamed）的读-改-写。
	mu sync.Mutex
	// state 是当前快照；nil 表示没有任何已知明文。
	state atomic.Pointer[snapshot]
}

// New 返回一个登记了给定已知明文的 Redactor。空串与短于 8 字节的值被忽略。
func New(known ...string) *Redactor {
	r := &Redactor{}
	// 不变量：New 之后 state 永不为 nil。
	//
	// 内部读路径本来都用 `if current := r.state.Load(); current != nil` 兜住了，
	// 但"有时 nil 有时非 nil"会让任何新加的读路径都可能踩空（真的踩过：并发用例
	// 在读 r.state.Load().secrets 时 nil deref）。这里预置一次空快照，代价是一次
	// 分配，换来一条无条件的简单不变量。
	empty := &snapshot{}
	r.state.Store(empty)
	for _, secret := range known {
		r.Add(secret)
	}
	return r
}

// Add 登记一条已知明文（例如当前 API key）。空值与短于 8 字节的值被忽略；
// 与占位符本身重叠的值（如 "REDACTED"）也被忽略，否则会破坏幂等性。
//
// 登记后立即对所有正在进行的 Redact（含流式 Writer/Reader）生效。
func (r *Redactor) Add(secret string) {
	r.add("", secret)
}

// AddNamed 登记一条已知明文，并让替换文案带上标签：[REDACTED:name]。标签只保留
// 字母、数字、'_'、'-'、'.'；其余字符替换为 '_'，避免破坏占位符本身。
//
// name 为空（或净化后为空）时等价于 Add。同一个值重复登记时保留首次登记的标签。
func (r *Redactor) AddNamed(name, secret string) {
	r.add(name, secret)
}

// add 是 Add/AddNamed 的共同实现。
func (r *Redactor) add(name, secret string) {
	if r == nil {
		return
	}
	// 凭据两侧的空白（环境变量尾随换行很常见）不属于凭据本体。
	secret = strings.TrimSpace(secret)
	if len(secret) < minKnownLen {
		return
	}
	replacement := markerRedacted
	if label := sanitizeName(name); label != "" {
		replacement = markerNamedPrefix + label + markerNamedSuffix
	}
	// 与占位符重叠的明文会让 Redact(Redact(x)) 继续变化，必须拒绝登记。
	if strings.Contains(replacement, secret) {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var current []knownSecret
	if existing := r.state.Load(); existing != nil {
		current = existing.secrets
	}
	next := make([]knownSecret, len(current), len(current)+1)
	copy(next, current)
	for i := range next {
		if next[i].secret != secret {
			continue
		}
		// 同一个值重复登记：只有"从无名升级为有标签"才更新。
		if next[i].replacement == markerRedacted && replacement != markerRedacted {
			next[i].replacement = replacement
			r.state.Store(&snapshot{secrets: sortByLengthDesc(next)})
		}
		return
	}
	next = append(next, knownSecret{secret: secret, replacement: replacement})
	r.state.Store(&snapshot{secrets: sortByLengthDesc(next)})
}

// sortByLengthDesc 按明文长度降序排列（稳定，长度相同的保持登记顺序）。
func sortByLengthDesc(secrets []knownSecret) []knownSecret {
	for i := 1; i < len(secrets); i++ {
		for j := i; j > 0 && len(secrets[j].secret) > len(secrets[j-1].secret); j-- {
			secrets[j], secrets[j-1] = secrets[j-1], secrets[j]
		}
	}
	return secrets
}

// sanitizeName 把标签净化为占位符里安全的字符集，并按 maxNameLen 截断。
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(name))
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '_', char == '-', char == '.':
			builder.WriteRune(char)
		default:
			builder.WriteByte('_')
		}
		if builder.Len() >= maxNameLen {
			break
		}
	}
	return strings.Trim(builder.String(), "_")
}

// Empty 报告是否没有登记任何已知明文。
//
// 注意：Empty() 为真**不代表** Redact 是空操作——内置模式脱敏始终生效。它只用来
// 判断"是否需要额外登记"，以及让调用方跳过不必要的包装。
func (r *Redactor) Empty() bool {
	if r == nil {
		return true
	}
	current := r.state.Load()
	return current == nil || len(current.secrets) == 0
}

// Redact 返回脱敏后的 s。已知明文先替换（与长度无关），随后逐条应用内置模式。
//
// 幂等：Redact(Redact(x)) == Redact(x)。
func (r *Redactor) Redact(s string) string {
	if s == "" || r == nil {
		return s
	}
	if current := r.state.Load(); current != nil {
		for _, known := range current.secrets {
			if strings.Contains(s, known.secret) {
				s = strings.ReplaceAll(s, known.secret, known.replacement)
			}
		}
	}
	// 每条规则自带字面量前置检查：不含任何相关字面量的文本连正则都不会跑。
	for _, rule := range rules {
		s = rule.apply(s)
	}
	return s
}

// RedactBytes 是 Redact 的 []byte 版本。内容没有变化时返回原切片（不复制）。
func (r *Redactor) RedactBytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	source := string(b)
	redacted := r.Redact(source)
	if redacted == source {
		return b
	}
	return []byte(redacted)
}

// RedactJSON 对 JSON 载荷做结构保留式脱敏：只有字符串**值**会被改写，键名、
// 空白、键序、数字/布尔/null 全部逐字节保留。
//
// 两层判定：
//   - 字符串值先按 Redact 的模式/已知明文脱敏；
//   - 若该值位于**敏感键**（password/token/api_key/secret/pwd 等）之下，整个字符串
//     值直接置为占位符。敏感范围是"该键的整个值"，因此数组里的每个字符串元素、
//     嵌套对象里的每个字符串值都会被替换（早期版本只替换数组第一个元素、且完全
//     放过嵌套对象——那是一条明文泄露通道，见 audit_leak_test.go）。
//
// 已经是占位符的值原样保留（幂等）；`${VAR}` 这类模板引用也保留（不含秘密，
// 抹掉只会破坏配置模板）。数字/布尔/null 即使挂在敏感键下也逐字节保留：改类型会
// 破坏调用方的 JSON 契约，这一条是刻意的取舍（见包文档与审计报告 §D）。
//
// 输入不是合法 JSON 时退化为纯文本脱敏并按原字节布局返回，因此本函数不会因为
// 半截报文（流式输出、被截断的工具结果）而失败。错误返回值保留给将来的编码失败，
// 当前实现恒为 nil。
func (r *Redactor) RedactJSON(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return b, nil
	}
	if !json.Valid(b) {
		return r.RedactBytes(b), nil
	}
	out := make([]byte, 0, len(b))
	// depth 是当前容器深度；sensitiveDepth >= 0 表示"正处在某个敏感键的值里"，
	// 此时所有字符串值都要替换；pendingSensitive 表示刚读完一个敏感键、值还没开始。
	depth := 0
	sensitiveDepth := -1
	pendingSensitive := false
	i := 0
	for i < len(b) {
		switch char := b[i]; char {
		case '{', '[':
			depth++
			if pendingSensitive {
				sensitiveDepth = depth
				pendingSensitive = false
			}
			out = append(out, char)
			i++
			continue
		case '}', ']':
			if sensitiveDepth >= 0 && depth == sensitiveDepth {
				sensitiveDepth = -1
			}
			pendingSensitive = false
			depth--
			out = append(out, char)
			i++
			continue
		case ',':
			pendingSensitive = false
			out = append(out, char)
			i++
			continue
		}
		if b[i] != '"' {
			out = append(out, b[i])
			i++
			continue
		}
		// 找到一个 JSON 字符串字面量的结束位置（跳过转义）。
		end := i + 1
		for end < len(b) {
			if b[end] == '\\' {
				end += 2
				continue
			}
			if b[end] == '"' {
				end++
				break
			}
			end++
		}
		raw := b[i:end]
		// 跳过空白后紧跟 ':' 说明这是对象的键，键名一律不动。
		probe := end
		for probe < len(b) && isJSONSpace(b[probe]) {
			probe++
		}
		if probe < len(b) && b[probe] == ':' {
			out = append(out, raw...)
			if name, err := decodeJSONString(raw); err == nil && sensitiveKey(name) {
				pendingSensitive = true
			}
			i = end
			continue
		}
		// 字符串值。
		sensitive := sensitiveDepth >= 0 || pendingSensitive
		pendingSensitive = false
		decoded, err := decodeJSONString(raw)
		if err != nil {
			out = append(out, raw...)
			i = end
			continue
		}
		if sensitive {
			if decoded == "" || isMarker(decoded) || isTemplateReference(decoded) {
				out = append(out, raw...)
				i = end
				continue
			}
			encoded, marshalErr := json.Marshal(markerRedacted)
			if marshalErr != nil {
				out = append(out, raw...)
				i = end
				continue
			}
			out = append(out, encoded...)
			i = end
			continue
		}
		redacted := r.Redact(decoded)
		if redacted == decoded {
			out = append(out, raw...)
			i = end
			continue
		}
		encoded, marshalErr := json.Marshal(redacted)
		if marshalErr != nil {
			out = append(out, raw...)
			i = end
			continue
		}
		out = append(out, encoded...)
		i = end
	}
	return out, nil
}

// decodeJSONString 把带引号的 JSON 字符串字面量解码成其真实值。
func decodeJSONString(raw []byte) (string, error) {
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}
	return decoded, nil
}

// isJSONSpace 报告 b 是否是 JSON 允许的空白。
func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// sensitiveKeyFragments 是"整值脱敏"的键名片段（含这些片段即视为敏感）。
var sensitiveKeyFragments = []string{
	"password", "passwd", "passphrase", "secret", "apikey", "privatekey",
	"credential", "authorization", "bearer",
}

// sensitiveKey 报告一个 JSON 键名是否表示凭据。比较前会去掉 '-'、'_'、空格并转
// 小写，因此 api_key / apiKey / API-KEY 都命中。
func sensitiveKey(name string) bool {
	squashed := strings.ToLower(strings.TrimSpace(name))
	replacer := strings.NewReplacer("-", "", "_", "", " ", "", ".", "")
	squashed = replacer.Replace(squashed)
	if squashed == "" {
		return false
	}
	for _, fragment := range sensitiveKeyFragments {
		if strings.Contains(squashed, fragment) {
			return true
		}
	}
	// token / auth 作为片段太宽（token_count、author 都含），只做整词与前后缀匹配。
	switch squashed {
	case "token", "authtoken", "accesstoken", "refreshtoken", "idtoken", "sessiontoken", "auth",
		// 与文本规则 21（key=value）的键名表对齐：文本路径把 pwd 当敏感键，JSON 路径不能漏。
		"pwd", "pass", "passcode", "pin", "passwd":
		return true
	}
	return strings.HasSuffix(squashed, "token")
}
