package terminal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// signalSpec 是一次请求的进程信号：符号名或 POSIX 信号编号。terminal_signal
// 两种形式都接受（`"INT"` 或 `2`）。
type signalSpec struct {
	name   string
	number int
}

// label 渲染用于面向模型文本的信号表示。
func (s signalSpec) label() string {
	if s.name != "" {
		return s.name
	}
	return fmt.Sprintf("%d", s.number)
}

// parseSignalArgument 解码 terminal_signal 的 "signal" 参数。缺省值表示
// KILL（卡住会话的安全默认值）。
func parseSignalArgument(raw json.RawMessage) (signalSpec, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return signalSpec{name: "KILL"}, nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		name = strings.ToUpper(strings.TrimSpace(name))
		switch name {
		case "INT", "SIGINT", "CTRL-C", "CTRL+C":
			return signalSpec{name: "INT"}, nil
		case "TERM", "SIGTERM":
			return signalSpec{name: "TERM"}, nil
		case "KILL", "SIGKILL":
			return signalSpec{name: "KILL"}, nil
		case "HUP", "SIGHUP":
			return signalSpec{name: "HUP"}, nil
		case "QUIT", "SIGQUIT":
			return signalSpec{name: "QUIT"}, nil
		case "TSTP", "SIGTSTP":
			return signalSpec{name: "TSTP"}, nil
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
