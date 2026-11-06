// Package greeter is a third-party-style Rig module: it ships its own typed
// config, requires the appinfo read model, and provides a greeting service.
// It imports only the kernel wrapper and the appinfo contract module.
package greeter

import (
	"github.com/zeokeel/zeokeel-rig/kernelx"
	"github.com/zeokeel/zeokeel-rig/modules/appinfo"
)

// ModuleID is the stable module identity used by the product's assembly.json.
const ModuleID = "example/greeter"

// Config is the module's typed assembly config.
type Config struct {
	Greeting string `json:"greeting"`
}

// Greeting is the service this module provides.
type Greeting struct {
	Text string
}

// Key is the kernel service key the greeting is provided under.
var Key = kernelx.NewServiceKey[Greeting]("example/greeter")

// Module returns the descriptor the product registers. It requires the appinfo
// contract (provided by the rig/appinfo entry in the product's assembly) and
// provides its own greeting service.
func Module() *kernelx.Descriptor[Config] {
	return kernelx.Define(ModuleID, func(ctx kernelx.Context, cfg Config) error {
		info, err := ctx.Require(appinfo.Key)
		if err != nil {
			return err
		}
		text := cfg.Greeting
		if text == "" {
			text = "hello from a third-party module"
		}
		_, err = ctx.Provide(Key, Greeting{Text: text + " (" + info.Name + " " + info.Version + ")"})
		return err
	}).
		AddProvides(kernelx.Provides(Key)).
		AddRequires(kernelx.Requires(appinfo.Key)).
		WithName("Example greeter")
}
