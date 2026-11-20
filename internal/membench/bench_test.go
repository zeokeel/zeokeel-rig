package membench

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func armByName(t *testing.T, report Report, name string) Arm {
	t.Helper()
	for _, arm := range report.Arms {
		if arm.Name == name {
			return arm
		}
	}
	t.Fatalf("arm %q missing; have %v", name, armNames(report))
	return Arm{}
}

func armNames(report Report) []string {
	names := make([]string, 0, len(report.Arms))
	for _, arm := range report.Arms {
		names = append(names, arm.Name)
	}
	return names
}

func TestGenerateIsDeterministic(t *testing.T) {
	first := Generate(7)
	second := Generate(7)
	if len(first.Docs) != len(second.Docs) || len(first.Queries) != len(second.Queries) {
		t.Fatalf("sizes differ: %d/%d docs, %d/%d queries",
			len(first.Docs), len(second.Docs), len(first.Queries), len(second.Queries))
	}
	for idx := range first.Docs {
		if !reflect.DeepEqual(first.Docs[idx], second.Docs[idx]) {
			t.Fatalf("doc %d differs between runs", idx)
		}
	}
	for idx := range first.Queries {
		if first.Queries[idx] != second.Queries[idx] {
			t.Fatalf("query %d differs between runs", idx)
		}
	}
	other := Generate(8)
	if reflect.DeepEqual(other.Docs, first.Docs) || reflect.DeepEqual(other.Queries, first.Queries) {
		t.Fatalf("different seeds produced the same dataset")
	}
}

func TestDatasetShape(t *testing.T) {
	dataset := Generate(42)
	if dataset.Sessions != 60 || dataset.TurnsPerSession != 30 {
		t.Fatalf("shape = %d sessions x %d turns, want 60 x 30",
			dataset.Sessions, dataset.TurnsPerSession)
	}
	var facts, hardFacts, distractors, twins int
	entities := make(map[string]int, len(dataset.Docs))
	gold := make(map[memoryvfs.URI]bool, len(dataset.Docs))
	for _, doc := range dataset.Docs {
		switch {
		case doc.Twin:
			twins++
		case doc.Gold && doc.Hard:
			hardFacts++
		case doc.Gold:
			facts++
		default:
			distractors++
		}
		entities[doc.Entity]++
		if len(doc.Topics) < 2 || len(doc.Topics) > 4 {
			t.Fatalf("%s has %d topics, want 2-4", doc.URI, len(doc.Topics))
		}
		if _, err := memoryvfs.ParseURI(string(doc.URI)); err != nil {
			t.Fatalf("%s is not a valid URI: %v", doc.URI, err)
		}
		if doc.Summary == "" || doc.Title == "" {
			t.Fatalf("%s missing title/summary", doc.URI)
		}
		if doc.Gold {
			gold[doc.URI] = true
		}
	}
	if facts != 240 {
		t.Fatalf("easy facts = %d, want 240", facts)
	}
	if hardFacts != 120 {
		t.Fatalf("hard facts = %d, want 120", hardFacts)
	}
	if distractors != 300 {
		t.Fatalf("distractors = %d, want 300", distractors)
	}
	if twins != 120 {
		t.Fatalf("hard twins = %d, want 120", twins)
	}
	if len(entities) != len(dataset.Docs) {
		t.Fatalf("entities = %d, want one unique entity per doc (%d)", len(entities), len(dataset.Docs))
	}
	if len(dataset.Queries) != 600 {
		t.Fatalf("queries = %d, want 600 (480 easy + 120 hard)", len(dataset.Queries))
	}

	counts := map[QueryKind]int{}
	subsets := map[string]int{}
	for _, query := range dataset.Queries {
		counts[query.Kind]++
		subsets[query.Subset]++
		if !gold[query.Gold] {
			t.Fatalf("query %q points at non-gold URI %s", query.Text, query.Gold)
		}
		if strings.TrimSpace(query.Text) == "" {
			t.Fatalf("query has no text")
		}
	}
	if counts[QueryRewrite] != 240 || counts[QueryIntent] != 240 || counts[QuerySemantic] != 120 {
		t.Fatalf("query kinds = %v, want 240 rewrite + 240 intent + 120 semantic", counts)
	}
	if subsets[SubsetEasy] != 480 || subsets[SubsetHard] != 120 {
		t.Fatalf("subsets = %v, want 480 easy + 120 hard", subsets)
	}
}

