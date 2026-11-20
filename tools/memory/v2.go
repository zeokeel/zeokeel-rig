package memory

// Memory v2：把 `memory` 工具从"六动作 wiki"扩成 memoryvfs 之上的上下文数据库
// 工具面，同时保留旧的 index/read/write/append/search/delete 语义（映射到
// `mem://user/self/memories/pages`）。
//
// 旧的 `memory.go`（memory.Wiki 六方法实现与工具）保持原样：宿主可以继续注入
// `kit.Config.Memory`，也可以用 `ToolsWithStore(store)` 换到 v2 后端。两者注册
// 同一个工具名 `memory`，宿主二选一。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/internal/memwiki"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// v2 的固定命名空间。记忆页继续放在旧 wiki 的等价位置，这样默认检索范围与
// `WikiFromStoreV2` 的投影都只有一处定义。
const (
	memoriesRoot = memoryvfs.URI("mem://user/self/memories")
	pagesRoot    = memoryvfs.URI("mem://user/self/memories/pages")
	userRoot     = memoryvfs.URI("mem://user/self")
	rulesRoot    = memoryvfs.URI("mem://user/self/rules")
)

// v2ActionList 是模型可见的动作清单（错误文案与描述共用一份）。
const v2ActionList = "index/read/write/append/search/delete/ls/tree/find/remember/forget/tag/commit/rules/stats/history/restore/mv/mkdir/snapshot"

// v2Actions 是 Schema 里的 action 枚举，顺序与 v2ActionList 一致。
var v2Actions = []string{
	"index", "read", "write", "append", "search", "delete",
	"ls", "tree", "find", "remember", "forget", "tag",
	"commit", "rules", "stats", "history", "restore",
	"mv", "mkdir", "snapshot",
}

// ToolsWithStore 返回绑定到 memoryvfs.Store 的 `memory` 工具（v2 动作集）。
//
// store 为 nil 时工具仍然注册，但每次调用都返回"没有配置存储"的可执行错误——
// 这样装配错误不会在构造期把一个常驻工具变成 panic。
func ToolsWithStore(store memoryvfs.Store, opts ...Option) []rig.Tool {
	cfg := config{maxOutput: DefaultMaxOutputBytes, maxRead: DefaultMaxReadBytes}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return []rig.Tool{&v2Tool{store: store, cfg: cfg}}
}

// v2Tool 是 `memory` 工具的 v2 实现。
type v2Tool struct {
	store memoryvfs.Store
	cfg   config
}

var _ rig.Tool = (*v2Tool)(nil)

// Name 实现 rig.Tool。
func (t *v2Tool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *v2Tool) Description() string {
	return "Long-term memory over the mem:// context store: browse, search, read, write, merge, tag and version memories with L0 summaries and L2 bodies.\n" +
		"Actions: " + v2ActionList + ".\n" +
		"The index only shows one-line summaries; read a body only when the summary says it is relevant."
}

// Schema 实现 rig.Tool。常驻工具的体积是每个回合的成本，所以每个参数只留一句
// 最短描述（细节由运行时错误去教）。
func (t *v2Tool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        v2Actions,
				"description": "Operation to run.",
			},
			"uri": map[string]any{
				"type":        "string",
				"description": "mem:// uri (e.g. mem://user/self/memories/pages/build).",
			},
			"to": map[string]any{
				"type":        "string",
				"description": "Destination uri or slug for action: mv.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Page slug under memories/pages (a-z, 0-9, dashes).",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Human title; defaults to the slug.",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "One-line L0 summary shown in lists.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Markdown body (L2).",
			},
			"text": map[string]any{
				"type":        "string",
				"description": "Text to append, or rule instructions to compile.",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Search query.",
			},
			"kind": map[string]any{
				"type":        "string",
				"description": "memory, resource, skill, rule, session or dir.",
			},
			"type": map[string]any{
				"type":        "string",
				"description": "Memory type: profile, preferences, entities, events, cases, trajectories, experiences.",
			},
			"tags": map[string]any{
				"type":        []string{"object", "string"},
				"description": "k=v,k2=v2 string or JSON object; tag also accepts k+=1 / k-=1.",
			},
			"level": map[string]any{
				"type":        "string",
				"enum":        []string{"L0", "L1", "L2"},
				"description": "Detail level (default L2).",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max results, or tree depth.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "Skip this many results.",
			},
			"token_budget": map[string]any{
				"type":        "integer",
				"description": "Approximate token cap for the output.",
			},
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{"quick", "thinking", "context", "search", "working", "full"},
				"description": "Retrieval mode; 'search' for action rules, 'working'/'full' for action commit.",
			},
			"snapshot": map[string]any{
				"type":        "string",
				"description": "Snapshot id for action restore.",
			},
			"session_id": map[string]any{
				"type":        "string",
				"description": "Session id for action commit (default \"default\").",
			},
		},
		"required":             []string{"action"},
		"additionalProperties": false,
	}
}

// v2Args 是解码后的参数对象。tags 保留原始 JSON：它既可以是一个对象，也可以是
// "k=v,k2=v2" 字符串（模型两种写法都会用）。
type v2Args struct {
	Action      string          `json:"action"`
	URI         string          `json:"uri"`
	To          string          `json:"to"`
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Summary     string          `json:"summary"`
	Content     string          `json:"content"`
	Text        string          `json:"text"`
	Query       string          `json:"query"`
	Kind        string          `json:"kind"`
	Type        string          `json:"type"`
	Tags        json.RawMessage `json:"tags"`
	Level       string          `json:"level"`
	Limit       int             `json:"limit"`
	Offset      int             `json:"offset"`
	TokenBudget int             `json:"token_budget"`
	Mode        string          `json:"mode"`
	Snapshot    string          `json:"snapshot"`
	SessionID   string          `json:"session_id"`
}

// Invoke 实现 rig.Tool。
func (t *v2Tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	var a v2Args
	if err := toolutil.DecodeArgs(call, &a); err != nil {
		return rig.ToolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(a.Action))
	if action == "" {
		return toolutil.ErrorResultf("memory: action is required; use one of %s", v2ActionList), nil
	}
	if t.store == nil {
		return toolutil.ErrorResultf("memory: no memory store is configured; the host must pass one to memory.ToolsWithStore"), nil
	}
	switch action {
	case "index":
		return t.index(ctx), nil
	case "read":
		return t.read(ctx, a), nil
	case "write":
		return t.write(ctx, a), nil
	case "append":
		return t.appendText(ctx, a), nil
	case "search":
		return t.search(ctx, a), nil
	case "delete":
		return t.deleteEntry(ctx, a), nil
	case "ls":
		return t.list(ctx, a), nil
	case "tree":
		return t.tree(ctx, a), nil
	case "find":
		return t.find(ctx, a), nil
	case "remember":
		return t.remember(ctx, a), nil
	case "forget":
		return t.forget(ctx, a), nil
	case "tag":
		return t.tag(ctx, a), nil
	case "commit":
		return t.commit(ctx, a), nil
	case "rules":
		return t.rules(ctx, a), nil
	case "stats":
		return t.stats(ctx), nil
	case "history":
		return t.history(ctx, a), nil
	case "restore":
		return t.restore(ctx, a), nil
	case "mv":
		return t.move(ctx, a), nil
	case "mkdir":
		return t.mkdir(ctx, a), nil
	case "snapshot":
		return t.snapshot(ctx, a), nil
	default:
		return toolutil.ErrorResultf(`memory: unknown action %q; use one of %s`, a.Action, v2ActionList), nil
	}
}

