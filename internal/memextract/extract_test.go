package memextract

// 抽取/合并/规则三个算法的确定性用例。
//
// 抽取的重点是"宁可漏记"：双语七类要能分对，system/tool 消息、代码块与链接列表
// 必须一条都不抽。合并的重点是分档与幂等：重复不重复追加、Tags/Links 不丢、时间
// 不在这里读时钟。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func userSession(text string) memoryvfs.Session {
	return memoryvfs.Session{ID: "s1", Messages: []memoryvfs.Message{{Role: "user", Content: text}}}
}

// TestExtractClassifiesBilingualTypes 断言七类记忆（含中英文）都能分对。
func TestExtractClassifiesBilingualTypes(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"profile-en", "I am a backend engineer who maintains the payments API.", memoryvfs.TypeProfile},
		{"profile-zh", "我是负责支付系统的后端工程师。", memoryvfs.TypeProfile},
		{"preferences-en", "I prefer running the tests before every commit.", memoryvfs.TypePreferences},
		{"preferences-zh", "我习惯把日志按天切分。", memoryvfs.TypePreferences},
		{"entities-en", "The billing service stores invoices in Postgres.", memoryvfs.TypeEntities},
		{"entities-zh", "账单服务把发票存在 Postgres 里。", memoryvfs.TypeEntities},
		{"events-en", "We decided to move the cache to SQLite last week.", memoryvfs.TypeEvents},
		{"events-zh", "我们决定把缓存迁到 SQLite。", memoryvfs.TypeEvents},
		{"cases-en", "We fixed the flaky upload test by adding a retry.", memoryvfs.TypeCases},
		{"cases-zh", "我们修复了上传测试的偶发失败。", memoryvfs.TypeCases},
		{"trajectories-en", "The release workflow is step 1 run tests and step 2 tag the build.", memoryvfs.TypeTrajectories},
		{"trajectories-zh", "发布流程是：先跑测试，然后打标签。", memoryvfs.TypeTrajectories},
		{"experiences-en", "The lesson from the outage was to keep a rollback ready.", memoryvfs.TypeExperiences},
		{"experiences-zh", "这次故障的教训是要留好回滚方案。", memoryvfs.TypeExperiences},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := New().Extract(context.Background(), userSession(tc.text))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("抽到 %d 条候选，want 1: %+v", len(got), got)
			}
			candidate := got[0]
			if candidate.Kind != memoryvfs.KindMemory {
				t.Fatalf("Kind = %q", candidate.Kind)
			}
			if candidate.Type != tc.want {
				t.Fatalf("Type = %q, want %q（sentence = %q）", candidate.Type, tc.want, candidate.Summary)
			}
			if candidate.Summary == "" || candidate.Title == "" || candidate.Content == "" {
				t.Fatalf("候选字段不完整: %+v", candidate)
			}
			if candidate.Source != "session:s1" {
				t.Fatalf("Source = %q", candidate.Source)
			}
			if candidate.Confidence <= 0 || candidate.Confidence > 1 {
				t.Fatalf("Confidence = %v", candidate.Confidence)
			}
		})
	}
}

// TestExtractNeverExtractsNoise 断言 system/tool 消息、代码块与链接列表都不产生候选。
func TestExtractNeverExtractsNoise(t *testing.T) {
	noise := []struct {
		name    string
		message memoryvfs.Message
	}{
		{"system", memoryvfs.Message{Role: "system", Content: "I prefer concise answers. We decided to ship on Friday."}},
		{"tool", memoryvfs.Message{Role: "tool", Content: "We fixed the bug in build.go."}},
		{"function", memoryvfs.Message{Role: "function", Content: "I use Postgres for the billing service."}},
		{"developer", memoryvfs.Message{Role: "developer", Content: "Always run the full test suite."}},
		{"fenced-code", memoryvfs.Message{Role: "user", Content: "Here is the patch:\n```go\nfunc main() {\n    // I prefer this style\n    logger.Info(\"we fixed it\")\n}\n```\nThat is all."}},
		{"unclosed-fence", memoryvfs.Message{Role: "user", Content: "```\nI prefer tabs over spaces in Go files.\n"}},
		{"indented-code", memoryvfs.Message{Role: "user", Content: "    func main() {}\n\tgo test ./...\n"}},
		{"url-list", memoryvfs.Message{Role: "user", Content: "- https://example.com/a\n- https://example.com/b\n- www.example.org/c\n"}},
		{"url-only", memoryvfs.Message{Role: "user", Content: "https://example.com/docs/reference"}},
		{"shell", memoryvfs.Message{Role: "user", Content: "$ go test ./...\n# output\nok  pkg 1.2s\n"}},
	}
	for _, tc := range noise {
		t.Run(tc.name, func(t *testing.T) {
			got, err := New().Extract(context.Background(), memoryvfs.Session{ID: "s", Messages: []memoryvfs.Message{tc.message}})
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("噪声消息被抽成记忆: %+v", got)
			}
		})
	}
}

