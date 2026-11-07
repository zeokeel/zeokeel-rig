package terminal

import (
	"strconv"
	"strings"
	"sync"
)

// outputBuffer 是一个带绝对偏移与硬容量上限的线程安全字节缓冲：写满后
// 丢弃最旧的字节。绝对偏移让读者（terminal_send/terminal_read）能从上次
// 停止的位置继续，而无需复制整个缓冲。
type outputBuffer struct {
	mu   sync.Mutex
	data []byte
	base int64 // data[0] 的绝对偏移
	cap  int
}

func newOutputBuffer(capacity int) *outputBuffer {
	if capacity <= 0 {
		capacity = DefaultBufferBytes
	}
	return &outputBuffer{cap: capacity}
}

// Write 实现 io.Writer。它从不返回错误（写入要么被保存，要么被丢弃），
// 因此即使输出巨大，exec 的复制 goroutine 也不会失败。
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

// Total 返回最后一个已存字节之后的绝对偏移。
func (b *outputBuffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.base + int64(len(b.data))
}

// Slice 返回从绝对偏移 from 开始的输出，并报告 from 之前是否有字节被
// 丢弃（此时文本以截断提示开头）。指向未来的偏移返回 ""。
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

// LastLines 返回至多 n 行结尾内容（n <= 0 表示全部输出）。
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

// Snapshot 返回保留的文本以及更早的字节是否已被丢弃。
func (b *outputBuffer) Snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data), b.base > 0
}

// Reset 清空缓冲（用于复用任务/会话时）。
func (b *outputBuffer) Reset() {
	b.mu.Lock()
	b.data = b.data[:0]
	b.base = 0
	b.mu.Unlock()
}
