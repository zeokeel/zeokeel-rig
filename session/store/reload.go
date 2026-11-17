package store

// 本文件是"跨实例自愈"：同一目录上的两个 Dir（两个 kit、重启前的老实例）必须能
// 看见对方建的会话，但又不能靠跨进程文件锁把"重启后新实例接管"这种合法用法挡掉。
//
// 策略：
//  1. 内存索引未命中时**从磁盘重载一次**再重试（Append/AppendBatch/Load/Delete/
//     Replace）；重载会把 index.json 读回来，并**收养**只有文件、没有索引条目的
//     会话（另一实例刚 Create、或索引被 last-writer-wins 覆盖过）。
//  2. 重载按触发键节流（reloadInterval=200ms）：反复查同一个不存在的会话不会
//     每次都读盘；不同键互不影响。
//  3. 单实例的常规路径**不增加任何重载**：命中内存索引时根本不会走到这里。
//  4. 同一进程里同目录的多个实例用一把共享的 processMu 串行化写入（避免
//     O_APPEND 交错与索引互相覆盖）；跨进程仍是"磁盘为准 + 下次 Load 自愈"。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// listReloadKey 是 List 用的一次性重载键（不带会话 ID）。
const listReloadKey = "#list"

// lookupWithRefresh 先读内存索引；未命中时按节流策略重载磁盘再读一次。
//
// 返回 false 表示"重载后仍然没有"（调用方据此返回 ErrNotFound）。
func (d *Dir) lookupWithRefresh(ctx context.Context, id string) (Meta, bool) {
	if meta, ok := d.lookup(id); ok {
		return meta, true
	}
	if !d.claimReload(id) {
		return Meta{}, false
	}
	d.reloadFromDisk(ctx, id)
	return d.lookup(id)
}

// claimReload 报告这次触发点是否值得重载（同一键 200ms 内只允许一次）。
//
// 节流使用真实时间（time.Since）：WithClock 注入的时钟只用于 Meta 时间戳，
// 不应该影响"要不要读盘"这种运维行为。
func (d *Dir) claimReload(key string) bool {
	now := time.Now()
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()
	if d.reloadAt == nil {
		d.reloadAt = make(map[string]time.Time, 8)
	}
	if when, seen := d.reloadAt[key]; seen && now.Sub(when) < reloadInterval {
		return false
	}
	if len(d.reloadAt) >= maxReloadKeys {
		// 表满了就整体重置：节流是尽力而为的保护，不能成为内存增长的来源。
		d.reloadAt = make(map[string]time.Time, 8)
	}
	d.reloadAt[key] = now
	d.reloads++
	return true
}

// reloadFromDisk 从磁盘重载索引，并收养只有文件、没有索引条目的会话。
//
// 它**不**持 processMu：重载是读多写少的一次性动作，且每次写 index.json 都是
// 原子替换；与并发写入撞上时结果是 last-writer-wins，下一轮自愈会再收敛
// （持锁反而会在 Append 已持锁时造成自锁）。
func (d *Dir) reloadFromDisk(ctx context.Context, reason string) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return
		}
	}
	d.indexMu.Lock()
	loadErr := d.load()
	d.indexMu.Unlock()
	if loadErr != nil {
		d.emitf("session store: 自愈重载失败（%s）: %v", reason, loadErr)
		return
	}
	adopted, adoptErr := d.adoptOrphanSessions()
	if adoptErr != nil {
		d.emitf("session store: 收养磁盘会话失败（%s）: %v", reason, adoptErr)
	}
	if adopted > 0 {
		d.emitf("session store: 自愈重载从磁盘收养了 %d 个会话", adopted)
	}
}

// adoptOrphanSessions 把 sessions/*.ndjson 里存在、但索引里没有的会话补进索引。
//
// 这些"孤儿"来自：另一个实例刚 Create（它的索引还没写）、索引被 last-writer-wins
// 覆盖、或者上次进程在 SaveState 之前崩掉。条数与字节数都从文件重算，因此收养
// 出来的 Meta 是准确的。
func (d *Dir) adoptOrphanSessions() (int, error) {
	entries, err := os.ReadDir(d.sessionsDir)
	if err != nil {
		return 0, err
	}
	type orphan struct {
		id   string
		meta Meta
	}
	orphans := make([]orphan, 0, 4)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, sessionExt) {
			continue
		}
		id := strings.TrimSuffix(name, sessionExt)
		if !validStem(id) {
			continue
		}
		if _, exists := d.lookup(id); exists {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		read, readErr := readRecords(filepath.Join(d.sessionsDir, name))
		if readErr != nil && !errors.Is(readErr, ErrCorrupt) {
			continue
		}
		stamp := info.ModTime().UTC()
		orphans = append(orphans, orphan{id: id, meta: Meta{
			ID:        id,
			Title:     clampTitle(firstUserTitle(read.messages)),
			CreatedAt: stamp,
			UpdatedAt: stamp,
			Messages:  read.records,
			Bytes:     read.size,
		}})
	}
	if len(orphans) == 0 {
		return 0, nil
	}
	d.indexMu.Lock()
	for _, entry := range orphans {
		if _, exists := d.index[entry.id]; exists {
			continue
		}
		d.index[entry.id] = entry.meta
	}
	saveErr := d.saveIndexLocked()
	d.indexMu.Unlock()
	return len(orphans), saveErr
}

// refreshListIfStale 在 List 之前用一次 stat 判断磁盘索引是否被别的实例改过；
// 变了才重载（避免单实例的 List 每次读盘）。
func (d *Dir) refreshListIfStale(ctx context.Context) {
	info, err := os.Stat(d.indexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && d.claimReload(listReloadKey) {
			d.reloadFromDisk(ctx, listReloadKey)
		}
		return
	}
	d.indexMu.RLock()
	stamp := d.indexStamp
	d.indexMu.RUnlock()
	if info.ModTime() == stamp.at && info.Size() == stamp.size {
		return
	}
	if !d.claimReload(listReloadKey) {
		return
	}
	d.reloadFromDisk(ctx, listReloadKey)
}

// refreshBeforeWrite 在写入前用一次 stat 判断"别的实例是否改过索引"，变了就
// 重载 + 收养。它保证本实例的索引是磁盘的**超集**，从而不会在保存时把别的实例
// 的会话条目抹掉（last-writer-wins 的伤害被压到最小）。
//
// 只 stat 不读盘：单实例路径上只多一次 stat（计数器 reloads 不动）。
func (d *Dir) refreshBeforeWrite(ctx context.Context, reason string) {
	info, err := os.Stat(d.indexPath)
	if err != nil {
		return
	}
	d.indexMu.RLock()
	stamp := d.indexStamp
	d.indexMu.RUnlock()
	if info.ModTime() == stamp.at && info.Size() == stamp.size {
		return
	}
	if !d.claimReload(reason) {
		return
	}
	d.reloadFromDisk(ctx, reason)
}

// recordIndexStamp 记录 index.json 的最新指纹。调用方必须持有 indexMu 的写锁。
func (d *Dir) recordIndexStamp() {
	info, err := os.Stat(d.indexPath)
	if err != nil {
		d.indexStamp = indexStamp{}
		return
	}
	d.indexStamp = indexStamp{at: info.ModTime(), size: info.Size()}
}

// 编译期断言：收养/重载路径同样只依赖 rig.Message 作为记录类型。
var _ = rig.Message{}
