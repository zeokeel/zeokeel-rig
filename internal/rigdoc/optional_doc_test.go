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

	// 条件**已打开**的 Optional 条目必须都出现在目录里。BuildCatalog 打开了工作区
	// 根（Root + Policy roots），因此 Root 类条件工具（apply_patch/search/undo）
	// 必须齐全。只遍历"目录里已有"的条目做比对会漏掉"整类工具从目录消失"：
	// marked > 0 会被 sandbox_status/run_code 兜住，测试仍然绿。
	for _, entry := range catalog.Optional {
		if entry.Field != "AllOptions.Root" {
			continue
		}
		if _, ok := byName[entry.Name]; !ok {
			t.Errorf("工作区根条件已在 BuildCatalog 中打开，但目录里缺少条件工具 %q", entry.Name)
		}
	}

	// 条件在这个目录里没打开的工具不能冒充"已构造"：spill_read 需要 SpillStore，
	// 目录里必须保留它，但可用性要写成 requires AllOptions.SpillStore。
	spill, ok := byName["spill_read"]
	if !ok {
		t.Fatal("BuildCatalog 没有注入 SpillStore，spill_read 仍必须出现在目录里（requires 形态）")
	}
	if spill.Availability != "requires AllOptions.SpillStore" {
		t.Errorf("spill_read availability = %q；want %q", spill.Availability, "requires AllOptions.SpillStore")
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

// TestEveryOptionalToolAppearsInCatalog 断言 catalog.Optional 的**每一条**都出现
// 在目录里：当前配置没构造的条目写成 `requires <字段>`。
//
// 否则条件工具会整条从 docs/ai 消失（spill_read 就因为没有 SpillStore 长期缺席），
// optional 标记对它天然失效——模型读完目录根本不知道这个工具存在。
func TestEveryOptionalToolAppearsInCatalog(t *testing.T) {
	docs := BuildCatalog()
	byName := make(map[string]ToolDoc, len(docs))
	for _, doc := range docs {
		byName[doc.Name] = doc
	}
	for _, entry := range catalog.Optional {
		doc, ok := byName[entry.Name]
		if !ok {
			t.Errorf("catalog.Optional[%q] 没有出现在目录里：未构造的条目也要以 requires <字段> 的形式列出", entry.Name)
			continue
		}
		if !doc.Optional {
			t.Errorf("catalog.Optional[%q] 的目录条目没有 optional=true", entry.Name)
		}
		if doc.Availability == "" || doc.Description == "" {
			t.Errorf("catalog.Optional[%q] 的目录条目缺少可用性/说明: %+v", entry.Name, doc)
		}
	}

	// BuildCatalog 没注入 SpillStore：spill_read 必须以 requires 形态出现，
	// 并且人读的 Markdown 也要能看到它。
	spill, ok := byName["spill_read"]
	if !ok {
		t.Fatal("spill_read 完全不在目录里")
	}
	if !strings.HasPrefix(spill.Availability, "requires ") || !strings.Contains(spill.Availability, "SpillStore") {
		t.Errorf("spill_read availability = %q；want 形如 %q", spill.Availability, "requires AllOptions.SpillStore")
	}
	if markdown := Markdown(docs); !strings.Contains(markdown, "`spill_read`") {
		t.Error("docs/ai/tools.md 的条件工具小节没有提到 spill_read")
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
