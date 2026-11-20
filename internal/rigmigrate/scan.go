package rigmigrate

// 扫描器实现：只读、只解析，不做类型检查。
//
// 为什么可以在"编译不过"的目录上工作：宿主升级时正是这种状态，而 go/parser
// 只要求语法正确。这也意味着匹配以**名字**为准——它对符号表负责，不对类型系统
// 负责。正因如此，两处必须收窄，否则会把宿主自己的同名东西算成 rig 的：
//
//   - import 只在**真在本仓模块前缀下**时才折成 rig 包名：宿主自己的
//     `github.com/host/secrets`、`github.com/host/internal/kit` 与 rig 的
//     `secrets`/`kit` 同基名，但不是同一个包；
//   - 三段条目（`pkg.Type.Member`）只报**接收者被声明成该包限定类型**的选择器
//     （`cfg kit.Config` 里的 `cfg.Redactor` 命中；宿主本地 struct 的
//     `s.Redactor` 不命中）。不做类型推导，`cfg := load()` 这类拿不到类型的
//     接收者不参与匹配——宁可少报一条，也不给宿主的本地字段扣上 rig 的帽子。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// 命中类别，出现在 JSON 的 kind 字段与文本输出的动词里。
const (
	// KindRemoved 表示符号已删除。
	KindRemoved = "removed"
	// KindSignature 表示符号还在但签名收紧（旧调用形态编译不过）。
	KindSignature = "signature"
	// KindImport 表示整个包被删除。
	KindImport = "import"
)

// addFinding 收集一条命中（位置、类别、旧符号、替代做法、备注）。
type addFinding func(pos token.Pos, kind, old, newText, note string)

// Finding 是一条命中：哪个文件的哪一行、旧写法是什么、该换成什么。
type Finding struct {
	File   string `json:"file"`   // 相对扫描根，/ 分隔
	Line   int    `json:"line"`   // 1 起
	Column int    `json:"column"` // 1 起
	Kind   string `json:"kind"`   // removed / signature / import
	Old    string `json:"old"`    // 旧符号（或旧 import 路径）
	New    string `json:"new"`    // 替代做法
	Note   string `json:"note"`   // 备注
}

// String 渲染人读的一行：文件:行:列、旧符号、替代做法与备注。
func (f Finding) String() string {
	verb := "was removed"
	switch f.Kind {
	case KindSignature:
		verb = "changed signature"
	case KindImport:
		verb = "was removed"
	}
	return fmt.Sprintf("%s:%d:%d: %s %s; use %s - %s", f.File, f.Line, f.Column, f.Old, verb, f.New, f.Note)
}

// Scan 扫描 dir 下的 .go 文件，返回全部命中（按文件、行、列排序）。
//
// 会跳过 vendor/、testdata/、node_modules/ 以及 . 与 _ 开头的目录：宿主的
// vendor 里可能有旧版 SDK 的副本，报它只是噪音；样例目录也不是宿主代码。
// 目录不存在、不是目录、或某个 .go 文件语法错误都返回 error（调用方折成退出码 2），
// 绝不把"读不懂"当成"干净"。
func Scan(dir string) ([]Finding, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", dir)
	}

	findings := make([]Finding, 0, 8)
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == dir {
				return nil
			}
			if skippedDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		fileFindings, scanErr := scanFile(dir, p)
		if scanErr != nil {
			return scanErr
		}
		findings = append(findings, fileFindings...)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		return a.Old < b.Old
	})
	return findings, nil
}

