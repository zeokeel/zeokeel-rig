package update_test

// GitHub Releases 源的测试：release 选择、资产命名/校验和解析、digest 兜底与
// HTTP/文档错误。全部走 httptest，不访问真实 GitHub。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/update"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestGitHubCheckResolvesChecksumsAsset(t *testing.T) {
	assetData := []byte("installer-bytes")
	sum := sha256Hex(assetData)
	checksums := strings.Repeat("0", 64) + "  other.exe\n" + sum + "  ./app-setup-1.2.3-amd64.exe\n"

	var latestHits, checksumHits atomic.Int32
	var gotUserAgent string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/app/releases/latest":
			latestHits.Add(1)
			gotUserAgent = request.Header.Get("User-Agent")
			writeJSON(t, writer, map[string]any{
				"tag_name":   "v1.2.3",
				"body":       "notes",
				"draft":      false,
				"prerelease": false,
				"assets": []map[string]any{
					{
						"name":                 "app-setup-1.2.3-amd64.exe",
						"browser_download_url": server.URL + "/assets/app-setup-1.2.3-amd64.exe",
						"size":                 len(assetData),
						// checksums 资产优先于 digest：这里放一个错误的 digest。
						"digest": "sha256:" + strings.Repeat("a", 64),
					},
					{
						"name":                 "SHA256SUMS.txt",
						"browser_download_url": server.URL + "/assets/SHA256SUMS.txt",
						"size":                 len(checksums),
					},
				},
			})
		case "/assets/SHA256SUMS.txt":
			checksumHits.Add(1)
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = writer.Write([]byte(checksums))
		case "/assets/app-setup-1.2.3-amd64.exe":
			_, _ = writer.Write(assetData)
		default:
			t.Errorf("unexpected path %q", request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	checkedAt := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	source, err := update.NewGitHubSource(update.GitHubConfig{
		Repo:              "acme/app",
		BaseURL:           server.URL,
		Client:            server.Client(),
		AllowInsecureHTTP: true,
		Now:               func() time.Time { return checkedAt },
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	result, err := source.Check(context.Background(), "1.0.0", update.DefaultPlatform("windows", "amd64"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result.Available {
		t.Fatalf("Available = false, want true: %+v", result)
	}
	if result.CurrentVersion != "1.0.0" || result.LatestVersion != "1.2.3" || result.Notes != "notes" {
		t.Fatalf("result = %+v", result)
	}
	if result.Asset.Name != "app-setup-1.2.3-amd64.exe" ||
		result.Asset.URL != server.URL+"/assets/app-setup-1.2.3-amd64.exe" ||
		result.Asset.Size != int64(len(assetData)) {
		t.Fatalf("asset = %+v", result.Asset)
	}
	if result.Asset.SHA256 != sum {
		t.Fatalf("SHA256 = %q, want checksums entry %q", result.Asset.SHA256, sum)
	}
	if !result.CheckedAt.Equal(checkedAt) {
		t.Fatalf("CheckedAt = %v, want %v", result.CheckedAt, checkedAt)
	}
	if latestHits.Load() != 1 || checksumHits.Load() != 1 {
		t.Fatalf("requests: latest=%d checksums=%d, want 1/1", latestHits.Load(), checksumHits.Load())
	}
	if gotUserAgent != update.DefaultUserAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUserAgent, update.DefaultUserAgent)
	}
}

func TestGitHubCheckFallsBackToDigest(t *testing.T) {
	sum := strings.Repeat("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, map[string]any{
			"tag_name": "v2.0.0",
			"assets": []map[string]any{
				{
					"name":                 "app-linux-amd64.AppImage",
					"browser_download_url": "http://" + request.Host + "/app.AppImage",
					"size":                 42,
					"digest":               "sha256:" + sum,
				},
			},
		})
	}))
	defer server.Close()
	source := mustGitHubSource(t, server, nil)
	result, err := source.Check(context.Background(), "1.0.0", update.DefaultPlatform("linux", "amd64"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result.Available || result.Asset.SHA256 != sum {
		t.Fatalf("result = %+v, want digest fallback %s", result, sum)
	}
}

func TestGitHubCheckChecksumsFailureFallsBackToDigest(t *testing.T) {
	sum := strings.Repeat("c", 64)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/app/releases/latest":
			writeJSON(t, writer, map[string]any{
				"tag_name": "v2.0.0",
				"assets": []map[string]any{
					{
						"name":                 "app-linux-amd64.AppImage",
						"browser_download_url": server.URL + "/app.AppImage",
						"digest":               "sha256:" + sum,
					},
					{
						"name":                 "SHA256SUMS.txt",
						"browser_download_url": server.URL + "/checksums",
					},
				},
			})
		case "/checksums":
			http.Error(writer, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	source := mustGitHubSource(t, server, nil)
	result, err := source.Check(context.Background(), "1.0.0", update.DefaultPlatform("linux", "amd64"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Asset.SHA256 != sum {
		t.Fatalf("SHA256 = %q, want digest fallback %q", result.Asset.SHA256, sum)
	}
}

func TestGitHubCheckNoChecksum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, map[string]any{
			"tag_name": "v2.0.0",
			"assets": []map[string]any{
				{"name": "app-linux-amd64.AppImage", "browser_download_url": "http://" + request.Host + "/app.AppImage"},
			},
		})
	}))
	defer server.Close()
	source := mustGitHubSource(t, server, nil)
	_, err := source.Check(context.Background(), "1.0.0", update.DefaultPlatform("linux", "amd64"))
	if !errors.Is(err, update.ErrNoChecksum) {
		t.Fatalf("Check error = %v, want ErrNoChecksum", err)
	}
}

