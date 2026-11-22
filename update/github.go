package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// GitHub Releases 源默认值。
const (
	// DefaultGitHubAPIBaseURL 是公开的 GitHub REST API base URL（无需 token）。
	DefaultGitHubAPIBaseURL = "https://api.github.com"
	// DefaultChecksumsAsset 是存放 "<sha256>  <filename>" 行的资产名。
	DefaultChecksumsAsset = "SHA256SUMS.txt"
	// DefaultHTTPTimeout 是一次 JSON 请求的默认超时；资产下载不受它限制。
	DefaultHTTPTimeout = 15 * time.Second
	// DefaultUserAgent 是缺省的 User-Agent（GitHub 拒绝空 User-Agent）。
	DefaultUserAgent = "zeokeel-rig-update"
	// MaxManifestBytes 是 release/manifest JSON 响应的上限（1 MiB）。
	MaxManifestBytes = 1 << 20
	// MaxChecksumsBytes 是 checksums 资产正文的上限（1 MiB）。
	MaxChecksumsBytes = 1 << 20
)

// GitHubConfig 配置 GitHub Releases 源。所有外部依赖都可注入。
type GitHubConfig struct {
	// Repo 是 "owner/name"。
	Repo string
	// BaseURL 覆盖 GitHub REST API base URL（默认 DefaultGitHubAPIBaseURL）。
	BaseURL string
	// Client 是 HTTP 客户端；nil 时使用带 DefaultHTTPTimeout 的新客户端。
	Client *http.Client
	// ChecksumsAsset 是存放 SHA-256 行的资产名（默认 DefaultChecksumsAsset）。
	ChecksumsAsset string
	// UserAgent 覆盖请求的 User-Agent（默认 DefaultUserAgent）。
	UserAgent string
	// AllowInsecureHTTP 允许 http:// API/资产/checksums URL（仅测试/内网）。
	AllowInsecureHTTP bool
	// Now 是检查结果里 CheckedAt 的时钟；nil 时使用 time.Now。
	Now func() time.Time
}

// GitHubSource 从 GitHub Releases 读取稳定版并解析平台资产与 SHA-256。
type GitHubSource struct {
	owner             string
	name              string
	baseURL           string
	client            *http.Client
	checksumsAsset    string
	userAgent         string
	allowInsecureHTTP bool
	now               func() time.Time
}

// NewGitHubSource 构建一个 GitHub Releases 源。Repo 必须是 "owner/name"。
func NewGitHubSource(cfg GitHubConfig) (*GitHubSource, error) {
	repo := strings.TrimSpace(cfg.Repo)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("update: github repo must be \"owner/name\", got %q", repo)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultGitHubAPIBaseURL
	}
	checksumsAsset := strings.TrimSpace(cfg.ChecksumsAsset)
	if checksumsAsset == "" {
		checksumsAsset = DefaultChecksumsAsset
	}
	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &GitHubSource{
		owner:             strings.TrimSpace(owner),
		name:              strings.TrimSpace(name),
		baseURL:           baseURL,
		client:            client,
		checksumsAsset:    checksumsAsset,
		userAgent:         userAgent,
		allowInsecureHTTP: cfg.AllowInsecureHTTP,
		now:               now,
	}, nil
}

// githubReleasePayload 是 GET /repos/{owner}/{repo}/releases/latest 里本包
// 需要的子集。该端点按 GitHub 定义只返回最新稳定版（不含 draft/prerelease），
// 这里仍防御性地拒绝带这两个标记的响应。
type githubReleasePayload struct {
	TagName     string               `json:"tag_name"`
	Body        string               `json:"body"`
	Draft       bool                 `json:"draft"`
	Prerelease  bool                 `json:"prerelease"`
	PublishedAt time.Time            `json:"published_at"`
	Assets      []githubReleaseAsset `json:"assets"`
}

// githubReleaseAsset 是一个发布附件的下载信息。
type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	// Digest 是 GitHub 可选的内容摘要（"sha256:<hex>"）。
	Digest string `json:"digest"`
}

