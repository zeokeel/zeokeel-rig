package search

// 本文件是 search 工具的装配面：New/Option、模型可见的 Description 与 Schema、
// 参数校验与模式分发（files / text / mentions / auto）。检索实现见 fuzzy.go、
// bm25.go、mentions.go，遍历与路径校验见 fs.go。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName 是模型侧的稳定工具名。
const ToolName = "search"

// 默认与上限。
const (
	// DefaultMaxResults 是每次调用的默认结果条数。
	DefaultMaxResults = 20
	// MaxMaxResults 是 max_results 的硬上限（含按调用传入的值）。
	MaxMaxResults = 200
	// DefaultMaxFileBytes 是单文件入索引的默认上限（1 MiB）。
	DefaultMaxFileBytes = 1 << 20
	// DefaultTimeout 是一次搜索的默认预算。
	DefaultTimeout = 10 * time.Second
	// maxSnippetRunes 是命中行内容的截断长度（按字符计）。
	maxSnippetRunes = 200
)

// 模式名。
const (
	modeAuto     = "auto"
	modeFiles    = "files"
	modeText     = "text"
	modeMentions = "mentions"
)

// Option 配置搜索工具的可选行为。
type Option func(*config)

// WithMaxResults 设置默认结果条数（<=0 表示回到 DefaultMaxResults；超过
// MaxMaxResults 会被夹到上限）。
func WithMaxResults(n int) Option {
	return func(cfg *config) { cfg.maxResults = n }
}

// WithMaxFileBytes 设置单文件入索引的字节上限（<=0 表示回到 DefaultMaxFileBytes）。
func WithMaxFileBytes(n int) Option {
	return func(cfg *config) { cfg.maxFileBytes = n }
}

// WithTimeout 设置单次搜索的时间预算（<=0 表示回到 DefaultTimeout）。
func WithTimeout(d time.Duration) Option {
	return func(cfg *config) { cfg.timeout = d }
}

// config 是工具的不可变配置（New 之后不再修改）。
type config struct {
	// rootAbs 是工作区根的绝对路径（结果都相对它渲染）。
	rootAbs string
	// rootReal 是 rootAbs 解析符号链接后的真实路径（路径校验用）。
	rootReal string
	// rootErr 是 New 时解析根目录失败的原因（Invoke 时作为错误返回）。
	rootErr error
	// maxResults 是默认结果条数。
	maxResults int
	// maxFileBytes 是单文件入索引的字节上限。
	maxFileBytes int
	// timeout 是单次搜索的时间预算。
	timeout time.Duration
}

// tool 实现 rig.Tool。
type tool struct {
	config
}

var _ rig.Tool = (*tool)(nil)

// New 返回搜索工具。root 是工作区根：所有结果都必须落在它之内，遍历也不会跟随
// 符号链接逃出去。
func New(root string, opts ...Option) rig.Tool {
	cfg := config{
		maxResults:   DefaultMaxResults,
		maxFileBytes: DefaultMaxFileBytes,
		timeout:      DefaultTimeout,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.maxResults <= 0 {
		cfg.maxResults = DefaultMaxResults
	}
	cfg.maxResults = min(cfg.maxResults, MaxMaxResults)
	if cfg.maxFileBytes <= 0 {
		cfg.maxFileBytes = DefaultMaxFileBytes
	}
	if cfg.timeout <= 0 {
		cfg.timeout = DefaultTimeout
	}
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		cfg.rootErr = fmt.Errorf("search: workspace root is not configured")
		return &tool{config: cfg}
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		cfg.rootErr = fmt.Errorf("search: workspace root %q: %w", trimmed, err)
		return &tool{config: cfg}
	}
	cfg.rootAbs = absolute
	cfg.rootReal = absolute
	if real, realErr := filepath.EvalSymlinks(absolute); realErr == nil {
		cfg.rootReal = real
	}
	return &tool{config: cfg}
}

// Name 实现 rig.Tool。
func (t *tool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	return "Search the workspace by relevance instead of by exact pattern. " +
		"mode=files ranks paths by fuzzy filename match (case-insensitive subsequence; query may contain \"/\" segments). " +
		"mode=text ranks file contents with BM25 and returns hits as path:line:content; multiple words are ANDed. " +
		"mode=mentions resolves @path, @\"path with space\", @path:12 and @path:12-20 references found in query and flags missing paths. " +
		"mode=auto (default) picks mentions when query starts with @ or contains an @ amid prose, text when query has several words, " +
		"and otherwise tries files then falls back to text. Paths are relative to the workspace root and capped at max_results. " +
		"Ignores .git, node_modules, vendor, dist, build, binary files (containing NUL) and files larger than the index limit."
}

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"query": toolutil.StringProp("What to look for: a fuzzy filename query, a multi-word content query, or text containing @mentions."),
		"path":  toolutil.StringProp("Optional subdirectory of the workspace root to search (default: the whole root)."),
		"mode": map[string]any{
			"type":        "string",
			"enum":        []string{modeAuto, modeFiles, modeText, modeMentions},
			"description": "Search mode. auto (default) chooses from the query shape: mentions for @-queries, text for multi-word queries, files otherwise.",
		},
		"max_results": toolutil.IntegerProp(fmt.Sprintf(
			"Maximum number of results; default %d, capped at %d. Raise it when a result set is truncated ([truncated: N more matches]).",
			DefaultMaxResults, MaxMaxResults)),
	}, "query")
}

