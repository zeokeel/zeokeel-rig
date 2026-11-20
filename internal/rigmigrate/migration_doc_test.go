package rigmigrate_test

// docs/MIGRATION-v0.25.md 的守护测试。
//
// 迁移文档最容易烂掉的方式是"代码又删了一轮，文档还列着已经不存在的符号"或者
// "文档列了一个其实还活着的符号，宿主白改一遍"。这里双向钉住：
//
//  1. 文档表格第一列里的每个旧符号必须在符号表里（文档不得自说自话）；
//  2. 符号表里的每条必须出现在文档里（文档不得漏项）；
//  3. 删除类条目的旧符号在仓库源码里**不存在**；签名收紧类条目的符号还在，
//     但当前参数个数已经不是旧调用的形态（否则文档在骗人）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/internal/rigmigrate"
)

// migrationDocPath 是文档相对仓库根的位置。
const migrationDocPath = "docs/MIGRATION-v0.25.md"

// docSymbolRe 只接受两种旧符号写法：`pkg.Name` / `pkg.Type.Member`。
// 带参数、带空格的调用形态（`NewEventSink(w, r)`）不算。
var docSymbolRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z0-9_]+){1,2}$`)

// docImportRe 接受被整包删除的 import 路径（含域名里的点）。
var docImportRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$`)

// TestMigrationDocSymbolsMatchTable 断言文档与符号表互为子集。
func TestMigrationDocSymbolsMatchTable(t *testing.T) {
	docSymbols := readDocSymbols(t)

	want := make(map[string]bool)
	for _, entry := range rigmigrate.RemovedSymbols() {
		want[entry.Old] = true
	}
	for _, removed := range rigmigrate.RemovedImports() {
		want[removed.Path] = true
	}

	var missing []string
	for symbol := range want {
		if !docSymbols[symbol] {
			missing = append(missing, symbol)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s 漏了 %d 条符号表条目: %s", migrationDocPath, len(missing), strings.Join(missing, ", "))
	}

	var extra []string
	for symbol := range docSymbols {
		if !want[symbol] {
			extra = append(extra, symbol)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("%s 列了符号表里没有的旧符号: %s", migrationDocPath, strings.Join(extra, ", "))
	}
}

// TestMigrationDocSymbolsAreGoneFromCode 断言文档里"已删除"的说法与源码一致。
func TestMigrationDocSymbolsAreGoneFromCode(t *testing.T) {
	root := moduleRoot(t)

	for _, entry := range rigmigrate.RemovedSymbols() {
		dir, typeName, member := splitOld(t, entry.Old)
		decls := packageDeclarations(t, filepath.Join(root, filepath.FromSlash(dir)))
		if entry.Args > 0 {
			// 签名收紧：符号还在，但旧调用形态（Args 个实参）已经编译不过。
			if !decls[member] {
				t.Errorf("%s: 文档说 %s 只是改了签名，但源码里连声明都没有", dir, entry.Old)
				continue
			}
			if got := funcParamCount(t, filepath.Join(root, filepath.FromSlash(dir)), member); got == entry.Args {
				t.Errorf("%s: %s 的参数个数仍是 %d，文档写的「签名收紧」已经过时", dir, entry.Old, got)
			}
			continue
		}
		if decls[member] {
			t.Errorf("%s: 文档说 %s 已删除，但源码仍然声明它", dir, entry.Old)
		}
		if member == "" || typeName == "" {
			continue
		}
		// pkg.Type.Member：可能是方法，也可能是结构体字段，两种都查。
		if decls[typeName+"."+member] {
			t.Errorf("%s: 文档说 %s 已删除，但源码仍然声明这个方法", dir, entry.Old)
		}
		if structFieldNames(t, filepath.Join(root, filepath.FromSlash(dir)), typeName)[member] {
			t.Errorf("%s: 文档说 %s 已删除，但源码仍然声明这个字段", dir, entry.Old)
		}
	}

	for _, removed := range rigmigrate.RemovedImports() {
		dir := strings.TrimPrefix(removed.Path, modulePath+"/")
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir))); err == nil {
			t.Errorf("%s: 文档说包 %s 已删除，但目录仍然存在", migrationDocPath, removed.Path)
		}
	}
}

