package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// toolFixture is one initialized client plus a real file the tools can read.
type toolFixture struct {
	client  *Client
	record  string
	path    string
	content string
}

func newToolFixture(t *testing.T, mode string) *toolFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.go")
	content := "package sample\n\nfunc Main() {\n}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write sample file: %v", err)
	}
	client, record := startTestClient(t, mode)
	mustInitialize(t, client)
	return &toolFixture{client: client, record: record, path: path, content: content}
}

// invoke runs one tool by name with raw JSON arguments.
func (f *toolFixture) invoke(t *testing.T, name, arguments string) (rig.ToolResult, error) {
	t.Helper()
	return invokeTool(t, f.client, name, arguments)
}

func invokeTool(t *testing.T, client *Client, name, arguments string) (rig.ToolResult, error) {
	t.Helper()
	for _, tool := range Tools(client, nil) {
		if tool.Name() == name {
			return tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
		}
	}
	t.Fatalf("tool %q not found", name)
	return rig.ToolResult{}, nil
}

func TestToolsNamesAndSchemas(t *testing.T) {
	client, _ := startTestClient(t, "happy")
	tools := Tools(client, nil)
	want := []string{ToolHover, ToolDefinition, ToolReferences, ToolImplementation, ToolDiagnostics, ToolSymbols}
	if len(tools) != len(want) {
		t.Fatalf("Tools() = %d tools, want %d", len(tools), len(want))
	}
	for index, tool := range tools {
		if tool.Name() != want[index] {
			t.Fatalf("tool[%d] = %q, want %q", index, tool.Name(), want[index])
		}
		if tool.Description() == "" {
			t.Fatalf("%s has an empty description", tool.Name())
		}
		schema := tool.Schema()
		if schema["type"] != "object" {
			t.Fatalf("%s schema type = %v, want object", tool.Name(), schema["type"])
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s schema has no properties object", tool.Name())
		}
		if _, ok := properties["path"]; !ok {
			t.Fatalf("%s schema has no path property", tool.Name())
		}
		required, _ := schema["required"].([]string)
		if len(required) == 0 || required[0] != "path" {
			t.Fatalf("%s schema required = %v, want path first", tool.Name(), required)
		}
		if tool.Name() == ToolHover || tool.Name() == ToolDefinition ||
			tool.Name() == ToolReferences || tool.Name() == ToolImplementation {
			for _, field := range []string{"line", "character"} {
				if _, ok := properties[field]; !ok {
					t.Fatalf("%s schema has no %q property", tool.Name(), field)
				}
				if !strings.Contains(strings.Join(required, ","), field) {
					t.Fatalf("%s schema required = %v, want %q", tool.Name(), required, field)
				}
			}
		}
	}
}

func TestToolsWithCustomURIConverter(t *testing.T) {
	client, _ := startTestClient(t, "happy")
	tools := Tools(client, func(path string) (string, error) {
		return "untitled:in-memory.go", nil
	})
	if len(tools) != 6 {
		t.Fatalf("Tools(custom converter) = %d tools, want 6", len(tools))
	}
}

