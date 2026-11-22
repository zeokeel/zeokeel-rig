package host

// 包内测试覆盖 filterTools 的防御性分支：rig lsp.Tools 当前总是返回六个
// 唯一非空工具，所以外部测试无法触发 nil/空名称/重名过滤。

import (
	"context"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
)

type stubTool struct{ name string }

func (t stubTool) Name() string           { return t.name }
func (t stubTool) Description() string    { return "stub" }
func (t stubTool) Schema() map[string]any { return nil }
func (t stubTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{}, nil
}

func TestFilterToolsDropsNilEmptyAndDuplicate(t *testing.T) {
	host, err := New(Config{Command: "stub"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	filtered := host.filterTools([]rig.Tool{
		nil,
		stubTool{name: ""},
		stubTool{name: "  "},
		stubTool{name: "lsp_hover"},
		stubTool{name: "lsp_hover"},
		stubTool{name: " lsp_definition "},
	})
	if len(filtered) != 2 {
		t.Fatalf("filtered = %v, want two unique tools", filtered)
	}
	if filtered[0].Name() != "lsp_hover" || filtered[1].Name() != " lsp_definition " {
		t.Fatalf("filtered names = %q/%q", filtered[0].Name(), filtered[1].Name())
	}
}
