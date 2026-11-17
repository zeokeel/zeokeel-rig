package fsguard

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

// Root 是受限于一棵目录树的安全句柄。它包装 os.Root：所有操作都由内核在系统调用
// 时保证不逃出根，因此没有"先检查后使用"的窗口。可被多个 goroutine 同时使用。
//
// 零值（或 nil）不是可用句柄：它的所有方法都返回错误而不是 panic，因此宿主可以把
// *Root 当成可选字段直接传递。
type Root struct {
	// dir 是 Open 时传入并清理后的根路径（供错误文案与诊断）。
	dir string
	// mu 只保护 handle 字段，使 Close 与并发调用之间不会读到撕裂的指针。
	// 操作本身依赖 os.Root 的并发安全，不持锁运行。
	mu sync.RWMutex
	// handle 是 os.Root；nil 表示未打开或已关闭。
	handle *os.Root
}

// 归一化的错误分类。
var (
	// ErrTooLarge 表示文件超过 ReadFileLimit 的上限（此时不会整读文件）。
	ErrTooLarge = errors.New("fsguard: file exceeds the size limit")
	// ErrEscape 表示路径逃出了根（绝对路径、".." 逃逸、或经符号链接逃逸）。
	ErrEscape = errors.New("fsguard: path escapes the root")
	// ErrNotExist 表示目标不存在；它包装 os.ErrNotExist，因此
	// errors.Is(err, fsguard.ErrNotExist) 与 errors.Is(err, os.ErrNotExist) 都成立。
	ErrNotExist = fmt.Errorf("fsguard: file does not exist: %w", os.ErrNotExist)

	errNilRoot       = errors.New("fsguard: root is nil (not opened)")
	errClosedRoot    = errors.New("fsguard: root is closed")
	errEmptyPath     = errors.New("fsguard: empty path")
	errAbsolutePath  = errors.New("fsguard: absolute paths are not allowed")
	errRootTarget    = errors.New("fsguard: the root itself cannot be modified")
	errEscapeTarget  = errors.New("fsguard: path escapes the root")
	errDirectory     = errors.New("fsguard: target is a directory")
	errTooManyLinks  = errors.New("fsguard: too many levels of symbolic links")
	errSymlinkTarget = errors.New("fsguard: refusing to operate on a symbolic link (pass its target explicitly)")
)

// opError 是一次失败的根内操作。它保留底层错误（可 errors.As 到 *os.PathError，
// 因此 os.ErrPermission/os.ErrExist 等的可判性不丢），并附加归一化分类
// （ErrEscape / ErrNotExist / ErrTooLarge），使调用方能在跨平台、跨错误源的前提下
// 用 errors.Is 判断"是不是逃逸"。
type opError struct {
	op   string
	path string
	kind error
	err  error
}

func (e *opError) Error() string {
	var builder strings.Builder
	builder.WriteString("fsguard: ")
	builder.WriteString(e.op)
	if e.path != "" {
		builder.WriteString(" ")
		builder.WriteString(fmt.Sprintf("%q", e.path))
	}
	if e.err != nil {
		builder.WriteString(": ")
		builder.WriteString(e.err.Error())
	}
	return builder.String()
}

// Unwrap 同时暴露分类与底层错误：errors.Is(err, ErrEscape) 与
// errors.As(err, &pathError) 都能命中。
func (e *opError) Unwrap() []error {
	if e.kind == nil {
		if e.err == nil {
			return nil
		}
		return []error{e.err}
	}
	if e.err == nil || e.err == e.kind {
		return []error{e.kind}
	}
	return []error{e.kind, e.err}
}

// Open 打开 dir 作为根。dir 会被 Abs+Clean；不解析符号链接（由 os.Root 语义决定，
// 因此即使根本身是符号链接，也是"以链接解析后的目录为根"，且事后移动该目录不会
// 改变句柄指向）。传空串或非目录会返回错误。
func Open(dir string) (*Root, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("fsguard: empty root directory")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("fsguard: resolve root %q: %w", dir, err)
	}
	absolute = filepath.Clean(absolute)
	handle, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("fsguard: open root %q: %w", absolute, err)
	}
	return &Root{dir: absolute, handle: handle}, nil
}

// Close 关闭句柄。关闭后再次 Close 是幂等的（返回 nil），其它方法返回错误。
func (r *Root) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	handle := r.handle
	r.handle = nil
	r.mu.Unlock()
	if handle == nil {
		return nil
	}
	return handle.Close()
}

