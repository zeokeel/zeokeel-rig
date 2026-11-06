package agent

// Internal tests for the structured-output JSON Schema subset: definition
// validation, value validation and the structured_output tool.

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

func TestValidateStructured(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":   map[string]any{"type": "string"},
			"count":  map[string]any{"type": "integer"},
			"score":  map[string]any{"type": "number"},
			"active": map[string]any{"type": "boolean"},
			"tags": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			"level": map[string]any{"enum": []any{"low", "high"}},
			"meta": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"note": map[string]any{"type": "string"},
				},
				"required":             []string{"note"},
				"additionalProperties": false,
			},
		},
		"required": []string{"name", "count"},
	}

	cases := []struct {
		name    string
		value   any
		wantErr string
	}{
		{
			name: "valid full object",
			value: map[string]any{
				"name": "x", "count": float64(2), "score": 1.5, "active": true,
				"tags": []any{"a", "b"}, "level": "low",
				"meta": map[string]any{"note": "ok"},
			},
		},
		{
			name:    "missing required",
			value:   map[string]any{"name": "x"},
			wantErr: `missing required property "count"`,
		},
		{
			name:    "wrong type",
			value:   map[string]any{"name": 5, "count": float64(1)},
			wantErr: "expected string",
		},
		{
			name:    "integer rejects fraction",
			value:   map[string]any{"name": "x", "count": 1.5},
			wantErr: "expected integer",
		},
		{
			name:    "enum mismatch",
			value:   map[string]any{"name": "x", "count": float64(1), "level": "medium"},
			wantErr: "not one of the enum values",
		},
		{
			name:    "nested object validation",
			value:   map[string]any{"name": "x", "count": float64(1), "meta": map[string]any{}},
			wantErr: `missing required property "note"`,
		},
		{
			name:    "nested additional property",
			value:   map[string]any{"name": "x", "count": float64(1), "meta": map[string]any{"note": "ok", "extra": true}},
			wantErr: `unexpected property "extra"`,
		},
		{
			name:    "array item type",
			value:   map[string]any{"name": "x", "count": float64(1), "tags": []any{"a", float64(2)}},
			wantErr: "expected string",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateStructured(schema, testCase.value)
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("validateStructured = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("validateStructured = %v, want %q", err, testCase.wantErr)
			}
		})
	}

	// Integer accepts whole-valued floats (JSON numbers decode to float64).
	if err := validateStructured(map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}}}, map[string]any{"n": float64(3)}); err != nil {
		t.Fatalf("whole float integer rejected: %v", err)
	}
	// extra properties are allowed when additionalProperties is not false.
	extraSchema := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	if err := validateStructured(extraSchema, map[string]any{"a": "x", "b": true}); err != nil {
		t.Fatalf("extra property rejected: %v", err)
	}
}

func TestNormalizeOutputSchema(t *testing.T) {
	if schema, err := normalizeOutputSchema(nil); schema != nil || err != nil {
		t.Fatalf("normalizeOutputSchema(nil) = %v, %v", schema, err)
	}
	valid := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	if _, err := normalizeOutputSchema(valid); err != nil {
		t.Fatalf("valid schema rejected: %v", err)
	}
	cases := []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{"missing type", map[string]any{"properties": map[string]any{}}, "top-level schema must declare"},
		{"non-object type", map[string]any{"type": "array"}, "top-level type"},
		{"unsupported nested type", map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "uuid"}}}, "unsupported type"},
		{"bad required", map[string]any{"type": "object", "required": []any{"x", 2}}, "entries must be strings"},
		{"bad items", map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "array", "items": []any{}}}}, "items must be a schema object"},
		{"bad enum", map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"enum": "nope"}}}, "enum: must be an array"},
		{"bad additionalProperties", map[string]any{"type": "object", "additionalProperties": "no"}, "boolean additionalProperties"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := normalizeOutputSchema(testCase.schema); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("normalizeOutputSchema error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestStructuredOutputToolInvoke(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
		"required":   []string{"name"},
	}
	var captured map[string]any
	tool := &structuredOutputTool{schema: schema, capture: func(value map[string]any) { captured = value }}
	if tool.Name() != StructuredOutputToolName {
		t.Fatalf("Name = %q", tool.Name())
	}
	if tool.Description() == "" || tool.Schema() == nil {
		t.Fatal("structured_output needs a description and schema")
	}

	// Malformed JSON and non-object arguments are model-facing errors.
	result, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: "{"})
	if err != nil || !result.IsError || !strings.Contains(result.Content, "invalid arguments JSON") {
		t.Fatalf("malformed JSON = %+v, %v", result, err)
	}
	result, err = tool.Invoke(context.Background(), v1.ToolCall{Arguments: `["a"]`})
	if err != nil || !result.IsError || !strings.Contains(result.Content, "JSON object") {
		t.Fatalf("array arguments = %+v, %v", result, err)
	}

	// Schema violations are IsError results so the model can retry.
	result, err = tool.Invoke(context.Background(), v1.ToolCall{Arguments: `{"name":5}`})
	if err != nil || !result.IsError || !strings.Contains(result.Content, "expected string") {
		t.Fatalf("invalid value = %+v, %v", result, err)
	}
	if captured != nil {
		t.Fatalf("invalid call was captured: %+v", captured)
	}

	// A valid call is captured and acknowledged.
	result, err = tool.Invoke(context.Background(), v1.ToolCall{Arguments: `{"name":"ok"}`})
	if err != nil || result.IsError {
		t.Fatalf("valid call = %+v, %v", result, err)
	}
	if captured == nil || captured["name"] != "ok" {
		t.Fatalf("captured = %+v", captured)
	}
}

func TestStructuredFromText(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"answer": map[string]any{"type": "string"}},
		"required":   []string{"answer"},
	}
	if result := structuredFromText(schema, `{"answer":"yes"}`); result == nil || result["answer"] != "yes" {
		t.Fatalf("valid JSON text = %+v", result)
	}
	if result := structuredFromText(schema, "not json"); result != nil {
		t.Fatalf("invalid JSON text = %+v, want nil", result)
	}
	if result := structuredFromText(schema, `{"answer":2}`); result != nil {
		t.Fatalf("schema-mismatched text = %+v, want nil", result)
	}
	if result := structuredFromText(schema, `["answer"]`); result != nil {
		t.Fatalf("non-object JSON = %+v, want nil", result)
	}
}
