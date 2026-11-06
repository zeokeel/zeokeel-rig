package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Position is a zero-based LSP position. Character counts UTF-16 code units, as
// the protocol requires; this package never reinterprets it.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is an LSP range.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is one source range returned by a language server.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// HoverResult is the normalized answer to textDocument/hover. Contents is the
// hover text (markdown or plain text, see Kind); Range is the range the server
// attached to it, when any.
type HoverResult struct {
	Contents string
	Kind     string
	Range    *Range
}

// DocumentSymbol is one node of a textDocument/documentSymbol tree.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// Diagnostic is one problem published by a language server through
// textDocument/publishDiagnostics.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"`
	Code     any    `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// LSP diagnostic severities.
const (
	SeverityError       = 1
	SeverityWarning     = 2
	SeverityInformation = 3
	SeverityHint        = 4
)

// severityName returns the model-facing name of one diagnostic severity.
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

// symbolKindName returns the LSP name of one SymbolKind value (1-based, see the
// protocol's SymbolKind enum).
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

// --- wire params -----------------------------------------------------------

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

// --- result parsing --------------------------------------------------------

// parseHover normalizes a textDocument/hover result. A null result (the server
// has no hover information at that position) returns (nil, nil).
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

// normalizeHoverContents flattens the LSP hover contents union (MarkupContent,
// MarkedString, MarkedString[] or a plain string) into text.
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

// parseLocations normalizes a textDocument/definition result: null, one
// Location, one LocationLink, or an array of either.
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

// parseLocation accepts a Location {uri, range} or a LocationLink
// {targetUri, targetSelectionRange/targetRange}.
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

// parseSymbols normalizes a textDocument/documentSymbol result. Servers answer
// with hierarchical DocumentSymbol values or with the deprecated flat
// SymbolInformation list; both become DocumentSymbol values.
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
		// The flat SymbolInformation form carries a Location instead of a
		// Range/SelectionRange pair; it is also always a leaf.
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

// parseSymbolInformation converts one flat SymbolInformation entry.
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

// parseDiagnostics decodes one publishDiagnostics notification.
func parseDiagnostics(raw json.RawMessage) (publishDiagnosticsParams, error) {
	var params publishDiagnosticsParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return publishDiagnosticsParams{}, fmt.Errorf("lsp: decode publishDiagnostics: %w", err)
	}
	return params, nil
}
