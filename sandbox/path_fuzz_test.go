package sandbox_test

// Fuzz target for filesystem path resolution. ResolvePath is the single choke
// point every built-in file tool funnels through, so arbitrary model/user
// strings must never panic it and, critically, must never resolve outside the
// configured root. The target is deterministic and confined to t.TempDir.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// FuzzResolvePath resolves arbitrary path strings under a policy rooted in one
// temporary directory. Denials must wrap sandbox.ErrDenied; successes must be
// absolute, NUL-free, contain no "..", stay inside the root, and also pass the
// post-resolution CheckPath gate.
func FuzzResolvePath(f *testing.F) {
	root := f.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		f.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	realRoot = filepath.Clean(realRoot)
	if err := os.MkdirAll(filepath.Join(realRoot, "sub", "dir"), 0o755); err != nil {
		f.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realRoot, "sub", "dir", "file.txt"), []byte("x"), 0o644); err != nil {
		f.Fatalf("WriteFile: %v", err)
	}
	// A symlink escaping the root and one staying inside it. Both are
	// best-effort: on platforms without symlink support they are just absent.
	_ = os.Symlink(filepath.Dir(realRoot), filepath.Join(realRoot, "escape"))
	_ = os.Symlink(filepath.Join(realRoot, "sub", "dir"), filepath.Join(realRoot, "inside-link"))

	policy := sandbox.New(
		sandbox.WithRoots(realRoot),
		sandbox.WithDeniedGlobs("**/.env", "secrets/**"),
	)
	roots := policy.Roots()
	if len(roots) != 1 {
		f.Fatalf("policy roots = %v, want exactly the temporary root", roots)
	}
	realRoot = roots[0]

	seeds := []string{
		"",
		" ",
		".",
		"./",
		"sub",
		"sub/",
		"sub/dir",
		"sub/dir/file.txt",
		"sub//dir/file.txt",
		"sub/./dir/file.txt",
		"sub/../dir",
		"../sub",
		"..",
		"../../etc/passwd",
		"escape",
		"escape/etc/passwd",
		filepath.ToSlash(filepath.Join("escape", "etc")),
		"inside-link",
		"inside-link/file.txt",
		"secrets/key.txt",
		"sub/.env",
		".env",
		realRoot,
		filepath.Join(realRoot, "sub", "dir", "file.txt"),
		"/etc/passwd",
		"/",
		"//etc/passwd",
		filepath.Dir(realRoot),
		`C:\Windows\System32`,
		`sub\dir\file.txt`,
		"a\x00b",
		"\x00",
		"~/.ssh/id_rsa",
		strings.Repeat("a/", 64) + "b",
		"sub/dir/../../..",
		`\\server\share`,
		"/proc/self/root/etc/passwd",
		// Component names may legitimately contain spaces; this exits the
		// policy as a *rejection* of CheckPath's input trimming (see below).
		".. /",
		"sub/dir /file",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, requested string) {
		if len(requested) > 1<<16 {
			requested = requested[:1<<16]
		}
		resolved, err := policy.ResolvePath(requested)
		if err != nil {
			if !errors.Is(err, sandbox.ErrDenied) {
				t.Fatalf("ResolvePath(%.60q) returned a non-denial error: %v", requested, err)
			}
			return
		}
		if !filepath.IsAbs(resolved) {
			t.Fatalf("ResolvePath(%.60q) returned a relative path %q", requested, resolved)
		}
		if strings.ContainsRune(resolved, 0) {
			t.Fatalf("ResolvePath(%.60q) returned a NUL-containing path", requested)
		}
		if !fuzzPathWithin(realRoot, resolved) {
			t.Fatalf("ResolvePath(%.60q) escaped root %q: %q", requested, realRoot, resolved)
		}
		// The post-resolution gate must agree with the resolver's decision,
		// including for legal file names that end in whitespace.
		if err := policy.CheckPath(resolved); err != nil {
			t.Fatalf("CheckPath rejected a resolved path %q: %v", resolved, err)
		}
	})
}

// fuzzPathWithin reports whether path is root or a descendant of root. Both
// arguments are cleaned absolute paths; it is deliberately lexical (no
// filesystem access) so it cannot be fooled by a race on the filesystem.
func fuzzPathWithin(root, path string) bool {
	if path == root {
		return true
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
