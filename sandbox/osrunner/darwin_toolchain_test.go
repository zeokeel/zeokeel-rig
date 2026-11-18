package osrunner

// macOS（Seatbelt/SBPL）"开发工具能力对齐"的纯逻辑用例：不执行任何沙箱，只断言
// **渲染出来的 profile 文本**在语义上应该长什么样。
//
// 为什么可以这样测：SBPL 是声明式语言，`(allow …)` / `(deny …)` 与 subpath/literal
// 匹配规则的语义完全可以静态推导；加上 SBPL 是 last-match-wins，规则**顺序**也是
// 可测试的契约。本文件在 Linux 上也能跑（渲染层与平台无关，见 darwin.go 的说明）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// renderProfile 构造一个 workspace-write 策略并渲染 SBPL。
func renderProfile(t *testing.T, mutate func(*sandbox.Policy, string)) (string, string) {
	t.Helper()
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithProtectedPaths(filepath.Join(root, ".git")))
	if mutate != nil {
		mutate(policy, root)
	}
	profile := SeatbeltProfile(policy)
	if profile == "" {
		t.Fatal("SeatbeltProfile 返回空串（策略本应可渲染）")
	}
	return profile, root
}

// sbplSubpath 返回某个路径在 SBPL 里的 subpath 形式（用于包含性断言）。
func sbplSubpath(path string) string { return `(subpath ` + sbplString(path) + `)` }

// 姿态：`(allow default)` + `(deny file-write*)`。
//
// 这一条决定了"漏掉一条规则会怎样"：**读、sysctl、mach-lookup、exec 都是默认允许**
// 的，所以不需要（也**不应该**）为它们写白名单；需要白名单的只有**写**。
func TestSeatbeltPostureIsAllowDefaultWithWriteDeny(t *testing.T) {
	profile, _ := renderProfile(t, nil)

	if !strings.Contains(profile, "(allow default)") {
		t.Fatalf("缺少 (allow default)：本后端是「默认允许 + 只拦写」的姿态\n%s", profile)
	}
	if !strings.Contains(profile, "(deny file-write*)") {
		t.Fatalf("缺少 (deny file-write*)：写围栏就不存在了\n%s", profile)
	}
	// 读路径一律放行：漏掉 file-read-metadata 不会让 `ls` 失败（这是与
	// Codex 的 deny-default 姿态最大的不同）。
	for _, forbidden := range []string{"(deny file-read*", "(deny sysctl", "(deny mach-lookup", "(deny process-exec"} {
		if strings.Contains(profile, forbidden) {
			t.Fatalf("出现了 %q：本后端是 allow-default，任何 deny 读/exec 的规则都会直接削弱工具链能力\n%s", forbidden, profile)
		}
	}
	// 两条"写旁路"必须显式关掉：F_MAKECOMPRESSED/F_TRANSFEREXTENTS 能通过只读描述符
	// 改文件内容；CFPreferences 的写由未受限的 cfprefsd 代做。
	if !strings.Contains(profile, "(deny system-fcntl (fcntl-command 80 110))") {
		t.Fatalf("缺少 system-fcntl 旁路拦截（能通过只读 fd 改文件内容）\n%s", profile)
	}
	if !strings.Contains(profile, "(deny user-preference-write)") {
		t.Fatalf("缺少 user-preference-write 拦截（cfprefsd 会绕过文件写围栏）\n%s", profile)
	}
}

// 顺序契约：SBPL 是 last-match-wins，Protected 的 deny 必须排在可写 allow **之后**。
func TestSeatbeltProtectedDenyComesLast(t *testing.T) {
	profile, root := renderProfile(t, func(policy *sandbox.Policy, root string) {
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
	})
	allowAt := strings.Index(profile, "(allow file-write* "+sbplSubpath(root))
	if allowAt < 0 {
		t.Fatalf("工作区没有出现在可写白名单里\n%s", profile)
	}
	denyAt := strings.Index(profile, "(deny file-write* "+sbplSubpath(filepath.Join(root, ".git")))
	if denyAt < 0 {
		t.Fatalf(".git 没有被保护\n%s", profile)
	}
	if denyAt < allowAt {
		t.Fatalf(".git 的 deny 排在工作区 allow 之前：last-match-wins 语义下保护会失效\n%s", profile)
	}
}

