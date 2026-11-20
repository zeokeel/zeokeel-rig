// Package memextract 提供确定性（不联网）的记忆抽取、摘要、相似度合并与规则编译。
//
// 它实现 memoryvfs.Extractor / Summarizer 需要的算法，但默认实现刻意保持保守：
// 只从"用户明确表达"的句子里抽候选，宁可漏记也不把整段对话灌进记忆。代码块、
// 链接列表、system/tool 消息一律跳过——它们几乎从不包含"用户希望被长期记住"
// 的事实，却会把记忆刷满。想要更强的抽取，宿主注入自己的 memoryvfs.Extractor
// （例如用 rig.Provider 调模型）。
package memextract

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig/internal/memsearch"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// 合并分档：相似度 ≥ 0.90 视为重复（保留已有内容），≥ 0.65 视为同一主题（追加
// 内容、保留较长摘要），其余不合并。
const (
	duplicateThreshold = 0.90
	mergeThreshold     = 0.65
)

// Extractor 是默认的启发式抽取器。
type Extractor struct {
	// MaxCandidates 限制一次提交最多抽多少条（默认 20）。
	MaxCandidates int
}

// New 返回默认抽取器。
func New() *Extractor { return &Extractor{MaxCandidates: 20} }

// Extract 实现 memoryvfs.Extractor。
//
// 只从 user/assistant 消息里抽"用户明确表达"的句子：profile、preferences、
// entities、events、cases、trajectories、experiences。system/tool/function 消息、
// 代码块与链接列表一律跳过；同一句在本次提交里只出现一次。
func (e *Extractor) Extract(ctx context.Context, session memoryvfs.Session) ([]memoryvfs.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := e.MaxCandidates
	if limit <= 0 {
		limit = 20
	}
	seen := make(map[string]struct{})
	var candidates []memoryvfs.Candidate
	for _, message := range session.Messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !extractableRole(message.Role) {
			continue
		}
		for _, line := range splitSentences(stripFencedBlocks(message.Content)) {
			if looksLikeCode(line) {
				continue
			}
			sentence := strings.TrimSpace(strings.TrimLeft(line, bulletCutset))
			if utf8.RuneCountInString(sentence) < 8 || len(sentence) > 600 {
				continue
			}
			if isURLList(sentence) {
				continue
			}
			kind, memType := classify(sentence)
			if kind == "" {
				continue
			}
			key := memType + "|" + strings.ToLower(strings.Join(memsearch.Tokenize(sentence), " "))
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			summary, _ := memoryvfs.TruncateBytes(sentence, 200)
			candidates = append(candidates, memoryvfs.Candidate{
				Kind:       kind,
				Type:       memType,
				Title:      titleFor(sentence),
				Summary:    summary,
				Content:    sentence,
				Source:     "session:" + session.ID,
				Confidence: confidenceFor(memType),
			})
			if len(candidates) >= limit {
				return candidates, nil
			}
		}
	}
	return candidates, nil
}

// extractableRole 报告该角色是否可能包含可抽取的记忆。用白名单而不是黑名单：
// 新出现的工具角色（tool/function/developer/tool_result…）默认不抽取。
func extractableRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "", "user", "human", "assistant", "model":
		return true
	default:
		return false
	}
}

func confidenceFor(memType string) float64 {
	switch memType {
	case memoryvfs.TypePreferences, memoryvfs.TypeProfile:
		return 0.8
	case memoryvfs.TypeEvents, memoryvfs.TypeEntities:
		return 0.7
	default:
		return 0.6
	}
}

