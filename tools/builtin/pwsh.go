package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolPwsh is the model-facing name of the PowerShell tool.
const ToolPwsh = "pwsh"

// PwshConfig configures the pwsh tool.
//
// There are two modes, mirroring bash:
//
//   - policy mode (Policy != nil): command allowlist, environment allowlist,
//     timeout ceiling, output cap and roots come from the sandbox policy. Use
//     NewPwshWithPolicy for the auto-detected binary or NewPwshWithConfig to
//     inject a binary.
//   - legacy mode (Policy == nil): Root is the tool root, the process
//     environment is inherited and only the root/workdir confinement applies.
//
// Binary overrides the executable: empty auto-detects pwsh then powershell on
// PATH (pwsh.exe/powershell.exe first on Windows). Tests inject a fake script
// through Binary.
type PwshConfig struct {
	// Binary is the PowerShell executable (a path or a PATH lookup name).
	// Empty means auto-detect.
	Binary string
	// Root is the tool root in legacy mode (empty means the process working
	// directory). Ignored in policy mode.
	Root string
	// Timeout is the default per-call timeout; zero means the policy's timeout
	// (policy mode) or DefaultShellTimeout.
	Timeout time.Duration
	// Policy, when non-nil, enables sandbox-policy enforcement.
	Policy *sandbox.Policy
}

// NewPwshWithPolicy returns the pwsh tool governed by policy, using the
// auto-detected PowerShell binary. A nil policy means the fail-closed default
// policy (sandbox.New), exactly like NewWithPolicy. It is the PowerShell
// counterpart of NewShell's policy mode.
func NewPwshWithPolicy(policy *sandbox.Policy) rig.Tool {
	if policy == nil {
		policy = sandbox.New()
	}
	return NewPwshWithConfig(PwshConfig{Policy: policy})
}

// NewPwsh returns the legacy (root + timeout) pwsh tool, aligned with NewShell:
// commands run in root, the process environment is inherited, and there is no
// command allowlist.
func NewPwsh(root string, timeout time.Duration) rig.Tool {
	return NewPwshWithConfig(PwshConfig{Root: root, Timeout: timeout})
}

// NewPwshWithConfig returns the pwsh tool with an explicit configuration. It is
// the constructor to use when the binary must be injected (tests, portable
// installs) or when policy mode and a custom binary are combined.
func NewPwshWithConfig(config PwshConfig) rig.Tool {
	tool := &pwshTool{
		binary: strings.TrimSpace(config.Binary),
		policy: config.Policy,
	}
	if tool.policy != nil {
		tool.root = newPolicyRoot(tool.policy)
		tool.timeout = config.Timeout
		if tool.timeout <= 0 {
			tool.timeout = tool.policy.Timeout()
		}
	} else {
		tool.root = newFSRoot(config.Root)
		tool.timeout = config.Timeout
	}
	if tool.timeout <= 0 {
		tool.timeout = DefaultShellTimeout
	}
	if tool.timeout > MaxShellTimeout {
		tool.timeout = MaxShellTimeout
	}
	return tool
}

// pwshTool is the rig.Tool implementation of pwsh.
type pwshTool struct {
	binary  string // explicit binary; empty means auto-detect per call
	root    fsRoot
	policy  *sandbox.Policy // nil in legacy mode
	timeout time.Duration
}

// pwshArgs is the pwsh argument object (same shape as bash).
type pwshArgs struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
	Workdir   string `json:"workdir"`
}

var _ rig.Tool = (*pwshTool)(nil)

// Name implements rig.Tool.
func (t *pwshTool) Name() string { return ToolPwsh }

// Description implements rig.Tool.
func (t *pwshTool) Description() string {
	return "Run a PowerShell script with <binary> -NoProfile -NonInteractive -Command. " +
		"stdout and stderr are merged and capped; the exit code is appended; " +
		"non-zero exits and timeouts are reported as tool errors. workdir must be " +
		"inside the tool root (or the sandbox policy roots). In policy mode the binary " +
		"must be allow-listed and unquoted operators (;, |, &&, $(...), redirects) are " +
		"rejected unless the policy opts in. This tool is not an OS sandbox: the approval gate is."
}

// Schema implements rig.Tool.
func (t *pwshTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"command":    toolutil.StringProp("PowerShell script to run with -NoProfile -NonInteractive -Command."),
		"timeout_ms": toolutil.IntegerProp("Timeout in milliseconds (default 30000, maximum 600000)."),
		"workdir":    toolutil.StringProp("Working directory relative to the tool root / policy roots (default: the root)."),
	}, "command")
}

