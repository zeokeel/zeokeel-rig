package memvfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// relativeURIPath 把 URI 映射成"不许逃出存储根"的相对路径。
//
// 契约（memoryvfs.ParseURI）已经拒绝了 `..`、空段和大小写，这里再做一次兜底：
// Store 是公共实现，可能被直接喂进绕过 ParseURI 构造出来的 URI，路径拼接处不能
// 只信任上游。根 URI 没有条目文件，返回 ErrInvalidURI。
func relativeURIPath(uri memoryvfs.URI) (string, error) {
	raw := string(uri)
	if !strings.HasPrefix(raw, memoryvfs.Scheme) {
		return "", fmt.Errorf("%w: %q must start with %q", memoryvfs.ErrInvalidURI, raw, memoryvfs.Scheme)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(raw, memoryvfs.Scheme), "/")
	if path == "" {
		return "", fmt.Errorf("%w: the root has no storage path", memoryvfs.ErrInvalidURI)
	}
	segments := strings.Split(path, "/")
	for _, segment := range segments {
		if segment == "" {
			return "", fmt.Errorf("%w: empty path segment", memoryvfs.ErrInvalidURI)
		}
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: relative segment %q", memoryvfs.ErrInvalidURI, segment)
		}
		for _, r := range segment {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
				continue
			}
			return "", fmt.Errorf("%w: segment %q contains %q", memoryvfs.ErrInvalidURI, segment, r)
		}
	}
	return strings.Join(segments, "/"), nil
}

// resolvePathIn 在 root 之下解析 kind 子目录里的目标文件，并校验结果仍在 root 内。
func resolvePathIn(root, kind string, uri memoryvfs.URI, suffix string) (string, error) {
	rel, err := relativeURIPath(uri)
	if err != nil {
		return "", err
	}
	base := filepath.Join(root, kind)
	target := filepath.Join(base, filepath.FromSlash(rel)) + suffix
	check, err := filepath.Rel(base, target)
	if err != nil || filepath.IsAbs(check) || check == ".." ||
		strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s escapes the storage root", memoryvfs.ErrInvalidURI, uri)
	}
	return target, nil
}

// resolvePath 在存储根下解析目标文件。
func (s *Store) resolvePath(kind string, uri memoryvfs.URI, suffix string) (string, error) {
	if s.dir == "" {
		return "", errors.New("memvfs: memory store has no filesystem paths")
	}
	return resolvePathIn(s.dir, kind, uri, suffix)
}

// entryFilePath 返回条目文件路径（诊断/测试用）。URI 非法时返回空串。
func (s *Store) entryFilePath(uri memoryvfs.URI) string {
	path, err := s.resolvePath(entriesDirName, uri, ".json")
	if err != nil {
		return ""
	}
	return path
}

// writeJSONAtomic 把 value 编码后原子写入 path。
func writeJSONAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("memvfs: encode %s: %w", filepath.Base(path), err)
	}
	return writeFileAtomic(path, data)
}

// writeFileAtomic 用"同目录临时文件 → fsync → rename → fsync 目录"写文件，
// 崩溃时要么看到旧内容、要么看到完整新内容，不会出现半条记录。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, layoutPerm); err != nil {
		return fmt.Errorf("memvfs: create %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".memvfs-*.tmp")
	if err != nil {
		return fmt.Errorf("memvfs: create temporary file in %s: %w", dir, err)
	}
	tempName := temp.Name()
	discard := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	if err := temp.Chmod(filePerm); err != nil {
		discard()
		return fmt.Errorf("memvfs: chmod %s: %w", tempName, err)
	}
	if _, err := temp.Write(data); err != nil {
		discard()
		return fmt.Errorf("memvfs: write %s: %w", tempName, err)
	}
	if err := temp.Sync(); err != nil {
		discard()
		return fmt.Errorf("memvfs: fsync %s: %w", tempName, err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("memvfs: close %s: %w", tempName, err)
	}
	if err := os.Rename(tempName, path); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("memvfs: rename %s: %w", path, err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	// 新 URI 首次写入时 MkdirAll 会新建多层目录；只 fsync 末级目录不够，掉电后
	// 祖先目录项可能丢失。向上同步有限层（调用方路径都在存储根之下）。
	parent := filepath.Dir(dir)
	for depth := 0; depth < 4; depth++ {
		if parent == dir || parent == "." || parent == string(filepath.Separator) {
			break
		}
		if err := syncDir(parent); err != nil {
			return err
		}
		dir = parent
		parent = filepath.Dir(parent)
	}
	return nil
}

// syncDir 把目录项（rename/remove 的结果）也刷到磁盘。失败时返回错误，让调用方
// 知道"文件内容在、目录项可能没持久化"。
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("memvfs: open directory %s for fsync: %w", dir, err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		// 部分文件系统（例如某些网络盘）不支持目录 fsync；这不是数据损坏。
		if errors.Is(err, os.ErrInvalid) {
			return nil
		}
		return fmt.Errorf("memvfs: fsync directory %s: %w", dir, err)
	}
	return nil
}