// Latest 读取仓库最新的稳定版。draft/prerelease 响应被拒绝为 ErrNoRelease。
func (s *GitHubSource) Latest(ctx context.Context) (Release, error) {
	if s == nil {
		return Release{}, errors.New("update: nil GitHub source")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Release{}, err
	}
	if err := s.validateURL(s.baseURL); err != nil {
		return Release{}, err
	}
	endpoint := s.baseURL + "/repos/" + url.PathEscape(s.owner) + "/" + url.PathEscape(s.name) + "/releases/latest"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, fmt.Errorf("%w: build latest release request: %v", ErrManifest, err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", s.userAgent)
	response, err := s.client.Do(request)
	if err != nil {
		return Release{}, fmt.Errorf("update: fetch latest release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("%w: %w: latest release status %s", ErrManifest, ErrHTTPStatus, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxManifestBytes+1))
	if err != nil {
		return Release{}, fmt.Errorf("update: read latest release: %w", err)
	}
	if len(data) > MaxManifestBytes {
		return Release{}, fmt.Errorf("%w: latest release larger than %d bytes", ErrManifest, MaxManifestBytes)
	}
	var payload githubReleasePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return Release{}, fmt.Errorf("%w: latest release: %v", ErrManifest, err)
	}
	if payload.Draft || payload.Prerelease {
		return Release{}, fmt.Errorf("%w: latest endpoint returned a draft/prerelease", ErrNoRelease)
	}
	version := stripVersionPrefix(payload.TagName)
	if version == "" {
		return Release{}, fmt.Errorf("%w: latest release has no tag_name", ErrManifest)
	}
	if _, err := ParseVersion(version); err != nil {
		return Release{}, fmt.Errorf("%w: tag_name %q: %v", ErrManifest, payload.TagName, err)
	}
	assets := make([]Asset, 0, len(payload.Assets))
	for _, asset := range payload.Assets {
		assets = append(assets, Asset{
			Name:   strings.TrimSpace(asset.Name),
			URL:    strings.TrimSpace(asset.BrowserDownloadURL),
			Size:   asset.Size,
			Digest: strings.TrimSpace(asset.Digest),
		})
	}
	return Release{
		Version:     version,
		Notes:       payload.Body,
		PublishedAt: payload.PublishedAt,
		Assets:      assets,
	}, nil
}

// Check 读取最新稳定版、与 current 比较，并在有更新时选出平台资产、解析其
// SHA-256。current 为空或不可解析时按 0.0.0 比较（展示仍保留原值/dev）。
//
// 已是最新时不会要求资产存在（旧发布可能缺少当前平台的资产），返回
// Available=false 的 Result。
func (s *GitHubSource) Check(ctx context.Context, current string, platform Platform) (Result, error) {
	if s == nil {
		return Result{}, errors.New("update: nil GitHub source")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	release, err := s.Latest(ctx)
	if err != nil {
		return Result{}, err
	}
	currentDisplay, currentComparable := currentVersion(current)
	comparison, err := CompareVersions(currentComparable, release.Version)
	if err != nil {
		return Result{}, fmt.Errorf("%w: version %q: %v", ErrManifest, release.Version, err)
	}
	checkedAt := time.Now()
	if s.now != nil {
		checkedAt = s.now()
	}
	result := Result{
		CurrentVersion: currentDisplay,
		LatestVersion:  release.Version,
		Notes:          release.Notes,
		CheckedAt:      checkedAt,
	}
	if comparison >= 0 {
		return result, nil
	}
	asset, ok := SelectAsset(release, platform)
	if !ok {
		return Result{}, fmt.Errorf("%w: %s/%s", ErrNoAsset, platform.GOOS, platform.GOARCH)
	}
	if err := s.validateURL(asset.URL); err != nil {
		return Result{}, err
	}
	sum, err := s.SHA256For(ctx, release, asset)
	if err != nil {
		return Result{}, err
	}
	asset.SHA256 = sum
	result.Available = true
	result.Asset = asset
	return result, nil
}

// SHA256For 返回资产的小写十六进制 SHA-256。优先级：资产自带 SHA256 →
// checksums 资产里的对应行 → 提供商 digest。都不可用时返回 ErrNoChecksum：
// 无法校验的更新包不允许下载。
func (s *GitHubSource) SHA256For(ctx context.Context, release Release, asset Asset) (string, error) {
	if s == nil {
		return "", errors.New("update: nil GitHub source")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if sum := strings.ToLower(strings.TrimSpace(asset.SHA256)); sum != "" {
		if !isSHA256Hex(sum) {
			return "", fmt.Errorf("%w: invalid sha256 %q", ErrNoChecksum, asset.SHA256)
		}
		return sum, nil
	}
	var checksumErr error
	if checksumAsset, ok := findChecksumsAsset(release.Assets, s.checksumsAsset); ok {
		sum, err := s.fetchChecksumForAsset(ctx, checksumAsset, asset.Name)
		if err == nil {
			return sum, nil
		}
		// URL 策略错误不能被"降级到 digest"掩盖，否则会悄悄绕过 https 要求。
		if errors.Is(err, ErrInsecureURL) {
			return "", err
		}
		checksumErr = err
	} else {
		checksumErr = fmt.Errorf("release has no %q asset", s.checksumsAsset)
	}
	if digest := normalizeDigest(asset.Digest); digest != "" {
		return digest, nil
	}
	return "", fmt.Errorf("%w: asset %q: %v", ErrNoChecksum, asset.Name, checksumErr)
}

// fetchChecksumForAsset 下载 checksums 资产（上限 MaxChecksumsBytes）并返回
// 选中资产对应的 SHA-256。
func (s *GitHubSource) fetchChecksumForAsset(ctx context.Context, checksumAsset Asset, assetName string) (string, error) {
	if err := s.validateURL(checksumAsset.URL); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumAsset.URL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: build checksums request: %v", ErrManifest, err)
	}
	request.Header.Set("Accept", "text/plain")
	request.Header.Set("User-Agent", s.userAgent)
	response, err := s.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("%w: fetch checksums: %v", ErrManifest, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %w: checksums status %s", ErrManifest, ErrHTTPStatus, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxChecksumsBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: read checksums: %v", ErrManifest, err)
	}
	if len(data) > MaxChecksumsBytes {
		return "", fmt.Errorf("%w: checksums larger than %d bytes", ErrManifest, MaxChecksumsBytes)
	}
	return ParseChecksums(data, assetName)
}

// ValidateURL 按源的安全策略校验 URL：默认只允许 https；AllowInsecureHTTP
// 时才允许 http。供宿主在下载前复核同一个策略。
func (s *GitHubSource) ValidateURL(raw string) error {
	if s == nil {
		return errors.New("update: nil GitHub source")
	}
	return s.validateURL(raw)
}

func (s *GitHubSource) validateURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("%w: empty URL", ErrInsecureURL)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%w: invalid URL %q", ErrInsecureURL, raw)
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if s.allowInsecureHTTP {
			return nil
		}
		return fmt.Errorf("%w: %q", ErrInsecureURL, raw)
	default:
		return fmt.Errorf("%w: unsupported scheme in %q", ErrInsecureURL, raw)
	}
}

