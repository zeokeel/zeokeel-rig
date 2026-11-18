//go:build linux

package osrunner

import (
	"fmt"
	"os"

	"github.com/zeokeel/zeokeel-rig/internal/seccomp"
)

// 网络 seccomp 过滤器的装载路径。
//
// # 为什么必须是 seccomp
//
// 按路径屏蔽 `/var/run/docker.sock` 只能挡住名单里的几个 socket；`SSH_AUTH_SOCK`、
// dbus、X11、以及任何自定义守护进程的 socket 全都漏。拦 `connect(2)` 才是根治：
// unix socket 连接在 syscall 层就失败，请求根本发不出去。
//
// # 两种装载方式
//
//   - bwrap 后端：bwrap 0.9 支持 `--seccomp FD`（读 seccomp_export_bpf 格式的
//     原始 sock_filter 数组）。argv 里只能传数字 fd，所以用一个极小的 sh 包装把
//     程序文件打开成 fd 3，随即 unlink（fd 仍有效），再 exec bwrap。
//   - Landlock 后端：helper（宿主可执行文件的隐藏子命令）在 exec 目标命令之前
//     自己装过滤器——不需要任何额外进程。
//
// 两种方式都在"以为装了其实没装"时 fail closed：装不上就直接报错，不让命令跑。

// seccompProgram 返回本次策略需要的过滤器；AllowNetwork=true 时返回空程序
// （开发流程需要回环，不能把本地 dev server 打死）。
func seccompProgram(networkAllowed bool) (seccomp.Program, error) {
	if networkAllowed {
		return seccomp.Program{}, nil
	}
	return seccomp.NetworkDeny()
}

// seccompWrapperScript 是打开过滤器文件为 fd 3 并立刻 unlink 的包装脚本。
//
// 参数约定：`sh -c <script> rig-seccomp <progPath> <bwrap> <bwrap args...> -- <cmd...>`
// ——用 "$@" 传递，完全不经过字符串拼接，路径里有空格/引号也安全。
const seccompWrapperScript = `exec 3<"$1"; rm -f "$1"; shift; exec "$@"`

// bwrapSeccompPrefix 返回把过滤器交给 bwrap 所需的 argv 前缀与 `--seccomp` 参数。
//
// 返回的 cleanup 必须在 exec 失败时调用（成功 exec 后 fd 与文件都没了）。
func bwrapSeccompPrefix(program seccomp.Program, bwrapPath string, bwrapArgs []string) (argv []string, cleanup func(), err error) {
	if program.Empty() {
		return nil, func() {}, nil
	}
	shell, lookErr := lookPathShell()
	if lookErr != nil {
		// fail closed：不能因为缺 shell 就退回"没有 seccomp"的姿态。
		return nil, nil, fmt.Errorf("osrunner: seccomp requires a POSIX shell to hand the filter to bwrap: %w", lookErr)
	}
	file, createErr := os.CreateTemp("", "rig-seccomp-*.bpf")
	if createErr != nil {
		return nil, nil, fmt.Errorf("osrunner: create seccomp filter file: %w", createErr)
	}
	cleanup = func() { _ = os.Remove(file.Name()) }
	if _, writeErr := file.Write(program.Bytes()); writeErr != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, fmt.Errorf("osrunner: write seccomp filter: %w", writeErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		cleanup()
		return nil, nil, fmt.Errorf("osrunner: close seccomp filter: %w", closeErr)
	}
	// 0o600 + 临时文件名随机：过滤器内容不是秘密，但它不该被别的用户改写
	// （改写等于换掉过滤器）。
	if chmodErr := os.Chmod(file.Name(), 0o600); chmodErr != nil {
		cleanup()
		return nil, nil, fmt.Errorf("osrunner: chmod seccomp filter: %w", chmodErr)
	}

	argv = []string{shell, "-c", seccompWrapperScript, "rig-seccomp", file.Name(), bwrapPath}
	argv = append(argv, bwrapArgs...)
	return argv, cleanup, nil
}

// lookPathShell 找到第一个可用的 POSIX shell（bwrap 包装脚本要用它打开 fd）。
func lookPathShell() (string, error) {
	for _, candidate := range []string{"/bin/sh", "/usr/bin/sh", "/bin/bash"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no POSIX shell found in /bin/sh, /usr/bin/sh, /bin/bash")
}