func TestGitHubCheckNoUpdateNeedsNoAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, map[string]any{"tag_name": "v1.0.0", "body": "current"})
	}))
	defer server.Close()
	source := mustGitHubSource(t, server, nil)
	result, err := source.Check(context.Background(), "1.0.0", update.DefaultPlatform("linux", "amd64"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Available || result.Asset.URL != "" || result.LatestVersion != "1.0.0" {
		t.Fatalf("result = %+v, want up to date", result)
	}
}

func TestGitHubCheckNoAssetForPlatform(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, map[string]any{
			"tag_name": "v2.0.0",
			"assets": []map[string]any{
				{"name": "README.txt", "browser_download_url": "http://" + request.Host + "/README.txt"},
			},
		})
	}))
	defer server.Close()
	source := mustGitHubSource(t, server, nil)
	_, err := source.Check(context.Background(), "1.0.0", update.DefaultPlatform("linux", "amd64"))
	if !errors.Is(err, update.ErrNoAsset) {
		t.Fatalf("Check error = %v, want ErrNoAsset", err)
	}
}

func TestGitHubLatestRejectsDraftAndPrerelease(t *testing.T) {
	for _, payload := range []map[string]any{
		{"tag_name": "v2.0.0", "draft": true},
		{"tag_name": "v2.0.0", "prerelease": true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(t, writer, payload)
		}))
		source := mustGitHubSource(t, server, nil)
		_, err := source.Latest(context.Background())
		server.Close()
		if !errors.Is(err, update.ErrNoRelease) {
			t.Fatalf("Latest(%+v) error = %v, want ErrNoRelease", payload, err)
		}
	}
}

