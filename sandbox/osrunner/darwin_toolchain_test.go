//go:build darwin || linux

package osrunner

// macOS（Seatbelt/SBPL）"开发工具能力对齐"的纯逻辑用例：不执行任何沙箱，只断言
// **渲染出来的 profile 文本**在语义上应该长什么样。
//
// 为什么可以这样测：SBPL 是声明式语言，`(allow …)` / `(deny …)` 与 subpath/literal
// 匹配规则的语义完全可以静态推导；加上 SBPL 是 last-match-wins，规则**顺序**也是
// 可测试的契约。渲染层与平台无关（见 darwin.go 的说明），因此这些用例在 Linux 上
// 也跑——macOS 真机第一次跑就红了，正是因为此前没有 Linux 侧的同类覆盖。
//
// # 真机 macOS 上的两条注意
//
//  1. **路径必须先规范化**：`t.TempDir()` 在 macOS 返回 `/var/folders/…`，而渲染层
//     会 `EvalSymlinks` 成 `/private/var/folders/…`。断言必须比较规范化后的形式，
//     否则 Linux 上绿、真机上红（第一次真机 CI 就是这么红的）。
//  2. **不存在的目录不是缺陷**：干净机器上 `~/.cargo`、`~/Library/Developer` 等
//     根本没装。工具链缓存清单的断言只对"**存在却没被授权**"失败，其余只记录。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// canonicalPath 返回解析符号链接后的路径；解析失败时原样返回。
func canonicalAuditPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// rendered 是一次渲染的结果，带上策略本身供后续断言复用。
type rendered struct {
	profile string
	policy  *sandbox.Policy
	// root 是**规范化后**的工作区根（macOS 上 /var/folders/… → /private/var/folders/…）。
	root string
}

// render 构造一个 workspace-write 策略并渲染 SBPL。
func render(t *testing.T, mutate func(*sandbox.Policy, string)) rendered {
	t.Helper()
	root := canonicalAuditPath(t.TempDir())
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithProtectedPaths(filepath.Join(root, ".git")))
	if mutate != nil {
		mutate(policy, root)
	}
	profile := SeatbeltProfile(policy)
	if profile == "" {
		t.Fatal("SeatbeltProfile 返回空串（策略本应可渲染）")
	}
	return rendered{profile: profile, policy: policy, root: root}
}

// sbplSubpath 返回某个路径在 SBPL 里的 subpath 形式（用于包含性断言）。
func sbplSubpath(path string) string { return `(subpath ` + sbplString(path) + `)` }

// 姿态：`(allow default)` + `(deny file-write*)`。
//
// 这一条决定了"漏掉一条规则会怎样"：**读、sysctl、mach-lookup、exec 都是默认允许**
// 的，所以不需要（也**不应该**）为它们写白名单；需要白名单的只有**写**。
func TestSeatbeltPostureIsAllowDefaultWithWriteDeny(t *testing.T) {
	got := render(t, nil)

	if !strings.Contains(got.profile, "(allow default)") {
		t.Fatalf("缺少 (allow default)：本后端是「默认允许 + 只拦写」的姿态\n%s", got.profile)
	}
	if !strings.Contains(got.profile, "(deny file-write*)") {
		t.Fatalf("缺少 (deny file-write*)：写围栏就不存在了\n%s", got.profile)
	}
	// 读路径一律放行：漏掉 file-read-metadata 不会让 `ls` 失败（这是与
	// Codex 的 deny-default 姿态最大的不同）。
	for _, forbidden := range []string{"(deny file-read*", "(deny sysctl", "(deny mach-lookup", "(deny process-exec"} {
		if strings.Contains(got.profile, forbidden) {
			t.Fatalf("出现了 %q：本后端是 allow-default，任何 deny 读/exec 的规则都会直接削弱工具链能力\n%s", forbidden, got.profile)
		}
	}
	// 两条"写旁路"必须显式关掉：F_MAKECOMPRESSED/F_TRANSFEREXTENTS 能通过只读描述符
	// 改文件内容；CFPreferences 的写由未受限的 cfprefsd 代做。
	if !strings.Contains(got.profile, "(deny system-fcntl (fcntl-command 80 110))") {
		t.Fatalf("缺少 system-fcntl 旁路拦截（能通过只读 fd 改文件内容）\n%s", got.profile)
	}
	if !strings.Contains(got.profile, "(deny user-preference-write)") {
		t.Fatalf("缺少 user-preference-write 拦截（cfprefsd 会绕过文件写围栏）\n%s", got.profile)
	}
}

