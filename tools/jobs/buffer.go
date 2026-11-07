package jobs

import (
	"strconv"
	"strings"
	"sync"
)

// outputBuffer 是一个带绝对偏移与硬容量上限的线程安全字节缓冲：写满后
// 丢弃最旧的字节。exec 的 stdio 复制 goroutine 会并发写入（stdout 与 stderr
// 已合并），因此每个方法都加锁。
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

// Write 实现 io.Writer。
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

// Slice 返回从绝对偏移 from 开始的输出；当 from 之前的字节已被丢弃时
// truncated 为 true。
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
		text = "[truncated: " + strconv.FormatInt(start-from, 10) + " earlier bytes discarded]\n" + text
	}
	return text, truncated
}

// LastLines 返回至多 n 行结尾内容（n <= 0 表示全部）。
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
