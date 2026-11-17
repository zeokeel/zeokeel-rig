package instructions_test

// 指令层级（AGENTS.md 发现 + @import 展开）与环境上下文的行为契约。
//
// 这些用例先于实现写成（TDD）：它们钉住的是模型可见文本的字节形状——发现顺序、
// 来源标签、import 展开与失败的注释、截断尾注，以及 <environment> 块——因为这些
// 文本一旦发到 system prompt 里就属于接口，必须有 golden 级测试。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/instructions"
)

// --- 测试辅助 -------------------------------------------------------------

// writeFile 在 path 写入内容（自动创建父目录）。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

// mkdir 创建目录（含父目录）。
func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

// mustLoad 调用 Load 并要求成功。
func mustLoad(t *testing.T, cwd string, opts ...instructions.Option) instructions.Set {
	t.Helper()
	set, err := instructions.Load(cwd, opts...)
	if err != nil {
		t.Fatalf("Load(%q): %v", cwd, err)
	}
	return set
}

// findDoc 返回指定路径的文档，找不到则失败。
func findDoc(t *testing.T, set instructions.Set, path string) instructions.Document {
	t.Helper()
	for _, doc := range set.Documents {
		if doc.Path == path {
			return doc
		}
	}
	t.Fatalf("document %q not found in %+v", path, set.Documents)
	return instructions.Document{}
}

// localDoc 返回 cwd 本级的文档内容（测试 import 时的主文档）。
func localDoc(t *testing.T, set instructions.Set, dir string) instructions.Document {
	t.Helper()
	return findDoc(t, set, filepath.Join(dir, "AGENTS.md"))
}

// docsUnder 只保留位于 root 之下的文档（避免依赖 /tmp 或 / 上可能存在的文件）。
func docsUnder(set instructions.Set, root string) []instructions.Document {
	var out []instructions.Document
	for _, doc := range set.Documents {
		rel, err := filepath.Rel(root, doc.Path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		out = append(out, doc)
	}
	return out
}

// docPaths 返回文档的绝对路径列表。
func docPaths(docs []instructions.Document) []string {
	out := make([]string, 0, len(docs))
	for _, doc := range docs {
		out = append(out, doc.Path)
	}
	return out
}

// --- 发现：层级与顺序 -----------------------------------------------------

// TestLoadDiscoversRootAncestorLocal 覆盖仓库根 / 中间目录 / cwd 三级发现与
// 「从低优先级到高优先级」的文档顺序。
func TestLoadDiscoversRootAncestorLocal(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root")
	writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), "ancestor")
	writeFile(t, filepath.Join(root, "sub", "deep", "AGENTS.md"), "local")
	cwd := filepath.Join(root, "sub", "deep")

	set := mustLoad(t, cwd, instructions.WithUserPath(""))
	wantOrder := []string{
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(root, "sub", "AGENTS.md"),
		filepath.Join(root, "sub", "deep", "AGENTS.md"),
	}
	if got := docPaths(set.Documents); len(got) != len(wantOrder) || got[0] != wantOrder[0] || got[1] != wantOrder[1] || got[2] != wantOrder[2] {
		t.Fatalf("documents = %v, want %v", got, wantOrder)
	}
	if got := findDoc(t, set, wantOrder[0]).Source; got != instructions.SourceRoot {
		t.Fatalf("root doc source = %v, want SourceRoot", got)
	}
	if got := findDoc(t, set, wantOrder[1]).Source; got != instructions.SourceAncestor {
		t.Fatalf("ancestor doc source = %v, want SourceAncestor", got)
	}
	if got := findDoc(t, set, wantOrder[2]).Source; got != instructions.SourceLocal {
		t.Fatalf("local doc source = %v, want SourceLocal", got)
	}
	if got := findDoc(t, set, wantOrder[2]).Content; got != "local" {
		t.Fatalf("local content = %q, want %q", got, "local")
	}
}

