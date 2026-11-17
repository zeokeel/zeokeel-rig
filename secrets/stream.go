package secrets

import (
	"bytes"
	"io"
	"strings"
)

const (
	// maxBuffered 是流式处理未决缓冲的硬上限，同时也是"强行冲刷时保留的尾部窗口"：
	//
	//   - 未决缓冲永远不会超过 maxBuffered；
	//   - 只有一整行都没有换行（无法借行边界判定）且超过上限时才强制冲刷，且冲刷
	//     后仍保留最后 maxBuffered 字节，因此任何不超过 64 KiB 的凭据都不会被切断。
	//
	// 之所以可以这么宽：正常输入（有换行）根本用不到这个窗口，行边界已经足够。
	maxBuffered = 64 << 10
	// forcedBackoff 是强制冲刷时为了不切断 token 而允许把边界回退的最大字节数。
	// 它从 maxBuffered 里预留出来，因此回退不会让未决缓冲越过上限。
	forcedBackoff = 1 << 10
)

// MaxBuffered 是流式 Writer/Reader 允许扣留的最大字节数（64 KiB）。
const MaxBuffered = maxBuffered

// Flusher 是 Writer 返回值提供的可选接口：写完最后一块后调用 Flush，
// 才能把被扣留的尾巴（通常只有最后一个未完成行）输出到底层 writer。
//
// 不调用 Flush 不会泄露秘密，但会丢尾巴——因此典型用法是
// defer writer.(Flusher).Flush() 或 defer writer.(io.Closer).Close()。
type Flusher interface {
	Flush() error
}

// Writer 返回一个流式脱敏的 io.Writer，写入内容按块脱敏后转给 w。
//
// 为了不把一个 token 切成两半泄露出去，它扣留"最后一个未完成行"，并且至少扣留
// 未完成行（未决缓冲上限 MaxBuffered）。返回值的具体类型还实现
// Flusher（Flush 输出尾巴）与 io.Closer（Close 等价于 Flush，不会关闭 w）。
//
// w 为 nil 时按丢弃处理（不 panic）。
func (r *Redactor) Writer(w io.Writer) io.Writer {
	if w == nil {
		w = io.Discard
	}
	return &redactingWriter{r: r, dst: w}
}

// redactingWriter 是 Writer 的实现。
type redactingWriter struct {
	r   *Redactor
	dst io.Writer
	// buf 是尚未输出的尾巴（可能包含一个未完成的凭据）。
	buf []byte
	// suppressKeyBody 为真表示私钥块的占位符已经输出，正在丢弃块内正文直到 END。
	suppressKeyBody bool
	// err 记录第一次写失败，后续 Write/Flush 直接返回它。
	err error
}

// Write 实现 io.Writer。
func (w *redactingWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	w.buf = append(w.buf, p...)
	w.drain(false)
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

// Flush 输出所有被扣留的内容。
func (w *redactingWriter) Flush() error {
	if w.err != nil {
		return w.err
	}
	w.drain(true)
	return w.err
}

// Close 等同于 Flush（不关闭底层 writer）。
func (w *redactingWriter) Close() error { return w.Flush() }

// pending 是未决缓冲的字节数（供包内测试断言上限，不导出）。
func (w *redactingWriter) pending() int { return len(w.buf) }

// drain 尽可能多地输出 buf：final 为真时连同扣留的尾巴一起输出。
func (w *redactingWriter) drain(final bool) {
	for {
		if w.err != nil {
			return
		}
		if w.suppressKeyBody {
			index := privateKeyEndIndex(w.buf)
			if index < 0 {
				// 还没等到 END：块内正文继续丢弃。
				w.buf = w.buf[:0]
				return
			}
			w.buf = append(w.buf[:0], w.buf[index:]...)
			w.suppressKeyBody = false
			continue
		}
		if final {
			if len(w.buf) > 0 {
				w.emit(w.r.Redact(string(w.buf)))
				w.buf = w.buf[:0]
			}
			return
		}
		emit, suppress := nextEmit(w.buf)
		if emit <= 0 {
			return
		}
		w.emit(w.r.Redact(string(w.buf[:emit])))
		if emit >= len(w.buf) {
			w.buf = w.buf[:0]
		} else {
			w.buf = append(w.buf[:0], w.buf[emit:]...)
		}
		if suppress {
			w.suppressKeyBody = true
			w.buf = w.buf[:0]
		}
	}
}

// emit 把一段已脱敏的文本写给底层 writer。
func (w *redactingWriter) emit(s string) {
	if s == "" || w.err != nil {
		return
	}
	if _, err := io.WriteString(w.dst, s); err != nil {
		w.err = err
	}
}

// Reader 返回一个流式脱敏的 io.Reader，与 Writer 共享同一套边界判定。
//
// rd 为 nil 时返回空 reader（不 panic）。
func (r *Redactor) Reader(rd io.Reader) io.Reader {
	if rd == nil {
		return strings.NewReader("")
	}
	return &redactingReader{r: r, src: rd, chunk: make([]byte, 32<<10)}
}

// redactingReader 是 Reader 的实现。
type redactingReader struct {
	r     *Redactor
	src   io.Reader
	chunk []byte
	// buf 是尚未判定的尾巴；out 是已脱敏、等待交给调用方的数据。
	buf             []byte
	out             []byte
	suppressKeyBody bool
	err             error
}

// Read 实现 io.Reader。
func (rr *redactingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if len(rr.out) > 0 {
			n := copy(p, rr.out)
			rr.out = rr.out[n:]
			if len(rr.out) == 0 {
				rr.out = nil
			}
			return n, nil
		}
		if rr.err != nil {
			return 0, rr.err
		}
		n, err := rr.src.Read(rr.chunk)
		if n > 0 {
			rr.buf = append(rr.buf, rr.chunk[:n]...)
		}
		rr.pump(err != nil)
		if err != nil {
			rr.err = err
		}
		if n == 0 && err == nil && len(rr.out) == 0 {
			// 源返回 (0,nil) 且没有任何可输出的内容：这是 io.Reader 允许的"暂时没
			// 有数据"，把它如实交回调用方，而不是在本循环里继续重试（那会把一个
			// 坏 reader 变成输出路径上的忙等）。
			return 0, nil
		}
	}
}

