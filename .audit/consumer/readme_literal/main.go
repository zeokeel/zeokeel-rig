// Command readme_literal is the README quickstart copied verbatim by a new
// consumer: the snippet body only, with imports added according to the package
// names the README itself mentions ("agent", "tool", "provider/openai",
// "gate", "session", "contracts/v1") and the obvious context/fmt imports.
package main

import (
	"context"
	"fmt"

	"github.com/zeokeel/zeokeel-rig/agent"
	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/provider/openai"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/tools"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
	"github.com/zeokeel/zeokeel-rig/tools/web"
)

func main() {
	ctx := context.Background()

	policy := sandbox.New(
		sandbox.WithRoots("/workspace"),
		sandbox.WithCommandAllowlist("sed", "rg", "go", "git"),
		sandbox.WithNetwork(false),
	)
	reg := tools.NewRegistry(builtin.NewWithPolicy(policy)...)
	reg.Register(web.NewFetcher(policy)) // web_fetch (allow network in the policy first)
	prov, _ := openai.New(openai.Config{DefaultModel: "gpt-4o-mini"})
	a, _ := agent.New(v1.AgentSpec{
		Provider: prov,
		Model:    "gpt-4o-mini",
		Tools:    reg.Tools(),
		Gate:     gate.AllowTools("read_file", "list_dir", "glob", "grep", "web_fetch"),
		History:  session.NewMemory(),
	})
	res, err := a.Run(ctx, v1.RunRequest{
		SessionID: "s1",
		Input:     []v1.Message{v1.NewMessage(v1.RoleUser, "list the repo and summarize it")},
		Emit:      func(ev v1.Event) { fmt.Print(ev.Text) },
	})
	fmt.Println(res, err)
}
