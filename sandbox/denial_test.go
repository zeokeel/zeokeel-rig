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

func TestDetectDenialWithExitSuppressesQuickRejectCodes(t *testing.T) {
	policy := testPolicy(t)
	// 126/127/2 表示命令没跑起来（不可执行/找不到/shell 用法错误），输出里的
	// "permission denied" 来自 shell，不是沙箱。
	for _, code := range []int{2, 126, 127} {
		if denial := sandbox.DetectDenialWithExit("permission denied", code, []string{"permission denied"}, policy, sandbox.EnforcementFull); denial != nil {
			t.Fatalf("exit %d: Denial = %+v, want nil (quick-reject)", code, denial)
		}
	}
}

func TestDetectDenialWithExitAttributesNormalFailures(t *testing.T) {
	policy := testPolicy(t)
	denial := sandbox.DetectDenialWithExit("cannot create /home/me/.tool/x: Read-only file system", 1, []string{"read-only file system"}, policy, sandbox.EnforcementFull)
	if denial == nil {
		t.Fatal("Denial = nil, want non-nil")
	}
	if denial.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", denial.ExitCode)
	}
	if !strings.Contains(denial.Summary(), "exit 1") {
		t.Fatalf("Summary = %q, want it to mention the exit code", denial.Summary())
	}
}

func TestDetectDenialWithExitAttributesSIGSYSWithoutOutput(t *testing.T) {
	policy := testPolicy(t)
	// 被杀于 SIGSYS（128+31）时没有任何文案：seccomp 拒绝只能靠信号识别。
	denial := sandbox.DetectDenialWithExit("", 159, nil, policy, sandbox.EnforcementFull)
	if denial == nil {
		t.Fatal("Denial = nil, want a SIGSYS attribution")
	}
	if denial.Signal != "SIGSYS" {
		t.Fatalf("Signal = %q, want SIGSYS", denial.Signal)
	}
	if !strings.Contains(denial.Summary(), "signal SIGSYS") {
		t.Fatalf("Summary = %q, want the signal", denial.Summary())
	}
	if !strings.Contains(denial.Note(), "workspace roots: ") {
		t.Fatalf("Note() missing the writable surface:\n%s", denial.Note())
	}
}

func TestDetectDenialWithExitUnknownExitKeepsTextBehaviour(t *testing.T) {
	policy := testPolicy(t)
	denial := sandbox.DetectDenialWithExit("permission denied", -1, []string{"permission denied"}, policy, sandbox.EnforcementPartial)
	if denial == nil {
		t.Fatal("exit -1 must behave like DetectDenial (text-driven)")
	}
	if denial.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", denial.ExitCode)
	}
}

func TestPolicyDenialSinkReceivesDetections(t *testing.T) {
	policy := testPolicy(t)
	var got []sandbox.Denial
	policy.SetDenialSink(func(d sandbox.Denial) { got = append(got, d) })

	denial := sandbox.DetectDenialWithExit("cannot create /home/me/.tool/x: Read-only file system", 1,
		[]string{"read-only file system"}, policy, sandbox.EnforcementFull)
	if denial == nil {
		t.Fatal("Denial = nil")
	}
	policy.ReportDenial(*denial)

	if len(got) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(got))
	}
	if got[0].Path != "/home/me/.tool/x" || got[0].ExitCode != 1 || got[0].Mode != sandbox.ModeWorkspaceWrite {
		t.Fatalf("sink denial = %+v, want path/exit/mode recorded", got[0])
	}

	// 替换钩子后旧钩子不再被调用；nil 钩子安全跳过。
	calls := 0
	policy.SetDenialSink(func(sandbox.Denial) { calls++ })
	policy.ReportDenial(*denial)
	if len(got) != 1 {
		t.Fatal("previous sink must not be called after replacement")
	}
	if calls != 1 {
		t.Fatalf("replacement sink calls = %d, want 1", calls)
	}
	policy.SetDenialSink(nil)
	policy.ReportDenial(*denial)
	if calls != 1 {
		t.Fatal("nil sink must be a no-op")
	}
}

func TestNilPolicyDenialSinkIsSafe(t *testing.T) {
	var policy *sandbox.Policy
	policy.SetDenialSink(func(sandbox.Denial) { t.Fatal("must not be called") })
	policy.ReportDenial(sandbox.Denial{})
}