// pump 把 buf 中可安全输出的部分移入 out；final 为真表示源已结束。
func (rr *redactingReader) pump(final bool) {
	for {
		if rr.suppressKeyBody {
			index := privateKeyEndIndex(rr.buf)
			if index < 0 {
				rr.buf = rr.buf[:0]
				return
			}
			rr.buf = append(rr.buf[:0], rr.buf[index:]...)
			rr.suppressKeyBody = false
			continue
		}
		if final {
			if len(rr.buf) > 0 {
				rr.out = append(rr.out, rr.r.Redact(string(rr.buf))...)
				rr.buf = rr.buf[:0]
			}
			return
		}
		emit, suppress := nextEmit(rr.buf)
		if emit <= 0 {
			return
		}
		rr.out = append(rr.out, rr.r.Redact(string(rr.buf[:emit]))...)
		if emit >= len(rr.buf) {
			rr.buf = rr.buf[:0]
		} else {
			rr.buf = append(rr.buf[:0], rr.buf[emit:]...)
		}
		if suppress {
			rr.suppressKeyBody = true
			rr.buf = rr.buf[:0]
		}
	}
}

// nextEmit 决定当前缓冲里可以立即输出多少字节。
//
// 规则（按顺序）：
//  1. 未闭合的私钥块：从 BEGIN 起整块扣留，等 END 到达后由模式一次性替换；
//     若该块本身已超过 maxBuffered，则吐出一整段（含块头，替换成占位符）并置
//     suppress 为真，后续字节一直丢弃到 END。
//  2. 平时只输出到最后一个 '\n' 之前——所有内置模式都只在单行内匹配，所以扣留
//     未完成行就足以避免切断凭据。
//  3. 单行超过 maxBuffered（换行永远不会来）时强制放出，但仍保留 maxBuffered 字节；
//     此时边界会先回退到最近的分隔符之后，避免把一条没有分隔符的超长 token 切断。
func nextEmit(buf []byte) (emit int, suppress bool) {
	if len(buf) == 0 {
		return 0, false
	}
	if index := unterminatedPrivateKeyIndex(buf); index >= 0 {
		if index > 0 {
			return index, false
		}
		if len(buf) > maxBuffered {
			return len(buf), true
		}
		return 0, false
	}
	// 完整行永远可以立刻输出：所有内置模式都只在单行内匹配，所以只有"最后一个
	// 未完成行"可能藏着跨 chunk 的凭据。
	safe := bytes.LastIndexByte(buf, '\n') + 1
	if safe < 0 {
		safe = 0
	}
	// 单行过长（没有换行可借来判定边界）时按硬上限强制放出，只保留尾部窗口。
	if len(buf) > maxBuffered {
		if floor := len(buf) - (maxBuffered - forcedBackoff); safe < floor {
			safe = floor
		}
		// 边界落在一条 token 中间时向前回退到分隔符，回退量上限为 forcedBackoff，
		// 因此未决缓冲仍然不超过 maxBuffered。
		if safe > 0 && safe < len(buf) && isTokenByte(buf[safe-1]) {
			safe = backoffToDelimiter(buf, safe, forcedBackoff)
		}
	}
	// 私钥块是唯一跨行的模式：如果边界落在一个"BEGIN 与 END 都在缓冲里"的块中间，
	// 就把边界退到块头，让整块和它的 END 一起被替换掉（否则会先替换掉半个块，
	// 再把 -----END----- 原样吐出去）。
	if begin := bytes.LastIndex(buf[:safe], []byte("-----BEGIN")); begin >= 0 {
		if end := privateKeyEndIndex(buf[begin:]); end >= 0 && begin+end > safe {
			safe = begin
		}
	}
	return safe, false
}