// skippedDir 报告某个子目录是否不参与扫描。
func skippedDir(name string) bool {
	if name == "" {
		return true
	}
	switch name {
	case "vendor", "testdata", "node_modules":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// scanFile 解析单个文件并收集命中。
func scanFile(root, filename string) ([]Finding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	rel := filename
	if r, relErr := filepath.Rel(root, filename); relErr == nil {
		rel = r
	}
	rel = filepath.ToSlash(rel)

	// 本地名 -> **完整** import 路径（别名 import 也要认）。点导入与空导入拿不到
	// 限定名，跳过。只留基名会让宿主自有的同名包被当成 rig 的包，所以折基名的
	// 动作交给 rigBase：它先验模块前缀。
	imports := make(map[string]string, len(file.Imports))
	importPaths := make(map[*ast.ImportSpec]string, len(file.Imports))
	for _, spec := range file.Imports {
		importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			continue
		}
		importPaths[spec] = importPath
		local := path.Base(importPath)
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				continue
			}
			local = spec.Name.Name
		}
		imports[local] = importPath
	}
	// rigBase 把本地包名折成 rig 包基名；不是本仓的包返回 false。
	rigBase := func(local string) (string, bool) {
		importPath, ok := imports[local]
		if !ok {
			return "", false
		}
		return rigPackageBase(importPath)
	}

	var findings []Finding
	add := func(pos token.Pos, kind string, old, newText, note string) {
		position := fset.Position(pos)
		findings = append(findings, Finding{
			File:   rel,
			Line:   position.Line,
			Column: position.Column,
			Kind:   kind,
			Old:    old,
			New:    newText,
			Note:   note,
		})
	}

	// 已删除的包：命中点是 import 声明本身。
	for spec, importPath := range importPaths {
		for _, removed := range removedImports {
			if importPath == removed.Path {
				add(spec.Pos(), KindImport, removed.Path, removed.New, removed.Note)
			}
		}
	}

	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.SelectorExpr:
			// 两段条目：pkg.Name，包必须是从本仓 import 来的（含别名）。
			if ident, ok := typed.X.(*ast.Ident); ok {
				if base, imported := rigBase(ident.Name); imported {
					for _, entry := range removedSymbols {
						if entry.Args != 0 {
							continue
						}
						if pkg, name, ok := splitTwo(entry.Old); ok && pkg == base && name == typed.Sel.Name {
							add(typed.Pos(), KindRemoved, entry.Old, entry.New, entry.Note)
						}
					}
				}
			}
			// 三段条目不走这里：它们需要接收者的声明类型，按函数作用域在
			// matchTypedSelectors 里匹配（见下方）。
		case *ast.CallExpr:
			// 签名收紧类条目：只有实参个数等于旧形态才算命中。
			selector, ok := typed.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			base, imported := rigBase(ident.Name)
			if !imported {
				return true
			}
			for _, entry := range removedSymbols {
				if entry.Args == 0 {
					continue
				}
				if pkg, name, ok := splitTwo(entry.Old); ok && pkg == base && name == selector.Sel.Name && len(typed.Args) == entry.Args {
					add(typed.Pos(), KindSignature, entry.Old, entry.New, entry.Note)
				}
			}
		case *ast.CompositeLit:
			// 键值字面量：`kit.Config{Redactor: nil}`，类型与字段都对得上才报。
			typeSelector, ok := typed.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := typeSelector.X.(*ast.Ident)
			if !ok {
				return true
			}
			base, imported := rigBase(ident.Name)
			if !imported {
				return true
			}
			for _, entry := range removedSymbols {
				pkg, typeName, member, ok := splitThree(entry.Old)
				if !ok || pkg != base || typeName != typeSelector.Sel.Name {
					continue
				}
				for _, elt := range typed.Elts {
					keyValue, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := keyValue.Key.(*ast.Ident)
					if !ok || key.Name != member {
						continue
					}
					add(keyValue.Pos(), KindRemoved, entry.Old, entry.New, entry.Note)
				}
			}
		}
		return true
	})

	// 三段条目：按函数作用域判定接收者的声明类型。
	matchTypedSelectors(file, rigBase, add)

	return dedupe(findings), nil
}

// rigPackageBase 把 import 路径折成 rig 的包基名。
//
// 只有真在本仓模块前缀下的路径才算：宿主自己的 `github.com/host/internal/kit`
// 与 rig 的 `kit` 同基名，但不是同一个包——按基名匹配会让"已经迁移干净"的宿主
// 收到一批假红。
func rigPackageBase(importPath string) (string, bool) {
	if importPath == modulePath {
		return "rig", true
	}
	if !strings.HasPrefix(importPath, modulePrefix) {
		return "", false
	}
	return path.Base(importPath), true
}

