package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/workspace"
)

// fileFixture is one workspace tree with the files the file-service tests use.
type fileFixture struct {
	service *workspace.Service
	root    string
	outside string
}

func newFileFixture(t *testing.T) *fileFixture {
	t.Helper()
	base := t.TempDir()
	root := makeDir(t, base, "workspace")
	makeDir(t, root, "Alpha")
	makeDir(t, root, "beta")
	makeDir(t, root, "sub", "deep")
	writeFile(t, filepath.Join(root, "Alpha", "inner.txt"), []byte("inner"))
	writeFile(t, filepath.Join(root, "a.txt"), []byte("a"))
	writeFile(t, filepath.Join(root, "B.txt"), []byte("b"))
	writeFile(t, filepath.Join(root, "c.md"), []byte("c"))
	writeFile(t, filepath.Join(root, ".hidden"), []byte("hidden"))
	writeFile(t, filepath.Join(root, "sub", "one.txt"), []byte("nested"))
	writeFile(t, filepath.Join(root, "sub", "deep", "two.go"), []byte("deep"))
	outside := makeDir(t, base, "outside")
	writeFile(t, filepath.Join(outside, "secret.txt"), []byte("secret"))
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "inside-link")); err != nil {
		t.Fatalf("symlink inside: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "dirlink")); err != nil {
		t.Fatalf("directory symlink inside: %v", err)
	}
	service := mustNew(t, t.TempDir(), workspace.Config{})
	mustCreate(t, service, "files", root)
	return &fileFixture{service: service, root: root, outside: outside}
}

func fileNames(entries []workspace.FileEntry) []string {
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name
	}
	return names
}

