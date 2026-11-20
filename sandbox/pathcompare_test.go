package sandbox

// A5：路径相等/包含判定的平台语义。
//
// 这一组用例在 Linux 上运行，但通过 pathOSName 注入 darwin/windows 语义，因此
// "根归属判定必须按平台大小写语义"可以在 CI 上被钉住，而不是只能在 Mac/Win 上人工
// 验证。DSH 对齐后保护路径与拒绝 glob 不参与强制，相关用例改为断言"不再拦"。

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

// TestWorkspaceMetadataPathsAreWritableOnAnyPlatform 钉住"没有保护路径"这一事实：
// `.git/config`（及其大小写变体）在三种平台语义下都可写。
//
// 为什么保留这条：旧版本这里断言"大小写变体命中了 .git 保护"。策略层没有保护
// 路径之后该断言不再成立，但"平台大小写语义"本身仍由
// TestContainingRootUsesPlatformSemantics 与 TestPathWithinOSPlatformSemantics
// 守护，写侧也要有一条同口径的用例。
func TestWorkspaceMetadataPathsAreWritableOnAnyPlatform(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			policy := New(WithRoots(root), WithMode(ModeWorkspaceWrite))
			withPathOS(t, goos)
			for _, name := range []string{".GIT/config", ".Git/Config", ".gIt/cOnFiG", ".git/config"} {
				target := filepath.Join(root, filepath.FromSlash(name))
				if err := policy.CheckWritablePath(target); err != nil {
					t.Fatalf("%s: CheckWritablePath(%q) = %v, want nil", goos, name, err)
				}
			}
		})
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