func TestHardQueriesOmitEntityNames(t *testing.T) {
	dataset := Generate(42)
	entities := make(map[memoryvfs.URI]string, len(dataset.Docs))
	for _, doc := range dataset.Docs {
		entities[doc.URI] = doc.Entity
	}
	hard := 0
	for _, query := range dataset.Queries {
		if query.Subset != SubsetHard {
			continue
		}
		hard++
		entity := entities[query.Gold]
		if entity == "" {
			t.Fatalf("hard query %q has no gold doc", query.Text)
		}
		if strings.Contains(query.Text, entity) {
			t.Fatalf("hard query leaks the entity name %q: %q", entity, query.Text)
		}
		if strings.TrimSpace(query.Text) == "" {
			t.Fatalf("hard query %q is empty", query.Text)
		}
	}
	if hard != 120 {
		t.Fatalf("hard queries = %d, want 120", hard)
	}
}

func TestHardTwinsAreNearDuplicates(t *testing.T) {
	dataset := Generate(42)
	byURI := make(map[memoryvfs.URI]Doc, len(dataset.Docs))
	golds := 0
	for _, doc := range dataset.Docs {
		byURI[doc.URI] = doc
		if doc.Hard && doc.Gold {
			golds++
		}
	}
	if golds != 120 {
		t.Fatalf("hard golds = %d, want 120", golds)
	}
	pairs := 0
	for _, twin := range dataset.Docs {
		if !twin.Twin {
			continue
		}
		gold, ok := byURI[twin.TwinOf]
		if !ok {
			t.Fatalf("twin %s points at unknown gold %q", twin.URI, twin.TwinOf)
		}
		// 共享全部主题词与属性，L0/L1 文本逐字相同——只有正文里的实体不同。
		if !reflect.DeepEqual(gold.Topics, twin.Topics) {
			t.Fatalf("twin %s topics differ from %s", twin.URI, gold.URI)
		}
		if !reflect.DeepEqual(gold.Attributes, twin.Attributes) {
			t.Fatalf("twin %s attributes differ from %s", twin.URI, gold.URI)
		}
		if gold.Title != twin.Title || gold.Summary != twin.Summary || gold.Overview != twin.Overview {
			t.Fatalf("twin %s L0/L1 is not byte-identical to %s", twin.URI, gold.URI)
		}
		if gold.Content == twin.Content {
			t.Fatalf("twin %s is a byte-identical copy of %s; the entity must differ in the body",
				twin.URI, gold.URI)
		}
		if !strings.Contains(gold.Content, gold.Entity) || !strings.Contains(twin.Content, twin.Entity) {
			t.Fatalf("twin/gold body must mention its own entity: %s / %s", twin.URI, gold.URI)
		}
		if twin.URI > gold.URI {
			t.Fatalf("twin URI %s should sort before gold %s (conservative tie-break)",
				twin.URI, gold.URI)
		}
		pairs++
	}
	if pairs != 120 {
		t.Fatalf("twin pairs = %d, want 120", pairs)
	}
}

func TestDatasetPlantsDistractorsSharingTopics(t *testing.T) {
	dataset := Generate(42)
	topicOf := make(map[string]bool, len(dataset.Docs))
	for _, doc := range dataset.Docs {
		if !doc.Gold || doc.Hard {
			continue
		}
		for _, topic := range doc.Topics {
			topicOf[topic] = true
		}
	}
	shared := 0
	for _, doc := range dataset.Docs {
		if doc.Gold || doc.Hard {
			continue
		}
		for _, topic := range doc.Topics {
			if topicOf[topic] {
				shared++
				break
			}
		}
	}
	if shared != 300 {
		t.Fatalf("distractors sharing a planted topic = %d, want 300", shared)
	}
}

