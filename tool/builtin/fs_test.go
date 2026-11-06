package builtin_test

// Tests for the built-in file tools: normal behavior, output limits, error
// semantics and the root sandbox (relative, absolute and symlink escapes).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tool/builtin"
)

func TestReadFile(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "notes.txt", "alpha\nbeta\ngamma\n")

	result := fixture.invoke(t, builtin.ToolReadFile, `{"path":"notes.txt"}`)
	if result.IsError {
		t.Fatalf("read_file: %q", result.Content)
	}
	if want := "1\talpha\n2\tbeta\n3\tgamma\n"; result.Content != want {
		t.Fatalf("read_file content = %q, want %q", result.Content, want)
	}

	// offset is 1-based; limit bounds the returned lines.
	result = fixture.invoke(t, builtin.ToolReadFile, `{"path":"notes.txt","offset":2,"limit":1}`)
	if result.IsError || !strings.HasPrefix(result.Content, "2\tbeta\n") {
		t.Fatalf("read_file(offset=2, limit=1) = %q (IsError=%v), want line 2", result.Content, result.IsError)
	}
	requireContains(t, result.Content, "of 3") // paging notice
	// An offset past EOF is an empty successful result.
	result = fixture.invoke(t, builtin.ToolReadFile, `{"path":"notes.txt","offset":99}`)
	if result.IsError || result.Content != "" {
		t.Fatalf("read_file(offset past EOF) = %q (IsError=%v), want empty", result.Content, result.IsError)
	}
	// A non-positive limit means the default.
	result = fixture.invoke(t, builtin.ToolReadFile, `{"path":"notes.txt","limit":-1}`)
	if result.IsError || !strings.Contains(result.Content, "3\tgamma") {
		t.Fatalf("read_file(limit=-1) = %q, want all lines", result.Content)
	}

	// A missing path is a model-facing error.
	requireContains(t, fixture.invokeError(t, builtin.ToolReadFile, `{}`), "path is required")
	requireContains(t, fixture.invokeError(t, builtin.ToolReadFile, `{"path":"missing.txt"}`), "no such file")
	fixture.mkdir(t, "dir")
	requireContains(t, fixture.invokeError(t, builtin.ToolReadFile, `{"path":"dir"}`), "directory")
	requireContains(t, fixture.invokeError(t, builtin.ToolReadFile, `{"path":"notes.txt","offset":-2}`), "offset")
}

func TestReadFileTruncation(t *testing.T) {
	fixture := newFSFixture(t)
	var builder strings.Builder
	for line := 1; line <= builtin.MaxReadLines+500; line++ {
		fmt.Fprintf(&builder, "line-%d\n", line)
	}
	fixture.write(t, "big.txt", builder.String())

	result := fixture.invoke(t, builtin.ToolReadFile, `{"path":"big.txt"}`)
	if result.IsError {
		t.Fatalf("read_file: %q", result.Content)
	}
	requireContains(t, result.Content, fmt.Sprintf("%d\tline-%d", builtin.MaxReadLines, builtin.MaxReadLines), "truncated")
	requireNotContains(t, result.Content, fmt.Sprintf("line-%d", builtin.MaxReadLines+1))

	// One very long line is shortened to the documented per-line limit.
	long := strings.Repeat("x", builtin.MaxReadLineBytes+500)
	fixture.write(t, "long.txt", long+"\n")
	result = fixture.invoke(t, builtin.ToolReadFile, `{"path":"long.txt"}`)
	firstLine := strings.SplitN(result.Content, "\n", 2)[0]
	if len(firstLine) > builtin.MaxReadLineBytes+32 {
		t.Fatalf("long line length = %d, want <= %d+marker", len(firstLine), builtin.MaxReadLineBytes)
	}
	requireContains(t, firstLine, "…")
}

