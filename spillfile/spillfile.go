// Package spillfile 是 rig.SpillStore 的**文件型**实现：把过大的工具结果落成
// 一个 `.txt` 文件，模型看到的句柄就是文件名。
//
// # 解决什么问题
//
// agent 内部的落盘实现（spill 策略的文件后端）是私有函数，数据库实现又在独立子
// 模块里。宿主想把"工作区可见、按会话回收、可 GC"这套行为复用起来时没有导出实现；
// 本包补上它：目录由宿主给（通常在工作区内），文件名带作用域前缀，回收只作用于
// 同一作用域。
//
// # 为什么句柄是文件名而不是路径
//
// 句柄要能跨宿主重启与目录迁移后继续可用，也要能直接放进 `spill_read` 的参数。
// 所以 `Save` 返回 `rig.SpillRef{ID: 文件名}`（不含目录），`Load` 只在这个目录里
// 找普通文件：带路径分隔符或 `..` 的句柄一律拒绝，读到目录/符号链接也拒绝。
//
// # 回收按作用域分区
//
// 文件名形如 `spill.<scope>.<纳秒时间戳>-<随机后缀>.txt`，空作用域落 `global`
// 桶。`GC` 只删与 rig.SpillScope(ctx) 同作用域的旧文件（按文件名里的时间戳倒序
// 保留最新 keep 个）。**不能**全局裁剪：两个会话共用一个目录时，A 的 MaxFiles
// 会删掉 B 的句柄，而句柄失效就是模型的工具输出永久丢失。
//
// 作用域标签只保留 `[A-Za-z0-9_-]`，清洗改变了原值时追加原始值的短哈希，避免两个
// 不同会话清洗后撞进同一个桶（撞桶就会互删）。因此作用域本身就是 `global` 时与
// 空作用域同桶——这是文件名格式的约定，宿主给会话 ID 时不要用 `global` 这个词。
//
// # 并发
//
// Save 并发安全：目录名与句柄在同一把锁下生成（时间戳在进程内严格递增，回收排序
// 因此是确定的），内容写到同目录临时文件后 rename 到最终名字（原子替换）。
package spillfile

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// globalScope 是空作用域（ctx 上没有 rig.WithSpillScope）的文件名标签。
	globalScope = "global"
	// fileSuffix 是溢出文件的扩展名；GC 只扫这个后缀。
	fileSuffix = ".txt"
	// scopeLabelLimit 是清洗后作用域标签的最大长度，避免文件名过长。
	scopeLabelLimit = 64
	// scopeHashLen 是作用域被清洗时追加的短哈希字节数（8 字节 = 16 个十六进制字符）。
	scopeHashLen = 8
	// randomSuffixBytes 是同一纳秒内区分两个句柄的随机后缀字节数。
	randomSuffixBytes = 4
)

// Store 是一个目录里的溢出结果存储。零值不可用，必须用 New 构造。
type Store struct {
	dir  string
	mu   sync.Mutex // 保护 last：句柄里的时间戳在进程内严格递增
	last int64
}

// 断言本类型实现宿主契约。
var _ rig.SpillStore = (*Store)(nil)

// New 返回落在 dir 里的存储；目录不存在时按 0755 创建（宿主不必先 MkdirAll）。
//
// dir 通常是工作区内的目录（例如 `.rig/spill`），这样用户能在文件树里看见溢出
// 结果、模型也能通过宿主自己的读文件工具找到它们。
func New(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("spillfile: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("spillfile: create directory %q: %w", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("spillfile: resolve directory %q: %w", dir, err)
	}
	return &Store{dir: abs}, nil
}

// Dir 返回存储的绝对目录，便于宿主把文件树里的位置展示出来。
func (s *Store) Dir() string { return s.dir }

