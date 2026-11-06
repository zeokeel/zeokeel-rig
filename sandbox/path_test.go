package sandbox_test

// Tests for the sandbox policy path layer: roots, traversal, symlinks, denied
// globs, multi-root resolution and the fail-closed default.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// testRoot returns a real (symlink-resolved) temporary directory.
func testRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	return real
}

// mustMkdirAll creates a directory tree under root and returns its absolute path.
func mustMkdirAll(t *testing.T, root, relative string) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", abs, err)
	}
	return abs
}

// mustWriteFile writes one file under root.
func mustWriteFile(t *testing.T, root, relative, content string) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("MkdirAll for %q: %v", abs, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", abs, err)
	}
	return abs
}

func requireDenied(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a denial, got nil", context)
	}
	if !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("%s: error %v does not wrap sandbox.ErrDenied", context, err)
	}
}

func TestDefaultPolicyFailsClosed(t *testing.T) {
	policy := sandbox.New()
	if got := policy.Roots(); len(got) != 0 {
		t.Fatalf("default policy roots = %v, want none", got)
	}
	if policy.ReadOnly() {
		t.Fatal("default policy should not be read-only")
	}
	if policy.NetworkAllowed() {
		t.Fatal("default policy should not allow network")
	}
	if policy.Timeout() != sandbox.DefaultTimeout {
		t.Fatalf("Timeout = %s, want %s", policy.Timeout(), sandbox.DefaultTimeout)
	}
	if policy.MaxOutputBytes() != sandbox.DefaultMaxOutputBytes {
		t.Fatalf("MaxOutputBytes = %d, want %d", policy.MaxOutputBytes(), sandbox.DefaultMaxOutputBytes)
	}
	_, err := policy.ResolvePath("anything.txt")
	requireDenied(t, err, "default ResolvePath")
	requireDenied(t, policy.CheckPath(filepath.Join(t.TempDir(), "x")), "default CheckPath")
	requireDenied(t, policy.CheckCommand([]string{"echo"}, ""), "default CheckCommand")
}

func TestResolvePathInsideRoot(t *testing.T) {
	root := testRoot(t)
	mustWriteFile(t, root, "notes.txt", "hello")
	mustMkdirAll(t, root, "sub")
	policy := sandbox.New(sandbox.WithRoots(root))

	got, err := policy.ResolvePath("notes.txt")
	if err != nil {
		t.Fatalf("ResolvePath(notes.txt): %v", err)
	}
	if want := filepath.Join(root, "notes.txt"); got != want {
		t.Fatalf("ResolvePath = %q, want %q", got, want)
	}

	// The root itself.
	got, err = policy.ResolvePath("")
	if err != nil || got != root {
		t.Fatalf("ResolvePath(\"\") = %q, %v; want %q", got, err, root)
	}

	// A not-yet-created file resolves through its existing ancestors.
	got, err = policy.ResolvePath("sub/new/deep.txt")
	if err != nil {
		t.Fatalf("ResolvePath(new file): %v", err)
	}
	if want := filepath.Join(root, "sub", "new", "deep.txt"); got != want {
		t.Fatalf("ResolvePath(new file) = %q, want %q", got, want)
	}

	// An absolute path inside the root is accepted.
	abs := filepath.Join(root, "notes.txt")
	got, err = policy.ResolvePath(abs)
	if err != nil || got != abs {
		t.Fatalf("ResolvePath(absolute inside) = %q, %v; want %q", got, err, abs)
	}
	if err := policy.CheckPath(abs); err != nil {
		t.Fatalf("CheckPath(inside): %v", err)
	}
}

func TestResolvePathRejectsEscapes(t *testing.T) {
	root := testRoot(t)
	mustMkdirAll(t, root, "sub")
	policy := sandbox.New(sandbox.WithRoots(root))
	outside := testRoot(t)
	mustWriteFile(t, outside, "secret.txt", "top secret")
	outsideFile := filepath.Join(outside, "secret.txt")

	requested := []string{
		"../secret.txt",
		"sub/../../secret.txt",
		"sub/../notes.txt", // parent components are rejected even when harmless
		outsideFile,
		filepath.Join(root, "..", "x"),
		"a\x00b.txt",
	}
	for _, path := range requested {
		if _, err := policy.ResolvePath(path); err == nil {
			t.Fatalf("ResolvePath(%q) was allowed, want denial", path)
		} else {
			requireDenied(t, err, "ResolvePath("+path+")")
		}
	}
}

