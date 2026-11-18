package gate

// 本文件提供"先给我看 diff"的审批文案：把一次**写类工具**调用翻译成"将会发生
// 什么"的可读预览，供 AskGate 的提问渲染。
//
// # 为什么是 prompt 而不是 gate
//
// 门禁（rig.Gate）在工具执行**之前**运行，拿不到工具结果，因此"先执行再给看
// 结果"的链路在这里做不到。审批界面真正需要的是"执行**前**的预告"——这次调用会
// 改哪个文件、改成什么样。所以本文件提供的是**预览渲染器**，而不是一个新的门禁：
//
//	hostGate := gate.AllowThenAsk(
//		gate.SandboxReadOnlyTools,
//		gate.Ask(host.Asker,
//			gate.WithAskTimeout(10*time.Minute),
//			gate.WithDiffPrompt(gate.WithDiffRoot(workspaceRoot)),
//		),
//	)
//
// 对写类工具它返回"标题 + 每个文件的 unified diff + 汇总"，对其它工具返回空串，
// 于是 AskGate 回退到默认文案（"这个工具带这些参数"）。两个选项可以叠加，但同一个
// 字段只有一个值：最后一次设置生效（WithDiffPrompt 内部就是 WithAskPrompt）。
//
// # 覆盖哪些工具
//
//   - write_file：目标路径 + 新旧内容 diff（旧文件存在时）；新文件显示为新建。
//   - edit_file：把 old_string→new_string 就地套用后 diff；上下文不匹配/不唯一时
//     明确告诉审批人"这次编辑大概会失败"。
//   - str_replace_editor：线协议形态的 create / str_replace / insert。
//   - apply_patch：直接调用 tools/patch 的只读预览（parsePatch + buildPlans），
//     **不落盘**；因此预览与真正执行给出的结论必然一致。
//
// # 有界
//
// diff 超过 WithDiffMaxLines（默认 200 行）时只显示汇总与逐文件 +N/-M；单侧内容
// 超过 diffpreview.MaxBytes（1 MiB）或含 NUL 时由 diffpreview 降级成字节数摘要。
// 审批弹窗永远不该因为预览而卡住。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/internal/diffpreview"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// DefaultDiffMaxLines 是审批预览里最多展示的 diff 行数（超过只给摘要）。
const DefaultDiffMaxLines = 200

// maxPatchFallbackBytes 是"补丁无法解析"时附带的原始补丁文本上限：审批人需要看到
// 一点原文，但不该被 1 MiB 的补丁撑爆 UI。
const maxPatchFallbackBytes = 4000

// 本包认识的写类工具名（稳定接口，与 tools/builtin 和 tools/patch 保持一致）。
const (
	// ToolWriteFile 是覆盖式写文件的工具名。
	ToolWriteFile = "write_file"
	// ToolEditFile 是精确替换的工具名。
	ToolEditFile = "edit_file"
	// ToolStrReplaceEditor 风格的编辑工具名。
	ToolStrReplaceEditor = "str_replace_editor"
	// ToolApplyPatch 是多文件补丁工具名。
	ToolApplyPatch = "apply_patch"
)

// diffPromptConfig 是 DiffPrompt 的配置。
type diffPromptConfig struct {
	root     string
	maxLines int
	policy   *sandbox.Policy
	readFile func(rel string) ([]byte, error)
}

// DiffPromptOption 配置 DiffPrompt。
type DiffPromptOption func(*diffPromptConfig)

// WithDiffRoot 设置工作区根：用于读取"变更前内容"，以及 apply_patch 预览的根。
//
// 不设置时：write_file 一律按"新建文件"预览，edit_file 无法预览（会明说），
// apply_patch 无法预览（会明说）。
func WithDiffRoot(root string) DiffPromptOption {
	return func(c *diffPromptConfig) { c.root = strings.TrimSpace(root) }
}

// WithDiffMaxLines 设置展示阈值：diff 总行数超过它时只显示摘要。<=0 用默认值。
func WithDiffMaxLines(lines int) DiffPromptOption {
	return func(c *diffPromptConfig) {
		if lines > 0 {
			c.maxLines = lines
		}
	}
}

// WithDiffPolicy 把沙箱策略传给 apply_patch 预览（只读模式、保护区路径等在预览
// 阶段同样生效）。
func WithDiffPolicy(policy *sandbox.Policy) DiffPromptOption {
	return func(c *diffPromptConfig) { c.policy = policy }
}

