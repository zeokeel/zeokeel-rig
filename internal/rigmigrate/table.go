// Package rigmigrate 是宿主升级用的迁移知识库：一份"已经不存在 / 已经改了签名"的
// 符号表，外加一个基于 go/parser 的只读扫描器。
//
// 为什么需要它：破坏性变更在宿主编译器里的表现是"挨个报错、改一个再报下一个"，
// 一轮轮往返。符号表是这些变更的唯一真相源——`cmd/rigmigrate` 拿它扫代码，
// `docs/MIGRATION-v0.25.md` 拿它写文档，守护测试拿它钉住"文档说的和代码里的一致"。
//
// 它只解析、不类型检查：宿主升级时的真实状态恰恰是"编译不过"，
// 扫描器必须能在这种状态下工作。
package rigmigrate

// Entry 描述一条破坏性变更。
//
// Old 是旧符号，写法与 go/ast 一致：
//
//   - `pkg.Func` / `pkg.Var` / `pkg.Type`（两段）：包级符号；
//   - `pkg.Type.Member`（三段）：结构体字段或方法。
//
// 三段形式的成员按**成员名**匹配：宿主既可能写 `kit.Config{Redactor: nil}`，
// 也可能写 `cfg.Redactor`。前者是精确匹配（键值字面量的类型对得上），后者只看
// 成员名——所以一个三段条目会命中任何 `.Member` 选择器，这是有意的宽匹配。
//
// Args 非 0 表示"符号还在、签名收紧了"：只有实参个数等于 Args 的调用才算命中，
// 此时不做名称选择器匹配（否则同一条会被报两次）。
type Entry struct {
	Old  string // 旧符号
	New  string // 新做法（宿主应当改成什么）
	Note string // 备注：为什么删、迁移时容易踩什么
	Args int    // 旧调用的实参个数；0 表示"符号已删除"
}

// ImportPath 描述一个被整包删除的 import 路径。
//
// 单列而不是塞进 Entry：包名既不是声明也不是选择器，命中点是 import 声明。
type ImportPath struct {
	Path string // 旧 import 路径
	New  string // 新做法
	Note string // 备注
}

