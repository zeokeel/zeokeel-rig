package osrunner

import (
	"path/filepath"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 工具链缓存根：**只作为只读诊断清单保留**，不再参与任何渲染。
//
// # 为什么不再授权
//
// 本包现在与 DSH（sandbox-local）严格对齐：workspace-write 的可写面就是"工作区
// 根 + 平台临时目录"，read-only 只有 /dev/null。早期版本为了让沙箱内跑得起
// `go build`/`npm ci`，会把 ~/.cache、~/.cargo、~/.m2 等十来个目录追加进可写面，
// 并**在宿主的 HOME 里 mkdir 出**它们（连 read-only 模式也会建）。那套行为与
// "与 DSH 完全一致"互斥，已经删除：
//
//   - `ensureToolchainCacheRoots`（在宿主 HOME 里创建目录）消失；
//   - `Profile.ToolchainCaches` / `MissingToolchainCaches` 恒为空；
//   - 四个后端都不再渲染任何缓存根。
//
// 保留 `ExportToolchainCacheRootsFor` 是因为它是**导出 API**（诊断工具与示例在
// 用），返回值只描述"某平台历史上会追加哪些缓存根"，不产生任何副作用、也不参与
// 授权判定。需要这份清单的宿主可以自己决定要不要把它们交给策略
// （`WithWritableRoots`）——那是宿主的显式选择，不是 runner 的隐式追加。

// ExportToolchainCacheRootsFor 返回某个平台"历史上会追加"的缓存根（纯函数）。
//
// 给诊断工具与示例用，不参与运行时判定：本包不再授权这些目录。
func ExportToolchainCacheRootsFor(goos string, env func(string) string) []sandbox.WritableRoot {
	return toolchainCacheRootsFor(goos, env)
}

// toolchainCacheRootsFor 是纯函数版本（可跨平台测试）。
//
// env 只用于取 home 与 Windows 的 %LOCALAPPDATA%/%APPDATA%；取不到的项直接跳过
// ——宁可少列也不要猜一个错误路径。
func toolchainCacheRootsFor(goos string, env func(string) string) []sandbox.WritableRoot {
	home := strings.TrimSpace(env("HOME"))
	if home == "" {
		home = strings.TrimSpace(env("USERPROFILE"))
	}
	if home == "" {
		return nil
	}
	home = filepath.Clean(home)

	var roots []sandbox.WritableRoot
	add := func(root string) {
		root = strings.TrimSpace(root)
		if root == "" {
			return
		}
		roots = append(roots, sandbox.WritableRoot{Root: filepath.Clean(root)})
	}
	switch goos {
	case "darwin":
		add(filepath.Join(home, "Library", "Caches"))
		add(filepath.Join(home, "go"))
		add(filepath.Join(home, ".npm"))
		add(filepath.Join(home, ".cargo"))
		add(filepath.Join(home, ".rustup"))
		add(filepath.Join(home, ".m2"))
		add(filepath.Join(home, ".gradle"))
		add(filepath.Join(home, "Library", "Developer"))
	case "windows":
		if localAppData := strings.TrimSpace(env("LOCALAPPDATA")); localAppData != "" {
			add(localAppData)
		}
		add(filepath.Join(home, "go"))
		add(filepath.Join(home, ".cargo"))
		add(filepath.Join(home, ".rustup"))
		add(filepath.Join(home, ".m2"))
		add(filepath.Join(home, ".gradle"))
		if appData := strings.TrimSpace(env("APPDATA")); appData != "" {
			add(filepath.Join(appData, "npm-cache"))
		}
	default: // linux 与其它类 unix
		add(filepath.Join(home, ".cache"))
		if xdg := strings.TrimSpace(env("XDG_CACHE_HOME")); xdg != "" && filepath.IsAbs(xdg) {
			add(filepath.Clean(xdg))
		}
		add(filepath.Join(home, "go"))
		add(filepath.Join(home, ".npm"))
		add(filepath.Join(home, ".cargo"))
		add(filepath.Join(home, ".rustup"))
		add(filepath.Join(home, ".m2"))
		add(filepath.Join(home, ".gradle"))
		add(filepath.Join(home, ".local", "share", "pnpm"))
		add(filepath.Join(home, ".local", "lib"))
		add(filepath.Join(home, ".local", "bin"))
	}

	seen := make(map[string]struct{}, len(roots))
	out := roots[:0]
	for _, root := range roots {
		if _, ok := seen[root.Root]; ok {
			continue
		}
		seen[root.Root] = struct{}{}
		out = append(out, root)
	}
	return out
}