// WithDiffReadFile 替换"读取变更前内容"的实现（相对工作区根的斜杠路径）。
//
// 面向远程/虚拟工作区：宿主可以注入自己的读取逻辑。fn 返回错误时预览按"文件不存在"
// 处理。
func WithDiffReadFile(fn func(rel string) ([]byte, error)) DiffPromptOption {
	return func(c *diffPromptConfig) {
		if fn != nil {
			c.readFile = fn
		}
	}
}

// WithDiffPrompt 返回可直接传给 Ask 的选项：把写类工具的审批问题渲染成 diff 预览。
//
//	g := gate.Ask(host.Asker, gate.WithDiffPrompt(gate.WithDiffRoot(root)))
//
// 它等价于 WithAskPrompt(DiffPrompt(opts...))，因此与其它 prompt 选项互斥（后设
// 置的生效）。
func WithDiffPrompt(opts ...DiffPromptOption) AskOption {
	prompt := DiffPrompt(opts...)
	return func(gate *AskGate) { gate.prompt = prompt }
}

// DiffPrompt 返回一个审批文案构造器：对写类工具给出 diff 预览，对其余工具返回
// 空串（调用方回退到默认文案）。
//
// 返回的函数无状态、可并发调用；它只读文件，不修改任何东西。
func DiffPrompt(opts ...DiffPromptOption) func(tool rig.Tool, call rig.ToolCall) string {
	config := &diffPromptConfig{maxLines: DefaultDiffMaxLines}
	for _, opt := range opts {
		if opt != nil {
			opt(config)
		}
	}
	return func(tool rig.Tool, call rig.ToolCall) string {
		return config.prompt(tool, call)
	}
}

// prompt 按工具名分派。
func (c *diffPromptConfig) prompt(tool rig.Tool, call rig.ToolCall) string {
	switch toolCallName(tool, call) {
	case ToolWriteFile:
		return c.writeFilePrompt(call)
	case ToolEditFile:
		return c.editFilePrompt(call)
	case ToolStrReplaceEditor:
		return c.strReplaceEditorPrompt(call)
	case ToolApplyPatch:
		return c.applyPatchPrompt(call)
	default:
		return ""
	}
}

// writeFileArgs 是 write_file 的参数。
type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// editFileArgs 是 edit_file 的参数。
type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// strReplaceArgs 是 str_replace_editor（线协议形态）的参数。
type strReplaceArgs struct {
	Command    string `json:"command"`
	Path       string `json:"path"`
	FileText   string `json:"file_text"`
	OldStr     string `json:"old_str"`
	NewStr     string `json:"new_str"`
	InsertLine int    `json:"insert_line"`
}

// patchArgs 是 apply_patch 的参数。
type patchArgs struct {
	Patch string `json:"patch"`
}

// writeFilePrompt 渲染 write_file 的预览。
func (c *diffPromptConfig) writeFilePrompt(call rig.ToolCall) string {
	var args writeFileArgs
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return ""
	}
	path := normalizeRel(args.Path)
	if path == "" {
		return ""
	}
	content := []byte(args.Content)
	old, existed := c.readOld(path)
	change := diffpreview.Change{Path: path, Op: diffpreview.OpModify, Old: old, New: content}
	title := ""
	if existed {
		change.Op = diffpreview.OpModify
		title = fmt.Sprintf("%s: %s (overwrite: %d lines → %d lines)",
			ToolWriteFile, path, diffpreview.LineCount(old), diffpreview.LineCount(content))
	} else {
		change.Op = diffpreview.OpAdd
		title = fmt.Sprintf("%s: %s (create: %d lines)",
			ToolWriteFile, path, diffpreview.LineCount(content))
	}
	return c.renderChanges(title, []diffpreview.Change{change})
}

// editFilePrompt 渲染 edit_file 的预览（就地套用替换后 diff）。
func (c *diffPromptConfig) editFilePrompt(call rig.ToolCall) string {
	var args editFileArgs
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return ""
	}
	path := normalizeRel(args.Path)
	if path == "" || args.OldString == "" {
		return ""
	}
	old, existed := c.readOld(path)
	if !existed {
		return fmt.Sprintf("%s: %s\n\npreview unavailable: the file does not exist (the tool will fail)\n",
			ToolEditFile, path)
	}
	updated, occurrences, err := applyTextEdit(old, args.OldString, args.NewString, args.ReplaceAll)
	if err != nil {
		return fmt.Sprintf("%s: %s\n\npreview unavailable: %v (the tool will fail)\n",
			ToolEditFile, path, err)
	}
	title := fmt.Sprintf("%s: %s (replace %s)", ToolEditFile, path, plural(occurrences, "occurrence"))
	change := diffpreview.Change{Path: path, Op: diffpreview.OpModify, Old: old, New: updated}
	return c.renderChanges(title, []diffpreview.Change{change})
}