// TestLoadStopsAtGitDir 证明发现停在含 .git 的目录（含），不会继续向上。
func TestLoadStopsAtGitDir(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "AGENTS.md"), "outside")
	root := filepath.Join(outside, "repo")
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root")
	writeFile(t, filepath.Join(root, "pkg", "AGENTS.md"), "local")
	cwd := filepath.Join(root, "pkg")

	set := mustLoad(t, cwd, instructions.WithUserPath(""))
	for _, doc := range set.Documents {
		if doc.Path == filepath.Join(outside, "AGENTS.md") {
			t.Fatalf("discovery walked past the repository root: %+v", set.Documents)
		}
	}
	if got := len(set.Documents); got != 2 {
		t.Fatalf("documents = %v, want only the root and local docs", docPaths(set.Documents))
	}
}

// TestLoadWithoutGitWalksToFilesystemRoot 证明没有 .git 时一路向上到文件系统根，
// 且这些文档一律标为 ancestor（不存在仓库根）。
func TestLoadWithoutGitWalksToFilesystemRoot(t *testing.T) {
	base := t.TempDir() // 没有 .git
	writeFile(t, filepath.Join(base, "AGENTS.md"), "base")
	writeFile(t, filepath.Join(base, "outer", "AGENTS.md"), "outer")
	cwd := filepath.Join(base, "outer", "inner", "deep")
	mkdir(t, cwd)

	set := mustLoad(t, cwd, instructions.WithUserPath(""))
	var sources []instructions.Source
	for _, doc := range docsUnder(set, base) {
		sources = append(sources, doc.Source)
	}
	// base 与 outer 都是 ancestor；cwd 没有 AGENTS.md，因此没有 local 文档。
	if len(sources) != 2 {
		t.Fatalf("docs under base = %v, want exactly base and outer", docPaths(docsUnder(set, base)))
	}
	for _, source := range sources {
		if source != instructions.SourceAncestor {
			t.Fatalf("source = %v, want SourceAncestor when there is no repository", source)
		}
	}
	for _, doc := range set.Documents {
		if doc.Source == instructions.SourceRoot {
			t.Fatalf("no .git anywhere, yet a root doc was reported: %+v", doc)
		}
	}
}

// TestLoadCwdIsRepoRootIsRootSource 证明 cwd 本身是仓库根时，它的文档来源是
// root（而不是 local）。
func TestLoadCwdIsRepoRootIsRootSource(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	doc := localDoc(t, set, root)
	if doc.Source != instructions.SourceRoot {
		t.Fatalf("source = %v, want SourceRoot for cwd == repository root", doc.Source)
	}
}

// TestLoadOnlyReadsAgentsFile 证明只认 AGENTS.md，不发明 override 之类的文件名。
func TestLoadOnlyReadsAgentsFile(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.override.md"), "override")
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "claude")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	if len(set.Documents) != 0 {
		t.Fatalf("documents = %v, want none (only AGENTS.md is discovered)", docPaths(set.Documents))
	}
}

// TestLoadMultipleAncestorsFarToNear 覆盖两级以上祖先，顺序必须是「从远到近」。
func TestLoadMultipleAncestorsFarToNear(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root")
	writeFile(t, filepath.Join(root, "a", "AGENTS.md"), "a")
	writeFile(t, filepath.Join(root, "a", "b", "AGENTS.md"), "b")
	writeFile(t, filepath.Join(root, "a", "b", "c", "AGENTS.md"), "c")
	cwd := filepath.Join(root, "a", "b", "c")

	set := mustLoad(t, cwd, instructions.WithUserPath(""))
	var contents []string
	for _, doc := range set.Documents {
		contents = append(contents, doc.Content)
	}
	want := []string{"root", "a", "b", "c"}
	if len(contents) != len(want) {
		t.Fatalf("contents = %v, want %v", contents, want)
	}
	for i := range want {
		if contents[i] != want[i] {
			t.Fatalf("contents = %v, want %v (far-to-near order)", contents, want)
		}
	}
}

// --- 发现：用户级与额外路径 -----------------------------------------------

