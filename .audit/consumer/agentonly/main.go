// Command agentonly imports only the agent loop and contracts: the minimal
// dependency footprint a consumer that brings its own tools/provider can have.
package main

import (
	"fmt"

	"github.com/zeokeel/zeokeel-rig/agent"
)

func main() { fmt.Println(agent.DefaultMaxIterations) }
