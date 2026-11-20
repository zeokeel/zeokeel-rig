//go:build windows

package builtin

import (
	"errors"
	"os"
	"os/exec"

	"github.com/zeokeel/zeokeel-rig/internal/procattr"
)

// 前台 shell 的进程树治理（Windows）。
//
// Windows 没有 POSIX 进程组，所以"杀整棵进程树"只能靠 taskkill /T /F。这是**尽力
// 而为**：taskkill 不存在（极简镜像）、被策略禁止或超时返回错误时，退化为只杀
// 直接子进程 —— 此时孙进程可能继续运行，要根治需要宿主注入
// builtin.WithRunner(sandbox/osrunner)（Confine 之后由 runner 用自己的 Job 对象
// 治理进程树，见 sandbox/osrunner 的 windows_job_object.go）。
//
// 顺序很重要：先 taskkill（此时父进程还活着，能枚举到子进程树），再 Process.Kill()
// 兜底（taskkill 慢于截止时间时不要让工具卡住）。

// configureForegroundProcessGroup 在 Windows 上为空操作：进程树由 taskkill 处理。
func configureForegroundProcessGroup(_ *exec.Cmd) {}

// killForegroundProcessGroup 先 taskkill /T /F 整棵树，再杀直接子进程兜底。
func killForegroundProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	if argv := windowsTaskkillArgv(command.Process.Pid); len(argv) > 0 {
		// BestEffort：失败（找不到 taskkill、权限不足、进程已退出）不报错，
		// 交给下面的 Process.Kill 兜底。仍要走 procattr.HideWindow：Windows 上
		// 不隐藏控制台窗口的话，每次超时都会闪一个黑框（仓库级不变式由
		// internal/procattr 的 TestEveryChildSpawnHidesWindow 强制）。
		helper := exec.Command("taskkill", argv...)
		procattr.HideWindow(helper)
		_ = helper.Run()
	}
	if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
