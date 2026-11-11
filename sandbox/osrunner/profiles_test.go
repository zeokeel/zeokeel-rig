package osrunner

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// workspacePolicy 构造一个 workspace-write 策略，根为 root。
func workspacePolicy(t *testing.T, root string) *sandbox.Policy {
	t.Helper()
	return sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
}

// contains 报告 args 是否包含 want 子串。
func contains(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// indexOf 返回 want 在 args 中的下标，不存在返回 -1。
func indexOf(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}

func TestWritableRootsReadOnlyIsEmpty(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeReadOnly))
	if roots := WritableRoots(policy); len(roots) != 0 {
		t.Fatalf("WritableRoots(read-only) = %v, want empty", roots)
	}
}

func TestWritableRootsWorkspaceIncludesRootAndTemp(t *testing.T) {
	root := t.TempDir()
	roots := WritableRoots(workspacePolicy(t, root))
	if len(roots) == 0 {
		t.Fatal("WritableRoots(workspace) is empty")
	}
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		wantRoot = root
	}
	found := false
	for _, candidate := range roots {
		if candidate == wantRoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("WritableRoots = %v, want it to contain %q", roots, wantRoot)
	}
}

func TestBwrapArgsReadOnlyHasNoWritableBind(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeReadOnly))
	args := BwrapArgs(policy)
	for _, want := range []string{"--ro-bind", "/", "--dev", "/dev", "--unshare-pid", "--proc", "/proc", "--die-with-parent"} {
		if !contains(args, want) {
			t.Fatalf("BwrapArgs(read-only) = %v, missing %q", args, want)
		}
	}
	if contains(args, "--tmpfs") {
		t.Fatalf("BwrapArgs(read-only) = %v, must not grant a temp mount", args)
	}
	if contains(args, "--bind") {
		t.Fatalf("BwrapArgs(read-only) = %v, must not grant a workspace bind", args)
	}
}

func TestBwrapArgsWorkspaceAddsTempAndBind(t *testing.T) {
	root := t.TempDir()
	args := BwrapArgs(workspacePolicy(t, root))
	tmpfs := indexOf(args, "--tmpfs")
	if tmpfs < 0 || tmpfs+1 >= len(args) || args[tmpfs+1] != "/tmp" {
		t.Fatalf("BwrapArgs(workspace) = %v, want --tmpfs /tmp", args)
	}
	bind := indexOf(args, "--bind")
	if bind < 0 || bind+2 >= len(args) {
		t.Fatalf("BwrapArgs(workspace) = %v, want a workspace bind", args)
	}
	if args[bind+1] != args[bind+2] {
		t.Fatalf("BwrapArgs(workspace) = %v, bind source/destination differ", args)
	}
}

func TestLandlockGrants(t *testing.T) {
	readOnly, readWrite := LandlockGrants(sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeReadOnly)))
	if !contains(readOnly, "/") {
		t.Fatalf("read-only grants = %v, want /", readOnly)
	}
	if !contains(readWrite, "/dev/null") {
		t.Fatalf("read-write grants = %v, want /dev/null", readWrite)
	}
	if len(readWrite) != 1 {
		t.Fatalf("read-only read-write grants = %v, want only /dev/null", readWrite)
	}

	root := t.TempDir()
	_, readWrite = LandlockGrants(workspacePolicy(t, root))
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		wantRoot = root
	}
	if !contains(readWrite, wantRoot) {
		t.Fatalf("workspace read-write grants = %v, want %q", readWrite, wantRoot)
	}
	if !contains(readWrite, osTempDir(t)) {
		t.Fatalf("workspace read-write grants = %v, want temp dir", readWrite)
	}
}

func osTempDir(t *testing.T) string {
	t.Helper()
	return canonicalUnique([]string{platformTempRoots()[0]})[0]
}

func TestSeatbeltProfileDeniesWrites(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeReadOnly))
	profile := SeatbeltProfile(policy)
	for _, want := range []string{"(version 1)", "(allow default)", "(deny file-write*)", `(literal "/dev/null")`} {
		if !strings.Contains(profile, want) {
			t.Fatalf("SeatbeltProfile = %q, missing %q", profile, want)
		}
	}
	if strings.Contains(profile, "(subpath ") {
		t.Fatalf("SeatbeltProfile(read-only) = %q, must not grant a writable subpath", profile)
	}
}

