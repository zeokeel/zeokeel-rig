package terminal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// signalSpec is one requested process signal: a symbolic name or a POSIX signal
// number. Both forms are accepted by terminal_signal (`"INT"` or `2`).
type signalSpec struct {
	name   string
	number int
}

// parseSignalArgument decodes the terminal_signal "signal" argument. A missing
// value means KILL (the safe default for a stuck session).
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
