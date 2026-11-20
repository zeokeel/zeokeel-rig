package memoryvfs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// 哨兵错误。调用方一律用 errors.Is 判断。
var (
	// ErrInvalidURI 表示 URI 不合法（不是 mem://、含非法段、过长等）。
	ErrInvalidURI = errors.New("memoryvfs: invalid uri")
	// ErrInvalidEntry 表示条目字段不合法（类别/类型/长度）。
	ErrInvalidEntry = errors.New("memoryvfs: invalid entry")
	// ErrSummaryRequired 表示记忆类条目缺少 L0 摘要。
	ErrSummaryRequired = errors.New("memoryvfs: summary is required")
	// ErrNotFound 表示 URI 不存在。
	ErrNotFound = errors.New("memoryvfs: not found")
	// ErrExists 表示目标已存在且调用不允许覆盖。
	ErrExists = errors.New("memoryvfs: already exists")
	// ErrConflict 表示版本冲突或同一 URI 被并发修改。
	ErrConflict = errors.New("memoryvfs: conflict")
	// ErrQuotaExceeded 表示条目数或字节数超过配额。
	ErrQuotaExceeded = errors.New("memoryvfs: quota exceeded")
	// ErrUnsupported 表示后端不支持该能力（可选能力未实现）。
	ErrUnsupported = errors.New("memoryvfs: unsupported")
	// ErrClosed 表示 Store 已关闭。
	ErrClosed = errors.New("memoryvfs: closed")
	// ErrCorrupt 表示磁盘数据损坏且无法安全读取。
	ErrCorrupt = errors.New("memoryvfs: corrupt data")
)

// 条目字段上限。
const (
	MaxTitleBytes    = 200
	MaxSummaryBytes  = 512
	MaxOverviewBytes = 16 << 10
	MaxContentBytes  = 1 << 20
	MaxTags          = 64
	MaxTagKeyBytes   = 64
	MaxTagValueBytes = 256
	MaxSessionID     = 128
)

// ValidateURI 校验 raw 并返回规范化 URI。
func ValidateURI(raw string) (URI, error) { return ParseURI(raw) }

// ValidateSessionID 校验会话 ID：非空、≤128 字节、不含路径分隔符与控制字符。
func ValidateSessionID(id string) error {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return fmt.Errorf("%w: session id is empty", ErrInvalidEntry)
	}
	if len(trimmed) > MaxSessionID {
		return fmt.Errorf("%w: session id longer than %d bytes", ErrInvalidEntry, MaxSessionID)
	}
	if strings.ContainsAny(trimmed, "/\\\x00\r\n\t") || trimmed == "." || trimmed == ".." {
		return fmt.Errorf("%w: session id %q contains a path separator or control character", ErrInvalidEntry, id)
	}
	return nil
}

// ValidateEntry 校验条目的必填字段与长度上限。它不检查 URI 是否已存在。
func ValidateEntry(entry Entry) error {
	if _, err := ParseURI(string(entry.URI)); err != nil {
		return err
	}
	if !entry.Kind.Valid() {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidEntry, entry.Kind)
	}
	if entry.Kind == KindDir {
		return nil
	}
	if entry.Kind == KindMemory {
		if strings.TrimSpace(entry.Type) == "" {
			return fmt.Errorf("%w: memory entry needs a type (profile/preferences/entities/events/cases/trajectories/experiences/tools/skills)", ErrInvalidEntry)
		}
		if !validSlug(entry.Type) {
			return fmt.Errorf("%w: memory type %q must match [a-z0-9-]+", ErrInvalidEntry, entry.Type)
		}
	}
	if strings.TrimSpace(entry.Summary) == "" {
		return fmt.Errorf("%w: entry %s", ErrSummaryRequired, entry.URI)
	}
	if len(entry.Summary) > MaxSummaryBytes {
		return fmt.Errorf("%w: summary is %d bytes (max %d)", ErrInvalidEntry, len(entry.Summary), MaxSummaryBytes)
	}
	if len(entry.Overview) > MaxOverviewBytes {
		return fmt.Errorf("%w: overview is %d bytes (max %d)", ErrInvalidEntry, len(entry.Overview), MaxOverviewBytes)
	}
	if len(entry.Content) > MaxContentBytes {
		return fmt.Errorf("%w: content is %d bytes (max %d)", ErrInvalidEntry, len(entry.Content), MaxContentBytes)
	}
	if len(entry.Title) > MaxTitleBytes {
		return fmt.Errorf("%w: title is %d bytes (max %d)", ErrInvalidEntry, len(entry.Title), MaxTitleBytes)
	}
	if len(entry.Tags) > MaxTags {
		return fmt.Errorf("%w: %d tags (max %d)", ErrInvalidEntry, len(entry.Tags), MaxTags)
	}
	for key, value := range entry.Tags {
		if key == "" || len(key) > MaxTagKeyBytes || len(value) > MaxTagValueBytes {
			return fmt.Errorf("%w: tag %q is empty or too long", ErrInvalidEntry, key)
		}
	}
	for _, link := range entry.Links {
		if _, err := ParseURI(string(link)); err != nil {
			return fmt.Errorf("%w: link %q: %v", ErrInvalidEntry, link, err)
		}
	}
	return nil
}

