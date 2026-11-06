// Package editor provides the str_replace_editor tool: a sandboxed file viewer
// and exact-text editor for agents.
//
// The tool exposes four commands through one rig.Tool:
//
//   - view: show a file with 1-based line numbers, optionally limited to an
//     inclusive line range (-1 as the end means end of file);
//   - create: create a new file and fail if it already exists;
//   - str_replace: replace an exact old_str (unique unless replace_all is set)
//     and report the replacement count plus a one-line diff summary;
//   - insert: insert text after a 1-based line number (0 inserts at the start).
//
// Every path is resolved through a sandbox.Policy, so the tool honours allowed
// roots, symlink containment and denied globs. Edits are refused on a read-only
// policy while view keeps working. Edits are exact: no whitespace normalization
// and no regular expressions, and binary or oversized files are refused.
package editor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

const (
	// Name is the model-facing tool name.
	Name = "str_replace_editor"
	// MaxFileBytes is the largest file the editor views or edits (1 MiB).
	// Larger files are refused with a clear error; a dedicated read_file tool
	// is the right choice for paging through big logs.
	MaxFileBytes = 1 << 20
	// DefaultFileMode is the permission used when create makes a new file.
	DefaultFileMode os.FileMode = 0o644
)

// command names accepted by the tool.
const (
	commandView       = "view"
	commandCreate     = "create"
	commandStrReplace = "str_replace"
	commandInsert     = "insert"
)

// New returns the str_replace_editor tool governed by policy. A nil policy
// means sandbox.New(), which denies every path: the tool is useless without a
// root, which is the documented "root sandbox required" contract.
func New(policy *sandbox.Policy) rig.Tool {
	if policy == nil {
		policy = sandbox.New()
	}
	return &tool{policy: policy}
}

// tool is the str_replace_editor implementation. It holds no per-call state and
// is safe for concurrent use; each call opens its own sandbox root handle.
type tool struct {
	policy *sandbox.Policy
}

var _ rig.Tool = (*tool)(nil)

// Name implements rig.Tool.
func (t *tool) Name() string { return Name }

// Description implements rig.Tool.
func (t *tool) Description() string {
	return `View and edit files inside the sandbox root using exact text operations.

Commands:
- view: show a file with line numbers (N<TAB>text). Optional view_range [start, end] is 1-based and inclusive; -1 as the end means end of file. Directories are not listed (use list_dir).
- create: create a new file from file_text. Fails if the path already exists; use str_replace to edit existing files.
- str_replace: replace exact old_str with new_str. old_str must occur exactly once unless replace_all is true; 0 or ambiguous matches are errors. Returns the replacement count and a one-line-context diff.
- insert: insert new_str after line insert_line (1-based); insert_line 0 inserts at the start of the file.

All paths go through the sandbox policy (allowed roots, no "..", symlink containment, denied globs). Edits are refused on a read-only policy; view still works. Files larger than 1 MiB, binary files and directories are refused.`
}

// Schema implements rig.Tool.
func (t *tool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Command to run.",
				"enum":        []any{commandView, commandCreate, commandStrReplace, commandInsert},
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File path relative to a sandbox root (an absolute path inside a root is accepted).",
			},
			"view_range": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"description": "Optional [start, end] 1-based inclusive line range for view; -1 as end means end of file.",
			},
			"file_text": map[string]any{
				"type":        "string",
				"description": "Full content for create.",
			},
			"old_str": map[string]any{
				"type":        "string",
				"description": "Exact text to replace (required for str_replace).",
			},
			"new_str": map[string]any{
				"type":        "string",
				"description": "Replacement text for str_replace (empty deletes) or text to insert for insert.",
			},
			"replace_all": map[string]any{
				"type":        "boolean",
				"description": "Replace every occurrence of old_str instead of requiring a unique match (default false).",
			},
			"insert_line": map[string]any{
				"type":        "integer",
				"description": "Insert new_str AFTER this 1-based line number; 0 inserts at the beginning (required for insert).",
			},
		},
		"required":             []string{"command", "path"},
		"additionalProperties": false,
	}
}