// index 列出整个 memories 子树（v2 原生记忆 + 迁移页），只给 name — summary (age)。
func (t *v2Tool) index(ctx context.Context) rig.ToolResult {
	entries, err := t.store.Tree(ctx, memoriesRoot, 0)
	if err != nil {
		if errors.Is(err, memoryvfs.ErrNotFound) {
			return toolutil.TextResult("no memories yet under " + memoriesRoot.String() +
				`; use action: remember with summary and content to add the first one`)
		}
		return toolutil.ErrorResultf("%s", describeStoreError(err, "list memory entries"))
	}
	pages := filterDirs(entries)
	if len(pages) == 0 {
		return toolutil.TextResult("no memories yet under " + memoriesRoot.String() +
			`; use action: remember with summary and content to add the first one`)
	}
	sort.SliceStable(pages, func(a, b int) bool {
		if !pages[a].Updated.Equal(pages[b].Updated) {
			return pages[a].Updated.After(pages[b].Updated)
		}
		return pages[a].URI < pages[b].URI
	})
	limit := memwiki.MaxIndexResults
	shown := pages
	if len(shown) > limit {
		shown = shown[:limit]
	}
	lines := make([]string, 0, len(shown)+1)
	for _, entry := range shown {
		lines = append(lines, fmt.Sprintf("%s — %s (%s)", entry.URI.Base(), entrySummary(entry), formatAge(entry.Updated)))
	}
	if remaining := len(pages) - len(shown); remaining > 0 {
		lines = append(lines, fmt.Sprintf("… and %d more entries (use action: search to narrow)", remaining))
	}
	return toolutil.TextResult(limitV2Output(strings.Join(lines, "\n"), t.cfg.maxOutput,
		"use action: search to narrow"))
}

// read 实现 read：按 level 取一层内容，正文上限 cfg.maxRead。
func (t *v2Tool) read(ctx context.Context, a v2Args) rig.ToolResult {
	level, err := parseLevel(a.Level)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	uri, err := resolveTarget(a, pagesRoot)
	if err != nil {
		return toolutil.ErrorResultf("%s", targetError(err))
	}
	entry, err := t.store.Read(ctx, uri, level)
	if err != nil && errors.Is(err, memoryvfs.ErrNotFound) && strings.TrimSpace(a.URI) == "" {
		// remember 写入的是 memories/<type>/<slug>，旧 read 默认 pages：按 basename 回退。
		if found, findErr := t.findByName(ctx, strings.TrimSpace(a.Name)); findErr == nil {
			entry, err = t.store.Read(ctx, found.URI, level)
		}
	}
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "read "+uri.String()))
	}
	header := fmt.Sprintf("%s [%s] — %s (%s)", entry.URI, level, entrySummary(entry), formatAge(entry.Updated))
	body := ""
	switch level {
	case memoryvfs.Level0:
		body = ""
	case memoryvfs.Level1:
		body = strings.TrimSpace(entry.Overview)
	default:
		body = strings.TrimSpace(entry.Content)
		if body == "" {
			body = strings.TrimSpace(entry.Overview)
		}
	}
	if body == "" {
		return toolutil.TextResult(header)
	}
	if len(body) > t.cfg.maxRead {
		originalBytes := len(body)
		body, _ = memoryvfs.TruncateBytes(body, t.cfg.maxRead)
		body += fmt.Sprintf("\n[truncated: %s body is %d bytes; use level L0/L1 or a narrower entry]", level, originalBytes)
	}
	return toolutil.TextResult(limitV2Output(header+"\n\n"+body, t.cfg.maxOutput, "use level L0/L1"))
}

// write 实现旧 write 语义：覆盖写 pages 下的一页；summary 必填。
func (t *v2Tool) write(ctx context.Context, a v2Args) rig.ToolResult {
	uri, err := resolveTarget(a, pagesRoot)
	if err != nil {
		return toolutil.ErrorResultf("%s", targetError(err))
	}
	summary := strings.TrimSpace(a.Summary)
	if summary == "" {
		return toolutil.ErrorResultf("memory: summary is required for action: write; the list only shows the summary, so every entry needs a one-line summary")
	}
	kind, memType, err := entryKindType(a, uri)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	ops, err := parseTagOps(a.Tags)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	tags, err := setTagsOnly(ops)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	entry := memoryvfs.Entry{
		URI:     uri,
		Kind:    kind,
		Type:    memType,
		Title:   strings.TrimSpace(a.Title),
		Summary: summary,
		Content: a.Content,
		Tags:    tags,
	}
	written, err := t.store.Write(ctx, entry)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "write "+uri.String()))
	}
	return toolutil.TextResult(fmt.Sprintf("wrote %s (%d bytes, type %s)", written.URI, len(written.Content), typeOrDash(written.Type)))
}

// appendText 实现旧 append 语义：把 text 追加到条目正文；条目不自动创建。
func (t *v2Tool) appendText(ctx context.Context, a v2Args) rig.ToolResult {
	uri, err := resolveTarget(a, pagesRoot)
	if err != nil {
		return toolutil.ErrorResultf("%s", targetError(err))
	}
	text := strings.TrimSpace(a.Text)
	if text == "" {
		return toolutil.ErrorResultf(`memory: text is required for action: append (use action: read to fetch the current body)`)
	}
	entry, err := t.store.Append(ctx, uri, text)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "append to "+uri.String()))
	}
	return toolutil.TextResult(fmt.Sprintf("appended to %s (%d bytes)", entry.URI, len(entry.Content)))
}

// search 实现旧 search 语义：只在 pages 范围里找，只回元数据。
func (t *v2Tool) search(ctx context.Context, a v2Args) rig.ToolResult {
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return toolutil.ErrorResultf("memory: query is required for action: search (use action: index to list all pages)")
	}
	result, err := t.store.Find(ctx, memoryvfs.Query{
		Text:  query,
		Scope: memoriesRoot,
		Kinds: []memoryvfs.Kind{memoryvfs.KindMemory},
		Limit: memwiki.MaxSearchResults,
	})
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "search "+query))
	}
	entries := filterDirs(result.Entries)
	if len(entries) == 0 {
		return toolutil.TextResult(fmt.Sprintf("no pages matched %q; use action: index to list all pages or action: find for a wider search", query))
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("%s — %s (%s)", entry.URI.Base(), entrySummary(entry), formatAge(entry.Updated)))
	}
	return toolutil.TextResult(limitV2Output(strings.Join(lines, "\n"), t.cfg.maxOutput,
		"use action: find with a narrower query"))
}

// deleteEntry 实现旧 delete 语义。
func (t *v2Tool) deleteEntry(ctx context.Context, a v2Args) rig.ToolResult {
	uri, err := resolveTarget(a, pagesRoot)
	if err != nil {
		return toolutil.ErrorResultf("%s", targetError(err))
	}
	if err := t.store.Delete(ctx, uri); err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "delete "+uri.String()))
	}
	return toolutil.TextResult(fmt.Sprintf("deleted %s", uri))
}

// list 实现 ls：列直接子项，一行一条 `uri — summary`。
func (t *v2Tool) list(ctx context.Context, a v2Args) rig.ToolResult {
	uri := userRoot
	if strings.TrimSpace(a.URI) != "" || strings.TrimSpace(a.Name) != "" {
		resolved, err := resolveTarget(a, userRoot)
		if err != nil {
			return toolutil.ErrorResultf("%s", targetError(err))
		}
		uri = resolved
	}
	entries, err := t.store.List(ctx, uri)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "list "+uri.String()))
	}
	entries = sortEntriesForList(entries)
	if len(entries) == 0 {
		return toolutil.TextResult(fmt.Sprintf("empty: %s has no entries; use action: write or action: remember to create one", uri))
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("%s — %s", entry.URI, entrySummary(entry)))
		if a.Limit > 0 && len(lines) >= a.Limit {
			break
		}
	}
	if a.Limit > 0 && len(entries) > len(lines) {
		lines = append(lines, fmt.Sprintf("… and %d more entries (raise limit or use action: find)", len(entries)-len(lines)))
	}
	return toolutil.TextResult(limitV2Output(strings.Join(lines, "\n"), t.cfg.maxOutput,
		"raise limit or use action: find"))
}

