package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 资源/prompt 工具的基础名。模型可见名 = <prefix><server>__<base>（server 的清洗规则
// 与 Tools 完全一致），例如 `mcp__srv_a__list_resources`。
//
// 旧名（`mcp_list_resources` 等）**不保留**：它把 server 维度整个丢掉了，配两个 MCP
// server 时五个名字逐字重复（宿主只能靠上层改名兜底），同一个 server 配两次时也看不出
// 属于哪个连接。这是有意的破坏性改名，规则与 Tools 统一后，工具面才自洽。
const (
	// ToolListResources 是"列出资源"工具的基础名。
	ToolListResources = "list_resources"
	// ToolReadResource 是"读一个资源"工具的基础名。
	ToolReadResource = "read_resource"
	// ToolListResourceTemplates 是"列出资源模板"工具的基础名。
	ToolListResourceTemplates = "list_resource_templates"
	// ToolListPrompts 是"列出提示词模板"工具的基础名。
	ToolListPrompts = "list_prompts"
	// ToolGetPrompt 是"渲染一个提示词模板"工具的基础名。
	ToolGetPrompt = "get_prompt"
)

// resourceToolBases 是五个资源/提示工具的基础名，顺序即 ResourceTools 的返回顺序。
var resourceToolBases = []string{
	ToolListResources,
	ToolReadResource,
	ToolListResourceTemplates,
	ToolListPrompts,
	ToolGetPrompt,
}

// ResourceToolName 返回某个连接上某个资源/提示工具的模型可见名：
//
//	<prefix><sanitize(server)>__<base>     正常情况
//	<prefix><base>                         没有连接身份（client 为 nil / 未知 server）
//
// server 清洗规则与 Tools 一致，因此 MCP 工具名整体只有一条规则。
func ResourceToolName(client *Client, prefix, base string) string {
	server := ""
	if client != nil {
		server = strings.TrimSpace(client.ServerName())
	}
	if server == "" {
		return prefix + base
	}
	return prefix + sanitizeName(server) + "__" + base
}

// ResourceToolNames 返回 ResourceTools 为该连接生成的全部名字（顺序与 ResourceTools
// 一致）。宿主用它做接线/文档/过滤，不必自己拼字符串。
func ResourceToolNames(client *Client, prefix string) []string {
	names := make([]string, 0, len(resourceToolBases))
	for _, base := range resourceToolBases {
		names = append(names, ResourceToolName(client, prefix, base))
	}
	return names
}

// ResourceTools 把一个 MCP client 的 resources/ 与 prompts/ 子集适配为五个 rig.Tool：
//
//	mcp__<server>__list_resources           （无参数）      -> 每行一个资源
//	mcp__<server>__read_resource            （uri）         -> 资源文本，二进制只做摘要
//	mcp__<server>__list_resource_templates  （无参数）      -> 每个 URI 模板一项
//	mcp__<server>__list_prompts             （无参数）      -> 每个 prompt 一项
//	mcp__<server>__get_prompt               （name, arguments?） -> prompt 消息渲染为文本
//
// 名字里的 server 来自 client.ServerName()（与 Tools 同一套清洗规则），因此多个 client
// 的资源工具天然不重名；server 未知时退化为 <prefix><base>。prefix 原样加在最前面。
//
// 这些工具每次调用时才懒加载列表（ResourceTools 不产生 I/O），因此 client 可以在服务端
// 就绪前创建；nil client 会在调用时报告配置错误。
func ResourceTools(client *Client, prefix string) []rig.Tool {
	tools := make([]rig.Tool, 0, len(resourceToolBases))
	for _, base := range resourceToolBases {
		tools = append(tools, newResourceTool(client, ResourceToolName(client, prefix, base), base))
	}
	return tools
}

