package host

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/assembly"
	"github.com/zeokeel/zeokeel-rig/kernelx"
	"github.com/zeokeel/zeokeel-rig/modules/appinfo"
)

func TestOptionsNormalize(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dataDirEnv, dir)

	normalized := Options{}.normalize()
	if normalized.Product != assembly.ProductHeadless {
		t.Fatalf("default product = %q, want %q", normalized.Product, assembly.ProductHeadless)
	}
	if normalized.DataDir != dir {
		t.Fatalf("default data dir = %q, want $%s = %q", normalized.DataDir, dataDirEnv, dir)
	}
	if normalized.Version != Version || normalized.Commit != Commit || normalized.BuildTime != BuildTime {
		t.Fatalf("default build identity = %q/%q/%q, want the ldflags values",
			normalized.Version, normalized.Commit, normalized.BuildTime)
	}

	explicit := Options{
		Product:   "minimal.json",
		DataDir:   "/tmp/explicit",
		Version:   "1.2.3",
		Commit:    "abc",
		BuildTime: "now",
	}.normalize()
	if explicit.Product != assembly.ProductMinimal || explicit.DataDir != "/tmp/explicit" ||
		explicit.Version != "1.2.3" || explicit.Commit != "abc" || explicit.BuildTime != "now" {
		t.Fatalf("explicit options were overwritten: %+v", explicit)
	}

	custom := Options{Entries: []kernelx.Entry{{ID: "appinfo", Module: appinfo.ModuleID}}}.normalize()
	if custom.Product != "custom" {
		t.Fatalf("custom product label = %q, want custom", custom.Product)
	}
}

