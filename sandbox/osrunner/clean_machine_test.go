package osrunner

// 干净机器首次构建：缓存根不存在时必须"可创建"，而不是被静默剔出可写面。
//
// 缺陷原状：`toolchainCacheCandidates` 把不存在的缓存根从 ReadWrite 剔掉，只留
// 在 MissingToolchainCaches 上报。干净机器上 `~/.cargo`/`~/.m2`/`~/Library/Developer`
// 本来就不存在 → 不被授权 → 用户第一次 `cargo build`/`mvn` 在 mkdir 上吃 EPERM。
//
// 修法按后端能力分：Seatbelt 渲染（SBPL 按访问时求值）；bwrap/Landlock/ACL 在
// Confine 之前 MkdirAll（只建目录、0700、失败不阻断）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// cleanMachineHome 造一个"干净 HOME"：里面没有任何缓存目录。
func cleanMachineHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	return home
}

// 纯逻辑：Confine 之前缺失的缓存根必须被创建出来，且是 0700 的目录。
func TestMissingCacheRootsAreCreatedBeforeConfine(t *testing.T) {
	home := cleanMachineHome(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	_, missingBefore := toolchainCacheCandidates(toolchainCacheRoots(policy))
	if len(missingBefore) == 0 {
		t.Skip("本机所有缓存根都已存在，构造不出干净机器场景")
	}

	created, failed := ensureToolchainCacheRoots(policy)
	if len(created) == 0 {
		t.Fatalf("没有创建任何目录（missing=%v, failed=%v）", missingRootPaths(missingBefore), failed)
	}
	for _, path := range created {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("创建后仍然 stat 不到 %q: %v", path, err)
		}
		if !info.IsDir() {
			t.Fatalf("%q 不是目录", path)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("%q 权限 %o，应当只给属主（0700）", path, perm)
		}
		// 只建目录，不建文件。
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			t.Fatalf("readdir %q: %v", path, readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("%q 创建后不是空的（说明建了文件而不是目录）", path)
		}
	}

	// 创建成功之后，它们不再出现在 MissingToolchainCaches 里（别长期喊狼来了）。
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	for _, path := range created {
		for _, stillMissing := range profile.MissingToolchainCaches {
			if sandbox.PathEqual(stillMissing, path) {
				t.Fatalf("%q 已经创建成功，却仍在 MissingToolchainCaches 里：%v", path, profile.MissingToolchainCaches)
			}
		}
	}
	// 而且必须已经进了授权集。
	granted := make(map[string]bool)
	for _, root := range profile.ToolchainCaches {
		granted[filepath.Clean(root.Root)] = true
	}
	for _, path := range created {
		if !granted[filepath.Clean(path)] {
			t.Fatalf("%q 创建后没有进 ToolchainCaches：%v", path, rootPaths(profile.ToolchainCaches))
		}
	}
}

