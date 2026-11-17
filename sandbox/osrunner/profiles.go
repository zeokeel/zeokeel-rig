package osrunner

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件是 Profile 到各后端参数的渲染层。所有渲染函数都只消费 BuildProfile 的
// 结果（见 profile.go），因此“策略等于什么”只有一份定义；这里只负责把它翻译成
// 每个后端的具体语法。

// platformTempRoots 返回本平台允许写入的临时目录候选（未按策略过滤）。
func platformTempRoots() []string {
	roots := []string{os.TempDir()}
	if runtime.GOOS != "windows" {
		// /var/tmp 与 /tmp 一样是系统临时目录（跨重启保留，语义更保守），
		// 工具链和安装脚本经常用它；Codex 的 macOS scratch 策略把
		// /private/tmp 与 /private/var/tmp 都当作隐式可写根。
		roots = append(roots, "/tmp", "/var/tmp")
	}
	if runtime.GOOS == "darwin" {
		roots = append(roots, "/private/tmp", "/private/var/tmp")
	}
	return roots
}

// WritableRoots 返回本策略实际可写的宿主路径，即 BuildProfile 的 ReadWrite。
//
// 兼容旧调用方：签名不变，但语义跟随 Profile。
//   - workspace-write：策略根 + 平台临时目录；
//   - read-only：只有设备节点（/dev/null 等）——bwrap 的私有 tmpfs /tmp 不是宿主
//     路径，无法出现在这里，所以调用方不能把“只剩设备”理解成“没有任何可写位置”。
//   - danger-full-access：nil（没有渲染，也不需要）。
func WritableRoots(policy *sandbox.Policy) []string {
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		return nil
	}
	return append([]string(nil), profile.ReadWrite...)
}

// BwrapArgs 返回 bwrap 的 profile 参数（不含 bwrap 本身与结尾的 "--"）。
//
// 形状（顺序即挂载顺序，后面的挂载覆盖前面的）：
//
//	--ro-bind / /            整个宿主根只读
//	--tmpfs /tmp             PrivateTmp：私有 tmpfs（不落宿主）
//	--dev /dev               最小设备集
//	--remount-ro /dev        read-only：/dev 的文件系统必须只读（R-03/R-05）
//	--bind p p               ReadWrite 里的每个非 /、非 /dev/*、非 /tmp 路径
//	--ro-bind p p            Protected：在 rw bind 之后覆盖成只读
//	--unshare-net            !Network
//	--cap-drop ALL           丢弃全部 capability
//
// 返回 nil 表示“没有可渲染的 profile”（nil 策略或 danger-full-access）。
func BwrapArgs(policy *sandbox.Policy) []string {
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		return nil
	}
	args := []string{"--ro-bind", "/", "/"}
	if profile.PrivateTmp {
		args = append(args, "--tmpfs", "/tmp")
	}
	args = append(args,
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
		"--cap-drop", "ALL",
	)
	// read-only 下 --dev 挂的是一个新的、可写的 tmpfs：不加 remount-ro 时
	// `touch /dev/zzz` 会成功，与“除 /dev/null 等设备外没有写授权”矛盾。
	// remount-ro 只让文件系统元数据只读，写 /dev/null 这类字符设备不受影响。
	if profile.Mode == sandbox.ModeReadOnly {
		args = append(args, "--remount-ro", "/dev")
	}
	for _, path := range profile.ReadWrite {
		if !bwrapBindable(path) {
			continue
		}
		args = append(args, "--bind", path, path)
	}
	for _, path := range profile.Protected {
		args = append(args, "--ro-bind", path, path)
	}
	if !profile.Network {
		args = append(args, "--unshare-net")
	}
	return args
}

