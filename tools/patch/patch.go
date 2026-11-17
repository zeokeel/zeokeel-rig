package patch

import (
	"context"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName 是面向模型的工具名。
const ToolName = "apply_patch"

// DefaultMaxPatchBytes 是补丁文本的默认上限（1 MiB）。超过上限的补丁在解析前
// 就被拒绝：一次补丁能改多少内容应当由模型的分段策略决定，而不是由内存上限决定。
const DefaultMaxPatchBytes = 1 << 20

// DefaultMaxFileBytes 是被修改文件的默认读取上限（16 MiB）。补丁要先在内存里算出
// 新内容才能保证原子性，因此目标文件必须整体读入；上限把"误对一个 GB 级日志文件
// 打补丁"挡在整读之前，而不是让进程吃满内存。
const DefaultMaxFileBytes int64 = 16 << 20

// DefaultMaxFiles 是单个补丁能触及的文件数上限（200）。补丁文本可以很小却写很多
// 文件（每行一个单行文件），而每个文件都要一次暂存 + 一次 rename：上限把"一次
// 补丁上千次落盘"挡在外面。
const DefaultMaxFiles = 200

// option 保存 apply_patch 的构造选项。
type option struct {
	dryRun        bool
	maxPatchBytes int
	maxFileBytes  int64
	maxFiles      int
	policy        *sandbox.Policy
}

// Option 配置 apply_patch 的行为。
type Option func(*option)

// WithDryRun 设置默认的 dry-run 行为：为 true 时解析、校验与匹配都照常执行，
// 但不写任何文件。单次调用里的 dry_run 参数优先于本选项。
func WithDryRun(dry bool) Option {
	return func(o *option) { o.dryRun = dry }
}

// WithPolicy 让工具遵守沙箱策略：只读模式下拒绝任何改动，并且逐个校验写入目标
// 是否命中策略的拒绝 glob（例如 **/.env）或保护区（ProtectedPaths）。
//
// 传 nil 表示不接策略（保持工具自带的"只在 workspace root 内"围栏）。宿主应当
// 传入与其它文件工具相同的 *sandbox.Policy，否则 apply_patch 会成为绕过只读模式
// 与 deny 规则的写入口。
func WithPolicy(policy *sandbox.Policy) Option {
	return func(o *option) { o.policy = policy }
}

// WithMaxFileBytes 设置被修改文件的读取上限（默认 DefaultMaxFileBytes，16 MiB）。
// 超过上限的目标文件会被拒绝（提示改用 write_file 重写），绝不整读。非正值使用
// 默认值。
func WithMaxFileBytes(n int64) Option {
	return func(o *option) {
		if n > 0 {
			o.maxFileBytes = n
		}
	}
}

// WithMaxFiles 设置单个补丁能触及的文件数上限（默认 DefaultMaxFiles，200）。
// 超过上限的补丁整体拒绝（"split it"），一个文件都不改。非正值使用默认值。
func WithMaxFiles(n int) Option {
	return func(o *option) {
		if n > 0 {
			o.maxFiles = n
		}
	}
}

// WithMaxPatchBytes 设置补丁文本的字节上限；非正值恢复默认值
// （DefaultMaxPatchBytes）。
func WithMaxPatchBytes(n int) Option {
	return func(o *option) {
		if n > 0 {
			o.maxPatchBytes = n
		}
	}
}

// tool 是 apply_patch 的实现。它不保存按调用产生的状态，可安全并发使用。
type tool struct {
	root          string
	dryRun        bool
	maxPatchBytes int
	maxFileBytes  int64
	maxFiles      int
	// policy 是可选的沙箱策略；nil 表示只做工具自带的根校验。
	policy *sandbox.Policy
}

var _ rig.Tool = (*tool)(nil)

// New 返回作用于 root 的 apply_patch 工具。
//
// root 是工作区根：补丁里所有路径都相对它解析，且最终必须落在它里面。root 的
// 校验（存在、是目录）在调用时进行，因此构造不会失败，也不会因为 root 暂时不
// 存在而 panic。
func New(root string, opts ...Option) rig.Tool {
	cfg := option{dryRun: false, maxPatchBytes: DefaultMaxPatchBytes, maxFileBytes: DefaultMaxFileBytes, maxFiles: DefaultMaxFiles}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &tool{
		root:          root,
		dryRun:        cfg.dryRun,
		maxPatchBytes: cfg.maxPatchBytes,
		maxFileBytes:  cfg.maxFileBytes,
		maxFiles:      cfg.maxFiles,
		policy:        cfg.policy,
	}
}

// Name 实现 rig.Tool。
func (t *tool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	return `Apply a multi-file patch atomically. Use this instead of several single-file edits when a change spans files or moves/creates/deletes files.

Two patch formats are accepted:

1) Unified diff: "--- a/path" / "+++ b/path" / "@@ -oldStart,oldCount +newStart,newCount @@" with space (context), "-" (removed) and "+" (added) lines. Multiple files are allowed; "--- /dev/null" adds a file and "+++ /dev/null" deletes one. Leading "a/" and "b/" prefixes are stripped.

2) Codex patch: "*** Begin Patch" ... "*** End Patch" with "*** Add File: path" (all lines prefixed "+"), "*** Update File: path" followed by optional "@@ <context anchor>" and change lines, "*** Move to: path" (renames the file being updated), and "*** Delete File: path".

Semantics: the whole patch is parsed, validated and resolved in memory first; if anything fails (syntax, unsafe path, missing or ambiguous context, binary file, duplicate path, size cap) no file is modified at all. Context lines match exactly first, then ignoring trailing whitespace, then ignoring all whitespace; a context that matches more than once is rejected as ambiguous instead of guessed. File modes are preserved (new files are 0644), CRLF line endings are preserved, and lines the patch does not touch are left byte for byte.

Set dry_run to validate a patch and see the +N/-M summary without writing anything.

Prefer edit_file for small single-file edits; do not retry an insert-only hunk (line-number based) — it is not idempotent.`
}

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"patch": toolutil.StringProp("Patch text in unified diff or Codex (*** Begin Patch) format. Paths are relative to the workspace root."),
		"dry_run": toolutil.BooleanProp(
			"When true, parse and validate the patch and report the resulting +N/-M summary without writing any file. Defaults to the tool's configured dry-run mode (false unless the host enabled it)."),
	}, "patch")
}

