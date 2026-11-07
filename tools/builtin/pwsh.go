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
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
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
//
// BackgroundJobs attaches the optional DSH background-job surface: when set,
// run_in_background returns a job id immediately and a foreground command that
// reaches its timeout is promoted to a background job. DisableRunInBackground
// and DisablePromotion turn those two behaviors off.
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
	// BackgroundJobs attaches the host's background-job manager (optional).
	BackgroundJobs BackgroundJobs
	// DisableRunInBackground removes and rejects run_in_background.
	DisableRunInBackground bool
	// DisablePromotion keeps a timed-out foreground command on the deadline-kill
	// path instead of promoting it to its background job.
	DisablePromotion bool
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

// NewPwshWithPolicyOptions is NewPwshWithPolicy with background-execution
// options: builtin.WithBackgroundJobs(manager) enables run_in_background and
// timeout promotion without having to build a PwshConfig by hand.
func NewPwshWithPolicyOptions(policy *sandbox.Policy, opts ...Option) rig.Tool {
	if policy == nil {
		policy = sandbox.New()
	}
	resolved := resolveOptions(opts)
	return NewPwshWithConfig(PwshConfig{
		Policy:                 policy,
		BackgroundJobs:         resolved.BackgroundJobs,
		DisableRunInBackground: !resolved.EnableRunInBackground,
		DisablePromotion:       !resolved.PromoteOnTimeout,
	})
}

// NewPwsh returns the legacy (root + timeout) pwsh tool, aligned with NewShell:
// commands run in root, the process environment is inherited, and there is no
// command allowlist.
func NewPwsh(root string, timeout time.Duration) rig.Tool {
	return NewPwshWithConfig(PwshConfig{Root: root, Timeout: timeout})
}

// NewPwshWithOptions is NewPwsh with background-execution options.
func NewPwshWithOptions(root string, timeout time.Duration, opts ...Option) rig.Tool {
	resolved := resolveOptions(opts)
	return NewPwshWithConfig(PwshConfig{
		Root:                   root,
		Timeout:                timeout,
		BackgroundJobs:         resolved.BackgroundJobs,
		DisableRunInBackground: !resolved.EnableRunInBackground,
		DisablePromotion:       !resolved.PromoteOnTimeout,
	})
}

// NewPwshWithConfig returns the pwsh tool with an explicit configuration. It is
// the constructor to use when the binary must be injected (tests, portable
// installs) or when policy mode and a custom binary are combined.
func NewPwshWithConfig(config PwshConfig) rig.Tool {
	tool := &pwshTool{
		binary:               strings.TrimSpace(config.Binary),
		policy:               config.Policy,
		background:           config.BackgroundJobs,
		allowRunInBackground: !config.DisableRunInBackground,
		promote:              !config.DisablePromotion,
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

	background           BackgroundJobs // nil = foreground-only
	allowRunInBackground bool
	promote              bool
}

// pwshArgs is the pwsh argument object (same shape as bash).
type pwshArgs struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	TimeoutMS   int    `json:"timeout_ms"`
	// TimeoutMSCamel is the DSH spelling of the timeout override; the legacy
	// timeout_ms wins when both are present.
	TimeoutMSCamel  int    `json:"timeoutMs"`
	Workdir         string `json:"workdir"`
	RunInBackground bool   `json:"run_in_background"`
	// SandboxPermissions and Justification are accepted for DSH wire
	// compatibility and rejected: Rig has no per-call escalation executor.
	SandboxPermissions string `json:"sandbox_permissions"`
	Justification      string `json:"justification"`
}

var _ rig.Tool = (*pwshTool)(nil)

// Name implements rig.Tool.
func (t *pwshTool) Name() string { return ToolPwsh }

// Description implements rig.Tool.
func (t *pwshTool) Description() string {
	description := "Run a PowerShell script with <binary> -NoProfile -NonInteractive -Command. " +
		"stdout and stderr are merged and capped; the exit code is appended; " +
		"non-zero exits and timeouts are reported as tool errors. workdir must be " +
		"inside the tool root (or the sandbox policy roots). In policy mode the binary " +
		"must be allow-listed and unquoted operators (;, |, &&, $(...), redirects) are " +
		"rejected unless the policy opts in. This tool is not an OS sandbox: the approval gate is."
	if t.backgroundEnabled() {
		description += " Long scripts can run in the background (run_in_background returns a job id immediately; " +
			"collect output with job_output, stop with job_kill); a foreground command that reaches its timeout is " +
			"moved to a background job instead of being killed."
	}
	return description
}

// backgroundEnabled reports whether the tool can start background jobs.
func (t *pwshTool) backgroundEnabled() bool { return t.background != nil }