// bwrapBindable 报告一个 ReadWrite 路径是否需要单独 --bind。
//
// 跳过三类："/" 已经整体 ro-bind；/dev 下的设备由 --dev 提供（再 bind 会把宿主
// /dev 完整暴露出来，反而更宽）；/tmp 由私有 tmpfs 承载，bind 宿主 /tmp 会破坏
// PrivateTmp 的语义。
func bwrapBindable(path string) bool {
	switch path {
	case "", "/", "/tmp":
		return false
	}
	return path != "/dev" && !strings.HasPrefix(path, "/dev/")
}

// LandlockGrants 返回 Landlock 启动器的 --ro/--rw 授权集合（见 landlockGrants）。
func LandlockGrants(policy *sandbox.Policy) (readOnly, readWrite []string) {
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		return nil, nil
	}
	return landlockGrants(profile)
}

// landlockGrants 把 Profile 渲染成 Landlock 的 --ro/--rw 集合。
//
// # 保护路径为什么要“展开”而不是直接 --ro
//
// 直觉写法是“策略根放 --rw、保护路径放 --ro”，以为 path-beneath 规则取**最深
// 匹配**。实测不成立：Landlock 的规则是**并集**语义——只要某条祖先规则授予了
// 某个访问权，该权利就成立，更深、更严的子路径规则不会收窄它。在 Linux 6.8
// （ABI 4）上实测 `--rw /ws --ro /ws/.git` 时写 /ws/.git/x 依然成功。
//
// 因此把写授权“下移”：根 R 本身只读，R 的每个子项单独授权——保护链上的子项
// 只读并继续下移，其余子项读写，符号链接一概不授权（O_PATH 会跟随它，可能把
// 写权限带到宿主别处）。
//
// 代价（Landlock 的固有局限，必须让调用方知道）：一旦某个目录的子树里存在保护
// 路径，该目录本身就没有 MAKE_*/REMOVE_*，即**不能在它里面新建或删除条目**；
// 已经存在的直接子文件仍然可写。这是并集语义下唯一能真正保护子树的做法。
func landlockGrants(profile *Profile) (readOnly, readWrite []string) {
	readOnly = append(readOnly, profile.ReadOnly...)
	readOnly = append(readOnly, profile.Protected...)
	readWrite = append(readWrite, profile.ReadWrite...)
	// 平台临时目录（/tmp、$TMPDIR）绝不展开：它们是共享目录，枚举其子项既慢又
	// 不稳定（并发进程随时增删），而且"把 /tmp 顶层逐个授权"本来就是错的语义。
	// 当保护路径落在某个临时目录之下时，直接放弃该临时目录的写授权（fail
	// closed）——工作区根仍会按普通情况展开授权。
	tempRoots := platformTempRoots()
	for _, root := range profile.ReadWrite {
		if !pathHasProtectedBeneath(root, profile.Protected) {
			continue
		}
		readWrite = removePath(readWrite, root)
		if isPlatformTempRoot(root, tempRoots) {
			// 临时目录本身不展开（见上）；任务工作区即使恰好落在临时目录之下也
			// 照常展开，因为它是策略根、语义明确。
			continue
		}
		expandedWrite, expandedRead := expandLandlockAroundProtected(root, profile.Protected)
		readWrite = append(readWrite, expandedWrite...)
		readOnly = append(readOnly, expandedRead...)
	}
	return canonicalUnique(readOnly), canonicalUnique(readWrite)
}

// expandLandlockAroundProtected 把一个含保护路径的读写根展开成“根只读 + 子项
// 逐个授权”，只沿保护链继续下沉（不遍历整棵树）。
func expandLandlockAroundProtected(root string, protected []string) (readWrite, readOnly []string) {
	readOnly = append(readOnly, root)
	queue := []string{root}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(dir)
		if err != nil {
			// 读不到的目录不放行写：保护优先，宁可少授权。
			continue
		}
		for _, entry := range entries {
			child := filepath.Join(dir, entry.Name())
			switch {
			case pathWithinAny(child, protected):
				// 保护路径本身（或它之下的项）：只读，不再下沉。
				readOnly = append(readOnly, child)
			case pathHasProtectedBeneath(child, protected):
				// 保护链上的中间目录：只读，并继续下沉到它的子项。
				readOnly = append(readOnly, child)
				queue = append(queue, child)
			case entry.Type()&os.ModeSymlink != 0:
				// 符号链接不做写授权：O_PATH 会跟随链接目标。
			default:
				readWrite = append(readWrite, child)
			}
		}
	}
	return readWrite, readOnly
}

