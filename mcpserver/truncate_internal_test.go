package mcpserver

// 内部测试：截断只应作用于 text 块。非 text 块（当前 server 不产出，但 MCP 允许
// image/audio/resource）必须原样保留，且不计入大小预算。

import "testing"

func TestTruncateContentLeavesNonTextBlocksAlone(t *testing.T) {
	blocks := []wireContent{
		{Type: "text", Text: "0123456789"},
		{Type: "image", Data: "AAAA", MimeType: "image/png"},
	}
	truncated := truncateContent(blocks, 5)
	if len(truncated) != 2 {
		t.Fatalf("blocks = %d, want 2", len(truncated))
	}
	if truncated[1].Type != "image" || truncated[1].Data != "AAAA" || truncated[1].Meta != nil {
		t.Fatalf("non-text block was modified: %+v", truncated[1])
	}
	if truncated[0].Type != "text" || !truncated[0].Meta["truncated"].(bool) {
		t.Fatalf("text block was not truncated: %+v", truncated[0])
	}
}

func TestTruncateContentIgnoresNonTextWeight(t *testing.T) {
	// 只有非 text 块时永远不需要截断（不占预算）。
	blocks := []wireContent{{Type: "image", Data: "AAAA"}}
	truncated := truncateContent(blocks, 1)
	if truncated[0].Meta != nil || truncated[0].Data != "AAAA" {
		t.Fatalf("non-text-only content was touched: %+v", truncated[0])
	}
}

func TestTruncateContentKeepsRuneBoundary(t *testing.T) {
	// 中文 3 字节/rune：上限落在 rune 中间时必须回退到边界，而不是产出半个字符。
	blocks := []wireContent{{Type: "text", Text: "中文中文中文"}}
	truncated := truncateContent(blocks, 20)
	text := truncated[0].Text
	for index := 0; index < len(text); index++ {
		if text[index] >= 0x80 {
			continue
		}
	}
	// 简单的合法性检查：整体是合法 UTF-8（含注释里的 "…"）。
	if !validUTF8(text) {
		t.Fatalf("truncated text is not valid UTF-8: %q", text)
	}
}

// validUTF8 判断字符串是否是合法 UTF-8（避免在测试里再引入 unicode/utf8 的用法争议）。
func validUTF8(text string) bool {
	for len(text) > 0 {
		_, size := decodeRune(text)
		if size == 0 {
			return false
		}
		text = text[size:]
	}
	return true
}

// decodeRune 是 utf8.DecodeRuneInString 的最小等价物（避免测试文件依赖顺序问题）。
func decodeRune(text string) (rune, int) {
	if len(text) == 0 {
		return 0, 0
	}
	first := text[0]
	switch {
	case first < 0x80:
		return rune(first), 1
	case first&0xE0 == 0xC0:
		if len(text) < 2 || text[1]&0xC0 != 0x80 {
			return 0, 0
		}
		return 0, 2
	case first&0xF0 == 0xE0:
		if len(text) < 3 || text[1]&0xC0 != 0x80 || text[2]&0xC0 != 0x80 {
			return 0, 0
		}
		return 0, 3
	case first&0xF8 == 0xF0:
		if len(text) < 4 || text[1]&0xC0 != 0x80 || text[2]&0xC0 != 0x80 || text[3]&0xC0 != 0x80 {
			return 0, 0
		}
		return 0, 4
	default:
		return 0, 0
	}
}
