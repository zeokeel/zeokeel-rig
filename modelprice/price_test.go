package modelprice

// 本文件是价格表的契约测试：查找语义（精确/前缀/provider 限定/本地/未知）、
// 边界（不许把 gpt-4o 误判成 gpt-4）、以及"表是数据契约"的稳定性断言。

import (
	"strings"
	"testing"
)

// 精确匹配：常见模型的三个价都取到。
func TestLookupExact(t *testing.T) {
	cases := map[string]struct {
		input, cached, output float64
	}{
		"gpt-4o":        {2.50, 1.25, 10.00},
		"gpt-5":         {1.25, 0.125, 10.00},
		"deepseek-chat": {0.27, 0.07, 1.10},
	}
	for model, want := range cases {
		price, ok := Lookup(model)
		if !ok {
			t.Fatalf("Lookup(%q) = false, want true", model)
		}
		if price.InputPerMillion != want.input || price.CachedInputPerMillion != want.cached || price.OutputPerMillion != want.output {
			t.Fatalf("Lookup(%q) = %+v, want in=%v cached=%v out=%v", model, price, want.input, want.cached, want.output)
		}
		if !price.Estimated {
			t.Fatalf("Lookup(%q).Estimated = false；云端价目都是估算，必须标注", model)
		}
		if price.Model == "" {
			t.Fatalf("Lookup(%q).Model 为空：调用方需要原样的表内规范名", model)
		}
	}
}

// 前缀匹配：带日期/版本后缀的模型名落到同一条目。
func TestLookupPrefixWithSuffix(t *testing.T) {
	cases := map[string]string{
		"gpt-4o-2024-08-06":          "gpt-4o",
		"gpt-4o-mini-2024-07-18":     "gpt-4o-mini",
		"claude-3-5-sonnet-20241022": "claude-3-5-sonnet",
		"claude-sonnet-4-20250514":   "claude-sonnet-4",
		"deepseek-reasoner-20250115": "deepseek-reasoner",
	}
	for model, wantModel := range cases {
		price, ok := Lookup(model)
		if !ok {
			t.Fatalf("Lookup(%q) = false，前缀匹配没生效", model)
		}
		if price.Model != wantModel {
			t.Fatalf("Lookup(%q).Model = %q, want %q", model, price.Model, wantModel)
		}
	}
}

// 前缀必须在分隔符处断开：gpt-4o 不能被 gpt-4 吃掉。
func TestLookupPrefixRequiresBoundary(t *testing.T) {
	short, ok := Lookup("gpt-4")
	if !ok {
		t.Fatal("Lookup(gpt-4) 应当命中")
	}
	long, ok := Lookup("gpt-4o")
	if !ok {
		t.Fatal("Lookup(gpt-4o) 应当命中")
	}
	if long.Model == short.Model {
		t.Fatalf("gpt-4o 被前缀匹配成了 %q：边界判定失效", short.Model)
	}
	if long.InputPerMillion == short.InputPerMillion && long.OutputPerMillion == short.OutputPerMillion {
		t.Fatal("gpt-4o 与 gpt-4 价格相同，说明取到了同一条目")
	}
	// 贴着前缀但不是分隔符的名字必须落到未知，而不是猜一个。
	if price, ok := Lookup("gpt-4oz"); ok {
		t.Fatalf("Lookup(gpt-4oz) = %+v, want ok=false（不许把未知模型猜成 gpt-4）", price)
	}
}

// provider/model 形式：剥掉 provider 段再查，并把 provider 记进结果。
func TestLookupProviderQualified(t *testing.T) {
	cases := map[string]string{
		"openai/gpt-4o":               "openai",
		"anthropic/claude-3-5-sonnet": "anthropic",
		"deepseek/deepseek-chat":      "deepseek",
		"openai/gpt-4o-2024-08-06":    "openai",
	}
	for model, wantProvider := range cases {
		price, ok := Lookup(model)
		if !ok {
			t.Fatalf("Lookup(%q) = false", model)
		}
		if price.Provider != wantProvider {
			t.Fatalf("Lookup(%q).Provider = %q, want %q", model, price.Provider, wantProvider)
		}
	}
}

// 大小写与空白：用户从配置文件里抄来的名字不该失败。
func TestLookupNormalizesCaseAndSpace(t *testing.T) {
	for _, model := range []string{"GPT-4o", "  gpt-4o  ", "OpenAI/GPT-4O"} {
		if _, ok := Lookup(model); !ok {
			t.Fatalf("Lookup(%q) = false，应当做大小写/空白归一", model)
		}
	}
}