func TestResolvePathRejectsSymlinkEscapes(t *testing.T) {
	root := testRoot(t)
	outside := testRoot(t)
	mustWriteFile(t, outside, "secret.txt", "top secret")
	// An absolute symlink pointing outside the root: os.Root and the policy
	// must both refuse to follow it.
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A relative symlink inside the root remains usable.
	mustMkdirAll(t, root, "sub")
	if err := os.Symlink("sub", filepath.Join(root, "inside-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("inside-link", filepath.Join(root, "inside2")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(root))

	_, err := policy.ResolvePath("escape.txt")
	requireDenied(t, err, "ResolvePath(absolute symlink escape)")

	got, err := policy.ResolvePath("inside2")
	if err != nil {
		t.Fatalf("ResolvePath(in-root relative symlink): %v", err)
	}
	if want := filepath.Join(root, "sub"); got != want {
		t.Fatalf("ResolvePath(in-root symlink) = %q, want %q", got, want)
	}

	// OpenPath must refuse the escape as well (never hand out an os.Root for
	// an escaping path).
	if opened, _, _, _, err := policy.OpenPath("escape.txt"); err == nil {
		opened.Close()
		t.Fatal("OpenPath(absolute symlink escape) was allowed")
	}
}

func TestResolvePathMultipleRoots(t *testing.T) {
	primary := testRoot(t)
	secondary := testRoot(t)
	mustWriteFile(t, primary, "a.txt", "a")
	mustWriteFile(t, secondary, "b.txt", "b")
	policy := sandbox.New(sandbox.WithRoots(primary, secondary))

	// Relative paths use the primary root.
	got, err := policy.ResolvePath("a.txt")
	if err != nil || got != filepath.Join(primary, "a.txt") {
		t.Fatalf("ResolvePath(primary relative) = %q, %v", got, err)
	}
	// Absolute paths may enter any root.
	absSecondary := filepath.Join(secondary, "b.txt")
	got, err = policy.ResolvePath(absSecondary)
	if err != nil || got != absSecondary {
		t.Fatalf("ResolvePath(secondary absolute) = %q, %v", got, err)
	}
	if err := policy.CheckPath(absSecondary); err != nil {
		t.Fatalf("CheckPath(secondary): %v", err)
	}
	// A third directory is still outside.
	if _, err := policy.ResolvePath(testRoot(t)); err == nil {
		t.Fatal("ResolvePath(third root) was allowed")
	}
	if roots := policy.Roots(); len(roots) != 2 || roots[0] != primary || roots[1] != secondary {
		t.Fatalf("Roots() = %v", roots)
	}
}

func TestWithRootsNormalization(t *testing.T) {
	root := testRoot(t)
	mustMkdirAll(t, root, "sub")
	policy := sandbox.New(sandbox.WithRoots("", root, root+"/", filepath.Join(root, "sub")))
	roots := policy.Roots()
	if len(roots) != 2 {
		t.Fatalf("Roots() = %v, want the root and root/sub de-duplicated", roots)
	}
	for _, got := range roots {
		if got != root && got != filepath.Join(root, "sub") {
			t.Fatalf("unexpected root %q", got)
		}
	}
	empty := sandbox.New(sandbox.WithRoots())
	if len(empty.Roots()) != 0 {
		t.Fatalf("WithRoots() should remove roots, got %v", empty.Roots())
	}
}

func TestCheckPath(t *testing.T) {
	root := testRoot(t)
	mustWriteFile(t, root, "ok.txt", "ok")
	policy := sandbox.New(sandbox.WithRoots(root))

	if err := policy.CheckPath(filepath.Join(root, "ok.txt")); err != nil {
		t.Fatalf("CheckPath(inside): %v", err)
	}
	if err := policy.CheckPath(root); err != nil {
		t.Fatalf("CheckPath(root): %v", err)
	}
	for _, path := range []string{
		filepath.Join(root, "..", "outside.txt"),
		"/etc/passwd",
		"relative/path.txt",
		"",
	} {
		if err := policy.CheckPath(path); err == nil {
			t.Fatalf("CheckPath(%q) was allowed", path)
		}
	}
	if policy.DeniedPath(filepath.Join(root, "ok.txt")) {
		t.Fatal("DeniedPath(inside) = true, want false")
	}
}

func TestDeniedGlobs(t *testing.T) {
	root := testRoot(t)
	mustWriteFile(t, root, ".env", "SECRET=1")
	mustWriteFile(t, root, "sub/.env", "SECRET=2")
	mustWriteFile(t, root, "key.pem", "pem")
	mustWriteFile(t, root, "secrets/db.txt", "db")
	mustWriteFile(t, root, "ok.txt", "ok")
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithDeniedGlobs("**/.env", "*.pem", "secrets/**"),
	)

	denied := []string{".env", "sub/.env", "key.pem", "sub/key.pem", "secrets/db.txt"}
	for _, path := range denied {
		if _, err := policy.ResolvePath(path); err == nil {
			t.Fatalf("ResolvePath(%q) was allowed by denied globs", path)
		} else {
			requireDenied(t, err, "ResolvePath("+path+")")
		}
	}
	for _, path := range []string{"ok.txt", "sub/ok.txt"} {
		if _, err := policy.ResolvePath(path); err != nil {
			t.Fatalf("ResolvePath(%q) denied: %v", path, err)
		}
	}
}

func TestReadOnlyAccessors(t *testing.T) {
	root := testRoot(t)
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithReadOnly(true))
	if !policy.ReadOnly() {
		t.Fatal("ReadOnly() = false")
	}
	// Read-only does not block path resolution; write tools enforce it.
	if _, err := policy.ResolvePath("file.txt"); err != nil {
		t.Fatalf("ResolvePath under read-only: %v", err)
	}
	network := sandbox.New(sandbox.WithNetwork(true))
	if !network.NetworkAllowed() {
		t.Fatal("NetworkAllowed() = false")
	}
	limits := sandbox.New(sandbox.WithMaxOutputBytes(1024), sandbox.WithTimeout(5*time.Second))
	if limits.MaxOutputBytes() != 1024 {
		t.Fatalf("MaxOutputBytes = %d", limits.MaxOutputBytes())
	}
	if limits.Timeout() != 5*time.Second {
		t.Fatalf("Timeout = %s", limits.Timeout())
	}
	if got := sandbox.New(sandbox.WithMaxOutputBytes(-1)).MaxOutputBytes(); got != sandbox.DefaultMaxOutputBytes {
		t.Fatalf("negative max output should keep the default, got %d", got)
	}
}

