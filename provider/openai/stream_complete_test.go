package openai

// Tests for Config.RequireCompleteStream: a stream that closes before either a
// finish_reason or the [DONE] terminator is suspicious (the answer may be
// truncated), while the default keeps the historical compatibility behavior.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// newCompleteProvider returns a provider with the completeness option and fast
// retry settings.
func newCompleteProvider(t *testing.T, handler http.HandlerFunc, require bool) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := New(Config{
		BaseURL:               server.URL,
		APIKey:                "test-key",
		DefaultModel:          "test-model",
		HTTPClient:            server.Client(),
		RetryBackoff:          time.Millisecond,
		RequireCompleteStream: require,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider
}

// writeSSEWithoutDone writes SSE data events and closes the response without the
// [DONE] terminator.
func writeSSEWithoutDone(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	for _, event := range events {
		fmt.Fprintf(w, "data: %s\n\n", event)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// drainCompleteStream reads a stream to its terminal error.
func drainCompleteStream(t *testing.T, stream rig.Stream) error {
	t.Helper()
	for {
		if _, err := stream.Recv(); err != nil {
			return err
		}
	}
}

func TestRequireCompleteStreamRejectsEarlyEOF(t *testing.T) {
	provider := newCompleteProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSEWithoutDone(w, `{"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`)
	}, true)
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	err = drainCompleteStream(t, stream)
	if !errors.Is(err, ErrIncompleteStream) {
		t.Fatalf("terminal error = %v, want ErrIncompleteStream", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("terminal error = %v, want io.ErrUnexpectedEOF via ErrIncompleteStream", err)
	}
}

func TestRequireCompleteStreamAllowsFinishReason(t *testing.T) {
	provider := newCompleteProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSEWithoutDone(w,
			`{"choices":[{"delta":{"content":"done"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	}, true)
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if err := drainCompleteStream(t, stream); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal error = %v, want io.EOF", err)
	}
}

func TestRequireCompleteStreamAllowsDoneTerminator(t *testing.T) {
	provider := newCompleteProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"done"},"finish_reason":null}]}`)
	}, true)
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if err := drainCompleteStream(t, stream); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal error = %v, want io.EOF", err)
	}
}

func TestRequireCompleteStreamDeliversPartialContentBeforeError(t *testing.T) {
	provider := newCompleteProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSEWithoutDone(w, `{"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`)
	}, true)
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunk, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if chunk.Delta != "partial" {
		t.Fatalf("first chunk delta = %q, want partial", chunk.Delta)
	}
	if _, err := stream.Recv(); !errors.Is(err, ErrIncompleteStream) {
		t.Fatalf("terminal error = %v, want ErrIncompleteStream after partial content", err)
	}
}

func TestRequireCompleteStreamDisabledKeepsCompatibility(t *testing.T) {
	provider := newCompleteProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSEWithoutDone(w, `{"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`)
	}, false)
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if err := drainCompleteStream(t, stream); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal error = %v, want io.EOF (compatibility default)", err)
	}
}