// tree 实现 tree：递归列子项，按层级缩进。limit 是最大深度（默认 3）。
func (t *v2Tool) tree(ctx context.Context, a v2Args) rig.ToolResult {
	uri := userRoot
	if strings.TrimSpace(a.URI) != "" || strings.TrimSpace(a.Name) != "" {
		resolved, err := resolveTarget(a, userRoot)
		if err != nil {
			return toolutil.ErrorResultf("%s", targetError(err))
		}
		uri = resolved
	}
	depth := a.Limit
	if depth <= 0 {
		depth = 3
	}
	entries, err := t.store.Tree(ctx, uri, depth)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "tree "+uri.String()))
	}
	if len(entries) == 0 {
		return toolutil.TextResult(fmt.Sprintf("empty: %s has no entries; use action: write or action: remember to create one", uri))
	}
	baseDepth := len(uri.Segments())
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		indent := ""
		if extra := len(entry.URI.Segments()) - baseDepth - 1; extra > 0 {
			indent = strings.Repeat("  ", extra)
		}
		lines = append(lines, fmt.Sprintf("%s%s — %s", indent, entry.URI, entrySummary(entry)))
	}
	return toolutil.TextResult(limitV2Output(strings.Join(lines, "\n"), t.cfg.maxOutput,
		"lower limit (depth) or use action: find"))
}

// find 实现检索：Find/Search 按 mode 选择，输出 uri [Lx] (score) — summary，
// 末尾给 total 与 tokens。
func (t *v2Tool) find(ctx context.Context, a v2Args) rig.ToolResult {
	mode, err := parseMode(a.Mode)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	// level 只在调用方显式给出时作为过滤条件：默认不加层级过滤，否则只写了
	// 摘要（没有 L2 正文）的条目会从 find 结果里消失。
	var levels []memoryvfs.Level
	if strings.TrimSpace(a.Level) != "" {
		level, err := parseLevel(a.Level)
		if err != nil {
			return toolutil.ErrorResultf("%s", err)
		}
		if level != memoryvfs.LevelAny {
			levels = []memoryvfs.Level{level}
		}
	}
	scope := memoriesRoot
	if strings.TrimSpace(a.URI) != "" {
		parsed, err := memoryvfs.ParseURI(strings.TrimSpace(a.URI))
		if err != nil {
			return toolutil.ErrorResultf("%s", describeStoreError(err, "find"))
		}
		scope = parsed
	}
	kinds, err := parseKinds(a.Kind)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	types, err := parseTypes(a.Type)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	ops, err := parseTagOps(a.Tags)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	tags, err := setTagsOnly(ops)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	query := memoryvfs.Query{
		Text:        strings.TrimSpace(a.Query),
		Scope:       scope,
		Kinds:       kinds,
		Types:       types,
		Tags:        tags,
		Mode:        mode,
		Levels:      levels,
		Limit:       a.Limit,
		Offset:      a.Offset,
		TokenBudget: a.TokenBudget,
	}
	var result memoryvfs.Result
	if mode == memoryvfs.ModeThinking || mode == memoryvfs.ModeContext {
		result, err = t.store.Search(ctx, query)
	} else {
		result, err = t.store.Find(ctx, query)
	}
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "find "+query.Text))
	}
	lines := make([]string, 0, len(result.Entries)+1)
	usedTokens := 0
	truncated := false
	for _, entry := range result.Entries {
		matched := entry.MatchedLevel
		if matched < memoryvfs.Level0 || matched > memoryvfs.Level2 {
			matched = memoryvfs.Level2
		}
		line := fmt.Sprintf("%s [%s] (%.2f) — %s", entry.URI, matched, entry.Score, entrySummary(entry))
		cost := memoryvfs.EstimateTokens(line)
		if a.TokenBudget > 0 && usedTokens+cost > a.TokenBudget && len(lines) > 0 {
			truncated = true
			break
		}
		usedTokens += cost
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		if result.Total > 0 {
			return toolutil.TextResult(fmt.Sprintf("found %d matches for %q in %s but token_budget %d is too small for one line; raise token_budget or narrow the query",
				result.Total, query.Text, scope, a.TokenBudget))
		}
		return toolutil.TextResult(fmt.Sprintf("no matches for %q in %s; use action: ls to browse or a shorter query", query.Text, scope))
	}
	if truncated {
		lines = append(lines, fmt.Sprintf("… truncated at token_budget %d (use a larger token_budget or a narrower query)", a.TokenBudget))
	}
	tokens := result.Tokens
	if tokens <= 0 {
		tokens = usedTokens
	}
	lines = append(lines, fmt.Sprintf("total %d · tokens %d", result.Total, tokens))
	return toolutil.TextResult(limitV2Output(strings.Join(lines, "\n"), t.cfg.maxOutput,
		"raise limit or refine the query"))
}

// remember 实现相似度合并写入：同一主题会合并而不是新建。
func (t *v2Tool) remember(ctx context.Context, a v2Args) rig.ToolResult {
	summary := strings.TrimSpace(a.Summary)
	content := a.Content
	if summary == "" {
		summary = deriveSummary(a.Title, content)
	}
	if summary == "" {
		return toolutil.ErrorResultf("memory: summary or content is required for action: remember; pass a one-line summary (and optionally a longer content body)")
	}
	uri, err := t.rememberURI(a, summary)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	kind, memType, err := entryKindType(a, uri)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	ops, err := parseTagOps(a.Tags)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	tags, err := setTagsOnly(ops)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	_, existed := t.statEntry(ctx, uri)
	entry, err := t.store.Remember(ctx, memoryvfs.Entry{
		URI:     uri,
		Kind:    kind,
		Type:    memType,
		Title:   strings.TrimSpace(a.Title),
		Summary: summary,
		Content: content,
		Tags:    tags,
	})
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "remember"))
	}
	verb := "written"
	if existed {
		verb = "merged into"
	}
	return toolutil.TextResult(fmt.Sprintf("%s %s (%s)", verb, entry.URI, typeOrDash(entry.Type)))
}

// forget 实现软删（实现会写 trash 快照）。
func (t *v2Tool) forget(ctx context.Context, a v2Args) rig.ToolResult {
	uri, err := resolveTarget(a, pagesRoot)
	if err != nil {
		return toolutil.ErrorResultf("%s", targetError(err))
	}
	if err := t.store.Forget(ctx, uri); err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "forget "+uri.String()))
	}
	return toolutil.TextResult(fmt.Sprintf("forgot %s (use action: history and action: restore to roll back)", uri))
}

// tag 实现 tags 的 set/append/increment：读现有条目 → 改 Tags → 写回。
func (t *v2Tool) tag(ctx context.Context, a v2Args) rig.ToolResult {
	uri, err := resolveTarget(a, pagesRoot)
	if err != nil {
		return toolutil.ErrorResultf("%s", targetError(err))
	}
	ops, err := parseTagOps(a.Tags)
	if err != nil {
		return toolutil.ErrorResultf("%s", err)
	}
	if len(ops) == 0 {
		return toolutil.ErrorResultf(`memory: tags is required for action: tag; pass "k=v", "k+=1" or "k-=1" (comma separated, or a JSON object)`)
	}
	entry, err := t.store.Read(ctx, uri, memoryvfs.Level2)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "tag "+uri.String()))
	}
	tags := make(map[string]string, len(entry.Tags)+len(ops))
	for key, value := range entry.Tags {
		tags[key] = value
	}
	for _, op := range ops {
		switch op.op {
		case '+', '-':
			current := 0
			if raw, ok := tags[op.key]; ok {
				parsed, convErr := strconv.Atoi(strings.TrimSpace(raw))
				if convErr != nil {
					return toolutil.ErrorResultf("memory: tag %q is %q, which is not a number; set it with %s=<value> instead of %s%c1", op.key, raw, op.key, op.key, op.op)
				}
				current = parsed
			}
			delta := op.delta
			if delta == 0 {
				delta = 1
			}
			if op.op == '+' {
				current += delta
			} else {
				current -= delta
			}
			tags[op.key] = strconv.Itoa(current)
		default:
			tags[op.key] = op.value
		}
	}
	entry.Tags = tags
	written, err := t.store.Write(ctx, entry)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "tag "+uri.String()))
	}
	return toolutil.TextResult(fmt.Sprintf("tagged %s: %s", written.URI, formatTags(written.Tags)))
}

