package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 内置工具名称。它们是稳定的、面向模型的标识符。
const (
	ToolReadFile  = "read_file"
	ToolWriteFile = "write_file"
	ToolEditFile  = "edit_file"
	ToolListDir   = "list_dir"
	ToolGlob      = "glob"
	ToolGrep      = "grep"
	ToolBash      = "bash"
)

// 输出上限。所有上限均导出，便于宿主与测试在不重复数字的前提下断言文档化行为。
const (
	// MaxReadLines 是 read_file 返回行数的默认值兼上限。
	MaxReadLines = 2000
	// MaxReadLineBytes 会截断 read_file 的每一行（UTF-8 安全）。
	MaxReadLineBytes = 2000
	// MaxReadBytes 是 read_file 在按行过滤前的正文硬上限。
	MaxReadBytes = 256 << 10
	// MaxDirEntries 是 list_dir 条目数的硬上限。
	MaxDirEntries = 500
	// MaxGlobMatches 是 glob 结果的硬上限。
	MaxGlobMatches = 500
	// MaxGrepMatches 是 grep 匹配行数的硬上限。
	MaxGrepMatches = 200
	// MaxGrepLineBytes 会截断 grep 的每一行。
	MaxGrepLineBytes = 500
	// MaxGrepFileBytes 使 grep 跳过大于该值的文件（避免读取二进制
	// 文件与生成的大文件）。
	MaxGrepFileBytes = 8 << 20
	// MaxEditFileBytes 拒绝编辑大于该值的文件。
	MaxEditFileBytes = 8 << 20
	// MaxShellOutputBytes 限制 bash 合并后的 stdout+stderr。
	MaxShellOutputBytes = 32 << 10
	// DefaultShellTimeout 在构造参数 timeout 为零且调用未传 timeout_ms
	// 时使用。
	DefaultShellTimeout = 30 * time.Second
	// MaxShellTimeout 限制单次 shell 调用的超时时间。
	MaxShellTimeout = 10 * time.Minute
)

// BackgroundJobs 是 bash 与 pwsh 工具用于登记后台执行的可选生产者侧后台任务
// 能力。*jobs.Manager 实现了该接口；宿主把它传给
// NewShellWithOptions/NewWithPolicyOptions（或设置 PwshConfig.BackgroundJobs）
// 即可启用 run_in_background 与超时提升。
type BackgroundJobs = background.Manager

// Options 配置 bash 与 pwsh 工具共享的可选 DSH 后台执行支持。用 With* 系列
// 辅助函数构建；在已挂接 BackgroundJobs 管理器时，解析后的默认值为
// EnableRunInBackground=true、PromoteOnTimeout=true。
type Options struct {
	// BackgroundJobs 是宿主机提供的后台任务接口。为 nil 时 shell 工具仅支持
	// 前台执行（run_in_background 会被拒绝；前台超时会杀死命令）。
	BackgroundJobs BackgroundJobs
	// EnableRunInBackground 决定是否暴露并接受 run_in_background。
	EnableRunInBackground bool
	// PromoteOnTimeout 让达到超时的前台命令继续以后台任务运行，而不是将其
	// 杀死。
	PromoteOnTimeout bool
}

// Option 配置 bash 与 pwsh 工具的后台执行行为。
type Option func(*Options)

// WithBackgroundJobs 挂接宿主的后台任务管理器。这是宿主启用 DSH 后台执行
// 唯一必须添加的选项。
func WithBackgroundJobs(manager BackgroundJobs) Option {
	return func(options *Options) { options.BackgroundJobs = manager }
}

// WithRunInBackground 启用或禁用 run_in_background 参数（挂接后台管理器
// 时默认启用）。
func WithRunInBackground(enabled bool) Option {
	return func(options *Options) { options.EnableRunInBackground = enabled }
}

// WithPromoteOnTimeout 启用或禁用在超时后把前台命令转入后台任务，而不是
// 将其杀死（默认启用）。
func WithPromoteOnTimeout(enabled bool) Option {
	return func(options *Options) { options.PromoteOnTimeout = enabled }
}

