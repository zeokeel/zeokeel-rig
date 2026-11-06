package image_test

// Black-box tests for the read_image tool: magic-byte detection, sandbox
// wiring, multimodal Parts encoding and the model-facing error cases.

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	rigimage "github.com/zeokeel/zeokeel-rig/tools/image"
)

// onePixelPNGBase64 is a valid 1x1 transparent PNG.
const onePixelPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// onePixelPNG decodes the test PNG.
func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(onePixelPNGBase64)
	if err != nil {
		t.Fatalf("decode test PNG: %v", err)
	}
	return data
}

// fixture is one read_image tool bound to a temporary root.
type fixture struct {
	root string
	tool rig.Tool
}

// newFixture builds a policy over a new temporary root. policyOpts are applied
// after WithRoots.
func newFixture(t *testing.T, policyOpts []sandbox.Option, toolOpts ...rigimage.Option) *fixture {
	t.Helper()
	root := t.TempDir()
	options := append([]sandbox.Option{sandbox.WithRoots(root)}, policyOpts...)
	return &fixture{root: root, tool: rigimage.New(sandbox.New(options...), toolOpts...)}
}

// write creates one file under the root.
func (f *fixture) write(t *testing.T, relative string, data []byte) string {
	t.Helper()
	absolute := filepath.Join(f.root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relative, err)
	}
	if err := os.WriteFile(absolute, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
	return absolute
}

// invoke runs one call and fails on an uncontained Go error.
func (f *fixture) invoke(t *testing.T, arguments string) rig.ToolResult {
	t.Helper()
	result, err := f.tool.Invoke(context.Background(), rig.ToolCall{Name: rigimage.Name, Arguments: arguments})
	if err != nil {
		t.Fatalf("Invoke(%s): %v", arguments, err)
	}
	return result
}

// requireError asserts IsError and that the content mentions fragments.
func requireError(t *testing.T, result rig.ToolResult, want ...string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("expected a tool error, got %q", result.Content)
	}
	for _, fragment := range want {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("error %q does not contain %q", result.Content, fragment)
		}
	}
}

// TestMIME covers magic-byte detection for every supported format.
func TestMIME(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
		ok   bool
	}{
		{"png", append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, 0, 0), "image/png", true},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}, "image/jpeg", true},
		{"gif87a", []byte("GIF87a...."), "image/gif", true},
		{"gif89a", []byte("GIF89a...."), "image/gif", true},
		{"webp", []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P', 0, 0, 0, 0}, "image/webp", true},
		{"bmp", []byte{'B', 'M', 0, 0, 0, 0}, "image/bmp", true},
		{"text", []byte("plain text"), "", false},
		{"short png prefix", []byte{0x89, 'P'}, "", false},
		{"empty", nil, "", false},
	}
	for _, testCase := range cases {
		mime, ok := rigimage.MIME(testCase.data)
		if ok != testCase.ok || mime != testCase.mime {
			t.Fatalf("%s: MIME = %q/%v, want %q/%v", testCase.name, mime, ok, testCase.mime, testCase.ok)
		}
	}
}

// TestReadImagePNG verifies the content text, image part and base64 round trip.
func TestReadImagePNG(t *testing.T) {
	fixture := newFixture(t, nil)
	png := onePixelPNG(t)
	fixture.write(t, "pixel.png", png)

	result := fixture.invoke(t, `{"path":"pixel.png"}`)
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content)
	}
	wantContent := fmt.Sprintf("pixel.png (image/png, %d bytes)", len(png))
	if result.Content != wantContent {
		t.Fatalf("Content = %q, want %q", result.Content, wantContent)
	}
	if len(result.Parts) != 1 {
		t.Fatalf("Parts = %v, want one image part", result.Parts)
	}
	part := result.Parts[0]
	if part.Type != rig.PartImageURL {
		t.Fatalf("part type = %q, want %q", part.Type, rig.PartImageURL)
	}
	if part.Detail != "auto" {
		t.Fatalf("detail = %q, want auto", part.Detail)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(part.ImageURL, prefix) {
		t.Fatalf("image URL = %q, want a %q data URL", part.ImageURL, prefix)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(part.ImageURL, prefix))
	if err != nil {
		t.Fatalf("decode data URL: %v", err)
	}
	if !bytes.Equal(decoded, png) {
		t.Fatal("decoded image bytes differ from the file")
	}
	if !bytes.HasPrefix(decoded, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("decoded bytes do not start with the PNG magic: %x", decoded[:4])
	}
}

