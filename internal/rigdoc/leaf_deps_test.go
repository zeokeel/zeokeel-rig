package rigdoc_test

// 叶子包的**依赖面**守门测试（审计 A6 #1）。
//
// `tools/presets` 是纯名字表：它只该依赖标准库与根包契约，因此宿主可以把它单独
// 拿走。一旦它 import `sandbox`（策略层），整条依赖链上就多出 19 个包——包括唯一
// 的第三方依赖 `mvdan.cc/sh/v3`（POSIX shell 解析器）与 `net`/`regexp` 等。
// "能单独拿走"的成本从 77 个包变成 96 个，而这在 code review 里看不出来。
//
// 这里用 AST 递归展开**仓库内**的 import 图（不用 `os/exec` 跑 `go list`：单测里
// 依赖 go 可执行文件与模块缓存不可靠，AST 扫描在 CGO_ENABLED=0 下也能跑）。
// 外部（含点）依赖一律记为违规：叶子包的依赖面只允许标准库 + 根包。

import (
	"strings"
	"testing"
)

// TestPresetsDependencyTreeStaysLeafOnly 断言 tools/presets 的传递依赖里没有
// 策略层 `sandbox`，也没有任何第三方包（例如 mvdan.cc/sh）。
func TestPresetsDependencyTreeStaysLeafOnly(t *testing.T) {
	chdirRepoRoot(t)
	const root = "tools/presets"
	seen := map[string]bool{}
	imports := 0
	var walk func(dir string)
	walk = func(dir string) {
		if seen[dir] {
			return
		}
		seen[dir] = true
		for dep := range packageImports(t, dir) {
			imports++
			if !strings.HasPrefix(dep, modulePath) {
				if strings.Contains(dep, ".") {
					// 域名开头的非标准库依赖：叶子包不该有。
					t.Errorf("%s 的依赖树引用了第三方包 %q：叶子包只允许标准库与根包",
						root, dep)
				}
				continue
			}
			rel := strings.TrimPrefix(strings.TrimPrefix(dep, modulePath), "/")
			if rel == "" {
				// 模块根包 rig：预设立足于它，是允许的。
				continue
			}
			if rel == "sandbox" {
				t.Errorf("%s 的依赖树包含 sandbox：策略层会把它拖进 mvdan.cc/sh 解析器与 net/regexp，"+
					"破坏“预设可单独拿走”的承诺（改用 mode string 表达档位）", root)
				continue
			}
			walk(rel)
		}
	}
	walk(root)
	if imports == 0 {
		t.Fatalf("%s 没有扫描到任何 import（依赖图解析坏了，测试会恒真）", root)
	}
}
