// Command rigdoctor 是沙箱自检/诊断工具：它按给定策略渲染每个平台后端的真实
// profile，并做一次功能探测，回答"这台机器上哪些模式真的能强制"。
//
// 与同类实现的沙箱诊断子命令同形：当某个命令在沙箱里莫名失败时，
// 先用它确认实际生效的可写面，而不是凭猜测调策略。
//
// 用法：
//
//	go run ./cmd/rigdoctor -root /srv/workspace
//	go run ./cmd/rigdoctor -root /srv/workspace -mode read-only -writable ~/.some-tool
//	go run ./cmd/rigdoctor -root . -json
//	go run ./cmd/rigdoctor sandbox -root .           # 给用户看：我现在安全吗
//	go run ./cmd/rigdoctor sandbox -root . -json     # 同一份状态的机器可读版
//
// 注意：报告只描述**可写根围栏**。保护路径与拒绝 glob 的 API 已删除（替代是
// 可写根的 ReadOnlySubpaths 与策略层的 WithHidden），所以这里不会、也不该把它们
// 列成一个空列表。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
)

type report struct {
	Platform      string       `json:"platform"`
	Runner        string       `json:"runner"`
	RunnerError   string       `json:"runnerError,omitempty"`
	Policy        policyReport `json:"policy"`
	Modes         []modeReport `json:"modes"`
	TemporaryDirs []string     `json:"temporaryDirs,omitempty"`
	Warnings      []string     `json:"warnings,omitempty"`
}

type policyReport struct {
	Roots                []string `json:"roots"`
	WritableRoots        []string `json:"writableRoots"`
	DroppedWritableRoots []string `json:"droppedWritableRoots,omitempty"`
	ReadOnlySubpaths     []string `json:"readOnlySubpaths,omitempty"`
	IncludeSlashTmp      bool     `json:"includeSlashTmp"`
	IncludeTmpdirEnv     bool     `json:"includeTmpdirEnv"`
	Network              bool     `json:"network"`
	GrantBudget          int      `json:"grantBudget"`
}

type modeReport struct {
	Mode        string   `json:"mode"`
	Supported   bool     `json:"supported"`
	Error       string   `json:"error,omitempty"`
	Enforcement string   `json:"enforcement,omitempty"`
	ReadOnly    []string `json:"readOnly,omitempty"`
	ReadWrite   []string `json:"readWrite,omitempty"`
	ArgvPrefix  []string `json:"argvPrefix,omitempty"`
	Network     bool     `json:"network"`
	Denials     []string `json:"denialSignatures,omitempty"`
	// DroppedWritableRoots 是配置了但渲染不了的追加可写根。
	DroppedWritableRoots []string `json:"droppedWritableRoots,omitempty"`
}

