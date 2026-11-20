package memsearch

import (
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

var testClock = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func mustURI(t *testing.T, raw string) memoryvfs.URI {
	t.Helper()
	uri, err := memoryvfs.ParseURI(raw)
	if err != nil {
		t.Fatalf("ParseURI(%q): %v", raw, err)
	}
	return uri
}

// doc 构造一条记忆条目；URI 必须是合法 mem:// 地址。
func doc(t *testing.T, raw, title, summary, overview, content string) memoryvfs.Entry {
	t.Helper()
	return memoryvfs.Entry{
		URI:      mustURI(t, raw),
		Kind:     memoryvfs.KindMemory,
		Type:     memoryvfs.TypeEntities,
		Title:    title,
		Summary:  summary,
		Overview: overview,
		Content:  content,
		Updated:  testClock,
	}
}

func newIndex(entries ...memoryvfs.Entry) *Index {
	index := New(1, 1, 0)
	for _, entry := range entries {
		index.Put(entry)
	}
	return index
}

func urisOf(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, string(hit.URI))
	}
	return out
}

func scoreOf(hits []Hit, uri memoryvfs.URI) float64 {
	for _, hit := range hits {
		if hit.URI == uri {
			return hit.Score
		}
	}
	return 0
}

func TestIndexPutLenAndReplace(t *testing.T) {
	index := New(1, 1, 0)
	if index.Len() != 0 {
		t.Fatalf("Len = %d, want 0", index.Len())
	}
	index.Put(doc(t, "mem://user/self/facts/alpha", "alpha", "alpha summary", "", "alpha body"))
	if index.Len() != 1 {
		t.Fatalf("Len = %d, want 1", index.Len())
	}
	// 同 URI 覆盖写不应增加条目数，也不应留下旧词项。
	index.Put(doc(t, "mem://user/self/facts/alpha", "gamma", "gamma summary", "", "gamma body"))
	if index.Len() != 1 {
		t.Fatalf("Len after replace = %d, want 1", index.Len())
	}
	if got := index.DocFreq("alpha"); got != 0 {
		t.Fatalf("DocFreq(alpha) = %d after replace, want 0", got)
	}
	if got := index.DocFreq("gamma"); got != 1 {
		t.Fatalf("DocFreq(gamma) = %d, want 1", got)
	}
}

func TestSearchRanksBM25MatchingDoc(t *testing.T) {
	alpha := doc(t, "mem://user/self/facts/alpha", "alpha", "alpha beta gamma", "", "alpha alpha")
	other := doc(t, "mem://user/self/facts/other", "delta", "delta epsilon", "", "zeta")
	index := newIndex(alpha, other)

	hits := index.Search(memoryvfs.Query{Text: "alpha"})
	if len(hits) != 1 {
		t.Fatalf("hits = %#v, want exactly the matching doc", urisOf(hits))
	}
	if hits[0].URI != alpha.URI {
		t.Fatalf("hit = %s, want %s", hits[0].URI, alpha.URI)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("score = %v, want > 0", hits[0].Score)
	}
}

func TestSearchReturnsNothingForUnknownTerm(t *testing.T) {
	index := newIndex(doc(t, "mem://user/self/facts/alpha", "alpha", "alpha", "", "alpha"))
	if hits := index.Search(memoryvfs.Query{Text: "nonexistent"}); len(hits) != 0 {
		t.Fatalf("hits = %#v, want none", urisOf(hits))
	}
	// 纯标点查询没有词项，也不能把所有条目都倒出来。
	if hits := index.Search(memoryvfs.Query{Text: "???"}); len(hits) != 0 {
		t.Fatalf("hits = %#v for punctuation-only query, want none", urisOf(hits))
	}
}

