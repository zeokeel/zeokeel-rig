package kit_test

import (
	"os"
	"testing"
)

// TestMain 给所有 kit 测试一个可写的状态目录；默认后端的 SQLite 库落在
// <XDG_STATE_HOME>/rig/memory/<workspace>/memory.db，不能依赖 CI 的只读 HOME。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rig-kit-state-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("XDG_STATE_HOME", dir)
	_ = os.Setenv("LOCALAPPDATA", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
