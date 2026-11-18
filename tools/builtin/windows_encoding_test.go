//go:build windows

package builtin_test

// Windows 中文路径 / 中文输出的**真机**用例（本机 Linux 只编译不执行）。
//
// 四条对应审计任务 A1–A4：
//  1. 中文路径：temp 目录下 `测试目录/中文文件.txt` 能写、能读、glob/grep 能命中；
//  2. 中文输出：`pwsh -Command "Write-Output '中文输出'"` 与 `cmd /c echo 中文`
//     的结果里就是「中文输出」，不是替换字符；
//  3. 中文错误：stderr 上的中文原样保留；
//  4. 中文文件名 + OS 沙箱：workspace-write 下写 `测试/中文.txt` 成功（ACL 授权
//     路径全程 UTF-16）。
//
// 为什么这些用例必须真机跑：代码页是**进程/控制台**状态（936 vs 65001），
// 只能由真实的 Windows 内核与控制台决定；在 Linux 上用假的 pwsh 脚本无法复现。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/internal/procattr"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// windowsPwshAvailable 报告本机是否有可用的 PowerShell（pwsh 优先）。
func windowsPwshAvailable() bool {
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := exec.LookPath(name); err == nil && strings.TrimSpace(path) != "" {
			return true
		}
	}
	return false
}

// windowsInvoke 调用一个工具并返回结果（不把工具失败当测试失败）。
func windowsInvoke(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID:        "call-" + tool.Name(),
		Name:      tool.Name(),
		Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", tool.Name(), err)
	}
	return result
}

// windowsTool 按名字从一组工具里挑一个。
func windowsTool(t *testing.T, tools []rig.Tool, name string) rig.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("工具 %q 不在集合里", name)
	return nil
}

// A1：中文路径在全部文件工具里可用。
func TestWindowsChinesePathsWorkAcrossFileTools(t *testing.T) {
	root := t.TempDir()
	tools := builtin.NewFS(root)

	result := windowsInvoke(t, windowsTool(t, tools, builtin.ToolWriteFile), `{
		"path":"测试目录/中文文件.txt",
		"content":"第一行 中文内容\n第二行 needle-中文\n"
	}`)
	if result.IsError {
		t.Fatalf("写中文路径失败：%q", result.Content)
	}

	// 落盘的文件确实存在（用 Go 自己的 API 交叉验证，绕开工具实现）。
	onDisk := filepath.Join(root, "测试目录", "中文文件.txt")
	if _, err := os.Stat(onDisk); err != nil {
		t.Fatalf("中文路径文件没有真的落盘：%v", err)
	}

	read := windowsInvoke(t, windowsTool(t, tools, builtin.ToolReadFile), `{"path":"测试目录/中文文件.txt"}`)
	if read.IsError || !strings.Contains(read.Content, "中文内容") {
		t.Fatalf("读中文路径失败：%q", read.Content)
	}
	if !utf8.ValidString(read.Content) {
		t.Fatalf("读出的内容不是合法 UTF-8：%q", read.Content)
	}

	glob := windowsInvoke(t, windowsTool(t, tools, builtin.ToolGlob), `{"pattern":"**/*中文*"}`)
	if glob.IsError || !strings.Contains(glob.Content, "中文文件.txt") {
		t.Fatalf("glob 没有命中中文文件名：%q", glob.Content)
	}

	grep := windowsInvoke(t, windowsTool(t, tools, builtin.ToolGrep), `{"pattern":"needle-中文","path":"测试目录"}`)
	if grep.IsError || !strings.Contains(grep.Content, "needle-中文") {
		t.Fatalf("grep 在中文路径里没命中：%q", grep.Content)
	}
}

// A2/A3：pwsh 与 cmd 的中文 stdout/stderr 都是 UTF-8。
func TestWindowsPwshChineseOutputAndError(t *testing.T) {
	if !windowsPwshAvailable() {
		t.Skip("本机没有 pwsh/powershell")
	}
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Root: t.TempDir(), Timeout: 60 * time.Second})

	out := windowsInvoke(t, tool, `{"command":"Write-Output '中文输出'"}`)
	if out.IsError {
		t.Fatalf("pwsh 中文输出失败：%q", out.Content)
	}
	if !strings.Contains(out.Content, "中文输出") {
		t.Fatalf("中文输出没有原样返回（代码页没切到 UTF-8？）：%q", out.Content)
	}
	if strings.Contains(out.Content, "\uFFFD") {
		t.Fatalf("中文输出含替换字符：%q", out.Content)
	}

	// cmd.exe 经由 pwsh 调用：只设 PowerShell 的 OutputEncoding 不够，必须
	// 同时把控制台代码页切成 65001（前置语句里的 chcp.com 65001）。
	cmdOut := windowsInvoke(t, tool, `{"command":"cmd /c echo 中文命令行"}`)
	if cmdOut.IsError || !strings.Contains(cmdOut.Content, "中文命令行") {
		t.Fatalf("cmd /c 中文输出乱码：%q", cmdOut.Content)
	}

	errOut := windowsInvoke(t, tool, `{"command":"[Console]::Error.WriteLine('中文错误')"}`)
	if !strings.Contains(errOut.Content, "中文错误") {
		t.Fatalf("中文 stderr 没有原样返回：%q", errOut.Content)
	}
}

