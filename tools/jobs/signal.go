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

// splitShellCommand tokenizes one command line far enough to find argv[0] for
// the sandbox command allow/deny check (quotes group, backslash escapes,
// whitespace separates). It is not a full parser; the approval gate remains the
// real boundary.
func splitShellCommand(command string) ([]string, error) {
	var (
		args     []string
		current  strings.Builder
		inSingle bool
		inDouble bool
		escaped  bool
		started  bool
	)
	flush := func() {
		if started {
			args = append(args, current.String())
			current.Reset()
			started = false
		}
	}
	for _, r := range command {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
			started = true
		case r == '\\' && !inSingle:
			escaped = true
			started = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
			started = true
		case r == '"' && !inSingle:
			inDouble = !inDouble
			started = true
		case (r == ' ' || r == '\t' || r == '\n' || r == '\r') && !inSingle && !inDouble:
			flush()
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if escaped || inSingle || inDouble {
		return nil, errors.New("unterminated quote or escape in command")
	}
	flush()
	return args, nil
}