// 顺序契约：SBPL 是 last-match-wins，Protected 的 deny 必须排在可写 allow **之后**。
func TestSeatbeltProtectedDenyComesLast(t *testing.T) {
	got := render(t, func(_ *sandbox.Policy, root string) {
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
	})
	allowAt := strings.Index(got.profile, "(allow file-write* "+sbplSubpath(got.root))
	if allowAt < 0 {
		t.Fatalf("工作区没有出现在可写白名单里（root=%q）\n%s", got.root, got.profile)
	}
	denyAt := strings.Index(got.profile, "(deny file-write* "+sbplSubpath(filepath.Join(got.root, ".git")))
	if denyAt < 0 {
		t.Fatalf(".git 没有被保护（root=%q）\n%s", got.root, got.profile)
	}
	if denyAt < allowAt {
		t.Fatalf(".git 的 deny 排在工作区 allow 之前：last-match-wins 语义下保护会失效\n%s", got.profile)
	}
}

// 平台临时目录（macOS 上是 $TMPDIR=/var/folders/…/T，解析后是 /private/var/folders/…/T）
// 必须出现在可写白名单里：`mktemp`、`go build` 的中间文件、`npm` 的临时解包都依赖它。
func TestSeatbeltGrantsPlatformTempDir(t *testing.T) {
	got := render(t, nil)
	temp := canonicalAuditPath(os.TempDir())
	if !strings.Contains(got.profile, sbplSubpath(temp)) {
		t.Fatalf("平台临时目录 %q 没有被授权写（mktemp/go build 会失败）\n%s", temp, got.profile)
	}
	if strings.HasPrefix(temp, "/private/var/folders/") || strings.HasPrefix(temp, "/var/folders/") {
		t.Logf("macOS 形态的 TMPDIR: %s（已授权）", temp)
	}
}

// 符号链接必须在渲染前被解析：macOS 的 /tmp 是 /private/tmp 的符号链接，SBPL 的
// subpath 按**访问时看到的路径**匹配，不做符号链接解析——所以 profile 里必须是
// 真实路径，否则 `mktemp` 往 /var/folders/… 写会被拒。
func TestSeatbeltResolvesSymlinkedWritableRoots(t *testing.T) {
	realDir := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "tmp-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("本文件系统不支持符号链接: %v", err)
	}

	policy := sandbox.New(sandbox.WithRoots(link))
	profile := SeatbeltProfile(policy)
	if profile == "" {
		t.Fatal("SeatbeltProfile 返回空串")
	}
	resolved := canonicalAuditPath(realDir)
	if !strings.Contains(profile, sbplSubpath(resolved)) {
		t.Fatalf("profile 里没有解析后的真实路径 %q（符号链接形式会让 subpath 不命中）\n%s", resolved, profile)
	}
	if strings.Contains(profile, sbplSubpath(link)) {
		t.Fatalf("profile 里出现了未解析的符号链接路径 %q\n%s", link, profile)
	}
}

// 设备节点必须是 literal（或 pty 正则），**不能**整目录 subpath：
// `(subpath "/dev/pts")` 会让被约束进程能写同用户任意 pty（跨会话注入面）。
func TestSeatbeltDevicesAreLiteralsNotSubpaths(t *testing.T) {
	got := render(t, nil)
	for _, literal := range []string{`(literal "/dev/null")`, `(literal "/dev/ptmx")`, `(literal "/dev/tty")`} {
		if !strings.Contains(got.profile, literal) {
			t.Fatalf("缺少设备字面量 %s：shell 的 `>/dev/null` 会失败\n%s", literal, got.profile)
		}
	}
	if strings.Contains(got.profile, `(subpath "/dev`) {
		t.Fatalf("出现了 /dev 的 subpath 规则（会连带放开同目录其它设备）\n%s", got.profile)
	}
	if !strings.Contains(got.profile, `(regex #"^/dev/ttys[0-9]+$")`) {
		t.Fatalf("缺少 ttysN 正则：交互式终端会失败\n%s", got.profile)
	}
}

// 网络开关：Network=false 时必须整类 deny（含回环——本地 dev server 也会被拦，
// 这是与 Linux `--unshare-net` 对齐的部分）；true 时不出现 deny。
func TestSeatbeltNetworkSwitch(t *testing.T) {
	off := render(t, nil)
	if !strings.Contains(off.profile, "(deny network*)") {
		t.Fatalf("Network=false 却没有 (deny network*)\n%s", off.profile)
	}
	onRoot := canonicalAuditPath(t.TempDir())
	on := SeatbeltProfile(sandbox.New(sandbox.WithRoots(onRoot), sandbox.WithNetwork(true)))
	if strings.Contains(on, "(deny network*)") {
		t.Fatalf("Network=true 却仍然 deny network\n%s", on)
	}
}