// arguments 是一次调用的解码参数。
type arguments struct {
	Query      string `json:"query"`
	Path       string `json:"path"`
	Mode       string `json:"mode"`
	MaxResults *int   `json:"max_results"`
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	var args arguments
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return toolutil.ErrorResultf("search: query is required"), nil
	}
	mode := strings.ToLower(strings.TrimSpace(args.Mode))
	switch mode {
	case "", modeAuto, modeFiles, modeText, modeMentions:
	default:
		return toolutil.ErrorResultf("search: unknown mode %q (want %s, %s, %s or %s)",
			args.Mode, modeAuto, modeFiles, modeText, modeMentions), nil
	}
	if mode == "" {
		mode = modeAuto
	}
	if t.rootErr != nil {
		return toolutil.ErrorResult(t.rootErr), nil
	}
	info, err := os.Stat(t.rootAbs)
	if err != nil {
		return toolutil.ErrorResultf("search: workspace root %q: %v", t.rootAbs, err), nil
	}
	if !info.IsDir() {
		return toolutil.ErrorResultf("search: workspace root %q is not a directory", t.rootAbs), nil
	}
	baseDir, err := resolveBaseDir(t.rootAbs, t.rootReal, args.Path)
	if err != nil {
		return toolutil.ErrorResult(err), nil
	}
	limit := t.maxResults
	if args.MaxResults != nil {
		if *args.MaxResults > 0 {
			limit = min(*args.MaxResults, MaxMaxResults)
		}
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	content, runErr := t.execute(timeoutCtx, baseDir, query, mode, limit)
	if runErr != nil {
		if parentErr := ctx.Err(); parentErr != nil {
			return rig.ToolResult{}, parentErr
		}
		if timeoutCtx.Err() != nil {
			return toolutil.ErrorResultf("search: timed out after %s; narrow the query or the path", t.timeout), nil
		}
		return toolutil.ErrorResult(runErr), nil
	}
	return toolutil.TextResult(content), nil
}

// execute 按模式执行一次搜索并渲染模型可见文本。
func (t *tool) execute(ctx context.Context, baseDir, query, mode string, limit int) (string, error) {
	if mode == modeAuto {
		switch autoMode(query) {
		case modeMentions:
			return t.executeMentions(baseDir, query, limit)
		case modeText:
			return t.executeTextWithHint(ctx, baseDir, query, limit)
		default:
			content, err := t.executeFiles(ctx, baseDir, query, limit)
			if err != nil || content != "" {
				return content, err
			}
			// 文件名没有命中：auto 再试一次内容检索（对标 Codex 的 search 回退）。
			return t.executeTextWithHint(ctx, baseDir, query, limit)
		}
	}
	switch mode {
	case modeFiles:
		return t.executeFiles(ctx, baseDir, query, limit)
	case modeText:
		return t.executeTextWithHint(ctx, baseDir, query, limit)
	case modeMentions:
		return t.executeMentions(baseDir, query, limit)
	default:
		return "", fmt.Errorf("search: unknown mode %q", mode)
	}
}

// autoMode 从查询形状推断模式。
//
//   - 以 @ 开头 → mentions（明确的"解析这些引用"意图）；
//   - 含空白 → text（多词内容查询）；
//   - 否则 → files（单 token 文件名查询；execute 会在无命中时回退到 text）。
//
// 自然语言整句里的 @（"解释 @src/a.go:12 这个函数"）**不再**切成 mentions：那样会
// 把句子其余部分整体丢掉。它走 text，并在结果末尾附一条提示让调用方显式改用
// mentions 模式。
func autoMode(query string) string {
	if strings.HasPrefix(query, "@") {
		return modeMentions
	}
	if looksLikeProse(query) {
		return modeText
	}
	return modeFiles
}

// mentionHint 是"查询里含 @引用、但当前模式不解析它们"时追加的提示行。
const mentionHint = "# hints: contains @references, run mode=mentions to resolve them"

