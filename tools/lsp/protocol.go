package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Position 是 0 基的 LSP 位置。character 按 UTF-16 code unit 计数（协议要求）；本包
// 从不重新解释它。
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range 是一个 LSP 范围。
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location 是语言服务器返回的一个源码范围。
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// HoverResult 是 textDocument/hover 的规范化答复。Contents 是 hover 文本（markdown 或
// 纯文本，见 Kind）；Range 是服务端附带的范围（存在时）。
type HoverResult struct {
	Contents string
	Kind     string
	Range    *Range
}

// DocumentSymbol 是 textDocument/documentSymbol 树的一个节点。
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// Diagnostic 是语言服务器通过 textDocument/publishDiagnostics 发布的一个问题。
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"`
	Code     any    `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// LSP 诊断严重级别。
const (
	SeverityError       = 1
	SeverityWarning     = 2
	SeverityInformation = 3
	SeverityHint        = 4
)

// severityName 返回一个诊断严重级别的模型可见名称。
func severityName(severity int) string {
	switch severity {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityInformation:
		return "information"
	case SeverityHint:
		return "hint"
	default:
		return "diagnostic"
	}
}

// symbolKindName 返回一个 SymbolKind 值（1 基，见协议 SymbolKind 枚举）的 LSP 名称。
func symbolKindName(kind int) string {
	names := [...]string{
		"file", "module", "namespace", "package", "class", "method", "property",
		"field", "constructor", "enum", "interface", "function", "variable",
		"constant", "string", "number", "boolean", "array", "object", "key",
		"null", "enum-member", "struct", "event", "operator", "type-parameter",
	}
	if kind >= 1 && kind <= len(names) {
		return names[kind-1]
	}
	return "symbol"
}

// --- 线上参数 ---------------------------------------------------------------

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type textDocumentPositionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

type documentSymbolParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type publishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// --- 结果解析 ---------------------------------------------------------------

// parseHover 规范化 textDocument/hover 结果。null 结果（该位置没有 hover 信息）返回
// (nil, nil)。
func parseHover(raw json.RawMessage) (*HoverResult, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var envelope struct {
		Contents json.RawMessage `json:"contents"`
		Range    *Range          `json:"range,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("lsp: decode hover result: %w", err)
	}
	contents, kind, err := normalizeHoverContents(envelope.Contents)
	if err != nil {
		return nil, err
	}
	return &HoverResult{Contents: contents, Kind: kind, Range: envelope.Range}, nil
}

// normalizeHoverContents 把 LSP hover 内容的联合类型（MarkupContent、MarkedString、
// MarkedString[] 或普通字符串）拍平为文本。
func normalizeHoverContents(raw json.RawMessage) (string, string, error) {
	trimmed := bytes.TrimSpace(raw)
	switch {
	case len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")):
		return "", "", nil
	case trimmed[0] == '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", "", fmt.Errorf("lsp: decode hover contents: %w", err)
		}
		return text, "plaintext", nil
	case trimmed[0] == '{':
		var markup struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(trimmed, &markup); err != nil {
			return "", "", fmt.Errorf("lsp: decode hover markup contents: %w", err)
		}
		return markup.Value, strings.TrimSpace(markup.Kind), nil
	case trimmed[0] == '[':
		var parts []json.RawMessage
		if err := json.Unmarshal(trimmed, &parts); err != nil {
			return "", "", fmt.Errorf("lsp: decode hover contents array: %w", err)
		}
		texts := make([]string, 0, len(parts))
		kind := ""
		for _, part := range parts {
			text, partKind, err := normalizeHoverContents(part)
			if err != nil {
				return "", "", err
			}
			if text == "" {
				continue
			}
			texts = append(texts, text)
			if kind == "" {
				kind = partKind
			}
		}
		joined := strings.Join(texts, "\n\n")
		if kind == "" {
			kind = "plaintext"
		}
		return joined, kind, nil
	default:
		return "", "", fmt.Errorf("lsp: unsupported hover contents encoding")
	}
}