// rigType 是"包基名 + 类型名"，用于把选择器接收者对上三段条目的类型。
type rigType struct {
	pkg  string // rig 包基名（kit / sandbox / …）
	name string // 类型名（Config / Policy / …）
}

// matchTypedSelectors 匹配三段条目（`pkg.Type.Member`）：只报接收者**被声明成
// 该包限定类型**的选择器。宿主本地的同名成员（`s.ProtectedPaths`、`m.Redactor`）
// 不报——把它们的输出归因成 `sandbox.Policy.ProtectedPaths` 只会让宿主去改本来
// 正确的代码。
//
// 类型表按函数切分：同一个文件里两个函数各有一个叫 `cfg` 的参数、类型不同时，
// 文件级的一张表会让两者互相污染。刻意不做类型推导（扫描器不类型检查，宿主这时
// 也编译不过），所以 `x := load()` 这类接收者不参与匹配。
//
// 遮蔽规则是"按名字"而不是"按类型"：**任意**局部声明（参数、`var`、`:=`、闭包参数、
// range 变量）出现之后，该名字就不再按外层类型匹配——`func(cfg localConfig)` 与块内
// `cfg := local{}` 都会让后面的 `cfg.Redactor` 变成宿主自有字段。判断依据只是"同名
// 局部声明出现了"，不需要类型检查。
func matchTypedSelectors(file *ast.File, rigBase func(string) (string, bool), add addFinding) {
	// 包级变量先入表：`var p sandbox.Policy`。
	pkgEnv := make(map[string]rigType)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if ok {
				recordRigTyped(valueSpec.Names, valueSpec.Type, rigBase, pkgEnv)
			}
		}
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		env := make(map[string]rigType, len(pkgEnv)+4)
		for name, ref := range pkgEnv {
			env[name] = ref
		}
		if fn.Recv != nil {
			for _, field := range fn.Recv.List {
				recordRigTyped(field.Names, field.Type, rigBase, env)
			}
		}
		if fn.Type != nil && fn.Type.Params != nil {
			for _, field := range fn.Type.Params.List {
				recordRigTyped(field.Names, field.Type, rigBase, env)
			}
		}
		// 单趟遍历：源码里先声明后使用，任意同名局部声明都会遮蔽外层类型。
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.ValueSpec:
				recordRigTyped(typed.Names, typed.Type, rigBase, env)
			case *ast.AssignStmt:
				recordRigAssign(typed, rigBase, env)
			case *ast.FuncLit:
				// 闭包参数同样是局部声明：`func(cfg localConfig)` 之后外层那个
				// kit.Config 的 cfg 在这个闭包里已经不可见。
				if typed.Type != nil && typed.Type.Params != nil {
					for _, field := range typed.Type.Params.List {
						recordRigTyped(field.Names, field.Type, rigBase, env)
					}
				}
			case *ast.RangeStmt:
				if typed.Tok == token.DEFINE {
					shadowNames([]ast.Expr{typed.Key, typed.Value}, env)
				}
			case *ast.SelectorExpr:
				ident, ok := typed.X.(*ast.Ident)
				if !ok {
					return true
				}
				ref, known := env[ident.Name]
				if !known {
					return true
				}
				for _, entry := range removedSymbols {
					pkg, typeName, member, ok := splitThree(entry.Old)
					if !ok || pkg != ref.pkg || typeName != ref.name || member != typed.Sel.Name {
						continue
					}
					add(typed.Pos(), KindRemoved, entry.Old, entry.New, entry.Note)
				}
			}
			return true
		})
	}
}

