package lsp

import (
	"context"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolLegacy 是拆分前合并版 LSP 工具的模型可见名称。
const ToolLegacy = "lsp"

// LegacyTool 返回旧版单个 LSP 工具：一个工具，用 action 参数选择 hover、definition、
// diagnostics 或 symbols。它映射到与 lsp_* 相同的客户端方法与格式化，使从旧工具面迁移的
// 宿主可以保持稳定的工具名。
//
// filePathToURI 把路径参数转换为文档 URI；nil 时使用 PathToURI。
func LegacyTool(client *Client, filePathToURI func(string) (string, error), opts ...ToolOption) rig.Tool {
	tool := &legacyLSPTool{client: client, toURI: filePathToURI}
	for _, opt := range opts {
		if opt != nil {
			// 借用一次性的 lspTool 应用与 lsp_* 相同的选项，然后复制路径策略。
			carrier := &lspTool{}
			opt(carrier)
			tool.pathPolicy = carrier.pathPolicy
		}
	}
	return tool
}

// legacyLSPTool 是合并版旧工具的 rig.Tool 实现。
type legacyLSPTool struct {
	client     *Client
	toURI      func(string) (string, error)
	pathPolicy PathPolicy
}

var _ rig.Tool = (*legacyLSPTool)(nil)

// Name 实现 rig.Tool。
func (t *legacyLSPTool) Name() string { return ToolLegacy }

// Description 实现 rig.Tool。
func (t *legacyLSPTool) Description() string {
	return "Legacy consolidated language-server tool: action=hover or definition inspects the symbol at line/character " +
		"(1-based); action=diagnostics lists the file's diagnostics; action=symbols lists its symbol outline. " +
		"Prefer lsp_hover/lsp_definition/lsp_diagnostics/lsp_symbols when the host exposes them."
}

// Schema 实现 rig.Tool。
func (t *legacyLSPTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"action": map[string]any{
			"type":        "string",
			"enum":        []any{"hover", "definition", "diagnostics", "symbols"},
			"description": "Operation to perform on the document.",
		},
		"path":      toolutil.StringProp("File path of the document to inspect."),
		"line":      toolutil.IntegerProp("1-based line number (required for hover and definition)."),
		"character": toolutil.IntegerProp("1-based column in UTF-16 code units (required for hover and definition)."),
	}, "action", "path")
}

// legacyActionArgs 是合并版工具的参数对象。
type legacyActionArgs struct {
	Action    string `json:"action"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

// Invoke 实现 rig.Tool。
func (t *legacyLSPTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if t.client == nil {
		return toolutil.ErrorResultf("lsp: no language server client is configured"), nil
	}
	var args legacyActionArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		return toolutil.ErrorResultf("lsp: action is required (hover|definition|diagnostics|symbols)"), nil
	}
	base := &lspTool{client: t.client, toURI: t.toURI, name: ToolLegacy, pathPolicy: t.pathPolicy}
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
		return toolutil.ErrorResultf("lsp: unsupported action %q (want hover, definition, diagnostics or symbols)", args.Action), nil
	}
}