// A2 补充：退出码语义不能被编码前置语句改掉。
func TestWindowsPwshExitCodeSurvivesEncodingPrologue(t *testing.T) {
	if !windowsPwshAvailable() {
		t.Skip("本机没有 pwsh/powershell")
	}
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Root: t.TempDir(), Timeout: 60 * time.Second})

	// 纯 cmdlet 脚本：退出码 0（前置语句里的 chcp 也是 0，但不能把它"漏"出去）。
	ok := windowsInvoke(t, tool, `{"command":"Write-Output '完成'"}`)
	if ok.IsError {
		t.Fatalf("正常脚本被判失败：%q", ok.Content)
	}

	// 原生命令的退出码必须透传：cmd /c exit 3 → 3。
	failed := windowsInvoke(t, tool, `{"command":"cmd /c exit 3"}`)
	if !strings.Contains(failed.Content, "[exit code: 3]") {
		t.Fatalf("退出码没有透传：%q", failed.Content)
	}

	// Unix 风格的 pwsh 里非零退出不算工具错误，但内容里要有退出码标记。
	if !failed.IsError && !strings.Contains(failed.Content, "exit code") {
		t.Fatalf("失败结果缺少退出码信息：%q", failed.Content)
	}
}

// A4：中文文件名经 OS 沙箱（workspace-write + Windows ACL）仍然可写。
func TestWindowsChinesePathWriteInsideSandbox(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("只在 Windows 上验证")
	}
	root := t.TempDir()
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	runner, err := osrunner.New(osrunner.Config{})
	if err != nil {
		t.Skipf("本机没有可用的 OS runner：%v", err)
	}
	tools := builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner))
	tool := windowsTool(t, tools, builtin.ToolWriteFile)

	result := windowsInvoke(t, tool, `{"path":"测试/中文.txt","content":"沙箱内写中文\n"}`)
	if result.IsError {
		t.Fatalf("沙箱内写中文路径失败：%q", result.Content)
	}
	if _, err := os.Stat(filepath.Join(root, "测试", "中文.txt")); err != nil {
		t.Fatalf("中文路径没有被真的创建：%v", err)
	}

	// 反过来：工作区外的中文路径必须仍然被拒（别把编码修成"路径匹配失效"）。
	outside := filepath.Join(t.TempDir(), "外部", "中文.txt")
	denied := windowsInvoke(t, tool, `{"path":"`+strings.ReplaceAll(outside, `\`, `\\`)+`","content":"x"}`)
	if !denied.IsError {
		t.Fatalf("工作区外的中文路径不该写成功：%q", denied.Content)
	}
}

// 编码兜底在 Windows 真机上的对照：故意让子进程吐 GBK 字节。
//
// 用 PowerShell 直接以 CP936 写出字节，模拟"不理会代码页的老程序"。
func TestWindowsNonUTF8OutputGetsWarning(t *testing.T) {
	if !windowsPwshAvailable() {
		t.Skip("本机没有 pwsh/powershell")
	}
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Root: t.TempDir(), Timeout: 60 * time.Second})
	// 936 编码的"中文"= D6 D0 CE C4：绕过编码前置语句，直接写原始字节。
	script := `$bytes = [byte[]](0xD6,0xD0,0xCE,0xC4); $out = [Console]::OpenStandardOutput(); $out.Write($bytes, 0, 4); $out.Flush()`
	result := windowsInvoke(t, tool, `{"command":`+encodingJSONString(script)+`}`)
	if !utf8.ValidString(result.Content) {
		t.Fatalf("结果不是合法 UTF-8：%q", result.Content)
	}
	if !strings.Contains(result.Content, procattr.NonUTF8OutputWarning) {
		t.Fatalf("非 UTF-8 输出没有告警：%q", result.Content)
	}
}
