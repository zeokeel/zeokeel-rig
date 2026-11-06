package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// Model-facing names of the resource/prompt tools. A non-empty prefix is put in
// front of these names by ResourceTools.
const (
	ToolListResources         = "mcp_list_resources"
	ToolReadResource          = "mcp_read_resource"
	ToolListResourceTemplates = "mcp_list_resource_templates"
	ToolListPrompts           = "mcp_list_prompts"
	ToolGetPrompt             = "mcp_get_prompt"
)

// ResourceTools adapts one MCP client's resources/ and prompts/ subsets to five
// contracts/rig.Tool values:
//
//	mcp_list_resources           (no arguments)      -> one line per resource
//	mcp_read_resource            (uri)               -> resource text, binary summarized
//	mcp_list_resource_templates  (no arguments)      -> one entry per URI template
//	mcp_list_prompts             (no arguments)      -> one entry per prompt
//	mcp_get_prompt               (name, arguments?)  -> prompt messages as text
//
// prefix is prepended verbatim to each name; with an empty prefix the names are
// exactly the constants above. The tools list lazily on every call (no I/O
// happens in ResourceTools), so a client can be created before the server is
// ready; a nil client yields tools that report a configuration error on invoke.
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

// mcpResourceTool is one resource/prompt tool adapted to contracts/rig.Tool.
type mcpResourceTool struct {
	name        string
	description string
	schema      map[string]any
	invoke      func(context.Context, rig.ToolCall) (rig.ToolResult, error)
}

var _ rig.Tool = (*mcpResourceTool)(nil)

// Name implements rig.Tool.
func (t *mcpResourceTool) Name() string { return t.name }

// Description implements rig.Tool.
func (t *mcpResourceTool) Description() string { return t.description }

// Schema implements rig.Tool.
func (t *mcpResourceTool) Schema() map[string]any { return t.schema }

// Invoke implements rig.Tool.
func (t *mcpResourceTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.invoke(ctx, call)
}

// renderResourceTemplates renders resources/templates/list as one line per
// template: the URI template, optional name/media type and description.
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

// renderResources renders resources/list as one line per resource.
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

// renderResourceContents renders resources/read. A single content item is
// returned as its text body; several items are labeled with their URI so the
// model can tell them apart.
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

// renderResourceContent renders one content item. Blob payloads are never sent
// to the model: only their media type and decoded size are reported.
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

// decodedBlobSize is the byte size the base64 blob represents; an invalid
// payload falls back to the length's upper bound.
func decodedBlobSize(blob string) int {
	if decoded, err := base64.StdEncoding.DecodeString(blob); err == nil {
		return len(decoded)
	}
	return len(blob) * 3 / 4
}

// renderPrompts renders prompts/list as one block per prompt.
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

// renderPromptMessages renders prompts/get messages as "role: text" lines.
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

// collapseMCPWhitespace folds whitespace runs into one space.
func collapseMCPWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
