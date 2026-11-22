// Package taskstore 为一个共享团队任务板提供持久化：把 team.TaskManager 的任务
// 快照写成一份有界 JSON 文档，并在加载时通过 manager 的 create/update 路径重放，
// 从而复现任务 id 与顺序。
//
// 设计约定：
//   - 一个 Store 只拥有一个任务板（一个文件）。New 的 sessionID 为空时使用
//     进程级旧文档 team-tasks.json，否则使用 team-tasks/<session>.json；
//   - 文档写入是原子的（同目录临时文件 + fsync + rename），Save/Delete/Load 由
//     Store 的互斥锁串行化；
//   - 删除在任务板上是墓碑（管理器的任务不会被物理删除，因为 rig 的管理器没有
//     Delete）；墓碑随文档持久化，由宿主在读模型里隐藏；
//   - 文档损坏或版本不认识时被隔离为 <path>.corrupt-<纳秒>，随后按空任务板处理；
//   - 旧版进程级文档通过 MigrateLegacy 一次性合并进某个会话任务板（按内容合并，
//     可重入，不会重复）。
//
// 本包与宿主无关：它不认识产品类型，只依赖 rig 与 tools/team。
package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/team"
)

const (
	// DocumentVersion 是当前文档 schema 版本；其他版本会被隔离。
	DocumentVersion = 1
	// LegacyFileName 是进程级（无 session 域）任务板文档名。
	LegacyFileName = "team-tasks.json"
	// SessionDirName 是每会话文档所在子目录。
	SessionDirName = "team-tasks"
	// DefaultInProgressOwner 是重放 in_progress 但没有 owner 的任务时补的伪
	// owner（rig 管理器要求 in_progress 必须有 owner）。
	DefaultInProgressOwner = "taskboard"
	// DefaultMaxTasks 是文档保留的任务上限（与 rig 管理器默认值一致）。
	DefaultMaxTasks = team.DefaultMaxTasks
)

// ErrManagerNotEmpty 表示 Load 的目标 manager 已经有任务：Load 只能重放进空
// 管理器，否则会产生重复任务。
var ErrManagerNotEmpty = errors.New("taskstore: target task manager is not empty")

// Document 是持久化的任务板文档。
type Document struct {
	Version int            `json:"version"`
	Tasks   []rig.TeamTask `json:"tasks,omitempty"`
	Deleted []string       `json:"deleted,omitempty"`
}

// Board 是一次 Load 的结果。
type Board struct {
	// Tasks 是重放后 manager 的任务视图（id 已按实际重放结果重编号）。
	Tasks []rig.TeamTask
	// Deleted 是墓碑 id（已随重编号映射）。
	Deleted []string
	// Loaded 表示文档存在并成功解析（合法的空任务板也算 true）。
	Loaded bool
	// Renumbered 表示文档里的 id 不连续，重放时被一致重编号（依赖与墓碑已重写）。
	Renumbered bool
}

// Store 持久化一个任务板。零值不可用，请用 New 构造。
type Store struct {
	mu            sync.Mutex
	path          string
	logf          func(format string, args ...any)
	retired       bool
	deleted       map[string]bool
	maxTasks      int
	ownerFallback string
}

// Option 配置 Store。
type Option func(*Store)

// WithLogger 设置日志函数（默认丢弃）。
func WithLogger(logf func(format string, args ...any)) Option {
	return func(store *Store) {
		if logf != nil {
			store.logf = logf
		}
	}
}

// WithMaxTasks 限制文档保留的任务数（非正数保留 DefaultMaxTasks）。
func WithMaxTasks(max int) Option {
	return func(store *Store) {
		if max > 0 {
			store.maxTasks = max
		}
	}
}

// WithInProgressOwner 覆盖重放 in_progress 无 owner 任务时使用的伪 owner。
// 空串保留 DefaultInProgressOwner。
func WithInProgressOwner(owner string) Option {
	return func(store *Store) {
		if strings.TrimSpace(owner) != "" {
			store.ownerFallback = strings.TrimSpace(owner)
		}
	}
}

// New 返回一个任务板 Store。dir 为空时持久化被禁用：Load 返回空任务板，Save
// 与 Delete 是 no-op。sessionID 为空时使用进程级旧文档路径；非空时使用每会话
// 文档。
func New(dir, sessionID string, opts ...Option) *Store {
	store := &Store{
		path:          documentPath(dir, sessionID),
		logf:          func(string, ...any) {},
		deleted:       make(map[string]bool),
		maxTasks:      DefaultMaxTasks,
		ownerFallback: DefaultInProgressOwner,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(store)
		}
	}
	return store
}

// Path 返回文档路径（持久化禁用时为空串）。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// documentPath 返回一个任务板的文档路径。
func documentPath(dir, sessionID string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return filepath.Join(dir, LegacyFileName)
	}
	return filepath.Join(dir, SessionDirName, sanitizeSessionFileName(sessionID)+".json")
}

