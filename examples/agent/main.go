// Command agent is the smallest useful agent built from the Rig toolset:
// an OpenAI-compatible provider, the built-in file tools, an approval gate,
// in-memory history, and the streaming ReAct loop.
//
// It needs credentials to talk to a model:
//
//	OPENAI_API_KEY=... go run ./examples/agent "list /tmp and summarize it"
//
// The provider defaults to https://api.openai.com/v1; set OPENAI_BASE_URL for a
// compatible gateway and RIG_MODEL for the model name.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/agent"
	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/provider/openai"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/tool"
	"github.com/zeokeel/zeokeel-rig/tool/builtin"
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
	reg := tool.NewRegistry(builtin.NewBuiltins(workdir, 30*time.Second)...)
	a, err := agent.New(v1.AgentSpec{
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
	_, err = a.Run(ctx, v1.RunRequest{
		SessionID: "demo",
		Input:     []v1.Message{v1.NewMessage(v1.RoleUser, prompt)},
		Emit: func(ev v1.Event) {
			switch ev.Type {
			case v1.EventTextDelta:
				fmt.Print(ev.Text)
			case v1.EventToolCall:
				fmt.Printf("\n[tool call] %s %s\n", ev.Call.Name, ev.Call.Arguments)
			case v1.EventToolResult:
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
