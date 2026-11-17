package search

// 对抗审计用例：路径逃逸、符号链接、资源放大、解析边界、打分反例、输出契约。
//
// 每个用例都是"攻击者能构造出来的输入"而不是"实现细节的快照"，因此它们同时也是
// 回归测试：任何一条变绿到红都说明防线破了。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// invokeRaw 调用工具并同时返回 (result, error)，用于区分"工具级失败"与"Go error"。
func invokeRaw(t *testing.T, tool rig.Tool, arguments string) (rig.ToolResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return tool.Invoke(ctx, rig.ToolCall{Name: ToolName, Arguments: arguments})
}

// mustNotLeak 断言输出里没有出现根之外的蛛丝马迹。
func mustNotLeak(t *testing.T, output, secret string) {
	t.Helper()
	if strings.Contains(output, secret) {
		t.Fatalf("output leaked content from outside the workspace (%q): %q", secret, output)
	}
}

// ---------------------------------------------------------------------------
// 1. 路径安全
// ---------------------------------------------------------------------------

func TestAuditPathCannotEscapeRoot(t *testing.T) {
	root := writeTree(t, map[string]string{
		"src/a.txt": "alpha beta\n",
	})
	tool := newTool(t, root)

	// 这些 path 必须被拒绝（错误或空结果），绝不能变成"从 / 开始搜"。
	for _, path := range []string{
		"/", "/etc", "..", "../..", "../../", "../..", "a/../..",
		`C:\`, `C:\Windows`, `\\server\share`, `//server/share`,
	} {
		arguments := fmt.Sprintf(`{"query":"alpha","mode":"text","path":%q}`, path)
		result, err := invokeRaw(t, tool, arguments)
		if err != nil {
			continue // Go error 也是合法拒绝
		}
		if !result.IsError {
			t.Fatalf("path=%q was accepted (IsError=false, content=%q); it must be rejected",
				path, result.Content)
		}
		if result.Content == "" {
			t.Fatalf("path=%q rejected without a reason", path)
		}
	}
}

func TestAuditPathVariantsThatMustWork(t *testing.T) {
	root := writeTree(t, map[string]string{
		"src/deep/a.txt": "alpha beta\n",
	})
	tool := newTool(t, root)
	for _, path := range []string{"", ".", "src", "src/deep", "./src", "src/"} {
		arguments := fmt.Sprintf(`{"query":"alpha","mode":"text","path":%q}`, path)
		result := invoke(t, tool, arguments)
		if result.IsError {
			t.Fatalf("path=%q rejected: %s", path, result.Content)
		}
	}
}

func TestAuditPathPointingAtFileIsRejected(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "alpha\n"})
	result := invoke(t, newTool(t, root), `{"query":"alpha","mode":"text","path":"a.txt"}`)
	if !result.IsError {
		t.Fatalf("path pointing at a file was accepted: %q", result.Content)
	}
}

func TestAuditPathPointingAtMissingPathIsRejected(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "alpha\n"})
	result := invoke(t, newTool(t, root), `{"query":"alpha","mode":"text","path":"nope/deeper"}`)
	if !result.IsError {
		t.Fatalf("missing path was accepted: %q", result.Content)
	}
}

func TestAuditSymlinkInsideRootCannotLeakOutside(t *testing.T) {
	outside := t.TempDir()
	secret := "TOPSECRET_OUTSIDE_ROOT"
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret+"\n"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	// 根内的符号链接指向根外的文件与目录。
	root := writeTree(t, map[string]string{"real.txt": "inside content\n"})
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	tool := newTool(t, root)
	// 内容检索：绝不能读到 secret。
	if result := invoke(t, tool, `{"query":"TOPSECRET","mode":"text"}`); strings.Contains(result.Content, "TOPSECRET") {
		t.Fatalf("text mode read through a symlink: %q", result.Content)
	}
	// 文件名检索：符号链接本身不是普通文件，不该出现在结果里。
	if result := invoke(t, tool, `{"query":"txt","mode":"files"}`); strings.Contains(result.Content, "link.txt") {
		t.Fatalf("files mode listed a symlink: %q", result.Content)
	}
	// path 指向根外目录的符号链接：必须拒绝。
	result, err := invokeRaw(t, tool, `{"query":"secret","mode":"text","path":"linkdir"}`)
	if err == nil && !result.IsError {
		t.Fatalf("path through an escaping symlink was accepted: %q", result.Content)
	}
	if err == nil {
		mustNotLeak(t, result.Content, secret)
	}
}

