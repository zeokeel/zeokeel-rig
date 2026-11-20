package undo

// 回合号的持久化。
//
// 只有一项状态需要跨进程保留：**最近分配过的回合号**。快照本身各有各的存放方式
// （git 对象库或快照目录），回合号却必须由宿主目录记住——否则重启后从 1 重新开始，
// 用户点"撤销"就会撤到上一进程的某个回合，或者干脆报"回合不可用"。
//
// 文件格式是 `Dir/state.json`：
//
//	{"turn": 7}
//
// 写入是"临时文件 + rename"，避免崩溃时留下半个 JSON 让下一次启动读不回来。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// stateFileName 是回合号状态文件名，位于后端目录下。
const stateFileName = "state.json"

// turnState 是持久化的回合号状态。
type turnState struct {
	// Turn 是最近分配过的回合号（0 表示还没分配过）。
	Turn int `json:"turn"`
}

// loadTurnState 读回 dir 里持久化的回合号。dir 为空或文件不存在时返回 0（没有历史）。
func loadTurnState(dir string) (int, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return 0, nil
	}
	data, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("undo: read turn state: %w", err)
	}
	var state turnState
	if err := json.Unmarshal(data, &state); err != nil {
		return 0, fmt.Errorf("undo: parse %s: %w", stateFileName, err)
	}
	if state.Turn < 0 {
		return 0, fmt.Errorf("undo: %s has an invalid turn number %d", stateFileName, state.Turn)
	}
	return state.Turn, nil
}

// saveTurnState 原子写入 dir 里的回合号。dir 为空或 turn<=0 时什么也不做。
func saveTurnState(dir string, turn int) error {
	dir = strings.TrimSpace(dir)
	if dir == "" || turn <= 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("undo: create snapshot directory %q: %w", dir, err)
	}
	data, err := json.Marshal(turnState{Turn: turn})
	if err != nil {
		return fmt.Errorf("undo: encode turn state: %w", err)
	}
	target := filepath.Join(dir, stateFileName)
	// 临时文件名带随机后缀：同一个 Dir 上的两个实例（或两个进程）同时落盘时不会写到
	// 同一个临时文件上。
	temporary, err := os.CreateTemp(dir, stateFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("undo: write turn state: %w", err)
	}
	temporaryName := temporary.Name()
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		_ = os.Remove(temporaryName)
		return fmt.Errorf("undo: write turn state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryName)
		return fmt.Errorf("undo: write turn state: %w", err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		_ = os.Remove(temporaryName)
		return fmt.Errorf("undo: replace turn state: %w", err)
	}
	return nil
}
