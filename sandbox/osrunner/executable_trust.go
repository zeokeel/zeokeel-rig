package osrunner

import (
	"os"
	"path/filepath"
	"strings"
)

// 沙箱本体的可信性检查。
//
// runner 会 exec 一个"沙箱本体"（bwrap / sandbox-exec）。如果那个二进制可以被
// 被约束的进程替换，边界就形同虚设——agent 工作区经常在 PATH 靠前的位置放一个
// bin/ 目录。本文件集中放跨平台的判定逻辑，Linux 与 macOS 的探测都调用它。

// untrustedExecutableReason 报告为什么某个可执行文件不配当沙箱本体（空串表示可信）。
func untrustedExecutableReason(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return err.Error()
	}
	if info.IsDir() {
		return "path is a directory"
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "binary is writable by group or others"
	}
	if dirInfo, dirErr := os.Stat(filepath.Dir(path)); dirErr == nil && dirInfo.Mode().Perm()&0o002 != 0 {
		return "containing directory is world-writable"
	}
	if wd, wdErr := os.Getwd(); wdErr == nil {
		if rel, relErr := filepath.Rel(wd, path); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "binary lives inside the current working directory (the agent workspace)"
		}
	}
	return ""
}
