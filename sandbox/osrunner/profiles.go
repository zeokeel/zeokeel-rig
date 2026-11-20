package osrunner

import (
	"os"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件是 Profile 到各后端参数的渲染层。所有渲染函数都只消费 BuildProfile 的
// 结果（见 profile.go），因此“策略等于什么”只有一份定义；这里只负责把它翻译成
// 每个后端的具体语法。渲染结果与 DSH（sandbox-local）逐 token / 逐字符对齐，
// 由 conformance_dsh_test.go 钉死。

// platformTempRoots 返回本平台的临时目录根，与 DSH 的 `os.tmpdir()` + `/tmp` 一致。
//
// 顺序固定为 `/tmp` 在前、`os.tmpdir()` 在后（DSH 的 SBPL 也是这个顺序），
// canonical + 去重：Linux 上两者通常是同一个路径，darwin 上 `/tmp` 会解析成
// `/private/tmp`、`os.tmpdir()` 是 `/var/folders/...`。Windows 只有 `os.tmpdir()`，
// 它由本后端的私有 temp 目录承担（见 windows 侧），这里不参与渲染。
func platformTempRoots() []string {
	return canonicalUnique([]string{"/tmp", os.TempDir()})
}

// profileTempRoots 返回**本策略**放行的平台临时目录根：两个开关
// （WithSlashTmp / WithTmpdirEnv，默认都开）在这里真的生效。
//
// 它是 platformTempRoots 的策略感知版本；两者用同一套规范化（canonical + 去重），
// 因此默认配置下结果一致（conformance 钉死的正是默认值）。
func profileTempRoots(policy *sandbox.Policy) []string {
	if policy == nil {
		return platformTempRoots()
	}
	var want []string
	if policy.IncludeSlashTmp() {
		want = append(want, "/tmp")
	}
	if policy.IncludeTmpdirEnv() {
		want = append(want, os.TempDir())
	}
	return canonicalUnique(want)
}

// firstPath 返回列表的第一项（空列表返回空串），用于"至多一个"的 profile 字段。
func firstPath(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

// WritableRoots 返回本策略实际可写的宿主路径，即 BuildProfile 的 ReadWrite。
//
// 兼容旧调用方：签名不变，但语义跟随 Profile。
//   - workspace-write：/dev/null + 平台临时目录 + 策略工作区根；
//   - read-only：只有 /dev/null；
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
// 与 DSH 逐 token 一致（顺序即挂载顺序）：
//
//	--ro-bind / /  --dev /dev  --unshare-pid  --proc /proc  --die-with-parent
//	[仅 workspace-write] --tmpfs /tmp  --bind <workspaceRoot> <workspaceRoot>
//
// read-only 档在策略带 scratch 时追加一条 `--bind <scratch> <scratch>`：/dev/null
// 由 `--dev` 提供（设备节点，不 bind），scratch 是目录，必须显式 bind 才可写。
// 没有 scratch 时这里**一个 token 都不多**（DSH 对齐由 conformance 钉死）。
//
// 返回 nil 表示“没有可渲染的 profile”（nil 策略或 danger-full-access）。
func BwrapArgs(policy *sandbox.Policy) []string {
	profile, err := BuildProfile(policy)
	if err != nil || profile == nil {
		return nil
	}
	args := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
	}
	if profile.Mode != sandbox.ModeWorkspaceWrite {
		for _, root := range workspaceBindRoots(profile) {
			args = append(args, "--bind", root, root)
		}
		return args
	}
	// 私有 tmpfs /tmp 只在 /tmp 真的进入可写面时挂（WithSlashTmp(false) 关掉它）。
	if profileHasSlashTmp(profile) {
		args = append(args, "--tmpfs", "/tmp")
	}
	for _, root := range workspaceBindRoots(profile) {
		args = append(args, "--bind", root, root)
	}
	return args
}

// profileHasSlashTmp 报告本 profile 是否把字面 /tmp 纳入可写面（= 要挂私有 tmpfs）。
func profileHasSlashTmp(profile *Profile) bool {
	if profile == nil {
		return false
	}
	for _, root := range profile.TempRoots {
		if sandbox.PathEqual(root, "/tmp") {
			return true
		}
	}
	return false
}

// workspaceBindRoots 返回需要 `--bind <p> <p>` 的目录（按策略顺序、已规范化）。
//
// 只取 Profile.ReadWrite 里"需要 bind"的部分：/dev/null 由 `--dev` 提供；平台
// 临时目录（Profile.TempRoots）由 `--tmpfs /tmp` 或宿主的 $TMPDIR 承载（不再
// bind 宿主临时目录，否则私有 tmpfs 就没意义了）。其余项——工作区根、追加可写根、
// read-only 的 scratch——都 bind。
// 工作区恰好等于 /tmp 时，`--bind` 在 `--tmpfs` 之后执行，会把工作区重新挂回来——
// 这正是 DSH 的语义（顺序固定：tmpfs 在前）。
func workspaceBindRoots(profile *Profile) []string {
	if profile == nil {
		return nil
	}
	out := make([]string, 0, len(profile.ReadWrite))
	for _, path := range profile.ReadWrite {
		if path == "/dev/null" {
			continue
		}
		if isAnyOf(path, profile.TempRoots) {
			continue
		}
		out = append(out, path)
	}
	return out
}

// isAnyOf 报告 path 是否等于 paths 中任意一个。
func isAnyOf(path string, paths []string) bool {
	for _, candidate := range paths {
		if sandbox.PathEqual(path, candidate) {
			return true
		}
	}
	return false
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
// 与 DSH 一致，**没有**任何"保护路径收窄"的展开：read-only 是 `--ro / --rw
// /dev/null`；workspace-write 再追加平台临时目录与工作区根。Landlock 的
// path-beneath 规则是并集语义（祖先授予的权利不会被更深的规则收窄），所以
// "根内只读子路径"本来就无法表达；对齐后这条限制连尝试都不做，`Profile.Protected`
// 只用于诊断。
func landlockGrants(profile *Profile) (readOnly, readWrite []string) {
	if profile == nil {
		return nil, nil
	}
	readOnly = append(readOnly, profile.ReadOnly...)
	readWrite = append(readWrite, profile.ReadWrite...)
	return canonicalUnique(readOnly), canonicalUnique(readWrite)
}

// SeatbeltProfile 返回 macOS sandbox-exec 的内联 SBPL profile。
//
// 与 DSH 的字符串逐字符一致：
//
//	(version 1) (allow default) (deny file-write*)
//	(allow file-write* (literal "/dev/null"))
//	[仅 workspace-write] (allow file-write* (subpath "<ws>") (subpath "<tmp>") (subpath "<tmpdir>"))
//
// 姿态是 allow-default + deny-write：DSH 用同样的取舍（deny-default 需要维护一份
// 随 macOS 版本漂移的平台默认白名单）。代价见 .audit/impl-osrunner.md：DSH 没有
// pty / system-fcntl / user-preference-write / 保护路径的 deny 规则。
//
// 返回空串表示没有可渲染的 profile（nil 策略、danger-full-access，或路径里含控制
// 字符——见 sbplRenderable）。
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
		`(allow file-write* (literal "/dev/null"))`,
	}
	// 目录放行规则：workspace-write 是工作区根 + 临时目录；read-only 只有 scratch
	// （没有 scratch 时一条都不渲染，与 DSH 的 read-only 逐字符一致）。
	if subpaths := seatbeltWritableSubpaths(profile); len(subpaths) > 0 {
		parts := make([]string, 0, len(subpaths))
		for _, path := range subpaths {
			parts = append(parts, "(subpath "+sbplString(path)+")")
		}
		forms = append(forms, "(allow file-write* "+strings.Join(parts, " ")+")")
	}
	return strings.Join(forms, " ")
}

