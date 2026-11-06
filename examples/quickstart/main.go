// Command quickstart is the smallest complete agent built from the Rig SDK:
// an OpenAI-compatible provider, the sandboxed file tools, an approval gate,
// in-memory history and the streaming ReAct loop. It is the code shown in the
// README quickstart and the CI build compiles it on every push.
//
// It needs credentials to talk to a model:
//
//	OPENAI_API_KEY=... go run ./examples/quickstart "list the repo and summarize it"
//
// The provider defaults to https://api.openai.com/v1. To use an OpenAI-
// compatible gateway, set Config.BaseURL (the provider does not read an
// OPENAI_BASE_URL environment variable); set Config.DefaultModel for the model.
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
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/tools"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

func main() {
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "List the files in this directory and summarize them."
	}
	workdir, err := os.Getwd()
	if err != nil {
		fail(err)
	}

	// 1. Sandbox policy: file access rooted at the working directory, a small
	// command allowlist for bash, no network.
	policy := sandbox.New(
		sandbox.WithRoots(workdir),
		sandbox.WithCommandAllowlist("git", "go", "rg", "sed"),
		sandbox.WithNetwork(false),
	)

	// 2. Tool registry: the built-in file/shell tools enforce the policy.
	reg := tools.NewRegistry(builtin.NewWithPolicy(policy)...)

	// 3. Model provider: any OpenAI-compatible endpoint. The provider reads
	// OPENAI_API_KEY from the environment when Config.APIKey is empty.
	prov, err := openai.New(openai.Config{
		BaseURL:      "https://api.openai.com/v1", // override for a gateway
		DefaultModel: "gpt-4o-mini",
	})
	if err != nil {
		fail(err)
	}

	// 4. Agent: provider + tools + approval gate + session history.
	a, err := agent.New(v1.AgentSpec{
		Provider:      prov,
		Model:         "gpt-4o-mini",
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

	// 5. Run one turn; events stream synchronously to Emit.
	_, err = a.Run(ctx, v1.RunRequest{
		SessionID: "quickstart",
		Input:     []v1.Message{v1.NewMessage(v1.RoleUser, prompt)},
		Emit: func(ev v1.Event) {
			if ev.Type == v1.EventTextDelta {
				fmt.Print(ev.Text)
			}
		},
	})
	if err != nil {
		fail(err)
	}
	fmt.Println()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "quickstart:", err)
	os.Exit(1)
}
