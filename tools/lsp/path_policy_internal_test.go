package lsp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// TestLSPToolsEnforcePathPolicy: the tools read model-provided paths before the
// language server sees them, so a host-installed path policy must deny the read
// (a hostile model could otherwise use lsp_* to read arbitrary files and use
// diagnostics as an oracle).
func TestLSPToolsEnforcePathPolicy(t *testing.T) {
	f := &lspTool{
		name:       ToolHover,
		pathPolicy: func(path string) error { return errors.New("outside the sandbox roots") },
	}
	_, _, failure, err := openDocument(context.Background(), f, filepath.Join(t.TempDir(), "secret.txt"))
	if err != nil {
		t.Fatalf("openDocument returned a transport error: %v", err)
	}
	if failure == nil || !failure.IsError || !strings.Contains(failure.Content, "blocked by policy") {
		t.Fatalf("path policy did not block the read: failure=%+v", failure)
	}

	// The exported constructors accept the option so tools/full and hosts can
	// install the same policy.
	for _, tool := range []rig.Tool{
		NewHoverTool(nil, nil, WithPathPolicy(func(string) error { return errors.New("denied-by-test") })),
		NewDefinitionTool(nil, nil, WithPathPolicy(func(string) error { return errors.New("denied-by-test") })),
		NewDiagnosticsTool(nil, nil, WithPathPolicy(func(string) error { return errors.New("denied-by-test") })),
		NewSymbolsTool(nil, nil, WithPathPolicy(func(string) error { return errors.New("denied-by-test") })),
	} {
		if tool == nil {
			t.Fatal("constructor returned a nil tool")
		}
	}
	if legacy := LegacyTool(nil, nil, WithPathPolicy(func(string) error { return errors.New("denied-by-test") })); legacy == nil {
		t.Fatal("LegacyTool returned nil")
	}
}
