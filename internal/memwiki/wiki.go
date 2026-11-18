// Package memwiki 提供"渐进式 wiki 记忆"的存储层：一页 = 一个主题，索引只带元数据，
// 正文按需读取。
//
// # 为什么是 wiki 而不是"条目列表"
//
// 个人客户端的记忆只能在**需要时**进上下文，否则一次全倒进去会把窗口塞满、把注意力
// 稀释。wiki 的组织方式天然满足这一点：
//
//   - `Index` 只返回 `name — summary (N 天前)` 这样的元数据行，几十页也只有一两 KB；
//   - `Read` 才把某一页的正文取回来（并有 8 KiB 上限）；
//   - `Search` 在标题/摘要/正文里找，仍然只返回元数据行，让模型自己决定读哪页。
//
// # 两种实现
//
//   - `FileWiki`（NewFileWiki）：落到用户级状态目录，跨进程/跨会话持久；
//   - `MemoryWiki`（NewMemoryWiki）：进程内，测试与无盘环境用。
//
// 两者语义一致（同一套 front-matter、slug 校验与 Log 追加规则），因此工具层只依赖
// `Wiki` 接口。
//
// # 磁盘布局
//
//	<stateDir>/memory/<workspaceID>/pages/<name>.md   一页一个文件（0600）
//	<stateDir>/memory/<workspaceID>/index.json        派生缓存（可重建，损坏即重建）
//
// 页面文件是 front-matter + Markdown 正文：
//
//	---
//	title: Build flaky tests
//	system: false
//	summary: 记录偶发失败的测试与重试策略
//	updated: 2026-11-18T08:00:00Z
//	---
//
//	# Build flaky tests
//
//	正文……
//
// 不用 YAML 库（本仓库只允许 mvdan.cc/sh 一个依赖）：front-matter 是"每行一个
// `key: value`"，值取整行剩余部分（去首尾空白），不支持多行值。
package memwiki

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MaxSummaryBytes 是一句话摘要的上限（UTF-8 字节）。index 只显示摘要，太长就失去
// "一眼扫过"的意义。
const MaxSummaryBytes = 200

// MaxNameLength 是 slug 的长度上限。
const MaxNameLength = 64

// LogHeading 是"追加日志"段的标题。Append 与覆盖写都会写进这一段。
const LogHeading = "## Log"

// 本包的哨兵错误。调用方（工具层）用 errors.Is 判断后渲染成模型可读文案。
var (
	// ErrInvalidName 表示 slug 不合法（只允许 [a-z0-9-]{1,64}）。
	ErrInvalidName = errors.New("memwiki: invalid page name")
	// ErrSummaryRequired 表示缺少摘要（index 只显示摘要，没有摘要的页面等于不可见）。
	ErrSummaryRequired = errors.New("memwiki: summary is required")
	// ErrSummaryTooLong 表示摘要超过 MaxSummaryBytes。
	ErrSummaryTooLong = errors.New("memwiki: summary is too long")
	// ErrNotFound 表示页面不存在。
	ErrNotFound = errors.New("memwiki: page not found")
	// ErrEmptyContent 表示正文为空（允许，但写入空正文等于清空页面，需要显式 delete）。
	ErrEmptyContent = errors.New("memwiki: content is empty")
)

// Page 是一页记忆。
type Page struct {
	// Name 是 slug（[a-z0-9-]{1,64}），稳定标识，模型用它 read/append/delete。
	Name string
	// Title 是人读标题；空时回退到 Name。
	Title string
	// Summary 是一句话摘要，**必填**（≤200 字节）——这是 index 里显示的全部内容。
	Summary string
	// Content 是 Markdown 正文。
	Content string
	// Updated 是最后修改时间（UTC）。零值时由存储层填当前时间。
	Updated time.Time
}

// PageMeta 是 index/search 返回的元数据（不含正文）。
type PageMeta struct {
	Name    string
	Title   string
	Summary string
	Updated time.Time
	// Bytes 是正文（Content）的字节数，用于"这页有多大"的提示。
	Bytes int
}

