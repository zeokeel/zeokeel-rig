// Package host is ZeoKeel-Rig's headless process host.
//
// It owns everything a headless product needs around the kernel: the command
// line, the data directory, the settings and log files, the kernel root with
// the host services provided, the assembly load, a readable startup report and
// the shutdown sequence.
//
// The host is intentionally small and display-free: it can run in CI, in a
// container and over SSH. A product that needs a UI layers one on top of the
// same assembly; the SDK itself does not import a UI toolkit.
//
// Lifecycle:
//
//	Boot(ctx, opts)  -> kernel root + provided host services + loaded assembly
//	                  (tests use it directly; it does not wait)
//	Run/RunContext   -> Boot, print product + instances, wait for shutdown
//	                  (SIGINT/SIGTERM for Run), then unload and dispose
package host

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig/assembly"
	"github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/kernelx"
	"github.com/zeokeel/zeokeel-rig/keys"
	"github.com/zeokeel/zeokeel-rig/modules/appinfo"
)

// Process identity and defaults.
const (
	appName            = "ZeoKeel-Rig"
	programName        = "zeokeel-rig"
	dataDirEnv         = "RIG_DATA_DIR"
	defaultDataDirName = ".zeokeel-rig"
)

// Build identity of the running binary. Release builds fill these with
//
//	-ldflags "-X github.com/zeokeel/zeokeel-rig/host.Version=... \
//	          -X github.com/zeokeel/zeokeel-rig/host.Commit=... \
//	          -X github.com/zeokeel/zeokeel-rig/host.BuildTime=..."
//
// A plain `go build` / `go test` keeps the defaults.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// Service keys of the services the host provides on the kernel root before the
// assembly loads. They are exported so modules can declare them as
// requirements; Boot declares both as registry builtins, so pre-flight resolves
// such modules and verifies the host really provided them.
var (
	// KeySettings is the key of the host's v1.Settings store.
	// KeySettings is kept for compatibility; new modules should use keys.SettingsKey.
	KeySettings = keys.SettingsKey
	// KeyLogger is the key of the host's v1.Logger sink.
	// KeyLogger is kept for compatibility; new modules should use keys.LoggerKey.
	KeyLogger = keys.LoggerKey
)

// Options is one headless run's configuration. Zero values are meaningful: the
// product defaults to "headless", the data directory to $RIG_DATA_DIR or
// ~/.zeokeel-rig, and build identity to the ldflags values.
type Options struct {
	// Product selects the embedded assembly product (see assembly.Products).
	Product string
	// DataDir is the product's data directory: settings and logs live here.
	DataDir string
	// Version, Commit and BuildTime are the running binary's build identity;
	// the host overlays them onto the appinfo module's config.
	Version   string
	Commit    string
	BuildTime string

	// Registry optionally replaces the built-in compile-time registry, so a
	// product can add its own modules before loading:
	//
	//	registry := assembly.NewRegistry()
	//	kernelx.MustRegister(registry, mymodule.Module())
	//
	// Nil uses assembly.NewRegistry().
	Registry *kernelx.Registry
	// Entries optionally replaces the embedded product's entry tree, for
	// products that ship their own assembly document (parse it with
	// assembly.ParseEntries). Nil uses assembly.Entries(Product).
	Entries []kernelx.Entry
}

// normalize fills zero-valued options with their process defaults.
func (o Options) normalize() Options {
	o.Product = strings.TrimSpace(o.Product)
	o.Product = strings.TrimSpace(strings.TrimSuffix(o.Product, ".json"))
	if o.Product == "" {
		if o.Entries != nil {
			o.Product = "custom"
		} else {
			o.Product = assembly.ProductHeadless
		}
	}
	if strings.TrimSpace(o.DataDir) == "" {
		o.DataDir = defaultDataDir()
	}
	if o.Version == "" {
		o.Version = Version
	}
	if o.Commit == "" {
		o.Commit = Commit
	}
	if o.BuildTime == "" {
		o.BuildTime = BuildTime
	}
	return o
}

