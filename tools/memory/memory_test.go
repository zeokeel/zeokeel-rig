package memory_test

// `memory` 工具的用例。重点是**渐进披露**：index 只给元数据、正文只能 read 一页、
// search 只回 meta 行，以及各条上限与错误文案。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
)

// invoke 构造一次工具调用并返回结果（不检查 err，调用方自己看）。
func invoke(t *testing.T, tool rig.Tool, argsJSON string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: tool.Name(), Arguments: argsJSON})
	if err != nil {
		t.Fatalf("Invoke 返回 go error: %v", err)
	}
	return result
}

// call 构造参数 JSON：`call(t, `action`, "index")` 之类的简写用 JSON 字面量。
func jsonArgs(fields map[string]string) string {
	parts := make([]string, 0, len(fields))
	for key, value := range fields {
		parts = append(parts, fmt.Sprintf("%q:%q", key, value))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func newTool(t *testing.T, opts ...memory.Option) (rig.Tool, memory.Wiki) {
	t.Helper()
	wiki := memory.NewMemoryWiki()
	return memory.Tools(wiki, opts...)[0], wiki
}

// TestToolMetadata 断言工具名、描述关键词与 schema 形状（模型可见契约）。
func TestToolMetadata(t *testing.T) {
	tool, _ := newTool(t)
	if tool.Name() != memory.ToolName || memory.ToolName != "memory" {
		t.Fatalf("Name = %q", tool.Name())
	}
	description := strings.ToLower(tool.Description())
	for _, want := range []string{"index", "read", "workspace", "summary", "wiki"} {
		if !strings.Contains(description, want) {
			t.Fatalf("Description 缺少关键词 %q:\n%s", want, tool.Description())
		}
	}

	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema.type = %v", schema["type"])
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("additionalProperties 必须为 false")
	}
	if !reflect.DeepEqual(schema["required"], []string{"action"}) {
		t.Fatalf("required = %v", schema["required"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties 类型不对: %T", schema["properties"])
	}
	wantProps := []string{"action", "name", "title", "summary", "content", "text", "query"}
	if len(properties) != len(wantProps) {
		t.Fatalf("属性数量 = %d, want %d（%v）", len(properties), len(wantProps), wantProps)
	}
	for _, name := range wantProps {
		spec, ok := properties[name].(map[string]any)
		if !ok {
			t.Fatalf("缺少属性 %q", name)
		}
		if spec["type"] != "string" {
			t.Fatalf("%q.type = %v, want string", name, spec["type"])
		}
	}
	actionSpec := properties["action"].(map[string]any)
	wantActions := []string{"index", "read", "write", "append", "search", "delete"}
	if !reflect.DeepEqual(actionSpec["enum"], wantActions) {
		t.Fatalf("action.enum = %v, want %v", actionSpec["enum"], wantActions)
	}
}

// TestIndexEmptyWiki 断言空库不是错误。
func TestIndexEmptyWiki(t *testing.T) {
	tool, _ := newTool(t)
	result := invoke(t, tool, `{"action":"index"}`)
	if result.IsError {
		t.Fatalf("空库 index 不应报错: %s", result.Content)
	}
	if !strings.Contains(result.Content, "还没有记忆") {
		t.Fatalf("空库提示不对: %q", result.Content)
	}
}

// TestIndexShowsMetaOnly 断言 index 只给 `name — summary (age)`，绝不含正文。
func TestIndexShowsMetaOnly(t *testing.T) {
	tool, wiki := newTool(t)
	ctx := context.Background()
	for _, page := range []memory.Page{
		{Name: "build-flaky-tests", Summary: "偶发失败的测试与重试策略", Content: "SECRET-BODY-ONE"},
		{Name: "release-checklist", Summary: "发布前必须跑的检查", Content: "SECRET-BODY-TWO"},
		{Name: "notes", Summary: "杂项", Content: "SECRET-BODY-THREE"},
	} {
		if err := wiki.Write(ctx, page); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	result := invoke(t, tool, `{"action":"index"}`)
	if result.IsError {
		t.Fatalf("index: %s", result.Content)
	}
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	if len(lines) != 3 {
		t.Fatalf("行数 = %d, want 3:\n%s", len(lines), result.Content)
	}
	for _, line := range lines {
		if !strings.Contains(line, " — ") || !strings.Contains(line, "(") || !strings.Contains(line, "前") && !strings.Contains(line, "刚刚") {
			t.Fatalf("元数据行格式不对: %q", line)
		}
	}
	if strings.Contains(result.Content, "SECRET-BODY") {
		t.Fatalf("index 泄漏了正文:\n%s", result.Content)
	}
	t.Logf("index 样例:\n%s", result.Content)
}

// TestIndexCapsAtSixty 断言 60 条上限与 "…and N more" 尾巴。
func TestIndexCapsAtSixty(t *testing.T) {
	tool, wiki := newTool(t)
	ctx := context.Background()
	for index := 0; index < 65; index++ {
		name := fmt.Sprintf("page-%02d", index)
		if err := wiki.Write(ctx, memory.Page{Name: name, Summary: "摘要 " + name, Content: "body"}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	result := invoke(t, tool, `{"action":"index"}`)
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	if len(lines) != 61 {
		t.Fatalf("行数 = %d, want 60+1（尾巴）:\n%s", len(lines), result.Content)
	}
	if !strings.Contains(lines[60], "and 5 more pages") || !strings.Contains(lines[60], "search") {
		t.Fatalf("尾巴不对: %q", lines[60])
	}
	// index 按"最近修改"降序：最后写入的 60 页在列表里，最早写入的 page-00..page-04
	// 被折叠成尾巴里的 "and 5 more"。
	if strings.Contains(result.Content, "page-00 —") {
		t.Fatalf("超过 60 条后仍然逐条输出（最旧的页不该出现）")
	}
}

// TestReadReturnsBody 断言 read 才给正文。
func TestReadReturnsBody(t *testing.T) {
	tool, wiki := newTool(t)
	if err := wiki.Write(context.Background(), memory.Page{
		Name: "build-flaky-tests", Summary: "偶发失败", Content: "# 结论\n\n重试即可。",
	}); err != nil {
		t.Fatal(err)
	}
	result := invoke(t, tool, `{"action":"read","name":"build-flaky-tests"}`)
	if result.IsError {
		t.Fatalf("read: %s", result.Content)
	}
	if !strings.Contains(result.Content, "重试即可。") {
		t.Fatalf("read 没有返回正文: %q", result.Content)
	}
	if !strings.Contains(result.Content, "build-flaky-tests — 偶发失败") {
		t.Fatalf("read 缺少页头: %q", result.Content)
	}
}

// TestReadTruncatesAtEightKiB 断言正文 8 KiB 上限与截断提示。
func TestReadTruncatesAtEightKiB(t *testing.T) {
	tool, wiki := newTool(t)
	body := strings.Repeat("x", 20<<10)
	if err := wiki.Write(context.Background(), memory.Page{Name: "big", Summary: "大页", Content: body}); err != nil {
		t.Fatal(err)
	}
	result := invoke(t, tool, `{"action":"read","name":"big"}`)
	if result.IsError {
		t.Fatalf("read: %s", result.Content)
	}
	if !strings.Contains(result.Content, "[truncated: page is") || !strings.Contains(result.Content, "bytes; use append or read a narrower page]") {
		t.Fatalf("缺少截断提示: %q", result.Content[maxInt(0, len(result.Content)-200):])
	}
	if len(result.Content) > memory.DefaultMaxReadBytes+512 {
		t.Fatalf("正文没有被截断到 8 KiB: %d 字节", len(result.Content))
	}
}

// TestReadErrors 断言 read 的参数与未知页面错误。
func TestReadErrors(t *testing.T) {
	tool, _ := newTool(t)
	result := invoke(t, tool, `{"action":"read"}`)
	if !result.IsError || !strings.Contains(result.Content, "name is required") {
		t.Fatalf("缺 name 应当报错: %q", result.Content)
	}
	result = invoke(t, tool, `{"action":"read","name":"nope"}`)
	if !result.IsError || !strings.Contains(result.Content, "no page named") || !strings.Contains(result.Content, "index") {
		t.Fatalf("未知页面错误文案不对: %q", result.Content)
	}
}

// TestSearchReturnsMetaOnlyAndCaps 断言 search 只回 meta 且最多 20 条。
func TestSearchReturnsMetaOnlyAndCaps(t *testing.T) {
	tool, wiki := newTool(t)
	ctx := context.Background()
	for index := 0; index < 25; index++ {
		name := fmt.Sprintf("hit-%02d", index)
		if err := wiki.Write(ctx, memory.Page{Name: name, Summary: "包含 flaky 的摘要", Content: "正文也提到 flaky"}); err != nil {
			t.Fatal(err)
		}
	}
	result := invoke(t, tool, `{"action":"search","query":"flaky"}`)
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	if len(lines) != 20 {
		t.Fatalf("search 返回 %d 行, want 20:\n%s", len(lines), result.Content)
	}
	if strings.Contains(result.Content, "正文也提到") {
		t.Fatalf("search 泄漏正文:\n%s", result.Content)
	}
}

// TestSearchOrdering 断言标题 > 摘要 > 正文。
func TestSearchOrdering(t *testing.T) {
	tool, wiki := newTool(t)
	ctx := context.Background()
	write := func(page memory.Page) {
		t.Helper()
		if err := wiki.Write(ctx, page); err != nil {
			t.Fatal(err)
		}
	}
	write(memory.Page{Name: "body", Title: "无关", Summary: "无关", Content: "flaky 在正文"})
	write(memory.Page{Name: "summary", Title: "无关", Summary: "flaky 在摘要", Content: "正文"})
	write(memory.Page{Name: "title", Title: "Flaky 测试", Summary: "无关", Content: "正文"})
	result := invoke(t, tool, `{"action":"search","query":"flaky"}`)
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "title") || !strings.HasPrefix(lines[1], "summary") || !strings.HasPrefix(lines[2], "body") {
		t.Fatalf("排序不对:\n%s", result.Content)
	}
}

// TestSearchErrors 断言 query 必填与无匹配的可读输出。
func TestSearchErrors(t *testing.T) {
	tool, _ := newTool(t)
	if result := invoke(t, tool, `{"action":"search"}`); !result.IsError || !strings.Contains(result.Content, "query is required") {
		t.Fatalf("缺 query 应当报错: %q", result.Content)
	}
	result := invoke(t, tool, `{"action":"search","query":"nothing-matches"}`)
	if result.IsError {
		t.Fatalf("无匹配不应报错: %q", result.Content)
	}
	if !strings.Contains(result.Content, "没有匹配") || !strings.Contains(result.Content, "index") {
		t.Fatalf("无匹配文案不对: %q", result.Content)
	}
}

// TestWriteRequiresSummary 断言 summary 必填并解释原因。
func TestWriteRequiresSummary(t *testing.T) {
	tool, _ := newTool(t)
	result := invoke(t, tool, `{"action":"write","name":"p","content":"body"}`)
	if !result.IsError {
		t.Fatalf("缺 summary 应当报错: %q", result.Content)
	}
	if !strings.Contains(result.Content, "summary is required") || !strings.Contains(result.Content, "index only shows the summary") {
		t.Fatalf("错误文案应解释原因: %q", result.Content)
	}
}

// TestWriteThenIndex 断言写入后立刻能在 index 看到。
func TestWriteThenIndex(t *testing.T) {
	tool, _ := newTool(t)
	if result := invoke(t, tool, `{"action":"write","name":"build-flaky-tests","title":"Build: flaky","summary":"偶发失败与重试","content":"# 结论\n重试"} `); result.IsError {
		t.Fatalf("write: %s", result.Content)
	}
	result := invoke(t, tool, `{"action":"index"}`)
	if !strings.Contains(result.Content, "build-flaky-tests — 偶发失败与重试") {
		t.Fatalf("index 没有新页: %q", result.Content)
	}
}

// TestWriteInvalidName 断言 slug 校验（不静默改写）。
func TestWriteInvalidName(t *testing.T) {
	tool, _ := newTool(t)
	result := invoke(t, tool, `{"action":"write","name":"Build Notes","summary":"s","content":"c"}`)
	if !result.IsError {
		t.Fatalf("非法 slug 应当报错: %q", result.Content)
	}
	if !strings.Contains(result.Content, "invalid page name") || !strings.Contains(result.Content, "build-flaky-tests") {
		t.Fatalf("错误文案应给出合法示例: %q", result.Content)
	}
}

// TestWriteOverwriteAddsLog 断言覆盖写保留审计行。
func TestWriteOverwriteAddsLog(t *testing.T) {
	tool, _ := newTool(t)
	if result := invoke(t, tool, `{"action":"write","name":"p","summary":"v1","content":"第一版"} `); result.IsError {
		t.Fatalf("write: %s", result.Content)
	}
	if result := invoke(t, tool, `{"action":"write","name":"p","summary":"v2","content":"第二版"} `); result.IsError {
		t.Fatalf("rewrite: %s", result.Content)
	}
	result := invoke(t, tool, `{"action":"read","name":"p"}`)
	if !strings.Contains(result.Content, "第二版") || !strings.Contains(result.Content, "rewritten (was") {
		t.Fatalf("覆盖写没有留下审计行: %q", result.Content)
	}
}

// TestAppendCreatesAndExtendsLog 断言 append 建 Log 段并带日期。
func TestAppendCreatesAndExtendsLog(t *testing.T) {
	tool, _ := newTool(t)
	if result := invoke(t, tool, `{"action":"write","name":"p","summary":"s","content":"正文"} `); result.IsError {
		t.Fatalf("write: %s", result.Content)
	}
	if result := invoke(t, tool, `{"action":"append","name":"p","text":"第一次观察"} `); result.IsError {
		t.Fatalf("append: %s", result.Content)
	}
	if result := invoke(t, tool, `{"action":"append","name":"p","text":"第二次观察"} `); result.IsError {
		t.Fatalf("append: %s", result.Content)
	}
	result := invoke(t, tool, `{"action":"read","name":"p"}`)
	if strings.Count(result.Content, "## Log") != 1 {
		t.Fatalf("Log 段应当只有一个: %q", result.Content)
	}
	if !strings.Contains(result.Content, "第一次观察") || !strings.Contains(result.Content, "第二次观察") {
		t.Fatalf("append 内容缺失: %q", result.Content)
	}
}

// TestAppendErrors 断言 append 的参数与未知页面错误。
func TestAppendErrors(t *testing.T) {
	tool, _ := newTool(t)
	if result := invoke(t, tool, `{"action":"append","name":"p"}`); !result.IsError || !strings.Contains(result.Content, "text is required") {
		t.Fatalf("缺 text 应当报错: %q", result.Content)
	}
	if result := invoke(t, tool, `{"action":"append","name":"nope","text":"x"}`); !result.IsError || !strings.Contains(result.Content, "no page named") {
		t.Fatalf("未知页面应当报错: %q", result.Content)
	}
}

// TestDeleteAndErrors 断言删除与未知页面。
func TestDeleteAndErrors(t *testing.T) {
	tool, _ := newTool(t)
	if result := invoke(t, tool, `{"action":"write","name":"p","summary":"s","content":"c"} `); result.IsError {
		t.Fatalf("write: %s", result.Content)
	}
	if result := invoke(t, tool, `{"action":"delete","name":"p"}`); result.IsError || !strings.Contains(result.Content, "deleted page p") {
		t.Fatalf("delete: %q", result.Content)
	}
	if result := invoke(t, tool, `{"action":"index"}`); !strings.Contains(result.Content, "还没有记忆") {
		t.Fatalf("删除后 index 应为空: %q", result.Content)
	}
	if result := invoke(t, tool, `{"action":"delete","name":"p"}`); !result.IsError || !strings.Contains(result.Content, "index") {
		t.Fatalf("重复删除应当报错并提示 index: %q", result.Content)
	}
}

// TestActionValidation 断言缺失/未知 action。
func TestActionValidation(t *testing.T) {
	tool, _ := newTool(t)
	if result := invoke(t, tool, `{}`); !result.IsError || !strings.Contains(result.Content, "action is required") {
		t.Fatalf("缺 action: %q", result.Content)
	}
	if result := invoke(t, tool, `{"action":"nope"}`); !result.IsError || !strings.Contains(result.Content, "unknown action") {
		t.Fatalf("未知 action: %q", result.Content)
	}
}

// TestMaxOutputTruncatesAtItemLevel 断言 32 KiB 上限按整行截断（不切断半行）。
func TestMaxOutputTruncatesAtItemLevel(t *testing.T) {
	tool, wiki := newTool(t, memory.WithMaxOutputBytes(200))
	ctx := context.Background()
	for index := 0; index < 30; index++ {
		name := fmt.Sprintf("page-%02d", index)
		if err := wiki.Write(ctx, memory.Page{Name: name, Summary: "摘要 " + name, Content: "body"}); err != nil {
			t.Fatal(err)
		}
	}
	result := invoke(t, tool, `{"action":"index"}`)
	if !strings.Contains(result.Content, "[truncated: output exceeded 200 bytes") {
		t.Fatalf("缺少输出截断提示: %q", result.Content)
	}
	for _, line := range strings.Split(result.Content, "\n") {
		if line == "" || strings.HasPrefix(line, "[truncated") {
			continue
		}
		if !strings.Contains(line, " — ") {
			t.Fatalf("出现了半行: %q", line)
		}
	}
	if len(result.Content) > 200+120 {
		t.Fatalf("输出没有被限制: %d 字节", len(result.Content))
	}
}

// TestUnicodeAndEmojiSurvive 断言中文/emoji 的摘要与正文不被破坏。
func TestUnicodeAndEmojiSurvive(t *testing.T) {
	tool, _ := newTool(t)
	if result := invoke(t, tool, jsonArgs(map[string]string{
		"action": "write", "name": "emoji", "summary": "🚀 发布流程（含中文）", "content": "正文 emoji ✅",
	})); result.IsError {
		t.Fatalf("write: %s", result.Content)
	}
	if result := invoke(t, tool, `{"action":"index"}`); !strings.Contains(result.Content, "🚀 发布流程（含中文）") {
		t.Fatalf("index 丢了 emoji: %q", result.Content)
	}
	if result := invoke(t, tool, `{"action":"read","name":"emoji"}`); !strings.Contains(result.Content, "✅") {
		t.Fatalf("read 丢了 emoji: %q", result.Content)
	}
}

// TestContextCanceled 断言取消的 ctx 直接返回 error。
func TestContextCanceled(t *testing.T) {
	tool, _ := newTool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Invoke(ctx, rig.ToolCall{Arguments: `{"action":"index"}`}); err == nil {
		t.Fatal("取消的 ctx 应当返回 error")
	}
}

// TestWikiFromStoreAdapter 断言宿主的扁平 rig.MemoryStore 仍可用（只读迁移）。
func TestWikiFromStoreAdapter(t *testing.T) {
	store := &flatStore{items: []rig.Memory{
		{ID: "mem-1", Text: "第一条记忆：构建很慢", Created: timeForTest()},
		{ID: "mem-2", Text: "第二条记忆：发布要先跑 e2e", Created: timeForTest()},
	}}
	tool := memory.Tools(memory.WikiFromStore(store))[0]
	index := invoke(t, tool, `{"action":"index"}`)
	if index.IsError || !strings.Contains(index.Content, "mem-1") || !strings.Contains(index.Content, "构建很慢") {
		t.Fatalf("适配器 index 不对: %q", index.Content)
	}
	read := invoke(t, tool, `{"action":"read","name":"mem-1"}`)
	if read.IsError || !strings.Contains(read.Content, "第一条记忆") {
		t.Fatalf("适配器 read 不对: %q", read.Content)
	}
}

// TestWorkspacePartitionedFileWiki 断言按工作区分区（工具层视角）。
func TestWorkspacePartitionedFileWiki(t *testing.T) {
	stateDir := t.TempDir()
	rootA := filepath.Join(t.TempDir(), "alpha")
	rootB := filepath.Join(t.TempDir(), "beta")
	for _, dir := range []string{rootA, rootB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wikiA, err := memory.NewWorkspaceWiki(stateDir, rootA)
	if err != nil {
		t.Fatalf("NewWorkspaceWiki(A): %v", err)
	}
	wikiB, err := memory.NewWorkspaceWiki(stateDir, rootB)
	if err != nil {
		t.Fatalf("NewWorkspaceWiki(B): %v", err)
	}
	toolA := memory.Tools(wikiA)[0]
	toolB := memory.Tools(wikiB)[0]
	if result := invoke(t, toolA, `{"action":"write","name":"only-a","summary":"A 的记忆","content":"body"} `); result.IsError {
		t.Fatalf("write A: %s", result.Content)
	}
	if result := invoke(t, toolB, `{"action":"index"}`); !strings.Contains(result.Content, "还没有记忆") {
		t.Fatalf("B 看到了 A 的记忆: %q", result.Content)
	}
	if result := invoke(t, toolA, `{"action":"index"}`); !strings.Contains(result.Content, "only-a") {
		t.Fatalf("A 看不到自己的记忆: %q", result.Content)
	}
}

// TestConcurrentToolWrites 并发 20 次写不同页（-race 也跑）。
func TestConcurrentToolWrites(t *testing.T) {
	tool, _ := newTool(t)
	const workers = 20
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			args := jsonArgs(map[string]string{
				"action": "write", "name": fmt.Sprintf("concurrent-%02d", index),
				"summary": "并发写", "content": "body",
			})
			if result := invoke(t, tool, args); result.IsError {
				t.Errorf("write: %s", result.Content)
			}
		}(index)
	}
	wait.Wait()
	if result := invoke(t, tool, `{"action":"index"}`); !strings.Contains(result.Content, "concurrent-19") {
		t.Fatalf("并发写丢失页面: %q", result.Content)
	}
}

// flatStore 是最小 rig.MemoryStore 实现（测试宿主自定义存储）。
type flatStore struct {
	mu    sync.Mutex
	items []rig.Memory
	seq   int
}

func (s *flatStore) List(context.Context) ([]rig.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rig.Memory(nil), s.items...), nil
}

func (s *flatStore) Add(_ context.Context, entry rig.Memory) (rig.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("mem-%d", s.seq)
	}
	s.items = append(s.items, entry)
	return entry, nil
}

func (s *flatStore) Remove(_ context.Context, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, entry := range s.items {
		if entry.ID == id {
			s.items = append(s.items[:index], s.items[index+1:]...)
			return true
		}
	}
	return false
}

func (s *flatStore) Search(_ context.Context, query string) ([]rig.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	needle := strings.ToLower(query)
	var matches []rig.Memory
	for _, entry := range s.items {
		if strings.Contains(strings.ToLower(entry.Text), needle) {
			matches = append(matches, entry)
		}
	}
	return matches, nil
}

// TestErrorsAreToolResults 断言错误以 IsError 的工具结果返回（不是 go error），
// 否则 agent 会把整个 run 判为失败。
func TestErrorsAreToolResults(t *testing.T) {
	tool, _ := newTool(t)
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		Arguments: `{"action":"read","name":"missing"}`,
	})
	if err != nil {
		t.Fatalf("业务错误不该是 go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("业务错误必须是 IsError 的工具结果")
	}
}

// TestMaxReadOptionOverride 断言 WithMaxReadBytes 生效。
func TestMaxReadOptionOverride(t *testing.T) {
	tool, wiki := newTool(t, memory.WithMaxReadBytes(32))
	if err := wiki.Write(context.Background(), memory.Page{Name: "p", Summary: "s", Content: strings.Repeat("y", 100)}); err != nil {
		t.Fatal(err)
	}
	result := invoke(t, tool, `{"action":"read","name":"p"}`)
	if !strings.Contains(result.Content, "[truncated: page is 100 bytes") {
		t.Fatalf("自定义上限没生效: %q", result.Content)
	}
}

// TestIndexAgeRendering 断言"多久以前"的渲染（新页显示刚刚/分钟前）。
func TestIndexAgeRendering(t *testing.T) {
	tool, wiki := newTool(t)
	if err := wiki.Write(context.Background(), memory.Page{Name: "fresh", Summary: "刚写的", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	result := invoke(t, tool, `{"action":"index"}`)
	if !strings.Contains(result.Content, "刚刚") && !strings.Contains(result.Content, "分钟前") {
		t.Fatalf("新页的时间渲染不对: %q", result.Content)
	}
}

// timeForTest 返回一个固定时间（适配器测试用）。
func timeForTest() (stamp time.Time) {
	return time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
}

// maxInt 是测试内的小工具。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestReadTruncationIsUTF8Safe 断言 8 KiB 截断切在字符边界上（不会切出半个汉字/emoji）。
func TestReadTruncationIsUTF8Safe(t *testing.T) {
	tool, wiki := newTool(t)
	// 3 字节一个汉字，8192/3 不整除，截断点必然落在字符中间——实现必须回退到边界。
	body := strings.Repeat("记", 3000)
	if err := wiki.Write(context.Background(), memory.Page{Name: "cjk", Summary: "中文大页", Content: body}); err != nil {
		t.Fatal(err)
	}
	result := invoke(t, tool, `{"action":"read","name":"cjk"}`)
	if result.IsError {
		t.Fatalf("read: %s", result.Content)
	}
	payload := result.Content
	if index := strings.Index(payload, "\n[truncated:"); index >= 0 {
		payload = payload[:index]
	}
	// 去掉页头（两行）后，剩下的部分必须是合法 UTF-8 且由完整汉字组成。
	lines := strings.SplitN(payload, "\n\n", 2)
	if len(lines) != 2 {
		t.Fatalf("read 输出缺少页头/正文分隔: %q", result.Content[:minInt(len(result.Content), 120)])
	}
	text := lines[1]
	if !utf8.ValidString(text) {
		t.Fatalf("截断切出了半个字符（正文非法 UTF-8）")
	}
	if strings.Trim(text, "记") != "" {
		t.Fatalf("正文里出现了半个汉字: %q", text[len(text)-8:])
	}
}

// minInt 是测试内的小工具。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestUnknownActionPointsAtTheList 断言"用错的那一刻再教"：常驻描述里不再列全细节，
// 未知 action 的错误必须给出完整清单（这是瘦身 schema 的前提）。
func TestUnknownActionPointsAtTheList(t *testing.T) {
	tool, _ := newTool(t)
	result := invoke(t, tool, `{"action":"fetch"}`)
	if !result.IsError {
		t.Fatalf("未知 action 应当报错: %q", result.Content)
	}
	want := `memory: unknown action "fetch"; use one of index/read/search/write/append/delete`
	if result.Content != want {
		t.Fatalf("错误文案 = %q, want %q", result.Content, want)
	}
	// 缺 action 也要指路（保持原有文案）。
	result = invoke(t, tool, `{}`)
	if !result.IsError || !strings.Contains(result.Content, "action is required") {
		t.Fatalf("缺 action 文案 = %q", result.Content)
	}
}

// TestSchemaStaysLean 断言常驻 schema 的体积上限（它是每个回合都付的钱）。
//
// 2265 字节（≈571 tokens）曾经占零配置 active 集的 13%：description 把 6 个 action
// 全解释了一遍。现在压到 ~1.1 KB，细节交给运行时错误。这条测试防回涨。
func TestSchemaStaysLean(t *testing.T) {
	tool, _ := newTool(t)
	encoded, err := json.Marshal(struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}{Name: tool.Name(), Description: tool.Description(), Parameters: tool.Schema()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const limit = 1500
	if len(encoded) > limit {
		t.Fatalf("memory schema = %d 字节（≈%d tokens），超过上限 %d。常驻工具的体积是每个回合的成本："+
			"把细节移到运行时错误，或把 action 拆成独立工具。", len(encoded), len(encoded)/4, limit)
	}
	t.Logf("memory schema = %d 字节 ≈ %d tokens", len(encoded), len(encoded)/4)
	// 关键信息不能因为瘦身丢掉：action 枚举、七个参数、additionalProperties=false。
	properties, _ := tool.Schema()["properties"].(map[string]any)
	if len(properties) != 7 {
		t.Fatalf("参数数量 = %d, want 7", len(properties))
	}
	action, _ := properties["action"].(map[string]any)
	enum, _ := action["enum"].([]string)
	if len(enum) != 6 {
		t.Fatalf("action 枚举 = %v", action["enum"])
	}
}