func filePaths(entries []workspace.FileEntry) []string {
	paths := make([]string, len(entries))
	for index, entry := range entries {
		paths[index] = entry.Path
	}
	return paths
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func TestFilesListingAndSandbox(t *testing.T) {
	fixture := newFileFixture(t)
	ctx := context.Background()

	entries, err := fixture.service.Files(ctx, "")
	if err != nil {
		t.Fatalf("Files(root): %v", err)
	}
	want := []string{"Alpha", "beta", "sub", ".hidden", "a.txt", "B.txt", "c.md", "dirlink", "escape", "inside-link"}
	if got := fileNames(entries); !equalStrings(got, want) {
		t.Fatalf("Files(root) = %v, want %v (dirs first, case-insensitive names)", got, want)
	}
	if !entries[0].Dir || !entries[1].Dir || !entries[2].Dir {
		t.Fatalf("first entries are not directories: %+v", entries[:3])
	}
	if entries[3].Dir {
		t.Fatalf("hidden file marked as a directory: %+v", entries[3])
	}
	for _, entry := range entries {
		if entry.Name == "Alpha" && entry.Path != "Alpha" {
			t.Fatalf("Alpha path = %q", entry.Path)
		}
		if entry.Name == "a.txt" && (entry.Dir || entry.Size != 1 || entry.ModTime.IsZero()) {
			t.Fatalf("a.txt entry = %+v", entry)
		}
	}

	nested, err := fixture.service.Files(ctx, "Alpha")
	if err != nil || len(nested) != 1 || nested[0].Path != "Alpha/inner.txt" {
		t.Fatalf("Files(Alpha) = %+v/%v", nested, err)
	}
	sub, err := fixture.service.Files(ctx, "sub")
	if err != nil {
		t.Fatalf("Files(sub): %v", err)
	}
	if got, want := filePaths(sub), []string{"sub/deep", "sub/one.txt"}; !equalStrings(got, want) {
		t.Fatalf("Files(sub) paths = %v, want %v", got, want)
	}
	// A symlink to a directory inside the root is followed after re-checking.
	linked, err := fixture.service.Files(ctx, "dirlink")
	if err != nil {
		t.Fatalf("Files(dirlink): %v", err)
	}
	if got, want := filePaths(linked), []string{"sub/deep", "sub/one.txt"}; !equalStrings(got, want) {
		t.Fatalf("Files(dirlink) paths = %v, want %v", got, want)
	}

	if _, err := fixture.service.Files(ctx, "missing"); err == nil {
		t.Fatal("Files(missing) returned nil error")
	}
	if _, err := fixture.service.Files(ctx, "a.txt"); !errors.Is(err, workspace.ErrNotDirectory) {
		t.Fatalf("Files(file) = %v, want ErrNotDirectory", err)
	}
	for _, escape := range []string{"..", "../", "../outside", "/etc", "escape", "escape/secret.txt"} {
		if _, err := fixture.service.Files(ctx, escape); !errors.Is(err, workspace.ErrPathOutsideRoot) {
			t.Fatalf("Files(%q) = %v, want ErrPathOutsideRoot", escape, err)
		}
	}
	// A nil context is tolerated.
	if _, err := fixture.service.Files(nil, ""); err != nil {
		t.Fatalf("Files(nil ctx) = %v", err)
	}
}

func TestFilesSortingTies(t *testing.T) {
	root := makeDir(t, t.TempDir(), "root")
	makeDir(t, root, "Dir")
	makeDir(t, root, "dir")
	for _, name := range []string{"Apple.txt", "apple.txt", "Zeta.txt", "zeta.txt"} {
		writeFile(t, filepath.Join(root, name), []byte("x"))
	}
	service := mustNew(t, t.TempDir(), workspace.Config{})
	mustCreate(t, service, "sort", root)

	entries, err := service.Files(context.Background(), "")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	want := []string{"Dir", "dir", "Apple.txt", "apple.txt", "Zeta.txt", "zeta.txt"}
	if got := fileNames(entries); !equalStrings(got, want) {
		t.Fatalf("Files names = %v, want %v", got, want)
	}
}

func TestReadFile(t *testing.T) {
	base := t.TempDir()
	root := makeDir(t, base, "workspace")
	writeFile(t, filepath.Join(root, "hello.txt"), []byte("hello world"))
	makeDir(t, root, "dir")
	writeFile(t, filepath.Join(root, "dir", "ignored"), []byte("x"))
	binary := []byte{0x00, 0x01, 0xff, 0x10}
	writeFile(t, filepath.Join(root, "blob.bin"), binary)
	outside := makeDir(t, base, "outside")
	writeFile(t, filepath.Join(outside, "secret.txt"), []byte("secret"))
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "hello.txt"), filepath.Join(root, "inside-link")); err != nil {
		t.Fatalf("symlink inside: %v", err)
	}

	service := mustNew(t, t.TempDir(), workspace.Config{ReadLimitBytes: 4})
	mustCreate(t, service, "read", root)
	ctx := context.Background()

	data, err := service.ReadFile(ctx, "hello.txt", 0)
	if err != nil || string(data) != "hell" {
		t.Fatalf("ReadFile(default limit) = %q/%v, want the configured 4-byte default", data, err)
	}
	data, err = service.ReadFile(ctx, "hello.txt", 100)
	if err != nil || string(data) != "hello world" {
		t.Fatalf("ReadFile(100) = %q/%v", data, err)
	}
	data, err = service.ReadFile(ctx, "inside-link", 0)
	if err != nil || string(data) != "hell" {
		t.Fatalf("ReadFile(inside symlink) = %q/%v", data, err)
	}
	data, err = service.ReadFile(ctx, "blob.bin", 4)
	if err != nil || string(data) != string(binary) {
		t.Fatalf("ReadFile(binary) = %x/%v", data, err)
	}
	// A relative path that cleans to a path inside the root is allowed.
	data, err = service.ReadFile(ctx, "dir/../hello.txt", 100)
	if err != nil || string(data) != "hello world" {
		t.Fatalf("ReadFile(dir/../hello.txt) = %q/%v", data, err)
	}
	if _, err := service.ReadFile(ctx, "hello.txt", -1); !errors.Is(err, workspace.ErrInvalidLimit) {
		t.Fatalf("ReadFile(-1) = %v, want ErrInvalidLimit", err)
	}
	if _, err := service.ReadFile(ctx, "", 0); err == nil {
		t.Fatal("ReadFile(empty) returned nil error")
	}
	if _, err := service.ReadFile(ctx, "bad\x00name", 0); err == nil {
		t.Fatal("ReadFile(NUL path) returned nil error")
	}
	if _, err := service.ReadFile(ctx, "missing.txt", 0); err == nil {
		t.Fatal("ReadFile(missing) returned nil error")
	}
	if _, err := service.ReadFile(ctx, "dir", 0); !errors.Is(err, workspace.ErrNotFile) {
		t.Fatalf("ReadFile(directory) = %v, want ErrNotFile", err)
	}
	for _, escape := range []string{"../", "/etc/passwd", "escape/secret.txt", "escape"} {
		if _, err := service.ReadFile(ctx, escape, 0); !errors.Is(err, workspace.ErrPathOutsideRoot) {
			t.Fatalf("ReadFile(%q) = %v, want ErrPathOutsideRoot", escape, err)
		}
	}
	data, err = service.ReadFile(nil, "hello.txt", 100)
	if err != nil || string(data) != "hello world" {
		t.Fatalf("ReadFile(nil ctx) = %q/%v", data, err)
	}
}