// newResourceTool 构造五个资源/提示工具之一。name 是最终模型可见名（多连接聚合时可能
// 带 `__2` 这类消歧后缀），base 决定它是哪一个。
func newResourceTool(client *Client, name, base string) rig.Tool {
	switch base {
	case ToolReadResource:
		return &mcpResourceTool{
			name:        name,
			description: "Read one MCP resource by URI. Text contents are returned as-is; binary (blob) contents are summarized.",
			schema: toolutil.ObjectSchema(map[string]any{
				"uri": map[string]any{"type": "string", "description": "URI of the resource to read, as listed by the list-resources tool."},
			}, "uri"),
			invoke: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
				if client == nil {
					return rig.ToolResult{}, fmt.Errorf("mcp: %s has no client", name)
				}
				var args struct {
					URI string `json:"uri"`
				}
				if err := toolutil.DecodeArgsAs("mcp", call, &args); err != nil {
					return rig.ToolResult{}, err
				}
				uri := strings.TrimSpace(args.URI)
				if uri == "" {
					return rig.ToolResult{Content: name + ": uri is required", IsError: true}, nil
				}
				contents, err := client.ReadResource(ctx, uri)
				if err != nil {
					return rig.ToolResult{}, err
				}
				return rig.ToolResult{Content: renderResourceContents(contents)}, nil
			},
		}
	case ToolListResourceTemplates:
		return &mcpResourceTool{
			name: name,
			description: "List the MCP resource templates the server exposes. Each URI template (RFC 6570) has a name, " +
				"optional description and media type; fill the template variables and use the read-resource tool with " +
				"the resulting URI to read one resource.",
			schema: toolutil.ObjectSchema(map[string]any{}),
			invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
				if client == nil {
					return rig.ToolResult{}, fmt.Errorf("mcp: %s has no client", name)
				}
				templates, err := client.ListResourceTemplates(ctx)
				if err != nil {
					return rig.ToolResult{}, err
				}
				return rig.ToolResult{Content: renderResourceTemplates(templates)}, nil
			},
		}
	case ToolListPrompts:
		return &mcpResourceTool{
			name:        name,
			description: "List the MCP prompt templates the server exposes, with their arguments.",
			schema:      toolutil.ObjectSchema(map[string]any{}),
			invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
				if client == nil {
					return rig.ToolResult{}, fmt.Errorf("mcp: %s has no client", name)
				}
				prompts, err := client.ListPrompts(ctx)
				if err != nil {
					return rig.ToolResult{}, err
				}
				return rig.ToolResult{Content: renderPrompts(prompts)}, nil
			},
		}
	case ToolGetPrompt:
		return &mcpResourceTool{
			name:        name,
			description: "Render one MCP prompt with arguments and return its messages as plain text (one role: text per line).",
			schema: toolutil.ObjectSchema(map[string]any{
				"name": map[string]any{"type": "string", "description": "Prompt name, as listed by the list-prompts tool."},
				"arguments": map[string]any{
					"type":                 "object",
					"description":          "Prompt arguments as name/value string pairs.",
					"additionalProperties": map[string]any{"type": "string"},
				},
			}, "name"),
			invoke: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
				if client == nil {
					return rig.ToolResult{}, fmt.Errorf("mcp: %s has no client", name)
				}
				var args struct {
					Name      string            `json:"name"`
					Arguments map[string]string `json:"arguments"`
				}
				if err := toolutil.DecodeArgsAs("mcp", call, &args); err != nil {
					return rig.ToolResult{}, err
				}
				promptName := strings.TrimSpace(args.Name)
				if promptName == "" {
					return rig.ToolResult{Content: name + ": name is required", IsError: true}, nil
				}
				messages, err := client.GetPrompt(ctx, promptName, args.Arguments)
				if err != nil {
					return rig.ToolResult{}, err
				}
				return rig.ToolResult{Content: renderPromptMessages(messages)}, nil
			},
		}
	default:
		return &mcpResourceTool{
			name:        name,
			description: "List the MCP resources (documents, files, records, ...) the server exposes.",
			schema:      toolutil.ObjectSchema(map[string]any{}),
			invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
				if client == nil {
					return rig.ToolResult{}, fmt.Errorf("mcp: %s has no client", name)
				}
				resources, err := client.ListResources(ctx)
				if err != nil {
					return rig.ToolResult{}, err
				}
				return rig.ToolResult{Content: renderResources(resources)}, nil
			},
		}
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
