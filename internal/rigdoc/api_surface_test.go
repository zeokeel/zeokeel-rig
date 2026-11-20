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
//   - **同名值再导出**（例如 `tools/jobs.KindBash = background.KindBash`、
//     `memory.ErrNotFound = memwiki.ErrNotFound`）：与类型别名同类，宿主用公共
//     名字书写那个值；
//   - internal 包自己的导出声明（本测试不扫描 internal 目录）。
//
// 明确不允许 dot-import：点导入无法追踪标识符来自哪个包，命中即报，请改别名导入。
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

// TestSurfaceGuardSeesDotImportsAndInferredValues 是守护自身的变异回归：它曾
// 有两条语法级盲区，实测都放行真泄漏——
//
//  1. dot-import（`import . "…/tools/internal/background"`）：导入局部名是 "."，
//     旧实现直接跳过，于是 `func LeakDot(view JobView)`、`struct{ JobView }` 都
//     不可见，宿主拿到的仍是 internal 具名类型；
//  2. 推断类型的包级 `var`/`const`（`var LeakInferred = background.StatusRunning`）：
//     旧实现在 Type == nil 时直接放行，而且不计入 decls。
//
// 别名导入（含公共类型别名再导出）必须继续放行，否则这条守护会误伤刻意设计。
func TestSurfaceGuardSeesDotImportsAndInferredValues(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "dot-import",
			src: `package leak

import . "github.com/zeokeel/zeokeel-rig/tools/internal/background"

func LeakDot(view JobView) Status { return view.Status }
`,
			want: []string{"dot-import", "改用别名"},
		},
		{
			name: "推断类型的包级 var/const",
			src: `package leak

import "github.com/zeokeel/zeokeel-rig/tools/internal/background"

var LeakInferred = background.StatusRunning

const LeakConst = background.KindBash
`,
			want: []string{"LeakInferred", "LeakConst", "background.StatusRunning"},
		},
		{
			name: "别名导入与公共类型别名仍然放行",
			src: `package clean

import bg "github.com/zeokeel/zeokeel-rig/tools/internal/background"

type Notice = bg.Notice

var _ bg.ProcessSpec
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offenders := scanSourceSurface(t, tc.src)
			if len(tc.want) == 0 {
				if len(offenders) != 0 {
					t.Errorf("干净样例被误报: %v", offenders)
				}
				return
			}
			joined := strings.Join(offenders, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("守护没有报出 %q；offenders = %v", want, offenders)
				}
			}
		})
	}
}

// scanSourceSurface 用与真实扫描同一条路径检查一段源码。
func scanSourceSurface(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "leak.go", src, 0)
	if err != nil {
		t.Fatalf("parse 探针源码: %v", err)
	}
	_, offenders := fileSurfaceOffenders(file, fset, "leak.go")
	return offenders
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
		scannedPackages[filepath.Dir(path)] = true
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		decls, fileOffenders := fileSurfaceOffenders(file, fset, relative)
		scanned.decls += decls
		offenders = append(offenders, fileOffenders...)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历模块 %s: %v", root, err)
	}
	scanned.packages = len(scannedPackages)
	return offenders, scanned
}

// fileSurfaceOffenders 返回一个文件里的全部泄漏项，以及计入的导出声明数。
//
// 除了签名里显式写出 internal 类型，这里还兜住两条语法级盲区：dot-import
// （无法判断某个裸标识符来自哪个包，保守地"出现即报"）与推断类型的包级
// `var`/`const`（初始化表达式里的 internal 选择器同样是泄漏）。
func fileSurfaceOffenders(file *ast.File, fset *token.FileSet, relative string) (decls int, offenders []string) {
	if spec, path, ok := internalDotImport(file); ok {
		position := fset.Position(spec.Pos())
		offenders = append(offenders, relative+":"+strconv.Itoa(position.Line)+
			" dot-import 了 internal 包 "+path+"：请改用别名导入（点导入会让守护与宿主都无法判断某个名字来自 internal）")
	}
	aliases := internalImportAliases(file)
	decls, leaks := inspectFileSurface(file, aliases)
	for _, leak := range leaks {
		position := fset.Position(leak.selector.Pos())
		offenders = append(offenders, relative+":"+strconv.Itoa(position.Line)+" "+leak.label+
			" 使用了 "+leak.packageName+"."+leak.selector.Sel.Name+"（"+aliases[leak.packageName]+"）")
	}
	return decls, offenders
}

// isInternalImportPath 报告 import 路径是否指向本模块的 internal 包。
func isInternalImportPath(path string) bool {
	return strings.Contains(path, "/internal/") || strings.HasSuffix(path, "/internal") ||
		strings.HasPrefix(path, "internal/")
}

// internalDotImport 报告文件是否 dot-import 了 internal 包，并返回那个 import 与
// 路径。点导入把 internal 包的名字直接注入文件作用域，语法级检查无法再判断某个
// 标识符来自哪里，因此这里保守地"出现即报"，要求改用别名。
func internalDotImport(file *ast.File) (spec *ast.ImportSpec, path string, ok bool) {
	for _, candidate := range file.Imports {
		path := strings.Trim(candidate.Path.Value, `"`)
		if candidate.Name == nil || candidate.Name.Name != "." || !isInternalImportPath(path) {
			continue
		}
		return candidate, path, true
	}
	return nil, "", false
}

// internalImportAliases 返回文件里指向 internal 包的 import 局部名到 import
// 路径的映射。dot-import 由 internalDotImport 单独处理。
func internalImportAliases(file *ast.File) map[string]string {
	aliases := make(map[string]string)
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if !isInternalImportPath(path) {
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

// isDirectInternalReexport 报告一个初始化表达式是否是同名再导出（`var Foo =
// pkg.Foo`）。这是值层面的别名，允许；改名后的推断类型才是泄漏。
func isDirectInternalReexport(name string, value ast.Expr) bool {
	selector, ok := value.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == name
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
			// 推断类型（var X = background.StatusRunning）同样是泄漏：宿主拿到的
			// 仍是 internal 具名类型。因此 Type == nil 时也要看初始化表达式，并且
			// 同样计入 decls（否则“没写类型”就等于从守护里消失）。
			exported := make([]string, 0, len(decl.Names))
			for _, name := range decl.Names {
				if name.IsExported() {
					exported = append(exported, name.Name)
				}
			}
			if len(exported) == 0 {
				return true
			}
			decls += len(exported)
			label := "变量 " + strings.Join(exported, "/")
			check(label, decl.Type)
			if decl.Type == nil {
				// 同名值再导出（`var KindBash = background.KindBash`、
				// `memory.ErrNotFound = memwiki.ErrNotFound`）与类型别名同类，是刻意
				// 的公共再导出：宿主用公共名字书写那个值。真正要拦的是**改名**后的
				// 推断类型变量——宿主拿到 internal 具名类型却无法书写。
				for index, value := range decl.Values {
					if index < len(decl.Names) && isDirectInternalReexport(decl.Names[index].Name, value) {
						continue
					}
					check(label, value)
				}
			}
		}
		return true
	})
	return decls, leaks
}
