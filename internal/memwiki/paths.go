package memwiki

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CanonicalRoot 返回工作区根的规范形式：绝对路径 + 求值符号链接 + Clean。
//
// 必要性：同一个仓库可以经不同路径打开（`/work/repo`、`/work/repo/`、
// `~/src/../repo`、以及 `/work/link-to-repo`），它们必须是**同一份记忆**。
func CanonicalRoot(root string) (string, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return "", fmt.Errorf("memwiki: workspace root is empty")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("memwiki: resolve workspace root %q: %w", root, err)
	}
	absolute = filepath.Clean(absolute)
	// 根可能还不存在（宿主在建目录前就装配）：此时退回词法绝对路径。
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return absolute, nil
	}
	return filepath.Clean(resolved), nil
}

// WorkspaceID 把工作区根映射成稳定的目录名：`<base>-<sha256(canonical)[:8]>`。
//
// 为什么要带哈希：不同工作区可能同名（`~/work/api` 与 `~/oss/api`），只按 basename
// 分区会串味；而纯哈希不可读，出问题时人找不到目录。两者拼起来兼顾唯一与可读。
func WorkspaceID(root string) (string, error) {
	canonical, err := CanonicalRoot(root)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	base := filepath.Base(canonical)
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = "root"
	}
	// 目录名只保留安全字符：Windows 的盘符/空格在目录名里会带来麻烦。
	base = sanitizeSegment(base)
	if base == "" {
		base = "root"
	}
	return base + "-" + hex.EncodeToString(sum[:])[:8], nil
}

// sanitizeSegment 把任意 basename 收敛成可做目录名的形式。
func sanitizeSegment(name string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-.")
}

// DefaultStateDir 返回用户级状态目录（**不是**仓库目录）：
//
//   - `$XDG_STATE_HOME/rig`（XDG 规范，Linux 常见）；
//   - 否则 Windows：`%LOCALAPPDATA%\rig`；其它平台 `~/.local/state/rig`。
//
// 记忆不写进仓库：① 里面可能有不该提交给团队的内容（私有偏好、临时结论）；
// ② `.rig/` 出现在 `git status` 里会干扰"帮我提交"这类任务。
func DefaultStateDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); dir != "" {
		return filepath.Join(dir, "rig"), nil
	}
	if runtime.GOOS == "windows" {
		if dir := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); dir != "" {
			return filepath.Join(dir, "rig"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("memwiki: locate state dir: %w", err)
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "AppData", "Local", "rig"), nil
	}
	return filepath.Join(home, ".local", "state", "rig"), nil
}

// WikiDir 返回某个工作区的 wiki 目录：`<stateDir>/memory/<workspaceID>`。
func WikiDir(stateDir, workspaceRoot string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		defaultDir, err := DefaultStateDir()
		if err != nil {
			return "", err
		}
		stateDir = defaultDir
	}
	id, err := WorkspaceID(workspaceRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "memory", id), nil
}
