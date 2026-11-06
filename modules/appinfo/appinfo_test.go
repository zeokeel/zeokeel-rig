package appinfo_test

import (
	"testing"

	"github.com/zeokeel/zeokeel-rig/kernelx"
	"github.com/zeokeel/zeokeel-rig/modules/appinfo"
)

// loadAppinfo starts appinfo configured with config and returns the loader.
func loadAppinfo(t *testing.T, config map[string]any) (*kernelx.Loader, kernelx.Context) {
	t.Helper()
	root := kernelx.Root()
	t.Cleanup(func() {
		if err := root.DisposeAll(); err != nil {
			t.Errorf("DisposeAll: %v", err)
		}
	})
	registry := kernelx.NewRegistry()
	kernelx.MustRegister(registry, appinfo.Module())
	loader, err := kernelx.Load(root, registry, []kernelx.Entry{{
		ID:     "appinfo",
		Module: appinfo.ModuleID,
		Config: config,
	}})
	if err != nil {
		t.Fatalf("load appinfo: %v", err)
	}
	t.Cleanup(func() {
		if err := loader.Close(); err != nil {
			t.Errorf("loader.Close: %v", err)
		}
	})
	return loader, root
}

func TestModuleProvidesInfo(t *testing.T) {
	loader, root := loadAppinfo(t, map[string]any{
		"name":      "test-product",
		"version":   "1.2.3",
		"commit":    "deadbeef",
		"buildTime": "2026-11-06T00:00:00Z",
	})

	instances := loader.Instances()
	if len(instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(instances))
	}
	if state := instances[0].State(); state.String() != "active" {
		t.Fatalf("appinfo state = %s, want active", state)
	}

	info, ok := root.Get(appinfo.Key)
	if !ok {
		t.Fatal("appinfo.Key is not available after activation")
	}
	if info.Name != "test-product" || info.Version != "1.2.3" ||
		info.Commit != "deadbeef" || info.BuildTime != "2026-11-06T00:00:00Z" {
		t.Fatalf("Info = %+v, want the configured build identity", info)
	}
	if info.GoVersion == "" {
		t.Fatal("Info.GoVersion is empty")
	}
}

func TestModuleDefaults(t *testing.T) {
	_, root := loadAppinfo(t, nil)
	info, ok := root.Get(appinfo.Key)
	if !ok {
		t.Fatal("appinfo.Key is not available after activation")
	}
	if info.Name != "ZeoKeel-Rig" || info.Version != "dev" ||
		info.Commit != "unknown" || info.BuildTime != "unknown" {
		t.Fatalf("Info = %+v, want the documented defaults", info)
	}
}

func TestDescriptorContracts(t *testing.T) {
	descriptor := appinfo.Module()
	if descriptor.ID != appinfo.ModuleID {
		t.Fatalf("ID = %q, want %q", descriptor.ID, appinfo.ModuleID)
	}
	if descriptor.Name == "" {
		t.Fatal("descriptor has no display name")
	}
	if len(descriptor.Provides) != 1 || descriptor.Provides[0].Name != appinfo.Key.Name() {
		t.Fatalf("Provides = %+v, want exactly %q", descriptor.Provides, appinfo.Key.Name())
	}
	if len(descriptor.Requires) != 0 {
		t.Fatalf("Requires = %+v, want none", descriptor.Requires)
	}
}
