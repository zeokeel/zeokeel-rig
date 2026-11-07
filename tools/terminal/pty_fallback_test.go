package terminal

// Internal tests for the PTY -> pipe fallback path; they need the ptyStart hook
// and therefore live in the terminal package.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"time"
)

func TestPTYFallbackWarningAndPipeSession(t *testing.T) {
	original := ptyStart
	ptyStart = func(_ *sandbox.Policy, _, _ string, _ []string, _, _ int) (*os.File, *exec.Cmd, error) {
		return nil, nil, errors.New("injected PTY failure")
	}
	t.Cleanup(func() { ptyStart = original })

	manager := NewManager(WithTransport(TransportPTY))
	t.Cleanup(func() { _ = manager.CloseAll() })

	firstID, firstTransport, warning, err := manager.open("", "")
	if err != nil {
		t.Fatalf("open with PTY fallback: %v", err)
	}
	if firstTransport != TransportPipe {
		t.Fatalf("fallback transport = %s, want pipe", firstTransport)
	}
	if !strings.Contains(warning, "injected PTY failure") {
		t.Fatalf("fallback warning = %q", warning)
	}
	first, err := manager.lookup(firstID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	sent, timedOut, sendErr := first.send(context.Background(), "echo fallback-ok", 3*time.Second)
	if sendErr != nil || timedOut {
		t.Fatalf("send on fallback session: %q, %v", sent, sendErr)
	}
	if !strings.Contains(sent, "fallback-ok") {
		t.Fatalf("fallback pipe send = %q", sent)
	}

	// The warning is reported once per manager; later sessions are silent.
	_, secondTransport, secondWarning, err := manager.open("", "")
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if secondTransport != TransportPipe {
		t.Fatalf("second transport = %s, want pipe", secondTransport)
	}
	if secondWarning != "" {
		t.Fatalf("second warning = %q, want empty (warned once)", secondWarning)
	}
}