// args is the decoded str_replace_editor argument object.
type args struct {
	Command    string  `json:"command"`
	Path       string  `json:"path"`
	ViewRange  []int   `json:"view_range"`
	FileText   *string `json:"file_text"`
	OldStr     *string `json:"old_str"`
	NewStr     *string `json:"new_str"`
	ReplaceAll bool    `json:"replace_all"`
	InsertLine *int    `json:"insert_line"`
}

// Invoke implements rig.Tool.
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	var a args
	if err := toolutil.DecodeArgs(call, &a); err != nil {
		return rig.ToolResult{}, err
	}
	switch strings.TrimSpace(a.Command) {
	case commandView:
		return t.view(a), nil
	case commandCreate:
		return t.create(a), nil
	case commandStrReplace:
		return t.strReplace(a), nil
	case commandInsert:
		return t.insert(a), nil
	case "":
		return toolutil.ErrorResultf("%s: command is required", Name), nil
	default:
		return toolutil.ErrorResultf("%s: unknown command %q; allowed commands are view, create, str_replace, insert", Name, a.Command), nil
	}
}

// open validates path through the sandbox policy and opens the containing root.
// The caller must close the returned root.
func (t *tool) open(path string) (root *os.Root, relative, display string, err error) {
	display = strings.TrimSpace(path)
	if display == "" {
		return nil, "", "", fmt.Errorf("path is required")
	}
	root, relative, _, _, err = t.policy.OpenPath(display)
	if err != nil {
		return nil, "", "", err
	}
	return root, relative, display, nil
}

// view implements the view command.
func (t *tool) view(a args) rig.ToolResult {
	root, relative, display, err := t.open(a.Path)
	if err != nil {
		return toolutil.ErrorResultf("%s: view %s: %v", Name, strings.TrimSpace(a.Path), err)
	}
	defer root.Close()

	data, err := readTextFile(root, relative, display)
	if err != nil {
		return toolutil.ErrorResultf("%s: view %s: %v", Name, display, err)
	}
	lines := strings.Split(string(data), "\n")
	start, end, err := lineRange(a.ViewRange, len(lines))
	if err != nil {
		return toolutil.ErrorResultf("%s: view %s: %v", Name, display, err)
	}

	var builder strings.Builder
	for line := start; line <= end; line++ {
		fmt.Fprintf(&builder, "%d\t%s\n", line, lines[line-1])
	}
	return toolutil.TextResult(toolutil.TruncateBytes(builder.String(), t.policy.MaxOutputBytes(), fmt.Sprintf("\n[truncated: output exceeded the %d byte sandbox limit; use view_range to inspect smaller sections]", t.policy.MaxOutputBytes())))
}

// create implements the create command.
func (t *tool) create(a args) rig.ToolResult {
	if a.FileText == nil {
		return toolutil.ErrorResultf("%s: file_text is required for command: create", Name)
	}
	if t.policy.ReadOnly() {
		return toolutil.ErrorResultf("%s: sandbox policy is read-only", Name)
	}
	root, relative, display, err := t.open(a.Path)
	if err != nil {
		return toolutil.ErrorResultf("%s: create %s: %v", Name, strings.TrimSpace(a.Path), err)
	}
	defer root.Close()
	if relative == "." {
		return toolutil.ErrorResultf("%s: create %s: path must name a file, not the sandbox root", Name, display)
	}
	if _, err := root.Stat(relative); err == nil {
		return toolutil.ErrorResultf("%s: create %s: file already exists; use str_replace to edit an existing file", Name, display)
	} else if !os.IsNotExist(err) {
		return toolutil.ErrorResultf("%s: create %s: stat: %v", Name, display, err)
	}
	if parent := filepath.Dir(relative); parent != "." && parent != "" {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return toolutil.ErrorResultf("%s: create %s: create parent directories: %v", Name, display, err)
		}
	}
	content := *a.FileText
	if err := root.WriteFile(relative, []byte(content), DefaultFileMode); err != nil {
		return toolutil.ErrorResultf("%s: create %s: %v", Name, display, err)
	}
	return toolutil.TextResult(fmt.Sprintf("created %s (%d bytes)", display, len(content)))
}