// Invoke implements rig.Tool.
func (t *pwshTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args pwshArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	if strings.TrimSpace(args.Command) == "" {
		return toolutil.ErrorResultf("pwsh: command is required"), nil
	}

	binary, err := resolvePwshBinary(t.binary)
	if err != nil {
		return toolutil.ErrorResultf("pwsh: %v", err), nil
	}

	// Policy mode: read-only disables the shell, and the command (including the
	// PowerShell flags and the script argument) must pass the allow/deny check
	// with a workdir inside the policy roots.
	if t.policy != nil {
		if t.policy.ReadOnly() {
			return toolutil.ErrorResultf("pwsh: sandbox policy is read-only; shell commands are disabled"), nil
		}
		if err := t.policy.CheckShellCommand(args.Command); err != nil {
			return toolutil.ErrorResult(err), nil
		}
		argv := []string{filepath.Base(binary), "-NoProfile", "-NonInteractive", "-Command", args.Command}
		if err := t.policy.CheckCommand(argv, args.Workdir); err != nil {
			return toolutil.ErrorResult(err), nil
		}
	}

	effectiveTimeout := t.timeout
	if args.TimeoutMS > 0 {
		effectiveTimeout = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	if t.policy != nil && effectiveTimeout > t.policy.Timeout() {
		effectiveTimeout = t.policy.Timeout()
	}
	effectiveTimeout = min(effectiveTimeout, MaxShellTimeout)

	var workdir string
	if t.policy != nil {
		workdir, err = resolvePolicyWorkdir(t.root, args.Workdir)
	} else {
		workdir, err = resolveWorkdir(t.root, args.Workdir)
	}
	if err != nil {
		return toolutil.ErrorResult(err), nil
	}

	outputLimit := MaxShellOutputBytes
	if t.policy != nil {
		outputLimit = t.policy.MaxOutputBytes()
	}
	runCtx, cancel := context.WithTimeout(ctx, effectiveTimeout)
	defer cancel()
	output := &truncatingBuffer{limit: outputLimit}
	command, cmdErr := t.policy.CommandContext(runCtx, workdir, binary, "-NoProfile", "-NonInteractive", "-Command", args.Command)
	if cmdErr != nil {
		return toolutil.ErrorResult(cmdErr), nil
	}
	if t.policy != nil {
		command.Env = t.policy.CommandEnv(os.Environ())
	}
	// A timed-out shell is killed, but a child process may keep the output pipes
	// open (e.g. `sleep`): WaitDelay closes them so the tool returns at the
	// deadline instead of waiting for the child.
	command.WaitDelay = 500 * time.Millisecond
	command.Stdout = output
	command.Stderr = output
	command.Stdin = nil
	runErr := command.Run()

	content := output.String()
	switch {
	case ctx.Err() != nil:
		// The caller's context was cancelled: abort the run instead of
		// misreporting it to the model as a tool failure.
		return rig.ToolResult{}, ctx.Err()
	case runCtx.Err() == context.DeadlineExceeded:
		content += fmt.Sprintf("\n[pwsh timeout: command did not finish within %s]", effectiveTimeout)
		return rig.ToolResult{Content: content, IsError: true}, nil
	case runErr != nil:
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			// Start failure (missing binary, permission denied, ...) is a
			// configuration problem, not an exit code.
			content += fmt.Sprintf("\n[pwsh: cannot start %q: %v]", binary, runErr)
			return rig.ToolResult{Content: content, IsError: true}, nil
		}
		content += fmt.Sprintf("\n[exit code: %d]", exitErr.ExitCode())
		return rig.ToolResult{Content: content, IsError: true}, nil
	default:
		return toolutil.TextResult(content), nil
	}
}

// resolvePwshBinary returns the injected binary or detects pwsh/powershell on
// PATH. The error is model-readable and tells the operator how to fix it.
func resolvePwshBinary(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	names := pwshBinaryNames()
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf(
		"PowerShell not found: looked for %s on PATH; install PowerShell or set PwshConfig.Binary",
		strings.Join(names, ", "))
}

// pwshBinaryNames returns the executable names to probe, in priority order.
func pwshBinaryNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"pwsh.exe", "powershell.exe", "pwsh", "powershell"}
	}
	return []string{"pwsh", "powershell"}
}