// read-only：不得给出任何工作区/临时目录的可写授权，只剩设备节点。
func TestSeatbeltReadOnlyGrantsOnlyDevices(t *testing.T) {
	root := canonicalAuditPath(t.TempDir())
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
	profile := SeatbeltProfile(policy)
	if profile == "" {
		t.Fatal("SeatbeltProfile 返回空串")
	}
	if strings.Contains(profile, sbplSubpath(root)) {
		t.Fatalf("read-only 却授权了工作区写\n%s", profile)
	}
	if strings.Contains(profile, "(subpath") {
		t.Fatalf("read-only 不该有任何 subpath 写授权\n%s", profile)
	}
	if !strings.Contains(profile, `(literal "/dev/null")`) {
		t.Fatalf("read-only 也必须有 /dev/null\n%s", profile)
	}
}

// profile 是**单行内联**传给 `sandbox-exec -p` 的：含换行会变成语法错误。
func TestSeatbeltProfileIsSingleLine(t *testing.T) {
	got := render(t, nil)
	if strings.ContainsAny(got.profile, "\n\r") {
		t.Fatalf("profile 含换行（-p 内联传参会解析失败）: %q", got.profile)
	}
	// 含控制字符的路径 → 必须 fail closed（返回空串），而不是渲染出坏 profile。
	// 注意：路径必须**真实存在**，否则会在 canonicalSplit 阶段就被丢弃，测不到
	// sbplRenderable 这道闸。
	bad := filepath.Join(t.TempDir(), "bad\nname")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Skipf("本文件系统不允许路径含换行: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(bad))
	if got := SeatbeltProfile(policy); got != "" {
		t.Fatalf("含换行的路径没有被拒绝（fail closed）: %q", got)
	}
}

// 跨后端一致性：bwrap 的每个 `--bind` 路径都必须在 Seatbelt 的可写白名单里。
// 两个后端都从同一个 BuildProfile 派生，这条用例防止有人只改一边。
func TestSeatbeltCoversEveryBwrapBind(t *testing.T) {
	root := canonicalAuditPath(t.TempDir())
	extra := canonicalAuditPath(t.TempDir())
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithWritableRoots(extra))
	profile := SeatbeltProfile(policy)
	args := BwrapArgs(policy)

	checked := 0
	for index := 0; index < len(args)-1; index++ {
		if args[index] != "--bind" {
			continue
		}
		checked++
		path := args[index+1]
		if !strings.Contains(profile, sbplSubpath(path)) {
			t.Fatalf("bwrap 绑定了 %q，但 Seatbelt profile 没有对应可写授权\n%s", path, profile)
		}
	}
	if checked == 0 {
		t.Fatal("没有检查到任何 --bind：用例失去意义")
	}
}

// 工具链缓存覆盖：**只对"存在却没被授权"失败**。
//
// 干净机器上 `~/.cargo`、`~/Library/Developer` 之类根本没装（GitHub runner 就是
// 这样），那是环境而不是缺陷；而"已经存在的缓存目录没被授权"是真缺陷——用户跑
// `cargo build` 会因为写缓存失败而中断。
func TestSeatbeltToolchainCacheCoverage(t *testing.T) {
	got := render(t, nil)
	roots := toolchainCacheRoots(got.policy)
	if len(roots) == 0 {
		t.Skip("本平台没有工具链缓存清单（或策略关闭了 IncludeToolchainCaches）")
	}
	covered, absent := 0, 0
	for _, root := range roots {
		path := canonicalAuditPath(root.Root)
		_, statErr := os.Stat(path)
		exists := statErr == nil
		inProfile := strings.Contains(got.profile, sbplSubpath(path))
		switch {
		case exists && !inProfile:
			t.Errorf("已存在的工具链缓存 %q 没有被授权写（真实缺陷：工具会因写缓存失败而中断）", path)
		case !exists && inProfile:
			// 行为已按"可写 = 可创建"修正（干净机器首次 `cargo build` 需要它）：
			// Seatbelt 的 SBPL subpath 规则按**访问时**求值，所以尚不存在的缓存根
			// 也可以直接授权。这条不再是缺陷，而是与 bwrap/Landlock/ACL 的差异点
			//（它们要靠 Confine 前 MkdirAll 把目录补出来）。
			t.Logf("尚不存在的缓存根 %q 也被授权（SBPL 按访问时求值；其余后端由 Confine 前建目录补齐）", path)
			absent++
		case exists:
			covered++
			t.Logf("已覆盖: %s", path)
		default:
			absent++
			t.Logf("环境：%q 不存在（该工具尚未安装/首次运行；父目录可写性见下一条用例）", path)
		}
	}
	t.Logf("工具链缓存：%d 个已存在且已授权 / %d 个本机不存在", covered, absent)
}

