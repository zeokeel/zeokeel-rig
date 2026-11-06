package builtin_test

// Tests for the sandbox-policy-aware built-in tool set (NewWithPolicy): path
// confinement, read-only mode, denied globs, shell command/env/timeout/output
// policy, and the legacy constructor compatibility.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// policyRoot returns a real (symlink-resolved) temporary root.
func policyRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	return real
}

// policyToolMap indexes a NewWithPolicy tool set by name.
func policyToolMap(t *testing.T, tools []rig.Tool) map[string]rig.Tool {
	t.Helper()
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

// invokePolicy invokes one policy tool and requires no Go error.
func invokePolicy(t *testing.T, tools map[string]rig.Tool, name, arguments string) rig.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("tool %q not found in %v", name, toolNames(tools))
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", name, err)
	}
	return result
}

func toolNames(tools map[string]rig.Tool) []string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	return names
}

// invokePolicyError requires IsError and returns the model-facing text.
func invokePolicyError(t *testing.T, tools map[string]rig.Tool, name, arguments string) string {
	t.Helper()
	result := invokePolicy(t, tools, name, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", name, result.Content)
	}
	return result.Content
}

func writePolicyFile(t *testing.T, root, relative, content string) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", abs, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", abs, err)
	}
	return abs
}

func TestNewWithPolicyToolSet(t *testing.T) {
	root := policyRoot(t)
	cases := map[string]*sandbox.Policy{
		"policy": sandbox.New(sandbox.WithRoots(root)),
		"nil":    nil,
	}
	for name, policy := range cases {
		tools := builtin.NewWithPolicy(policy)
		if len(tools) != 7 {
			t.Fatalf("%s: NewWithPolicy returned %d tools, want 7", name, len(tools))
		}
		byName := policyToolMap(t, tools)
		for _, want := range []string{
			builtin.ToolReadFile, builtin.ToolWriteFile, builtin.ToolEditFile,
			builtin.ToolListDir, builtin.ToolGlob, builtin.ToolGrep, builtin.ToolBash,
		} {
			if _, ok := byName[want]; !ok {
				t.Fatalf("%s: missing tool %q", name, want)
			}
		}
	}
}

func TestPolicyFileToolsWork(t *testing.T) {
	root := policyRoot(t)
	tools := policyToolMap(t, builtin.NewWithPolicy(sandbox.New(sandbox.WithRoots(root))))

	if got := invokePolicy(t, tools, builtin.ToolWriteFile, `{"path":"sub/a.txt","content":"hello\n"}`); got.IsError {
		t.Fatalf("write_file: %q", got.Content)
	}
	if got := invokePolicy(t, tools, builtin.ToolReadFile, `{"path":"sub/a.txt"}`); got.IsError || !strings.Contains(got.Content, "hello") {
		t.Fatalf("read_file: %q (IsError=%v)", got.Content, got.IsError)
	}
	if got := invokePolicy(t, tools, builtin.ToolEditFile, `{"path":"sub/a.txt","old_string":"hello","new_string":"bye"}`); got.IsError {
		t.Fatalf("edit_file: %q", got.Content)
	}
	if got := invokePolicy(t, tools, builtin.ToolReadFile, `{"path":"sub/a.txt"}`); !strings.Contains(got.Content, "bye") {
		t.Fatalf("edit did not apply: %q", got.Content)
	}
	if got := invokePolicy(t, tools, builtin.ToolListDir, `{"path":"sub"}`); !strings.Contains(got.Content, "a.txt") {
		t.Fatalf("list_dir: %q", got.Content)
	}
	if got := invokePolicy(t, tools, builtin.ToolGlob, `{"pattern":"**/*.txt"}`); !strings.Contains(got.Content, "sub/a.txt") {
		t.Fatalf("glob: %q", got.Content)
	}
	if got := invokePolicy(t, tools, builtin.ToolGrep, `{"pattern":"bye"}`); !strings.Contains(got.Content, "sub/a.txt") {
		t.Fatalf("grep: %q", got.Content)
	}
}

