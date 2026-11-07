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
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

type bashArgs struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
	Workdir   string `json:"workdir"`
}

// newBash builds the bash tool: /bin/sh -c, combined stdout+stderr, output
// capped, and a per-call timeout (constructor default, overridable by
// timeout_ms). In policy mode the working directory is validated against the
// roots, the command must pass the allow/deny check, and unquoted shell control
// operators are rejected by the policy (CheckShellCommand) so an allow-listed
// command cannot chain a second one. The approval gate remains the real
// boundary (documented in the package comment).
func newBash(root fsRoot, timeout time.Duration) rig.Tool {
	defaultTimeout := timeout
	if root.policy != nil && defaultTimeout <= 0 {
		defaultTimeout = root.policy.Timeout()
	}
	if defaultTimeout <= 0 {
		defaultTimeout = DefaultShellTimeout
	}
	defaultTimeout = min(defaultTimeout, MaxShellTimeout)
	return &builtinTool{
		name: ToolBash,
		description: "Run a shell command with /bin/sh -c. stdout and stderr are merged and capped at 32 KiB. " +
			"The exit code is appended; a non-zero exit or a timeout is reported as a tool error. " +
			"workdir must be inside the tool root. In policy mode the first command must be allow-listed and " +
			"unquoted shell operators (;, |, &&, $(...), redirects) are rejected unless the policy opts in. " +
			"This tool is not an OS sandbox: the approval gate is the boundary.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"command":    toolutil.StringProp("Shell command to run with /bin/sh -c."),
			"timeout_ms": toolutil.IntegerProp("Timeout in milliseconds (default 30000, maximum 600000)."),
			"workdir":    toolutil.StringProp("Working directory relative to the tool root (default: the tool root)."),
		}, "command"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
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

			// Policy mode: read-only disables the shell, and the command must
			// pass the shell-syntax policy and the command allow/deny check
			// with a workdir inside the roots. ParseShellCommand is the same
			// parser the policy uses, so validation and argv[0] cannot disagree
			// about quoting or line continuations.
			if root.policy != nil {
				if root.policy.ReadOnly() {
					return toolutil.ErrorResultf("bash: sandbox policy is read-only; shell commands are disabled"), nil
				}
				argv, parseErr := root.policy.ParseShellCommand(args.Command)
				if parseErr != nil {
					return toolutil.ErrorResult(parseErr), nil
				}
				if err := root.policy.CheckCommand(argv, args.Workdir); err != nil {
					return toolutil.ErrorResult(err), nil
				}
			}

			effectiveTimeout := defaultTimeout
			if args.TimeoutMS > 0 {
				effectiveTimeout = time.Duration(args.TimeoutMS) * time.Millisecond
			}
			if root.policy != nil && effectiveTimeout > root.policy.Timeout() {
				effectiveTimeout = root.policy.Timeout()
			}
			effectiveTimeout = min(effectiveTimeout, MaxShellTimeout)

			var (
				workdir string
				err     error
			)
			if root.policy != nil {
				workdir, err = resolvePolicyWorkdir(root, args.Workdir)
			} else {
				workdir, err = resolveWorkdir(root, args.Workdir)
			}
			if err != nil {
				return toolutil.ErrorResult(err), nil
			}

			outputLimit := MaxShellOutputBytes
			if root.policy != nil {
				outputLimit = root.policy.MaxOutputBytes()
			}
			runCtx, cancel := context.WithTimeout(ctx, effectiveTimeout)
			defer cancel()
			output := &truncatingBuffer{limit: outputLimit}
			command := exec.CommandContext(runCtx, "/bin/sh", "-c", args.Command)
			if root.policy != nil {
				command.Env = root.policy.CommandEnv(os.Environ())
			}
			// A timed-out shell is killed, but a child process may keep the
			// output pipes open (e.g. `sleep`): WaitDelay closes them so the
			// tool returns at the deadline instead of waiting for the child.
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
				content += fmt.Sprintf("\n[exit code: %d]", exitCode(runErr))
				return rig.ToolResult{Content: content, IsError: true}, nil
			default:
				return toolutil.TextResult(content), nil
			}
		},
	}
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