// TestExtractKeepsProseBesideNoise 断言过滤不会误伤同一消息里的自然语言。
func TestExtractKeepsProseBesideNoise(t *testing.T) {
	content := "I prefer tabs over spaces in Go files.\n```go\nfunc main() {}\n```\nSee https://example.com/a\n"
	got, err := New().Extract(context.Background(), userSession(content))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 || got[0].Type != memoryvfs.TypePreferences {
		t.Fatalf("候选 = %+v, want 一条 preferences", got)
	}
}

// TestExtractDedupesAndCaps 断言同一句只抽一次，且 MaxCandidates 生效。
func TestExtractDedupesAndCaps(t *testing.T) {
	same := "I prefer running the full test suite before every commit."
	got, err := New().Extract(context.Background(), memoryvfs.Session{ID: "s", Messages: []memoryvfs.Message{
		{Role: "user", Content: same},
		{Role: "assistant", Content: same},
	}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("重复句抽到 %d 条, want 1", len(got))
	}

	limited := &Extractor{MaxCandidates: 2}
	many := "I prefer running the full test suite. I prefer short commits. I prefer rebasing before merge."
	got, err = limited.Extract(context.Background(), userSession(many))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("MaxCandidates=2 抽到 %d 条", len(got))
	}

	if got, err := (&Extractor{}).Extract(context.Background(), userSession(many)); err != nil || len(got) != 3 {
		t.Fatalf("默认限制下抽到 %d 条（err=%v）, want 3", len(got), err)
	}
}

// TestExtractDefaultCapIsTwenty 断言零值抽取器的默认上限是 20 条。
func TestExtractDefaultCapIsTwenty(t *testing.T) {
	var builder strings.Builder
	for i := 0; i < 30; i++ {
		builder.WriteString("I prefer pipeline option number ")
		builder.WriteString(strings.Repeat("x", i+1))
		builder.WriteString(" for the build.\n")
	}
	got, err := New().Extract(context.Background(), userSession(builder.String()))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("默认上限抽到 %d 条, want 20", len(got))
	}
}

// TestExtractRespectsCanceledContext 断言取消会被及时看到。
func TestExtractRespectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Extract(ctx, userSession("I prefer short commits.")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestSplitSentences 断言中英文断句与 URL/版本号不误切。
func TestSplitSentences(t *testing.T) {
	got := splitSentences("第一句。第二句！Third sentence. Fourth one! Check https://example.com/a. Then run v1.2 for 3.5s")
	want := []string{"第一句。", "第二句！", "Third sentence.", "Fourth one!", "Check https://example.com/a.", "Then run v1.2 for 3.5s"}
	if len(got) != len(want) {
		t.Fatalf("splitSentences = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %q, want %q（全量 %#v）", i, got[i], want[i], got)
		}
	}
	if got := splitSentences("   \n\n  "); len(got) != 0 {
		t.Fatalf("空白文本切出 %#v", got)
	}
}

// TestSummarizeDeterministic 断言摘要/概览的确定性与回退顺序。
func TestSummarizeDeterministic(t *testing.T) {
	entry := memoryvfs.Entry{
		Title:   "Build notes",
		Content: "# Goal\n\nKeep the build fast.\n\n## Steps\n\n- cache modules\n",
	}
	summary, overview := Summarize(entry)
	if summary != "# Goal" {
		t.Fatalf("summary = %q, want 首句", summary)
	}
	if !strings.Contains(overview, "# Build notes") || !strings.Contains(overview, "## Steps") {
		t.Fatalf("overview = %q", overview)
	}

	// 明确摘要优先，并折叠成一行。
	summary, _ = Summarize(memoryvfs.Entry{Summary: "  one\n two  ", Content: "ignored"})
	if summary != "one two" {
		t.Fatalf("summary = %q", summary)
	}

	// 没有摘要与正文时回退到标题；全都空则空。
	if summary, overview := Summarize(memoryvfs.Entry{Title: "Only title"}); summary != "Only title" || overview != "# Only title" {
		t.Fatalf("title fallback = (%q, %q)", summary, overview)
	}
	if summary, overview := Summarize(memoryvfs.Entry{}); summary != "" || overview != "" {
		t.Fatalf("empty summary = (%q, %q)", summary, overview)
	}

	// 超长摘要按字节截断且不切坏 UTF-8。
	long := strings.Repeat("长", 300)
	summary, _ = Summarize(memoryvfs.Entry{Summary: long})
	if len(summary) > 200 || !strings.HasPrefix(long, summary) {
		t.Fatalf("长摘要截断 = %d 字节", len(summary))
	}
}