// removedSymbols 是破坏性变更清单。顺序按主题分组，与迁移文档的章节顺序一致。
//
// 新增/删除条目时**必须**同步 docs/MIGRATION-v0.25.md：守护测试会双向比对。
var removedSymbols = []Entry{
	// --- 脱敏整体删除 ---------------------------------------------------
	{
		Old:  "agent.NewWithRedactor",
		New:  "agent.New(spec)",
		Note: "SDK 不再做任何脱敏：工具结果、助手文本、事件与错误都是原文，凭据屏蔽由宿主的 UI/导出通道负责。",
	},
	{
		Old:  "agent.RedactMessages",
		New:  "（无替代）删掉这次调用",
		Note: "没有任何脱敏层，历史里的文本原样进出。",
	},
	{
		Old:  "agent.RunnerOptions.Redactor",
		New:  "（无替代）删掉这个字段",
		Note: "子 agent 与父 agent 一样不做脱敏。",
	},
	{
		Old:  "secrets.RedactForExport",
		New:  "（无替代）宿主导出前自己清洗",
		Note: "secrets 包整体删除（含 Redactor 与 22 条内置模式）。",
	},
	{
		Old:  "kit.Config.Redactor",
		New:  "kit.Config.PermissionPreset / Mode / Approval / Policy",
		Note: "构造期不再接受脱敏器；权限只由权限档、模式、审批与策略表达。",
	},
	{
		Old:  "kit.NewEventSink",
		Args: 2,
		New:  "kit.NewEventSink(w)",
		Note: "redactor 参数已删除：事件桥只做 rig.Event 到 events.Envelope 的映射，不改写内容。",
	},

	// --- 保护路径与拒绝 glob 整族删除 ------------------------------------
	{
		Old:  "sandbox.WithProtectedPaths",
		New:  "sandbox.WithWritableRoot(root, readOnly...)",
		Note: "根内局部只读改由可写根自己的 ReadOnlySubpaths 表达；策略层没有独立的保护路径。",
	},
	{
		Old:  "sandbox.WithProtectMissingPaths",
		New:  "（无替代）删掉这个选项",
		Note: "没有保护路径，就没有「保护路径不存在」这个概念。",
	},
	{
		Old:  "sandbox.WorkspaceMetadataPaths",
		New:  "（无替代）删掉这次调用",
		Note: "不再需要枚举「需要保护的位置」；要只读保护某目录就把可写面排除它。",
	},
	{
		Old:  "sandbox.WorkspaceNonGitMetadataPaths",
		New:  "（无替代）删掉这次调用",
		Note: "同上。",
	},
	{
		Old:  "sandbox.WithDeniedGlobs",
		New:  "sandbox.WithHidden(patterns...)",
		Note: "隐藏路径是参与判定的替代品：读取、列目录、glob/grep/search 结果与写入都被拦；它只约束工具面，不是 OS 边界。",
	},
	{
		Old:  "sandbox.ErrGrantProtected",
		New:  "（无替代）按错误文案或 WritableRootsFor 自行判断",
		Note: "授权校验只看可写根、静态可写根与已授权项，不再有「与保护路径冲突」这一类拒绝。",
	},
	{
		Old:  "sandbox.ValidateWritableGrantPath",
		Args: 5,
		New:  "sandbox.ValidateWritableGrantPath(path, roots, configured, granted)",
		Note: "protected 参数已删除，现在收 4 个实参。",
	},
	{
		Old:  "sandbox.Policy.AddProtectedPaths",
		New:  "sandbox.WithWritableRoot(root, readOnly...)",
		Note: "保护路径整族删除，包括追加接口。",
	},
	{
		Old:  "sandbox.Policy.ProtectedPaths",
		New:  "sandbox.Policy.WritableRootsFor(mode) 里的 ReadOnlySubpaths",
		Note: "回读可写面时看每个可写根自己的只读子路径。",
	},
	{
		Old:  "sandbox.Policy.WriteDenyPaths",
		New:  "sandbox.Policy.WritableRootsFor(mode) 里的 ReadOnlySubpaths",
		Note: "它以前返回「减去保护路径后的可写面」，这一层不存在了。",
	},
	{
		Old:  "sandbox.Policy.ProtectMissingPaths",
		New:  "（无替代）删掉这个选项",
		Note: "已无缺失保护路径的概念。",
	},
	{
		Old:  "sandbox.Policy.DeniedGlobs",
		New:  "sandbox.WithHidden / sandbox.Policy.HiddenPatterns",
		Note: "拒绝 glob 删除；隐藏路径是仍然生效的按模式判定机制。",
	},
	{
		Old:  "sandbox.Denial.ProtectedPaths",
		New:  "sandbox.Denial.WritableRoots",
		Note: "拒绝归因不再渲染保护路径；可写面看 WritableRoots 与 Policy.WritableRootsFor。",
	},
	{
		Old:  "osrunner.Profile.Protected",
		New:  "（无替代）删掉这个字段",
		Note: "OS 后端从不渲染保护规则，字段保留只会造成「设了边界」的错觉。",
	},
	{
		Old:  "osrunner.Profile.MissingProtected",
		New:  "（无替代）删掉这个字段",
		Note: "同上。",
	},
	{
		Old:  "osrunner.ErrMissingProtected",
		New:  "（无替代）删掉这个判断",
		Note: "没有保护路径就不会有「缺失」错误。",
	},

	// --- kit 与 tools/full 的保护开关 ------------------------------------
	{
		Old:  "kit.Config.DisableProtection",
		New:  "kit.Config.PermissionPreset / Mode / Approval / Policy",
		Note: "kit 从不追加任何保护项，没有可关的东西。",
	},
	{
		Old:  "kit.Config.ProtectPaths",
		New:  "（无替代）删掉这个字段",
		Note: "kit 从不追加保护路径。",
	},
	{
		Old:  "kit.Config.ProtectWorkspaceMetadata",
		New:  "（无替代）删掉这个字段",
		Note: "kit 从不追加保护路径。",
	},
	{
		Old:  "kit.HomeSecretPaths",
		New:  "（无替代）宿主自己维护凭据位置",
		Note: "工具面不做凭据位置保护；需要在部署层（容器/VM）或宿主自己的校验里处理。",
	},
	{
		Old:  "full.AllOptions.ProtectWorkspaceMetadata",
		New:  "（无替代）删掉这个字段",
		Note: "sandbox.Policy 里不再有任何「保护」开关。",
	},
}

// modulePrefix 是根模块路径前缀。
//
// 已删除包的 import 路径用拼接而不是整串字面量：internal/rigdoc 的
// TestRemovedPackagesAreGone 按「带引号的完整路径」扫源码，而这里的字符串是
// 文档数据（告诉宿主哪个 import 被删了），不是真的 import。拼接让它继续只抓
// 真正的 import 语句，不必为一个数据文件开白名单。
const modulePrefix = "github.com/zeokeel/zeokeel-rig/"

// removedImports 是被整包删除的 import 路径。
var removedImports = []ImportPath{
	{
		Path: modulePrefix + "secrets",
		New:  "（无替代）宿主导出/展示前自己清洗",
		Note: "脱敏包整体删除：SDK 原样传递，半吊子脱敏反而让人误以为已经清洗过。",
	},
}

// RemovedSymbols 返回符号表的副本（调用方可以排序/过滤而不影响包内状态）。
func RemovedSymbols() []Entry {
	out := make([]Entry, len(removedSymbols))
	copy(out, removedSymbols)
	return out
}

// RemovedImports 返回已删除 import 路径的副本。
func RemovedImports() []ImportPath {
	out := make([]ImportPath, len(removedImports))
	copy(out, removedImports)
	return out
}