// 平台临时目录（macOS 上是 $TMPDIR=/var/folders/…/T，解析后是 /private/var/folders/…/T）
// 必须出现在可写白名单里：`mktemp`、`go build` 的中间文件、`npm` 的临时解包都依赖它。
func TestSeatbeltGrantsPlatformTempDir(t *testing.T) {
	profile, _ := renderProfile(t, nil)
	temp := os.TempDir()
	resolved, err := filepath.EvalSymlinks(temp)
	if err != nil {
		resolved = temp
	}
	if !strings.Contains(profile, sbplSubpath(resolved)) {
		t.Fatalf("平台临时目录 %q 没有被授权写（mktemp/go build 会失败）\n%s", resolved, profile)
	}
	// macOS 的真机路径形态：解析后应当落在 /private/var/folders 之下。
	if strings.HasPrefix(resolved, "/private/var/folders/") || strings.HasPrefix(resolved, "/var/folders/") {
		t.Logf("macOS 形态的 TMPDIR: %s（已授权）", resolved)
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
	resolvedReal, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if !strings.Contains(profile, sbplSubpath(resolvedReal)) {
		t.Fatalf("profile 里没有解析后的真实路径 %q（符号链接形式会让 subpath 不命中）\n%s", resolvedReal, profile)
	}
	if strings.Contains(profile, sbplSubpath(link)) {
		t.Fatalf("profile 里出现了未解析的符号链接路径 %q\n%s", link, profile)
	}
}

// 设备节点必须是 literal（或 pty 正则），**不能**整目录 subpath：
// `(subpath "/dev/pts")` 会让被约束进程能写同用户任意 pty（跨会话注入面）。
func TestSeatbeltDevicesAreLiteralsNotSubpaths(t *testing.T) {
	profile, _ := renderProfile(t, nil)
	for _, literal := range []string{`(literal "/dev/null")`, `(literal "/dev/ptmx")`, `(literal "/dev/tty")`} {
		if !strings.Contains(profile, literal) {
			t.Fatalf("缺少设备字面量 %s：shell 的 `>/dev/null` 会失败\n%s", literal, profile)
		}
	}
	if strings.Contains(profile, `(subpath "/dev`) {
		t.Fatalf("出现了 /dev 的 subpath 规则（会连带放开同目录其它设备）\n%s", profile)
	}
	if !strings.Contains(profile, `(regex #"^/dev/ttys[0-9]+$")`) {
		t.Fatalf("缺少 ttysN 正则：交互式终端会失败\n%s", profile)
	}
}

// 网络开关：Network=false 时必须整类 deny（含回环——本地 dev server 也会被拦，
// 这是与 Linux `--unshare-net` 对齐的部分）；true 时不出现 deny。
func TestSeatbeltNetworkSwitch(t *testing.T) {
	off, _ := renderProfile(t, nil)
	if !strings.Contains(off, "(deny network*)") {
		t.Fatalf("Network=false 却没有 (deny network*)\n%s", off)
	}
	onRoot := t.TempDir()
	on := SeatbeltProfile(sandbox.New(sandbox.WithRoots(onRoot), sandbox.WithNetwork(true)))
	if strings.Contains(on, "(deny network*)") {
		t.Fatalf("Network=true 却仍然 deny network\n%s", on)
	}
}

// read-only：不得给出任何工作区/临时目录的可写授权，只剩设备节点。
func TestSeatbeltReadOnlyGrantsOnlyDevices(t *testing.T) {
	root := t.TempDir()
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
	profile, _ := renderProfile(t, nil)
	if strings.ContainsAny(profile, "\n\r") {
		t.Fatalf("profile 含换行（-p 内联传参会解析失败）: %q", profile)
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
	root := t.TempDir()
	extra := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithWritableRoots(extra))
	profile := SeatbeltProfile(policy)
	args := BwrapArgs(policy)

	for index := 0; index < len(args)-1; index++ {
		if args[index] != "--bind" {
			continue
		}
		path := args[index+1]
		if !strings.Contains(profile, sbplSubpath(path)) {
			t.Fatalf("bwrap 绑定了 %q，但 Seatbelt profile 没有对应可写授权\n%s", path, profile)
		}
	}
}

// 工具链缺口（审计用，不失败）：把"常见开发工具的缓存目录"逐个对照 profile，
// 记录当前是否可写。修复后这些行会变成"已覆盖"，用例保持绿。
func TestSeatbeltToolchainCacheCoverage(t *testing.T) {
	profile, _ := renderProfile(t, nil)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("拿不到 HOME: %v", err)
	}
	candidates := map[string]string{
		"GOCACHE":        filepath.Join(home, ".cache", "go-build"),
		"GOMODCACHE":     filepath.Join(home, "go", "pkg", "mod"),
		"GOENV":          filepath.Join(home, "Library", "Application Support", "go", "env"),
		"npm":            filepath.Join(home, ".npm"),
		"pip":            filepath.Join(home, ".cache", "pip"),
		"cargo":          filepath.Join(home, ".cargo"),
		"rustup":         filepath.Join(home, ".rustup"),
		"maven":          filepath.Join(home, ".m2"),
		"Library/Caches": filepath.Join(home, "Library", "Caches"),
		"DerivedData":    filepath.Join(home, "Library", "Developer", "Xcode", "DerivedData"),
		"CoreSimulator":  filepath.Join(home, "Library", "Developer", "CoreSimulator"),
		"gradle":         filepath.Join(home, ".gradle"),
	}
	covered, missing := 0, 0
	for name, path := range candidates {
		if strings.Contains(profile, sbplSubpath(path)) {
			covered++
			t.Logf("已覆盖: %-14s %s", name, path)
			continue
		}
		missing++
		t.Logf("缺口(不可写): %-14s %s", name, path)
	}
	t.Logf("工具链缓存覆盖：%d 已授权 / %d 缺口", covered, missing)
	// 唯一硬断言：工作区本身必须可写（防止 profile 退化成空壳）。
	if !strings.Contains(profile, "(allow file-write*") {
		t.Fatalf("profile 里没有任何 file-write 授权\n%s", profile)
	}
}
