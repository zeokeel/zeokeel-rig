package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// signalSpec 是一次请求的进程信号：符号名或 POSIX 信号编号。job_kill
// 两者都接受（`"TERM"` 或 `15`）。
type signalSpec struct {
	name   string
	number int
}

// label 渲染用于面向模型文本与任务详情的信号表示。
func (s signalSpec) label() string {
	if s.name != "" {
		return s.name
	}
	return fmt.Sprintf("%d", s.number)
}

// parseSignalArgument 解码 job_kill 的 "signal" 参数。缺省值表示 KILL
// （后台任务通常需要强制停止）。
func parseSignalArgument(raw json.RawMessage) (signalSpec, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return signalSpec{name: "KILL"}, nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		name = strings.ToUpper(strings.TrimSpace(name))
		switch name {
		case "INT", "TERM", "KILL", "HUP", "QUIT":
			return signalSpec{name: name}, nil
		default:
			return signalSpec{}, fmt.Errorf("unknown signal %q (want INT, TERM, KILL, HUP or QUIT)", name)
		}
	}
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		if number < 1 || number > 64 {
			return signalSpec{}, fmt.Errorf("signal number %d is out of range 1-64", number)
		}
		return signalSpec{number: number}, nil
	}
	return signalSpec{}, errors.New("signal must be a name or a number")
}
