package instructions

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Source 是一条指令文档的来源。Render 把它渲染成
// `(source: user|root|ancestor|local)`，因此取值顺序即优先级顺序（从低到高）。
type Source int

const (
	// SourceUser 是用户级文档：默认 ~/.config/rig/AGENTS.md（Windows 上是
	// %AppData%\rig\AGENTS.md），或用 WithUserPath/WithExtraPaths 指定。
	SourceUser Source = iota
	// SourceRoot 是仓库根（含 .git 的那一级）的 AGENTS.md。
	SourceRoot
	// SourceAncestor 是仓库根与 cwd 之间的目录里的 AGENTS.md。
	SourceAncestor
	// SourceLocal 是 cwd 本级的 AGENTS.md（cwd 恰好是仓库根时归为 SourceRoot）。
	SourceLocal
)

// String 返回模型可见的来源标签。
func (s Source) String() string {
	switch s {
	case SourceUser:
		return "user"
	case SourceRoot:
		return "root"
	case SourceAncestor:
		return "ancestor"
	case SourceLocal:
		return "local"
	}
	return "unknown"
}

// Document 是一条已读取、已展开 @import、已截断的指令文档。
type Document struct {
	// Path 是文档的绝对路径（已 Clean，不解析符号链接）。
	Path string
	// Source 是该文档在层级中的位置。
	Source Source
	// Content 是展开 @import 并截断之后的文本。
	Content string
	// Imports 记录本文档里出现过的 import 目标（按出现顺序去重，写成指令里的样子），
	// 含成功与失败两类，供诊断使用。
	Imports []string
}

// Set 是一次发现的结果。Documents 按优先级从低到高排列：SourceUser 最先，
// 最后是 SourceLocal。
type Set struct {
	Documents []Document
	// skipped 记录因总预算（WithTotalBytes）被整份跳过的文档数；Render 据此在末尾
	// 附一行 `[instructions truncated: N document(s) skipped]`。
	skipped int
}

// SkippedDocuments 返回因总预算被跳过的文档数。
func (s Set) SkippedDocuments() int { return s.skipped }

const (
	// DefaultMaxBytes 是单文档的默认上限（64 KiB）。超限时保留头部并附截断尾注。
	DefaultMaxBytes = 64 << 10
	// DefaultMaxImportDepth 是默认允许的 import 嵌套层数（顶层文档为第 0 层，
	// 因此默认最多展开 4 层 import）。
	DefaultMaxImportDepth = 4
	// agentsFileName 是默认被自动发现的文件名。宿主可以用 WithRepoFileNames
	// 追加自己仓库的文档名（默认名永远保留）。
	agentsFileName = "AGENTS.md"
	// DefaultTotalBytes 是全部文档渲染内容的合计上限（256 KiB）。单文档上限
	// （DefaultMaxBytes）防的是"一份文档过大"，总量上限防的是"很多份都不大，
	// 但拼起来把 system prompt 挤爆"。
	DefaultTotalBytes = 256 << 10
	// skippedDocumentPlaceholder 是超预算文档在渲染结果里的占位行。
	skippedDocumentPlaceholder = "<!-- document skipped: total budget exceeded -->"
)

// Option 配置 Load 的行为。
type Option func(*options)

// options 是 Load 的解析后配置。
type options struct {
	// userPath 是用户级文档路径；userPathSet 区分"未设置（用默认路径）"与
	// "显式空串（关闭用户级文档）"。
	userPath    string
	userPathSet bool
	// extra 是额外的文档路径（按用户级文档对待），相对路径相对 cwd 解析。
	extra []string
	// maxBytes 是单文档上限。
	maxBytes int
	// maxImportDepth 是 import 嵌套上限；0 表示不展开任何 import。
	maxImportDepth int
	// importRoot 限定"仓库内文档"的 @import 必须落在该根内；空串表示不限制。
	// importRootSet 区分"未设置（用发现到的仓库根）"与"显式空串（不限制）"。
	importRoot    string
	importRootSet bool
	// totalBytes 是全部文档内容的合计上限。
	totalBytes int
	// repoFileNames 是仓库级/祖先级发现时额外尝试的文件名（默认名总在最前）。
	repoFileNames []string
}

