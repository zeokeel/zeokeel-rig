package sandbox_test

// Benchmarks for the filesystem path policy: resolving a requested path (which
// resolves the longest existing prefix with EvalSymlinks) and the cheaper
// post-resolution CheckPath. The setup uses a real temporary directory so the
// measured work matches production, but no network is involved and the fixture
// is deterministic.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// benchSink keeps measured values observable so the compiler cannot eliminate
// the benchmarked work.
var benchSink any

// benchPathFixture builds a normalized policy rooted at a temporary directory
// containing a small source tree.
func benchPathFixture(b *testing.B) (policy *sandbox.Policy, relative, absolute string) {
	b.Helper()
	root := b.TempDir()
	nested := filepath.Join(root, "src", "pkg", "internal")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		b.Fatalf("MkdirAll: %v", err)
	}
	file := filepath.Join(nested, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		b.Fatalf("WriteFile: %v", err)
	}
	policy = sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithDeniedGlobs("**/.env", "secrets/**"),
	)
	roots := policy.Roots()
	if len(roots) != 1 {
		b.Fatalf("policy roots = %v, want exactly the temporary root", roots)
	}
	return policy, filepath.Join("src", "pkg", "internal", "main.go"), filepath.Join(roots[0], "src", "pkg", "internal", "main.go")
}

// BenchmarkResolvePathRelative measures ResolvePath on a relative request, the
// form model-generated tool arguments usually take.
func BenchmarkResolvePathRelative(b *testing.B) {
	policy, relative, _ := benchPathFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resolved, err := policy.ResolvePath(relative)
		if err != nil {
			b.Fatalf("ResolvePath: %v", err)
		}
		benchSink = resolved
	}
}

// BenchmarkResolvePathAbsolute measures ResolvePath on an absolute request
// inside the root.
func BenchmarkResolvePathAbsolute(b *testing.B) {
	policy, _, absolute := benchPathFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resolved, err := policy.ResolvePath(absolute)
		if err != nil {
			b.Fatalf("ResolvePath: %v", err)
		}
		benchSink = resolved
	}
}

// BenchmarkCheckPath measures the post-resolution containment/glob check tools
// run on paths they computed themselves (glob and directory-walk results).
func BenchmarkCheckPath(b *testing.B) {
	policy, _, absolute := benchPathFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := policy.CheckPath(absolute); err != nil {
			b.Fatalf("CheckPath: %v", err)
		}
	}
}

// BenchmarkCheckPathDenied measures the denied-glob path, which walks the
// configured patterns.
func BenchmarkCheckPathDenied(b *testing.B) {
	policy, _, absolute := benchPathFixture(b)
	denied := filepath.Join(filepath.Dir(absolute), ".env")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := policy.CheckPath(denied); err == nil {
			b.Fatal("CheckPath allowed a denied path")
		}
	}
}
