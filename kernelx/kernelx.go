// Package kernelx is the only place in zeokeel-rig that imports the ZeoKeel
// kernel. It re-exports the small surface the SDK and its modules use, so a
// kernel API change is absorbed here instead of rippling through the SDK.
//
// The kernel is a plain MIT Go module (github.com/zeokeel/zeokeel-kernel) with
// no third-party dependencies; Rig pins it in go.mod and documents the tested
// version in COMPATIBILITY (README).
package kernelx

import (
	cordis "github.com/zeokeel/zeokeel-kernel"
	"github.com/zeokeel/zeokeel-kernel/module"
)

// KernelVersion is the kernel release this SDK is tested against.
const KernelVersion = "v0.1.1"

// Type aliases: public so module authors can implement the kernel contracts
// without importing the kernel package directly (they still may).
type (
	Context       = cordis.Context
	Effect        = cordis.Effect
	Cleanup       = cordis.Cleanup
	FiberState    = cordis.FiberState
	Plugin        = cordis.Plugin
	Entry         = module.Entry
	Registry      = module.Registry
	Loader        = module.Loader
	ValidationErr = module.ValidationError
)

// Generic aliases (Go 1.27): module authors use kernelx.Descriptor[T] and
// kernelx.ServiceKey[T] and never import the kernel directly.
type (
	Descriptor[T any] = module.Descriptor[T]
	ServiceKey[T any] = cordis.ServiceKey[T]
)

// Register adds a module descriptor to a registry (panic-free: returns error).
func Register[T any](reg *module.Registry, desc *module.Descriptor[T]) error {
	return reg.Register(desc)
}

// MustRegister is Register for compile-time-static module lists: a duplicate or
// invalid descriptor is a programming error and fails loudly at startup.
func MustRegister[T any](reg *module.Registry, desc *module.Descriptor[T]) {
	if err := reg.Register(desc); err != nil {
		panic("rig: " + err.Error())
	}
}

// ServiceKey is generic: Go aliases cannot be generic, so this function form is
// the public constructor instead of a type alias.
func NewServiceKey[T any](name string) cordis.ServiceKey[T] {
	return cordis.NewServiceKey[T](name)
}

// Root creates the kernel root context.
func Root() cordis.Context { return cordis.Root() }

// Define declares one module with its typed config and factory.
func Define[T any](id string, factory func(cordis.Context, T) error) *module.Descriptor[T] {
	return module.Define(id, factory)
}

// Provides/Requires describe a module's service contracts for preflight.
func Provides[T any](key cordis.ServiceKey[T]) module.Provider { return module.Provides(key) }

func Requires[T any](key cordis.ServiceKey[T]) module.Requirement { return module.Requires(key) }

func RequiresOptional[T any](key cordis.ServiceKey[T]) module.Requirement {
	return module.RequiresOptional(key)
}

// EffectCleanup registers a cleanup owned by the calling fiber; it is the only
// supported way for a module to release registrations (kernel unwinds them in
// reverse order and rolls back on activation failure).
func EffectCleanup(ctx cordis.Context, fn func()) error {
	_, err := ctx.Effect(func() cordis.Effect { return cordis.Effect{cordis.CleanupOf(fn)} })
	return err
}

// Fiber lifecycle states, re-exported so tests and hosts can reason about
// module instances without importing the kernel directly.
const (
	Pending   = cordis.Pending
	Loading   = cordis.Loading
	Active    = cordis.Active
	Failed    = cordis.Failed
	Unloading = cordis.Unloading
	Disposed  = cordis.Disposed
)

// LoadError wraps the issues of a failed assembly start.
type LoadError = module.LoadError

// NewRegistry builds an explicit compile-time registry.
func NewRegistry() *module.Registry { return module.NewRegistry() }

// Load validates and starts an assembly.
func Load(ctx cordis.Context, reg *module.Registry, entries []module.Entry) (*module.Loader, error) {
	return module.Load(ctx, reg, entries)
}

// ParseConfig parses one assembly JSON document into entries.
func ParseConfig(data []byte) ([]module.Entry, error) { return module.ParseConfig(data) }
