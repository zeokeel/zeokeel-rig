package sandbox

// A5：路径相等/包含判定的平台语义。
//
// 这一组用例在 Linux 上运行，但通过 pathOSName 注入 darwin/windows 语义，因此
// "大小写不敏感平台上 `.GIT/config` 命中 `.git` 保护"这条**真实漏洞**可以在 CI 上
// 被钉住，而不是只能在 Mac/Win 上人工验证。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withPathOS 临时把路径比较语义切成 goos，并在测试结束时恢复。
func withPathOS(t *testing.T, goos string) {
	t.Helper()
	previous := pathOSName
	pathOSName = goos
	t.Cleanup(func() { pathOSName = previous })
}

// TestPathWithinOSPlatformSemantics 覆盖相等/包含在三种平台语义下的差异。
func TestPathWithinOSPlatformSemantics(t *testing.T) {
	root := filepath.FromSlash("/work/.git")
	cases := []struct {
		name string
		goos string
		path string
		want bool
	}{
		{"linux 精确命中", "linux", "/work/.git/config", true},
		{"linux 大写不命中", "linux", "/work/.GIT/config", false},
		{"linux 混合大小写不命中", "linux", "/work/.Git/Config", false},
		{"darwin 大写命中", "darwin", "/work/.GIT/config", true},
		{"darwin 混合大小写命中", "darwin", "/work/.Git/Config", true},
		{"windows 大写命中", "windows", "/work/.GIT/config", true},
		{"windows 混合大小写命中", "windows", "/work/.Git/Config", true},
		// 反方向：root 大写、path 小写。
		{"darwin root 大写 path 小写", "darwin", "/WORK/.git/x", true},
		// 分量边界：/work/.gitlab 不是 /work/.git 之下。
		{"linux 分量边界", "linux", "/work/.gitlab/config", false},
		{"darwin 分量边界", "darwin", "/work/.GITLAB/config", false},
		// 相等本身。
		{"darwin 相等（大小写不同）", "darwin", "/WORK/.Git", true},
		{"linux 相等（大小写不同）", "linux", "/WORK/.Git", false},
	}
	for _, testCase := range cases {
		if got := pathWithinOS(testCase.goos, root, testCase.path); got != testCase.want {
			t.Errorf("%s: pathWithinOS(%q, %q, %q) = %v, want %v",
				testCase.name, testCase.goos, root, testCase.path, got, testCase.want)
		}
	}
	// 空根不包含任何东西（旧实现会对任何绝对路径返回 true，这里收敛为 false）。
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if pathWithinOS(goos, "", "/x") {
			t.Errorf("%s: 空根不应当包含任何路径", goos)
		}
	}
}

// TestPathEqualOSPlatformSemantics 覆盖"同一位置"判定。
func TestPathEqualOSPlatformSemantics(t *testing.T) {
	if !pathEqualOS("linux", "/w/.git", "/w/.git") {
		t.Fatal("linux 上同一路径应当相等")
	}
	if pathEqualOS("linux", "/w/.git", "/w/.GIT") {
		t.Fatal("linux 上大小写不同是不同路径")
	}
	if !pathEqualOS("darwin", "/w/.git", "/w/.GIT") {
		t.Fatal("darwin 上大小写不同是同一位置")
	}
	if !pathEqualOS("windows", `C:\w\.git`, `C:\W\.GIT`) {
		t.Fatal("windows 上大小写不同是同一位置")
	}
}

// TestProtectedPathIsCaseInsensitiveOnDarwin 是**真实漏洞**的回归：kit 默认保护
// .git，写 `.GIT/config` 在 macOS/Windows 上落到同一个文件。
func TestProtectedPathIsCaseInsensitiveOnDarwin(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			policy := New(WithRoots(root), WithMode(ModeWorkspaceWrite))
			policy.AddProtectedPaths(filepath.Join(root, ".git"))

			// 用真实存在的大小写找到文件系统中该目录的"真名"：Linux 上 .GIT 不存在，
			// 这里只验证**判定**，不依赖文件系统行为。
			withPathOS(t, goos)
			for _, name := range []string{".GIT/config", ".Git/Config", ".gIt/cOnFiG"} {
				target := filepath.Join(root, filepath.FromSlash(name))
				err := policy.CheckWritablePath(target)
				if err == nil {
					t.Fatalf("%s: CheckWritablePath(%q) 未被拒绝（保护被大小写绕过）", goos, name)
				}
				if !strings.Contains(err.Error(), "protected") {
					t.Fatalf("%s: 错误文案没有指出保护路径: %v", goos, err)
				}
			}
			// 对照组：同一平台上正常大小写同样被拒。
			if err := policy.CheckWritablePath(filepath.Join(root, ".git", "config")); err == nil {
				t.Fatalf("%s: 正常大小写没有被拒绝", goos)
			}
		})
	}
}

// TestProtectedPathStaysCaseSensitiveOnLinux 断言 Linux 上 `.GIT` 是另一个目录，
// 不会被误判成受保护（否则用户建一个 .GITHUB 目录都会被挡）。
func TestProtectedPathStaysCaseSensitiveOnLinux(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	policy := New(WithRoots(root), WithMode(ModeWorkspaceWrite))
	policy.AddProtectedPaths(filepath.Join(root, ".git"))

	withPathOS(t, "linux")
	if err := policy.CheckWritablePath(filepath.Join(root, ".GIT", "config")); err != nil {
		t.Fatalf("linux 上 .GIT 不应命中 .git 保护: %v", err)
	}
	if err := policy.CheckWritablePath(filepath.Join(root, ".gitlab", "config")); err != nil {
		t.Fatalf("linux 上 .gitlab 不应命中 .git 保护: %v", err)
	}
}

// TestContainingRootUsesPlatformSemantics 断言根归属判定同样按平台语义：否则
// darwin 上 `.GIT/x` 会被判成"根外"（错误地把合法写入拒掉）或反之。
func TestContainingRootUsesPlatformSemantics(t *testing.T) {
	root := t.TempDir()
	policy := New(WithRoots(root), WithMode(ModeWorkspaceWrite))
	withPathOS(t, "darwin")
	upper := strings.ToUpper(filepath.Join(root, "sub", "file.txt"))
	if _, ok := policy.containingRoot(upper); !ok {
		t.Fatalf("darwin 语义下 %q 应当归属根 %q", upper, root)
	}
	withPathOS(t, "linux")
	if _, ok := policy.containingRoot(upper); ok {
		t.Fatalf("linux 语义下 %q 不应当在根 %q 内", upper, root)
	}
}

// TestDeniedGlobsUsePlatformSemantics 断言拒绝 glob 在大小写不敏感平台上同样生效
// （`secrets/**` 必须命中 `SECRETS/x`）。
func TestDeniedGlobsUsePlatformSemantics(t *testing.T) {
	root := t.TempDir()
	policy := New(WithRoots(root), WithMode(ModeWorkspaceWrite), WithDeniedGlobs("secrets/**"))
	withPathOS(t, "darwin")
	if err := policy.CheckWritablePath(filepath.Join(root, "SECRETS", "x.md")); err == nil {
		t.Fatal("darwin 语义下 SECRETS/x.md 应当命中 secrets/**")
	}
	withPathOS(t, "linux")
	if err := policy.CheckWritablePath(filepath.Join(root, "SECRETS", "x.md")); err != nil {
		t.Fatalf("linux 上 SECRETS 是另一个目录，不应命中: %v", err)
	}
}