// WithUserPath 覆盖用户级文档路径。传空串表示关闭用户级文档——需要完全确定性
// 输出的宿主（以及测试）依赖这一行为，而"不设置"则使用默认路径
// os.UserConfigDir()/rig/AGENTS.md。
func WithUserPath(path string) Option {
	return func(opts *options) {
		opts.userPath = path
		opts.userPathSet = true
	}
}

// WithExtraPaths 追加额外指令文档（按 SourceUser 对待），按给定顺序排在默认用户级
// 文档之后。相对路径相对 cwd 解析；不存在的项被静默跳过；重复路径只保留一次。
func WithExtraPaths(paths ...string) Option {
	return func(opts *options) {
		opts.extra = append(opts.extra, paths...)
	}
}

// WithRepoFileNames 追加仓库级/祖先级发现时尝试的文件名（宿主设置项
// agent.agentsFile 就落在这里）。语义是**追加**而不是替换：默认的 AGENTS.md
// 永远还会被发现，并且排在给定名字之前——这样旧行为不会因为某个宿主配了新名字
// 而悄悄消失，重名文件（同一目录下的 AGENTS.md 与自定义名）也会一起进 prompt。
//
// 名字相对于所在目录解析，因此 "docs/AGENTS.md" 这类带子目录的名字同样可用。
// 空串、绝对路径与被忽略；重复名字只保留第一次。
func WithRepoFileNames(names ...string) Option {
	return func(opts *options) {
		opts.repoFileNames = append(opts.repoFileNames, names...)
	}
}

// WithMaxBytes 设置单文档上限（默认 DefaultMaxBytes）。非正值保持默认。
func WithMaxBytes(maxBytes int) Option {
	return func(opts *options) {
		if maxBytes > 0 {
			opts.maxBytes = maxBytes
		}
	}
}

// WithMaxImportDepth 设置 import 嵌套上限（默认 DefaultMaxImportDepth）。0 表示
// 关闭 @import 展开；负值保持默认。
func WithMaxImportDepth(depth int) Option {
	return func(opts *options) {
		if depth >= 0 {
			opts.maxImportDepth = depth
		}
	}
}

// WithImportRoot 限定"仓库内文档"的 @import 必须落在该根内（EvalSymlinks 后判定，
// 因此指向根外的符号链接同样被拦住）。逃逸的 import 在原位保留
// `<!-- import not found: … -->` 注释，不会泄露根外路径是否存在。
//
// 默认根 = 发现到的仓库根（没有 .git 时 = cwd）。宿主显式传入的 user/extra 文档
// 不受限：它们不是仓库内容，是宿主自己选的配置。传空串表示完全不限制（旧行为），
// 传相对路径时相对 cwd 解析。根不存在时等价于"不允许任何 import"（fail closed），
// 而不是悄悄退回不限制。
func WithImportRoot(root string) Option {
	return func(opts *options) {
		opts.importRoot = root
		opts.importRootSet = true
	}
}

// WithTotalBytes 设置全部文档渲染内容的合计上限（默认 DefaultTotalBytes，256 KiB）。
// 非正值使用默认值。
//
// 超限时按优先级**从高到低**（local → 最近的 ancestor → … → root → user）累计，
// 先保留更具体的文档；被跳过的文档整份换成一 行
// `<!-- document skipped: total budget exceeded -->` 占位，Render 末尾再附
// `[instructions truncated: N document(s) skipped]`。这样模型至少知道"这里本该有
// 内容"。
func WithTotalBytes(n int) Option {
	return func(opts *options) {
		if n > 0 {
			opts.totalBytes = n
		}
	}
}

