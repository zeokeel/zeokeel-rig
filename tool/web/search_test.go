package web_test

// Tests for web_search: pluggable Searcher delegation, result formatting, limit
// clamping and error semantics.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tool/web"
)

// fakeSearcher records the delegated call and returns canned results.
type fakeSearcher struct {
	mu      sync.Mutex
	queries []string
	limits  []int
	results []web.SearchResult
	err     error
}

func (f *fakeSearcher) Search(_ context.Context, query string, limit int) ([]web.SearchResult, error) {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.limits = append(f.limits, limit)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	results := f.results
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (f *fakeSearcher) last() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[len(f.queries)-1], f.limits[len(f.limits)-1]
}

func searchArgs(t *testing.T, query string, limit any) string {
	t.Helper()
	if limit == nil {
		return fmt.Sprintf(`{"query":%q}`, query)
	}
	return fmt.Sprintf(`{"query":%q,"limit":%d}`, query, limit)
}

func TestSearchFormatsResults(t *testing.T) {
	searcher := &fakeSearcher{results: []web.SearchResult{
		{Title: "First hit", URL: "https://example.com/a", Snippet: "snippet one"},
		{Title: "Second hit", URL: "https://example.com/b", Snippet: "snippet  two\nwith spaces"},
	}}
	tool := web.NewSearcher(searcher)
	if tool.Name() != "web_search" {
		t.Fatalf("Name = %q, want web_search", tool.Name())
	}
	result := invoke(t, tool, searchArgs(t, "rig sdk", 2))
	if result.IsError {
		t.Fatalf("search = %q", result.Content)
	}
	requireContains(t, result.Content,
		"1. First hit", "https://example.com/a", "snippet one",
		"2. Second hit", "https://example.com/b", "snippet two with spaces")
	query, limit := searcher.last()
	if query != "rig sdk" || limit != 2 {
		t.Fatalf("searcher got query=%q limit=%d, want rig sdk/2", query, limit)
	}
}

func TestSearchLimitClamping(t *testing.T) {
	cases := []struct {
		name  string
		limit any
		want  int
	}{
		{"default", nil, web.DefaultSearchLimit},
		{"explicit", 2, 2},
		{"maximum", web.MaxSearchLimit + 50, web.MaxSearchLimit},
		{"zero", 0, web.DefaultSearchLimit},
		{"negative", -3, web.DefaultSearchLimit},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			searcher := &fakeSearcher{results: []web.SearchResult{{Title: "hit", URL: "https://example.com"}}}
			result := invoke(t, web.NewSearcher(searcher), searchArgs(t, "q", testCase.limit))
			if result.IsError {
				t.Fatalf("search = %q", result.Content)
			}
			_, limit := searcher.last()
			if limit != testCase.want {
				t.Fatalf("searcher limit = %d, want %d", limit, testCase.want)
			}
		})
	}
}

func TestSearchWithoutSearcherIsAConfigurationError(t *testing.T) {
	_, err := web.NewSearcher(nil).Invoke(context.Background(), v1.ToolCall{
		Arguments: `{"query":"q"}`,
	})
	if err == nil {
		t.Fatal("search with no Searcher returned nil error")
	}
	requireContains(t, err.Error(), "Searcher")
}

func TestSearchSearcherErrorAndNoResults(t *testing.T) {
	failing := &fakeSearcher{err: errors.New("backend down")}
	text := invokeError(t, web.NewSearcher(failing), searchArgs(t, "q", nil))
	requireContains(t, text, "search failed", "backend down")

	empty := &fakeSearcher{}
	result := invoke(t, web.NewSearcher(empty), searchArgs(t, "q", nil))
	if result.IsError || result.Content != "no results" {
		t.Fatalf("empty search = %q (IsError=%v), want no results", result.Content, result.IsError)
	}
}

func TestSearchArgumentErrors(t *testing.T) {
	tool := web.NewSearcher(&fakeSearcher{})
	if text := invokeError(t, tool, `{}`); !strings.Contains(text, "query") {
		t.Fatalf("missing query = %q", text)
	}
	if _, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: "{"}); err == nil {
		t.Fatal("malformed arguments returned nil error")
	}
}

func TestSearchSchema(t *testing.T) {
	tool := web.NewSearcher(&fakeSearcher{})
	if tool.Description() == "" {
		t.Fatal("web_search has an empty description")
	}
	schema := tool.Schema()
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "query" {
		t.Fatalf("schema required = %v, want [query]", required)
	}
}

func TestSearchIsConcurrent(t *testing.T) {
	searcher := &fakeSearcher{results: []web.SearchResult{{Title: "hit", URL: "https://example.com"}}}
	tool := web.NewSearcher(searcher)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for call := 0; call < 10; call++ {
				result, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: searchArgs(t, "q", 3)})
				if err != nil || result.IsError {
					t.Errorf("concurrent search: result=%q err=%v", result.Content, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	searcher.mu.Lock()
	defer searcher.mu.Unlock()
	if len(searcher.queries) != 80 {
		t.Fatalf("searcher calls = %d, want 80", len(searcher.queries))
	}
}