// removePath 返回去掉 target 之后的新切片（保序）。
func removePath(paths []string, target string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == target {
			continue
		}
		out = append(out, path)
	}
	return out
}

// pathWithin 报告 path 是否等于 root 或位于 root 之下（纯路径判断，不解析符号
// 链接）。
func pathWithin(root, path string) bool {
	if root == "" {
		return false
	}
	if root == string(filepath.Separator) {
		return strings.HasPrefix(path, root)
	}
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// pathWithinAny 报告 path 是否落在 dirs 中任意一个之内（含相等）。
func pathWithinAny(path string, dirs []string) bool {
	for _, dir := range dirs {
		if pathWithin(dir, path) {
			return true
		}
	}
	return false
}

// isPlatformTempRoot 报告 path 是否**就是**某个平台临时目录（不是它之下的路径）。
func isPlatformTempRoot(path string, tempRoots []string) bool {
	for _, root := range tempRoots {
		if filepath.Clean(root) == filepath.Clean(path) {
			return true
		}
	}
	return false
}

// pathHasProtectedBeneath 报告 dir 的子树里是否有保护路径（dir 本身被保护也算）。
func pathHasProtectedBeneath(dir string, protected []string) bool {
	if pathWithinAny(dir, protected) {
		return true
	}
	for _, path := range protected {
		if pathWithin(dir, path) {
			return true
		}
	}
	return false
}

// SeatbeltProfile 返回 macOS sandbox-exec 的内联 SBPL profile。
//
// 姿态：`(allow default)` + `(deny file-write*)`，再按 Profile.ReadWrite 用
// subpath 放行。选 allow-default 而不是 Codex 的 deny-default 是有意的取舍：
// deny-default 需要维护一份随 macOS 版本漂移的平台默认白名单（框架 map、mach
// 服务、系统读路径…），与"opt-in 的文件效果围栏"定位不符。代价是必须把
// **能代理文件写的非 file-write* 通道**显式关掉，见文件末尾的三条 deny。
//
// 形状：
//
//	(version 1)
//	(allow default)
//	(deny file-write*)
//	(allow file-write* (literal "/dev/null") (literal "/dev/ptmx") (literal "/dev/tty"))
//	(allow file-write* (regex #"^/dev/ttys[0-9]+$"))
//	(allow file-write* (subpath …)*)          ; /dev/* 已由上面的 pty 规则覆盖
//	(deny file-write* (subpath …)*)           ; Protected，必须在 allow 之后
//	(deny system-fcntl (fcntl-command 80 110))
//	(deny user-preference-write)
//	(deny network*)                            ; !Network
//
// 所有 deny 都排在 allow 之后：SBPL 是 last-match-wins。
//
// 返回空串表示没有可渲染的 profile（nil 策略、danger-full-access，或路径里含
// 控制字符）。
func SeatbeltProfile(policy *sandbox.Policy) string {
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		return ""
	}
	if !sbplRenderable(profile) {
		// 含换行/NUL 的路径会把 SBPL 字符串字面量撑成多行，让整份 profile 解析
		// 失败——那样**每一条**命令都会失败且没有任何线索。宁可返回空串让调用方
		// fail closed。
		return ""
	}
	forms := []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		// PTY：只放行伪终端设备本身。整目录 (subpath "/dev/pts") 会让被约束
		// 进程可以写**同用户任意** pty（跨会话注入面），所以按 Codex 的写法收窄
		// 成 /dev/ptmx + /dev/ttysN。
		"(allow pseudo-tty)",
		`(allow file-write* (literal "/dev/null") (literal "/dev/ptmx") (literal "/dev/tty"))`,
		`(allow file-write* (regex #"^/dev/ttys[0-9]+$"))`,
	}
	if writable := seatbeltWritableSubpaths(profile.ReadWrite); len(writable) > 0 {
		parts := make([]string, 0, len(writable))
		for _, path := range writable {
			parts = append(parts, "(subpath "+sbplString(path)+")")
		}
		forms = append(forms, "(allow file-write* "+strings.Join(parts, " ")+")")
	}
	// Protected 与 MissingProtected 都要渲染：SBPL 的 subpath 规则按访问时的
	// 路径求值，因此"先把尚不存在的 .git 保护起来"是有效的（Codex 的
	// missing_path_behavior 也是这个语义）。bwrap/Landlock 没有等价能力，它们的
	// MissingProtected 只用于诊断。
	protectedForms := make([]string, 0, len(profile.Protected)+len(profile.MissingProtected))
	for _, path := range append(append([]string(nil), profile.Protected...), profile.MissingProtected...) {
		protectedForms = append(protectedForms, "(subpath "+sbplString(path)+")")
	}
	if len(protectedForms) > 0 {
		forms = append(forms, "(deny file-write* "+strings.Join(protectedForms, " ")+")")
	}
	// system-fcntl：F_MAKECOMPRESSED(80) / F_TRANSFEREXTENTS(110) 能通过**只读
	// 描述符**改文件内容，绕过 file-write* 与 file-ioctl——Codex 的注释明确指出
	// "即使 deny-default 也挡不住"，所以我们这种 allow-default 姿态必须显式关。
	forms = append(forms, "(deny system-fcntl (fcntl-command 80 110))")
	// user-preference-write：CFPreferences 的写由未受限的 cfprefsd 代做，
	// `(deny file-write*)` 拦不住 ~/Library/Preferences；Codex 的
	// seatbelt_preferences_policy.sbpl 也只放 user-preference-read。
	forms = append(forms, "(deny user-preference-write)")
	if !profile.Network {
		forms = append(forms, "(deny network*)")
	}
	return strings.Join(forms, " ")
}

