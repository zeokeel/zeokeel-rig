package discovery_test

// tool_search 的行为契约：它自己也是"每个请求都要占 token"的工具，因此 schema 必须
// 小；清单必须短而有界；激活必须把 host 的幂等语义原样转述给模型（activated /
// unknown），而不是自己猜。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/discovery"
)

// call 构造一次工具调用。
func call(t *testing.T, arguments string) rig.ToolCall {
	t.Helper()
	return rig.ToolCall{ID: "c1", Name: discovery.ToolSearch, Arguments: arguments}
}

// invoke 调用工具并要求成功（错误由工具自身以 IsError 表达）。
func invoke(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), call(t, arguments))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return result
}

// fixture 构造一个含 3 个 inactive 工具的 registry。
func fixture(t *testing.T, limit int) (rig.Tool, *[]string) {
	t.Helper()
	var activatedCalls []string
	tool := discovery.New(discovery.Options{
		Limit: limit,
		List: func(context.Context) []discovery.Item {
			return []discovery.Item{
				{Name: "web_fetch", Summary: "Fetch a URL over HTTP."},
				{Name: "web_search", Summary: "Search the web."},
				{Name: "pdf_extract", Summary: "Extract text from a PDF file.\nSecond line."},
			}
		},
		Activate: func(_ context.Context, names []string) ([]string, []string) {
			activatedCalls = append(activatedCalls, names...)
			var activated, unknown []string
			for _, name := range names {
				switch name {
				case "web_fetch", "web_search", "pdf_extract":
					activated = append(activated, name)
				default:
					unknown = append(unknown, name)
				}
			}
			return activated, unknown
		},
	})
	return tool, &activatedCalls
}

// TestSchemaIsMinimal：schema 只有 query / activate 两个可选字段，且字节数很小
// （它每个请求都要发）。
func TestSchemaIsMinimal(t *testing.T) {
	tool, _ := fixture(t, 0)
	schema := tool.Schema()
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if len(encoded) > 600 {
		t.Fatalf("schema = %d 字节，太大（%s）", len(encoded), encoded)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) != 2 {
		t.Fatalf("properties = %#v, want 恰好 query 与 activate", schema["properties"])
	}
	for _, name := range []string{"query", "activate"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("缺少字段 %q: %v", name, properties)
		}
	}
	if required, exists := schema["required"]; exists {
		if list, _ := required.([]string); len(list) != 0 {
			t.Fatalf("schema 不该有必填字段: %v", required)
		}
	}
	if name := tool.Name(); name != discovery.ToolSearch {
		t.Fatalf("Name = %q", name)
	}
	if description := tool.Description(); len(description) > 400 {
		t.Fatalf("Description = %d 字符，太长", len(description))
	}
}

// TestListInactiveTools：空参数 → 列出全部 inactive 工具。
func TestListInactiveTools(t *testing.T) {
	tool, _ := fixture(t, 0)
	result := invoke(t, tool, `{}`)
	if result.IsError {
		t.Fatalf("IsError = true: %s", result.Content)
	}
	for _, want := range []string{"web_fetch", "web_search", "pdf_extract"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("清单缺少 %q:\n%s", want, result.Content)
		}
	}
	// 每条带一句话说明；摘要只取第一行。
	if !strings.Contains(result.Content, "— Fetch a URL over HTTP.") {
		t.Fatalf("缺少说明行:\n%s", result.Content)
	}
	if strings.Contains(result.Content, "Second line.") {
		t.Fatalf("摘要没有被截到首行:\n%s", result.Content)
	}
}

// TestQueryFiltersByNameAndSummary：query 同时匹配名字与描述。
func TestQueryFiltersByNameAndSummary(t *testing.T) {
	tool, _ := fixture(t, 0)
	byName := invoke(t, tool, `{"query":"WEB"}`).Content
	if !strings.Contains(byName, "web_fetch") || !strings.Contains(byName, "web_search") {
		t.Fatalf("按名字过滤失败:\n%s", byName)
	}
	if strings.Contains(byName, "pdf_extract") {
		t.Fatalf("不该出现 pdf_extract:\n%s", byName)
	}
	bySummary := invoke(t, tool, `{"query":"pdf"}`).Content
	if !strings.Contains(bySummary, "pdf_extract") {
		t.Fatalf("按摘要过滤失败:\n%s", bySummary)
	}
	noMatch := invoke(t, tool, `{"query":"zzz"}`).Content
	if !strings.Contains(noMatch, "no tool matches") {
		t.Fatalf("无匹配的文案 = %q", noMatch)
	}
}

