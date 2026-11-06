package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PromptArgument is one argument accepted by an MCP prompt.
type PromptArgument struct {
	Name        string
	Description string
	Required    bool
}

// PromptInfo is one MCP prompt advertised by prompts/list.
type PromptInfo struct {
	Name        string
	Description string
	Arguments   []PromptArgument
}

// PromptMessage is one message returned by prompts/get, reduced to plain text.
// Text is the prompt content: text content as-is, embedded resources as their
// text (or URI), and binary content as a placeholder.
type PromptMessage struct {
	Role string
	Text string
}

// ListPrompts returns every prompt the server exposes, following nextCursor
// pagination and suppressing duplicate names across pages.
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
				arguments = append(arguments, PromptArgument{
					Name:        argument.Name,
					Description: argument.Description,
					Required:    argument.Required,
				})
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

// GetPrompt renders one prompt with its arguments. The server returns messages
// (roles + content); each message is reduced to plain text so it can be fed to
// the model without translating MCP content types.
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

// promptContentText reduces one MCP content object to text.
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
