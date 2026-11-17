//go:build unix

package instructions_test

import "syscall"

// mkfifoHelper 在 Unix 上创建命名管道（FIFO）。
func mkfifoHelper(path string) error { return syscall.Mkfifo(path, 0o644) }
