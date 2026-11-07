package terminal

import "slices"

// StripANSI 从一个输出块中移除 ANSI/VT 转义序列，并把 CRLF 归一化为 LF。
// 它被导出，便于宿主复用 PTY 传输所执行的同一套清洗逻辑；跨读取边界被切开
// 的多字节序列由 ansiFilter 处理，本函数用于完整字符串。
func StripANSI(text string) string {
	var filter ansiFilter
	return string(filter.Feed([]byte(text)))
}

// ANSI 过滤器状态。
const (
	ansiNormal = iota
	ansiEscape
	ansiCSI
	ansiOSC
	ansiOSCEscape
)

// ansiFilter 是流式清洗器：PTY 读取可能把转义序列或 CRLF 对切开，因此
// 解析状态必须在多次 Feed 调用之间保留。
type ansiFilter struct {
	state int
	out   []byte
}

// Feed 消费一个输出块并返回清洗后的字节。转义序列会被丢弃，'\r' 会被
// 丢弃（CRLF 变成 LF），块边界上孤立的 ESC 会保留到下一个块。
func (f *ansiFilter) Feed(chunk []byte) []byte {
	f.out = f.out[:0]
	for _, c := range chunk {
		switch f.state {
		case ansiNormal:
			switch {
			case c == 0x1b: // ESC
				f.state = ansiEscape
			case c == '\r':
				// 丢弃 CRLF 中的 CR；终端用 LF 表示换行。
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
				// 中间字节（例如 ESC ( B 中的 ESC (）：继续停留在转义状态，
				// 直到终止字节到达。
			case c >= 0x30 && c <= 0x7e:
				// 双字符转义的终止字节；序列到此结束。
				f.state = ansiNormal
			default:
				f.state = ansiNormal
			}
		case ansiCSI:
			// CSI = ESC '[' 参数字节 0x30-0x3F、中间字节 0x20-0x2F、
			// 终止字节 0x40-0x7E。
			if c >= 0x40 && c <= 0x7e {
				f.state = ansiNormal
			}
		case ansiOSC:
			switch c {
			case 0x07: // BEL 终止符
				f.state = ansiNormal
			case 0x1b:
				f.state = ansiOSCEscape
			}
		case ansiOSCEscape:
			if c == '\\' { // ST（ESC \）
				f.state = ansiNormal
			} else if c != 0x1b {
				f.state = ansiOSC
			}
		}
	}
	return slices.Clone(f.out)
}
