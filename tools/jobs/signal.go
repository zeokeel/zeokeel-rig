package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// signalSpec is one requested process signal: a symbolic name or a POSIX signal
// number. job_kill accepts both (`"TERM"` or `15`).
type signalSpec struct {
	name   string
	number int
}

// label renders the signal for model-facing text and job details.
func (s signalSpec) label() string {
	if s.name != "" {
		return s.name
	}
	return fmt.Sprintf("%d", s.number)
}

// parseSignalArgument decodes the job_kill "signal" argument. A missing value
// means KILL (background jobs usually need a forceful stop).
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
