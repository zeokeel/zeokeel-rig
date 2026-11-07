package web

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// searchArgs is the web_search argument object.
type searchArgs struct {
	Query   string   `json:"query"`
	Queries []string `json:"queries"`
	Limit   int      `json:"limit"`
}

// SearcherFunc adapts a function to Searcher.
type SearcherFunc func(ctx context.Context, query string, limit int) ([]SearchResult, error)

// Search implements Searcher.
func (f SearcherFunc) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	return f(ctx, query, limit)
}

// searchTool is the rig.Tool implementation of web_search.
type searchTool struct {
	searcher Searcher
}

// Name implements rig.Tool.
func (t *searchTool) Name() string { return "web_search" }

// Description implements rig.Tool.
func (t *searchTool) Description() string {
	return "Search the web through the host's configured search backend and return the top results as " +
		"title, URL and snippet. Pass one `query` or a DSH-compatible `queries` array (1-4 strings); " +
		"the search backend is pluggable and the result limit is passed to it."
}

// Schema implements rig.Tool.
func (t *searchTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"query": toolutil.StringProp("Single search query (legacy alias; use queries for multi-query search)."),
		"queries": map[string]any{
			"type":        "array",
			"description": fmt.Sprintf("DSH-compatible search queries (1-%d non-empty strings; duplicates run once).", MaxSearchQueries),
			"items":       map[string]any{"type": "string"},
		},
		"limit": toolutil.IntegerProp(fmt.Sprintf(
			"Maximum number of results (default %d, maximum %d).", DefaultSearchLimit, MaxSearchLimit)),
	})
}

// Invoke implements rig.Tool.
//
// An unconfigured tool (nil Searcher) returns an error rather than a model
// tool result: that is a host wiring mistake, not something the model can fix.
func (t *searchTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args searchArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	var queries []string
	seenQueries := make(map[string]bool, len(args.Queries)+1)
	addQuery := func(raw string) {
		query := strings.TrimSpace(raw)
		if query == "" || seenQueries[query] {
			return
		}
		seenQueries[query] = true
		queries = append(queries, query)
	}
	addQuery(args.Query)
	for _, query := range args.Queries {
		addQuery(query)
	}
	if len(queries) == 0 {
		return toolutil.ErrorResultf("web_search: query or queries is required"), nil
	}
	if len(queries) > MaxSearchQueries {
		return toolutil.ErrorResultf("web_search: at most %d queries are allowed, got %d", MaxSearchQueries, len(queries)), nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	limit = min(limit, MaxSearchLimit)
	if t.searcher == nil {
		return rig.ToolResult{}, fmt.Errorf(
			"web_search: no Searcher configured (construct the tool with NewSearcher)")
	}
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	results := make([]SearchResult, 0, limit)
	seenResults := make(map[string]bool, limit)
	for _, query := range queries {
		batch, err := t.searcher.Search(ctx, query, limit)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return rig.ToolResult{}, ctxErr
			}
			return toolutil.ErrorResultf("web_search: search failed: %v", err), nil
		}
		for _, result := range batch {
			key := strings.TrimSpace(result.URL)
			if key == "" {
				key = strings.TrimSpace(result.Title)
			}
			if key != "" && seenResults[key] {
				continue
			}
			seenResults[key] = true
			results = append(results, result)
			if len(results) >= limit {
				break
			}
		}
		if len(results) >= limit {
			break
		}
	}
	if len(results) == 0 {
		return toolutil.TextResult("no results"), nil
	}
	var builder strings.Builder
	for index, result := range results {
		title := strings.TrimSpace(result.Title)
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&builder, "%d. %s\n", index+1, title)
		if strings.TrimSpace(result.URL) != "" {
			fmt.Fprintf(&builder, "   %s\n", strings.TrimSpace(result.URL))
		}
		if snippet := collapseWhitespace(result.Snippet); snippet != "" {
			fmt.Fprintf(&builder, "   %s\n", snippet)
		}
	}
	return toolutil.TextResult(strings.TrimRight(builder.String(), "\n")), nil
}
