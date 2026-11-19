// Package memory 是"长期记忆"的存储契约：一个渐进式 wiki，索引每页只带一行摘要，
// 正文只在模型要读时才取回来。
//
// 契约放在这个公开包里，是为了让本模块之外的存储后端也能实现它。
// `internal/memwiki` 自带两个不需要数据库的实现（磁盘文件、进程内）；
// `session/sqlite` 自带把记忆与会话状态放在同一个库里的实现。
//
// 六个方法（Index/Read/Write/Append/Delete/Search）与 `internal/memwiki` 里那套
// 语义完全一致；本包把规则函数（ValidateName/NormalizeSummary/AppendLog/…）原样
// 导出，是为了让第二个实现不必复制规则——复制出来的规则第一次改动就会漂移。
package memory

import "github.com/zeokeel/zeokeel-rig/internal/memwiki"

// Page 是一页记忆。
type Page = memwiki.Page

// PageMeta 是一页记忆的索引项（不含正文）。
type PageMeta = memwiki.PageMeta

// Wiki 是存储契约。
//
// 实现必须并发安全，且 Index/Search 只返回元数据——正文只能通过 Read 取。
type Wiki = memwiki.Wiki

// 后端需要遵守的上限。
const (
	// MaxSummaryBytes 是一句话摘要的上限（UTF-8 字节）。
	MaxSummaryBytes = memwiki.MaxSummaryBytes
	// MaxNameLength 是页名（slug）的长度上限。
	MaxNameLength = memwiki.MaxNameLength
	// LogHeading 是追加日志段的标题（Append 与覆盖写都写进这一段）。
	LogHeading = memwiki.LogHeading
	// MaxIndexResults 是 Index 单次返回的条数上限（更多时由工具层写"… and N more"）。
	MaxIndexResults = memwiki.MaxIndexResults
	// MaxSearchResults 是 Search 单次返回的条数上限。
	MaxSearchResults = memwiki.MaxSearchResults
)

// 调用方用 errors.Is 判断的哨兵错误。
var (
	// ErrInvalidName 表示页名不是 slug（只允许 [a-z0-9-]{1,64}）。
	ErrInvalidName = memwiki.ErrInvalidName
	// ErrSummaryRequired 表示缺少摘要（索引只显示摘要，没有摘要的页等于不可见）。
	ErrSummaryRequired = memwiki.ErrSummaryRequired
	// ErrSummaryTooLong 表示摘要超过 MaxSummaryBytes。
	ErrSummaryTooLong = memwiki.ErrSummaryTooLong
	// ErrNotFound 表示页面不存在。
	ErrNotFound = memwiki.ErrNotFound
)

// ValidateName 校验页名是 slug。
//
// 刻意**不做**自动改写：记忆是模型自己写的，静默把 `Build Notes` 改成
// `build-notes` 会让它之后用原名找不到。
func ValidateName(name string) error { return memwiki.ValidateName(name) }

// NormalizeSummary 校验并规整摘要：去首尾空白、折叠换行（索引一行一条）。
func NormalizeSummary(summary string) (string, error) { return memwiki.NormalizeSummary(summary) }

// NormalizeTitle 返回展示用标题（空则回退到页名）。
func NormalizeTitle(name, title string) string { return memwiki.NormalizeTitle(name, title) }

// AppendLog 把一条带日期的条目追加到正文的 Log 段。
func AppendLog(content, text string) string { return memwiki.AppendLog(content, text) }

// RewriteLog 追加 Write 覆盖已有页时那一行 `rewritten (was N bytes)`。
func RewriteLog(content string, previousBytes int) string {
	return memwiki.RewriteLog(content, previousBytes)
}

// SortMetas 按契约要求排序页面元数据（最近修改降序、同时间按页名升序）。
func SortMetas(metas []PageMeta) { memwiki.SortMetas(metas) }

// ClampText 把 text 截断到 limit 字节，**切在 UTF-8 字符边界上**，返回截断结果与
// 原始字节数。摘要上限与 read 的 8 KiB 上限都靠它：直接 `text[:limit]` 会把一个
// 多字节字符劈成两半，输出里就出现半个汉字。
func ClampText(text string, limit int) (string, int) { return memwiki.ClampText(text, limit) }
