package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Decoder 逐行解析 NDJSON 事件流，一次返回一个 Envelope。
//
// 语义：
//   - 空行（只有空白）被跳过；CRLF 与 LF 都接受。
//   - 空流返回 io.EOF。
//   - version 不等于 Version（更高，或缺失/非正）返回 ErrUnsupportedVersion；
//     本包绝不"尽力解释"未知版本。
//   - 非法 JSON（含被截断的最后一行）返回 ErrMalformed。
//   - 单行超过 MaxLineBytes 返回 ErrLineTooLong，并终止该流的解析（无法可靠
//     重新对齐行边界）；终止后的 Next 反复返回同一错误，不会退化成 io.EOF。
//   - 未知 kind 不报错：payload 原样保留，消费方自行透传。
//
// 返回的 Envelope 的 Time 已规范成 UTC；Payload 是独立拷贝，不与内部读缓冲
// 共享内存，可长期持有。
type Decoder struct {
	r     *bufio.Reader
	line  int   // 已读物理行数
	eof   bool  // 已到流末：后续 Next 返回 io.EOF
	fatal error // 不可恢复的解析错误：后续 Next 反复返回它，绝不静默变成 EOF
}

// NewDecoder 返回从 r 读取事件流的 decoder。
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, 64*1024)}
}

// Next 返回下一条事件；流结束时返回 io.EOF。
func (d *Decoder) Next() (Envelope, error) {
	if d.fatal != nil {
		return Envelope{}, d.fatal
	}
	if d.eof {
		return Envelope{}, io.EOF
	}
	for {
		line, err := d.readLine()
		if err != nil {
			return Envelope{}, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			return Envelope{}, fmt.Errorf("%w: 第 %d 行: %v", ErrMalformed, d.line, err)
		}
		if env.Version != Version {
			d.fatal = fmt.Errorf("%w: 第 %d 行 version=%d，本包支持 %d", ErrUnsupportedVersion, d.line, env.Version, Version)
			return Envelope{}, d.fatal
		}
		env.Time = normalizeTime(env.Time)
		return env, nil
	}
}

// readLine 读一行（去掉行尾 \r\n / \n）。
//
// 用 ReadSlice 手工成行而不是 Scanner：这样可以在"超长"与"截断"两种坏输入上
// 给出确定的错误，而不是让 Scanner 的 ErrTooLong 泛化成一个模糊的失败。
func (d *Decoder) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := d.r.ReadSlice('\n')
		if len(line)+len(chunk) > MaxLineBytes+1 { // +1 是行尾换行
			d.fatal = fmt.Errorf("%w: 第 %d 行超过 %d 字节", ErrLineTooLong, d.line+1, MaxLineBytes)
			return nil, d.fatal
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			d.line++
			return trimEOL(line), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				d.eof = true
				return nil, io.EOF
			}
			// 最后一行可以没有换行（编辑器/管道截断很常见）。
			d.line++
			d.eof = true
			return trimEOL(line), nil
		default:
			d.fatal = err
			return nil, err
		}
	}
}

// trimEOL 去掉行尾的 \n 与 \r。
func trimEOL(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line
}

// Replay 回放整个事件流：对每条事件调用 fn，直到流结束（返回 nil）、解码出错
// （返回该错误）或 fn 返回错误（原样返回并中断）。
//
// 未知 kind 会被照常交给 fn——回放/转发的调用方不需要认识所有 kind。
func Replay(r io.Reader, fn func(Envelope) error) error {
	if fn == nil {
		return errors.New("events: Replay 需要非 nil 回调")
	}
	dec := NewDecoder(r)
	for {
		env, err := dec.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(env); err != nil {
			return err
		}
	}
}