// versionString is the one line this run reports: the binary's build identity
// plus the runtime facts a bug report needs.
func (o Options) versionString() string {
	return formatVersion(appName, o.Version, o.Commit, o.BuildTime)
}

func versionString() string { return formatVersion(appName, Version, Commit, BuildTime) }

func formatVersion(name, version, commit, buildTime string) string {
	return fmt.Sprintf("%s %s (commit %s, built %s, %s/%s, %s)",
		name, orDefault(version, "dev"), orDefault(commit, "unknown"),
		orDefault(buildTime, "unknown"), runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// defaultDataDir resolves the data directory: $RIG_DATA_DIR when set, else
// ~/.zeokeel-rig.
func defaultDataDir() string {
	if dir := strings.TrimSpace(os.Getenv(dataDirEnv)); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, defaultDataDirName)
	}
	return filepath.Join(".", defaultDataDirName)
}

// Instance is a read-only summary of one loaded module instance. The kernel
// does not expose a stable instance type here, so the host reports the stable
// entry ID, the module ID and the lifecycle state.
type Instance struct {
	// ID is the stable entry ID from the assembly document.
	ID string
	// Module is the registered module ID that was instantiated.
	Module string
	// State is the kernel lifecycle state (active, pending, failed, ...).
	State kernelx.FiberState
}

// String renders one instance as the host's startup report does.
func (i Instance) String() string {
	return fmt.Sprintf("%s (module %s) %s", i.ID, i.Module, i.State)
}

// Process is one loaded headless process: the kernel root with the host
// services provided and the assembly's module tree running.
//
// It does not wait and does not read signals: Run/RunContext add the wait, and
// tests call Boot directly, inspect Loader/Instances/Root and then Close it.
// (Go does not allow a type and a function to share a name, so the constructor
// is Boot and the value it returns is a Process.)
type Process struct {
	// Product is the product label this boot reports ("headless", "custom", ...).
	Product string
	// Loader is the loaded assembly (nil until loading succeeded). It is
	// exported for tests and embedders that need raw kernel instances.
	Loader *kernelx.Loader

	opts     Options
	root     kernelx.Context
	settings *Settings
	logger   *Logger

	closeOnce sync.Once
	closeErr  error
}

// Boot validates and starts one product: resolve the entry tree, overlay the
// build identity, create the kernel root, provide the host services, load the
// assembly. ctx is checked before the expensive work and again after the load,
// so a canceled boot leaves nothing behind.
//
// On success the caller owns the Process and must Close it.
func Boot(ctx context.Context, opts Options) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts = opts.normalize()

	entries, err := resolveEntries(opts)
	if err != nil {
		return nil, err
	}
	entries = applyBuildInfo(entries, opts)
	registry := opts.Registry
	if registry == nil {
		registry = assembly.NewRegistry()
	}
	// The host provides these on the root before loading; declaring them as
	// builtins lets pre-flight resolve modules that require them and lets the
	// loader verify they are actually available.
	registry.AddBuiltin(KeySettings)
	registry.AddBuiltin(KeyLogger)

	logger := NewLogger(opts.DataDir)
	settings, err := NewSettings(opts.DataDir)
	if err != nil {
		_ = logger.Close()
		return nil, err
	}
	root := kernelx.Root()
	boot := &Process{
		Product:  opts.Product,
		opts:     opts,
		root:     root,
		settings: settings,
		logger:   logger,
	}
	if err := provideHostServices(root, settings, logger); err != nil {
		_ = boot.Close()
		return nil, err
	}
	loader, err := assembly.LoadEntries(root, registry, entries)
	if err != nil {
		_ = boot.Close()
		return nil, fmt.Errorf("rig: load product %q: %w", opts.Product, err)
	}
	boot.Loader = loader
	if err := ctx.Err(); err != nil {
		_ = boot.Close()
		return nil, err
	}
	logger.Printf("rig: product %q loaded: %d module instances", boot.Product, len(boot.Instances()))
	return boot, nil
}

