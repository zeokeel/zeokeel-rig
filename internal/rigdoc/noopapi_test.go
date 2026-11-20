package rigdoc

// "保留符号但不干活"接口的守护测试。
//
// 背景：DSH 对齐时有一批能力被降级成「导出符号保留 + 语义 no-op + Deprecated 注释」
// （保护路径、拒绝 glob、kit 的三个保护开关）。这比删掉更糟：宿主编译通过、以为
// 设了边界，运行时什么都没发生。本用例把它们钉成"不得回来"：
//
//  1. 源码里不得再声明这些标识符（含未导出字段）；
//  2. 承载它们的整个文件不得回来；
//  3. 少数签名被收紧的导出函数，参数个数也一并钉住。
//
// 它只扫**非测试**源码：测试文件若引用这些名字本来就编译不过。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// removedNoOpDecls 是被删除的"保留符号但不干活"的声明。
//
// 名字写法与 go/ast 一致：方法写成 `Policy.ProtectedPaths`，函数/类型/变量写原名。
type removedNoOpDecl struct {
	dir  string // 仓库相对目录
	name string // 声明名（方法为 Type.Name）
	why  string // 替代做法（给回归者看的一句话）
}

var removedNoOpDecls = []removedNoOpDecl{
	// --- sandbox：保护路径 -----------------------------------------------
	// 策略层已没有"保护路径"这一概念。真需要根内局部只读，用可写根的
	// ReadOnlySubpaths（WithWritableRoot / WithReadOnlySubpaths）。
	{"sandbox", "WithProtectedPaths", "用 WithWritableRoot 的只读子路径"},
	{"sandbox", "Policy.AddProtectedPaths", "用 WithWritableRoot 的只读子路径"},
	{"sandbox", "Policy.ProtectedPaths", "用 WritableRoots()[].ReadOnlySubpaths"},
	{"sandbox", "Policy.WriteDenyPaths", "用 WritableRoots()[].ReadOnlySubpaths"},
	{"sandbox", "WithProtectMissingPaths", "已无缺失保护路径的概念"},
	{"sandbox", "Policy.ProtectMissingPaths", "已无缺失保护路径的概念"},
	{"sandbox", "WorkspaceMetadataPaths", "不再需要枚举「保护位置」"},
	{"sandbox", "WorkspaceNonGitMetadataPaths", "不再需要枚举「保护位置」"},
	{"sandbox", "ErrGrantProtected", "授权校验只看可写根与只读子路径"},
	// --- sandbox：拒绝 glob ----------------------------------------------
	// 隐藏路径（WithHidden / Hidden / GlobMatch）是**参与判定**的替代品。
	{"sandbox", "WithDeniedGlobs", "用 WithHidden"},
	{"sandbox", "Policy.DeniedGlobs", "用 WithHidden / HiddenPatterns"},
	// --- sandbox/osrunner -------------------------------------------------
	{"sandbox/osrunner", "ErrMissingProtected", "没有保护路径就不会有「缺失」"},
	// --- kit / tools/full -------------------------------------------------
	{"kit", "HomeSecretPaths", "工具面不做凭据位置保护"},
	{"tools/full", "AllOptions.ProtectWorkspaceMetadata", "已无保护路径"},
}

// removedStructFields 是被删除的结构体字段（结构体名 + 字段名）。
//
// 字段单独列：它们不构成顶层声明，但同样是「看起来在配置、实际不生效」的入口。
var removedStructFields = []struct {
	dir        string
	structName string
	field      string
	why        string
}{
	{"sandbox", "Policy", "protectedPaths", "策略层没有保护路径"},
	{"sandbox", "Policy", "deniedGlobs", "拒绝 glob 已删除"},
	{"sandbox", "Policy", "protectMissing", "已无缺失保护路径的概念"},
	{"sandbox", "Denial", "ProtectedPaths", "拒绝归因不再渲染保护路径"},
	{"sandbox/osrunner", "Profile", "Protected", "OS 后端从不渲染保护规则"},
	{"sandbox/osrunner", "Profile", "MissingProtected", "同上"},
	{"kit", "Config", "DisableProtection", "kit 从不追加保护路径，没有可关的东西"},
	{"kit", "Config", "ProtectPaths", "kit 从不追加保护路径"},
	{"kit", "Config", "ProtectWorkspaceMetadata", "kit 从不追加保护路径"},
	{"tools/sandboxstatus", "status", "PathProtection", "沙箱没有这一层强制，不必渲染"},
	{"tools/sandboxstatus", "RuntimeView", "UnprotectedPaths", "已无「声明了保护却保护不了」的路径"},
	{"tools/sandboxstatus", "RuntimeView", "UnprotectedWarning", "同上"},
}

// removedNoOpFiles 是承载上述实现的整个文件：删干净后不应再出现。
var removedNoOpFiles = []string{
	"sandbox/protected.go",
	"sandbox/osrunner/missing_protected.go",
}

// TestRemovedNoOpAPIsAreGone 断言这些「no-op 但保留导出」的接口没有回来。
func TestRemovedNoOpAPIsAreGone(t *testing.T) {
	root := moduleRoot(t)

	for _, file := range removedNoOpFiles {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(file))); err == nil {
			t.Errorf("%s 仍然存在：应为空壳 API 的承载文件，已整文件删除", file)
		}
	}

	decls := make(map[string]map[string]bool)
	for _, removed := range removedNoOpDecls {
		set, ok := decls[removed.dir]
		if !ok {
			set = packageDeclarations(t, filepath.Join(root, filepath.FromSlash(removed.dir)))
			decls[removed.dir] = set
		}
		if set[removed.name] {
			t.Errorf("%s: 仍然声明 %s（no-op 导出接口已删除；替代做法：%s）", removed.dir, removed.name, removed.why)
		}
	}

	fields := make(map[string]map[string]bool)
	for _, removed := range removedStructFields {
		key := removed.dir + "/" + removed.structName
		set, ok := fields[key]
		if !ok {
			set = structFieldNames(t, filepath.Join(root, filepath.FromSlash(removed.dir)), removed.structName)
			fields[key] = set
		}
		if set[removed.field] {
			t.Errorf("%s.%s.%s 仍然存在（替代做法：%s）", removed.dir, removed.structName, removed.field, removed.why)
		}
	}

	// ValidateWritableGrantPath 去掉了 protected 参数：钉住参数个数，防止有人
	// 为了"兼容"再把这一层加回来（加回来就一定又是个 no-op）。
	if got := funcParamCount(t, filepath.Join(root, "sandbox"), "ValidateWritableGrantPath"); got != 4 {
		t.Errorf("sandbox.ValidateWritableGrantPath 参数个数 = %d, want 4（path, roots, configured, granted；protected 已删除）", got)
	}
}

// packageDeclarations 返回目录下所有非测试 Go 文件的声明名集合。
//
// 方法写成 `Type.Name`，函数写成 `Name`，类型与包级变量/常量用原名。
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

// structFieldNames 返回目录里某个结构体类型声明的字段名集合（含内嵌字段）。
func structFieldNames(t *testing.T, dir, typeName string) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
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

// parseDirFiles 解析目录里的非测试 Go 文件（不看 build tag，Windows 文件也要扫）。
func parseDirFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
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
	if len(files) == 0 {
		t.Fatalf("目录 %q 里没有可解析的 Go 文件（路径写错了？）", dir)
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
	case *ast.IndexExpr: // 泛型接收者
		return receiverTypeName(typed.X)
	case *ast.IndexListExpr:
		return receiverTypeName(typed.X)
	}
	return ""
}
