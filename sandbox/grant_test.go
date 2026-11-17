package sandbox_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestValidateWritableGrantPathRejectsBadInput(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root))
	_ = policy

	cases := []struct {
		name string
		path string
		want error
	}{
		{"empty", "", sandbox.ErrGrantNotAbsolute},
		{"relative", "relative/dir", sandbox.ErrGrantNotAbsolute},
		{"dotdot", "/tmp/../etc", sandbox.ErrGrantNotAbsolute},
		{"filesystem root", string(filepath.Separator), sandbox.ErrGrantSensitivePath},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, struct {
			name string
			path string
			want error
		}{"system dir", "/etc", sandbox.ErrGrantSensitivePath})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sandbox.ValidateWritableGrantPath(tc.path, []string{root}, nil, nil, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateWritableGrantPath(%q) = %v, want %v", tc.path, err, tc.want)
			}
		})
	}
}

func TestValidateWritableGrantPathRejectsWorkspaceRelations(t *testing.T) {
	root := t.TempDir()
	// 已经可写：工作区内部
	if _, err := sandbox.ValidateWritableGrantPath(filepath.Join(root, "sub"), []string{root}, nil, nil, nil); !errors.Is(err, sandbox.ErrGrantRedundant) {
		t.Fatalf("path inside root: err = %v, want ErrGrantRedundant", err)
	}
	// 过宽：工作区的祖先
	if _, err := sandbox.ValidateWritableGrantPath(filepath.Dir(root), []string{root}, nil, nil, nil); !errors.Is(err, sandbox.ErrGrantTooBroad) {
		t.Fatalf("ancestor of root: err = %v, want ErrGrantTooBroad", err)
	}
}

func TestValidateWritableGrantPathRejectsHomeItself(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	if real, realErr := filepath.EvalSymlinks(home); realErr == nil {
		home = real
	}
	if _, err := sandbox.ValidateWritableGrantPath(home, []string{t.TempDir()}, nil, nil, nil); !errors.Is(err, sandbox.ErrGrantTooBroad) {
		t.Fatalf("home itself: err = %v, want ErrGrantTooBroad", err)
	}
}

func TestValidateWritableGrantPathAcceptsExistingAndCreatable(t *testing.T) {
	base := t.TempDir()
	existing := filepath.Join(base, "creds")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := sandbox.ValidateWritableGrantPath(existing, []string{filepath.Join(base, "ws")}, nil, nil, nil)
	if err != nil {
		t.Fatalf("existing dir: %v", err)
	}
	if got != existing && !strings.HasSuffix(got, "creds") {
		t.Fatalf("resolved = %q, want it to end with creds", got)
	}

	pending := filepath.Join(base, "not-yet-created")
	resolved, err := sandbox.ValidateWritableGrantPath(pending, []string{filepath.Join(base, "ws")}, nil, nil, nil)
	if err != nil {
		t.Fatalf("creatable dir: %v", err)
	}
	if !strings.HasSuffix(resolved, "not-yet-created") {
		t.Fatalf("resolved = %q, want pending tail preserved", resolved)
	}
}

func TestValidateWritableGrantPathRejectsMissingParent(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "a", "b", "c")
	if _, err := sandbox.ValidateWritableGrantPath(path, []string{filepath.Join(base, "ws")}, nil, nil, nil); err == nil {
		t.Fatal("expected error when parent directory is missing")
	}
}

