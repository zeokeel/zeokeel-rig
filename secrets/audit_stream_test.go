package secrets

// 本文件是流式正确性与 API 契约审计：未 Flush 的尾部、Flush 之后继续写、Close、
// 底层错误、空写、Reader 的 EOF 与 (0,nil) 行为、Reader/Writer 混用、并发竞态、
// nil 接收者与 Add 的边角输入、RedactBytes 的别名语义。

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// auditFailingWriter 记录调用次数并持续失败（包内已有同名类型的简化版）。
type auditFailingWriter struct {
	writes int
	err    error
}

func (w *auditFailingWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, w.err
}

// TestAuditStream_FlushSemantics 固定尾部语义：不 Flush 时最后一行被扣留（数据
// 丢失但绝不泄露），Flush 后输出；Flush 之后继续写仍然脱敏。
func TestAuditStream_FlushSemantics(t *testing.T) {
	var dst bytes.Buffer
	w := New().Writer(&dst)
	if _, err := w.Write([]byte("first line\n" + "token=abcdefghijklmnop")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.Contains(dst.String(), "abcdefghijklmnop") {
		t.Fatalf("未完成的尾行被提前输出: %q", dst.String())
	}
	if !strings.Contains(dst.String(), "first line") {
		t.Fatalf("完整行没有被输出: %q", dst.String())
	}
	if err := w.(Flusher).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if strings.Contains(dst.String(), "abcdefghijklmnop") {
		t.Fatalf("Flush 后泄露: %q", dst.String())
	}
	if !strings.Contains(dst.String(), Marker) {
		t.Fatalf("Flush 后没有占位符: %q", dst.String())
	}
	// Flush 后继续写：仍然脱敏，并且可以再次 Flush。
	before := dst.Len()
	if _, err := w.Write([]byte("\ntoken=anothersecretvalue")); err != nil {
		t.Fatalf("Write after Flush: %v", err)
	}
	if err := w.(Flusher).Flush(); err != nil {
		t.Fatalf("Flush after Flush: %v", err)
	}
	if strings.Contains(dst.String()[before:], "anothersecretvalue") {
		t.Fatalf("Flush 之后的写入泄露: %q", dst.String())
	}
	// Close 等价于 Flush，且不关闭底层 writer。
	if err := w.(io.Closer).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.Write([]byte("token=afterclosevalue")); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}
	if err := w.(Flusher).Flush(); err != nil {
		t.Fatalf("Flush after Close: %v", err)
	}
	if strings.Contains(dst.String(), "afterclosevalue") {
		t.Fatalf("Close 之后的写入泄露: %q", dst.String())
	}
}

// TestAuditStream_ZeroLengthAndError 固定空写与底层错误语义。
func TestAuditStream_ZeroLengthAndError(t *testing.T) {
	var dst bytes.Buffer
	w := New().Writer(&dst)
	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Fatalf("Write(nil) = %d, %v; want 0, nil", n, err)
	}
	if dst.Len() != 0 {
		t.Fatalf("Write(nil) 产生了输出: %q", dst.String())
	}
	boom := errors.New("boom")
	failing := &auditFailingWriter{err: boom}
	fw := New().Writer(failing)
	if _, err := fw.Write([]byte("token=abcdefghijklmnop\n")); !errors.Is(err, boom) {
		t.Fatalf("Write error = %v, want boom", err)
	}
	if _, err := fw.Write([]byte("more\n")); !errors.Is(err, boom) {
		t.Fatalf("第二次 Write error = %v, want the sticky boom", err)
	}
	if err := fw.(Flusher).Flush(); !errors.Is(err, boom) {
		t.Fatalf("Flush error = %v, want the sticky boom", err)
	}
}

// nonzeroSpinReader 是"先返回 (0,nil)，再返回数据"的合法 reader。
type nonzeroSpinReader struct {
	empties int
	data    string
	offset  int
	seen    int
}

func (r *nonzeroSpinReader) Read(p []byte) (int, error) {
	r.seen++
	if r.empties > 0 {
		r.empties--
		return 0, nil
	}
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

// stuckReader 永远返回 (0,nil)：一个坏 reader 不应让我们的 Read 变成忙等。
type stuckReader struct{ calls int }

func (r *stuckReader) Read(p []byte) (int, error) {
	r.calls++
	return 0, nil
}

// TestAuditStream_ReaderZeroReadDoesNotSpin 固定 (0,nil) 语义：既不能吞掉数据，
// 也不能在坏 reader 上无限自旋（把 CPU 烧在输出路径上）。
func TestAuditStream_ReaderZeroReadDoesNotSpin(t *testing.T) {
	src := &nonzeroSpinReader{empties: 3, data: "hello " + fakeOpenAIKey + "\n"}
	const configuredEmpties = 3
	output, err := io.ReadAll(New().Reader(src))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if strings.Contains(string(output), fakeOpenAIKey) {
		t.Fatalf("Reader 泄露: %q", output)
	}
	if !strings.Contains(string(output), "hello") {
		t.Fatalf("Reader 丢了数据: %q", output)
	}
	if src.seen > configuredEmpties+4 {
		t.Fatalf("reader 被无谓重试了 %d 次", src.seen)
	}

	stuck := &stuckReader{}
	reader := New().Reader(stuck)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 16)
		_, _ = reader.Read(buffer)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Reader 在 (0,nil) 源上自旋：%d 次 Read 之后仍未返回", stuck.calls)
	}
	if stuck.calls > 8 {
		t.Fatalf("Reader 对 (0,nil) 源调用了 %d 次 src.Read，疑似自旋", stuck.calls)
	}
}