// commit 实现会话提交：归档 pending 消息并抽取长期记忆。
func (t *v2Tool) commit(ctx context.Context, a v2Args) rig.ToolResult {
	sessionID := strings.TrimSpace(a.SessionID)
	if sessionID == "" {
		sessionID = "default"
	}
	result, err := t.store.Commit(ctx, sessionID, memoryvfs.CommitOptions{
		Extract:       true,
		WorkingMemory: wantsWorkingMemory(a.Mode),
		TokenBudget:   a.TokenBudget,
	})
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "commit "+sessionID))
	}
	archive := "none"
	if !result.Archive.IsZero() {
		archive = result.Archive.String()
	}
	return toolutil.TextResult(fmt.Sprintf("committed session %q: archive %s, wrote %d, merged %d, skipped %d",
		sessionID, archive, len(result.Written), len(result.Merged), result.Skipped))
}

// rules 实现规则编译（text）与规则检索（query + mode: search）。
func (t *v2Tool) rules(ctx context.Context, a v2Args) rig.ToolResult {
	instruction := strings.TrimSpace(a.Text)
	if instruction != "" {
		entry, err := t.store.CompileRules(ctx, instruction, nil)
		if err != nil {
			return toolutil.ErrorResultf("%s", describeStoreError(err, "compile rules"))
		}
		return toolutil.TextResult(fmt.Sprintf("compiled rule %s — %s", entry.URI, entrySummary(entry)))
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return toolutil.ErrorResultf(`memory: rules needs text (to compile) or query with mode: "search" (to search); example: {"action":"rules","query":"release","mode":"search"}`)
	}
	if !strings.EqualFold(strings.TrimSpace(a.Mode), "search") {
		return toolutil.ErrorResultf(`memory: searching rules needs mode: "search"; use {"action":"rules","query":%q,"mode":"search"}`, query)
	}
	rules, err := t.store.SearchRules(ctx, query, 0.5)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "search rules"))
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Probability != rules[j].Probability {
			return rules[i].Probability > rules[j].Probability
		}
		return rules[i].URI < rules[j].URI
	})
	limit := a.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(rules) == 0 {
		return toolutil.TextResult(fmt.Sprintf("no rules matched %q at probability >= 0.50; compile one with action: rules and text", query))
	}
	lines := make([]string, 0, len(rules)+1)
	for _, rule := range rules {
		lines = append(lines, fmt.Sprintf("%s (%.2f) — %s", rule.URI, rule.Probability, memoryvfs.CollapseWhitespace(rule.Content)))
		if len(lines) >= limit {
			break
		}
	}
	if len(rules) > len(lines) {
		lines = append(lines, fmt.Sprintf("… and %d more rules (raise limit)", len(rules)-len(lines)))
	}
	return toolutil.TextResult(limitV2Output(strings.Join(lines, "\n"), t.cfg.maxOutput, "raise limit"))
}

// stats 实现本地统计。
func (t *v2Tool) stats(ctx context.Context) rig.ToolResult {
	stats, err := t.store.Stats(ctx)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "stats"))
	}
	lines := []string{fmt.Sprintf("entries %d · dirs %d · tokens %d · bytes %d",
		stats.Entries, stats.Dirs, stats.Tokens, stats.Bytes)}
	if line := formatCounts("by kind", kindCounts(stats.ByKind)); line != "" {
		lines = append(lines, line)
	}
	if line := formatCounts("by type", typeCounts(stats.ByType)); line != "" {
		lines = append(lines, line)
	}
	lines = append(lines, fmt.Sprintf("sessions %d · snapshots %d · vectors %d",
		stats.Sessions, stats.Snapshots, stats.Vectors))
	if !stats.Oldest.IsZero() || !stats.Newest.IsZero() {
		lines = append(lines, fmt.Sprintf("oldest %s · newest %s", formatAge(stats.Oldest), formatAge(stats.Newest)))
	}
	return toolutil.TextResult(strings.Join(lines, "\n"))
}

// history 实现快照列表。
func (t *v2Tool) history(ctx context.Context, a v2Args) rig.ToolResult {
	snapshots, err := t.store.Snapshots(ctx)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "list snapshots"))
	}
	if len(snapshots) == 0 {
		return toolutil.TextResult("no snapshots yet; snapshots are created by the store when entries change")
	}
	sort.SliceStable(snapshots, func(i, j int) bool {
		if !snapshots[i].Created.Equal(snapshots[j].Created) {
			return snapshots[i].Created.After(snapshots[j].Created)
		}
		return snapshots[i].ID < snapshots[j].ID
	})
	limit := a.Limit
	if limit <= 0 {
		limit = 20
	}
	lines := make([]string, 0, len(snapshots)+1)
	for _, snapshot := range snapshots {
		label := snapshot.Label
		if label == "" {
			label = "snapshot"
		}
		lines = append(lines, fmt.Sprintf("%s — %s (%d entries, %d bytes, %s)",
			snapshot.ID, label, snapshot.Entries, snapshot.Bytes, formatAge(snapshot.Created)))
		if len(lines) >= limit {
			break
		}
	}
	if len(snapshots) > len(lines) {
		lines = append(lines, fmt.Sprintf("… and %d more snapshots (raise limit)", len(snapshots)-len(lines)))
	}
	return toolutil.TextResult(limitV2Output(strings.Join(lines, "\n"), t.cfg.maxOutput, "raise limit"))
}

// restore 实现快照恢复。
func (t *v2Tool) restore(ctx context.Context, a v2Args) rig.ToolResult {
	snapshot := strings.TrimSpace(a.Snapshot)
	if snapshot == "" {
		return toolutil.ErrorResultf(`memory: snapshot is required for action: restore; use action: history to list snapshot ids`)
	}
	if err := t.store.Restore(ctx, snapshot); err != nil {
		if errors.Is(err, memoryvfs.ErrNotFound) {
			return toolutil.ErrorResultf("memory: no snapshot %q; use action: history to list snapshot ids", snapshot)
		}
		return toolutil.ErrorResultf("%s", describeStoreError(err, "restore "+snapshot))
	}
	return toolutil.TextResult(fmt.Sprintf("restored snapshot %s", snapshot))
}

// move 实现动作 mv：uri/name 是源，to 是目标（绝对 mem:// 或同目录下的 slug）。
func (t *v2Tool) move(ctx context.Context, a v2Args) rig.ToolResult {
	from, err := resolveTarget(a, pagesRoot)
	if err != nil {
		return toolutil.ErrorResultf("%s", targetError(err))
	}
	rawTo := strings.TrimSpace(a.To)
	if rawTo == "" {
		return toolutil.ErrorResultf(`memory: to is required for action: mv (a mem:// uri or a new slug)`)
	}
	var to memoryvfs.URI
	if strings.HasPrefix(rawTo, memoryvfs.Scheme) {
		to, err = memoryvfs.ParseURI(rawTo)
	} else {
		to, err = from.Parent().Child(rawTo)
	}
	if err != nil {
		return toolutil.ErrorResultf("memory: invalid destination %q: %v", rawTo, err)
	}
	if err := t.store.Move(ctx, from, to); err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "move "+from.String()+" to "+to.String()))
	}
	return toolutil.TextResult(fmt.Sprintf("moved %s to %s", from, to))
}

