package osrunner

import (
	"os"
	"path/filepath"
)

// 宿主守护进程 socket：沙箱的**完全逃逸面**。
//
// 实测（read-only 模式，bwrap 0.9）：
//
//	docker run --rm -v /:/host:ro alpine cat /host/etc/hostname
//
// 会打印出宿主 hostname。`--ro-bind / /`、`--unshare-net`、`--cap-drop ALL`
// 都拦不住，因为限制只作用于**文件效果**，而经宿主守护进程 unix socket 发出的
// 请求是"宿主动作"——容器由宿主 docker daemon 创建，与沙箱域无关。
//
// 因此这些 socket 必须被显式屏蔽（`--ro-bind /dev/null <sock>`）：被约束进程
// 看到的仍然是一个文件（不是"不存在"，避免制造新的失败模式），但连上去只会读到
// 一个空设备，命令必然失败。
//
// Landlock 无法治理 unix socket（它只管 TCP bind/connect），所以 Landlock 后端
// 下这条只能靠 RunnerLimits 如实上报。

// daemonSocketPaths 返回需要屏蔽的宿主守护进程 socket 候选路径（可能不存在）。
func daemonSocketPaths() []string {
	return []string{
		"/var/run/docker.sock",
		"/run/docker.sock",
		"/run/podman/podman.sock",
		"/var/run/podman/podman.sock",
		"/run/containerd/containerd.sock",
		"/var/run/containerd/containerd.sock",
	}
}

// existingDaemonSockets 返回**当前存在**的守护进程 socket 路径。
//
// 只屏蔽存在的：给不存在的路径做挂载会制造新路径（bwrap 要求挂载目标存在，
// 否则直接失败），也会让诊断里出现一堆幻影条目。
func existingDaemonSockets() []string {
	var out []string
	seen := make(map[string]struct{})
	for _, path := range daemonSocketPaths() {
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSocket == 0 && !info.Mode().IsRegular() {
			continue
		}
		// 解析符号链接（/var/run -> /run 是最常见的形态）：bwrap 在创建挂载点时
		// **不穿越符号链接**，直接挂 /var/run/docker.sock 会失败并报
		// "Can't create file at /var/run/docker.sock: No such file or directory"，
		// 而 /run/docker.sock 是同一个 inode 的真实路径，挂上去才能生效。
		resolved := filepath.Clean(path)
		if real, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
			resolved = filepath.Clean(real)
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		out = append(out, resolved)
	}
	return out
}

// bwrapSocketMaskingArgs 把被屏蔽的 socket 渲染成 bwrap 参数。
//
// `--ro-bind /dev/null <sock>` 在 `--ro-bind / /` 之后执行，所以它覆盖那个
// socket 文件本身；其它路径不受影响。
func bwrapSocketMaskingArgs(sockets []string) []string {
	args := make([]string, 0, 3*len(sockets))
	for _, sock := range sockets {
		if sock == "" {
			continue
		}
		args = append(args, "--ro-bind", "/dev/null", sock)
	}
	return args
}