// TestActivateReportsActivatedAndUnknown：激活把 host 的幂等语义原样转述。
func TestActivateReportsActivatedAndUnknown(t *testing.T) {
	tool, calls := fixture(t, 0)
	result := invoke(t, tool, `{"activate":["web_fetch","nope"]}`)
	if result.IsError {
		t.Fatalf("IsError = true: %s", result.Content)
	}
	if !strings.Contains(result.Content, "activated: web_fetch") {
		t.Fatalf("content = %q，缺少 activated", result.Content)
	}
	if !strings.Contains(result.Content, "unknown: nope") {
		t.Fatalf("content = %q，缺少 unknown", result.Content)
	}
	if !strings.Contains(result.Content, "next message") {
		t.Fatalf("content = %q，缺少「下一轮可用」的提示", result.Content)
	}
	if len(*calls) != 2 {
		t.Fatalf("传给 activator 的名字 = %v", *calls)
	}
}

// TestActivateDuplicateNamesArePassedThrough：重复名字原样交给 host（幂等由 host
// 保证，工具层不去重——去重会让 host 无法如实报告"No-op"）。
func TestActivateDuplicateNamesArePassedThrough(t *testing.T) {
	tool, calls := fixture(t, 0)
	result := invoke(t, tool, `{"activate":["web_fetch","web_fetch"]}`)
	if result.IsError {
		t.Fatalf("IsError = true: %s", result.Content)
	}
	if len(*calls) != 2 {
		t.Fatalf("传入名字 = %v, want 原样两条", *calls)
	}
	if !strings.Contains(result.Content, "activated: web_fetch, web_fetch") {
		t.Fatalf("content = %q", result.Content)
	}
}

// TestEmptyActivateListFallsBackToListing：空数组等价于未提供。
func TestEmptyActivateListFallsBackToListing(t *testing.T) {
	tool, calls := fixture(t, 0)
	result := invoke(t, tool, `{"activate":[]}`)
	if result.IsError {
		t.Fatalf("IsError = true: %s", result.Content)
	}
	if len(*calls) != 0 {
		t.Fatalf("空数组不该调用 activator: %v", *calls)
	}
	if !strings.Contains(result.Content, "web_fetch") {
		t.Fatalf("应回退到清单:\n%s", result.Content)
	}
}

// TestListIsTruncated：超过上限时截断并说明还剩多少。
//
// 提示文本是模型可见文案，必须是英文（仓库硬规则：模型可见的 tool description /
// schema / 错误文案不翻译成中文）。
func TestListIsTruncated(t *testing.T) {
	tool, _ := fixture(t, 2)
	result := invoke(t, tool, `{}`)
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	if len(lines) != 3 {
		t.Fatalf("行数 = %d, want 2 条 + 1 行提示:\n%s", len(lines), result.Content)
	}
	if !strings.Contains(lines[2], "and 1 more") || !strings.Contains(lines[2], "query") {
		t.Fatalf("截断提示 = %q", lines[2])
	}
	for _, r := range lines[2] {
		if r > 0x7f && r != '…' {
			t.Fatalf("截断提示含非英文字符 %q: %q", r, lines[2])
		}
	}
}

// TestEmptyRegistry：没有可发现工具时给出明确说明（而不是空字符串）。
func TestEmptyRegistry(t *testing.T) {
	tool := discovery.New(discovery.Options{
		List:     func(context.Context) []discovery.Item { return nil },
		Activate: func(context.Context, []string) ([]string, []string) { return nil, nil },
	})
	result := invoke(t, tool, `{}`)
	if result.IsError {
		t.Fatalf("IsError = true: %s", result.Content)
	}
	if !strings.Contains(result.Content, "already active") {
		t.Fatalf("content = %q", result.Content)
	}
}

// TestMissingCallbacksFailReadably：宿主没注入回调时不 panic，返回可读错误。
func TestMissingCallbacksFailReadably(t *testing.T) {
	tool := discovery.New(discovery.Options{})
	listed := invoke(t, tool, `{}`)
	if !listed.IsError || !strings.Contains(listed.Content, "no discovery registry is configured") {
		t.Fatalf("清单 = (%v, %q)", listed.IsError, listed.Content)
	}
	activated := invoke(t, tool, `{"activate":["x"]}`)
	if !activated.IsError || !strings.Contains(activated.Content, "no activator is configured") {
		t.Fatalf("激活 = (%v, %q)", activated.IsError, activated.Content)
	}
}

// TestBadArgumentsAreReadable：参数解不开时给可读错误，不 panic。
func TestBadArgumentsAreReadable(t *testing.T) {
	tool, _ := fixture(t, 0)
	result := invoke(t, tool, `{"activate": "not-an-array"}`)
	if !result.IsError {
		t.Fatalf("want IsError, got %q", result.Content)
	}
	if !strings.Contains(result.Content, discovery.ToolSearch) {
		t.Fatalf("错误应带工具名前缀: %q", result.Content)
	}
}

// TestInvokeHonorsCanceledContext：取消传播。
func TestInvokeHonorsCanceledContext(t *testing.T) {
	tool, _ := fixture(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Invoke(ctx, call(t, `{}`)); err == nil {
		t.Fatal("取消的 ctx 必须返回错误")
	}
}