func TestWriteFile(t *testing.T) {
	fixture := newFSFixture(t)
	result := fixture.invoke(t, builtin.ToolWriteFile, `{"path":"sub/deep/new.txt","content":"hello\nworld\n"}`)
	if result.IsError {
		t.Fatalf("write_file: %q", result.Content)
	}
	requireContains(t, result.Content, "wrote 12 bytes")
	data, err := os.ReadFile(filepath.Join(fixture.root, "sub", "deep", "new.txt"))
	if err != nil {
		t.Fatalf("read back written file: %v", err)
	}
	if string(data) != "hello\nworld\n" {
		t.Fatalf("written content = %q", data)
	}

	// Overwrite.
	if result := fixture.invoke(t, builtin.ToolWriteFile, `{"path":"sub/deep/new.txt","content":"v2"}`); result.IsError {
		t.Fatalf("overwrite: %q", result.Content)
	}
	// Absolute path inside the root is accepted.
	inside := filepath.Join(fixture.root, "absolute.txt")
	result = fixture.invoke(t, builtin.ToolWriteFile, fmt.Sprintf(`{"path":%q,"content":"abs"}`, inside))
	if result.IsError {
		t.Fatalf("absolute inside write: %q", result.Content)
	}
	// The root itself is not a file.
	requireContains(t, fixture.invokeError(t, builtin.ToolWriteFile, `{"path":".","content":"x"}`), "file")
	requireContains(t, fixture.invokeError(t, builtin.ToolWriteFile, `{"path":"../escape.txt","content":"x"}`), "outside")
}

func TestEditFile(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "code.go", "package main\n\nfunc main() {}\n")

	result := fixture.invoke(t, builtin.ToolEditFile, `{"path":"code.go","old_string":"func main()","new_string":"func run()"}`)
	if result.IsError {
		t.Fatalf("edit_file: %q", result.Content)
	}
	requireContains(t, result.Content, "replaced 1 occurrence")
	data, _ := os.ReadFile(filepath.Join(fixture.root, "code.go"))
	if !strings.Contains(string(data), "func run()") || strings.Contains(string(data), "func main()") {
		t.Fatalf("edited content = %q", data)
	}

	// replace_all replaces every occurrence.
	fixture.write(t, "many.txt", "x\nx\nx\n")
	result = fixture.invoke(t, builtin.ToolEditFile, `{"path":"many.txt","old_string":"x","new_string":"y","replace_all":true}`)
	if result.IsError || !strings.Contains(result.Content, "replaced 3 occurrence") {
		t.Fatalf("replace_all result = %q (IsError=%v)", result.Content, result.IsError)
	}
	data, _ = os.ReadFile(filepath.Join(fixture.root, "many.txt"))
	if string(data) != "y\ny\ny\n" {
		t.Fatalf("replace_all content = %q", data)
	}

	// Zero matches and ambiguous non-replace_all edits are errors.
	fixture.write(t, "two.txt", "same\nsame\n")
	requireContains(t, fixture.invokeError(t, builtin.ToolEditFile, `{"path":"two.txt","old_string":"absent","new_string":"y"}`), "not found")
	ambiguous := fixture.invokeError(t, builtin.ToolEditFile, `{"path":"two.txt","old_string":"same","new_string":"y"}`)
	requireContains(t, ambiguous, "occurs 2 times", "replace_all")
	requireContains(t, fixture.invokeError(t, builtin.ToolEditFile, `{"path":"two.txt","old_string":"","new_string":"y"}`), "old_string")
	requireContains(t, fixture.invokeError(t, builtin.ToolEditFile, `{"path":"../two.txt","old_string":"a","new_string":"b"}`), "outside")
}

func TestListDir(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "b.txt", "b")
	fixture.write(t, "a.txt", "a")
	fixture.mkdir(t, "dir2")
	fixture.mkdir(t, "dir1")
	fixture.write(t, "dir1/nested.txt", "n")

	result := fixture.invoke(t, builtin.ToolListDir, `{}`)
	if result.IsError {
		t.Fatalf("list_dir: %q", result.Content)
	}
	want := "dir1/\ndir2/\na.txt\nb.txt"
	if result.Content != want {
		t.Fatalf("list_dir = %q, want %q (directories first, then names)", result.Content, want)
	}

	// A path argument lists a subdirectory.
	result = fixture.invoke(t, builtin.ToolListDir, `{"path":"dir1"}`)
	if result.Content != "nested.txt" {
		t.Fatalf("list_dir(dir1) = %q, want nested.txt", result.Content)
	}
	// Empty directory.
	fixture.mkdir(t, "empty")
	if result := fixture.invoke(t, builtin.ToolListDir, `{"path":"empty"}`); result.Content != "" || result.IsError {
		t.Fatalf("list_dir(empty) = %q (IsError=%v), want empty", result.Content, result.IsError)
	}
	requireContains(t, fixture.invokeError(t, builtin.ToolListDir, `{"path":"missing"}`), "no such file")
	requireContains(t, fixture.invokeError(t, builtin.ToolListDir, `{"path":".."}`), "outside")

	// Cap: 600 entries become 500 + a truncation notice.
	for index := 0; index < builtin.MaxDirEntries+100; index++ {
		fixture.write(t, fmt.Sprintf("many/file-%03d.txt", index), "")
	}
	result = fixture.invoke(t, builtin.ToolListDir, `{"path":"many"}`)
	requireContains(t, result.Content, "truncated", "more entries")
	if lines := strings.Count(result.Content, "\n"); lines != builtin.MaxDirEntries {
		t.Fatalf("truncated list_dir has %d lines, want %d entries + notice", lines, builtin.MaxDirEntries)
	}
}

