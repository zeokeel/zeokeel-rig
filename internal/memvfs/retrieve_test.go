package memvfs

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func seedCorpus(t *testing.T, store *Store) {
	t.Helper()
	mustWrite(t, store, memEntry("mem://user/self/preferences/dark", "preferences",
		"prefers dark mode theme", "prefers dark mode theme in the editor and terminal"))
	mustWrite(t, store, memEntry("mem://user/self/preferences/tabs", "preferences",
		"prefers tabs over spaces", "tabs for indentation in go code"))
	mustWrite(t, store, memoryvfs.Entry{
		URI:     "mem://resources/docs/go",
		Kind:    memoryvfs.KindResource,
		Title:   "Go guide",
		Summary: "go build and go test guide",
		Content: "run go build then go test in the repo",
		Tags:    map[string]string{"topic": "golang"},
	})
	// 只命中一个词项的弱相关条目：让 threshold 能区分分数。
	mustWrite(t, store, memEntry("mem://user/self/events/dark-launch", "events",
		"shipped the dark launch feature", "shipped it behind a flag"))
}

func TestFindFiltersPaginationAndBody(t *testing.T) {
	store := newStore(t)
	seedCorpus(t, store)
	ctx := context.Background()

	all, err := store.Find(ctx, memoryvfs.Query{Text: "dark mode", IncludeBody: true})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(all.Entries) == 0 {
		t.Fatal("no hits")
	}
	if all.Entries[0].URI != "mem://user/self/preferences/dark" {
		t.Fatalf("top hit = %s", all.Entries[0].URI)
	}
	if all.Entries[0].Content == "" {
		t.Fatal("IncludeBody should keep content")
	}

	noBody, err := store.Find(ctx, memoryvfs.Query{Text: "dark mode"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	for _, entry := range noBody.Entries {
		if entry.Content != "" {
			t.Fatalf("IncludeBody=false kept content for %s", entry.URI)
		}
	}

	kinds, err := store.Find(ctx, memoryvfs.Query{Text: "go", Kinds: []memoryvfs.Kind{memoryvfs.KindResource}})
	if err != nil {
		t.Fatalf("Find kinds: %v", err)
	}
	for _, entry := range kinds.Entries {
		if entry.Kind != memoryvfs.KindResource {
			t.Fatalf("kind filter leaked %s (%s)", entry.URI, entry.Kind)
		}
	}
	if len(kinds.Entries) == 0 {
		t.Fatal("kind filter returned nothing")
	}

	types, err := store.Find(ctx, memoryvfs.Query{Text: "prefers", Types: []string{"preferences"}})
	if err != nil {
		t.Fatalf("Find types: %v", err)
	}
	if len(types.Entries) != 2 {
		t.Fatalf("types filter = %+v", types.Entries)
	}

	tagged, err := store.Find(ctx, memoryvfs.Query{Text: "go", Tags: map[string]string{"topic": "golang"}})
	if err != nil {
		t.Fatalf("Find tags: %v", err)
	}
	if len(tagged.Entries) != 1 || tagged.Entries[0].URI != "mem://resources/docs/go" {
		t.Fatalf("tag filter = %+v", tagged.Entries)
	}

	scoped, err := store.Find(ctx, memoryvfs.Query{Text: "prefers", Scope: "mem://resources"})
	if err != nil {
		t.Fatalf("Find scope: %v", err)
	}
	for _, entry := range scoped.Entries {
		if !entry.URI.IsUnder("mem://resources") {
			t.Fatalf("scope leaked %s", entry.URI)
		}
	}

	// 分页：Total 是过滤后、分页前的命中数。
	for index := 0; index < 5; index++ {
		mustWrite(t, store, memEntry(
			"mem://user/self/cases/budget"+string(rune('a'+index)),
			"cases", "budget case "+string(rune('a'+index)), "budget review notes"))
	}
	page, err := store.Find(ctx, memoryvfs.Query{Text: "budget", Limit: 2})
	if err != nil {
		t.Fatalf("Find page: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("page size = %d", len(page.Entries))
	}
	if page.Total != 5 {
		t.Fatalf("total = %d, want 5", page.Total)
	}
	next, err := store.Find(ctx, memoryvfs.Query{Text: "budget", Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("Find offset: %v", err)
	}
	if len(next.Entries) != 2 {
		t.Fatalf("offset page = %d", len(next.Entries))
	}
	if next.Entries[0].URI == page.Entries[0].URI {
		t.Fatalf("offset did not advance: %s", next.Entries[0].URI)
	}
	beyond, err := store.Find(ctx, memoryvfs.Query{Text: "budget", Limit: 2, Offset: 99})
	if err != nil {
		t.Fatalf("Find beyond: %v", err)
	}
	if len(beyond.Entries) != 0 || beyond.Total != 5 {
		t.Fatalf("beyond page = %+v", beyond)
	}
}

func TestFindThresholdOnFinalScores(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	mustWrite(t, store, memoryvfs.Entry{
		URI: "mem://user/self/memories/preferences/theme", Kind: memoryvfs.KindMemory,
		Type: "preferences", Summary: "dark mode theme editor", Content: "dark mode theme editor",
	})
	mustWrite(t, store, memoryvfs.Entry{
		URI: "mem://user/self/memories/preferences/cooking", Kind: memoryvfs.KindMemory,
		Type: "preferences", Summary: "cooking recipe", Content: "cooking recipe",
	})

	all, err := store.Find(ctx, memoryvfs.Query{Text: "dark mode", Limit: 10})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(all.Entries) == 0 {
		t.Fatal("baseline should return the theme entry")
	}
	if all.Entries[0].Score >= 1 {
		t.Fatalf("absolute squash must keep scores below 1, got %.4f", all.Entries[0].Score)
	}
	// 高阈值必须能表达"没有足够相关的命中"：只有一条弱命中时 0.99 应返回空。
	strict, err := store.Find(ctx, memoryvfs.Query{Text: "dark mode", Limit: 10, Threshold: 0.99})
	if err != nil {
		t.Fatalf("Find threshold: %v", err)
	}
	if len(strict.Entries) != 0 {
		t.Fatalf("threshold 0.99 仍返回 %d 条（最高分 %.4f）", len(strict.Entries), strict.Entries[0].Score)
	}
	// 低阈值仍能过滤掉完全不相关的条目。
	filtered, err := store.Find(ctx, memoryvfs.Query{Text: "dark mode", Limit: 10, Threshold: all.Entries[0].Score / 2})
	if err != nil {
		t.Fatalf("Find threshold: %v", err)
	}
	for _, entry := range filtered.Entries {
		if entry.Score < all.Entries[0].Score/2 {
			t.Fatalf("score %.3f below threshold", entry.Score)
		}
	}
}

func TestFindLevelsFilter(t *testing.T) {
	store := newStore(t)
	mustWrite(t, store, memoryvfs.Entry{
		URI: "mem://user/self/events/withbody", Kind: memoryvfs.KindMemory, Type: "events",
		Summary: "body entry", Overview: "see notes", Content: "full body text with indentation notes",
	})
	mustWrite(t, store, memoryvfs.Entry{
		URI: "mem://user/self/events/nobody", Kind: memoryvfs.KindMemory, Type: "events",
		Summary: "summary only entry", Overview: "no body here",
	})
	ctx := context.Background()

	// Level 过滤按"命中层级"判定：只出现在正文里的词项才算 L2 命中。
	onlyL2, err := store.Find(ctx, memoryvfs.Query{Text: "indentation", Levels: []memoryvfs.Level{memoryvfs.Level2}})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(onlyL2.Entries) != 1 || onlyL2.Entries[0].URI != "mem://user/self/events/withbody" {
		t.Fatalf("L2 filter = %+v", onlyL2.Entries)
	}
	l0Only, err := store.Find(ctx, memoryvfs.Query{Text: "indentation", Levels: []memoryvfs.Level{memoryvfs.Level0}})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(l0Only.Entries) != 0 {
		t.Fatalf("L0 filter should exclude content-only hits: %+v", l0Only.Entries)
	}
	anyLevel, err := store.Find(ctx, memoryvfs.Query{Text: "entry", Levels: []memoryvfs.Level{memoryvfs.LevelAny}})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(anyLevel.Entries) != 2 {
		t.Fatalf("any level = %d", len(anyLevel.Entries))
	}
}

func TestSearchTokenBudgetDegrades(t *testing.T) {
	store := newStore(t)
	long := strings.Repeat("budget checkpoint review notes ", 120)
	for index := 0; index < 4; index++ {
		mustWrite(t, store, memEntry(
			"mem://user/self/cases/long"+string(rune('a'+index)),
			"cases", "checkpoint review "+string(rune('a'+index)), long))
	}
	budgeted, err := store.Search(context.Background(), memoryvfs.Query{Text: "checkpoint", TokenBudget: 60})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if budgeted.Tokens > 60 {
		t.Fatalf("tokens = %d, over budget", budgeted.Tokens)
	}
	if budgeted.Total != 4 {
		t.Fatalf("total = %d, want 4", budgeted.Total)
	}
	if len(budgeted.Entries) == 0 {
		t.Fatal("budget should still fit L0 entries")
	}
	for _, entry := range budgeted.Entries {
		if entry.Content != "" {
			t.Fatalf("entry %s should be degraded below L2", entry.URI)
		}
		if entry.TokenEstimate(memoryvfs.Level0) > 60 {
			t.Fatalf("L0 estimate over budget: %+v", entry)
		}
	}

	tight, err := store.Search(context.Background(), memoryvfs.Query{Text: "checkpoint", TokenBudget: 1})
	if err != nil {
		t.Fatalf("Search tight: %v", err)
	}
	if tight.Tokens > 1 {
		t.Fatalf("tight tokens = %d", tight.Tokens)
	}
}

func TestSearchIntentPlanAndDedup(t *testing.T) {
	intent := &fakeIntent{queries: []memoryvfs.TypedQuery{
		{Text: "dark mode", Kind: memoryvfs.KindMemory, Type: "preferences", Priority: 1},
		{Text: "dark mode theme", Kind: memoryvfs.KindMemory, Type: "preferences", Priority: 2},
		{Text: "go build", Kind: memoryvfs.KindResource, Priority: 3},
	}}
	store := newStore(t, memoryvfs.WithIntentAnalyzer(intent))
	seedCorpus(t, store)

	result, err := store.Search(context.Background(), memoryvfs.Query{Text: "mode", Mode: memoryvfs.ModeThinking, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Plan) != 3 {
		t.Fatalf("plan = %+v", result.Plan)
	}
	seen := map[memoryvfs.URI]int{}
	for _, entry := range result.Entries {
		seen[entry.URI]++
	}
	for uri, count := range seen {
		if count > 1 {
			t.Fatalf("duplicate uri %s in results", uri)
		}
	}
	var foundDark, foundGo bool
	for _, entry := range result.Entries {
		switch entry.URI {
		case "mem://user/self/preferences/dark":
			foundDark = true
		case "mem://resources/docs/go":
			foundGo = true
		}
	}
	if !foundDark || !foundGo {
		t.Fatalf("expected hits from both queries: %+v", result.Entries)
	}
}

func TestSearchKeywordExpansionWithoutIntent(t *testing.T) {
	store := newStore(t)
	mustWrite(t, store, memEntry("mem://user/self/preferences/goose", "preferences",
		"prefers goose for deployments", "we always deploy with goose"))
	ctx := context.Background()
	if err := store.Capture(ctx, "sess-expand",
		memoryvfs.Message{Role: "user", Content: "we prefer goose for deployments every time"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	result, err := store.Search(ctx, memoryvfs.Query{
		Text: "goose", Mode: memoryvfs.ModeThinking, SessionID: "sess-expand", Limit: 5,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Plan) < 2 {
		t.Fatalf("expected keyword expansion, plan = %+v", result.Plan)
	}
	if result.Plan[0].Text != "goose" {
		t.Fatalf("first plan entry = %q", result.Plan[0].Text)
	}
	if len(result.Entries) == 0 {
		t.Fatal("search lost the direct hit")
	}

	quick, err := store.Search(ctx, memoryvfs.Query{Text: "goose", Mode: memoryvfs.ModeQuick, Limit: 5})
	if err != nil {
		t.Fatalf("Search quick: %v", err)
	}
	if len(quick.Plan) != 1 {
		t.Fatalf("quick plan = %+v", quick.Plan)
	}
}

// orderReranker 把候选顺序反转，用来验证 Reranker 被调用。
type orderReranker struct{ calls int }

func (r *orderReranker) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	r.calls++
	scores := make([]float64, len(documents))
	for index := range documents {
		scores[index] = float64(len(documents) - index)
	}
	return scores, nil
}

func TestSearchUsesReranker(t *testing.T) {
	reranker := &orderReranker{}
	store := newStore(t, memoryvfs.WithReranker(reranker))
	seedCorpus(t, store)

	result, err := store.Search(context.Background(), memoryvfs.Query{Text: "go", Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if reranker.calls == 0 {
		t.Fatal("reranker not called")
	}
	if len(result.Entries) > 2 {
		t.Fatalf("limit ignored: %d", len(result.Entries))
	}
}

func TestSearchEmbedsQueryWithEmbedder(t *testing.T) {
	embedder := &fakeEmbedder{dimension: 16}
	store := newStore(t, memoryvfs.WithEmbedder(embedder))
	seedCorpus(t, store)
	if embedder.calls == 0 {
		t.Fatal("embedder should be called on write")
	}
	before := embedder.calls
	if _, err := store.Search(context.Background(), memoryvfs.Query{Text: "dark theme"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if embedder.calls <= before {
		t.Fatal("embedder should be called for the query")
	}
}
