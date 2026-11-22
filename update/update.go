package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// 更新检查/下载的可分类错误。调用方用 errors.Is 分类。
var (
	// ErrVersion reports an unparsable version string.
	ErrVersion = errors.New("update: invalid version")
	// ErrManifest reports a malformed release/manifest document.
	ErrManifest = errors.New("update: invalid manifest")
	// ErrNoRelease reports that no stable (non-draft, non-prerelease) release
	// is available.
	ErrNoRelease = errors.New("update: no stable release")
	// ErrNoAsset reports that the release has no asset for the requested
	// platform.
	ErrNoAsset = errors.New("update: no asset for platform")
	// ErrNoChecksum reports that no SHA-256 could be resolved for an asset. An
	// unverifiable update must not be downloaded or installed.
	ErrNoChecksum = errors.New("update: no sha256 for asset")
	// ErrInsecureURL reports a non-https URL while insecure HTTP is not
	// explicitly allowed.
	ErrInsecureURL = errors.New("update: URL must use https")
	// ErrHTTPStatus reports a non-200 response.
	ErrHTTPStatus = errors.New("update: unexpected HTTP status")
	// ErrDownload reports a failed download (transport, status, staging I/O).
	ErrDownload = errors.New("update: download failed")
	// ErrVerify reports a failed size or SHA-256 verification.
	ErrVerify = errors.New("update: verification failed")
)

// Version 是一个解析后的语义化（semver-ish）版本：
// MAJOR[.MINOR[.PATCH]][-prerelease][+build]。缺少的分量按 0 处理；prerelease
// 排在同版本正式版之前；数字标识符按数值比较，字母标识符按字典序比较。
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease []string
}

// ParseVersion 解析版本字符串。接受并忽略前导 "v"/"V"（发布 tag 约定），
// 忽略 "+build" 元数据。
func ParseVersion(raw string) (Version, error) {
	parsed, err := parseVersion(raw)
	if err != nil {
		return Version{}, fmt.Errorf("%w: %v", ErrVersion, err)
	}
	return parsed, nil
}

func parseVersion(raw string) (Version, error) {
	var parsed Version
	text := strings.TrimSpace(raw)
	if text == "" {
		return parsed, errors.New("empty version")
	}
	if len(text) > 1 && (text[0] == 'v' || text[0] == 'V') {
		text = text[1:]
	}
	if index := strings.IndexByte(text, '+'); index >= 0 {
		text = text[:index]
	}
	core := text
	if index := strings.IndexByte(text, '-'); index >= 0 {
		core = text[:index]
		prerelease := text[index+1:]
		if prerelease == "" {
			return parsed, fmt.Errorf("empty prerelease in %q", raw)
		}
		parsed.Prerelease = strings.Split(prerelease, ".")
		for _, identifier := range parsed.Prerelease {
			if identifier == "" {
				return parsed, fmt.Errorf("empty prerelease identifier in %q", raw)
			}
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) > 3 {
		return parsed, fmt.Errorf("too many version components in %q", raw)
	}
	numbers := [3]int{}
	for index, part := range parts {
		if part == "" {
			return parsed, fmt.Errorf("empty version component in %q", raw)
		}
		value := 0
		for _, character := range part {
			if character < '0' || character > '9' {
				return parsed, fmt.Errorf("non-numeric version component %q in %q", part, raw)
			}
			value = value*10 + int(character-'0')
		}
		numbers[index] = value
	}
	parsed.Major, parsed.Minor, parsed.Patch = numbers[0], numbers[1], numbers[2]
	return parsed, nil
}

// CompareVersions 比较两个版本：a < b 返回 -1，a == b 返回 0，a > b 返回 +1。
// 任一版本非法时返回包装了 ErrVersion 的错误。
func CompareVersions(a, b string) (int, error) {
	left, err := ParseVersion(a)
	if err != nil {
		return 0, err
	}
	right, err := ParseVersion(b)
	if err != nil {
		return 0, err
	}
	return left.Compare(right), nil
}

// Compare 比较两个已解析版本，语义同 CompareVersions。
func (v Version) Compare(other Version) int {
	if comparison := compareInt(v.Major, other.Major); comparison != 0 {
		return comparison
	}
	if comparison := compareInt(v.Minor, other.Minor); comparison != 0 {
		return comparison
	}
	if comparison := compareInt(v.Patch, other.Patch); comparison != 0 {
		return comparison
	}
	switch {
	case len(v.Prerelease) == 0 && len(other.Prerelease) == 0:
		return 0
	case len(v.Prerelease) == 0:
		return 1
	case len(other.Prerelease) == 0:
		return -1
	}
	for index := 0; index < len(v.Prerelease) && index < len(other.Prerelease); index++ {
		if comparison := comparePrereleaseID(v.Prerelease[index], other.Prerelease[index]); comparison != 0 {
			return comparison
		}
	}
	return compareInt(len(v.Prerelease), len(other.Prerelease))
}

// String 返回规范化的 "MAJOR.MINOR.PATCH[-prerelease]" 形式（不含 build
// 元数据与前导 v）。
func (v Version) String() string {
	base := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Prerelease) == 0 {
		return base
	}
	return base + "-" + strings.Join(v.Prerelease, ".")
}