// TestMigrationDocWritableDiagnosticsAreAccurate 钉住文档里被审计抓到的两处失真：
//
//   - §5 说的是 `osrunner.Profile.Writable`，而这个字段不存在（真名 WritableRoots）；
//   - §2 说 `Preflight` / `sandbox_status` 会"如实说没有保护路径"，但代码里根本没有
//     这句话——它们只按档位说可写面。
func TestMigrationDocWritableDiagnosticsAreAccurate(t *testing.T) {
	doc := readMigrationDoc(t)
	if regexp.MustCompile(`Profile\.Writable\b`).MatchString(doc) {
		t.Errorf("%s 提到 Profile.Writable，但 osrunner.Profile 里只有 WritableRoots/ReadWrite/ReadOnly", migrationDocPath)
	}
	if !strings.Contains(doc, "Profile.WritableRoots") {
		t.Errorf("%s 没有给出可写面诊断的真实字段名 Profile.WritableRoots", migrationDocPath)
	}
	if strings.Contains(doc, "会如实说") {
		t.Errorf("%s 说 Preflight/sandbox_status \"会如实说没有保护路径\"，代码里没有这句话", migrationDocPath)
	}
	if !strings.Contains(doc, "没有可写目录") {
		t.Errorf("%s 没写 read-only 档（无 scratch 时）的真实说法：没有可写目录", migrationDocPath)
	}
	// §2 说 `sandbox_status` 的 `writable` 数组：实际顶层数组叫 `writableRoots`，
	// `writable` 只是 `tempWritable` 里的布尔。字段名写错比措辞失真更糟——宿主照着
	// 文档去读一个不存在的键。所以既查文档措辞，也查"文档提到的字段名真的在代码里"。
	if regexp.MustCompile("`writable` 数组").MatchString(doc) {
		t.Errorf("%s 说 sandbox_status 的 `writable` 数组：顶层数组的真名是 writableRoots"+
			"（`writable` 是 tempWritable 里的布尔）", migrationDocPath)
	}
	if !strings.Contains(doc, "`writableRoots`") {
		t.Errorf("%s 没有给出 sandbox_status 的真实字段名 writableRoots", migrationDocPath)
	}
	if !jsonFieldExists(t, filepath.Join(moduleRoot(t), "tools", "sandboxstatus"), "writableRoots") {
		t.Errorf("tools/sandboxstatus 里没有 json 字段 writableRoots：%s 的字段名已经过时", migrationDocPath)
	}
}

// TestMigrationDocSymbolsAreNotMethodForms 补上"两段符号以方法形态回归"的守护。
//
// 已有守护只看包级声明与该类型上的同名方法/字段，所以把 `kit.HomeSecretPaths`
// 做成 `Config` 的方法时它仍然通过：文档继续宣称已删除，宿主却能编译通过。
func TestMigrationDocSymbolsAreNotMethodForms(t *testing.T) {
	violations := methodFormViolations(t, moduleRoot(t), rigmigrate.RemovedSymbols())
	if len(violations) > 0 {
		t.Errorf("%s 说这些符号已删除，但它们在源码里以方法形态存在: %s", migrationDocPath, strings.Join(violations, "; "))
	}
}

// TestMethodFormGuardCatchesRegressedSymbol 用一份真实的 kit 包副本证明上面那条守护
// 会红：只在副本里加一个 `func (c Config) HomeSecretPaths()`，不改工作区。
func TestMethodFormGuardCatchesRegressedSymbol(t *testing.T) {
	src := filepath.Join(moduleRoot(t), "kit")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/zeokeel/zeokeel-rig\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	dst := filepath.Join(root, "kit")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(src, name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(dst, name), body, 0o644); writeErr != nil {
			t.Fatalf("write %s: %v", name, writeErr)
		}
	}
	probe := "package kit\n\n// 探针：把已删除的包级函数做成 Config 的方法。\nfunc (c Config) HomeSecretPaths() []string { return nil }\n"
	if err := os.WriteFile(filepath.Join(dst, "zz_probe.go"), []byte(probe), 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	violations := methodFormViolations(t, root, []rigmigrate.Entry{
		{Old: "kit.HomeSecretPaths", New: "（无替代）", Note: "探针"},
	})
	if len(violations) == 0 {
		t.Fatal("方法形态回归没有被抓到（这条守护会恒真）")
	}
	if !strings.Contains(strings.Join(violations, ";"), "kit.HomeSecretPaths") {
		t.Errorf("违规项里没有 kit.HomeSecretPaths: %v", violations)
	}
}

