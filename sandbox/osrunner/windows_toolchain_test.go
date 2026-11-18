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

func winPlanKeySet(plan winACLPlan) map[string]bool {
	out := make(map[string]bool, len(plan.Workspaces))
	for _, workspace := range plan.Workspaces {
		out[winPathKey(workspace)] = true
	}
	return out
}

func winPlanHas(plan winACLPlan, path string) bool {
	return winPlanKeySet(plan)[winPathKey(path)]
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

// A3C-W2：工具链缓存目录的可写面由 `osrunner-profile` 在 profile.go 统一实现。
//
// 本侧（windows_*.go）只负责"把 AdditionalWritableRoots 如实下发"（A3C-W1）。
// 等 profile 侧落盘 `toolchainCacheCandidates` 之后，这里应改成断言 Windows 名单
// （GOCACHE/GOMODCACHE/npm/pip/cargo/m2/gradle/nuget）确实出现在可写面里。
//
// 保留这张清单是为了让未来的改动有对照物：它刻意只列**缓存子目录**，不含
// %APPDATA%、%USERPROFILE%、.cargo 根、.m2 根（那里有 .npmrc / credentials.toml /
// settings.xml）。
func TestAuditWinToolchain_ToolchainCachesPendingProfileSide(t *testing.T) {
	t.Skip("工具链缓存由 osrunner-profile 在 profile.go 统一实现（toolchainCacheCandidates）；" +
		"Windows 侧只保证 AdditionalWritableRoots 如实下发（见 A3C-W1）。" +
		"profile 侧落盘后把本用例改为正向断言，名单见 winToolchainCacheDirs。")
	root := t.TempDir()
	cacheBase := t.TempDir()
	caches := make([]string, 0, len(winToolchainCacheDirs))
	for _, entry := range winToolchainCacheDirs {
		dir := filepath.Join(cacheBase, strings.NewReplacer(`\`, "/", ":", "_").Replace(entry.path))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		caches = append(caches, dir)
	}
	options := []sandbox.Option{
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithWritableRoots(caches...),
	}
	plan := winPlanFromPolicy(t, sandbox.New(options...))
	var missing []string
	for index, cache := range caches {
		if !winPlanHas(plan, cache) {
			missing = append(missing, winToolchainCacheDirs[index].label+" :: "+winToolchainCacheDirs[index].path)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("Windows 可写面里缺少 %d/%d 个工具链缓存：\n  %s", len(missing), len(caches), strings.Join(missing, "\n  "))
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
		if winPathWithinFold(escape, filepath.FromSlash(strings.TrimPrefix(path, "/"))) ||
			strings.Contains(path, "esc") {
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
			if winPathKey(protect) == winPathKey(want) {
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
		if winPathKey(protect) == winPathKey(missing) {
			t.Fatalf("不存在的只读子路径被下发了：helper 会 fail closed、所有命令都跑不起来（%v）", plan.Protected)
		}
	}
	found := false
	for _, protect := range plan.Protected {
		if winPathKey(protect) == winPathKey(present) {
			found = true
		}
	}
	if !found {
		t.Fatalf("存在的只读子路径必须仍然下发：%v", plan.Protected)
	}
}
