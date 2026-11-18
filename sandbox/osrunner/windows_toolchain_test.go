package osrunner

// Windows "开发工具链能力对齐" 审计（第 3C 轮）：纯计划层断言，任意宿主可跑。
//
// 本文件不做真机执行（本机是 Linux）：它断言的是 helper argv 的**形状**与纯路径
// 函数的行为；真机行为差异在报告里逐条标注"未执行"。
//
// 命名约定：`TestAuditWinToolchain_Finding*` 是**当前不满足预期**的用例（红），
// 它们就是审计发现本身；其余是钉住现状的 characterization。

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// winPlanFromPolicy 复现 runner 在 Windows 上的实际包装：返回 helper 会收到的
// --workspace / --protect 列表。失败时直接 fatal（形状错了本身就是 bug）。
func winPlanFromPolicy(t *testing.T, policy *sandbox.Policy) winACLPlan {
	t.Helper()
	command, err := buildWindowsConfinedCommand(`C:\rig\rig.exe`, []string{"cmd.exe", "/c", "go", "build", "./..."}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	// 形状：[exe, HelperArgWindowsACL, args..., --, argv...]
	if len(command.Argv) < 2 || command.Argv[1] != HelperArgWindowsACL {
		t.Fatalf("helper 前缀形状不对：%v", command.Argv)
	}
	plan, err := parseWinACLArgs(command.Argv[2:])
	if err != nil {
		t.Fatalf("parseWinACLArgs: %v", err)
	}
	return plan
}

func winPlanHas(plan winACLPlan, path string) bool {
	for _, workspace := range plan.Workspaces {
		if samePathObject(workspace, path) {
			return true
		}
	}
	return false
}

// samePathObject 报告两个路径是否指向同一个文件系统对象。
//
// 为什么不用字符串比较：Windows 上同一个目录可能以 8.3 短名（`RUNNER~1`）、不同
// 大小写或 `\?\` 前缀出现；os.SameFile 比较的是卷序列号 + 文件索引，对这些写法
// **免疫**（真机 CI 上就是被 8.3 短名咬到的）。取不到 stat 时退回 winPathKey。
func samePathObject(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(infoA, infoB)
	}
	return winPathKey(a) == winPathKey(b)
}

// 具链在 workspace-write 下必须能写的目录（Windows 默认位置）。
//
// 每一条都是"只写缓存、不含凭据"的子目录：刻意**不**包含 %APPDATA%、%USERPROFILE%、
// .cargo 根、.m2 根（那里面有 .npmrc / credentials.toml / settings.xml）。
var winToolchainCacheDirs = []struct {
	label string
	path  string
}{
	{"Go 构建缓存 GOCACHE", `C:\Users\u\AppData\Local\go-build`},
	{"Go 模块缓存 GOMODCACHE", `C:\Users\u\go\pkg\mod`},
	{"npm 缓存", `C:\Users\u\AppData\Local\npm-cache`},
	{"pnpm store", `C:\Users\u\AppData\Local\pnpm-store`},
	{"pip 缓存", `C:\Users\u\AppData\Local\pip\Cache`},
	{"uv 缓存", `C:\Users\u\AppData\Local\uv\cache`},
	{"cargo registry", `C:\Users\u\.cargo\registry`},
	{"cargo git", `C:\Users\u\.cargo\git`},
	{"maven repository", `C:\Users\u\.m2\repository`},
	{"gradle caches", `C:\Users\u\.gradle\caches`},
	{"nuget packages", `C:\Users\u\.nuget\packages`},
}

