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
	// 建议按"组合里实际可用的手段"生成：这个组合同时有授权工具与提权通道。
	denial.Options = sandbox.Options{HasGrantTool: true, HasEscalation: true}
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

// TestDetectDenialClassifiesNonPathResources 是"非路径资源"分类的表驱动用例。
//
// 真实背景：Windows 上 agent 让 CLI 登录时，CLI 要写 HKCU 注册表（DPAPI 存凭据）
// → 注册表不是路径，申请可写目录救不了；模型只能说"注册表写入被拒绝"，然后去试
// DWS_DISABLE_KEYCHAIN=1 这类偏方。这里钉住两件事：**识别**出来，并给出正确顺序
// 的处置建议（先提权，而不是先申请目录）。
func TestDetectDenialClassifiesNonPathResources(t *testing.T) {
	policy := testPolicy(t)
	windowsSignatures := []string{"access is denied", "拒绝访问"}
	posixSignatures := []string{"permission denied", "read-only file system"}
	cases := []struct {
		name         string
		output       string
		signatures   []string
		wantKind     sandbox.DenialKind
		wantResource string // 子串匹配；空表示不校验
		wantNoPath   bool
	}{
		// --- 注册表 ---------------------------------------------------------
		{
			name:       "HKEY root denied",
			output:     "ERROR: Access is denied.\nHKEY_CURRENT_USER\\Software\\Contoso",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialRegistry,
		},
		{
			name:         "HKCU abbreviation with key",
			output:       "Could not open key HKCU\\Software\\RigSmoke: Access is denied.",
			signatures:   windowsSignatures,
			wantKind:     sandbox.DenialRegistry,
			wantResource: "HKCU\\Software\\RigSmoke",
		},
		{
			name:       "reg add denied",
			output:     "reg add HKCU\\Software\\RigSmoke /v t /d 1 /f\r\nAccess is denied.\r\n",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialRegistry,
		},
		{
			name:       "reg.exe add denied",
			output:     "reg.exe add \"HKCU\\Software\\Contoso\" /v Token /t REG_SZ /d x /f\nERROR: Access is denied.",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialRegistry,
		},
		{
			name:       "reg import denied",
			output:     "reg import settings.reg\nERROR: Access is denied.",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialRegistry,
		},
		{
			name:       "dot reg file access denied",
			output:     "Importing creds.reg: Access is denied (the .reg file could not be read).",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialRegistry,
		},
		{
			name:       "Set-ItemProperty HKCU",
			output:     "Set-ItemProperty -Path \"HKCU:\\Software\\Contoso\" -Name Token -Value x : Access is denied.",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialRegistry,
		},
		{
			name:       "RegistryKey API",
			output:     "System.Security.AccessControl: cannot write RegistryKey HKEY_LOCAL_MACHINE\\SOFTWARE\\Contoso (Access is denied).",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialRegistry,
		},
		{
			name:       "RegSetValue API",
			output:     "RegSetValueExW failed: Access is denied.",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialRegistry,
		},
		{
			name:       "error code 0x5 with registry",
			output:     "registry write failed: 0x5 (Access is denied)",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialRegistry,
		},
		// --- 凭据库 ---------------------------------------------------------
		{
			name:         "DPAPI",
			output:       "DPAPI: CryptProtectData failed (Access is denied).",
			signatures:   windowsSignatures,
			wantKind:     sandbox.DenialCredentialStore,
			wantResource: "DPAPI",
		},
		{
			name:       "CryptUnprotectData",
			output:     "secure store: CryptUnprotectData returned 0x80070005 — Access is denied.",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialCredentialStore,
		},
		{
			name:       "keychain",
			output:     "security: SecKeychainItemCreateFromContent: Access is denied (keychain locked)",
			signatures: []string{"access is denied"},
			wantKind:   sandbox.DenialCredentialStore,
		},
		{
			name:       "libsecret",
			output:     "libsecret: secret-tool store failed: permission denied",
			signatures: posixSignatures,
			wantKind:   sandbox.DenialCredentialStore,
		},
		{
			name:       "security add-generic-password",
			output:     "security add-generic-password -a me -s dws failed: operation not permitted",
			signatures: []string{"operation not permitted"},
			wantKind:   sandbox.DenialCredentialStore,
		},
		{
			name:         "Windows Credential Manager",
			output:       "Windows Credential Manager: CredWrite failed with Access is denied.",
			signatures:   windowsSignatures,
			wantKind:     sandbox.DenialCredentialStore,
			wantResource: "Windows Credential Manager",
		},
		{
			name:       "wincred",
			output:     "wincred: store failed, Access is denied",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialCredentialStore,
		},
		{
			name:       "CredWrite API",
			output:     "CredWriteW returned ERROR_ACCESS_DENIED (Access is denied)",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialCredentialStore,
		},
		// --- 路径类回归（真实日志第 1 条）----------------------------------
		{
			name:       "real dws lock file denial stays a path denial",
			output:     "Error: Access is denied. (os error 5): C:\\Users\\dev\\.dws\\.data.lock",
			signatures: windowsSignatures,
			wantKind:   sandbox.DenialPath,
		},
		{
			name:       "plain posix path denial",
			output:     "open('/home/me/.cache/tool/state.json'): Permission denied",
			signatures: posixSignatures,
			wantKind:   sandbox.DenialPath,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			denial := sandbox.DetectDenial(tc.output, tc.signatures, policy, sandbox.EnforcementPartial)
			if denial == nil {
				t.Fatalf("Denial = nil, want non-nil for %q", tc.output)
			}
			if denial.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q (output %q)", denial.Kind, tc.wantKind, tc.output)
			}
			if tc.wantResource != "" && !strings.Contains(denial.Resource, tc.wantResource) {
				t.Fatalf("Resource = %q, want it to contain %q", denial.Resource, tc.wantResource)
			}
			if denial.Kind != sandbox.DenialPath && denial.Path != "" {
				t.Fatalf("Path = %q, want empty for non-path resources（给出路径会把模型引向申请目录）", denial.Path)
			}
			summary := denial.Summary()
			switch denial.Kind {
			case sandbox.DenialRegistry:
				if !strings.Contains(summary, "Windows registry") {
					t.Fatalf("Summary = %q, want it to name the registry", summary)
				}
			case sandbox.DenialCredentialStore:
				if !strings.Contains(summary, "credential store") {
					t.Fatalf("Summary = %q, want it to name the credential store", summary)
				}
			}
			t.Logf("observed: kind=%s resource=%q op=%q summary=%s", denial.Kind, denial.Resource, denial.Op, summary)
		})
	}
}

