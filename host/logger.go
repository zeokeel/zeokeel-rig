package host

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// LogFileName is the log file inside a product's data directory.
const LogFileName = "rig.log"

// Logger is the process host's v1.Logger: one line-oriented sink writing to
// stderr and, when a data directory is available, to <dataDir>/logs/rig.log.
//
// File logging is best effort: a product must start even when its data
// directory is unwritable. The logger's file handle is shared, so Printf is
// safe from any goroutine.
type Logger struct {
	logger *log.Logger
	file   *os.File
	path   string

	closeOnce sync.Once
	closeErr  error
}

// compile-time check: the host provides this as v1.Logger.
var _ v1.Logger = (*Logger)(nil)

// NewLogger opens the log file under dataDir (creating <dataDir>/logs) when
// possible and returns a logger writing to stderr and that file. An empty or
// unwritable data directory yields a stderr-only logger; the failure is
// reported on stderr, never returned, because a logger must not block startup.
func NewLogger(dataDir string) *Logger {
	writers := []io.Writer{os.Stderr}
	var file *os.File
	var path string
	if dataDir = strings.TrimSpace(dataDir); dataDir != "" {
		logPath := filepath.Join(dataDir, "logs", LogFileName)
		if opened, err := openLogFile(logPath); err != nil {
			fmt.Fprintf(os.Stderr, "%s: file logging unavailable (%v); logging to stderr\n", appName, err)
		} else {
			file, path = opened, logPath
			writers = append(writers, file)
		}
	}
	return &Logger{
		logger: log.New(io.MultiWriter(writers...), "", log.LstdFlags),
		file:   file,
		path:   path,
	}
}

func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return file, nil
}

// Path returns the log file path, or "" when only stderr is written.
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Printf implements v1.Logger.
func (l *Logger) Printf(format string, args ...any) {
	if l == nil || l.logger == nil {
		return
	}
	l.logger.Printf(format, args...)
}

// Close flushes and closes the log file. It is idempotent.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.file == nil {
			return
		}
		if err := l.file.Sync(); err != nil {
			l.closeErr = err
		}
		if err := l.file.Close(); err != nil && l.closeErr == nil {
			l.closeErr = err
		}
	})
	return l.closeErr
}
