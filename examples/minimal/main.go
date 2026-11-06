// Command minimal is the smallest headless Rig product: it runs the SDK's
// built-in headless assembly through the SDK host.
//
// Build it with `go build ./examples/minimal` and run it with --help; it needs
// no display environment and no model provider.
package main

import (
	"os"

	"github.com/zeokeel/zeokeel-rig/host"
)

func main() {
	os.Exit(host.Main(os.Args[1:]))
}
