package osrunner

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// WritableRoots 返回 workspace-write 模式允许写入的规范路径：策略的全部根加
// 平台临时目录。只读模式返回 nil（除了 /dev/null 之外没有写授权）。
//
// 所有路径都做 Abs、Clean 与（存在时）EvalSymlinks，因此 profile 匹配与
// 平台实际路径一致（例如 macOS 上 /tmp 实际是 /private/tmp）。
func WritableRoots(policy *sandbox.Policy) []string {
	if policy == nil || policy.ReadOnly() || policy.Mode() == sandbox.ModeReadOnly {
		return nil
	}
	roots := append([]string(nil), policy.Roots()...)
	roots = append(roots, platformTempRoots()...)
	return canonicalUnique(roots)
}

// platformTempRoots 返回本平台允许写入的临时目录候选。
func platformTempRoots() []string {
	roots := []string{os.TempDir()}
	if runtime.GOOS != "windows" {
		roots = append(roots, "/tmp")
	}
	if runtime.GOOS == "darwin" {
		roots = append(roots, "/private/tmp")
	}
	return roots
}

// BwrapArgs 返回 bwrap 的 profile 参数（不含 bwrap 本身与结尾的 "--"）。
//
// read-only 只读绑定整个宿主根，并由 --dev 提供可写的 /dev/null；
// workspace-write 额外挂一个临时 /tmp，并把策略根以读写 bind 挂回原路径。
func BwrapArgs(policy *sandbox.Policy) []string {
	args := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
	}
	if policy == nil || policy.ReadOnly() || policy.Mode() != sandbox.ModeWorkspaceWrite {
		return args
	}
	args = append(args, "--tmpfs", "/tmp")
	for _, root := range policy.Roots() {
		args = append(args, "--bind", root, root)
	}
	return args
}

// LandlockGrants 返回 Landlock 启动器的 --ro/--rw 授权集合。
//
// read-only 授予整个 "/" 的读+执行，并只给 /dev/null 写；workspace-write
// 额外把平台临时目录与策略根加入读写给。
func LandlockGrants(policy *sandbox.Policy) (readOnly, readWrite []string) {
	readOnly = []string{"/"}
	readWrite = []string{"/dev/null"}
	if policy != nil && !policy.ReadOnly() && policy.Mode() == sandbox.ModeWorkspaceWrite {
		readWrite = append(readWrite, platformTempRoots()...)
		readWrite = append(readWrite, policy.Roots()...)
	}
	return canonicalUnique(readOnly), canonicalUnique(readWrite)
}

// SeatbeltProfile 返回 macOS sandbox-exec 的内联 SBPL profile。
//
// 它是 allow-default + deny file-write*，再按 WritableRoots 用 subpath 放行；
// /dev/null 始终单独放行，否则 shell 的 >/dev/null 会失败。
func SeatbeltProfile(policy *sandbox.Policy) string {
	forms := []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(allow file-write* (literal "/dev/null"))`,
	}
	if roots := WritableRoots(policy); len(roots) > 0 {
		parts := make([]string, 0, len(roots))
		for _, root := range roots {
			parts = append(parts, "(subpath "+sbplString(root)+")")
		}
		forms = append(forms, "(allow file-write* "+strings.Join(parts, " ")+")")
	}
	return strings.Join(forms, " ")
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

// canonicalUnique 把路径规范化为真实绝对路径并去重；不存在的候选被丢弃。
func canonicalUnique(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = filepath.Clean(real)
		}
		if _, err := os.Stat(abs); err != nil {
			continue
		}
		if _, exists := seen[abs]; exists {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	return out
}
