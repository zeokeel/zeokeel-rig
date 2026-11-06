package kit_test

// Cover the small kit alias constructors in one place so the kit facade keeps a
// high per-package coverage floor (the specialized packages test the underlying
// rig constructors).

import (
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
)

func TestAliasConstructors(t *testing.T) {
	text := kit.TextPart("hello")
	if text.Type != kit.PartText || text.Text != "hello" {
		t.Fatalf("TextPart = %+v", text)
	}
	image := kit.ImagePart("https://example.com/a.png", "high")
	if image.Type != kit.PartImageURL || image.ImageURL != "https://example.com/a.png" || image.Detail != "high" {
		t.Fatalf("ImagePart = %+v", image)
	}
	message := kit.NewMessage(kit.RoleUser, "hi")
	if message.Role != kit.RoleUser || message.Text() != "hi" {
		t.Fatalf("NewMessage = %+v", message)
	}
}