func TestGitHubLatestHTTPAndDocumentErrors(t *testing.T) {
	t.Run("http status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "nope", http.StatusInternalServerError)
		}))
		defer server.Close()
		source := mustGitHubSource(t, server, nil)
		_, err := source.Latest(context.Background())
		if !errors.Is(err, update.ErrHTTPStatus) || !errors.Is(err, update.ErrManifest) {
			t.Fatalf("Latest error = %v, want ErrHTTPStatus + ErrManifest", err)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"tag_name":`))
		}))
		defer server.Close()
		source := mustGitHubSource(t, server, nil)
		if _, err := source.Latest(context.Background()); !errors.Is(err, update.ErrManifest) {
			t.Fatalf("Latest error = %v, want ErrManifest", err)
		}
	})
	t.Run("bad tag", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(t, writer, map[string]any{"tag_name": "latest"})
		}))
		defer server.Close()
		source := mustGitHubSource(t, server, nil)
		if _, err := source.Latest(context.Background()); !errors.Is(err, update.ErrManifest) {
			t.Fatalf("Latest error = %v, want ErrManifest", err)
		}
	})
}

func TestGitHubRejectsInsecureBaseURL(t *testing.T) {
	source, err := update.NewGitHubSource(update.GitHubConfig{
		Repo:    "acme/app",
		BaseURL: "http://127.0.0.1:1",
		Client:  &http.Client{},
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	if _, err := source.Latest(context.Background()); !errors.Is(err, update.ErrInsecureURL) {
		t.Fatalf("Latest error = %v, want ErrInsecureURL", err)
	}
}

func TestNewGitHubSourceValidatesRepo(t *testing.T) {
	for _, repo := range []string{"", "acme", "acme/", "/app", "acme/app/extra"} {
		if _, err := update.NewGitHubSource(update.GitHubConfig{Repo: repo}); err == nil {
			t.Fatalf("NewGitHubSource(%q) accepted an invalid repo", repo)
		}
	}
}

func TestParseChecksumsFormats(t *testing.T) {
	sum := strings.Repeat("d", 64)
	data := []byte(
		"# comment\n" +
			"not-a-checksum  file.bin\n" +
			sum + " *./tool-linux-amd64.tar.gz\n",
	)
	got, err := update.ParseChecksums(data, "tool-linux-amd64.tar.gz")
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	if got != sum {
		t.Fatalf("ParseChecksums = %q, want %q", got, sum)
	}
	if _, err := update.ParseChecksums(data, "other.bin"); !errors.Is(err, update.ErrNoChecksum) {
		t.Fatalf("missing entry error = %v, want ErrNoChecksum", err)
	}
}

func mustGitHubSource(t *testing.T, server *httptest.Server, now func() time.Time) *update.GitHubSource {
	t.Helper()
	source, err := update.NewGitHubSource(update.GitHubConfig{
		Repo:              "acme/app",
		BaseURL:           server.URL,
		Client:            server.Client(),
		AllowInsecureHTTP: true,
		Now:               now,
	})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	return source
}

func TestGitHubValidateURLPolicy(t *testing.T) {
	source, err := update.NewGitHubSource(update.GitHubConfig{Repo: "acme/app"})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	if err := source.ValidateURL("https://example.test/file"); err != nil {
		t.Fatalf("ValidateURL(https) = %v", err)
	}
	for _, raw := range []string{"http://example.test/file", "ftp://example.test/file", "", "not a url"} {
		if err := source.ValidateURL(raw); !errors.Is(err, update.ErrInsecureURL) {
			t.Fatalf("ValidateURL(%q) = %v, want ErrInsecureURL", raw, err)
		}
	}
}

func TestGitHubSHA256ForPrefersAssetChecksumAndAcceptsNilContext(t *testing.T) {
	source, err := update.NewGitHubSource(update.GitHubConfig{Repo: "acme/app"})
	if err != nil {
		t.Fatalf("NewGitHubSource: %v", err)
	}
	sum := strings.Repeat("e", 64)
	got, err := source.SHA256For(context.TODO(), update.Release{}, update.Asset{Name: "app.bin", SHA256: strings.ToUpper(sum)})
	if err != nil {
		t.Fatalf("SHA256For: %v", err)
	}
	if got != sum {
		t.Fatalf("SHA256For = %q, want %q", got, sum)
	}
}
