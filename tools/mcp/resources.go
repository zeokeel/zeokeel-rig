package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ResourceInfo 是 resources/list 公布的一个 MCP 资源。
type ResourceInfo struct {
	URI         string
	Name        string
	Description string
	MimeType    string
}

// ResourceTemplate 是 resources/templates/list 公布的一个资源模板。URITemplate 是
// RFC 6570 URI 模板；填充其变量即可得到用于 ReadResource 的资源 URI。
type ResourceTemplate struct {
	URITemplate string
	Name        string
	Description string
	MimeType    string
}

// ResourceContents 是 resources/read 返回的一项内容。通常 Text 与 Blob 恰好设置一个；
// Blob 是服务端发送的 base64。
type ResourceContents struct {
	URI      string
	MimeType string
	Text     string
	Blob     string
}

// ListResources 返回服务端暴露的所有资源，跟随 nextCursor 分页并跨页去除重复 URI。
func (c *Client) ListResources(ctx context.Context) ([]ResourceInfo, error) {
	seen := make(map[string]struct{})
	cursors := make(map[string]struct{})
	var resources []ResourceInfo
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("mcp: resources/list exceeded %d pages", maxListPages)
		}
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		raw, err := c.request(ctx, "resources/list", params)
		if err != nil {
			return nil, err
		}
		var result wireResourceListResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("mcp: decode resources/list result: %w", err)
		}
		for _, resource := range result.Resources {
			uri := strings.TrimSpace(resource.URI)
			if uri == "" {
				continue
			}
			if _, duplicate := seen[uri]; duplicate {
				continue
			}
			seen[uri] = struct{}{}
			resources = append(resources, ResourceInfo{
				URI:         uri,
				Name:        resource.Name,
				Description: resource.Description,
				MimeType:    resource.MimeType,
			})
		}
		next := strings.TrimSpace(result.NextCursor)
		if next == "" || next == cursor {
			return resources, nil
		}
		if _, repeated := cursors[next]; repeated {
			return nil, fmt.Errorf("mcp: resources/list cursor %q repeated", next)
		}
		cursors[next] = struct{}{}
		cursor = next
	}
}

// ReadResource 按 URI 读取一个资源。资源可能携带多个内容（例如类目录资源）；文本内容保留
// 文本，二进制内容保留 base64 载荷供调用方自行摘要。
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ResourceContents, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, fmt.Errorf("mcp: resource URI is required")
	}
	raw, err := c.request(ctx, "resources/read", map[string]string{"uri": uri})
	if err != nil {
		return nil, err
	}
	var result wireReadResourceResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("mcp: decode resources/read result: %w", err)
	}
	contents := make([]ResourceContents, 0, len(result.Contents))
	for _, content := range result.Contents {
		contents = append(contents, ResourceContents(content))
	}
	return contents, nil
}

// ListResourceTemplates 返回服务端暴露的所有资源模板，跟随 nextCursor 分页并跨页去除
// 重复 uriTemplate。未声明 resources 能力的服务端会以 JSON-RPC -32601 错误应答
// resources/templates/list，该错误原样返回给调用方。
func (c *Client) ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, error) {
	seen := make(map[string]struct{})
	cursors := make(map[string]struct{})
	var templates []ResourceTemplate
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("mcp: resources/templates/list exceeded %d pages", maxListPages)
		}
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		raw, err := c.request(ctx, "resources/templates/list", params)
		if err != nil {
			return nil, err
		}
		var result wireResourceTemplateListResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("mcp: decode resources/templates/list result: %w", err)
		}
		for _, template := range result.ResourceTemplates {
			uriTemplate := strings.TrimSpace(template.URITemplate)
			if uriTemplate == "" {
				continue
			}
			if _, duplicate := seen[uriTemplate]; duplicate {
				continue
			}
			seen[uriTemplate] = struct{}{}
			templates = append(templates, ResourceTemplate{
				URITemplate: uriTemplate,
				Name:        template.Name,
				Description: template.Description,
				MimeType:    template.MimeType,
			})
		}
		next := strings.TrimSpace(result.NextCursor)
		if next == "" || next == cursor {
			return templates, nil
		}
		if _, repeated := cursors[next]; repeated {
			return nil, fmt.Errorf("mcp: resources/templates/list cursor %q repeated", next)
		}
		cursors[next] = struct{}{}
		cursor = next
	}
}
