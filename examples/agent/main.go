// Command agent 是用 Rig 工具集构建的最小可用 agent：兼容 OpenAI 的 provider、
// 内置文件工具、审批门禁、内存历史，以及流式 ReAct 循环。
//
// 与模型对话需要凭据：
//
//	OPENAI_API_KEY=... go run ./examples/agent "list /tmp and summarize it"
//
// provider 默认使用 https://api.openai.com/v1（兼容网关请设置
// openai.Config.BaseURL）；RIG_MODEL 选择模型名。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/provider/openai"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/tools"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

func main() {
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "List the files in the current directory and summarize them."
	}
	workdir, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	prov, err := openai.New(openai.Config{DefaultModel: envOr("RIG_MODEL", "gpt-4o-mini")})
	if err != nil {
		fail(err)
	}
	reg := tools.NewRegistry(builtin.NewBuiltins(workdir, 30*time.Second)...)
	a, err := agent.New(rig.AgentSpec{
		Provider:      prov,
		Model:         envOr("RIG_MODEL", "gpt-4o-mini"),
		SystemPrompt:  "You are a concise coding assistant. Prefer read-only tools.",
		Tools:         reg.Tools(),
		Gate:          gate.AllowTools("read_file", "list_dir", "glob", "grep"),
		History:       session.NewMemory(),
		MaxIterations: 20,
	})
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err = a.Run(ctx, rig.RunRequest{
		SessionID: "demo",
		Input:     []rig.Message{rig.NewMessage(rig.RoleUser, prompt)},
		Emit: func(ev rig.Event) {
			switch ev.Type {
			case rig.EventTextDelta:
				fmt.Print(ev.Text)
			case rig.EventToolCall:
				fmt.Printf("\n[tool call] %s %s\n", ev.Call.Name, ev.Call.Arguments)
			case rig.EventToolResult:
				fmt.Printf("[tool result] %s\n", ev.Result.Content)
			}
		},
	})
	fmt.Println()
	if err != nil {
		fail(err)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
