//go:build unix

package fsguard_test

import "syscall"

// mkfifoForTest 在 Unix 上创建命名管道；非 Unix 平台由另一个文件返回错误。
func mkfifoForTest(path string, mode uint32) error { return syscall.Mkfifo(path, mode) }
