package misc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// workingDirectoryArgs is the working_directory argument object.
//
// A pointer distinguishes "path was omitted" (a query) from an empty string.
type workingDirectoryArgs struct {
	Path *string `json:"path"`
}

// workingDirectoryTool is a session-local current directory confined to a root.
type workingDirectoryTool struct {
	root string // absolute filesystem root

	mu  sync.Mutex
	cwd string // path relative to root, slash-separated; "." is the root
}

// NewWorkingDirectory returns the working_directory tool: query the current
// directory with no arguments, or change it with path.
//
// Rules (explicit because a sandboxed cwd needs them):
//   - the tool starts at its root, reported as ".";
//   - a relative path is resolved against the current directory, an absolute
//     path must resolve inside the root;
//   - the target must already exist and be a directory (missing parents are not
//     created);
//   - ".." may not escape the root, and symlinks that point outside the root are
//     refused (os.Root validation);
//   - the reported path is always root-relative, so no host path leaks to the
//     model.
//
// The tool is safe for concurrent calls; the current directory is updated
// atomically under a mutex.
func NewWorkingDirectory(root string) v1.Tool {
	return &workingDirectoryTool{root: normalizeRoot(root), cwd: "."}
}

// WorkingDirectoryTool is the named constructor of working_directory; it is
// NewWorkingDirectory.
func WorkingDirectoryTool(root string) v1.Tool { return NewWorkingDirectory(root) }

// Name implements v1.Tool.
func (t *workingDirectoryTool) Name() string { return "working_directory" }

// Description implements v1.Tool.
func (t *workingDirectoryTool) Description() string {
	return "Query or change the session's current directory. With no path it returns the current directory " +
		"(relative to the tool root); with a path it switches to that existing directory. The directory is " +
		"confined to the tool root."
}

// Schema implements v1.Tool.
func (t *workingDirectoryTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"path": stringProp("Directory to switch to, relative to the current directory (or absolute inside the " +
			"tool root). Omit it to query the current directory."),
	})
}

// Invoke implements v1.Tool.
func (t *workingDirectoryTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	var args workingDirectoryArgs
	if err := decodeArgs(call, &args); err != nil {
		return v1.ToolResult{}, err
	}
	if args.Path == nil || strings.TrimSpace(*args.Path) == "" {
		return textResult(t.current()), nil
	}
	if err := ctx.Err(); err != nil {
		return v1.ToolResult{}, err
	}
	target, err := t.resolve(*args.Path)
	if err != nil {
		return errorResult("working_directory: %v", err), nil
	}
	t.mu.Lock()
	t.cwd = target
	t.mu.Unlock()
	return textResult(target), nil
}

// current returns the current directory under the lock.
func (t *workingDirectoryTool) current() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cwd
}

// resolve validates and normalizes one path argument against the current
// directory and the root, and requires the result to be an existing directory.
func (t *workingDirectoryTool) resolve(input string) (string, error) {
	input = strings.TrimSpace(input)
	if strings.ContainsRune(input, 0) {
		return "", errors.New("path contains a NUL byte")
	}
	var relative string
	if filepath.IsAbs(input) {
		computed, err := filepath.Rel(t.root, filepath.Clean(input))
		if err != nil || escapesRoot(computed) {
			return "", fmt.Errorf("path %q is outside the tool root", input)
		}
		relative = filepath.ToSlash(computed)
	} else {
		current := t.current()
		// Resolve relative to the current directory. path.Join applies ".."
		// before we check for an escape, so "sub/../.." cannot slip through.
		joined := path.Join(current, filepath.ToSlash(strings.TrimSpace(input)))
		if escapesRoot(joined) {
			return "", fmt.Errorf("path %q is outside the tool root", input)
		}
		relative = joined
	}
	if relative == "" {
		relative = "."
	}
	fsys, err := os.OpenRoot(t.root)
	if err != nil {
		return "", fmt.Errorf("open tool root: %w", err)
	}
	defer fsys.Close()
	info, err := fsys.Stat(relative)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", relative, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", relative)
	}
	return relative, nil
}

// normalizeRoot resolves root once; an empty root means the process's current
// working directory.
func normalizeRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	if absolute, err := filepath.Abs(root); err == nil {
		root = absolute
	}
	return root
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