// TestDenialNonPathMentionsAreNotDenials 证明"提到 keychain/注册表"本身不算拒绝：
// 只有同时命中拒绝方言才归因（成功路径的输出不能被误判）。
func TestDenialNonPathMentionsAreNotDenials(t *testing.T) {
	policy := testPolicy(t)
	for _, output := range []string{
		"stored token in the OS keychain as dws-token",
		"wrote HKCU\\Software\\Contoso successfully",
		"debug: DPAPI available, using CryptProtectData",
	} {
		if denial := sandbox.DetectDenial(output, []string{"access is denied"}, policy, sandbox.EnforcementPartial); denial != nil {
			t.Fatalf("Denial = %+v, want nil for a success message %q", denial, output)
		}
	}
}

// TestDenialNonPathNextStepsPutElevationFirst 证明两类资源的处置顺序相反。
func TestDenialNonPathNextStepsPutElevationFirst(t *testing.T) {
	policy := testPolicy(t)
	registry := sandbox.DetectDenial("reg add HKCU\\Software\\RigSmoke /f\nERROR: Access is denied.",
		[]string{"access is denied"}, policy, sandbox.EnforcementPartial)
	if registry == nil || registry.Kind != sandbox.DenialRegistry {
		t.Fatalf("registry denial = %+v", registry)
	}
	// 有提权通道的组合：第一条必须点名真实参数（而不是泛泛的 "elevation parameter"）。
	registry.Options = sandbox.Options{HasEscalation: true}
	steps := registry.NextSteps()
	if len(steps) == 0 || !strings.Contains(steps[0], `sandbox_permissions:"require_escalated"`) {
		t.Fatalf("registry NextSteps[0] = %q, want the real escalation parameter first", steps[0])
	}
	// 没有提权通道时**不许**指向不存在的参数：改成让用户去设置里开。
	noEscalation := sandbox.DetectDenial("reg add HKCU\\Software\\RigSmoke /f\nERROR: Access is denied.",
		[]string{"access is denied"}, policy, sandbox.EnforcementPartial)
	if noEscalation == nil {
		t.Fatal("second registry denial = nil")
	}
	for _, step := range noEscalation.NextSteps() {
		if strings.Contains(step, "sandbox_permissions") || strings.Contains(step, "request_sandbox_access") {
			t.Fatalf("没有提权/授权通道时不该点名工具或参数: %q", step)
		}
	}
	if joined := strings.Join(noEscalation.NextSteps(), " "); !strings.Contains(joined, "app settings") {
		t.Fatalf("没有通道时应让用户去设置里开启: %q", joined)
	}
	note := registry.Note()
	if !strings.Contains(note, "NON-FILESYSTEM resource") {
		t.Fatalf("Note = %q, want an explicit non-filesystem explanation", note)
	}
	if strings.Contains(note, "additional writable roots") {
		t.Fatalf("Note = %q, want no writable-root block for a registry denial", note)
	}

	pathDenial := sandbox.DetectDenial("open('/home/me/.cache/x'): Permission denied",
		[]string{"permission denied"}, policy, sandbox.EnforcementFull)
	if pathDenial == nil || pathDenial.Kind != sandbox.DenialPath {
		t.Fatalf("path denial = %+v", pathDenial)
	}
	pathSteps := pathDenial.NextSteps()
	if len(pathSteps) == 0 || !strings.Contains(pathSteps[0], "redirecting") {
		t.Fatalf("path NextSteps[0] = %q, want redirect first", pathSteps[0])
	}
}