func TestPolicyReadOnly(t *testing.T) {
	root := policyRoot(t)
	writePolicyFile(t, root, "existing.txt", "original")
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithReadOnly(true))
	tools := policyToolMap(t, builtin.NewWithPolicy(policy))

	if got := invokePolicy(t, tools, builtin.ToolReadFile, `{"path":"existing.txt"}`); got.IsError {
		t.Fatalf("read_file under read-only: %q", got.Content)
	}
	for name, arguments := range map[string]string{
		builtin.ToolWriteFile: `{"path":"new.txt","content":"x"}`,
		builtin.ToolEditFile:  `{"path":"existing.txt","old_string":"original","new_string":"changed"}`,
	} {
		text := invokePolicyError(t, tools, name, arguments)
		if !strings.Contains(text, "read-only") {
			t.Fatalf("%s denial = %q, want read-only mention", name, text)
		}
	}
	if got := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo hi"}`); !got.IsError || !strings.Contains(got.Content, "read-only") {
		t.Fatalf("bash under read-only = %q (IsError=%v)", got.Content, got.IsError)
	}
	if data, err := os.ReadFile(filepath.Join(root, "existing.txt")); err != nil || string(data) != "original" {
		t.Fatalf("read-only policy modified the file: %q, %v", data, err)
	}
}

func TestPolicyPathConfinement(t *testing.T) {
	root := policyRoot(t)
	outside := policyRoot(t)
	writePolicyFile(t, outside, "secret.txt", "secret")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writePolicyFile(t, root, "ok.txt", "ok")
	tools := policyToolMap(t, builtin.NewWithPolicy(sandbox.New(sandbox.WithRoots(root))))

	for name, arguments := range map[string]string{
		builtin.ToolReadFile:  `{"path":"../secret.txt"}`,
		builtin.ToolWriteFile: `{"path":"../escape.txt","content":"x"}`,
		builtin.ToolListDir:   `{"path":".."}`,
		builtin.ToolGlob:      `{"pattern":"*"}`,
	} {
		if name == builtin.ToolGlob {
			continue // glob has no path escape of its own in this fixture
		}
		text := invokePolicyError(t, tools, name, arguments)
		if !strings.Contains(text, "sandbox: denied") {
			t.Fatalf("%s denial = %q, want sandbox denial", name, text)
		}
	}
	// Absolute path outside the policy root.
	absolute := filepath.Join(outside, "secret.txt")
	if text := invokePolicyError(t, tools, builtin.ToolReadFile, `{"path":"`+absolute+`"}`); !strings.Contains(text, "sandbox: denied") {
		t.Fatalf("absolute escape denial = %q", text)
	}
	// A symlink that resolves outside the root is refused.
	if text := invokePolicyError(t, tools, builtin.ToolReadFile, `{"path":"escape.txt"}`); !strings.Contains(text, "sandbox: denied") {
		t.Fatalf("symlink escape denial = %q", text)
	}
	if got := invokePolicy(t, tools, builtin.ToolReadFile, `{"path":"ok.txt"}`); got.IsError {
		t.Fatalf("read_file(ok.txt) = %q", got.Content)
	}
}

func TestPolicyMultiRoot(t *testing.T) {
	primary := policyRoot(t)
	secondary := policyRoot(t)
	writePolicyFile(t, primary, "a.txt", "primary")
	secondaryFile := writePolicyFile(t, secondary, "b.txt", "secondary")
	policy := sandbox.New(sandbox.WithRoots(primary, secondary))
	tools := policyToolMap(t, builtin.NewWithPolicy(policy))

	if got := invokePolicy(t, tools, builtin.ToolReadFile, `{"path":"a.txt"}`); got.IsError || !strings.Contains(got.Content, "primary") {
		t.Fatalf("primary root read = %q", got.Content)
	}
	arguments := `{"path":"` + secondaryFile + `"}`
	if got := invokePolicy(t, tools, builtin.ToolReadFile, arguments); got.IsError || !strings.Contains(got.Content, "secondary") {
		t.Fatalf("secondary root read = %q (IsError=%v)", got.Content, got.IsError)
	}
	// Writes to the secondary root work through the same policy.
	secondaryNew := filepath.Join(secondary, "new.txt")
	if got := invokePolicy(t, tools, builtin.ToolWriteFile, `{"path":"`+secondaryNew+`","content":"written"}`); got.IsError {
		t.Fatalf("secondary root write = %q", got.Content)
	}
	if data, err := os.ReadFile(secondaryNew); err != nil || string(data) != "written" {
		t.Fatalf("secondary write = %q, %v", data, err)
	}
}

