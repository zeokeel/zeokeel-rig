package sandbox_test

// Tests for the sandbox command policy: allow/deny lists, workdir confinement
// and the environment whitelist.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestCheckCommandDefaultDeniesShell(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(testRoot(t)))
	err := policy.CheckCommand([]string{"echo", "hi"}, "")
	requireDenied(t, err, "default CheckCommand")
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("error = %v, want a disabled-shell message", err)
	}
	if err := sandbox.New().CheckCommand([]string{"echo"}, ""); !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("empty policy CheckCommand = %v", err)
	}
}

func TestCheckCommandAllowlist(t *testing.T) {
	root := testRoot(t)
	mustMkdirAll(t, root, "sub")
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist("echo", "env"),
	)

	allowed := [][]string{
		{"echo", "hello"},
		{"env"},
	}
	for _, argv := range allowed {
		if err := policy.CheckCommand(argv, ""); err != nil {
			t.Fatalf("CheckCommand(%v) denied: %v", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"ls"},
		{"/bin/ls", "-la"}, // path form is rejected before the allowlist
		{"./echo", "hi"},   // workspace-relative binary is rejected
		{"PATH=.", "echo"}, // leading env assignments are rejected
		{"LD_PRELOAD=/x.so", "env"},
		{"rm", "-rf", "/"},
		{},
		{" "},
	} {
		if err := policy.CheckCommand(argv, ""); err == nil {
			t.Fatalf("CheckCommand(%v) was allowed", argv)
		}
	}
	// Workdir inside the roots is accepted.
	if err := policy.CheckCommand([]string{"echo"}, "sub"); err != nil {
		t.Fatalf("CheckCommand with allowed workdir: %v", err)
	}
	if err := policy.CheckCommand([]string{"echo"}, root); err != nil {
		t.Fatalf("CheckCommand with absolute workdir: %v", err)
	}
}

func TestCheckCommandDenylistWins(t *testing.T) {
	policy := sandbox.New(
		sandbox.WithRoots(testRoot(t)),
		sandbox.WithCommandAllowlist("git", "echo"),
		sandbox.WithCommandDenylist("git"),
	)
	if err := policy.CheckCommand([]string{"git", "status"}, ""); err == nil {
		t.Fatal("deny-listed command was allowed")
	} else if !strings.Contains(err.Error(), "denied") {
		t.Fatalf("error = %v, want denied message", err)
	}
	if err := policy.CheckCommand([]string{"echo", "hi"}, ""); err != nil {
		t.Fatalf("echo denied: %v", err)
	}
}

func TestCheckCommandWorkdirConfinement(t *testing.T) {
	root := testRoot(t)
	outside := testRoot(t)
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("pwd"))
	for _, workdir := range []string{
		"..",
		"../outside",
		outside,
	} {
		if err := policy.CheckCommand([]string{"pwd"}, workdir); err == nil {
			t.Fatalf("CheckCommand(workdir=%q) was allowed", workdir)
		}
	}
	if err := policy.CheckCommand([]string{"pwd"}, "missing-but-resolvable"); err != nil {
		t.Fatalf("CheckCommand(missing workdir) = %v, want it accepted (existence is checked by the shell tool)", err)
	}
}

func TestCommandEnvDefaultMinimal(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(testRoot(t)))
	base := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/user",
		"LANG=C.UTF-8",
		"SECRET_TOKEN=super-secret",
		"AWS_ACCESS_KEY_ID=AKIA...",
		"RIG_CUSTOM=1",
	}
	env := policy.CommandEnv(base)
	want := []string{"PATH=/usr/bin:/bin", "HOME=/home/user", "LANG=C.UTF-8"}
	if len(env) != len(want) {
		t.Fatalf("CommandEnv = %v, want %v", env, want)
	}
	for index := range want {
		if env[index] != want[index] {
			t.Fatalf("CommandEnv = %v, want %v", env, want)
		}
	}
	for _, entry := range env {
		if strings.Contains(entry, "SECRET") || strings.Contains(entry, "AWS") || strings.Contains(entry, "CUSTOM") {
			t.Fatalf("CommandEnv leaked %q", entry)
		}
	}
}

func TestCommandEnvExplicitAllowlist(t *testing.T) {
	policy := sandbox.New(
		sandbox.WithRoots(testRoot(t)),
		sandbox.WithEnvAllowlist("RIG_CUSTOM", "PATH"),
	)
	env := policy.CommandEnv([]string{"PATH=/bin", "RIG_CUSTOM=1", "SECRET=2", "HOME=/h"})
	if len(env) != 2 || env[0] != "PATH=/bin" || env[1] != "RIG_CUSTOM=1" {
		t.Fatalf("CommandEnv = %v", env)
	}

	empty := sandbox.New(sandbox.WithRoots(testRoot(t)), sandbox.WithEnvAllowlist())
	if got := empty.CommandEnv([]string{"PATH=/bin"}); len(got) != 0 {
		t.Fatalf("explicit empty allowlist returned %v", got)
	}

	// Duplicate variables keep their first spelling only.
	dupes := sandbox.New(sandbox.WithRoots(testRoot(t))).CommandEnv([]string{"PATH=/bin", "PATH=/other"})
	if len(dupes) != 1 || dupes[0] != "PATH=/bin" {
		t.Fatalf("duplicate env handling = %v", dupes)
	}
}

func TestCommandEnvUsesProcessEnvironment(t *testing.T) {
	t.Setenv("RIG_SANDBOX_ENV_TEST", "visible")
	policy := sandbox.New(sandbox.WithRoots(testRoot(t)), sandbox.WithEnvAllowlist("RIG_SANDBOX_ENV_TEST"))
	env := policy.CommandEnv(os.Environ())
	found := false
	for _, entry := range env {
		if entry == "RIG_SANDBOX_ENV_TEST=visible" {
			found = true
		}
	}
	if !found {
		t.Fatalf("allowed variable missing from %v", env)
	}
}

func TestPolicyAccessorsDoNotLeakInternalSlices(t *testing.T) {
	root := testRoot(t)
	policy := sandbox.New(sandbox.WithRoots(root))
	roots := policy.Roots()
	if len(roots) != 1 {
		t.Fatalf("Roots() = %v", roots)
	}
	roots[0] = filepath.Join(root, "tampered")
	if policy.Roots()[0] != root {
		t.Fatal("Roots() returned an aliased slice")
	}
}
