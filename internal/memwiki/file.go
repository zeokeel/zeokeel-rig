package memwiki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileWiki 是落到用户级状态目录的 Wiki 实现。
//
// 布局：
//
//	<dir>/pages/<name>.md    一页一个文件（0600）
//	<dir>/index.json         派生缓存（可重建；损坏即重建）
//
// 并发：写操作在进程内串行（记忆写入是低频的人工/模型动作，不值得引入锁竞争）；
// 文件本身用"临时文件 + rename"原子替换，因此崩溃不会留下半页。
type FileWiki struct {
	dir string

	mu  sync.Mutex
	now func() time.Time
}

var _ Wiki = (*FileWiki)(nil)

const (
	pagesDirName  = "pages"
	indexFileName = "index.json"
	pageSuffix    = ".md"
	dirPerm       = 0o700
	filePerm      = 0o600
	indexVersion  = 1
)

// NewFileWiki 打开（必要时创建）一个目录作为 wiki 存储。
//
// 目录与页面文件都是私有权限（0700/0600）：记忆里可能有用户不愿分享的内容。
func NewFileWiki(dir string) (*FileWiki, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return nil, errors.New("memwiki: wiki directory is empty")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("memwiki: resolve wiki directory %q: %w", dir, err)
	}
	wiki := &FileWiki{dir: filepath.Clean(absolute), now: func() time.Time { return time.Now().UTC() }}
	if err := os.MkdirAll(wiki.pagesDir(), dirPerm); err != nil {
		return nil, fmt.Errorf("memwiki: create wiki directory %q: %w", wiki.dir, err)
	}
	return wiki, nil
}

// Dir 返回 wiki 的根目录（诊断用）。
func (w *FileWiki) Dir() string { return w.dir }

// Now 覆盖时钟（测试用）。
func (w *FileWiki) Now(now func() time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = now
}

func (w *FileWiki) pagesDir() string            { return filepath.Join(w.dir, pagesDirName) }
func (w *FileWiki) indexPath() string           { return filepath.Join(w.dir, indexFileName) }
func (w *FileWiki) pagePath(name string) string { return filepath.Join(w.pagesDir(), name+pageSuffix) }

// Index 实现 Wiki：优先读 index.json 缓存，缓存失效（缺失/损坏/文件变动）时重建。
func (w *FileWiki) Index(ctx context.Context) ([]PageMeta, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.indexLocked()
}

func (w *FileWiki) indexLocked() ([]PageMeta, error) {
	entries, err := w.pageFiles()
	if err != nil {
		return nil, err
	}
	if cached, ok := w.readCache(entries); ok {
		metas := make([]PageMeta, 0, len(cached))
		for _, entry := range cached {
			metas = append(metas, PageMeta{
				Name: entry.Name, Title: entry.Title, Summary: entry.Summary,
				Updated: entry.Updated, Bytes: entry.Bytes,
			})
		}
		sortMetas(metas)
		return metas, nil
	}
	metas := make([]PageMeta, 0, len(entries))
	rebuilt := make([]indexEntry, 0, len(entries))
	for name, info := range entries {
		raw, readErr := os.ReadFile(w.pagePath(name))
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) {
				continue // 竞态删除：跳过
			}
			return nil, fmt.Errorf("memwiki: read page %q: %w", name, readErr)
		}
		page := parsePage(name, raw)
		meta := metaOf(page)
		metas = append(metas, meta)
		rebuilt = append(rebuilt, indexEntry{
			Name: meta.Name, Title: meta.Title, Summary: meta.Summary,
			Updated: meta.Updated, Bytes: meta.Bytes,
			Size: info.Size(), ModUnixNano: info.ModTime().UnixNano(),
		})
	}
	sortMetas(metas)
	w.writeCache(rebuilt)
	return metas, nil
}

// pageFiles 返回 pages/ 下所有页面文件的 stat 信息（键 = slug）。
func (w *FileWiki) pageFiles() (map[string]fs.FileInfo, error) {
	dirEntries, err := os.ReadDir(w.pagesDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]fs.FileInfo{}, nil
		}
		return nil, fmt.Errorf("memwiki: list pages: %w", err)
	}
	files := make(map[string]fs.FileInfo, len(dirEntries))
	for _, entry := range dirEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), pageSuffix) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		files[strings.TrimSuffix(entry.Name(), pageSuffix)] = info
	}
	return files, nil
}

// indexEntry 是 index.json 里的一条：既有元数据，也有用于判失效的 stat 指纹。
type indexEntry struct {
	Name        string    `json:"name"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary"`
	Updated     time.Time `json:"updated"`
	Bytes       int       `json:"bytes"`
	Size        int64     `json:"size"`
	ModUnixNano int64     `json:"mod_unix_nano"`
}

// indexCache 是 index.json 的形状。它有 Version：格式变了就整体重建，而不是猜着读。
type indexCache struct {
	Version int          `json:"version"`
	Pages   []indexEntry `json:"pages"`
}

