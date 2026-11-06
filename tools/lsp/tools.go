package lsp

import (
	"context"
	"os"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// Model-facing LSP tool names. They are stable identifiers.
const (
	ToolHover       = "lsp_hover"
	ToolDefinition  = "lsp_definition"
	ToolDiagnostics = "lsp_diagnostics"
	ToolSymbols     = "lsp_symbols"
)

// Tools returns the four read-only LSP tools bound to client:
// lsp_hover, lsp_definition, lsp_diagnostics and lsp_symbols.
//
// filePathToURI converts a tool path argument to a document URI; nil uses
// PathToURI (file:// URI of the absolute path). Callers whose language servers
// address documents differently (in-memory buffers, virtual workspaces) can
// inject their own conversion.
func Tools(client *Client, filePathToURI func(string) (string, error)) []rig.Tool {
	return []rig.Tool{
		NewHoverTool(client, filePathToURI),
		NewDefinitionTool(client, filePathToURI),
		NewDiagnosticsTool(client, filePathToURI),
		NewSymbolsTool(client, filePathToURI),
	}
}

// NewHoverTool returns the lsp_hover tool: "what is the type or documentation
// of the symbol at line/character".
func NewHoverTool(client *Client, filePathToURI func(string) (string, error)) rig.Tool {
	return &lspTool{
		client: client,
		toURI:  filePathToURI,
		name:   ToolHover,
		description: "Show hover information (type, documentation, signature) for the symbol at a position in a source file " +
			"as reported by the configured language server. Use it to understand a symbol without reading the whole file. " +
			"line and character are 1-based; character counts UTF-16 code units as LSP does.",
		parameters: positionSchema("File path of the document to inspect."),
		invoke:     invokeHover,
	}
}

// NewDefinitionTool returns the lsp_definition tool: "where is the symbol at
// line/character defined".
func NewDefinitionTool(client *Client, filePathToURI func(string) (string, error)) rig.Tool {
	return &lspTool{
		client: client,
		toURI:  filePathToURI,
		name:   ToolDefinition,
		description: "Find where the symbol at a position in a source file is defined, as reported by the configured language server. " +
			"Returns one or more path:line:column locations. line and character are 1-based; character counts UTF-16 code units as LSP does.",
		parameters: positionSchema("File path of the document to inspect."),
		invoke:     invokeDefinition,
	}
}

// NewDiagnosticsTool returns the lsp_diagnostics tool: "what does the language
// server report for this file".
func NewDiagnosticsTool(client *Client, filePathToURI func(string) (string, error)) rig.Tool {
	return &lspTool{
		client: client,
		toURI:  filePathToURI,
		name:   ToolDiagnostics,
		description: "List the diagnostics (errors, warnings, hints) the configured language server publishes for a source file. " +
			"Diagnostics are published asynchronously, so this is best effort: the tool waits briefly for them and may report none " +
			"for a file the server has not analyzed yet. Each line reads: severity message (line:column), 1-based.",
		parameters: pathSchema("File path of the document to inspect."),
		invoke:     invokeDiagnostics,
	}
}

// NewSymbolsTool returns the lsp_symbols tool: the document's symbol outline.
func NewSymbolsTool(client *Client, filePathToURI func(string) (string, error)) rig.Tool {
	return &lspTool{
		client: client,
		toURI:  filePathToURI,
		name:   ToolSymbols,
		description: "List the code symbols (types, functions, methods, fields, ...) of a source file as an indented tree, " +
			"as reported by the configured language server. Each line reads: kind name (line:column), 1-based; nested symbols are indented.",
		parameters: pathSchema("File path of the document to inspect."),
		invoke:     invokeSymbols,
	}
}

// lspTool is the shared rig.Tool implementation of the LSP tools.
type lspTool struct {
	client      *Client
	toURI       func(string) (string, error)
	name        string
	description string
	parameters  map[string]any
	invoke      func(ctx context.Context, tool *lspTool, call rig.ToolCall) (rig.ToolResult, error)
}

func (t *lspTool) Name() string           { return t.name }
func (t *lspTool) Description() string    { return t.description }
func (t *lspTool) Schema() map[string]any { return t.parameters }
func (t *lspTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if t.client == nil {
		return toolutil.ErrorResultf("%s: no language server client is configured", t.name), nil
	}
	return t.invoke(ctx, t, call)
}

// --- argument structs -------------------------------------------------------

type positionArgs struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

type pathArgs struct {
	Path string `json:"path"`
}

// --- tool bodies ------------------------------------------------------------

func invokeHover(ctx context.Context, tool *lspTool, call rig.ToolCall) (rig.ToolResult, error) {
	var args positionArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	uri, text, failure, err := openDocument(ctx, tool, args.Path)
	if err != nil || failure != nil {
		return failureResult(failure), err
	}
	line, character, failure := positionFor(args.Line, args.Character, text)
	if failure != nil {
		return *failure, nil
	}
	hover, err := tool.client.Hover(ctx, uri, line, character)
	if err != nil {
		return callFailure(ctx, tool.name, err)
	}
	formatted := FormatHover(hover)
	if formatted == "" {
		formatted = "(no hover information)"
	}
	return rig.ToolResult{Content: formatted}, nil
}

func invokeDefinition(ctx context.Context, tool *lspTool, call rig.ToolCall) (rig.ToolResult, error) {
	var args positionArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	uri, text, failure, err := openDocument(ctx, tool, args.Path)
	if err != nil || failure != nil {
		return failureResult(failure), err
	}
	line, character, failure := positionFor(args.Line, args.Character, text)
	if failure != nil {
		return *failure, nil
	}
	locations, err := tool.client.Definition(ctx, uri, line, character)
	if err != nil {
		return callFailure(ctx, tool.name, err)
	}
	if len(locations) == 0 {
		return rig.ToolResult{Content: "no definition found"}, nil
	}
	return rig.ToolResult{Content: FormatLocations(locations)}, nil
}

func invokeDiagnostics(ctx context.Context, tool *lspTool, call rig.ToolCall) (rig.ToolResult, error) {
	var args pathArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	uri, _, failure, err := openDocument(ctx, tool, args.Path)
	if err != nil || failure != nil {
		return failureResult(failure), err
	}
	diagnostics, err := tool.client.Diagnostics(ctx, uri)
	if err != nil {
		return callFailure(ctx, tool.name, err)
	}
	if len(diagnostics) == 0 {
		return rig.ToolResult{Content: "no diagnostics reported"}, nil
	}
	return rig.ToolResult{Content: FormatDiagnostics(diagnostics)}, nil
}

func invokeSymbols(ctx context.Context, tool *lspTool, call rig.ToolCall) (rig.ToolResult, error) {
	var args pathArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	uri, _, failure, err := openDocument(ctx, tool, args.Path)
	if err != nil || failure != nil {
		return failureResult(failure), err
	}
	symbols, err := tool.client.DocumentSymbols(ctx, uri)
	if err != nil {
		return callFailure(ctx, tool.name, err)
	}
	if len(symbols) == 0 {
		return rig.ToolResult{Content: "no symbols found"}, nil
	}
	return rig.ToolResult{Content: FormatSymbols(symbols)}, nil
}

// --- shared tool helpers ----------------------------------------------------

// openDocument resolves a tool path argument to a URI, reads the file and syncs
// its content to the language server. Model-facing problems (missing file,
// unusable path, server write failure) come back as a *rig.ToolResult; the error
// result is reserved for cancellation of the caller's context.
func openDocument(ctx context.Context, tool *lspTool, path string) (uri, text string, failure *rig.ToolResult, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", nil, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		result := toolutil.ErrorResultf("%s: path is required", tool.name)
		return "", "", &result, nil
	}
	toURI := tool.toURI
	if toURI == nil {
		toURI = PathToURI
	}
	uri, err = toURI(path)
	if err != nil {
		result := toolutil.ErrorResultf("%s: invalid path: %v", tool.name, err)
		return "", "", &result, nil
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		result := toolutil.ErrorResultf("%s: cannot read %s: %v", tool.name, path, readErr)
		return "", "", &result, nil
	}
	text = string(data)
	if syncErr := tool.client.DidOpen(ctx, uri, text); syncErr != nil {
		if err := ctx.Err(); err != nil {
			return "", "", nil, err
		}
		result := toolutil.ErrorResultf("%s: sync document to language server: %v", tool.name, syncErr)
		return "", "", &result, nil
	}
	return uri, text, nil, nil
}