// strReplace implements the str_replace command.
func (t *tool) strReplace(a args) rig.ToolResult {
	if a.OldStr == nil || *a.OldStr == "" {
		return toolutil.ErrorResultf("%s: old_str is required and must be non-empty for command: str_replace", Name)
	}
	if t.policy.ReadOnly() {
		return toolutil.ErrorResultf("%s: sandbox policy is read-only", Name)
	}
	root, relative, display, err := t.open(a.Path)
	if err != nil {
		return toolutil.ErrorResultf("%s: str_replace %s: %v", Name, strings.TrimSpace(a.Path), err)
	}
	defer root.Close()

	data, mode, err := readEditableFile(root, relative, display)
	if err != nil {
		return toolutil.ErrorResultf("%s: str_replace %s: %v", Name, display, err)
	}
	content := string(data)
	oldStr := *a.OldStr
	newStr := ""
	if a.NewStr != nil {
		newStr = *a.NewStr
	}
	count := strings.Count(content, oldStr)
	switch {
	case count == 0:
		return toolutil.ErrorResultf("%s: str_replace %s: old_str did not appear verbatim", Name, display)
	case count > 1 && !a.ReplaceAll:
		return toolutil.ErrorResultf("%s: str_replace %s: old_str occurs %d times; set replace_all=true or make old_str unique", Name, display, count)
	}
	replacements := 1
	if a.ReplaceAll {
		replacements = -1
	}
	updated := strings.Replace(content, oldStr, newStr, replacements)
	if err := root.WriteFile(relative, []byte(updated), mode); err != nil {
		return toolutil.ErrorResultf("%s: str_replace %s: %v", Name, display, err)
	}

	// One-line-context diff summary around the first replaced match.
	matchIndex := strings.Index(content, oldStr)
	startLine := 1 + strings.Count(content[:matchIndex], "\n")
	endLine := startLine + strings.Count(newStr, "\n")
	summary := formatDiffSummary(updated, startLine, endLine)
	result := fmt.Sprintf("replaced %d occurrence(s) in %s (first replacement at line %d)", count, display, startLine)
	if summary != "" {
		result += "\n" + summary
	}
	return toolutil.TextResult(toolutil.TruncateBytes(result, t.policy.MaxOutputBytes(), fmt.Sprintf("\n[truncated: output exceeded the %d byte sandbox limit; use view_range to inspect smaller sections]", t.policy.MaxOutputBytes())))
}

// insert implements the insert command.
func (t *tool) insert(a args) rig.ToolResult {
	if a.InsertLine == nil {
		return toolutil.ErrorResultf("%s: insert_line is required for command: insert", Name)
	}
	if a.NewStr == nil {
		return toolutil.ErrorResultf("%s: new_str is required for command: insert", Name)
	}
	if t.policy.ReadOnly() {
		return toolutil.ErrorResultf("%s: sandbox policy is read-only", Name)
	}
	root, relative, display, err := t.open(a.Path)
	if err != nil {
		return toolutil.ErrorResultf("%s: insert %s: %v", Name, strings.TrimSpace(a.Path), err)
	}
	defer root.Close()

	data, mode, err := readEditableFile(root, relative, display)
	if err != nil {
		return toolutil.ErrorResultf("%s: insert %s: %v", Name, display, err)
	}
	lines := strings.Split(string(data), "\n")
	line := *a.InsertLine
	if line < 0 || line > len(lines) {
		return toolutil.ErrorResultf("%s: insert %s: invalid insert_line %d; it must be between 0 and %d", Name, display, line, len(lines))
	}
	inserted := strings.Split(*a.NewStr, "\n")
	combined := make([]string, 0, len(lines)+len(inserted))
	combined = append(combined, lines[:line]...)
	combined = append(combined, inserted...)
	combined = append(combined, lines[line:]...)
	if err := root.WriteFile(relative, []byte(strings.Join(combined, "\n")), mode); err != nil {
		return toolutil.ErrorResultf("%s: insert %s: %v", Name, display, err)
	}

	position := "at the beginning"
	if line > 0 {
		position = fmt.Sprintf("after line %d", line)
	}
	return toolutil.TextResult(fmt.Sprintf("inserted %d line(s) %s in %s (new lines %d-%d)",
		len(inserted), position, display, line+1, line+len(inserted)))
}

