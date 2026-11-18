package osrunner

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 工具链缓存根：沙箱内 `go build` / `npm ci` / `pip install` / `cargo build`
// 能跑起来的前提。
//
// 背景（三平台审计的第一条 P0）：这些工具把缓存与构建产物写在**工作区之外**
// （darwin 的 ~/Library/Caches/go-build、linux 的 ~/.cache/go-build、windows 的
// %LOCALAPPDATA%），而 workspace-write 只授权工作区。结果是被约束的进程连
// `go build` 都跑不起来——不是"缓存没命中慢一点"，而是 GOCACHE 不可写直接失败。
//
// 设计约束：
//  1. **只列缓存/构建产物目录**，绝不列 ~/.config、%APPDATA%（不含 npm-cache）、
//     ~/.npmrc、~/.cargo/credentials.toml、~/.m2/settings.xml；
//  2. 缓存目录里混着的**凭据文件**（npmrc/cargo 凭据/maven settings/gradle
//     properties/gitconfig）用 ReadOnlySubpaths 标成只读——目录可写不等于内容
//     可改，这些文件能改出命令执行或凭据外泄；
//  3. 不存在的位置也返回：`go build` 会在首次使用时创建缓存目录（bwrap 的
//     --bind 需要目标存在，因此安装后不存在时由 BuildProfile 过滤，见
//     toolchainCacheRootsAndMissing）。
//
// 它是 **runner 级追加**，不写进 Policy.WritableRoots：这些目录是"给子进程的
// 构建缓存"，不是"给 agent 的文件写入面"。进程内文件工具（write_file）仍然不能
// 写 ~/.cargo——那本来也不该是它的能力。

// ExportToolchainCacheRootsFor 是 toolchainCacheRootsFor 的导出包装：给诊断工具
// 与示例用（列出某个平台会追加哪些缓存根），不参与运行时判定。
func ExportToolchainCacheRootsFor(goos string, env func(string) string) []sandbox.WritableRoot {
	return toolchainCacheRootsFor(goos, env)
}

// toolchainCacheRoots 返回当前平台应当追加的可写根（受策略开关控制）。
func toolchainCacheRoots(policy *sandbox.Policy) []sandbox.WritableRoot {
	if policy == nil || !policy.IncludeToolchainCaches() {
		return nil
	}
	return toolchainCacheRootsFor(runtime.GOOS, os.Getenv)
}

