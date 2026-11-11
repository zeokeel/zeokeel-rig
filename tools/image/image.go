package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

const (
	// Name 是面向模型的工具名。
	Name = "read_image"
	// DefaultMaxBytes 是默认文件大小上限（10 MiB）。
	DefaultMaxBytes = 10 << 20
	// DefaultDetail 是调用未指定时发送给模型的图像细节提示。
	DefaultDetail = "auto"
)

// 图像魔数前缀。
var (
	pngMagic  = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	jpegMagic = []byte{0xff, 0xd8, 0xff}
	gif87a    = []byte("GIF87a")
	gif89a    = []byte("GIF89a")
	riffMagic = []byte("RIFF")
	webpMagic = []byte("WEBP")
	bmpMagic  = []byte("BM")
)

// MIME 根据 data 的魔数返回识别出的图像 MIME 类型以及该格式是否受支持。
// 它只检查文件头；调用方仍负责大小与沙箱校验。
func MIME(data []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(data, pngMagic):
		return "image/png", true
	case bytes.HasPrefix(data, jpegMagic):
		return "image/jpeg", true
	case bytes.HasPrefix(data, gif87a), bytes.HasPrefix(data, gif89a):
		return "image/gif", true
	case len(data) >= 12 && bytes.HasPrefix(data, riffMagic) && bytes.Equal(data[8:12], webpMagic):
		return "image/webp", true
	case bytes.HasPrefix(data, bmpMagic):
		return "image/bmp", true
	default:
		return "", false
	}
}

// Option 定制 read_image 工具。
type Option func(*config)

// config 是解析后的工具配置。
type config struct {
	maxBytes int
	cwd      func() string
}

// WithWorkingDirectory 让 read_image 从会话当前目录解析相对路径（由
// misc.WorkingDirectory.Current 提供）；未挂接时相对沙箱根。
func WithWorkingDirectory(current func() string) Option {
	return func(cfg *config) { cfg.cwd = current }
}

// WithMaxBytes 限制 read_image 读取与返回的字节数。值 <= 0 时保留
// DefaultMaxBytes。
func WithMaxBytes(maxBytes int) Option {
	return func(cfg *config) {
		if maxBytes > 0 {
			cfg.maxBytes = maxBytes
		}
	}
}

// New 返回由 policy 治理的 read_image 工具。nil policy 表示 sandbox.New()，
// 它会拒绝所有路径（没有根，工具无法读取任何内容）。
func New(policy *sandbox.Policy, opts ...Option) rig.Tool {
	if policy == nil {
		policy = sandbox.New()
	}
	cfg := config{maxBytes: DefaultMaxBytes}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &tool{policy: policy, maxBytes: cfg.maxBytes, cwd: cfg.cwd}
}

// tool 是 read_image 的实现；它不保存按调用产生的状态。
type tool struct {
	policy   *sandbox.Policy
	maxBytes int
	cwd      func() string
}

var _ rig.Tool = (*tool)(nil)

// Name 实现 rig.Tool。
func (t *tool) Name() string { return Name }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	return `Read an image file from the sandbox and attach it to the conversation as an image.

Supported formats (detected from magic bytes): PNG, JPEG, GIF, WebP, BMP. The default size limit is 10 MiB. The result text reports the path, MIME type and byte size; the image itself is returned as an image content part so the model can see it. Use detail to control image resolution: "auto" (default), "low" or "high".

The path is resolved through the sandbox policy (allowed roots, no "..", symlink containment, denied globs). Reading is allowed on a read-only policy. Unsupported formats, oversized files, missing files and paths outside the sandbox are reported as tool errors. PDFs, audio and video are not supported.`
}

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Image file path relative to a sandbox root (an absolute path inside a root is accepted).",
			},
			"detail": map[string]any{
				"type":        "string",
				"description": "Image resolution hint for the model (default auto).",
				"enum":        []any{"auto", "low", "high"},
			},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}

// args 是解码后的 read_image 参数对象。
type args struct {
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	var a args
	if err := toolutil.DecodeArgs(call, &a); err != nil {
		return rig.ToolResult{}, err
	}
	path := strings.TrimSpace(a.Path)
	if path == "" {
		return toolutil.ErrorResultf("%s: path is required", Name), nil
	}
	detail := strings.TrimSpace(a.Detail)
	if detail == "" {
		detail = DefaultDetail
	}
	switch detail {
	case "auto", "low", "high":
	default:
		return toolutil.ErrorResultf("%s: detail must be one of auto, low, high; got %q", Name, a.Detail), nil
	}

	display := strings.TrimSpace(path)
	if t.cwd != nil && display != "" && !filepath.IsAbs(display) {
		if cwd := filepath.ToSlash(strings.TrimSpace(t.cwd())); cwd != "" {
			display = pathpkg.Join(cwd, filepath.ToSlash(display))
		}
	}
	root, relative, _, _, err := t.policy.OpenPath(display)
	if err != nil {
		return toolutil.ErrorResultf("%s: %s: %v", Name, path, err), nil
	}
	defer root.Close()

	info, err := root.Stat(relative)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return toolutil.ErrorResultf("%s: %s: the path does not exist", Name, path), nil
	case err != nil:
		return toolutil.ErrorResultf("%s: %s: stat: %v", Name, path, err), nil
	case info.IsDir():
		return toolutil.ErrorResultf("%s: %s: the path is a directory", Name, path), nil
	case !info.Mode().IsRegular():
		return toolutil.ErrorResultf("%s: %s: not a regular file", Name, path), nil
	case info.Size() > int64(t.maxBytes):
		return toolutil.ErrorResultf("%s: %s: file is %d bytes, larger than the %d byte limit", Name, path, info.Size(), t.maxBytes), nil
	}
	data, err := root.ReadFile(relative)
	if err != nil {
		return toolutil.ErrorResultf("%s: %s: read: %v", Name, path, err), nil
	}
	if len(data) > t.maxBytes {
		return toolutil.ErrorResultf("%s: %s: file is %d bytes, larger than the %d byte limit", Name, path, len(data), t.maxBytes), nil
	}
	mime, ok := MIME(data)
	if !ok {
		return toolutil.ErrorResultf("%s: %s: unsupported image format (supported: PNG, JPEG, GIF, WebP, BMP)", Name, path), nil
	}

	content := fmt.Sprintf("%s (%s, %d bytes)", path, mime, len(data))
	imageURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	return rig.ToolResult{
		Content: content,
		Parts:   []rig.ContentPart{rig.ImagePart(imageURL, detail)},
	}, nil
}