func comparePrereleaseID(left, right string) int {
	leftNumber, leftNumeric := numericID(left)
	rightNumber, rightNumeric := numericID(right)
	switch {
	case leftNumeric && rightNumeric:
		return compareInt(leftNumber, rightNumber)
	case leftNumeric:
		return -1
	case rightNumeric:
		return 1
	default:
		return strings.Compare(left, right)
	}
}

func numericID(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	number := 0
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
		number = number*10 + int(character-'0')
	}
	return number, true
}

func compareInt(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

// Asset 是一个可下载的发布资产。Manifest 源用 GOOS/GOARCH 直接匹配；
// GitHub 源用 Name 按平台命名模式匹配。
type Asset struct {
	// Name 是资产文件名（用于命名匹配与落盘文件名）。
	Name string `json:"name,omitempty"`
	// URL 是资产的下载地址。
	URL string `json:"url,omitempty"`
	// Size 是资产字节数；0 表示发布方未提供（下载后按实际大小报告）。
	Size int64 `json:"size,omitempty"`
	// SHA256 是期望的 SHA-256（64 位小写十六进制）；空表示未提供。
	SHA256 string `json:"sha256,omitempty"`
	// Digest 是提供商附带的摘要（例如 GitHub 的 "sha256:<hex>"）；用作
	// SHA-256 的兜底来源。
	Digest string `json:"digest,omitempty"`
	// GOOS/GOARCH 是 Manifest 源里声明的平台；GitHub 源不使用。
	GOOS   string `json:"goos,omitempty"`
	GOARCH string `json:"goarch,omitempty"`
}

// Release 是一次发布（GitHub Releases 或宿主自己的清单）。Draft/Prerelease
// 的发布不会被 SelectRelease 选中。
type Release struct {
	Version     string    `json:"version"`
	Notes       string    `json:"notes,omitempty"`
	Draft       bool      `json:"draft,omitempty"`
	Prerelease  bool      `json:"prerelease,omitempty"`
	PublishedAt time.Time `json:"publishedAt,omitempty"`
	Assets      []Asset   `json:"assets,omitempty"`
}

// Manifest 是通用发布清单文档：一个版本、发布说明和平台资产列表。
type Manifest struct {
	Version string  `json:"version"`
	Notes   string  `json:"notes,omitempty"`
	Assets  []Asset `json:"assets"`
}

// ParseManifest 解码并校验一份发布清单：必须有可解析的 version。
func ParseManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if strings.TrimSpace(manifest.Version) == "" {
		return Manifest{}, fmt.Errorf("%w: missing version", ErrManifest)
	}
	if _, err := ParseVersion(manifest.Version); err != nil {
		return Manifest{}, fmt.Errorf("%w: version %q: %v", ErrManifest, manifest.Version, err)
	}
	return manifest, nil
}

// AssetFor 返回清单中匹配 goos/goarch 的资产（大小写不敏感，取第一个）。
func (m Manifest) AssetFor(goos, goarch string) (Asset, bool) {
	goos = strings.TrimSpace(goos)
	goarch = strings.TrimSpace(goarch)
	if goos == "" || goarch == "" {
		return Asset{}, false
	}
	for _, asset := range m.Assets {
		if strings.EqualFold(strings.TrimSpace(asset.GOOS), goos) &&
			strings.EqualFold(strings.TrimSpace(asset.GOARCH), goarch) {
			return asset, true
		}
	}
	return Asset{}, false
}

// Platform 描述宿主的资产命名方案。Patterns 是按优先级排列的、大小写不敏感的
// 文件名 glob（path.Match），依次尝试，第一个命中的资产获胜。支持
// {os}/{arch}/{version} 占位符；{version} 是去掉前导 v 的发布版本。
//
// Patterns 为空时使用 DefaultPlatform(goos, goarch) 的内置命名。
type Platform struct {
	GOOS     string
	GOARCH   string
	Patterns []string
}

