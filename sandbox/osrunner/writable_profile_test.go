package osrunner

// 工具链缓存与可写面模型的用例（BuildProfile 派生 + 三个平台的缓存清单）。
//
// 审计代理的 linux_toolchain_test.go / darwin_toolchain_test.go /
// windows_toolchain_test.go 是**端到端**用例（真的跑 go build/npm 之类）；这里
// 只覆盖"清单本身正确、且被渲染进 profile"这一层，属于单一真源的自测。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// fakeEnv 构造一个确定性的环境查询函数。
func fakeEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func rootPaths(roots []sandbox.WritableRoot) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		out = append(out, root.Root)
	}
	return out
}

// 三个平台的缓存清单必须只含缓存/构建产物，且凭据文件在 ReadOnlySubpaths 里。
func TestToolchainCacheRootsPerPlatform(t *testing.T) {
	home := filepath.FromSlash("/home/u")
	cases := []struct {
		goos    string
		env     map[string]string
		wantSub []string // 必须出现的缓存根（后缀匹配）
		deny    []string // 绝不能出现的目录名
	}{
		{
			goos: "darwin",
			env:  map[string]string{"HOME": home},
			wantSub: []string{
				"Library/Caches", "go", ".npm", ".cargo", ".rustup", ".m2", ".gradle",
				"Library/Developer", "Library/pnpm", ".bun",
			},
			deny: []string{".config", "Library/Application Support/Keychains"},
		},
		{
			goos: "linux",
			env:  map[string]string{"HOME": home},
			wantSub: []string{
				".cache", "go", ".npm", ".cargo", ".rustup", ".m2", ".gradle", ".local/share/pnpm",
			},
			deny: []string{".config", ".ssh", ".aws", ".gnupg"},
		},
		{
			goos: "windows",
			env: map[string]string{
				"HOME":         home,
				"USERPROFILE":  home,
				"LOCALAPPDATA": filepath.FromSlash("/home/u/AppData/Local"),
				"APPDATA":      filepath.FromSlash("/home/u/AppData/Roaming"),
			},
			wantSub: []string{"AppData/Local", "go", ".cargo", ".rustup", ".m2", ".gradle", "npm-cache"},
			deny:    []string{"Microsoft/Credentials", "Microsoft/Vault", "Microsoft/TokenBroker"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			roots := toolchainCacheRootsFor(tc.goos, fakeEnv(tc.env))
			if len(roots) == 0 {
				t.Fatalf("%s: 缓存清单为空", tc.goos)
			}
			// 用 ToSlash 归一后再比较：期望值写的是 POSIX 形态（缓存布局本身
			// 是跨平台的），在 Windows 上 filepath.Join 会产出 `\\`。
			joined := filepath.ToSlash(strings.Join(rootPaths(roots), "\n"))
			for _, want := range tc.wantSub {
				if !strings.Contains(joined, want) {
					t.Errorf("%s: 缓存清单缺少 %q\n实际：\n%s", tc.goos, want, joined)
				}
			}
			for _, bad := range tc.deny {
				for _, root := range roots {
					if strings.HasSuffix(root.Root, bad) {
						t.Errorf("%s: 缓存清单包含不该授权的路径 %q", tc.goos, root.Root)
					}
					for _, sub := range root.ReadOnlySubpaths {
						if strings.Contains(sub, bad) && !strings.Contains(bad, "Microsoft") {
							t.Errorf("%s: 只读子路径不该是 %q", tc.goos, sub)
						}
					}
				}
			}
			// 凭据文件必须是只读子路径。
			wantCreds := map[string]bool{
				".npmrc": false, "credentials.toml": false, "settings.xml": false, "gradle.properties": false,
			}
			for _, root := range roots {
				for _, sub := range root.ReadOnlySubpaths {
					for name := range wantCreds {
						if strings.HasSuffix(sub, name) {
							wantCreds[name] = true
						}
					}
				}
			}
			for name, found := range wantCreds {
				if !found {
					t.Errorf("%s: 凭据文件 %q 没有出现在任何 ReadOnlySubpaths 里", tc.goos, name)
				}
			}
		})
	}
}

// 策略开关关掉时，BuildProfile 不追加任何缓存根。
func TestBuildProfileRespectsToolchainCacheSwitch(t *testing.T) {
	ws := t.TempDir()
	on := sandbox.New(sandbox.WithRoots(ws), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	profileOn, err := BuildProfile(on)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	off := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithToolchainCaches(false),
	)
	profileOff, err := BuildProfile(off)
	if err != nil {
		t.Fatalf("BuildProfile(off): %v", err)
	}
	if len(profileOff.ToolchainCaches) != 0 {
		t.Errorf("关闭后仍追加了缓存根：%v", rootPaths(profileOff.ToolchainCaches))
	}
	// 默认开启时，**尚不存在**的缓存根也必须进授权集：可写的语义是"可以创建"。
	// 干净机器上 `~/.cargo`/`~/.m2` 还不存在，若不授权，用户第一次
	// `cargo build` 就会在 mkdir 上吃到 EPERM。
	if len(profileOn.ToolchainCaches) == 0 {
		t.Fatal("默认开启时授权集为空")
	}
	for _, missing := range profileOn.MissingToolchainCaches {
		found := false
		for _, root := range profileOn.ToolchainCaches {
			if sandbox.PathEqual(root.Root, missing) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("缺失缓存根 %q 既不在授权集里、也在 MissingToolchainCaches 里：它必须两者都在（授权 + 待创建）", missing)
		}
	}
}

