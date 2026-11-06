package mcp

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// This file holds the JSON-RPC 2.0 and MCP wire types. Only the fields the
// client reads or writes are modelled; unknown server fields are ignored.

// requestMessage is one JSON-RPC request sent to the server.
type requestMessage struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// notificationMessage is one JSON-RPC notification (no id, no response).
type notificationMessage struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// responseEnvelope is one message read from the server. A response carries an
// id plus result or error; a notification/server request carries a method and
// no id. Server-to-client requests (sampling, roots, elicitation) are outside
// this client's subset and are ignored.
type responseEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// RPCError is one JSON-RPC error object. Error() includes the numeric code,
// the server message and a short rendering of the optional data payload.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// wireTool is one tool in a tools/list result.
type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// wireToolListResult is the tools/list result. NextCursor is empty on the last
// page.
type wireToolListResult struct {
	Tools      []wireTool `json:"tools"`
	NextCursor string     `json:"nextCursor"`
}

// wireCallToolResult is the tools/call result.
type wireCallToolResult struct {
	Content           []wireContent   `json:"content"`
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
}

// wireContent is one content item of a tools/call result. MCP defines text,
// image, audio and resource items; the client maps them into Content.
type wireContent struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	Data     string        `json:"data,omitempty"`
	MimeType string        `json:"mimeType,omitempty"`
	Resource *wireResource `json:"resource,omitempty"`
}

// wireResource is the embedded resource payload of a resource content item.
type wireResource struct {
	URI      string `json:"uri"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// initializeResult is the validated part of the initialize response.
type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Capabilities json.RawMessage `json:"capabilities"`
}

// Error implements the error interface. The message and a short rendering of
// the data payload are included so a server-side failure is diagnosable from
// the Go error alone.
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

// summarizeData renders the optional JSON-RPC error data with a hard length
// cap; errors must stay readable in logs.
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