// 本地模型：provider 前缀即"没有 API 计费"。
func TestLookupLocalProvidersAreFree(t *testing.T) {
	for _, model := range []string{
		"ollama/llama3.1", "lmstudio/qwen2.5-7b", "vllm/mistral-7b",
		"llamacpp/phi-3", "mlx/gemma-2b", "local/anything",
	} {
		price, ok := Lookup(model)
		if !ok {
			t.Fatalf("Lookup(%q) = false, want 本地模型 0 价", model)
		}
		if price.InputPerMillion != 0 || price.OutputPerMillion != 0 || price.CachedInputPerMillion != 0 {
			t.Fatalf("Lookup(%q) = %+v, want 全 0", model, price)
		}
		if price.Estimated {
			t.Fatalf("Lookup(%q).Estimated = true；本地 0 价是事实，不是估算", model)
		}
	}
	// ollama 风格 tag（name:tag）也识别为本地。
	price, ok := Lookup("llama3.1:8b")
	if !ok || price.InputPerMillion != 0 {
		t.Fatalf("Lookup(llama3.1:8b) = %+v ok=%v, want 本地 0 价", price, ok)
	}
}

// 未知模型：ok=false，且不返回编造的价格。
func TestLookupUnknown(t *testing.T) {
	for _, model := range []string{"totally-unknown-model", "gpt-4oz", "", "   ", "/", "vendor/"} {
		price, ok := Lookup(model)
		if ok {
			t.Fatalf("Lookup(%q) = %+v ok=true, want ok=false（不许编造价格）", model, price)
		}
		if price != (Price{}) {
			t.Fatalf("Lookup(%q) 返回了非零 Price %+v", model, price)
		}
	}
}

// All 返回按名字排序的副本，修改它不会污染表。
func TestAllReturnsSortedCopy(t *testing.T) {
	all := All()
	if len(all) < 8 {
		t.Fatalf("All() 只有 %d 条，价格表太小", len(all))
	}
	for index := 1; index < len(all); index++ {
		if all[index-1].Model > all[index].Model {
			t.Fatalf("All() 未排序: %q > %q", all[index-1].Model, all[index].Model)
		}
	}
	// 表内数据自洽：没有负价，output 通常不便宜于 input。
	for _, price := range all {
		if price.InputPerMillion < 0 || price.CachedInputPerMillion < 0 || price.OutputPerMillion < 0 {
			t.Fatalf("%+v 有负价", price)
		}
		if price.OutputPerMillion < price.InputPerMillion {
			t.Fatalf("%+v 的 output 比 input 便宜，价格表可能填错了", price)
		}
		if price.CachedInputPerMillion > price.InputPerMillion {
			t.Fatalf("%+v 的缓存价高于输入价", price)
		}
	}
	// 副本语义。
	all[0].InputPerMillion = -1
	if again := All(); again[0].InputPerMillion < 0 {
		t.Fatal("All() 返回的是内部切片，外部修改污染了价格表")
	}
}

// 表里每一条都必须能被自己的规范名查回来（防止 key 与 Model 字段不一致）。
func TestTableSelfConsistent(t *testing.T) {
	for _, price := range All() {
		got, ok := Lookup(price.Model)
		if !ok {
			t.Fatalf("表内条目 %q 查不回来", price.Model)
		}
		if got.InputPerMillion != price.InputPerMillion || got.OutputPerMillion != price.OutputPerMillion {
			t.Fatalf("表内条目 %q 查询结果与 All() 不一致: %+v vs %+v", price.Model, got, price)
		}
		if strings.TrimSpace(price.Model) != price.Model || price.Model != strings.ToLower(price.Model) {
			t.Fatalf("表内条目 %q 未归一（应小写、无空白）", price.Model)
		}
	}
}

// 多段 provider（OpenRouter 风格 vendor/org/model）：逐段剥离后仍能命中。
func TestLookupMultiSegmentProvider(t *testing.T) {
	price, ok := Lookup("openrouter/anthropic/claude-3-5-sonnet")
	if !ok {
		t.Fatal("多段 provider 形式没命中")
	}
	if price.Model != "claude-3-5-sonnet" {
		t.Fatalf("Model = %q", price.Model)
	}
	if price.Provider != "openrouter" {
		t.Fatalf("Provider = %q, want 第一段 openrouter", price.Provider)
	}
	// 本地后端的多段形式也应当识别（ollama 在前缀里）。
	if price, ok := Lookup("ollama/llama3.1:8b"); !ok || price.InputPerMillion != 0 {
		t.Fatalf("ollama/llama3.1:8b = %+v ok=%v", price, ok)
	}
}

// 表元数据：采集日期必须存在且形状合法（UI 会直接显示它）。
func TestTableUpdatedShape(t *testing.T) {
	if len(TableUpdated) != len("2006-01-02") {
		t.Fatalf("TableUpdated = %q, want YYYY-MM-DD", TableUpdated)
	}
	for _, price := range All() {
		if price.Estimated && price.Updated == "" {
			t.Fatalf("估算条目 %q 没有 Updated 日期", price.Model)
		}
	}
}
