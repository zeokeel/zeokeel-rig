package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// StructuredOutputToolName is the reserved tool name the runner adds to a child
// agent when the SubagentSpec carries an OutputSchema.
const (
	StructuredOutputToolName = "structured_output"
	// structuredOutputInstruction is appended to the child system prompt.
	structuredOutputInstruction = "You must report your final result by calling the " + StructuredOutputToolName +
		" tool with arguments that match its schema. Call it exactly once when the task is complete."
)

// structuredOutputTool captures the child's structured result. It validates the
// call arguments against the OutputSchema before accepting them (a validation
// failure is a model-facing tool error, so the model can correct and retry).
type structuredOutputTool struct {
	schema  map[string]any
	capture func(value map[string]any)
}

var _ rig.Tool = (*structuredOutputTool)(nil)

// Name implements rig.Tool.
func (t *structuredOutputTool) Name() string { return StructuredOutputToolName }

// Description implements rig.Tool.
func (t *structuredOutputTool) Description() string {
	return "Report the final structured result of the task. Call this tool exactly once when the task is complete; " +
		"the arguments must match this tool's JSON schema."
}

// Schema implements rig.Tool.
func (t *structuredOutputTool) Schema() map[string]any { return t.schema }

// Invoke implements rig.Tool.
func (t *structuredOutputTool) Invoke(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return rig.ToolResult{
			Content: "structured_output: invalid arguments JSON: " + err.Error(),
			IsError: true,
		}, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return rig.ToolResult{
			Content: fmt.Sprintf("structured_output: arguments must be a JSON object, got %s", jsonTypeName(value)),
			IsError: true,
		}, nil
	}
	if err := validateStructured(t.schema, object); err != nil {
		return rig.ToolResult{
			Content: "structured_output: " + err.Error(),
			IsError: true,
		}, nil
	}
	if t.capture != nil {
		t.capture(object)
	}
	return rig.ToolResult{Content: "structured output captured"}, nil
}

// --- schema definition validation ------------------------------------------

// normalizeOutputSchema validates an OutputSchema at Start time. A nil schema
// means the child has no structured output contract.
func normalizeOutputSchema(schema map[string]any) (map[string]any, error) {
	if schema == nil {
		return nil, nil
	}
	if err := validateSchemaDefinition(schema, "$"); err != nil {
		return nil, fmt.Errorf("OutputSchema: %w", err)
	}
	types, err := schemaTypeNames(schema["type"])
	if err != nil || len(types) == 0 {
		return nil, fmt.Errorf("OutputSchema: top-level schema must declare type \"object\"")
	}
	for _, typeName := range types {
		if typeName != "object" {
			return nil, fmt.Errorf("OutputSchema: top-level type must be \"object\", got %q", typeName)
		}
	}
	return schema, nil
}

// validateSchemaDefinition checks the subset of JSON Schema this package
// supports. Unknown keywords are ignored (JSON Schema is open).
func validateSchemaDefinition(schema map[string]any, path string) error {
	if schema == nil {
		return nil
	}
	if rawType, ok := schema["type"]; ok {
		types, err := schemaTypeNames(rawType)
		if err != nil {
			return fmt.Errorf("at %s: %w", path, err)
		}
		for _, typeName := range types {
			switch typeName {
			case "object", "array", "string", "number", "integer", "boolean", "null":
			default:
				return fmt.Errorf("at %s: unsupported type %q", path, typeName)
			}
		}
	}
	if rawProperties, ok := schema["properties"]; ok {
		properties, ok := rawProperties.(map[string]any)
		if !ok {
			return fmt.Errorf("at %s: properties must be an object", path)
		}
		for key, rawSub := range properties {
			sub, ok := rawSub.(map[string]any)
			if !ok {
				return fmt.Errorf("at %s/%s: property schema must be an object", path, key)
			}
			if err := validateSchemaDefinition(sub, path+"/"+key); err != nil {
				return err
			}
		}
	}
	if rawRequired, ok := schema["required"]; ok {
		if _, err := schemaStringList(rawRequired); err != nil {
			return fmt.Errorf("at %s: required: %w", path, err)
		}
	}
	if rawItems, ok := schema["items"]; ok {
		items, ok := rawItems.(map[string]any)
		if !ok {
			return fmt.Errorf("at %s: items must be a schema object", path)
		}
		if err := validateSchemaDefinition(items, path+"[]"); err != nil {
			return err
		}
	}
	if rawEnum, ok := schema["enum"]; ok {
		if _, err := schemaAnyList(rawEnum); err != nil {
			return fmt.Errorf("at %s: enum: %w", path, err)
		}
	}
	if rawAdditional, ok := schema["additionalProperties"]; ok {
		if _, ok := rawAdditional.(bool); !ok {
			return fmt.Errorf("at %s: only boolean additionalProperties is supported", path)
		}
	}
	return nil
}

// --- value validation -------------------------------------------------------

// validateStructured validates one decoded JSON value against the supported
// JSON Schema subset: type (object/array/string/number/integer/boolean/null),
// properties, required, items, enum and additionalProperties:false.
func validateStructured(schema map[string]any, value any) error {
	return validateSchemaValue(schema, value, "$")
}

