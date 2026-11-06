package lsp

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// PathToURI converts a filesystem path to a file:// URI for LSP messages. The
// path is made absolute, slash-normalized and percent-encoded per segment (so
// spaces, '#', '?' and non-ASCII names survive the round trip while the path
// separators stay readable).
func PathToURI(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("lsp: path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("lsp: resolve path %q: %w", path, err)
	}
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	segments := strings.Split(slashed, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return "file://" + strings.Join(segments, "/"), nil
}

// URIToPath converts a file:// URI back to a host filesystem path. Non-file
// URIs (untitled:, https:, jdt:, ...) are rejected with a stable error so
// callers can fall back to displaying the URI as-is.
func URIToPath(uri string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return "", fmt.Errorf("lsp: invalid URI %q: %w", uri, err)
	}
	if !strings.EqualFold(parsed.Scheme, "file") {
		return "", fmt.Errorf("lsp: unsupported URI scheme %q (want a file:// URI)", parsed.Scheme)
	}
	path := parsed.Path
	if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
		path = "//" + parsed.Host + path
	}
	if goruntime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(path), nil
}
