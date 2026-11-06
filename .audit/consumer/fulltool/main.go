// Command fulltool shows the batteries-included path: tools/full.All gives
// every static tool in one call, but the consumer still wires provider, agent,
// gate and history itself.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/zeokeel/zeokeel-rig/agent"
	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/provider/openai"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

func main() {
	prov, err := openai.New(openai.Config{DefaultModel: "gpt-4o-mini"})
	if err != nil {
		fail(err)
	}
	all := full.All(full.AllOptions{Root: "."})
	fmt.Fprintf(os.Stderr, "full.All provided %d tools\n", len(all))
	a, err := agent.New(v1.AgentSpec{
		Provider: prov,
		Model:    "gpt-4o-mini",
		Tools:    all,
		Gate:     gate.AllowTools("read_file", "list_dir", "glob", "grep", "web_fetch"),
		History:  session.NewMemory(),
	})
	if err != nil {
		fail(err)
	}
	_, _ = a.Run(context.Background(), v1.RunRequest{})
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
