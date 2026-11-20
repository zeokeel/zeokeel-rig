package memoryvfs

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind 是上下文条目的类别，对应 OpenViking 的三类上下文加规则/会话/目录。
type Kind string

const (
	KindMemory   Kind = "memory"
	KindResource Kind = "resource"
	KindSkill    Kind = "skill"
	KindRule     Kind = "rule"
	KindSession  Kind = "session"
	KindDir      Kind = "dir"
)

// Valid 报告 k 是否是已知类别。
func (k Kind) Valid() bool {
	switch k {
	case KindMemory, KindResource, KindSkill, KindRule, KindSession, KindDir:
		return true
	}
	return false
}

// 内置记忆类型（对应 OpenViking 的 memory types）。
const (
	TypeProfile      = "profile"
	TypePreferences  = "preferences"
	TypeEntities     = "entities"
	TypeEvents       = "events"
	TypeCases        = "cases"
	TypeTrajectories = "trajectories"
	TypeExperiences  = "experiences"
	TypeTools        = "tools"
	TypeSkills       = "skills"
	TypeRules        = "rules"
	TypePages        = "pages" // 旧 wiki 页迁移后的落点
)

// MemoryTypes 返回全部内置记忆类型（不含 resources/skills 目录）。
func MemoryTypes() []string {
	return []string{
		TypeProfile, TypePreferences, TypeEntities, TypeEvents, TypeCases,
		TypeTrajectories, TypeExperiences, TypeTools, TypeSkills,
	}
}

// Level 是内容层级：L0 摘要、L1 概览、L2 正文。
type Level int

const (
	Level0 Level = 0 // 摘要
	Level1 Level = 1 // 概览
	Level2 Level = 2 // 正文
	// LevelAny 只用于 Query.Levels：表示不限层级。
	LevelAny Level = -1
)

// String 返回 "L0"/"L1"/"L2"/"any"。
func (l Level) String() string {
	switch l {
	case Level0:
		return "L0"
	case Level1:
		return "L1"
	case Level2:
		return "L2"
	case LevelAny:
		return "any"
	}
	return fmt.Sprintf("L%d", int(l))
}

// Mode 是检索模式：quick 单查询、thinking 意图分析+多查询、context 组装注入块。
type Mode string

const (
	ModeQuick    Mode = "quick"
	ModeThinking Mode = "thinking"
	ModeContext  Mode = "context"
)

// Valid 报告 m 是否是已知模式（空串视为 quick）。
func (m Mode) Valid() bool {
	switch m {
	case "", ModeQuick, ModeThinking, ModeContext:
		return true
	}
	return false
}

// URI 是 `mem://` 命名空间里的绝对地址。
type URI string

// MaxURILength 是 URI 的长度上限。
const MaxURILength = 512

// Scheme 是 URI 的固定前缀。
const Scheme = "mem://"

// ParseURI 校验并规范化一个 URI：必须是 `mem://` 开头、路径段只含小写字母/数字/
// `.`/`_`/`-`，不能有空段、`.`、`..`、查询串或片段。
//
// 空串会返回 ErrInvalidURI；调用方不要用零值 URI 表示"根"，用 RootURI。
func ParseURI(raw string) (URI, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidURI)
	}
	if len(trimmed) > MaxURILength {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidURI, MaxURILength)
	}
	if !strings.HasPrefix(trimmed, Scheme) {
		return "", fmt.Errorf("%w: must start with %q", ErrInvalidURI, Scheme)
	}
	if strings.ContainsAny(trimmed, "?#\\ \t\r\n") {
		return "", fmt.Errorf("%w: must not contain spaces, '?', '#', '\\' or control characters", ErrInvalidURI)
	}
	path := strings.TrimPrefix(trimmed, Scheme)
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return URI(Scheme), nil
	}
	segments := strings.Split(path, "/")
	for _, segment := range segments {
		if segment == "" {
			return "", fmt.Errorf("%w: empty path segment", ErrInvalidURI)
		}
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: relative segment %q", ErrInvalidURI, segment)
		}
		for _, r := range segment {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
				continue
			}
			return "", fmt.Errorf("%w: segment %q must use a-z, 0-9, '.', '_' or '-'", ErrInvalidURI, segment)
		}
	}
	return URI(Scheme + path), nil
}

