package rigmigrate

// 扫描器实现：只读、只解析，不做类型检查。
//
// 为什么可以在"编译不过"的目录上工作：宿主升级时正是这种状态，而 go/parser
// 只要求语法正确。这也意味着匹配以**名字**为准——它对符号表负责，不对类型系统
// 负责；三段条目的成员选择器会命中任何同名成员，这是宽匹配的代价，换来的是
// 不漏报 `cfg.Redactor` 这种最常见写法。

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

	// 本地名 -> 包基名（别名 import 也要认）。点导入与空导入拿不到限定名，跳过。
	imports := make(map[string]string, len(file.Imports))
	importPaths := make(map[*ast.ImportSpec]string, len(file.Imports))
	for _, spec := range file.Imports {
		importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			continue
		}
		importPaths[spec] = importPath
		base := path.Base(importPath)
		local := base
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				continue
			}
			local = spec.Name.Name
		}
		imports[local] = base
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
			// 两段条目：pkg.Name，包必须是从 import 来的（含别名）。
			if ident, ok := typed.X.(*ast.Ident); ok {
				if base, imported := imports[ident.Name]; imported {
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
			// 三段条目：成员按名字宽匹配（`cfg.Redactor` 的接收者不可能是包名）。
			// 同名成员可能对应多条（例如 Redactor 同时出现在 kit.Config 与
			// agent.RunnerOptions 上），这里只报一条：优先给出"有具体替代做法"
			// 的那条，并在备注里点出其它同名旧符号，避免同一个位置刷两行。
			var candidates []Entry
			for _, entry := range removedSymbols {
				if _, _, member, ok := splitThree(entry.Old); ok && member == typed.Sel.Name {
					candidates = append(candidates, entry)
				}
			}
			if len(candidates) > 0 {
				pick := candidates[0]
				for _, candidate := range candidates {
					if !strings.HasPrefix(candidate.New, "（无替代）") {
						pick = candidate
						break
					}
				}
				note := pick.Note
				if len(candidates) > 1 {
					others := make([]string, 0, len(candidates)-1)
					for _, candidate := range candidates {
						if candidate.Old != pick.Old {
							others = append(others, candidate.Old)
						}
					}
					note = fmt.Sprintf("%s（同名成员 %s 也已删除，这里按成员名宽匹配）", note, strings.Join(others, "、"))
				}
				add(typed.Pos(), KindRemoved, pick.Old, pick.New, note)
			}
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
			base, imported := imports[ident.Name]
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
			base, imported := imports[ident.Name]
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

	return dedupe(findings), nil
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

// dedupe 去掉同一位置同一条目的重复命中（宽匹配与精确匹配可能同时看到一处）。
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