// mkdir 实现动作 mkdir：在 memories 下建一个带 L0/L1 描述的目录。
func (t *v2Tool) mkdir(ctx context.Context, a v2Args) rig.ToolResult {
	target, err := resolveTarget(a, memoriesRoot)
	if err != nil {
		return toolutil.ErrorResultf("%s", targetError(err))
	}
	description := strings.TrimSpace(a.Summary)
	if description == "" {
		description = strings.TrimSpace(a.Text)
	}
	if description == "" {
		description = target.Base()
	}
	entry, err := t.store.Mkdir(ctx, target, description)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "mkdir "+target.String()))
	}
	return toolutil.TextResult(fmt.Sprintf("created directory %s", entry.URI))
}

// snapshot 实现动作 snapshot：显式创建一个可恢复的版本快照。
func (t *v2Tool) snapshot(ctx context.Context, a v2Args) rig.ToolResult {
	label := strings.TrimSpace(a.Title)
	if label == "" {
		label = strings.TrimSpace(a.Text)
	}
	snapshot, err := t.store.Snapshot(ctx, label)
	if err != nil {
		return toolutil.ErrorResultf("%s", describeStoreError(err, "snapshot"))
	}
	return toolutil.TextResult(fmt.Sprintf("snapshot %s (%d entries) — use action: history / restore", snapshot.ID, snapshot.Entries))
}

// statEntry 返回条目是否存在（用于区分 written 与 merged 文案）。
func (t *v2Tool) statEntry(ctx context.Context, uri memoryvfs.URI) (memoryvfs.Entry, bool) {
	entry, err := t.store.Stat(ctx, uri)
	if err != nil {
		return memoryvfs.Entry{}, false
	}
	return entry, true
}

// rememberURI 解析 remember 的目标：显式 uri > name > 由 type/title/summary 生成
// 的稳定 slug。稳定 slug 让"同一句话重复记住"落到同一个 URI，从而走合并。
func (t *v2Tool) rememberURI(a v2Args, summary string) (memoryvfs.URI, error) {
	if raw := strings.TrimSpace(a.URI); raw != "" {
		uri, err := memoryvfs.ParseURI(raw)
		if err != nil {
			return "", errors.New(describeStoreError(err, "remember"))
		}
		return uri, nil
	}
	kind := memoryvfs.KindMemory
	if raw := strings.TrimSpace(a.Kind); raw != "" {
		kind = memoryvfs.Kind(strings.ToLower(raw))
		if !kind.Valid() {
			return "", fmt.Errorf("memory: invalid kind %q; use one of memory/resource/skill/rule/session/dir", a.Kind)
		}
	}
	root := defaultRootFor(kind)
	memType := strings.ToLower(strings.TrimSpace(a.Type))
	if kind == memoryvfs.KindMemory {
		if memType == "" {
			memType = memoryvfs.TypeEvents
		}
		child, err := memoriesRoot.Child(memType)
		if err != nil {
			return "", fmt.Errorf("memory: invalid type %q; use a lowercase slug such as preferences", a.Type)
		}
		root = child
	}
	name := strings.TrimSpace(a.Name)
	if name != "" {
		if err := memwiki.ValidateName(name); err != nil {
			return "", fmt.Errorf(`memory: name must be 1-64 chars of a-z, 0-9 and "-" (for example "build-flaky-tests"); it is never rewritten silently`)
		}
	} else {
		name = deriveSlug(summary)
	}
	return root.Child(name)
}

func defaultRootFor(kind memoryvfs.Kind) memoryvfs.URI {
	switch kind {
	case memoryvfs.KindResource:
		return memoryvfs.URI("mem://resources")
	case memoryvfs.KindSkill:
		return memoryvfs.URI("mem://skills")
	case memoryvfs.KindRule:
		return rulesRoot
	case memoryvfs.KindSession:
		return memoryvfs.URI("mem://sessions")
	default:
		return memoriesRoot
	}
}

// deriveSlug 从摘要派生稳定 slug；非拉丁文本没有可用的 slug 时退回内容哈希，
// 这样同一段中文摘要每次都落到同一个 URI。
func deriveSlug(seed string) string {
	if slug := slugify(seed); slug != "" {
		if len(slug) > 48 {
			slug = strings.Trim(slug[:48], "-")
		}
		if slug != "" {
			return slug
		}
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(seed))
	return fmt.Sprintf("mem-%08x", hash.Sum32())
}

// deriveSummary 从正文首行派生一句话摘要（≤200 字节）。
func deriveSummary(title, content string) string {
	summary := firstLine(content)
	if summary == "" {
		summary = strings.TrimSpace(title)
	}
	summary, _ = memoryvfs.TruncateBytes(memoryvfs.CollapseWhitespace(summary), 200)
	return summary
}

// entryKindType 决定写入条目用的 Kind/Type：显式参数 > 路径推断 > 默认值。
func entryKindType(a v2Args, uri memoryvfs.URI) (memoryvfs.Kind, string, error) {
	kind := inferKind(uri)
	if raw := strings.TrimSpace(a.Kind); raw != "" {
		kind = memoryvfs.Kind(strings.ToLower(raw))
		if !kind.Valid() {
			return "", "", fmt.Errorf("memory: invalid kind %q; use one of memory/resource/skill/rule/session/dir", a.Kind)
		}
	}
	if kind != memoryvfs.KindMemory {
		return kind, strings.ToLower(strings.TrimSpace(a.Type)), nil
	}
	memType := strings.ToLower(strings.TrimSpace(a.Type))
	if memType != "" {
		if !isSlug(memType) {
			return "", "", fmt.Errorf("memory: invalid type %q; use a lowercase slug such as preferences", a.Type)
		}
		return kind, memType, nil
	}
	if inferred := inferType(uri); inferred != "" {
		return kind, inferred, nil
	}
	if uri.IsUnder(pagesRoot) {
		return kind, memoryvfs.TypePages, nil
	}
	return kind, memoryvfs.TypeEvents, nil
}

// inferKind 从 URI 的第一段推断类别。
func inferKind(uri memoryvfs.URI) memoryvfs.Kind {
	segments := uri.Segments()
	if len(segments) == 0 {
		return memoryvfs.KindMemory
	}
	switch segments[0] {
	case "resources":
		return memoryvfs.KindResource
	case "skills":
		return memoryvfs.KindSkill
	case "sessions":
		return memoryvfs.KindSession
	}
	if uri.IsUnder(rulesRoot) {
		return memoryvfs.KindRule
	}
	return memoryvfs.KindMemory
}

// inferType 从 `mem://user/self/memories/<type>/...` 的路径段推断记忆类型。
func inferType(uri memoryvfs.URI) string {
	segments := uri.Segments()
	for _, segment := range segments {
		for _, known := range memoryvfs.MemoryTypes() {
			if segment == known {
				return known
			}
		}
		if segment == memoryvfs.TypePages {
			return memoryvfs.TypePages
		}
	}
	return ""
}

