package toolutil

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
)

// DecodeArgs 把一次工具调用的参数 JSON 解码到 target。
//
// 空（或仅含空白）的 Arguments 字符串按空对象解码，因此参数全部可选的工具在模型
// 不发送任何参数时也能正常工作。格式错误的载荷会返回
// "invalid arguments JSON: <cause>"。
func DecodeArgs(call rig.ToolCall, target any) error {
	return DecodeArgsAs("", call, target)
}

// DecodeArgsAs 是带来源前缀的 DecodeArgs，例如
// "ralph: invalid arguments JSON: ..."。
//
// 历史上少数包以这种方式给解码错误加命名空间前缀。保留该辅助函数是为了让这些包的
// 工具调用错误在去重后保持逐字节一致；新增调用点应使用 DecodeArgs。
func DecodeArgsAs(prefix string, call rig.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		if prefix != "" {
			return fmt.Errorf("%s: invalid arguments JSON: %w", prefix, err)
		}
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return nil
}

// TextResult 构造一次成功的工具结果。
func TextResult(content string) rig.ToolResult {
	return rig.ToolResult{Content: content}
}

// ErrorResult 构造面向模型的工具失败结果，其文本为 err.Error()。
// agent 会把该结果回报给模型，而不会中止本轮运行。
func ErrorResult(err error) rig.ToolResult {
	return rig.ToolResult{Content: err.Error(), IsError: true}
}

// ErrorResultf 构造带格式化消息的模型可见工具失败结果。
// 格式动词与参数遵循 fmt.Sprintf；消息中不是格式串的裸 "%" 必须通过
// ErrorResultf("%s", message) 传入。
func ErrorResultf(format string, args ...any) rig.ToolResult {
	return rig.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

// ObjectSchema 构造一个工具对外宣告的 JSON Schema 对象：
// type 为 "object"、包含给定 properties、不允许额外属性，并在至少给出一项时
// 设置 required 列表。
func ObjectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// StringProp 构造一个字符串属性的 schema。
func StringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// IntegerProp 构造一个整数属性的 schema。
func IntegerProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// BooleanProp 构造一个布尔属性的 schema。
func BooleanProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// StringArrayProp 构造一个字符串数组属性的 schema。
func StringArrayProp(description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": description,
		"items":       map[string]any{"type": "string"},
	}
}

// TruncateBytes 把 s 截断到至多 max 字节，且不切坏 UTF-8 字符；真正发生截断时
// 追加 suffix。max <= 0 表示不截断，因此策略中为零的上限表示“不设上限”。
// 若调用方对非正上限另有语义，必须在调用前自行处理。
func TruncateBytes(s string, max int, suffix string) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}