// 创建失败不阻断、不 panic，并且留在 MissingToolchainCaches 里如实上报。
func TestMissingCacheRootsCreationFailureIsReported(t *testing.T) {
	home := cleanMachineHome(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// 让 HOME 本身"不是一个目录"：在它的位置上放一个普通文件，于是所有
	// `<HOME>/xxx` 都无法创建（MkdirAll 返回 ENOTDIR）。选这种构造而不是
	// chmod 0555，是因为测试常以 root 跑，root 会忽略只读目录位。
	blocker := filepath.Join(filepath.Dir(home), "home-is-a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("prep: %v", err)
	}
	t.Setenv("HOME", filepath.Join(blocker, "nested"))
	t.Setenv("USERPROFILE", filepath.Join(blocker, "nested"))
	blocked := filepath.Join(blocker, "nested", ".cargo")

	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	created, failed := ensureToolchainCacheRoots(policy)
	foundFailed := false
	for _, path := range failed {
		if sandbox.PathEqual(path, blocked) {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Fatalf("%q 不可创建，却没有进 failed（created=%v failed=%v）", blocked, created, failed)
	}
	for _, path := range created {
		if sandbox.PathEqual(path, blocked) {
			t.Fatalf("%q 报告创建成功，但它是文件", blocked)
		}
	}
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	stillReported := false
	for _, path := range profile.MissingToolchainCaches {
		if sandbox.PathEqual(path, blocked) {
			stillReported = true
		}
	}
	if !stillReported {
		t.Fatalf("创建失败的 %q 没有出现在 MissingToolchainCaches：%v", blocked, profile.MissingToolchainCaches)
	}
}

// 纯逻辑：Seatbelt 必须把"尚不存在"的缓存根也渲染成 subpath 放行
// （SBPL 按访问时求值，这是它相对其它后端的优势）。
func TestSeatbeltRendersMissingCacheRoots(t *testing.T) {
	home := cleanMachineHome(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	_, missing := toolchainCacheCandidates(toolchainCacheRoots(policy))
	if len(missing) == 0 {
		t.Skip("本机所有缓存根都已存在")
	}
	profile := SeatbeltProfile(policy)
	if profile == "" {
		t.Fatal("SeatbeltProfile 为空")
	}
	for _, root := range missing {
		if !strings.Contains(profile, sbplString(root.Root)) {
			t.Fatalf("尚不存在的缓存根 %q 没有出现在 SBPL 里（首次 cargo build 会 EPERM）\nprofile=%s", root.Root, profile)
		}
	}
}

// 回归：保护项的语义必须保持"不存在就不下发"（别把"可写＝可创建"套到保护上）。
//
// 两个层面都要钉住：
//   - 全局保护路径（WithProtectedPaths）在构造期就丢掉不存在的项；
//   - 只读子路径（WithReadOnlySubpaths，.git 控制面用的那条路）保留"将来的位置"，
//     但渲染时只下发**存在**的项，缺失的进 MissingProtected 供诊断。
//
// 无论哪条路，都不能因为本轮的"缓存根可创建"改动而开始下发不存在的保护项。
func TestProtectedStillSkipsMissing(t *testing.T) {
	// 用一个干净的 HOME，确保"尚未存在的缓存根"一定存在（之前跑过的用例可能已经
	// 在真实 HOME 下把这些目录建出来了）。
	home := cleanMachineHome(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ws := t.TempDir()
	// 父目录存在、自身不存在：这是"值得报告"的缺失（父目录都不存在的纯噪声会被
	// missingProtectionReport 过滤，见诊断用例）。
	if err := os.MkdirAll(filepath.Join(ws, "repo", ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	missingConfig := filepath.Join(ws, "repo", ".git", "config")
	globalMissing := filepath.Join(ws, "repo", ".git", "hooks")

	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(globalMissing),
		sandbox.WithReadOnlySubpaths(ws, filepath.Join("repo", ".git", "config")),
	)
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	for _, path := range profile.Protected {
		if sandbox.PathEqual(path, missingConfig) || sandbox.PathEqual(path, globalMissing) {
			t.Fatalf("不存在的保护项 %q 被下发了（保护的是既有文件，不存在就不该下发）", path)
		}
	}
	// 只读子路径的缺失项要被报告出来（供诊断报警），而不是静默消失。
	found := false
	for _, path := range profile.MissingProtected {
		if sandbox.PathEqual(path, missingConfig) {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺失的只读子路径 %q 没有出现在 MissingProtected：%v", missingConfig, profile.MissingProtected)
	}
	// 而缓存根（可写语义）必须相反：缺失也授权。
	grantedMissing := false
	for _, root := range profile.ToolchainCaches {
		if _, statErr := os.Stat(root.Root); statErr != nil {
			grantedMissing = true
		}
	}
	if !grantedMissing {
		t.Fatal("这个用例需要至少一个尚未存在的缓存根来对比两种语义（本机缓存目录都齐了）")
	}
}