// sbplRenderable 报告 profile 里所有路径都能安全放进 SBPL 字符串字面量。
func sbplRenderable(profile *Profile) bool {
	for _, group := range [][]string{profile.ReadOnly, profile.ReadWrite, profile.Protected} {
		for _, path := range group {
			if strings.ContainsAny(path, "\n\r\x00") {
				return false
			}
		}
	}
	return true
}

// seatbeltWritableSubpaths 从 ReadWrite 里挑出需要 subpath 放行的路径。
//
// /dev/null 已由 literal 规则覆盖，这里跳过它让 profile 保持干净；其余保序。
func seatbeltWritableSubpaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		// 设备节点由 pty/null 的 literal/regex 规则覆盖；把 /dev 下的路径当
		// subpath 放行会顺带放开同目录的其它设备。
		if strings.HasPrefix(path, "/dev/") || path == "/dev" {
			continue
		}
		out = append(out, path)
	}
	return out
}

// sbplString 把一个路径转成 SBPL 字符串字面量。
func sbplString(path string) string {
	escaped := strings.ReplaceAll(path, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// DenialSignatures 返回某个 runner 的拒绝方言（stderr 子串，大小写不敏感）。
func DenialSignatures(runner string) []string {
	switch runner {
	case RunnerBwrap:
		return []string{"read-only file system"}
	case RunnerLandlock:
		return []string{"permission denied"}
	case RunnerSeatbelt:
		return []string{"operation not permitted"}
	case RunnerWindowsACL:
		return []string{"access is denied", "拒绝访问"}
	default:
		return nil
	}
}
