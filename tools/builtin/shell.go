package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

type bashArgs struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	TimeoutMS   int    `json:"timeout_ms"`
	// TimeoutMSCamel is the DSH spellings of the timeout override; the legacy
	// timeout_ms wins when both are present.
	TimeoutMSCamel  int    `json:"timeoutMs"`
	Workdir         string `json:"workdir"`
	RunInBackground bool   `json:"run_in_background"`
	// SandboxPermissions and Justification are accepted for DSH wire
	// compatibility. Rig has no per-call sandbox-escalation executor, so a call
	// that sets them is rejected with the DSH "no sandboxing executor" error.
	SandboxPermissions string `json:"sandbox_permissions"`
	Justification      string `json:"justification"`
}

// bashTool is the bash built-in: /bin/sh -c, combined stdout+stderr, output
// capped, and a per-call timeout (constructor default, overridable by
// timeout_ms/timeoutMs). In policy mode the working directory is validated
// against the roots, the command must pass the allow/deny check, and unquoted
// shell control operators are rejected by the policy (CheckShellCommand) so an
// allow-listed command cannot chain a second one. The approval gate remains the
// real boundary (documented in the package comment).
//
// With a background-job manager attached (Options.BackgroundJobs) the tool also
// supports DSH background execution: run_in_background returns a job id
// immediately, and a foreground command that reaches its timeout is promoted to
// the background job instead of being killed.
type bashTool struct {
	root           fsRoot
	defaultTimeout time.Duration
	options        Options
}

// newBash builds the bash tool. options.BackgroundJobs is nil for the
// foreground-only constructors (NewShell, NewWithPolicy).
func newBash(root fsRoot, timeout time.Duration, options Options) rig.Tool {
	defaultTimeout := timeout
	if root.policy != nil && defaultTimeout <= 0 {
		defaultTimeout = root.policy.Timeout()
	}
	if defaultTimeout <= 0 {
		defaultTimeout = DefaultShellTimeout
	}
	defaultTimeout = min(defaultTimeout, MaxShellTimeout)
	return &bashTool{root: root, defaultTimeout: defaultTimeout, options: options}
}

var _ rig.Tool = (*bashTool)(nil)

// Name implements rig.Tool.
func (t *bashTool) Name() string { return ToolBash }

// Description implements rig.Tool.
func (t *bashTool) Description() string {
	description := "Run a bash command with /bin/sh -c. stdout and stderr are merged and capped at 32 KiB. " +
		"Each call runs in a fresh shell; pass workdir instead of using cd. " +
		"The exit code is appended; a non-zero exit or a timeout is reported as a tool error. " +
		"workdir must be inside the tool root. In policy mode the first command must be allow-listed and " +
		"unquoted shell operators (;, |, &&, $(...), redirects) are rejected unless the policy opts in. " +
		"This tool is not an OS sandbox: the approval gate is the boundary."
	if t.backgroundEnabled() {
		description += " Long commands can run in the background (run_in_background returns a job id immediately; " +
			"collect output with job_output, stop with job_kill); a foreground command that reaches its timeout is " +
			"moved to a background job instead of being killed."
	}
	return description
}

// backgroundEnabled reports whether the tool can start background jobs and
// promote timed-out foreground commands.
func (t *bashTool) backgroundEnabled() bool {
	return t.options.BackgroundJobs != nil
}

// Schema implements rig.Tool.
func (t *bashTool) Schema() map[string]any {
	properties := map[string]any{
		"command":     toolutil.StringProp("Shell command to run with /bin/sh -c."),
		"description": toolutil.StringProp("Optional active-voice description of the command for the UI, 5-10 words. Provide it before command when the wire format allows."),
		"timeout_ms":  toolutil.IntegerProp("Timeout in milliseconds (default 30000, maximum 600000)."),
		"timeoutMs":   toolutil.IntegerProp("DSH alias for timeout_ms."),
		"workdir":     toolutil.StringProp("Working directory relative to the tool root (default: the tool root)."),
	}
	if t.backgroundEnabled() && t.options.EnableRunInBackground {
		properties["run_in_background"] = toolutil.BooleanProp("Run in the background and return a job id immediately (collect with job_output, stop with job_kill). No timeout applies.")
	}
	return toolutil.ObjectSchema(properties, "command")
}

