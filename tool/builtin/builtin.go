// Package builtin provides ZeoKeel-Rig's built-in tools: the file tools
// (read/write/edit/list/glob/grep) and the shell tool. They are deliberately
// stdlib-only and stateless: every call opens the tool root, so no file handle
// or working directory survives between calls and a tool is safe for
// concurrent use.
//
// # Path safety
//
// Every file tool is bound to a root directory at construction time. Path
// arguments may be relative to the root or absolute paths inside it; anything
// that resolves outside the root is refused, including paths that traverse
// ".." and symbolic links that point outside the root. The implementation uses
// os.Root, which validates every path component (and rejects escaping symlinks)
// at the syscall-adjacent layer rather than trying to re-implement path
// canonicalization. One os.Root rule worth knowing: symlinks must be relative;
// an absolute symlink is refused even when it points inside the root, so the
// tools accept relative links only.
//
// The shell tool is the documented exception: it validates its workdir against
// the root, but a shell command can always use absolute paths or start other
// programs, so a shell cannot be sandboxed with the standard library. Approval
// (contracts/v1.Gate) is the real boundary for shell; the root is a
// convenience and misconfiguration guard, not a security sandbox.
//
// # Output limits
//
// Model-facing text is bounded so one tool call cannot flood the context. The
// limits are exported constants; truncated output ends with a "[truncated: ...]"
// notice that says what was cut.
package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// Built-in tool names. They are stable model-facing identifiers.
const (
	ToolReadFile  = "read_file"
	ToolWriteFile = "write_file"
	ToolEditFile  = "edit_file"
	ToolListDir   = "list_dir"
	ToolGlob      = "glob"
	ToolGrep      = "grep"
	ToolBash      = "bash"
)

// Output limits. Every limit is exported so hosts and tests can assert the
// documented behavior without duplicating the numbers.
const (
	// MaxReadLines is the default and maximum number of lines read_file returns.
	MaxReadLines = 2000
	// MaxReadLineBytes truncates one read_file line (UTF-8 safe).
	MaxReadLineBytes = 2000
	// MaxReadBytes is the hard cap on a read_file body before line filtering.
	MaxReadBytes = 256 << 10
	// MaxDirEntries is the hard cap on list_dir entries.
	MaxDirEntries = 500
	// MaxGlobMatches is the hard cap on glob results.
	MaxGlobMatches = 500
	// MaxGrepMatches is the hard cap on grep matching lines.
	MaxGrepMatches = 200
	// MaxGrepLineBytes truncates one grep line.
	MaxGrepLineBytes = 500
	// MaxGrepFileBytes skips files larger than this in grep (avoids reading
	// binaries and generated blobs).
	MaxGrepFileBytes = 8 << 20
	// MaxEditFileBytes refuses to edit files larger than this.
	MaxEditFileBytes = 8 << 20
	// MaxShellOutputBytes caps bash's combined stdout+stderr.
	MaxShellOutputBytes = 32 << 10
	// DefaultShellTimeout is used when the constructor timeout is zero and the
	// call does not pass timeout_ms.
	DefaultShellTimeout = 30 * time.Second
	// MaxShellTimeout caps one shell call's timeout.
	MaxShellTimeout = 10 * time.Minute
)

// builtinTool is the common v1.Tool implementation of the built-ins.
type builtinTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error)
}

func (t *builtinTool) Name() string           { return t.name }
func (t *builtinTool) Description() string    { return t.description }
func (t *builtinTool) Schema() map[string]any { return t.parameters }
func (t *builtinTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	return t.run(ctx, call)
}

// NewFS returns the six file tools bound to root: read_file, write_file,
// edit_file, list_dir, glob and grep. An empty root means the process's current
// working directory at construction time.
func NewFS(root string) []v1.Tool {
	base := newFSRoot(root)
	return []v1.Tool{
		newReadFile(base),
		newWriteFile(base),
		newEditFile(base),
		newListDir(base),
		newGlob(base),
		newGrep(base),
	}
}

// NewShell returns the bash tool bound to root, with timeout as the default
// per-call timeout (zero or negative means DefaultShellTimeout).
func NewShell(root string, timeout time.Duration) v1.Tool {
	return newBash(newFSRoot(root), timeout)
}

// NewBuiltins returns every built-in tool (NewFS followed by NewShell) in one
// slice, ready for tool.NewRegistry.
func NewBuiltins(root string, timeout time.Duration) []v1.Tool {
	return append(NewFS(root), NewShell(root, timeout))
}

// fsRoot is the validated root of one file-tool family.
type fsRoot struct {
	dir string
}

// newFSRoot resolves root to an absolute path once, at construction time.
func newFSRoot(root string) fsRoot {
	dir := strings.TrimSpace(root)
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return fsRoot{dir: dir}
}

// open opens the root for one call. os.Root confines every operation to the
// root and rejects escaping symlinks.
func (r fsRoot) open() (*os.Root, error) {
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("open tool root %q: %w", r.dir, err)
	}
	return root, nil
}

// rel validates one user path against the root and returns it relative to the
// root, slash-separated (the form os.Root and fs.FS use). A path that escapes
// the root is an error.
func (r fsRoot) rel(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == "." {
		return ".", nil
	}
	if strings.ContainsRune(path, 0) {
		return "", errors.New("path contains a NUL byte")
	}
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) {
		relative, err := filepath.Rel(r.dir, clean)
		if err != nil || escapesRoot(relative) {
			return "", fmt.Errorf("path %q is outside the tool root", path)
		}
		return filepath.ToSlash(relative), nil
	}
	if escapesRoot(clean) {
		return "", fmt.Errorf("path %q is outside the tool root", path)
	}
	return filepath.ToSlash(clean), nil
}

// abs returns the absolute filesystem path of one already-validated relative
// path (used for the shell working directory).
func (r fsRoot) abs(relative string) string {
	return filepath.Join(r.dir, filepath.FromSlash(relative))
}

// escapesRoot reports whether a cleaned relative path points outside the root.
func escapesRoot(relative string) bool {
	if relative == "" || filepath.IsAbs(relative) {
		return true
	}
	if relative == ".." {
		return true
	}
	return strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		strings.HasPrefix(relative, "../")
}

// decodeArgs decodes one call's raw arguments. A missing argument string is an
// empty object, so tools whose arguments are all optional work with "{}" and "".
func decodeArgs(call v1.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return nil
}

// textResult is a successful tool result.
func textResult(content string) v1.ToolResult {
	return v1.ToolResult{Content: content}
}

// errorResult is a model-facing tool failure: the model sees the text and can
// correct its next call.
func errorResult(format string, args ...any) v1.ToolResult {
	return v1.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

// errorResultErr is errorResult with an error value.
func errorResultErr(err error) v1.ToolResult {
	return v1.ToolResult{Content: err.Error(), IsError: true}
}

// truncateLine shortens one line to at most max bytes without splitting a
// UTF-8 rune and marks the cut.
func truncateLine(line string, max int) string {
	if len(line) <= max {
		return line
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "…"
}

// truncationNotice renders the model-facing notice appended to truncated output.
func truncationNotice(detail string) string {
	return "\n[truncated: " + detail + "]"
}

// objectSchema builds the JSON Schema object advertised for one tool.
func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integerProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func booleanProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}
