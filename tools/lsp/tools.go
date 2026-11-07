package lsp

import (
	"context"
	"os"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 模型可见的 LSP 工具名。它们是稳定标识。
const (
	ToolHover          = "lsp_hover"
	ToolDefinition     = "lsp_definition"
	ToolReferences     = "lsp_references"
	ToolImplementation = "lsp_implementation"
	ToolDiagnostics    = "lsp_diagnostics"
	ToolSymbols        = "lsp_symbols"
)

// PathPolicy 在工具读取文件并同步到语言服务器之前校验其路径参数。运行不可信模型的宿主
// 应安装一个（例如 sandbox.Policy.CheckPath），使模型可控路径无法读取工作区之外的文件。
type PathPolicy func(path string) error

// ToolOption 配置一个 LSP 工具。
type ToolOption func(*lspTool)

// WithPathPolicy 限制 LSP 工具可以打开哪些文件。拒绝会作为工具错误展示给模型。nil 策略
// 保持历史行为（由宿主决定语言服务器可以访问哪些路径）。
func WithPathPolicy(policy PathPolicy) ToolOption {
	return func(t *lspTool) { t.pathPolicy = policy }
}

// Tools 返回绑定到 client 的六个只读 LSP 工具：lsp_hover、lsp_definition、
// lsp_references、lsp_implementation、lsp_diagnostics 与 lsp_symbols。
//
// filePathToURI 把工具路径参数转换为文档 URI；nil 时使用 PathToURI（绝对路径的 file://
// URI）。语言服务器以不同方式寻址文档（内存缓冲、虚拟工作区）的调用方可以注入自己的转换。
func Tools(client *Client, filePathToURI func(string) (string, error), opts ...ToolOption) []rig.Tool {
	return []rig.Tool{
		NewHoverTool(client, filePathToURI, opts...),
		NewDefinitionTool(client, filePathToURI, opts...),
		NewReferencesTool(client, filePathToURI, opts...),
		NewImplementationTool(client, filePathToURI, opts...),
		NewDiagnosticsTool(client, filePathToURI, opts...),
		NewSymbolsTool(client, filePathToURI, opts...),
	}
}

// NewHoverTool 返回 lsp_hover 工具：查询“line/character 处符号的类型或文档”。
func NewHoverTool(client *Client, filePathToURI func(string) (string, error), opts ...ToolOption) rig.Tool {
	return newLSPTool(client, filePathToURI, ToolHover, opts, invokeHover,
		"Show hover information (type, documentation, signature) for the symbol at a position in a source file "+
			"as reported by the configured language server. Use it to understand a symbol without reading the whole file. "+
			"line and character are 1-based; character counts UTF-16 code units as LSP does.",
		positionSchema("File path of the document to inspect."))
}

// NewDefinitionTool 返回 lsp_definition 工具：查询“line/character 处符号在哪里定义”。
func NewDefinitionTool(client *Client, filePathToURI func(string) (string, error), opts ...ToolOption) rig.Tool {
	return newLSPTool(client, filePathToURI, ToolDefinition, opts, invokeDefinition,
		"Find where the symbol at a position in a source file is defined, as reported by the configured language server. "+
			"Returns one or more path:line:column locations. line and character are 1-based; character counts UTF-16 code units as LSP does.",
		positionSchema("File path of the document to inspect."))
}

// NewReferencesTool 返回 lsp_references 工具：查询“line/character 处符号的引用位置”。
func NewReferencesTool(client *Client, filePathToURI func(string) (string, error), opts ...ToolOption) rig.Tool {
	return newLSPTool(client, filePathToURI, ToolReferences, opts, invokeReferences,
		"Find all references to the symbol at a position in a source file, as reported by the configured language server. "+
			"Returns one or more path:line:column locations. line and character are 1-based; character counts UTF-16 code units as LSP does.",
		positionSchema("File path of the document to inspect."))
}

// NewImplementationTool 返回 lsp_implementation 工具：查询“line/character 处符号的实现在哪里”。
func NewImplementationTool(client *Client, filePathToURI func(string) (string, error), opts ...ToolOption) rig.Tool {
	return newLSPTool(client, filePathToURI, ToolImplementation, opts, invokeImplementation,
		"Find the implementations of the interface method or symbol at a position in a source file, as reported by the configured language server. "+
			"Returns one or more path:line:column locations. line and character are 1-based; character counts UTF-16 code units as LSP does.",
		positionSchema("File path of the document to inspect."))
}

// NewDiagnosticsTool 返回 lsp_diagnostics 工具：查询“语言服务器为该文件报告了什么”。
func NewDiagnosticsTool(client *Client, filePathToURI func(string) (string, error), opts ...ToolOption) rig.Tool {
	return newLSPTool(client, filePathToURI, ToolDiagnostics, opts, invokeDiagnostics,
		"List the diagnostics (errors, warnings, hints) the configured language server publishes for a source file. "+
			"Diagnostics are published asynchronously, so this is best effort: the tool waits briefly for them and may report none "+
			"for a file the server has not analyzed yet. Each line reads: severity message (line:column), 1-based.",
		pathSchema("File path of the document to inspect."))
}

// NewSymbolsTool 返回 lsp_symbols 工具：文档的符号大纲。
func NewSymbolsTool(client *Client, filePathToURI func(string) (string, error), opts ...ToolOption) rig.Tool {
	return newLSPTool(client, filePathToURI, ToolSymbols, opts, invokeSymbols,
		"List the code symbols (types, functions, methods, fields, ...) of a source file as an indented tree, "+
			"as reported by the configured language server. Each line reads: kind name (line:column), 1-based; nested symbols are indented.",
		pathSchema("File path of the document to inspect."))
}

// newLSPTool 构建一个 LSP 工具并应用其选项。
func newLSPTool(
	client *Client,
	filePathToURI func(string) (string, error),
	name string,
	opts []ToolOption,
	invoke func(ctx context.Context, tool *lspTool, call rig.ToolCall) (rig.ToolResult, error),
	description string,
	parameters map[string]any,
) rig.Tool {
	tool := &lspTool{
		client:      client,
		toURI:       filePathToURI,
		name:        name,
		description: description,
		parameters:  parameters,
		invoke:      invoke,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(tool)
		}
	}
	return tool
}