// backoffToDelimiter 把边界向前回退到最近的分隔符之后，最多回退 limit 字节。
// 找不到分隔符（整段都是 token 字符）时原样返回边界。
func backoffToDelimiter(buf []byte, boundary, limit int) int {
	lowest := boundary - limit
	if lowest < 0 {
		lowest = 0
	}
	for index := boundary - 1; index >= lowest; index-- {
		if !isTokenByte(buf[index]) {
			return index + 1
		}
	}
	return boundary
}

// isTokenByte 报告 b 是否可能出现在凭据中间。内置模式的字符集是字母、数字与
// "+/=-_.~"，这里取它们的并集：这段字符连续出现时不能从中间切断。
func isTokenByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '+', '/', '=', '_', '-', '.', '~':
		return true
	}
	return false
}

// unterminatedPrivateKeyIndex 返回 buf 中最后一个"私钥 BEGIN 且没有对应 END"的
// 偏移；没有则返回 -1。证书 BEGIN 之类的非私钥标记会被跳过。
func unterminatedPrivateKeyIndex(buf []byte) int {
	for offset := len(buf); offset > 0; {
		index := bytes.LastIndex(buf[:offset], []byte("-----BEGIN"))
		if index < 0 {
			return -1
		}
		lineEnd := bytes.IndexByte(buf[index:], '\n')
		line := buf[index:]
		if lineEnd >= 0 {
			line = buf[index : index+lineEnd]
		}
		if isPrivateKeyMarkerLine(line) {
			if privateKeyEndIndex(buf[index:]) >= 0 {
				return -1
			}
			return index
		}
		offset = index
	}
	return -1
}

// privateKeyEndIndex 返回 buf 中第一个 "-----END ... PRIVATE KEY-----" 之后的偏移
// （吃掉行尾的 \r\n）；找不到返回 -1。
func privateKeyEndIndex(buf []byte) int {
	const endMarker = "-----END"
	const privateKey = " PRIVATE KEY"
	offset := 0
	for {
		index := bytes.Index(buf[offset:], []byte(endMarker))
		if index < 0 {
			return -1
		}
		index += offset
		line := buf[index:]
		if lineEnd := bytes.IndexByte(line, '\n'); lineEnd >= 0 {
			line = buf[index : index+lineEnd]
		}
		if rel := bytes.Index(line, []byte(privateKey)); rel >= 0 {
			// 允许 "-----END RSA PRIVATE KEY-----" 与
			// "-----END PGP PRIVATE KEY BLOCK-----" 两种收尾形状。
			rest := line[rel+len(privateKey):]
			suffix := 0
			switch {
			case bytes.HasPrefix(rest, []byte("-----")):
				suffix = len("-----")
			case bytes.HasPrefix(rest, []byte(" BLOCK-----")):
				suffix = len(" BLOCK-----")
			}
			if suffix == 0 {
				offset = index + len(endMarker)
				continue
			}
			end := index + rel + len(privateKey) + suffix
			if end < len(buf) && buf[end] == '\r' {
				end++
			}
			if end < len(buf) && buf[end] == '\n' {
				end++
			}
			return end
		}
		offset = index + len(endMarker)
	}
}

// isPrivateKeyMarkerLine 报告一行是否是私钥块的 BEGIN/END 标记行。
//
// 只认含 "PRIVATE KEY" 的标记：证书块（`-----BEGIN CERTIFICATE-----`）与 PGP 公钥
// 块（`-----BEGIN PGP PUBLIC KEY BLOCK-----`）都不是秘密，不能整块吞掉。
func isPrivateKeyMarkerLine(line []byte) bool {
	return bytes.Contains(line, []byte("PRIVATE KEY"))
}
