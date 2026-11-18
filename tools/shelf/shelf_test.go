package shelf_test

// `progressive` 原语的用例：三条主路径 + 四个上限的边界（正好等于 / 超 1 字节）
// + 渐进性断言（list/search 不读正文）+ 懒加载计数 + 校验与并发。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/shelf"
)

// countingSource 记录每个方法的调用次数，用来断言"list/search 不触发 Load"。
type countingSource struct {
	mu      sync.Mutex
	items   []shelf.Item
	bodies  map[string]string
	listN   int32
	searchN int32
	loads   []string
}

func newCountingSource(items ...shelf.Item) *countingSource {
	source := &countingSource{items: items, bodies: make(map[string]string)}
	for _, item := range items {
		source.bodies[item.Name] = "BODY-OF-" + item.Name
	}
	return source
}

func (s *countingSource) List(context.Context) ([]shelf.Item, error) {
	atomic.AddInt32(&s.listN, 1)
	return append([]shelf.Item(nil), s.items...), nil
}

func (s *countingSource) Search(_ context.Context, query string) ([]shelf.Item, error) {
	atomic.AddInt32(&s.searchN, 1)
	needle := strings.ToLower(strings.TrimSpace(query))
	var matches []shelf.Item
	for _, item := range s.items {
		if strings.Contains(strings.ToLower(item.Name+" "+item.Summary), needle) {
			matches = append(matches, item)
		}
	}
	return matches, nil
}

func (s *countingSource) Load(_ context.Context, name string) (string, error) {
	s.mu.Lock()
	s.loads = append(s.loads, name)
	s.mu.Unlock()
	body, ok := s.bodies[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", shelf.ErrUnknownItem, name)
	}
	return body, nil
}

func (s *countingSource) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.loads)
}

func mustNew(t *testing.T, source shelf.Source, opts shelf.Options) rig.Tool {
	t.Helper()
	if opts.ToolName == "" {
		opts.ToolName = "browse"
	}
	tool, err := shelf.New(source, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tool
}

func call(t *testing.T, tool rig.Tool, argsJSON string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: tool.Name(), Arguments: argsJSON})
	if err != nil {
		t.Fatalf("Invoke 返回 go error: %v", err)
	}
	return result
}