// Load 从 cwd 向上发现 AGENTS.md（以及 WithRepoFileNames 追加的文件名），
// 停止于含 .git 的目录或文件系统根，并加入用户级与额外文档。返回的 Set 已按
// 优先级排序，可直接交给 Render。
//
// 错误只覆盖"无法开始"或"文档存在但读不出来"：cwd 不存在/不是目录、某个 AGENTS.md
// 是目录或不可读都会返回错误（fail loud，便于宿主提示用户）；文件不存在则静默
// 跳过。import 失败永远不报错（见包文档）。
func Load(cwd string, opts ...Option) (Set, error) {
	config := options{
		maxBytes:       DefaultMaxBytes,
		maxImportDepth: DefaultMaxImportDepth,
		totalBytes:     DefaultTotalBytes,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&config)
		}
	}
	start, err := resolveStartDir(cwd)
	if err != nil {
		return Set{}, err
	}

	// 先做发现：仓库根同时决定文档链与 @import 的默认根。
	chain, repo := discoverChain(start)
	importRoot := ""
	switch {
	case config.importRootSet:
		importRoot = strings.TrimSpace(config.importRoot)
		if importRoot != "" && !filepath.IsAbs(importRoot) {
			importRoot = filepath.Join(start, importRoot)
		}
	case repo:
		importRoot = chain[len(chain)-1]
	default:
		importRoot = start
	}
	importRoot = canonicalDirectory(importRoot)

	seen := make(map[string]struct{})
	var documents []Document

	// 单份文档的加载入口；restricted 决定 @import 是否受 importRoot 限制。
	loadInto := func(path string, source Source, restricted bool) error {
		root := ""
		if restricted {
			root = importRoot
		}
		doc, ok, loadErr := loadDocument(path, start, source, root, config)
		if loadErr != nil {
			return loadErr
		}
		if ok {
			documents = appendDocument(documents, doc, seen)
		}
		return nil
	}

	// 用户级文档：默认路径或显式覆盖（空串关闭）。它们不受 importRoot 限制。
	if config.userPathSet {
		if path := strings.TrimSpace(config.userPath); path != "" {
			if err := loadInto(path, SourceUser, false); err != nil {
				return Set{}, err
			}
		}
	} else if path := defaultUserPath(); path != "" {
		if err := loadInto(path, SourceUser, false); err != nil {
			return Set{}, err
		}
	}

	for _, path := range config.extra {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if err := loadInto(path, SourceUser, false); err != nil {
			return Set{}, err
		}
	}

	// 项目文档：chain 是 cwd → … → 停止目录，倒序即"从远到近"，恰好是
	// root → ancestor（远到近）→ local 的渲染顺序。每个目录里按"默认名优先、
	// 追加名随后"的顺序尝试，因此同一目录的两份文档按稳定顺序出现。
	fileNames := repoFileNames(config.repoFileNames)
	for index := len(chain) - 1; index >= 0; index-- {
		dir := chain[index]
		source := SourceAncestor
		switch {
		case repo && dir == chain[len(chain)-1]:
			source = SourceRoot
		case index == 0:
			source = SourceLocal
		}
		for _, name := range fileNames {
			if err := loadInto(filepath.Join(dir, name), source, true); err != nil {
				return Set{}, err
			}
		}
	}

	documents, skipped := applyTotalBudget(documents, config.totalBytes)
	return Set{Documents: documents, skipped: skipped}, nil
}

// canonicalDirectory 把目录规范成真实绝对路径（解析符号链接），失败时退回 Clean。
// 空串保持空串（表示"不限制"）。
func canonicalDirectory(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	absolute = filepath.Clean(absolute)
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(resolved)
	}
	return absolute
}

// applyTotalBudget 按"优先级从高到低"累计内容大小（即从切片尾部往前走），超出
// 预算的文档整份换成一 行占位。返回新文档列表与被跳过的数量。
//
// 从高到低而不是从低到高，是因为预算不够时必须优先保留最具体（离 cwd 最近）的
// 指令；反过来做会让模型看到"根目录的通用说明"却丢掉当前目录的具体规则。
func applyTotalBudget(documents []Document, total int) ([]Document, int) {
	if total <= 0 || len(documents) == 0 {
		return documents, 0
	}
	keep := make([]bool, len(documents))
	used := 0
	for index := len(documents) - 1; index >= 0; index-- {
		size := len(documents[index].Content)
		if used+size > total {
			continue
		}
		keep[index] = true
		used += size
	}
	skipped := 0
	out := make([]Document, len(documents))
	for index, doc := range documents {
		if keep[index] {
			out[index] = doc
			continue
		}
		skipped++
		out[index] = Document{
			Path:    doc.Path,
			Source:  doc.Source,
			Content: skippedDocumentPlaceholder,
			Imports: doc.Imports,
		}
	}
	return out, skipped
}