// toolchainCacheRootsFor 是 toolchainCacheRoots 的纯函数版本（可跨平台测试）。
//
// env 只用于取 home 与 Windows 的 %LOCALAPPDATA%/%APPDATA%；取不到的项直接跳过
// ——宁可少授权也不要猜一个错误路径（猜错等于把可写面放到别处）。
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
	add := func(root string, readOnly ...string) {
		root = strings.TrimSpace(root)
		if root == "" {
			return
		}
		roots = append(roots, sandbox.WritableRoot{Root: filepath.Clean(root), ReadOnlySubpaths: readOnly})
	}
	// home 下的缓存目录里的凭据文件（相对于该目录）。
	homeCreds := func(dir string) []string {
		var out []string
		for _, name := range []string{".npmrc", ".yarnrc", ".yarnrc.yml", ".bunfig.toml"} {
			out = append(out, filepath.Join(dir, name))
		}
		return out
	}

	switch goos {
	case "darwin":
		add(filepath.Join(home, "Library", "Caches"),
			// Application Support/go/env 是 go env 的落点（GOENV）；它不是缓存，
			// 但改它=改 GOFLAGS/GOPRIVATE，属执行面，标只读。
			filepath.Join(home, "Library", "Application Support", "go", "env"),
			filepath.Join(home, "Library", "Caches", "go-build"),
		)
		add(filepath.Join(home, "go"))
		add(filepath.Join(home, ".npm"), homeCreds(filepath.Join(home, ".npm"))...)
		add(filepath.Join(home, ".cargo"),
			filepath.Join(home, ".cargo", "credentials.toml"),
			filepath.Join(home, ".cargo", "credentials"),
			filepath.Join(home, ".cargo", "config.toml"),
			filepath.Join(home, ".cargo", "config"),
		)
		add(filepath.Join(home, ".rustup"))
		add(filepath.Join(home, ".m2"), filepath.Join(home, ".m2", "settings.xml"),
			filepath.Join(home, ".m2", "settings-security.xml"))
		add(filepath.Join(home, ".gradle"), filepath.Join(home, ".gradle", "gradle.properties"),
			filepath.Join(home, ".gradle", "init.gradle"), filepath.Join(home, ".gradle", "init.d"))
		add(filepath.Join(home, "Library", "Developer"))
		add(filepath.Join(home, "Library", "pnpm"), homeCreds(filepath.Join(home, "Library", "pnpm"))...)
		add(filepath.Join(home, ".bun"), homeCreds(filepath.Join(home, ".bun"))...)
	case "windows":
		localAppData := strings.TrimSpace(env("LOCALAPPDATA"))
		appData := strings.TrimSpace(env("APPDATA"))
		if localAppData != "" {
			// 排除凭据目录：Windows 的凭据管理器、Token Broker 缓存都在
			// %LOCALAPPDATA% 下，整目录可写等于把凭据交出去。
			add(filepath.Join(localAppData),
				filepath.Join(localAppData, "Microsoft", "Credentials"),
				filepath.Join(localAppData, "Microsoft", "Vault"),
				filepath.Join(localAppData, "Microsoft", "TokenBroker"),
				filepath.Join(localAppData, "Microsoft", "IdentityCache"),
			)
		}
		add(filepath.Join(home, "go"))
		add(filepath.Join(home, ".cargo"),
			filepath.Join(home, ".cargo", "credentials.toml"),
			filepath.Join(home, ".cargo", "credentials"),
			filepath.Join(home, ".cargo", "config.toml"),
			filepath.Join(home, ".cargo", "config"),
		)
		add(filepath.Join(home, ".rustup"))
		add(filepath.Join(home, ".m2"), filepath.Join(home, ".m2", "settings.xml"),
			filepath.Join(home, ".m2", "settings-security.xml"))
		add(filepath.Join(home, ".gradle"), filepath.Join(home, ".gradle", "gradle.properties"))
		if appData != "" {
			add(filepath.Join(appData, "npm-cache"), filepath.Join(appData, ".npmrc"))
		}
	default: // linux 与其它类 unix
		add(filepath.Join(home, ".cache"))
		add(filepath.Join(home, "go"))
		add(filepath.Join(home, ".npm"), homeCreds(filepath.Join(home, ".npm"))...)
		add(filepath.Join(home, ".cargo"),
			filepath.Join(home, ".cargo", "credentials.toml"),
			filepath.Join(home, ".cargo", "credentials"),
			filepath.Join(home, ".cargo", "config.toml"),
			filepath.Join(home, ".cargo", "config"),
		)
		add(filepath.Join(home, ".rustup"))
		add(filepath.Join(home, ".m2"), filepath.Join(home, ".m2", "settings.xml"),
			filepath.Join(home, ".m2", "settings-security.xml"))
		add(filepath.Join(home, ".gradle"), filepath.Join(home, ".gradle", "gradle.properties"))
		add(filepath.Join(home, ".local", "share", "pnpm"), homeCreds(filepath.Join(home, ".local", "share", "pnpm"))...)
		// pip install --user 的落点：~/.local/lib（site-packages）与 ~/.local/bin
		// （entry point 脚本）。**不能**给整个 ~/.local：它下面还有 state/ 与
		// keyrings/（凭据）。XDG_DATA_HOME 同上，只给 share/pnpm。
		add(filepath.Join(home, ".local", "lib"))
		add(filepath.Join(home, ".local", "bin"))
	}

	// 去重（同一路径可能被上层目录覆盖），保持顺序。
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

// toolchainCacheCandidates 返回缓存根里**当前存在**的部分，用于渲染；不存在
// 的根单独作为 missing 返回（诊断用：首次使用前缓存目录本来就不存在，bwrap 的
// --bind 需要目标存在，因此不能把它渲染成挂载参数）。
func toolchainCacheCandidates(roots []sandbox.WritableRoot) (existing []sandbox.WritableRoot, missing []string) {
	for _, root := range roots {
		if _, err := os.Stat(root.Root); err != nil {
			missing = append(missing, root.Root)
			continue
		}
		existing = append(existing, root)
	}
	return existing, missing
}