// TestNewValidation 断言构造期 fail closed。
func TestNewValidation(t *testing.T) {
	if _, err := shelf.New(nil, shelf.Options{ToolName: "x"}); err == nil {
		t.Fatal("nil Source 应当报错")
	}
	if _, err := shelf.New(newCountingSource(), shelf.Options{}); err == nil {
		t.Fatal("空 ToolName 应当报错")
	}
	if _, err := shelf.New(newCountingSource(), shelf.Options{ToolName: "  "}); err == nil {
		t.Fatal("全空白 ToolName 应当报错")
	}
	tool, err := shelf.New(newCountingSource(), shelf.Options{ToolName: "browse"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tool.Description() == "" {
		t.Fatal("默认描述不能为空")
	}
	if !strings.Contains(tool.Description(), "load") || !strings.Contains(tool.Description(), "list") {
		t.Fatalf("默认描述应说明 list/load 用法: %q", tool.Description())
	}
}

// TestListFormat 断言默认目录行格式。
func TestListFormat(t *testing.T) {
	source := newCountingSource(
		shelf.Item{Name: "alpha", Summary: "第一个条目", Bytes: 2048},
		shelf.Item{Name: "beta", Summary: "第二个条目"},
	)
	tool := mustNew(t, source, shelf.Options{})
	result := call(t, tool, `{"action":"list"}`)
	if result.IsError {
		t.Fatalf("list: %s", result.Content)
	}
	lines := strings.Split(result.Content, "\n")
	if len(lines) != 2 {
		t.Fatalf("行数 = %d:\n%s", len(lines), result.Content)
	}
	if lines[0] != "- alpha — 第一个条目 (2.0 KB)" {
		t.Fatalf("带字节数的行格式不对: %q", lines[0])
	}
	if lines[1] != "- beta — 第二个条目" {
		t.Fatalf("无字节数的行格式不对: %q", lines[1])
	}
}

// TestListCap 断言 MaxListed 上限与"… and N more"。
func TestListCap(t *testing.T) {
	items := make([]shelf.Item, 0, 45)
	for index := 0; index < 45; index++ {
		items = append(items, shelf.Item{Name: fmt.Sprintf("item-%02d", index), Summary: "摘要"})
	}
	tool := mustNew(t, newCountingSource(items...), shelf.Options{})
	result := call(t, tool, `{"action":"list"}`)
	lines := strings.Split(result.Content, "\n")
	if len(lines) != shelf.DefaultMaxListed+1 {
		t.Fatalf("行数 = %d, want %d+1", len(lines), shelf.DefaultMaxListed)
	}
	if lines[len(lines)-1] != "… and 5 more (use search to narrow)" {
		t.Fatalf("尾巴不对: %q", lines[len(lines)-1])
	}
}

// TestListUnlimited 断言负数表示不限制。
func TestListUnlimited(t *testing.T) {
	items := make([]shelf.Item, 0, 60)
	for index := 0; index < 60; index++ {
		items = append(items, shelf.Item{Name: fmt.Sprintf("item-%02d", index), Summary: "摘要"})
	}
	tool := mustNew(t, newCountingSource(items...), shelf.Options{
		MaxListed: -1, MaxOutputBytes: 1 << 20,
	})
	result := call(t, tool, `{"action":"list"}`)
	if lines := strings.Split(result.Content, "\n"); len(lines) != 60 {
		t.Fatalf("行数 = %d, want 60（不限制）:\n%.200s", len(lines), result.Content)
	}
}

// TestListEmpty 断言空 source 的可读文案。
func TestListEmpty(t *testing.T) {
	tool := mustNew(t, newCountingSource(), shelf.Options{})
	result := call(t, tool, `{"action":"list"}`)
	if result.IsError || !strings.Contains(result.Content, "no items yet") {
		t.Fatalf("空目录文案不对: %q (IsError=%v)", result.Content, result.IsError)
	}
	// 自定义文案。
	tool = mustNew(t, newCountingSource(), shelf.Options{
		Texts: shelf.Texts{EmptyList: "nothing here"},
	})
	if result := call(t, tool, `{"action":"list"}`); result.Content != "nothing here" {
		t.Fatalf("自定义空文案没生效: %q", result.Content)
	}
}

// TestListIsProgressive 断言 list 不读正文，也不触发 Load。
func TestListIsProgressive(t *testing.T) {
	source := newCountingSource(
		shelf.Item{Name: "alpha", Summary: "第一个"},
		shelf.Item{Name: "beta", Summary: "第二个"},
	)
	tool := mustNew(t, source, shelf.Options{})
	result := call(t, tool, `{"action":"list"}`)
	if strings.Contains(result.Content, "BODY-OF-") {
		t.Fatalf("list 泄漏了正文:\n%s", result.Content)
	}
	if got := source.loadCount(); got != 0 {
		t.Fatalf("list 触发了 %d 次 Load（渐进性被破坏）", got)
	}
	if got := atomic.LoadInt32(&source.listN); got != 1 {
		t.Fatalf("List 调用 %d 次, want 1", got)
	}
}

// TestSearchIsProgressive 断言 search 只回目录行，且不触发 Load。
func TestSearchIsProgressive(t *testing.T) {
	source := newCountingSource(
		shelf.Item{Name: "alpha", Summary: "第一个"},
		shelf.Item{Name: "beta", Summary: "第二个"},
	)
	tool := mustNew(t, source, shelf.Options{})
	result := call(t, tool, `{"action":"search","query":"beta"}`)
	if result.IsError {
		t.Fatalf("search: %s", result.Content)
	}
	if !strings.Contains(result.Content, "beta") || strings.Contains(result.Content, "alpha") {
		t.Fatalf("搜索结果不对: %q", result.Content)
	}
	if strings.Contains(result.Content, "BODY-OF-") {
		t.Fatalf("search 泄漏了正文: %q", result.Content)
	}
	if got := source.loadCount(); got != 0 {
		t.Fatalf("search 触发了 %d 次 Load", got)
	}
}

// TestSearchCap 断言 MaxSearch 上限。
func TestSearchCap(t *testing.T) {
	items := make([]shelf.Item, 0, 25)
	for index := 0; index < 25; index++ {
		items = append(items, shelf.Item{Name: fmt.Sprintf("hit-%02d", index), Summary: "共同关键词 match"})
	}
	tool := mustNew(t, newCountingSource(items...), shelf.Options{})
	result := call(t, tool, `{"action":"search","query":"match"}`)
	lines := strings.Split(result.Content, "\n")
	if len(lines) != shelf.DefaultMaxSearch+1 {
		t.Fatalf("行数 = %d, want %d+1:\n%s", len(lines), shelf.DefaultMaxSearch, result.Content)
	}
	if !strings.Contains(lines[len(lines)-1], "and 5 more") {
		t.Fatalf("尾巴不对: %q", lines[len(lines)-1])
	}
}

// TestSearchErrors 断言 query 必填与无匹配。
func TestSearchErrors(t *testing.T) {
	tool := mustNew(t, newCountingSource(shelf.Item{Name: "alpha", Summary: "第一个"}), shelf.Options{})
	if result := call(t, tool, `{"action":"search"}`); !result.IsError || !strings.Contains(result.Content, "query is required") {
		t.Fatalf("缺 query: %q", result.Content)
	}
	result := call(t, tool, `{"action":"search","query":"nothing"}`)
	if result.IsError || !strings.Contains(result.Content, "no items matched") {
		t.Fatalf("无匹配: %q (IsError=%v)", result.Content, result.IsError)
	}
}

// TestLoadReturnsBody 断言 load 才回正文。
func TestLoadReturnsBody(t *testing.T) {
	tool := mustNew(t, newCountingSource(shelf.Item{Name: "alpha", Summary: "第一个"}), shelf.Options{})
	result := call(t, tool, `{"action":"load","name":"alpha"}`)
	if result.IsError || result.Content != "BODY-OF-alpha" {
		t.Fatalf("load = %q (IsError=%v)", result.Content, result.IsError)
	}
}

// TestLoadReturnsFullBody 断言 load 不做单条截断：正文原样返回（上限只由全局
// MaxOutputBytes 兜底）。
func TestLoadReturnsFullBody(t *testing.T) {
	body := strings.Repeat("x", 9<<10) // 9 KiB —— 超过旧的 8 KiB 单条上限
	source := &countingSource{items: []shelf.Item{{Name: "a", Summary: "s"}}, bodies: map[string]string{"a": body}}
	tool := mustNew(t, source, shelf.Options{ToolName: "browse"})
	result := call(t, tool, `{"action":"load","name":"a"}`)
	if result.Content != body {
		t.Fatalf("正文被截断了：got %d 字节, want %d", len(result.Content), len(body))
	}
}

// TestLoadUTF8Safe 断言全局截断切在字符边界上。
func TestLoadUTF8Safe(t *testing.T) {
	body := strings.Repeat("记", 3000) // 9000 字节
	source := &countingSource{items: []shelf.Item{{Name: "a", Summary: "s"}}, bodies: map[string]string{"a": body}}
	tool := mustNew(t, source, shelf.Options{ToolName: "browse", MaxOutputBytes: 100}) // 100 不是 3 的倍数
	result := call(t, tool, `{"action":"load","name":"a"}`)
	prefix := result.Content
	if index := strings.Index(prefix, "\n[truncated:"); index >= 0 {
		prefix = prefix[:index]
	}
	if !utf8.ValidString(prefix) {
		t.Fatal("截断切出了非法 UTF-8")
	}
	if strings.Trim(prefix, "记") != "" {
		t.Fatalf("出现了半个汉字: %q", prefix[len(prefix)-6:])
	}
	if !strings.Contains(result.Content, "[truncated: content exceeds") {
		t.Fatalf("截断文案不对: %q", result.Content[len(result.Content)-120:])
	}
}

// TestLoadUnknownAndMissing 断言未知条目与缺参数。
func TestLoadUnknownAndMissing(t *testing.T) {
	tool := mustNew(t, newCountingSource(shelf.Item{Name: "alpha", Summary: "第一个"}), shelf.Options{})
	result := call(t, tool, `{"action":"load","name":"nope"}`)
	if !result.IsError || !strings.Contains(result.Content, "no item named") || !strings.Contains(result.Content, "action: list") {
		t.Fatalf("未知条目文案不对: %q", result.Content)
	}
	if result := call(t, tool, `{"action":"load"}`); !result.IsError || !strings.Contains(result.Content, "name is required") {
		t.Fatalf("缺 name: %q", result.Content)
	}
}

// TestActionDefaulting 断言省略 action 的兼容行为：有 name → load，无 name → list。
func TestActionDefaulting(t *testing.T) {
	tool := mustNew(t, newCountingSource(shelf.Item{Name: "alpha", Summary: "第一个"}), shelf.Options{})
	if result := call(t, tool, `{"name":"alpha"}`); result.Content != "BODY-OF-alpha" {
		t.Fatalf("name-only 调用应当 load: %q", result.Content)
	}
	if result := call(t, tool, `{}`); !strings.Contains(result.Content, "alpha") {
		t.Fatalf("无参数调用应当 list: %q", result.Content)
	}
}

// TestUnknownAction 断言未知 action 的错误。
func TestUnknownAction(t *testing.T) {
	tool := mustNew(t, newCountingSource(), shelf.Options{})
	result := call(t, tool, `{"action":"explode"}`)
	if !result.IsError || !strings.Contains(result.Content, "unknown action") {
		t.Fatalf("未知 action: %q", result.Content)
	}
}

// TestItemValidation 断言 Source 返回坏条目时 fail closed（不是静默渲染空摘要）。
func TestItemValidation(t *testing.T) {
	cases := map[string][]shelf.Item{
		"缺摘要":  {{Name: "a"}},
		"空名字":  {{Name: " ", Summary: "有摘要"}},
		"摘要超长": {{Name: "a", Summary: strings.Repeat("长", 100)}},
	}
	for label, items := range cases {
		tool := mustNew(t, newCountingSource(items...), shelf.Options{})
		result := call(t, tool, `{"action":"list"}`)
		if !result.IsError {
			t.Fatalf("%s: 应当报错，实际 %q", label, result.Content)
		}
		if !strings.Contains(result.Content, "summary") && !strings.Contains(result.Content, "name") {
			t.Fatalf("%s: 错误文案应说明缺什么: %q", label, result.Content)
		}
	}
}

// TestMaxOutputTruncatesWholeLines 断言总输出上限按整行截断。
func TestMaxOutputTruncatesWholeLines(t *testing.T) {
	items := make([]shelf.Item, 0, 20)
	for index := 0; index < 20; index++ {
		items = append(items, shelf.Item{Name: fmt.Sprintf("item-%02d", index), Summary: strings.Repeat("摘要", 10)})
	}
	tool := mustNew(t, newCountingSource(items...), shelf.Options{MaxOutputBytes: 200})
	result := call(t, tool, `{"action":"list"}`)
	if !strings.Contains(result.Content, "[truncated: content exceeds") {
		t.Fatalf("缺少输出截断提示: %q", result.Content)
	}
	for _, line := range strings.Split(result.Content, "\n") {
		if line == "" || strings.HasPrefix(line, "[truncated") {
			continue
		}
		if !strings.HasPrefix(line, "- item-") || !strings.HasSuffix(line, ")") && !strings.Contains(line, "摘要") {
			t.Fatalf("出现了半行: %q", line)
		}
	}
}

// TestConcurrentLoads 并发 10 个 load（-race 也跑）。
func TestConcurrentLoads(t *testing.T) {
	items := make([]shelf.Item, 0, 10)
	for index := 0; index < 10; index++ {
		items = append(items, shelf.Item{Name: fmt.Sprintf("item-%02d", index), Summary: "摘要"})
	}
	source := newCountingSource(items...)
	tool := mustNew(t, source, shelf.Options{})
	var wait sync.WaitGroup
	for index := 0; index < 10; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			name := fmt.Sprintf("item-%02d", index)
			result, err := tool.Invoke(context.Background(), rig.ToolCall{
				Arguments: `{"action":"load","name":"` + name + `"}`,
			})
			if err != nil || result.IsError {
				t.Errorf("load %s: %v %q", name, err, result.Content)
				return
			}
			if result.Content != "BODY-OF-"+name {
				t.Errorf("load %s = %q", name, result.Content)
			}
		}(index)
	}
	wait.Wait()
	if got := source.loadCount(); got != 10 {
		t.Fatalf("Load 调用 %d 次, want 10", got)
	}
}

