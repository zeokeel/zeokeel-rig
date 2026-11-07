package terminal

import (
	"strconv"
	"strings"
	"sync"
)

// outputBuffer is a thread-safe byte buffer with absolute offsets and a hard
// capacity: once full, the oldest bytes are discarded. Absolute offsets let a
// reader (terminal_send/terminal_read) resume from where it stopped without
// copying the whole buffer.
type outputBuffer struct {
	mu   sync.Mutex
	data []byte
	base int64 // absolute offset of data[0]
	cap  int
}

func newOutputBuffer(capacity int) *outputBuffer {
	if capacity <= 0 {
		capacity = DefaultBufferBytes
	}
	return &outputBuffer{cap: capacity}
}

// Write implements io.Writer. It never returns an error (writes are stored or
// discarded) so exec copy goroutines never fail on a huge output.
func (b *outputBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.cap {
		drop := len(b.data) - b.cap
		copy(b.data, b.data[drop:])
		b.data = b.data[:len(b.data)-drop]
		b.base += int64(drop)
	}
	return len(p), nil
}

// Total returns the absolute offset just past the last stored byte.
func (b *outputBuffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.base + int64(len(b.data))
}

// Slice returns the output starting at absolute offset from, and whether some
// bytes were discarded before from (the text then starts with a truncation
// notice). An offset in the future returns "".
func (b *outputBuffer) Slice(from int64) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := b.base + int64(len(b.data))
	if from >= total {
		return "", false
	}
	truncated := false
	start := from
	if start < b.base {
		start = b.base
		truncated = true
	}
	text := string(b.data[start-b.base:])
	if truncated {
		discarded := start - from
		text = "[truncated: " + strconv.FormatInt(discarded, 10) + " earlier bytes discarded]\n" + text
	}
	return text, truncated
}

// LastLines returns at most n trailing lines (n <= 0 means all output).
func (b *outputBuffer) LastLines(n int) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := string(b.data)
	truncated := b.base > 0
	if n > 0 {
		lines := strings.Split(text, "\n")
		if len(lines) > 1 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > n {
			text = strings.Join(lines[len(lines)-n:], "\n")
			truncated = true
		}
	}
	if truncated {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "[truncated: older output was discarded]\n"
	}
	return text, truncated
}

// Reset clears the buffer (used when a job/session is recycled).
func (b *outputBuffer) Reset() {
	b.mu.Lock()
	b.data = b.data[:0]
	b.base = 0
	b.mu.Unlock()
}