func TestGlob(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "a.go", "")
	fixture.write(t, "b.txt", "")
	fixture.write(t, "sub/c.go", "")
	fixture.write(t, "sub/deep/d.go", "")
	fixture.write(t, "other/e.md", "")

	cases := []struct {
		name      string
		arguments string
		want      string
	}{
		{"single segment", `{"pattern":"*.go"}`, "a.go"},
		{"recursive", `{"pattern":"**/*.go"}`, "a.go\nsub/c.go\nsub/deep/d.go"},
		{"explicit prefix", `{"pattern":"sub/**/*.go"}`, "sub/c.go\nsub/deep/d.go"},
		{"sub-root", `{"pattern":"*.go","root":"sub"}`, "sub/c.go"},
		{"all files", `{"pattern":"**","root":"other"}`, "other/e.md"},
		{"no match", `{"pattern":"**/*.rs"}`, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := fixture.invoke(t, builtin.ToolGlob, testCase.arguments)
			if result.IsError {
				t.Fatalf("glob %s: %q", testCase.arguments, result.Content)
			}
			if result.Content != testCase.want {
				t.Fatalf("glob %s = %q, want %q", testCase.arguments, result.Content, testCase.want)
			}
		})
	}

	requireContains(t, fixture.invokeError(t, builtin.ToolGlob, `{"pattern":"/etc/*"}`), "relative")
	requireContains(t, fixture.invokeError(t, builtin.ToolGlob, `{"pattern":"*.go","root":".."}`), "outside")
	requireContains(t, fixture.invokeError(t, builtin.ToolGlob, `{}`), "pattern")

	// Cap: more matches than MaxGlobMatches are truncated with a notice.
	for index := 0; index < builtin.MaxGlobMatches+100; index++ {
		fixture.write(t, fmt.Sprintf("many/file-%03d.txt", index), "")
	}
	result := fixture.invoke(t, builtin.ToolGlob, `{"pattern":"many/*.txt"}`)
	requireContains(t, result.Content, "truncated", "matches")
}

func TestGrep(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "a.txt", "hello world\nfoo bar\n")
	fixture.write(t, "b.go", "hello go\n")
	fixture.write(t, "sub/c.txt", "hello again\n")
	fixture.write(t, "binary.bin", "hello\x00binary\n")

	result := fixture.invoke(t, builtin.ToolGrep, `{"pattern":"hello"}`)
	requireContains(t, result.Content, "a.txt:1:hello world", "b.go:1:hello go", "sub/c.txt:1:hello again")
	requireNotContains(t, result.Content, "binary.bin") // binary files are skipped

	result = fixture.invoke(t, builtin.ToolGrep, `{"pattern":"hello","include":"*.go"}`)
	requireContains(t, result.Content, "b.go:1:hello go")
	requireNotContains(t, result.Content, "a.txt")

	result = fixture.invoke(t, builtin.ToolGrep, `{"pattern":"hello","path":"sub"}`)
	requireContains(t, result.Content, "sub/c.txt:1:hello again")
	requireNotContains(t, result.Content, "a.txt")

	if result := fixture.invoke(t, builtin.ToolGrep, `{"pattern":"zzz"}`); result.Content != "" || result.IsError {
		t.Fatalf("grep(no match) = %q (IsError=%v), want empty", result.Content, result.IsError)
	}
	requireContains(t, fixture.invokeError(t, builtin.ToolGrep, `{"pattern":"["}`), "regular expression")
	requireContains(t, fixture.invokeError(t, builtin.ToolGrep, `{"pattern":"x","include":"["}`), "include")
	requireContains(t, fixture.invokeError(t, builtin.ToolGrep, `{"pattern":"x","path":".."}`), "outside")
	requireContains(t, fixture.invokeError(t, builtin.ToolGrep, `{}`), "pattern")

	// Cap: at most MaxGrepMatches matching lines plus a notice.
	var builder strings.Builder
	for line := 1; line <= builtin.MaxGrepMatches+50; line++ {
		builder.WriteString("match line\n")
	}
	fixture.write(t, "many.txt", builder.String())
	result = fixture.invoke(t, builtin.ToolGrep, `{"pattern":"match","path":"many.txt"}`)
	requireContains(t, result.Content, "truncated")
	if matches := strings.Count(result.Content, "many.txt:"); matches != builtin.MaxGrepMatches {
		t.Fatalf("grep matches = %d, want %d", matches, builtin.MaxGrepMatches)
	}
}

