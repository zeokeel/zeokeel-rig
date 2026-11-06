package assembly_test

// Assembly-root tests. Everything here is headless: the loaded products are
// plain kernel module trees, with no process host needed.

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/assembly"
	"github.com/zeokeel/zeokeel-rig/kernelx"
	"github.com/zeokeel/zeokeel-rig/modules/appinfo"
)

const activeState = "active"

func parseEntries(t *testing.T, document string) []kernelx.Entry {
	t.Helper()
	entries, err := assembly.ParseEntries([]byte(document))
	if err != nil {
		t.Fatalf("ParseEntries: %v", err)
	}
	return entries
}

func newRoot(t *testing.T) kernelx.Context {
	t.Helper()
	root := kernelx.Root()
	t.Cleanup(func() {
		if err := root.DisposeAll(); err != nil {
			t.Errorf("DisposeAll: %v", err)
		}
	})
	return root
}

func instanceIDs(loader *kernelx.Loader) []string {
	instances := loader.Instances()
	ids := make([]string, len(instances))
	for index, instance := range instances {
		ids[index] = instance.ID()
	}
	return ids
}

func assertValidationError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("validation succeeded, want a %q error", want)
	}
	var validation *kernelx.ValidationErr
	if !errors.As(err, &validation) {
		t.Fatalf("error type = %T, want a kernel *ValidationError: %v", err, err)
	}
	t.Logf("pre-flight error: %v", err)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to mention %q", err, want)
	}
}

func TestProductsAndEntries(t *testing.T) {
	want := []string{assembly.ProductHeadless, assembly.ProductMinimal}
	if got := assembly.Products(); !slices.Equal(got, want) {
		t.Fatalf("Products() = %v, want %v", got, want)
	}

	headless, err := assembly.Entries(assembly.ProductHeadless)
	if err != nil {
		t.Fatalf("Entries(headless): %v", err)
	}
	if len(headless) != 2 || headless[0].Module != appinfo.ModuleID || headless[1].Module != assembly.GroupModuleID {
		t.Fatalf("headless entries = %+v, want appinfo + group", headless)
	}

	minimal, err := assembly.Entries(assembly.ProductMinimal + ".json")
	if err != nil {
		t.Fatalf("Entries(minimal.json): %v", err)
	}
	if len(minimal) != 1 || minimal[0].Module != appinfo.ModuleID {
		t.Fatalf("minimal entries = %+v, want appinfo only", minimal)
	}

	if _, err := assembly.Entries("no-such-product"); err == nil {
		t.Fatal("Entries(unknown product) succeeded")
	} else if !strings.Contains(err.Error(), "unknown product") {
		t.Fatalf("Entries(unknown product) = %v, want an unknown-product error", err)
	}
	if _, err := assembly.Entries(""); err == nil {
		t.Fatal("Entries(empty product) succeeded")
	}
}

func TestParseEntries(t *testing.T) {
	document := parseEntries(t, `{"version":1,"entries":[{"id":"appinfo","module":"rig/appinfo"}]}`)
	if len(document) != 1 || document[0].ID != "appinfo" {
		t.Fatalf("document entries = %+v, want one appinfo entry", document)
	}
	bare := parseEntries(t, `[{"id":"appinfo","module":"rig/appinfo"}]`)
	if len(bare) != 1 || bare[0].ID != "appinfo" {
		t.Fatalf("bare-array entries = %+v, want one appinfo entry", bare)
	}
	if _, err := assembly.ParseEntries([]byte("{")); err == nil {
		t.Fatal("ParseEntries accepted broken JSON")
	}
}

func TestValidateEmbeddedProducts(t *testing.T) {
	for _, product := range assembly.Products() {
		if err := assembly.Validate(product); err != nil {
			t.Errorf("Validate(%s): %v", product, err)
		}
	}
	if err := assembly.Validate("no-such-product"); err == nil {
		t.Fatal("Validate(unknown product) succeeded")
	}
}

func TestLoadEmbeddedProducts(t *testing.T) {
	tests := []struct {
		product string
		wantIDs []string
	}{
		{product: assembly.ProductHeadless, wantIDs: []string{"appinfo", "modules"}},
		{product: assembly.ProductMinimal, wantIDs: []string{"appinfo"}},
	}
	for _, test := range tests {
		t.Run(test.product, func(t *testing.T) {
			root := newRoot(t)
			loader, err := assembly.Load(root, test.product)
			if err != nil {
				t.Fatalf("Load(%s): %v", test.product, err)
			}

			instances := loader.Instances()
			if got := instanceIDs(loader); !slices.Equal(got, test.wantIDs) {
				t.Fatalf("instances = %v, want %v", got, test.wantIDs)
			}
			for _, instance := range instances {
				if state := instance.State().String(); state != activeState {
					t.Fatalf("instance %q state = %s, want %s", instance.ID(), state, activeState)
				}
			}
			if warnings := loader.Warnings(); len(warnings) != 0 {
				t.Fatalf("warnings = %v, want none", warnings)
			}

			// The assembly provides appinfo.Info while it is loaded.
			info, ok := root.Get(appinfo.Key)
			if !ok {
				t.Fatal("appinfo service is not available after Load")
			}
			if info.Name != "ZeoKeel-Rig" {
				t.Fatalf("appinfo name = %q, want ZeoKeel-Rig", info.Name)
			}

			// Closing the loader disposes the tree and withdraws its services.
			if err := loader.Close(); err != nil {
				t.Fatalf("loader.Close: %v", err)
			}
			if _, ok := root.Get(appinfo.Key); ok {
				t.Fatal("appinfo service survived loader.Close")
			}
		})
	}
}