// A3C-W1：`AdditionalWritableRoots()` 必须进入 Windows 的可写面（D1 验收）。
//
// 历史：这条曾经是 Finding（红）——`buildWindowsConfinedCommand` 只传
// `policy.Roots()`，于是宿主显式追加的可写根与**运行期授权**
// （`request_sandbox_access` → `Policy.AddWritableRoot`）在 Windows 上全是空操作，
// 而 Linux/macOS 的 BuildProfile 会把它们放进 ReadWrite。**同一 Policy 在 Windows
// 上行为不同**是这次审计最重要的一条。
func TestAuditWinToolchain_AdditionalWritableRootsAreGranted(t *testing.T) {
	root := t.TempDir()
	granted := t.TempDir()
	outside := t.TempDir()
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	// 运行期授权路径（与 request_sandbox_access 批准后走的是同一条）：
	if _, err := policy.AddWritableRoot(granted, "toolchain cache (test)"); err != nil {
		t.Fatalf("AddWritableRoot(%q): %v", granted, err)
	}

	plan := winPlanFromPolicy(t, policy)
	if !winPlanHas(plan, root) {
		t.Fatalf("策略根没有进入 --workspace：%v", plan.Workspaces)
	}
	if !winPlanHas(plan, granted) {
		t.Fatalf("运行期授权目录 %q 没进入 --workspace：Windows 上 request_sandbox_access 会是空操作；"+
			"实际下发 %v", granted, plan.Workspaces)
	}
	if winPlanHas(plan, outside) {
		t.Fatalf("未授权目录 %q 不应出现在可写面：%v", outside, plan.Workspaces)
	}
	// 仅"出现在 --workspace"还不够：真正生效要求 helper 为每个授权根派生一个
	// capability SID，并把它放进受限令牌的 restricting 列表（ACE 与令牌必须同源，
	// 少一边就是"看起来授权了、实际写不进去"）。
	sids := make([]string, 0, len(plan.Workspaces))
	for _, workspace := range plan.Workspaces {
		sid, err := winWorkspaceCapabilitySID(workspace)
		if err != nil {
			t.Fatalf("winWorkspaceCapabilitySID(%q): %v", workspace, err)
		}
		sids = append(sids, sid)
	}
	restricting, err := windowsRestrictingSIDStrings(winModeWorkspaceWrite,
		"S-1-5-21-9-9-9-1001", "S-1-1-0", sids, "S-1-5-21-9-9-9-2002")
	if err != nil {
		t.Fatalf("windowsRestrictingSIDStrings: %v", err)
	}
	for index, sid := range sids {
		found := false
		for _, entry := range restricting {
			if entry == sid {
				found = true
			}
		}
		if !found {
			t.Fatalf("授权根 #%d 的 capability SID 不在 restricting 列表里（ACE 与令牌不同源）：%v",
				index, restricting)
		}
	}

	// 静态声明的 AdditionalWritableRoots 同样必须生效。
	extra := t.TempDir()
	static := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithWritableRoots(extra),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	if plan := winPlanFromPolicy(t, static); !winPlanHas(plan, extra) {
		t.Fatalf("WithWritableRoots(%q) 没进入 --workspace：%v", extra, plan.Workspaces)
	}
}