// readCache 读取并校验缓存；不可信时返回 ok=false（调用方重建）。
func (w *FileWiki) readCache(entries map[string]fs.FileInfo) ([]indexEntry, bool) {
	raw, err := os.ReadFile(w.indexPath())
	if err != nil {
		return nil, false
	}
	var cache indexCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		return nil, false // 损坏即重建
	}
	if cache.Version != indexVersion {
		return nil, false
	}
	if len(cache.Pages) != len(entries) {
		return nil, false
	}
	for _, entry := range cache.Pages {
		info, ok := entries[entry.Name]
		if !ok {
			return nil, false
		}
		if info.Size() != entry.Size || info.ModTime().UnixNano() != entry.ModUnixNano {
			return nil, false
		}
	}
	return cache.Pages, true
}

// writeCache 尽力写缓存：失败不影响正确性（下次重建即可）。
func (w *FileWiki) writeCache(entries []indexEntry) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	payload, err := json.MarshalIndent(indexCache{Version: indexVersion, Pages: entries}, "", "  ")
	if err != nil {
		return
	}
	_ = w.writeFileAtomic(w.indexPath(), append(payload, '\n'))
}

// Read 实现 Wiki。
func (w *FileWiki) Read(ctx context.Context, name string) (Page, error) {
	if err := ctxErr(ctx); err != nil {
		return Page{}, err
	}
	if err := ValidateName(name); err != nil {
		return Page{}, err
	}
	raw, err := os.ReadFile(w.pagePath(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Page{}, notFound(name)
		}
		return Page{}, fmt.Errorf("memwiki: read page %q: %w", name, err)
	}
	page := parsePage(name, raw)
	if page.Updated.IsZero() {
		if info, statErr := os.Stat(w.pagePath(name)); statErr == nil {
			page.Updated = info.ModTime().UTC()
		}
	}
	return page, nil
}

// Write 实现 Wiki：覆盖已有页时在 Log 段追加 "rewritten (was N bytes)"。
func (w *FileWiki) Write(ctx context.Context, page Page) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := ValidateName(page.Name); err != nil {
		return err
	}
	summary, err := validateSummary(page.Summary)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	content := page.Content
	if previous, readErr := os.ReadFile(w.pagePath(page.Name)); readErr == nil {
		oldPage := parsePage(page.Name, previous)
		content = appendLogLine(content, rewriteLogText(len(oldPage.Content)))
	}
	page.Title = normalizeTitle(page.Name, page.Title)
	page.Summary = summary
	page.Content = content
	if page.Updated.IsZero() {
		page.Updated = w.now().UTC()
	} else {
		page.Updated = page.Updated.UTC()
	}
	if err := os.MkdirAll(w.pagesDir(), dirPerm); err != nil {
		return fmt.Errorf("memwiki: create pages directory: %w", err)
	}
	if err := w.writeFileAtomic(w.pagePath(page.Name), renderPage(page)); err != nil {
		return fmt.Errorf("memwiki: write page %q: %w", page.Name, err)
	}
	return nil
}

// Append 实现 Wiki。
func (w *FileWiki) Append(ctx context.Context, name, text string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	raw, err := os.ReadFile(w.pagePath(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return notFound(name)
		}
		return fmt.Errorf("memwiki: read page %q: %w", name, err)
	}
	page := parsePage(name, raw)
	page.Content = appendLogLine(page.Content, text)
	page.Updated = w.now().UTC()
	page.Title = normalizeTitle(name, page.Title)
	if err := w.writeFileAtomic(w.pagePath(name), renderPage(page)); err != nil {
		return fmt.Errorf("memwiki: append to page %q: %w", name, err)
	}
	return nil
}

// Delete 实现 Wiki。
func (w *FileWiki) Delete(ctx context.Context, name string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := os.Remove(w.pagePath(name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return notFound(name)
		}
		return fmt.Errorf("memwiki: delete page %q: %w", name, err)
	}
	return nil
}

// Search 实现 Wiki：标题命中 > 摘要命中 > 正文命中，最多 MaxSearchResults 条。
func (w *FileWiki) Search(ctx context.Context, query string) ([]PageMeta, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	w.mu.Lock()
	metas, err := w.indexLocked()
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]PageMeta, len(metas))
	for _, meta := range metas {
		byName[meta.Name] = meta
	}
	// 正文命中需要读文件：只对"标题/摘要都没命中"的页读，减少 IO。
	return searchMetas(metas, func(name string) Page {
		raw, readErr := os.ReadFile(w.pagePath(name))
		if readErr != nil {
			return Page{Name: name}
		}
		return parsePage(name, raw)
	}, query), nil
}

// writeFileAtomic 原子写文件并设私有权限。
func (w *FileWiki) writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".memwiki-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	fail := func(err error) error {
		_ = temp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return fail(err)
	}
	if err := temp.Chmod(filePerm); err != nil {
		return fail(err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
