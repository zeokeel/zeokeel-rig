package memory

// v2 后端的构造函数。真正的文件实现放在 `internal/memvfs`（VFS/分层/会话/规则/
// 版本/GC），这里只做"目录约定 + 选项透传"。
//
// 记忆按工作区分区，目录与旧 wiki 同源：
//
//	<stateDir>/memory/<workspaceID>/           旧 wiki（pages/、index.json）
//	<stateDir>/memory/<workspaceID>/v2/        v2 存储（entries/、sessions/…）
//
// 两者并列而不是互相覆盖：宿主可以先把旧 wiki 迁到 v2，再切换工具面。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/memvfs"
	"github.com/zeokeel/zeokeel-rig/internal/memwiki"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// NewFileStore 打开/创建一个目录作为 v2 文件存储。
func NewFileStore(dir string, opts ...memoryvfs.Option) (memoryvfs.Store, error) {
	return memvfs.Open(dir, opts...)
}

// NewMemoryStore 返回进程内的 v2 存储（测试与无盘环境）。
func NewMemoryStore(opts ...memoryvfs.Option) memoryvfs.Store {
	return memvfs.OpenMemory(opts...)
}

// NewWorkspaceStore 返回"按工作区分区"的文件存储：目录是
// `<stateDir>/memory/<workspaceID>/v2`（stateDir 为空时用用户级默认状态目录）。
func NewWorkspaceStore(stateDir, workspaceRoot string, opts ...memoryvfs.Option) (memoryvfs.Store, error) {
	dir, err := memwiki.WikiDir(stateDir, workspaceRoot)
	if err != nil {
		return nil, err
	}
	store, err := memvfs.Open(filepath.Join(dir, "v2"), opts...)
	if err != nil {
		return nil, err
	}
	// 首次切换到 v2 时把旧 wiki 页迁进来；迁移失败不阻断打开（旧 wiki 还在），
	// 只是把原因写进日志。成功后写 `.v2-migrated` 标记，重复打开不再扫描。
	if migrated, migrateErr := MigrateWiki(store, dir); migrateErr != nil {
		slog.Warn("kit: migrate legacy memory wiki failed (legacy pages are left untouched)",
			"dir", dir, "migrated", migrated, "err", migrateErr)
	}
	return store, nil
}

// MigrateWiki 把旧 wiki 目录（`<dir>/pages/*.md`）一次性导入 v2 存储。
//
// 迁移成功后写 `<dir>/.v2-migrated` 标记；再次调用是 no-op。这样用户在 v2 里
// delete/forget 掉的旧页不会在下次重启时被 `pages/*.md` 复活。返回导入的页数。
// 旧页一条不漏地成为 `mem://user/self/memories/pages/<name>`（Type=pages），
// 摘要/标题/时间原样保留，并打上 `migrated=wiki` 标签，便于宿主区分。
func MigrateWiki(store memoryvfs.Store, legacyDir string) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("memory: nil store")
	}
	if strings.TrimSpace(legacyDir) == "" {
		return 0, fmt.Errorf("memory: legacy dir is empty")
	}
	marker := filepath.Join(legacyDir, ".v2-migrated")
	if _, err := os.Stat(marker); err == nil {
		return 0, nil
	}
	wiki, err := memwiki.NewFileWiki(legacyDir)
	if err != nil {
		return 0, err
	}
	metas, err := wiki.Index(context.Background())
	if err != nil {
		return 0, err
	}
	migrated := 0
	var errs []error
	for _, meta := range metas {
		uri, uriErr := memoryvfs.Join("user/self/memories/pages", meta.Name)
		if uriErr != nil {
			errs = append(errs, fmt.Errorf("legacy page %q: %w", meta.Name, uriErr))
			continue
		}
		// 逐页幂等：已存在的页跳过；失败继续下一页，下次打开还能重试。
		if _, statErr := store.Stat(context.Background(), uri); statErr == nil {
			continue
		}
		page, readErr := wiki.Read(context.Background(), meta.Name)
		if readErr != nil {
			errs = append(errs, fmt.Errorf("read legacy page %q: %w", meta.Name, readErr))
			continue
		}
		created := page.Updated
		if created.IsZero() {
			created = time.Now().UTC()
		}
		if _, writeErr := store.Write(context.Background(), memoryvfs.Entry{
			URI:     uri,
			Kind:    memoryvfs.KindMemory,
			Type:    memoryvfs.TypePages,
			Title:   page.Title,
			Summary: page.Summary,
			Content: page.Content,
			Tags:    map[string]string{"migrated": "wiki"},
			Created: created,
			Updated: page.Updated,
		}); writeErr != nil {
			errs = append(errs, fmt.Errorf("write migrated page %q: %w", page.Name, writeErr))
			continue
		}
		migrated++
	}
	if joined := errors.Join(errs...); joined != nil {
		return migrated, joined
	}
	if err := os.WriteFile(marker, []byte("1\n"), 0o600); err != nil {
		return migrated, fmt.Errorf("memory: write migration marker: %w", err)
	}
	return migrated, nil
}
