package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 资源/prompt 工具的模型可见名称。ResourceTools 会把非空前缀加到这些名称前面。
const (
	ToolListResources         = "mcp_list_resources"
	ToolReadResource          = "mcp_read_resource"
	ToolListResourceTemplates = "mcp_list_resource_templates"
	ToolListPrompts           = "mcp_list_prompts"
	ToolGetPrompt             = "mcp_get_prompt"
)

// ResourceTools 把一个 MCP client 的 resources/ 与 prompts/ 子集适配为五个
// rig.Tool：
//
//	mcp_list_resources           （无参数）      -> 每行一个资源
//	mcp_read_resource            （uri）         -> 资源文本，二进制只做摘要
//	mcp_list_resource_templates  （无参数）      -> 每个 URI 模板一项
//	mcp_list_prompts             （无参数）      -> 每个 prompt 一项
//	mcp_get_prompt               （name, arguments?） -> prompt 消息渲染为文本
//
// prefix 会原样加到每个名称前；空前缀时名称就是上面的常量。这些工具每次调用时才懒加载
// 列表（ResourceTools 不产生 I/O），因此 client 可以在服务端就绪前创建；nil client 会在
// 调用时报告配置错误。
func ResourceTools(client *Client, prefix string) []rig.Tool {
	return []rig.Tool{
		&mcpResourceTool{
			name:        prefix + ToolListResources,
			description: "List the MCP resources (documents, files, records, ...) the server exposes.",
			schema:      toolutil.ObjectSchema(map[string]any{}),
			invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
				if client == nil {
					return rig.ToolResult{}, fmt.Errorf("mcp: %s has no client", prefix+ToolListResources)
				}
				resources, err := client.ListResources(ctx)
				if err != nil {
					return rig.ToolResult{}, err
				}
				return rig.ToolResult{Content: renderResources(resources)}, nil
			},
		},
		&mcpResourceTool{
			name:        prefix + ToolReadResource,
			description: "Read one MCP resource by URI. Text contents are returned as-is; binary (blob) contents are summarized.",
			schema: toolutil.ObjectSchema(map[string]any{
				"uri": map[string]any{"type": "string", "description": "URI of the resource to read, as listed by mcp_list_resources."},
			}, "uri"),
			invoke: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
				if client == nil {
					return rig.ToolResult{}, fmt.Errorf("mcp: %s has no client", prefix+ToolReadResource)
				}
				var args struct {
					URI string `json:"uri"`
				}
				if err := toolutil.DecodeArgsAs("mcp", call, &args); err != nil {
					return rig.ToolResult{}, err
				}
				uri := strings.TrimSpace(args.URI)
				if uri == "" {
					return rig.ToolResult{Content: "mcp_read_resource: uri is required", IsError: true}, nil
				}
				contents, err := client.ReadResource(ctx, uri)
				if err != nil {
					return rig.ToolResult{}, err
				}
				return rig.ToolResult{Content: renderResourceContents(contents)}, nil
			},
		},
		&mcpResourceTool{
			name: prefix + ToolListResourceTemplates,
			description: "List the MCP resource templates the server exposes. Each URI template (RFC 6570) has a name, " +
				"optional description and media type; fill the template variables and use mcp_read_resource with the " +
				"resulting URI to read one resource.",
			schema: toolutil.ObjectSchema(map[string]any{}),
			invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
				if client == nil {
					return rig.ToolResult{}, fmt.Errorf("mcp: %s has no client", prefix+ToolListResourceTemplates)
				}
				templates, err := client.ListResourceTemplates(ctx)
				if err != nil {
					return rig.ToolResult{}, err
				}
				return rig.ToolResult{Content: renderResourceTemplates(templates)}, nil
			},
		},
		&mcpResourceTool{
			name:        prefix + ToolListPrompts,
			description: "List the MCP prompt templates the server exposes, with their arguments.",
			schema:      toolutil.ObjectSchema(map[string]any{}),
			invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
				if client == nil {
					return rig.ToolResult{}, fmt.Errorf("mcp: %s has no client", prefix+ToolListPrompts)
				}
				prompts, err := client.ListPrompts(ctx)
				if err != nil {
					return rig.ToolResult{}, err
				}
				return rig.ToolResult{Content: renderPrompts(prompts)}, nil
			},
		},
		&mcpResourceTool{
			name:        prefix + ToolGetPrompt,
			description: "Render one MCP prompt with arguments and return its messages as plain text (one role: text per line).",
			schema: toolutil.ObjectSchema(map[string]any{
				"name": map[string]any{"type": "string", "description": "Prompt name, as listed by mcp_list_prompts."},
				"arguments": map[string]any{
					"type":                 "object",
					"description":          "Prompt arguments as name/value string pairs.",
					"additionalProperties": map[string]any{"type": "string"},
				},
			}, "name"),
			invoke: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
				if client == nil {
					return rig.ToolResult{}, fmt.Errorf("mcp: %s has no client", prefix+ToolGetPrompt)
				}
				var args struct {
					Name      string            `json:"name"`
					Arguments map[string]string `json:"arguments"`
				}
				if err := toolutil.DecodeArgsAs("mcp", call, &args); err != nil {
					return rig.ToolResult{}, err
				}
				name := strings.TrimSpace(args.Name)
				if name == "" {
					return rig.ToolResult{Content: "mcp_get_prompt: name is required", IsError: true}, nil
				}
				messages, err := client.GetPrompt(ctx, name, args.Arguments)
				if err != nil {
					return rig.ToolResult{}, err
				}
				return rig.ToolResult{Content: renderPromptMessages(messages)}, nil
			},
		},
	}
}