func TestAuditSymlinkLoopDoesNotHang(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "alpha\n"})
	loopDir := filepath.Join(root, "loop")
	if err := os.MkdirAll(loopDir, 0o755); err != nil {
		t.Fatalf("mkdir loop: %v", err)
	}
	// 自环 + 互环。
	if err := os.Symlink(loopDir, filepath.Join(loopDir, "self")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	sub := filepath.Join(loopDir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	_ = os.Symlink(loopDir, filepath.Join(sub, "back"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		tool := newTool(t, root, WithTimeout(5*time.Second))
		_ = invoke(t, tool, `{"query":"alpha","mode":"text"}`)
		_ = invoke(t, tool, `{"query":"txt","mode":"files"}`)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("symlink loops caused the search to hang (walk followed a symlink?)")
	}
}

// 根目录本身是符号链接时，输出必须是"相对根"的路径，不能出现 ".." 或真实路径段。
func TestAuditSymlinkedRootProducesRootRelativePaths(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-workspace")
	if err := os.MkdirAll(filepath.Join(realRoot, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realRoot, "src", "needle.go"), []byte("needle here\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	linkRoot := filepath.Join(base, "link-workspace")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	tool := newTool(t, linkRoot)
	files := invoke(t, tool, `{"query":"needle","mode":"files"}`)
	if files.IsError {
		t.Fatalf("files mode failed on a symlinked root: %s", files.Content)
	}
	if strings.Contains(files.Content, "..") {
		t.Fatalf("files mode returned a path outside the root: %q", files.Content)
	}
	if strings.TrimSpace(files.Content) != "src/needle.go" {
		t.Fatalf("files mode = %q, want \"src/needle.go\"", files.Content)
	}

	text := invoke(t, tool, `{"query":"needle","mode":"text"}`)
	if text.IsError {
		t.Fatalf("text mode failed on a symlinked root: %s", text.Content)
	}
	if strings.Contains(text.Content, "..") {
		t.Fatalf("text mode returned a path outside the root: %q", text.Content)
	}
	if !strings.HasPrefix(text.Content, "src/needle.go:1:") {
		t.Fatalf("text mode = %q, want a src/needle.go:1: hit", text.Content)
	}
}

// ---------------------------------------------------------------------------
// 2. 资源 / DoS
// ---------------------------------------------------------------------------

func TestAuditLargeRepositoryStaysFast(t *testing.T) {
	root := t.TempDir()
	const files = 5000
	for index := 0; index < files; index++ {
		dir := filepath.Join(root, fmt.Sprintf("pkg%02d", index%50))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		name := filepath.Join(dir, fmt.Sprintf("file%04d.go", index))
		if err := os.WriteFile(name, []byte("package pkg\n// marker token abcdef\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	tool := newTool(t, root, WithTimeout(20*time.Second))

	start := time.Now()
	result := invoke(t, tool, `{"query":"file4999","mode":"files"}`)
	elapsed := time.Since(start)
	if result.IsError {
		t.Fatalf("files mode failed on a large repo: %s", result.Content)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("files mode took %s on 5000 files (want < 15s)", elapsed)
	}

	start = time.Now()
	text := invoke(t, tool, `{"query":"marker abcdef","mode":"text"}`)
	elapsed = time.Since(start)
	if text.IsError {
		t.Fatalf("text mode failed on a large repo: %s", text.Content)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("text mode took %s on 5000 files (want < 20s)", elapsed)
	}
}

func TestAuditHugeFileIsSkipped(t *testing.T) {
	root := t.TempDir()
	// 稀疏文件：100 MB 但几乎不占磁盘，且内容是 NUL（二进制）。
	huge := filepath.Join(root, "huge.log")
	file, err := os.Create(huge)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := file.Truncate(100 << 20); err != nil {
		_ = file.Close()
		t.Fatalf("truncate: %v", err)
	}
	_ = file.Close()
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("needle alpha\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	start := time.Now()
	result := invoke(t, newTool(t, root), `{"query":"needle","mode":"text"}`)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("searching next to a 100MB file took %s", elapsed)
	}
	if result.IsError {
		t.Fatalf("text mode failed: %s", result.Content)
	}
	if !strings.Contains(result.Content, "small.txt") {
		t.Fatalf("small.txt missing from results: %q", result.Content)
	}
	if strings.Contains(result.Content, "huge.log") {
		t.Fatalf("100MB file was indexed: %q", result.Content)
	}
}

func TestAuditDeepTree(t *testing.T) {
	root := t.TempDir()
	deep := root
	for level := 0; level < 100; level++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deep, "leaf.txt"), []byte("alpha leaf\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	result := invoke(t, newTool(t, root), `{"query":"leaf","mode":"files"}`)
	if result.IsError {
		t.Fatalf("deep tree failed: %s", result.Content)
	}
	if !strings.Contains(result.Content, "leaf.txt") {
		t.Fatalf("leaf not found at depth 100: %q", result.Content)
	}
}

func TestAuditLongQueryDoesNotExplode(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 200; index++ {
		content := "alpha beta gamma delta epsilon\n"
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%03d.txt", index)), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// 1 MB 查询：大量互不相同的词项。
	var builder strings.Builder
	builder.WriteString("alpha ")
	for builder.Len() < 1<<20 {
		builder.WriteString("zzz")
		builder.WriteString(fmt.Sprintf("%d ", builder.Len()))
	}
	query := builder.String()
	arguments := fmt.Sprintf(`{"query":%q,"mode":"text"}`, query)

	done := make(chan rig.ToolResult, 1)
	go func() {
		result, err := invokeRaw(t, newTool(t, root, WithTimeout(10*time.Second)), arguments)
		if err != nil {
			result = rig.ToolResult{Content: err.Error(), IsError: true}
		}
		done <- result
	}()
	select {
	case result := <-done:
		// 期望：要么快速返回空结果，要么明确超时；绝不能长时间占死 CPU。
		if !result.IsError && result.Content != "" {
			t.Fatalf("1MB query produced results (%q); terms are ANDed so it must be empty", result.Content)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("1MB query did not return within 45s (unbounded term amplification)")
	}
}

func TestAuditMaxResultsAbuse(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "alpha\n"})
	tool := newTool(t, root, WithMaxResults(3))
	// 巨值 → 夹到上限（不 panic、不分配巨量内存）。
	if result := invoke(t, tool, `{"query":"alpha","mode":"files","max_results":1000000000}`); result.IsError {
		t.Fatalf("max_results=1e9 rejected: %s", result.Content)
	}
	// 负值 → 回落到默认（不是"0 结果"，也不是错误）。
	if result := invoke(t, tool, `{"query":"alpha","mode":"files","max_results":-5}`); result.IsError {
		t.Fatalf("max_results=-5 rejected: %s", result.Content)
	}
	// 浮点 → 明确报错（Go error），而不是静默取整。
	if _, err := invokeRaw(t, tool, `{"query":"alpha","mode":"files","max_results":1.5}`); err == nil {
		t.Fatal("max_results=1.5 accepted, want an explicit argument error")
	}
	// 超大整数（超过 int64）→ 明确报错。
	if _, err := invokeRaw(t, tool, `{"query":"alpha","mode":"files","max_results":999999999999999999999999}`); err == nil {
		t.Fatal("max_results overflow accepted, want an error")
	}
}

func TestAuditTimeoutIsEnforcedByWalk(t *testing.T) {
	// 一个几乎无限的目录树 + 极短预算：必须真的被打断，而不是跑完。
	root := t.TempDir()
	for index := 0; index < 300; index++ {
		dir := filepath.Join(root, fmt.Sprintf("d%03d", index))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for inner := 0; inner < 20; inner++ {
			name := filepath.Join(dir, fmt.Sprintf("f%02d.txt", inner))
			if err := os.WriteFile(name, []byte("alpha beta\n"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
	tool := newTool(t, root, WithTimeout(time.Nanosecond))
	result, err := invokeRaw(t, tool, `{"query":"alpha beta","mode":"text"}`)
	if err != nil {
		return // 父 ctx 取消路径
	}
	if !result.IsError || !strings.Contains(result.Content, "timed out") {
		t.Fatalf("tiny budget was not enforced: IsError=%t content=%q", result.IsError, result.Content)
	}
}

// ---------------------------------------------------------------------------
// 3. @提及解析（纯文本，不允许 panic / 溢出 / 死循环）
// ---------------------------------------------------------------------------

func TestAuditMentionParsingAdversarial(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []Mention // nil 表示"不应产生任何提及"
	}{
		{name: "lone at", input: "@", want: nil},
		{name: "double at", input: "@@x", want: []Mention{{Path: "@x"}}},
		{name: "at at", input: "@a@b", want: []Mention{{Path: "a@b"}}},
		{name: "at space", input: "@ x", want: nil},
		{name: "at newline", input: "@\nnext", want: nil},
		{name: "quoted then bare", input: `@"a"b`, want: []Mention{{Path: "a"}}},
		{name: "colon quoted", input: `@:"x"`, want: nil},
		{name: "email", input: "mail me at a@b.com", want: nil},
		{name: "url", input: "see https://a@b/c", want: nil},
		{name: "trailing dash", input: "@a:1-", want: []Mention{{Path: "a:1-"}}},
		{name: "negative line", input: "@a:-1", want: []Mention{{Path: "a:-1"}}},
		{name: "overflow line", input: "@a:999999999999999999999", want: []Mention{{Path: "a:999999999999999999999"}}},
		{name: "reversed range", input: "@a:5-2", want: []Mention{{Path: "a", Line: 2, EndLine: 5}}},
		{name: "crlf", input: "@a.txt\r\n@b.txt", want: []Mention{{Path: "a.txt"}, {Path: "b.txt"}}},
		{name: "dot slash", input: "@./x", want: []Mention{{Path: "./x"}}},
		{name: "home", input: "@~/x", want: []Mention{{Path: "~/x"}}},
		{name: "windows", input: `@C:\x\y.txt`, want: []Mention{{Path: `C:\x\y.txt`}}},
		{name: "duplicates", input: "@a.txt @a.txt", want: []Mention{{Path: "a.txt"}}},
		{name: "empty", input: "", want: nil},
		{name: "only whitespace", input: "   \n\t ", want: nil},
		{name: "unclosed quote", input: `@"a.txt`, want: nil},
		{name: "punctuation", input: "@a.txt,", want: []Mention{{Path: "a.txt"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := ParseMentions(testCase.input)
			if len(got) != len(testCase.want) {
				t.Fatalf("ParseMentions(%q) = %+v, want %+v", testCase.input, got, testCase.want)
			}
			for index := range testCase.want {
				if got[index].Path != testCase.want[index].Path ||
					got[index].Line != testCase.want[index].Line ||
					got[index].EndLine != testCase.want[index].EndLine {
					t.Fatalf("ParseMentions(%q)[%d] = %+v, want %+v",
						testCase.input, index, got[index], testCase.want[index])
				}
			}
		})
	}
}

func TestAuditParseMentionsNeverPanicsOnNoise(t *testing.T) {
	inputs := []string{
		strings.Repeat("@", 1000),
		strings.Repeat("@\"", 500),
		strings.Repeat("@a:", 500),
		strings.Repeat("@@@", 300),
		"@" + strings.Repeat("\r\n", 100),
		"@a:" + strings.Repeat("9", 100),
		"\x00@a\x00",
		"@\x00",
	}
	for _, input := range inputs {
		_ = ParseMentions(input) // 只要求不 panic / 不挂
	}
}

// ---------------------------------------------------------------------------
// 4. 打分正确性
// ---------------------------------------------------------------------------

func TestAuditFuzzyScoreEdgeInputs(t *testing.T) {
	cases := []struct {
		query, path string
		wantMatch   bool
	}{
		{"", "a/b", false},
		{"x", "", false},
		{"/", "a/b", false},
		{"/", "/", false},
		{"a/", "a/b", true},
		{"./a", "a", true},
		{"A", "a", true},
		{"İ", "İstanbul", true},
		{"ß", "straße", true},
		{"🚀", "rocket🚀.go", true},
		{"..", "a.txt", false},
	}
	for _, testCase := range cases {
		score, matched := FuzzyScore(testCase.query, testCase.path)
		if matched != testCase.wantMatch {
			t.Fatalf("FuzzyScore(%q, %q) matched=%t (score %d), want %t",
				testCase.query, testCase.path, matched, score, testCase.wantMatch)
		}
	}
}

// "更匹配的" 不应拿到更低的分：精确文件名 > 子序列 > 只有目录命中。
func TestAuditFuzzyScoreNoObviousInversion(t *testing.T) {
	type sample struct{ query, path string }
	pairs := [][2]sample{
		// 精确匹配必须赢过子序列匹配。
		{{"search", "search.go"}, {"search", "s_e_a_r_c_h_helper.go"}},
		// 连续匹配必须赢过完全散落的匹配（无词首加成的那种）。
		{{"abc", "abc.go"}, {"abc", "xaxbxc.go"}},
		// 文件名命中必须赢过目录命中。
		{{"util", "src/util.go"}, {"util", "util/src/other.go"}},
		// 短路径必须赢过长路径（同样的匹配质量）。
		{{"main", "main.go"}, {"main", "a/very/deep/and/long/main.go"}},
	}
	for _, pair := range pairs {
		firstScore, firstMatched := FuzzyScore(pair[0].query, pair[0].path)
		secondScore, secondMatched := FuzzyScore(pair[1].query, pair[1].path)
		if !firstMatched || !secondMatched {
			t.Fatalf("expected both to match: %+v / %+v", pair[0], pair[1])
		}
		if firstScore <= secondScore {
			t.Fatalf("inversion: %q vs %q scored %d <= %d (query %q)",
				pair[0].path, pair[1].path, firstScore, secondScore, pair[0].query)
		}
	}
}

// 排序反例的回归测试：文件名"就叫这个"必须赢过"逐字符落在词首"的散落匹配。
//
// 修复前（fuzzyBasenamePrefixBonus 之前）这三个反例都成立：
//
//	query=abc    → a_b_c.go(78)        > abc.go(76)
//	query=search → s_e_a_r_c_h.go(138) > search.go(133)
//	query=util   → util/src/other.go(95) > src/util.go(91)
//
// 根因是词首加成（+12/字符）高于连续加成（+10/字符），逐字符的词首分会把
// "文件名就叫这个"压下去。
func TestAuditFuzzyBasenamePrefixOutranksSegmentedMatch(t *testing.T) {
	pairs := [][2]string{
		{"abc", "abc.go"}, {"abc", "a_b_c.go"},
		{"search", "search.go"}, {"search", "s_e_a_r_c_h.go"},
		{"util", "src/util.go"}, {"util", "util/src/other.go"},
	}
	for index := 0; index < len(pairs); index += 2 {
		good, goodMatched := FuzzyScore(pairs[index][0], pairs[index][1])
		bad, badMatched := FuzzyScore(pairs[index+1][0], pairs[index+1][1])
		if !goodMatched || !badMatched {
			t.Fatalf("both should match: %+v / %+v", pairs[index], pairs[index+1])
		}
		if good <= bad {
			t.Fatalf("regression: %q scored %d <= %q's %d (query %q)",
				pairs[index][1], good, pairs[index+1][1], bad, pairs[index][0])
		}
	}
	// 前缀加成不能推翻"词首比词中分高"这条既有契约（fuzzy_test.go 的用例）。
	wordStart, _ := FuzzyScore("ab", "a-b.go")
	midWord, _ := FuzzyScore("ab", "xab.go")
	if wordStart <= midWord {
		t.Fatalf("word-start (%d) no longer beats mid-word (%d)", wordStart, midWord)
	}
}

// 全库随机化：任何输入都不允许 panic；分数必须是确定的。
func TestAuditFuzzyScoreDeterministicUnderNoise(t *testing.T) {
	alphabet := []rune("abAB/_-. 中文🚀İß9:")
	for _, seed := range []int{1, 7, 42} {
		state := uint32(seed)
		next := func() rune {
			state = state*1664525 + 1013904223
			return alphabet[int(state>>16)%len(alphabet)]
		}
		for iteration := 0; iteration < 500; iteration++ {
			var query, path strings.Builder
			for index := 0; index < iteration%12; index++ {
				query.WriteRune(next())
			}
			for index := 0; index < iteration%20; index++ {
				path.WriteRune(next())
			}
			first, firstMatched := FuzzyScore(query.String(), path.String())
			second, secondMatched := FuzzyScore(query.String(), path.String())
			if first != second || firstMatched != secondMatched {
				t.Fatalf("non-deterministic score for (%q, %q)", query.String(), path.String())
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 5. BM25 / 分词退化情形
// ---------------------------------------------------------------------------

func TestAuditBM25DegenerateInputs(t *testing.T) {
	// 空语料。
	empty := newBM25Index()
	empty.finalize()
	if matches, err := empty.search(context.Background(), Tokenize("anything")); err != nil || matches != nil {
		t.Fatalf("empty index returned matches: %+v", matches)
	}

	// 单文档。
	single := newBM25Index()
	single.add("a.txt", []byte("alpha"))
	single.finalize()
	matches, err := single.search(context.Background(), Tokenize("alpha"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 1 || matches[0].rel != "a.txt" {
		t.Fatalf("single-doc matches = %+v", matches)
	}
	// 单文档里 df == N：idf 必须仍是正数（否则"唯一的匹配"会被算成负分）。
	if matches[0].score <= 0 {
		t.Fatalf("single-document score = %v, want > 0", matches[0].score)
	}

	// df == N 的常见词也必须给正分。
	all := newBM25Index()
	all.add("a.txt", []byte("common thing"))
	all.add("b.txt", []byte("common other"))
	all.finalize()
	commonMatches, err := all.search(context.Background(), Tokenize("common"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, match := range commonMatches {
		if match.score <= 0 {
			t.Fatalf("df==N term scored %v for %s, want > 0", match.score, match.rel)
		}
	}

	// 空文件（词数 0）不进索引，avgdl 不会变成 0。
	sparse := newBM25Index()
	sparse.add("empty.txt", nil)
	sparse.finalize()
	if sparse.averageLength != 0 {
		t.Fatalf("averageLength = %v for an empty corpus, want 0", sparse.averageLength)
	}
	if matches, err := sparse.search(context.Background(), Tokenize("alpha")); err != nil || matches != nil {
		t.Fatalf("empty-content index returned matches: %+v", matches)
	}

	// 同分时顺序必须确定（不是 map 迭代顺序）。
	tied := newBM25Index()
	tied.add("b.txt", []byte("same text here"))
	tied.add("a.txt", []byte("same text here"))
	tied.finalize()
	first, err := tied.search(context.Background(), Tokenize("same"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	second, err := tied.search(context.Background(), Tokenize("same"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("tied matches = %+v / %+v", first, second)
	}
	if first[0].rel != second[0].rel || first[1].rel != second[1].rel {
		t.Fatalf("tie order is not deterministic: %+v vs %+v", first, second)
	}
	if first[0].rel != "a.txt" {
		t.Fatalf("tie order = %+v, want the shorter path first", first)
	}
}

func TestAuditTokenizeEdgeInputs(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"12345", []string{"12345"}},
		{"!!!", nil},
		{"___", nil},
		{"a-b_c", []string{"a", "b", "c"}},
		{"HTTPServer", []string{"http", "server"}},
		{"http2Token", []string{"http2", "token"}},
		{"中文分词", []string{"中文分词"}},
		{"🚀rocket", []string{"rocket"}},
		{"İstanbul", []string{"istanbul"}},
	}
	for _, testCase := range cases {
		got := Tokenize(testCase.input)
		if len(got) != len(testCase.want) {
			t.Fatalf("Tokenize(%q) = %v, want %v", testCase.input, got, testCase.want)
		}
		for index := range testCase.want {
			if got[index] != testCase.want[index] {
				t.Fatalf("Tokenize(%q) = %v, want %v", testCase.input, got, testCase.want)
			}
		}
	}
	// 非法 UTF-8 不能 panic。
	_ = Tokenize(string([]byte{0xff, 0xfe, 0xfd, 'a', 'b', 0x80}))
	// 超长单词不能 panic（只是一个很长的 token）。
	long := strings.Repeat("a", 1<<20)
	if got := Tokenize(long); len(got) != 1 || len(got[0]) != len(long) {
		t.Fatalf("Tokenize(1MB word) = %d tokens", len(got))
	}
}

// ---------------------------------------------------------------------------
// 6. 输出契约
// ---------------------------------------------------------------------------

func TestAuditOutputContractWithColonsAndLongLines(t *testing.T) {
	longPrefix := strings.Repeat("x", maxSnippetRunes+50)
	root := writeTree(t, map[string]string{
		"a.txt": "key: value: alpha\n",
		"b.txt": longPrefix + " alpha\n",
	})
	result := invoke(t, newTool(t, root, WithMaxResults(10)), `{"query":"alpha","mode":"text"}`)
	if result.IsError {
		t.Fatalf("text mode failed: %s", result.Content)
	}
	lines := strings.Split(result.Content, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected two hits, got %q", result.Content)
	}
	// 每条结果都必须是 path:line:content 三段（content 内部的 ':' 允许存在）。
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			t.Fatalf("result line is not path:line:content: %q", line)
		}
		if parts[0] == "" || parts[1] == "" {
			t.Fatalf("result line has an empty path/line: %q", line)
		}
	}
	// 长行必须按 rune 截断到 200 字符 + 省略号。
	for _, line := range lines {
		if !strings.HasPrefix(line, "b.txt:") {
			continue
		}
		content := strings.SplitN(line, ":", 3)[2]
		runes := []rune(content)
		if len(runes) != maxSnippetRunes+1 || runes[len(runes)-1] != '…' {
			t.Fatalf("long line was not truncated at %d runes: %d runes", maxSnippetRunes, len(runes))
		}
	}
}

func TestAuditTruncationCountIsAccurate(t *testing.T) {
	files := map[string]string{}
	for index := 0; index < 30; index++ {
		files[fmt.Sprintf("f%02d.txt", index)] = "alpha beta\n"
	}
	root := writeTree(t, files)
	result := invoke(t, newTool(t, root, WithMaxResults(5)), `{"query":"alpha beta","mode":"text"}`)
	if result.IsError {
		t.Fatalf("text mode failed: %s", result.Content)
	}
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	note := lines[len(lines)-1]
	if !strings.HasPrefix(note, "[truncated: ") {
		t.Fatalf("missing truncation note: %q", result.Content)
	}
	var reported int
	if _, err := fmt.Sscanf(note, "[truncated: %d more matches]", &reported); err != nil {
		t.Fatalf("cannot parse truncation note %q: %v", note, err)
	}
	// 30 条命中、返回 5 条 → 必须报 25。
	if reported != 25 {
		t.Fatalf("truncation note says %d more matches, want 25", reported)
	}
	if got := len(lines) - 1; got != 5 {
		t.Fatalf("returned %d result lines, want 5", got)
	}
}

func TestAuditEmptyResultsAreEmptyString(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "alpha\n"})
	tool := newTool(t, root)
	for _, arguments := range []string{
		`{"query":"zzzznomatch","mode":"files"}`,
		`{"query":"zzzznomatch","mode":"text"}`,
		`{"query":"no mentions here","mode":"mentions"}`,
		`{"query":"zzzznomatch","mode":"auto"}`,
	} {
		result := invoke(t, tool, arguments)
		if result.IsError || result.Content != "" {
			t.Fatalf("%s = (IsError=%t, %q), want an empty success", arguments, result.IsError, result.Content)
		}
	}
}

// ---------------------------------------------------------------------------
// 7. 工具契约
// ---------------------------------------------------------------------------

func TestAuditModeAndQueryContract(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "alpha\n"})
	tool := newTool(t, root)

	// 非法 mode → 工具级错误（IsError），不是 Go error。
	result, err := invokeRaw(t, tool, `{"query":"alpha","mode":"bogus"}`)
	if err != nil {
		t.Fatalf("invalid mode returned a Go error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("invalid mode accepted: %q", result.Content)
	}

	// 缺失 / 空白 query → 工具级错误。
	for _, arguments := range []string{
		`{"mode":"files"}`,
		`{"query":"","mode":"files"}`,
		`{"query":"   ","mode":"files"}`,
		`{"query":"\t\n ","mode":"files"}`,
	} {
		got, err := invokeRaw(t, tool, arguments)
		if err != nil {
			t.Fatalf("%s returned a Go error: %v", arguments, err)
		}
		if !got.IsError {
			t.Fatalf("%s was accepted: %q", arguments, got.Content)
		}
	}

	// 非对象 arguments → Go error（参数解码失败）。
	if _, err := invokeRaw(t, tool, `[1,2,3]`); err == nil {
		t.Fatal("array arguments accepted")
	}
}

func TestAuditCanceledContextReturnsImmediately(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "alpha\n"})
	tool := newTool(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := tool.Invoke(ctx, rig.ToolCall{Name: ToolName, Arguments: `{"query":"alpha","mode":"text"}`})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Invoke error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled Invoke took %s", elapsed)
	}
}

func TestAuditSchemaContract(t *testing.T) {
	schema := newTool(t, t.TempDir()).Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %v", schema["properties"])
	}
	for _, name := range []string{"query", "path", "mode", "max_results"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("schema is missing property %q", name)
		}
	}
	switch required := schema["required"].(type) {
	case []string:
		if len(required) != 1 || required[0] != "query" {
			t.Fatalf("schema required = %v, want [query]", required)
		}
	case []any:
		if len(required) != 1 || required[0] != "query" {
			t.Fatalf("schema required = %v, want [query]", required)
		}
	default:
		t.Fatalf("schema required has unexpected type %T (%v)", schema["required"], schema["required"])
	}
}

// 深校验：模式名大小写不敏感，未声明字段被忽略（json.Unmarshal 语义）。
func TestAuditModeIsCaseInsensitiveAndUnknownFieldsIgnored(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "alpha\n"})
	tool := newTool(t, root)
	for _, mode := range []string{"FILES", "Files", " files "} {
		arguments := fmt.Sprintf(`{"query":"a","mode":%q}`, mode)
		if result := invoke(t, tool, arguments); result.IsError {
			t.Fatalf("mode=%q rejected: %s", mode, result.Content)
		}
	}
	if result := invoke(t, tool, `{"query":"a","mode":"files","nonsense":true}`); result.IsError {
		t.Fatalf("unknown field caused a failure: %s", result.Content)
	}
}

// 只在支持符号链接的平台上跑（Windows 需要开发者模式）。
func TestAuditSymlinkSupportGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows; covered on unix")
	}
	dir := t.TempDir()
	if err := os.Symlink(dir, filepath.Join(dir, "x")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 8. 模糊测试（go test -fuzz 用；默认只跑种子语料）
// ---------------------------------------------------------------------------

// FuzzParseMentions 断言解析器对任意输入都不 panic、结果确定、且行号恒为
// 0/1 起算的合法值（0 = 未指定；否则 1 <= Line <= EndLine）。
func FuzzParseMentions(f *testing.F) {
	for _, seed := range []string{
		"", "@", "@a.txt", `@"a b.txt"`, "@a:1-3", "@a:3-1", "a@b.com",
		"@@x", "@\r\n", "@" + strings.Repeat("9", 40), "见 @src/a.go:12",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		first := ParseMentions(input)
		second := ParseMentions(input)
		if len(first) != len(second) {
			t.Fatalf("non-deterministic mention count for %q", input)
		}
		for index, mention := range first {
			if mention.Path == "" {
				t.Fatalf("empty Path for %q: %+v", input, mention)
			}
			if mention.Line < 0 || mention.EndLine < 0 {
				t.Fatalf("negative line for %q: %+v", input, mention)
			}
			if mention.Line > 0 && (mention.EndLine < mention.Line || mention.EndLine == 0) {
				t.Fatalf("inconsistent range for %q: %+v", input, mention)
			}
			if second[index].Path != mention.Path {
				t.Fatalf("non-deterministic path for %q", input)
			}
		}
	})
}

// FuzzFuzzyScore 断言打分器对任意输入都不 panic、确定，并且分数为有限整数
// （不会因为整数溢出变成负数——查询与路径都很长时逐字符加分是有上限的，
// 但我们在 fuzz 里只要求"不 panic / 确定"，溢出属已知边界）。
func FuzzFuzzyScore(f *testing.F) {
	for _, seed := range [][2]string{
		{"", ""}, {"a", "a"}, {"search", "src/search/deep.go"},
		{"/", "a/b"}, {"ab", "a-b.go"}, {"🚀", "x🚀y.go"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, query, pathText string) {
		firstScore, firstMatched := FuzzyScore(query, pathText)
		secondScore, secondMatched := FuzzyScore(query, pathText)
		if firstScore != secondScore || firstMatched != secondMatched {
			t.Fatalf("non-deterministic score for (%q, %q)", query, pathText)
		}
		if !firstMatched && firstScore != 0 {
			t.Fatalf("non-match returned score %d for (%q, %q)", firstScore, query, pathText)
		}
	})
}

// FuzzTokenize 断言分词器对任意字节串都不 panic、结果确定。
func FuzzTokenize(f *testing.F) {
	for _, seed := range []string{"", "abc", "HTTPServer", "中文", "\xff\xfe", "a_b-c"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		first := Tokenize(input)
		second := Tokenize(input)
		if len(first) != len(second) {
			t.Fatalf("non-deterministic token count for %q", input)
		}
		for index := range first {
			if first[index] != second[index] {
				t.Fatalf("non-deterministic token for %q", input)
			}
			if first[index] == "" {
				t.Fatalf("empty token for %q", input)
			}
		}
	})
}