// findChecksumsAsset 返回名称匹配（大小写不敏感）的 checksums 资产。
func findChecksumsAsset(assets []Asset, name string) (Asset, bool) {
	wanted := strings.TrimSpace(name)
	if wanted == "" {
		return Asset{}, false
	}
	for _, asset := range assets {
		if strings.EqualFold(strings.TrimSpace(asset.Name), wanted) {
			return asset, true
		}
	}
	return Asset{}, false
}

// ParseChecksums 解析 sha256sum 风格正文（"<hex>  <filename>" 或
// "<hex> *<filename>"），返回 assetName 对应的 64 位小写十六进制。
func ParseChecksums(data []byte, assetName string) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		sum, filename, ok := parseChecksumLine(line)
		if !ok {
			continue
		}
		if checksumNamesAsset(filename, assetName) {
			return sum, nil
		}
	}
	return "", fmt.Errorf("%w: checksums have no entry for %q", ErrNoChecksum, assetName)
}

// parseChecksumLine 解析一行 "<hex><两个空格或空格+*><filename>"。
func parseChecksumLine(line string) (sum, filename string, ok bool) {
	line = strings.TrimSpace(line)
	if len(line) < 66 || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	sum = strings.ToLower(line[:64])
	if !isSHA256Hex(sum) {
		return "", "", false
	}
	rest := line[64:]
	switch {
	case strings.HasPrefix(rest, "  "):
		filename = rest[2:]
	case strings.HasPrefix(rest, " *"):
		filename = rest[2:]
	default:
		return "", "", false
	}
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return "", "", false
	}
	return sum, filename, true
}

// checksumNamesAsset 把 checksums 行里的文件名与资产名比对。sha256sum 可能
// 带 "./" 前缀，有些工具只记录 basename，两者都接受（大小写不敏感）。
func checksumNamesAsset(filename, assetName string) bool {
	wanted := strings.TrimSpace(assetName)
	if wanted == "" {
		return false
	}
	name := strings.TrimSpace(filename)
	name = strings.TrimPrefix(name, "./")
	if strings.EqualFold(name, wanted) {
		return true
	}
	return strings.EqualFold(path.Base(name), path.Base(wanted))
}

// normalizeDigest 规范化 "sha256:<hex>"（缺失或非 SHA-256 时返回空）。
func normalizeDigest(digest string) string {
	trimmed := strings.ToLower(strings.TrimSpace(digest))
	const prefix = "sha256:"
	if !strings.HasPrefix(trimmed, prefix) {
		return ""
	}
	sum := trimmed[len(prefix):]
	if !isSHA256Hex(sum) {
		return ""
	}
	return sum
}

// isSHA256Hex 报告 raw 是否恰好是 64 位小写十六进制。
func isSHA256Hex(raw string) bool {
	if len(raw) != 64 {
		return false
	}
	for _, character := range raw {
		switch {
		case character >= '0' && character <= '9', character >= 'a' && character <= 'f':
		default:
			return false
		}
	}
	return true
}