// Root returns the kernel root context the assembly was loaded on.
func (b *Process) Root() kernelx.Context {
	if b == nil {
		return kernelx.Context{}
	}
	return b.root
}

// Settings returns the host settings store (also provided as KeySettings).
func (b *Process) Settings() *Settings {
	if b == nil {
		return nil
	}
	return b.settings
}

// Logger returns the host logger (also provided as KeyLogger).
func (b *Process) Logger() *Logger {
	if b == nil {
		return nil
	}
	return b.logger
}

// AppInfo resolves the appinfo.Info service provided by the assembly.
func (b *Process) AppInfo() (appinfo.Info, bool) {
	if b == nil {
		return appinfo.Info{}, false
	}
	return b.root.Get(appinfo.Key)
}

// Instances returns a snapshot of the loaded instances in creation order.
func (b *Process) Instances() []Instance {
	if b == nil || b.Loader == nil {
		return nil
	}
	loaded := b.Loader.Instances()
	instances := make([]Instance, 0, len(loaded))
	for _, instance := range loaded {
		instances = append(instances, Instance{
			ID:     instance.ID(),
			Module: instance.ModuleID(),
			State:  instance.State(),
		})
	}
	return instances
}

// Instance returns one loaded instance summary by stable entry ID.
func (b *Process) Instance(entryID string) (Instance, bool) {
	for _, instance := range b.Instances() {
		if instance.ID == entryID {
			return instance, true
		}
	}
	return Instance{}, false
}

// Print writes the startup report: version, product, instances, warnings and
// the wait notice.
func (b *Process) Print(out io.Writer) {
	if b == nil {
		return
	}
	fmt.Fprintf(out, "%s\n", b.opts.versionString())
	fmt.Fprintf(out, "product: %s\n", b.Product)
	for _, instance := range b.Instances() {
		fmt.Fprintf(out, "  instance %-12s module=%-24s state=%s\n", instance.ID, instance.Module, instance.State)
	}
	if b.Loader != nil {
		for _, warning := range b.Loader.Warnings() {
			fmt.Fprintf(out, "  warning: %s\n", warning)
		}
	}
	fmt.Fprintln(out, "ready; waiting for SIGINT or SIGTERM")
}

// Close unloads the assembly (module cleanups run in reverse order), disposes
// the kernel root and closes the log file. It is idempotent.
func (b *Process) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		var errs []error
		if b.Loader != nil {
			if err := b.Loader.Close(); err != nil {
				errs = append(errs, fmt.Errorf("rig: close assembly: %w", err))
			}
			b.Loader = nil
		}
		if err := b.root.DisposeAll(); err != nil {
			errs = append(errs, fmt.Errorf("rig: dispose kernel root: %w", err))
		}
		if b.logger != nil {
			if err := b.logger.Close(); err != nil {
				errs = append(errs, fmt.Errorf("rig: close log: %w", err))
			}
		}
		b.closeErr = errors.Join(errs...)
	})
	return b.closeErr
}

// Run runs one headless product until SIGINT or SIGTERM, then shuts down.
func Run(opts Options) error {
	ctx, stop := signal.NotifyContext(context.Background(), terminationSignals...)
	defer stop()
	return RunContext(ctx, opts)
}

// RunContext is Run with a caller-provided shutdown context: it boots the
// product, prints the startup report, waits for ctx to be canceled and then
// closes everything. Tests use it with a cancelable context instead of signals.
func RunContext(ctx context.Context, opts Options) error {
	return runContext(ctx, opts, os.Stdout)
}

func runContext(ctx context.Context, opts Options, out io.Writer) error {
	boot, err := Boot(ctx, opts)
	if err != nil {
		return err
	}
	boot.Print(out)
	<-ctx.Done()
	boot.logger.Printf("rig: shutting down: %d module instances", len(boot.Instances()))
	fmt.Fprintln(out, "shutting down")
	return boot.Close()
}

