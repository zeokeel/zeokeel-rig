package mcpserver

// 本文件是 JSON-RPC 2.0 + MCP 的线上类型与编解码：一条消息 = 一行 JSON（NDJSON），
// 与 tools/mcp 的 stdio 客户端逐字段对齐（client 的 requestMessage/responseEnvelope
// 就在那边）。这里只建模 server 侧需要读懂/写出的字段，未知字段一律忽略。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MainVersion 是服务器默认回复的协议版本（客户端请求的版本不在
// SupportedProtocolVersions 里时回落到它）。
const ProtocolVersion = "2025-06-18"

// SupportedProtocolVersions 列出本 server 理解的 MCP 协议版本。initialize 会回显
// 客户端的版本（双方都能接受），否则回落到 ProtocolVersion。
var SupportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC 2.0 错误码。
const (
	// CodeParseError 是"收到的不是合法 JSON"（含超长行）。
	CodeParseError = -32700
	// CodeInvalidRequest 是"是合法 JSON 但不是合法的 JSON-RPC 请求"（含批量数组）。
	CodeInvalidRequest = -32600
	// CodeMethodNotFound 是未知方法。
	CodeMethodNotFound = -32601
	// CodeInvalidParams 是参数形状不对（未知工具名、arguments 不是对象等）。
	CodeInvalidParams = -32602
	// CodeInternalError 是服务端内部失败（正常路径不应出现）。
	CodeInternalError = -32603
)

// MCP 方法名。
const (
	methodInitialize   = "initialize"
	methodInitialized  = "notifications/initialized"
	methodPing         = "ping"
	methodToolsList    = "tools/list"
	methodToolsCall    = "tools/call"
	methodCancelled    = "notifications/cancelled"
	methodLogMessage   = "notifications/message"
	notificationPrefix = "notifications/"
)

// wireRequest 是一条入站 JSON-RPC 消息。ID 保留原始字节以做到"id 类型保真"
// （客户端发 number 就回 number，发 string 就回 string）。
type wireRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// isNotification 报告这条消息是否是通知（无 id，或显式 id:null——后者按 JSON-RPC
// 的建议当作"无法应答的请求"处理，不产生输出）。
func (r wireRequest) isNotification() bool {
	trimmed := bytes.TrimSpace(r.ID)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// idKey 返回用于关联通知/应答的 ID 键（number 与 string 各自保真）。
func (r wireRequest) idKey() string {
	return string(bytes.TrimSpace(r.ID))
}

// wireError 是 JSON-RPC 错误对象。
type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// wireResponse 是一条出站响应。Result 用 json.RawMessage：ping 的空结果必须序列化
// 成 `{}` 而不是被 omitempty 丢掉。
type wireResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

// wireNotification 是一条出站通知（无 id）。
type wireNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// wireTool 是 tools/list 里的一项。InputSchema 一定是 JSON 对象。
type wireTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// wireToolListResult 是 tools/list 结果。tools 永远是数组（空也是 []）。
type wireToolListResult struct {
	Tools []wireTool `json:"tools"`
}

// wireContent 是 tools/call 结果中的一个内容块。当前只产出 text（图片等二进制块
// 需要 base64 数据，不属于本子集）。
type wireContent struct {
	Type string `json:"type"`
	// Text 没有 omitempty：MCP 要求 text 块必须带 text 字段，工具返回空字符串时
	// 也要写出 `"text":""`（缺字段会让严格的客户端解析失败）。
	Text     string         `json:"text"`
	Data     string         `json:"data,omitempty"`     // image/audio/resource 的 base64 载荷
	MimeType string         `json:"mimeType,omitempty"` // 同上
	Meta     map[string]any `json:"_meta,omitempty"`    // 例如 {"truncated":true}
}

// wireCallResult 是 tools/call 结果。IsError 只在 true 时出现，成功响应里没有这个
// 字段（与 tools/mcp 客户端的解析一致）。
type wireCallResult struct {
	Content []wireContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// rpcFailure 是"协议层失败"：它会被编码成 JSON-RPC error 对象（而不是 Go error），
// 因为对客户端来说这仍然是"请求得到应答"。
type rpcFailure struct {
	Code    int
	Message string
}

// Error 实现 error（只在内部用于收集诊断）。
func (f *rpcFailure) Error() string {
	if f == nil {
		return "<nil>"
	}
	return fmt.Sprintf("json-rpc error %d: %s", f.Code, f.Message)
}

// parseRequest 解析一行入站消息。
//
// 返回 *rpcFailure 表示"能定位到问题、应当回一个 JSON-RPC error"，一定是协议层
// 失败（parse error / invalid request）；返回 error 表示不可恢复（I/O 层）。
func parseRequest(line []byte) (wireRequest, *rpcFailure) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return wireRequest{}, &rpcFailure{Code: CodeInvalidRequest, Message: "empty message"}
	}
	// 批量请求（数组）不支持：MCP 的 stdio 传输里也不会出现。
	if trimmed[0] == '[' {
		return wireRequest{}, &rpcFailure{Code: CodeInvalidRequest, Message: "batch requests are not supported"}
	}
	var request wireRequest
	if err := json.Unmarshal(trimmed, &request); err != nil {
		return wireRequest{}, &rpcFailure{Code: CodeParseError, Message: "invalid JSON: " + err.Error()}
	}
	if request.Method == "" {
		return request, &rpcFailure{Code: CodeInvalidRequest, Message: "missing method"}
	}
	return request, nil
}

