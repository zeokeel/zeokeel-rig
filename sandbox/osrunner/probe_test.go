package osrunner

// Seatbelt 探测层与 profile 渲染的**平台无关**单元测试（darwin.go 不带 build tag）。
//
// 这些断言不依赖 macOS：它们测的是纯函数（探测 argv、诊断裁剪、SBPL 转义）与
// Confine 的错误分支，因此在 CI 的 linux job 上也能覆盖到。

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestSeatbeltProbeTailIsAbsolute 断言探测命令是绝对路径（不依赖 PATH）。
func TestSeatbeltProbeTailIsAbsolute(t *testing.T) {
	tail := seatbeltProbeTail()
	if len(tail) == 0 {
		t.Fatal("seatbeltProbeTail 为空")
	}
	// 断言"不依赖 PATH"而不是 `filepath.IsAbs`：Windows 上 `/bin/sh` 不是
	// `IsAbs` 意义上的绝对路径（缺盘符），但它同样不经 PATH 查找。
	if !filepath.IsAbs(tail[0]) && !strings.HasPrefix(tail[0], "/") {
		t.Fatalf("探测 argv[0] = %q, want 绝对路径（不经 PATH 查找）", tail[0])
	}
}

// TestSeatbeltProbeDetailTrims 覆盖诊断裁剪：空输出、多行、超长。
func TestSeatbeltProbeDetailTrims(t *testing.T) {
	if got := seatbeltProbeDetail(nil); got != "" {
		t.Fatalf("空输出 = %q, want empty", got)
	}
	if got := seatbeltProbeDetail([]byte("first line\nsecond line\n")); got != "first line" {
		t.Fatalf("多行输出 = %q, want 只保留首行", got)
	}
	long := strings.Repeat("y", 500)
	got := seatbeltProbeDetail([]byte(long))
	if len(got) > 220 {
		t.Fatalf("超长输出没有被裁剪：%d 字节", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("裁剪后应当带省略号：%q", got)
	}
}

// TestSeatbeltDenialProbeArgvShape 覆盖拒绝自检的 argv 形状（target 走位置参数，
// 不做字符串拼接）。
func TestSeatbeltDenialProbeArgvShape(t *testing.T) {
	target := filepath.Join(t.TempDir(), "must-not-exist")
	argv := seatbeltDenialProbeArgv("/usr/bin/sandbox-exec", target)
	if len(argv) < 8 {
		t.Fatalf("argv = %q，形状不完整", argv)
	}
	if argv[0] != "/usr/bin/sandbox-exec" || argv[1] != "-p" || argv[3] != "--" {
		t.Fatalf("argv = %q，want sandbox-exec -p <profile> -- …", argv)
	}
	if !strings.Contains(argv[2], "(deny file-write*)") {
		t.Fatalf("内联 profile = %q，want 只读 SBPL", argv[2])
	}
	if argv[len(argv)-1] != target {
		t.Fatalf("argv 末尾 = %q, want target %q（必须走位置参数）", argv[len(argv)-1], target)
	}
}

// TestSeatbeltConfineErrorBranches 覆盖 Confine 的三条 fail-closed 分支。
func TestSeatbeltConfineErrorBranches(t *testing.T) {
	runner := &seatbeltRunner{execPath: "/usr/bin/sandbox-exec"}
	if _, err := runner.Confine([]string{"/bin/true"}, nil); err == nil {
		t.Fatal("nil 策略应当 fail closed")
	}
	if _, err := runner.Confine(nil, sandbox.New(sandbox.WithMode(sandbox.ModeReadOnly))); err == nil {
		t.Fatal("空 argv 应当 fail closed")
	}
	var unconfigured *seatbeltRunner
	if _, err := unconfigured.Confine([]string{"/bin/true"}, sandbox.New(sandbox.WithMode(sandbox.ModeReadOnly))); err == nil {
		t.Fatal("未配置的 runner 应当 fail closed")
	}
}

// TestSeatbeltProfileEmptyForUnrenderablePolicy 覆盖"路径含控制字符 → 返回空串"的
// fail-closed 分支：这条是保留下来唯一与 DSH 无关的健壮性检查。
func TestSeatbeltProfileEmptyForUnrenderablePolicy(t *testing.T) {
	if got := SeatbeltProfile(nil); got != "" {
		t.Fatalf("nil 策略的 profile = %q, want empty", got)
	}
	profile := &Profile{Mode: sandbox.ModeReadOnly, ReadOnly: []string{"/"}, ReadWrite: []string{"/dev/null\nboom"}}
	if sbplRenderable(profile) {
		t.Fatal("sbplRenderable 接受了含换行的路径")
	}
	if got := SeatbeltProfile(sandbox.New(sandbox.WithMode(sandbox.ModeReadOnly))); !sbplRenderable(&Profile{ReadOnly: []string{"/"}, ReadWrite: []string{"/dev/null"}}) || got == "" {
		t.Fatalf("正常策略的 profile 不应为空：%q", got)
	}
}