// Main is the process entry point of a headless product. It returns the process
// exit code instead of calling os.Exit so a thin main can do that.
func Main(args []string) int {
	return mainWithStreams(args, os.Stdout, os.Stderr)
}

func mainWithStreams(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(programName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	// Print help ourselves: --help is a success, not an error.
	flags.Usage = func() {}
	product := flags.String("product", assembly.ProductHeadless, "assembly product to run")
	dataDir := flags.String("data-dir", "", "data directory")
	showVersion := flags.Bool("version", false, "print version information and exit")
	listProducts := flags.Bool("list-products", false, "print the embedded assembly products and exit")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usageText)
			return 0
		}
		fmt.Fprint(stderr, usageText)
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n\n%s", programName, flags.Arg(0), usageText)
		return 2
	}
	if *listProducts {
		for _, name := range assembly.Products() {
			fmt.Fprintln(stdout, name)
		}
		return 0
	}
	if *showVersion {
		fmt.Fprintln(stdout, versionString())
		return 0
	}

	opts := Options{
		Product:   *product,
		DataDir:   *dataDir,
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
	}
	if err := Run(opts); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return 1
	}
	return 0
}

const usageText = `ZeoKeel-Rig - a compile-time modular agent SDK.

Usage:
  zeokeel-rig [flags]

Flags:
  --product <name>   assembly product to run (default "headless")
  --data-dir <dir>   data directory (default $RIG_DATA_DIR or ~/.zeokeel-rig)
  --version          print version information and exit
  --list-products    print the embedded assembly products and exit
  -h, --help         print this help and exit
`

// resolveEntries returns the entry tree a boot should load: the caller's
// explicit tree when given, else the embedded product.
func resolveEntries(opts Options) ([]kernelx.Entry, error) {
	if opts.Entries != nil {
		if len(opts.Entries) == 0 {
			return nil, errors.New("rig: Options.Entries is empty")
		}
		return opts.Entries, nil
	}
	return assembly.Entries(opts.Product)
}

// applyBuildInfo overlays the running binary's build identity onto every
// enabled appinfo entry. It returns new entries so a caller-provided tree is
// never mutated.
func applyBuildInfo(entries []kernelx.Entry, opts Options) []kernelx.Entry {
	out := make([]kernelx.Entry, len(entries))
	for index, entry := range entries {
		clone := entry
		if len(entry.Children) > 0 {
			clone.Children = applyBuildInfo(entry.Children, opts)
		}
		if !entry.Disabled && entry.Module == appinfo.ModuleID {
			config := cloneConfig(entry.Config)
			setIfNotEmpty(config, "version", opts.Version)
			setIfNotEmpty(config, "commit", opts.Commit)
			setIfNotEmpty(config, "buildTime", opts.BuildTime)
			clone.Config = config
		}
		out[index] = clone
	}
	return out
}

// cloneConfig copies an entry config map so overlaying build info cannot change
// the caller's map.
func cloneConfig(raw any) map[string]any {
	if values, ok := raw.(map[string]any); ok && values != nil {
		out := make(map[string]any, len(values)+3)
		for key, value := range values {
			out[key] = value
		}
		return out
	}
	return make(map[string]any, 3)
}

func setIfNotEmpty(config map[string]any, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		config[key] = value
	}
}

// provideHostServices provides the host services on the kernel root before the
// assembly loads.
func provideHostServices(root kernelx.Context, settings *Settings, logger *Logger) error {
	var settingsService v1.Settings = settings
	if _, err := root.Provide(KeySettings, settingsService); err != nil {
		return fmt.Errorf("rig: provide settings: %w", err)
	}
	var logService v1.Logger = logger
	if _, err := root.Provide(KeyLogger, logService); err != nil {
		return fmt.Errorf("rig: provide log: %w", err)
	}
	return nil
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
