// Package appinfo is the Rig SDK's build-identity module.
//
// It is a normal compile-time module: the assembly configures it (name,
// version, commit, build time), and on activation it provides an appinfo.Info
// through the kernel service Key. Any other module can require that key to
// report the product identity without knowing how the host obtained it.
package appinfo

import (
	goruntime "runtime"

	"github.com/zeokeel/zeokeel-rig/kernelx"
)

// ModuleID is the stable module identity used by assembly documents.
const ModuleID = "rig/appinfo"

// Info is the build identity this module provides to the rest of the process.
type Info struct {
	Name      string
	Version   string
	Commit    string
	BuildTime string
	GoVersion string
}

// Key is the kernel service key appinfo.Info is provided under. Modules
// require appinfo.Key; they never import this package's implementation.
var Key = kernelx.NewServiceKey[Info]("rig/appinfo")

// Config is the module's typed assembly config.
type Config struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
}

// Module returns the appinfo descriptor. The host overlays the running binary's
// real build identity onto this module's entry config at load time, so a
// product's assembly.json only carries the product name.
func Module() *kernelx.Descriptor[Config] {
	return kernelx.Define(ModuleID, func(ctx kernelx.Context, cfg Config) error {
		info := Info{
			Name:      firstNonEmpty(cfg.Name, "ZeoKeel-Rig"),
			Version:   firstNonEmpty(cfg.Version, "dev"),
			Commit:    firstNonEmpty(cfg.Commit, "unknown"),
			BuildTime: firstNonEmpty(cfg.BuildTime, "unknown"),
			GoVersion: goruntime.Version(),
		}
		_, err := ctx.Provide(Key, info)
		return err
	}).
		AddProvides(kernelx.Provides(Key)).
		WithName("Rig app info")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