// methodFormViolations 返回"符号表里的两段符号以方法形态存在于源码"的清单。
func methodFormViolations(t *testing.T, root string, entries []rigmigrate.Entry) []string {
	t.Helper()
	methods := exportedMethodNames(t, root)
	var out []string
	for _, entry := range entries {
		parts := strings.Split(entry.Old, ".")
		if len(parts) != 2 {
			continue
		}
		if sites := methods[parts[1]]; len(sites) > 0 {
			out = append(out, entry.Old+"（以方法形态存在: "+strings.Join(sites, ", ")+"）")
		}
	}
	sort.Strings(out)
	return out
}

// exportedMethodNames 收集 root 下非 internal 包里的导出方法名，值为出现位置
// （相对路径:接收者类型）。internal/ 下的包宿主 import 不到，那里的同名方法不构成
// "文档说已删除、宿主却还能调"的矛盾，因此整目录跳过。
func exportedMethodNames(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := make(map[string][]string)
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			name := d.Name()
			if name == "internal" || name == "vendor" || name == "testdata" || name == "node_modules" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			rel = p
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || !ast.IsExported(fn.Name.Name) {
				continue
			}
			out[fn.Name.Name] = append(out[fn.Name.Name],
				filepath.ToSlash(rel)+":"+receiverTypeName(fn.Recv.List[0].Type))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %q 收集导出方法名: %v", root, err)
	}
	return out
}

// readDocSymbols 从迁移文档的表格第一列里取出所有旧符号/旧 import 路径。
func readDocSymbols(t *testing.T) map[string]bool {
	t.Helper()
	cells := codeSpansInFirstColumn(readMigrationDoc(t))
	out := make(map[string]bool)
	for _, cell := range cells {
		for _, symbol := range cell {
			if docSymbolRe.MatchString(symbol) || docImportRe.MatchString(symbol) {
				out[symbol] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s 的表格第一列里没有解析出任何旧符号", migrationDocPath)
	}
	return out
}

// readMigrationDoc 读出迁移文档原文。
func readMigrationDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(migrationDocPath)))
	if err != nil {
		t.Fatalf("读 %s: %v（迁移文档不存在就没法给宿主升级指引）", migrationDocPath, err)
	}
	return string(raw)
}

// codeSpansInFirstColumn 返回每个表格行第一列里的反引号代码片段。
func codeSpansInFirstColumn(doc string) [][]string {
	var out [][]string
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		fields := strings.Split(trimmed, "|")
		if len(fields) < 3 {
			continue
		}
		out = append(out, backticked(fields[1]))
	}
	return out
}

// backticked 取出一个单元格里所有反引号包的代码片段。
func backticked(cell string) []string {
	var out []string
	for {
		start := strings.Index(cell, "`")
		if start < 0 {
			return out
		}
		rest := cell[start+1:]
		end := strings.Index(rest, "`")
		if end < 0 {
			return out
		}
		out = append(out, strings.TrimSpace(rest[:end]))
		cell = rest[end+1:]
	}
}

// splitOld 把 Old 拆成（仓库目录、类型名、成员名）。
func splitOld(t *testing.T, old string) (dir, typeName, member string) {
	t.Helper()
	parts := strings.Split(old, ".")
	if len(parts) < 2 {
		t.Fatalf("符号表条目 %q 少于两段", old)
	}
	dir = packageDir(t, parts[0])
	member = parts[len(parts)-1]
	if len(parts) == 3 {
		typeName = parts[1]
	}
	return dir, typeName, member
}