// Schema implements rig.Tool.
func (t *pwshTool) Schema() map[string]any {
	properties := map[string]any{
		"command":     toolutil.StringProp("PowerShell script to run with -NoProfile -NonInteractive -Command."),
		"description": toolutil.StringProp("Optional active-voice description of the script for the UI, 5-10 words. Provide it before command when the wire format allows."),
		"timeout_ms":  toolutil.IntegerProp("Timeout in milliseconds (default 30000, maximum 600000)."),
		"timeoutMs":   toolutil.IntegerProp("DSH alias for timeout_ms."),
		"workdir":     toolutil.StringProp("Working directory relative to the tool root / policy roots (default: the root)."),
	}
	if t.backgroundEnabled() && t.allowRunInBackground {
		properties["run_in_background"] = toolutil.BooleanProp("Run in the background and return a job id immediately (collect with job_output, stop with job_kill). No timeout applies.")
	}
	return toolutil.ObjectSchema(properties, "command")
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
	if args.TimeoutMS <= 0 {
		args.TimeoutMS = args.TimeoutMSCamel
	}
	if err := checkEscalationArgs("pwsh", args.SandboxPermissions, args.Justification); err != nil {
		return toolutil.ErrorResult(err), nil
	}
	if args.RunInBackground && !t.backgroundEnabled() {
		return toolutil.ErrorResultf("pwsh: background jobs unavailable: no background job manager is configured; run the command in the foreground"), nil
	}
	if args.RunInBackground && !t.allowRunInBackground {
		return toolutil.ErrorResultf("pwsh: run_in_background is disabled for this tool configuration"), nil
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
	argv := []string{binary, "-NoProfile", "-NonInteractive", "-Command", args.Command}

	if args.RunInBackground {
		return t.runInBackground(args, argv, workdir, outputLimit)
	}
	if t.backgroundEnabled() && t.promote {
		if result, handled, runErr := t.runPromotedForeground(ctx, args, argv, workdir, effectiveTimeout, outputLimit); handled {
			return result, runErr
		}
	}
	return t.runForeground(ctx, args, binary, workdir, effectiveTimeout, outputLimit)
}

// runInBackground starts the script as a managed background job.
func (t *pwshTool) runInBackground(args pwshArgs, argv []string, workdir string, outputLimit int) (rig.ToolResult, error) {
	job, err := t.background.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindPwsh),
		Label:            args.Command,
		Command:          args.Command,
		Argv:             argv,
		Dir:              workdir,
		OutputLimitBytes: outputLimit,
		PolicyChecked:    true,
	})
	if err != nil {
		return toolutil.ErrorResultf("pwsh: background jobs unavailable: %v", err), nil
	}
	return toolutil.TextResult("started background job " + job.ID()), nil
}

// runPromotedForeground registers a foreground script as a job, waits up to
// timeout and, when it is still running, hands it to the model as a background
// job.
func (t *pwshTool) runPromotedForeground(ctx context.Context, args pwshArgs, argv []string, workdir string, timeout time.Duration, outputLimit int) (rig.ToolResult, bool, error) {
	job, err := t.background.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindPwsh),
		Label:            args.Command,
		Command:          args.Command,
		Argv:             argv,
		Dir:              workdir,
		OutputLimitBytes: outputLimit,
		PolicyChecked:    true,
	})
	if err != nil {
		return rig.ToolResult{}, false, nil
	}
	_, settled, waitErr := job.Wait(ctx, timeout)
	if waitErr != nil {
		job.Cancel("tool call aborted")
		_, _, _ = job.Wait(context.Background(), killWaitGrace)
		_ = job.Drop()
		return rig.ToolResult{}, true, waitErr
	}
	if settled {
		output, view := job.Read()
		_ = job.Drop()
		return toolutil.TextResult(renderShellOutput(output, view.ExitCode)), true, nil
	}
	output, _ := job.Read()
	return toolutil.TextResult(renderPromoted(output, timeout, job.ID())), true, nil
}

// runForeground is the deadline-kill foreground path.
func (t *pwshTool) runForeground(ctx context.Context, args pwshArgs, binary, workdir string, effectiveTimeout time.Duration, outputLimit int) (rig.ToolResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, effectiveTimeout)
	defer cancel()
	output := &truncatingBuffer{limit: outputLimit}
	command := exec.CommandContext(runCtx, binary, "-NoProfile", "-NonInteractive", "-Command", args.Command)
	if t.policy != nil {
		command.Env = t.policy.CommandEnv(os.Environ())
	}
	// A timed-out shell is killed, but a child process may keep the output pipes
	// open (e.g. `sleep`): WaitDelay closes them so the tool returns at the
	// deadline instead of waiting for the child.
	command.WaitDelay = 500 * time.Millisecond
	command.Dir = workdir
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
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			// DSH semantics: a non-zero exit is a result for the model to
			// interpret, not a tool error. A timeout above is still an error.
			content += fmt.Sprintf("\n[exit code: %d]", exitErr.ExitCode())
			return toolutil.TextResult(content), nil
		}
		// Start failure (missing binary, permission denied, ...) is a
		// configuration problem, not an exit code.
		content += fmt.Sprintf("\n[pwsh: cannot start %q: %v]", binary, runErr)
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