// 干净机器上的真实问题：`go build` 会在**不存在**的缓存目录上就地创建
// （macOS 是 `~/Library/Caches/go-build`，Linux 是 `~/.cache/go-build`）。
// 创建要求**父目录**可写——这不是"目录存在与否"的问题，而是父目录在不在可写面。
//
// 断言：目录存在 → 必须在可写白名单里（否则 go 一定失败）；目录不存在 →
// 记录父目录是否可写，不可写就是产品缺口（见审计报告）。
func TestSeatbeltGoCacheParentIsWritable(t *testing.T) {
	got := render(t, nil)
	cacheRoot, err := os.UserCacheDir()
	if err != nil || cacheRoot == "" {
		t.Skipf("拿不到用户缓存目录: %v", err)
	}
	cacheRoot = canonicalAuditPath(cacheRoot)
	parent := filepath.Dir(cacheRoot)
	inProfile := strings.Contains(got.profile, sbplSubpath(cacheRoot))
	_, statErr := os.Stat(cacheRoot)

	switch {
	case statErr == nil && !inProfile:
		t.Errorf("用户缓存目录 %q 存在却没有被授权写：`go build` 无法创建 GOCACHE（真实缺陷）", cacheRoot)
	case statErr == nil:
		t.Logf("用户缓存目录 %q 已授权：go 能在里面创建 go-build", cacheRoot)
	default:
		if strings.Contains(got.profile, sbplSubpath(parent)) {
			t.Logf("用户缓存目录 %q 不存在，但父目录 %q 可写：go 能自行创建", cacheRoot, parent)
			return
		}
		t.Logf("缺口：用户缓存目录 %q 与父目录 %q 都不可写——干净机器上 `go build` 会在创建 GOCACHE 时失败（上层策略问题，见审计报告）", cacheRoot, parent)
	}
}

// 探测子进程必须是**绝对路径**：真机 CI 上 `sandbox-exec -p <profile> -- true`
// 因为 PATH 不可靠而报
//
//	sandbox-exec: execvp() of 'true' failed: No such file or directory   (exit 71)
//
// 于是整台机器被误判成"不可用"。这条用例把修复钉死：探测 argv 的每一项都不得
// 依赖 PATH 查找。
func TestSeatbeltProbeTailIsAbsolute(t *testing.T) {
	tail := seatbeltProbeTail()
	if len(tail) == 0 {
		t.Fatal("seatbeltProbeTail() 返回空")
	}
	if !filepath.IsAbs(tail[0]) {
		t.Fatalf("探测子进程 %q 不是绝对路径（会依赖 PATH 查找）", tail[0])
	}
	if _, err := os.Stat(tail[0]); err != nil {
		t.Fatalf("探测子进程 %q 不存在: %v", tail[0], err)
	}
}

// 回归：**PATH 为空**时探测也必须成功（复现真机 CI 的失败条件）。
//
// 真机 CI 里测试把 PATH 改成只剩假二进制目录，而探测用裸 "true" → execvp 找不到。
// 修复后探测走绝对路径，因此 PATH 再怎么变都不影响。这条用例在 Linux 上就能跑
// （探测本身不依赖 macOS：它只是 exec 一个可执行文件）。
func TestSeatbeltProbeWorksWithEmptyPath(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "sandbox-exec")
	// 假二进制要**模拟 sandbox-exec 的行为**，否则测不出探测的 PATH 依赖：
	//   - 先跳过 `-p <profile> --`，然后像真 sandbox-exec 一样 execvp 子进程
	//     （因此 PATH 为空时会失败——这正是真机 CI 的失败条件）；
	//   - 拒绝自检那一档（子进程是 /bin/sh）模拟 EPERM：非零退出、不写文件。
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ] && [ \"$1\" != \"--\" ]; do shift; done\n" +
		"[ $# -gt 0 ] && shift\n" +
		"if [ \"$1\" = \"/bin/sh\" ]; then\n" +
		"  echo \"sandbox-exec: Operation not permitted\" >&2; exit 1\n" +
		"fi\n" +
		"exec \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sandbox-exec: %v", err)
	}
	t.Setenv("PATH", "") // 极端条件：连 /usr/bin 都不在 PATH 里

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := probeSeatbelt(ctx, fake); err != nil {
		t.Fatalf("PATH 为空时探测失败——说明探测子进程仍在依赖 PATH 查找: %v", err)
	}
}
