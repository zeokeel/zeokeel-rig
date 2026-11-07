package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Language Server Protocol 的 JSON-RPC 分帧。
//
// 每条消息是 Content-Length 头、一个空行，然后恰好这么多字节的 UTF-8 JSON：
// "Content-Length: 42\r\n\r\n{...}"。

// maxMessageBytes 是单条入站消息的硬上限，防止损坏或恶意的服务端让客户端分配无界内存。
const maxMessageBytes = 16 << 20

// maxHeaderBytes 是单条入站消息头块的硬上限。头在 Content-Length 已知之前读取，没有上限
// 时恶意服务端可以发送任意大的头（或一条无尽的行），在消息上限生效前就让客户端分配无界
// 内存。
const maxHeaderBytes = 64 << 10

// wireMessage 是一个 LSP JSON-RPC 信封：客户端的请求/通知、服务端的响应，或服务端发起的
// 请求/通知。
type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

// wireError 是语言服务器返回的 JSON-RPC 错误对象。
type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error 实现 error。
func (e *wireError) Error() string {
	if e == nil {
		return "language server error"
	}
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("language server error (code %d)", e.Code)
	}
	return fmt.Sprintf("language server error (code %d): %s", e.Code, strings.TrimSpace(e.Message))
}

// writeMessage 写出一个 Content-Length 分帧的 JSON-RPC 消息。
func writeMessage(w io.Writer, msg wireMessage) error {
	if w == nil {
		return errors.New("lsp: cannot write to a nil stream")
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("lsp: encode %s: %w", msg.Method, err)
	}
	header := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	if _, err := io.WriteString(w, header); err != nil {
		return fmt.Errorf("lsp: write message header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("lsp: write message body: %w", err)
	}
	return nil
}

// maxHeaderLines 限制头行数，避免服务端把整个 64 KiB 字节预算耗在数以百万计的空行上
// （map 开销）。
const maxHeaderLines = 128

// readHeader 读取一个有界的 MIME 风格头块。键会被转小写；块以空行结束。上限在读取过程中
// 通过 bufio.Reader.ReadLine 强制，长行以有界片段返回：单条未终止的头行会在增长缓冲区之前
// 被拒绝，而不是在分配之后。
func readHeader(r *bufio.Reader) (map[string]string, error) {
	header := make(map[string]string, 4)
	total := 0
	for {
		line, consumed, err := readHeaderLine(r, maxHeaderBytes-total)
		if err != nil {
			if errors.Is(err, io.EOF) && total == 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("lsp: read message header: %w", err)
		}
		total += consumed
		if line == "" {
			return header, nil
		}
		if len(header) >= maxHeaderLines {
			return nil, fmt.Errorf("lsp: message header has more than %d lines", maxHeaderLines)
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("lsp: malformed message header line %q", line)
		}
		header[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
}

// readHeaderLine 读取一行以 CRLF/LF 结尾的行，并对其可消费字节（含行终止符）施加强制
// 上限。它使用 bufio.Reader.ReadLine，使超过 reader 缓冲区的行以片段返回，从而能在调用方
// 追加超过剩余预算之前拒绝。
func readHeaderLine(r *bufio.Reader, remaining int) (line string, consumed int, err error) {
	if remaining <= 0 {
		return "", 0, fmt.Errorf("lsp: message header exceeds %d bytes", maxHeaderBytes)
	}
	var buf []byte
	for {
		fragment, isPrefix, readErr := r.ReadLine()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if consumed == 0 && len(buf) == 0 {
					return "", 0, io.EOF
				}
				return "", consumed, fmt.Errorf("lsp: truncated message header: %w", io.ErrUnexpectedEOF)
			}
			return "", consumed, readErr
		}
		consumed += len(fragment) + 1 // + 换行；一对 CRLF 在预算上至多多消耗一个字节
		if consumed > remaining {
			return "", consumed, fmt.Errorf("lsp: message header exceeds %d bytes", maxHeaderBytes)
		}
		buf = append(buf, fragment...)
		if !isPrefix {
			return string(buf), consumed, nil
		}
	}
}

// readMessage 读取一条 Content-Length 分帧的 JSON-RPC 消息。
func readMessage(r *bufio.Reader) (wireMessage, error) {
	if r == nil {
		return wireMessage{}, errors.New("lsp: cannot read from a nil stream")
	}
	header, err := readHeader(r)
	if err != nil {
		return wireMessage{}, err
	}
	rawLength := header["content-length"]
	if rawLength == "" {
		return wireMessage{}, errors.New("lsp: message is missing the Content-Length header")
	}
	length, err := strconv.Atoi(rawLength)
	if err != nil || length < 0 || length > maxMessageBytes {
		return wireMessage{}, fmt.Errorf("lsp: invalid Content-Length %q", rawLength)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return wireMessage{}, fmt.Errorf(
				"lsp: truncated message body: Content-Length %d does not match the readable bytes: %w", length, err)
		}
		return wireMessage{}, fmt.Errorf("lsp: read message body: %w", err)
	}
	var msg wireMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return wireMessage{}, fmt.Errorf("lsp: message body is not valid JSON: %w", err)
	}
	return msg, nil
}

// isJSONNull 判断 raw 是否为 JSON 字面量 null（或为空）。
func isJSONNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// encodeParams 序列化请求/通知参数。nil 参数保持缺省。
func encodeParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	data, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("lsp: encode params: %w", err)
	}
	return data, nil
}

// rawID 把数字请求 ID 渲染为 JSON-RPC id 值。
func rawID(id int64) json.RawMessage {
	return json.RawMessage(strconv.FormatInt(id, 10))
}
