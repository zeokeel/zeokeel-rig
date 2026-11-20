package kit

// 系统提示词的区段顺序：宿主自定义区段插在 persona 之后、environment 与仓库指令
// 之前。顺序是契约：模型对更靠后的具体约束权重更高，而 AGENTS.md 是不可信输入，
// 必须排在宿主自己的规则之后。

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/instructions"
)

func TestAssembleSystemPromptOrdersSections(t *testing.T) {
	environment := instructions.Environment{WorkspaceRoot: "/w", OS: "linux"}
	set := instructions.Set{Documents: []instructions.Document{{Path: "/w/AGENTS.md", Content: "repo rules", Source: instructions.SourceRoot}}}

	got := assembleSystemPrompt("persona", &environment, &set, "/w", PromptSection{Name: "product", Body: "product rule"})

	personaAt := strings.Index(got, "persona")
	sectionAt := strings.Index(got, "product rule")
	ruleAt := strings.Index(got, "repo rules")
	if personaAt < 0 || sectionAt < 0 || ruleAt < 0 {
		t.Fatalf("缺少区段:\n%s", got)
	}
	if !(personaAt < sectionAt && sectionAt < ruleAt) {
		t.Fatalf("顺序应为 persona → 自定义区段 → 仓库指令，实际位置 %d/%d/%d:\n%s",
			personaAt, sectionAt, ruleAt, got)
	}
	if !strings.Contains(got, "persona\n\nproduct rule") {
		t.Fatalf("区段之间应恰好一个空行:\n%s", got)
	}
}

func TestAssembleSystemPromptEmptySectionIgnored(t *testing.T) {
	got := assembleSystemPrompt("persona", nil, nil, "/w", PromptSection{Name: "empty", Body: "   "})
	if got != "persona" {
		t.Fatalf("空区段不该留下多余空行，实际 %q", got)
	}
}