func TestBaselineMirrorsWikiSubstringSemantics(t *testing.T) {
	title := Doc{URI: mustURI(t, "mem://user/self/facts/title"), Title: "zephyr notes", Summary: "summary", Content: "body"}
	summary := Doc{URI: mustURI(t, "mem://user/self/facts/summary"), Title: "other", Summary: "zephyr summary", Content: "body"}
	content := Doc{URI: mustURI(t, "mem://user/self/facts/content"), Title: "other", Summary: "other summary", Content: "zephyr body"}
	miss := Doc{URI: mustURI(t, "mem://user/self/facts/miss"), Title: "other", Summary: "other", Content: "other"}
	index := newWikiSubstringIndex([]Doc{miss, content, summary, title})

	hits := index.search("ZEPHYR")
	if len(hits) != 3 {
		t.Fatalf("hits = %d, want 3", len(hits))
	}
	want := []memoryvfs.URI{title.URI, summary.URI, content.URI}
	for idx, uri := range want {
		if hits[idx].URI != uri {
			t.Fatalf("rank %d = %s, want %s", idx, hits[idx].URI, uri)
		}
	}
	if hits[0].Level != memoryvfs.Level0 || hits[2].Level != memoryvfs.Level2 {
		t.Fatalf("levels = %s/%s, want L0/L2", hits[0].Level, hits[2].Level)
	}
	if got := index.search("   "); got != nil {
		t.Fatalf("empty query = %#v, want nil", got)
	}
	if got := index.search("nothing-matches"); len(got) != 0 {
		t.Fatalf("hits = %#v, want none", got)
	}
}

func TestBaselineCapsAtTwenty(t *testing.T) {
	docs := make([]Doc, 0, 25)
	for idx := 0; idx < 25; idx++ {
		docs = append(docs, Doc{
			URI:     mustURI(t, "mem://user/self/facts/item"+string(rune('a'+idx%26))+string(rune('a'+idx/26))),
			Title:   "zephyr",
			Summary: "zephyr",
			Content: "zephyr",
		})
	}
	hits := newWikiSubstringIndex(docs).search("zephyr")
	if len(hits) != 20 {
		t.Fatalf("hits = %d, want the 20-result cap", len(hits))
	}
}