// Name 返回 Open 时传入并清理后的根路径（供错误文案与诊断，不保证是真实路径）。
func (r *Root) Name() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// acquire 取出可用的 os.Root 句柄；零值/已关闭时返回错误而不是 panic。
func (r *Root) acquire() (*os.Root, error) {
	if r == nil {
		return nil, errNilRoot
	}
	r.mu.RLock()
	handle := r.handle
	r.mu.RUnlock()
	if handle == nil {
		return nil, errClosedRoot
	}
	return handle, nil
}

// normalize 把根相对路径清理成可交给 os.Root 的形式，并挡住绝对路径与 ".." 逃逸。
// allowRoot 为 true 时 "."（根本身）合法（读操作），否则返回 ErrEscape。
func normalize(op, rel string, allowRoot bool) (string, error) {
	if rel == "" {
		return "", &opError{op: op, path: rel, kind: ErrEscape, err: errEmptyPath}
	}
	if looksAbsolute(rel) {
		return "", &opError{op: op, path: rel, kind: ErrEscape, err: errAbsolutePath}
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." {
		if allowRoot {
			return ".", nil
		}
		return "", &opError{op: op, path: rel, kind: ErrEscape, err: errRootTarget}
	}
	if escapesRelative(clean) {
		return "", &opError{op: op, path: rel, kind: ErrEscape, err: errEscapeTarget}
	}
	return clean, nil
}

// looksAbsolute 跨平台判断"绝对路径"：除了 filepath.IsAbs，还识别 Windows 盘符与
// UNC（"C:\x"、"\\server\share"）。这些在 Linux 上不是绝对路径，但同一份代码迁到
// Windows 后语义会突变——统一拒绝比"在某个平台上悄悄合法"安全。
func looksAbsolute(rel string) bool {
	if filepath.IsAbs(rel) {
		return true
	}
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return true
	}
	if len(rel) >= 2 && rel[1] == ':' {
		// Windows 的 "C:"、"C:\\x" 是盘符路径（drive-relative 也算逃逸）；但在
		// POSIX 上 "a:b.txt" 是合法文件名，不能一律拒绝。因此只在 Windows、或盘符
		// 后紧跟分隔符时判定为绝对路径。
		if runtime.GOOS == "windows" || (len(rel) > 2 && (rel[2] == '/' || rel[2] == '\\')) {
			return true
		}
	}
	return false
}