func TestReadFileBoundedDefault(t *testing.T) {
	root := makeDir(t, t.TempDir(), "root")
	content := []byte(strings.Repeat("x", workspace.DefaultReadLimitBytes+100))
	writeFile(t, filepath.Join(root, "big.txt"), content)
	service := mustNew(t, t.TempDir(), workspace.Config{})
	mustCreate(t, service, "big", root)
	ctx := context.Background()

	data, err := service.ReadFile(ctx, "big.txt", 0)
	if err != nil || len(data) != workspace.DefaultReadLimitBytes {
		t.Fatalf("ReadFile(default) len = %d/%v, want %d", len(data), err, workspace.DefaultReadLimitBytes)
	}
	data, err = service.ReadFile(ctx, "big.txt", 10)
	if err != nil || len(data) != 10 {
		t.Fatalf("ReadFile(10) len = %d/%v, want 10", len(data), err)
	}
	data, err = service.ReadFile(ctx, "big.txt", len(content))
	if err != nil || len(data) != len(content) {
		t.Fatalf("ReadFile(full) len = %d/%v, want %d", len(data), err, len(content))
	}
}

func TestFileServiceUsesCurrentRootAndReload(t *testing.T) {
	base := t.TempDir()
	rootA := makeDir(t, base, "a")
	rootB := makeDir(t, base, "b")
	writeFile(t, filepath.Join(rootA, "a.txt"), []byte("A"))
	writeFile(t, filepath.Join(rootB, "b.txt"), []byte("B"))
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{})
	ctx := context.Background()

	first := mustCreate(t, service, "a", rootA)
	second := mustCreate(t, service, "b", rootB)

	if data, err := service.ReadFile(ctx, "b.txt", 0); err != nil || string(data) != "B" {
		t.Fatalf("ReadFile(b.txt) = %q/%v, want the current root", data, err)
	}
	if _, err := service.ReadFile(ctx, "a.txt", 0); err == nil {
		t.Fatal("ReadFile(a.txt) succeeded while the other root is current")
	}
	if _, err := service.Open(ctx, first.ID); err != nil {
		t.Fatalf("Open(first): %v", err)
	}
	if data, err := service.ReadFile(ctx, "a.txt", 0); err != nil || string(data) != "A" {
		t.Fatalf("ReadFile(a.txt) after Open = %q/%v", data, err)
	}
	entries, err := service.Files(ctx, "")
	if err != nil || len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("Files after Open = %+v/%v", entries, err)
	}

	restarted := mustNew(t, dataDir, workspace.Config{})
	assertCurrent(t, restarted, first.ID)
	if data, err := restarted.ReadFile(ctx, "a.txt", 0); err != nil || string(data) != "A" {
		t.Fatalf("restarted ReadFile(a.txt) = %q/%v", data, err)
	}
	if second.ID == first.ID {
		t.Fatal("two separate roots got the same workspace ID")
	}
}

func TestFileServiceWithoutWorkspace(t *testing.T) {
	service := mustNew(t, t.TempDir(), workspace.Config{})
	ctx := context.Background()
	if _, err := service.Files(ctx, ""); !errors.Is(err, workspace.ErrNoWorkspace) {
		t.Fatalf("Files without a workspace = %v, want ErrNoWorkspace", err)
	}
	if _, err := service.ReadFile(ctx, "a.txt", 0); !errors.Is(err, workspace.ErrNoWorkspace) {
		t.Fatalf("ReadFile without a workspace = %v, want ErrNoWorkspace", err)
	}
	if _, err := service.Search(ctx, "a", 0); !errors.Is(err, workspace.ErrNoWorkspace) {
		t.Fatalf("Search without a workspace = %v, want ErrNoWorkspace", err)
	}
}

func TestFileServiceRootRemoved(t *testing.T) {
	root := makeDir(t, t.TempDir(), "root")
	service := mustNew(t, t.TempDir(), workspace.Config{})
	mustCreate(t, service, "gone", root)
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll(root): %v", err)
	}
	ctx := context.Background()
	if _, err := service.Files(ctx, ""); err == nil {
		t.Fatal("Files with a missing root returned nil error")
	}
	if _, err := service.ReadFile(ctx, "a.txt", 0); err == nil {
		t.Fatal("ReadFile with a missing root returned nil error")
	}
	if _, err := service.Search(ctx, "a", 0); err == nil {
		t.Fatal("Search with a missing root returned nil error")
	}
}