func TestBootHeadlessProduct(t *testing.T) {
	dir := t.TempDir()
	boot, err := Boot(context.Background(), Options{DataDir: dir})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := boot.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if boot.Product != assembly.ProductHeadless {
		t.Fatalf("Product = %q, want %q", boot.Product, assembly.ProductHeadless)
	}
	instances := boot.Instances()
	if len(instances) != 2 {
		t.Fatalf("instances = %+v, want appinfo + modules group", instances)
	}
	wantIDs := []string{"appinfo", "modules"}
	for index, instance := range instances {
		if instance.ID != wantIDs[index] {
			t.Fatalf("instance %d ID = %q, want %q", index, instance.ID, wantIDs[index])
		}
		if instance.State.String() != "active" {
			t.Fatalf("instance %q state = %s, want active", instance.ID, instance.State)
		}
	}
	if instance, ok := boot.Instance("appinfo"); !ok || instance.Module != appinfo.ModuleID {
		t.Fatalf("Instance(appinfo) = %+v (found=%v)", instance, ok)
	}

	info, ok := boot.AppInfo()
	if !ok {
		t.Fatal("appinfo service is not available after Boot")
	}
	if info.Name != "ZeoKeel-Rig" || info.Version != Version {
		t.Fatalf("appinfo = %+v, want name ZeoKeel-Rig and version %q", info, Version)
	}

	// The host services are provided on the root for modules to consume.
	if _, ok := boot.Root().Get(KeySettings); !ok {
		t.Fatal("settings service is not provided on the root")
	}
	if _, ok := boot.Root().Get(KeyLogger); !ok {
		t.Fatal("log service is not provided on the root")
	}
	if settings := boot.Settings(); settings.Get("missing", "fallback") != "fallback" {
		t.Fatal("boot settings do not return defaults")
	}

	// Close unloads the assembly and disposes the root, withdrawing services;
	// it is idempotent.
	if err := boot.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := boot.AppInfo(); ok {
		t.Fatal("appinfo service survived Close")
	}
	if err := boot.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestBootBuildInfoOverlay(t *testing.T) {
	boot, err := Boot(context.Background(), Options{
		DataDir:   t.TempDir(),
		Version:   "9.9.9",
		Commit:    "deadbeef",
		BuildTime: "2026-11-06T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer func() {
		if err := boot.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	info, ok := boot.AppInfo()
	if !ok {
		t.Fatal("appinfo service is not available")
	}
	if info.Version != "9.9.9" || info.Commit != "deadbeef" || info.BuildTime != "2026-11-06T12:00:00Z" {
		t.Fatalf("appinfo = %+v, want the host build identity overlaid", info)
	}
	// The product name from the embedded assembly config is preserved.
	if info.Name != "ZeoKeel-Rig" {
		t.Fatalf("appinfo name = %q, want the assembly-configured ZeoKeel-Rig", info.Name)
	}
}

func TestBootUnknownProductHasNoSideEffects(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "never-created")
	_, err := Boot(context.Background(), Options{Product: "no-such-product", DataDir: dataDir})
	if err == nil {
		t.Fatal("Boot(unknown product) succeeded")
	}
	if !strings.Contains(err.Error(), "unknown product") {
		t.Fatalf("Boot(unknown product) = %v, want an unknown-product error", err)
	}
	if _, statErr := os.Stat(dataDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unknown product created the data directory: %v", statErr)
	}
}

func TestBootCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Boot(ctx, Options{DataDir: t.TempDir()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Boot(canceled ctx) = %v, want context.Canceled", err)
	}
}

// fixtureConfig is the empty typed config of the fixture module.
type fixtureConfig struct {
	Message string `json:"message"`
}

// fixtureModule consumes both a kernel service (appinfo.Info, provided by an
// entry) and a host service (the log, declared as a builtin by Boot). It only
// activates when the assembly and the host agree.
func fixtureModule() *kernelx.Descriptor[fixtureConfig] {
	return kernelx.Define("test/fixture", func(ctx kernelx.Context, cfg fixtureConfig) error {
		logger, err := ctx.Require(KeyLogger)
		if err != nil {
			return err
		}
		info, err := ctx.Require(appinfo.Key)
		if err != nil {
			return err
		}
		logger.Printf("test/fixture: %s (%s %s)", cfg.Message, info.Name, info.Version)
		return nil
	}).
		AddRequires(kernelx.Requires(KeyLogger)).
		AddRequires(kernelx.Requires(appinfo.Key))
}

func TestBootCustomRegistryAndEntries(t *testing.T) {
	dir := t.TempDir()
	registry := assembly.NewRegistry()
	kernelx.MustRegister(registry, fixtureModule())
	entries := []kernelx.Entry{
		{ID: "appinfo", Module: appinfo.ModuleID, Config: map[string]any{"name": "custom-product"}},
		{ID: "fixture", Module: "test/fixture", Config: map[string]any{"message": "hello"}},
	}

	boot, err := Boot(context.Background(), Options{
		DataDir:  dir,
		Registry: registry,
		Entries:  entries,
		Version:  "1.2.3",
	})
	if err != nil {
		t.Fatalf("Boot(custom product): %v", err)
	}
	defer func() {
		if err := boot.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if boot.Product != "custom" {
		t.Fatalf("Product = %q, want custom", boot.Product)
	}
	if instance, ok := boot.Instance("fixture"); !ok || instance.State.String() != "active" {
		t.Fatalf("fixture instance = %+v (found=%v), want active", instance, ok)
	}
	info, ok := boot.AppInfo()
	if !ok || info.Name != "custom-product" || info.Version != "1.2.3" {
		t.Fatalf("appinfo = %+v (found=%v), want custom-product 1.2.3", info, ok)
	}

	// The fixture logged through the host service during activation.
	logPath := filepath.Join(dir, "logs", LogFileName)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log %q: %v", logPath, err)
	}
	if !strings.Contains(string(data), "test/fixture: hello (custom-product 1.2.3)") {
		t.Fatalf("log = %q, want the fixture line", data)
	}
}

func TestRunContextShutsDown(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	if err := runContext(ctx, Options{DataDir: dir}, &out); err != nil {
		t.Fatalf("runContext: %v", err)
	}
	output := out.String()
	for _, want := range []string{
		"ZeoKeel-Rig",
		"product: headless",
		"instance appinfo",
		"state=active",
		"ready; waiting for SIGINT or SIGTERM",
		"shutting down",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("run output = %q, want it to mention %q", output, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", LogFileName)); err != nil {
		t.Fatalf("log file after RunContext: %v", err)
	}
}

func TestMainVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := mainWithStreams([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--version exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	for _, want := range []string{appName, "commit", runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("--version output = %q, want %q", stdout.String(), want)
		}
	}
}

func TestMainListProducts(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := mainWithStreams([]string{"--list-products"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--list-products exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if got := strings.Fields(stdout.String()); len(got) != 2 ||
		got[0] != assembly.ProductHeadless || got[1] != assembly.ProductMinimal {
		t.Fatalf("--list-products output = %q, want headless and minimal", stdout.String())
	}
}

func TestMainHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := mainWithStreams([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("--help exit code = %d, want 0", code)
	}
	for _, want := range []string{"--product", "--data-dir", "--version", "--list-products"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("--help output = %q, want %q", stdout.String(), want)
		}
	}
}

func TestMainUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown flag", args: []string{"--no-such-flag"}, want: "flag provided but not defined"},
		{name: "unexpected argument", args: []string{"extra"}, want: "unexpected argument"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := mainWithStreams(test.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), test.want)
			}
		})
	}
}

func TestMainUnknownProduct(t *testing.T) {
	t.Setenv(dataDirEnv, t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := mainWithStreams([]string{"--product", "nope"}, &stdout, &stderr); code != 1 {
		t.Fatalf("unknown product exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown product") {
		t.Fatalf("stderr = %q, want an unknown-product error", stderr.String())
	}
}

func TestVersionString(t *testing.T) {
	opts := Options{Version: "2.0.0", Commit: "abcdef", BuildTime: "yesterday"}
	got := opts.versionString()
	for _, want := range []string{appName, "2.0.0", "abcdef", "yesterday", runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Fatalf("versionString() = %q, want %q", got, want)
		}
	}
	if strings.Count(got, "\n") != 0 {
		t.Fatalf("versionString() = %q, want a single line", got)
	}
}

func TestDefaultDataDirEnvOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custom-data")
	t.Setenv(dataDirEnv, "  "+dir+"  ")
	if got := defaultDataDir(); got != dir {
		t.Fatalf("defaultDataDir() = %q, want the trimmed $%s %q", got, dataDirEnv, dir)
	}
}