// TestLoadUserDocumentComesFirst 证明用户级文档排在所有项目文档之前。
func TestLoadUserDocumentComesFirst(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root")
	user := filepath.Join(t.TempDir(), "AGENTS.md")
	writeFile(t, user, "user")

	set := mustLoad(t, root, instructions.WithUserPath(user))
	if len(set.Documents) != 2 {
		t.Fatalf("documents = %v, want the user doc and the root doc", docPaths(set.Documents))
	}
	if set.Documents[0].Path != user || set.Documents[0].Source != instructions.SourceUser {
		t.Fatalf("first document = %+v, want the user doc with SourceUser", set.Documents[0])
	}
}

// TestLoadUserPathMissingIsSkipped 证明用户级文件不存在时静默跳过。
func TestLoadUserPathMissingIsSkipped(t *testing.T) {
	root := t.TempDir()
	set := mustLoad(t, root, instructions.WithUserPath(filepath.Join(t.TempDir(), "missing.md")))
	if len(set.Documents) != 0 {
		t.Fatalf("documents = %v, want none", docPaths(set.Documents))
	}
}

// TestLoadEmptyUserPathDisablesUserDocument 证明 WithUserPath("") 关闭用户级文档
// （测试与需要完全确定性输出的宿主依赖这一点）。
func TestLoadEmptyUserPathDisablesUserDocument(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	if len(set.Documents) != 1 || set.Documents[0].Source != instructions.SourceRoot {
		t.Fatalf("documents = %+v, want only the root doc", set.Documents)
	}
}

// TestLoadExtraPathsAppendedAsUserDocs 证明额外路径按给定顺序追加，且相对路径
// 相对 cwd 解析。
func TestLoadExtraPathsAppendedAsUserDocs(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root")
	writeFile(t, filepath.Join(root, "policies", "team.md"), "team")

	set := mustLoad(t, root,
		instructions.WithUserPath(""),
		instructions.WithExtraPaths(filepath.Join("policies", "team.md")),
	)
	if len(set.Documents) != 2 {
		t.Fatalf("documents = %v, want the extra doc plus the root doc", docPaths(set.Documents))
	}
	extra := set.Documents[0]
	if extra.Path != filepath.Join(root, "policies", "team.md") {
		t.Fatalf("extra path = %q, want it resolved against cwd", extra.Path)
	}
	if extra.Source != instructions.SourceUser {
		t.Fatalf("extra source = %v, want SourceUser", extra.Source)
	}
}

// TestLoadDedupesUserAndExtraPaths 证明同一文件不会因重复配置而出现两次。
func TestLoadDedupesUserAndExtraPaths(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(t.TempDir(), "AGENTS.md")
	writeFile(t, user, "user")

	set := mustLoad(t, root,
		instructions.WithUserPath(user),
		instructions.WithExtraPaths(user, user),
	)
	if len(set.Documents) != 1 {
		t.Fatalf("documents = %v, want a single deduped document", docPaths(set.Documents))
	}
}

// TestLoadMissingCwdReturnsError 证明 cwd 不存在时返回错误而不是空集合。
func TestLoadMissingCwdReturnsError(t *testing.T) {
	if _, err := instructions.Load(filepath.Join(t.TempDir(), "nope"), instructions.WithUserPath("")); err == nil {
		t.Fatal("Load accepted a nonexistent cwd")
	}
}

// TestLoadUnreadableDocumentReturnsError 证明文档存在但不可读（这里是目录）时
// 返回错误，而不是静默吞掉。
func TestLoadUnreadableDocumentReturnsError(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "AGENTS.md"))
	if _, err := instructions.Load(root, instructions.WithUserPath("")); err == nil {
		t.Fatal("Load accepted a directory named AGENTS.md")
	}
}