// RootURI 是命名空间根。
const RootURI URI = URI(Scheme)

// String 返回 URI 原文。
func (u URI) String() string { return string(u) }

// IsZero 报告 URI 是否未设置。
func (u URI) IsZero() bool { return u == "" }

// Segments 返回路径段（根返回空切片）。
func (u URI) Segments() []string {
	path := strings.TrimPrefix(string(u), Scheme)
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// Base 返回最后一段（根返回空串）。
func (u URI) Base() string {
	segments := u.Segments()
	if len(segments) == 0 {
		return ""
	}
	return segments[len(segments)-1]
}

// Parent 返回父 URI；根返回自身。
func (u URI) Parent() URI {
	segments := u.Segments()
	if len(segments) <= 1 {
		return RootURI
	}
	return URI(Scheme + strings.Join(segments[:len(segments)-1], "/"))
}

// Child 返回拼接后的子 URI，并校验 name 是单个合法路径段。
func (u URI) Child(name string) (URI, error) {
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: child %q is not a single segment", ErrInvalidURI, name)
	}
	raw := strings.TrimSuffix(string(u), "/") + "/" + name
	return ParseURI(raw)
}

// IsUnder 报告 u 是否等于 prefix 或位于其下（按段比较，`mem://a/b` 不包含 `mem://a/bc`）。
func (u URI) IsUnder(prefix URI) bool {
	if prefix == RootURI {
		return true
	}
	if u == prefix {
		return true
	}
	return strings.HasPrefix(string(u), strings.TrimSuffix(string(prefix), "/")+"/")
}

// Join 用 "/" 拼接并校验，供内部构造 URI 使用。
func Join(parts ...string) (URI, error) {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(part, "/")
		if part == "" {
			continue
		}
		cleaned = append(cleaned, part)
	}
	return ParseURI(Scheme + strings.Join(cleaned, "/"))
}

// Freshness 描述一个目录 sidecar 对其直接子项的覆盖情况。
type Freshness struct {
	TotalEntries   int       `json:"total_entries"`
	SampledEntries int       `json:"sampled_entries"`
	PendingChanges int       `json:"pending_changes,omitempty"`
	Updated        time.Time `json:"updated"`
}

// Entry 是记忆/资源/技能/规则/会话/目录的统一数据载体。
//
// Summary/Overview/Content 对应 L0/L1/L2；查询结果的 Score 与 MatchedLevel 只在
// 检索返回时填充。
type Entry struct {
	URI       URI               `json:"uri"`
	Kind      Kind              `json:"kind"`
	Type      string            `json:"type,omitempty"`
	Title     string            `json:"title,omitempty"`
	Summary   string            `json:"summary,omitempty"`
	Overview  string            `json:"overview,omitempty"`
	Content   string            `json:"content,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	Links     []URI             `json:"links,omitempty"`
	Source    string            `json:"source,omitempty"`
	Created   time.Time         `json:"created"`
	Updated   time.Time         `json:"updated"`
	Version   int64             `json:"version,omitempty"`
	Tokens    int               `json:"tokens,omitempty"`
	Pinned    bool              `json:"pinned,omitempty"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
	// LastAccess 由宿主在读取/展示后写回（SDK 不自动写，避免读放大）；
	// GC 用 max(Updated, LastAccess) 判衰减。
	LastAccess *time.Time `json:"last_access,omitempty"`
	Freshness  *Freshness `json:"freshness,omitempty"`

	// 以下字段只在检索结果里有意义。
	Score        float64 `json:"score,omitempty"`
	MatchedLevel Level   `json:"matched_level,omitempty"`
}

// Clone 返回浅拷贝（Tags/Links 复制，避免调用方改到存储内部）。
func (e Entry) Clone() Entry {
	out := e
	if e.Tags != nil {
		out.Tags = make(map[string]string, len(e.Tags))
		for k, v := range e.Tags {
			out.Tags[k] = v
		}
	}
	if e.Links != nil {
		out.Links = append([]URI(nil), e.Links...)
	}
	if e.ExpiresAt != nil {
		t := *e.ExpiresAt
		out.ExpiresAt = &t
	}
	if e.LastAccess != nil {
		t := *e.LastAccess
		out.LastAccess = &t
	}
	if e.Freshness != nil {
		f := *e.Freshness
		out.Freshness = &f
	}
	return out
}

