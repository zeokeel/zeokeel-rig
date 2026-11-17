package undo

import (
	"os"
	"path/filepath"
	"strings"
)

// 这一层只保留两个"纯词法"的路径助手：
//   - isAbsoluteAnyOS：跨平台识别绝对路径（工作区里的记录可能来自别的机器）；
//   - osStat / evalSymlinks：根的规范化。

// isAbsoluteAnyOS 报告路径是否像绝对路径：POSIX 的 "/x"，Windows 的 "C:\x"、"C:/x"
// 与 UNC 的 `\\server\share` 都算。跨平台判断是刻意的——工作区里的路径可能来自另一台
// 机器生成的记录。
func isAbsoluteAnyOS(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return true
	}
	if len(path) >= 2 && path[1] == ':' {
		drive := path[0]
		if (drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z') {
			return true
		}
	}
	return false
}

// osStat 是 os.Stat 的间接层（保持 undo.go 的导入面干净，也便于测试替换）。
func osStat(name string) (os.FileInfo, error) { return os.Stat(name) }

// evalSymlinks 是 filepath.EvalSymlinks 的间接层。
func evalSymlinks(name string) (string, error) { return filepath.EvalSymlinks(name) }