// TestBuildOverview 断言标题 + 最多 12 个标题；无标题时取首句。
func TestBuildOverview(t *testing.T) {
	var builder strings.Builder
	for i := 0; i < 20; i++ {
		builder.WriteString("# heading\n")
	}
	overview := buildOverview("T", builder.String())
	if strings.Count(overview, "# heading") != 12 {
		t.Fatalf("overview 收进了 %d 个标题, want 12", strings.Count(overview, "# heading"))
	}
	if got := buildOverview("T", "first line only"); got != "# T\n\nfirst line only" {
		t.Fatalf("无标题 overview = %q", got)
	}
	if got := buildOverview("", ""); got != "" {
		t.Fatalf("空 overview = %q", got)
	}
}

// TestSimilarityBounds 断言 Jaccard 的边界与对称性。
func TestSimilarityBounds(t *testing.T) {
	if got := Similarity("the same words here", "the same words here"); got != 1 {
		t.Fatalf("相同文本 = %v, want 1", got)
	}
	if got := Similarity("alpha beta gamma", "delta epsilon zeta"); got != 0 {
		t.Fatalf("无关文本 = %v, want 0", got)
	}
	if got := Similarity("", "anything"); got != 0 {
		t.Fatalf("空文本 = %v, want 0", got)
	}
	a, b := "Rig memory keeps a summary layer", "Rig memory keeps a summary index"
	if Similarity(a, b) != Similarity(b, a) {
		t.Fatalf("相似度不对称")
	}
}

// TestMergeTiers 断言三档合并：重复/合并/不合并。
func TestMergeTiers(t *testing.T) {
	base := memoryvfs.Entry{
		URI:     "mem://user/self/memories/preferences/build",
		Kind:    memoryvfs.KindMemory,
		Type:    memoryvfs.TypePreferences,
		Summary: "the build uses esbuild for bundling frontend assets",
		Tags:    map[string]string{"priority": "high"},
		Updated: time.Now().UTC(),
	}

	t.Run("duplicate", func(t *testing.T) {
		outcome := Merge(base, memoryvfs.Candidate{
			Type:    memoryvfs.TypePreferences,
			Summary: base.Summary,
		})
		if !outcome.Merge || outcome.Append {
			t.Fatalf("重复档 = %+v", outcome)
		}
		if outcome.Distance != 0 {
			t.Fatalf("Distance = %v, want 0", outcome.Distance)
		}
		if !outcome.Entry.Updated.IsZero() {
			t.Fatalf("Merge 不应设置 Updated（由调用方填）: %v", outcome.Entry.Updated)
		}
	})

	t.Run("merge-band", func(t *testing.T) {
		candidate := memoryvfs.Candidate{
			Type:    memoryvfs.TypePreferences,
			Summary: "the build uses esbuild for bundling backend assets today",
			Content: "shared body text extra note",
		}
		existing := base
		existing.Content = "shared body text"
		similarity := Similarity(existing.Summary+"\n"+existing.Content, candidate.Summary+"\n"+candidate.Content)
		if similarity < mergeThreshold || similarity >= duplicateThreshold {
			t.Fatalf("测试数据落在重复/不合并档（similarity=%.3f），需要 0.65..0.90", similarity)
		}
		outcome := Merge(existing, candidate)
		if !outcome.Merge || !outcome.Append {
			t.Fatalf("合并档 = %+v（similarity=%.3f）", outcome, similarity)
		}
		if !strings.Contains(outcome.Entry.Content, "shared body text") ||
			!strings.Contains(outcome.Entry.Content, "extra note") {
			t.Fatalf("合并正文 = %q", outcome.Entry.Content)
		}
		if outcome.Entry.Summary != candidate.Summary {
			t.Fatalf("应保留更长摘要，得到 %q", outcome.Entry.Summary)
		}
		if outcome.Entry.Tags["priority"] != "high" {
			t.Fatalf("既有 Tags 丢了: %+v", outcome.Entry.Tags)
		}
		if !outcome.Entry.Updated.IsZero() {
			t.Fatalf("Merge 不应设置 Updated: %v", outcome.Entry.Updated)
		}
	})

	t.Run("merge-dedupes-content", func(t *testing.T) {
		existing := base
		existing.Content = "shared body text"
		candidate := memoryvfs.Candidate{
			Type:    memoryvfs.TypePreferences,
			Summary: "the build uses esbuild for bundling backend assets today",
			Content: "shared  body\ntext",
		}
		outcome := Merge(existing, candidate)
		if !outcome.Merge {
			t.Fatalf("应当合并: %+v", outcome)
		}
		if outcome.Entry.Content != "shared body text" {
			t.Fatalf("重复内容被再次追加: %q", outcome.Entry.Content)
		}
	})

	t.Run("no-merge", func(t *testing.T) {
		outcome := Merge(base, memoryvfs.Candidate{
			Type:    memoryvfs.TypePreferences,
			Summary: "prefer dark mode in the terminal at night",
		})
		if outcome.Merge || outcome.Append {
			t.Fatalf("无关候选被合并: %+v", outcome)
		}
		if outcome.Distance <= mergeThreshold {
			t.Fatalf("无关候选 Distance = %v，应远离合并阈值", outcome.Distance)
		}
	})

	t.Run("type-mismatch", func(t *testing.T) {
		outcome := Merge(base, memoryvfs.Candidate{Type: memoryvfs.TypeEvents, Summary: base.Summary})
		if outcome.Merge {
			t.Fatalf("类型不同不应合并: %+v", outcome)
		}
		if !strings.Contains(outcome.Reason, "type mismatch") {
			t.Fatalf("Reason = %q", outcome.Reason)
		}
	})
}