// appendDocument 追加去重后的文档（同一文件只出现一次）。
func appendDocument(documents []Document, doc Document, seen map[string]struct{}) []Document {
	if _, exists := seen[doc.Path]; exists {
		return documents
	}
	seen[doc.Path] = struct{}{}
	return append(documents, doc)
}

// resolveStartDir 把 cwd 规范化成存在的绝对目录。
//
// TODO(symlink-cwd): 这里刻意不做 EvalSymlinks——解析链接会改变 Document.Path 与
// 宿主的相对化显示（"我在哪个目录"是用户的语义，不是文件系统的语义）。代价是：
// cwd 是符号链接时，向上发现走链接所在路径，真实仓库根那一侧的 AGENTS.md 会漏掉。
// 将来若要改，建议只在"发现"时用真实路径，仍按字面路径填 Document.Path。
func resolveStartDir(cwd string) (string, error) {
	raw := strings.TrimSpace(cwd)
	if raw == "" {
		raw = "."
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("instructions: resolve cwd %q: %w", raw, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("instructions: cwd %q: %w", absolute, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("instructions: cwd %q is not a directory", absolute)
	}
	return absolute, nil
}

// discoverChain 返回 cwd → 父 → … 的目录链，并在遇到含 .git 的目录时停止（该目录
// 包含在链里）。第二个返回值报告链尾是否真的是仓库根。
func discoverChain(start string) ([]string, bool) {
	var chain []string
	dir := start
	for {
		chain = append(chain, dir)
		if hasRepoMarker(dir) {
			return chain, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return chain, false
		}
		dir = parent
	}
}

// hasRepoMarker 报告 dir 是否含 .git（目录或 gitdir 文件都算）。
func hasRepoMarker(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// repoFileNames 返回仓库级发现实际尝试的文件名列表：默认名在最前，追加名随后。
// 目的是"旧行为永远还在"——宿主配置了新名字也不会让 AGENTS.md 消失。空串、
// 绝对路径与被清理后为空的项被跳过，重复名字只保留第一次。
func repoFileNames(extra []string) []string {
	names := []string{agentsFileName}
	seen := map[string]struct{}{agentsFileName: {}}
	for _, name := range extra {
		name = strings.TrimSpace(name)
		if name == "" || filepath.IsAbs(name) {
			continue
		}
		name = filepath.Clean(name)
		if name == "." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || name == ".." {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// defaultUserPath 返回用户级文档的默认路径；无法确定用户配置目录时返回空。
func defaultUserPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, "rig", agentsFileName)
}

// loadDocument 读取并展开一份文档。第二个返回值报告文件是否存在（不存在时静默
// 跳过，不视为错误）。
func loadDocument(path, base string, source Source, importRoot string, config options) (Document, bool, error) {
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(base, resolved)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return Document{}, false, fmt.Errorf("instructions: resolve %q: %w", resolved, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Document{}, false, nil
		}
		return Document{}, false, fmt.Errorf("instructions: inspect %q: %w", absolute, err)
	}
	if info.IsDir() {
		return Document{}, false, fmt.Errorf("instructions: %q is a directory, not a document", absolute)
	}
	if !info.Mode().IsRegular() {
		// FIFO、socket、设备文件在 ReadFile 上会永久阻塞（FIFO 等待写端）：宿主会卡在
		// prompt 组装。它们不是文档，按 fail loud 拒绝，与目录一致。
		return Document{}, false, fmt.Errorf("instructions: %q is not a regular file (mode %s)", absolute, info.Mode().Type())
	}
	raw, err := os.ReadFile(absolute)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Document{}, false, nil
		}
		return Document{}, false, fmt.Errorf("instructions: read %q: %w", absolute, err)
	}
	content, imports := expandDocumentWithin(absolute, sanitizeText(string(raw)), config.maxImportDepth, importRoot)
	return Document{
		Path:    absolute,
		Source:  source,
		Content: truncate(content, config.maxBytes),
		Imports: imports,
	}, true, nil
}