// mcpResourceTool 是适配为 rig.Tool 的一个资源/prompt 工具。
type mcpResourceTool struct {
	name        string
	description string
	schema      map[string]any
	invoke      func(context.Context, rig.ToolCall) (rig.ToolResult, error)
}

var _ rig.Tool = (*mcpResourceTool)(nil)

// Name 实现 rig.Tool。
func (t *mcpResourceTool) Name() string { return t.name }

// Description 实现 rig.Tool。
func (t *mcpResourceTool) Description() string { return t.description }

// Schema 实现 rig.Tool。
func (t *mcpResourceTool) Schema() map[string]any { return t.schema }

// Invoke 实现 rig.Tool。
func (t *mcpResourceTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.invoke(ctx, call)
}

// renderResourceTemplates 把 resources/templates/list 渲染为每个模板一行：URI 模板、
// 可选名称/媒体类型与描述。
func renderResourceTemplates(templates []ResourceTemplate) string {
	if len(templates) == 0 {
		return "no resource templates"
	}
	lines := make([]string, 0, len(templates))
	for _, template := range templates {
		line := "- " + template.URITemplate
		if template.Name != "" {
			line += " (" + template.Name + ")"
		}
		if template.MimeType != "" {
			line += " [" + template.MimeType + "]"
		}
		if template.Description != "" {
			line += "\n  " + collapseMCPWhitespace(template.Description)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// renderResources 把 resources/list 渲染为每个资源一行。
func renderResources(resources []ResourceInfo) string {
	if len(resources) == 0 {
		return "no resources"
	}
	lines := make([]string, 0, len(resources))
	for _, resource := range resources {
		line := "- " + resource.URI
		if resource.Name != "" {
			line += " (" + resource.Name + ")"
		}
		if resource.MimeType != "" {
			line += " [" + resource.MimeType + "]"
		}
		if resource.Description != "" {
			line += "\n  " + collapseMCPWhitespace(resource.Description)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// renderResourceContents 渲染 resources/read。单个内容项直接返回其文本正文；多个内容项
// 会标注各自 URI，便于模型区分。
func renderResourceContents(contents []ResourceContents) string {
	if len(contents) == 0 {
		return "no contents"
	}
	parts := make([]string, 0, len(contents))
	for index, content := range contents {
		body := renderResourceContent(content)
		if len(contents) == 1 {
			parts = append(parts, body)
			continue
		}
		label := content.URI
		if label == "" {
			label = fmt.Sprintf("content %d", index+1)
		}
		parts = append(parts, label+":\n"+body)
	}
	return strings.Join(parts, "\n\n")
}

// renderResourceContent 渲染一个内容项。blob 载荷永远不发送给模型：只报告其媒体类型与
// 解码后大小。
func renderResourceContent(content ResourceContents) string {
	if content.Text != "" {
		return content.Text
	}
	if content.Blob != "" {
		mimeType := content.MimeType
		if mimeType == "" {
			mimeType = "unknown type"
		}
		return fmt.Sprintf("[binary content omitted: %s, %d bytes (base64)]", mimeType, decodedBlobSize(content.Blob))
	}
	return "(empty content)"
}

// decodedBlobSize 是 base64 blob 代表的字节数；非法载荷回退为长度上界。
func decodedBlobSize(blob string) int {
	if decoded, err := base64.StdEncoding.DecodeString(blob); err == nil {
		return len(decoded)
	}
	return len(blob) * 3 / 4
}

// renderPrompts 把 prompts/list 渲染为每个 prompt 一个块。
func renderPrompts(prompts []PromptInfo) string {
	if len(prompts) == 0 {
		return "no prompts"
	}
	lines := make([]string, 0, len(prompts)*3)
	for _, prompt := range prompts {
		lines = append(lines, "- "+prompt.Name)
		if prompt.Description != "" {
			lines = append(lines, "  "+collapseMCPWhitespace(prompt.Description))
		}
		if len(prompt.Arguments) > 0 {
			rendered := make([]string, 0, len(prompt.Arguments))
			for _, argument := range prompt.Arguments {
				text := argument.Name
				if argument.Required {
					text += " (required)"
				}
				if argument.Description != "" {
					text += ": " + collapseMCPWhitespace(argument.Description)
				}
				rendered = append(rendered, text)
			}
			lines = append(lines, "  arguments: "+strings.Join(rendered, ", "))
		}
	}
	return strings.Join(lines, "\n")
}

// renderPromptMessages 把 prompts/get 的消息渲染为 "role: text" 行。
func renderPromptMessages(messages []PromptMessage) string {
	if len(messages) == 0 {
		return "no messages"
	}
	lines := make([]string, 0, len(messages))
	for _, message := range messages {
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		lines = append(lines, role+": "+message.Text)
	}
	return strings.Join(lines, "\n")
}

// collapseMCPWhitespace 把连续空白折叠为一个空格。
func collapseMCPWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