// strReplaceEditorPrompt 渲染 str_replace_editor 的预览。
func (c *diffPromptConfig) strReplaceEditorPrompt(call rig.ToolCall) string {
	var args strReplaceArgs
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return ""
	}
	path := normalizeRel(args.Path)
	if path == "" {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(args.Command)) {
	case "create":
		content := []byte(args.FileText)
		title := fmt.Sprintf("%s create: %s (%d lines)", ToolStrReplaceEditor, path, diffpreview.LineCount(content))
		change := diffpreview.Change{Path: path, Op: diffpreview.OpAdd, New: content}
		return c.renderChanges(title, []diffpreview.Change{change})
	case "str_replace":
		if args.OldStr == "" {
			return ""
		}
		old, existed := c.readOld(path)
		if !existed {
			return fmt.Sprintf("%s str_replace: %s\n\npreview unavailable: the file does not exist (the tool will fail)\n",
				ToolStrReplaceEditor, path)
		}
		updated, occurrences, err := applyTextEdit(old, args.OldStr, args.NewStr, false)
		if err != nil {
			return fmt.Sprintf("%s str_replace: %s\n\npreview unavailable: %v (the tool will fail)\n",
				ToolStrReplaceEditor, path, err)
		}
		title := fmt.Sprintf("%s str_replace: %s (replace %s)", ToolStrReplaceEditor, path, plural(occurrences, "occurrence"))
		change := diffpreview.Change{Path: path, Op: diffpreview.OpModify, Old: old, New: updated}
		return c.renderChanges(title, []diffpreview.Change{change})
	case "insert":
		old, existed := c.readOld(path)
		if !existed {
			return fmt.Sprintf("%s insert: %s\n\npreview unavailable: the file does not exist (the tool will fail)\n",
				ToolStrReplaceEditor, path)
		}
		updated, err := insertAtLine(old, args.NewStr, args.InsertLine)
		if err != nil {
			return fmt.Sprintf("%s insert: %s\n\npreview unavailable: %v\n", ToolStrReplaceEditor, path, err)
		}
		title := fmt.Sprintf("%s insert: %s (after line %d)", ToolStrReplaceEditor, path, args.InsertLine)
		change := diffpreview.Change{Path: path, Op: diffpreview.OpModify, Old: old, New: updated}
		return c.renderChanges(title, []diffpreview.Change{change})
	default:
		// view 等只读命令不需要 diff 预览：回退到默认文案。
		return ""
	}
}

// applyPatchPrompt 渲染 apply_patch 的预览：直接复用 tools/patch 的只读入口。
func (c *diffPromptConfig) applyPatchPrompt(call rig.ToolCall) string {
	var args patchArgs
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return ""
	}
	if strings.TrimSpace(args.Patch) == "" {
		return ""
	}
	if c.root == "" {
		return fmt.Sprintf("%s\n\npreview unavailable: no workspace root was configured (gate.WithDiffRoot)\n",
			ToolApplyPatch)
	}
	preview, err := patch.PreviewChanges(c.root, args.Patch, c.policy)
	if err != nil {
		return fmt.Sprintf("%s\n\npreview unavailable: %v\n\n%s\n",
			ToolApplyPatch, err, fencePatch(args.Patch))
	}
	changes := make([]diffpreview.Change, 0, len(preview))
	for _, change := range preview {
		converted := diffpreview.Change{
			Path:    change.Path,
			Op:      change.Op,
			Old:     change.Old,
			New:     change.New,
			Added:   change.Added,
			Removed: change.Removed,
		}
		changes = append(changes, converted)
	}
	// 标题只说"有几个文件"，+N/-M 由末尾的汇总行给出（避免同一句话出现两次）。
	title := fmt.Sprintf("%s: %s", ToolApplyPatch, plural(len(changes), "file"))
	return c.renderChanges(title, changes)
}

