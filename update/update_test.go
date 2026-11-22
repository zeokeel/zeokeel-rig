package update_test

// 版本比较、稳定版选择、清单解析与平台资产命名的单元测试。

import (
	"errors"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/update"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		{"1.2.0", "1.2.0", 0},
		{"1.2", "1.2.0", 0},
		{"1", "1.0.0", 0},
		{"1.2.1", "1.2.0", 1},
		{"1.2.0", "1.2.1", -1},
		{"2.0.0", "1.9.9", 1},
		{"v1.2.0", "1.2.0", 0},
		{"V1.2.3", "v1.2.3", 0},
		{"1.2.0-rc.1", "1.2.0", -1},
		{"1.2.0", "1.2.0-rc.1", 1},
		{"1.2.0-rc.10", "1.2.0-rc.2", 1},
		{"1.2.0-alpha", "1.2.0-beta", -1},
		{"1.2.0-alpha.1", "1.2.0-alpha", 1},
		{"1.2.0+build.5", "1.2.0", 0},
		{" 1.2.0 ", "1.2.0", 0},
	}
	for _, testCase := range cases {
		got, err := update.CompareVersions(testCase.left, testCase.right)
		if err != nil {
			t.Fatalf("CompareVersions(%q, %q): %v", testCase.left, testCase.right, err)
		}
		if got != testCase.want {
			t.Fatalf("CompareVersions(%q, %q) = %d, want %d", testCase.left, testCase.right, got, testCase.want)
		}
	}
	for _, value := range []string{"", "x", "1.x", "1.2.3.4", "1.2.0-", "1.2.0-alpha..1", "-1.0.0"} {
		if _, err := update.CompareVersions(value, "1.0.0"); !errors.Is(err, update.ErrVersion) {
			t.Fatalf("CompareVersions(%q) error = %v, want ErrVersion", value, err)
		}
	}
}

func TestParseVersionString(t *testing.T) {
	version, err := update.ParseVersion("v2.3.4-rc.1+build.7")
	if err != nil {
		t.Fatalf("ParseVersion: %v", err)
	}
	if version.Major != 2 || version.Minor != 3 || version.Patch != 4 {
		t.Fatalf("version = %+v, want 2.3.4", version)
	}
	if got := version.String(); got != "2.3.4-rc.1" {
		t.Fatalf("String() = %q, want 2.3.4-rc.1 (build metadata dropped)", got)
	}
	plain, err := update.ParseVersion("1.2.3")
	if err != nil {
		t.Fatalf("ParseVersion(plain): %v", err)
	}
	if got := plain.String(); got != "1.2.3" {
		t.Fatalf("String() = %q, want 1.2.3", got)
	}
}

func TestSelectReleasePrefersNewestStable(t *testing.T) {
	releases := []update.Release{
		{Version: "1.4.0"},
		{Version: "2.0.0", Draft: true},
		{Version: "1.5.0-rc.1", Prerelease: true},
		{Version: "1.5.0", Prerelease: true},
		{Version: "not-a-version"},
		{Version: "1.3.9"},
	}
	selected, err := update.SelectRelease(releases)
	if err != nil {
		t.Fatalf("SelectRelease: %v", err)
	}
	if selected.Version != "1.4.0" {
		t.Fatalf("selected = %q, want 1.4.0", selected.Version)
	}
}

func TestSelectReleaseTieBreaksByPublishedAt(t *testing.T) {
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	selected, err := update.SelectRelease([]update.Release{
		{Version: "1.2.0", PublishedAt: early, Notes: "old"},
		{Version: "1.2.0", PublishedAt: late, Notes: "new"},
	})
	if err != nil {
		t.Fatalf("SelectRelease: %v", err)
	}
	if selected.Notes != "new" {
		t.Fatalf("selected = %+v, want the later publication", selected)
	}
}

func TestSelectReleaseNoStableCandidate(t *testing.T) {
	_, err := update.SelectRelease([]update.Release{
		{Version: "2.0.0", Draft: true},
		{Version: "1.0.0-rc.1", Prerelease: true},
		{Version: "bad"},
	})
	if !errors.Is(err, update.ErrNoRelease) {
		t.Fatalf("SelectRelease error = %v, want ErrNoRelease", err)
	}
}

