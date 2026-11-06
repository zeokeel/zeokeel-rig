package terminal

import "strings"

// StripANSI removes ANSI/VT escape sequences from one chunk of terminal output
// and normalizes CRLF to LF. It is exported so hosts can reuse the exact
// sanitizer the PTY transport applies; multi-byte sequences split across reads
// are handled by ansiFilter, this function is for whole strings.
func StripANSI(text string) string {
	var filter ansiFilter
	return string(filter.Feed([]byte(text)))
}

// ANSI filter states.
const (
	ansiNormal = iota
	ansiEscape
	ansiCSI
	ansiOSC
	ansiOSCEscape
)

// ansiFilter is a streaming sanitizer: PTY reads can split an escape sequence
// or a CRLF pair, so parser state must survive between Feed calls.
type ansiFilter struct {
	state int
	out   []byte
}

// Feed consumes one output chunk and returns the sanitized bytes. Escape
// sequences are dropped, '\r' is dropped (CRLF becomes LF), and a lone ESC at a
// chunk boundary is held until the next chunk.
func (f *ansiFilter) Feed(chunk []byte) []byte {
	f.out = f.out[:0]
	for _, c := range chunk {
		switch f.state {
		case ansiNormal:
			switch {
			case c == 0x1b: // ESC
				f.state = ansiEscape
			case c == '\r':
				// Drop the CR of CRLF; terminals use LF for line breaks.
			default:
				f.out = append(f.out, c)
			}
		case ansiEscape:
			switch {
			case c == '[':
				f.state = ansiCSI
			case c == ']':
				f.state = ansiOSC
			case c >= 0x20 && c <= 0x2f:
				// Intermediate byte (e.g. ESC ( in ESC ( B): stay escaped
				// until the final byte arrives.
			case c >= 0x30 && c <= 0x7e:
				// Final byte of a two-character escape; the sequence is done.
				f.state = ansiNormal
			default:
				f.state = ansiNormal
			}
		case ansiCSI:
			// CSI = ESC '[' parameter bytes 0x30-0x3F, intermediates 0x20-0x2F,
			// final byte 0x40-0x7E.
			if c >= 0x40 && c <= 0x7e {
				f.state = ansiNormal
			}
		case ansiOSC:
			switch c {
			case 0x07: // BEL terminator
				f.state = ansiNormal
			case 0x1b:
				f.state = ansiOSCEscape
			}
		case ansiOSCEscape:
			if c == '\\' { // ST (ESC \)
				f.state = ansiNormal
			} else if c != 0x1b {
				f.state = ansiOSC
			}
		}
	}
	return append([]byte(nil), f.out...)
}

// stripANSIString is the one-shot variant used by tests and last-minute
// sanitizing of already-buffered text.
func stripANSIString(text string) string {
	if !strings.ContainsRune(text, 0x1b) && !strings.ContainsRune(text, '\r') {
		return text
	}
	return StripANSI(text)
}