func TestSeatbeltProfileWorkspaceGrantsRoot(t *testing.T) {
	root := t.TempDir()
	profile := SeatbeltProfile(workspacePolicy(t, root))
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		wantRoot = root
	}
	if !strings.Contains(profile, "(subpath "+sbplString(wantRoot)+")") {
		t.Fatalf("SeatbeltProfile = %q, want subpath %q", profile, wantRoot)
	}
}

func TestSBPLStringEscapes(t *testing.T) {
	if got, want := sbplString(`/a"b\c`), `"/a\"b\\c"`; got != want {
		t.Fatalf("sbplString = %q, want %q", got, want)
	}
}

func TestDenialSignatures(t *testing.T) {
	for _, runner := range []string{RunnerBwrap, RunnerLandlock, RunnerSeatbelt, RunnerWindowsACL} {
		if signatures := DenialSignatures(runner); len(signatures) == 0 {
			t.Fatalf("DenialSignatures(%q) is empty", runner)
		}
	}
	if signatures := DenialSignatures("unknown"); signatures != nil {
		t.Fatalf("DenialSignatures(unknown) = %v, want nil", signatures)
	}
}

func TestCustomRunnerWrapsWithBwrapProfile(t *testing.T) {
	runner, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"myns: failed"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi"}, workspacePolicy(t, t.TempDir()))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := confined.Argv[0], "myns"; got != want {
		t.Fatalf("Argv[0] = %q, want %q", got, want)
	}
	if !contains(confined.Argv, "--ro-bind") {
		t.Fatalf("Argv = %v, want bwrap profile args", confined.Argv)
	}
	if sep := indexOf(confined.Argv, "--"); sep < 0 || sep+1 >= len(confined.Argv) || confined.Argv[sep+1] != "/bin/sh" {
		t.Fatalf("Argv = %v, want -- /bin/sh ...", confined.Argv)
	}
	if confined.Enforcement != sandbox.EnforcementFull {
		t.Fatalf("Enforcement = %q, want full", confined.Enforcement)
	}
	if len(confined.DenialSignatures) != 1 || confined.DenialSignatures[0] != "read-only file system" {
		t.Fatalf("DenialSignatures = %v", confined.DenialSignatures)
	}
}

func TestCustomRunnerDangerFullAccessPassesThrough(t *testing.T) {
	runner, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"myns: failed"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	policy := sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))
	confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := strings.Join(confined.Argv, " "), "/bin/sh -c echo hi"; got != want {
		t.Fatalf("Argv = %q, want %q", got, want)
	}
}

func TestCustomRunnerRequiresSignatures(t *testing.T) {
	if _, err := New(Config{RunnerCommand: []string{"myns"}}); err == nil {
		t.Fatal("New accepted RunnerCommand without RunnerFailureSignatures")
	}
	if _, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"bad\nline"}}); err == nil {
		t.Fatal("New accepted a multi-line failure signature")
	}
}

func TestCustomRunnerNilPolicyFailsClosed(t *testing.T) {
	runner, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"myns: failed"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runner.Confine([]string{"/bin/sh"}, nil); err == nil {
		t.Fatal("Confine(nil policy) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("Confine(nil policy) error = %v, want ErrSandboxUnavailable", err)
	}
}

func TestCanonicalUniqueDropsMissingAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	got := canonicalUnique([]string{dir, dir, filepath.Join(dir, "missing")})
	if len(got) != 1 {
		t.Fatalf("canonicalUnique = %v, want one existing path", got)
	}
}

func TestPlatformTempRootsIncludeOSTemp(t *testing.T) {
	roots := platformTempRoots()
	if len(roots) == 0 {
		t.Fatal("platformTempRoots is empty")
	}
	if runtime.GOOS != "windows" && !contains(roots, "/tmp") {
		t.Fatalf("platformTempRoots = %v, want /tmp", roots)
	}
}
