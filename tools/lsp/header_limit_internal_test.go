package lsp

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// TestReadMessageRejectsOversizedHeader: the header is read before
// Content-Length is known, so it must be bounded (v0.9.1 security fix; a 300 MB
// header previously grew the heap to ~859 MiB).
func TestReadMessageRejectsOversizedHeader(t *testing.T) {
	// One endless header line: ReadString would allocate the whole line without
	// the per-header cap.
	r := bufio.NewReaderSize(strings.NewReader("X-Huge: "+strings.Repeat("A", 2*maxHeaderBytes)+"\r\n"), 64)
	if _, err := readMessage(r); err == nil || !strings.Contains(err.Error(), "header exceeds") {
		t.Fatalf("readMessage(oversized header) error = %v, want a header-size error", err)
	}
}

// TestReadMessageRejectsUnterminatedLongHeader is the v0.9.4 regression for the
// residual v0.9.1 bug: ReadString allocated a whole unterminated header line
// before the 64 KiB cap was checked. The bounded reader must stop after roughly
// maxHeaderBytes instead of consuming the endless stream.
func TestReadMessageRejectsUnterminatedLongHeader(t *testing.T) {
	const streamBytes = 8 << 20
	source := &endlessReader{remaining: streamBytes}
	reader := bufio.NewReaderSize(source, 64)
	_, err := readMessage(reader)
	if err == nil || !strings.Contains(err.Error(), "header exceeds") {
		t.Fatalf("readMessage(unterminated header) error = %v, want a header-size error", err)
	}
	if source.read > maxHeaderBytes+64*1024 {
		t.Fatalf("reader consumed %d bytes for a %d-byte header cap", source.read, maxHeaderBytes)
	}
}

// endlessReader streams 'A' bytes up to remaining without ever emitting a
// newline, modelling a hostile server that keeps one header line open.
type endlessReader struct {
	remaining int
	read      int
}

func (r *endlessReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'A'
	}
	r.remaining -= n
	r.read += n
	return n, nil
}

// TestReadHeaderParsesAndEnds verifies the bounded parser still accepts a normal
// header block and stops at the blank line.
func TestReadHeaderParsesAndEnds(t *testing.T) {
	raw := "Content-Length: 2\r\nContent-Type: application/json\r\n\r\n{}"
	r := bufio.NewReader(strings.NewReader(raw))
	header, err := readHeader(r)
	if err != nil {
		t.Fatalf("readHeader: %v", err)
	}
	if got := header["content-length"]; got != "2" {
		t.Fatalf("content-length = %q, want 2", got)
	}
	if got := header["content-type"]; got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	msg, err := readMessage(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	_ = msg
}