// sanitizeSessionFileName 把 session id 变成安全的文件名；被改写的 id 带短哈希
// 后缀，避免两个不同会话落到同一个文件。
func sanitizeSessionFileName(sessionID string) string {
	var builder strings.Builder
	clean := true
	for _, char := range sessionID {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '-', char == '_':
			builder.WriteRune(char)
		default:
			builder.WriteByte('_')
			clean = false
		}
	}
	name := builder.String()
	if name == "" {
		name = "session"
		clean = false
	}
	if !clean {
		name += "-" + shortHash(sessionID)
	}
	return name
}

// shortHash 返回 session id sha256 的前 8 个十六进制字符。
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}

// Deleted 返回当前墓碑 id 的排序副本。
func (s *Store) Deleted() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedDeleted(s.deleted)
}

// SetDeleted 用给定 id 替换墓碑集合（trim、去空、去重）。
func (s *Store) SetDeleted(ids []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = make(map[string]bool, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			s.deleted[id] = true
		}
	}
}

// MarkDeleted 在同一进程里记录一个墓碑（随后 Save 才会落盘）。
func (s *Store) MarkDeleted(id string) {
	if s == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	s.mu.Lock()
	s.deleted[id] = true
	s.mu.Unlock()
}

// ClearDeleted 移除一个墓碑（恢复被任务板 API 隐藏的任务）。
func (s *Store) ClearDeleted(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.deleted, strings.TrimSpace(id))
	s.mu.Unlock()
}

// Load 读取文档并通过 manager 的 create/update 路径重放它，返回重放后的视图与
// 墓碑。manager 必须为空（ErrManagerNotEmpty）；一个损坏或不支持版本的文档会被
// 隔离并按空任务板处理（返回空 Board，不返回错误）。id 不连续时按升序一致重
// 编号，依赖与墓碑一起映射。重放失败是尽力而为的：单条任务失败会记日志并跳过。
func (s *Store) Load(ctx context.Context, manager *team.TaskManager) (Board, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return Board{}, err
		}
	}
	if s == nil || manager == nil {
		return Board{}, errors.New("taskstore: nil store or task manager")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return Board{}, nil
	}
	existing, err := manager.List(ctx, rig.TeamTaskFilter{})
	if err != nil {
		return Board{}, err
	}
	if len(existing) > 0 {
		return Board{}, ErrManagerNotEmpty
	}
	document, ok, reason := s.readDocumentLocked()
	if !ok {
		if reason != "" {
			s.quarantineLocked(reason)
		}
		s.deleted = make(map[string]bool)
		return Board{}, nil
	}
	board := s.replayLocked(ctx, manager, document)
	s.deleted = make(map[string]bool, len(board.Deleted))
	for _, id := range board.Deleted {
		s.deleted[id] = true
	}
	return board, nil
}

// Save 把 manager 的当前任务与内存墓碑原子写入文档。文档最多保留 maxTasks 条
// 任务；Store 已 Delete（retired）后 Save 是 no-op。
func (s *Store) Save(ctx context.Context, manager *team.TaskManager) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if s == nil {
		return nil
	}
	if manager == nil {
		return errors.New("taskstore: nil task manager")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(ctx, manager)
}

// saveLocked 在持有 Store 锁时写文档。
func (s *Store) saveLocked(ctx context.Context, manager *team.TaskManager) error {
	if s.path == "" || s.retired {
		return nil
	}
	tasks, err := manager.List(ctx, rig.TeamTaskFilter{})
	if err != nil {
		return err
	}
	document := Document{Version: DocumentVersion}
	for _, task := range tasks {
		if len(document.Tasks) >= s.maxTasks {
			break
		}
		document.Tasks = append(document.Tasks, cloneTask(task))
	}
	document.Deleted = sortedDeleted(s.deleted)
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		s.logf("[taskstore] encode %s: %v", s.path, err)
		return err
	}
	if err := writeFileAtomic(s.path, data); err != nil {
		s.logf("[taskstore] write %s: %v", s.path, err)
		return err
	}
	return nil
}

// Delete 删除文档并让 Store 退役：在途的 Save 会先完成（同一把锁），之后的
// Save 都是 no-op，因此删除后的晚到写入不会重建文档。
func (s *Store) Delete() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = true
	s.deleted = make(map[string]bool)
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) && !isNotDirOrMissing(err) {
		return err
	}
	return nil
}