// TokenEstimate 估算以 level 为上限注入时的 token 数（不含标题与标签）。
func (e Entry) TokenEstimate(level Level) int {
	switch level {
	case Level0:
		return EstimateTokens(e.Summary)
	case Level1:
		return EstimateTokens(e.Summary) + EstimateTokens(e.Overview)
	default:
		return EstimateTokens(e.Summary) + EstimateTokens(e.Overview) + EstimateTokens(e.Content)
	}
}

// Query 是一次检索请求。零值可用：默认 limit 10、层级不限、模式 quick。
type Query struct {
	Text        string            `json:"text"`
	Scope       URI               `json:"scope,omitempty"`
	Kinds       []Kind            `json:"kinds,omitempty"`
	Types       []string          `json:"types,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	Levels      []Level           `json:"levels,omitempty"`
	Mode        Mode              `json:"mode,omitempty"`
	SessionID   string            `json:"session_id,omitempty"`
	Limit       int               `json:"limit,omitempty"`
	Offset      int               `json:"offset,omitempty"`
	TokenBudget int               `json:"token_budget,omitempty"`
	Threshold   float64           `json:"threshold,omitempty"`
	IncludeBody bool              `json:"include_body,omitempty"`
	// Vector 是调用方预先算好的查询向量（宿主 Embedder）；为空时后端自行处理。
	Vector []float32 `json:"-"`
	// UseHashVector 允许在没有 Query.Vector 时用确定性词袋向量兜底。它只用于
	// 测试/管线演示：真实文档向量与 hash 向量不在同一空间，混用会召回噪声。
	UseHashVector bool `json:"-"`
	// DisableVector 由后端在"没有 Embedder / embedding 失败"时置位，确保查询
	// 不会退化成 hash 向量去污染真实向量空间。
	DisableVector bool `json:"-"`
}

// Result 是一次检索的结果。
type Result struct {
	Entries []Entry       `json:"entries"`
	Total   int           `json:"total"`
	Tokens  int           `json:"tokens"`
	Plan    []TypedQuery  `json:"plan,omitempty"`
	Took    time.Duration `json:"took,omitempty"`
}

// TypedQuery 是意图分析产出的一条带类型查询。
type TypedQuery struct {
	Text     string    `json:"text"`
	Kind     Kind      `json:"kind,omitempty"`
	Type     string    `json:"type,omitempty"`
	Priority int       `json:"priority,omitempty"`
	Vector   []float32 `json:"-"`
}

// Message 是会话里的一条消息（Capture/Commit 使用）。
type Message struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	Name    string    `json:"name,omitempty"`
	At      time.Time `json:"at,omitempty"`
	// Key 是调用方/后端用于幂等去重的稳定键。为空时后端按 role|content|原始 At
	// 生成（At 为零则忽略时间），因此同一条零时间戳消息重复 Capture 只存一次。
	Key string `json:"key,omitempty"`
}

// Candidate 是抽取器产出的候选记忆，尚未落库。
type Candidate struct {
	Kind       Kind              `json:"kind"`
	Type       string            `json:"type"`
	Title      string            `json:"title,omitempty"`
	Summary    string            `json:"summary"`
	Content    string            `json:"content,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Links      []URI             `json:"links,omitempty"`
	Source     string            `json:"source,omitempty"`
	Confidence float64           `json:"confidence,omitempty"`
}

// Rule 是一条可检索的行为规则。
type Rule struct {
	URI         URI     `json:"uri"`
	Content     string  `json:"content"`
	Scenario    string  `json:"scenario,omitempty"`
	Probability float64 `json:"probability"`
}

// Snapshot 是一次版本快照。
type Snapshot struct {
	ID      string    `json:"id"`
	Label   string    `json:"label,omitempty"`
	Created time.Time `json:"created"`
	Entries int       `json:"entries"`
	Bytes   int64     `json:"bytes"`
}