// TestLoadEntriesWithCustomRegistry covers the extension path: a product adds
// its own module to the built-in registry and loads its own entry tree.
func TestLoadEntriesWithCustomRegistry(t *testing.T) {
	root := newRoot(t)
	registry := assembly.NewRegistry()
	kernelx.MustRegister(registry, needsAppInfo())
	entries := parseEntries(t, `{"version":1,"entries":[
		{"id":"appinfo","module":"rig/appinfo"},
		{"id":"needs","module":"test/needs-appinfo"}]}`)

	loader, err := assembly.LoadEntries(root, registry, entries)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	if got := instanceIDs(loader); !slices.Equal(got, []string{"appinfo", "needs"}) {
		t.Fatalf("instances = %v, want [appinfo needs]", got)
	}
	for _, instance := range loader.Instances() {
		if state := instance.State().String(); state != activeState {
			t.Fatalf("instance %q state = %s, want %s", instance.ID(), state, activeState)
		}
	}
	if err := loader.Close(); err != nil {
		t.Fatalf("loader.Close: %v", err)
	}

	if _, err := assembly.LoadEntries(root, nil, entries); err == nil {
		t.Fatal("LoadEntries(nil registry) succeeded")
	}
}

func TestLoadUnknownProduct(t *testing.T) {
	root := newRoot(t)
	if _, err := assembly.Load(root, "no-such-product"); err == nil {
		t.Fatal("Load(unknown product) succeeded")
	} else if !strings.Contains(err.Error(), "unknown product") {
		t.Fatalf("Load(unknown product) = %v, want an unknown-product error", err)
	}
}

// fixtureConfig is an empty typed config for the pre-flight fixture module.
type fixtureConfig struct{}

// needsAppInfo is a test-only module that declares a required appinfo contract
// but no entry in these assemblies provides it: the missing-provider case.
func needsAppInfo() *kernelx.Descriptor[fixtureConfig] {
	return kernelx.Define("test/needs-appinfo", func(ctx kernelx.Context, _ fixtureConfig) error {
		_, err := ctx.Require(appinfo.Key)
		return err
	}).AddRequires(kernelx.Requires(appinfo.Key))
}

func TestPreflightFailures(t *testing.T) {
	t.Run("unknown module", func(t *testing.T) {
		registry := assembly.NewRegistry()
		entries := parseEntries(t, `{"version":1,"entries":[{"id":"ghost","module":"rig/missing"}]}`)
		_, err := registry.Validate(entries)
		assertValidationError(t, err, "unknown module")
	})

	t.Run("duplicate instance id", func(t *testing.T) {
		registry := assembly.NewRegistry()
		entries := parseEntries(t, `{"version":1,"entries":[
			{"id":"appinfo","module":"rig/appinfo"},
			{"id":"appinfo","module":"rig/appinfo"}]}`)
		_, err := registry.Validate(entries)
		assertValidationError(t, err, "duplicate instance ID")
	})

	t.Run("missing provider", func(t *testing.T) {
		registry := assembly.NewRegistry()
		kernelx.MustRegister(registry, needsAppInfo())
		entries := parseEntries(t, `{"version":1,"entries":[{"id":"needs","module":"test/needs-appinfo"}]}`)
		_, err := registry.Validate(entries)
		assertValidationError(t, err, "no enabled entry in the assembly provides")
	})
}

// brokenModule fails during activation (valid pre-flight, failed startup).
type brokenConfig struct{}

func brokenModule() *kernelx.Descriptor[brokenConfig] {
	return kernelx.Define("test/broken", func(kernelx.Context, brokenConfig) error {
		return errors.New("boom")
	})
}

// A failed load must roll the tree back before any module keeps its
// registrations: the same root can load a valid product afterwards.
func TestFailedLoadRollsBackAndLeavesRootUsable(t *testing.T) {
	root := newRoot(t)
	registry := kernelx.NewRegistry()
	kernelx.MustRegister(registry, brokenModule())
	if _, err := kernelx.Load(root, registry, []kernelx.Entry{{ID: "broken", Module: "test/broken"}}); err == nil {
		t.Fatal("load of a failing required module succeeded")
	}

	loader, err := assembly.Load(root, assembly.ProductMinimal)
	if err != nil {
		t.Fatalf("Load after a failed load: %v", err)
	}
	if got := instanceIDs(loader); !slices.Equal(got, []string{"appinfo"}) {
		t.Fatalf("instances after recovery load = %v, want [appinfo]", got)
	}
	if err := loader.Close(); err != nil {
		t.Fatalf("loader.Close: %v", err)
	}
}
