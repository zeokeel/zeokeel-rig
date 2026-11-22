package update_test

// Download 的端到端测试：真实 httptest 流式响应 + 临时目录，覆盖成功、大小
// 不符、摘要不符、HTTP/传输错误、取消、文件名安全与部分文件清理。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/zeokeel/zeokeel-rig/update"
)

func downloadServer(t *testing.T, data []byte, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if request.URL.Path == "/missing" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write(data)
	}))
	t.Cleanup(server.Close)
	return server
}

func readDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestDownloadVerifiesAndStages(t *testing.T) {
	data := []byte("installer-bytes")
	sum := sha256Hex(data)
	server := downloadServer(t, data, nil)
	dest := t.TempDir()

	result, err := update.Download(context.Background(), server.Client(), update.Asset{
		Name:   "app-1.2.3.bin",
		URL:    server.URL + "/app.bin",
		Size:   int64(len(data)),
		SHA256: sum,
	}, dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	wantPath := filepath.Join(dest, "app-1.2.3.bin")
	if result.Path != wantPath {
		t.Fatalf("Path = %q, want %q", result.Path, wantPath)
	}
	if result.Size != int64(len(data)) || result.SHA256 != sum {
		t.Fatalf("result = %+v", result)
	}
	got, err := os.ReadFile(wantPath)
	if err != nil || string(got) != string(data) {
		t.Fatalf("staged content = %q/%v, want %q", got, err, data)
	}
	if names := readDirNames(t, dest); len(names) != 1 || names[0] != "app-1.2.3.bin" {
		t.Fatalf("staging dir = %v, want only the final file", names)
	}
}

func TestDownloadWithoutDeclaredSize(t *testing.T) {
	data := []byte("unknown-size")
	sum := sha256Hex(data)
	server := downloadServer(t, data, nil)
	dest := t.TempDir()
	result, err := update.Download(context.Background(), server.Client(), update.Asset{
		URL:    server.URL + "/downloads/tool.bin",
		SHA256: sum,
	}, dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if result.Size != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", result.Size, len(data))
	}
	if result.Path != filepath.Join(dest, "tool.bin") {
		t.Fatalf("Path = %q, want URL basename tool.bin", result.Path)
	}
}

func TestDownloadSizeMismatch(t *testing.T) {
	data := []byte("short")
	sum := sha256Hex(data)
	server := downloadServer(t, data, nil)
	for _, declared := range []int64{int64(len(data)) + 1, int64(len(data)) - 1} {
		dest := t.TempDir()
		_, err := update.Download(context.Background(), server.Client(), update.Asset{
			Name:   "app.bin",
			URL:    server.URL + "/app.bin",
			Size:   declared,
			SHA256: sum,
		}, dest)
		if !errors.Is(err, update.ErrVerify) || !errors.Is(err, update.ErrDownload) {
			t.Fatalf("declared size %d: error = %v, want ErrVerify + ErrDownload", declared, err)
		}
		if names := readDirNames(t, dest); len(names) != 0 {
			t.Fatalf("declared size %d left partial files: %v", declared, names)
		}
	}
}

func TestDownloadHashMismatch(t *testing.T) {
	data := []byte("tampered")
	server := downloadServer(t, data, nil)
	dest := t.TempDir()
	// 预置同名文件：校验失败时旧文件必须原样保留。
	existing := filepath.Join(dest, "app.bin")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := update.Download(context.Background(), server.Client(), update.Asset{
		Name:   "app.bin",
		URL:    server.URL + "/app.bin",
		Size:   int64(len(data)),
		SHA256: sha256Hex([]byte("different")),
	}, dest)
	if !errors.Is(err, update.ErrVerify) {
		t.Fatalf("error = %v, want ErrVerify", err)
	}
	got, readErr := os.ReadFile(existing)
	if readErr != nil || string(got) != "old" {
		t.Fatalf("existing file = %q/%v, want untouched old", got, readErr)
	}
}

func TestDownloadHTTPError(t *testing.T) {
	server := downloadServer(t, []byte("x"), nil)
	dest := t.TempDir()
	_, err := update.Download(context.Background(), server.Client(), update.Asset{
		Name:   "app.bin",
		URL:    server.URL + "/missing",
		SHA256: sha256Hex([]byte("x")),
	}, dest)
	if !errors.Is(err, update.ErrHTTPStatus) || !errors.Is(err, update.ErrDownload) {
		t.Fatalf("error = %v, want ErrHTTPStatus + ErrDownload", err)
	}
	if names := readDirNames(t, dest); len(names) != 0 {
		t.Fatalf("HTTP failure left files: %v", names)
	}
}

func TestDownloadRequiresChecksum(t *testing.T) {
	var hits atomic.Int32
	server := downloadServer(t, []byte("x"), &hits)
	dest := t.TempDir()
	_, err := update.Download(context.Background(), server.Client(), update.Asset{
		Name: "app.bin",
		URL:  server.URL + "/app.bin",
	}, dest)
	if !errors.Is(err, update.ErrNoChecksum) {
		t.Fatalf("error = %v, want ErrNoChecksum", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("download without checksum hit the server %d times", hits.Load())
	}
}

func TestDownloadSanitizesAssetName(t *testing.T) {
	data := []byte("payload")
	sum := sha256Hex(data)
	server := downloadServer(t, data, nil)
	dest := t.TempDir()
	result, err := update.Download(context.Background(), server.Client(), update.Asset{
		Name:   "../../evil.sh",
		URL:    server.URL + "/app.bin",
		SHA256: sum,
	}, dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if filepath.Dir(result.Path) != filepath.Clean(dest) {
		t.Fatalf("staged path escaped dest: %q", result.Path)
	}
	if filepath.Base(result.Path) != "evil.sh" {
		t.Fatalf("sanitized base = %q, want evil.sh", filepath.Base(result.Path))
	}
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatalf("staged file missing: %v", err)
	}
}

func TestDownloadRejectsEmptyDestDir(t *testing.T) {
	server := downloadServer(t, []byte("x"), nil)
	_, err := update.Download(context.Background(), server.Client(), update.Asset{
		Name:   "app.bin",
		URL:    server.URL + "/app.bin",
		SHA256: sha256Hex([]byte("x")),
	}, "  ")
	if !errors.Is(err, update.ErrDownload) {
		t.Fatalf("error = %v, want ErrDownload", err)
	}
}

func TestDownloadHonorsCanceledContext(t *testing.T) {
	server := downloadServer(t, []byte("x"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := update.Download(ctx, server.Client(), update.Asset{
		Name:   "app.bin",
		URL:    server.URL + "/app.bin",
		SHA256: sha256Hex([]byte("x")),
	}, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestDownloadTransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.URL
	server.Close()
	_, err := update.Download(context.Background(), server.Client(), update.Asset{
		Name:   "app.bin",
		URL:    address + "/app.bin",
		SHA256: sha256Hex([]byte("x")),
	}, t.TempDir())
	if !errors.Is(err, update.ErrDownload) {
		t.Fatalf("error = %v, want ErrDownload", err)
	}
}
