package builtin_test

// 输出编码的**可执行证据**（任意宿主可跑的部分）。
//
// 乱码链条：子进程按非 UTF-8 代码页写出字节 → 我们按 UTF-8 解码 → 模型看到替换
// 字符。这里用 fake pwsh / fake bash 直接吐**真实 GBK 字节**（中文 Windows 的控制
// 台默认代码页 936），断言工具结果里：
//   - 没有原始 GBK 字节；
//   - 是合法 UTF-8；
//   - 带一条明确的"输出不是 UTF-8"告警。
//
// Windows 上的真实场景（中文路径、pwsh/cmd 中文输出、沙箱内中文文件名）见
// windows_encoding_test.go（`//go:build windows`，本机只编译不执行）。

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/internal/procattr"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

func TestTruncatingBufferDecodesNonUTF8Output(t *testing.T) {
	// truncatingBuffer 是 bash 与 pwsh 共用的前台输出缓冲；通过 pwsh 工具走到它，
	// 保证"改的是真实路径而不是孤立的辅助函数"。
	requireWindowsOrUnixish(t)
	script := fakePwsh(t, `printf '\326\320\316\304\n'`) // "中文" 的 GBK 字节
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Root: t.TempDir()})

	result := invokePwsh(t, tool, `{"command":"Write-Output 'x'"}`)
	if !utf8.ValidString(result.Content) {
		t.Fatalf("工具结果不是合法 UTF-8：%q", result.Content)
	}
	if strings.Contains(result.Content, "\uFFFD") == false {
		t.Fatalf("非法字节没有被替换成 U+FFFD：%q", result.Content)
	}
	if !strings.Contains(result.Content, procattr.NonUTF8OutputWarning) {
		t.Fatalf("非法 UTF-8 输出没有附带告警：%q", result.Content)
	}
	// 原始 GBK 字节一个都不许留（否则模型会看到"乱码原样"）。
	if strings.Contains(result.Content, string([]byte{0xD6, 0xD0, 0xCE, 0xC4})) {
		t.Fatalf("原始 GBK 字节被原样透传：%q", result.Content)
	}
}

func TestTruncatingBufferKeepsValidUTF8Clean(t *testing.T) {
	requireWindowsOrUnixish(t)
	script := fakePwsh(t, `printf '中文输出\n'`)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Root: t.TempDir()})

	result := invokePwsh(t, tool, `{"command":"Write-Output 'x'"}`)
	if !strings.Contains(result.Content, "中文输出") {
		t.Fatalf("合法 UTF-8 输出被破坏：%q", result.Content)
	}
	if strings.Contains(result.Content, procattr.NonUTF8OutputWarning) {
		t.Fatalf("合法 UTF-8 输出不该带非 UTF-8 告警：%q", result.Content)
	}
	if strings.Contains(result.Content, "\uFFFD") {
		t.Fatalf("合法 UTF-8 输出不该出现 U+FFFD：%q", result.Content)
	}
}

// TestPwshCommandScriptIsUntouchedOffWindows 钉住"编码前缀只在 Windows 注入"。
//
// 理由：Unix 上的 pwsh 默认就是 UTF-8；注入前置语句会改动用户的 `$LASTEXITCODE`
// 与变量命名空间，属于无收益的行为变化。Windows 分支见 windows_encoding_test.go。
func TestPwshCommandScriptIsUntouchedOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 -Command 会带 UTF-8 前置语句，见 windows_encoding_test.go")
	}
	script := fakePwsh(t, `echo "pwsh-output"`)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Root: t.TempDir()})
	const command = "Write-Output '中文输出'"
	if result := invokePwsh(t, tool, `{"command":`+encodingJSONString(command)+`}`); result.IsError {
		t.Fatalf("pwsh 失败：%q", result.Content)
	}
	args := fakePwshArgs(t, script)
	want := []string{"-NoProfile", "-NonInteractive", "-Command", command}
	if len(args) != len(want) {
		t.Fatalf("argv = %q，want %q", args, want)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("argv[%d] = %q，want %q（非 Windows 不该改写脚本）", index, args[index], want[index])
		}
	}
}

// encodingJSONString 把字符串包成 JSON 字符串字面量（测试只用 ASCII 引号，够用）。
func encodingJSONString(text string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range text {
		switch r {
		case '"', '\\':
			builder.WriteByte('\\')
			builder.WriteRune(r)
		case '\n':
			builder.WriteString(`\n`)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// requireWindowsOrUnixish 允许两类宿主：Unix（fake pwsh 是 shell 脚本）与
// Windows（真 pwsh）。Windows 上的 fake 二进制不适用，因此那里跳过本文件用
// windows_encoding_test.go 的真机用例。
func requireWindowsOrUnixish(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake pwsh 是 Unix shell 脚本；Windows 请见 windows_encoding_test.go")
	}
	_ = os.PathSeparator
	_ = filepath.Separator
	_ = rig.ToolResult{}
}

// 审批文案必须是用户原文：编码前置语句不能出现在 escalation 的 Argv 里。
//
// 审批人看到的是"到底要跑什么"；把我们自己的实现细节塞进去会让审批变成盲签。
func TestPwshApprovalArgvStaysUserScript(t *testing.T) {
	escalator := &recordingEscalator{}
	runner := &fakeRunnerForEscalation{}
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithAnyCommandAllowed(true),
	)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{
		Binary:    fakePwsh(t, `echo "ok"`),
		Policy:    policy,
		Runner:    runner,
		Escalator: escalator,
	})
	const script = "Write-Output '中文'"
	// 需要 escalation：传 sandbox_permissions + 足够长的 justification。
	arguments := `{"command":` + encodingJSONString(script) +
		`,"sandbox_permissions":"require_escalated",` +
		`"justification":"这条命令需要访问沙箱外的系统目录，沙箱内无法完成构建"}`
	invokePwsh(t, tool, arguments)
	if escalator.request.Argv == nil {
		t.Fatalf("escalator 没有被调用（策略/runner 组合不满足前置条件？）")
	}
	last := escalator.request.Argv[len(escalator.request.Argv)-1]
	if strings.Contains(last, "OutputEncoding") || strings.Contains(last, "chcp") {
		t.Fatalf("审批 argv 里混入了编码前置语句：%q", escalator.request.Argv)
	}
	if last != script {
		t.Fatalf("审批 argv 的脚本不是用户原文：%q", last)
	}
}

// recordingEscalator 记录一次提权请求（并一律拒绝，避免真的提升权限）。
type recordingEscalator struct {
	request builtin.EscalationRequest
}

func (e *recordingEscalator) Escalate(_ context.Context, req builtin.EscalationRequest) (bool, error) {
	e.request = req
	return false, nil
}

// fakeRunnerForEscalation 是最小的 sandbox.Runner：声明能强制、不做任何包装。
type fakeRunnerForEscalation struct{}

func (r *fakeRunnerForEscalation) Confine(argv []string, _ *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	return sandbox.ConfinedCommand{Argv: argv, Enforcement: sandbox.EnforcementFull}, nil
}

func (r *fakeRunnerForEscalation) Name() string { return "fake" }

func (r *fakeRunnerForEscalation) Available() error { return nil }
