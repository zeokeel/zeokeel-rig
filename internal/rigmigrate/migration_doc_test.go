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
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// readDocSymbols 从迁移文档的表格第一列里取出所有旧符号/旧 import 路径。
func readDocSymbols(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(migrationDocPath)))
	if err != nil {
		t.Fatalf("读 %s: %v（迁移文档不存在就没法给宿主升级指引）", migrationDocPath, err)
	}
	cells := codeSpansInFirstColumn(string(raw))
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
