package lsp

// Tests for the legacy consolidated lsp tool: action routing to the shared
// client methods/formatting, validation, and the custom URI converter.

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

func invokeLegacyTool(t *testing.T, tool rig.Tool, arguments string) (rig.ToolResult, error) {
	t.Helper()
	return tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: tool.Name(), Arguments: arguments})
}

func TestLegacyToolShape(t *testing.T) {
	tool := LegacyTool(nil, nil)
	if tool.Name() != ToolLegacy {
		t.Fatalf("Name = %q, want %q", tool.Name(), ToolLegacy)
	}
	if tool.Description() == "" {
		t.Fatal("lsp has an empty description")
	}
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	properties, _ := schema["properties"].(map[string]any)
	action, _ := properties["action"].(map[string]any)
	if got := strings.Join(toStrings(action["enum"]), ","); got != "hover,definition,references,implementation,diagnostics,symbols" {
		t.Fatalf("action enum = %q", got)
	}
	required, _ := schema["required"].([]string)
	if strings.Join(required, ",") != "action,path" {
		t.Fatalf("required = %v, want [action path]", required)
	}
}

func toStrings(value any) []string {
	values, _ := value.([]any)
	out := make([]string, 0, len(values))
	for _, item := range values {
		out = append(out, item.(string))
	}
	return out
}

func TestLegacyToolHoverAndDefinition(t *testing.T) {
	fixture := newToolFixture(t, "happy")
	tool := LegacyTool(fixture.client, nil)

	hover, err := invokeLegacyTool(t, tool, toolArguments(t, map[string]any{
		"action": "hover", "path": fixture.path, "line": 3, "character": 5,
	}))
	if err != nil {
		t.Fatalf("hover: %v", err)
	}
	if hover.IsError || !strings.Contains(hover.Content, "**fake hover**") {
		t.Fatalf("legacy hover = %q (IsError=%v)", hover.Content, hover.IsError)
	}

	definition, err := invokeLegacyTool(t, tool, toolArguments(t, map[string]any{
		"action": "definition", "path": fixture.path, "line": 1, "character": 1,
	}))
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if definition.IsError || !strings.HasSuffix(definition.Content, ":3:5") {
		t.Fatalf("legacy definition = %q (IsError=%v)", definition.Content, definition.IsError)
	}
}

func TestLegacyToolReferencesAndImplementation(t *testing.T) {
	referencesFixture := newToolFixture(t, "references-multi")
	references, err := invokeLegacyTool(t, LegacyTool(referencesFixture.client, nil), toolArguments(t, map[string]any{
		"action": "references", "path": referencesFixture.path, "line": 1, "character": 1,
	}))
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if references.IsError || !strings.Contains(references.Content, "/tmp/other/target.go:10:2") {
		t.Fatalf("legacy references = %q (IsError=%v)", references.Content, references.IsError)
	}

	implementationFixture := newToolFixture(t, "implementation-multi")
	implementation, err := invokeLegacyTool(t, LegacyTool(implementationFixture.client, nil), toolArguments(t, map[string]any{
		"action": "implementation", "path": implementationFixture.path, "line": 1, "character": 1,
	}))
	if err != nil {
		t.Fatalf("implementation: %v", err)
	}
	if implementation.IsError || !strings.Contains(implementation.Content, "/tmp/other/impl.go:5:2") {
		t.Fatalf("legacy implementation = %q (IsError=%v)", implementation.Content, implementation.IsError)
	}

	// NormalizeOperation aliases are accepted too: findReferences / goToImplementation.
	aliasFixture := newToolFixture(t, "references-multi")
	alias, err := invokeLegacyTool(t, LegacyTool(aliasFixture.client, nil), toolArguments(t, map[string]any{
		"action": "findReferences", "path": aliasFixture.path, "line": 1, "character": 1,
	}))
	if err != nil || alias.IsError {
		t.Fatalf("legacy findReferences alias = %q (IsError=%v, err=%v)", alias.Content, alias.IsError, err)
	}
	if !strings.Contains(alias.Content, "/tmp/other/target.go:10:2") {
		t.Fatalf("legacy findReferences = %q, want the reference locations", alias.Content)
	}
}

func TestLegacyToolDiagnosticsAndSymbols(t *testing.T) {
	diagnosticsFixture := newToolFixture(t, "diagnostics")
	diagnostics, err := invokeLegacyTool(t, LegacyTool(diagnosticsFixture.client, nil), toolArguments(t, map[string]any{
		"action": "diagnostics", "path": diagnosticsFixture.path,
	}))
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if diagnostics.IsError {
		t.Fatalf("legacy diagnostics = %q", diagnostics.Content)
	}
	for _, want := range []string{"undefined: foo", "unused variable x"} {
		if !strings.Contains(diagnostics.Content, want) {
			t.Fatalf("legacy diagnostics = %q, want %q", diagnostics.Content, want)
		}
	}

	symbolsFixture := newToolFixture(t, "happy")
	symbols, err := invokeLegacyTool(t, LegacyTool(symbolsFixture.client, nil), toolArguments(t, map[string]any{
		"action": "symbols", "path": symbolsFixture.path,
	}))
	if err != nil {
		t.Fatalf("symbols: %v", err)
	}
	if symbols.IsError {
		t.Fatalf("legacy symbols = %q", symbols.Content)
	}
	for _, want := range []string{"class FakeService (1:1)", "  function Handler (3:3)", "function helper (8:1)"} {
		if !strings.Contains(symbols.Content, want) {
			t.Fatalf("legacy symbols = %q, want %q", symbols.Content, want)
		}
	}
}