// packageDir 把文档里的包名前缀映射到仓库目录。
func packageDir(t *testing.T, prefix string) string {
	t.Helper()
	dirs := map[string]string{
		"rig":           ".",
		"sandbox":       "sandbox",
		"secrets":       "secrets",
		"osrunner":      "sandbox/osrunner",
		"kit":           "kit",
		"agent":         "agent",
		"full":          "tools/full",
		"sqlite":        "session/sqlite",
		"sandboxstatus": "tools/sandboxstatus",
		"hosttools":     "tools/hosttools",
	}
	dir, ok := dirs[prefix]
	if !ok {
		t.Fatalf("未知包名前缀 %q（在 packageDir 里补一条映射）", prefix)
	}
	return dir
}

// modulePath 是根模块路径，用于把 import 路径折成目录。
const modulePath = "github.com/zeokeel/zeokeel-rig"

// moduleRoot 返回仓库根（测试的工作目录是 internal/rigmigrate）。
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %q has no go.mod: %v", root, err)
	}
	return root
}

// packageDeclarations 返回目录下所有非测试 Go 文件的声明名集合（方法写成 Type.Name）。
func packageDeclarations(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	for _, file := range parseDirFiles(t, dir) {
		for _, decl := range file.Decls {
			switch node := decl.(type) {
			case *ast.FuncDecl:
				if node.Recv != nil && len(node.Recv.List) > 0 {
					out[receiverTypeName(node.Recv.List[0].Type)+"."+node.Name.Name] = true
					continue
				}
				out[node.Name.Name] = true
			case *ast.GenDecl:
				for _, spec := range node.Specs {
					switch typed := spec.(type) {
					case *ast.TypeSpec:
						out[typed.Name.Name] = true
					case *ast.ValueSpec:
						for _, name := range typed.Names {
							out[name.Name] = true
						}
					}
				}
			}
		}
	}
	return out
}

// jsonFieldExists 报告目录下的结构体里是否有该 JSON 名字的字段（含 `name,omitempty`）。
//
// 符号表守护只管"文档列出的旧符号"，抓不到"文档写了一个不存在的字段名"这类失真；
// 对文档里点名的字段加一条轻量检查，代价只是多解析一个包。
func jsonFieldExists(t *testing.T, dir, jsonName string) bool {
	t.Helper()
	for _, file := range parseDirFiles(t, dir) {
		found := false
		ast.Inspect(file, func(node ast.Node) bool {
			structType, ok := node.(*ast.StructType)
			if !ok || structType.Fields == nil {
				return true
			}
			for _, field := range structType.Fields.List {
				if field.Tag == nil {
					continue
				}
				tag, err := strconv.Unquote(field.Tag.Value)
				if err != nil {
					continue
				}
				name := reflect.StructTag(tag).Get("json")
				if name == jsonName || strings.HasPrefix(name, jsonName+",") {
					found = true
					return false
				}
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// structFieldNames 返回目录里某个结构体的字段名集合（含内嵌字段）。
func structFieldNames(t *testing.T, dir, typeName string) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	if typeName == "" {
		return out
	}
	for _, file := range parseDirFiles(t, dir) {
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok || spec.Name.Name != typeName {
				return true
			}
			structType, ok := spec.Type.(*ast.StructType)
			if !ok || structType.Fields == nil {
				return true
			}
			for _, field := range structType.Fields.List {
				for _, name := range field.Names {
					out[name.Name] = true
				}
			}
			return false
		})
	}
	return out
}

// funcParamCount 返回目录里某个包级函数的参数个数；找不到时返回 -1。
func funcParamCount(t *testing.T, dir, name string) int {
	t.Helper()
	for _, file := range parseDirFiles(t, dir) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != name {
				continue
			}
			if fn.Type.Params == nil {
				return 0
			}
			count := 0
			for _, field := range fn.Type.Params.List {
				if len(field.Names) == 0 {
					count++
					continue
				}
				count += len(field.Names)
			}
			return count
		}
	}
	return -1
}

// parseDirFiles 解析目录里的非测试 Go 文件；目录不存在时返回空集（删除的包）。
func parseDirFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read dir %q: %v", dir, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		files = append(files, file)
	}
	return files
}

// receiverTypeName 取方法接收者的类型名（`*Policy` -> `Policy`）。
func receiverTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(typed.X)
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		return receiverTypeName(typed.X)
	case *ast.IndexListExpr:
		return receiverTypeName(typed.X)
	}
	return ""
}
