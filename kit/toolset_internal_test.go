package kit

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// namedTool 是一个只用于工具集装配测试的最小 rig.Tool。
type namedTool struct {
	name string
}

func (t namedTool) Name() string           { return t.name }
func (t namedTool) Description() string    { return t.name }
func (t namedTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (namedTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{}, nil
}

// TestAppendUniqueTools 覆盖可选工具并入默认工具集时的去重与 nil 处理：宿主或
// tools/full.All 已经提供同名工具时不能再追加一份（agent.New 会拒绝重名）。
func TestAppendUniqueTools(t *testing.T) {
	existing := []rig.Tool{namedTool{name: "read_file"}, namedTool{name: "sandbox_status"}}

	got := appendUniqueTools(existing,
		nil, // nil 工具跳过，由 agent.New 负责报错
		namedTool{name: "request_sandbox_access"},
		namedTool{name: "sandbox_status"}, // 已存在，跳过
	)

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3: %v", len(got), got)
	}
	names := make([]string, 0, len(got))
	for _, tool := range got {
		names = append(names, tool.Name())
	}
	want := []string{"read_file", "sandbox_status", "request_sandbox_access"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v (existing tools keep their order)", names, want)
		}
	}

	// 空工具集 + 无新工具时保持 nil（不改变 nil 切片的语义）。
	if got := appendUniqueTools(nil); got != nil {
		t.Fatalf("appendUniqueTools(nil) = %v, want nil", got)
	}
}
