// Package assembly is ZeoKeel-Rig's assembly root: it owns the compile-time
// module registry, the embedded product assemblies and the pre-flight/loading
// entry points a host calls.
//
// A product is one JSON entry tree (assemblies/<product>.json). Loading is
// two-phase, exactly as the kernel defines it:
//
//  1. pre-flight validation of the whole tree (unknown modules, duplicate
//     instance IDs, missing/duplicate/type-mismatched providers, dependency
//     cycles, rejected config) before any fiber exists; and
//  2. starting the validated tree, which settles every required module.
//
// The registry is compile-time: every module is linked into the binary, and an
// assembly only selects and configures the linked modules. Adding a module
// means adding a dependency, a registration and a recompile — never a runtime
// code load.
package assembly

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/zeokeel/zeokeel-rig/kernelx"
)

// Product names embedded in the SDK.
const (
	// ProductHeadless is the default headless product: appinfo plus the no-op
	// group container that future modules nest under.
	ProductHeadless = "headless"
	// ProductMinimal is the smallest usable product: appinfo only.
	ProductMinimal = "minimal"
)

//go:embed assemblies/*.json
var assemblyFS embed.FS

const (
	assemblyDir = "assemblies"
	assemblyExt = ".json"
)

// Products lists the embedded product names in ascending order.
func Products() []string {
	entries, err := fs.ReadDir(assemblyFS, assemblyDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), assemblyExt) {
			continue
		}
		names = append(names, strings.TrimSuffix(entry.Name(), assemblyExt))
	}
	sort.Strings(names)
	return names
}

// Entries parses one embedded product assembly into a module entry tree. The
// document format is the kernel's assembly config ({"version":1,"entries":[...]});
// a bare array is accepted too.
func Entries(product string) ([]kernelx.Entry, error) {
	name, err := normalizeProduct(product)
	if err != nil {
		return nil, err
	}
	data, err := assemblyFS.ReadFile(assemblyDir + "/" + name + assemblyExt)
	if err != nil {
		return nil, fmt.Errorf("rig/assembly: read embedded product %q: %w", name, err)
	}
	entries, err := ParseEntries(data)
	if err != nil {
		return nil, fmt.Errorf("rig/assembly: product %q: %w", name, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("rig/assembly: product %q has no entries", name)
	}
	return entries, nil
}

// ParseEntries parses one assembly document. Products that ship their own
// document (instead of using an embedded product) call it and pass the result
// to host.Options.Entries.
func ParseEntries(data []byte) ([]kernelx.Entry, error) {
	entries, err := kernelx.ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("rig/assembly: parse assembly: %w", err)
	}
	return entries, nil
}

// Validate runs pre-flight validation for one embedded product. It creates no
// fibers: every problem (unknown module, duplicate instance ID, missing
// provider, type mismatch, dependency cycle, rejected config) is reported
// before the process starts loading.
func Validate(product string) error {
	entries, err := Entries(product)
	if err != nil {
		return err
	}
	registry := NewRegistry()
	if _, err := registry.Validate(entries); err != nil {
		return fmt.Errorf("rig/assembly: product %q: %w", product, err)
	}
	return nil
}

// Load validates one embedded product and starts its module tree on ctx.
//
// The caller must have provided the host services the assembly's modules
// require (rig/host does this before loading). Load is synchronous: when it
// returns, every required module is settled (Active, or an error that has
// already rolled the whole tree back).
func Load(ctx kernelx.Context, product string) (*kernelx.Loader, error) {
	entries, err := Entries(product)
	if err != nil {
		return nil, err
	}
	loader, err := LoadEntries(ctx, NewRegistry(), entries)
	if err != nil {
		return nil, fmt.Errorf("rig/assembly: product %q: %w", product, err)
	}
	return loader, nil
}

// LoadEntries validates an in-memory entry tree against an explicit registry
// and starts it. It is the load path for products that extend the compile-time
// registry with their own modules and ship their own assembly document; the
// host's Options.Registry/Options.Entries use it.
func LoadEntries(ctx kernelx.Context, registry *kernelx.Registry, entries []kernelx.Entry) (*kernelx.Loader, error) {
	if registry == nil {
		return nil, errors.New("rig/assembly: nil registry")
	}
	loader, err := kernelx.Load(ctx, registry, entries)
	if err != nil {
		return nil, fmt.Errorf("rig/assembly: load assembly: %w", err)
	}
	return loader, nil
}

// normalizeProduct accepts a product name or its file name ("headless" or
// "headless.json") and verifies it is embedded.
func normalizeProduct(product string) (string, error) {
	product = strings.TrimSpace(product)
	name := strings.TrimSpace(strings.TrimSuffix(product, assemblyExt))
	known := Products()
	if name == "" {
		return "", fmt.Errorf("rig/assembly: product is required (known: %s)", strings.Join(known, ", "))
	}
	for _, candidate := range known {
		if name == candidate {
			return name, nil
		}
	}
	return "", fmt.Errorf("rig/assembly: unknown product %q (known: %s)", name, strings.Join(known, ", "))
}