// classify 用双语线索判断句子属于哪类记忆；返回空 kind 表示不抽取。
//
// 顺序即优先级：越"用户自述"的类别越先判定，避免 "我更喜欢先跑测试" 被抽成
// trajectory。
func classify(sentence string) (memoryvfs.Kind, string) {
	lower := strings.ToLower(sentence)
	switch {
	case containsAny(lower, "i am ", "i'm ", "my role", "my name is", "我是", "我的角色", "我的名字", "叫我"):
		return memoryvfs.KindMemory, memoryvfs.TypeProfile
	case containsAny(lower, "prefer", "i like", "i use", "always ", "never ", "don't ", "do not ",
		"喜欢", "偏好", "习惯", "倾向于", "以后都", "别再", "不要", "请记住", "记住"):
		return memoryvfs.KindMemory, memoryvfs.TypePreferences
	case containsAny(lower, "step 1", "step one", "first step", "then run", "after that",
		"workflow", "procedure", "playbook", "runbook", "流程", "步骤", "然后", "接着", "依次"):
		return memoryvfs.KindMemory, memoryvfs.TypeTrajectories
	case containsAny(lower, "decided", "decide to", "we chose", "migrated to", "moved to",
		"决定", "选择", "定下"):
		return memoryvfs.KindMemory, memoryvfs.TypeEvents
	case containsAny(lower, "launched", "released", "shipped", "上线", "发布", "交付"):
		return memoryvfs.KindMemory, memoryvfs.TypeEvents
	case containsAny(lower, "fixed", "resolved", "implemented", "root cause", "修复", "解决", "实现", "完成"):
		return memoryvfs.KindMemory, memoryvfs.TypeCases
	case containsAny(lower, "learned", "lesson", "next time", "takeaway", "教训", "经验", "下次", "心得"):
		return memoryvfs.KindMemory, memoryvfs.TypeExperiences
	case containsAny(lower, "project ", "项目", "repo ", "仓库", "service ", "服务", "database", "数据库"):
		return memoryvfs.KindMemory, memoryvfs.TypeEntities
	default:
		return "", ""
	}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func titleFor(sentence string) string {
	title, _ := memoryvfs.TruncateBytes(sentence, 60)
	return strings.TrimSpace(title)
}

// bulletCutset 是句首列表符号；抽取前剥掉，避免 "- I prefer X" 被当成正文的一部分。
const bulletCutset = "-*+•·0123456789.)、 "

// splitSentences 按中英文句末标点与换行切句，保留标点。
//
// ASCII 句末标点只在后面跟着空白/引号/括号/文本结尾时才算断句：否则
// `example.com`、`v1.2`、`3.5s` 会被切碎，URL 与版本号就没法识别了。
func splitSentences(text string) []string {
	runes := []rune(text)
	var out []string
	var builder strings.Builder
	flush := func() {
		if sentence := strings.TrimSpace(builder.String()); sentence != "" {
			out = append(out, sentence)
		}
		builder.Reset()
	}
	for i, r := range runes {
		builder.WriteRune(r)
		boundary := false
		switch r {
		case '。', '！', '？', '；', '\n':
			boundary = true
		case '.', '!', '?', ';':
			if i+1 >= len(runes) {
				boundary = true
				break
			}
			next := runes[i+1]
			boundary = unicode.IsSpace(next) || next == '"' || next == '\'' ||
				next == ')' || next == ']' || next == '。' || next == '，'
		}
		if boundary {
			flush()
		}
	}
	flush()
	return out
}

// 围栏代码块：``` 或 ~~~ 开始到同种围栏结束（未闭合时吃到文本末尾）。
var fencedBlockPattern = regexp.MustCompile("(?s)(?:^|\n)(?:```|~~~)[^\n]*\n.*?(?:\n(?:```|~~~)[^\n]*(?:\n|$)|$)")

// stripFencedBlocks 删除整段围栏代码块（含未闭合的块）。
func stripFencedBlocks(text string) string {
	if !strings.Contains(text, "```") && !strings.Contains(text, "~~~") {
		return text
	}
	return fencedBlockPattern.ReplaceAllString(text, "\n")
}

// URL 线索：http(s):// 或 www. 开头的连续片段。
var urlPattern = regexp.MustCompile(`(?i)(?:https?://|www\.)[^\s<>()\[\]"]+`)

// isURLList 判断一句是否只是"链接列表"：剥掉链接与列表符号后几乎没有实义文字。
// 链接列表不是记忆——它既没有主张，也没有可检索的语义。
func isURLList(sentence string) bool {
	matches := urlPattern.FindAllString(sentence, -1)
	if len(matches) == 0 {
		return false
	}
	stripped := urlPattern.ReplaceAllString(sentence, " ")
	stripped = strings.TrimSpace(strings.TrimLeft(stripped, bulletCutset))
	stripped = strings.Trim(stripped, "()-[]{}<>,;:。，、 ")
	return utf8.RuneCountInString(stripped) < 12
}

// looksLikeCode 判断一行是否代码/日志，而不是自然语言陈述。宁可漏抽，也不要把
// 代码块写进记忆。
func looksLikeCode(line string) bool {
	if strings.Contains(line, "```") || strings.Contains(line, "~~~") {
		return true
	}
	if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
		return true
	}
	trimmed := strings.TrimSpace(strings.TrimLeft(line, bulletCutset))
	if trimmed == "" {
		return true
	}
	if strings.HasPrefix(trimmed, "#!") || strings.HasPrefix(trimmed, "$ ") {
		return true
	}
	lower := strings.ToLower(trimmed)
	for _, marker := range []string{"func ", "func(", "def ", "class ", "import (", "package ",
		"=>", ":=", "();", "}{", "/*", "*/", "<?xml", "<html"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// Summarize 生成确定性的 L0/L1：L0 取首句（≤200 字节），L1 取标题层级 + 首段。
func Summarize(entry memoryvfs.Entry) (string, string) {
	summary := strings.TrimSpace(entry.Summary)
	content := entry.Content
	if summary == "" {
		summary = firstSentence(content)
	}
	if summary == "" {
		summary = strings.TrimSpace(entry.Title)
	}
	summary, _ = memoryvfs.TruncateBytes(memoryvfs.CollapseWhitespace(summary), 200)
	overview := strings.TrimSpace(entry.Overview)
	if overview == "" {
		overview = buildOverview(entry.Title, content)
	}
	overview, _ = memoryvfs.TruncateBytes(overview, 4000)
	return summary, overview
}

func firstSentence(text string) string {
	for _, sentence := range splitSentences(text) {
		if strings.TrimSpace(sentence) != "" {
			return strings.TrimSpace(sentence)
		}
	}
	return ""
}

func buildOverview(title, content string) string {
	var builder strings.Builder
	if title != "" {
		builder.WriteString("# ")
		builder.WriteString(title)
		builder.WriteString("\n\n")
	}
	lines := strings.Split(content, "\n")
	headings := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			builder.WriteString(trimmed)
			builder.WriteString("\n")
			headings++
			if headings >= 12 {
				break
			}
		}
	}
	if headings == 0 {
		first := firstSentence(content)
		if first != "" {
			builder.WriteString(first)
			builder.WriteString("\n")
		}
	}
	return strings.TrimSpace(builder.String())
}