// encodeMessage 把一条出站消息编码成"一行 JSON + '\n'"。
func encodeMessage(message any) ([]byte, error) {
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: encode message: %w", err)
	}
	return append(encoded, '\n'), nil
}

// encodeResult 编码成功响应。id 为空（无法定位）时用 null。
func encodeResult(id json.RawMessage, result any) ([]byte, error) {
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: encode result: %w", err)
	}
	return encodeMessage(wireResponse{JSONRPC: "2.0", ID: normalizeID(id), Result: payload})
}

// encodeFailure 编码错误响应。id 是 json.RawMessage，无法定位时传 nil。
func encodeFailure(id json.RawMessage, failure *rpcFailure) ([]byte, error) {
	return encodeMessage(wireResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Error:   &wireError{Code: failure.Code, Message: failure.Message},
	})
}

// normalizeID 保证响应里一定有 id 字段（JSON-RPC 要求 error 响应也带 id，未知时为
// null）。
func normalizeID(id json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(trimmed)
}

// readLine 从 reader 读一行（以 '\n' 结尾；末行可以没有换行），并强制行长上限。
//
// tooLong=true 表示这一行超过了 limit：调用方必须回 parse error 并**继续读**，
// 而不是断开流。返回值里的 err 只在 I/O 失败或 EOF 时非 nil；EOF 时如果还带回了
// 一条末行，调用方需要先处理它再退出。
func readLine(reader *bufio.Reader, limit int) (line []byte, tooLong bool, err error) {
	buffer := make([]byte, 0, 4096)
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if len(chunk) > 0 && !tooLong {
			if len(buffer)+len(chunk) > limit+1 { // +1 是行尾换行符
				tooLong = true
				buffer = nil
			} else {
				buffer = append(buffer, chunk...)
			}
		}
		switch {
		case readErr == nil:
			return bytes.TrimRight(buffer, "\r\n"), tooLong, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			// 缓冲区满但还没读到换行：继续读同一行。
			continue
		case errors.Is(readErr, io.EOF):
			if len(buffer) == 0 && !tooLong {
				return nil, false, io.EOF
			}
			return bytes.TrimRight(buffer, "\r\n"), tooLong, io.EOF
		default:
			return nil, tooLong, readErr
		}
	}
}

// 编译期断言：错误对象必须能编码（Data 为 any，nil 时被 omitempty 丢掉）。
var _ = json.Marshal