func TestHoverToolInvocation(t *testing.T) {
	fixture := newToolFixture(t, "happy")
	arguments := toolArguments(t, map[string]any{
		"path": fixture.path, "line": 3, "character": 5,
	})
	result, err := fixture.invoke(t, ToolHover, arguments)
	if err != nil {
		t.Fatalf("lsp_hover: %v", err)
	}
	if result.IsError {
		t.Fatalf("lsp_hover returned an error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "**fake hover**") {
		t.Fatalf("lsp_hover content = %q, want the hover text", result.Content)
	}

	// The tool converted the 1-based arguments to a 0-based LSP position and
	// sent the file's URI.
	hoverRecord := findRecord(t, readFakeRecords(t, fixture.record), "textDocument/hover")
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"position"`
	}
	if err := json.Unmarshal(hoverRecord.Params, &params); err != nil {
		t.Fatalf("decode hover params: %v", err)
	}
	wantURI, err := PathToURI(fixture.path)
	if err != nil {
		t.Fatalf("PathToURI: %v", err)
	}
	if params.TextDocument.URI != wantURI {
		t.Fatalf("hover URI = %q, want %q", params.TextDocument.URI, wantURI)
	}
	if params.Position.Line != 2 || params.Position.Character != 4 {
		t.Fatalf("hover position = %+v, want 0-based {2 4}", params.Position)
	}
}

func TestDefinitionToolInvocation(t *testing.T) {
	fixture := newToolFixture(t, "happy")
	result, err := fixture.invoke(t, ToolDefinition, toolArguments(t, map[string]any{
		"path": fixture.path, "line": 1, "character": 1,
	}))
	if err != nil {
		t.Fatalf("lsp_definition: %v", err)
	}
	if result.IsError {
		t.Fatalf("lsp_definition returned an error result: %s", result.Content)
	}
	if !strings.HasSuffix(result.Content, ":3:5") {
		t.Fatalf("lsp_definition content = %q, want a path:line:column location", result.Content)
	}
}

func TestReferencesToolInvocation(t *testing.T) {
	fixture := newToolFixture(t, "references-multi")
	result, err := fixture.invoke(t, ToolReferences, toolArguments(t, map[string]any{
		"path": fixture.path, "line": 1, "character": 1,
	}))
	if err != nil {
		t.Fatalf("lsp_references: %v", err)
	}
	if result.IsError {
		t.Fatalf("lsp_references returned an error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, ":3:5") || !strings.Contains(result.Content, "/tmp/other/target.go:10:2") {
		t.Fatalf("lsp_references content = %q, want both reference locations", result.Content)
	}
	// The references request must carry includeDeclaration=true in its context.
	records := waitForRecord(t, fixture.record, "textDocument/references")
	var params struct {
		Context struct {
			IncludeDeclaration bool `json:"includeDeclaration"`
		} `json:"context"`
	}
	if err := json.Unmarshal(findRecord(t, records, "textDocument/references").Params, &params); err != nil {
		t.Fatalf("decode references params: %v", err)
	}
	if !params.Context.IncludeDeclaration {
		t.Fatalf("references context.includeDeclaration = false, want true")
	}
}

func TestImplementationToolInvocation(t *testing.T) {
	fixture := newToolFixture(t, "implementation-multi")
	result, err := fixture.invoke(t, ToolImplementation, toolArguments(t, map[string]any{
		"path": fixture.path, "line": 1, "character": 1,
	}))
	if err != nil {
		t.Fatalf("lsp_implementation: %v", err)
	}
	if result.IsError {
		t.Fatalf("lsp_implementation returned an error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, ":3:5") || !strings.Contains(result.Content, "/tmp/other/impl.go:5:2") {
		t.Fatalf("lsp_implementation content = %q, want both implementation locations", result.Content)
	}
}

func TestReferencesAndImplementationNoResults(t *testing.T) {
	referencesFixture := newToolFixture(t, "references-null")
	references, err := referencesFixture.invoke(t, ToolReferences, toolArguments(t, map[string]any{
		"path": referencesFixture.path, "line": 1, "character": 1,
	}))
	if err != nil || references.IsError || !strings.Contains(references.Content, "no references") {
		t.Fatalf("no-references = %q (IsError=%v, err=%v)", references.Content, references.IsError, err)
	}
	implementationFixture := newToolFixture(t, "implementation-null")
	implementation, err := implementationFixture.invoke(t, ToolImplementation, toolArguments(t, map[string]any{
		"path": implementationFixture.path, "line": 1, "character": 1,
	}))
	if err != nil || implementation.IsError || !strings.Contains(implementation.Content, "no implementation") {
		t.Fatalf("no-implementation = %q (IsError=%v, err=%v)", implementation.Content, implementation.IsError, err)
	}
}

func TestReferencesAndImplementationCapabilityErrors(t *testing.T) {
	// The server advertises hover/definition only: the new operations must fail with a
	// readable capability error instead of sending the request.
	fixture := newToolFixture(t, "capabilities-minimal")
	for _, toolName := range []string{ToolReferences, ToolImplementation} {
		result, err := fixture.invoke(t, toolName, toolArguments(t, map[string]any{
			"path": fixture.path, "line": 1, "character": 1,
		}))
		if err != nil {
			t.Fatalf("%s: %v", toolName, err)
		}
		if !result.IsError || !strings.Contains(result.Content, "does not support") {
			t.Fatalf("%s result = %q (IsError=%v), want a capability error", toolName, result.Content, result.IsError)
		}
	}
	// hover/definition keep working on the same minimal server.
	hover, err := fixture.invoke(t, ToolHover, toolArguments(t, map[string]any{
		"path": fixture.path, "line": 1, "character": 1,
	}))
	if err != nil || hover.IsError {
		t.Fatalf("lsp_hover on minimal server = %q (IsError=%v, err=%v)", hover.Content, hover.IsError, err)
	}
	if records := readFakeRecords(t, fixture.record); hasMethod(records, "textDocument/references") ||
		hasMethod(records, "textDocument/implementation") {
		t.Fatalf("capability-gated requests were sent to the server: %v", recordMethods(records))
	}
}

func TestDiagnosticsToolInvocation(t *testing.T) {
	fixture := newToolFixture(t, "diagnostics")
	result, err := fixture.invoke(t, ToolDiagnostics, toolArguments(t, map[string]any{
		"path": fixture.path,
	}))
	if err != nil {
		t.Fatalf("lsp_diagnostics: %v", err)
	}
	if result.IsError {
		t.Fatalf("lsp_diagnostics returned an error result: %s", result.Content)
	}
	for _, want := range []string{"undefined: foo", "unused variable x"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("lsp_diagnostics content = %q, want %q", result.Content, want)
		}
	}
}

func TestDiagnosticsToolNoDiagnostics(t *testing.T) {
	fixture := newToolFixture(t, "happy")
	result, err := fixture.invoke(t, ToolDiagnostics, toolArguments(t, map[string]any{
		"path": fixture.path,
	}))
	if err != nil {
		t.Fatalf("lsp_diagnostics: %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "no diagnostics") {
		t.Fatalf("lsp_diagnostics result = %q (isError=%v), want a no-diagnostics note", result.Content, result.IsError)
	}
}

func TestSymbolsToolInvocation(t *testing.T) {
	fixture := newToolFixture(t, "happy")
	result, err := fixture.invoke(t, ToolSymbols, toolArguments(t, map[string]any{
		"path": fixture.path,
	}))
	if err != nil {
		t.Fatalf("lsp_symbols: %v", err)
	}
	if result.IsError {
		t.Fatalf("lsp_symbols returned an error result: %s", result.Content)
	}
	for _, want := range []string{"class FakeService (1:1)", "  function Handler (3:3)", "function helper (8:1)"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("lsp_symbols content = %q, want %q", result.Content, want)
		}
	}
}

func TestToolValidationErrors(t *testing.T) {
	fixture := newToolFixture(t, "happy")
	cases := []struct {
		name      string
		tool      string
		arguments string
		want      string
	}{
		{
			name:      "missing path",
			tool:      ToolHover,
			arguments: `{"line":1,"character":1}`,
			want:      "path is required",
		},
		{
			name:      "line out of range",
			tool:      ToolHover,
			arguments: `{"path":` + quoteJSON(fixture.path) + `,"line":99,"character":1}`,
			want:      "outside the file",
		},
		{
			name:      "zero line",
			tool:      ToolDefinition,
			arguments: `{"path":` + quoteJSON(fixture.path) + `,"line":0,"character":1}`,
			want:      "1-based",
		},
		{
			name:      "zero character",
			tool:      ToolDefinition,
			arguments: `{"path":` + quoteJSON(fixture.path) + `,"line":1,"character":0}`,
			want:      "1-based",
		},
		{
			name:      "missing file",
			tool:      ToolSymbols,
			arguments: `{"path":` + quoteJSON(filepath.Join(t.TempDir(), "nope.go")) + `}`,
			want:      "cannot read",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result, err := fixture.invoke(t, test.tool, test.arguments)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if !result.IsError {
				t.Fatalf("Invoke result is not an error: %q", result.Content)
			}
			if !strings.Contains(result.Content, test.want) {
				t.Fatalf("result = %q, want %q", result.Content, test.want)
			}
		})
	}
}

func TestToolPositionValidationUTF16(t *testing.T) {
	fixture := newToolFixture(t, "happy")
	cjkPath := filepath.Join(t.TempDir(), "cjk.go")
	// Lines: 1 "package 示例", 2 "", 3 "var 名字 = \"😀\"".
	// Line 3 has 4+2+3+1+2+1 = 13 UTF-16 code units: character 14 is the end, 15 is out of range.
	cjkContent := "package 示例\n\nvar 名字 = \"😀\"\n"
	if err := os.WriteFile(cjkPath, []byte(cjkContent), 0o644); err != nil {
		t.Fatalf("write CJK file: %v", err)
	}
	cases := []struct {
		name      string
		tool      string
		arguments string
		want      string
	}{
		{
			name:      "ASCII line out of range",
			tool:      ToolDefinition,
			arguments: toolArguments(t, map[string]any{"path": fixture.path, "line": 99, "character": 1}),
			want:      "outside the file",
		},
		{
			name:      "CJK UTF-16 column beyond line end",
			tool:      ToolHover,
			arguments: toolArguments(t, map[string]any{"path": cjkPath, "line": 3, "character": 15}),
			want:      "outside line 3",
		},
		{
			name:      "zero line",
			tool:      ToolReferences,
			arguments: toolArguments(t, map[string]any{"path": fixture.path, "line": 0, "character": 1}),
			want:      "1-based",
		},
		{
			name:      "zero character",
			tool:      ToolImplementation,
			arguments: toolArguments(t, map[string]any{"path": fixture.path, "line": 1, "character": 0}),
			want:      "character 0 is invalid",
		},
		{
			name:      "line past CJK file end",
			tool:      ToolImplementation,
			arguments: toolArguments(t, map[string]any{"path": cjkPath, "line": 99, "character": 1}),
			want:      "outside the file",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := fixture.invoke(t, testCase.tool, testCase.arguments)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if !result.IsError || !strings.Contains(result.Content, testCase.want) {
				t.Fatalf("result = %q (IsError=%v), want %q", result.Content, result.IsError, testCase.want)
			}
		})
	}

	// A position exactly at the end of a line with a surrogate pair is valid: hover
	// must reach the server (13 UTF-16 units, so character 14 = end).
	result, err := fixture.invoke(t, ToolHover, toolArguments(t, map[string]any{
		"path": cjkPath, "line": 3, "character": 14,
	}))
	if err != nil || result.IsError {
		t.Fatalf("end-of-line position = %q (IsError=%v, err=%v), want a successful hover", result.Content, result.IsError, err)
	}
	if !strings.Contains(result.Content, "**fake hover**") {
		t.Fatalf("hover content = %q, want the fake hover text", result.Content)
	}

	// Invalid positions must never reach the server as queries (didOpen is fine).
	if records := readFakeRecords(t, fixture.record); hasMethod(records, "textDocument/definition") ||
		hasMethod(records, "textDocument/references") ||
		hasMethod(records, "textDocument/implementation") {
		t.Fatalf("invalid position was forwarded as a query: %v", recordMethods(records))
	}
}

func TestToolInvalidArgumentsJSON(t *testing.T) {
	fixture := newToolFixture(t, "happy")
	if _, err := fixture.invoke(t, ToolHover, "not-json"); err == nil {
		t.Fatal("Invoke accepted invalid arguments JSON")
	}
}

func TestToolServerErrorIsModelFacing(t *testing.T) {
	fixture := newToolFixture(t, "error")
	result, err := fixture.invoke(t, ToolHover, toolArguments(t, map[string]any{
		"path": fixture.path, "line": 1, "character": 1,
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "fake hover failure") {
		t.Fatalf("result = %q (isError=%v), want the server failure", result.Content, result.IsError)
	}
}

func TestToolContextCancellation(t *testing.T) {
	fixture := newToolFixture(t, "hang-hover")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	tool := findTool(t, Tools(fixture.client, nil), ToolHover)
	result, err := tool.Invoke(ctx, rig.ToolCall{Name: ToolHover, Arguments: toolArguments(t, map[string]any{
		"path": fixture.path, "line": 1, "character": 1,
	})})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Invoke(expired ctx) = %v, want context.DeadlineExceeded", err)
	}
	if result.Content != "" {
		t.Fatalf("cancelled Invoke returned content %q", result.Content)
	}
}

func TestToolWithoutClient(t *testing.T) {
	tool := NewHoverTool(nil, nil)
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: ToolHover, Arguments: `{"path":"x","line":1,"character":1}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "no language server client") {
		t.Fatalf("result = %+v, want a model-facing missing-client error", result)
	}
}