// Stats 是存储的本地统计。
type Stats struct {
	Entries   int            `json:"entries"`
	Dirs      int            `json:"dirs"`
	Bytes     int64          `json:"bytes"`
	Tokens    int            `json:"tokens"`
	ByKind    map[Kind]int   `json:"by_kind,omitempty"`
	ByType    map[string]int `json:"by_type,omitempty"`
	Vectors   int            `json:"vectors,omitempty"`
	Sessions  int            `json:"sessions,omitempty"`
	Snapshots int            `json:"snapshots,omitempty"`
	Oldest    time.Time      `json:"oldest,omitempty"`
	Newest    time.Time      `json:"newest,omitempty"`
}

// GCOptions 控制一次回收。
type GCOptions struct {
	MaxEntries int           `json:"max_entries,omitempty"`
	MaxBytes   int64         `json:"max_bytes,omitempty"`
	TTL        time.Duration `json:"ttl,omitempty"`
	DryRun     bool          `json:"dry_run,omitempty"`
	// IncludeSessions 为 true 时才允许 GC 删除会话归档/工作记忆。默认 false：
	// 它们是"原始转录"，过期淘汰应由宿主显式决定，而不是被记忆配额顺手清掉。
	IncludeSessions bool `json:"include_sessions,omitempty"`
}

// GCResult 是一次回收的结果。
type GCResult struct {
	Expired      int   `json:"expired"`
	Evicted      int   `json:"evicted"`
	RemovedBytes int64 `json:"removed_bytes"`
	Kept         int   `json:"kept"`
}

// Pack 是可导出/导入的条目集合（OVPack 的等价物）。
type Pack struct {
	Version   int       `json:"version"`
	Generated time.Time `json:"generated"`
	Scope     URI       `json:"scope,omitempty"`
	Entries   []Entry   `json:"entries"`
}

// ImportOptions 控制导入行为。
type ImportOptions struct {
	Overwrite bool `json:"overwrite,omitempty"`
	DryRun    bool `json:"dry_run,omitempty"`
}

// ImportResult 是一次导入的结果。
type ImportResult struct {
	Imported int `json:"imported"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
}

// CommitOptions 控制一次会话提交。
type CommitOptions struct {
	Extract       bool    `json:"extract,omitempty"`
	WorkingMemory bool    `json:"working_memory,omitempty"`
	TokenBudget   int     `json:"token_budget,omitempty"`
	MinConfidence float64 `json:"min_confidence,omitempty"`
}

// CommitResult 是一次会话提交的结果。
type CommitResult struct {
	Archive URI   `json:"archive,omitempty"`
	Written []URI `json:"written,omitempty"`
	Merged  []URI `json:"merged,omitempty"`
	Skipped int   `json:"skipped,omitempty"`
	Tokens  int   `json:"tokens,omitempty"`
}

// SessionState 是一个会话的待提交状态。
type SessionState struct {
	ID         string    `json:"id"`
	Pending    int       `json:"pending"`
	Tokens     int       `json:"tokens"`
	Updated    time.Time `json:"updated"`
	LastCommit time.Time `json:"last_commit,omitempty"`
}

// EstimateTokens 是保守的 token 估算：CJK 字符按 1 token，其余每 4 字节 1 token。
// 它只用于预算与截断决策，不用于计费。
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	cjk := 0
	other := 0
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
			continue
		}
		other += utf8.RuneLen(r)
	}
	tokens := cjk + (other+3)/4
	if tokens == 0 {
		return 1
	}
	return tokens
}

// TruncateTokens 把 text 截到约 budget 个 token（按 rune 边界），返回截断结果与
// 是否发生截断。估算不精确，宁可少给也不超太多。
func TruncateTokens(text string, budget int) (string, bool) {
	if budget <= 0 {
		return "", text != ""
	}
	if EstimateTokens(text) <= budget {
		return text, false
	}
	var builder strings.Builder
	used := 0.0
	for _, r := range text {
		cost := float64(utf8.RuneLen(r)) / 4
		if isCJK(r) {
			cost = 1
		}
		if used+cost > float64(budget) {
			return builder.String(), true
		}
		builder.WriteRune(r)
		used += cost
	}
	return builder.String(), false
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}