func TestBenchmarkReport(t *testing.T) {
	report := Run(42)

	baseline := armByName(t, report, "baseline-wiki-substring")
	bm25 := armByName(t, report, "bm25")
	hybrid := armByName(t, report, "hybrid-hash-rrf")
	layered := armByName(t, report, "hybrid-layer")

	// easy 集：BM25 必须显著优于旧 wiki 子串语义（结构化字段 + 排序收益）。
	if bm25.Easy.RecallAt5 < 1.5*baseline.Easy.RecallAt5 {
		t.Fatalf("easy: bm25 recall@5 = %.3f is not 1.5x baseline %.3f",
			bm25.Easy.RecallAt5, baseline.Easy.RecallAt5)
	}
	if bm25.Easy.RecallAt5 < 0.6 {
		t.Fatalf("easy: bm25 recall@5 = %.3f, want >= 0.6", bm25.Easy.RecallAt5)
	}
	if bm25.Easy.MRR <= baseline.Easy.MRR {
		t.Fatalf("easy: bm25 MRR = %.3f not better than baseline %.3f",
			bm25.Easy.MRR, baseline.Easy.MRR)
	}

	// hard 集：只要求"明显高于 baseline"，不要求接近 1.0——它的作用是暴露词法上限。
	if bm25.Hard.RecallAt5 < 1.5*baseline.Hard.RecallAt5 {
		t.Fatalf("hard: bm25 recall@5 = %.3f is not 1.5x baseline %.3f",
			bm25.Hard.RecallAt5, baseline.Hard.RecallAt5)
	}
	if bm25.Hard.RecallAt5 < 0.35 {
		t.Fatalf("hard: bm25 recall@5 = %.3f, want >= 0.35", bm25.Hard.RecallAt5)
	}
	// hard 必须真的更难：词法检索在无实体名、且有近重复句时应该明显掉分。
	if bm25.Hard.RecallAt5 >= bm25.Easy.RecallAt5 {
		t.Fatalf("hard bm25 recall %.3f is not below easy %.3f; the hard subset is not hard",
			bm25.Hard.RecallAt5, bm25.Easy.RecallAt5)
	}
	if bm25.Hard.MRR >= bm25.Easy.MRR {
		t.Fatalf("hard MRR %.3f not below easy MRR %.3f; the hard subset is not exposing the ceiling",
			bm25.Hard.MRR, bm25.Easy.MRR)
	}
	if bm25.Hard.RecallAt5 > 0.95 {
		t.Fatalf("hard bm25 recall %.3f is suspiciously high; the hard subset lost its teeth",
			bm25.Hard.RecallAt5)
	}

	// 混合臂是管线演示：不能比 baseline 更差。
	if hybrid.Easy.RecallAt5 < baseline.Easy.RecallAt5 || hybrid.Hard.RecallAt5 < baseline.Hard.RecallAt5 {
		t.Fatalf("hybrid below baseline: easy %.3f vs %.3f, hard %.3f vs %.3f",
			hybrid.Easy.RecallAt5, baseline.Easy.RecallAt5,
			hybrid.Hard.RecallAt5, baseline.Hard.RecallAt5)
	}
	// 分层注入只改变注入成本，不改变排名。
	if layered.Easy.RecallAt5 != hybrid.Easy.RecallAt5 || layered.Hard.RecallAt5 != hybrid.Hard.RecallAt5 {
		t.Fatalf("layered recall differs from hybrid: easy %.3f/%.3f hard %.3f/%.3f",
			layered.Easy.RecallAt5, hybrid.Easy.RecallAt5,
			layered.Hard.RecallAt5, hybrid.Hard.RecallAt5)
	}
	if baseline.Easy.AvgInjectedTokens <= 0 || bm25.Hard.AvgInjectedTokens <= 0 {
		t.Fatalf("token metrics must be computed: %+v", report.Arms)
	}
	if bm25.Easy.P50LatencyMS <= 0 || bm25.Easy.P95LatencyMS < bm25.Easy.P50LatencyMS {
		t.Fatalf("latency metrics look wrong: p50=%.4f p95=%.4f",
			bm25.Easy.P50LatencyMS, bm25.Easy.P95LatencyMS)
	}
	if report.Index.WriteBytes <= 0 || report.Index.Docs != 780 {
		t.Fatalf("index info not populated: %+v", report.Index)
	}
	if report.Index.BM25BuildMS <= 0 {
		t.Fatalf("index build time not measured: %+v", report.Index)
	}

	// 分层 token 曲线：两个子集各 3 行；层数越多注入越多，召回来自同一份排名。
	if len(report.LayerCurve) != 6 {
		t.Fatalf("layer curve has %d rows, want 6 (2 subsets x 3 levels)", len(report.LayerCurve))
	}
	for _, subset := range []string{SubsetEasy, SubsetHard} {
		var points []LayerPoint
		for _, point := range report.LayerCurve {
			if point.Subset == subset {
				points = append(points, point)
			}
		}
		if len(points) != 3 {
			t.Fatalf("subset %s has %d layer points, want 3", subset, len(points))
		}
		for idx := 1; idx < len(points); idx++ {
			if points[idx].AvgInjectedTokens <= points[idx-1].AvgInjectedTokens {
				t.Fatalf("layer tokens not increasing for %s: %+v", subset, points)
			}
			if points[idx].RecallAt5 != points[0].RecallAt5 {
				t.Fatalf("layer recall changed by level for %s: %+v", subset, points)
			}
		}
	}

	table := FormatTable(report)
	for _, arm := range report.Arms {
		if !strings.Contains(table, arm.Name) {
			t.Fatalf("table missing arm %q:\n%s", arm.Name, table)
		}
	}
	for _, column := range []string{"recall@5", "MRR", "tokens", "p50", "p95", "index build", "write bytes"} {
		if !strings.Contains(table, column) {
			t.Fatalf("table missing column %q:\n%s", column, table)
		}
	}
	for _, section := range []string{"easy 集", "hard 集", "数据集与索引"} {
		if !strings.Contains(table, section) {
			t.Fatalf("table missing section %q:\n%s", section, table)
		}
	}
	tableRows := 0
	for _, line := range strings.Split(table, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") {
			tableRows++
		}
	}
	if tableRows < 20 {
		t.Fatalf("expected several Markdown tables, found %d rows:\n%s", tableRows, table)
	}
	joined := strings.Join(report.Notes, " ")
	for _, must := range []string{"hash", "Embedder", "Reranker", "easy", "hard"} {
		if !strings.Contains(joined, must) {
			t.Fatalf("report notes missing %q: %v", must, report.Notes)
		}
	}

	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var decoded Report
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(decoded.Arms) != len(report.Arms) || decoded.Index.WriteBytes != report.Index.WriteBytes {
		t.Fatalf("round-trip lost data")
	}

	// 只有计时字段随机器负载浮动，其余内容必须逐字节可复现；比较前把计时清零
	// （不写回报告，报告里的延迟/构建耗时是真实测量值）。
	stripTiming := func(input Report) Report {
		out := input
		out.Arms = append([]Arm(nil), input.Arms...)
		for idx := range out.Arms {
			out.Arms[idx].BuildMS = 0
			out.Arms[idx].All.P50LatencyMS, out.Arms[idx].All.P95LatencyMS = 0, 0
			out.Arms[idx].Easy.P50LatencyMS, out.Arms[idx].Easy.P95LatencyMS = 0, 0
			out.Arms[idx].Hard.P50LatencyMS, out.Arms[idx].Hard.P95LatencyMS = 0, 0
		}
		out.Index.BaselineBuildMS, out.Index.BM25BuildMS, out.Index.HybridBuildMS = 0, 0, 0
		out.LayerCurve = append([]LayerPoint(nil), input.LayerCurve...)
		return out
	}
	if string(mustMarshal(t, stripTiming(Run(42)))) != string(mustMarshal(t, stripTiming(report))) {
		t.Fatalf("Run is not reproducible for the same seed")
	}

	t.Logf("\n%s", table)
}

