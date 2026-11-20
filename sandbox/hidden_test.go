package sandbox

// 本文件钉住"对模型隐藏某些路径"这条能力（WithHidden）。
//
// 背景：v0.24.0 把拒绝 glob（WithDeniedGlobs）降级成记录型 no-op，连 glob 匹配器
// （sandbox/glob.go）都整文件删掉了。宿主要兑现"设置里勾了隐藏 .env，模型就读不到"
// 只能自己写匹配器——每个宿主的 `**/`、大小写、基名匹配语义都会不一样。
//
// 这里要求的是一个**单一判据**：策略层导出 GlobMatch，Policy.Hidden 回答
// "这个绝对路径对模型是否可见"，并且 ResolvePath / CheckPath / CheckWritablePath
// 三处都据此拒绝。隐藏是"模型看不到"，不是"文件系统只读"——所以它三次都要拦。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hiddenFixture 建一棵小树：根下有 .env、ok.txt，以及 src/main.go、secrets/key.pem。
func hiddenFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		".env":            "SECRET=42\n",
		"ok.txt":          "hello\n",
		"src/main.go":     "package main\n",
		"secrets/key.pem": "-----BEGIN KEY-----\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll %q: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %q: %v", name, err)
		}
	}
	return root
}

// TestHiddenGlobMatch 钉住导出的 glob 语义：** 跨任意多段（含零段），* 与 ? 只在
// 单个路径段内匹配，其余字符是字面量。
func TestHiddenGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"**/.env", ".env", true},
		{"**/.env", "a/b/.env", true},
		{"**/*.pem", "secrets/key.pem", true},
		{"secrets/**", "secrets/key.pem", true},
		{"secrets/**", "secrets/a/b.txt", true},
		{"secrets/**", "src/secrets.txt", false},
		{"*.pem", "a/b.pem", false}, // 含 "/" 的模式只按整条相对路径匹配
		{"?d_rsa", "id_rsa", true},
		{"literal.name", "literalXname", false}, // 点不是通配符
	}
	for _, testCase := range cases {
		if got := GlobMatch(testCase.pattern, testCase.name); got != testCase.want {
			t.Errorf("GlobMatch(%q, %q) = %v, want %v", testCase.pattern, testCase.name, got, testCase.want)
		}
	}
}

// TestHiddenDeniedOnRead 要求隐藏的路径在三条判定上一致被拒：解析、只读检查、
// 结果过滤用的 DeniedPath。
func TestHiddenDeniedOnRead(t *testing.T) {
	root := hiddenFixture(t)
	policy := New(WithRoots(root), WithMode(ModeWorkspaceWrite), WithHidden("**/.env", "secrets/**"))

	if _, err := policy.ResolvePath(".env"); err == nil {
		t.Fatal("ResolvePath(.env) 应当被隐藏拒绝，实际放行")
	} else if !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("ResolvePath(.env) 的错误应说明是隐藏命中，实际: %v", err)
	}

	if _, err := policy.ResolvePath("ok.txt"); err != nil {
		t.Fatalf("ResolvePath(ok.txt) 不该被拒: %v", err)
	}

	keyPEM := filepath.Join(root, "secrets", "key.pem")
	if err := policy.CheckPath(keyPEM); err == nil || !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("CheckPath(secrets/key.pem) 应被隐藏拒绝，实际: %v", err)
	}
	if !policy.DeniedPath(keyPEM) {
		t.Fatal("DeniedPath(secrets/key.pem) 应为 true（目录列表/glob/grep 靠它剔除）")
	}
	if policy.DeniedPath(filepath.Join(root, "ok.txt")) {
		t.Fatal("DeniedPath(ok.txt) 应为 false")
	}
	if !policy.Hidden(keyPEM) {
		t.Fatal("Hidden(secrets/key.pem) 应为 true")
	}
}

// TestHiddenDeniedOnWrite 要求隐藏同时挡住写入：隐藏的文件不该既能读又能被改。
func TestHiddenDeniedOnWrite(t *testing.T) {
	root := hiddenFixture(t)
	policy := New(WithRoots(root), WithMode(ModeWorkspaceWrite), WithHidden("**/.env"))
	if err := policy.CheckWritablePath(filepath.Join(root, ".env")); err == nil {
		t.Fatal("CheckWritablePath(.env) 应被隐藏拒绝")
	} else if !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("写入拒绝的理由应是隐藏命中，实际: %v", err)
	}
	if err := policy.CheckWritablePath(filepath.Join(root, "ok.txt")); err != nil {
		t.Fatalf("CheckWritablePath(ok.txt) 不该被拒: %v", err)
	}
}

// TestHiddenBasenameMatchesAnyDepth 钉住"不带斜杠的模式按基名匹配任意深度"。
func TestHiddenBasenameMatchesAnyDepth(t *testing.T) {
	root := hiddenFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	deep := filepath.Join(root, "a", "b", "id_rsa")
	if err := os.WriteFile(deep, []byte("key\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	policy := New(WithRoots(root), WithMode(ModeWorkspaceWrite), WithHidden("id_rsa"))
	if err := policy.CheckPath(deep); err == nil {
		t.Fatal("基名模式 id_rsa 应当隐藏任意深度的同名文件")
	}
}

// TestHiddenCaseFoldFollowsPlatform 钉住大小写不敏感平台上的折叠：darwin/windows
// 上 `**/*.ENV` 必须命中 `.env`（否则用户在 mac 上配了等于没配）。
func TestHiddenCaseFoldFollowsPlatform(t *testing.T) {
	previous := pathOSName
	t.Cleanup(func() { pathOSName = previous })

	root := hiddenFixture(t)
	pathOSName = "darwin"
	folded := New(WithRoots(root), WithMode(ModeWorkspaceWrite), WithHidden("**/*.ENV"))
	if !folded.Hidden(filepath.Join(root, ".env")) {
		t.Fatal("darwin 语义下 **/*.ENV 应命中 .env")
	}

	pathOSName = "linux"
	sensitive := New(WithRoots(root), WithMode(ModeWorkspaceWrite), WithHidden("**/*.ENV"))
	if sensitive.Hidden(filepath.Join(root, ".env")) {
		t.Fatal("linux 语义下 **/*.ENV 不该命中 .env")
	}
}

// TestHiddenAppliesInFullAccess 钉住"换档不改变隐藏承诺"：danger-full-access 只是
// 去掉根围栏与命令白名单，隐藏仍然是产品级配置（否则用户在设置里勾了隐藏，一换
// 全权档就静默失效）。
func TestHiddenAppliesInFullAccess(t *testing.T) {
	root := hiddenFixture(t)
	full := New(WithRoots(root), WithMode(ModeDangerFullAccess), WithHidden("**/.env"))

	if err := full.CheckPath(filepath.Join(root, ".env")); err == nil {
		t.Fatal("full-access 下隐藏仍然应当生效")
	} else if !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("拒绝理由应是隐藏命中，实际: %v", err)
	}
	if _, err := full.ResolvePath(".env"); err == nil {
		t.Fatal("full-access 下 ResolvePath(.env) 应当被隐藏拒绝")
	}
	if _, err := full.ResolvePath("ok.txt"); err != nil {
		t.Fatalf("可见文件不该被拒: %v", err)
	}
}
