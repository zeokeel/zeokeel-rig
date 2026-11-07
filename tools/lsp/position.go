package lsp

import (
	"fmt"
	"strings"
)

// UTF16Len 返回把 s 编码为 UTF-16 所需的 code unit 数。LSP 位置按这些单元计数：BMP 之外
// 的字符（emoji、部分 CJK 扩展）占两个，BMP 内的字符各占一个。
func UTF16Len(s string) int {
	units := 0
	for _, r := range s {
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// ValidatePosition 用文档文本校验工具侧的 1 基行号与 UTF-16 字符列，返回要发送给服务端的
// 0 基 LSP 位置。越界坐标返回稳定、可读的错误，而不是转发给语言服务器。
//
// 语义按文档精确计数：行号范围为 1..len(lines)；character 是 1 基 UTF-16 code unit 列，
// 允许等于该行宽度加一（表示行尾）。CRLF、CR 与 LF 都算换行，文件末尾换行会产生一个最后
// 的空行（编辑器中可定位的空行）。
func ValidatePosition(text string, line, character int) (Position, error) {
	if line < 1 {
		return Position{}, fmt.Errorf("line %d is invalid: lines are 1-based", line)
	}
	if character < 1 {
		return Position{}, fmt.Errorf("character %d is invalid: characters are 1-based UTF-16 code units", character)
	}
	lines := documentLines(text)
	if line > len(lines) {
		return Position{}, fmt.Errorf("line %d is outside the file (it has %d line(s))", line, len(lines))
	}
	width := UTF16Len(lines[line-1])
	if character > width+1 {
		return Position{}, fmt.Errorf("character %d is outside line %d (it has %d UTF-16 code unit(s))", character, line, width)
	}
	return Position{Line: line - 1, Character: character - 1}, nil
}

// documentLines 按 LSP 文档的方式切分文本：\r\n、\r 与 \n 都结束一行；文件末尾的换行会
// 产生一个最后的空行，使文档末尾位置可被定位。
func documentLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Split(text, "\n")
}