// MigrateLegacy 把一份旧版任务板文档（例如进程级的 team-tasks.json）一次性合并
// 进这个 Store 的任务板：先读旧文档，再按内容合并（内容相同的旧任务复用已存在
// 的 id，其余通过 create 路径新增），恢复依赖与墓碑，保存，最后删除旧文件。
// 合并是幂等的：中途崩溃后重试不会产生重复任务。
func (s *Store) MigrateLegacy(ctx context.Context, manager *team.TaskManager, legacyPath string) (int, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}
	if s == nil || manager == nil {
		return 0, errors.New("taskstore: nil store or task manager")
	}
	legacyPath = strings.TrimSpace(legacyPath)
	if legacyPath == "" || legacyPath == s.path {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" || s.retired {
		return 0, nil
	}
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return 0, nil
	}
	var document Document
	if err := json.Unmarshal(data, &document); err != nil {
		return 0, fmt.Errorf("taskstore: read legacy board %s: %w", legacyPath, err)
	}
	if document.Version != DocumentVersion {
		return 0, fmt.Errorf("taskstore: legacy board %s has unsupported version %d", legacyPath, document.Version)
	}
	created, err := s.mergeLocked(ctx, manager, document)
	if err != nil {
		return created, err
	}
	if err := s.saveLocked(ctx, manager); err != nil {
		return created, err
	}
	if err := os.Remove(legacyPath); err != nil && !os.IsNotExist(err) {
		return created, err
	}
	if created > 0 {
		s.logf("[taskstore] migrated %d legacy task(s) from %s", created, legacyPath)
	}
	return created, nil
}

// mergeLocked 把旧文档按内容合并进 manager，并返回新建的任务数。
func (s *Store) mergeLocked(ctx context.Context, manager *team.TaskManager, document Document) (int, error) {
	existing, err := manager.List(ctx, rig.TeamTaskFilter{})
	if err != nil {
		return 0, err
	}
	available := make(map[string][]string, len(existing))
	for _, task := range existing {
		available[taskContentKey(task)] = append(available[taskContentKey(task)], task.ID)
	}
	remap := make(map[string]string, len(document.Tasks))
	created := 0
	for _, task := range document.Tasks {
		if s.deleted[task.ID] {
			continue
		}
		key := taskContentKey(task)
		if ids := available[key]; len(ids) > 0 {
			remap[task.ID] = ids[0]
			available[key] = ids[1:]
			continue
		}
		replayed, createErr := manager.Create(ctx, s.replayTask(task))
		if createErr != nil {
			return created, createErr
		}
		remap[task.ID] = replayed.ID
		created++
	}
	for _, task := range document.Tasks {
		if s.deleted[task.ID] || len(task.BlockedBy) == 0 {
			continue
		}
		target, ok := remap[task.ID]
		if !ok {
			continue
		}
		mapped := make([]string, 0, len(task.BlockedBy))
		for _, dependency := range task.BlockedBy {
			if id, ok := remap[dependency]; ok {
				mapped = append(mapped, id)
			}
		}
		if len(mapped) == 0 {
			continue
		}
		if _, updateErr := manager.Update(ctx, target, rig.TeamTaskUpdate{AddBlocked: mapped}); updateErr != nil {
			s.logf("[taskstore] legacy task %q dependencies: %v", task.ID, updateErr)
		}
	}
	for _, id := range document.Deleted {
		if mapped, ok := remap[id]; ok {
			s.deleted[mapped] = true
		}
	}
	return created, nil
}

// replayLocked 把文档重放进空 manager，返回重放后的视图。
func (s *Store) replayLocked(ctx context.Context, manager *team.TaskManager, document Document) Board {
	sorted := append([]rig.TeamTask(nil), document.Tasks...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left, leftOK := taskNumericID(sorted[i].ID)
		right, rightOK := taskNumericID(sorted[j].ID)
		if leftOK && rightOK {
			return left < right
		}
		return sorted[i].ID < sorted[j].ID
	})
	contiguous := true
	for index, task := range sorted {
		if id, ok := taskNumericID(task.ID); !ok || id != index+1 {
			contiguous = false
			break
		}
	}
	if !contiguous && len(sorted) > 0 {
		s.logf("[taskstore] task ids are not contiguous; renumbering %d task(s)", len(sorted))
	}
	remap := make(map[string]string, len(sorted))
	for _, task := range sorted {
		replayed, err := manager.Create(ctx, s.replayTask(task))
		if err != nil {
			s.logf("[taskstore] replay task %q: %v", task.ID, err)
			continue
		}
		remap[task.ID] = replayed.ID
	}
	for _, task := range sorted {
		if len(task.BlockedBy) == 0 {
			continue
		}
		target, ok := remap[task.ID]
		if !ok {
			continue
		}
		mapped := make([]string, 0, len(task.BlockedBy))
		for _, dependency := range task.BlockedBy {
			if id, ok := remap[dependency]; ok {
				mapped = append(mapped, id)
			}
		}
		if len(mapped) == 0 {
			continue
		}
		if _, err := manager.Update(ctx, target, rig.TeamTaskUpdate{AddBlocked: mapped}); err != nil {
			s.logf("[taskstore] replay task %q dependencies: %v", task.ID, err)
		}
	}
	deleted := make([]string, 0, len(document.Deleted))
	for _, id := range document.Deleted {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if mapped, ok := remap[id]; ok {
			deleted = append(deleted, mapped)
		} else {
			deleted = append(deleted, id)
		}
	}
	sort.Strings(deleted)
	tasks, err := manager.List(ctx, rig.TeamTaskFilter{})
	if err != nil {
		s.logf("[taskstore] list replayed tasks: %v", err)
		tasks = nil
	}
	return Board{Tasks: tasks, Deleted: deleted, Loaded: true, Renumbered: !contiguous}
}

