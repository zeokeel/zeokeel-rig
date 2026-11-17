package secrets

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readTestdata 读取 secrets/testdata 下的样例文件。
func readTestdata(t *testing.T, name string) (string, error) {
	t.Helper()
	path := filepath.Join("testdata", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// assertNoFragment 断言 output 里不出现 secret 的任何前缀片段（长度 ≥3），这是
// 流式脱敏最关键的性质：跨 chunk 的 token 不能被切一半泄露出去。
func assertNoFragment(t *testing.T, output, secret string) {
	t.Helper()
	for n := 3; n <= len(secret); n++ {
		if fragment := secret[:n]; strings.Contains(output, fragment) {
			t.Fatalf("输出泄露了秘密的前 %d 字节 %q\n输出: %q", n, fragment, output)
		}
	}
}

// writeChunks 按给定切点把 text 分块写入，Flush 后返回收集到的输出。
func writeChunks(t *testing.T, r *Redactor, text string, cuts ...int) string {
	t.Helper()
	var dst bytes.Buffer
	writer := r.Writer(&dst)
	start := 0
	for _, cut := range cuts {
		if _, err := writer.Write([]byte(text[start:cut])); err != nil {
			t.Fatalf("Write: %v", err)
		}
		start = cut
	}
	if _, err := writer.Write([]byte(text[start:])); err != nil {
		t.Fatalf("Write: %v", err)
	}
	flusher, ok := writer.(Flusher)
	if !ok {
		t.Fatal("Writer 返回值必须实现 Flusher，否则调用方无法输出被扣留的尾巴")
	}
	if err := flusher.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return dst.String()
}

// TestWriterSplitTokenDoesNotLeak 是被任务点名的那条关键用例：一条 token 被切成三段，
// 输出里既不能出现任何前缀片段，也必须正常完成替换。
func TestWriterSplitTokenDoesNotLeak(t *testing.T) {
	token := "sk-" + strings.Repeat("Q", 30)
	filler := strings.Repeat("no secrets on this line\n", 500) // 约 24 KiB，确保前面的内容已经流出
	text := filler + "Authorization: Bearer " + token + "\nend\n"
	tokenStart := strings.Index(text, token)

	output := writeChunks(t, New(), text, tokenStart+5, tokenStart+20)

	want := filler + "Authorization: Bearer " + Marker + "\nend\n"
	if output != want {
		t.Fatalf("流式输出 =\n %q\nwant\n %q", output, want)
	}
	assertNoFragment(t, output, token)
}

// TestWriterStreamsBeforeFlush 断言 Writer 不是"全靠 Flush"：足够长的输入在写入过程中
// 就应该出现在底层 writer 上。
func TestWriterStreamsBeforeFlush(t *testing.T) {
	var dst bytes.Buffer
	writer := New().Writer(&dst)
	if _, err := writer.Write([]byte(strings.Repeat("harmless line\n", 1000))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if dst.Len() == 0 {
		t.Fatal("写入 14KiB 后底层 writer 仍为空：完全缓冲不可接受")
	}
}

// TestWriterSplitPatterns 对每类模式各做一次"跨 chunk 切断"检查。
func TestWriterSplitPatterns(t *testing.T) {
	tests := []struct {
		name    string
		known   string
		prefix  string
		secret  string
		suffix  string
		replace string
	}{
		{"openai key", "", "key=", fakeOpenAIKey, "\n", "key=" + Marker + "\n"},
		{"jwt behind bearer", "", "Authorization: Bearer ", fakeJWT, "\n", "Authorization: Bearer " + Marker + "\n"},
		{"aws secret", "", "aws_secret_access_key = ", fakeAWSSecret, "\n", "aws_secret_access_key = " + Marker + "\n"},
		{"known value", "hunter2hunter2", "password=", "hunter2hunter2", "\n", "password=" + Marker + "\n"},
		{"github token", "", "token ", fakeGitHubPAT, "\n", "token " + Marker + "\n"},
		{"pem block", "", "before\n", fakePEM, "after\n", "before\n" + Marker + "\nafter\n"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			r := New(testCase.known)
			text := testCase.prefix + testCase.secret + testCase.suffix
			start := len(testCase.prefix)

			// 在秘密内部切三刀（至少一段完全落在秘密里）。
			cuts := []int{start + 1, start + len(testCase.secret)/2, start + len(testCase.secret) - 1}
			output := writeChunks(t, r, text, cuts...)

			if output != testCase.replace {
				t.Fatalf("流式输出 = %q, want %q", output, testCase.replace)
			}
			if testCase.secret != fakePEM { // PEM 是整块替换，做片段检查意义有限
				assertNoFragment(t, output, testCase.secret)
			}
			if strings.Contains(output, "PRIVATE KEY") {
				t.Fatalf("私钥块头泄露: %q", output)
			}
		})
	}
}

// TestWriterBufferBound 断言未决缓冲不超过 64 KiB。
func TestWriterBufferBound(t *testing.T) {
	var dst bytes.Buffer
	writer := New().Writer(&dst)
	bounded, ok := writer.(*redactingWriter)
	if !ok {
		t.Fatalf("Writer 返回值类型 = %T, want *redactingWriter", writer)
	}
	// 单行 300 KiB：没有任何换行可以借来判断边界，只能靠硬上限。
	blob := bytes.Repeat([]byte("x"), 300<<10)
	if _, err := writer.Write(blob); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if pending := bounded.pending(); pending > maxBuffered {
		t.Fatalf("未决缓冲 %d 字节，超过上限 %d", pending, maxBuffered)
	}
	if err := writer.(Flusher).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := dst.Len(); got != len(blob) {
		t.Fatalf("输出 %d 字节, want %d", got, len(blob))
	}
}

// TestWriterPassthrough 断言没有秘密时输出与输入逐字节一致（不能改坏正常日志）。
func TestWriterPassthrough(t *testing.T) {
	text := "line one\nline two with id=123e4567-e89b-12d3-a456-426614174000\nno secrets\n"
	if output := writeChunks(t, New(), text, 5, 20, 61); output != text {
		t.Fatalf("passthrough 输出 = %q, want %q", output, text)
	}
}

// TestWriterErrorPropagates 断言底层 writer 的错误会被返回，而不是静默丢弃。
func TestWriterErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	writer := New().Writer(failingWriter{err: boom})
	_, err := writer.Write([]byte("hello\n"))
	if err == nil {
		t.Fatal("Write 应返回底层写入错误")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

// failingWriter 总是失败。
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// TestWriterEmpty 断言没有写入时 Flush 输出为空。
func TestWriterEmpty(t *testing.T) {
	var dst bytes.Buffer
	writer := New().Writer(&dst)
	if err := writer.(Flusher).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if dst.Len() != 0 {
		t.Fatalf("空写入产生了输出: %q", dst.String())
	}
}

// TestWriterNilDestination 断言 nil 目标不会 panic（当作丢弃）。
func TestWriterNilDestination(t *testing.T) {
	writer := New().Writer(nil)
	if _, err := writer.Write([]byte("k=" + fakeOpenAIKey + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reader
// ---------------------------------------------------------------------------

// TestReaderSplitToken 断言 Reader 与 Writer 共享同一套流式语义。
func TestReaderSplitToken(t *testing.T) {
	token := "sk-" + strings.Repeat("R", 30)
	filler := strings.Repeat("another harmless log line\n", 400)
	text := filler + "key=" + token + "\nend\n"

	got, err := io.ReadAll(New().Reader(strings.NewReader(text)))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := filler + "key=" + Marker + "\nend\n"
	if string(got) != want {
		t.Fatalf("Reader 输出 =\n %q\nwant\n %q", got, want)
	}
	assertNoFragment(t, string(got), token)
}

// TestReaderPassthrough 断言无秘密时逐字节一致。
func TestReaderPassthrough(t *testing.T) {
	text := "alpha\nbeta\ngamma\n"
	got, err := io.ReadAll(New().Reader(strings.NewReader(text)))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != text {
		t.Fatalf("Reader 输出 = %q, want %q", got, text)
	}
}

// TestReaderEmpty 断言空输入得到空输出与 io.EOF。
func TestReaderEmpty(t *testing.T) {
	got, err := io.ReadAll(New().Reader(strings.NewReader("")))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Reader 输出 = %q", got)
	}
}

// TestReaderLargeStream 断言大输入（分多次 Read）也能逐字节保真。
func TestReaderLargeStream(t *testing.T) {
	line := "2026-11-17T00:00:00Z ok id=123e4567-e89b-12d3-a456-426614174000\n"
	text := strings.Repeat(line, 4000) // 约 240 KiB
	got, err := io.ReadAll(New().Reader(strings.NewReader(text)))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != text {
		t.Fatalf("大输入被改动：got %d 字节, want %d", len(got), len(text))
	}
}

// TestReaderErrorPropagates 断言底层 reader 的错误会被透传。
func TestReaderErrorPropagates(t *testing.T) {
	boom := errors.New("read boom")
	reader := New().Reader(io.MultiReader(strings.NewReader("abc\n"), errReader{boom}))
	_, err := io.ReadAll(reader)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

// errReader 总是返回错误。
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// ---------------------------------------------------------------------------
// 边界判定（包内单元测试）
// ---------------------------------------------------------------------------

// TestNextEmitKeepsUnfinishedLine 断言完整行可以立刻输出、未完成行被扣留。
func TestNextEmitKeepsUnfinishedLine(t *testing.T) {
	buf := []byte("line one\nline two\npartial")
	emit, suppress := nextEmit(buf)
	if suppress {
		t.Fatal("不该进入私钥抑制态")
	}
	if emit != len("line one\nline two\n") {
		t.Fatalf("emit = %d, want %d", emit, len("line one\nline two\n"))
	}

	if emit, _ := nextEmit([]byte("no newline at all")); emit != 0 {
		t.Fatalf("没有换行时 emit = %d, want 0", emit)
	}
}

// TestNextEmitHoldsPrivateKeyBlock 断言未闭合的私钥块整块扣留，且只输出块头之前
// 的内容。
func TestNextEmitHoldsPrivateKeyBlock(t *testing.T) {
	buf := []byte("before\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\n")
	if emit, _ := nextEmit(buf); emit != len("before\n") {
		t.Fatalf("emit = %d, want %d（块头之前的内容）", emit, len("before\n"))
	}
	// 块闭合之后可以整块交出（交给模式一次性替换）。
	closed := []byte("before\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----\n")
	if emit, _ := nextEmit(closed); emit != len(closed) {
		t.Fatalf("emit = %d, want %d", emit, len(closed))
	}
	// 证书不是私钥，不该被扣留。
	cert := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n")
	if emit, _ := nextEmit(cert); emit != len(cert) {
		t.Fatalf("证书被误扣留: emit = %d", emit)
	}
}

// TestNextEmitBacksOffToDelimiter 断言超长单行强制冲刷时边界会回退到分隔符之后，
// 不会把 token 从中间切断。
func TestNextEmitBacksOffToDelimiter(t *testing.T) {
	token := "sk-" + strings.Repeat("T", 40)
	// 构造一条超过 maxBuffered 的单行：让强制边界恰好落在 token 中间。
	const prefixLen = 2000
	const tokenStart = prefixLen + 1
	fillLen := 64488 // 使 len(buf)-(maxBuffered-forcedBackoff) ∈ (tokenStart, tokenStart+len(token))
	buf := []byte(strings.Repeat("x", prefixLen) + " " + token + " " + strings.Repeat("y", fillLen))
	if boundary := len(buf) - (maxBuffered - forcedBackoff); boundary <= tokenStart || boundary >= tokenStart+len(token) {
		t.Fatalf("测试构造有误：边界 %d 不在 token [%d,%d) 内", boundary, tokenStart, tokenStart+len(token))
	}

	emit, _ := nextEmit(buf)
	if emit != tokenStart {
		t.Fatalf("emit = %d, want %d（应回退到 token 之前的分隔符之后）", emit, tokenStart)
	}
	if pending := len(buf) - emit; pending > maxBuffered {
		t.Fatalf("未决缓冲 %d 超过上限 %d", pending, maxBuffered)
	}
}

// TestWriterGiantSingleLine 是流式层的端到端检查：一条 200 KiB、没有换行的输出
// 里放一条 token，写入过程中未决缓冲不超过上限，最终 token 也不得泄露。
func TestWriterGiantSingleLine(t *testing.T) {
	token := "sk-" + strings.Repeat("Z", 36)
	text := strings.Repeat("x", 90<<10) + " " + token + " " + strings.Repeat("y", 110<<10)

	var dst bytes.Buffer
	writer := New().Writer(&dst)
	bounded := writer.(*redactingWriter)
	for start := 0; start < len(text); start += 8 << 10 {
		end := start + 8<<10
		if end > len(text) {
			end = len(text)
		}
		if _, err := writer.Write([]byte(text[start:end])); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if pending := bounded.pending(); pending > MaxBuffered {
			t.Fatalf("写入 %d 字节后未决缓冲 %d 超过上限 %d", end, pending, MaxBuffered)
		}
	}
	if err := writer.(Flusher).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	output := dst.String()
	if strings.Contains(output, token) {
		t.Fatal("超长单行里的 token 泄露了")
	}
	if !strings.Contains(output, Marker) {
		t.Fatalf("没有产生占位符（输出长度 %d）", len(output))
	}
	assertNoFragment(t, output, token)
}
