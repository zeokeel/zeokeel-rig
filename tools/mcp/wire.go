package mcp

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// 本文件保存 JSON-RPC 2.0 与 MCP 线上类型。只建模客户端读写需要的字段；服务端未知字段
// 会被忽略。

// requestMessage 是发给服务端的一个 JSON-RPC 请求。
type requestMessage struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// notificationMessage 是一个 JSON-RPC 通知（无 id、无响应）。
type notificationMessage struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// responseEnvelope 是从服务端读到的一条消息。响应携带 id 与 result 或 error；通知/服务端
// 请求携带 method 且无 id。服务端到客户端的请求（sampling、roots、elicitation）不在本
// 客户端子集内，会被忽略。
type responseEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// RPCError 是一个 JSON-RPC 错误对象。Error() 包含数字错误码、服务端消息与可选 data 载荷
// 的简短渲染。
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// wireTool 是 tools/list 结果中的一个工具。
type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// wireToolListResult 是 tools/list 结果。最后一页的 NextCursor 为空。
type wireToolListResult struct {
	Tools      []wireTool `json:"tools"`
	NextCursor string     `json:"nextCursor"`
}

// wireCallToolResult 是 tools/call 结果。
type wireCallToolResult struct {
	Content           []wireContent   `json:"content"`
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
}

// wireContent 是 tools/call 结果中的一个内容项。MCP 定义 text、image、audio 与 resource
// 项；客户端把它们映射为 Content。
type wireContent struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	Data     string        `json:"data,omitempty"`
	MimeType string        `json:"mimeType,omitempty"`
	Resource *wireResource `json:"resource,omitempty"`
}

// wireResource 是资源内容项的内嵌资源载荷。
type wireResource struct {
	URI      string `json:"uri"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// initializeResult 是 initialize 响应中被校验的部分。
type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Capabilities json.RawMessage `json:"capabilities"`
}

// Error 实现 error 接口。消息与 data 载荷的简短渲染都会包含，使仅凭 Go 错误即可诊断
// 服务端失败。
func (e *RPCError) Error() string {
	if e == nil {
		return "mcp: json-rpc error"
	}
	message := e.Message
	if message == "" {
		message = "server error"
	}
	base := "mcp: json-rpc error " + strconv.Itoa(e.Code) + ": " + message
	if data := summarizeData(e.Data); data != "" {
		base += " (data: " + data + ")"
	}
	return base
}

// summarizeData 渲染可选的 JSON-RPC 错误 data，并施加强制长度上限；错误在日志中必须
// 保持可读。
func summarizeData(raw json.RawMessage) string {
	trimmed := string(raw)
	trimmed = string(bytes.TrimSpace([]byte(trimmed)))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	const max = 200
	if len(trimmed) > max {
		return trimmed[:max] + "…"
	}
	return trimmed
}

// --- resources/ 与 prompts/ 线上类型 ---------------------------------------
//
// resources 与 prompts 子集复用与 tools 相同的 JSON-RPC 信封；只有方法专属的结果形状是
// 新的。

// wireResourceInfo 是 resources/list 的一项。（tools/call 内容项的内嵌资源载荷是上面
// 单独的 wireResource 类型。）
type wireResourceInfo struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// wireResourceListResult 是 resources/list 结果。最后一页的 NextCursor 为空。
type wireResourceListResult struct {
	Resources  []wireResourceInfo `json:"resources"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// wireResourceTemplate 是 resources/templates/list 的一项。URITemplate 是 RFC 6570
// URI 模板，模型可填充其变量来读取资源。
type wireResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// wireResourceTemplateListResult 是 resources/templates/list 结果。最后一页的
// NextCursor 为空。
type wireResourceTemplateListResult struct {
	ResourceTemplates []wireResourceTemplate `json:"resourceTemplates"`
	NextCursor        string                 `json:"nextCursor,omitempty"`
}

// wireResourceContents 是 resources/read 的一项内容。
type wireResourceContents struct {
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// wireReadResourceResult 是 resources/read 结果。
type wireReadResourceResult struct {
	Contents []wireResourceContents `json:"contents"`
}

// wirePromptArgument 是 prompt 接受的一个参数。
type wirePromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// wirePrompt 是 prompts/list 的一项。
type wirePrompt struct {
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Arguments   []wirePromptArgument `json:"arguments,omitempty"`
}

// wirePromptListResult 是 prompts/list 结果。
type wirePromptListResult struct {
	Prompts    []wirePrompt `json:"prompts"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

// wirePromptMessage 是 prompts/get 结果中的一条消息。MCP prompt 内容是单个内容对象，
// 因此复用工具内容形状。
type wirePromptMessage struct {
	Role    string      `json:"role"`
	Content wireContent `json:"content"`
}

// wireGetPromptResult 是 prompts/get 结果。
type wireGetPromptResult struct {
	Description string              `json:"description,omitempty"`
	Messages    []wirePromptMessage `json:"messages"`
}