func main() {
	// 子命令：`rigdoctor sandbox` 是"给用户看的一屏状态"，与默认的 profile
	// 详单分开（默认输出是给实现者看的）。
	if len(os.Args) > 1 && os.Args[1] == "sandbox" {
		os.Exit(runSandboxCommand(os.Args[2:]))
	}
	root := flag.String("root", ".", "workspace root (absolute or relative to the process cwd)")
	mode := flag.String("mode", "", "restrict the report to one mode (read-only|workspace-write|danger-full-access)")
	var writable stringList
	flag.Var(&writable, "writable", "additional writable root (repeatable; same as a runtime grant)")
	asJSON := flag.Bool("json", false, "emit JSON instead of a human-readable summary")
	flag.Parse()

	// 没有 -protect / -deny：保护路径与拒绝 glob 已从策略层删除，策略只围栏可写
	// 根。留着这两个参数会让人以为它们还在选什么东西。
	policy := sandbox.New(
		sandbox.WithRoots(*root),
		sandbox.WithWritableRoots(writable...),
	)
	// 报告**实际运行**的操作系统。不要读 GOOS 环境变量：它可能来自交叉编译的
	// shell 环境，与这份报告（以及实际探测到的后端）无关。
	result := report{Platform: runtime.GOOS}
	result.Policy = policyReport{
		Roots:                policy.Roots(),
		WritableRoots:        policy.AdditionalWritableRoots(),
		DroppedWritableRoots: policy.DroppedWritableRoots(),
		ReadOnlySubpaths:     declaredReadOnlySubpaths(policy),
		IncludeSlashTmp:      policy.IncludeSlashTmp(),
		IncludeTmpdirEnv:     policy.IncludeTmpdirEnv(),
		Network:              policy.NetworkAllowed(),
		GrantBudget:          policy.GrantBudget(),
	}
	if dropped := policy.DroppedWritableRoots(); len(dropped) > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("these configured writable roots were dropped (missing parent or sensitive path): %s", strings.Join(dropped, ", ")))
	}

	runner, err := osrunner.New(osrunner.Config{})
	if err != nil {
		result.RunnerError = err.Error()
		result.Warnings = append(result.Warnings, "no usable OS sandbox runner on this host; confined execution will fail closed")
	} else {
		result.Runner = runnerName(runner)
	}

	modes := []sandbox.Mode{sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite, sandbox.ModeDangerFullAccess}
	if *mode != "" {
		modes = []sandbox.Mode{sandbox.Mode(*mode)}
	}
	for _, candidate := range modes {
		result.Modes = append(result.Modes, describeMode(policy, candidate, runner))
	}

	if *asJSON {
		encoded, encodeErr := json.MarshalIndent(result, "", "  ")
		if encodeErr != nil {
			fmt.Fprintln(os.Stderr, "rigdoctor:", encodeErr)
			os.Exit(1)
		}
		fmt.Println(string(encoded))
		return
	}
	printHuman(result)
}

// describeMode 用给定模式构造策略副本并渲染 profile + 一次真实 Confine。
//
// 副本只复制**真实的写面围栏**：根、追加可写根，以及它们内部声明的只读子路径。
// 保护路径与拒绝 glob 的 API 已删除（替代见上文），这里没有可拷贝的东西。
func describeMode(base *sandbox.Policy, mode sandbox.Mode, runner sandbox.Runner) modeReport {
	options := []sandbox.Option{
		sandbox.WithRoots(base.Roots()...),
		sandbox.WithWritableRoots(base.AdditionalWritableRoots()...),
		sandbox.WithSlashTmp(base.IncludeSlashTmp()),
		sandbox.WithTmpdirEnv(base.IncludeTmpdirEnv()),
		sandbox.WithNetwork(base.NetworkAllowed()),
		sandbox.WithMode(mode),
	}
	// 只读子路径仍是生效的写面约束（Policy.AllowsWrite 会拒绝落在其中的写入），
	// 因此按根原样带进副本。只对"本来就是根/追加可写根"的根这么做，避免把运行时
	// 授权根变成副本里的静态可写根（那是两件不同的事）。
	known := make(map[string]struct{}, len(base.Roots())+len(base.AdditionalWritableRoots()))
	for _, root := range base.Roots() {
		known[root] = struct{}{}
	}
	for _, root := range base.AdditionalWritableRoots() {
		known[root] = struct{}{}
	}
	for _, root := range base.WritableRoots() {
		if _, ok := known[root.Root]; !ok || len(root.ReadOnlySubpaths) == 0 {
			continue
		}
		options = append(options, sandbox.WithReadOnlySubpaths(root.Root, root.ReadOnlySubpaths...))
	}
	policy := sandbox.New(options...)
	entry := modeReport{Mode: string(mode), Network: policy.NetworkAllowed()}
	if runner == nil {
		entry.Error = "no usable OS sandbox runner"
		return entry
	}
	confined, err := runner.Confine([]string{"true"}, policy)
	if err != nil {
		entry.Error = err.Error()
		return entry
	}
	entry.Supported = true
	entry.Enforcement = string(confined.Enforcement)
	entry.Denials = confined.DenialSignatures
	// argv 前缀（去掉末尾的 "true" 与分隔符）足以展示 profile 形状。
	if len(confined.Argv) > 1 {
		prefix := confined.Argv[:len(confined.Argv)-1]
		if len(prefix) > 0 && prefix[len(prefix)-1] == "--" {
			prefix = prefix[:len(prefix)-1]
		}
		entry.ArgvPrefix = prefix
	}
	if profile, profileErr := osrunner.BuildProfile(policy); profileErr == nil && profile != nil {
		entry.ReadOnly = profile.ReadOnly
		entry.ReadWrite = profile.ReadWrite
		entry.Network = profile.Network
		entry.DroppedWritableRoots = profile.Dropped
	}
	return entry
}

