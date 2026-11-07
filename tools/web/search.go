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
	Query string `json:"query"`
	Limit int    `json:"limit"`
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
		"title, URL and snippet. The search backend is pluggable; query and result limits are passed to it."
}

// Schema implements rig.Tool.
func (t *searchTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"query": toolutil.StringProp("Search query."),
		"limit": toolutil.IntegerProp(fmt.Sprintf(
			"Maximum number of results (default %d, maximum %d).", DefaultSearchLimit, MaxSearchLimit)),
	}, "query")
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
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return toolutil.ErrorResultf("web_search: query is required"), nil
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
	results, err := t.searcher.Search(ctx, query, limit)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return rig.ToolResult{}, ctxErr
		}
		return toolutil.ErrorResultf("web_search: search failed: %v", err), nil
	}
	if len(results) > limit {
		results = results[:limit]
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