func TestFileToolsRejectPathEscapes(t *testing.T) {
	fixture := newFSFixture(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	// An absolute path outside the root.
	for _, toolName := range []string{
		builtin.ToolReadFile, builtin.ToolWriteFile, builtin.ToolEditFile,
		builtin.ToolListDir, builtin.ToolGrep,
	} {
		arguments := fmt.Sprintf(`{"path":%q}`, secret)
		if toolName == builtin.ToolWriteFile {
			arguments = fmt.Sprintf(`{"path":%q,"content":"x"}`, secret)
		}
		if toolName == builtin.ToolEditFile {
			arguments = fmt.Sprintf(`{"path":%q,"old_string":"top","new_string":"x"}`, secret)
		}
		if toolName == builtin.ToolGrep {
			arguments = fmt.Sprintf(`{"pattern":"secret","path":%q}`, secret)
		}
		requireContains(t, fixture.invokeError(t, toolName, arguments), "outside")
	}
	requireContains(t, fixture.invokeError(t, builtin.ToolGlob, fmt.Sprintf(`{"pattern":"*","root":%q}`, outside)), "outside")

	// A ".." escape.
	requireContains(t, fixture.invokeError(t, builtin.ToolReadFile, `{"path":"../secret.txt"}`), "outside")

	// A symlink that points outside the root is refused by the os.Root layer,
	// for reads and for writes through the link.
	link := filepath.Join(fixture.root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	requireContains(t, fixture.invokeError(t, builtin.ToolReadFile, `{"path":"escape/secret.txt"}`), "read_file")
	requireContains(t, fixture.invokeError(t, builtin.ToolWriteFile, `{"path":"escape/new.txt","content":"x"}`), "write_file")
	requireContains(t, fixture.invokeError(t, builtin.ToolListDir, `{"path":"escape"}`), "list_dir")
	// And glob must not walk through the escaping link into the outside tree.
	result := fixture.invoke(t, builtin.ToolGlob, `{"pattern":"**/*.txt"}`)
	requireNotContains(t, result.Content, "secret.txt")

	// A relative in-root symlink is fine and stays within the sandbox.
	// (os.Root refuses absolute symlinks even when they point inside the root,
	// so only relative links are usable.)
	fixture.mkdir(t, "inside")
	if err := os.Symlink("inside", filepath.Join(fixture.root, "inside-link")); err != nil {
		t.Fatalf("inside symlink: %v", err)
	}
	fixture.write(t, "inside/ok.txt", "ok\n")
	result = fixture.invoke(t, builtin.ToolReadFile, `{"path":"inside-link/ok.txt"}`)
	if result.IsError || !strings.Contains(result.Content, "ok") {
		t.Fatalf("read through in-root symlink = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestMalformedArgumentsAreGoErrors(t *testing.T) {
	fixture := newFSFixture(t)
	for _, toolName := range fixture.toolNames() {
		tool := fixture.tool(t, toolName)
		_, err := tool.Invoke(context.Background(), v1.ToolCall{Name: toolName, Arguments: "{"})
		if err == nil {
			t.Errorf("%s.Invoke(malformed JSON) returned nil error", toolName)
		}
	}
}

func TestFileToolsAreConcurrent(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "shared.txt", "shared\n")
	tool := fixture.tool(t, builtin.ToolReadFile)
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for call := 0; call < 25; call++ {
				result, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: `{"path":"shared.txt"}`})
				if err != nil || result.IsError {
					t.Errorf("concurrent read_file: result=%q err=%v", result.Content, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
