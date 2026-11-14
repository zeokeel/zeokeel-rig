package procattr

// Guards the Windows console fix: the desktop host is a GUI-subsystem process,
// so a console child (cmd, PowerShell, a job, an MCP/LSP server) that Windows
// gives a fresh console window pops a black window for every tool call. Every
// place in this repository that starts a child process must therefore call
// HideWindow on the command before running it, so child windows start hidden on
// Windows (and the call is a no-op elsewhere).
//
// The check walks the source instead of running on Windows so it fails in Linux
// CI too, where the regression would otherwise only be noticed by users.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// spawnFuncs are the stdlib constructors that start a child process.
var spawnFuncs = map[string]bool{
	"Command":        true,
	"CommandContext": true,
}

// hideFunc is the preparation call each spawn must make.
const hideFunc = "HideWindow"

// TestEveryChildSpawnHidesWindow fails for any exec.Command/exec.CommandContext
// site whose command variable is not prepared with procattr.HideWindow.
func TestEveryChildSpawnHidesWindow(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, dir := range []string{"tools", "sandbox"} {
		walkErr := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				name := entry.Name()
				if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			for _, violation := range spawnViolations(t, path) {
				t.Errorf("%s: %s", filepath.ToSlash(path), violation)
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}
}

// spawnViolations reports the unprepared child spawns in one source file.
func spawnViolations(t *testing.T, path string) []string {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var violations []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		prepared := preparedVars(fn.Body)
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || assignment.Tok != token.DEFINE {
				return true
			}
			for index, rhs := range assignment.Rhs {
				if !isSpawnCall(rhs) {
					continue
				}
				line := fileSet.Position(rhs.Pos()).Line
				if index >= len(assignment.Lhs) {
					violations = append(violations, "spawn at line "+itoa(line)+" must assign the command to a variable and prepare it with procattr.HideWindow")
					continue
				}
				name, ok := assignment.Lhs[index].(*ast.Ident)
				if !ok || !prepared[name.Name] {
					violations = append(violations, "spawn at line "+itoa(line)+" is not prepared with procattr.HideWindow")
				}
			}
			return true
		})
		// Inline spawns (no command variable) cannot be prepared; report the
		// ones an assignment did not claim above.
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isSpawnCall(call) {
				return true
			}
			if !insideAssignment(fn.Body, call) {
				violations = append(violations,
					"inline spawn at line "+itoa(fileSet.Position(call.Pos()).Line)+
						" must assign the command to a variable and prepare it with procattr.HideWindow")
			}
			return true
		})
	}
	return violations
}

// preparedVars returns the command variables one function prepares.
func preparedVars(body *ast.BlockStmt) map[string]bool {
	prepared := map[string]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != hideFunc {
			return true
		}
		name, ok := selector.X.(*ast.Ident)
		if !ok || name.Name != "procattr" {
			return true
		}
		argument, ok := call.Args[0].(*ast.Ident)
		if ok {
			prepared[argument.Name] = true
		}
		return true
	})
	return prepared
}

// isSpawnCall reports whether call is exec.Command or exec.CommandContext.
func isSpawnCall(node ast.Node) bool {
	call, ok := node.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !spawnFuncs[selector.Sel.Name] {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "exec"
}

// insideAssignment reports whether call is the right-hand side of a := or =
// statement (so spawnViolations does not report it twice).
func insideAssignment(body *ast.BlockStmt, call *ast.CallExpr) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, rhs := range assignment.Rhs {
			if rhs == call {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// itoa avoids importing strconv for one line number.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