// readTextFile reads one regular text file and enforces the editor limits.
func readTextFile(root *os.Root, relative, display string) ([]byte, error) {
	data, _, err := readEditableFile(root, relative, display)
	return data, err
}

// readEditableFile reads one regular UTF-8 text file, returning its content and
// the permission bits used when writing it back.
func readEditableFile(root *os.Root, relative, display string) ([]byte, os.FileMode, error) {
	info, err := root.Stat(relative)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("the path does not exist")
		}
		return nil, 0, fmt.Errorf("stat: %w", err)
	}
	switch {
	case info.IsDir():
		return nil, 0, fmt.Errorf("the path is a directory; view supports files only (use list_dir)")
	case !info.Mode().IsRegular():
		return nil, 0, fmt.Errorf("not a regular file")
	case info.Size() > MaxFileBytes:
		return nil, 0, fmt.Errorf("file is %d bytes, larger than the %d byte limit", info.Size(), MaxFileBytes)
	}
	data, err := root.ReadFile(relative)
	if err != nil {
		return nil, 0, fmt.Errorf("read: %w", err)
	}
	if len(data) > MaxFileBytes {
		return nil, 0, fmt.Errorf("file is %d bytes, larger than the %d byte limit", len(data), MaxFileBytes)
	}
	if !isText(data) {
		return nil, 0, fmt.Errorf("file is not valid UTF-8 text (binary files are not supported)")
	}
	return data, info.Mode().Perm(), nil
}

// lineRange validates view_range and returns inclusive 1-based start and end
// line numbers. An empty range selects the whole file.
func lineRange(viewRange []int, lineCount int) (int, int, error) {
	if viewRange == nil {
		return 1, lineCount, nil
	}
	if len(viewRange) != 2 {
		return 0, 0, fmt.Errorf("invalid view_range: expected [start, end]")
	}
	start, end := viewRange[0], viewRange[1]
	if start < 1 || start > lineCount {
		return 0, 0, fmt.Errorf("invalid view_range [%d, %d]: start must be between 1 and %d", start, end, lineCount)
	}
	if end == -1 {
		end = lineCount
	}
	if end < start {
		return 0, 0, fmt.Errorf("invalid view_range [%d, %d]: end must be greater than or equal to start", start, end)
	}
	if end > lineCount {
		return 0, 0, fmt.Errorf("invalid view_range [%d, %d]: end must be at most %d", start, end, lineCount)
	}
	return start, end, nil
}

// formatDiffSummary renders the changed region with one context line before and
// after it, using the editor's view line-number format ("N<TAB>line").
func formatDiffSummary(updated string, startLine, endLine int) string {
	lines := strings.Split(updated, "\n")
	from := startLine - 1
	if from < 1 {
		from = 1
	}
	to := endLine + 1
	if to > len(lines) {
		to = len(lines)
	}
	if from > to || from > len(lines) {
		return ""
	}
	var builder strings.Builder
	for line := from; line <= to; line++ {
		fmt.Fprintf(&builder, "%d\t%s\n", line, lines[line-1])
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// isText reports whether data is valid UTF-8 without NUL bytes.
func isText(data []byte) bool {
	return bytes.IndexByte(data, 0) < 0 && utf8.Valid(data)
}