// Wiki 是渐进式 wiki 记忆的存储接口。
//
// 实现必须并发安全，且 Index/Search 只返回元数据——正文只能通过 Read 取。
type Wiki interface {
	// Index 返回全部页面的元数据，按"最近修改"降序（同时间按 Name 升序）。
	Index(ctx context.Context) ([]PageMeta, error)
	// Read 返回一页的完整内容；不存在时返回 ErrNotFound。
	Read(ctx context.Context, name string) (Page, error)
	// Write 覆盖一页（不存在则创建）。Summary 必填；覆盖已有页时会在 Log 段追加一行
	// `- YYYY-MM-DD rewritten (was N bytes)`。
	Write(ctx context.Context, page Page) error
	// Append 把 text 追加到页面的 Log 段（没有则创建），自动带日期前缀。
	Append(ctx context.Context, name, text string) error
	// Delete 删除一页；不存在时返回 ErrNotFound。
	Delete(ctx context.Context, name string) error
	// Search 在标题/摘要/正文里查找，只返回元数据，最多 MaxSearchResults 条。
	Search(ctx context.Context, query string) ([]PageMeta, error)
}

// 结果条数上限：index 给得多一些（它是"目录"），search 少一些（它是"缩小范围"）。
const (
	// MaxIndexResults 是 Index 返回的条数上限（更多时由工具层写 "… and N more"）。
	MaxIndexResults = 60
	// MaxSearchResults 是 Search 返回的条数上限。
	MaxSearchResults = 20
)

var slugPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// ValidateName 校验 slug：只允许小写字母、数字与连字符，长度 1..64。
//
// 刻意**不做**"自动改写"：记忆是模型自己写的，静默把 `Build Notes` 改成
// `build-notes` 会让它之后用原名找不到。
func ValidateName(name string) error {
	if !slugPattern.MatchString(name) {
		return fmt.Errorf("%w: %q (use 1-%d chars of a-z, 0-9 and '-', e.g. \"build-flaky-tests\")",
			ErrInvalidName, name, MaxNameLength)
	}
	return nil
}

// validateSummary 校验并规整摘要：去首尾空白、折叠换行（index 一行一条）。
func validateSummary(summary string) (string, error) {
	collapsed := collapseWhitespace(summary)
	if collapsed == "" {
		return "", fmt.Errorf("%w: the index only shows the summary, so every page needs a one-line summary",
			ErrSummaryRequired)
	}
	if len(collapsed) > MaxSummaryBytes {
		return "", fmt.Errorf("%w: %d bytes > %d; the index only shows the summary, keep it to one short line",
			ErrSummaryTooLong, len(collapsed), MaxSummaryBytes)
	}
	return collapsed, nil
}

// collapseWhitespace 把任意空白序列折叠成一个空格并去掉首尾空白。
func collapseWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// normalizeTitle 返回展示用标题：空则回退到 name。
func normalizeTitle(name, title string) string {
	title = collapseWhitespace(title)
	if title == "" {
		return name
	}
	return title
}

// sortMetas 按"最近修改降序、同时间 Name 升序"排序（确定且便于 UI 逐行渲染）。
func sortMetas(metas []PageMeta) {
	sort.SliceStable(metas, func(i, j int) bool {
		if !metas[i].Updated.Equal(metas[j].Updated) {
			return metas[i].Updated.After(metas[j].Updated)
		}
		return metas[i].Name < metas[j].Name
	})
}

// metaOf 从一页构造元数据。
func metaOf(page Page) PageMeta {
	return PageMeta{
		Name:    page.Name,
		Title:   normalizeTitle(page.Name, page.Title),
		Summary: page.Summary,
		Updated: page.Updated,
		Bytes:   len(page.Content),
	}
}

// ClampText 把 text 截断到 limit 字节（UTF-8 安全），返回截断结果与原始长度。
//
// 导出给工具层复用：`read` 的 8 KiB 上限与摘要的 200 字节上限都要"切在字符边界上"。
func ClampText(text string, limit int) (string, int) {
	if len(text) <= limit {
		return text, len(text)
	}
	if limit <= 0 {
		return "", len(text)
	}
	cut := limit
	for cut > 0 && cut < len(text) && !utf8Start(text[cut]) {
		cut--
	}
	return text[:cut], len(text)
}

// utf8Start 报告 b 是否是 UTF-8 字符的起始字节（续字节是 10xxxxxx）。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