func TestPathToURI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dir with space", "héllo #1.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	uri, err := PathToURI(path)
	if err != nil {
		t.Fatalf("PathToURI: %v", err)
	}
	if !strings.HasPrefix(uri, "file://") {
		t.Fatalf("URI = %q, want a file URI", uri)
	}
	for _, encoded := range []string{"%20", "%23", "h%C3%A9llo"} {
		if !strings.Contains(uri, encoded) {
			t.Fatalf("URI = %q, want it to contain %q", uri, encoded)
		}
	}
	if strings.Contains(uri, "%2F") {
		t.Fatalf("URI = %q, path separators must stay literal", uri)
	}
	roundTripped, err := URIToPath(uri)
	if err != nil {
		t.Fatalf("URIToPath: %v", err)
	}
	if roundTripped != path {
		t.Fatalf("URIToPath(PathToURI(path)) = %q, want %q", roundTripped, path)
	}
	if _, err := PathToURI("   "); err == nil {
		t.Fatal("PathToURI(blank) succeeded")
	}
	if _, err := URIToPath("https://example.com/x.go"); err == nil {
		t.Fatal("URIToPath(https URI) succeeded")
	}
}

// --- small test helpers -----------------------------------------------------

func hasMethod(records []fakeRecord, method string) bool {
	for _, record := range records {
		if record.Method == method {
			return true
		}
	}
	return false
}

func findTool(t *testing.T, tools []rig.Tool, name string) rig.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func toolArguments(t *testing.T, values map[string]any) string {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	return string(data)
}

func quoteJSON(value string) string {
	data, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(data)
}
