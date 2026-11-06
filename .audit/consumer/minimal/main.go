// Command minimal is the smallest real (model-backed) agent a consumer would
// write: OpenAI-compatible provider, built-in tools under a sandbox policy, an
// approval gate, in-memory history, and the agent loop. Builds offline; running
// it needs model credentials.
package main

import (
	"context"
	"fmt"
	"os"
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
	policy := sandbox.New(sandbox.WithRoots("."), sandbox.WithNetwork(false))
	reg := tools.NewRegistry(builtin.NewWithPolicy(policy)...)

	prov, err := openai.New(openai.Config{DefaultModel: "gpt-4o-mini"})
	if err != nil {
		fail(err)
	}
	a, err := agent.New(v1.AgentSpec{
		Provider: prov,
		Model:    "gpt-4o-mini",
		Tools:    reg.Tools(),
		Gate:     gate.AllowTools("read_file", "list_dir", "glob", "grep"),
		History:  session.NewMemory(),
	})
	if err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := a.Run(ctx, v1.RunRequest{
		SessionID: "s1",
		Input:     []v1.Message{v1.NewMessage(v1.RoleUser, "list this directory")},
	})
	if err != nil {
		fail(err)
	}
	fmt.Println(res.FinishReason)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
