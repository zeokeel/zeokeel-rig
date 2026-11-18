package skill_test

// 逐字节兼容性守门：把 v0.20 的**旧实现算法**在测试里重算一遍，与现在（走
// `tools/shelf` 原语）的输出逐字节比较。
//
// 为什么需要这份测试：`skill` 是渐进式原语的第一位消费者，改写它的目的正是"证明
// 原语够用"。只靠既有的 fragment 断言（Contains）还不够——格式、空白、截断后缀
// 变一个字节，模型看到的就是另一个工具。这里对 list/search/load/空库/错误五条
// 路径都做全字符串比对。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/skill"
)

// legacyList 是 v0.20 的目录渲染（`- name: description`，每条以换行结尾）。
func legacyList(skills []rig.Skill) string {
	var builder strings.Builder
	for _, entry := range skills {
		builder.WriteString("- ")
		builder.WriteString(entry.Name)
		if description := strings.TrimSpace(entry.Description); description != "" {
			builder.WriteString(": ")
			builder.WriteString(description)
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

// legacySearch 是 v0.20 的匹配规则。
func legacySearch(skills []rig.Skill, query string) []rig.Skill {
	terms := strings.Fields(strings.ToLower(query))
	var matches []rig.Skill
	for _, entry := range skills {
		haystack := strings.ToLower(entry.Name + "\n" + entry.Description + "\n" + entry.Body)
		matchedAll := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matchedAll = false
				break
			}
		}
		if matchedAll {
			matches = append(matches, entry)
		}
	}
	return matches
}

// legacyLoad 是 v0.20 的正文渲染。
func legacyLoad(entry rig.Skill) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "name: %s\n", entry.Name)
	if description := strings.TrimSpace(entry.Description); description != "" {
		fmt.Fprintf(&builder, "description: %s\n", description)
	}
	fmt.Fprintf(&builder, "path: %s\n", entry.Path)
	builder.WriteString("---\n")
	builder.WriteString(entry.Body)
	if !strings.HasSuffix(entry.Body, "\n") {
		builder.WriteString("\n")
	}
	return builder.String()
}

// legacyTruncate 是 v0.20 的截断（UTF-8 安全的字节切 + 固定后缀）。
func legacyTruncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", limit)
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// paritySkills 覆盖有描述、无描述、多行正文、超长正文四种形态。
func paritySkills() []rig.Skill {
	return []rig.Skill{
		{Name: "release-checklist", Description: "How to cut a release", Path: "/skills/release/SKILL.md", Body: "# Release\nRun the tests, tag, push."},
		{Name: "undocumented", Description: "", Path: "/skills/undocumented/SKILL.md", Body: "# Undocumented\nNo description here."},
		{Name: "debugging", Description: "Systematic   debugging", Path: "/skills/debug/SKILL.md", Body: "# Debug\nreproduce, bisect, fix.\n"},
	}
}

// TestParityList 逐步比对 list 输出。
func TestParityList(t *testing.T) {
	store := &fakeStore{skills: paritySkills()}
	tool := skill.Tools(store)[0]
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"action":"list"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	want := legacyTruncate(legacyList(paritySkills()), skill.MaxOutputBytes)
	if result.Content != want {
		t.Fatalf("list 输出与 v0.20 不一致:\n got %q\nwant %q", result.Content, want)
	}
	// 无描述的那条必须渲染成 `- undocumented`（不带冒号）。
	if !strings.Contains(result.Content, "- undocumented\n") {
		t.Fatalf("无描述技能的目录行不对: %q", result.Content)
	}
}

// TestParitySearch 逐步比对 search 输出。
func TestParitySearch(t *testing.T) {
	store := &fakeStore{skills: paritySkills()}
	tool := skill.Tools(store)[0]
	for _, query := range []string{"release", "BISECT", "systematic fix", "undocumented", "no-such-term"} {
		result, err := tool.Invoke(context.Background(), rig.ToolCall{
			Arguments: fmt.Sprintf(`{"action":"search","query":%q}`, query),
		})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		matches := legacySearch(paritySkills(), query)
		want := fmt.Sprintf("no skills found matching %q", query)
		if len(matches) > 0 {
			want = legacyTruncate(legacyList(matches), skill.MaxOutputBytes)
		}
		if result.Content != want {
			t.Fatalf("search %q 输出与 v0.20 不一致:\n got %q\nwant %q", query, result.Content, want)
		}
	}
}