// parseLocations 规范化 textDocument/definition 结果：null、单个 Location、单个
// LocationLink，或它们的数组。
func parseLocations(raw json.RawMessage) ([]Location, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("lsp: decode locations: %w", err)
		}
		locations := make([]Location, 0, len(items))
		for _, item := range items {
			location, err := parseLocation(item)
			if err != nil {
				return nil, err
			}
			locations = append(locations, location)
		}
		return locations, nil
	}
	location, err := parseLocation(trimmed)
	if err != nil {
		return nil, err
	}
	return []Location{location}, nil
}

// parseLocation 接受 Location {uri, range} 或 LocationLink {targetUri,
// targetSelectionRange/targetRange}。
func parseLocation(raw json.RawMessage) (Location, error) {
	var location struct {
		URI   string `json:"uri"`
		Range Range  `json:"range"`
	}
	if err := json.Unmarshal(raw, &location); err == nil && location.URI != "" {
		return Location{URI: location.URI, Range: location.Range}, nil
	}
	var link struct {
		TargetURI            string `json:"targetUri"`
		TargetRange          Range  `json:"targetRange"`
		TargetSelectionRange Range  `json:"targetSelectionRange"`
	}
	if err := json.Unmarshal(raw, &link); err != nil {
		return Location{}, fmt.Errorf("lsp: decode location: %w", err)
	}
	if link.TargetURI == "" {
		return Location{}, fmt.Errorf("lsp: location without a uri")
	}
	selected := link.TargetSelectionRange
	if selected == (Range{}) {
		selected = link.TargetRange
	}
	return Location{URI: link.TargetURI, Range: selected}, nil
}

// parseSymbols 规范化 textDocument/documentSymbol 结果。服务端以层级 DocumentSymbol 或
// 已废弃的扁平 SymbolInformation 列表应答；两者都转换为 DocumentSymbol 值。
func parseSymbols(raw json.RawMessage) ([]DocumentSymbol, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("lsp: decode document symbols: %w", err)
	}
	symbols := make([]DocumentSymbol, 0, len(items))
	for _, item := range items {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, fmt.Errorf("lsp: decode document symbol: %w", err)
		}
		// 扁平 SymbolInformation 形式携带 Location 而不是 Range/SelectionRange 对；它永远是
		// 叶子。
		if _, flat := probe["location"]; flat {
			symbol, err := parseSymbolInformation(item)
			if err != nil {
				return nil, err
			}
			symbols = append(symbols, symbol)
			continue
		}
		var symbol DocumentSymbol
		if err := json.Unmarshal(item, &symbol); err != nil {
			return nil, fmt.Errorf("lsp: decode document symbol: %w", err)
		}
		symbols = append(symbols, symbol)
	}
	return symbols, nil
}

// parseSymbolInformation 转换一个扁平 SymbolInformation 条目。
func parseSymbolInformation(raw json.RawMessage) (DocumentSymbol, error) {
	var info struct {
		Name          string `json:"name"`
		Detail        string `json:"detail,omitempty"`
		Kind          int    `json:"kind"`
		ContainerName string `json:"containerName,omitempty"`
		Location      struct {
			Range Range `json:"range"`
		} `json:"location"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return DocumentSymbol{}, fmt.Errorf("lsp: decode symbol information: %w", err)
	}
	container := strings.TrimSpace(info.ContainerName)
	detail := strings.TrimSpace(info.Detail)
	if container != "" {
		if detail != "" {
			detail = container + "." + detail
		} else {
			detail = container
		}
	}
	return DocumentSymbol{
		Name:           info.Name,
		Detail:         detail,
		Kind:           info.Kind,
		Range:          info.Location.Range,
		SelectionRange: info.Location.Range,
		Children:       nil,
	}, nil
}

// parseDiagnostics 解码一个 publishDiagnostics 通知。
func parseDiagnostics(raw json.RawMessage) (publishDiagnosticsParams, error) {
	var params publishDiagnosticsParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return publishDiagnosticsParams{}, fmt.Errorf("lsp: decode publishDiagnostics: %w", err)
	}
	return params, nil
}