func TestSearch(t *testing.T) {
	base := t.TempDir()
	root := makeDir(t, base, "workspace")
	makeDir(t, root, "sub", "deep", "deeper")
	makeDir(t, root, ".hidden")
	writeFile(t, filepath.Join(root, "Needle.txt"), []byte("1"))
	writeFile(t, filepath.Join(root, "sub", "needle-two.md"), []byte("2"))
	writeFile(t, filepath.Join(root, "sub", "deep", "needle-three.go"), []byte("3"))
	writeFile(t, filepath.Join(root, "sub", "deep", "deeper", "needle-four.txt"), []byte("4"))
	writeFile(t, filepath.Join(root, ".hidden", "needle-hidden.txt"), []byte("5"))
	writeFile(t, filepath.Join(root, "other.txt"), []byte("6"))
	if err := os.Symlink(makeDir(t, base, "outside"), filepath.Join(root, "sub", "outside")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	service := mustNew(t, t.TempDir(), workspace.Config{SearchDepth: 1})
	mustCreate(t, service, "search", root)
	ctx := context.Background()

	results, err := service.Search(ctx, "NEEDLE", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := []string{"Needle.txt", "sub/needle-two.md"}; !equalStrings(filePaths(results), want) {
		t.Fatalf("Search(depth 1) = %v, want %v (deep and hidden skipped)", filePaths(results), want)
	}
	if entries, err := service.Search(ctx, "needle", 1); err != nil || len(entries) != 1 {
		t.Fatalf("Search(limit 1) = %d/%v", len(entries), err)
	}
	// A huge limit is a cap, not an allocation size.
	if entries, err := service.Search(ctx, "other", 1<<30); err != nil || len(entries) != 1 {
		t.Fatalf("Search(huge limit) = %d/%v, want the single match", len(entries), err)
	}
	if entries, err := service.Search(ctx, "   ", 0); err != nil || len(entries) != 0 {
		t.Fatalf("Search(empty query) = %d/%v, want empty", len(entries), err)
	}
	if _, err := service.Search(ctx, "needle", -1); !errors.Is(err, workspace.ErrInvalidLimit) {
		t.Fatalf("Search(-1) = %v, want ErrInvalidLimit", err)
	}
	if entries, err := service.Search(ctx, "other", 0); err != nil || len(entries) != 1 || entries[0].Path != "other.txt" {
		t.Fatalf("Search(other) = %+v/%v", entries, err)
	}
	// Hidden directories are skipped and directories are never results.
	if entries, err := service.Search(ctx, "hidden", 0); err != nil || len(entries) != 0 {
		t.Fatalf("Search(hidden) = %+v/%v, want no results", entries, err)
	}
	if entries, err := service.Search(ctx, "sub", 0); err != nil || len(entries) != 0 {
		t.Fatalf("Search(directory name) = %+v/%v, want no results", entries, err)
	}
	if entries, err := service.Search(nil, "other", 0); err != nil || len(entries) != 1 {
		t.Fatalf("Search(nil ctx) = %+v/%v", entries, err)
	}
}

func TestSearchDefaultDepth(t *testing.T) {
	base := t.TempDir()
	root := makeDir(t, base, "workspace")
	makeDir(t, root, "sub", "deep", "deeper")
	makeDir(t, root, ".hidden")
	writeFile(t, filepath.Join(root, "Needle.txt"), []byte("1"))
	writeFile(t, filepath.Join(root, "sub", "needle-two.md"), []byte("2"))
	writeFile(t, filepath.Join(root, "sub", "deep", "needle-three.go"), []byte("3"))
	writeFile(t, filepath.Join(root, "sub", "deep", "deeper", "needle-four.txt"), []byte("4"))
	writeFile(t, filepath.Join(root, ".hidden", "needle-hidden.txt"), []byte("5"))

	service := mustNew(t, t.TempDir(), workspace.Config{})
	mustCreate(t, service, "search", root)
	results, err := service.Search(context.Background(), "needle", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []string{
		"Needle.txt",
		"sub/deep/deeper/needle-four.txt",
		"sub/deep/needle-three.go",
		"sub/needle-two.md",
	}
	if got := filePaths(results); !equalStrings(got, want) {
		t.Fatalf("Search(default depth) = %v, want %v", got, want)
	}
}

func TestFileContextCancellation(t *testing.T) {
	root := makeDir(t, t.TempDir(), "root")
	writeFile(t, filepath.Join(root, "a.txt"), []byte("a"))
	service := mustNew(t, t.TempDir(), workspace.Config{})
	mustCreate(t, service, "ctx", root)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := service.Files(cancelled, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("Files(cancelled) = %v, want context.Canceled", err)
	}
	if _, err := service.ReadFile(cancelled, "a.txt", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadFile(cancelled) = %v, want context.Canceled", err)
	}
	if _, err := service.Search(cancelled, "a", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search(cancelled) = %v, want context.Canceled", err)
	}
}
