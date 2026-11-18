//go:build darwin

package osrunner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// sandboxExecSystemPath 是首选的 sandbox-exec 路径。
//
// 固定成 /usr/bin/sandbox-exec 而不是走 PATH：runner 是**沙箱本体**，从 PATH 里挑
// 一个可被被约束进程改写的候选等于把边界交给攻击者（工作区里的 bin/ 常在 PATH
// 靠前的位置）。/usr/bin 只有 root 可写，这与 Codex 的做法一致。
//
// 声明成变量只为**测试可替换**（测试无法在真机上删掉系统自带的 sandbox-exec，
// 否则"缺 sandbox-exec 时 fail closed"这条分支永远测不到）。
var sandboxExecSystemPath = "/usr/bin/sandbox-exec"

// sandboxExecLookPath 是系统路径不可信时的回退查找（同样为测试可替换）。
var sandboxExecLookPath = exec.LookPath

// newPlatformRunner 选择 macOS 的 runner：系统 sandbox-exec（Seatbelt/SBPL）。
//
// 选择时用只读 profile 做一次功能探测（`sandbox-exec -p <profile> -- true`）。
// 二进制缺失、不可执行、profile 被拒或探测超时都返回包装
// sandbox.ErrSandboxUnavailable 的错误——调用方必须拒绝执行而不是裸跑。
//
// 探测成功后被选中的 runner 被缓存：New 只选择/探测一次，返回的 runner 的
// Confine 不再探测。
func newPlatformRunner(cfg Config) (sandbox.Runner, error) {
	execPath, err := seatbeltExecutable()
	if err != nil {
		return nil, fmt.Errorf("osrunner: sandbox-exec not usable: %w: %w", err, sandbox.ErrSandboxUnavailable)
	}
	// 固定成绝对路径：argv[0] 不带分隔符时 exec.Command 会再次查 PATH，而 PATH
	// 在探测之后可能被调用方修改。
	absolute, err := filepath.Abs(execPath)
	if err != nil {
		return nil, fmt.Errorf("osrunner: resolve sandbox-exec path %q: %w: %w", execPath, err, sandbox.ErrSandboxUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.probeTimeout())
	defer cancel()
	if err := probeSeatbelt(ctx, absolute); err != nil {
		return nil, fmt.Errorf("osrunner: sandbox-exec is not usable: %w: %w", err, sandbox.ErrSandboxUnavailable)
	}
	return &seatbeltRunner{execPath: absolute}, nil
}

// runHelper 在 macOS 上没有 helper（sandbox-exec 直接作为 argv 前缀执行）。
func runHelper(args []string) (handled bool, code int) {
	return false, 0
}

// seatbeltExecutable 返回可信的 sandbox-exec 路径。
//
// 优先固定 /usr/bin/sandbox-exec：runner 是**沙箱本体**，从 PATH 里挑一个可被
// 被约束进程改写的候选等于把边界交给攻击者（agent 工作区里常见 bin/ 在 PATH
// 靠前的位置）。/usr/bin 只有 root 可写，这与常见实现的排列一致。只有在系统
// 路径不存在时才回退 LookPath，并拒绝 group/world 可写的候选。
func seatbeltExecutable() (string, error) {
	const systemPath = "/usr/bin/sandbox-exec"
	if trustedExecutable(systemPath) {
		return systemPath, nil
	}
	candidate, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return "", fmt.Errorf("sandbox-exec not found (also missing at %s): %w", systemPath, err)
	}
	if !trustedExecutable(candidate) {
		return "", fmt.Errorf("sandbox-exec candidate %q is writable by a non-owner account; refusing to trust it", candidate)
	}
	return candidate, nil
}

// trustedExecutable 报告路径存在、是普通/符号链接文件、且不是 group/world 可写。
func trustedExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if info.Mode().Perm()&0o022 != 0 {
		return false
	}
	return true
}