// TestParityLoad 逐步比对 load 输出。
func TestParityLoad(t *testing.T) {
	store := &fakeStore{skills: paritySkills()}
	tool := skill.Tools(store)[0]
	for _, entry := range paritySkills() {
		result, err := tool.Invoke(context.Background(), rig.ToolCall{
			Arguments: fmt.Sprintf(`{"action":"load","name":%q}`, entry.Name),
		})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		want := legacyTruncate(legacyLoad(entry), skill.MaxOutputBytes)
		if result.Content != want {
			t.Fatalf("load %q 输出与 v0.20 不一致:\n got %q\nwant %q", entry.Name, result.Content, want)
		}
	}
	// name-only 调用（DSH 兼容）同样逐字节一致。
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"name":"debugging"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if want := legacyTruncate(legacyLoad(paritySkills()[2]), skill.MaxOutputBytes); result.Content != want {
		t.Fatalf("name-only load 输出与 v0.20 不一致:\n got %q\nwant %q", result.Content, want)
	}
}

// TestParityEmptyAndErrors 逐步比对空库与错误文案。
func TestParityEmptyAndErrors(t *testing.T) {
	empty := skill.Tools(&fakeStore{})[0]
	result, err := empty.Invoke(context.Background(), rig.ToolCall{Arguments: `{"action":"list"}`})
	if err != nil || result.Content != "no skills found" {
		t.Fatalf("空库文案 = %q (err=%v), want %q", result.Content, err, "no skills found")
	}
	result, err = empty.Invoke(context.Background(), rig.ToolCall{Arguments: `{"action":"search","query":"x"}`})
	if err != nil || result.Content != `no skills found matching "x"` {
		t.Fatalf("空搜索文案 = %q (err=%v)", result.Content, err)
	}

	store := &fakeStore{skills: paritySkills()}
	tool := skill.Tools(store)[0]
	// 未知技能：错误来自 store，前缀由工具补成 `skill: `（与 v0.20 一致）。
	result, err = tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"action":"load","name":"nope"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError || !strings.HasPrefix(result.Content, "skill: ") || !strings.Contains(result.Content, "unknown skill") {
		t.Fatalf("未知技能错误文案 = %q", result.Content)
	}
	// 参数错误文案。
	for _, testCase := range []struct{ args, want string }{
		{`{"action":"explode"}`, `skill: unknown action "explode" (want list, search or load)`},
		{`{"action":"search"}`, "skill: query is required for action=search"},
		{`{"action":"load"}`, "skill: name is required for action=load"},
	} {
		result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: testCase.args})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if result.Content != testCase.want {
			t.Fatalf("%s 文案 = %q, want %q", testCase.args, result.Content, testCase.want)
		}
	}
}

// TestParityTruncation 逐步比对 32 KiB 截断（真的发生截断时）。
func TestParityTruncation(t *testing.T) {
	big := rig.Skill{
		Name:        "big-skill",
		Description: "a very large skill",
		Path:        "/skills/big/SKILL.md",
		Body:        strings.Repeat("x", skill.MaxOutputBytes+500),
	}
	tool := skill.Tools(&fakeStore{skills: []rig.Skill{big}})[0]
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{"action":"load","name":"big-skill"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	want := legacyTruncate(legacyLoad(big), skill.MaxOutputBytes)
	if result.Content != want {
		t.Fatalf("截断输出与 v0.20 不一致（长度 got=%d want=%d）", len(result.Content), len(want))
	}
	if !strings.Contains(result.Content, fmt.Sprintf("[truncated: output exceeded %d bytes]", skill.MaxOutputBytes)) {
		t.Fatalf("缺少 v0.20 的截断后缀: %q", result.Content[len(result.Content)-80:])
	}
}
