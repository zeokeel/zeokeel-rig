// Command custom-tool 展示最小的第三方工具：实现 rig.Tool、注册它、调用它。
// 它离线运行且结果确定。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools"
)

// wordCount 是用户自定义工具：统计以空白分隔的单词数。
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

func (wordCount) Invoke(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return rig.ToolResult{Content: "invalid arguments: " + err.Error(), IsError: true}, nil
	}
	return rig.ToolResult{Content: fmt.Sprintf("%d words", len(splitWords(args.Text)))}, nil
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
	reg := tools.NewRegistry(wordCount{})
	specs := reg.Specs()
	fmt.Printf("registered %d tool(s): %s\n", len(specs), specs[0].Name)

	result, err := reg.Invoke(context.Background(), rig.ToolCall{
		ID: "call-1", Name: "word_count", Arguments: `{"text":"hello rig sdk"}`,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "invoke:", err)
		os.Exit(1)
	}
	fmt.Println("result:", result.Content)
}
