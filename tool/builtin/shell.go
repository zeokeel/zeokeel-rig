package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

type bashArgs struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
	Workdir   string `json:"workdir"`
}

// newBash builds the bash tool: /bin/sh -c, combined stdout+stderr, output
// capped, and a per-call timeout (constructor default, overridable by
// timeout_ms). The working directory is validated against the root; the
// command itself is not sandboxed (documented in the package comment).
func newBash(root fsRoot, timeout time.Duration) v1.Tool {
	defaultTimeout := timeout
	if defaultTimeout <= 0 {
		defaultTimeout = DefaultShellTimeout
	}
	if defaultTimeout > MaxShellTimeout {
		defaultTimeout = MaxShellTimeout
	}
	return &builtinTool{
		name: ToolBash,
		description: "Run a shell command with /bin/sh -c. stdout and stderr are merged and capped at 32 KiB. " +
			"The exit code is appended; a non-zero exit or a timeout is reported as a tool error. " +
			"workdir must be inside the tool root. This tool is not a sandbox: the approval gate is.",
		parameters: objectSchema(map[string]any{
			"command":    stringProp("Shell command to run with /bin/sh -c."),
			"timeout_ms": integerProp("Timeout in milliseconds (default 30000, maximum 600000)."),
			"workdir":    stringProp("Working directory relative to the tool root (default: the tool root)."),
		}, "command"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args bashArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if strings.TrimSpace(args.Command) == "" {
				return errorResult("bash: command is required"), nil
			}
			effectiveTimeout := defaultTimeout
			if args.TimeoutMS > 0 {
				effectiveTimeout = time.Duration(args.TimeoutMS) * time.Millisecond
			}
			if effectiveTimeout > MaxShellTimeout {
				effectiveTimeout = MaxShellTimeout
			}

			workdir, err := resolveWorkdir(root, args.Workdir)
			if err != nil {
				return errorResultErr(err), nil
			}

			runCtx, cancel := context.WithTimeout(ctx, effectiveTimeout)
			defer cancel()
			output := &truncatingBuffer{limit: MaxShellOutputBytes}
			command := exec.CommandContext(runCtx, "/bin/sh", "-c", args.Command)
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
				return v1.ToolResult{}, ctx.Err()
			case runCtx.Err() == context.DeadlineExceeded:
				content += fmt.Sprintf("\n[timeout: command did not finish within %s]", effectiveTimeout)
				return v1.ToolResult{Content: content, IsError: true}, nil
			case runErr != nil:
				content += fmt.Sprintf("\n[exit code: %d]", exitCode(runErr))
				return v1.ToolResult{Content: content, IsError: true}, nil
			default:
				return textResult(content), nil
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

// exitCode is the process exit code of a failed command (-1 when killed).
func exitCode(err error) int {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
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
