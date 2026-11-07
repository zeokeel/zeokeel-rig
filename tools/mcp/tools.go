package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// DefaultPrefix 是 Tools 与 NewTools 在调用方传入空前缀时使用的模型可见工具名前缀，
// 生成 "mcp__<server>__<tool>" 形式的名字。
const DefaultPrefix = "mcp__"

// ToolInfo 是 MCP server 公布的一个工具。
type ToolInfo struct {
	Name        string
	Description string
	// InputSchema 是服务端返回的工具 JSON Schema 对象。
	InputSchema json.RawMessage
}

// Content 是 tools/call 结果中的一个内容项。Type 为 "text"、"image"、"audio"、
// "resource" 或服务端自定义的将来类型；Text 用于文本（与资源）项，Data 是二进制项的
// base64 载荷，MimeType 描述 image/audio/resource 载荷。
type Content struct {
	Type     string
	Text     string
	Data     string
	MimeType string
}

// CallResult 是一次 tools/call 结果。IsError 标记工具级失败（调用本身成功）；
// StructuredContent 在存在时原样保留服务端的 structuredContent JSON。
type CallResult struct {
	Content           []Content
	IsError           bool
	StructuredContent json.RawMessage
}

// mapContent 把一条 wire 内容项转换为公开的 Content 值。
func mapContent(wire wireContent) Content {
	content := Content{
		Type:     wire.Type,
		Text:     wire.Text,
		Data:     wire.Data,
		MimeType: wire.MimeType,
	}
	if wire.Resource != nil {
		content.MimeType = wire.Resource.MimeType
		switch {
		case wire.Resource.URI != "":
			content.Text = wire.Resource.URI
		case wire.Resource.Name != "":
			content.Text = wire.Resource.Name
		}
	}
	return content
}

// Tools 把 client 公布的所有工具适配为 rig.Tool。
//
// 它是编译期接线便利函数：用后台上下文（受客户端超时约束）列出工具，列出失败时返回
// nil。需要在错误时处理时，改用 ListTools + NewTools。
func Tools(client *Client, prefix string) []rig.Tool {
	if client == nil {
		return nil
	}
	infos, err := client.ListTools(context.Background())
	if err != nil {
		return nil
	}
	return NewTools(client, client.ServerName(), infos, prefix)
}

// NewTools 包装已经列出的工具，不执行 I/O。client 是工具调用使用的连接（通常就是列出
// 它们的 client），server 是名称中使用的服务端身份（通常是 client.ServerName()），
// prefix 默认是 DefaultPrefix。
//
// 每个包装工具把 Invoke 转发给 client.CallTool；文本内容用换行连接，image/audio/resource
// 项变成占位行，只有 structuredContent 的结果回退为它的 JSON 文本。工具级 isError 映射为
// ToolResult.IsError。
func NewTools(client *Client, server string, infos []ToolInfo, prefix string) []rig.Tool {
	prefix = normalizedPrefix(prefix)
	serverPart := sanitizeName(server)
	tools := make([]rig.Tool, 0, len(infos))
	for _, info := range infos {
		if info.Name == "" {
			continue
		}
		tools = append(tools, &tool{
			client:      client,
			server:      server,
			remoteName:  info.Name,
			name:        prefix + serverPart + "__" + sanitizeName(info.Name),
			description: info.Description,
			schema:      schemaMap(info.InputSchema),
		})
	}
	return tools
}

// normalizedPrefix 在 prefix 为空时应用默认前缀。
func normalizedPrefix(prefix string) string {
	if prefix == "" {
		return DefaultPrefix
	}
	return prefix
}

// tool 是适配为 rig.Tool 的一个 MCP 工具。
type tool struct {
	client      *Client
	server      string
	remoteName  string
	name        string
	description string
	schema      map[string]any
}

var _ rig.Tool = (*tool)(nil)

// Name 实现 rig.Tool。
func (t *tool) Name() string { return t.name }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	if strings.TrimSpace(t.description) != "" {
		return t.description
	}
	return "MCP tool " + t.remoteName + " from " + t.server
}

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any { return t.schema }

// Invoke 通过调用 MCP server 并渲染其内容来实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if t.client == nil {
		return rig.ToolResult{}, fmt.Errorf("mcp: tool %q has no client", t.name)
	}
	result, err := t.client.CallTool(ctx, t.remoteName, json.RawMessage(call.Arguments))
	if err != nil {
		return rig.ToolResult{}, err
	}
	text := renderContent(result.Content)
	if text == "" && len(bytes.TrimSpace(result.StructuredContent)) > 0 {
		text = string(bytes.TrimSpace(result.StructuredContent))
	}
	return rig.ToolResult{Content: text, IsError: result.IsError}, nil
}

// renderContent 把工具内容渲染为模型可读的纯文本：文本项直接拼接，非文本项变成说明性
// 占位。
func renderContent(contents []Content) string {
	parts := make([]string, 0, len(contents))
	for _, content := range contents {
		switch content.Type {
		case "text":
			parts = append(parts, content.Text)
		case "image":
			parts = append(parts, placeholder("image", content.MimeType))
		case "audio":
			parts = append(parts, placeholder("audio", content.MimeType))
		case "resource":
			label := strings.TrimSpace(content.Text)
			if label == "" {
				label = "embedded resource"
			}
			parts = append(parts, "[resource: "+label+"]")
		default:
			typeName := content.Type
			if typeName == "" {
				typeName = "unknown"
			}
			parts = append(parts, placeholder(typeName, content.MimeType))
		}
	}
	return strings.Join(parts, "\n")
}

// placeholder 把一个非文本内容项渲染为模型可读的说明。
func placeholder(kind, mimeType string) string {
	if mimeType == "" {
		return "[" + kind + "]"
	}
	return "[" + kind + ": " + mimeType + "]"
}

// schemaMap 把原始 JSON Schema 转换为 rig.Tool 期望的 map。缺失/非法的 schema 退化为
// 空对象 schema，使工具仍可调用（校验权威仍在服务端）。
func schemaMap(raw json.RawMessage) map[string]any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]any{"type": "object"}
	}
	var schema map[string]any
	if err := json.Unmarshal(trimmed, &schema); err != nil || schema == nil {
		return map[string]any{"type": "object"}
	}
	return schema
}

// sanitizeName 把 MCP 名称映射到模型函数名允许的字符集（[A-Za-z0-9_-]）。集合之外的
// 字符变为下划线，结果为空时回退为 "tool"。
func sanitizeName(name string) string {
	var builder strings.Builder
	builder.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	out := strings.Trim(builder.String(), "_")
	if out == "" {
		return "tool"
	}
	return out
}
