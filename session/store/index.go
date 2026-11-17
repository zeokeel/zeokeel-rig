package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// indexDoc 是 index.json 的磁盘格式。
//
// 它只保存 Meta 列表：会话的真相在 sessions/*.ndjson 里，索引是可由数据文件重建
// 的派生视图（因此"索引丢了"不是灾难，"数据文件丢了"才是）。
type indexDoc struct {
	// Version 是格式版本；不认识就重建。
	Version int `json:"version"`
	// Sessions 是会话索引，按 ID 升序落盘（diff 友好）。
	Sessions []Meta `json:"sessions"`
}

// load 加载索引；缺失/损坏/版本不认识时从会话文件重建。
func (d *Dir) load() error {
	data, err := os.ReadFile(d.indexPath)
	switch {
	case err == nil:
		var doc indexDoc
		if jsonErr := json.Unmarshal(data, &doc); jsonErr == nil && doc.Version == indexVersion {
			for _, meta := range doc.Sessions {
				if !validStem(meta.ID) {
					d.emitf("session store: 索引里的会话 ID %q 不能安全用作文件名，已忽略", meta.ID)
					continue
				}
				d.index[meta.ID] = normalizeMeta(meta)
			}
			if err := d.pruneMissingFiles(); err != nil {
				return err
			}
			d.recordIndexStamp()
			return nil
		}
		reason := "版本不认识"
		if jsonErr := json.Unmarshal(data, &doc); jsonErr != nil {
			reason = jsonErr.Error()
		}
		d.emitf("session store: %s 无法使用（%s），从会话文件重建索引", d.indexPath, reason)
		return d.rebuild()
	case errors.Is(err, os.ErrNotExist):
		if err := d.rebuild(); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("session store: read index %q: %w", d.indexPath, err)
	}
}

// pruneMissingFiles 丢弃"索引里有、文件不在"的条目（Delete 中途崩溃的残留），
// 并把结果写回索引。打开 Store 时调用一次，避免幽灵会话出现在 List 里。
func (d *Dir) pruneMissingFiles() error {
	var missing []string
	for id := range d.index {
		if _, err := os.Stat(d.sessionPath(id)); errors.Is(err, os.ErrNotExist) {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	for _, id := range missing {
		delete(d.index, id)
		d.emitf("session store: 索引里的会话 %q 已无数据文件，已移除", id)
	}
	return d.saveIndexLocked()
}

// rebuild 扫描 sessions/*.ndjson 重建索引。
//
// 能恢复的：ID（来自文件名）、条数、字节数、标题（首条用户消息摘要）、时间
// （退化为文件修改时间）。
// 无法恢复的：Workspace、Model（数据文件里没有这两项信息），以及不安全 ID 的原
// 始写法（文件名是它的哈希）。
func (d *Dir) rebuild() error {
	entries, err := os.ReadDir(d.sessionsDir)
	if err != nil {
		return fmt.Errorf("session store: read sessions dir %q: %w", d.sessionsDir, err)
	}
	rebuilt := make(map[string]Meta, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, sessionExt) {
			// 临时文件（"." 前缀）与其它杂项一律跳过。
			continue
		}
		id := strings.TrimSuffix(name, sessionExt)
		if !validStem(id) {
			continue
		}
		path := filepath.Join(d.sessionsDir, name)
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		read, readErr := readRecords(path)
		if readErr != nil && !errors.Is(readErr, ErrCorrupt) {
			continue
		}
		// 全损坏的会话也保留条目（Messages=0），让 Load 能给调用方明确的 ErrCorrupt。
		stamp := info.ModTime().UTC()
		rebuilt[id] = Meta{
			ID:        id,
			Title:     clampTitle(firstUserTitle(read.messages)),
			CreatedAt: stamp,
			UpdatedAt: stamp,
			Messages:  read.records,
			Bytes:     read.size,
		}
	}
	d.index = rebuilt
	if err := d.saveIndexLocked(); err != nil {
		return err
	}
	if len(rebuilt) > 0 {
		d.emitf("session store: 已从 %d 个会话文件重建索引", len(rebuilt))
	}
	return nil
}

// saveIndexLocked 原子写索引：临时文件 + Sync + rename + fsync 目录。
//
// 调用方必须持有 indexMu 的写锁（构造函数在单线程阶段调用除外）。
func (d *Dir) saveIndexLocked() error {
	doc := indexDoc{Version: indexVersion, Sessions: make([]Meta, 0, len(d.index))}
	for _, meta := range d.index {
		doc.Sessions = append(doc.Sessions, meta)
	}
	sort.Slice(doc.Sessions, func(i, j int) bool { return doc.Sessions[i].ID < doc.Sessions[j].ID })
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("session store: encode index: %w", err)
	}
	temporary, err := os.CreateTemp(d.root, indexTmpPrefix+"*")
	if err != nil {
		return fmt.Errorf("session store: create temp index in %q: %w", d.root, err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if _, err := temporary.Write(encoded); err != nil {
		cleanup()
		return fmt.Errorf("session store: write temp index: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("session store: sync temp index: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("session store: close temp index: %w", err)
	}
	if err := os.Rename(temporaryPath, d.indexPath); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("session store: replace index %q: %w", d.indexPath, err)
	}
	// 记录指纹：List 靠它判断"别的实例是否改过索引"（一次 stat，不读盘）。
	d.recordIndexStamp()
	if err := syncDir(d.root); err != nil {
		return fmt.Errorf("session store: sync store dir: %w", err)
	}
	return nil
}

// mergeDiskIndex 把磁盘索引里"我们没有"的条目并进内存（内存里的条目优先）。
//
// 用途：Create 在保存索引之前先合并。否则一个"索引视图陈旧"的实例 Create 时
// 会用自己那份内存索引覆盖 index.json，把**另一个实例刚建的会话**从索引里抹掉
// （文件还在，但要等下次收养才回来）。Create 是低频操作，多一次读盘可以接受；
// Append/Load 这些高频路径不受影响。
func (d *Dir) mergeDiskIndex() {
	data, err := os.ReadFile(d.indexPath)
	if err != nil {
		return
	}
	var doc indexDoc
	if err := json.Unmarshal(data, &doc); err != nil || doc.Version != indexVersion {
		return
	}
	d.indexMu.Lock()
	defer d.indexMu.Unlock()
	for _, meta := range doc.Sessions {
		if !validStem(meta.ID) {
			continue
		}
		if _, exists := d.index[meta.ID]; exists {
			continue
		}
		d.index[meta.ID] = normalizeMeta(meta)
	}
}

// normalizeMeta 把磁盘读到的 Meta 归一化（时间统一 UTC、计数器不为负）。
func normalizeMeta(meta Meta) Meta {
	meta.CreatedAt = meta.CreatedAt.UTC()
	meta.UpdatedAt = meta.UpdatedAt.UTC()
	if meta.Messages < 0 {
		meta.Messages = 0
	}
	if meta.Bytes < 0 {
		meta.Bytes = 0
	}
	return meta
}
