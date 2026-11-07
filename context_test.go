package rig_test

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

func TestEstimateTextTokens(t *testing.T) {
	if got := rig.EstimateTextTokens(""); got != 0 {
		t.Fatalf("empty = %d", got)
	}
	ascii := rig.EstimateTextTokens(strings.Repeat("a", 400))
	if ascii < 80 || ascii > 120 {
		t.Fatalf("ascii tokens = %d, want ~100", ascii)
	}
	cjk := rig.EstimateTextTokens(strings.Repeat("中", 100))
	if cjk < 80 || cjk > 140 {
		t.Fatalf("cjk tokens = %d, want ~100", cjk)
	}
}

func TestEstimateTokensToolCallsAndImages(t *testing.T) {
	messages := []rig.Message{{
		Role: rig.RoleAssistant,
		Content: []rig.ContentPart{
			rig.ImagePart("data:image/png;base64,AAAA", ""),
			rig.TextPart("hello"),
		},
		ToolCalls: []rig.ToolCall{{ID: "call_1", Name: "bash", Arguments: "{}"}},
	}}
	got := rig.EstimateTokens(messages)
	if got < 1024 {
		t.Fatalf("tokens = %d, want image cost included", got)
	}
}