func isSlug(value string) bool {
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

// tagOp 是一条 tags 修改：op '=' 赋值，'+'/'-' 对整数做增减。
type tagOp struct {
	key   string
	op    byte
	value string
	delta int
}

// parseTagOps 解析两种写法：
//   - 字符串："k=v,k2=v2,k3+=1,k4-=2"（逗号分隔；值里不能有逗号）；
//   - 对象：{"k":"v","count":"+=1"} 或 {"count+":"1"}；数字/布尔值按字面量转字符串。
//
// 单靠 "k" 没有值的写法会报错（与其猜，不如告诉模型正确写法）。
func parseTagOps(raw json.RawMessage) ([]tagOp, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" || text == `""` {
		return nil, nil
	}
	if strings.HasPrefix(text, "{") {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, fmt.Errorf(`memory: tags must be a JSON object or a "k=v,k2=v2" string; got %s`, text)
		}
		ops := make([]tagOp, 0, len(object))
		for key, value := range object {
			parsed, err := parseTagValue(key, value)
			if err != nil {
				return nil, err
			}
			ops = append(ops, parsed...)
		}
		sort.SliceStable(ops, func(i, j int) bool { return ops[i].key < ops[j].key })
		return ops, nil
	}
	var literal string
	if err := json.Unmarshal(raw, &literal); err != nil {
		return nil, fmt.Errorf(`memory: tags must be a JSON object or a "k=v,k2=v2" string; got %s`, text)
	}
	return parseTagString(literal)
}

// parseTagValue 解析对象形式的一个键值：键尾的 '+'/'-' 或值前缀的 "+="/"-="
// 表示增减，其余是赋值。
func parseTagValue(key string, value json.RawMessage) ([]tagOp, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("memory: tag keys must not be empty; use \"k=v\"")
	}
	op := byte('=')
	if strings.HasSuffix(key, "+") || strings.HasSuffix(key, "-") {
		op = key[len(key)-1]
		key = strings.TrimSpace(key[:len(key)-1])
		if key == "" {
			return nil, fmt.Errorf("memory: tag key before %q is empty; use \"name%c=1\"", string(op), op)
		}
	}
	text := strings.TrimSpace(string(value))
	var literal string
	if err := json.Unmarshal(value, &literal); err == nil {
		text = strings.TrimSpace(literal)
	} else {
		text = strings.Trim(text, `"`)
	}
	delta := 0
	if op == '=' && (strings.HasPrefix(text, "+=") || strings.HasPrefix(text, "-=")) {
		op = text[0]
		text = strings.TrimSpace(text[2:])
	}
	if op == '+' || op == '-' {
		parsed, err := strconv.Atoi(text)
		if err != nil {
			return nil, fmt.Errorf("memory: tag %q increment must be an integer, got %q (example: \"%s%c1\")", key, text, key, op)
		}
		delta = parsed
	}
	return []tagOp{{key: key, op: op, value: text, delta: delta}}, nil
}

// parseTagString 解析 "k=v,k2=v2,k3+=1" 字符串形式。
func parseTagString(text string) ([]tagOp, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	tokens := strings.Split(text, ",")
	ops := make([]tagOp, 0, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		index := strings.Index(token, "=")
		if index <= 0 {
			return nil, fmt.Errorf(`memory: tag %q needs a value; use "k=v", "k+=1" or "k-=1" (comma separated)`, token)
		}
		key := strings.TrimSpace(token[:index])
		value := strings.TrimSpace(token[index+1:])
		op := byte('=')
		delta := 0
		if strings.HasSuffix(key, "+") || strings.HasSuffix(key, "-") {
			op = key[len(key)-1]
			key = strings.TrimSpace(key[:len(key)-1])
		}
		if key == "" {
			return nil, fmt.Errorf(`memory: tag %q has an empty key; use "name=v"`, token)
		}
		if op == '+' || op == '-' {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("memory: tag %q increment must be an integer, got %q (example: \"%s%c1\")", key, value, key, op)
			}
			delta = parsed
		}
		ops = append(ops, tagOp{key: key, op: op, value: value, delta: delta})
	}
	if len(ops) == 0 {
		return nil, nil
	}
	return ops, nil
}

// setTagsOnly 把 tags 操作转成赋值 map；增减只允许出现在 action: tag 里。
func setTagsOnly(ops []tagOp) (map[string]string, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	tags := make(map[string]string, len(ops))
	for _, op := range ops {
		if op.op != '=' {
			return nil, fmt.Errorf("memory: tag increments (%s%c...) are only supported by action: tag, not by write/remember", op.key, op.op)
		}
		tags[op.key] = op.value
	}
	return tags, nil
}

// formatTags 渲染 tags：按 key 排序的 "k=v, k2=v2"。
func formatTags(tags map[string]string) string {
	if len(tags) == 0 {
		return "(no tags)"
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+tags[key])
	}
	return strings.Join(parts, ", ")
}

// parseLevel 把 "L0/L1/L2"（也接受 0/1/2 与 full/summary/overview）转成 Level。
func parseLevel(raw string) (memoryvfs.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "l2", "2", "full", "body":
		return memoryvfs.Level2, nil
	case "l0", "0", "summary":
		return memoryvfs.Level0, nil
	case "l1", "1", "overview":
		return memoryvfs.Level1, nil
	case "any", "all":
		return memoryvfs.LevelAny, nil
	default:
		return 0, fmt.Errorf(`memory: unknown level %q; use "L0" (summary), "L1" (overview) or "L2" (body)`, raw)
	}
}

// parseMode 校验检索模式。
func parseMode(raw string) (memoryvfs.Mode, error) {
	mode := memoryvfs.Mode(strings.ToLower(strings.TrimSpace(raw)))
	if mode == "search" {
		return "", fmt.Errorf(`memory: mode "search" is only for action: rules; use quick, thinking or context for action: find`)
	}
	// schema 允许 working/full（给 action: commit）；find/search 把它们当 quick。
	if mode == "working" || mode == "full" {
		mode = memoryvfs.ModeQuick
	}
	if !mode.Valid() {
		return "", fmt.Errorf(`memory: unknown mode %q; use "quick", "thinking" or "context"`, raw)
	}
	if mode == "" {
		mode = memoryvfs.ModeQuick
	}
	return mode, nil
}

// parseKinds 解析逗号分隔的类别列表。
func parseKinds(raw string) ([]memoryvfs.Kind, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	kinds := make([]memoryvfs.Kind, 0, len(parts))
	for _, part := range parts {
		kind := memoryvfs.Kind(strings.ToLower(strings.TrimSpace(part)))
		if kind == "" {
			continue
		}
		if !kind.Valid() {
			return nil, fmt.Errorf("memory: invalid kind %q; use one of memory/resource/skill/rule/session/dir", part)
		}
		kinds = append(kinds, kind)
	}
	return kinds, nil
}

// parseTypes 解析逗号分隔的记忆类型列表。
func parseTypes(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	types := make([]string, 0, len(parts))
	for _, part := range parts {
		memType := strings.ToLower(strings.TrimSpace(part))
		if memType == "" {
			continue
		}
		if !isSlug(memType) {
			return nil, fmt.Errorf("memory: invalid type %q; use a lowercase slug such as preferences", part)
		}
		types = append(types, memType)
	}
	return types, nil
}

// wantsWorkingMemory 把 mode 映射到 CommitOptions.WorkingMemory。
func wantsWorkingMemory(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "working", "working-memory", "working_memory", "full":
		return true
	}
	return false
}

// findByName 在 memories 子树里按 basename 精确找一条记忆。
func (t *v2Tool) findByName(ctx context.Context, name string) (memoryvfs.Entry, error) {
	if name == "" {
		return memoryvfs.Entry{}, errMissingTarget
	}
	entries, err := t.store.Tree(ctx, memoriesRoot, 0)
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	for _, entry := range entries {
		if entry.Kind == memoryvfs.KindMemory && entry.URI.Base() == name {
			return entry, nil
		}
	}
	return memoryvfs.Entry{}, fmt.Errorf("%w: memory entry %q not found", memoryvfs.ErrNotFound, name)
}

