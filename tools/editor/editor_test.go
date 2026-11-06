package editor_test

// Black-box tests for the str_replace_editor tool. Every test builds a fresh
// sandbox policy over a temporary root, so the tests cover the policy wiring
// (roots, read-only, denied globs, output cap) rather than a private path
// implementation.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/editor"
)

// fixture is one editor tool bound to a temporary root.
type fixture struct {
	root string
	tool v1.Tool
}

// newFixture builds a policy over a new temporary root. policyOpts are applied
// after WithRoots, so they can add read-only, denied globs or an output cap.
func newFixture(t *testing.T, policyOpts ...sandbox.Option) *fixture {
	t.Helper()
	root := t.TempDir()
	options := append([]sandbox.Option{sandbox.WithRoots(root)}, policyOpts...)
	return &fixture{root: root, tool: editor.New(sandbox.New(options...))}
}

// invoke runs one tool call and fails the test on an uncontained Go error
// (invalid argument JSON, context cancellation).
func (f *fixture) invoke(t *testing.T, arguments string) v1.ToolResult {
	t.Helper()
	result, err := f.tool.Invoke(context.Background(), v1.ToolCall{Name: editor.Name, Arguments: arguments})
	if err != nil {
		t.Fatalf("Invoke(%s): %v", arguments, err)
	}
	return result
}

// write creates one file under the root.
func (f *fixture) write(t *testing.T, relative, content string) string {
	t.Helper()
	absolute := filepath.Join(f.root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relative, err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
	return absolute
}

// read returns one file's content under the root.
func (f *fixture) read(t *testing.T, relative string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(data)
}

// requireContains asserts a successful result containing want.
func requireContains(t *testing.T, result v1.ToolResult, want string) {
	t.Helper()
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content)
	}
	if !strings.Contains(result.Content, want) {
		t.Fatalf("result %q does not contain %q", result.Content, want)
	}
}

// requireError asserts IsError and that the content mentions every want.
func requireError(t *testing.T, result v1.ToolResult, want ...string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("expected a tool error, got %q", result.Content)
	}
	for _, fragment := range want {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("error %q does not contain %q", result.Content, fragment)
		}
	}
}