func TestLegacyToolNoResults(t *testing.T) {
	// "happy" publishes no diagnostics, and definition-null returns no location.
	fixture := newToolFixture(t, "happy")
	tool := LegacyTool(fixture.client, nil)
	diagnostics, err := invokeLegacyTool(t, tool, toolArguments(t, map[string]any{
		"action": "diagnostics", "path": fixture.path,
	}))
	if err != nil || diagnostics.IsError || !strings.Contains(diagnostics.Content, "no diagnostics") {
		t.Fatalf("no-diagnostics = %q (IsError=%v, err=%v)", diagnostics.Content, diagnostics.IsError, err)
	}
	nullFixture := newToolFixture(t, "definition-null")
	definition, err := invokeLegacyTool(t, LegacyTool(nullFixture.client, nil), toolArguments(t, map[string]any{
		"action": "definition", "path": nullFixture.path, "line": 1, "character": 1,
	}))
	if err != nil || definition.IsError || !strings.Contains(definition.Content, "no definition") {
		t.Fatalf("no-definition = %q (IsError=%v, err=%v)", definition.Content, definition.IsError, err)
	}
}

func TestLegacyToolValidation(t *testing.T) {
	fixture := newToolFixture(t, "happy")
	tool := LegacyTool(fixture.client, nil)
	cases := []struct {
		name      string
		arguments string
		want      string
	}{
		{"missing action", toolArguments(t, map[string]any{"path": fixture.path}), "action is required"},
		{"unknown action", toolArguments(t, map[string]any{"action": "rename", "path": fixture.path}), "unsupported action"},
		{"missing path", toolArguments(t, map[string]any{"action": "symbols"}), "path is required"},
		{"hover missing line", toolArguments(t, map[string]any{"action": "hover", "path": fixture.path}), "line 0 is invalid"},
		{"definition missing character", toolArguments(t, map[string]any{
			"action": "definition", "path": fixture.path, "line": 1,
		}), "character 0 is invalid"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := invokeLegacyTool(t, tool, testCase.arguments)
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if !result.IsError || !strings.Contains(result.Content, testCase.want) {
				t.Fatalf("result = %q (IsError=%v), want %q", result.Content, result.IsError, testCase.want)
			}
		})
	}

	missingFile := toolArguments(t, map[string]any{"action": "symbols", "path": fixture.path + ".nope"})
	result, err := invokeLegacyTool(t, tool, missingFile)
	if err != nil || !result.IsError || !strings.Contains(result.Content, "cannot read") {
		t.Fatalf("missing file = %q (IsError=%v, err=%v)", result.Content, result.IsError, err)
	}

	// A nil client is a readable tool error, like the split tools.
	nilClient, err := invokeLegacyTool(t, LegacyTool(nil, nil), `{"action":"symbols","path":"x"}`)
	if err != nil || !nilClient.IsError || !strings.Contains(nilClient.Content, "no language server client") {
		t.Fatalf("nil client = %q (IsError=%v, err=%v)", nilClient.Content, nilClient.IsError, err)
	}

	// Malformed JSON is a Go error.
	if _, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: "{"}); err == nil {
		t.Fatal("malformed arguments returned nil error")
	}
}

func TestLegacyToolServerError(t *testing.T) {
	fixture := newToolFixture(t, "error") // hover requests fail on the server
	result, err := invokeLegacyTool(t, LegacyTool(fixture.client, nil), toolArguments(t, map[string]any{
		"action": "hover", "path": fixture.path, "line": 3, "character": 5,
	}))
	if err != nil {
		t.Fatalf("hover: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "fake hover failure") {
		t.Fatalf("server error result = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestLegacyToolCustomURIConverter(t *testing.T) {
	fixture := newToolFixture(t, "happy")
	customURI := "untitled:legacy-sample.go"
	tool := LegacyTool(fixture.client, func(string) (string, error) { return customURI, nil })
	result, err := invokeLegacyTool(t, tool, toolArguments(t, map[string]any{
		"action": "hover", "path": fixture.path, "line": 3, "character": 5,
	}))
	if err != nil || result.IsError {
		t.Fatalf("custom converter hover = %q (IsError=%v, err=%v)", result.Content, result.IsError, err)
	}
	// The fake server recorded the didOpen URI: it must be the injected one.
	opened := false
	for _, record := range readFakeRecords(t, fixture.record) {
		if record.Method != "textDocument/didOpen" {
			continue
		}
		if strings.Contains(string(record.Params), customURI) {
			opened = true
		}
	}
	if !opened {
		t.Fatalf("fake server did not receive the custom URI %q", customURI)
	}
}
