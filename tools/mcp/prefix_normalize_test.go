package mcp

// 前缀规范化的唯一规则：kit（`effectiveMCPPrefix`）与 mcp 包（工具名生成）必须给出
// 同一个结果，否则带空白的前缀会生成"带空白的工具名"，而调用方用 trim 后的前缀去
// 匹配服务端绑定工具时一个都摘不出来。

import (
	"slices"
	"testing"
)

// TestNormalizePrefixTrimsAndKeepsSingleSeparator：trim 空白、空串落默认值、非 "__"
// 结尾补齐后缀分隔符。
func TestNormalizePrefixTrimsAndKeepsSingleSeparator(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", DefaultPrefix},
		{"   ", DefaultPrefix},
		{" mcp__ ", "mcp__"},
		{"mcp__", "mcp__"},
		{"\tmcp__\n", "mcp__"},
		{"mcp", "mcp__"},
		{" custom ", "custom__"},
	}
	for _, tc := range cases {
		if got := NormalizePrefix(tc.in); got != tc.want {
			t.Fatalf("NormalizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildToolSetUsesNormalizedPrefix：生成出来的工具名不带空白，且与 kit 用
// NormalizePrefix 得到的前缀可匹配。
func TestBuildToolSetUsesNormalizedPrefix(t *testing.T) {
	tools, _ := buildToolSet([]serverTools{{
		server: "fake",
		infos:  []ToolInfo{{Name: "echo"}},
	}}, " mcp__ ")
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	if !slices.Contains(names, "mcp__fake__echo") {
		t.Fatalf("带空白前缀生成的工具名没有规范化: %v", names)
	}
}