// runnerName 从 argv 前缀推断实际选中的后端（osrunner 的 Runner 接口没有 Name）。
func runnerName(runner sandbox.Runner) string {
	if runner == nil {
		return ""
	}
	confined, err := runner.Confine([]string{"true"}, sandbox.New(sandbox.WithRoots("."), sandbox.WithMode(sandbox.ModeReadOnly)))
	if err != nil || len(confined.Argv) == 0 {
		return "unknown"
	}
	argv0 := confined.Argv[0]
	switch {
	case strings.Contains(argv0, "bwrap"):
		return osrunner.RunnerBwrap
	case strings.Contains(argv0, "sandbox-exec"):
		return osrunner.RunnerSeatbelt
	case strings.Contains(argv0, osrunner.HelperArgWindowsACL), strings.HasSuffix(argv0, ".exe"):
		return osrunner.RunnerWindowsACL
	default:
		return osrunner.RunnerLandlock
	}
}

func printHuman(result report) {
	fmt.Printf("platform: %s\nrunner: %s\n", result.Platform, fallback(result.Runner, "(none)"))
	if result.RunnerError != "" {
		fmt.Printf("runner error: %s\n", result.RunnerError)
	}
	fmt.Printf("workspace roots: %s\n", strings.Join(result.Policy.Roots, ", "))
	fmt.Printf("additional writable roots: %s\n", fallback(strings.Join(result.Policy.WritableRoots, ", "), "(none)"))
	fmt.Printf("read-only subpaths inside those roots: %s\n", fallback(strings.Join(result.Policy.ReadOnlySubpaths, ", "), "(none)"))
	fmt.Printf("temp writable: /tmp=%t  $TMPDIR=%t\n", result.Policy.IncludeSlashTmp, result.Policy.IncludeTmpdirEnv)
	fmt.Printf("network allowed: %t   grant budget: %d\n", result.Policy.Network, result.Policy.GrantBudget)
	for _, warning := range result.Warnings {
		fmt.Printf("warning: %s\n", warning)
	}
	for _, mode := range result.Modes {
		fmt.Printf("\n[%s]\n", mode.Mode)
		if !mode.Supported {
			fmt.Printf("  not enforceable: %s\n", fallback(mode.Error, "unknown reason"))
			continue
		}
		fmt.Printf("  enforcement: %s\n", mode.Enforcement)
		fmt.Printf("  read-only: %s\n", fallback(strings.Join(mode.ReadOnly, ", "), "(none)"))
		fmt.Printf("  read-write: %s\n", fallback(strings.Join(mode.ReadWrite, ", "), "(none)"))
		if len(mode.DroppedWritableRoots) > 0 {
			fmt.Printf("  ⚠ configured but dropped writable roots: %s\n", strings.Join(mode.DroppedWritableRoots, ", "))
		}
		fmt.Printf("  argv prefix: %s\n", strings.Join(mode.ArgvPrefix, " "))
	}
}

// declaredReadOnlySubpaths 汇总策略里声明的"根内只读子路径"（去重、保持声明顺序）。
//
// 保护路径停用后，这是策略层剩下的唯一"根内不可写"约束，因此诊断必须把它列
// 出来——否则报告会让人以为可写根内一切可写。
func declaredReadOnlySubpaths(policy *sandbox.Policy) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, root := range policy.WritableRoots() {
		for _, subpath := range root.ReadOnlySubpaths {
			subpath = strings.TrimSpace(subpath)
			if subpath == "" {
				continue
			}
			if _, ok := seen[subpath]; ok {
				continue
			}
			seen[subpath] = struct{}{}
			out = append(out, subpath)
		}
	}
	return out
}

func fallback(value, whenEmpty string) string {
	if strings.TrimSpace(value) == "" {
		return whenEmpty
	}
	return value
}

// stringList 是可重复命令行参数的收集器。
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}