// Save 把 content 写成 `spill.<scope>.<纳秒时间戳>-<随机后缀>.txt`，返回句柄。
//
// 内容先写到同目录的临时文件再 rename：读到的永远是完整文件，进程在写入中途被杀
// 也不会留下半个句柄。返回值里的 ID 只是文件名，不含目录。
func (s *Store) Save(ctx context.Context, call rig.ToolCall, content string) (rig.SpillRef, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return rig.SpillRef{}, fmt.Errorf("spillfile: save result of tool %q: %w", call.Name, err)
	}
	name := s.nextName(scopeLabel(rig.SpillScope(ctx)))
	path := filepath.Join(s.dir, name)

	tmp, err := os.CreateTemp(s.dir, ".spill-*.tmp")
	if err != nil {
		return rig.SpillRef{}, fmt.Errorf("spillfile: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// 成功 rename 之后临时名已经不存在，这次 Remove 是无害的空操作；失败路径上
	// 它负责把残片清掉。
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return rig.SpillRef{}, fmt.Errorf("spillfile: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return rig.SpillRef{}, fmt.Errorf("spillfile: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return rig.SpillRef{}, fmt.Errorf("spillfile: rename temp file to %q: %w", name, err)
	}
	return rig.SpillRef{ID: name, Bytes: len(content)}, nil
}

// Load 返回句柄背后的正文；不存在返回 rig.ErrSpillNotFound。
//
// 句柄只能是本目录里的普通文件名：含路径分隔符、`..` 或是绝对路径的直接报错，
// 读到目录或符号链接也报错——句柄来自模型，不能变成读任意路径的入口。
func (s *Store) Load(ctx context.Context, id string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("spillfile: load %q: %w", id, err)
	}
	if err := checkID(id); err != nil {
		return "", err
	}
	path := filepath.Join(s.dir, id)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("spillfile: load %q: %w", id, rig.ErrSpillNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("spillfile: stat %q: %w", id, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("spillfile: load %q: not a regular file", id)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("spillfile: load %q: %w", id, rig.ErrSpillNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("spillfile: read %q: %w", id, err)
	}
	return string(data), nil
}

// GC 只回收**与 ctx 同作用域**的旧条目，保留其中最新的 keep 个，返回删掉的条数。
//
// keep <= 0 时不做任何事（返回 0, nil）：调用方没设上限就是"不清理"，不能把
// "没设上限"实现成"清空"。排序看文件名里的纳秒时间戳（不是 mtime）：同一秒内
// 连续写入时 mtime 会并列，靠它无法确定谁最新。删除失败（例如文件已被别的清理
// 动作拿走）不阻断其余删除，也不报错，只有真正删掉的才计数。
func (s *Store) GC(ctx context.Context, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("spillfile: gc: %w", err)
	}
	prefix := filePrefix(scopeLabel(rig.SpillScope(ctx)))
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("spillfile: list directory: %w", err)
	}
	type entry struct {
		name string
		nano int64
	}
	files := make([]entry, 0, len(entries))
	for _, item := range entries {
		if item.IsDir() {
			continue
		}
		name := item.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		nano, ok := nanoFromName(name, prefix)
		if !ok {
			continue
		}
		files = append(files, entry{name: name, nano: nano})
	}
	// 倒序：最新的在前，前 keep 个留下，其余删掉。
	sort.Slice(files, func(i, j int) bool {
		if files[i].nano != files[j].nano {
			return files[i].nano > files[j].nano
		}
		return files[i].name > files[j].name
	})
	dropped := 0
	for i := keep; i < len(files); i++ {
		if err := os.Remove(filepath.Join(s.dir, files[i].name)); err == nil {
			dropped++
		}
	}
	return dropped, nil
}

// nextName 生成一个新句柄：`spill.<scope>.<纳秒>-<随机>.txt`。
//
// 时间戳在进程内严格递增（同一纳秒里两次 Save 会得到 last+1），所以 GC 的
// "最新 keep 个"与 Save 的先后顺序一致，不依赖 mtime 或目录遍历顺序；随机后缀
// 保证跨进程、跨目录合并时也不会撞名。
func (s *Store) nextName(scope string) string {
	now := time.Now().UTC().UnixNano()
	s.mu.Lock()
	if now <= s.last {
		now = s.last + 1
	}
	s.last = now
	s.mu.Unlock()

	var suffix [randomSuffixBytes]byte
	_, _ = rand.Read(suffix[:])
	return fmt.Sprintf("spill.%s.%d-%s%s", scope, now, hex.EncodeToString(suffix[:]), fileSuffix)
}

// filePrefix 返回某个作用域在目录里的文件名前缀。
//
// 以 "." 结尾作为分隔符，因此作用域 `session-a` 的前缀不会误匹配 `session-a-b`
// 的文件（后者的前缀是 `spill.session-a-b.`）。
func filePrefix(scope string) string { return "spill." + scope + "." }

// nanoFromName 从 `<prefix><纳秒>-<随机>.txt` 里取出纳秒时间戳。
func nanoFromName(name, prefix string) (int64, bool) {
	rest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), fileSuffix)
	nanoPart, _, ok := strings.Cut(rest, "-")
	if !ok || nanoPart == "" {
		return 0, false
	}
	nano, err := strconv.ParseInt(nanoPart, 10, 64)
	if err != nil || nano <= 0 {
		return 0, false
	}
	return nano, true
}

// checkID 拒绝把句柄当路径用：空串、分隔符、`..`、绝对路径都不接受。
func checkID(id string) error {
	if id == "" {
		return errors.New("spillfile: empty spilled result id")
	}
	if strings.Contains(id, "..") {
		return fmt.Errorf("spillfile: unsafe spilled result id %q: contains %q", id, "..")
	}
	if strings.ContainsAny(id, `/\`) || filepath.IsAbs(id) {
		return fmt.Errorf("spillfile: unsafe spilled result id %q: contains a path separator", id)
	}
	return nil
}

// scopeLabel 把会话 ID 变成可进文件名的标签；空作用域落 global 桶。
//
// 清洗改变了原值时追加原始值的短哈希：两个不同会话 ID 清洗后撞成同一个标签，
// 就会在 GC 时互删对方的句柄。
func scopeLabel(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return globalScope
	}
	label := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, scope)
	if len(label) > scopeLabelLimit {
		label = label[:scopeLabelLimit]
	}
	if label == scope {
		return label
	}
	sum := sha256.Sum256([]byte(scope))
	return label + "-" + hex.EncodeToString(sum[:scopeHashLen])
}
