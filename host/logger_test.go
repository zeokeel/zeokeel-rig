package host

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoggerWritesToFile(t *testing.T) {
	dir := t.TempDir()
	logger := NewLogger(dir)
	wantPath := filepath.Join(dir, "logs", LogFileName)
	if got := logger.Path(); got != wantPath {
		t.Fatalf("Path() = %q, want %q", got, wantPath)
	}
	logger.Printf("hello %s %d", "rig", 42)
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "hello rig 42") {
		t.Fatalf("log file = %q, want the formatted line", data)
	}
}

func TestLoggerWithoutDataDir(t *testing.T) {
	logger := NewLogger("")
	if got := logger.Path(); got != "" {
		t.Fatalf("Path() = %q, want empty for a stderr-only logger", got)
	}
	logger.Printf("stderr only")
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var nilLogger *Logger
	nilLogger.Printf("no-op on nil")
	if err := nilLogger.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
	if nilLogger.Path() != "" {
		t.Fatal("nil logger Path() is not empty")
	}
}

func TestLoggerConcurrentPrintf(t *testing.T) {
	dir := t.TempDir()
	logger := NewLogger(dir)
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for round := 0; round < 20; round++ {
				logger.Printf("worker %d round %d", worker, round)
			}
		}(worker)
	}
	group.Wait()
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(logger.Path())
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 8*20 {
		t.Fatalf("log lines = %d, want %d", lines, 8*20)
	}
}