// lspTool 是 LSP 工具共用的 rig.Tool 实现。
type lspTool struct {
	client      *Client
	toURI       func(string) (string, error)
	name        string
	description string
	parameters  map[string]any
	invoke      func(ctx context.Context, tool *lspTool, call rig.ToolCall) (rig.ToolResult, error)
	// pathPolicy 非 nil 时在工具读取文件之前校验模型提供的路径（本地文件泄露防护）。
	pathPolicy PathPolicy
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

// --- 参数结构 ---------------------------------------------------------------

type positionArgs struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

type pathArgs struct {
	Path string `json:"path"`
}

// --- 工具主体 ---------------------------------------------------------------

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

func invokeReferences(ctx context.Context, tool *lspTool, call rig.ToolCall) (rig.ToolResult, error) {
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
	locations, err := tool.client.References(ctx, uri, line, character)
	if err != nil {
		return callFailure(ctx, tool.name, err)
	}
	if len(locations) == 0 {
		return rig.ToolResult{Content: "no references found"}, nil
	}
	return rig.ToolResult{Content: FormatLocations(locations)}, nil
}

func invokeImplementation(ctx context.Context, tool *lspTool, call rig.ToolCall) (rig.ToolResult, error) {
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
	locations, err := tool.client.Implementation(ctx, uri, line, character)
	if err != nil {
		return callFailure(ctx, tool.name, err)
	}
	if len(locations) == 0 {
		return rig.ToolResult{Content: "no implementation found"}, nil
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

// --- 共享工具辅助函数 -------------------------------------------------------

// openDocument 把工具路径参数解析为 URI、读取文件并把内容同步到语言服务器。模型可见的
// 问题（文件缺失、路径不可用、服务端写失败）以 *rig.ToolResult 返回；error 保留给调用方
// 上下文取消。
func openDocument(ctx context.Context, tool *lspTool, path string) (uri, text string, failure *rig.ToolResult, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", nil, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		result := toolutil.ErrorResultf("%s: path is required", tool.name)
		return "", "", &result, nil
	}
	if tool.pathPolicy != nil {
		if policyErr := tool.pathPolicy(path); policyErr != nil {
			result := toolutil.ErrorResultf("%s: path blocked by policy: %v", tool.name, policyErr)
			return "", "", &result, nil
		}
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

// positionFor 把工具的 1 基 line/character 参数交给 ValidatePosition 校验并转换为 LSP 的
// 0 基位置。越界坐标返回模型可见错误，而不是把请求发给语言服务器。
func positionFor(line, character int, text string) (lspLine, lspCharacter int, failure *rig.ToolResult) {
	position, err := ValidatePosition(text, line, character)
	if err != nil {
		result := toolutil.ErrorResultf("%s", err.Error())
		return 0, 0, &result
	}
	return position.Line, position.Character, nil
}

// callFailure 为模型渲染客户端错误。调用方上下文取消是错误（agent 运行即将结束）；服务端
// 错误则是工具结果，便于模型自行调整。
func callFailure(ctx context.Context, name string, err error) (rig.ToolResult, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return rig.ToolResult{}, ctxErr
	}
	return toolutil.ErrorResultf("%s: %v", name, err), nil
}

// failureResult 解引用可选的模型可见失败。
func failureResult(failure *rig.ToolResult) rig.ToolResult {
	if failure == nil {
		return rig.ToolResult{}
	}
	return *failure
}

// --- schema -----------------------------------------------------------------

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