func TestFieldWeightingTitleBeatsSummaryBeatsContent(t *testing.T) {
	// 三个文档 token 数相同，只有命中字段不同：title 3 / summary 2 / overview 1.5 / content 1。
	titleHit := doc(t, "mem://user/self/facts/title", "zephyr", "", "", "")
	summaryHit := doc(t, "mem://user/self/facts/summary", "", "zephyr", "", "")
	overviewHit := doc(t, "mem://user/self/facts/overview", "", "", "zephyr", "")
	contentHit := doc(t, "mem://user/self/facts/content", "", "", "", "zephyr")
	index := newIndex(titleHit, summaryHit, overviewHit, contentHit)

	hits := index.Search(memoryvfs.Query{Text: "zephyr"})
	if len(hits) != 4 {
		t.Fatalf("hits = %#v, want 4", urisOf(hits))
	}
	want := []memoryvfs.URI{titleHit.URI, summaryHit.URI, overviewHit.URI, contentHit.URI}
	for idx, uri := range want {
		if hits[idx].URI != uri {
			t.Fatalf("rank %d = %s, want %s (order %#v)", idx, hits[idx].URI, uri, urisOf(hits))
		}
	}
	if scoreOf(hits, titleHit.URI) <= scoreOf(hits, summaryHit.URI) {
		t.Fatalf("title score %v not greater than summary score %v",
			scoreOf(hits, titleHit.URI), scoreOf(hits, summaryHit.URI))
	}
}

func TestBM25IDFFavorsRareTerm(t *testing.T) {
	rare := doc(t, "mem://user/self/facts/rare", "common rare", "common rare body", "", "")
	index := newIndex(
		rare,
		doc(t, "mem://user/self/facts/one", "common only", "common only body", "", ""),
		doc(t, "mem://user/self/facts/two", "common only", "common only body", "", ""),
		doc(t, "mem://user/self/facts/three", "common only", "common only body", "", ""),
	)
	rareHits := index.Search(memoryvfs.Query{Text: "rare"})
	commonHits := index.Search(memoryvfs.Query{Text: "common"})
	rareScore := scoreOf(rareHits, rare.URI)
	commonScore := scoreOf(commonHits, rare.URI)
	if rareScore <= commonScore {
		t.Fatalf("rare idf score %v not greater than common idf score %v", rareScore, commonScore)
	}
	if len(commonHits) != 4 {
		t.Fatalf("common hits = %#v, want 4", urisOf(commonHits))
	}
}

func TestEmptyQueryOrdersByUpdatedDescending(t *testing.T) {
	older := doc(t, "mem://user/self/facts/older", "older", "older summary", "", "older body")
	older.Updated = testClock.Add(-24 * time.Hour)
	newer := doc(t, "mem://user/self/facts/newer", "newer", "newer summary", "", "newer body")
	newer.Updated = testClock
	middle := doc(t, "mem://user/self/facts/middle", "middle", "middle summary", "", "middle body")
	middle.Updated = testClock.Add(-time.Hour)
	index := newIndex(older, newer, middle)

	hits := index.Search(memoryvfs.Query{Text: "  "})
	want := []memoryvfs.URI{newer.URI, middle.URI, older.URI}
	if got := urisOf(hits); len(got) != len(want) {
		t.Fatalf("hits = %#v, want %d", got, len(want))
	}
	for idx := range want {
		if hits[idx].URI != want[idx] {
			t.Fatalf("rank %d = %s, want %s", idx, hits[idx].URI, want[idx])
		}
	}
	if hits[0].Level != memoryvfs.Level1 {
		t.Fatalf("empty query level = %s, want L1", hits[0].Level)
	}
}

