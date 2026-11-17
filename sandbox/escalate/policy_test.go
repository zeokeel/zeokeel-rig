package escalate_test

// 策略判定（Policy.Decide）与客户端环境变量过滤的用例。
//
// Decide 必须纯函数、顺序确定：argv → reason → Deny → Allow → Dir/Roots → 审批。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

// allowPolicy 构造一个只允许 git fetch / curl 的策略，根为 root。
func allowPolicy(root string) escalate.Policy {
	return escalate.Policy{
		Allow: [][]string{{"git", "fetch"}, {"curl"}},
		Roots: []string{root},
	}
}

func TestDecideAllowPrefixSemantics(t *testing.T) {
	root := t.TempDir()
	policy := allowPolicy(root)

	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"精确前缀", []string{"git", "fetch"}, true},
		{"前缀带参数", []string{"git", "fetch", "origin"}, true},
		{"更长的单命令前缀", []string{"curl", "-sS", "https://example.com"}, true},
		{"同前缀不同子命令", []string{"git", "push"}, false},
		{"前缀不是字符串包含", []string{"gitea", "fetch"}, false},
		{"不同命令", []string{"rm", "-rf", "/"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := policy.Decide(escalate.Request{
				ID: "r1", Argv: tc.argv, Dir: root, Reason: "需要拉取远端",
			})
			if got.Allow != tc.want {
				t.Fatalf("Decide(%v).Allow = %v, want %v (reason=%q)", tc.argv, got.Allow, tc.want, got.Reason)
			}
			if !tc.want && got.Reason == "" {
				t.Fatal("拒绝必须给出原因")
			}
		})
	}
}

func TestDecideDenyMatchesEqualsForm(t *testing.T) {
	root := t.TempDir()
	policy := allowPolicy(root)
	policy.Allow = [][]string{{"git"}}
	policy.Deny = [][]string{{"git", "fetch", "--upload-pack"}}

	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"裸 flag 形式", []string{"git", "fetch", "--upload-pack"}, false},
		{"等号形式", []string{"git", "fetch", "--upload-pack=/tmp/x"}, false},
		{"token 边界不匹配", []string{"git", "fetch", "--upload-package=1"}, true},
		{"同前缀子命令", []string{"git", "status"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := policy.Decide(escalate.Request{ID: "r1", Argv: tc.argv, Dir: root, Reason: "测试"})
			if got.Allow != tc.want {
				t.Fatalf("Decide(%v).Allow = %v, want %v (reason=%q)", tc.argv, got.Allow, tc.want, got.Reason)
			}
		})
	}
}

func TestDecideDenyBeatsAllow(t *testing.T) {
	root := t.TempDir()
	policy := allowPolicy(root)
	policy.Deny = [][]string{{"git", "fetch", "--upload-pack"}}

	got := policy.Decide(escalate.Request{
		ID: "r1", Argv: []string{"git", "fetch", "--upload-pack=/tmp/x"}, Dir: root, Reason: "测试",
	})
	if got.Allow {
		t.Fatalf("Deny 应优先于 Allow，得到 Allow=true")
	}
	mustContain(t, got.Reason, "禁止")

	// 不命中 Deny 的同前缀请求仍然放行。
	if ok := policy.Decide(escalate.Request{
		ID: "r2", Argv: []string{"git", "fetch", "origin"}, Dir: root, Reason: "测试",
	}); !ok.Allow {
		t.Fatalf("未命中 Deny 的请求应放行，reason=%q", ok.Reason)
	}
}

func TestDecideRejectsEmptyArgv(t *testing.T) {
	root := t.TempDir()
	policy := allowPolicy(root)
	for _, argv := range [][]string{nil, {}} {
		got := policy.Decide(escalate.Request{ID: "r1", Argv: argv, Dir: root, Reason: "测试"})
		if got.Allow {
			t.Fatalf("空 argv 必须拒绝")
		}
		mustContain(t, got.Reason, "argv")
	}
}

func TestDecideRejectsEmptyReason(t *testing.T) {
	root := t.TempDir()
	policy := allowPolicy(root)
	for _, reason := range []string{"", "   ", "\t\n"} {
		got := policy.Decide(escalate.Request{ID: "r1", Argv: []string{"git", "fetch"}, Dir: root, Reason: reason})
		if got.Allow {
			t.Fatalf("reason=%q 必须拒绝", reason)
		}
		mustContain(t, got.Reason, "理由")
	}
}

func TestDecideRequiresApproval(t *testing.T) {
	root := t.TempDir()
	policy := allowPolicy(root)
	policy.Approval = [][]string{{"curl"}}

	got := policy.Decide(escalate.Request{ID: "r1", Argv: []string{"curl", "https://x"}, Dir: root, Reason: "要用宿主凭据"})
	if !got.Allow || !got.NeedApproval {
		t.Fatalf("curl 应放行但需要审批，得到 %+v", got)
	}

	got = policy.Decide(escalate.Request{ID: "r2", Argv: []string{"git", "fetch"}, Dir: root, Reason: "拉代码"})
	if !got.Allow || got.NeedApproval {
		t.Fatalf("git fetch 不需要审批，得到 %+v", got)
	}
}

func TestDecideDirValidation(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{"根自身", root, true},
		{"子目录", filepath.Join(root, "sub"), true},
		{"孙目录", filepath.Join(root, "sub", "deep"), true},
		{"另一棵树", outside, false},
		{"空 dir", "", false},
		{"相对路径", "sub", false},
		{"点号", ".", false},
		{"点号点号逃逸", filepath.Join(root, ".."), false},
		{"内部点号点号", filepath.Join(root, "sub", "..", "sub"), true},
		{"不存在", filepath.Join(root, "nope"), false},
		{"前缀伪装", root + "-evil", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := allowPolicy(root).Decide(escalate.Request{
				ID: "r1", Argv: []string{"git", "fetch"}, Dir: tc.dir, Reason: "测试",
			})
			if got.Allow != tc.want {
				t.Fatalf("Dir=%q Allow=%v want %v (reason=%q)", tc.dir, got.Allow, tc.want, got.Reason)
			}
		})
	}
}

func TestDecideDirSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows 上创建符号链接需要额外权限")
	}
	root := t.TempDir()
	outside := t.TempDir()
	inner := filepath.Join(root, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	if err := os.Symlink(inner, filepath.Join(root, "alias")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}

	policy := allowPolicy(root)
	if got := policy.Decide(escalate.Request{ID: "r1", Argv: []string{"git", "fetch"}, Dir: filepath.Join(root, "escape"), Reason: "测试"}); got.Allow {
		t.Fatalf("指向根外的符号链接必须拒绝，reason=%q", got.Reason)
	}
	if got := policy.Decide(escalate.Request{ID: "r2", Argv: []string{"git", "fetch"}, Dir: filepath.Join(root, "alias"), Reason: "测试"}); !got.Allow {
		t.Fatalf("指向根内的符号链接应放行，reason=%q", got.Reason)
	}
	// 经由符号链接的"根内子目录"也仍然在根内。
	if got := policy.Decide(escalate.Request{ID: "r3", Argv: []string{"git", "fetch"}, Dir: filepath.Join(root, "alias", "..", "inner"), Reason: "测试"}); !got.Allow {
		t.Fatalf("归一化后仍在根内，应放行，reason=%q", got.Reason)
	}
}

func TestDecideWithoutRootsDeniesEverything(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"git"}}}
	got := policy.Decide(escalate.Request{ID: "r1", Argv: []string{"git", "fetch"}, Dir: root, Reason: "测试"})
	if got.Allow {
		t.Fatal("未配置 Roots 时必须拒绝（fail closed）")
	}
	mustContain(t, got.Reason, "Roots")
}

func TestFilterEnvWhitelist(t *testing.T) {
	policy := escalate.Policy{}
	kept, dropped := policy.FilterEnv(map[string]string{
		"GIT_AUTHOR_NAME":       "Ada",
		"LC_ALL":                "C",
		"PATH":                  "/tmp/evil",
		"AWS_SECRET_ACCESS_KEY": "s3cr3t-value",
		"LD_PRELOAD":            "/tmp/evil.so",
	})
	if len(kept) != 2 || kept["GIT_AUTHOR_NAME"] != "Ada" || kept["LC_ALL"] != "C" {
		t.Fatalf("白名单保留结果不对: %v", kept)
	}
	if len(dropped) != 3 {
		t.Fatalf("丢弃列表 = %v, want 3 个键", dropped)
	}
	joined := strings.Join(dropped, ",")
	for _, key := range []string{"AWS_SECRET_ACCESS_KEY", "LD_PRELOAD", "PATH"} {
		mustContain(t, joined, key)
	}
	// 诊断只记键名，绝不泄漏值。
	if _, ok := kept["GIT_AUTHOR_NAME"]; !ok {
		t.Fatalf("白名单变量应保留: %v", kept)
	}
	if strings.Contains(joined, "s3cr3t-value") {
		t.Fatal("诊断里不能出现环境变量的值")
	}
}

func TestFilterEnvDropsDangerousGitNames(t *testing.T) {
	policy := escalate.Policy{}
	kept, dropped := policy.FilterEnv(map[string]string{
		"GIT_SSH_COMMAND":     "/tmp/evil.sh",
		"GIT_EXTERNAL_DIFF":   "/tmp/evil.sh",
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "http.proxy",
		"GIT_TERMINAL_PROMPT": "0",
	})
	if _, ok := kept["GIT_SSH_COMMAND"]; ok {
		t.Fatal("GIT_SSH_COMMAND 是可执行注入点，必须丢弃")
	}
	if _, ok := kept["GIT_EXTERNAL_DIFF"]; ok {
		t.Fatal("GIT_EXTERNAL_DIFF 是可执行注入点，必须丢弃")
	}
	if _, ok := kept["GIT_CONFIG_COUNT"]; ok {
		t.Fatal("GIT_CONFIG_* 可注入 proxyCommand，必须丢弃")
	}
	if kept["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("普通 GIT_ 变量应保留: %v", kept)
	}
	if len(dropped) != 4 {
		t.Fatalf("dropped = %v, want 4", dropped)
	}
}

func TestFilterEnvCustomPrefixes(t *testing.T) {
	policy := escalate.Policy{EnvPrefixes: []string{"MYAPP_"}}
	kept, dropped := policy.FilterEnv(map[string]string{
		"MYAPP_TOKEN":     "t",
		"GIT_AUTHOR_NAME": "Ada",
	})
	if kept["MYAPP_TOKEN"] != "t" {
		t.Fatalf("自定义前缀应生效: %v", kept)
	}
	if _, ok := kept["GIT_AUTHOR_NAME"]; ok {
		t.Fatal("覆盖前缀后 GIT_ 不再保留")
	}
	if len(dropped) != 1 || dropped[0] != "GIT_AUTHOR_NAME" {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestDecideIsPureAndRepeatable(t *testing.T) {
	root := t.TempDir()
	policy := allowPolicy(root)
	req := escalate.Request{ID: "r1", Argv: []string{"git", "fetch"}, Dir: root, Reason: "测试"}
	first := policy.Decide(req)
	for i := 0; i < 3; i++ {
		if got := policy.Decide(req); got != first {
			t.Fatalf("Decide 必须可重复且无副作用: %+v vs %+v", first, got)
		}
	}
}
