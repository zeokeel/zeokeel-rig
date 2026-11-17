//go:build !windows

package fsguard

import "syscall"

// nonBlockFlag 让 OpenRegular 打开 FIFO/socket/设备时**不阻塞**：不带它的
// open(2) 在没有写端的 FIFO 上会永久等待，宿主就会卡在"读文件"里。对普通文件
// 该标志无副作用。
const nonBlockFlag = syscall.O_NONBLOCK