// resolveOptions 在文档化默认值之上依次应用 opts。
func resolveOptions(opts []Option) Options {
	resolved := Options{EnableRunInBackground: true, PromoteOnTimeout: true}
	for _, opt := range opts {
		if opt != nil {
			opt(&resolved)
		}
	}
	return resolved
}

// builtinTool 是内置工具的通用 rig.Tool 实现。
type builtinTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
}

func (t *builtinTool) Name() string           { return t.name }
func (t *builtinTool) Description() string    { return t.description }
func (t *builtinTool) Schema() map[string]any { return t.parameters }
func (t *builtinTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.run(ctx, call)
}

// NewFS 返回绑定到 root 的六个文件工具：read_file、write_file、edit_file、
// list_dir、glob 与 grep。root 为空表示构造时进程的当前工作目录。
func NewFS(root string) []rig.Tool {
	base := newFSRoot(root)
	return []rig.Tool{
		newReadFile(base),
		newWriteFile(base),
		newEditFile(base),
		newListDir(base),
		newGlob(base),
		newGrep(base),
	}
}

// NewShell 返回绑定到 root 的 bash 工具，timeout 作为每次调用的默认超时
// （零或负值表示 DefaultShellTimeout）。这是仅前台执行的构造函数；要挂接
// 后台任务请使用 NewShellWithOptions。
func NewShell(root string, timeout time.Duration) rig.Tool {
	return newBash(newFSRoot(root), timeout, resolveOptions(nil))
}

// NewShellWithOptions 返回绑定到 root 的 bash 工具，并应用给定的后台执行
// 选项。传入 WithBackgroundJobs(manager) 会启用 run_in_background 与
// 超时提升。
func NewShellWithOptions(root string, timeout time.Duration, opts ...Option) rig.Tool {
	return newBash(newFSRoot(root), timeout, resolveOptions(opts))
}

// NewBuiltins 以单个切片返回全部内置工具（先 NewFS 再 NewShell），可直接
// 交给 tool.NewRegistry。
func NewBuiltins(root string, timeout time.Duration) []rig.Tool {
	return append(NewFS(root), NewShell(root, timeout))
}

// NewBuiltinsWithOptions 是带 bash 后台执行选项的 NewBuiltins。
func NewBuiltinsWithOptions(root string, timeout time.Duration, opts ...Option) []rig.Tool {
	return append(NewFS(root), NewShellWithOptions(root, timeout, opts...))
}

// NewWithPolicy 返回由 policy 治理的全部内置工具（六个文件工具加 shell）。
// 它是 NewBuiltins 的策略感知版本：
//
//   - 文件工具通过 Policy.ResolvePath/OpenPath 解析每个路径，因此多根、符号
//     链接约束与拒绝 glob 都会生效；
//   - 当策略为只读时，write_file 与 edit_file 返回工具错误，glob/grep/list_dir
//     会隐藏被拒绝的路径；
//   - bash 使用 Policy.CheckCommand 校验命令，只继承
//     Policy.CommandEnv(os.Environ())，以 Policy.Timeout() 作为每次调用超时的
//     默认值与上限，并按 Policy.MaxOutputBytes() 限制输出；
//   - 只读策略会完全禁用 bash（shell 可以写文件）。
//
// nil policy 表示 sandbox.New()（拒绝一切）。返回的工具可安全并发使用。
func NewWithPolicy(policy *sandbox.Policy) []rig.Tool {
	return NewWithPolicyOptions(policy)
}

// NewWithPolicyOptions 是带 bash 后台执行选项的 NewWithPolicy。组合了 jobs
// 管理器的宿主按如下方式连接二者：
//
//	background := jobs.NewManager(jobs.WithPolicy(policy))
//	tools := builtin.NewWithPolicyOptions(policy, builtin.WithBackgroundJobs(background))
func NewWithPolicyOptions(policy *sandbox.Policy, opts ...Option) []rig.Tool {
	if policy == nil {
		policy = sandbox.New()
	}
	root := newPolicyRoot(policy)
	limit := policy.MaxOutputBytes()
	tools := []rig.Tool{
		limitOutput(newReadFile(root), limit),
		limitOutput(newWriteFile(root), limit),
		limitOutput(newEditFile(root), limit),
		limitOutput(newListDir(root), limit),
		limitOutput(newGlob(root), limit),
		limitOutput(newGrep(root), limit),
		newBash(root, policy.Timeout(), resolveOptions(opts)),
	}
	return tools
}

