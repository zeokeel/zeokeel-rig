package patch

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件提供审计用例共用的小工具（不包含断言型用例）。

// newForTest 构造带默认 workspace-write 策略的工具（审计用例共用的构造入口）。
//
// apply_patch 现在强制要求策略（见 New），这里给一份以 root 为唯一可写根的
// workspace-write 策略：它不限制根内写入，因此不会遮盖被测的补丁行为。
// 显式传入的 Option（例如 WithPolicy）排在后面，会覆盖这份默认值。
func newForTest(root string, opts ...Option) rig.Tool {
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	return New(root, append([]Option{WithPolicy(policy)}, opts...)...)
}

// probeInvoke 直接调用工具并返回结果文本。
func probeInvoke(t *testing.T, root, patchText string) (string, bool) {
	t.Helper()
	tool := newForTest(root)
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "c1", Name: ToolName, Arguments: `{"patch":` + mustJSONString(t, patchText) + `}`,
	})
	if err != nil {
		return "GO ERROR: " + err.Error(), true
	}
	return result.Content, result.IsError
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[(r>>4)&0xf])
				b.WriteByte(hex[r&0xf])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "<err: " + err.Error() + ">"
	}
	return string(data)
}

// rigCall 构造一次工具调用。
func rigCall(arguments string) rig.ToolCall {
	return rig.ToolCall{ID: "c1", Name: ToolName, Arguments: arguments}
}
