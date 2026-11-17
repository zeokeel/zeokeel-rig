package toolutil

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
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

// SandboxDenialNoteFor 为一次执行渲染结构化的沙箱拒绝说明（带前导换行，
// 便于调用方直接追加到结果文本）。
//
// 它调用 sandbox.DetectDenial：只要 content（stdout+stderr 合并文本）按大小写
// 不敏感子串命中 confined.DenialSignatures 中的某个方言，就返回
//
//	"\n" + denial.Note()
//
// 否则返回空串。Note 的第一行是单行摘要（被拒路径、命中方言、模式与
// enforcement），随后是可写根、保护路径、已授权目录与通用处置顺序，因此模型
// 能据此判断「该重定向工具自己的状态」还是「该申请一个具体的可写目录」，而不是
// 反复重试同一条命令。
//
// 与已废弃的 SandboxDenialNote 的关键差异：**不要求退出码非零**。很多 CLI 把
// 落盘失败包成自己的错误文案后仍以 0 退出（或交互式 shell 没有逐命令退出码
// 语义），此时同样需要归因。调用方只需在结果文本上调用本函数，不要先判断退出
// 码；未命中方言时返回空串，因此「成功且未命中方言」的结果保持逐字节不变。
//
// confined.DenialSignatures 为空或 content 为空时返回空串；policy 为 nil 时仍
// 做归因，只是不渲染可写根/授权信息（sandbox.DetectDenial 接受 nil policy）。
func SandboxDenialNoteFor(content string, confined sandbox.ConfinedCommand, policy *sandbox.Policy) string {
	denial := sandbox.DetectDenial(content, confined.DenialSignatures, policy, confined.Enforcement)
	if denial == nil {
		return ""
	}
	return "\n" + denial.Note()
}

// SandboxDenialNote 为一次失败的执行渲染面向模型的沙箱拒绝归因行：当输出
// （stdout+stderr 合并文本）按大小写不敏感子串匹配到 OS runner 的某个拒绝
// 方言（sandbox.ConfinedCommand.DenialSignatures）时，返回形如
//
//	[sandbox: suspected OS sandbox denial (matched "…"; policy mode …, enforcement …); …]
//
// 的结构化标记，让模型/用户能把「OS 沙箱拒绝」与「命令自身失败」区分开。
// signatures 为空、输出为空或无匹配时返回空串（调用方不追加任何内容，成功
// 路径与不匹配失败保持逐字节不变）。匹配按 signatures 给定顺序取第一个命中，
// 保证文案确定；mode/enforcement 为空时渲染为 unknown。归因是“疑似”提示：
// 方言子串也可能来自命令自身的普通错误，最终判断仍属于模型/用户。
//
// Deprecated: 它只渲染一句“疑似被拒绝”，且调用方往往要求退出码非零才追加，
// 漏掉“命令自称成功但输出含拒绝”的场景。新调用点请使用
// SandboxDenialNoteFor（结构化 Note + 不依赖退出码）。
func SandboxDenialNote(output string, signatures []string, mode sandbox.Mode, enforcement sandbox.Enforcement) string {
	if output == "" || len(signatures) == 0 {
		return ""
	}
	lowered := strings.ToLower(output)
	matched := ""
	for _, signature := range signatures {
		signature = strings.TrimSpace(signature)
		if signature == "" {
			continue
		}
		if strings.Contains(lowered, strings.ToLower(signature)) {
			matched = signature
			break
		}
	}
	if matched == "" {
		return ""
	}
	modeText := string(mode)
	if modeText == "" {
		modeText = "unknown"
	}
	enforcementText := string(enforcement)
	if enforcementText == "" {
		enforcementText = "unknown"
	}
	return fmt.Sprintf("[sandbox: suspected OS sandbox denial (matched %q; policy mode %s, enforcement %s); "+
		"the OS sandbox blocked a file effect rather than the command failing on its own. "+
		"Keep file effects inside the sandbox policy, or ask the operator to adjust the sandbox mode; per-call escalation is not available]",
		matched, modeText, enforcementText)
}