func TestFiltersScopeKindsTypesTags(t *testing.T) {
	base := doc(t, "mem://user/self/facts/alpha", "alpha", "alpha summary", "", "alpha body")
	inScope := doc(t, "mem://user/self/profiles/alpha", "alpha", "alpha summary", "", "alpha body")
	otherScope := doc(t, "mem://user/other/facts/alpha", "alpha", "alpha summary", "", "alpha body")
	resource := doc(t, "mem://resources/docs/alpha", "alpha", "alpha summary", "", "alpha body")
	resource.Kind = memoryvfs.KindResource
	typed := doc(t, "mem://user/self/events/alpha", "alpha", "alpha summary", "", "alpha body")
	typed.Type = memoryvfs.TypeEvents
	tagged := doc(t, "mem://user/self/facts/beta", "alpha", "alpha summary", "", "alpha body")
	tagged.Tags = map[string]string{"topic": "deploy"}
	index := newIndex(base, inScope, otherScope, resource, typed, tagged)

	cases := []struct {
		name  string
		query memoryvfs.Query
		want  []memoryvfs.URI
	}{
		{
			name:  "scope is a segment prefix",
			query: memoryvfs.Query{Text: "alpha", Scope: mustURI(t, "mem://user/self/facts")},
			want:  []memoryvfs.URI{base.URI, tagged.URI},
		},
		{
			name:  "scope does not match a partial segment",
			query: memoryvfs.Query{Text: "alpha", Scope: mustURI(t, "mem://user/sel")},
			want:  nil,
		},
		{
			name:  "root scope means everything",
			query: memoryvfs.Query{Text: "alpha", Scope: memoryvfs.RootURI},
			want:  []memoryvfs.URI{base.URI, inScope.URI, otherScope.URI, resource.URI, typed.URI, tagged.URI},
		},
		{
			name:  "kind filter",
			query: memoryvfs.Query{Text: "alpha", Kinds: []memoryvfs.Kind{memoryvfs.KindResource}},
			want:  []memoryvfs.URI{resource.URI},
		},
		{
			name:  "type filter",
			query: memoryvfs.Query{Text: "alpha", Types: []string{memoryvfs.TypeEvents}},
			want:  []memoryvfs.URI{typed.URI},
		},
		{
			name:  "tag filter",
			query: memoryvfs.Query{Text: "alpha", Tags: map[string]string{"topic": "deploy"}},
			want:  []memoryvfs.URI{tagged.URI},
		},
		{
			name:  "tag filter requires the value",
			query: memoryvfs.Query{Text: "alpha", Tags: map[string]string{"topic": "other"}},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := index.Search(tc.query)
			if len(got) != len(tc.want) {
				t.Fatalf("hits = %#v, want %#v", urisOf(got), tc.want)
			}
			seen := make(map[memoryvfs.URI]bool, len(got))
			for _, hit := range got {
				seen[hit.URI] = true
			}
			for _, uri := range tc.want {
				if !seen[uri] {
					t.Fatalf("hits = %#v, missing %s", urisOf(got), uri)
				}
			}
		})
	}
}

func TestLevelsFilterAndMatchedLevel(t *testing.T) {
	titleHit := doc(t, "mem://user/self/facts/title", "zephyr", "unrelated summary", "", "unrelated body")
	summaryHit := doc(t, "mem://user/self/facts/summary", "unrelated", "zephyr summary", "", "unrelated body")
	overviewHit := doc(t, "mem://user/self/facts/overview", "unrelated", "unrelated summary", "zephyr overview", "unrelated body")
	contentHit := doc(t, "mem://user/self/facts/content", "unrelated", "unrelated summary", "unrelated overview", "zephyr body")
	index := newIndex(titleHit, summaryHit, overviewHit, contentHit)

	hits := index.Search(memoryvfs.Query{Text: "zephyr"})
	wantLevel := map[memoryvfs.URI]memoryvfs.Level{
		titleHit.URI:    memoryvfs.Level0,
		summaryHit.URI:  memoryvfs.Level0,
		overviewHit.URI: memoryvfs.Level1,
		contentHit.URI:  memoryvfs.Level2,
	}
	if len(hits) != len(wantLevel) {
		t.Fatalf("hits = %#v, want 4", urisOf(hits))
	}
	for _, hit := range hits {
		if hit.Level != wantLevel[hit.URI] {
			t.Fatalf("%s level = %s, want %s", hit.URI, hit.Level, wantLevel[hit.URI])
		}
	}

	l0 := index.Search(memoryvfs.Query{Text: "zephyr", Levels: []memoryvfs.Level{memoryvfs.Level0}})
	if len(l0) != 2 {
		t.Fatalf("L0 hits = %#v, want title+summary", urisOf(l0))
	}
	l12 := index.Search(memoryvfs.Query{Text: "zephyr", Levels: []memoryvfs.Level{memoryvfs.Level1, memoryvfs.Level2}})
	if len(l12) != 2 {
		t.Fatalf("L1+L2 hits = %#v, want overview+content", urisOf(l12))
	}
	anyLevel := index.Search(memoryvfs.Query{Text: "zephyr", Levels: []memoryvfs.Level{memoryvfs.LevelAny}})
	if len(anyLevel) != 4 {
		t.Fatalf("LevelAny hits = %#v, want 4", urisOf(anyLevel))
	}
}