// TestView covers line numbering, ranges and error handling.
func TestView(t *testing.T) {
	fixture := newFixture(t)
	fixture.write(t, "notes.txt", "alpha\nbeta\ngamma\n")

	all := fixture.invoke(t, `{"command":"view","path":"notes.txt"}`)
	for _, want := range []string{"1\talpha", "2\tbeta", "3\tgamma"} {
		requireContains(t, all, want)
	}

	rangeOnly := fixture.invoke(t, `{"command":"view","path":"notes.txt","view_range":[2,2]}`)
	requireContains(t, rangeOnly, "2\tbeta")
	if strings.Contains(rangeOnly.Content, "1\talpha") || strings.Contains(rangeOnly.Content, "3\tgamma") {
		t.Fatalf("view_range [2,2] leaked other lines: %q", rangeOnly.Content)
	}

	toEnd := fixture.invoke(t, `{"command":"view","path":"notes.txt","view_range":[2,-1]}`)
	requireContains(t, toEnd, "2\tbeta")
	requireContains(t, toEnd, "3\tgamma")

	requireError(t, fixture.invoke(t, `{"command":"view","path":"notes.txt","view_range":[3,2]}`), "end must be greater")
	requireError(t, fixture.invoke(t, `{"command":"view","path":"notes.txt","view_range":[9,10]}`), "start must be between")
	requireError(t, fixture.invoke(t, `{"command":"view","path":"notes.txt","view_range":[1]}`), "expected [start, end]")
	requireError(t, fixture.invoke(t, `{"command":"view","path":"missing.txt"}`), "does not exist")

	if err := os.MkdirAll(filepath.Join(fixture.root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	requireError(t, fixture.invoke(t, `{"command":"view","path":"dir"}`), "directory")
}

// TestCreate covers creating files, parent directories and the exists conflict.
func TestCreate(t *testing.T) {
	fixture := newFixture(t)
	created := fixture.invoke(t, `{"command":"create","path":"sub/new.txt","file_text":"hello"}`)
	requireContains(t, created, "created sub/new.txt")
	if got := fixture.read(t, "sub/new.txt"); got != "hello" {
		t.Fatalf("file content = %q, want hello", got)
	}

	requireError(t, fixture.invoke(t, `{"command":"create","path":"sub/new.txt","file_text":"again"}`),
		"already exists", "str_replace")
	requireError(t, fixture.invoke(t, `{"command":"create","path":"other.txt"}`), "file_text is required")
}

// TestStrReplace covers zero/unique/duplicate matching, replace_all and the
// one-line-context diff summary.
func TestStrReplace(t *testing.T) {
	fixture := newFixture(t)
	fixture.write(t, "dup.txt", "one two one two")

	requireError(t, fixture.invoke(t, `{"command":"str_replace","path":"dup.txt","old_str":"one","new_str":"1"}`),
		"occurs 2 times", "replace_all")
	requireError(t, fixture.invoke(t, `{"command":"str_replace","path":"dup.txt","old_str":"absent","new_str":"x"}`),
		"did not appear")
	requireError(t, fixture.invoke(t, `{"command":"str_replace","path":"dup.txt","new_str":"x"}`), "old_str is required")

	replaced := fixture.invoke(t, `{"command":"str_replace","path":"dup.txt","old_str":"one","new_str":"1","replace_all":true}`)
	requireContains(t, replaced, "replaced 2 occurrence(s)")
	requireContains(t, replaced, "1\t")
	if got := fixture.read(t, "dup.txt"); got != "1 two 1 two" {
		t.Fatalf("file content = %q, want %q", got, "1 two 1 two")
	}

	fixture.write(t, "unique.txt", "hello world")
	unique := fixture.invoke(t, `{"command":"str_replace","path":"unique.txt","old_str":"world","new_str":"there"}`)
	requireContains(t, unique, "replaced 1 occurrence(s)")
	requireContains(t, unique, "first replacement at line 1")
	requireContains(t, unique, "1\thello there")
	if got := fixture.read(t, "unique.txt"); got != "hello there" {
		t.Fatalf("file content = %q, want %q", got, "hello there")
	}

	// A multi-line replacement reports the surrounding context lines.
	fixture.write(t, "multi.txt", "a\nb\nc")
	multiline := fixture.invoke(t, `{"command":"str_replace","path":"multi.txt","old_str":"b","new_str":"b1\nb2"}`)
	for _, want := range []string{"1\ta", "2\tb1", "3\tb2", "4\tc"} {
		requireContains(t, multiline, want)
	}
}

// TestInsert covers the start-of-file, middle, end and out-of-range cases.
func TestInsert(t *testing.T) {
	fixture := newFixture(t)
	fixture.write(t, "list.txt", "a\nb")

	atStart := fixture.invoke(t, `{"command":"insert","path":"list.txt","insert_line":0,"new_str":"start"}`)
	requireContains(t, atStart, "at the beginning")
	requireContains(t, atStart, "new lines 1-1")
	if got := fixture.read(t, "list.txt"); got != "start\na\nb" {
		t.Fatalf("file after insert at 0 = %q", got)
	}

	afterFirst := fixture.invoke(t, `{"command":"insert","path":"list.txt","insert_line":1,"new_str":"middle"}`)
	requireContains(t, afterFirst, "after line 1")
	requireContains(t, afterFirst, "new lines 2-2")
	if got := fixture.read(t, "list.txt"); got != "start\nmiddle\na\nb" {
		t.Fatalf("file after insert at 1 = %q", got)
	}

	afterLast := fixture.invoke(t, `{"command":"insert","path":"list.txt","insert_line":4,"new_str":"end"}`)
	requireContains(t, afterLast, "after line 4")
	if got := fixture.read(t, "list.txt"); got != "start\nmiddle\na\nb\nend" {
		t.Fatalf("file after insert at end = %q", got)
	}

	requireError(t, fixture.invoke(t, `{"command":"insert","path":"list.txt","insert_line":99,"new_str":"x"}`),
		"invalid insert_line", "between 0 and")
	requireError(t, fixture.invoke(t, `{"command":"insert","path":"list.txt","new_str":"x"}`), "insert_line is required")
	requireError(t, fixture.invoke(t, `{"command":"insert","path":"list.txt","insert_line":0}`), "new_str is required")
}

// TestReadOnly verifies view keeps working while every edit is refused.
func TestReadOnly(t *testing.T) {
	fixture := newFixture(t, sandbox.WithReadOnly(true))
	fixture.write(t, "file.txt", "content")
	requireContains(t, fixture.invoke(t, `{"command":"view","path":"file.txt"}`), "1\tcontent")

	for _, arguments := range []string{
		`{"command":"create","path":"new.txt","file_text":"x"}`,
		`{"command":"str_replace","path":"file.txt","old_str":"content","new_str":"changed"}`,
		`{"command":"insert","path":"file.txt","insert_line":0,"new_str":"x"}`,
	} {
		requireError(t, fixture.invoke(t, arguments), "read-only")
	}
	if got := fixture.read(t, "file.txt"); got != "content" {
		t.Fatalf("read-only file changed to %q", got)
	}
}

// TestPathConfinement verifies traversal, absolute escapes and denied globs.
func TestPathConfinement(t *testing.T) {
	fixture := newFixture(t, sandbox.WithDeniedGlobs("**/.env"))
	fixture.write(t, ".env", "SECRET=1")
	requireError(t, fixture.invoke(t, `{"command":"view","path":".env"}`), "denied")

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	requireError(t, fixture.invoke(t, `{"command":"view","path":"../outside.txt"}`), "denied")
	requireError(t, fixture.invoke(t, fmt.Sprintf(`{"command":"view","path":%q}`, outsideFile)), "denied")

	// An absolute path inside the root is accepted.
	inside := fixture.write(t, "inside.txt", "inside")
	requireContains(t, fixture.invoke(t, fmt.Sprintf(`{"command":"view","path":%q}`, inside)), "1\tinside")
}

// TestBinaryAndOversized verifies the editor refuses binary and huge files.
func TestBinaryAndOversized(t *testing.T) {
	fixture := newFixture(t)
	binaryPath := filepath.Join(fixture.root, "binary.dat")
	if err := os.WriteFile(binaryPath, []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	requireError(t, fixture.invoke(t, `{"command":"view","path":"binary.dat"}`), "binary")

	hugePath := filepath.Join(fixture.root, "huge.txt")
	if err := os.WriteFile(hugePath, []byte(strings.Repeat("x", editor.MaxFileBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	requireError(t, fixture.invoke(t, `{"command":"view","path":"huge.txt"}`), "larger than")
}

// TestTruncation verifies view output is capped by the sandbox policy.
func TestTruncation(t *testing.T) {
	fixture := newFixture(t, sandbox.WithMaxOutputBytes(64))
	var builder strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&builder, "line-%d\n", i)
	}
	fixture.write(t, "long.txt", builder.String())
	result := fixture.invoke(t, `{"command":"view","path":"long.txt"}`)
	requireContains(t, result, "[truncated:")
	if len(result.Content) > 64+256 {
		t.Fatalf("truncated content is %d bytes, want a bounded result", len(result.Content))
	}
}

// TestToolMetadataAndArgs verifies the public identity, schema and argument
// error handling.
func TestToolMetadataAndArgs(t *testing.T) {
	tool := editor.New(sandbox.New(sandbox.WithRoots(t.TempDir())))
	if tool.Name() != "str_replace_editor" {
		t.Fatalf("Name() = %q", tool.Name())
	}
	if !strings.Contains(tool.Description(), "str_replace") {
		t.Fatalf("Description() looks unrelated: %q", tool.Description())
	}
	schema := tool.Schema()
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 2 {
		t.Fatalf("schema required = %v", schema["required"])
	}

	if result, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: `{invalid`}); err == nil {
		t.Fatalf("invalid argument JSON returned no Go error: %+v", result)
	}
	result, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: `{"command":"view"}`})
	if err != nil {
		t.Fatalf("missing path: %v", err)
	}
	requireError(t, result, "path is required")

	result, err = tool.Invoke(context.Background(), v1.ToolCall{Arguments: `{"command":"nope","path":"x"}`})
	if err != nil {
		t.Fatalf("unknown command: %v", err)
	}
	requireError(t, result, "unknown command")
}

// TestNilPolicyFailsClosed verifies a nil policy denies every path.
func TestNilPolicyFailsClosed(t *testing.T) {
	tool := editor.New(nil)
	result, err := tool.Invoke(context.Background(), v1.ToolCall{
		Arguments: `{"command":"view","path":"file.txt"}`,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	requireError(t, result, "denied")
}

// TestCreateThenViewRoundTrip verifies a created file is immediately viewable
// with the documented line format.
func TestCreateThenViewRoundTrip(t *testing.T) {
	fixture := newFixture(t)
	requireContains(t, fixture.invoke(t, `{"command":"create","path":"round.txt","file_text":"x\ny"}`), "created")
	view := fixture.invoke(t, `{"command":"view","path":"round.txt","view_range":[1,2]}`)
	requireContains(t, view, "1\tx")
	requireContains(t, view, "2\ty")

}