// Profile 必须从策略的可写面模型派生：ReadWrite 含根、Protected 含只读子路径。
func TestBuildProfileDerivesFromWritableRoots(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	configPath := filepath.Join(ws, ".git", "config")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithReadOnlySubpaths(ws, filepath.Join(".git", "config")),
		sandbox.WithToolchainCaches(false),
	)
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if len(profile.WritableRoots) != 1 || !sandbox.PathEqual(profile.WritableRoots[0].Root, ws) {
		t.Fatalf("WritableRoots = %+v, want the workspace root", profile.WritableRoots)
	}
	if !containsPath(profile.ReadWrite, ws) {
		t.Fatalf("ReadWrite = %v, 缺少工作区根", profile.ReadWrite)
	}
	if !containsPath(profile.Protected, configPath) {
		t.Fatalf("Protected = %v, 缺少 %q（只读子路径必须被渲染）", profile.Protected, configPath)
	}
	// 同一份判定：AllowsWrite 与 Protected 必须一致。
	if policy.AllowsWrite(configPath) {
		t.Fatal("AllowsWrite(.git/config) = true，与 Profile.Protected 矛盾")
	}
	if !policy.AllowsWrite(filepath.Join(ws, ".git", "objects", "x")) {
		t.Fatal("AllowsWrite(.git/objects/x) = false，git commit 会被拦")
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if sandbox.PathEqual(path, want) {
			return true
		}
	}
	return false
}

// bwrap 的渲染顺序：rw bind 在前、--ro-bind 覆盖在后（last-wins）。
func TestBwrapArgsOrderForReadOnlySubpaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap 只在 Linux 上渲染")
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	configPath := filepath.Join(ws, ".git", "config")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithReadOnlySubpaths(ws, filepath.Join(".git", "config")),
		sandbox.WithToolchainCaches(false),
	)
	args := BwrapArgs(policy)
	bindAt, roBindAt := -1, -1
	for i := 0; i+2 < len(args); i++ {
		switch args[i] {
		case "--bind":
			if sandbox.PathEqual(args[i+1], ws) {
				bindAt = i
			}
		case "--ro-bind":
			if sandbox.PathEqual(args[i+1], configPath) {
				roBindAt = i
			}
		}
	}
	if bindAt < 0 {
		t.Fatalf("缺少 --bind %s：%v", ws, args)
	}
	if roBindAt < 0 {
		t.Fatalf("缺少 --ro-bind %s：%v", configPath, args)
	}
	if roBindAt < bindAt {
		t.Fatalf("--ro-bind 必须在 --bind 之后（挂载 last-wins）：%v", args)
	}
}

// Seatbelt：deny 规则必须排在 allow 之后（SBPL last-match-wins）。
func TestSeatbeltDenyAfterAllow(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	configPath := filepath.Join(ws, ".git", "config")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithReadOnlySubpaths(ws, filepath.Join(".git", "config")),
		sandbox.WithToolchainCaches(false),
	)
	profile := SeatbeltProfile(policy)
	if profile == "" {
		t.Fatal("SeatbeltProfile 为空")
	}
	allowAt := strings.Index(profile, "(allow file-write* (subpath "+sbplString(ws))
	denyAt := strings.Index(profile, "(deny file-write* (subpath "+sbplString(configPath))
	if allowAt < 0 {
		t.Fatalf("profile 缺少 allow subpath %s:\n%s", ws, profile)
	}
	if denyAt < 0 {
		t.Fatalf("profile 缺少 deny subpath %s:\n%s", configPath, profile)
	}
	if denyAt < allowAt {
		t.Fatalf("deny 必须排在 allow 之后（SBPL last-match-wins）:\n%s", profile)
	}
}

// MissingProtected 只报告"父目录存在的缺失项"——父目录都不存在的是纯噪声
// （整台机器没装 cargo），那些根由 MissingToolchainCaches 统一上报。
func TestMissingProtectionReportFiltersNoise(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	realGap := filepath.Join(ws, ".git", "config.worktree") // 父目录存在、自身不存在
	noise := filepath.Join(ws, "not-installed", "credentials.toml")
	got := missingProtectionReport([]string{realGap, noise})
	if len(got) != 1 || got[0] != realGap {
		t.Fatalf("missingProtectionReport = %v, want 只有 %q（噪声应被过滤）", got, realGap)
	}
}

// pip install --user 的落点必须可写，但**不能**把整个 ~/.local 放进来
// （那里有 state/、keyrings/ 等凭据面）。
func TestLinuxToolchainCachesIncludePipUserDirs(t *testing.T) {
	roots := toolchainCacheRootsFor("linux", fakeEnv(map[string]string{"HOME": "/home/u"}))
	paths := rootPaths(roots)
	// 期望值写 POSIX 形态，比较前用 ToSlash 归一（Windows 上是 `\`）。
	has := func(suffix string) bool {
		for _, path := range paths {
			if strings.HasSuffix(filepath.ToSlash(path), suffix) {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"/home/u/.local/lib", "/home/u/.local/bin"} {
		if !has(want) {
			t.Errorf("linux 缓存清单缺少 %q（pip install --user 需要它）\n实际：%v", want, paths)
		}
	}
	for _, bad := range []string{"/home/u/.local", "/home/u/.local/state", "/home/u/.local/share"} {
		for _, path := range paths {
			if filepath.ToSlash(path) == bad {
				t.Errorf("缓存清单包含了不该授权的 %q（凭据/状态面）", bad)
			}
		}
	}
}
