package costmeter

// UI 契约（审计 2F 的 P2）：金额要能"带着限定"展示。
//
//   - Summary.TableUpdated 让 UI 显示"价目表更新于 …"；
//   - ModelUsage.Estimated 让逐行也能标 "≈"；
//   - 本地模型（ollama/vllm/…）是 priced=true + cost=0 + estimated=false，
//     UI 应显示"本地·免费"而不是 $0.00。

import (
	"encoding/json"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/modelprice"
)

// TestSummaryCarriesPriceTableDate 断言 Summary 带上价目表采集日期。
func TestSummaryCarriesPriceTableDate(t *testing.T) {
	m := New()
	m.Add("gpt-4o", rig.Usage{PromptTokens: 10})
	sum := m.Session()
	if sum.TableUpdated != modelprice.TableUpdated {
		t.Fatalf("TableUpdated = %q, want %q", sum.TableUpdated, modelprice.TableUpdated)
	}
	encoded, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["price_table_updated"] != modelprice.TableUpdated {
		t.Fatalf("JSON 字段 price_table_updated = %v, want %q", decoded["price_table_updated"], modelprice.TableUpdated)
	}
}

// TestModelUsageEstimatedFlag 断言逐模型的 estimated 标记：
// 表内估算价 → true；本地模型 → false（"免费"不是"估算"）；未定价 → true。
func TestModelUsageEstimatedFlag(t *testing.T) {
	m := New()
	m.Add("gpt-4o", rig.Usage{PromptTokens: 10})
	m.Add("ollama/llama3.1:8b", rig.Usage{PromptTokens: 10})
	m.Add("mystery-model", rig.Usage{PromptTokens: 10})

	byName := make(map[string]ModelUsage, len(m.Session().Models))
	for _, usage := range m.Session().Models {
		byName[usage.Model] = usage
	}
	if got, ok := byName["gpt-4o"]; !ok || !got.Estimated {
		t.Fatalf("gpt-4o 应当 estimated=true（表内估算价）: %+v", got)
	}
	local, ok := byName["ollama/llama3.1:8b"]
	if !ok {
		t.Fatalf("缺少本地模型条目: %+v", byName)
	}
	if !local.Priced || local.CostUSD != 0 || local.Estimated {
		t.Fatalf("本地模型应当是 priced=true cost=0 estimated=false（显示「本地·免费」）: %+v", local)
	}
	if unknown, ok := byName["mystery-model"]; !ok || !unknown.Estimated || unknown.Priced {
		t.Fatalf("未定价模型应当 priced=false estimated=true（金额不可信）: %+v", unknown)
	}
}
