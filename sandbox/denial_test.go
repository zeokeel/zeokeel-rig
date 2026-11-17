package sandbox_test

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func testPolicy(t *testing.T) *sandbox.Policy {
	t.Helper()
	return sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
}

func TestDetectDenialNoMatch(t *testing.T) {
	policy := testPolicy(t)
	if denial := sandbox.DetectDenial("hello world", []string{"permission denied"}, policy, sandbox.EnforcementFull); denial != nil {
		t.Fatalf("Denial = %+v, want nil", denial)
	}
	if denial := sandbox.DetectDenial("permission denied", nil, policy, sandbox.EnforcementFull); denial != nil {
		t.Fatalf("Denial without signatures = %+v, want nil", denial)
	}
}

func TestDetectDenialExtractsPathAndOp(t *testing.T) {
	policy := testPolicy(t)
	cases := []struct {
		name   string
		output string
		want   string
		wantOp string
	}{
		{
			name:   "sh cannot create",
			output: "/bin/sh: 1: cannot create /home/me/.tool/creds.json: Read-only file system",
			want:   "/home/me/.tool/creds.json",
			wantOp: "create",
		},
		{
			name:   "mkdir quoted",
			output: "mkdir: cannot create directory '/home/me/.tool': Permission denied",
			want:   "/home/me/.tool",
			wantOp: "create",
		},
		{
			name:   "bare path reason",
			output: "/home/me/.tool/config.json: Permission denied",
			want:   "/home/me/.tool/config.json",
			wantOp: "unknown",
		},
		{
			name:   "rename",
			output: "mv: cannot move '/repo/x' to '/home/me/x': Read-only file system",
			want:   "/repo/x",
			wantOp: "rename",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			denial := sandbox.DetectDenial(tc.output, []string{"read-only file system", "permission denied"}, policy, sandbox.EnforcementFull)
			if denial == nil {
				t.Fatalf("Denial = nil, want non-nil for %q", tc.output)
			}
			if denial.Path != tc.want {
				t.Fatalf("Path = %q, want %q", denial.Path, tc.want)
			}
			if denial.Op != tc.wantOp {
				t.Fatalf("Op = %q, want %q", denial.Op, tc.wantOp)
			}
		})
	}
}

func TestDetectDenialIgnoresRelativePaths(t *testing.T) {
	policy := testPolicy(t)
	denial := sandbox.DetectDenial("./build: Permission denied", []string{"permission denied"}, policy, sandbox.EnforcementFull)
	if denial == nil {
		t.Fatal("Denial = nil, want non-nil")
	}
	if denial.Path != "" {
		t.Fatalf("Path = %q, want empty for relative path", denial.Path)
	}
}

func TestDenialNoteExplainsNextSteps(t *testing.T) {
	policy := testPolicy(t)
	denial := sandbox.DetectDenial(
		"cannot create /home/me/.tool/creds.json: Read-only file system",
		[]string{"read-only file system"},
		policy,
		sandbox.EnforcementPartial,
	)
	if denial == nil {
		t.Fatal("Denial = nil")
	}
	note := denial.Note()
	for _, want := range []string{
		"sandbox: denied create to /home/me/.tool/creds.json",
		"workspace-write",
		"partial",
		"additional writable roots",
		"redirecting the tool's own state",
		"request a writable root",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("Note() missing %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "dws") || strings.Contains(note, ".aws") {
		t.Fatal("Note() must not hardcode vendor-specific directories")
	}
}

func TestDenialSummaryWithoutPath(t *testing.T) {
	policy := testPolicy(t)
	denial := sandbox.DetectDenial("permission denied", []string{"permission denied"}, policy, sandbox.EnforcementFull)
	if denial == nil {
		t.Fatal("Denial = nil")
	}
	summary := denial.Summary()
	if !strings.Contains(summary, "a path outside the writable roots") {
		t.Fatalf("Summary() = %q, want the generic placeholder", summary)
	}
}

func TestDenialBudgetReflectsGrants(t *testing.T) {
	policy := testPolicyWithBudget(t, 2)
	denial := sandbox.DetectDenial("permission denied", []string{"permission denied"}, policy, sandbox.EnforcementFull)
	if denial == nil || denial.Budget != 2 {
		t.Fatalf("Budget = %v, want 2", denial)
	}
}

func testPolicyWithBudget(t *testing.T, budget int) *sandbox.Policy {
	t.Helper()
	return sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithGrantBudget(budget),
	)
}