func TestValidateWritableGrantPathRejectsProtectedOverlap(t *testing.T) {
	base := t.TempDir()
	protected := filepath.Join(base, "vault")
	if err := os.Mkdir(protected, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(protected, "sub")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.ValidateWritableGrantPath(protected, []string{filepath.Join(base, "ws")}, nil, []string{protected}, nil); !errors.Is(err, sandbox.ErrGrantProtected) {
		t.Fatalf("err = %v, want ErrGrantProtected", err)
	}
	if _, err := sandbox.ValidateWritableGrantPath(inside, []string{filepath.Join(base, "ws")}, nil, []string{protected}, nil); !errors.Is(err, sandbox.ErrGrantProtected) {
		t.Fatalf("path inside protected: err = %v, want ErrGrantProtected", err)
	}
}

func TestPolicyGrantLifecycle(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(base, "tool-creds")
	if err := os.Mkdir(creds, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithGrantBudget(2),
	)
	if got := policy.GrantBudget(); got != 2 {
		t.Fatalf("budget = %d, want 2", got)
	}
	if len(policy.AdditionalWritableRoots()) != 0 {
		t.Fatalf("writable roots before grant = %v, want empty", policy.AdditionalWritableRoots())
	}

	grant, err := policy.AddWritableRoot(creds, "tool stores credentials here")
	if err != nil {
		t.Fatalf("AddWritableRoot: %v", err)
	}
	if grant.Path == "" || grant.Reason == "" {
		t.Fatalf("grant = %+v, want path and reason", grant)
	}
	roots := policy.AdditionalWritableRoots()
	if len(roots) != 1 || roots[0] != grant.Path {
		t.Fatalf("writable roots = %v, want [%s]", roots, grant.Path)
	}
	if len(policy.Grants()) != 1 {
		t.Fatalf("grants = %v, want 1", policy.Grants())
	}

	// 重复申请：冗余，不是失败。
	if _, err := policy.AddWritableRoot(creds, "again"); !errors.Is(err, sandbox.ErrGrantRedundant) {
		t.Fatalf("duplicate grant: err = %v, want ErrGrantRedundant", err)
	}

	second := filepath.Join(base, "second")
	if err := os.Mkdir(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.AddWritableRoot(second, "second"); err != nil {
		t.Fatalf("second grant: %v", err)
	}
	third := filepath.Join(base, "third")
	if err := os.Mkdir(third, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.AddWritableRoot(third, "third"); !errors.Is(err, sandbox.ErrGrantBudgetExceeded) {
		t.Fatalf("third grant: err = %v, want ErrGrantBudgetExceeded", err)
	}
}

func TestPolicyGrantIsRedundantUnderFullAccess(t *testing.T) {
	policy := sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))
	if _, err := policy.AddWritableRoot(t.TempDir(), "unnecessary"); !errors.Is(err, sandbox.ErrGrantRedundant) {
		t.Fatalf("err = %v, want ErrGrantRedundant", err)
	}
}

func TestWithWritableRootsRecordsDroppedEntries(t *testing.T) {
	base := t.TempDir()
	kept := filepath.Join(base, "kept")
	if err := os.Mkdir(kept, 0o755); err != nil {
		t.Fatal(err)
	}
	// 父目录存在但目录本身缺失：构造期创建它并保留（工具首次运行才建配置目录
	// 是最常见的情况）。
	createdByPolicy := filepath.Join(base, "created-by-policy")
	// 父目录也不存在：无法创建，记为 dropped 让宿主报警。
	uncreatable := filepath.Join(base, "no-such-parent", "creds")
	policy := sandbox.New(
		sandbox.WithRoots(filepath.Join(base, "ws")),
		sandbox.WithWritableRoots(kept, createdByPolicy, uncreatable),
	)
	roots := policy.ConfiguredWritableRoots()
	if len(roots) != 2 {
		t.Fatalf("configured roots = %v, want the existing and the created entry", roots)
	}
	if info, err := os.Stat(createdByPolicy); err != nil || !info.IsDir() {
		t.Fatalf("created writable root %q: err = %v", createdByPolicy, err)
	}
	dropped := policy.DroppedWritableRoots()
	if len(dropped) != 1 {
		t.Fatalf("dropped = %v, want exactly the uncreatable entry", dropped)
	}
}

func TestTempInclusionToggles(t *testing.T) {
	policy := sandbox.New()
	if !policy.IncludeSlashTmp() || !policy.IncludeTmpdirEnv() {
		t.Fatal("defaults must include /tmp and $TMPDIR")
	}
	strict := sandbox.New(sandbox.WithSlashTmp(false), sandbox.WithTmpdirEnv(false))
	if strict.IncludeSlashTmp() || strict.IncludeTmpdirEnv() {
		t.Fatal("toggles must be honored")
	}
}

func TestProtectedPathsResolveRelativeToPrimaryRoot(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithProtectedPaths(".git", "missing-dir"),
	)
	paths := policy.ProtectedPaths()
	// 不存在的保护路径会**保留**（"保护一个尚不存在的目录"是有效语义，例如会话
	// 中途 git init 出来的 .git）；能不能渲染由后端决定，缺失项进
	// Profile.MissingProtected 供诊断。
	if len(paths) != 2 {
		t.Fatalf("protected = %v, want the existing .git and the not-yet-created entry", paths)
	}
	if real, err := filepath.EvalSymlinks(gitDir); err == nil {
		if paths[0] != real {
			t.Fatalf("protected[0] = %q, want %q", paths[0], real)
		}
	}
	if !strings.HasSuffix(paths[1], "missing-dir") {
		t.Fatalf("protected[1] = %q, want the missing entry to be kept for diagnostics", paths[1])
	}
}