func validateSchemaValue(schema map[string]any, value any, path string) error {
	if schema == nil {
		return nil
	}
	if rawEnum, ok := schema["enum"]; ok {
		candidates, err := schemaAnyList(rawEnum)
		if err != nil {
			return fmt.Errorf("at %s: %w", path, err)
		}
		matched := false
		for _, candidate := range candidates {
			if jsonValueEqual(value, candidate) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("at %s: value %s is not one of the enum values", path, compactJSON(value))
		}
	}
	if rawType, ok := schema["type"]; ok {
		types, err := schemaTypeNames(rawType)
		if err != nil {
			return fmt.Errorf("at %s: %w", path, err)
		}
		matched := false
		for _, typeName := range types {
			if matchesJSONType(typeName, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("at %s: expected %s, got %s", path, strings.Join(types, "|"), jsonTypeName(value))
		}
	}
	if object, ok := value.(map[string]any); ok {
		if rawRequired, present := schema["required"]; present {
			required, err := schemaStringList(rawRequired)
			if err != nil {
				return fmt.Errorf("at %s: %w", path, err)
			}
			for _, key := range required {
				if _, exists := object[key]; !exists {
					return fmt.Errorf("at %s: missing required property %q", path, key)
				}
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for key, child := range object {
			rawSub, defined := properties[key]
			if !defined {
				if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
					return fmt.Errorf("at %s: unexpected property %q", path, key)
				}
				continue
			}
			sub, ok := rawSub.(map[string]any)
			if !ok {
				return fmt.Errorf("at %s/%s: invalid property schema", path, key)
			}
			if err := validateSchemaValue(sub, child, path+"/"+key); err != nil {
				return err
			}
		}
	}
	if array, ok := value.([]any); ok {
		if items, ok := schema["items"].(map[string]any); ok {
			for index, item := range array {
				if err := validateSchemaValue(items, item, fmt.Sprintf("%s[%d]", path, index)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// schemaTypeNames parses "type": either one name or a list of names.
func schemaTypeNames(raw any) ([]string, error) {
	switch typed := raw.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{typed}, nil
	case []string:
		return typed, nil
	case []any:
		names := make([]string, 0, len(typed))
		for _, item := range typed {
			name, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("type entries must be strings")
			}
			names = append(names, name)
		}
		return names, nil
	default:
		return nil, fmt.Errorf("type must be a string or an array of strings")
	}
}

// schemaStringList parses a list of strings ("required").
func schemaStringList(raw any) ([]string, error) {
	switch typed := raw.(type) {
	case []string:
		return typed, nil
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			value, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("entries must be strings")
			}
			values = append(values, value)
		}
		return values, nil
	default:
		return nil, fmt.Errorf("must be an array of strings")
	}
}

// schemaAnyList parses an arbitrary list ("enum").
func schemaAnyList(raw any) ([]any, error) {
	switch typed := raw.(type) {
	case []any:
		return typed, nil
	case nil:
		return nil, fmt.Errorf("must be a non-null array")
	default:
		return nil, fmt.Errorf("must be an array")
	}
}

// matchesJSONType implements the supported type keywords.
func matchesJSONType(typeName string, value any) bool {
	switch typeName {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		return isJSONNumber(value)
	case "integer":
		return isJSONInteger(value)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return true // unknown types are rejected at schema-definition time
	}
}

// isJSONNumber reports whether value is a JSON number representation.
func isJSONNumber(value any) bool {
	switch value.(type) {
	case json.Number, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return true
	default:
		return false
	}
}

// isJSONInteger reports whether value is a JSON number with no fractional part.
func isJSONInteger(value any) bool {
	switch typed := value.(type) {
	case json.Number:
		if _, err := typed.Int64(); err == nil {
			return true
		}
		parsed, err := typed.Float64()
		return err == nil && !math.IsInf(parsed, 0) && parsed == math.Trunc(parsed)
	case float64:
		return !math.IsInf(typed, 0) && !math.IsNaN(typed) && typed == math.Trunc(typed)
	case float32:
		return typed == float32(math.Trunc(float64(typed)))
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	default:
		return false
	}
}

// jsonTypeName renders the JSON type of one value for error messages.
func jsonTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	default:
		if isJSONNumber(value) {
			if isJSONInteger(value) {
				return "integer"
			}
			return "number"
		}
		return fmt.Sprintf("%T", value)
	}
}

// jsonValueEqual compares two decoded JSON values after normalizing numbers.
func jsonValueEqual(left, right any) bool {
	return reflect.DeepEqual(normalizeJSONValue(left), normalizeJSONValue(right))
}

// normalizeJSONValue round-trips a value through JSON so numbers and maps have
// one representation for comparison.
func normalizeJSONValue(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return value
	}
	return normalized
}

// compactJSON renders a value compactly for error messages.
func compactJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	const max = 200
	text := string(encoded)
	if len(text) > max {
		text = text[:max] + "…"
	}
	return text
}

// cloneMessages deep-copies a message list (history seeding).
func cloneMessages(messages []rig.Message) []rig.Message {
	copied := make([]rig.Message, len(messages))
	for index, message := range messages {
		copied[index] = cloneMessage(message)
	}
	return copied
}