// resolveTarget 解析 uri/name；两者都缺时返回 errMissingTarget。
func resolveTarget(a v2Args, fallback memoryvfs.URI) (memoryvfs.URI, error) {
	if raw := strings.TrimSpace(a.URI); raw != "" {
		return memoryvfs.ParseURI(raw)
	}
	name := strings.TrimSpace(a.Name)
	if name != "" {
		if err := memwiki.ValidateName(name); err != nil {
			return "", err
		}
		return fallback.Child(name)
	}
	return "", errMissingTarget
}

var errMissingTarget = errors.New("memory: target is missing")

// targetError 把解析目标的错误翻成可执行文案。
func targetError(err error) string {
	switch {
	case errors.Is(err, errMissingTarget):
		return `memory: uri or name is required; pass "uri": "mem://user/self/memories/pages/<name>" or "name": "<slug>" (use action: ls to see entries)`
	case errors.Is(err, memwiki.ErrInvalidName):
		return `memory: name must be 1-64 chars of a-z, 0-9 and "-" (for example "build-flaky-tests"); it is never rewritten silently`
	case errors.Is(err, memoryvfs.ErrInvalidURI):
		return `memory: invalid uri; memory uris look like "mem://user/self/memories/pages/<name>" (lowercase segments, no spaces)`
	default:
		return describeStoreError(err, "")
	}
}

// describeStoreError 把存储层错误翻译成模型可执行的英文文案。已知哨兵错误
// 不裸抛内部文本；未知错误保留原因但补上"下一步做什么"。
func describeStoreError(err error, hint string) string {
	switch {
	case errors.Is(err, memoryvfs.ErrNotFound):
		return "memory: entry not found; use action: ls or action: find to see what exists"
	case errors.Is(err, memoryvfs.ErrInvalidURI):
		return `memory: invalid uri; memory uris look like "mem://user/self/memories/pages/<name>" (lowercase segments, no spaces)`
	case errors.Is(err, memoryvfs.ErrInvalidEntry):
		return "memory: entry rejected; memory entries need a type (profile/preferences/entities/events/cases/trajectories/experiences) and a one-line summary"
	case errors.Is(err, memoryvfs.ErrSummaryRequired):
		return "memory: summary is required; the list only shows the summary, so pass a one-line summary"
	case errors.Is(err, memoryvfs.ErrExists):
		return "memory: an entry with that uri already exists; use action: write to replace it or action: append to extend it"
	case errors.Is(err, memoryvfs.ErrConflict):
		return "memory: concurrent update conflict; read the entry again and retry"
	case errors.Is(err, memoryvfs.ErrQuotaExceeded):
		return "memory: memory quota exceeded; delete entries with action: forget or raise the store limits"
	case errors.Is(err, memoryvfs.ErrUnsupported):
		return "memory: this backend does not support that operation; use action: stats to see what is available"
	case errors.Is(err, memoryvfs.ErrClosed):
		return "memory: the store is closed; the host must open a new one"
	case errors.Is(err, memoryvfs.ErrCorrupt):
		return "memory: stored data is corrupt; the host should rebuild the index"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "memory: operation canceled; retry once the turn is running again"
	default:
		detail := ""
		if hint != "" {
			detail = " (" + hint + ")"
		}
		return fmt.Sprintf("memory: store error%s: %v; retry the action, and if it persists the host should check the memory directory", detail, err)
	}
}

// entrySummary 返回一行摘要；目录没有摘要时显式标注。
func entrySummary(entry memoryvfs.Entry) string {
	if summary := strings.TrimSpace(entry.Summary); summary != "" {
		return memoryvfs.CollapseWhitespace(summary)
	}
	if entry.Kind == memoryvfs.KindDir {
		return "(directory)"
	}
	return "(no summary)"
}

// formatAge 渲染"多久以前"（英文，避免把中文混进 v2 的工具输出）。
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "unknown age"
	}
	delta := time.Since(t.UTC())
	switch {
	case delta < time.Minute:
		return "just now"
	case delta < time.Hour:
		return fmt.Sprintf("%dm ago", int(delta.Minutes()))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(delta.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(delta.Hours()/24))
	}
}

// sortEntriesForList 按更新时间降序、URI 升序（与 wiki 目录语义一致）。
func sortEntriesForList(entries []memoryvfs.Entry) []memoryvfs.Entry {
	out := append([]memoryvfs.Entry(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].URI < out[j].URI
	})
	return out
}

