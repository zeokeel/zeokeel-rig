package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// MaxAssetBytes 是资产大小未知时允许流式下载的硬上限（2 GiB）。已知
// Asset.Size 时按声明大小 + 1 字节设限，超出的响应会被判为大小不符。
const MaxAssetBytes int64 = 2 << 30

// DownloadResult 是一次校验通过的下载结果。宿主拿到 Path 后自行决定安装方式
// （本包不替换运行中的二进制）。
type DownloadResult struct {
	Path   string
	Size   int64
	SHA256 string
}

// Download 把 asset 流式下载到 destDir，同时计算 SHA-256，并校验声明的大小与
// 摘要。成功时文件已原子重命名到 destDir/<安全文件名>；任何失败都不会留下
// 部分文件。
//
// client 为 nil 时使用 http.DefaultClient；调用方应传入带合理超时的客户端，
// 或用 context 控制总时长。asset.SHA256 必填且必须是 64 位十六进制；缺少
// 校验和时返回 ErrNoChecksum（无法校验的更新包不允许落盘）。
func Download(ctx context.Context, client *http.Client, asset Asset, destDir string) (DownloadResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return DownloadResult{}, err
	}
	expectedSHA := strings.ToLower(strings.TrimSpace(asset.SHA256))
	if expectedSHA == "" {
		return DownloadResult{}, fmt.Errorf("%w: asset %q has no sha256", ErrNoChecksum, asset.Name)
	}
	if !isSHA256Hex(expectedSHA) {
		return DownloadResult{}, fmt.Errorf("%w: asset %q has invalid sha256 %q", ErrNoChecksum, asset.Name, asset.SHA256)
	}
	destDir = strings.TrimSpace(destDir)
	if destDir == "" {
		return DownloadResult{}, fmt.Errorf("%w: empty destination directory", ErrDownload)
	}
	if err := validateAssetURL(asset.URL); err != nil {
		return DownloadResult{}, err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return DownloadResult{}, fmt.Errorf("%w: create destination directory: %v", ErrDownload, err)
	}
	name := stagedFileName(asset)
	finalPath := filepath.Join(destDir, name)
	// 纵深防御：stagedFileName 只返回单个安全路径分量，最终路径必须仍在
	// destDir 的直接子级。
	if name == "" || name == "." || name == ".." || filepath.Dir(finalPath) != filepath.Clean(destDir) {
		return DownloadResult{}, fmt.Errorf("%w: unsafe staged file name %q", ErrDownload, name)
	}
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("%w: build asset request: %v", ErrDownload, err)
	}
	response, err := client.Do(request)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("%w: fetch asset: %v", ErrDownload, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return DownloadResult{}, fmt.Errorf("%w: %w: asset status %s", ErrDownload, ErrHTTPStatus, response.Status)
	}

	tmp, err := os.CreateTemp(destDir, ".update-*.part")
	if err != nil {
		return DownloadResult{}, fmt.Errorf("%w: create temp file: %v", ErrDownload, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	// 有界流式读取：已知大小只允许 size+1 字节（多读的 1 字节用于判定超限），
	// 大小未知时限制在 MaxAssetBytes+1。
	limit := MaxAssetBytes + 1
	if asset.Size > 0 {
		limit = asset.Size + 1
		if limit <= 0 {
			// 声明大小接近 int64 上限：不做 +1，避免溢出成负 limit。
			limit = asset.Size
		}
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(response.Body, limit))
	if copyErr != nil {
		_ = tmp.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return DownloadResult{}, ctxErr
		}
		return DownloadResult{}, fmt.Errorf("%w: read asset: %v", ErrDownload, copyErr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return DownloadResult{}, fmt.Errorf("%w: sync download: %v", ErrDownload, err)
	}
	if err := tmp.Close(); err != nil {
		return DownloadResult{}, fmt.Errorf("%w: close download: %v", ErrDownload, err)
	}
	switch {
	case asset.Size > 0 && written != asset.Size:
		return DownloadResult{}, fmt.Errorf("%w: %w: size mismatch (got %d, want %d)", ErrDownload, ErrVerify, written, asset.Size)
	case asset.Size <= 0 && written > MaxAssetBytes:
		return DownloadResult{}, fmt.Errorf("%w: %w: asset larger than %d bytes", ErrDownload, ErrVerify, MaxAssetBytes)
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(sum, expectedSHA) {
		return DownloadResult{}, fmt.Errorf("%w: %w: sha256 mismatch (got %s, want %s)", ErrDownload, ErrVerify, sum, expectedSHA)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return DownloadResult{}, fmt.Errorf("%w: chmod staged file: %v", ErrDownload, err)
	}
	if err := replaceStagedFile(tmpName, finalPath); err != nil {
		return DownloadResult{}, fmt.Errorf("%w: %v", ErrDownload, err)
	}
	committed = true
	return DownloadResult{
		Path:   finalPath,
		Size:   written,
		SHA256: sum,
	}, nil
}

// validateAssetURL 要求绝对的 http(s) URL。GitHubSource 的 https 策略由
// GitHubSource.ValidateURL 单独负责；直接调用 Download 的宿主可使用内网 http。
func validateAssetURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("%w: empty asset URL", ErrDownload)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%w: invalid asset URL %q", ErrDownload, raw)
	}
	switch parsed.Scheme {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("%w: unsupported asset URL scheme in %q", ErrDownload, raw)
	}
}

// stagedFileName 从资产名（或 URL basename）派生一个安全文件名：只保留
// [A-Za-z0-9._-]，去掉路径分隔符，确保落盘文件是 destDir 的直接子级。
func stagedFileName(asset Asset) string {
	base := strings.TrimSpace(asset.Name)
	if base == "" {
		if parsed, err := url.Parse(strings.TrimSpace(asset.URL)); err == nil {
			base = path.Base(parsed.Path)
		}
	}
	base = strings.ReplaceAll(base, "\\", "/")
	base = path.Base(base)
	base = sanitizeFileComponent(base)
	if base == "" || base == "." || base == ".." {
		return "update.bin"
	}
	return base
}

// sanitizeFileComponent 保留 [A-Za-z0-9._-]，其他字符替换为 '_'，并去掉首尾的
// '.'/'_'，因此结果永远不是 "."/".." 且不含路径分隔符。
func sanitizeFileComponent(raw string) string {
	var builder strings.Builder
	builder.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	return strings.Trim(builder.String(), "._")
}

// replaceStagedFile 原子替换 finalPath。Unix rename 可直接覆盖；Windows 不能，
// 因此先删除同名的旧文件再重试。
func replaceStagedFile(tmpPath, finalPath string) error {
	if err := os.Rename(tmpPath, finalPath); err == nil {
		return nil
	}
	if err := os.Remove(finalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace staged file: %v", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename staged file: %v", err)
	}
	return nil
}
