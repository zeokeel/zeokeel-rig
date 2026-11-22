package rigdoc_test

// 模块化守门测试。
//
// 产品定位是"模块化 AI SDK，给个人 agent 产品用"。模块化的定义不是"代码分了包"，
// 而是**每个能力都能被单独拿走用**。因此这里有两条硬性约束：
//
//  1. 叶子能力包不得依赖聚合层（`kit`）或工具装配层（`tools/full`）——
//     否则宿主想只用其中一项时会被迫拖进整棵树；
//  2. 根包 `rig` 必须保持仅依赖标准库——它是所有宿主与所有模块共享的契约层，
//     一旦它引入第三方依赖，整个 SDK 的依赖面就跟着变大。
//
// 这两条一旦被破坏，SDK 就会悄悄退化成"只能整套使用"，而这种事在 code review
// 里几乎看不出来。

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/zeokeel/zeokeel-rig"

// leafCapabilities 是所有"必须能独立使用"的能力包。
// 新增可选能力时请加进来——这张表就是 SDK 的模块清单。
var leafCapabilities = []string{
	"events",
	"costmeter",
	"modelprice",
	"instructions",
	"session",
	"session/store",
	"session/compact",
	// 会话工具集：宿主可只取 inbox/export/references/timeline/observability，
	// 不必拖入 agent 或 UI。
	"session/inbox",
	"session/export",
	"session/references",
	"session/timeline",
	"session/observability",
	"session/insights",
	"session/automation",
	"storage",
	// memoryvfs 是上下文数据库契约：宿主可以只 import 它并自己实现后端。
	"memoryvfs",
	// spillfile 是 rig.SpillStore 的文件型实现：只依赖 stdlib 与根包契约，
	// 宿主可以单独拿走它而不必引入 agent 的落盘实现或 session/sqlite。
	"spillfile",
	// storagetest 是 storage 契约的共享基线套件：任何实现 storage.Storage 的包
	// （含仓外宿主）都要能 import 它，所以它只能依赖 stdlib 与契约层。
	"storagetest",
	"tools/undo",
	"tools/patch",
	"tools/search",
	"tools/builtin",
	"tools/jobs",
	"tools/terminal",
	"tools/mcp",
	"tools/mcp/manager",
	"tools/mcp/auth",
	"tools/lsp",
	"tools/lsp/host",
	"tools/ptc",
	"tools/worktree",
	"tools/skill",
	"tools/memory",
	"tools/subagent",
	"tools/catalog",
	"tools/presets",
	"tools/team/taskstore",
	"workspace",
	"update",
	"acp/server",
	"agent/team",
	"internal/fsguard",
	"internal/gitsnap",
	"internal/diffpreview",
	"provider/local",
	"provider/ollama",
	"provider/lmstudio",
	"provider/openai",
	"sandbox",
}

// TestLeafCapabilitiesDoNotImportAggregators 断言叶子包不 import 聚合层。
func TestLeafCapabilitiesDoNotImportAggregators(t *testing.T) {
	chdirRepoRoot(t)
	forbidden := []string{modulePath + "/kit", modulePath + "/tools/full"}
	for _, pkg := range leafCapabilities {
		imports := packageImports(t, pkg)
		for _, bad := range forbidden {
			if imports[bad] {
				t.Errorf("%s 不得 import %s：这会让只需要这一个能力的宿主被拖进整棵树",
					pkg, strings.TrimPrefix(bad, modulePath+"/"))
			}
		}
	}
}

// TestRootPackageStaysStdlibOnly 断言根包只依赖标准库。
func TestRootPackageStaysStdlibOnly(t *testing.T) {
	chdirRepoRoot(t)
	imports := packageImports(t, ".")
	for dep := range imports {
		if strings.Contains(dep, ".") && !strings.HasPrefix(dep, modulePath) {
			// 含点的路径 = 域名开头 = 非标准库。
			t.Errorf("根包 rig 引入了非标准库依赖 %q：根包必须是所有宿主与模块共享的最小契约层", dep)
		}
		if strings.HasPrefix(dep, modulePath+"/") {
			t.Errorf("根包 rig 引入了本仓库子包 %q：它必须是 import 图的叶子", dep)
		}
	}
}

// TestKitIsTheOnlyAggregator 断言"同时装配 provider 与工具面"的只有 kit。
// 这保证聚合逻辑集中在一处：新增一个 provider 或一组工具时，只有 kit 需要知道。
func TestKitIsTheOnlyAggregator(t *testing.T) {
	chdirRepoRoot(t)
	dirs := goPackageDirs(t)
	for _, dir := range dirs {
		if dir == "kit" || strings.HasPrefix(dir, "cmd/") || strings.HasPrefix(dir, "examples/") {
			continue
		}
		imports := packageImports(t, dir)
		aggregatesToolset := imports[modulePath+"/tools/full"]
		aggregatesProvider := false
		for dep := range imports {
			if strings.HasPrefix(dep, modulePath+"/provider/") {
				aggregatesProvider = true
			}
		}
		if aggregatesToolset && aggregatesProvider {
			t.Errorf("%s 同时 import 了 tools/full 与 provider/*：聚合逻辑应集中在 kit，"+
				"否则新增 provider/工具时要改多处", dir)
		}
	}
}

// chdirRepoRoot 把工作目录切到仓库根（go test 的 CWD 是包目录）。
func chdirRepoRoot(t *testing.T) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if err := os.Chdir(dir); err != nil {
				t.Fatalf("Chdir(%s): %v", dir, err)
			}
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("找不到 go.mod（从 %s 向上）", dir)
		}
		dir = parent
	}
}

// packageImports 返回某个包（目录）的非测试源码 import 集合。
func packageImports(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	fset := token.NewFileSet()
	out := make(map[string]bool)
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", path, err)
		}
		files++
		for _, spec := range file.Imports {
			value, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: 无法解析 import %s", path, spec.Path.Value)
			}
			out[value] = true
		}
	}
	if files == 0 {
		t.Fatalf("%s 下没有非测试源码文件（模块清单里是否多了已删除的包？）", dir)
	}
	return out
}

// goPackageDirs 返回仓库里所有非隐藏、非 vendor 的包目录（相对路径）。
func goPackageDirs(t *testing.T) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != "." && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" || name == "node_modules") {
			return filepath.SkipDir
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
				rel := filepath.ToSlash(strings.TrimPrefix(path, "./"))
				if rel == "" {
					rel = "."
				}
				dirs = append(dirs, rel)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return dirs
}