func validSlug(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// NormalizeEntry 返回一个规整后的副本：去首尾空白、折叠摘要空白、补标题与时间。
// now 为 nil 时用 time.Now().UTC()。
func NormalizeEntry(entry Entry, now func() time.Time) Entry {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	out := entry.Clone()
	out.Summary = CollapseWhitespace(out.Summary)
	out.Title = strings.TrimSpace(out.Title)
	if out.Title == "" {
		out.Title = out.URI.Base()
	}
	if out.Created.IsZero() {
		out.Created = now().UTC()
	}
	if out.Updated.IsZero() {
		out.Updated = out.Created
	}
	out.Tokens = EstimateTokens(out.Summary) + EstimateTokens(out.Overview) + EstimateTokens(out.Content)
	return out
}

// CollapseWhitespace 去首尾空白并把任意空白序列折叠成一个空格。
func CollapseWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// NormalizeQuery 返回补默认值后的查询副本：Limit 默认 10、上限 100；Mode 默认
// quick；Levels 为空表示不限；Offset 为负归零。
func NormalizeQuery(query Query) Query {
	out := query
	out.Text = strings.TrimSpace(out.Text)
	if out.Mode == "" {
		out.Mode = ModeQuick
	}
	if out.Limit <= 0 {
		out.Limit = 10
	}
	if out.Limit > 100 {
		out.Limit = 100
	}
	if out.Offset < 0 {
		out.Offset = 0
	}
	if out.Threshold < 0 {
		out.Threshold = 0
	}
	if out.Threshold > 1 {
		out.Threshold = 1
	}
	return out
}

// TruncateBytes 把 text 截到 limit 字节以内（切在 rune 边界），返回结果与是否截断。
func TruncateBytes(text string, limit int) (string, bool) {
	if limit <= 0 {
		return "", text != ""
	}
	if len(text) <= limit {
		return text, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

// Embedder 把文本变成向量。Dimension 必须稳定；不同维度会让旧向量失效。
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimension() int
}

// Reranker 对候选做精排。返回的分数顺序必须与 documents 一一对应。
type Reranker interface {
	Rerank(ctx context.Context, query string, documents []string) ([]float64, error)
}

// Summarizer 生成 L0/L1。实现可以调用模型；nil 时使用确定性摘要。
// Write/Commit/CompileRules/Import/Remember 都在锁外调用本接口，可以回读 Store。
type Summarizer interface {
	Summarize(ctx context.Context, entry Entry) (summary, overview string, err error)
}

// Extractor 从会话消息里抽取候选记忆。实现可以调用模型；nil 时使用启发式抽取。
// 它在 Commit 的锁外调用，可以安全回读 Store。
type Extractor interface {
	Extract(ctx context.Context, session Session) ([]Candidate, error)
}

// Session 是一次待抽取的会话。
type Session struct {
	ID       string    `json:"id"`
	Messages []Message `json:"messages"`
	Started  time.Time `json:"started"`
	Updated  time.Time `json:"updated"`
}

// IntentAnalyzer 分析查询意图并产出多条 TypedQuery。nil 时使用关键词扩展。
type IntentAnalyzer interface {
	Analyze(ctx context.Context, query string, recent []Message) ([]TypedQuery, error)
}

// Options 是后端配置。零值不可用，请从 DefaultOptions 起改。
type Options struct {
	Embedder      Embedder
	Reranker      Reranker
	Summarizer    Summarizer
	Extractor     Extractor
	Intent        IntentAnalyzer
	Now           func() time.Time
	MaxEntries    int
	MaxBytes      int64
	TTL           time.Duration
	DecayHalfLife time.Duration
	CommitTokens  int
	RecallTokens  int
	PageSize      int
	BM25Weight    float64
	VectorWeight  float64
}

// DefaultOptions 返回默认配置：2 万条 / 256 MiB / 无 TTL / 90 天半衰期 /
// 每次 commit 阈值 4000 token / 召回预算 2000 token / 默认页 20 条。
func DefaultOptions() Options {
	return Options{
		MaxEntries:    20000,
		MaxBytes:      256 << 20,
		DecayHalfLife: 90 * 24 * time.Hour,
		CommitTokens:  4000,
		RecallTokens:  2000,
		PageSize:      20,
		BM25Weight:    1,
		VectorWeight:  1,
	}
}

// Option 修改 Options。
type Option func(*Options)

func WithEmbedder(embedder Embedder) Option { return func(o *Options) { o.Embedder = embedder } }
func WithReranker(reranker Reranker) Option { return func(o *Options) { o.Reranker = reranker } }
func WithSummarizer(summarizer Summarizer) Option {
	return func(o *Options) { o.Summarizer = summarizer }
}
func WithExtractor(extractor Extractor) Option { return func(o *Options) { o.Extractor = extractor } }
func WithIntentAnalyzer(intent IntentAnalyzer) Option {
	return func(o *Options) { o.Intent = intent }
}
func WithClock(now func() time.Time) Option { return func(o *Options) { o.Now = now } }
func WithMaxEntries(limit int) Option       { return func(o *Options) { o.MaxEntries = limit } }
func WithMaxBytes(limit int64) Option       { return func(o *Options) { o.MaxBytes = limit } }
func WithTTL(ttl time.Duration) Option      { return func(o *Options) { o.TTL = ttl } }
func WithDecayHalfLife(halfLife time.Duration) Option {
	return func(o *Options) { o.DecayHalfLife = halfLife }
}
func WithCommitTokens(tokens int) Option { return func(o *Options) { o.CommitTokens = tokens } }
func WithRecallTokens(tokens int) Option { return func(o *Options) { o.RecallTokens = tokens } }
func WithPageSize(size int) Option       { return func(o *Options) { o.PageSize = size } }
func WithHybridWeights(bm25, vector float64) Option {
	return func(o *Options) {
		if bm25 >= 0 {
			o.BM25Weight = bm25
		}
		if vector >= 0 {
			o.VectorWeight = vector
		}
	}
}

// ApplyOptions 把 opts 应用到一份默认配置上。
func ApplyOptions(opts ...Option) Options {
	options := DefaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	return options
}
