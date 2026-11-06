package assembly

import (
	"github.com/zeokeel/zeokeel-rig/kernelx"
	"github.com/zeokeel/zeokeel-rig/modules/appinfo"
)

// NewRegistry builds the compile-time module registry every product starts
// from: the modules linked into this SDK, registered explicitly. There is no
// init-time registration and no runtime code loading; a module appears here (or
// in a product's own registration) or it does not exist for that binary.
//
// The registry is a value, not a singleton: a product or test builds one, adds
// its own modules, and loads an assembly against it. A third-party product
// extends the SDK like this:
//
//	registry := assembly.NewRegistry()
//	kernelx.MustRegister(registry, mymodule.Module())
//	// ... host.Options{Product: "my-product", Registry: registry, ...}
//
// NewRegistry itself is deliberately free of host concerns: it registers
// modules, not the services the process host provides (the kernel loader
// resolves those against the root context at load time).
func NewRegistry() *kernelx.Registry {
	registry := kernelx.NewRegistry()
	kernelx.MustRegister(registry, appinfo.Module())
	kernelx.MustRegister(registry, GroupModule())
	return registry
}
