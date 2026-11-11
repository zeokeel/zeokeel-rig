package editor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

const (
	// Name 是面向模型的工具名。
	Name = "str_replace_editor"
	// MaxFileBytes 是编辑器允许查看或编辑的最大文件（1 MiB）。更大的文件会
	// 以清晰的错误被拒绝；要分页查看大日志，应使用专门的 read_file 工具。
	MaxFileBytes = 1 << 20
	// DefaultFileMode 是 create 新建文件时使用的权限。
	DefaultFileMode os.FileMode = 0o644
)

// 工具接受的命令名。
const (
	commandView       = "view"
	commandCreate     = "create"
	commandStrReplace = "str_replace"
	commandInsert     = "insert"
)

// New 返回由 policy 治理的 str_replace_editor 工具。nil policy 表示
// sandbox.New()，它会拒绝所有路径：没有根时该工具不可用，这就是文档化的
// “必须提供根沙箱”约定。
func New(policy *sandbox.Policy, opts ...Option) rig.Tool {
	if policy == nil {
		policy = sandbox.New()
	}
	cfg := config{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &tool{policy: policy, cwd: cfg.cwd}
}

// Option 配置 str_replace_editor 的相对路径基准。
type Option func(*config)

type config struct {
	cwd func() string
}

// WithWorkingDirectory 让编辑工具从会话当前目录解析相对路径（由
// misc.WorkingDirectory.Current 提供）；未挂接时相对沙箱根。
func WithWorkingDirectory(current func() string) Option {
	return func(cfg *config) { cfg.cwd = current }
}

// tool 是 str_replace_editor 的实现。它不保存按调用产生的状态，可安全并发
// 使用；每次调用都会打开自己的沙箱根句柄。
type tool struct {
	policy *sandbox.Policy
	cwd    func() string
}

var _ rig.Tool = (*tool)(nil)

// Name 实现 rig.Tool。
func (t *tool) Name() string { return Name }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	return `View and edit files inside the sandbox root using exact text operations.

Commands:
- view: show a file with line numbers (N<TAB>text). Optional view_range [start, end] is 1-based and inclusive; -1 as the end means end of file. Directories are not listed (use list_dir).
- create: create a new file from file_text. Fails if the path already exists; use str_replace to edit existing files.
- str_replace: replace exact old_str with new_str. old_str must occur exactly once unless replace_all is true; 0 or ambiguous matches are errors. Returns the replacement count and a one-line-context diff.
- insert: insert new_str after line insert_line (1-based); insert_line 0 inserts at the start of the file.

All paths go through the sandbox policy (allowed roots, no "..", symlink containment, denied globs). Edits are refused on a read-only policy; view still works. Files larger than 1 MiB, binary files and directories are refused.`
}

// Schema 实现 rig.Tool。
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

// args 是解码后的 str_replace_editor 参数对象。
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

// Invoke 实现 rig.Tool。
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

// open 通过沙箱策略校验 path，并打开包含它的根。调用方必须关闭返回的根。
func (t *tool) open(path string) (root *os.Root, relative, display string, err error) {
	display = strings.TrimSpace(path)
	if display == "" {
		return nil, "", "", fmt.Errorf("path is required")
	}
	if t.cwd != nil && !filepath.IsAbs(display) {
		if cwd := filepath.ToSlash(strings.TrimSpace(t.cwd())); cwd != "" {
			display = pathpkg.Join(cwd, filepath.ToSlash(display))
		}
	}
	root, relative, _, _, err = t.policy.OpenPath(display)
	if err != nil {
		return nil, "", "", err
	}
	return root, relative, display, nil
}

// view 实现 view 命令。
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

// create 实现 create 命令。
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
	} else if !errors.Is(err, fs.ErrNotExist) {
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

// strReplace 实现 str_replace 命令。
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

	// 首次替换位置附近的一行上下文 diff 摘要。
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

// insert 实现 insert 命令。
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

// readTextFile 读取一个普通文本文件，并执行编辑器的各项上限。
func readTextFile(root *os.Root, relative, display string) ([]byte, error) {
	data, _, err := readEditableFile(root, relative, display)
	return data, err
}

// readEditableFile 读取一个普通 UTF-8 文本文件，返回其内容以及写回时使用
// 的权限位。
func readEditableFile(root *os.Root, relative, display string) ([]byte, os.FileMode, error) {
	info, err := root.Stat(relative)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
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

// lineRange 校验 view_range，返回包含端点的 1 基起止行号。空范围表示整个
// 文件。
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

// formatDiffSummary 以编辑器的视图行号格式（"N<TAB>line"）渲染变更区域
// 及其前后各一行上下文。
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

// isText 报告 data 是否为不含 NUL 字节的合法 UTF-8。
func isText(data []byte) bool {
	return bytes.IndexByte(data, 0) < 0 && utf8.Valid(data)
}
