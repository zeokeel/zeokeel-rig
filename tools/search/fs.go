package search

// 本文件是搜索的输入面：在工作区根内遍历文件、跳过忽略目录/二进制/超限文件，
// 并提供“结果必须落在根内”的路径校验。所有行走都不跟随符号链接（filepath.WalkDir
// 只 lstat），因此结果集不会逃出根目录。

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ignoredDirNames 是索引时整棵跳过的目录名（构建产物与依赖目录）。
var ignoredDirNames = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	// .rig 是 kit 的状态目录（默认 spill 落盘位置 <Root>/.rig/spill）。索引它既没有
	// 检索价值，还会把溢出内容带进 git status 与 gitsnap 的 `git add -A` 快照。
	".rig": true,
}

// sourceFile 是一个候选文件。
type sourceFile struct {
	// abs 是绝对路径（用于读取）。
	abs string
	// rel 是相对工作区根、以 "/" 分隔的路径（用于输出与打分）。
	rel string
	// size 是文件字节数（内容模式用它做上限判断，避免读取超大文件）。
	size int64
}

// walkSourceFiles 遍历 baseDir 下的常规文件，按相对路径排序返回。
//
// skipHardlinks 为真时跳过硬链接数 > 1 的文件（见 WithHardlinkGuard 与包文档的
// "已知边界"）。
//
// baseDir 必须是已经解析过符号链接的真实目录；遍历本身不跟随链接。
//
// rootForRel 必须与 baseDir 处在**同一次符号链接解析之后**的坐标系里：调用方传
// 的是真实根（config.rootReal），不是用户写的路径。若传 rootAbs 而在真实路径上
// 行走，符号链接根（例如 ~/code → /data/code、macOS 的 /tmp）会让
// filepath.Rel 返回 "../real-workspace/src/x.go" 这种逃出根的相对路径——结果文本
// 因此会泄露根之外的目录结构。
func walkSourceFiles(ctx context.Context, rootForRel, baseDir string, skipHardlinks bool) ([]sourceFile, error) {
	files := make([]sourceFile, 0, 64)
	walkErr := filepath.WalkDir(baseDir, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if entry.IsDir() {
			if current != baseDir && ignoredDirNames[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			// 符号链接、设备、管道一律不索引：读取它们可能逃出根目录。
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil // 遍历过程中消失的文件对搜索不算错误
		}
		if skipHardlinks {
			if links, known := hardLinkCount(info); known && links > 1 {
				// 硬链接：同一 inode 还有别的名字，其中可能在根之外。默认放行
				// （兼容），守卫开启时跳过——无法从 lstat 判断"另一个名字在哪"，
				// 因此最保守的做法是整类跳过。
				return nil
			}
		}
		relative, relErr := filepath.Rel(rootForRel, current)
		if relErr != nil {
			return nil
		}
		files = append(files, sourceFile{abs: current, rel: filepath.ToSlash(relative), size: info.Size()})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(files, func(left, right int) bool { return files[left].rel < files[right].rel })
	return files, nil
}

// readSourceFile 读取一个候选文件的内容。
//
// 返回 (content, ok, err)：ok=false 表示这个文件应当被跳过（超过上限、二进制、
// 读取失败）。读取失败对内容检索不算致命错误（文件可能在遍历期间消失）。
func readSourceFile(ctx context.Context, file sourceFile, maxBytes int64) ([]byte, bool, error) {
	if maxBytes > 0 && file.size > maxBytes {
		return nil, false, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, false, ctxErr
	}
	content, err := os.ReadFile(file.abs)
	if err != nil {
		return nil, false, nil
	}
	if maxBytes > 0 && int64(len(content)) > maxBytes {
		return nil, false, nil
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, false, nil // 二进制文件（含 NUL）不索引
	}
	return content, true, nil
}

// withinRoot 报告 candidate 是否等于 root 或位于 root 之下（纯路径判断）。
func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if relative == "." {
		return true
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// resolveBaseDir 把工具参数里的 path 解析成根内的真实目录。
func resolveBaseDir(rootAbs, rootReal, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return rootReal, nil
	}
	// 分隔符归一：`\` 与 `/` 都当作路径分隔符（与 resolveMentionPath 一致）。
	// 这既让 Windows 风格的写法在 Linux 上也能用，也让 "..\..\etc" 这类
	// 反斜杠穿越在 join 之前就被规范化、交给下面的 withinRoot 检查，而不是被当成
	// 一个合法的文件名侥幸放过。
	normalized := filepath.FromSlash(strings.ReplaceAll(trimmed, `\`, "/"))
	if filepath.IsAbs(normalized) {
		return "", fmt.Errorf("search: path %q is outside the workspace root", raw)
	}
	joined := filepath.Join(rootAbs, filepath.Clean(normalized))
	if !withinRoot(rootAbs, joined) {
		return "", fmt.Errorf("search: path %q is outside the workspace root", raw)
	}
	info, err := os.Stat(joined)
	if err != nil {
		return "", fmt.Errorf("search: path %q: %w", raw, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("search: path %q is not a directory", raw)
	}
	// 解析符号链接后再确认一次：一个指向根外的链接不能变成搜索起点。
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("search: path %q: %w", raw, err)
	}
	if !withinRoot(rootReal, real) {
		return "", fmt.Errorf("search: path %q is outside the workspace root", raw)
	}
	return real, nil
}

// resolveMentionPath 把一条提及解析成根内的相对路径；不存在或落在根外返回 ok=false。
//
// baseDir 是本次调用的解析基准（`path` 参数解析出来的真实目录）：`@a.txt` +
// `path:"src"` 解析的是 `src/a.txt`，与 files/text 模式的 path 语义一致。
//
// 返回的显示路径**始终相对工作区根**（`src/a.txt`），这样模型看到的路径能直接
// 复用给其它工具；提及原文只在解析失败时用于展示（配合 " (missing)"）。
func resolveMentionPath(rootReal, baseDir, mentionPath string) (string, bool) {
	trimmed := strings.TrimSpace(mentionPath)
	if trimmed == "" {
		return "", false
	}
	normalized := strings.ReplaceAll(trimmed, `\`, "/")
	candidate := normalized
	if !filepath.IsAbs(filepath.FromSlash(normalized)) {
		candidate = filepath.Join(baseDir, filepath.FromSlash(normalized))
	}
	if !withinRoot(rootReal, candidate) {
		return trimmed, false
	}
	if _, err := os.Lstat(candidate); err != nil {
		return trimmed, false
	}
	relative, err := filepath.Rel(rootReal, candidate)
	if err != nil {
		return normalized, true
	}
	return filepath.ToSlash(relative), true
}