// withMentionHint 在查询确实含可解析的 @引用时，给结果追加一行提示。
//
// 只对非 mentions 模式使用：mentions 模式本身已经在解析引用，不需要提示。
func withMentionHint(content, query string) string {
	if len(ParseMentions(query)) == 0 {
		return content
	}
	if content == "" {
		return mentionHint
	}
	return content + "\n" + mentionHint
}

// executeTextWithHint 执行内容检索，并在查询含 @引用时追加一行提示。
func (t *tool) executeTextWithHint(ctx context.Context, baseDir, query string, limit int) (string, error) {
	content, err := t.executeText(ctx, baseDir, query, limit)
	if err != nil {
		return "", err
	}
	return withMentionHint(content, query), nil
}

// looksLikeProse 报告查询是否像一段自然语言（含空白）。
func looksLikeProse(query string) bool {
	return strings.ContainsAny(query, " \t\r\n")
}

// scoringCheckInterval 是评分/统计循环里检查一次 ctx 的间隔（文件数）。
//
// 评分本身是 O(files × 路径长度)，十万级文件时可能吃掉整个时间预算；只在遍历里
// 检查 ctx 会让"超时"变成"算完再成功返回"。每 256 个文件检查一次，开销可以忽略。
const scoringCheckInterval = 256

// executeFiles 执行文件名模糊搜索。
func (t *tool) executeFiles(ctx context.Context, baseDir, query string, limit int) (string, error) {
	files, err := walkSourceFiles(ctx, t.rootReal, baseDir)
	if err != nil {
		return "", fmt.Errorf("search: %v", err)
	}
	return scoreCandidateFiles(ctx, files, query, limit)
}

// scoreCandidateFiles 对候选文件打分、排序并按 limit 截断。
func scoreCandidateFiles(ctx context.Context, files []sourceFile, query string, limit int) (string, error) {
	type candidate struct {
		rel   string
		score int
	}
	candidates := make([]candidate, 0, len(files))
	for index, file := range files {
		if index%scoringCheckInterval == 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", ctxErr
			}
		}
		score, matched := FuzzyScore(query, file.rel)
		if !matched {
			continue
		}
		candidates = append(candidates, candidate{rel: file.rel, score: score})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].score != candidates[right].score {
			return candidates[left].score > candidates[right].score
		}
		if len(candidates[left].rel) != len(candidates[right].rel) {
			return len(candidates[left].rel) < len(candidates[right].rel)
		}
		return candidates[left].rel < candidates[right].rel
	})
	total := len(candidates)
	if total > limit {
		candidates = candidates[:limit]
	}
	lines := make([]string, 0, len(candidates))
	for _, match := range candidates {
		lines = append(lines, match.rel)
	}
	return assembleResults(lines, total), nil
}

// executeText 执行 BM25 内容检索。
func (t *tool) executeText(ctx context.Context, baseDir, query string, limit int) (string, error) {
	lines, total, err := t.searchText(ctx, baseDir, query, limit)
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "", fmt.Errorf("search: %v", err)
	}
	return assembleResults(lines, total), nil
}

// executeMentions 解析并校验查询里的 @提及。
//
// baseDir 是解析基准：`path:"src"` + `@a.txt` 校验的是 `src/a.txt`（与 files/text
// 模式的 path 语义一致）。输出始终是相对工作区根的路径。
func (t *tool) executeMentions(baseDir, query string, limit int) (string, error) {
	mentions := ParseMentions(query)
	if len(mentions) == 0 {
		return "", nil
	}
	total := len(mentions)
	if total > limit {
		mentions = mentions[:limit]
	}
	lines := make([]string, 0, len(mentions))
	for _, mention := range mentions {
		display, exists := resolveMentionPath(t.rootReal, baseDir, mention.Path)
		if !exists {
			display = mention.Path
		}
		lines = append(lines, formatMention(display, mention, !exists))
	}
	return assembleResults(lines, total), nil
}

// formatMention 渲染一条提及（`path`、`path:12`、`path:12-20`，缺失时加 " (missing)"）。
func formatMention(displayPath string, mention Mention, missing bool) string {
	var builder strings.Builder
	builder.WriteString(displayPath)
	if mention.Line > 0 {
		if mention.EndLine > mention.Line {
			fmt.Fprintf(&builder, ":%d-%d", mention.Line, mention.EndLine)
		} else {
			fmt.Fprintf(&builder, ":%d", mention.Line)
		}
	}
	if missing {
		builder.WriteString(" (missing)")
	}
	return builder.String()
}

// assembleResults 拼接结果行，并在被 limit 截断时追加一条稳定的截断提示。
func assembleResults(lines []string, total int) string {
	if len(lines) == 0 {
		return ""
	}
	content := strings.Join(lines, "\n")
	if total > len(lines) {
		content += fmt.Sprintf("\n[truncated: %d more matches]", total-len(lines))
	}
	return content
}
