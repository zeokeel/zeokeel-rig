package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PromptArgument 是 MCP prompt 接受的一个参数。
type PromptArgument struct {
	Name        string
	Description string
	Required    bool
}

// PromptInfo 是 prompts/list 公布的一个 MCP prompt。
type PromptInfo struct {
	Name        string
	Description string
	Arguments   []PromptArgument
}

// PromptMessage 是 prompts/get 返回的一条消息，已简化为纯文本。Text 是 prompt 内容：
// 文本内容原样、内嵌资源取其文本（或 URI）、二进制内容取占位说明。
type PromptMessage struct {
	Role string
	Text string
}

// ListPrompts 返回服务端暴露的所有 prompt，跟随 nextCursor 分页并跨页去除重复名称。
func (c *Client) ListPrompts(ctx context.Context) ([]PromptInfo, error) {
	seen := make(map[string]struct{})
	cursors := make(map[string]struct{})
	var prompts []PromptInfo
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("mcp: prompts/list exceeded %d pages", maxListPages)
		}
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		raw, err := c.request(ctx, "prompts/list", params)
		if err != nil {
			return nil, err
		}
		var result wirePromptListResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("mcp: decode prompts/list result: %w", err)
		}
		for _, prompt := range result.Prompts {
			name := strings.TrimSpace(prompt.Name)
			if name == "" {
				continue
			}
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			seen[name] = struct{}{}
			arguments := make([]PromptArgument, 0, len(prompt.Arguments))
			for _, argument := range prompt.Arguments {
				if strings.TrimSpace(argument.Name) == "" {
					continue
				}
				arguments = append(arguments, PromptArgument(argument))
			}
			prompts = append(prompts, PromptInfo{
				Name:        name,
				Description: prompt.Description,
				Arguments:   arguments,
			})
		}
		next := strings.TrimSpace(result.NextCursor)
		if next == "" || next == cursor {
			return prompts, nil
		}
		if _, repeated := cursors[next]; repeated {
			return nil, fmt.Errorf("mcp: prompts/list cursor %q repeated", next)
		}
		cursors[next] = struct{}{}
		cursor = next
	}
}

// GetPrompt 用给定参数渲染一个 prompt。服务端返回消息（角色 + 内容）；每条消息都简化为
// 纯文本，以便直接交给模型而无需转换 MCP 内容类型。
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) ([]PromptMessage, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("mcp: prompt name is required")
	}
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	raw, err := c.request(ctx, "prompts/get", params)
	if err != nil {
		return nil, err
	}
	var result wireGetPromptResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("mcp: decode prompts/get result: %w", err)
	}
	messages := make([]PromptMessage, 0, len(result.Messages))
	for _, message := range result.Messages {
		messages = append(messages, PromptMessage{
			Role: message.Role,
			Text: promptContentText(message.Content),
		})
	}
	return messages, nil
}

// promptContentText 把一个 MCP 内容对象简化为文本。
func promptContentText(content wireContent) string {
	switch content.Type {
	case "text":
		return content.Text
	case "resource":
		if content.Resource != nil {
			if content.Resource.Text != "" {
				return content.Resource.Text
			}
			if content.Resource.URI != "" {
				return content.Resource.URI
			}
		}
		return content.Text
	case "image":
		return placeholder("image", content.MimeType)
	case "audio":
		return placeholder("audio", content.MimeType)
	case "":
		if content.Text != "" {
			return content.Text
		}
		return "[" + "unknown content" + "]"
	default:
		return placeholder(content.Type, content.MimeType)
	}
}