// Invoke implements rig.Tool.
func (t *bashTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if runtime.GOOS == "windows" {
		return toolutil.ErrorResultf("bash: /bin/sh is not available on Windows; use the pwsh tool"), nil
	}
	var args bashArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	if strings.TrimSpace(args.Command) == "" {
		return toolutil.ErrorResultf("bash: command is required"), nil
	}
	if args.TimeoutMS <= 0 {
		args.TimeoutMS = args.TimeoutMSCamel
	}
	if err := checkEscalationArgs("bash", args.SandboxPermissions, args.Justification); err != nil {
		return toolutil.ErrorResult(err), nil
	}
	if args.RunInBackground && !t.backgroundEnabled() {
		return toolutil.ErrorResultf("bash: background jobs unavailable: no background job manager is configured; run the command in the foreground"), nil
	}
	if args.RunInBackground && !t.options.EnableRunInBackground {
		return toolutil.ErrorResultf("bash: run_in_background is disabled for this tool configuration"), nil
	}

	// Policy mode: read-only disables the shell, and the command must pass the
	// shell-syntax policy and the command allow/deny check with a workdir
	// inside the roots. ParseShellCommand is the same parser the policy uses, so
	// validation and argv[0] cannot disagree about quoting or line continuations.
	if t.root.policy != nil {
		if t.root.policy.ReadOnly() {
			return toolutil.ErrorResultf("bash: sandbox policy is read-only; shell commands are disabled"), nil
		}
		argv, parseErr := t.root.policy.ParseShellCommand(args.Command)
		if parseErr != nil {
			return toolutil.ErrorResult(parseErr), nil
		}
		if err := t.root.policy.CheckCommand(argv, args.Workdir); err != nil {
			return toolutil.ErrorResult(err), nil
		}
	}

	effectiveTimeout := t.defaultTimeout
	if args.TimeoutMS > 0 {
		effectiveTimeout = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	if t.root.policy != nil && effectiveTimeout > t.root.policy.Timeout() {
		effectiveTimeout = t.root.policy.Timeout()
	}
	effectiveTimeout = min(effectiveTimeout, MaxShellTimeout)

	var (
		workdir string
		err     error
	)
	if t.root.policy != nil {
		workdir, err = resolvePolicyWorkdir(t.root, args.Workdir)
	} else {
		workdir, err = resolveWorkdir(t.root, args.Workdir)
	}
	if err != nil {
		return toolutil.ErrorResult(err), nil
	}

	outputLimit := MaxShellOutputBytes
	if t.root.policy != nil {
		outputLimit = t.root.policy.MaxOutputBytes()
	}
	argv := []string{"/bin/sh", "-c", args.Command}

	if args.RunInBackground {
		return t.runInBackground(args, argv, workdir, outputLimit)
	}
	if t.backgroundEnabled() && t.options.PromoteOnTimeout {
		if result, handled, runErr := t.runPromotedForeground(ctx, args, argv, workdir, effectiveTimeout, outputLimit); handled {
			return result, runErr
		}
		// Registration failed: fall back to the deadline-kill foreground path.
	}
	return t.runForeground(ctx, args, workdir, effectiveTimeout, outputLimit)
}

// runInBackground starts the command as a managed background job and returns
// its id immediately. No timeout applies to background jobs.
func (t *bashTool) runInBackground(args bashArgs, argv []string, workdir string, outputLimit int) (rig.ToolResult, error) {
	job, err := t.options.BackgroundJobs.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindBash),
		Label:            args.Command,
		Command:          args.Command,
		Argv:             argv,
		Dir:              workdir,
		OutputLimitBytes: outputLimit,
		PolicyChecked:    true,
	})
	if err != nil {
		return toolutil.ErrorResultf("bash: background jobs unavailable: %v", err), nil
	}
	return toolutil.TextResult("started background job " + job.ID()), nil
}

