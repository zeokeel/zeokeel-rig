package mcp

// Tests for the resources/ and prompts/ client subset and the four
// mcp_* resource/prompt tools. The fake server (helper_test.go) serves the
// corresponding scenarios over the same stdio JSON-RPC transport the tool tests
// use.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// invokeMCPTool runs one adapted tool; it requires no Go error.
func invokeMCPTool(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", tool.Name(), err)
	}
	return result
}

// toolMap indexes adapted tools by name.
func toolMap(tools []rig.Tool) map[string]rig.Tool {
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

func requireContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("text does not contain %q:\n%s", fragment, text)
		}
	}
}

func requireNotContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			t.Fatalf("text unexpectedly contains %q:\n%s", fragment, text)
		}
	}
}

func resourceURIs(resources []ResourceInfo) []string {
	uris := make([]string, 0, len(resources))
	for _, resource := range resources {
		uris = append(uris, resource.URI)
	}
	return uris
}

func templateURIs(templates []ResourceTemplate) []string {
	uris := make([]string, 0, len(templates))
	for _, template := range templates {
		uris = append(uris, template.URITemplate)
	}
	return uris
}

func promptNames(prompts []PromptInfo) []string {
	names := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		names = append(names, prompt.Name)
	}
	return names
}

// --- client subset ---------------------------------------------------------

func TestListResources(t *testing.T) {
	client := startClient(t, "full", nil)
	resources, err := client.ListResources(context.Background())
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources) != 3 {
		t.Fatalf("resources = %v, want 3", resourceURIs(resources))
	}
	first := resources[0]
	if first.URI != "mem://doc/plain" || first.Name != "plain" || first.MimeType != "text/plain" ||
		first.Description != "A plain text resource" {
		t.Fatalf("resource[0] = %+v, want all fields mapped", first)
	}
}

func TestListResourcesPaginationAndDeduplication(t *testing.T) {
	client := startClient(t, "resources-pagination", nil)
	resources, err := client.ListResources(context.Background())
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	want := []string{"mem://doc/a", "mem://doc/b", "mem://doc/c"}
	got := resourceURIs(resources)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("resources = %v, want %v (page 2 repeats mem://doc/b)", got, want)
	}
}

func TestListResourceTemplates(t *testing.T) {
	client := startClient(t, "resources", nil)
	templates, err := client.ListResourceTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	if len(templates) != 2 {
		t.Fatalf("templates = %v, want 2", templateURIs(templates))
	}
	first := templates[0]
	if first.URITemplate != "mem://doc/{id}" || first.Name != "document by id" ||
		first.Description != "A document selected by id" || first.MimeType != "text/plain" {
		t.Fatalf("template[0] = %+v, want all fields mapped", first)
	}
}

func TestListResourceTemplatesPaginationAndDeduplication(t *testing.T) {
	client := startClient(t, "templates-pagination", nil)
	templates, err := client.ListResourceTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	want := []string{"mem://doc/{id}", "mem://user/{id}", "mem://repo/{owner}/{name}"}
	if got := templateURIs(templates); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("templates = %v, want %v (page 2 repeats mem://doc/{id})", got, want)
	}
}

func TestListResourceTemplatesEmptyAndCapabilityError(t *testing.T) {
	client := startClient(t, "empty", nil)
	templates, err := client.ListResourceTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListResourceTemplates(empty): %v", err)
	}
	if len(templates) != 0 {
		t.Fatalf("empty templates = %v", templateURIs(templates))
	}

	// The default scenario declares tools only: resources/templates/list is
	// answered with -32601, which the client passes through unchanged.
	client = startClient(t, "default", nil)
	if _, err := client.ListResourceTemplates(context.Background()); err == nil {
		t.Fatal("ListResourceTemplates on a server without resources returned nil error")
	} else {
		requireContains(t, err.Error(), "json-rpc error", "-32601", "method not found")
	}
}