// positionFor converts a tool's 1-based line/character arguments to LSP's
// zero-based position, validating the line against the document text. The
// character is only shifted, never reinterpreted: it is expected to be an LSP
// UTF-16 code-unit column offset plus one.
func positionFor(line, character int, text string) (lspLine, lspCharacter int, failure *rig.ToolResult) {
	if line < 1 {
		result := toolutil.ErrorResultf("line %d is invalid: lines are 1-based", line)
		return 0, 0, &result
	}
	if character < 1 {
		result := toolutil.ErrorResultf("character %d is invalid: characters are 1-based", character)
		return 0, 0, &result
	}
	lines := documentLines(text)
	if line > len(lines) {
		result := toolutil.ErrorResultf("line %d is outside the file (it has %d line(s))", line, len(lines))
		return 0, 0, &result
	}
	return line - 1, character - 1, nil
}

// documentLines splits text into the lines an editor shows: \r\n and \r are
// line breaks, and a trailing newline does not add a line.
func documentLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// callFailure renders a client error for the model. Caller-context cancellation
// is an error (the agent run is ending); server errors are tool results so the
// model can adapt.
func callFailure(ctx context.Context, name string, err error) (rig.ToolResult, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return rig.ToolResult{}, ctxErr
	}
	return toolutil.ErrorResultf("%s: %v", name, err), nil
}

// failureResult dereferences an optional model-facing failure.
func failureResult(failure *rig.ToolResult) rig.ToolResult {
	if failure == nil {
		return rig.ToolResult{}
	}
	return *failure
}

// --- schemas ----------------------------------------------------------------

func positionSchema(pathDescription string) map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"path":      toolutil.StringProp(pathDescription),
		"line":      toolutil.IntegerProp("1-based line number of the symbol."),
		"character": toolutil.IntegerProp("1-based column of the symbol, counted in UTF-16 code units as LSP does."),
	}, "path", "line", "character")
}

func pathSchema(pathDescription string) map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"path": toolutil.StringProp(pathDescription),
	}, "path")
}