// TestSchemaOverride 断言宿主可以整体覆盖 schema（既有工具保持逐字节兼容用）。
func TestSchemaOverride(t *testing.T) {
	custom := map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string"}}}
	tool := mustNew(t, newCountingSource(), shelf.Options{ToolName: "skill", Schema: custom})
	if tool.Schema()["type"] != "object" {
		t.Fatalf("schema 没被覆盖: %v", tool.Schema())
	}
	if _, ok := tool.Schema()["required"]; ok {
		t.Fatalf("覆盖后的 schema 不该有 required: %v", tool.Schema())
	}
}

// TestItemsFromMap 断言静态映射助手可用。
func TestItemsFromMap(t *testing.T) {
	source := shelf.ItemsFromMap(map[string]shelf.Item{
		"b": {Summary: "第二个"},
		"a": {Summary: "第一个"},
	})
	tool := mustNew(t, source, shelf.Options{})
	result := call(t, tool, `{"action":"list"}`)
	lines := strings.Split(result.Content, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "- a — ") {
		t.Fatalf("静态映射的目录不对: %q", result.Content)
	}
}

// TestCancelledContext 断言取消的 ctx 返回错误。
func TestCancelledContext(t *testing.T) {
	tool := mustNew(t, newCountingSource(), shelf.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Invoke(ctx, rig.ToolCall{Arguments: `{"action":"list"}`}); err == nil {
		t.Fatal("取消的 ctx 应当返回 error")
	}
}

// TestErrorsAreToolResults 断言业务错误是 IsError 结果（否则 agent 会把 run 判失败）。
func TestErrorsAreToolResults(t *testing.T) {
	tool := mustNew(t, newCountingSource(), shelf.Options{})
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"action":"load","name":"x"}`})
	if err != nil {
		t.Fatalf("业务错误不该是 go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("业务错误必须是 IsError 的工具结果")
	}
}

// TestSourceErrorPassthrough 断言 Source 的错误原样透出（带工具名前缀）。
func TestSourceErrorPassthrough(t *testing.T) {
	tool := mustNew(t, &errorSource{err: errors.New("disk unreadable")}, shelf.Options{})
	result := call(t, tool, `{"action":"list"}`)
	if !result.IsError || !strings.Contains(result.Content, "disk unreadable") {
		t.Fatalf("Source 错误没透出: %q", result.Content)
	}
	if !strings.HasPrefix(result.Content, "browse: ") {
		t.Fatalf("错误应带工具名前缀: %q", result.Content)
	}
}

// errorSource 总是返回错误。
type errorSource struct{ err error }

func (s *errorSource) List(context.Context) ([]shelf.Item, error) { return nil, s.err }
func (s *errorSource) Search(context.Context, string) ([]shelf.Item, error) {
	return nil, s.err
}
func (s *errorSource) Load(context.Context, string) (string, error) { return "", s.err }

// ---------------------------------------------------------------------------
// 审计用：偏离检测、零单条截断、懒加载计数
// ---------------------------------------------------------------------------

// TestOptionsSurfaceIsFrozen 断言 Options 的字段集合**正好**是设计好的那些。
//
// 这是防回归：`MaxLoadBytes`（单条上限）、`Offset`/`Page`/`Limit`（分页）都被明确
// 否决过，后人想加回来必须先让这条测试红，从而强制过一次评审。
func TestOptionsSurfaceIsFrozen(t *testing.T) {
	want := map[string]bool{
		"ToolName":          true,
		"Description":       true,
		"Schema":            true,
		"MaxListed":         true,
		"MaxSearch":         true,
		"MaxOutputBytes":    true,
		"FormatItem":        true,
		"TrailingNewline":   true,
		"Truncate":          true,
		"Texts":             true,
		"AllowEmptySummary": true,
	}
	optionsType := reflect.TypeOf(shelf.Options{})
	got := make(map[string]bool, optionsType.NumField())
	for index := 0; index < optionsType.NumField(); index++ {
		got[optionsType.Field(index).Name] = true
	}
	for name := range got {
		if !want[name] {
			t.Fatalf("Options 多出了字段 %q：单条上限/分页被明确否决，加回前先过评审", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("Options 少了字段 %q", name)
		}
	}
	for _, banned := range []string{"MaxLoadBytes", "Offset", "Page", "PageSize", "Limit", "MaxItemBytes"} {
		if got[banned] {
			t.Fatalf("Options 不允许有 %q 字段", banned)
		}
	}
	// 默认全局上限是 64 KiB。
	if shelf.DefaultMaxOutputBytes != 64<<10 {
		t.Fatalf("DefaultMaxOutputBytes = %d, want 64 KiB", shelf.DefaultMaxOutputBytes)
	}
}

// TestLoadHasNoPerItemTruncation 断言 load 不做单条截断：
// 4 MiB 正文在全局上限足够大时**完整**返回。
func TestLoadHasNoPerItemTruncation(t *testing.T) {
	const size = 4 << 20 // 4 MiB
	body := strings.Repeat("x", size)
	source := &countingSource{items: []shelf.Item{{Name: "big", Summary: "大条目"}}, bodies: map[string]string{"big": body}}
	tool := mustNew(t, source, shelf.Options{ToolName: "browse", MaxOutputBytes: size + 1024})

	start := time.Now()
	result := call(t, tool, `{"action":"load","name":"big"}`)
	elapsed := time.Since(start)

	if result.IsError {
		t.Fatalf("load: %s", result.Content)
	}
	if len(result.Content) != size {
		t.Fatalf("正文被截断：got %d 字节, want %d", len(result.Content), size)
	}
	if strings.Contains(result.Content, "[truncated") {
		t.Fatalf("不应出现截断标记: %q", result.Content[len(result.Content)-80:])
	}
	t.Logf("4 MiB load 完整返回：%d 字节，耗时 %v", len(result.Content), elapsed)
	if elapsed > 2*time.Second {
		t.Fatalf("4 MiB load 耗时 %v（>2s）", elapsed)
	}
}

// TestLoadLargeBodyHitsOnlyGlobalLimit 断言 4 MiB 正文在默认 64 KiB 全局上限下
// **只**触发全局截断，且文案不提"单条/单页"。
func TestLoadLargeBodyHitsOnlyGlobalLimit(t *testing.T) {
	body := strings.Repeat("y", 4<<20)
	source := &countingSource{items: []shelf.Item{{Name: "big", Summary: "大条目"}}, bodies: map[string]string{"big": body}}
	tool := mustNew(t, source, shelf.Options{ToolName: "browse"}) // 默认 64 KiB

	result := call(t, tool, `{"action":"load","name":"big"}`)
	if result.IsError {
		t.Fatalf("load: %s", result.Content)
	}
	note := result.Content[strings.Index(result.Content, "\n[truncated"):]
	if !strings.Contains(note, "content exceeds 64.0 KiB") {
		t.Fatalf("全局截断文案不对: %q", note)
	}
	for _, banned := range []string{"item is", "page is", "single item", "single page", "单条", "单页"} {
		if strings.Contains(note, banned) {
			t.Fatalf("截断文案出现了单条/单页措辞 %q: %q", banned, note)
		}
	}
	// 截断点之后不应还有正文（说明是全局安全网，而不是"前 N 字节 + 剩余省略"的错觉）。
	if tail := result.Content[strings.Index(result.Content, "[truncated"):]; strings.Count(tail, "y") > 0 {
		t.Fatalf("截断标记之后仍有正文内容")
	}
	t.Logf("4 MiB load 在默认上限下的真实输出尾部: %q", note)
}

// TestLoadCallCountsAcrossActions 断言懒加载计数：
// list/search 期间 Source.Load 必须零调用，load 恰好 1 次。
func TestLoadCallCountsAcrossActions(t *testing.T) {
	source := newCountingSource(
		shelf.Item{Name: "alpha", Summary: "第一个"},
		shelf.Item{Name: "beta", Summary: "第二个"},
	)
	tool := mustNew(t, source, shelf.Options{ToolName: "browse"})

	call(t, tool, `{"action":"list"}`)
	if got := source.loadCount(); got != 0 {
		t.Fatalf("list 之后 Load 调用 %d 次（必须为 0）", got)
	}
	call(t, tool, `{"action":"search","query":"alpha"}`)
	if got := source.loadCount(); got != 0 {
		t.Fatalf("search 之后 Load 调用 %d 次（必须为 0）", got)
	}
	call(t, tool, `{"action":"load","name":"alpha"}`)
	if got := source.loadCount(); got != 1 {
		t.Fatalf("load 之后 Load 调用 %d 次（必须为 1）", got)
	}
	// list/search 各自只调一次自己的方法，不重复。
	if got := atomic.LoadInt32(&source.listN); got != 1 {
		t.Fatalf("List 调用 %d 次, want 1（search 不应复用/再调 List）", got)
	}
	if got := atomic.LoadInt32(&source.searchN); got != 1 {
		t.Fatalf("Search 调用 %d 次, want 1", got)
	}
}