// recordRigTyped 把一批标识符按类型表达式记进类型表。
//
// 无论类型是不是 rig 包限定，这些名字都是**局部声明**：先把它们从类型表里删掉，只有
// 类型确实是 rig 包限定时才重新记回去。这样 `var cfg localConfig` / `func(cfg local)`
// 之后，外层的 `cfg kit.Config` 就不再被当作接收者类型（否则会把宿主自有字段报成
// 旧符号）。expr 为 nil 表示"类型看不出来但确实声明了"，只做遮蔽。
func recordRigTyped(names []*ast.Ident, expr ast.Expr, rigBase func(string) (string, bool), env map[string]rigType) {
	if len(names) == 0 {
		return
	}
	shadowIdents(names, env)
	if expr == nil {
		return
	}
	ref, ok := rigTypeOf(expr, rigBase)
	if !ok {
		return
	}
	for _, name := range names {
		env[name.Name] = ref
	}
}

// shadowIdents 把同名局部声明从类型表里删掉（`_` 不绑定任何东西，跳过）。
func shadowIdents(names []*ast.Ident, env map[string]rigType) {
	for _, name := range names {
		if name != nil && name.Name != "_" {
			delete(env, name.Name)
		}
	}
}

// shadowNames 对"带各自身份的声明位置"做同样的事（range 的 key/value 等）。
func shadowNames(exprs []ast.Expr, env map[string]rigType) {
	for _, expr := range exprs {
		if ident, ok := expr.(*ast.Ident); ok && ident.Name != "_" {
			delete(env, ident.Name)
		}
	}
}

// recordRigAssign 记下 `x := pkg.Type{}` / `x := &pkg.Type{}`：只有复合字面量能看出类型。
//
// `:=` 本身就是局部声明：先把左侧所有名字从类型表里删掉（含 `cfg := load()` 这类看
// 不出类型的），再按右侧复合字面量把能确定的类型记回去。
func recordRigAssign(stmt *ast.AssignStmt, rigBase func(string) (string, bool), env map[string]rigType) {
	if stmt.Tok != token.DEFINE {
		return
	}
	for _, lhs := range stmt.Lhs {
		shadowNames([]ast.Expr{lhs}, env)
	}
	for i, rhs := range stmt.Rhs {
		if i >= len(stmt.Lhs) {
			return
		}
		name, ok := stmt.Lhs[i].(*ast.Ident)
		if !ok {
			continue
		}
		recordRigTyped([]*ast.Ident{name}, compositeLitType(rhs), rigBase, env)
	}
}

// rigTypeOf 从类型表达式里取 rig 包限定类型；`*sandbox.Policy` 与 `sandbox.Policy` 同义。
func rigTypeOf(expr ast.Expr, rigBase func(string) (string, bool)) (rigType, bool) {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return rigTypeOf(typed.X, rigBase)
	case *ast.SelectorExpr:
		ident, ok := typed.X.(*ast.Ident)
		if !ok {
			return rigType{}, false
		}
		pkg, ok := rigBase(ident.Name)
		if !ok {
			return rigType{}, false
		}
		return rigType{pkg: pkg, name: typed.Sel.Name}, true
	}
	return rigType{}, false
}

// compositeLitType 剥掉取地址后返回复合字面量的类型表达式；不是复合字面量时返回 nil。
func compositeLitType(expr ast.Expr) ast.Expr {
	switch typed := expr.(type) {
	case *ast.UnaryExpr:
		return compositeLitType(typed.X)
	case *ast.CompositeLit:
		if typed.Type != nil {
			return typed.Type
		}
	}
	return nil
}

// splitTwo 判断 Old 是否是 `pkg.Name`。
func splitTwo(old string) (pkg, name string, ok bool) {
	parts := strings.Split(old, ".")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// splitThree 判断 Old 是否是 `pkg.Type.Member`。
func splitThree(old string) (pkg, typeName, member string, ok bool) {
	parts := strings.Split(old, ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// dedupe 去掉同一位置同一条目的重复命中（同一条可能被不同匹配路径看到）。
func dedupe(findings []Finding) []Finding {
	if len(findings) < 2 {
		return findings
	}
	type key struct {
		line, column int
		old          string
	}
	seen := make(map[key]bool, len(findings))
	out := findings[:0]
	for _, finding := range findings {
		k := key{line: finding.Line, column: finding.Column, old: finding.Old}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, finding)
	}
	return out
}