// limitOutput 包装一个工具，使其面向模型的内容不超过 limit 字节（UTF-8
// 安全，并附带截断提示）。limit <= 0 时不做处理。
func limitOutput(tool rig.Tool, limit int) rig.Tool {
	if limit <= 0 {
		return tool
	}
	return &outputLimitedTool{inner: tool, limit: limit}
}

// outputLimitedTool 按策略输出上限裁剪单次工具结果。
type outputLimitedTool struct {
	inner rig.Tool
	limit int
}

func (t *outputLimitedTool) Name() string           { return t.inner.Name() }
func (t *outputLimitedTool) Description() string    { return t.inner.Description() }
func (t *outputLimitedTool) Schema() map[string]any { return t.inner.Schema() }

func (t *outputLimitedTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	result, err := t.inner.Invoke(ctx, call)
	if err != nil || len(result.Content) <= t.limit {
		return result, err
	}
	result.Content = toolutil.TruncateBytes(result.Content, t.limit, "…") +
		truncationNotice(fmt.Sprintf("output exceeded the sandbox limit of %d bytes", t.limit))
	return result, nil
}

// fsRoot 是一个文件工具族经校验的根。两种模式恰有一种生效：遗留单根模式
// （设置了 dir）或沙箱策略模式（设置了 policy，所有路径由策略解析）。
type fsRoot struct {
	dir    string
	policy *sandbox.Policy
}

// newPolicyRoot 把一个文件工具族绑定到沙箱策略。
func newPolicyRoot(policy *sandbox.Policy) fsRoot {
	return fsRoot{policy: policy}
}

// newFSRoot 在构造时一次性把 root 解析为绝对路径。
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

// open 为单次调用打开根目录。os.Root 会把所有操作限制在根内，并拒绝逃逸
// 的符号链接。
func (r fsRoot) open() (*os.Root, error) {
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return nil, fmt.Errorf("open tool root %q: %w", r.dir, err)
	}
	return root, nil
}

// rel 校验一个用户路径并返回其相对根目录、以斜杠分隔的形式（os.Root 与
// fs.FS 使用的形式）。逃出根目录的路径会报错。
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

// abs 返回一个已校验相对路径的绝对文件系统路径（用于 shell 工作目录）。
func (r fsRoot) abs(relative string) string {
	return filepath.Join(r.dir, filepath.FromSlash(relative))
}

// openFor 解析一个用户路径参数并打开包含它的根。策略模式下路径经由
// Policy.OpenPath（多根、符号链接约束、拒绝 glob）；遗留模式保留 v0.2 的
// open()+rel() 行为。调用方必须 Close 返回的 *os.Root。rootDir 是所属根的
// 真实绝对路径，用于在目录遍历与列表中过滤被拒绝的路径。
func (r fsRoot) openFor(requested string) (opened *os.Root, relative, rootDir string, err error) {
	if r.policy != nil {
		opened, relative, _, rootDir, err = r.policy.OpenPath(requested)
		return opened, relative, rootDir, err
	}
	opened, err = r.open()
	if err != nil {
		return nil, "", "", err
	}
	relative, err = r.rel(requested)
	if err != nil {
		opened.Close()
		return nil, "", "", err
	}
	return opened, relative, r.dir, nil
}

// readOnly 报告绑定的策略是否禁止写入。
func (r fsRoot) readOnly() bool {
	return r.policy != nil && r.policy.ReadOnly()
}

// denied 报告目录遍历发现的绝对路径是否会被策略拒绝。遗留根不拒绝任何
// 路径。
func (r fsRoot) denied(abs string) bool {
	return r.policy != nil && r.policy.DeniedPath(abs)
}

// absolutePath 把遍历结果（相对于 rootDir）与其根拼接起来。
func absolutePath(rootDir, slashRelative string) string {
	return filepath.Join(rootDir, filepath.FromSlash(slashRelative))
}

// escapesRoot 报告清理后的相对路径是否指向根之外。
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

// truncationNotice 生成追加到截断输出后的面向模型提示。
func truncationNotice(detail string) string {
	return "\n[truncated: " + detail + "]"
}