func TestPolicyDeniedGlobs(t *testing.T) {
	root := policyRoot(t)
	writePolicyFile(t, root, ".env", "SECRET=top")
	writePolicyFile(t, root, "sub/.env", "SECRET=nested")
	writePolicyFile(t, root, "sub/ok.txt", "SECRET visible")
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithDeniedGlobs("**/.env"))
	tools := policyToolMap(t, builtin.NewWithPolicy(policy))

	for _, path := range []string{".env", "sub/.env"} {
		if text := invokePolicyError(t, tools, builtin.ToolReadFile, `{"path":"`+path+`"}`); !strings.Contains(text, "sandbox: denied") {
			t.Fatalf("read_file(%s) denial = %q", path, text)
		}
		if text := invokePolicyError(t, tools, builtin.ToolWriteFile, `{"path":"`+path+`","content":"x"}`); !strings.Contains(text, "sandbox: denied") {
			t.Fatalf("write_file(%s) denial = %q", path, text)
		}
	}
	// list_dir hides denied entries.
	listing := invokePolicy(t, tools, builtin.ToolListDir, `{}`)
	if listing.IsError || strings.Contains(listing.Content, ".env") {
		t.Fatalf("list_dir leaked denied entry: %q", listing.Content)
	}
	if !strings.Contains(listing.Content, "sub/") {
		t.Fatalf("list_dir lost the sub directory: %q", listing.Content)
	}
	// glob hides denied matches.
	globbed := invokePolicy(t, tools, builtin.ToolGlob, `{"pattern":"**/*"}`)
	if globbed.IsError || strings.Contains(globbed.Content, ".env") {
		t.Fatalf("glob leaked denied match: %q", globbed.Content)
	}
	// grep does not search denied files (sub/ok.txt still matches).
	grep := invokePolicy(t, tools, builtin.ToolGrep, `{"pattern":"SECRET"}`)
	if grep.IsError {
		t.Fatalf("grep: %q", grep.Content)
	}
	if strings.Contains(grep.Content, ".env") {
		t.Fatalf("grep searched a denied file: %q", grep.Content)
	}
	if !strings.Contains(grep.Content, "sub/ok.txt") {
		t.Fatalf("grep missed the allowed file: %q", grep.Content)
	}
}