func TestRunIsStableAcrossSeeds(t *testing.T) {
	for _, seed := range []int64{1, 2, 99} {
		report := Run(seed)
		bm25 := armByName(t, report, "bm25")
		baseline := armByName(t, report, "baseline-wiki-substring")
		if report.Seed != seed {
			t.Fatalf("report seed = %d, want %d", report.Seed, seed)
		}
		if bm25.Easy.RecallAt5 < 0.6 || bm25.Easy.RecallAt5 <= baseline.Easy.RecallAt5 {
			t.Fatalf("seed %d easy: bm25 %.3f vs baseline %.3f",
				seed, bm25.Easy.RecallAt5, baseline.Easy.RecallAt5)
		}
		if bm25.Hard.RecallAt5 < 0.35 || bm25.Hard.RecallAt5 < 1.5*baseline.Hard.RecallAt5 {
			t.Fatalf("seed %d hard: bm25 %.3f vs baseline %.3f",
				seed, bm25.Hard.RecallAt5, baseline.Hard.RecallAt5)
		}
		if bm25.Hard.RecallAt5 >= bm25.Easy.RecallAt5 {
			t.Fatalf("seed %d: hard %.3f not below easy %.3f",
				seed, bm25.Hard.RecallAt5, bm25.Easy.RecallAt5)
		}
	}
}

func TestFormatTableIncludesHeaderAndNotes(t *testing.T) {
	report := Run(3)
	table := FormatTable(report)
	var header, separator string
	lines := strings.Split(strings.TrimSpace(table), "\n")
	for idx, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "|") && header == "" {
			header = line
			if idx+1 < len(lines) {
				separator = lines[idx+1]
			}
			break
		}
	}
	if header == "" || !strings.Contains(separator, "---") {
		t.Fatalf("missing markdown header/separator:\n%s", table)
	}
	if !strings.Contains(table, "seed=3") {
		t.Fatalf("table should state the seed:\n%s", table)
	}
	if !strings.Contains(table, "需要宿主提供 Embedder") {
		t.Fatalf("table should state the semantic ceiling:\n%s", table)
	}
}

func TestPercentile(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(values, 50); got != 5 {
		t.Fatalf("p50 = %v, want 5", got)
	}
	if got := percentile(values, 95); got != 10 {
		t.Fatalf("p95 = %v, want 10", got)
	}
	if got := percentile(nil, 95); got != 0 {
		t.Fatalf("empty percentile = %v, want 0", got)
	}
}

func mustURI(t *testing.T, raw string) memoryvfs.URI {
	t.Helper()
	uri, err := memoryvfs.ParseURI(raw)
	if err != nil {
		t.Fatalf("ParseURI(%q): %v", raw, err)
	}
	return uri
}

func mustMarshal(t *testing.T, report Report) []byte {
	t.Helper()
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return payload
}
