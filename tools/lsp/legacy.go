package lsp

import (
	"context"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// ToolLegacy is the model-facing name of the pre-split consolidated LSP tool.
const ToolLegacy = "lsp"

// LegacyTool returns the legacy single LSP tool: one tool whose action argument
// selects hover, definition, diagnostics or symbols. It maps to the same client
// methods and formatting as the lsp_* tools, so hosts migrating from the old
// surface can keep a stable tool name.
//
// filePathToURI converts a path argument to a document URI; nil uses PathToURI.
func LegacyTool(client *Client, filePathToURI func(string) (string, error)) rig.Tool {
	return &legacyLSPTool{client: client, toURI: filePathToURI}
}

// legacyLSPTool is the rig.Tool implementation of the consolidated legacy tool.
type legacyLSPTool struct {
	client *Client
	toURI  func(string) (string, error)
}

var _ rig.Tool = (*legacyLSPTool)(nil)

// Name implements rig.Tool.
func (t *legacyLSPTool) Name() string { return ToolLegacy }

// Description implements rig.Tool.
func (t *legacyLSPTool) Description() string {
	return "Legacy consolidated language-server tool: action=hover or definition inspects the symbol at line/character " +
		"(1-based); action=diagnostics lists the file's diagnostics; action=symbols lists its symbol outline. " +
		"Prefer lsp_hover/lsp_definition/lsp_diagnostics/lsp_symbols when the host exposes them."
}

// Schema implements rig.Tool.
func (t *legacyLSPTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"action": map[string]any{
			"type":        "string",
			"enum":        []any{"hover", "definition", "diagnostics", "symbols"},
			"description": "Operation to perform on the document.",
		},
		"path":      stringProp("File path of the document to inspect."),
		"line":      integerProp("1-based line number (required for hover and definition)."),
		"character": integerProp("1-based column in UTF-16 code units (required for hover and definition)."),
	}, "action", "path")
}

// legacyActionArgs is the consolidated tool's argument object.
type legacyActionArgs struct {
	Action    string `json:"action"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

// Invoke implements rig.Tool.
func (t *legacyLSPTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if t.client == nil {
		return errorResult("lsp: no language server client is configured"), nil
	}
	var args legacyActionArgs
	if err := decodeArguments(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		return errorResult("lsp: action is required (hover|definition|diagnostics|symbols)"), nil
	}
	base := &lspTool{client: t.client, toURI: t.toURI, name: ToolLegacy}
	switch action {
	case "hover":
		uri, text, failure, err := openDocument(ctx, base, args.Path)
		if err != nil || failure != nil {
			return failureResult(failure), err
		}
		line, character, failure := positionFor(args.Line, args.Character, text)
		if failure != nil {
			return *failure, nil
		}
		hover, err := t.client.Hover(ctx, uri, line, character)
		if err != nil {
			return callFailure(ctx, ToolLegacy, err)
		}
		formatted := FormatHover(hover)
		if formatted == "" {
			formatted = "(no hover information)"
		}
		return rig.ToolResult{Content: formatted}, nil
	case "definition":
		uri, text, failure, err := openDocument(ctx, base, args.Path)
		if err != nil || failure != nil {
			return failureResult(failure), err
		}
		line, character, failure := positionFor(args.Line, args.Character, text)
		if failure != nil {
			return *failure, nil
		}
		locations, err := t.client.Definition(ctx, uri, line, character)
		if err != nil {
			return callFailure(ctx, ToolLegacy, err)
		}
		if len(locations) == 0 {
			return rig.ToolResult{Content: "no definition found"}, nil
		}
		return rig.ToolResult{Content: FormatLocations(locations)}, nil
	case "diagnostics":
		uri, _, failure, err := openDocument(ctx, base, args.Path)
		if err != nil || failure != nil {
			return failureResult(failure), err
		}
		diagnostics, err := t.client.Diagnostics(ctx, uri)
		if err != nil {
			return callFailure(ctx, ToolLegacy, err)
		}
		if len(diagnostics) == 0 {
			return rig.ToolResult{Content: "no diagnostics reported"}, nil
		}
		return rig.ToolResult{Content: FormatDiagnostics(diagnostics)}, nil
	case "symbols":
		uri, _, failure, err := openDocument(ctx, base, args.Path)
		if err != nil || failure != nil {
			return failureResult(failure), err
		}
		symbols, err := t.client.DocumentSymbols(ctx, uri)
		if err != nil {
			return callFailure(ctx, ToolLegacy, err)
		}
		if len(symbols) == 0 {
			return rig.ToolResult{Content: "no symbols found"}, nil
		}
		return rig.ToolResult{Content: FormatSymbols(symbols)}, nil
	default:
		return errorResult("lsp: unsupported action %q (want hover, definition, diagnostics or symbols)", args.Action), nil
	}
}