// Similarity 返回两段文本的 token Jaccard 相似度（0..1）。
func Similarity(a, b string) float64 {
	tokensA := memsearch.Tokenize(a)
	tokensB := memsearch.Tokenize(b)
	if len(tokensA) == 0 || len(tokensB) == 0 {
		return 0
	}
	setA := make(map[string]struct{}, len(tokensA))
	for _, token := range tokensA {
		setA[token] = struct{}{}
	}
	setB := make(map[string]struct{}, len(tokensB))
	for _, token := range tokensB {
		setB[token] = struct{}{}
	}
	intersection := 0
	for token := range setA {
		if _, ok := setB[token]; ok {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// MergeOutcome 描述一次候选与既有条目的合并判断。
type MergeOutcome struct {
	Merge    bool
	Append   bool
	Entry    memoryvfs.Entry
	Reason   string
	Distance float64
}

// Merge 判断候选是否应合并进已有条目：
//   - 类型不同（两者都非空）：不合并；
//   - 相似度 ≥ 0.90：视为重复，保留已有内容（Merge=true，Append=false）；
//   - 0.65 ≤ 相似度 < 0.90：合并（追加内容、保留较长摘要、合并 Tags/Links），
//     Append=true；
//   - 其余：不合并。
//
// 合并结果把 Updated 留成零值：函数内不读时钟，时间由调用方（Store 的
// NormalizeEntry）统一填，这样同一次提交里的多条写入拿到同一个时间戳，测试也
// 不受时钟影响。
func Merge(existing memoryvfs.Entry, candidate memoryvfs.Candidate) MergeOutcome {
	outcome := MergeOutcome{Distance: 1, Entry: existing.Clone()}
	// 时间由调用方（Store.NormalizeEntry）统一填：同一次提交里的多条写入拿到
	// 同一个时间戳，测试也不受时钟影响。
	outcome.Entry.Updated = time.Time{}
	if existing.Type != "" && candidate.Type != "" && existing.Type != candidate.Type {
		outcome.Reason = fmt.Sprintf("type mismatch: %q vs %q", existing.Type, candidate.Type)
		return outcome
	}
	similarity := Similarity(existing.Summary+"\n"+existing.Content, candidate.Summary+"\n"+candidate.Content)
	outcome.Distance = 1 - similarity
	if similarity < mergeThreshold {
		outcome.Reason = fmt.Sprintf("similarity %.2f below %.2f", similarity, mergeThreshold)
		return outcome
	}
	outcome.Merge = true
	if similarity >= duplicateThreshold {
		outcome.Reason = fmt.Sprintf("duplicate (similarity %.2f)", similarity)
		return outcome
	}
	outcome.Append = true
	outcome.Reason = fmt.Sprintf("merged (similarity %.2f)", similarity)
	outcome.Entry.Content = mergeContent(existing.Content, candidate.Content)
	if len(candidate.Summary) > len(existing.Summary) {
		outcome.Entry.Summary = candidate.Summary
	}
	if strings.TrimSpace(outcome.Entry.Title) == "" {
		outcome.Entry.Title = strings.TrimSpace(candidate.Title)
	}
	if len(candidate.Tags) > 0 {
		if outcome.Entry.Tags == nil {
			outcome.Entry.Tags = make(map[string]string, len(candidate.Tags))
		}
		for key, value := range candidate.Tags {
			outcome.Entry.Tags[key] = value
		}
	}
	outcome.Entry.Links = mergeLinks(existing.Links, candidate.Links)
	return outcome
}

// mergeContent 追加候选正文，但同一段内容只出现一次（空白差异视为相同）。
func mergeContent(existing, addition string) string {
	addition = strings.TrimSpace(addition)
	if addition == "" {
		return existing
	}
	existing = strings.TrimRight(existing, "\n")
	if existing == "" {
		return addition
	}
	if strings.Contains(memoryvfs.CollapseWhitespace(existing), memoryvfs.CollapseWhitespace(addition)) {
		return existing
	}
	return existing + "\n\n" + addition
}

// mergeLinks 合并链接并去重（保留既有顺序，再按候选顺序追加新链接）。
func mergeLinks(existing, added []memoryvfs.URI) []memoryvfs.URI {
	if len(existing) == 0 && len(added) == 0 {
		return nil
	}
	out := make([]memoryvfs.URI, 0, len(existing)+len(added))
	seen := make(map[memoryvfs.URI]struct{}, len(existing)+len(added))
	for _, list := range [][]memoryvfs.URI{existing, added} {
		for _, link := range list {
			if _, ok := seen[link]; ok {
				continue
			}
			seen[link] = struct{}{}
			out = append(out, link)
		}
	}
	return out
}

// RuleDraft 是编译出的单条规则。
type RuleDraft struct {
	Scenario string
	Content  string
}

// CompileRules 把指令与来源文本拆成规则草稿：按行切分，剥掉项目符号（`-`/`*`/
// `+`/`•`/`1.`/`1)`/`1、`），去掉空行与纯标题；每段至少 4 个字符。来源文本整体
// 作为一条（≤2000 字节）规则，场景标为 "source"。
func CompileRules(instruction string, sourceTexts []string) []RuleDraft {
	var drafts []RuleDraft
	for _, line := range strings.Split(instruction, "\n") {
		trimmed := stripBullet(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if utf8.RuneCountInString(trimmed) < 4 {
			continue
		}
		content, _ := memoryvfs.TruncateBytes(trimmed, 1000)
		drafts = append(drafts, RuleDraft{Scenario: scenarioFor(trimmed), Content: content})
	}
	for _, source := range sourceTexts {
		trimmed := strings.TrimSpace(source)
		if trimmed == "" {
			continue
		}
		content, _ := memoryvfs.TruncateBytes(trimmed, 2000)
		drafts = append(drafts, RuleDraft{Scenario: "source", Content: content})
	}
	return drafts
}

// stripBullet 去掉行首的列表符号与序号，返回规则正文。
func stripBullet(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimSpace(strings.TrimLeft(trimmed, "-*+•·"))
	runes := []rune(trimmed)
	digits := 0
	for digits < len(runes) && runes[digits] >= '0' && runes[digits] <= '9' {
		digits++
	}
	if digits > 0 && digits < len(runes) {
		switch runes[digits] {
		case '.', ')', '、':
			trimmed = strings.TrimSpace(string(runes[digits+1:]))
		}
	}
	return strings.TrimSpace(trimmed)
}

func scenarioFor(text string) string {
	tokens := memsearch.Tokenize(text)
	if len(tokens) > 6 {
		tokens = tokens[:6]
	}
	if len(tokens) == 0 {
		return "general"
	}
	return strings.Join(tokens, "-")
}

// ScoreRule 返回规则与任务上下文的相关度（0..1）：token 覆盖率 + 强约束词加成。
// 强约束词只在实际命中任务 token 时加成——否则任何带 "must" 的无关规则都会
// 越过召回阈值。
func ScoreRule(task, content string) float64 {
	taskTokens := memsearch.Tokenize(task)
	if len(taskTokens) == 0 {
		return 0
	}
	ruleTokens := make(map[string]struct{})
	for _, token := range memsearch.Tokenize(content) {
		ruleTokens[token] = struct{}{}
	}
	covered := 0
	for _, token := range taskTokens {
		if _, ok := ruleTokens[token]; ok {
			covered++
		}
	}
	score := float64(covered) / float64(len(taskTokens))
	lower := strings.ToLower(content)
	if covered > 0 && (strings.Contains(lower, "must") || strings.Contains(lower, "never") ||
		strings.Contains(lower, "禁止") || strings.Contains(lower, "必须")) {
		score += 0.15
	}
	if score > 1 {
		score = 1
	}
	return score
}

// SortRules 按概率降序、URI 升序排序。
func SortRules(rules []memoryvfs.Rule) {
	sort.SliceStable(rules, func(a, b int) bool {
		if rules[a].Probability != rules[b].Probability {
			return rules[a].Probability > rules[b].Probability
		}
		return rules[a].URI < rules[b].URI
	})
}