// A3C-W2：Windows 工具链缓存清单必须覆盖真实开发工具的落点。
//
// 清单的真源在 profile 侧（toolchain_caches.go 的 toolchainCacheRootsFor），这里
// 只做**覆盖审计**：用假的 Windows 环境问一遍"这些已知落点是否被某个授权根覆
// 盖"，缺口只记日志（它的修复属于 profile 侧，本侧不重复实现）。
func TestAuditWinToolchain_ToolchainCacheCoverage(t *testing.T) {
	home := `C:\Users\u`
	local := `C:\Users\u\AppData\Local`
	appData := `C:\Users\u\AppData\Roaming`
	roots := toolchainCacheRootsFor("windows", func(key string) string {
		switch key {
		case "HOME", "USERPROFILE":
			return home
		case "LOCALAPPDATA":
			return local
		case "APPDATA":
			return appData
		}
		return ""
	})
	if len(roots) == 0 {
		t.Fatal("Windows 缓存清单为空")
	}
	granted := make([]string, 0, len(roots))
	protects := make([]string, 0, len(roots))
	for _, root := range roots {
		granted = append(granted, root.Root)
		protects = append(protects, root.ReadOnlySubpaths...)
	}
	covered := func(path string) bool {
		for _, root := range granted {
			if winPathWithinFold(root, path) {
				return true
			}
		}
		return false
	}
	required := []struct {
		label string
		path  string
	}{
		{"GOCACHE", filepath.Join(local, "go-build")},
		{"GOMODCACHE", filepath.Join(home, "go", "pkg", "mod")},
		{"npm 缓存", filepath.Join(local, "npm-cache")},
		{"pip 缓存", filepath.Join(local, "pip", "Cache")},
		{"uv 缓存", filepath.Join(local, "uv", "cache")},
		{"pnpm store", filepath.Join(local, "pnpm-store")},
		{"cargo registry", filepath.Join(home, ".cargo", "registry")},
		{"cargo git", filepath.Join(home, ".cargo", "git")},
		{"maven repository", filepath.Join(home, ".m2", "repository")},
		{"gradle caches", filepath.Join(home, ".gradle", "caches")},
	}
	var missing []string
	for _, item := range required {
		if !covered(item.path) {
			missing = append(missing, item.label+" :: "+item.path)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("Windows 缓存清单漏了 %d 个开发工具落点（沙箱内会写不进去）：\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	// 凭据文件必须被标成只读子路径，而不是靠"整目录可写 + 自觉"。
	for _, name := range []string{"\\.npmrc", "credentials.toml", "settings.xml", "gradle.properties"} {
		found := false
		for _, protect := range protects {
			if strings.HasSuffix(winPathKey(protect), winPathKey(name)) || strings.HasSuffix(protect, name) {
				found = true
			}
		}
		if !found {
			t.Errorf("凭据文件 %q 没有出现在任何 ReadOnlySubpaths 里（缓存目录整目录可写就等于交出 token）", name)
		}
	}
	// 明确记录仍未覆盖的落点（不 fail：修复属于 profile 侧）。
	for _, item := range []struct{ label, path string }{
		{"nuget packages", filepath.Join(home, ".nuget", "packages")},
		{"bun install cache", filepath.Join(local, "bun", "install", "cache")},
	} {
		if !covered(item.path) {
			t.Logf("未覆盖（profile 侧待定）: %s :: %s", item.label, item.path)
		}
	}
}

// A3C-W3：路径键归一（D3 验收 + 8.3/UNC 现状说明）。
//
// 该归一决定了 capability SID 的派生与**保护路径归属判定**（后者是 fail-open 的
// 入口，见 A3C-W7）。
func TestAuditWinToolchain_PathFormNormalization(t *testing.T) {
	// 大小写 + 分隔符 + 尾斜杠 + extended 前缀：同一目录的所有写法必须折叠成同一个键。
	variants := []string{
		`C:\Work\Repo`, `c:/work/repo`, `C:\Work\Repo\`, `c:\WORK\repo`,
		`\\?\C:\Work\Repo`, `\\?\C:\Work\Repo\`,
	}
	want := winPathKey(variants[0])
	for _, variant := range variants {
		if got := winPathKey(variant); got != want {
			t.Fatalf("winPathKey(%q) = %q，want %q（大小写/分隔符/尾斜杠/extended 前缀都必须归一）", variant, got, want)
		}
	}

	// UNC：extended 形式必须归一成普通形式；主机/共享的大小写按 Windows 语义折叠。
	if winPathKey(`\\?\UNC\srv\share\x`) != winPathKey(`\\srv\share\x`) {
		t.Fatalf("extended UNC 前缀没有归一：%q vs %q",
			winPathKey(`\\?\UNC\srv\share\x`), winPathKey(`\\srv\share\x`))
	}
	if winPathKey(`\\SERVER\Share`) != winPathKey(`\\server\share`) {
		t.Fatalf("UNC 主机/共享没有按 Windows 语义折叠大小写")
	}

	// 同名前缀但不是同一目录：不能因为归一而放宽（"Repo2" 不在 "Repo" 之下）。
	if winPathWithinFold(`C:\Work\Repo`, `C:\Work\Repo2\.git`) {
		t.Fatal(`Repo2 被误判为在 Repo 之下`)
	}
	if winPathWithinFold(`C:\Work\Repo`, `\\?\D:\Work\Repo\.git`) {
		t.Fatal("不同驱动器被误判为在其下")
	}
}

// A3C-W4：保护子树遍历必须跳过符号链接/重解析点（否则会给工作区外的真实对象
// 加 deny ACE）。
//
// 可测部分：符号链接被跳过并计入 Skipped。**未在真机验证**：Windows 的目录
// junction（IO_REPARSE_TAG_MOUNT_POINT）在 Go 里**不**是 fs.ModeSymlink，WalkDir
// 会把它当普通目录下钻——那会让保护遍历走进工作区之外。见报告"Windows 独有绕过面"。
func TestAuditWinToolchain_ProtectWalkSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	protected := filepath.Join(root, ".git")
	outside := t.TempDir()
	if err := os.MkdirAll(protected, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(protected, "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 链接到工作区外：遍历必须跳过它（否则 ACE 会落到 outside 里的真实对象上）。
	if err := os.Symlink(outside, filepath.Join(protected, "escape")); err != nil {
		t.Skipf("无法创建符号链接：%v", err)
	}

	set := newFakeProtectObjectSet()
	stats, err := winEnforceProtectTree(set.Object, protected, "S-1-5-21-1-2-3-4")
	if err != nil {
		t.Fatalf("winEnforceProtectTree: %v", err)
	}
	if stats.Skipped != 1 {
		t.Fatalf("Skipped = %d，want 1（符号链接必须跳过）", stats.Skipped)
	}
	set.mu.Lock()
	visited := make([]string, 0, len(set.present))
	for path := range set.present {
		visited = append(visited, path)
	}
	set.mu.Unlock()
	for _, path := range visited {
		if strings.Contains(path, "escape") || strings.Contains(path, winPathKey(outside)) {
			t.Fatalf("保护遍历走进了链接目标：%q", path)
		}
	}
}

// A3C-W5：私有临时目录必须落在工作区之外，且 TMP/TEMP/TMPDIR 被改写过去。
//
// 这是工具链"唯一的可写临时区"：GOCACHE 若被显式指到 TMP 下，这条能救一半命令。
func TestAuditWinToolchain_PrivateTempIsRewritten(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "tmp")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if windowsTempRootUsable([]string{root}, inside) {
		t.Fatal("工作区内的临时根必须被拒绝（否则两个授权面会被合并）")
	}
	if !windowsTempRootUsable([]string{root}, t.TempDir()) {
		t.Fatal("工作区外的临时根应可用")
	}

	// 环境改写（winPrivateTempEnvironment）在 //go:build windows 文件里，本机无法
	// 执行；代码审读结论写进报告（TMP/TEMP → 私有目录，TMPDIR 被移除）。本用例
	// 只钉住"私有临时根不能落在工作区内"这一条可测不变量。
}

// A3C-W6：策略里"包含凭据的父目录"绝不能被当成可写缓存整体授权。
//
// 这条用例是给未来改动的护栏：如果谁把 %APPDATA%、.cargo 根、.m2 根、%USERPROFILE%
// 加进工具链可写面，它会立刻变红。
func TestAuditWinToolchain_CredentialParentsAreNeverGranted(t *testing.T) {
	forbidden := []string{
		`C:\Users\u\AppData\Roaming`, // %APPDATA%：.npmrc、pip.ini、gh/hosts.yml
		`C:\Users\u\AppData\Local`,   // %LOCALAPPDATA% 根：含大量应用私有数据
		`C:\Users\u`,                 // %USERPROFILE% 根
		`C:\Users\u\.cargo`,          // credentials.toml
		`C:\Users\u\.m2`,             // settings.xml（server 凭据）
		`C:\Users\u\.gradle`,         // gradle.properties（仓库 token）
		`C:\Users\u\.ssh`,
		`C:\Users\u\.aws`,
	}
	for _, allowed := range winToolchainCacheDirs {
		for _, banned := range forbidden {
			if winPathKey(allowed.path) == winPathKey(banned) {
				t.Fatalf("工具链名单里出现了凭据父目录 %q（应只列其下的缓存子目录）", allowed.path)
			}
			if winPathWithinFold(banned, allowed.path) && winPathKey(allowed.path) == winPathKey(banned) {
				t.Fatalf("工具链名单项 %q 等于被禁父目录", allowed.path)
			}
		}
	}
}

// A3C-W7（D3）：`\\?\` 与 `\\?\UNC\` 前缀必须归一，否则保护路径的归属判定会
// fail-open（`.git` 变可写）。
//
// 背景：Windows 的 extended-length 前缀（`\\?\`）指向与普通写法**同一个**对象，
// 但 `winPathKey` / `winPathWithinFold` 只折叠大小写与分隔符，不剥前缀 ⇒
// 「workspace=C:\w、protect=\\?\C:\w\.git」判不出归属 ⇒ helper 记一行
// "outside every granted workspace; skipping" 并**跳过保护**。
func TestAuditWinToolchain_ExtendedPathPrefixIsNormalized(t *testing.T) {
	// winPathKey：三种写法必须是同一个键。
	base := winPathKey(`C:\Work\Repo\.git`)
	for _, variant := range []string{`\\?\C:\Work\Repo\.git`, `\\?\c:\work\repo\.git`, `C:/Work/Repo/.git`} {
		if got := winPathKey(variant); got != base {
			t.Errorf("winPathKey(%q) = %q，want %q（extended 前缀/分隔符/大小写必须归一）", variant, got, base)
		}
	}
	// UNC 的 extended 形式 `\\?\UNC\srv\share\x` 必须归一成 `\\srv\share\x`。
	if winPathKey(`\\?\UNC\srv\share\x`) != winPathKey(`\\srv\share\x`) {
		t.Errorf("winPathKey 未把 \\\\?\\UNC\\ 归一：%q vs %q",
			winPathKey(`\\?\UNC\srv\share\x`), winPathKey(`\\srv\share\x`))
	}

	// winPathWithinFold：保护路径归属判定必须在两种写法下一致（这是 fail-open 的入口）。
	for _, protected := range []string{`C:\Work\Repo\.git`, `\\?\C:\Work\Repo\.git`, `\\?\C:\Work\Repo\.git\objects`} {
		if !winPathWithinFold(`C:\Work\Repo`, protected) {
			t.Errorf("winPathWithinFold(%q, %q) = false：保护路径被判成「在授权面之外」，会被跳过",
				`C:\Work\Repo`, protected)
		}
	}
	// 反向：不同目录仍然必须判为"不在其下"（别把归一写成放宽）。
	for _, other := range []string{`C:\Work\Repo2\.git`, `\\?\C:\Work\Repo2\.git`, `\\?\D:\Work\Repo\.git`} {
		if winPathWithinFold(`C:\Work\Repo`, other) {
			t.Errorf("winPathWithinFold(%q, %q) = true（不同目录被误判为在其下）", `C:\Work\Repo`, other)
		}
	}
}

// A3C-W8（D4）：保护子树遍历必须按 `FILE_ATTRIBUTE_REPARSE_POINT` 跳过条目，
// 而不只是 `fs.ModeSymlink`。
//
// junction（`mklink /J`，reparse tag = IO_REPARSE_TAG_MOUNT_POINT）在 Go 里**不**
// 置 ModeSymlink，旧实现会把它当普通目录下钻，于是给**工作区外的真实对象**逐个加
// deny ACE（运行期间改了外部 DACL）。
func TestAuditWinToolchain_ReparsePointsAreSkipped(t *testing.T) {
	// ① 纯判定：属性位决定是否跳过，与 entry 类型正交。
	cases := []struct {
		name       string
		attributes uint32
		isSymlink  bool
		want       bool
	}{
		{"普通目录", 0x10 /* FILE_ATTRIBUTE_DIRECTORY */, false, false},
		{"普通文件", 0x80 /* FILE_ATTRIBUTE_NORMAL */, false, false},
		{"符号链接（属性位）", winFileAttributeReparsePoint, false, true},
		{"junction（属性位）", winFileAttributeReparsePoint | 0x10, false, true},
		{"entry 报 symlink", 0, true, true},
	}
	for _, testCase := range cases {
		if got := winEntryNeedsSkip(testCase.attributes, testCase.isSymlink); got != testCase.want {
			t.Errorf("%s: winEntryNeedsSkip = %v，want %v", testCase.name, got, testCase.want)
		}
	}

	// ② 遍历接线：跳过判定被真正用于 walk，且被跳过条目的整棵子树都不碰。
	root := t.TempDir()
	protected := filepath.Join(root, ".git")
	escape := filepath.Join(protected, "esc")
	if err := os.MkdirAll(filepath.Join(escape, "nested"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(escape, "nested", "outside.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(protected, "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	set := newFakeProtectObjectSet()
	// 模拟 Windows：esc 是 reparse point（junction）。
	skip := func(path string, entry fs.DirEntry) (bool, error) {
		return winPathKey(path) == winPathKey(escape), nil
	}
	stats, err := winEnforceProtectTreeWithSkip(set.Object, protected, "S-1-5-21-1-2-3-4", skip)
	if err != nil {
		t.Fatalf("winEnforceProtectTreeWithSkip: %v", err)
	}
	if stats.Skipped != 1 {
		t.Fatalf("Skipped = %d，want 1（reparse point 必须跳过）", stats.Skipped)
	}
	set.mu.Lock()
	visited := make([]string, 0, len(set.present))
	for path := range set.present {
		visited = append(visited, path)
	}
	set.mu.Unlock()
	for _, path := range visited {
		if strings.Contains(path, "esc") {
			t.Fatalf("保护遍历走进了 reparse point 的子树：%q（visited=%v）", path, visited)
		}
	}
	// 保护根自身与普通子项仍必须被处理。
	if !set.has(filepath.Join(protected, "config"), "S-1-5-21-1-2-3-4") {
		t.Fatalf("保护根下的普通文件没有被加 deny ACE：visited=%v", visited)
	}
}

// A3C-W9（D1 附带）：可写根内的**只读子路径**必须变成 `--protect`。
//
// 这是 Windows 侧另一处"没消费同一份模型"的缺口：`WithWritableRoot(dir, "sub")` /
// `WithReadOnlySubpaths` 在 Linux/macOS 上会让 sub 保持只读，而 Windows 早先只下发
// `policy.ProtectedPaths()`（全局保护），根内只读子路径**完全不生效**——即 workspace
// 的 `.git` 这类"根内保护"在 Windows 上变可写。
func TestAuditWinToolchain_ReadOnlySubpathsBecomeProtects(t *testing.T) {
	root := t.TempDir()
	git := filepath.Join(root, ".git")
	sub := filepath.Join(root, "vendor")
	for _, dir := range []string{git, sub, filepath.Join(git, "objects")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithWritableRoot(root, ".git", "vendor"),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	plan := winPlanFromPolicy(t, policy)
	for _, want := range []string{git, sub} {
		found := false
		for _, protect := range plan.Protected {
			if samePathObject(protect, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("根内只读子路径 %q 没进 --protect（Windows 上会变可写）：%v", want, plan.Protected)
		}
	}
}

// A3C-W10：声明了但**尚不存在**的只读子路径不能下发给 helper。
//
// helper 对不存在的 --protect 是 fail closed（报错、不 spawn），而"先声明
// `WithWritableRoot(root, ".git")`、仓库稍后才 git init"是极常见的使用顺序——
// 原样下发会让**每一条命令**都跑不起来。bwrap/Landlock 同样只能渲染存在的路径，
// "空承诺"由 Profile.MissingProtected 负责上报。
func TestAuditWinToolchain_MissingReadOnlySubpathIsNotRendered(t *testing.T) {
	root := t.TempDir()
	present := filepath.Join(root, "vendor")
	if err := os.MkdirAll(present, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	missing := filepath.Join(root, ".git") // 故意不创建
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithWritableRoot(root, ".git", "vendor"),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	plan := winPlanFromPolicy(t, policy)
	for _, protect := range plan.Protected {
		if samePathObject(protect, missing) {
			t.Fatalf("不存在的只读子路径被下发了：helper 会 fail closed、所有命令都跑不起来（%v）", plan.Protected)
		}
	}
	found := false
	for _, protect := range plan.Protected {
		if samePathObject(protect, present) {
			found = true
		}
	}
	if !found {
		t.Fatalf("存在的只读子路径必须仍然下发：%v", plan.Protected)
	}
}

// A3C-W11（CI 回归的最小复现）：授权面与保护面**必须共用同一个规范化函数**。
//
// Windows 上同一个目录有多种等价写法（8.3 短名 `C:\Users\RUNNER~1\...`、大小写、
// `\\?\` 前缀、junction）。如果 `--workspace` 用长名而 `--protect` 用短名，
// `winPathWithinFold` 判不出"保护路径在工作区之内"，helper 就记一行
// "outside every granted workspace; skipping" 并**跳过保护**（`.git` 变可写）。
//
// 用注入的规范化函数在任意宿主上复现该形态（不需要 Windows 真机）：
// `shortVendor` 与 `longVendor` 是同一个目录的两种写法。
func TestAuditWinToolchain_MixedPathFormsConvergeBeforeCompare(t *testing.T) {
	root := t.TempDir()
	longVendor := filepath.Join(root, "vendor")
	longGit := filepath.Join(root, ".git")
	for _, dir := range []string{longVendor, longGit} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// 模拟 Windows：8.3 短名写法与长名写法指向同一目录。
	shortRoot := filepath.Join(filepath.Dir(root), "RUNNER~1", filepath.Base(root))
	shortVendor := filepath.Join(shortRoot, "vendor")
	original := winCanonicalPath
	winCanonicalPath = func(path string) (string, error) {
		cleaned := winPathKeyFoldOnly(path)
		switch {
		case cleaned == winPathKeyFoldOnly(shortRoot), cleaned == winPathKeyFoldOnly(root):
			return root, nil
		case strings.HasPrefix(cleaned, winPathKeyFoldOnly(shortRoot)+"/"):
			rest := strings.TrimPrefix(cleaned, winPathKeyFoldOnly(shortRoot)+"/")
			return filepath.Join(root, filepath.FromSlash(rest)), nil
		}
		if _, err := os.Stat(path); err != nil {
			return "", err
		}
		return filepath.Clean(path), nil
	}
	defer func() { winCanonicalPath = original }()

	// 授权面拿到长名（策略根来自 t.TempDir()）。
	roots := winCanonicalAll([]string{root})
	// 保护面拿到**短名**（CI 上的真实形态）。
	protects := winFilterProtects([]string{shortVendor, longGit})
	if len(protects) == 0 {
		t.Fatal("保护路径全被丢掉：说明存在性判定/下发用的是未规范化的写法")
	}
	for _, protect := range protects {
		inside := false
		for _, workspace := range roots {
			if winPathWithinFold(workspace, protect) {
				inside = true
			}
		}
		if !inside {
			t.Fatalf("保护路径 %q 不在授权根 %v 之内：helper 会跳过保护（.git 变可写）", protect, roots)
		}
	}
	// 短名必须被展开成长名（与授权面同一写法）。
	if !winPathWithinFold(root, filepath.Join(root, "vendor")) {
		t.Fatal("测试自身的路径构造有问题")
	}
	if winPathKeyFoldOnly(protects[0]) == winPathKeyFoldOnly(shortVendor) {
		t.Fatalf("短名没有被规范化：%q", protects[0])
	}
}

// winPathKeyFoldOnly 只折叠大小写与分隔符（不剥 `\\?\`），供上面的模拟判定用。
func winPathKeyFoldOnly(path string) string {
	key := strings.ReplaceAll(filepath.Clean(strings.TrimSpace(path)), `\`, "/")
	for strings.Contains(key, "//") {
		key = strings.ReplaceAll(key, "//", "/")
	}
	return strings.ToLower(strings.TrimRight(key, "/"))
}

// A3C-W12：工具链缓存根必须进入 Windows 的可写面（与 Unix 后端同源）。
//
// 缓存清单来自 profile 侧的 toolchainCacheRoots（单一真源）；Windows 后端不消费
// Profile（它靠 ACL 而不是挂载），所以必须在 winWritableSurface 里显式并进来，
// 否则"沙箱里能跑 go build"只在 Unix 上成立。
func TestAuditWinToolchain_ToolchainCachesJoinWritableSurface(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	roots, _ := winWritableSurface(policy)
	if len(roots) <= 1 {
		t.Skipf("本机没有任何已存在的工具链缓存目录（%v）；Windows 真机上会有 %%LOCALAPPDATA%%\\go-build 等", roots)
	}
	caches, _ := toolchainCacheCandidates(toolchainCacheRoots(policy))
	if len(caches) == 0 {
		t.Skip("本机没有已存在的缓存根")
	}
	joined := strings.Join(roots, "\n")
	for _, cache := range caches {
		if !strings.Contains(joined, filepath.Base(cache.Root)) {
			t.Fatalf("缓存根 %q 没有进入可写面：%v", cache.Root, roots)
		}
	}
	// 关掉之后必须只剩策略根（一行开关就能完全回退）。
	off := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithToolchainCaches(false),
	)
	if offRoots, _ := winWritableSurface(off); len(offRoots) != 1 {
		t.Fatalf("WithToolchainCaches(false) 后仍有 %d 个可写根：%v", len(offRoots), offRoots)
	}
}

// A3C-W13：工作区为空时**不**追加缓存根 —— 空工作区必须继续 fail closed。
func TestAuditWinToolchain_NoRootsStaysFailClosed(t *testing.T) {
	empty := sandbox.New(sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	if _, err := buildWindowsConfinedCommand(`rig.exe`, []string{"cmd.exe"}, empty); err == nil {
		t.Fatal("没有策略根时仍构造出了 helper argv：workspace-write 的空工作区必须 fail closed")
	}
	if roots, _ := winWritableSurface(empty); len(roots) != 0 {
		t.Fatalf("空工作区不应有可写根：%v", roots)
	}
}

// A3C-W14（真机 CI 回归）：解决**写法的对称性** —— 保护路径的归属判定必须与
// 会话实际使用的工作区列表处于同一种写法。
//
// 真机证据（CI job 134761496038）：
//
//	windows_runtime_test.go:390: writing an existing protected file succeeded, want denial
//	windows_runtime_test.go:556: protect ledger = [], want ["C:\\Users\\RUNNER~1\\...\\.git"]
//
// 机制：`winResolveProtectedPaths` 把 `--protect` 规范化成长名、把归属工作区也记成
// 长名；而 `winACLSession` 是按**调用方传进来的**工作区列表逐个创建的
// （`protected.For(workspace)` 用 `winPathKey` 比对）。两者写法不同（8.3 短名
// `RUNNER~1` vs 长名 `runneradmin`）→ `For()` 返回空 → **保护静默失效**，
// 账本里一条都没有，`.git` 实际可写。
//
// 修法：`winValidateWorkspaces` 返回**规范化**后的工作区列表（与 plan 侧
// `winWritableSurface` 用同一个 `winCanonicalPath`），让解析、SID 派生、ACE 授权、
// `For()` 匹配四处看到同一个写法。本用例用注入的规范化函数在任意宿主复现。
func TestAuditWinToolchain_ProtectAttributionUsesSamePathForm(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 用符号链接当"另一种写法"：与 8.3 短名同构——**同一个对象、不同字符串**，
	// 而且两边 os.Stat 都成功（不像纯臆造的 `RUNNER~1` 路径，那会在 stat 上先失败，
	// 掩盖真正要验证的归属判定）。
	linkParent := t.TempDir()
	alias := filepath.Join(linkParent, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("无法创建符号链接（本用例需要两种写法指向同一目录）：%v", err)
	}

	// 调用方（plan）给的是 alias 写法；helper 侧校验后必须收敛到真实路径。
	workspaces, err := winValidateWorkspaces([]string{alias})
	if err != nil {
		t.Fatalf("winValidateWorkspaces: %v", err)
	}
	if len(workspaces) != 1 {
		t.Fatalf("workspaces = %v", workspaces)
	}
	if winPathKeyFoldOnly(workspaces[0]) == winPathKeyFoldOnly(alias) {
		// 不 Fatal：继续走到下面的归属判定，让"保护静默失效"这条**症状**也被报出来
		// （CI 上就是 `protect ledger = []` 与 `.git` 实际可写）。
		t.Errorf("工作区没有被规范化成真实路径：%q（仍是对调方传入的写法）", workspaces[0])
	}

	// 解析出的保护路径必须能归到那个工作区上（否则 For() 为空 → 保护静默失效）。
	resolution, err := winResolveProtectedPaths([]string{filepath.Join(alias, ".git")}, workspaces)
	if err != nil {
		t.Fatalf("winResolveProtectedPaths: %v", err)
	}
	if len(resolution.Enforced) != 1 {
		t.Fatalf("enforced = %+v, want 1（登记了但没归到任何工作区就是静默丢保护）", resolution)
	}
	assigned := resolution.For(workspaces[0])
	if len(assigned) != 1 {
		t.Fatalf("For(%q) = %v, want 1 条：归属判定与会话用的工作区写法不一致 → 真机上 .git 会变可写",
			workspaces[0], assigned)
	}
	if !samePathObject(assigned[0], gitDir) {
		t.Fatalf("归属的保护路径 = %q，want 指向 %q", assigned[0], gitDir)
	}
}

// winPathKeyFoldOnly 只折叠大小写与分隔符（不剥 `\\?\`），供上面的模拟判定用。
