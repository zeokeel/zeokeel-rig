package kit_test

// 审计（只读）：成本归因——"这次会话花了多少钱"必须记到**实际使用的模型**上。
//
// 关注点：① `RunRequest.Model` 覆盖 `Config.Model` 时，Meter 的 by_model/models 是
// 否按真实模型分组；② 未定价模型是否进 unpriced_models 且 Estimated=true；
// ③ 同一会话里换模型，两个模型的金额各自独立、总额等于两者之和。

import (
	"context"
	"math"
	"testing"

	"github.com/zeokeel/zeokeel-rig/costmeter"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/modelprice"
)

// TestAuditCostAttributionFollowsRunModel 断言成本记到 RunRequest.Model 覆盖后的
// 模型，而不是 Config.Model。
func TestAuditCostAttributionFollowsRunModel(t *testing.T) {
	meter := costmeter.New()
	provider := &usageProvider{}
	a := mustNew(t, kit.Config{
		Model:    "gpt-4o", // 有价模型（input 2.50 / output 10.00）
		Provider: provider,
		Root:     t.TempDir(),
		Meter:    meter,
	})

	// 第一次：用 Config.Model 默认值（gpt-4o）。
	if _, err := a.Run(context.Background(), kit.RunRequest{
		SessionID: "s1",
		Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "第一次")},
	}); err != nil {
		t.Fatalf("第一次 Run: %v", err)
	}

	// 第二次：RunRequest.Model 覆盖成另一个**不同价位**的模型（gpt-5）。
	if _, err := a.Run(context.Background(), kit.RunRequest{
		SessionID: "s1",
		Model:     "gpt-5",
		Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "第二次")},
	}); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}

	sum := meter.Session()
	if sum.Requests < 2 {
		t.Fatalf("Requests = %d, want ≥2（两次 Run 各至少一次模型调用）", sum.Requests)
	}

	// by_model：两个规范名各一条。
	first, ok := sum.ByModel["gpt-4o"]
	if !ok {
		t.Fatalf("by_model 缺少 gpt-4o: %+v", sum.ByModel)
	}
	second, ok := sum.ByModel["gpt-5"]
	if !ok {
		t.Fatalf("by_model 缺少 gpt-5（说明 RunRequest.Model 覆盖没有归因到实际模型）: %+v", sum.ByModel)
	}
	if first.CostUSD <= 0 || second.CostUSD <= 0 {
		t.Fatalf("成本为 0: gpt-4o=%v gpt-5=%v", first.CostUSD, second.CostUSD)
	}
	if first.CostUSD == second.CostUSD {
		t.Fatalf("两个价位不同的模型金额相同（%v），归因可疑", first.CostUSD)
	}

	// models：保留两个原始名。
	names := make(map[string]string, len(sum.Models))
	for _, usage := range sum.Models {
		names[usage.Model] = usage.Resolved
	}
	if _, ok := names["gpt-4o"]; !ok {
		t.Fatalf("models 缺少 gpt-4o: %+v", names)
	}
	if _, ok := names["gpt-5"]; !ok {
		t.Fatalf("models 缺少 gpt-5: %+v", names)
	}

	// 总额 = 各模型之和。
	if want := first.CostUSD + second.CostUSD; math.Abs(sum.CostUSD-want) > 1e-9 {
		t.Fatalf("总额 %v ≠ 分项之和 %v", sum.CostUSD, want)
	}

	// 用价目表手算一次，确认单价没算错（usageProvider 固定 prompt=1000/cached=200/completion=500）。
	price4o, _ := modelprice.Lookup("gpt-4o")
	wantFirst := (800*price4o.InputPerMillion + 200*price4o.CachedInputPerMillion + 500*price4o.OutputPerMillion) / 1e6
	if math.Abs(first.CostUSD-wantFirst) > 1e-9 {
		t.Fatalf("gpt-4o 金额 = %v, 手算 = %v", first.CostUSD, wantFirst)
	}
	t.Logf("归因正确：gpt-4o=$%.8f gpt-5=$%.8f 合计=$%.8f（Requests=%d）",
		first.CostUSD, second.CostUSD, sum.CostUSD, sum.Requests)
}

// TestAuditCostAttributionUnknownModelIsFlagged 断言未定价模型会被显式标记，
// 而不是悄悄计成 0 元让用户以为"这次没花钱"。
func TestAuditCostAttributionUnknownModelIsFlagged(t *testing.T) {
	meter := costmeter.New()
	provider := &usageProvider{}
	a := mustNew(t, kit.Config{
		Model:    "totally-unknown-model-9x",
		Provider: provider,
		Root:     t.TempDir(),
		Meter:    meter,
	})
	if _, err := a.Run(context.Background(), kit.RunRequest{
		SessionID: "s1",
		Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "hi")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sum := meter.Session()
	if sum.CostUSD != 0 {
		t.Fatalf("未定价模型应当记 0，实际 %v", sum.CostUSD)
	}
	if !sum.Estimated {
		t.Fatal("未定价模型必须让 Estimated=true（UI 才能提示金额偏低）")
	}
	found := false
	for _, name := range sum.UnpricedModels {
		if name == "totally-unknown-model-9x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unpriced_models 里没有未知模型: %+v", sum.UnpricedModels)
	}
	if sum.Requests == 0 || sum.PromptTokens == 0 {
		t.Fatalf("未定价调用也必须计入用量: %+v", sum)
	}
}

// TestAuditCostEstimatedAlwaysTrueForBuiltinTable 记录一个 UI 契约问题：表内所有
// 价目都标了 Estimated=true，因此只要有一次已定价调用，Summary.Estimated 就恒为
// true——UI 无法用它区分"表内估算"与"有未定价模型"。
func TestAuditCostEstimatedAlwaysTrueForBuiltinTable(t *testing.T) {
	meter := costmeter.New()
	provider := &usageProvider{}
	a := mustNew(t, kit.Config{
		Model:    "gpt-4o",
		Provider: provider,
		Root:     t.TempDir(),
		Meter:    meter,
	})
	if _, err := a.Run(context.Background(), kit.RunRequest{
		SessionID: "s1",
		Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "hi")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sum := meter.Session()
	if !sum.Estimated {
		t.Fatalf("表内价目 Estimated=true，Summary.Estimated 应当为 true: %+v", sum)
	}
	if len(sum.UnpricedModels) != 0 {
		t.Fatalf("全部已定价时不应有 unpriced_models: %+v", sum.UnpricedModels)
	}
	// 结论：Estimated=true 只说明"金额是估算"，不能反推"有未定价模型"；
	// 需要判断"是否漏算"必须看 UnpricedModels 是否为空。
	t.Log("Estimated=true 有两种成因（表内估算价 / 未定价模型），UI 必须同时读 unpriced_models")
}