// runPromotedForeground registers a foreground command as a job, waits up to
// timeout and, when the command is still running, hands it to the model as a
// background job. handled is false only when the job could not be registered
// (the caller then falls back to the deadline-kill path).
func (t *bashTool) runPromotedForeground(ctx context.Context, args bashArgs, argv []string, workdir string, timeout time.Duration, outputLimit int) (rig.ToolResult, bool, error) {
	job, err := t.options.BackgroundJobs.StartProcess(background.ProcessSpec{
		Kind:             string(background.KindBash),
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
		// The caller cancelled: stop the command on this call's account and
		// report the cancellation as a Go error, not a model-facing failure.
		job.Cancel("tool call aborted")
		_, _, _ = job.Wait(context.Background(), killWaitGrace)
		_ = job.Drop()
		return rig.ToolResult{}, true, waitErr
	}
	if settled {
		output, settledView := job.Read()
		_ = job.Drop()
		return toolutil.TextResult(renderShellOutput(output, settledView.ExitCode)), true, nil
	}
	output, _ := job.Read()
	return toolutil.TextResult(renderPromoted(output, timeout, job.ID())), true, nil
}

// killWaitGrace bounds how long a canceled foreground call waits for its
// background job to stop before handing control back.
const killWaitGrace = 2 * time.Second

// renderShellOutput renders one settled foreground shell result: its output,
// then the DSH exit marker for a non-zero exit.
func renderShellOutput(output string, exitCode int) string {
	var builder strings.Builder
	builder.WriteString(output)
	if output != "" && !strings.HasSuffix(output, "\n") {
		builder.WriteString("\n")
	}
	if exitCode > 0 {
		fmt.Fprintf(&builder, "[exit code: %d]", exitCode)
	}
	return builder.String()
}

// renderPromoted renders one foreground call that stopped waiting and handed
// its command to the background job: the output so far, the hand-off marker and
// the job guidance.
func renderPromoted(output string, timeout time.Duration, jobID string) string {
	var builder strings.Builder
	builder.WriteString(output)
	if output != "" && !strings.HasSuffix(output, "\n") {
		builder.WriteString("\n")
	}
	fmt.Fprintf(&builder, "[still running after %dms; moved to background job %s]\n", timeout.Milliseconds(), jobID)
	builder.WriteString("The command keeps running in the background. You will be notified when it finishes; " +
		"read newer output with job_output, stop it with job_kill.")
	return builder.String()
}

// runForeground is the deadline-kill foreground path used when background
// execution is unavailable or disabled.
func (t *bashTool) runForeground(ctx context.Context, args bashArgs, workdir string, effectiveTimeout time.Duration, outputLimit int) (rig.ToolResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, effectiveTimeout)
	defer cancel()
	output := &truncatingBuffer{limit: outputLimit}
	command := exec.CommandContext(runCtx, "/bin/sh", "-c", args.Command)
	if t.root.policy != nil {
		command.Env = t.root.policy.CommandEnv(os.Environ())
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
		content += fmt.Sprintf("\n[timeout: command did not finish within %s]", effectiveTimeout)
		return rig.ToolResult{Content: content, IsError: true}, nil
	case runErr != nil:
		// DSH semantics: a non-zero exit is a result for the model to
		// interpret, not a tool error. A timeout above is still an error.
		content += fmt.Sprintf("\n[exit code: %d]", exitCode(runErr))
		return toolutil.TextResult(content), nil
	default:
		return toolutil.TextResult(content), nil
	}
}

// checkEscalationArgs rejects DSH sandbox-escalation arguments. Rig has no
// sandboxing executor that can widen the current mode for one call, so the
// fields are accepted for wire compatibility but fail closed with the same
// guidance DSH gives when no escalation target is composed.
func checkEscalationArgs(tool, sandboxPermissions, justification string) error {
	if strings.TrimSpace(sandboxPermissions) != "" {
		return fmt.Errorf("%s: sandbox_permissions is not available in this composition (no sandboxing executor to escalate)", tool)
	}
	if strings.TrimSpace(justification) != "" {
		return fmt.Errorf("%s: justification requires sandbox_permissions, which is not available in this composition (no sandboxing executor to escalate)", tool)
	}
	return nil
}

// resolveWorkdir validates the optional workdir against the root and returns its
// absolute filesystem path.
func resolveWorkdir(root fsRoot, workdir string) (string, error) {
	if strings.TrimSpace(workdir) == "" {
		return root.dir, nil
	}
	fsys, err := root.open()
	if err != nil {
		return "", err
	}
	defer fsys.Close()
	relative, err := root.rel(workdir)
	if err != nil {
		return "", err
	}
	info, err := fsys.Stat(relative)
	if err != nil {
		return "", fmt.Errorf("bash: workdir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("bash: workdir %q is not a directory", workdir)
	}
	return root.abs(relative), nil
}

// resolvePolicyWorkdir validates the optional workdir through the sandbox
// policy and returns its real absolute path. The directory must exist and be a
// directory inside one of the policy roots.
func resolvePolicyWorkdir(root fsRoot, workdir string) (string, error) {
	requested := strings.TrimSpace(workdir)
	if requested == "" {
		requested = "."
	}
	fsys, relative, abs, _, err := root.policy.OpenPath(requested)
	if err != nil {
		return "", fmt.Errorf("bash: workdir: %w", err)
	}
	defer fsys.Close()
	info, err := fsys.Stat(relative)
	if err != nil {
		return "", fmt.Errorf("bash: workdir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("bash: workdir %q is not a directory", workdir)
	}
	return abs, nil
}

// exitCode is the process exit code of a failed command (-1 when killed).
func exitCode(err error) int {
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitError.ExitCode()
	}
	return -1
}

// truncatingBuffer collects command output up to limit bytes. It is safe for
// concurrent writes because exec copies stdout and stderr on separate
// goroutines when both are not *os.File values.
type truncatingBuffer struct {
	mu        sync.Mutex
	limit     int
	buffer    bytes.Buffer
	truncated bool
}

func (b *truncatingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(p) > remaining {
			b.buffer.Write(p[:remaining])
		} else {
			b.buffer.Write(p)
		}
	}
	if len(p) > remaining {
		b.truncated = true
	}
	return len(p), nil
}

// String returns the captured output plus a truncation notice.
func (b *truncatingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	content := b.buffer.String()
	if b.truncated {
		content += truncationNotice(fmt.Sprintf("output exceeded %d bytes", b.limit))
	}
	return content
}
