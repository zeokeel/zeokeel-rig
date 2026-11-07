package lsp

import (
	"fmt"
	"strings"
)

// Operation 是一种受支持的只读 LSP 查询。规范值沿用旧版 LSP 工具面的 DSH 命名，
// NormalizeOperation 同时接受短名与下划线别名。
type Operation string

// 受支持的查询操作。
const (
	// OpGoToDefinition 是 textDocument/definition 查询。
	OpGoToDefinition Operation = "goToDefinition"
	// OpFindReferences 是 textDocument/references 查询。
	OpFindReferences Operation = "findReferences"
	// OpGoToImplementation 是 textDocument/implementation 查询。
	OpGoToImplementation Operation = "goToImplementation"
	// OpHover 是 textDocument/hover 查询。
	OpHover Operation = "hover"
)

// 短名常量是规范操作名的别名，便于调用方使用更贴近模型工具名的拼写。
const (
	// OpDefinition 是 OpGoToDefinition 的短名别名。
	OpDefinition = OpGoToDefinition
	// OpReferences 是 OpFindReferences 的短名别名。
	OpReferences = OpFindReferences
	// OpImplementation 是 OpGoToImplementation 的短名别名。
	OpImplementation = OpGoToImplementation
)

// operationAliases 把可接受的操作拼写（小写）映射到规范操作。规范 DSH 名大小写不敏感，
// 同时接受短名与下划线拼写。
var operationAliases = map[string]Operation{
	"gotodefinition":      OpGoToDefinition,
	"goto_definition":     OpGoToDefinition,
	"definition":          OpGoToDefinition,
	"findreferences":      OpFindReferences,
	"find_references":     OpFindReferences,
	"references":          OpFindReferences,
	"gotoimplementation":  OpGoToImplementation,
	"goto_implementation": OpGoToImplementation,
	"implementation":      OpGoToImplementation,
	"hover":               OpHover,
}

// NormalizeOperation 校验并规范化一个操作名。规范 DSH 名按大小写不敏感接受，同时接受
// definition/references/implementation 与 hover 短别名。
func NormalizeOperation(raw string) (Operation, error) {
	value := strings.TrimSpace(raw)
	if operation, ok := operationAliases[strings.ToLower(value)]; ok {
		return operation, nil
	}
	const valid = "goToDefinition, findReferences, goToImplementation or hover"
	if value == "" {
		return "", fmt.Errorf(`"operation" is required (want %s)`, valid)
	}
	return "", fmt.Errorf("unknown operation %q (want %s)", value, valid)
}

// operationLabel 返回操作用于错误消息的人类可读名称。
func operationLabel(operation Operation) string {
	switch operation {
	case OpGoToDefinition:
		return "definition"
	case OpFindReferences:
		return "references"
	case OpGoToImplementation:
		return "implementation"
	case OpHover:
		return "hover"
	default:
		return string(operation)
	}
}

// operationMethod 返回操作对应的 LSP 方法名。
func operationMethod(operation Operation) string {
	switch operation {
	case OpGoToDefinition:
		return "textDocument/definition"
	case OpFindReferences:
		return "textDocument/references"
	case OpGoToImplementation:
		return "textDocument/implementation"
	case OpHover:
		return "textDocument/hover"
	default:
		return ""
	}
}

// referenceContext 是 textDocument/references 的 LSP 上下文参数。
type referenceContext struct {
	// IncludeDeclaration 表示结果包含符号声明本身；与旧版行为一致，固定为 true。
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// referenceParams 是 textDocument/references 的位置参数。
type referenceParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	Context      referenceContext       `json:"context"`
}

// operationParams 构建一次位置查询的 JSON-RPC 参数。references 额外携带
// includeDeclaration=true 的上下文；其余操作使用标准的文档+位置参数。
func operationParams(operation Operation, uri string, position Position) any {
	if operation == OpFindReferences {
		return referenceParams{
			TextDocument: textDocumentIdentifier{URI: uri},
			Position:     position,
			Context:      referenceContext{IncludeDeclaration: true},
		}
	}
	return textDocumentPositionParams{
		TextDocument: textDocumentIdentifier{URI: uri},
		Position:     position,
	}
}