// replayTask 把一个持久化任务转换成 create 路径的输入。空 id 让 manager 自己
// 编号（推进序号），从而复现 task-1..task-N。
func (s *Store) replayTask(task rig.TeamTask) rig.TeamTask {
	owner := strings.TrimSpace(task.Owner)
	status := strings.ToLower(strings.TrimSpace(task.Status))
	if status == team.TaskStatusInProgress && owner == "" {
		owner = s.ownerFallback
	}
	return rig.TeamTask{
		Subject:     task.Subject,
		Description: task.Description,
		Status:      status,
		Owner:       owner,
		BlockedBy:   nil,
		WriteScopes: append([]string(nil), task.WriteScopes...),
		Created:     task.Created,
	}
}

// readDocumentLocked 读取文档。ok=false 时 reason 非空表示文档损坏或版本不支
// 持（需要隔离）；reason 为空表示文档缺失或内容为空（安静地按空任务板处理）。
func (s *Store) readDocumentLocked() (Document, bool, string) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) && !isNotDirOrMissing(err) {
			s.logf("[taskstore] read %s: %v", s.path, err)
		}
		return Document{}, false, ""
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return Document{}, false, ""
	}
	var document Document
	if err := json.Unmarshal(data, &document); err != nil {
		return Document{}, false, "task board document is not valid JSON: " + err.Error()
	}
	if document.Version != DocumentVersion {
		return Document{}, false, fmt.Sprintf("task board document has unsupported version %d", document.Version)
	}
	return document, true, ""
}

// quarantineLocked 把无法使用的文档移到一边，让任务板从空开始。
func (s *Store) quarantineLocked(reason string) {
	if strings.TrimSpace(s.path) == "" {
		return
	}
	backup := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().UTC().UnixNano())
	message := reason
	if err := os.Rename(s.path, backup); err != nil {
		message += fmt.Sprintf(" (could not quarantine the file: %v)", err)
	} else {
		message += " (quarantined as " + backup + ")"
	}
	s.logf("%s; starting with an empty task board", message)
}

// taskContentKey 是旧文档按内容合并时使用的身份键。
func taskContentKey(task rig.TeamTask) string {
	return strings.Join([]string{
		strings.TrimSpace(task.Subject),
		strings.TrimSpace(task.Description),
		strings.ToLower(strings.TrimSpace(task.Status)),
		strings.TrimSpace(task.Owner),
		strings.Join(task.WriteScopes, "\x00"),
	}, "\x01")
}

// cloneTask 深拷贝一个任务，避免文档持有 manager 的切片。
func cloneTask(task rig.TeamTask) rig.TeamTask {
	task.BlockedBy = append([]string(nil), task.BlockedBy...)
	task.WriteScopes = append([]string(nil), task.WriteScopes...)
	return task
}

// sortedDeleted 返回墓碑集合的排序切片。
func sortedDeleted(deleted map[string]bool) []string {
	if len(deleted) == 0 {
		return nil
	}
	ids := make([]string, 0, len(deleted))
	for id := range deleted {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// taskNumericID 解析 "task-<n>" 的 n（n >= 1）。
func taskNumericID(id string) (int, bool) {
	trimmed := strings.TrimSpace(id)
	if !strings.HasPrefix(trimmed, "task-") {
		return 0, false
	}
	value, err := strconv.Atoi(strings.TrimPrefix(trimmed, "task-"))
	if err != nil || value < 1 {
		return 0, false
	}
	return value, true
}

// errNotDir 让 isNotDirOrMissing 不必在调用点导入 syscall。
var errNotDir = syscall.ENOTDIR

// isNotDirOrMissing 把 ENOTDIR 当作"路径不存在"（父目录在拆除期间被换成了
// 普通文件时文档不可能存在）。
func isNotDirOrMissing(err error) bool {
	return errors.Is(err, errNotDir)
}

// writeFileAtomic 通过同目录临时文件 + fsync + rename 原子写入一份小文档。
func writeFileAtomic(path string, data []byte) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".task-board-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if handle, err := os.Open(directory); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}
