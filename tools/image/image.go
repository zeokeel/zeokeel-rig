// Package image provides the read_image tool: it reads a sandboxed image file
// and returns it as a multimodal tool result.
//
// The tool does not decode pixels. It recognises the common image formats by
// their magic bytes (PNG, JPEG, GIF, WebP, BMP), reads the file up to a
// configurable size limit, and returns the bytes as a base64 data: URL in
// rig.ToolResult.Parts. The agent appends those parts to the model conversation
// as a follow-up user message, because providers cannot attach images to the
// tool message itself.
//
// Every path goes through a sandbox.Policy; read-only policies still allow
// reading. Unsupported formats, oversized files, missing files and paths
// outside the sandbox are reported as model-facing errors (IsError=true).
package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

const (
	// Name is the model-facing tool name.
	Name = "read_image"
	// DefaultMaxBytes is the default file-size limit (10 MiB).
	DefaultMaxBytes = 10 << 20
	// DefaultDetail is the image detail hint sent to the model when the call
	// does not specify one.
	DefaultDetail = "auto"
)

// image magic prefixes.
var (
	pngMagic  = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	jpegMagic = []byte{0xff, 0xd8, 0xff}
	gif87a    = []byte("GIF87a")
	gif89a    = []byte("GIF89a")
	riffMagic = []byte("RIFF")
	webpMagic = []byte("WEBP")
	bmpMagic  = []byte("BM")
)

// MIME returns the image MIME type recognised from data's magic bytes and
// whether the format is supported. It inspects only the header; callers still
// own size and sandbox checks.
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

// Option customizes the read_image tool.
type Option func(*config)

// config is the resolved tool configuration.
type config struct {
	maxBytes int
}

// WithMaxBytes caps how many bytes read_image will read and return. Values
// <= 0 keep DefaultMaxBytes.
func WithMaxBytes(maxBytes int) Option {
	return func(cfg *config) {
		if maxBytes > 0 {
			cfg.maxBytes = maxBytes
		}
	}
}

// New returns the read_image tool governed by policy. A nil policy means
// sandbox.New(), which denies every path (the root is required for the tool to
// read anything).
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
	return &tool{policy: policy, maxBytes: cfg.maxBytes}
}

// tool is the read_image implementation; it holds no per-call state.
type tool struct {
	policy   *sandbox.Policy
	maxBytes int
}

var _ rig.Tool = (*tool)(nil)

// Name implements rig.Tool.
func (t *tool) Name() string { return Name }

// Description implements rig.Tool.
func (t *tool) Description() string {
	return `Read an image file from the sandbox and attach it to the conversation as an image.

Supported formats (detected from magic bytes): PNG, JPEG, GIF, WebP, BMP. The default size limit is 10 MiB. The result text reports the path, MIME type and byte size; the image itself is returned as an image content part so the model can see it. Use detail to control image resolution: "auto" (default), "low" or "high".

The path is resolved through the sandbox policy (allowed roots, no "..", symlink containment, denied globs). Reading is allowed on a read-only policy. Unsupported formats, oversized files, missing files and paths outside the sandbox are reported as tool errors. PDFs, audio and video are not supported.`
}

// Schema implements rig.Tool.
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

// args is the decoded read_image argument object.
type args struct {
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// Invoke implements rig.Tool.
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

	root, relative, _, _, err := t.policy.OpenPath(path)
	if err != nil {
		return toolutil.ErrorResultf("%s: %s: %v", Name, path, err), nil
	}
	defer root.Close()

	info, err := root.Stat(relative)
	switch {
	case err != nil && os.IsNotExist(err):
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
