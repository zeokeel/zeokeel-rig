// Command custom-module is a third-party Rig product. It demonstrates the
// extension path of the compile-time module system:
//
//  1. depend on the SDK and add your module to the registry;
//  2. ship your own assembly.json (the same strict JSON format the SDK uses);
//  3. hand both to the headless host with host.Run.
//
// It builds and starts without a model provider: the demo module only needs the
// appinfo contract.
package main

import (
	_ "embed"
	"fmt"
	"os"

	"github.com/zeokeel/zeokeel-rig/assembly"
	"github.com/zeokeel/zeokeel-rig/examples/custom-module/greeter"
	"github.com/zeokeel/zeokeel-rig/host"
	"github.com/zeokeel/zeokeel-rig/kernelx"
)

// assemblyDocument is the product's own entry tree, embedded at build time.
//
//go:embed assembly.json
var assemblyDocument []byte

func main() {
	// The built-in registry plus this product's module: compile-time modules,
	// no runtime plugin loading.
	registry := assembly.NewRegistry()
	kernelx.MustRegister(registry, greeter.Module())

	entries, err := assembly.ParseEntries(assemblyDocument)
	if err != nil {
		fmt.Fprintln(os.Stderr, "custom-module:", err)
		os.Exit(1)
	}

	opts := host.Options{
		Product:  "custom",
		Registry: registry,
		Entries:  entries,
	}
	if err := host.Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "custom-module:", err)
		os.Exit(1)
	}
}
