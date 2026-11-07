package lsp

import (
	"fmt"
	"strings"
)

// 结果格式化。模型可见工具用它把客户端的规范化结构转换为紧凑纯文本；这些函数被导出，
// 便于宿主在自己的工具中一致地渲染 LSP 数据。
//
// 位置按 1 基（line:column）渲染，与工具参数及编辑器显示一致；客户端 API 本身是 LSP 原生
// （0 基）。

// FormatHover 渲染一个 hover 结果。服务端没有 hover 信息时返回 ""。
func FormatHover(hover *HoverResult) string {
	if hover == nil {
		return ""
	}
	return strings.TrimSpace(hover.Contents)
}

// FormatLocations 渲染定义位置，每行一个 "path:line:column"。非文件 URI 原样渲染。
func FormatLocations(locations []Location) string {
	lines := make([]string, 0, len(locations))
	for _, location := range locations {
		lines = append(lines, fmt.Sprintf("%s:%d:%d",
			displayURI(location.URI),
			location.Range.Start.Line+1,
			location.Range.Start.Character+1))
	}
	return strings.Join(lines, "\n")
}

// FormatSymbols 渲染文档符号树，每层缩进两个空格，每个节点一个
// "kind name (line:column)" 条目。
func FormatSymbols(symbols []DocumentSymbol) string {
	var builder strings.Builder
	writeSymbols(&builder, symbols, 0)
	return strings.TrimSuffix(builder.String(), "\n")
}

func writeSymbols(builder *strings.Builder, symbols []DocumentSymbol, depth int) {
	for _, symbol := range symbols {
		indent := strings.Repeat("  ", depth)
		name := strings.TrimSpace(symbol.Name)
		if name == "" {
			name = "(anonymous)"
		}
		fmt.Fprintf(builder, "%s%s %s (%d:%d)\n",
			indent, symbolKindName(symbol.Kind), name,
			symbol.Range.Start.Line+1, symbol.Range.Start.Character+1)
		if detail := strings.TrimSpace(symbol.Detail); detail != "" {
			fmt.Fprintf(builder, "%s  %s\n", indent, detail)
		}
		writeSymbols(builder, symbol.Children, depth+1)
	}
}

// FormatDiagnostics 把诊断渲染为 "severity message (line:column)" 行，每个诊断一行，
// 顺序与服务端发布顺序一致。
func FormatDiagnostics(diagnostics []Diagnostic) string {
	lines := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		message := strings.TrimSpace(diagnostic.Message)
		if message == "" {
			message = "(no message)"
		}
		lines = append(lines, fmt.Sprintf("%s %s (%d:%d)",
			severityName(diagnostic.Severity), message,
			diagnostic.Range.Start.Line+1, diagnostic.Range.Start.Character+1))
	}
	return strings.Join(lines, "\n")
}

// displayURI 在 URI 是文件 URI 时渲染为文件系统路径，其他 scheme 回退为原始 URI。
func displayURI(uri string) string {
	if path, err := URIToPath(uri); err == nil {
		return path
	}
	return uri
}
