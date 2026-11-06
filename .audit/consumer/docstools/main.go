// Command docstools is docs/TOOLS.md "Building a registry" copied by a
// consumer that follows the docs.
package main

import (
	"github.com/zeokeel/zeokeel-rig/agent"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/hosttools"
)

func main() {
	root := "."
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("ls", "git", "go"))
	agentRunner, _ := agent.NewRunner(v1spec(), agent.DefaultRunnerOptions())
	devices := full.All(full.AllOptions{
		Policy: policy,
		Root:   root,
		Host:   hosttools.Options{Interactive: true}, // or drive a real UI
		Runner: agentRunner,                          // agent.NewRunner(...)
		Models: []string{"gpt-4o-mini"},
	})
	reg := tools.NewRegistry(devices...)
	_ = reg
}