func TestNilPolicyFailsClosed(t *testing.T) {
	var policy *sandbox.Policy
	if _, err := policy.ResolvePath("x"); !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("nil ResolvePath error = %v", err)
	}
	if policy.ReadOnly() || policy.NetworkAllowed() {
		t.Fatal("nil policy should not report enabled features")
	}
	if policy.Timeout() != sandbox.DefaultTimeout || policy.MaxOutputBytes() != sandbox.DefaultMaxOutputBytes {
		t.Fatal("nil policy should report defaults")
	}
	if policy.Roots() != nil {
		t.Fatal("nil policy roots should be nil")
	}
	if env := policy.CommandEnv([]string{"PATH=/bin"}); env != nil {
		t.Fatalf("nil policy CommandEnv = %v", env)
	}
}

func TestDeniedPathExportedHelper(t *testing.T) {
	root := testRoot(t)
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithDeniedGlobs("**/.env"))
	if !policy.DeniedPath(filepath.Join(root, ".env")) {
		t.Fatal("DeniedPath(.env) = false")
	}
	if policy.DeniedPath(filepath.Join(root, "main.go")) {
		t.Fatal("DeniedPath(main.go) = true")
	}
	// A path outside the roots is "denied" too (invisible to tools).
	if !policy.DeniedPath(filepath.Join(testRoot(t), "x")) {
		t.Fatal("DeniedPath(outside) = false")
	}
}

func TestResolvePathErrorMessageMentionsReason(t *testing.T) {
	root := testRoot(t)
	policy := sandbox.New(sandbox.WithRoots(root))
	_, err := policy.ResolvePath("../escape")
	if err == nil || !strings.Contains(err.Error(), "parent-directory") {
		t.Fatalf("error = %v, want parent-directory reason", err)
	}
}