// TestMergeUnionTagsAndLinks 断言合并会把候选的 Tags/Links 并进来且去重。
func TestMergeUnionTagsAndLinks(t *testing.T) {
	existing := memoryvfs.Entry{
		URI:     "mem://user/self/memories/entities/rig",
		Type:    memoryvfs.TypeEntities,
		Summary: "the rig project keeps memory pages under the state directory",
		Content: "shared body",
		Tags:    map[string]string{"priority": "high"},
		Links:   []memoryvfs.URI{"mem://user/self/memories/entities/one"},
	}
	candidate := memoryvfs.Candidate{
		Type:    memoryvfs.TypeEntities,
		Summary: "the rig project keeps memory pages under the state directory and caches them",
		Content: "shared body",
		Tags:    map[string]string{"owner": "team", "priority": "low"},
		Links:   []memoryvfs.URI{"mem://user/self/memories/entities/one", "mem://user/self/memories/entities/two"},
	}
	outcome := Merge(existing, candidate)
	if !outcome.Merge || !outcome.Append {
		t.Fatalf("应当合并: %+v", outcome)
	}
	if outcome.Entry.Tags["priority"] != "low" || outcome.Entry.Tags["owner"] != "team" {
		t.Fatalf("Tags = %+v", outcome.Entry.Tags)
	}
	if len(outcome.Entry.Links) != 2 {
		t.Fatalf("Links = %v（应去重成 2 条）", outcome.Entry.Links)
	}
}

// TestCompileRulesBilingualBullets 断言中英文规则、各种项目符号与来源文本。
func TestCompileRulesBilingualBullets(t *testing.T) {
	instruction := strings.Join([]string{
		"# Release rules",
		"",
		"- must run the full test suite",
		"* never tag from a dirty worktree",
		"1. 必须先在本地跑 gofmt",
		"2、禁止跳过 pre-commit 检查",
		"• keep changelog entries short",
		"+ 部署前必须备份数据库",
		"do",
		"   ",
	}, "\n")
	drafts := CompileRules(instruction, []string{"\n# source doc\nalways keep a rollback ready\n", "   "})
	if len(drafts) != 7 {
		t.Fatalf("drafts = %d, want 7: %+v", len(drafts), drafts)
	}
	wantContents := []string{
		"must run the full test suite",
		"never tag from a dirty worktree",
		"必须先在本地跑 gofmt",
		"禁止跳过 pre-commit 检查",
		"keep changelog entries short",
		"部署前必须备份数据库",
		"# source doc\nalways keep a rollback ready",
	}
	for i, want := range wantContents {
		if drafts[i].Content != want {
			t.Fatalf("drafts[%d].Content = %q, want %q", i, drafts[i].Content, want)
		}
		if drafts[i].Scenario == "" {
			t.Fatalf("drafts[%d].Scenario 为空", i)
		}
	}
	if drafts[6].Scenario != "source" {
		t.Fatalf("来源规则 Scenario = %q, want source", drafts[6].Scenario)
	}
	if got := CompileRules("", nil); len(got) != 0 {
		t.Fatalf("空输入产出 %+v", got)
	}
}

