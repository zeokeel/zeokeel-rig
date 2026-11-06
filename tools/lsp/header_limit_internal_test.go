package lsp

import (
	"bufio"
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
