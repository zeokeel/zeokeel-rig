// Package web provides the network-facing built-in tools: web_fetch (fetch a
// URL and turn it into text) and web_search (delegate to a pluggable search
// backend).
//
// Both tools are standard-library only. web_fetch has a strict policy: HTTP(S)
// only, a bounded redirect chain, a total timeout, a response-size cap and an
// output-character cap, so one call cannot hang a run or flood the model
// context. Which URLs are acceptable is not decided here: a Policy is supplied
// by the caller.
//
// # Policy compatibility
//
// Policy is a one-method interface. Any value with a
// CheckURL(*url.URL) error method satisfies it, including a sandbox package's
// policy type — Rig does not import that package (it has no dependencies), the
// interface is structural (duck typing). A nil Policy allows every URL; hosts
// that run untrusted models should always pass one.
package web

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// Defaults and limits. They are exported so hosts and tests can assert the
// documented behavior.
const (
	// DefaultUserAgent is sent when the fetcher has no custom user agent.
	DefaultUserAgent = "ZeoKeel-Rig/" + rig.Version
	// DefaultTimeout bounds one web_fetch call end to end (all redirects).
	DefaultTimeout = 30 * time.Second
	// DefaultMaxBytes is the response body cap before HTML/text processing.
	DefaultMaxBytes int64 = 2 << 20 // 2 MiB
	// DefaultMaxChars is the default output character cap.
	DefaultMaxChars = 20000
	// MaxCharsLimit is the largest maxChars a call may ask for.
	MaxCharsLimit = 200000
	// MaxRedirects is the redirect chain cap.
	MaxRedirects = 5
	// DefaultSearchLimit is the web_search result count when limit is omitted.
	DefaultSearchLimit = 5
	// MaxSearchLimit caps one web_search result count.
	MaxSearchLimit = 20
)

// Policy decides whether the fetcher may request a URL. CheckURL is called for
// the initial URL and for every redirect hop; returning an error blocks the
// request (the error text is shown to the model).
type Policy interface {
	CheckURL(*url.URL) error
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(*url.URL) error

// CheckURL implements Policy.
func (f PolicyFunc) CheckURL(u *url.URL) error {
	if f == nil {
		return nil
	}
	return f(u)
}

// HostResolver is implemented by policies that can resolve a hostname to
// validated IP addresses. When the Policy given to a fetcher implements it, the
// fetcher resolves every dialed host itself, rejects non-public addresses and
// pins the validated IP for the connection. That closes the DNS-rebinding gap
// left by CheckURL's no-DNS contract (sandbox.Policy implements this).
//
// ResolveHost must return only public unicast addresses and an error for
// anything else; the fetcher dials the returned addresses directly.
type HostResolver interface {
	ResolveHost(ctx context.Context, host string) ([]net.IP, error)
}

// Searcher is the pluggable backend of web_search. Implement it by wrapping a
// search API, an MCP server or a local index. Search may be called
// concurrently; implementations must be safe for concurrent use.
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
}

// SearchResult is one web_search hit.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// NewFetcher returns web_fetch bounded by policy. A nil policy allows every
// URL. The returned tool uses the package defaults (30s timeout, 2 MiB body,
// 20k output characters, DefaultUserAgent).
func NewFetcher(policy Policy) rig.Tool {
	return NewHTTPFetcher(WithPolicy(policy))
}

// FetchTool is the named constructor of web_fetch; it is NewFetcher.
func FetchTool(policy Policy) rig.Tool { return NewFetcher(policy) }

// NewSearcher returns web_search backed by searcher. A nil searcher produces a
// tool that reports a configuration error when invoked (hosts may build the
// tool before the backend is ready).
func NewSearcher(searcher Searcher) rig.Tool {
	return &searchTool{searcher: searcher}
}

// SearchTool is the named constructor of web_search; it is NewSearcher.
func SearchTool(searcher Searcher) rig.Tool { return NewSearcher(searcher) }

// --- shared helpers --------------------------------------------------------

// truncateRunes shortens text to max runes and reports whether it cut.
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

// collapseWhitespace folds every whitespace run into one space.
func collapseWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// isHTML reports whether the content type or body looks like HTML.
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
