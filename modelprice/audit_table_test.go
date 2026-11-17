package modelprice_test

// 审计（只读）：价目表本身能不能支撑"给用户看花了多少钱"。
//
// 关注点：① 表结构自洽（键/大小写/Provider/Updated）；② 前缀匹配是否会把兄弟型号
// 错配成更贵/更便宜的型号；③ 本地推理与未知模型的分流是否"宁高不低"。

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/modelprice"
)

// TestAuditPriceTableSelfConsistent 断言表内每条记录自洽：键==Model、全小写、
// Provider 非空、标了 Estimated、Updated 与 TableUpdated 一致。
func TestAuditPriceTableSelfConsistent(t *testing.T) {
	all := modelprice.All()
	if len(all) == 0 {
		t.Fatal("价目表为空")
	}
	for _, price := range all {
		if price.Model != strings.ToLower(price.Model) {
			t.Fatalf("%s: Model 不是小写", price.Model)
		}
		if price.Provider == "" {
			t.Fatalf("%s: Provider 为空", price.Model)
		}
		if !price.Estimated {
			t.Fatalf("%s: 表内价目应当标 Estimated（会过时），实际 false", price.Model)
		}
		if price.Updated != modelprice.TableUpdated {
			t.Fatalf("%s: Updated=%q, want %q", price.Model, price.Updated, modelprice.TableUpdated)
		}
		if price.InputPerMillion <= 0 || price.OutputPerMillion <= 0 {
			t.Fatalf("%s: 价格非正: %+v", price.Model, price)
		}
		if price.CachedInputPerMillion > price.InputPerMillion {
			t.Fatalf("%s: cached 价高于 input 价（%v > %v），缓存反而更贵",
				price.Model, price.CachedInputPerMillion, price.InputPerMillion)
		}
	}
	t.Logf("价目表共 %d 条，TableUpdated=%s", len(all), modelprice.TableUpdated)
}

// TestAuditPriceLookupVariants 断言常见模型名形态都能命中同一个规范名。
func TestAuditPriceLookupVariants(t *testing.T) {
	tests := []struct {
		name       string
		wantModel  string
		wantProved string
	}{
		{"gpt-4o", "gpt-4o", ""},
		{"gpt-4o-2024-08-06", "gpt-4o", ""},
		{"openai/gpt-4o", "gpt-4o", "openai"},
		{"GPT-4O", "gpt-4o", ""},
		{"  gpt-4o  ", "gpt-4o", ""},
		{"claude-sonnet-4-20250514", "claude-sonnet-4", ""},
		{"anthropic/claude-3-5-sonnet-20241022", "claude-3-5-sonnet", "anthropic"},
		{"deepseek-chat", "deepseek-chat", ""},
		{"qwen-max-2025-01-25", "qwen-max", ""},
	}
	for _, testCase := range tests {
		price, ok := modelprice.Lookup(testCase.name)
		if !ok {
			t.Fatalf("%q 没命中价目", testCase.name)
		}
		if price.Model != testCase.wantModel {
			t.Fatalf("%q 命中 %q, want %q", testCase.name, price.Model, testCase.wantModel)
		}
		if testCase.wantProved != "" && price.Provider != testCase.wantProved {
			t.Fatalf("%q provider = %q, want %q", testCase.name, price.Provider, testCase.wantProved)
		}
	}
}

// TestAuditPricePrefixDoesNotCrossModelFamilies 断言前缀匹配不会把"更便宜/更贵的
// 兄弟型号"错配成父型号价位。
func TestAuditPricePrefixDoesNotCrossModelFamilies(t *testing.T) {
	// gpt-4oz / claude-3-5-sonnetx 这类"粘在一起"的名字不能命中。
	for _, name := range []string{"gpt-4oz", "claude-3-5-sonnetx", "gpt-4omini", "qwen-maxx"} {
		if price, ok := modelprice.Lookup(name); ok {
			t.Fatalf("%q 被错配成 %q（前缀没有边界检查）", name, price.Model)
		}
	}

	// o3-mini 是 o3 的**便宜兄弟**（$1.10/$4.40 vs o3 的 $2/$8）。表里只有 o3 与
	// o4-mini，于是 o3-mini 会按 o3 计价——金额被高估约 1.8 倍。
	if price, ok := modelprice.Lookup("o3-mini"); ok {
		t.Logf("发现：o3-mini 命中 %q（input=%v output=%v）；若与公开价不同即为高估/低估",
			price.Model, price.InputPerMillion, price.OutputPerMillion)
	}
	if price, ok := modelprice.Lookup("o4-mini"); !ok || price.Model != "o4-mini" {
		t.Fatalf("o4-mini 应当精确命中自身的价目: %+v", price)
	}
}

// TestAuditPriceLocalAndUnknownRouting 断言本地推理走 0 价、未知模型不编造价格。
func TestAuditPriceLocalAndUnknownRouting(t *testing.T) {
	tests := []struct {
		name       string
		wantPriced bool
		wantLocal  bool
	}{
		{"ollama/llama3.1", true, true},
		{"lmstudio/qwen2.5", true, true},
		{"llama3.1:8b", true, true},
		{"vllm/any-model-name", true, true},
		{"together/llama3.1:8b", false, false}, // 付费代理不能被当成本地 0 价
		{"groq/llama-3.3-70b", false, false},
		{"some-future-model", false, false},
		{"", false, false},
		{"   ", false, false},
	}
	for _, testCase := range tests {
		price, ok := modelprice.Lookup(testCase.name)
		if ok != testCase.wantPriced {
			t.Fatalf("%q: priced=%v, want %v（price=%+v）", testCase.name, ok, testCase.wantPriced, price)
		}
		if !ok {
			continue
		}
		isLocal := price.InputPerMillion == 0 && price.OutputPerMillion == 0
		if isLocal != testCase.wantLocal {
			t.Fatalf("%q: local=%v, want %v（price=%+v）", testCase.name, isLocal, testCase.wantLocal, price)
		}
	}
}

// TestAuditPriceTableMissingCurrentModels 记录"现行模型名缺条目"的风险：缺失条目会
// 让成本记 0（而不是算错），必须靠 Summary.Estimated/UnpricedModels 提示用户。
func TestAuditPriceTableMissingCurrentModels(t *testing.T) {
	// 这些是 2026 年在售/主流的模型名（部分按官方文档核对过，见审计报告）。
	candidates := []string{
		"deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash",
		"gpt-5.1", "gpt-5.1-mini", "o3-mini", "gpt-4.1-turbo",
		"claude-opus-4-1", "claude-sonnet-4-5", "claude-haiku-4-5",
		"qwen4-max", "qwen3.8-max", "qwen3.7-plus", "qwen-flash",
		"gemini-2.5-pro", "gemini-2.5-flash", "grok-4", "kimi-k2", "glm-4.6",
	}
	var missing []string
	for _, name := range candidates {
		if _, ok := modelprice.Lookup(name); !ok {
			missing = append(missing, name)
		}
	}
	t.Logf("未收录（会被记成未定价、成本 0）的候选模型 %d/%d: %v", len(missing), len(candidates), missing)
	if len(missing) == 0 {
		t.Log("价目表覆盖了全部候选模型")
	}
}
