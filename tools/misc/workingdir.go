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

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// workingDirectoryArgs 是 working_directory 的参数对象。
//
// 使用指针区分“省略 path”（查询）与空字符串。
type workingDirectoryArgs struct {
	Path *string `json:"path"`
}

// workingDirectoryTool 是被限制在根目录内的会话本地当前目录。
type workingDirectoryTool struct {
	root string // 绝对文件系统根目录

	mu  sync.Mutex
	cwd string // 相对根目录的路径，以斜杠分隔；"." 表示根目录
}

// NewWorkingDirectory 返回 working_directory 工具：无参数时查询当前目录，给出 path
// 时切换目录。
//
// 规则（沙箱内的 cwd 需要明确）：
//   - 工具从根目录开始，报告为 "."；
//   - 相对路径按当前目录解析，绝对路径必须解析到根目录内；
//   - 目标必须已存在且是目录（不创建缺失的父目录）；
//   - ".." 不得逃逸根目录，指向根目录外的符号链接会被拒绝（os.Root 校验）；
//   - 报告的路径始终相对根目录，不向模型泄漏宿主路径。
//
// 工具可安全并发调用；当前目录在互斥锁保护下原子更新。
func NewWorkingDirectory(root string) rig.Tool {
	return &workingDirectoryTool{root: normalizeRoot(root), cwd: "."}
}

// WorkingDirectoryTool 是 working_directory 的具名构造器，等同于 NewWorkingDirectory。
func WorkingDirectoryTool(root string) rig.Tool { return NewWorkingDirectory(root) }

// Name 实现 rig.Tool。
func (t *workingDirectoryTool) Name() string { return "working_directory" }

// Description 实现 rig.Tool。
func (t *workingDirectoryTool) Description() string {
	return "Query or change the session's current directory. With no path it returns the current directory " +
		"(relative to the tool root); with a path it switches to that existing directory. The directory is " +
		"confined to the tool root."
}

// Schema 实现 rig.Tool。
func (t *workingDirectoryTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"path": toolutil.StringProp("Directory to switch to, relative to the current directory (or absolute inside the " +
			"tool root). Omit it to query the current directory."),
	})
}

// Invoke 实现 rig.Tool。
func (t *workingDirectoryTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args workingDirectoryArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	if args.Path == nil || strings.TrimSpace(*args.Path) == "" {
		return toolutil.TextResult(t.current()), nil
	}
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	target, err := t.resolve(*args.Path)
	if err != nil {
		return toolutil.ErrorResultf("working_directory: %v", err), nil
	}
	t.mu.Lock()
	t.cwd = target
	t.mu.Unlock()
	return toolutil.TextResult(target), nil
}

// current 在持锁状态下返回当前目录。
func (t *workingDirectoryTool) current() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cwd
}

// resolve 按当前目录与根目录校验并规范化一个 path 参数，并要求结果是一个已存在的
// 目录。
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
		// 相对当前目录解析。path.Join 会先处理 ".." 再做逃逸检查，因此 "sub/../.." 无法
		// 绕过检查。
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

// normalizeRoot 只解析一次 root；空 root 表示进程当前工作目录。
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

// escapesRoot 判断清理后的相对路径是否指向根目录之外。
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