// TestLoadWindowsStylePathDoesNotPanic 证明 Windows 风格路径在任何平台上都不会
// panic（用 filepath 语义处理，非法路径返回错误）。
func TestLoadWindowsStylePathDoesNotPanic(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Load panicked on a Windows-style path: %v", recovered)
		}
	}()
	for _, cwd := range []string{`C:\repo\sub`, `C:\`, `\\server\share\dir`} {
		_, _ = instructions.Load(cwd, instructions.WithUserPath(""))
	}
}

// TestLoadNormalizesCwd 证明带尾部分隔符与 "."/".." 的 cwd 被规范化后再匹配。
func TestLoadNormalizesCwd(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), "local")
	cwd := filepath.Join(root, "sub") + string(filepath.Separator) + "." + string(filepath.Separator) + ".." + string(filepath.Separator) + "sub"

	set := mustLoad(t, cwd, instructions.WithUserPath(""))
	doc := localDoc(t, set, filepath.Join(root, "sub"))
	if doc.Source != instructions.SourceLocal {
		t.Fatalf("source = %v, want SourceLocal", doc.Source)
	}
}

// --- @import 展开 ---------------------------------------------------------

// TestImportRelativePath 证明相对当前文档目录展开。
func TestImportRelativePath(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "before\n@imports/a.md\nafter\n")
	writeFile(t, filepath.Join(root, "imports", "a.md"), "imported a\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	doc := localDoc(t, set, root)
	if want := "before\nimported a\nafter\n"; doc.Content != want {
		t.Fatalf("content = %q, want %q", doc.Content, want)
	}
	if len(doc.Imports) != 1 || filepath.Base(doc.Imports[0]) != "a.md" {
		t.Fatalf("imports = %v, want the expanded target", doc.Imports)
	}
}

// TestImportQuotedPathWithSpaces 证明带引号的路径（含空格）可展开。
func TestImportQuotedPathWithSpaces(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "@\"./x y.md\"\n")
	writeFile(t, filepath.Join(root, "x y.md"), "spaced\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	if got := localDoc(t, set, root).Content; got != "spaced\n" {
		t.Fatalf("content = %q, want %q", got, "spaced\n")
	}
}

// TestImportAbsolutePath 证明绝对路径可展开——但只限于仓库根之内：
// 根外的绝对路径与 `../` 逃逸一样被拦住（见 TestImportAbsolutePathOutsideRootIsBlocked）。
func TestImportAbsolutePath(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	inside := filepath.Join(root, "docs", "external.md")
	writeFile(t, inside, "external\n")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "@"+inside+"\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	if got := localDoc(t, set, root).Content; got != "external\n" {
		t.Fatalf("content = %q, want %q", got, "external\n")
	}
}

// TestImportAbsolutePathOutsideRootIsBlocked 证明根外的绝对路径 import 被拦住。
func TestImportAbsolutePathOutsideRootIsBlocked(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	external := filepath.Join(t.TempDir(), "external.md")
	writeFile(t, external, "OUTSIDE-CONTENT\n")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "@"+external+"\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	got := localDoc(t, set, root).Content
	if strings.Contains(got, "OUTSIDE-CONTENT") {
		t.Fatalf("content = %q, want the outside absolute path blocked", got)
	}
	if !strings.Contains(got, "<!-- import not found:") {
		t.Fatalf("content = %q, want a not-found comment", got)
	}
}

// TestImportRelativeToDocumentDirectory 证明嵌套文档里的 import 相对该文档所在
// 目录，而不是 cwd。
func TestImportRelativeToDocumentDirectory(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root\n")
	writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), "@parts/local.md\n")
	writeFile(t, filepath.Join(root, "sub", "parts", "local.md"), "parts local\n")
	cwd := filepath.Join(root, "sub")

	set := mustLoad(t, cwd, instructions.WithUserPath(""))
	if got := localDoc(t, set, cwd).Content; got != "parts local\n" {
		t.Fatalf("content = %q, want %q", got, "parts local\n")
	}
}

// TestImportRecursiveNested 证明 import 可以再 import（默认深度内）。
func TestImportRecursiveNested(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "0\n@d1.md\n")
	writeFile(t, filepath.Join(root, "d1.md"), "1\n@d2.md\n")
	writeFile(t, filepath.Join(root, "d2.md"), "2\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	if want := "0\n1\n2\n"; localDoc(t, set, root).Content != want {
		t.Fatalf("content = %q, want %q", localDoc(t, set, root).Content, want)
	}
}

// TestImportCycleIsSkipped 证明循环 import 被拦截并留下可诊断的注释。
func TestImportCycleIsSkipped(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "start\n@a.md\nend\n")
	writeFile(t, filepath.Join(root, "a.md"), "A\n@b.md\n")
	writeFile(t, filepath.Join(root, "b.md"), "B\n@a.md\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	want := "start\nA\nB\n<!-- import skipped: a.md (cycle) -->\nend\n"
	if got := localDoc(t, set, root).Content; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestImportDepthLimit 证明超过 WithMaxImportDepth 的展开被拦截。
func TestImportDepthLimit(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "0\n@d1.md\n")
	for i := 1; i <= 5; i++ {
		next := ""
		if i < 5 {
			next = "@d" + itoa(i+1) + ".md\n"
		}
		writeFile(t, filepath.Join(root, "d"+itoa(i)+".md"), itoa(i)+"\n"+next)
	}

	set := mustLoad(t, root, instructions.WithUserPath(""))
	want := "0\n1\n2\n3\n4\n<!-- import skipped: d5.md (max depth) -->\n"
	if got := localDoc(t, set, root).Content; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestImportDepthZeroDisablesImports 证明 WithMaxImportDepth(0) 关闭展开。
func TestImportDepthZeroDisablesImports(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "before\n@a.md\nafter\n")
	writeFile(t, filepath.Join(root, "a.md"), "imported\n")

	set := mustLoad(t, root, instructions.WithUserPath(""), instructions.WithMaxImportDepth(0))
	want := "before\n<!-- import skipped: a.md (max depth) -->\nafter\n"
	if got := localDoc(t, set, root).Content; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestImportMissingLeavesComment 证明 import 失败不报错，只留下注释并记入
// Document.Imports。
func TestImportMissingLeavesComment(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "before\n@missing.md\nafter\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	doc := localDoc(t, set, root)
	want := "before\n<!-- import not found: missing.md -->\nafter\n"
	if doc.Content != want {
		t.Fatalf("content = %q, want %q", doc.Content, want)
	}
	if len(doc.Imports) != 1 || doc.Imports[0] != "missing.md" {
		t.Fatalf("imports = %v, want [missing.md]", doc.Imports)
	}
}

// TestImportDirectiveNeedsOwnLine 证明只有独占一行的 @path 才是 import 指令，
// 行内出现的 @ 保持原样。
func TestImportDirectiveNeedsOwnLine(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	content := "see @a.md for details\n@not a path\n@\n"
	writeFile(t, filepath.Join(root, "AGENTS.md"), content)
	writeFile(t, filepath.Join(root, "a.md"), "imported\n")

	set := mustLoad(t, root, instructions.WithUserPath(""))
	if got := localDoc(t, set, root).Content; got != content {
		t.Fatalf("content = %q, want the input unchanged %q", got, content)
	}
}

// --- 截断 -----------------------------------------------------------------

// TestTruncationKeepsHeadAndNotesOmittedBytes 证明超限时保留头部并按字节数报告。
func TestTruncationKeepsHeadAndNotesOmittedBytes(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "0123456789ABC")

	set := mustLoad(t, root, instructions.WithUserPath(""), instructions.WithMaxBytes(10))
	want := "0123456789\n…[truncated: 3 bytes omitted]"
	if got := localDoc(t, set, root).Content; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestTruncationRespectsRuneBoundary 证明截断点落在 rune 边界上（不会切坏 UTF-8）。
func TestTruncationRespectsRuneBoundary(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "héllo")

	set := mustLoad(t, root, instructions.WithUserPath(""), instructions.WithMaxBytes(2))
	got := localDoc(t, set, root).Content
	if want := "h\n…[truncated: 5 bytes omitted]"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if !utf8Valid(got) {
		t.Fatalf("truncated content is not valid UTF-8: %q", got)
	}
}

// TestTruncationNotAppliedBelowLimit 证明未超限的内容逐字节不变。
func TestTruncationNotAppliedBelowLimit(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "short\n")

	set := mustLoad(t, root, instructions.WithUserPath(""), instructions.WithMaxBytes(1024))
	if got := localDoc(t, set, root).Content; got != "short\n" {
		t.Fatalf("content = %q, want %q", got, "short\n")
	}
}

// TestTruncationCountsExpandedImports 证明截断作用于展开 import 之后的完整内容。
func TestTruncationCountsExpandedImports(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "@big.md\n")
	writeFile(t, filepath.Join(root, "big.md"), strings.Repeat("x", 100))

	set := mustLoad(t, root, instructions.WithUserPath(""), instructions.WithMaxBytes(20))
	got := localDoc(t, set, root).Content
	if !strings.HasPrefix(got, strings.Repeat("x", 20)+"\n…[truncated: ") {
		t.Fatalf("content = %q, want the first 20 bytes plus the truncation note", got)
	}
}

// --- Render ---------------------------------------------------------------

// TestRenderGolden 逐字节钉住渲染结果：user → root → ancestor → local，段间空行。
func TestRenderGolden(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root rules\n")
	writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), "ancestor rules")
	writeFile(t, filepath.Join(root, "sub", "deep", "AGENTS.md"), "local rules\n")
	user := filepath.Join(t.TempDir(), "AGENTS.md")
	writeFile(t, user, "user rules\n")
	cwd := filepath.Join(root, "sub", "deep")

	set := mustLoad(t, cwd, instructions.WithUserPath(user))
	want := "## " + user + " (source: user)\nuser rules\n\n" +
		"## AGENTS.md (source: root)\nroot rules\n\n" +
		"## sub/AGENTS.md (source: ancestor)\nancestor rules\n\n" +
		"## sub/deep/AGENTS.md (source: local)\nlocal rules"
	if got := set.Render(root); got != want {
		t.Fatalf("Render mismatch:\ngot  %q\nwant %q", got, want)
	}
}

// TestRenderEmptySetReturnsEmptyString 证明没有文档时渲染为空串（可直接拼接）。
func TestRenderEmptySetReturnsEmptyString(t *testing.T) {
	var set instructions.Set
	if got := set.Render("/workspace"); got != "" {
		t.Fatalf("Render = %q, want empty", got)
	}
}

// TestRenderIsStable 证明同一 Set 两次渲染字节相同（无时间戳/无 map 迭代）。
func TestRenderIsStable(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root\n")
	writeFile(t, filepath.Join(root, "a", "AGENTS.md"), "a\n")
	cwd := filepath.Join(root, "a")
	set := mustLoad(t, cwd, instructions.WithUserPath(""))

	first := set.Render(root)
	if second := set.Render(root); first != second {
		t.Fatalf("Render is not stable:\nfirst  %q\nsecond %q", first, second)
	}
	if strings.Contains(first, "20") && strings.Contains(first, "T") {
		t.Fatalf("Render looks like it embeds a timestamp: %q", first)
	}
}

// TestRenderAbsoluteForOutsideWorkspace 证明工作区外的文档（用户级）用绝对路径，
// 工作区内用相对路径，且分隔符统一为 "/"。
func TestRenderAbsoluteForOutsideWorkspace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	user := filepath.Join(t.TempDir(), "home", "AGENTS.md")
	set := instructions.Set{Documents: []instructions.Document{
		{Path: user, Source: instructions.SourceUser, Content: "u"},
		{Path: filepath.Join(root, "pkg", "AGENTS.md"), Source: instructions.SourceLocal, Content: "l"},
	}}
	got := set.Render(root)
	if !strings.Contains(got, "## "+filepath.ToSlash(user)+" (source: user)") {
		t.Fatalf("user heading should be absolute: %q", got)
	}
	if !strings.Contains(got, "## pkg/AGENTS.md (source: local)") {
		t.Fatalf("workspace path should be relative with slashes: %q", got)
	}
}

// TestRenderWithoutWorkspaceRootUsesAbsolutePaths 证明 workspaceRoot 为空时用绝对
// 路径渲染（宿主未配置工作区时仍有可读标题）。
func TestRenderWithoutWorkspaceRootUsesAbsolutePaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	set := instructions.Set{Documents: []instructions.Document{{Path: path, Source: instructions.SourceRoot, Content: "x"}}}
	got := set.Render("")
	if !strings.Contains(got, "## "+filepath.ToSlash(path)+" (source: root)") {
		t.Fatalf("Render = %q, want the absolute path in the heading", got)
	}
}

// TestSourceString 钉住来源标签（模型可见：`source: root|ancestor|local|user`）。
func TestSourceString(t *testing.T) {
	cases := map[instructions.Source]string{
		instructions.SourceUser:     "user",
		instructions.SourceRoot:     "root",
		instructions.SourceAncestor: "ancestor",
		instructions.SourceLocal:    "local",
	}
	for source, want := range cases {
		if got := source.String(); got != want {
			t.Fatalf("Source(%d).String() = %q, want %q", int(source), got, want)
		}
	}
}

// --- 环境上下文 -----------------------------------------------------------

// TestDetectEnvironmentDefaults 证明基础字段来自运行时与入参。
func TestDetectEnvironmentDefaults(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	root := t.TempDir()
	env := instructions.DetectEnvironment(root, root, now)
	if env.OS != runtime.GOOS || env.Arch != runtime.GOARCH {
		t.Fatalf("platform = %s/%s, want %s/%s", env.OS, env.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if env.TempDir != os.TempDir() {
		t.Fatalf("TempDir = %q, want %q", env.TempDir, os.TempDir())
	}
	if !env.Time.Equal(now) {
		t.Fatalf("Time = %v, want %v", env.Time, now)
	}
	if env.WorkspaceRoot != root || env.Cwd != root {
		t.Fatalf("workspace/cwd = %q/%q, want %q", env.WorkspaceRoot, env.Cwd, root)
	}
	if env.GitBranch != "" {
		t.Fatalf("GitBranch = %q, want empty outside a repository", env.GitBranch)
	}
}

// TestDetectEnvironmentBranch 证明只读 .git/HEAD 就能解析分支（不 exec git）。
func TestDetectEnvironmentBranch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	env := instructions.DetectEnvironment(root, root, time.Now())
	if env.GitBranch != "main" {
		t.Fatalf("GitBranch = %q, want main", env.GitBranch)
	}
}

// TestDetectEnvironmentFromSubdirectory 证明 cwd 位于仓库子目录时也能找到仓库。
func TestDetectEnvironmentFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/feature/x\n")
	sub := filepath.Join(root, "a", "b")
	mkdir(t, sub)
	env := instructions.DetectEnvironment(root, sub, time.Now())
	if env.GitBranch != "feature/x" {
		t.Fatalf("GitBranch = %q, want feature/x", env.GitBranch)
	}
}

// TestDetectEnvironmentDetachedHead 证明 detached HEAD 渲染为 detached@<sha8>。
func TestDetectEnvironmentDetachedHead(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "0123456789abcdef0123456789abcdef01234567\n")
	env := instructions.DetectEnvironment(root, root, time.Now())
	if env.GitBranch != "detached@01234567" {
		t.Fatalf("GitBranch = %q, want detached@01234567", env.GitBranch)
	}
}

// TestDetectEnvironmentWorktreeGitFile 证明 .git 是文件（gitdir: …，worktree 与
// submodule 的形态）时仍能解析分支。
func TestDetectEnvironmentWorktreeGitFile(t *testing.T) {
	root := t.TempDir()
	realGitDir := t.TempDir()
	writeFile(t, filepath.Join(realGitDir, "HEAD"), "ref: refs/heads/worktree-branch\n")
	writeFile(t, filepath.Join(root, ".git"), "gitdir: "+realGitDir+"\n")
	env := instructions.DetectEnvironment(root, root, time.Now())
	if env.GitBranch != "worktree-branch" {
		t.Fatalf("GitBranch = %q, want worktree-branch", env.GitBranch)
	}
}

// TestEnvironmentRenderGolden 逐字节钉住 <environment> 块。
func TestEnvironmentRenderGolden(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	env := instructions.Environment{
		WorkspaceRoot: "/ws",
		Cwd:           "/ws/sub",
		OS:            "linux",
		Arch:          "amd64",
		Time:          now,
		GitBranch:     "main",
		TempDir:       "/tmp",
	}
	want := "<environment>\n" +
		"workspace: /ws\n" +
		"cwd: /ws/sub\n" +
		"platform: linux/amd64\n" +
		"date: 2026-11-17\n" +
		"git: branch=main dirty=unknown\n" +
		"temp: /tmp\n" +
		"</environment>"
	if got := env.Render(); got != want {
		t.Fatalf("Render mismatch:\ngot  %q\nwant %q", got, want)
	}
}

// TestEnvironmentRenderDateIsUTC 证明 date 用 UTC 日期（跨时区稳定）。
func TestEnvironmentRenderDateIsUTC(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 11, 17, 7, 30, 0, 0, zone) // UTC 是 2026-11-16
	env := instructions.Environment{OS: "linux", Arch: "amd64", Time: now}
	if !strings.Contains(env.Render(), "date: 2026-11-16") {
		t.Fatalf("Render = %q, want the UTC date", env.Render())
	}
}

// TestEnvironmentRenderNotARepo 证明非 git 目录渲染为 not-a-repo。
func TestEnvironmentRenderNotARepo(t *testing.T) {
	env := instructions.Environment{WorkspaceRoot: "/ws", Cwd: "/ws", OS: "linux", Arch: "amd64", Time: time.Now()}
	if got := env.Render(); !strings.Contains(got, "git: not-a-repo") {
		t.Fatalf("Render = %q, want git: not-a-repo", got)
	}
}

// TestEnvironmentRenderDirtyYes 证明宿主显式设置的脏状态渲染为 dirty=yes。
func TestEnvironmentRenderDirtyYes(t *testing.T) {
	env := instructions.Environment{GitBranch: "main", GitDirty: instructions.GitDirtyYes, OS: "linux", Arch: "amd64", Time: time.Now()}
	if got := env.Render(); !strings.Contains(got, "git: branch=main dirty=yes") {
		t.Fatalf("Render = %q, want dirty=yes", got)
	}
}

// TestEnvironmentRenderRecentFiles 证明可选的 RecentFiles 只在非空时渲染。
func TestEnvironmentRenderRecentFiles(t *testing.T) {
	base := instructions.Environment{OS: "linux", Arch: "amd64", Time: time.Now()}
	if strings.Contains(base.Render(), "recent:") {
		t.Fatalf("Render = %q, want no recent line when empty", base.Render())
	}
	withFiles := base
	withFiles.RecentFiles = []string{"a.go", "b.go"}
	if got := withFiles.Render(); !strings.Contains(got, "recent: a.go, b.go") {
		t.Fatalf("Render = %q, want the recent line", got)
	}
}

// TestEnvironmentRenderIsStable 证明同一 Environment 两次渲染字节相同。
func TestEnvironmentRenderIsStable(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	env := instructions.Environment{WorkspaceRoot: "/ws", Cwd: "/ws", OS: "linux", Arch: "arm64", Time: now, GitBranch: "main", TempDir: "/tmp"}
	first := env.Render()
	if second := env.Render(); first != second {
		t.Fatalf("Render is not stable:\nfirst  %q\nsecond %q", first, second)
	}
	if strings.Contains(first, now.Format(time.RFC3339)) {
		t.Fatalf("Render embeds a full timestamp: %q", first)
	}
}

// --- 小工具 ---------------------------------------------------------------

// itoa 避免在测试里引入 strconv（保持断言字符串直观）。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// utf8Valid 报告文本是否为合法 UTF-8。
func utf8Valid(text string) bool {
	return strings.ToValidUTF8(text, "\uFFFD") == text
}

// ensureError 断言 err 非 nil（compile-time 保证标准库 errors 被使用）。