// TestReadImageDetail verifies the detail hint passthrough and validation.
func TestReadImageDetail(t *testing.T) {
	fixture := newFixture(t, nil)
	fixture.write(t, "pixel.png", onePixelPNG(t))

	for _, detail := range []string{"low", "high"} {
		result := fixture.invoke(t, fmt.Sprintf(`{"path":"pixel.png","detail":%q}`, detail))
		if result.IsError || len(result.Parts) != 1 || result.Parts[0].Detail != detail {
			t.Fatalf("detail %q result = %+v", detail, result)
		}
	}
	requireError(t, fixture.invoke(t, `{"path":"pixel.png","detail":"ultra"}`), "detail must be one of")
}

// TestReadImageFormats verifies every supported magic-byte format end to end.
func TestReadImageFormats(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"photo.jpg", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'}, "image/jpeg"},
		{"anim.gif", []byte("GIF89a\x01\x00\x01\x00"), "image/gif"},
		{"photo.webp", []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P', 0, 0, 0, 0}, "image/webp"},
		{"image.bmp", []byte{'B', 'M', 0, 0, 0, 0}, "image/bmp"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, nil)
			fixture.write(t, testCase.name, testCase.data)
			result := fixture.invoke(t, fmt.Sprintf(`{"path":%q}`, testCase.name))
			if result.IsError {
				t.Fatalf("unexpected tool error: %s", result.Content)
			}
			if !strings.Contains(result.Content, testCase.mime) {
				t.Fatalf("Content = %q, want MIME %q", result.Content, testCase.mime)
			}
			if len(result.Parts) != 1 || !strings.HasPrefix(result.Parts[0].ImageURL, "data:"+testCase.mime+";base64,") {
				t.Fatalf("Parts = %+v, want a %s data URL", result.Parts, testCase.mime)
			}
		})
	}
}

// TestReadImageErrors covers the model-facing failure cases.
func TestReadImageErrors(t *testing.T) {
	png := onePixelPNG(t)
	fixture := newFixture(t, nil)
	fixture.write(t, "note.txt", []byte("plain text"))
	fixture.write(t, "pixel.png", png)
	if err := os.MkdirAll(filepath.Join(fixture.root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	requireError(t, fixture.invoke(t, `{"path":"missing.png"}`), "does not exist")
	requireError(t, fixture.invoke(t, `{"path":"note.txt"}`), "unsupported image format")
	requireError(t, fixture.invoke(t, `{"path":"dir"}`), "directory")
	requireError(t, fixture.invoke(t, `{"path":"../escape.png"}`), "denied")
	requireError(t, fixture.invoke(t, `{"path":""}`), "path is required")
	requireError(t, fixture.invoke(t, `{}`), "path is required")

	if result, err := fixture.tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{invalid`}); err == nil {
		t.Fatalf("invalid JSON returned no Go error: %+v", result)
	}

	// A small configured limit rejects the (valid, larger) PNG.
	small := newFixture(t, nil, rigimage.WithMaxBytes(8))
	small.write(t, "pixel.png", png)
	requireError(t, small.invoke(t, `{"path":"pixel.png"}`), "larger than the 8 byte limit")
}

// TestReadImageReadOnly verifies reading is allowed on a read-only policy.
func TestReadImageReadOnly(t *testing.T) {
	fixture := newFixture(t, []sandbox.Option{sandbox.WithReadOnly(true)})
	fixture.write(t, "pixel.png", onePixelPNG(t))
	result := fixture.invoke(t, `{"path":"pixel.png"}`)
	if result.IsError || len(result.Parts) != 1 {
		t.Fatalf("read-only read_image result = %+v", result)
	}
}

// TestReadImageDeniedGlob verifies denied globs are enforced by the policy.
func TestReadImageDeniedGlob(t *testing.T) {
	fixture := newFixture(t, []sandbox.Option{sandbox.WithDeniedGlobs("**/*.png")})
	fixture.write(t, "pixel.png", onePixelPNG(t))
	requireError(t, fixture.invoke(t, `{"path":"pixel.png"}`), "denied")
}

// TestToolMetadataAndNilPolicy verifies the public identity and fail-closed nil
// policy.
func TestToolMetadataAndNilPolicy(t *testing.T) {
	tool := rigimage.New(sandbox.New(sandbox.WithRoots(t.TempDir())))
	if tool.Name() != "read_image" {
		t.Fatalf("Name() = %q", tool.Name())
	}
	if !strings.Contains(tool.Description(), "PNG") {
		t.Fatalf("Description() looks unrelated: %q", tool.Description())
	}
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema = %v", schema)
	}

	closed := rigimage.New(nil)
	result, err := closed.Invoke(context.Background(), rig.ToolCall{Arguments: `{"path":"pixel.png"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	requireError(t, result, "denied")
}