// TestAuditStream_ReaderEOFAndMixing 固定 EOF 语义与 Reader→Writer 混用。
func TestAuditStream_ReaderEOFAndMixing(t *testing.T) {
	input := "a\n" + fakeOpenAIKey + "\npassword=hunter2hunter2\n" + fakePEM
	reader := New().Reader(strings.NewReader(input))
	// 1 字节缓冲区读到底：最容易触发边界问题。
	var out bytes.Buffer
	buffer := make([]byte, 1)
	for {
		n, err := reader.Read(buffer)
		out.Write(buffer[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if n == 0 {
			t.Fatal("Read 返回 0,nil 但没有数据推进")
		}
	}
	for _, secret := range []string{fakeOpenAIKey, "hunter2hunter2", "MIIEowIBAAKCAQEA"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("Reader 泄露 %q: %q", secret, out.String())
		}
	}
	// Reader 的输出再过一遍 Writer：结果必须一致（幂等 + 无二次泄漏）。
	var again bytes.Buffer
	writer := New().Writer(&again)
	if _, err := writer.Write(out.Bytes()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := writer.(Flusher).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if again.String() != out.String() {
		t.Fatalf("Reader→Writer 不是幂等:\n R: %q\n W: %q", out.String(), again.String())
	}
}

// TestAuditContract_NilRedactor 固定 nil 接收者不 panic（零值/未配置的宿主路径）。
func TestAuditContract_NilRedactor(t *testing.T) {
	var r *Redactor
	if got := r.Redact("x"); got != "x" {
		t.Fatalf("Redact = %q", got)
	}
	if got := r.RedactBytes([]byte("x")); string(got) != "x" {
		t.Fatalf("RedactBytes = %q", got)
	}
	if got, err := r.RedactJSON([]byte(`{"a":1}`)); err != nil || string(got) != `{"a":1}` {
		t.Fatalf("RedactJSON = %q, %v", got, err)
	}
	if !r.Empty() {
		t.Fatal("nil Redactor 的 Empty() = false")
	}
	r.Add("whatever")                 // 不得 panic
	r.AddNamed("name", "whatever")    // 不得 panic
	if w := r.Writer(nil); w == nil { // nil 目标不得 panic，且返回可写 writer
		t.Fatal("Writer(nil) = nil")
	} else if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("Write to discard writer: %v", err)
	}
	if rd := r.Reader(nil); rd == nil {
		t.Fatal("Reader(nil) = nil")
	} else if _, err := io.ReadAll(rd); err != nil {
		t.Fatalf("ReadAll(nil reader): %v", err)
	}
}

// TestAuditContract_AddEdges 固定 Add/AddNamed 的边角输入与幂等登记。
func TestAuditContract_AddEdges(t *testing.T) {
	r := New("", "short", "12345678")
	if len(r.state.Load().secrets) != 1 {
		t.Fatalf("只应登记 8 字节那条，实际 %d 条", len(r.state.Load().secrets))
	}
	r.AddNamed("", "12345678")
	r.AddNamed("bad name!!", "another-secret-value")
	output := r.Redact("a another-secret-value b")
	if !strings.Contains(output, "[REDACTED:bad_name") {
		t.Fatalf("标签净化失败: %q", output)
	}
	if strings.Contains(output, "another-secret-value") {
		t.Fatalf("AddNamed 后泄露: %q", output)
	}
	// 重复登记同一值：保留首次标签。
	r.Add("another-secret-value")
	if got := r.Redact("another-secret-value"); !strings.Contains(got, "[REDACTED:bad_name") {
		t.Fatalf("重复登记覆盖了首次标签: %q", got)
	}
	// 超长标签截断到 maxNameLen。
	longName := strings.Repeat("n", 100)
	r.AddNamed(longName, "long-name-secret-value")
	if got := r.Redact("long-name-secret-value"); !strings.Contains(got, "[REDACTED:"+strings.Repeat("n", maxNameLen)+"]") {
		t.Fatalf("超长标签没有截断到 %d: %q", maxNameLen, got)
	}
}

// TestAuditContract_RedactBytesAliasing 固定 RedactBytes 的别名语义：未改动时返回
// 原切片（不复制），改动时返回新切片。
func TestAuditContract_RedactBytesAliasing(t *testing.T) {
	r := New()
	unchanged := []byte("no secrets here")
	if got := r.RedactBytes(unchanged); &got[0] != &unchanged[0] {
		t.Fatal("未改动时 RedactBytes 复制了输入（文档承诺返回原切片）")
	}
	changed := []byte("token=abcdefghijklmnop")
	got := r.RedactBytes(changed)
	if string(changed) != "token=abcdefghijklmnop" {
		t.Fatalf("RedactBytes 就地改了输入: %q", changed)
	}
	if strings.Contains(string(got), "abcdefghijklmnop") {
		t.Fatalf("RedactBytes 泄露: %q", got)
	}
}

// TestAuditContract_ConcurrentAddAndRedact 是 8 读者 + 50 次 Add 的竞态压测
// （配合 -race 运行）；同时验证"Add 立即生效"不会读到半更新的快照。
func TestAuditContract_ConcurrentAddAndRedact(t *testing.T) {
	r := New()
	const readers = 8
	const adds = 50
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for reader := 0; reader < readers; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				output := r.Redact("line with token=abcdefghijklmnop and sk-" + fakeOpenAIKey[3:])
				if strings.Contains(output, "abcdefghijklmnop") {
					t.Errorf("并发 Redact 泄露: %q", output)
					return
				}
				if r.Empty() && len(r.state.Load().secrets) > 0 {
					t.Errorf("Empty 与快照不一致")
					return
				}
			}
		}()
	}
	for add := 0; add < adds; add++ {
		r.Add("known-" + strings.Repeat("x", add+8))
		r.AddNamed("n", "named-known-"+strings.Repeat("y", add+8))
	}
	close(stop)
	wg.Wait()
}

