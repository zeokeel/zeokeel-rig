package jobs

import (
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"bytes"

	"github.com/zeokeel/zeokeel-rig/internal/procattr"
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

// bufferRead 是一次**有上限**读取的结果。
//
// 它同时给出读到哪里（Next）以及是否还有剩余（More），调用方据此只在真正交付
// 了字节之后才推进自己的消费游标——否则被上限截断的尾巴会永久读不到。
type bufferRead struct {
	// Text 是本次交付的文本（已解码，可能带"更早字节已丢弃"与"非 UTF-8"提示）。
	Text string
	// From 是本次读取的起始绝对偏移（from 落在已丢弃区间时会被钳制到 b.base）。
	From int64
	// Next 是本次交付内容的结束绝对偏移，也是下次续读的起点。
	Next int64
	// More 表示缓冲里仍有未交付的字节（本次被 maxBytes 或调用方上限截断）。
	More bool
	// Discarded 是 from 之前已被环形缓冲丢掉的字节数。
	Discarded int64
}

// ReadFrom 从绝对偏移 from 起读取至多 maxBytes 字节（maxBytes <= 0 表示全部），
// 并返回实际交付到的绝对偏移。
func (b *outputBuffer) ReadFrom(from int64, maxBytes int) bufferRead {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := b.base + int64(len(b.data))
	read := bufferRead{From: from, Next: total}
	start := from
	if start < b.base {
		read.Discarded = b.base - from
		start = b.base
	}
	if start > total {
		start = total
	}
	read.From = start
	if start >= total {
		return read
	}
	end := total
	if maxBytes > 0 && int64(maxBytes) < total-start {
		cut := start + int64(maxBytes)
		// 优先按行边界切：分页读出的每页都以完整行结束，模型把几页拼起来就是
		// 原始输出（不会因为截断多出/少掉换行）。
		if index := bytes.LastIndexByte(b.data[start-b.base:cut-b.base], '\n'); index >= 0 {
			cut = start + int64(index) + 1
		}
		// 没有行边界（单行超长）时不切坏多字节字符，但保证至少前进一字节。
		for cut > start+1 && !utf8.RuneStart(b.data[cut-b.base]) {
			cut--
		}
		end = cut
	}
	text, replaced := procattr.DecodeOutput(b.data[start-b.base : end-b.base])
	if replaced {
		text += "\n" + procattr.NonUTF8OutputWarning
	}
	if read.Discarded > 0 {
		text = "[truncated: " + strconv.FormatInt(read.Discarded, 10) + " earlier bytes discarded]\n" + text
	}
	read.Text = text
	read.Next = end
	read.More = end < total
	return read
}

// LastLines 返回至多 n 行结尾内容（n <= 0 表示全部）。
func (b *outputBuffer) LastLines(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text, replaced := procattr.DecodeOutput(b.data)
	if replaced {
		text += "\n" + procattr.NonUTF8OutputWarning
	}
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
