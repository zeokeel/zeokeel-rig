package lsp

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// PathToURI 把文件系统路径转换为 LSP 消息使用的 file:// URI。路径会被绝对化、斜杠
// 规范化并按段百分号编码（因此空格、'#'、'?' 与非 ASCII 文件名可往返，而路径分隔符保持
// 可读）。
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

// URIToPath 把 file:// URI 转换回宿主机文件系统路径。非文件 URI（untitled:、https:、
// jdt: 等）会返回稳定错误，便于调用方回退为原样显示 URI。
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