// args 是解码后的 apply_patch 参数对象。
type args struct {
	Patch  string `json:"patch"`
	DryRun *bool  `json:"dry_run"`
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
	if strings.TrimSpace(a.Patch) == "" {
		return toolutil.ErrorResultf("%s: patch is required and must not be empty", ToolName), nil
	}
	if limit := t.effectiveLimit(); limit > 0 && len(a.Patch) > limit {
		return toolutil.ErrorResultf("%s: patch is %d bytes, which exceeds the %d byte limit (max_patch_bytes); split it into smaller patches", ToolName, len(a.Patch), limit), nil
	}
	dryRun := t.dryRun
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}

	root, err := newRootFS(t.root)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolName, err), nil
	}
	defer root.Close()
	ops, err := parsePatch(a.Patch)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolName, err), nil
	}
	plans, err := buildPlans(root, limits{maxFileBytes: t.effectiveMaxFileBytes(), maxFiles: t.effectiveMaxFiles()}, t.policy, ops)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolName, err), nil
	}
	if err := commit(root, plans, dryRun); err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolName, err), nil
	}
	return toolutil.TextResult(summarize(plans, dryRun)), nil
}

// effectiveLimit 返回生效的补丁大小上限；非正值视为不设上限。
func (t *tool) effectiveLimit() int {
	if t.maxPatchBytes > 0 {
		return t.maxPatchBytes
	}
	return DefaultMaxPatchBytes
}

// effectiveMaxFileBytes 返回生效的单文件读取上限（非正值回落到默认值）。
func (t *tool) effectiveMaxFileBytes() int64 {
	if t.maxFileBytes > 0 {
		return t.maxFileBytes
	}
	return DefaultMaxFileBytes
}

// effectiveMaxFiles 返回生效的单补丁文件数上限（非正值回落到默认值）。
func (t *tool) effectiveMaxFiles() int {
	if t.maxFiles > 0 {
		return t.maxFiles
	}
	return DefaultMaxFiles
}