func TestReadResourceText(t *testing.T) {
	client := startClient(t, "resources", nil)
	contents, err := client.ReadResource(context.Background(), "mem://doc/plain")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("contents = %+v, want 1", contents)
	}
	if contents[0].Text != "plain resource text" || contents[0].MimeType != "text/plain" ||
		contents[0].URI != "mem://doc/plain" {
		t.Fatalf("content = %+v, want text and metadata", contents[0])
	}
}

func TestReadResourceBlobAndMultipleContents(t *testing.T) {
	client := startClient(t, "resources", nil)
	binaryContents, err := client.ReadResource(context.Background(), "mem://doc/binary")
	if err != nil {
		t.Fatalf("ReadResource(binary): %v", err)
	}
	if len(binaryContents) != 1 || binaryContents[0].Blob == "" || binaryContents[0].Text != "" {
		t.Fatalf("binary content = %+v, want a base64 blob without text", binaryContents)
	}
	if len(binaryContents[0].Blob) != 16 { // base64("binary-data") = "YmluYXJ5LWRhdGE="
		t.Fatalf("blob length = %d, want 16 base64 chars", len(binaryContents[0].Blob))
	}
	multi, err := client.ReadResource(context.Background(), "mem://doc/multi")
	if err != nil {
		t.Fatalf("ReadResource(multi): %v", err)
	}
	if len(multi) != 2 || multi[0].Text != "first content" || multi[1].Text != "second content" {
		t.Fatalf("multi contents = %+v", multi)
	}
}

func TestReadResourceEmptyAndErrors(t *testing.T) {
	client := startClient(t, "resources", nil)
	empty, err := client.ReadResource(context.Background(), "mem://doc/empty")
	if err != nil {
		t.Fatalf("ReadResource(empty): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty contents = %+v, want none", empty)
	}
	if _, err := client.ReadResource(context.Background(), ""); err == nil {
		t.Fatal("ReadResource(empty URI) returned nil error")
	}
	_, err = client.ReadResource(context.Background(), "mem://doc/error")
	rpcError, ok := errors.AsType[*RPCError](err)
	if !ok || rpcError.Code != -32000 || !strings.Contains(err.Error(), "resource exploded") {
		t.Fatalf("ReadResource(error) = %v, want the JSON-RPC error", err)
	}
	if _, err := client.ReadResource(context.Background(), "mem://nope"); err == nil ||
		!strings.Contains(err.Error(), "resource not found") {
		t.Fatalf("ReadResource(unknown) = %v, want not-found error", err)
	}
}

func TestListPrompts(t *testing.T) {
	client := startClient(t, "full", nil)
	prompts, err := client.ListPrompts(context.Background())
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 4 {
		t.Fatalf("prompts = %v, want 4", promptNames(prompts))
	}
	greet := prompts[0]
	if greet.Name != "greet" || greet.Description != "Greet someone" {
		t.Fatalf("prompt[0] = %+v", greet)
	}
	if len(greet.Arguments) != 1 || greet.Arguments[0].Name != "name" ||
		!greet.Arguments[0].Required || greet.Arguments[0].Description != "Person to greet" {
		t.Fatalf("greet arguments = %+v", greet.Arguments)
	}
}

func TestListPromptsPaginationAndDeduplication(t *testing.T) {
	client := startClient(t, "prompts-pagination", nil)
	prompts, err := client.ListPrompts(context.Background())
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	want := []string{"greet", "cleanup", "review"}
	got := promptNames(prompts)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("prompts = %v, want %v (page 2 repeats greet)", got, want)
	}
}

