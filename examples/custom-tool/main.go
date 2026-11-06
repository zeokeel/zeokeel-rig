// Command custom-tool shows the smallest third-party tool: implement
// contracts.Tool, register it, invoke it. It runs offline and deterministically.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tool"
)

// wordCount is a user-defined tool: it counts whitespace-separated words.
type wordCount struct{}

func (wordCount) Name() string        { return "word_count" }
func (wordCount) Description() string { return "Count the words in a text." }
func (wordCount) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "text to count"},
		},
		"required":             []any{"text"},
		"additionalProperties": false,
	}
}

func (wordCount) Invoke(_ context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return v1.ToolResult{Content: "invalid arguments: " + err.Error(), IsError: true}, nil
	}
	return v1.ToolResult{Content: fmt.Sprintf("%d words", len(splitWords(args.Text)))}, nil
}

func splitWords(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func main() {
	reg := tool.NewRegistry(wordCount{})
	specs := reg.Specs()
	fmt.Printf("registered %d tool(s): %s\n", len(specs), specs[0].Name)

	result, err := reg.Invoke(context.Background(), v1.ToolCall{
		ID: "call-1", Name: "word_count", Arguments: `{"text":"hello rig sdk"}`,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "invoke:", err)
		os.Exit(1)
	}
	fmt.Println("result:", result.Content)
}
