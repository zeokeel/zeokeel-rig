package lsp

import (
	"fmt"
	"strings"
)

// Result formatting. The model-facing tools use these to turn the client's
// normalized structures into compact plain text; they are exported so hosts can
// render LSP data in their own tools consistently.
//
// Positions are rendered 1-based (line:column), matching the tools' arguments
// and what editors display; the client API itself is LSP-native (0-based).

// FormatHover renders one hover result. It returns "" when the server has no
// hover information.
func FormatHover(hover *HoverResult) string {
	if hover == nil {
		return ""
	}
	return strings.TrimSpace(hover.Contents)
}

// FormatLocations renders definition locations, one "path:line:column" per
// line. URIs that are not file URIs are rendered as-is.
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

// FormatSymbols renders a document-symbol tree with two spaces of indentation
// per level and one "kind name (line:column)" entry per node.
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

// FormatDiagnostics renders diagnostics as "severity message (line:column)"
// lines, one per diagnostic, in the order the server published them.
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

// displayURI renders a URI as a filesystem path when it is a file URI, falling
// back to the raw URI for other schemes.
func displayURI(uri string) string {
	if path, err := URIToPath(uri); err == nil {
		return path
	}
	return uri
}