// TestCompileRulesTruncates 断言单条规则与来源文本的字节上限。
func TestCompileRulesTruncates(t *testing.T) {
	long := strings.Repeat("x", 1200)
	drafts := CompileRules("- "+long, []string{strings.Repeat("y", 3000)})
	if len(drafts) != 2 {
		t.Fatalf("drafts = %d", len(drafts))
	}
	if len(drafts[0].Content) != 1000 {
		t.Fatalf("指令规则 = %d 字节, want 1000", len(drafts[0].Content))
	}
	if len(drafts[1].Content) != 2000 {
		t.Fatalf("来源规则 = %d 字节, want 2000", len(drafts[1].Content))
	}
}

// TestStripBullet 断言各种列表符号都被剥掉，普通数字开头不被误伤。
func TestStripBullet(t *testing.T) {
	cases := map[string]string{
		"- item one":      "item one",
		"* item two":      "item two",
		"+ item three":    "item three",
		"• item four":     "item four",
		"1. item five":    "item five",
		"12) item six":    "item six",
		"3、item seven":    "item seven",
		"2 weeks of work": "2 weeks of work",
		"   ":             "",
	}
	for line, want := range cases {
		if got := stripBullet(line); got != want {
			t.Fatalf("stripBullet(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestScenarioFor 断言场景 key 稳定且不超过 6 个 token。
func TestScenarioFor(t *testing.T) {
	if got := scenarioFor(""); got != "general" {
		t.Fatalf("空场景 = %q", got)
	}
	if got := scenarioFor("one two three four five six seven eight"); strings.Count(got, "-") != 5 {
		t.Fatalf("场景 token 数 = %q", got)
	}
}

// TestScoreRuleThresholds 断言覆盖率、强约束词加成与阈值边界。
func TestScoreRuleThresholds(t *testing.T) {
	task := "run tests before commit"
	if got := ScoreRule(task, "must run tests before commit"); got != 1 {
		t.Fatalf("完全覆盖 + must = %v, want 1（加成后截断）", got)
	}
	if got := ScoreRule(task, "run tests"); got != 0.5 {
		t.Fatalf("部分覆盖 = %v, want 0.5", got)
	}
	if got := ScoreRule(task, "must never touch production"); got != 0 {
		t.Fatalf("无关规则 = %v, want 0（强约束词不应单独加分）", got)
	}
	if got := ScoreRule("部署数据库备份", "部署前必须备份数据库"); got < 0.65 {
		t.Fatalf("中文强规则 = %v, want >= 0.65", got)
	}
	if got := ScoreRule("", "must run tests"); got != 0 {
		t.Fatalf("空任务 = %v", got)
	}
	if got := ScoreRule(task, ""); got != 0 {
		t.Fatalf("空规则 = %v", got)
	}
	// 阈值边界两侧：0.5 低于常用阈值 0.65；0.75+0.15 高于它。
	if got := ScoreRule("alpha beta gamma delta", "alpha beta"); got >= 0.65 {
		t.Fatalf("半覆盖 = %v 不应越过 0.65", got)
	}
	if got := ScoreRule("alpha beta gamma delta", "alpha beta gamma must hold"); got < 0.65 {
		t.Fatalf("四分之三覆盖 + must = %v 应越过 0.65", got)
	}
}

// TestSortRules 断言概率降序、URI 升序。
func TestSortRules(t *testing.T) {
	rules := []memoryvfs.Rule{
		{URI: "mem://user/self/rules/b", Probability: 0.5},
		{URI: "mem://user/self/rules/c", Probability: 0.9},
		{URI: "mem://user/self/rules/a", Probability: 0.9},
	}
	SortRules(rules)
	if rules[0].URI != "mem://user/self/rules/a" || rules[1].URI != "mem://user/self/rules/c" || rules[2].URI != "mem://user/self/rules/b" {
		t.Fatalf("排序 = %v", rules)
	}
}
