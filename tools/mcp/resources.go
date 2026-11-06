package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ResourceInfo is one MCP resource advertised by resources/list.
type ResourceInfo struct {
	URI         string
	Name        string
	Description string
	MimeType    string
}

// ResourceTemplate is one MCP resource template advertised by
// resources/templates/list. URITemplate is an RFC 6570 URI template; filling
// its variables yields a resource URI for ReadResource.
type ResourceTemplate struct {
	URITemplate string
	Name        string
	Description string
	MimeType    string
}

// ResourceContents is one item returned by resources/read. Exactly one of Text
// and Blob is normally set; Blob is base64 as the server sent it.
type ResourceContents struct {
	URI      string
	MimeType string
	Text     string
	Blob     string
}

// ListResources returns every resource the server exposes, following
// nextCursor pagination and suppressing duplicate URIs across pages.
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

// ReadResource reads one resource by URI. A resource may carry several contents
// (for example a directory-like resource); text contents keep their text and
// binary contents keep their base64 payload for the caller to summarize.
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
		contents = append(contents, ResourceContents{
			URI:      content.URI,
			MimeType: content.MimeType,
			Text:     content.Text,
			Blob:     content.Blob,
		})
	}
	return contents, nil
}

// ListResourceTemplates returns every resource template the server exposes,
// following nextCursor pagination and suppressing duplicate uriTemplate values
// across pages. A server that does not declare the resources capability answers
// resources/templates/list with a JSON-RPC -32601 error, which is returned to
// the caller unchanged.
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