// renderChanges 是公共渲染：标题 + 每个文件的 diff + 汇总；超过阈值时只给摘要。
func (c *diffPromptConfig) renderChanges(title string, changes []diffpreview.Change) string {
	var builder strings.Builder
	builder.WriteString(title)
	builder.WriteString("\n")

	limit := c.maxLines
	if limit <= 0 {
		limit = DefaultDiffMaxLines
	}
	diffTexts := make([]string, 0, len(changes))
	totalLines := 0
	for _, change := range changes {
		text, err := diffpreview.PreviewChange(change)
		if err != nil || text == "" {
			continue
		}
		diffTexts = append(diffTexts, text)
		totalLines += strings.Count(text, "\n")
	}

	if len(changes) == 0 {
		builder.WriteString("\n(no file changes)\n")
		return builder.String()
	}
	if totalLines > limit {
		fmt.Fprintf(&builder, "\ndiff omitted: %d lines exceeds the %d line limit\n\n", totalLines, limit)
		builder.WriteString(diffpreview.Summary(changes))
		builder.WriteString("\n")
		for _, change := range changes {
			added, removed := diffpreview.Stats(change)
			fmt.Fprintf(&builder, "  %-6s %s (+%d -%d)\n", change.Op, change.Path, added, removed)
		}
		builder.WriteString("\nAllow this change?")
		return builder.String()
	}

	for _, text := range diffTexts {
		builder.WriteString("\n")
		builder.WriteString(text)
	}
	builder.WriteString("\n")
	builder.WriteString(diffpreview.Summary(changes))
	builder.WriteString("\n\nAllow this change?")
	return builder.String()
}

// readOld 读取变更前内容；读取失败（不存在/越界/无权限）时返回 (nil, false)，
// 预览据此按"新建文件"处理。
func (c *diffPromptConfig) readOld(rel string) ([]byte, bool) {
	if c.readFile != nil {
		data, err := c.readFile(rel)
		if err != nil {
			return nil, false
		}
		return data, true
	}
	if c.root == "" {
		return nil, false
	}
	root, err := filepath.Abs(c.root)
	if err != nil {
		return nil, false
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if !withinRootPath(root, path) {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

// withinRootPath 报告 path 是否等于 root 或位于其下。
func withinRootPath(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if relative == "." {
		return true
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// normalizeRel 规范化一次展示用的相对路径：去空白、统一斜杠、去掉 "./" 前缀。
func normalizeRel(path string) string {
	path = strings.TrimSpace(path)
	path = strings.ReplaceAll(path, "\\", "/")
	for strings.HasPrefix(path, "./") {
		path = path[2:]
	}
	return path
}

// applyTextEdit 按 edit_file 的语义就地应用一次替换。
//
// 语义与工具一致：oldString 必须出现且唯一（replaceAll 为真时除外）；出现 0 次或
// 多次时返回错误——预览要提前告诉审批人"这一改会失败"。
func applyTextEdit(old []byte, oldString, newString string, replaceAll bool) ([]byte, int, error) {
	if oldString == "" {
		return nil, 0, fmt.Errorf("old_string must not be empty")
	}
	content := string(old)
	occurrences := strings.Count(content, oldString)
	switch {
	case occurrences == 0:
		return nil, 0, fmt.Errorf("old_string does not occur in the file")
	case occurrences > 1 && !replaceAll:
		return nil, occurrences, fmt.Errorf("old_string occurs %d times; the tool requires a unique match unless replace_all is true", occurrences)
	case replaceAll:
		return []byte(strings.ReplaceAll(content, oldString, newString)), occurrences, nil
	default:
		return []byte(strings.Replace(content, oldString, newString, 1)), occurrences, nil
	}
}

// insertAtLine 在指定行之后插入文本（str_replace_editor 的 insert 命令）。
func insertAtLine(old []byte, text string, afterLine int) ([]byte, error) {
	if afterLine < 0 {
		return nil, fmt.Errorf("insert_line must not be negative")
	}
	lines := strings.Split(string(old), "\n")
	if afterLine > len(lines) {
		return nil, fmt.Errorf("insert_line %d is past the end of the file (%d lines)", afterLine, len(lines))
	}
	inserted := append([]string(nil), lines[:afterLine]...)
	inserted = append(inserted, strings.Split(strings.TrimSuffix(text, "\n"), "\n")...)
	inserted = append(inserted, lines[afterLine:]...)
	return []byte(strings.Join(inserted, "\n")), nil
}

// plural 返回 "N occurrence(s)"。
func plural(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

// fencePatch 把无法解析的补丁原文包成代码块（截断到 maxPatchFallbackBytes）。
func fencePatch(patchText string) string {
	text := patchText
	if len(text) > maxPatchFallbackBytes {
		text = text[:maxPatchFallbackBytes] + "\n… (truncated)"
	}
	return "```diff\n" + strings.TrimRight(text, "\n") + "\n```"
}
