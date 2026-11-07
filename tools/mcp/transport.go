package mcp

// 本文件定义传输接缝：Client 通过 transport 收发 JSON-RPC，stdio（子进程）与 Streamable
// HTTP（远端端点）是两个实现。Client 负责与框架无关的行为（握手、id、超时、会话重试），
// transport 负责线上分帧。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNotInitialized 在传输被要求在 initialize 握手完成前发送请求时返回。HTTP 传输强制
// 这一点：新传输只允许 initialize。
var ErrNotInitialized = errors.New("mcp: transport is not initialized")

// ErrSessionExpired 在 Streamable HTTP 传输收到服务端 404/410（会话未知或过期）时返回。
// 调用方可以重启 client 建立新会话。
var ErrSessionExpired = errors.New("mcp: session expired")

// rpcResponse 是一次请求的传输结果：原始 result JSON 或 JSON-RPC 错误。
type rpcResponse struct {
	result json.RawMessage
	err    error
}

// transport 发送 JSON-RPC 请求/通知并返回匹配的响应。实现必须可安全并发调用
// Request/Notify；Close 幂等。
type transport interface {
	// Request 发送一个请求并返回匹配 msg.ID 的响应。JSON-RPC 错误以 rpcResponse.err 返回
	// （通常是 *RPCError）；协议/传输失败作为 error 返回。
	Request(ctx context.Context, msg requestMessage) (rpcResponse, error)
	// Notify 发送一个通知（不期待响应）。
	Notify(ctx context.Context, msg notificationMessage) error
	// Close 释放传输资源。
	Close() error
}

// responseFromEnvelope 把一个解码后的 JSON-RPC 信封转换为 id 对应的传输响应。
func responseFromEnvelope(envelope *responseEnvelope, id uint64) rpcResponse {
	response := rpcResponse{result: envelope.Result}
	if envelope.Error != nil {
		response.err = envelope.Error
	} else if len(bytes.TrimSpace(envelope.Result)) == 0 {
		response.err = fmt.Errorf("mcp: response %d carries neither result nor error", id)
	}
	return response
}

// decodeResponseID 解析服务端回显的 JSON-RPC id。客户端发送数字 id；同一数字的字符串形式
// 也可容忍。
func decodeResponseID(raw json.RawMessage) (uint64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false
	}
	text := string(trimmed)
	if strings.HasPrefix(text, `"`) {
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return 0, false
		}
	}
	id, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}