// filterDirs 去掉目录项（检索结果里目录没有可读正文）。
func filterDirs(entries []memoryvfs.Entry) []memoryvfs.Entry {
	out := make([]memoryvfs.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Kind == memoryvfs.KindDir {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func typeOrDash(memType string) string {
	if strings.TrimSpace(memType) == "" {
		return "-"
	}
	return memType
}

// kindCounts / typeCounts 把 map 变成稳定顺序的 "k=v" 列表。
func kindCounts(counts map[memoryvfs.Kind]int) map[string]int {
	if len(counts) == 0 {
		return nil
	}
	out := make(map[string]int, len(counts))
	for kind, count := range counts {
		out[string(kind)] = count
	}
	return out
}

func typeCounts(counts map[string]int) map[string]int { return counts }

// formatCounts 渲染 "by kind: memory=10, rule=2"（key 排序）。
func formatCounts(label string, counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return label + ": " + strings.Join(parts, ", ")
}

// limitV2Output 把输出限制在字节上限内，按整行截断并附上"下一步"提示。
func limitV2Output(text string, maxBytes int, hint string) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	lines := strings.Split(text, "\n")
	var builder strings.Builder
	for _, line := range lines {
		if builder.Len()+len(line)+1 > maxBytes {
			break
		}
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	truncated := strings.TrimSuffix(builder.String(), "\n")
	if truncated == "" {
		truncated, _ = memoryvfs.TruncateBytes(lines[0], maxBytes)
	}
	return truncated + fmt.Sprintf("\n[truncated: output exceeded %d bytes; %s]", maxBytes, hint)
}

// WikiFromStoreV2 把 v2 的 `mem://user/self/memories/pages/<name>` 投影成旧
// `Wiki` 六方法接口，供 `kit.Agent.MemoryWiki()` 等旧宿主兼容。
//
// 语义对齐 `internal/memwiki`：页名是 [a-z0-9-]{1,64} 的 slug（不静默改写），
// 摘要必填（≤200 字节），覆盖写会在 `## Log` 段留一行 "(was N bytes)"，append
// 追加 `- YYYY-MM-DD <text>`。错误映射：存储层的 not-found 变成
// `memwiki.ErrNotFound`，其余原样返回。
func WikiFromStoreV2(store memoryvfs.Store) Wiki {
	return &storeWikiV2{store: store}
}

// storeWikiV2 是 memoryvfs.Store → Wiki 的适配器。
type storeWikiV2 struct{ store memoryvfs.Store }

var _ Wiki = (*storeWikiV2)(nil)

// Index 实现 Wiki：投影整个 memories 子树（含 v2 原生记忆与迁移页），按
// "最近修改降序、同时间 name 升序"。空库返回空切片（对齐旧 memwiki）。
func (w *storeWikiV2) Index(ctx context.Context) ([]PageMeta, error) {
	if w == nil || w.store == nil {
		return nil, errNoV2Store
	}
	entries, err := w.store.Tree(ctx, memoriesRoot, 0)
	if err != nil {
		if errors.Is(err, memoryvfs.ErrNotFound) {
			return nil, nil
		}
		return nil, mapWikiError(err)
	}
	metas := make([]PageMeta, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range filterDirs(entries) {
		meta := metaFromV2Entry(entry)
		if _, dup := seen[meta.Name]; dup {
			continue
		}
		seen[meta.Name] = struct{}{}
		metas = append(metas, meta)
	}
	memwiki.SortMetas(metas)
	return metas, nil
}

// Read 实现 Wiki。
func (w *storeWikiV2) Read(ctx context.Context, name string) (Page, error) {
	if w == nil || w.store == nil {
		return Page{}, errNoV2Store
	}
	if err := memwiki.ValidateName(name); err != nil {
		return Page{}, err
	}
	uri, err := pagesRoot.Child(name)
	if err != nil {
		return Page{}, err
	}
	entry, err := w.store.Read(ctx, uri, memoryvfs.Level2)
	if err != nil {
		if !errors.Is(err, memoryvfs.ErrNotFound) {
			return Page{}, mapWikiError(err)
		}
		// v2 原生记忆（capture/remember 写入）不在 pages 下：按 basename 回退查找。
		found, findErr := w.findByName(ctx, name)
		if findErr != nil {
			return Page{}, mapWikiError(findErr)
		}
		entry = found
	}
	if entry.Kind == memoryvfs.KindDir {
		return Page{}, fmt.Errorf("%w: %q is a directory", memwiki.ErrNotFound, name)
	}
	return pageFromV2Entry(name, entry), nil
}

// findByName 在 memories 子树里按 basename 精确找一条记忆（v2 原生记忆的 URI
// 形如 memories/<type>/<slug>，旧 Wiki 只给了 slug）。
func (w *storeWikiV2) findByName(ctx context.Context, name string) (memoryvfs.Entry, error) {
	entries, err := w.store.Tree(ctx, memoriesRoot, 0)
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	for _, entry := range entries {
		if entry.Kind == memoryvfs.KindMemory && entry.URI.Base() == name {
			return w.store.Read(ctx, entry.URI, memoryvfs.Level2)
		}
	}
	return memoryvfs.Entry{}, fmt.Errorf("%w: %q", memwiki.ErrNotFound, name)
}

// Write 实现 Wiki：覆盖写，已有页时在 `## Log` 段追加 "(was N bytes)"。
func (w *storeWikiV2) Write(ctx context.Context, page Page) error {
	if w == nil || w.store == nil {
		return errNoV2Store
	}
	if err := memwiki.ValidateName(page.Name); err != nil {
		return err
	}
	summary, err := memwiki.NormalizeSummary(page.Summary)
	if err != nil {
		return err
	}
	uri, err := pagesRoot.Child(page.Name)
	if err != nil {
		return err
	}
	content := page.Content
	if existing, readErr := w.store.Read(ctx, uri, memoryvfs.Level2); readErr == nil {
		content = memwiki.RewriteLog(page.Content, len(existing.Content))
	}
	_, err = w.store.Write(ctx, memoryvfs.Entry{
		URI:     uri,
		Kind:    memoryvfs.KindMemory,
		Type:    memoryvfs.TypePages,
		Title:   memwiki.NormalizeTitle(page.Name, page.Title),
		Summary: summary,
		Content: content,
	})
	return mapWikiError(err)
}

// Append 实现 Wiki：把带日期的条目追加到 `## Log` 段。
func (w *storeWikiV2) Append(ctx context.Context, name, text string) error {
	if w == nil || w.store == nil {
		return errNoV2Store
	}
	if err := memwiki.ValidateName(name); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("memory: append text is empty; pass the line to add")
	}
	uri, err := pagesRoot.Child(name)
	if err != nil {
		return err
	}
	existing, err := w.store.Read(ctx, uri, memoryvfs.Level2)
	if err != nil {
		return mapWikiError(err)
	}
	entry := existing.Clone()
	entry.Content = memwiki.AppendLog(existing.Content, text)
	if _, err := w.store.Write(ctx, entry); err != nil {
		return mapWikiError(err)
	}
	return nil
}

// Delete 实现 Wiki。
func (w *storeWikiV2) Delete(ctx context.Context, name string) error {
	if w == nil || w.store == nil {
		return errNoV2Store
	}
	if err := memwiki.ValidateName(name); err != nil {
		return err
	}
	uri, err := pagesRoot.Child(name)
	if err != nil {
		return err
	}
	if err := w.store.Delete(ctx, uri); err != nil {
		if !errors.Is(err, memoryvfs.ErrNotFound) {
			return mapWikiError(err)
		}
		found, findErr := w.findByName(ctx, name)
		if findErr != nil {
			return mapWikiError(findErr)
		}
		return mapWikiError(w.store.Delete(ctx, found.URI))
	}
	return nil
}

// Search 实现 Wiki：空查询退化为 Index；只返回元数据，最多 MaxSearchResults 条。
func (w *storeWikiV2) Search(ctx context.Context, query string) ([]PageMeta, error) {
	if w == nil || w.store == nil {
		return nil, errNoV2Store
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return w.Index(ctx)
	}
	result, err := w.store.Find(ctx, memoryvfs.Query{
		Text:  query,
		Scope: memoriesRoot,
		Kinds: []memoryvfs.Kind{memoryvfs.KindMemory},
		Limit: memwiki.MaxSearchResults,
	})
	if err != nil {
		return nil, mapWikiError(err)
	}
	metas := make([]PageMeta, 0, len(result.Entries))
	for _, entry := range filterDirs(result.Entries) {
		metas = append(metas, metaFromV2Entry(entry))
		if len(metas) >= memwiki.MaxSearchResults {
			break
		}
	}
	return metas, nil
}

// metaFromV2Entry 把 v2 条目投影成 wiki 元数据。
func metaFromV2Entry(entry memoryvfs.Entry) PageMeta {
	name := entry.URI.Base()
	title := strings.TrimSpace(entry.Title)
	if title == "" {
		title = name
	}
	summary := strings.TrimSpace(entry.Summary)
	if summary == "" {
		summary = "(no summary)"
	}
	return PageMeta{
		Name:    name,
		Title:   title,
		Summary: memoryvfs.CollapseWhitespace(summary),
		Updated: entry.Updated.UTC(),
		Bytes:   len(entry.Content),
	}
}

// pageFromV2Entry 把 v2 条目投影成 wiki 页面。
func pageFromV2Entry(name string, entry memoryvfs.Entry) Page {
	meta := metaFromV2Entry(entry)
	return Page{
		Name:    name,
		Title:   meta.Title,
		Summary: meta.Summary,
		Content: entry.Content,
		Updated: meta.Updated,
	}
}

// mapWikiError 把存储错误映射成 wiki 契约错误。旧工具按 memwiki.ErrNotFound
// 渲染 "no page named …"，所以这里必须保持 errors.Is 可用。
func mapWikiError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, memwiki.ErrNotFound),
		errors.Is(err, memwiki.ErrInvalidName),
		errors.Is(err, memwiki.ErrSummaryRequired),
		errors.Is(err, memwiki.ErrSummaryTooLong):
		return err
	case errors.Is(err, memoryvfs.ErrNotFound):
		return fmt.Errorf("%w: memory entry not found", memwiki.ErrNotFound)
	case errors.Is(err, memoryvfs.ErrInvalidURI), errors.Is(err, memoryvfs.ErrInvalidEntry):
		return fmt.Errorf("%w: memory name is not a valid slug", memwiki.ErrInvalidName)
	case errors.Is(err, memoryvfs.ErrClosed):
		return errors.New("memory: backend is closed")
	case errors.Is(err, memoryvfs.ErrQuotaExceeded):
		return errors.New("memory: backend quota exceeded")
	case errors.Is(err, memoryvfs.ErrConflict):
		return errors.New("memory: concurrent update conflict; retry")
	case errors.Is(err, memoryvfs.ErrCorrupt):
		return errors.New("memory: backend data is corrupt; run a rebuild or restore")
	}
	return fmt.Errorf("memory: backend operation failed: %v", err)
}

var errNoV2Store = errors.New("memory: no store configured for the wiki adapter")