// DefaultPlatform 返回内置的跨平台发布命名：
//
//	Windows:   *-setup-*-<arch>.exe，然后 *-windows-<arch>.exe
//	Linux:     *-linux-<arch>.appimage，然后 *-linux-<arch>.tar.gz
//	macOS:     *-<arch>.dmg，然后任意 *.dmg，再退到 *-darwin-<arch>.tar.gz /
//	           *-darwin-universal.tar.gz
//
// 匹配不区分大小写；未知 GOOS 返回空 Patterns（不会选中任何资产）。
func DefaultPlatform(goos, goarch string) Platform {
	arch := strings.ToLower(strings.TrimSpace(goarch))
	if strings.TrimSpace(goos) == "" || arch == "" {
		return Platform{GOOS: goos, GOARCH: goarch}
	}
	var patterns []string
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "windows":
		patterns = []string{"*-setup-*-{arch}.exe", "*-setup-{arch}.exe", "*-windows-{arch}.exe"}
	case "linux":
		patterns = []string{"*-linux-{arch}.appimage", "*-linux-{arch}.tar.gz"}
	case "darwin":
		patterns = []string{"*-{arch}.dmg", "*.dmg", "*-darwin-{arch}.tar.gz", "*-darwin-universal.tar.gz"}
	}
	return Platform{GOOS: goos, GOARCH: goarch, Patterns: patterns}
}

// SelectAsset 按平台命名从 release 中选出唯一资产。Patterns 优先；为空时用
// DefaultPlatform 的内置命名。没有任何匹配时返回 (Asset{}, false)。
func SelectAsset(release Release, platform Platform) (Asset, bool) {
	patterns := platform.Patterns
	if len(patterns) == 0 {
		patterns = DefaultPlatform(platform.GOOS, platform.GOARCH).Patterns
	}
	if len(patterns) == 0 {
		return Asset{}, false
	}
	version := stripVersionPrefix(release.Version)
	for _, pattern := range patterns {
		expanded := strings.ToLower(expandPattern(pattern, platform.GOOS, platform.GOARCH, version))
		if expanded == "" {
			continue
		}
		for _, asset := range release.Assets {
			name := strings.ToLower(strings.TrimSpace(asset.Name))
			if name == "" {
				continue
			}
			if matched, err := path.Match(expanded, name); err == nil && matched {
				return asset, true
			}
		}
	}
	return Asset{}, false
}

func expandPattern(pattern, goos, goarch, version string) string {
	replacer := strings.NewReplacer(
		"{os}", strings.ToLower(strings.TrimSpace(goos)),
		"{arch}", strings.ToLower(strings.TrimSpace(goarch)),
		"{version}", strings.ToLower(strings.TrimSpace(version)),
	)
	return replacer.Replace(strings.TrimSpace(pattern))
}

// SelectRelease 返回版本最高的非 draft、非 prerelease 发布。版本不可解析的
// 发布被跳过（发布列表里的坏 tag 不应阻断选择）；同版本时取 PublishedAt 更晚
// 的一条；没有可用发布时返回 ErrNoRelease。
func SelectRelease(releases []Release) (Release, error) {
	var best Release
	var bestVersion Version
	found := false
	for _, release := range releases {
		if release.Draft || release.Prerelease {
			continue
		}
		version, err := ParseVersion(release.Version)
		if err != nil {
			continue
		}
		comparison := 0
		if found {
			comparison = version.Compare(bestVersion)
		}
		if !found || comparison > 0 || (comparison == 0 && release.PublishedAt.After(best.PublishedAt)) {
			best = release
			bestVersion = version
			found = true
		}
	}
	if !found {
		return Release{}, ErrNoRelease
	}
	return best, nil
}

// Result 是一次完成的更新检查。Available 为 false 时 Asset 为空。
type Result struct {
	CurrentVersion string
	LatestVersion  string
	Available      bool
	Notes          string
	Asset          Asset
	CheckedAt      time.Time
}

// stripVersionPrefix 去掉发布 tag 约定的前导 v/V。
func stripVersionPrefix(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > 1 && (trimmed[0] == 'v' || trimmed[0] == 'V') {
		return trimmed[1:]
	}
	return trimmed
}

// currentVersion 返回展示用版本和比较用版本：空版本展示 "dev"、按 0.0.0
// 比较；不可解析的版本原样展示、按 0.0.0 比较（让发布源总能提供更新，而不是
// 因为本地 dev 构建的版本字符串而拒绝）。
func currentVersion(raw string) (display, comparable string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "dev", "0.0.0"
	}
	if _, err := ParseVersion(raw); err != nil {
		return raw, "0.0.0"
	}
	return raw, raw
}
