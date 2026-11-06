package web

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// searchArgs is the web_search argument object.
type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// SearcherFunc adapts a function to Searcher.
type SearcherFunc func(ctx context.Context, query string, limit int) ([]SearchResult, error)

// Search implements Searcher.
func (f SearcherFunc) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	return f(ctx, query, limit)
}

// searchTool is the v1.Tool implementation of web_search.
type searchTool struct {
	searcher Searcher
}

// Name implements v1.Tool.
func (t *searchTool) Name() string { return "web_search" }

// Description implements v1.Tool.
func (t *searchTool) Description() string {
	return "Search the web through the host's configured search backend and return the top results as " +
		"title, URL and snippet. The search backend is pluggable; query and result limits are passed to it."
}

// Schema implements v1.Tool.
func (t *searchTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"query": stringProp("Search query."),
		"limit": integerProp(fmt.Sprintf(
			"Maximum number of results (default %d, maximum %d).", DefaultSearchLimit, MaxSearchLimit)),
	}, "query")
}

// Invoke implements v1.Tool.
//
// An unconfigured tool (nil Searcher) returns an error rather than a model
// tool result: that is a host wiring mistake, not something the model can fix.
func (t *searchTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	var args searchArgs
	if err := decodeArgs(call, &args); err != nil {
		return v1.ToolResult{}, err
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return errorResult("web_search: query is required"), nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	if t.searcher == nil {
		return v1.ToolResult{}, fmt.Errorf(
			"web_search: no Searcher configured (construct the tool with NewSearcher)")
	}
	if err := ctx.Err(); err != nil {
		return v1.ToolResult{}, err
	}
	results, err := t.searcher.Search(ctx, query, limit)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return v1.ToolResult{}, ctxErr
		}
		return errorResult("web_search: search failed: %v", err), nil
	}
	if len(results) > limit {
		results = results[:limit]
	}
	if len(results) == 0 {
		return textResult("no results"), nil
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
	return textResult(strings.TrimRight(builder.String(), "\n")), nil
}