// TestAuditContract_Performance 给两条可断言的性能边界：1 MiB 无害文本必须远低于
// 100ms；1 MiB 的"每行都命中"病态文本也不得慢到不可用（>2s 视为回归）。
func TestAuditContract_Performance(t *testing.T) {
	if raceEnabled {
		t.Skip("竞态构建下正则慢数倍，性能断言只在非 race 构建运行")
	}
	harmless := strings.Repeat("the quick brown fox jumps over the lazy dog\n", (1<<20)/44)
	start := time.Now()
	out := New().Redact(harmless)
	elapsed := time.Since(start)
	if out != harmless {
		t.Fatal("无害文本被改写")
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("1MiB 无害文本耗时 %s，超过 100ms 预算", elapsed)
	}
	pathological := strings.Repeat("token=abcdefghijklmnop\n", (1<<20)/24)
	pathologicalStart := time.Now()
	_ = New().Redact(pathological)
	pathologicalElapsed := time.Since(pathologicalStart)
	if pathologicalElapsed > 2*time.Second {
		t.Fatalf("1MiB 病态文本耗时 %s，超过 2s", pathologicalElapsed)
	}
	t.Logf("perf: 1MiB harmless=%s pathological=%s", elapsed, pathologicalElapsed)
}

// TestAuditStream_GiantLineNoDataLoss 固定"没有换行的超长单行不丢数据"（它不是秘密，
// 但被强制冲刷的路径不能悄悄吞掉）。
func TestAuditStream_GiantLineNoDataLoss(t *testing.T) {
	line := strings.Repeat("a", 100<<10)
	output := auditStream(t, New(), line[:50000], line[50000:])
	if len(output) != len(line) {
		t.Fatalf("超长单行丢数据：入 %d 字节，出 %d 字节", len(line), len(output))
	}
}

// TestAuditStream_PrivateKeyBlockBeyondBuffer 覆盖"私钥块大于 MaxBuffered"的分支：
// 块内正文被丢弃（suppress 路径），但 END 之后的正常输出必须恢复，且块内不得泄露。
func TestAuditStream_PrivateKeyBlockBeyondBuffer(t *testing.T) {
	body := strings.Repeat("MIIESECRETSECRETSECRETSECRETSE", 4000) // ~112 KiB
	input := "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----\n" + "after block\n"
	var dst bytes.Buffer
	writer := New().Writer(&dst)
	for offset := 0; offset < len(input); offset += 4096 {
		end := min(offset+4096, len(input))
		if _, err := writer.Write([]byte(input[offset:end])); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := writer.(Flusher).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	output := dst.String()
	if strings.Contains(output, "MIIESECRET") {
		t.Fatalf("超长私钥块泄露了正文（%d 字节输出）", len(output))
	}
	if !strings.Contains(output, "after block") {
		t.Fatalf("END 之后的正常输出被吞掉: %q", output)
	}
}

// TestAuditStream_ServerStyleChunks 用"假装是网络分片"的随机切分再验证一遍：每 7/13/
// 64 字节、以及把凭据放在行首/行尾/中间的各种位置都不泄露。
func TestAuditStream_ServerStyleChunks(t *testing.T) {
	for name, input := range auditCredentials() {
		secret := auditSecretOf(name)
		if secret == "" {
			continue
		}
		for _, chunk := range []int{7, 13, 64} {
			chunks := make([]string, 0, len(input)/chunk+1)
			for offset := 0; offset < len(input); offset += chunk {
				end := min(offset+chunk, len(input))
				chunks = append(chunks, input[offset:end])
			}
			output := auditStream(t, New(), chunks...)
			assertNoFragment(t, output, secret)
		}
	}
}