func TestMultiFieldHitTakesHighestPrecedenceLevel(t *testing.T) {
	entry := doc(t, "mem://user/self/facts/both", "zephyr", "zephyr summary", "zephyr overview", "zephyr body")
	index := newIndex(entry)
	hits := index.Search(memoryvfs.Query{Text: "zephyr"})
	if len(hits) != 1 {
		t.Fatalf("hits = %#v, want 1", urisOf(hits))
	}
	if hits[0].Level != memoryvfs.Level0 {
		t.Fatalf("level = %s, want L0 (title/summary wins)", hits[0].Level)
	}
}

func TestDeleteUpdatesDocFreqAndScores(t *testing.T) {
	first := doc(t, "mem://user/self/facts/first", "zephyr", "zephyr shared", "", "zephyr body")
	second := doc(t, "mem://user/self/facts/second", "zephyr", "zephyr shared", "", "zephyr body")
	index := newIndex(first, second)

	if got := index.DocFreq("zephyr"); got != 2 {
		t.Fatalf("DocFreq = %d, want 2", got)
	}
	before := scoreOf(index.Search(memoryvfs.Query{Text: "zephyr"}), first.URI)
	index.Delete(second.URI)
	if index.Len() != 1 {
		t.Fatalf("Len = %d, want 1", index.Len())
	}
	if got := index.DocFreq("zephyr"); got != 1 {
		t.Fatalf("DocFreq after delete = %d, want 1", got)
	}
	after := scoreOf(index.Search(memoryvfs.Query{Text: "zephyr"}), first.URI)
	if after <= before {
		t.Fatalf("score after delete = %v, want > %v (df got smaller, idf got bigger)", after, before)
	}
	index.Delete(first.URI)
	if got := index.DocFreq("zephyr"); got != 0 {
		t.Fatalf("DocFreq after deleting all = %d, want 0", got)
	}
	if hits := index.Search(memoryvfs.Query{Text: "zephyr"}); len(hits) != 0 {
		t.Fatalf("hits = %#v after deleting all docs", urisOf(hits))
	}
	if got := index.DocFreq("zephyr"); got != 0 {
		t.Fatalf("DocFreq = %d, want 0", got)
	}
}

func TestDeleteUnknownURIAndPostings(t *testing.T) {
	index := newIndex(doc(t, "mem://user/self/facts/alpha", "alpha", "alpha", "", "alpha"))
	index.Delete(mustURI(t, "mem://user/self/facts/missing"))
	if index.Len() != 1 {
		t.Fatalf("Len = %d, want 1", index.Len())
	}
	postings := index.PostingURIs("alpha")
	if len(postings) != 1 || postings[0] != mustURI(t, "mem://user/self/facts/alpha") {
		t.Fatalf("postings = %#v", postings)
	}
	if got := index.PostingURIs("missing"); got != nil {
		t.Fatalf("postings for unknown term = %#v, want nil", got)
	}
}

