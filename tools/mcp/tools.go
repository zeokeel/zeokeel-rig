package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// DefaultPrefix is the model-facing tool-name prefix applied by Tools and
// NewTools when the caller passes an empty prefix. It yields names of the form
// "mcp__<server>__<tool>".
const DefaultPrefix = "mcp__"

// ToolInfo is one tool advertised by an MCP server.
type ToolInfo struct {
	Name        string
	Description string
	// InputSchema is the tool's JSON Schema object as returned by the server.
	InputSchema json.RawMessage
}

// Content is one content item of a tools/call result. Type is "text", "image",
// "audio", "resource" or a future server-defined type; Text is set for text
// (and resource) items, Data is the base64 payload for binary items and
// MimeType describes image/audio/resource payloads.
type Content struct {
	Type     string
	Text     string
	Data     string
	MimeType string
}

// CallResult is one tools/call result. IsError marks a tool-level failure (the
// call itself succeeded); StructuredContent preserves the server's
// structuredContent JSON verbatim when present.
type CallResult struct {
	Content           []Content
	IsError           bool
	StructuredContent json.RawMessage
}

// mapContent translates one wire content item into the public Content value.
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

// Tools adapts every tool advertised by client into a contracts/rig.Tool.
//
// It is the compile-time wiring convenience: it lists tools with a background
// context (bounded by the client timeout) and returns nil when listing fails.
// Call ListTools + NewTools when the error must be handled.
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

// NewTools wraps already-listed tools without performing I/O. client is the
// connection the tools invoke (normally the same client that listed them),
// server is the server identity used in names (normally client.ServerName())
// and prefix defaults to DefaultPrefix.
//
// Each wrapped tool forwards Invoke to client.CallTool; text content is joined
// with newlines, image/audio/resource items become placeholder lines, and a
// result with only structuredContent falls back to its JSON text. A tool-level
// isError maps to ToolResult.IsError.
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

// normalizedPrefix applies the default prefix when prefix is empty.
func normalizedPrefix(prefix string) string {
	if prefix == "" {
		return DefaultPrefix
	}
	return prefix
}

// tool is one MCP tool adapted to contracts/rig.Tool.
type tool struct {
	client      *Client
	server      string
	remoteName  string
	name        string
	description string
	schema      map[string]any
}

var _ rig.Tool = (*tool)(nil)

// Name implements rig.Tool.
func (t *tool) Name() string { return t.name }

// Description implements rig.Tool.
func (t *tool) Description() string {
	if strings.TrimSpace(t.description) != "" {
		return t.description
	}
	return "MCP tool " + t.remoteName + " from " + t.server
}

// Schema implements rig.Tool.
func (t *tool) Schema() map[string]any { return t.schema }

// Invoke implements rig.Tool by calling the MCP server and rendering its content.
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

// renderContent renders tool content as plain text for the model: text items
// are concatenated, non-text items become explanatory placeholders.
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

// placeholder renders one non-text content item as a model-readable note.
func placeholder(kind, mimeType string) string {
	if mimeType == "" {
		return "[" + kind + "]"
	}
	return "[" + kind + ": " + mimeType + "]"
}

// schemaMap converts the raw JSON Schema into the map rig.Tool expects. An
// absent/invalid schema degrades to an empty object schema so the tool stays
// callable (the server remains the authority on validation).
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

// sanitizeName maps an MCP name to the character set model function names use
// ([A-Za-z0-9_-]). Characters outside the set become underscore, and an empty
// result falls back to "tool".
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
