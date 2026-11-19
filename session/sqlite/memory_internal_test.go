package sqlite

// 内部用例：firstLine 是"页已有但摘要为空"时补索引行的最后一道关，它必须切在
// UTF-8 字符边界上——按字节切会把一个汉字劈成两半，index 里就出现乱码。

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig/memory"
)

func TestFirstLineKeepsUTF8Intact(t *testing.T) {
	// 一条远超摘要上限的中文行：按字节截断必然切在字符中间。
	line := strings.Repeat("长", memory.MaxSummaryBytes) // 每字 3 字节，远超上限
	got := firstLine(line)
	if !utf8.ValidString(got) {
		t.Fatalf("firstLine 产生了非法 UTF-8：%q", got)
	}
	if len(got) > memory.MaxSummaryBytes {
		t.Fatalf("firstLine 超过上限：%d 字节", len(got))
	}
	if got == "" || !strings.HasPrefix(line, got) {
		t.Fatalf("firstLine 必须是原行的前缀：%q", got)
	}
}

func TestFirstLineTakesTheFirstNonEmptyLine(t *testing.T) {
	if got := firstLine("\n\n  第一条  \n第二条"); got != "第一条" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine("   "); got != "log" {
		t.Fatalf("空文本的兜底 = %q, want log", got)
	}
}