func TestSearchRespectsLimitAndOffsetNormalization(t *testing.T) {
	index := New(1, 1, 0)
	for idx := 0; idx < 3; idx++ {
		raw := "mem://user/self/facts/item" + string(rune('a'+idx))
		index.Put(doc(t, raw, "zephyr", "zephyr", "", "zephyr"))
	}
	// Search 不截断（分页由调用方做），但 Limit 会被规整成合法值。
	hits := index.Search(memoryvfs.Query{Text: "zephyr", Limit: 1, Offset: -5})
	if len(hits) != 3 {
		t.Fatalf("hits = %#v, want 3 (Search does not truncate)", urisOf(hits))
	}
}

func TestScopeFilterWithEmptyQuery(t *testing.T) {
	inside := doc(t, "mem://user/self/facts/inside", "inside", "inside", "", "inside")
	outside := doc(t, "mem://user/other/facts/outside", "outside", "outside", "", "outside")
	index := newIndex(inside, outside)
	hits := index.Search(memoryvfs.Query{Scope: mustURI(t, "mem://user/self")})
	if len(hits) != 1 || hits[0].URI != inside.URI {
		t.Fatalf("hits = %#v, want only %s", urisOf(hits), inside.URI)
	}
}

func TestConcurrentReadsAreSafe(t *testing.T) {
	index := New(1, 1, 2)
	for idx := 0; idx < 32; idx++ {
		raw := "mem://user/self/facts/item" + string(rune('a'+idx%26)) + string(rune('a'+idx/26))
		entry := doc(t, raw, "zephyr alpha", "zephyr alpha body", "", "alpha")
		index.Put(entry)
		index.SetVector(entry.URI, []float32{1, 0})
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < 50; round++ {
				hits := index.Search(memoryvfs.Query{Text: "zephyr", Vector: []float32{1, 0}})
				if len(hits) == 0 {
					t.Errorf("worker %d got no hits", worker)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}

// TestNoVectorChannelWithoutQueryVector 断言：文档有向量、但查询没有向量且未显式
// 允许 hash 兜底时，绝不把 hash 向量混进真实向量空间（审计 r1 #4）。
func TestNoVectorChannelWithoutQueryVector(t *testing.T) {
	index := New(1, 1, 4)
	index.Put(memoryvfs.Entry{URI: "mem://a", Kind: memoryvfs.KindMemory, Summary: "alpha beta", Updated: time.Now()})
	index.Put(memoryvfs.Entry{URI: "mem://b", Kind: memoryvfs.KindMemory, Summary: "cooking recipe", Updated: time.Now()})
	index.SetVector("mem://a", []float32{1, 0, 0, 0})
	index.SetVector("mem://b", []float32{1, 0, 0, 0}) // 与查询 hash 向量无关，但语义上"相似"
	hits := index.Search(memoryvfs.Query{Text: "alpha beta", Limit: 10})
	if len(hits) == 0 {
		t.Fatal("BM25 应至少命中 alpha")
	}
	for _, hit := range hits {
		if hit.URI == "mem://b" {
			t.Fatalf("没有查询向量时不应启用向量通道，却召回了 %s", hit.URI)
		}
	}
	// 显式允许 hash 兜底才启用（测试/管线演示用）。
	withHash := index.Search(memoryvfs.Query{Text: "alpha beta", Limit: 10, UseHashVector: true})
	found := false
	for _, hit := range withHash {
		if hit.URI == "mem://b" {
			found = true
		}
	}
	if !found {
		t.Fatal("UseHashVector 应启用 hash 向量通道")
	}
}

func TestAnyLevelAvailableUsesActualFields(t *testing.T) {
	entry := memoryvfs.Entry{Summary: "s", Overview: "o"}
	if !anyLevelAvailable(entry, []memoryvfs.Level{memoryvfs.Level1}) {
		t.Fatal("有 Overview 的条目应满足 L1")
	}
	if anyLevelAvailable(entry, []memoryvfs.Level{memoryvfs.Level2}) {
		t.Fatal("没有 Content 的条目不应满足 L2")
	}
	if !anyLevelAvailable(entry, nil) {
		t.Fatal("Levels 为空应视为不过滤")
	}
}