func TestGetPromptMessagesAndArguments(t *testing.T) {
	client := startClient(t, "full", nil)
	messages, err := client.GetPrompt(context.Background(), "greet", map[string]string{"name": "Ada"})
	if err != nil {
		t.Fatalf("GetPrompt(greet): %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %+v, want 2", messages)
	}
	if messages[0].Role != "user" || messages[0].Text != "Hello Ada" {
		t.Fatalf("message[0] = %+v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Text != "Hi! How can I help?" {
		t.Fatalf("message[1] = %+v", messages[1])
	}
	// No arguments is fine for a prompt that takes none.
	plain, err := client.GetPrompt(context.Background(), "plain", nil)
	if err != nil || len(plain) != 1 || plain[0].Text != "plain prompt" {
		t.Fatalf("GetPrompt(plain) = %+v, %v", plain, err)
	}
	// Embedded resources are reduced to their text.
	embedded, err := client.GetPrompt(context.Background(), "resource", nil)
	if err != nil || len(embedded) != 1 || embedded[0].Text != "embedded resource text" {
		t.Fatalf("GetPrompt(resource) = %+v, %v", embedded, err)
	}
}

func TestGetPromptErrors(t *testing.T) {
	client := startClient(t, "full", nil)
	if _, err := client.GetPrompt(context.Background(), "", nil); err == nil {
		t.Fatal("GetPrompt(empty name) returned nil error")
	}
	_, err := client.GetPrompt(context.Background(), "error", nil)
	rpcError, ok := errors.AsType[*RPCError](err)
	if !ok || rpcError.Code != -32000 {
		t.Fatalf("GetPrompt(error) = %v, want JSON-RPC error", err)
	}
	if _, err := client.GetPrompt(context.Background(), "missing", nil); err == nil {
		t.Fatal("GetPrompt(unknown) returned nil error")
	}
}

// --- adapted tools ---------------------------------------------------------

func TestResourceToolsNamesSchemasAndPrefix(t *testing.T) {
	client := startClient(t, "full", nil)
	tools := ResourceTools(client, "")
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	want := []string{ToolListResources, ToolReadResource, ToolListResourceTemplates, ToolListPrompts, ToolGetPrompt}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	prefixed := ResourceTools(client, "rig_")
	for index, tool := range prefixed {
		if tool.Name() != "rig_"+want[index] {
			t.Fatalf("prefixed tool[%d] = %q, want %q", index, tool.Name(), "rig_"+want[index])
		}
	}

	read := toolMap(tools)[ToolReadResource]
	required, _ := read.Schema()["required"].([]string)
	if len(required) != 1 || required[0] != "uri" {
		t.Fatalf("mcp_read_resource required = %v, want [uri]", required)
	}
	get := toolMap(tools)[ToolGetPrompt]
	required, _ = get.Schema()["required"].([]string)
	if len(required) != 1 || required[0] != "name" {
		t.Fatalf("mcp_get_prompt required = %v, want [name]", required)
	}
	properties, _ := get.Schema()["properties"].(map[string]any)
	if _, ok := properties["arguments"].(map[string]any); !ok {
		t.Fatalf("mcp_get_prompt has no arguments object: %v", properties)
	}
	list := toolMap(tools)[ToolListResources]
	if properties, _ := list.Schema()["properties"].(map[string]any); len(properties) != 0 {
		t.Fatalf("mcp_list_resources properties = %v, want none", properties)
	}
	templates := toolMap(tools)[ToolListResourceTemplates]
	if properties, _ := templates.Schema()["properties"].(map[string]any); len(properties) != 0 {
		t.Fatalf("mcp_list_resource_templates properties = %v, want none", properties)
	}
}

func TestResourceToolsRenderResults(t *testing.T) {
	client := startClient(t, "full", nil)
	byName := toolMap(ResourceTools(client, ""))
	ctx := context.Background()

	resources := invokeMCPTool(t, byName[ToolListResources], `{}`)
	requireContains(t, resources.Content, "mem://doc/plain", "(plain)", "[text/plain]", "A plain text resource", "mem://doc/binary")

	plain := invokeMCPTool(t, byName[ToolReadResource], `{"uri":"mem://doc/plain"}`)
	if plain.IsError || plain.Content != "plain resource text" {
		t.Fatalf("read plain = %q (IsError=%v)", plain.Content, plain.IsError)
	}

	binary := invokeMCPTool(t, byName[ToolReadResource], `{"uri":"mem://doc/binary"}`)
	requireContains(t, binary.Content, "binary content omitted", "application/octet-stream", "11 bytes", "base64")
	requireNotContains(t, binary.Content, "YmluYXJ5LWRhdGE=") // payload never reaches the model

	multi := invokeMCPTool(t, byName[ToolReadResource], `{"uri":"mem://doc/multi"}`)
	requireContains(t, multi.Content, "mem://doc/multi#one:", "first content", "mem://doc/multi#two:", "second content")

	templates := invokeMCPTool(t, byName[ToolListResourceTemplates], `{}`)
	requireContains(t, templates.Content, "mem://doc/{id}", "(document by id)", "[text/plain]", "A document selected by id", "mem://repo/{owner}/{name}")

	prompts := invokeMCPTool(t, byName[ToolListPrompts], `{}`)
	requireContains(t, prompts.Content, "- greet", "Greet someone", "arguments: name (required)", "- plain")

	greet := invokeMCPTool(t, byName[ToolGetPrompt], `{"name":"greet","arguments":{"name":"Ada"}}`)
	requireContains(t, greet.Content, "user: Hello Ada", "assistant: Hi! How can I help?")

	resourcePrompt := invokeMCPTool(t, byName[ToolGetPrompt], `{"name":"resource"}`)
	requireContains(t, resourcePrompt.Content, "user: embedded resource text")

	_ = ctx
}

func TestResourceToolsEmptyLists(t *testing.T) {
	client := startClient(t, "empty", nil)
	byName := toolMap(ResourceTools(client, ""))
	if got := invokeMCPTool(t, byName[ToolListResources], `{}`); got.Content != "no resources" || got.IsError {
		t.Fatalf("empty resources = %q (IsError=%v)", got.Content, got.IsError)
	}
	if got := invokeMCPTool(t, byName[ToolListPrompts], `{}`); got.Content != "no prompts" || got.IsError {
		t.Fatalf("empty prompts = %q (IsError=%v)", got.Content, got.IsError)
	}
	if got := invokeMCPTool(t, byName[ToolListResourceTemplates], `{}`); got.Content != "no resource templates" || got.IsError {
		t.Fatalf("empty resource templates = %q (IsError=%v)", got.Content, got.IsError)
	}
}

func TestResourceToolsErrorPassthroughAndValidation(t *testing.T) {
	// The default scenario declares tools only: the server answers
	// resources/list with a JSON-RPC -32601 method-not-found error, which the
	// tool must pass through instead of swallowing.
	client := startClient(t, "default", nil)
	byName := toolMap(ResourceTools(client, ""))
	_, err := byName[ToolListResources].Invoke(context.Background(), rig.ToolCall{Arguments: `{}`})
	if err == nil {
		t.Fatal("mcp_list_resources on a server without resources capability returned nil error")
	}
	requireContains(t, err.Error(), "json-rpc error", "-32601", "method not found")

	_, err = byName[ToolListResourceTemplates].Invoke(context.Background(), rig.ToolCall{Arguments: `{}`})
	if err == nil {
		t.Fatal("mcp_list_resource_templates on a server without resources capability returned nil error")
	}
	requireContains(t, err.Error(), "json-rpc error", "-32601", "method not found")

	// A nil client is a configuration error, not a model tool result.
	nilTools := ResourceTools(nil, "")
	for _, index := range []int{0, 2} { // list_resources and list_resource_templates
		if _, err := nilTools[index].Invoke(context.Background(), rig.ToolCall{Arguments: `{}`}); err == nil ||
			!strings.Contains(err.Error(), "no client") {
			t.Fatalf("nil-client invoke error = %v", err)
		}
	}

	// Missing required arguments are model-facing tool errors.
	result := invokeMCPTool(t, byName[ToolReadResource], `{}`)
	if !result.IsError || !strings.Contains(result.Content, "uri is required") {
		t.Fatalf("read without uri = %q (IsError=%v)", result.Content, result.IsError)
	}
	result = invokeMCPTool(t, byName[ToolGetPrompt], `{}`)
	if !result.IsError || !strings.Contains(result.Content, "name is required") {
		t.Fatalf("get without name = %q (IsError=%v)", result.Content, result.IsError)
	}

	// Malformed arguments JSON is a Go error.
	if _, err := byName[ToolReadResource].Invoke(context.Background(), rig.ToolCall{Arguments: "{"}); err == nil {
		t.Fatal("malformed arguments returned nil error")
	}
}