func TestPolicyShellAllowlist(t *testing.T) {
	root := policyRoot(t)
	tools := policyToolMap(t, builtin.NewWithPolicy(
		sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("echo"))))
	if got := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo allowed"}`); got.IsError || !strings.Contains(got.Content, "allowed") {
		t.Fatalf("allowed command = %q (IsError=%v)", got.Content, got.IsError)
	}
	for _, command := range []string{"ls", "rm -rf x"} {
		arguments := `{"command":"` + command + `"}`
		if text := invokePolicyError(t, tools, builtin.ToolBash, arguments); !strings.Contains(text, "not in the allowlist") {
			t.Fatalf("denied command %q = %q", command, text)
		}
	}
	// Path forms are rejected before the allowlist so a workspace binary cannot
	// borrow an allow-listed basename.
	for _, command := range []string{"/bin/ls -la", "./echo hi", "sub/echo hi"} {
		arguments := `{"command":"` + command + `"}`
		if text := invokePolicyError(t, tools, builtin.ToolBash, arguments); !strings.Contains(text, "bare command name") {
			t.Fatalf("path command %q = %q", command, text)
		}
	}
	// Quoted command substitution executes in /bin/sh even though the operator
	// is quoted: the policy must reject it.
	for _, command := range []string{`echo "$(id)"`, "echo `id`", `echo "${x:-$(id)}"`, `cat <(id)`} {
		arguments := `{"command":` + strconv.Quote(command) + `}`
		if text := invokePolicyError(t, tools, builtin.ToolBash, arguments); !strings.Contains(text, "not allowed") {
			t.Fatalf("substitution command %q = %q", command, text)
		}
	}
	// No allowlist means the shell is disabled.
	noShell := policyToolMap(t, builtin.NewWithPolicy(sandbox.New(sandbox.WithRoots(root))))
	if text := invokePolicyError(t, noShell, builtin.ToolBash, `{"command":"echo hi"}`); !strings.Contains(text, "disabled") {
		t.Fatalf("disabled shell = %q", text)
	}
	// Leading VAR=value assignments are rejected: they can change PATH or
	// LD_PRELOAD and make the allowlist advisory.
	if text := invokePolicyError(t, tools, builtin.ToolBash, `{"command":"RIG_TEST=1 echo assignment"}`); !strings.Contains(text, "environment variable assignments") {
		t.Fatalf("env-assignment command = %q", text)
	}
	if text := invokePolicyError(t, tools, builtin.ToolBash, `{"command":"PATH=. echo hijack"}`); !strings.Contains(text, "environment variable assignments") {
		t.Fatalf("PATH assignment command = %q", text)
	}
}

func TestPolicyShellEnvWhitelist(t *testing.T) {
	root := policyRoot(t)
	t.Setenv("RIG_TEST_VISIBLE", "visible-value")
	t.Setenv("RIG_TEST_HIDDEN", "hidden-value")
	tools := policyToolMap(t, builtin.NewWithPolicy(sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist("echo"),
		sandbox.WithEnvAllowlist("RIG_TEST_VISIBLE"),
	)))
	got := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo v=$RIG_TEST_VISIBLE h=$RIG_TEST_HIDDEN"}`)
	if got.IsError {
		t.Fatalf("bash: %q", got.Content)
	}
	if !strings.Contains(got.Content, "v=visible-value") {
		t.Fatalf("allowed variable missing: %q", got.Content)
	}
	if strings.Contains(got.Content, "hidden-value") || !strings.Contains(got.Content, "h=") {
		t.Fatalf("hidden variable leaked: %q", got.Content)
	}

	// Default (no WithEnvAllowlist) is the minimal PATH/HOME/LANG set.
	defaultTools := policyToolMap(t, builtin.NewWithPolicy(
		sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("echo"))))
	got = invokePolicy(t, defaultTools, builtin.ToolBash, `{"command":"echo h=$RIG_TEST_HIDDEN"}`)
	if got.IsError || strings.Contains(got.Content, "hidden-value") || !strings.Contains(got.Content, "h=") {
		t.Fatalf("default env leak: %q (IsError=%v)", got.Content, got.IsError)
	}
}

func TestPolicyShellTimeoutCeiling(t *testing.T) {
	root := policyRoot(t)
	tools := policyToolMap(t, builtin.NewWithPolicy(sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist("sleep"),
		sandbox.WithTimeout(50*time.Millisecond),
	)))
	started := time.Now()
	got := invokePolicy(t, tools, builtin.ToolBash, `{"command":"sleep 5"}`)
	elapsed := time.Since(started)
	if !got.IsError || !strings.Contains(got.Content, "timeout") {
		t.Fatalf("timeout result = %q (IsError=%v)", got.Content, got.IsError)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took %s, want it near the 50ms policy ceiling", elapsed)
	}
}

func TestPolicyShellOutputCap(t *testing.T) {
	root := policyRoot(t)
	// This test needs a pipe to produce more than the cap, so it opts out of the
	// strict shell-operator check explicitly.
	tools := policyToolMap(t, builtin.NewWithPolicy(sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist("dd"),
		sandbox.WithMaxOutputBytes(64),
		sandbox.WithAllowShellOperators(),
	)))
	got := invokePolicy(t, tools, builtin.ToolBash, `{"command":"dd if=/dev/zero bs=4096 count=8 2>/dev/null | tr '\\0' x"}`)
	if got.IsError {
		t.Fatalf("bash: %q", got.Content)
	}
	if len(got.Content) > 64+256 {
		t.Fatalf("output length = %d, want <= 64 + truncation notice", len(got.Content))
	}
	if !strings.Contains(got.Content, "truncated") {
		t.Fatalf("output was not truncated: %q", got.Content)
	}
}

