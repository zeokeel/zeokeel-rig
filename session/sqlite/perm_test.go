package sqlite_test

// 文件权限：库按 umask（通常 0644）创建时，同机其他账户可以读走全部会话、记忆与
// 溢出结果——而这几样东西在文件后端是 0700/0600。这里钉住"打开之后三件套不再对
// 组和其他人可读"。

import (
	"context"
	"os"
	"runtime"
	"testing"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestDatabaseFilesAreNotWorldReadable 断言 .db / -wal / -shm 三件套都收紧到 0600。
//
// -wal 与 -shm 由 SQLite 在第一个写事务时创建，创建时的模式不受库文件影响
// （实测 modernc 驱动给 0666&^umask，即 0644），所以只改 .db 不够：这三件套里任何
// 一个可读，泄露的内容是一样的。
func TestDatabaseFilesAreNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 的权限模型是 ACL，os.Chmod 只有只读位，没有可移植的"只给当前
		// 账户"表达；本包在 Windows 上跳过收紧并注释说明（见 perm.go）。
		t.Skip("Windows 上没有 0600 语义，权限由 ACL 决定")
	}
	db, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	// 写一次，确保 -wal / -shm 已经存在：它们在第一个写事务时出现。
	if _, err := db.Append(ctx, "perm", userMessage("权限")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	assertStateFilesPrivate(t, path)

	// 重开一次：Close 会删掉 -wal/-shm，重开时它们会被重新创建——那一次创建也必须
	// 落在 0600 上，否则"重启之后又开始泄露"。
	second, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }()
	if _, err := second.Append(ctx, "perm", userMessage("重开之后")); err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	assertStateFilesPrivate(t, path)
}

// assertStateFilesPrivate 检查三件套的权限里没有任何"组/其他人"的位。
func assertStateFilesPrivate(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatalf("stat %s: %v（写完之后三件套都应该存在）", path+suffix, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("%s 的权限 = %04o，want 0600：同机其他账户可以读到会话内容", path+suffix, perm)
		}
	}
}
