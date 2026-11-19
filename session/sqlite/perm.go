package sqlite

// 库文件的权限收紧。
//
// 数据库里装的是**全部会话正文、记忆与溢出结果**，而 SQLite 按 umask 创建文件
// （通常是 0644）：同一台机器上的其他账户可以直接读走。这正是"记忆是 0700/0600，
// 会话库却是 0644"的落差，也是 PLATFORM §4-4 要求修的第一件事。
//
// 为什么三件套都要管：WAL 模式下 -wal 里是**尚未 checkpoint 的最新写入**，-shm 是
// 共享内存索引；只看 .db 等于"把最近几轮对话留在 0644 的文件里"。实测 modernc
// 驱动创建 -wal/-shm 时并不继承库文件的模式（0666&^umask），所以必须逐个设。
//
// 为什么放在 Open 里（而不是每次写之后）：-wal/-shm 在 Open 期间就已经存在
// （建表/写 user_version 都会进入 WAL 模式），而它们在最后一个连接关闭时被删除、
// 下次 Open 时按当时的新模式重建。也就是说"每次打开收紧一次"覆盖了它们的整个
// 生命周期（实测：重开之后第一次写不会新建它们）。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
)

// privateFileMode 是库文件与其 WAL 附属文件的权限：只有属主可读写。
const privateFileMode = 0o600

// stateFileSuffixes 是 SQLite 在 WAL 模式下为一个数据库维护的全部文件。
var stateFileSuffixes = []string{"", "-wal", "-shm"}

// chmodStateFiles 把 .db / -wal / -shm 收紧到 0600。
//
// -wal/-shm 可能不存在（只读打开、或第一个写事务还没发生）：不存在就跳过，这
// **不是**错误——不能因为"还没有 WAL"就让 Open 失败。
func chmodStateFiles(path string) error {
	if runtime.GOOS == "windows" {
		// Windows 上没有 0600 这一套 POSIX 权限位：访问控制由 ACL 决定，os.Chmod
		// 只映射"只读"属性，改不成"仅当前账户"。这里跳过是**有意的**——在 Windows
		// 上假装设置了 0600 会比什么都不做更危险（给人一种已经保护了的错觉）。
		// 需要收紧时由宿主用 icacls 或文件系统 ACL 完成。
		return nil
	}
	for _, suffix := range stateFileSuffixes {
		name := path + suffix
		if err := os.Chmod(name, privateFileMode); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("sqlite: restrict permissions on %s: %w", name, err)
		}
	}
	return nil
}
