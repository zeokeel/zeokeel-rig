package web

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// 默认值与限制。它们被导出，便于宿主与测试断言文档中承诺的行为。
const (
	// DefaultUserAgent 是 fetcher 没有自定义 user agent 时发送的值。
	DefaultUserAgent = "ZeoKeel-Rig/" + rig.Version
	// DefaultTimeout 限制一次 web_fetch 端到端耗时（含所有重定向）。
	DefaultTimeout = 30 * time.Second
	// DefaultMaxBytes 是 HTML/文本处理前的响应体上限。
	DefaultMaxBytes int64 = 2 << 20 // 2 MiB
	// DefaultMaxChars 是默认输出字符上限。
	DefaultMaxChars = 20000
	// MaxCharsLimit 是单次调用可请求的最大 maxChars。
	MaxCharsLimit = 200000
	// MaxRedirects 是重定向链上限。
	MaxRedirects = 5
	// DefaultSearchLimit 是 web_search 未提供 limit 时的结果数。
	DefaultSearchLimit = 5
	// MaxSearchLimit 限制单次 web_search 的结果数。
	MaxSearchLimit = 20
	// MaxSearchQueries 限制单次 web_search 调用 DSH 兼容 queries 数组的长度。
	MaxSearchQueries = 4
)

// Policy 决定 fetcher 是否可以请求某个 URL。CheckURL 会对初始 URL 与每一跳重定向调用；
// 返回错误会阻止请求（错误文本会展示给模型）。
type Policy interface {
	CheckURL(*url.URL) error
}

// PolicyFunc 把函数适配为 Policy。
type PolicyFunc func(*url.URL) error

// CheckURL 实现 Policy。
func (f PolicyFunc) CheckURL(u *url.URL) error {
	if f == nil {
		return nil
	}
	return f(u)
}

// HostResolver 由能够把主机名解析为已验证 IP 的 policy 实现。传给 fetcher 的 Policy 实现
// 它时，fetcher 会自行解析每个拨号主机、拒绝非公网地址，并为连接钉住已验证 IP。这弥补了
// CheckURL 的“不做 DNS”契约留下的 DNS-rebinding 缺口（sandbox.Policy 实现了它）。
//
// ResolveHost 必须只返回公网单播地址，其余情况返回错误；fetcher 会直接拨号返回的地址。
type HostResolver interface {
	ResolveHost(ctx context.Context, host string) ([]net.IP, error)
}

// Searcher 是 web_search 的可插拔后端。可以包装搜索 API、MCP server 或本地索引。Search
// 可能被并发调用；实现必须可安全并发使用。
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
}

// SearchResult 是 web_search 的一条命中。
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// NewFetcher 返回受 policy 约束的 web_fetch。nil policy 允许所有 URL。返回的工具使用包默认
// 值（30s 超时、2 MiB 响应体、2 万输出字符、DefaultUserAgent）。
func NewFetcher(policy Policy) rig.Tool {
	return NewHTTPFetcher(WithPolicy(policy))
}

// FetchTool 是 web_fetch 的具名构造器，等同于 NewFetcher。
func FetchTool(policy Policy) rig.Tool { return NewFetcher(policy) }

// NewSearcher 返回由 searcher 支撑的 web_search。searcher 为 nil 时，构造出的工具在调用时
// 返回配置错误（宿主可以先构造工具、之后再接后端）。
func NewSearcher(searcher Searcher) rig.Tool {
	return &searchTool{searcher: searcher}
}

// SearchTool 是 web_search 的具名构造器，等同于 NewSearcher。
func SearchTool(searcher Searcher) rig.Tool { return NewSearcher(searcher) }

// --- 共享辅助函数 ---------------------------------------------------------

// truncateRunes 把文本截短到最多 max 个 rune，并报告是否发生截断。
func truncateRunes(text string, max int) (string, bool) {
	if max <= 0 {
		return text, false
	}
	count := 0
	for index := range text {
		if count == max {
			return text[:index], true
		}
		count++
	}
	return text, false
}

// collapseWhitespace 把每一段连续空白折叠为一个空格。
func collapseWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// isHTML 判断内容类型或正文是否看起来像 HTML。
func isHTML(contentType, body string) bool {
	lower := strings.ToLower(contentType)
	if strings.Contains(lower, "html") {
		return true
	}
	prefix := strings.ToLower(strings.TrimSpace(body))
	if len(prefix) > 512 {
		prefix = prefix[:512]
	}
	return strings.HasPrefix(prefix, "<!doctype html") || strings.HasPrefix(prefix, "<html")
}
