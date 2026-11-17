package modelprice

// 价目表守护测试（审计 2F 的 P1 修复后新增）。
//
// 三件事：① 表不能缩水（≥30 条）；② 每条自洽（provider 与型号族一致、Estimated
// 已标、Updated 已填、cached 不高于 input）；③ 新增子型号必须**显式加条目**——
// 否则会被父型号前缀吃掉（`o3-mini` 曾被当成 `o3`，高估 1.8 倍）。

import (
	"strings"
	"testing"
)

// TestTableSizeAtLeast30 防止有人"清理"价目表（缺条目 = 成本记 0，见审计 2F）。
func TestTableSizeAtLeast30(t *testing.T) {
	if got := len(All()); got < 30 {
		t.Fatalf("价目表只有 %d 条，下限是 30（缺条目会让成本记 0）", got)
	}
}

// TestTableAllIsSortedAndUnique 断言 All() 按 Model 升序且无重复（UI 直接逐行渲染）。
func TestTableAllIsSortedAndUnique(t *testing.T) {
	all := All()
	seen := make(map[string]bool, len(all))
	for index, price := range all {
		if index > 0 && all[index-1].Model >= price.Model {
			t.Fatalf("All() 未按 Model 升序/有重复: %q >= %q", all[index-1].Model, price.Model)
		}
		if seen[price.Model] {
			t.Fatalf("%q 在 All() 里出现两次", price.Model)
		}
		seen[price.Model] = true
		if price.Model != strings.ToLower(price.Model) {
			t.Fatalf("%q: Model 必须小写（Lookup 会小写化查询）", price.Model)
		}
	}
}

// TestTableProviderMatchesModelFamily 断言 provider 与型号族一致（防止复制粘贴后
// 忘了改 provider，导致 `?provider=openai` 之类的查询答错）。
func TestTableProviderMatchesModelFamily(t *testing.T) {
	families := []struct {
		prefixes []string
		provider string
	}{
		{[]string{"gpt-", "o1", "o3", "o4", "chatgpt"}, "openai"},
		{[]string{"claude-"}, "anthropic"},
		{[]string{"deepseek-"}, "deepseek"},
		{[]string{"qwen"}, "alibaba"},
		{[]string{"glm-"}, "zhipu"},
		{[]string{"gemini-", "gemma-"}, "google"},
		{[]string{"grok-"}, "xai"},
		{[]string{"kimi-", "moonshot-"}, "moonshot"},
	}
	for _, price := range All() {
		matched := ""
		for _, family := range families {
			for _, prefix := range family.prefixes {
				if strings.HasPrefix(price.Model, prefix) {
					matched = family.provider
					break
				}
			}
			if matched != "" {
				break
			}
		}
		if matched == "" {
			t.Fatalf("%q 不属于任何已知型号族，请先加到 families 表", price.Model)
		}
		if price.Provider != matched {
			t.Fatalf("%q: provider = %q, want %q", price.Model, price.Provider, matched)
		}
	}
}

// TestTableEntriesAreDatedAndEstimated 断言云端条目都标了估算与采集日期。
func TestTableEntriesAreDatedAndEstimated(t *testing.T) {
	for _, price := range All() {
		if !price.Estimated {
			t.Fatalf("%q: 云端表内条目必须标 Estimated（价格会过时）", price.Model)
		}
		if price.Updated != TableUpdated {
			t.Fatalf("%q: Updated = %q, want %q", price.Model, price.Updated, TableUpdated)
		}
		if price.InputPerMillion <= 0 || price.OutputPerMillion <= 0 {
			t.Fatalf("%q: 价格必须为正: %+v", price.Model, price)
		}
		if price.CachedInputPerMillion > price.InputPerMillion {
			t.Fatalf("%q: cached (%v) 高于 input (%v)", price.Model, price.CachedInputPerMillion, price.InputPerMillion)
		}
	}
}

// TestTableNewModelsHaveExplicitEntries 钉住审计 2F 修复后新增/修正的条目。
func TestTableNewModelsHaveExplicitEntries(t *testing.T) {
	tests := []struct {
		model         string
		input, cached float64
		output        float64
	}{
		{"deepseek-flash", 0.30, 0.006, 1.20},
		{"deepseek-v4-pro", 1.32, 0.044, 3.96},
		{"deepseek-v4-flash", 0.30, 0.006, 1.20}, // 官方：历史名，按 Flash 计费
		{"o3-mini", 1.10, 0.275, 4.40},
		{"qwen4-max", 1.50, 0.075, 4.50},
		{"qwen3.8-max", 2.00, 0, 6.00},
		{"qwen3.7-plus", 0.276, 0, 1.101},
		{"qwen-flash", 0.05, 0, 0.40},
		{"glm-4.6", 0.60, 0.11, 2.20},
	}
	for _, testCase := range tests {
		price, ok := Lookup(testCase.model)
		if !ok {
			t.Fatalf("%q 未收录（会被记成未定价、成本 0）", testCase.model)
		}
		if price.Model != testCase.model {
			t.Fatalf("%q 命中了 %q（应精确命中自身，不能被父型号前缀吃掉）", testCase.model, price.Model)
		}
		if price.InputPerMillion != testCase.input ||
			price.CachedInputPerMillion != testCase.cached ||
			price.OutputPerMillion != testCase.output {
			t.Fatalf("%q 价目 = %v/%v/%v, want %v/%v/%v", testCase.model,
				price.InputPerMillion, price.CachedInputPerMillion, price.OutputPerMillion,
				testCase.input, testCase.cached, testCase.output)
		}
	}
}

// TestLegacyDeepSeekNamesPointToCurrentModels 断言历史名仍可用但 Note 指向现行型号
// （UI 可以提示"这个模型名已过时"）。
func TestLegacyDeepSeekNamesPointToCurrentModels(t *testing.T) {
	for _, legacy := range []string{"deepseek-chat", "deepseek-reasoner"} {
		price, ok := Lookup(legacy)
		if !ok {
			t.Fatalf("%q 应当保留（兼容旧会话）", legacy)
		}
		if !strings.Contains(price.Note, "历史名") {
			t.Fatalf("%q 的 Note 应说明是历史名: %q", legacy, price.Note)
		}
		if !strings.Contains(price.Note, "deepseek-flash") && !strings.Contains(price.Note, "deepseek-v4-pro") {
			t.Fatalf("%q 的 Note 应指向现行型号: %q", legacy, price.Note)
		}
	}
}

// TestPeakPricedEntriesSaySoInNote 断言"取峰时价"的条目在 Note 里写明（否则用户会
// 对不上账单）。
func TestPeakPricedEntriesSaySoInNote(t *testing.T) {
	for _, model := range []string{"deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash"} {
		price, ok := Lookup(model)
		if !ok {
			t.Fatalf("%q 未收录", model)
		}
		if !strings.Contains(price.Note, "峰") {
			t.Fatalf("%q 的 Note 应写明取峰时价/谷时半价: %q", model, price.Note)
		}
	}
}
