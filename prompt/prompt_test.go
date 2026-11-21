package prompt_test

// The section model is the SDK contract for hosts that show prompt provenance:
// order, environment grouping, measurement and stable content hashes. These
// tests pin the rendering byte-for-byte because every host that displays a
// prompt breakdown compares those bytes with what the model received.

import (
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/prompt"
)

func TestAssembleGroupsTheEnvironmentFacts(t *testing.T) {
	rendered, sections := prompt.Assemble([]prompt.Section{
		{ID: prompt.IDIdentity, Source: "builtin", Text: "You are a test agent."},
		{ID: prompt.IDEnvironment, Source: "builtin", Text: "Environment: linux/amd64."},
		{ID: prompt.IDWorkingDirectory, Source: "builtin", Text: "Working directory: /work."},
		{ID: prompt.IDDate, Source: "builtin", Text: "Today's date is 2026-11-21 (Saturday)."},
		{ID: "approval", Source: "policy", Text: "Approval policy: ask."},
		{ID: "config", Source: "config", Text: "Configured prompt."},
	})
	want := "You are a test agent.\nEnvironment: linux/amd64.\nWorking directory: /work.\nToday's date is 2026-11-21 (Saturday).\n\nApproval policy: ask.\n\nConfigured prompt."
	if rendered != want {
		t.Fatalf("rendered = %q, want %q", rendered, want)
	}
	if len(sections) != 6 {
		t.Fatalf("sections = %d, want 6", len(sections))
	}
	for _, section := range sections {
		if section.Bytes != len(section.Text) || section.Tokens != rig.EstimateTextTokens(section.Text) || section.Hash == "" {
			t.Fatalf("section %q is not measured: %+v", section.ID, section)
		}
	}
}

func TestAssembleDropsEmptySectionsAndTrims(t *testing.T) {
	rendered, sections := prompt.Assemble([]prompt.Section{
		{ID: prompt.IDIdentity, Text: "  Identity.  "},
		{ID: "memory", Text: "   "},
		{ID: "config", Text: "\n\nbody\n"},
	})
	if rendered != "Identity.\n\nbody" {
		t.Fatalf("rendered = %q", rendered)
	}
	if len(sections) != 2 {
		t.Fatalf("sections = %+v, want the empty one dropped", sections)
	}
}

func TestRenderHonorsCustomGroups(t *testing.T) {
	sections := []prompt.Section{
		{ID: "a", Text: "A"},
		{ID: "b", Text: "B"},
		{ID: "c", Text: "C"},
	}
	if got := prompt.Render(sections, "a", "b"); got != "A\nB\n\nC" {
		t.Fatalf("grouped render = %q", got)
	}
	// A blank group list means the default group IDs, so private IDs are not
	// grouped accidentally.
	if got := prompt.Render(sections); got != "A\n\nB\n\nC" {
		t.Fatalf("default render = %q", got)
	}
}

func TestHashIsStableAndContentSensitive(t *testing.T) {
	first := prompt.Hash("same text")
	if first == "" || first != prompt.Hash("same text") {
		t.Fatalf("hash not stable: %q", first)
	}
	if prompt.Hash(" other text ") == first {
		t.Fatal("hash ignores meaningfully different text")
	}
	if prompt.Hash("  padded  ") != prompt.Hash("padded") {
		t.Fatal("hash should follow the trimmed section text")
	}
}

func TestDateSection(t *testing.T) {
	if section := prompt.DateSection(time.Time{}); section.Text != "" {
		t.Fatalf("zero clock produced %q", section.Text)
	}
	frozen := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	section := prompt.DateSection(frozen)
	if section.ID != prompt.IDDate || section.Source != "builtin" {
		t.Fatalf("date section = %+v", section)
	}
	if !strings.Contains(section.Text, "2026-11-21") || !strings.Contains(section.Text, "Saturday") {
		t.Fatalf("date text = %q", section.Text)
	}
}

func TestNewSectionDropsEmptyText(t *testing.T) {
	if section := prompt.NewSection("id", "source", "  "); section.Text != "" || section.Bytes != 0 {
		t.Fatalf("empty section = %+v", section)
	}
}