// escapesRelative 报告一个已 Clean 的相对路径是否指向根之外。
func escapesRelative(clean string) bool {
	if clean == ".." {
		return true
	}
	return strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// wrap 把底层错误归一化：逃逸 → ErrEscape，不存在 → ErrNotExist，其余原样保留。
func wrap(op, path string, err error) error {
	if err == nil {
		return nil
	}
	kind := error(nil)
	switch {
	case isEscapeError(err):
		kind = ErrEscape
	case errors.Is(err, os.ErrNotExist):
		kind = ErrNotExist
	}
	return &opError{op: op, path: path, kind: kind, err: err}
}

// isEscapeError 识别 os.Root 的逃逸错误。
//
// stdlib 没有导出该哨兵（os.errPathEscapes 是包私有的），唯一稳定的信号是它固定的
// 英文文案 "path escapes from parent"。判据是**某个被包装层的错误文本完全等于**
// 这句话，而不是"包含"：文件名本身可能包含这段文字（例如用户真建了一个叫
// "path escapes from parent" 的文件），用包含判断会把一次 NotExist 误判成逃逸。
//
// 测试在三个平台上锁定了这条归一化：Go 若改文案，CI 会立刻失败，而不是悄悄退化成
// "逃逸错误不可识别"。
func isEscapeError(err error) bool {
	const escapeMessage = "path escapes from parent"
	for current := err; current != nil; current = errors.Unwrap(current) {
		if current.Error() == escapeMessage {
			return true
		}
	}
	return false
}

// escapeError 构造一个逃逸分类错误（用于本包自己做的路径判定）。
func escapeError(op, path string, err error) error {
	return &opError{op: op, path: path, kind: ErrEscape, err: err}
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// ReadFile 读取 rel 指向的文件的全部内容（跟随符号链接）。
func (r *Root) ReadFile(rel string) ([]byte, error) {
	handle, err := r.acquire()
	if err != nil {
		return nil, err
	}
	path, err := normalize("read", rel, true)
	if err != nil {
		return nil, err
	}
	data, err := handle.ReadFile(path)
	if err != nil {
		return nil, wrap("read", rel, err)
	}
	return data, nil
}

// ReadFileLimit 与 ReadFile 相同，但超过 maxBytes 时返回 ErrTooLarge，且**不整读**：
// 普通文件先用 Stat 的大小判据短路，非普通文件（或大小说谎的文件）用
// LimitReader(maxBytes+1) 兜底。maxBytes <= 0 表示不限制。
func (r *Root) ReadFileLimit(rel string, maxBytes int64) ([]byte, error) {
	handle, err := r.acquire()
	if err != nil {
		return nil, err
	}
	path, err := normalize("read", rel, true)
	if err != nil {
		return nil, err
	}
	file, err := handle.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, wrap("read", rel, err)
	}
	defer file.Close()
	if maxBytes <= 0 {
		data, err := io.ReadAll(file)
		if err != nil {
			return nil, wrap("read", rel, err)
		}
		return data, nil
	}
	if info, statErr := file.Stat(); statErr == nil && info.Mode().IsRegular() && info.Size() > maxBytes {
		return nil, tooLargeError(rel, info.Size(), maxBytes)
	}
	// 大小不可信（管道、/proc 文件、读取过程中被追加）：最多读 maxBytes+1 字节。
	limit := maxBytes
	if limit < math.MaxInt64 {
		limit++
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, wrap("read", rel, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, tooLargeError(rel, int64(len(data)), maxBytes)
	}
	return data, nil
}

// tooLargeError 构造 ErrTooLarge 分类错误。
func tooLargeError(rel string, size, maxBytes int64) error {
	return &opError{
		op:   "read",
		path: rel,
		kind: ErrTooLarge,
		err:  fmt.Errorf("%d bytes exceed the %d byte limit", size, maxBytes),
	}
}

// Stat 返回 rel 的信息（跟随符号链接）。
func (r *Root) Stat(rel string) (os.FileInfo, error) {
	handle, err := r.acquire()
	if err != nil {
		return nil, err
	}
	path, err := normalize("stat", rel, true)
	if err != nil {
		return nil, err
	}
	info, err := handle.Stat(path)
	if err != nil {
		return nil, wrap("stat", rel, err)
	}
	return info, nil
}

// Lstat 返回 rel 自身的信息（不跟随末尾符号链接）。
func (r *Root) Lstat(rel string) (os.FileInfo, error) {
	handle, err := r.acquire()
	if err != nil {
		return nil, err
	}
	path, err := normalize("lstat", rel, true)
	if err != nil {
		return nil, err
	}
	info, err := handle.Lstat(path)
	if err != nil {
		return nil, wrap("lstat", rel, err)
	}
	return info, nil
}

// Readlink 返回符号链接的原始文本（不解析、不校验目标：读取链接文本本身不涉及
// 逃逸）。要判断"链接是否会带出根外"，用 Stat/ReadFile 或 WriteFileAtomic 的语义。
func (r *Root) Readlink(rel string) (string, error) {
	handle, err := r.acquire()
	if err != nil {
		return "", err
	}
	path, err := normalize("readlink", rel, true)
	if err != nil {
		return "", err
	}
	target, err := handle.Readlink(path)
	if err != nil {
		return "", wrap("readlink", rel, err)
	}
	return target, nil
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

// WriteFileAtomic 原子地写入 rel：同目录临时文件 → Chmod → Rename。若 rel 是符号
// 链接，写入的是**链接目标**（链接本身保持不变，与 write_file 语义一致）；链接
// 目标必须是根内的相对路径，否则 ErrEscape。写失败会清理临时文件。
func (r *Root) WriteFileAtomic(rel string, data []byte, perm os.FileMode) error {
	handle, err := r.acquire()
	if err != nil {
		return err
	}
	target, err := resolveWriteTarget(handle, rel)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)
	tempRel := tempName()
	if dir != "." {
		tempRel = filepath.Join(dir, tempRel)
	}
	file, err := handle.OpenFile(tempRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return wrap("write", rel, err)
	}
	discard := func() {
		_ = file.Close()
		_ = handle.Remove(tempRel)
	}
	if _, err := file.Write(data); err != nil {
		discard()
		return wrap("write", rel, err)
	}
	if err := file.Sync(); err != nil {
		discard()
		return wrap("write", rel, err)
	}
	if err := file.Close(); err != nil {
		_ = handle.Remove(tempRel)
		return wrap("write", rel, err)
	}
	if err := handle.Chmod(tempRel, perm); err != nil {
		_ = handle.Remove(tempRel)
		return wrap("chmod", rel, err)
	}
	if err := handle.Rename(tempRel, target); err != nil {
		_ = handle.Remove(tempRel)
		return wrap("rename", rel, err)
	}
	return nil
}

// resolveWriteTarget 把 rel 解析到"真正要写的根内路径"：若路径（或其链）是符号
// 链接，就沿链接走到底；目标不是普通文件（目录）或链接指向根外时报错。
//
// 解析用 Lstat+Readlink 自己走链，而不是让 os.Root 跟随：os.Root 会在系统调用时
// 拦逃逸，但 Rename 会把**链接本身**替换成普通文件——那不是"写穿链接"的语义。
func resolveWriteTarget(handle *os.Root, rel string) (string, error) {
	path, err := normalize("write", rel, false)
	if err != nil {
		return "", err
	}
	const maxLinks = 40
	current := path
	for depth := 0; depth < maxLinks; depth++ {
		info, err := handle.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return current, nil // 新建文件：直接写这个路径
			}
			return "", wrap("write", rel, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if info.IsDir() {
				return "", &opError{op: "write", path: rel, err: errDirectory}
			}
			return current, nil
		}
		target, err := handle.Readlink(current)
		if err != nil {
			return "", wrap("write", rel, err)
		}
		if looksAbsolute(target) {
			return "", escapeError("write", rel, fmt.Errorf("symbolic link %q points outside the root (%q)", current, target))
		}
		next := filepath.Clean(filepath.Join(filepath.Dir(current), filepath.FromSlash(target)))
		if next == ".." || escapesRelative(next) {
			return "", escapeError("write", rel, fmt.Errorf("symbolic link %q points outside the root (%q)", current, target))
		}
		current = next
	}
	return "", &opError{op: "write", path: rel, err: errTooManyLinks}
}

// tempName 生成同目录临时文件名。随机串避免并发写互相撞名（O_EXCL 兜底），
// ".fsguard-" 前缀便于诊断与清理残留。
func tempName() string {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		// crypto/rand 失败极罕见；退化成单调计数器仍然唯一。
		return fmt.Sprintf(".fsguard-%d-%d.tmp", os.Getpid(), tempCounter.Add(1))
	}
	return ".fsguard-" + hex.EncodeToString(buffer[:]) + ".tmp"
}

