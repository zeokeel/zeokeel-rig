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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/memextract"
	"github.com/zeokeel/zeokeel-rig/internal/memvfs"
	"github.com/zeokeel/zeokeel-rig/internal/memwiki"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
	memsqlite "github.com/zeokeel/zeokeel-rig/memoryvfs/sqlite"
)

// NewFileStore 打开/创建一个目录下的 SQLite 库（<dir>/memory.db）。
func NewFileStore(dir string, opts ...memoryvfs.Option) (memoryvfs.Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("memory: empty store directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return memsqlite.Open("file:"+filepath.Join(dir, "memory.db")+"?_pragma=busy_timeout(5000)",
		memsqlite.WithWorkspace(filepath.Base(filepath.Clean(dir))),
		memsqlite.WithExtractor(memextract.New()), memsqlite.WithOptions(opts...))
}

// NewMemoryStore 返回进程内 SQLite 库（测试与无盘环境）。
func NewMemoryStore(opts ...memoryvfs.Option) memoryvfs.Store {
	store, err := memsqlite.Open(":memory:", memsqlite.WithExtractor(memextract.New()), memsqlite.WithOptions(opts...))
	if err != nil {
		panic(fmt.Sprintf("memory: open in-memory sqlite: %v", err))
	}
	return store
}

// NewWorkspaceStore 返回"按工作区分区"的 SQLite 库：
// `<stateDir>/memory/<workspaceID>/memory.db`（stateDir 为空时用用户级默认状态目录），
// 行按 workspace_id 再隔离一次。
func NewWorkspaceStore(stateDir, workspaceRoot string, opts ...memoryvfs.Option) (memoryvfs.Store, error) {
	dir, err := memwiki.WikiDir(stateDir, workspaceRoot)
	if err != nil {
		return nil, err
	}
	workspaceID, err := WorkspaceID(workspaceRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	store, err := memsqlite.Open("file:"+filepath.Join(dir, "memory.db")+"?_pragma=busy_timeout(5000)",
		memsqlite.WithWorkspace(workspaceID), memsqlite.WithExtractor(memextract.New()), memsqlite.WithOptions(opts...))
	if err != nil {
		return nil, err
	}
	migrateLegacyV2(store, dir)
	// 首次切换到 v2 时把旧 wiki 页迁进来；迁移失败不阻断打开（旧 wiki 还在），
	// 只是把原因写进日志。成功后写 `.v2-migrated` 标记，重复打开不再扫描。
	if migrated, migrateErr := MigrateWiki(store, dir); migrateErr != nil {
		slog.Warn("kit: migrate legacy memory wiki failed (legacy pages are left untouched)",
			"dir", dir, "migrated", migrated, "err", migrateErr)
	}
	return store, nil
}

// migrateLegacyV2 把旧的 v2 文件后端（<dir>/v2/entries）一次性导入 SQLite。
// 失败只记录日志；旧目录保留，成功后写 .sqlite-migrated 标记避免重复扫描。
func migrateLegacyV2(store memoryvfs.Store, dir string) {
	legacyDir := filepath.Join(dir, "v2")
	if _, err := os.Stat(filepath.Join(legacyDir, "entries")); err != nil {
		return
	}
	marker := filepath.Join(dir, ".sqlite-migrated")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	legacy, err := memvfs.Open(legacyDir)
	if err != nil {
		slog.Warn("kit: open legacy v2 file memory failed", "dir", legacyDir, "err", err)
		return
	}
	defer func() { _ = legacy.Close() }()
	ctx := context.Background()
	pack, err := legacy.Export(ctx, memoryvfs.RootURI)
	if err != nil {
		slog.Warn("kit: export legacy v2 memory failed", "dir", legacyDir, "err", err)
		return
	}
	if _, err := store.Import(ctx, pack, memoryvfs.ImportOptions{}); err != nil {
		slog.Warn("kit: import legacy v2 memory failed", "dir", legacyDir, "err", err)
		return
	}
	_ = os.WriteFile(marker, []byte("migrated\n"), 0o600)
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
	type migrationMarker struct {
		Failed []string `json:"failed,omitempty"`
	}
	var failed []string
	if data, readErr := os.ReadFile(marker); readErr == nil {
		var previous migrationMarker
		if json.Unmarshal(data, &previous) == nil {
			failed = previous.Failed
		}
		if len(failed) == 0 {
			return 0, nil
		}
	}
	wiki, err := memwiki.NewFileWiki(legacyDir)
	if err != nil {
		return 0, err
	}
	metas, err := wiki.Index(context.Background())
	if err != nil {
		return 0, err
	}
	available := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		available[meta.Name] = struct{}{}
	}
	names := make([]string, 0, len(metas))
	if len(failed) > 0 {
		// 只重试上次失败且旧页仍存在的条目；用户删掉的失败页不复活。
		for _, name := range failed {
			if _, ok := available[name]; ok {
				names = append(names, name)
			}
		}
	} else {
		for _, meta := range metas {
			names = append(names, meta.Name)
		}
	}
	migrated := 0
	var errs []error
	remaining := make([]string, 0, 4)
	for _, name := range names {
		uri, uriErr := memoryvfs.Join("user/self/memories/pages", name)
		if uriErr != nil {
			errs = append(errs, fmt.Errorf("legacy page %q: %w", name, uriErr))
			remaining = append(remaining, name)
			continue
		}
		if _, statErr := store.Stat(context.Background(), uri); statErr == nil {
			continue
		}
		page, readErr := wiki.Read(context.Background(), name)
		if readErr != nil {
			errs = append(errs, fmt.Errorf("read legacy page %q: %w", name, readErr))
			remaining = append(remaining, name)
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
			errs = append(errs, fmt.Errorf("write migrated page %q: %w", name, writeErr))
			remaining = append(remaining, name)
			continue
		}
		migrated++
	}
	data, _ := json.Marshal(migrationMarker{Failed: remaining})
	if err := os.WriteFile(marker, data, 0o600); err != nil {
		return migrated, fmt.Errorf("memory: write migration marker: %w", err)
	}
	return migrated, errors.Join(errs...)
}