func TestPolicyShellWorkdir(t *testing.T) {
	root := policyRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	tools := policyToolMap(t, builtin.NewWithPolicy(
		sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("pwd"))))
	got := invokePolicy(t, tools, builtin.ToolBash, `{"command":"pwd","workdir":"sub"}`)
	if got.IsError || !strings.HasSuffix(strings.TrimSpace(got.Content), "sub") {
		t.Fatalf("pwd in sub = %q (IsError=%v)", got.Content, got.IsError)
	}
	if text := invokePolicyError(t, tools, builtin.ToolBash, `{"command":"pwd","workdir":".."}`); !strings.Contains(text, "sandbox: denied") {
		t.Fatalf("workdir escape = %q", text)
	}
}

func TestPolicyFileOutputCap(t *testing.T) {
	root := policyRoot(t)
	writePolicyFile(t, root, "big.txt", strings.Repeat("a", 500)+"\n")
	tools := policyToolMap(t, builtin.NewWithPolicy(sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMaxOutputBytes(32),
	)))
	got := invokePolicy(t, tools, builtin.ToolReadFile, `{"path":"big.txt"}`)
	if got.IsError {
		t.Fatalf("read_file: %q", got.Content)
	}
	if len(got.Content) > 32+256 {
		t.Fatalf("read_file output length = %d, want <= 32 + notice", len(got.Content))
	}
	if !strings.Contains(got.Content, "truncated") {
		t.Fatalf("read_file output was not truncated: %q", got.Content)
	}
}

func TestPolicyDefaultDeniesEverything(t *testing.T) {
	tools := policyToolMap(t, builtin.NewWithPolicy(nil))
	for name, arguments := range map[string]string{
		builtin.ToolReadFile:  `{"path":"x.txt"}`,
		builtin.ToolWriteFile: `{"path":"x.txt","content":"x"}`,
		builtin.ToolListDir:   `{}`,
		builtin.ToolGlob:      `{"pattern":"*"}`,
		builtin.ToolGrep:      `{"pattern":"x"}`,
	} {
		if text := invokePolicyError(t, tools, name, arguments); !strings.Contains(text, "sandbox: denied") {
			t.Fatalf("%s default denial = %q", name, text)
		}
	}
	if text := invokePolicyError(t, tools, builtin.ToolBash, `{"command":"echo hi"}`); !strings.Contains(text, "disabled") {
		t.Fatalf("default bash denial = %q", text)
	}
}

// TestLegacyConstructorsUnchanged proves the v0.2 constructors keep their
// behavior: no command allowlist, the process environment is inherited and
// write tools work.
func TestLegacyConstructorsUnchanged(t *testing.T) {
	root := policyRoot(t)
	t.Setenv("RIG_LEGACY_VISIBLE", "legacy-value")
	bash := builtin.NewShell(root, time.Second)
	result, err := bash.Invoke(context.Background(), rig.ToolCall{
		Name: builtin.ToolBash, Arguments: `{"command":"echo $RIG_LEGACY_VISIBLE"}`,
	})
	if err != nil {
		t.Fatalf("legacy bash error: %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "legacy-value") {
		t.Fatalf("legacy bash did not inherit the environment: %q (IsError=%v)", result.Content, result.IsError)
	}

	fsTools := policyToolMap(t, builtin.NewFS(root))
	write := invokePolicy(t, fsTools, builtin.ToolWriteFile, `{"path":"legacy.txt","content":"ok"}`)
	if write.IsError {
		t.Fatalf("legacy NewFS write_file = %q", write.Content)
	}
	if _, err := os.Stat(filepath.Join(root, "legacy.txt")); err != nil {
		t.Fatalf("legacy write not applied: %v", err)
	}
}
