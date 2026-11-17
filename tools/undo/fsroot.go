package undo

// 本文件是 undo 与工作区之间的唯一文件访问层：所有读写都经 internal/fsguard 的
// traversal-safe 句柄（内部是 os.Root），因此"校验之后、落盘之前"被换成符号链接的
// 竞态由内核在系统调用时兜住，而不是靠 undo 自己做字符串检查。
//
// 这里刻意只暴露一个窄接口（fileRoot）：undo 的失败注入测试需要一个能在写路径上
// 制造错误的接缝，而生产路径只有 fsguard 一个实现。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/zeokeel/zeokeel-rig/internal/fsguard"
)

// fileRoot 是 undo 需要的最小文件访问面，由 internal/fsguard.Root 实现。
//
// 所有路径都是**相对工作区根的斜杠路径**；绝对路径与 ".." 由 fsguard 拒绝（ErrEscape）。
type fileRoot interface {
	ReadFile(rel string) ([]byte, error)
	ReadFileLimit(rel string, maxBytes int64) ([]byte, error)
	Stat(rel string) (fs.FileInfo, error)
	Lstat(rel string) (fs.FileInfo, error)
	Readlink(rel string) (string, error)
	WriteFileAtomic(rel string, data []byte, perm fs.FileMode) error
	Rename(oldRel, newRel string) error
	Remove(rel string) error
	MkdirAll(rel string, perm fs.FileMode) error
	Chmod(rel string, mode fs.FileMode) error
	// Name 是 Open 时的根目录（用于创建符号链接这类仍需绝对路径的回落实现，以及
	// 诊断文案）。
	Name() string
	Close() error
}

// 编译期断言：fsguard.Root 必须满足 undo 需要的接口。
var _ fileRoot = (*fsguard.Root)(nil)

// openRoot 打开一个受限句柄。
func openRoot(dir string) (fileRoot, error) { return fsguard.Open(dir) }

// errNotExist 报告 err 是否表示"路径不存在"（兼容 fsguard 归一化与 os 原生错误）。
func errNotExist(err error) bool {
	return errors.Is(err, fsguard.ErrNotExist) || errors.Is(err, fs.ErrNotExist)
}

// preState 是"回退执行之前该路径的样子"，用于失败时精确回滚。
//
// kind 为 0 表示路径不存在；只支持普通文件与符号链接（回退不会主动改写目录等
// 其他类型，遇到即报错）。
type preState struct {
	kind    entryKind
	mode    fs.FileMode
	content []byte
	target  string
}

// readPreState 读取路径当前状态（不跟随末尾链接）。
func readPreState(handle fileRoot, rel string) (preState, error) {
	info, err := handle.Lstat(rel)
	if err != nil {
		if errNotExist(err) {
			return preState{}, nil
		}
		return preState{}, err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, linkErr := handle.Readlink(rel)
		if linkErr != nil {
			return preState{}, linkErr
		}
		return preState{kind: kindSymlink, mode: info.Mode() & modeBits, target: target}, nil
	case info.Mode().IsRegular():
		data, readErr := handle.ReadFile(rel)
		if readErr != nil {
			return preState{}, readErr
		}
		return preState{kind: kindFile, mode: info.Mode() & modeBits, content: data}, nil
	default:
		return preState{}, fmt.Errorf("undo: %q is neither a regular file nor a symbolic link (%v)", rel, info.Mode())
	}
}

// restorePreState 把路径恢复成 pre 描述的样子（回滚用）。
func restorePreState(handle fileRoot, rel string, pre preState) error {
	switch pre.kind {
	case 0:
		if err := handle.Remove(rel); err != nil && !errNotExist(err) {
			return err
		}
		return nil
	case kindSymlink:
		if err := handle.Remove(rel); err != nil && !errNotExist(err) {
			return err
		}
		return writeSymlink(handle, rel, pre.target)
	default: // kindFile
		// 若当前位置是链接/目录（例如本次回退把它换成了链接），必须先删掉再写：
		// WriteFileAtomic 会写穿符号链接，直接写会改到链接目标上去。
		if info, err := handle.Lstat(rel); err == nil && !info.Mode().IsRegular() {
			if err := handle.Remove(rel); err != nil {
				return err
			}
		}
		return handle.WriteFileAtomic(rel, pre.content, pre.mode)
	}
}

// writeSymlink 在根内创建符号链接（rel 是要创建的链接，target 是它的原始文本）。
//
// fsguard 目前没有 Symlink 方法；如果将来的实现提供
// `Symlink(rel, target string) error`，这里会自动改用它。回落实现沿用
// WriteFileAtomic 的结构：父目录先建好，临时链接 + Rename（rename 作用于路径本身，
// 不会跟随链接），失败清理临时项。
func writeSymlink(handle fileRoot, rel, target string) error {
	if linker, ok := handle.(interface {
		Symlink(rel, target string) error
	}); ok {
		return linker.Symlink(rel, target)
	}
	parent := path.Dir(rel)
	if parent != "." && parent != "/" {
		if err := handle.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	tempRel := path.Join(parent, tempPrefix+randomToken()+".lnk")
	absolute := filepath.Join(handle.Name(), filepath.FromSlash(tempRel))
	if err := os.Symlink(target, absolute); err != nil {
		return fmt.Errorf("undo: create symbolic link %q: %w", rel, err)
	}
	if err := handle.Rename(tempRel, rel); err != nil {
		_ = handle.Remove(tempRel)
		return fmt.Errorf("undo: install symbolic link %q: %w", rel, err)
	}
	return nil
}

// randomToken 生成短随机串（临时文件后缀）。
func randomToken() string {
	var buffer [6]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(buffer[:])
}

// hashRel 经根句柄读文件并算 sha256。maxBytes 限制单次读取量（内存有界）。
func hashRel(handle fileRoot, rel string, maxBytes int64) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	data, err := handle.ReadFileLimit(rel, maxBytes)
	if err != nil {
		return sum, err
	}
	sum = sha256.Sum256(data)
	return sum, nil
}

// countLinesRel 经根句柄统计行数；超过 maxBytes 的文件无法整读，返回 0（近似）。
func countLinesRel(handle fileRoot, rel string, maxBytes int64) (int, error) {
	data, err := handle.ReadFileLimit(rel, maxBytes)
	if err != nil {
		if errors.Is(err, fsguard.ErrTooLarge) {
			return 0, nil
		}
		return 0, err
	}
	return lineCount(data), nil
}
