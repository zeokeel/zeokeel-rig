package rigdoc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestExportedAPIsDoNotLeakInternalTypes 是公开 API 表面的守护：导出包的导出
// 函数/方法/字段（以及导出的具名类型声明）不得在签名里出现 internal/* 包的具名
// 类型。
//
// 理由：internal 包是**实现细节**，宿主既不能 import 它，也不能在自己的代码里
// 命名它。一旦 `func (m *Manager) Wait() (background.JobView, bool, error)` 这样
// 的签名出现在公共包上，宿主就只能靠类型推断摸黑使用返回值，SDK 也无法在不破坏
// 兼容面的前提下换实现。
//
// 允许的形态（不是泄漏）：
//
//   - `any`、匿名 struct、接口，以及公共包自己定义的类型；
//   - 公共包里的**类型别名**（例如 `tools/jobs.Notice = background.Notice`、
//     `tools/memory.Wiki = memwiki.Wiki`）：别名是刻意的公共再导出，宿主可以用
//     别名书写这个类型；
//   - internal 包自己的导出声明（本测试不扫描 internal 目录）。
//
// 检查是语法级的：只看签名里是否**写出**了 internal 包的限定类型（形如
// background.Notice）。它不追求 go/types 的完整解析，但足以在评审/CI 里拦住
// "把内部类型直接摆上公共签名"这类回归。
func TestExportedAPIsDoNotLeakInternalTypes(t *testing.T) {
	offenders, scanned := scanExportedSurfaceForInternalTypes(t, filepath.Join("..", ".."))

	if scanned.packages < 50 {
		t.Fatalf("只扫描到 %d 个导出包，模块应有 70+ 个 Go 包（其中 50+ 个非 main 库包）：守护测试可能扫空了", scanned.packages)
	}
	if scanned.decls < 1000 {
		t.Fatalf("只看到 %d 个导出声明：守护测试可能没有真正遍历到 API 表面", scanned.decls)
	}
	if len(offenders) > 0 {
		t.Fatalf("公共 API 签名里出现 internal 包类型（宿主无法命名这些类型）：\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// surfaceScan 记一次扫描的规模，用来识别"测试扫空了却假装通过"。
type surfaceScan struct {
	packages int
	decls    int
}

// scanExportedSurfaceForInternalTypes 遍历模块里的非 internal 包，返回泄漏项与
// 扫描规模。
func scanExportedSurfaceForInternalTypes(t *testing.T, root string) (offenders []string, scanned surfaceScan) {
	t.Helper()
	fset := token.NewFileSet()
	// 同一个目录里的多个文件算一个包；空目录（没有 .go 文件）不算。
	scannedPackages := make(map[string]bool)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			name := info.Name()
			// internal 目录是允许的（内部自用）；隐藏目录/下划线目录不是 Go 包；
			// testdata/vendor 不参与构建。
			if path != root && (name == "internal" || name == "testdata" || name == "vendor" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		// main 包不是可 import 的库 API。
		if file.Name.Name == "main" {
			return nil
		}
		aliases := internalImportAliases(file)
		scannedPackages[filepath.Dir(path)] = true
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		decls, leaks := inspectFileSurface(file, aliases)
		scanned.decls += decls
		for _, leak := range leaks {
			position := fset.Position(leak.selector.Pos())
			offenders = append(offenders, relative+":"+strconv.Itoa(position.Line)+" "+leak.label+
				" 使用了 "+leak.packageName+"."+leak.selector.Sel.Name+"（"+aliases[leak.packageName]+"）")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历模块 %s: %v", root, err)
	}
	scanned.packages = len(scannedPackages)
	return offenders, scanned
}

// internalImportAliases 返回文件里指向 internal 包的 import 局部名到 import
// 路径的映射。
func internalImportAliases(file *ast.File) map[string]string {
	aliases := make(map[string]string)
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if !strings.Contains(path, "/internal/") && !strings.HasSuffix(path, "/internal") &&
			!strings.HasPrefix(path, "internal/") {
			continue
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		aliases[name] = path
	}
	return aliases
}

// internalTypeUse 是一处"公共签名里写了 internal 类型"的发现。
type internalTypeUse struct {
	label       string
	packageName string
	selector    *ast.SelectorExpr
}

// inspectFileSurface 遍历文件里的导出声明，返回导出声明数量与泄漏项。
func inspectFileSurface(file *ast.File, aliases map[string]string) (decls int, leaks []internalTypeUse) {
	check := func(label string, node ast.Node) {
		if node == nil {
			return
		}
		ast.Inspect(node, func(n ast.Node) bool {
			selector, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if _, isInternal := aliases[ident.Name]; isInternal {
				leaks = append(leaks, internalTypeUse{label: label, packageName: ident.Name, selector: selector})
			}
			return true
		})
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch decl := node.(type) {
		case *ast.FuncDecl:
			if !decl.Name.IsExported() {
				return true
			}
			decls++
			if decl.Recv != nil {
				check("方法 "+decl.Name.Name, decl.Recv)
			}
			check("函数 "+decl.Name.Name, decl.Type)
		case *ast.TypeSpec:
			if !decl.Name.IsExported() {
				return true
			}
			decls++
			// 类型别名（type Foo = background.Foo）是允许的公共再导出。
			if decl.Assign != 0 {
				return true
			}
			check("类型 "+decl.Name.Name, decl.Type)
		case *ast.ValueSpec:
			if decl.Type == nil {
				return true
			}
			for _, name := range decl.Names {
				if !name.IsExported() {
					continue
				}
				decls++
				check("变量 "+name.Name, decl.Type)
			}
		}
		return true
	})
	return decls, leaks
}