// tempCounter 是 crypto/rand 失败时的兜底序号。
var tempCounter atomic.Uint64

// Rename 把 oldRel 改名为 newRel（作用于**路径本身**，不跟随符号链接：被移动的是
// 链接而不是它的目标）。目标路径的父目录必须存在。
func (r *Root) Rename(oldRel, newRel string) error {
	handle, err := r.acquire()
	if err != nil {
		return err
	}
	oldPath, err := normalize("rename", oldRel, false)
	if err != nil {
		return err
	}
	newPath, err := normalize("rename", newRel, false)
	if err != nil {
		return err
	}
	if err := handle.Rename(oldPath, newPath); err != nil {
		return wrap("rename", oldRel+" -> "+newRel, err)
	}
	return nil
}

// Remove 删除 rel（作用于**路径本身**：符号链接只删除链接，目标文件保留）。
func (r *Root) Remove(rel string) error {
	handle, err := r.acquire()
	if err != nil {
		return err
	}
	path, err := normalize("remove", rel, false)
	if err != nil {
		return err
	}
	if err := handle.Remove(path); err != nil {
		return wrap("remove", rel, err)
	}
	return nil
}

// MkdirAll 创建目录及其缺失的父目录。已存在是 no-op；"." 也是 no-op。
func (r *Root) MkdirAll(rel string, perm os.FileMode) error {
	handle, err := r.acquire()
	if err != nil {
		return err
	}
	path, err := normalize("mkdir", rel, true)
	if err != nil {
		return err
	}
	if path == "." {
		return nil
	}
	if err := handle.MkdirAll(path, perm); err != nil {
		return wrap("mkdir", rel, err)
	}
	return nil
}

// Chmod 修改 rel 的权限位。**不跟随符号链接**：rel 是符号链接时返回错误，而不会
// 悄悄改掉链接目标（os.Root.Chmod 本身是跟随的，那会让"改权限"意外作用到别处）。
// 要改目标权限，请显式传目标路径。
//
// 注意：os.Root.Chmod 在 Unix 上有已知竞态（Lstat 与 Chmod 之间目标被换成符号链接
// 时仍可能作用到链接本身）。这是 stdlib 的边界，fsguard 只把窗口缩小到一次断言，
// 不能消除。
func (r *Root) Chmod(rel string, mode os.FileMode) error {
	handle, err := r.acquire()
	if err != nil {
		return err
	}
	path, err := normalize("chmod", rel, false)
	if err != nil {
		return err
	}
	if info, statErr := handle.Lstat(path); statErr != nil {
		return wrap("chmod", rel, statErr)
	} else if info.Mode()&os.ModeSymlink != 0 {
		return &opError{op: "chmod", path: rel, err: errSymlinkTarget}
	}
	if err := handle.Chmod(path, mode); err != nil {
		return wrap("chmod", rel, err)
	}
	return nil
}

// IsEscape 报告 err 是否表示"路径逃出了根"（等价于 errors.Is(err, ErrEscape)）。
func IsEscape(err error) bool { return errors.Is(err, ErrEscape) }