func TestParseManifestAndAssetFor(t *testing.T) {
	manifest, err := update.ParseManifest([]byte(`{
		"version": "v1.2.3",
		"notes": "fixes",
		"assets": [
			{"goos": "linux", "goarch": "arm64", "url": "https://example.test/linux-arm64", "sha256": "abc", "size": 10},
			{"goos": "windows", "goarch": "AMD64", "url": "https://example.test/windows-amd64.exe", "size": 20}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if manifest.Version != "v1.2.3" || manifest.Notes != "fixes" {
		t.Fatalf("manifest = %+v", manifest)
	}
	asset, ok := manifest.AssetFor("WINDOWS", "amd64")
	if !ok || asset.URL != "https://example.test/windows-amd64.exe" || asset.Size != 20 {
		t.Fatalf("AssetFor(windows/amd64) = %+v/%v", asset, ok)
	}
	if _, ok := manifest.AssetFor("darwin", "amd64"); ok {
		t.Fatal("AssetFor matched a missing platform")
	}
	if _, ok := manifest.AssetFor("", "amd64"); ok {
		t.Fatal("AssetFor matched an empty GOOS")
	}
}

func TestParseManifestRejectsBadDocuments(t *testing.T) {
	for _, document := range []string{
		`{"version":""}`,
		`{"version":"not-a-version"}`,
		`{"version":"1.2.3"`,
		`[]`,
	} {
		if _, err := update.ParseManifest([]byte(document)); !errors.Is(err, update.ErrManifest) {
			t.Fatalf("ParseManifest(%s) error = %v, want ErrManifest", document, err)
		}
	}
}

func TestDefaultPlatformAssetSelection(t *testing.T) {
	assets := []update.Asset{
		{Name: "app-windows-amd64.exe"},
		{Name: "app-setup-1.2.3-amd64.exe"},
		{Name: "app-linux-amd64.AppImage"},
		{Name: "app-linux-amd64.tar.gz"},
		{Name: "app-1.2.3-arm64.dmg"},
		{Name: "app-1.2.3-universal.dmg"},
		{Name: "app-darwin-arm64.tar.gz"},
	}
	release := update.Release{Version: "1.2.3", Assets: assets}
	cases := []struct {
		name   string
		goos   string
		goarch string
		want   string
	}{
		{"windows prefers setup installer", "windows", "amd64", "app-setup-1.2.3-amd64.exe"},
		{"linux prefers AppImage", "linux", "amd64", "app-linux-amd64.AppImage"},
		{"darwin prefers arch dmg", "darwin", "arm64", "app-1.2.3-arm64.dmg"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			asset, ok := update.SelectAsset(release, update.DefaultPlatform(testCase.goos, testCase.goarch))
			if !ok || asset.Name != testCase.want {
				t.Fatalf("SelectAsset = %+v/%v, want %q", asset, ok, testCase.want)
			}
		})
	}

	// 没有架构专属 dmg 时退到任意 dmg（universal 发布）。
	asset, ok := update.SelectAsset(update.Release{
		Version: "1.2.3",
		Assets:  []update.Asset{{Name: "app-1.2.3-universal.dmg"}},
	}, update.DefaultPlatform("darwin", "amd64"))
	if !ok || asset.Name != "app-1.2.3-universal.dmg" {
		t.Fatalf("universal dmg fallback = %+v/%v", asset, ok)
	}
}

func TestSelectAssetCustomNaming(t *testing.T) {
	release := update.Release{
		Version: "v2.1.0",
		Assets: []update.Asset{
			{Name: "tool_2.1.0_linux_amd64.zip"},
			{Name: "tool_2.1.0_linux_amd64.tar.gz"},
		},
	}
	platform := update.Platform{
		GOOS:     "linux",
		GOARCH:   "amd64",
		Patterns: []string{"tool_{version}_{os}_{arch}.zip"},
	}
	asset, ok := update.SelectAsset(release, platform)
	if !ok || asset.Name != "tool_2.1.0_linux_amd64.zip" {
		t.Fatalf("custom SelectAsset = %+v/%v", asset, ok)
	}
	if _, ok := update.SelectAsset(release, update.Platform{GOOS: "plan9", GOARCH: "amd64"}); ok {
		t.Fatal("unknown GOOS selected an asset")
	}
}
