package jobs

import (
	"strings"
	"sync"
)

// outputBuffer is a thread-safe byte buffer with absolute offsets and a hard
// capacity: once full, the oldest bytes are discarded. The exec stdio copy
// goroutines write to it concurrently (stdout and stderr are merged), so every
// method locks.
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

// Write implements io.Writer.
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

// Slice returns output starting at absolute offset from; truncated is true when
// bytes before from were discarded.
func (b *outputBuffer) Slice(from int64) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := b.base + int64(len(b.data))
	if from >= total {
		return "", false
	}
	start := from
	truncated := false
	if start < b.base {
		start = b.base
		truncated = true
	}
	text := string(b.data[start-b.base:])
	if truncated {
		text = "[truncated: " + itoa(start-from) + " earlier bytes discarded]\n" + text
	}
	return text, truncated
}

// LastLines returns at most n trailing lines (n <= 0 means everything).
func (b *outputBuffer) LastLines(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := string(b.data)
	if n > 0 {
		lines := strings.Split(text, "\n")
		if len(lines) > 1 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > n {
			text = strings.Join(lines[len(lines)-n:], "\n")
		}
	}
	if b.base > 0 {
		text = "[truncated: older output was discarded]\n" + text
	}
	return text
}

// itoa formats a non-negative int64.
func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}

// truncateText shortens text to at most limit bytes without splitting a UTF-8
// rune and appends a truncation notice.
func truncateText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut] + "…\n[truncated: output exceeded " + itoa(int64(limit)) + " bytes]\n"
}