// seatbeltWritableSubpaths 返回放行的目录 subpath：工作区根在前，平台临时目录在
// 后（与 DSH 的渲染顺序一致），去重保序。
//
// read-only 档不放行任何临时目录，只放行 scratch（若设了）。`/dev/null` 已由
// literal 规则覆盖，其余路径（保护路径、缓存根）不再放行也不再 deny。
func seatbeltWritableSubpaths(profile *Profile) []string {
	if profile == nil {
		return nil
	}
	out := make([]string, 0, len(profile.ReadWrite))
	add := func(path string) {
		for _, existing := range out {
			if sandbox.PathEqual(existing, path) {
				return
			}
		}
		out = append(out, path)
	}
	for _, workspace := range workspaceBindRoots(profile) {
		add(workspace)
	}
	if profile.Mode == sandbox.ModeWorkspaceWrite {
		for _, temp := range profile.TempRoots {
			add(temp)
		}
	} else if profile.Scratch != "" {
		add(profile.Scratch)
	}
	return out
}

// sbplRenderable 报告 profile 里所有会被渲染的路径都能安全放进 SBPL 字符串字面量。
//
// 保留这一层**纯健壮性**检查（与 DSH 的输出无关）：控制字符会让 sandbox-exec 的
// profile 解析失败，把原因变成一个指向 SBPL 语法的错误。`"` 与 `\` 由 sbplString
// 转义，不需要在这里判。
func sbplRenderable(profile *Profile) bool {
	if profile == nil {
		return false
	}
	for _, group := range [][]string{profile.ReadOnly, profile.ReadWrite} {
		for _, path := range group {
			if strings.ContainsAny(path, "\n\r\x00") {
				return false
			}
		}
	}
	return true
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
		return []string{"read-only file system", "permission denied", "operation not permitted"}
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
