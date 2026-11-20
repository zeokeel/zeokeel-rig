package rigdoc

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/catalog"
)

// TestOptionalToolsAreMarkedInCatalog 守 docs/ai/tools.json 的可选标记：
// BuildCatalog 会打开"工作区根 + SandboxGrants + PTC"，因此它产出的每个
// catalog.Optional 工具都必须在目录里带 optional=true。漏标记的话，模型读完
// 目录会以为 sandbox_status、run_code 这类条件工具随时可用。
func TestOptionalToolsAreMarkedInCatalog(t *testing.T) {
	docs := BuildCatalog()
	byName := make(map[string]ToolDoc, len(docs))
	for _, doc := range docs {
		byName[doc.Name] = doc
	}

	marked := 0
	for _, doc := range docs {
		entry, isOptional := catalog.OptionalByName(doc.Name)
		if doc.Optional != isOptional {
			t.Errorf("%q 的 optional=%v，但 catalog.Optional 登记=%v", doc.Name, doc.Optional, isOptional)
		}
		if doc.Optional {
			marked++
			if entry.Field == "" || entry.Gate == "" {
				t.Errorf("%q 标了可选，但 catalog.Optional 没说明控制字段/条件", doc.Name)
			}
		}
	}
	if marked == 0 {
		t.Fatal("目录里没有任何 optional 工具：条件构造的工具会被当成常驻工具")
	}

	// 条件在这个目录里没打开的工具不应出现（spill_read 需要 SpillStore）。
	if _, ok := byName["spill_read"]; ok {
		t.Error("BuildCatalog 没有注入 SpillStore，却出现了 spill_read")
	}

	// run_code 是 kit.Config.PTC 的条件工具，目录应保留它的条件说明。
	runCode, ok := byName["run_code"]
	if !ok {
		t.Fatal("run_code 不在目录里")
	}
	if !runCode.Optional || runCode.Availability != "kit.Config.PTC" {
		t.Errorf("run_code = optional=%v availability=%q；want optional=true availability=%q",
			runCode.Optional, runCode.Availability, "kit.Config.PTC")
	}
}

// TestMarkdownShowsOptionalColumn 守人读目录也有可选标记（JSON 与 Markdown
// 是同一份生成结果的两个视图，不能只在一个视图里说清楚）。
func TestMarkdownShowsOptionalColumn(t *testing.T) {
	markdown := Markdown(BuildCatalog())
	for _, want := range []string{"| 可选 |", "| 是 |", "tools/catalog.Optional"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("docs/ai/tools.md 缺少 %q", want)
		}
	}
}
