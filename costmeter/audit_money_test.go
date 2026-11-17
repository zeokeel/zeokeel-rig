package costmeter_test

// 审计（只读）：这个数字能不能拿去给用户看？
//
// 关注点：① 纳美元整数累加的精度与解析解的误差；② Snapshot/Restore 往返后逐纳元
// 相等；③ 预算边界（79/80/81%）与并发下 warn 只触发一次；④ 缓存折扣是否真的用
// cached 单价；⑤ 脏数据（cached > prompt、负 token）会不会算出负成本；⑥ 归因
// （by_model 归并 / models 原始名 / 未定价模型）。

import (
	"encoding/json"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/costmeter"
	"github.com/zeokeel/zeokeel-rig/modelprice"
)

// nanoUSD 把美元换算成纳美元整数（与 Meter 内部口径一致），用于"逐纳元相等"比较。
func nanoUSD(usd float64) int64 { return int64(math.Round(usd * 1e9)) }

// TestAuditMeterPrecisionMillionSingleTokenCalls 用 100 万次 1-token 调用验证累加
// 精度：与解析解的误差必须 < 1e-6 美元（即 1000 纳美元）。
func TestAuditMeterPrecisionMillionSingleTokenCalls(t *testing.T) {
	const calls = 1_000_000
	for _, model := range []string{"gpt-4o", "deepseek-reasoner", "qwen-turbo"} {
		price, ok := modelprice.Lookup(model)
		if !ok {
			t.Fatalf("%s 没有价目", model)
		}
		m := costmeter.New()
		for i := 0; i < calls; i++ {
			m.Add(model, rig.Usage{PromptTokens: 1})
		}
		// 解析解：calls * P / 1e6
		want := float64(calls) * price.InputPerMillion / 1e6
		got := m.Session().CostUSD
		if math.Abs(got-want) >= 1e-6 {
			t.Fatalf("%s: 1M 次 1-token 调用成本 = %v, 解析解 = %v, 差 %v (≥1e-6)",
				model, got, want, math.Abs(got-want))
		}
		if nano := nanoUSD(got); nano != nanoUSD(want) {
			t.Fatalf("%s: 纳元口径不相等: %d vs %d", model, nano, nanoUSD(want))
		}
		t.Logf("%s: 1M×1token → $%v（纳元 %d，无累积漂移）", model, got, nanoUSD(got))
	}
}

// TestAuditMeterTablePricesAreNanoExact 断言表内每条价目的"每 token 纳美元"都是整数：
// 这是"逐次 round 到纳元也不丢精度"的前提。任何新价格若带 4 位以上小数会在这里炸。
func TestAuditMeterTablePricesAreNanoExact(t *testing.T) {
	for _, price := range modelprice.All() {
		for name, perMillion := range map[string]float64{
			"input":  price.InputPerMillion,
			"cached": price.CachedInputPerMillion,
			"output": price.OutputPerMillion,
		} {
			perToken := perMillion * 1000 // 每 token 的纳美元
			if math.Abs(perToken-math.Round(perToken)) > 1e-9 {
				t.Fatalf("%s 的 %s 价 %v/M 不是整数纳元/ token（%v）", price.Model, name, perMillion, perToken)
			}
		}
	}
}

// TestAuditMeterSubNanoPriceLosesPrecision 记录一个边界：单次成本小于 1 纳元时，
// 每次都 round 到 0，累加会**完全丢失**这部分金额（表内价目不会触发，但宿主用
// WithPricer 配置的廉价/折扣模型可能触发）。
func TestAuditMeterSubNanoPriceLosesPrecision(t *testing.T) {
	// 0.0001 美元/M → 每 token 0.1 纳元。
	cheap := costmeter.WithPricer(func(string) (modelprice.Price, bool) {
		return modelprice.Price{Model: "cheap", InputPerMillion: 0.0001}, true
	})
	const calls = 1_000_000
	m := costmeter.New(cheap)
	for i := 0; i < calls; i++ {
		m.Add("cheap", rig.Usage{PromptTokens: 1})
	}
	got := m.Session().CostUSD
	want := float64(calls) * 0.0001 / 1e6 // 1e-4 美元
	if math.Abs(got-want) < 1e-9 {
		t.Logf("意外地精确: got=%v want=%v", got, want)
		return
	}
	t.Logf("已知边界：亚纳元单次成本被 round 掉 → got=$%v want=$%v（丢失 %.0f%%）", got, want, 100*(1-got/want))
	if got != 0 {
		t.Fatalf("预期完全丢失（0），实际 %v", got)
	}
}

// TestAuditMeterSnapshotRestoreRoundTrip 断言 Snapshot→Restore 往返后累加器逐纳元
// 相等（而不是"美元浮点近似相等"）。
func TestAuditMeterSnapshotRestoreRoundTrip(t *testing.T) {
	source := costmeter.New()
	source.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CachedTokens: 400, CompletionTokens: 200})
	source.Add("openai/gpt-4o-2024-08-06", rig.Usage{PromptTokens: 300, CompletionTokens: 50})
	source.Add("gpt-5-nano", rig.Usage{PromptTokens: 12345})
	source.Add("totally-unknown-model", rig.Usage{PromptTokens: 999})
	source.Add("  ", rig.Usage{PromptTokens: 1})

	snapshot := source.Snapshot()
	if snapshot.Version != costmeter.SnapshotVersion {
		t.Fatalf("Version = %d, want %d", snapshot.Version, costmeter.SnapshotVersion)
	}
	target := costmeter.New()
	if err := target.Restore(snapshot); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := target.Snapshot(); !reflect.DeepEqual(got, snapshot) {
		t.Fatalf("往返后快照不同:\n got %+v\nwant %+v", got, snapshot)
	}
	// 逐纳元相等（走 Summary 的 CostUSD 也要能还原回同一个纳元数）。
	sourceSummary, targetSummary := source.Session(), target.Session()
	if nanoUSD(sourceSummary.CostUSD) != nanoUSD(targetSummary.CostUSD) {
		t.Fatalf("纳元口径不等: %d vs %d", nanoUSD(sourceSummary.CostUSD), nanoUSD(targetSummary.CostUSD))
	}
	// 序列化成 JSON 再读回，仍逐纳元相等（真实的持久化路径）。
	blob, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded costmeter.Snapshot
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	fresh := costmeter.New()
	if err := fresh.Restore(decoded); err != nil {
		t.Fatalf("Restore(decoded): %v", err)
	}
	if got, want := fresh.Snapshot().CostNanoUSD, snapshot.CostNanoUSD; got != want {
		t.Fatalf("JSON 往返后纳元不同: %d vs %d", got, want)
	}
	if !reflect.DeepEqual(fresh.Session(), source.Session()) {
		t.Fatalf("JSON 往返后 Summary 不同:\n got %+v\nwant %+v", fresh.Session(), source.Session())
	}
}

// TestAuditMeterBudgetBoundaries 断言 79%/80%/81% 处的告警与超限判定。
func TestAuditMeterBudgetBoundaries(t *testing.T) {
	// 每 token $0.001（1e6 纳元），于是 1 token = 0.1% 的 $1 预算。
	pricer := costmeter.WithPricer(func(string) (modelprice.Price, bool) {
		return modelprice.Price{Model: "unit", InputPerMillion: 1000}, true
	})
	const budget = 1.0
	tests := []struct {
		name     string
		tokens   int
		warned   bool
		exceeded bool
	}{
		{"79%", 790, false, false},
		{"80%", 800, true, false},
		{"81%", 810, true, false},
		{"100%", 1000, true, true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var warnings int32
			m := costmeter.New(
				costmeter.WithBudget(budget),
				pricer,
				costmeter.WithWarnFunc(func(costmeter.Summary) { atomic.AddInt32(&warnings, 1) }),
			)
			m.Add("unit", rig.Usage{PromptTokens: testCase.tokens})
			if got := m.Warned(); got != testCase.warned {
				t.Fatalf("Warned() = %v, want %v（摘要 %+v）", got, testCase.warned, m.Session())
			}
			if got := m.Exceeded(); got != testCase.exceeded {
				t.Fatalf("Exceeded() = %v, want %v", got, testCase.exceeded)
			}
			if got := atomic.LoadInt32(&warnings); (got > 0) != testCase.warned {
				t.Fatalf("warn 回调次数 = %d, want 触发=%v", got, testCase.warned)
			}
		})
	}
}

// TestAuditMeterConcurrentAddWarnsOnce 断言并发 Add 时 warn 回调只触发一次。
func TestAuditMeterConcurrentAddWarnsOnce(t *testing.T) {
	pricer := costmeter.WithPricer(func(string) (modelprice.Price, bool) {
		return modelprice.Price{Model: "unit", InputPerMillion: 1000}, true
	})
	var warnings int32
	m := costmeter.New(
		costmeter.WithBudget(1.0),
		pricer,
		costmeter.WithWarnFunc(func(costmeter.Summary) { atomic.AddInt32(&warnings, 1) }),
	)
	const workers, perWorker = 100, 10 // 每个 worker 100 token = 0.01 美元 → 总计 100 美元
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < perWorker; j++ {
				m.Add("unit", rig.Usage{PromptTokens: 100})
			}
		}()
	}
	wait.Wait()
	if got := atomic.LoadInt32(&warnings); got != 1 {
		t.Fatalf("并发 100 次 Add 后 warn 回调触发 %d 次, want 1", got)
	}
	if !m.Warned() {
		t.Fatal("Warned() 应当为 true")
	}
}

// TestAuditMeterCachedTokensUseCachedRate 断言缓存命中按 cached 单价计，而不是普通
// 输入单价；并覆盖 cached=0 折扣（回落到输入价）与 cached>prompt 的脏数据。
func TestAuditMeterCachedTokensUseCachedRate(t *testing.T) {
	t.Run("缓存命中按 cached 单价", func(t *testing.T) {
		price, ok := modelprice.Lookup("gpt-4o")
		if !ok || price.CachedInputPerMillion <= 0 {
			t.Fatalf("gpt-4o 应当有 cached 价: %+v", price)
		}
		m := costmeter.New()
		m.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CachedTokens: 1000})
		wantNano := int64(math.Round(1000 * price.CachedInputPerMillion * 1000))
		if got := m.Snapshot().CostNanoUSD; got != wantNano {
			t.Fatalf("全命中成本 = %d 纳元, want %d（若按输入价算会是 %d）",
				got, wantNano, int64(math.Round(1000*price.InputPerMillion*1000)))
		}
	})
	t.Run("mixed 命中与未命中", func(t *testing.T) {
		price, _ := modelprice.Lookup("gpt-4o")
		m := costmeter.New()
		m.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CachedTokens: 250, CompletionTokens: 100})
		wantNano := int64(math.Round(
			750*price.InputPerMillion*1000 +
				250*price.CachedInputPerMillion*1000 +
				100*price.OutputPerMillion*1000))
		if got := m.Snapshot().CostNanoUSD; got != wantNano {
			t.Fatalf("混合用量成本 = %d, want %d", got, wantNano)
		}
	})
	t.Run("没有缓存折扣时回落输入价", func(t *testing.T) {
		price, ok := modelprice.Lookup("gpt-4-turbo")
		if !ok || price.CachedInputPerMillion != 0 {
			t.Skipf("gpt-4-turbo 的 cached 价不是 0: %+v", price)
		}
		m := costmeter.New()
		m.Add("gpt-4-turbo", rig.Usage{PromptTokens: 1000, CachedTokens: 1000})
		wantNano := int64(math.Round(1000 * price.InputPerMillion * 1000))
		if got := m.Snapshot().CostNanoUSD; got != wantNano {
			t.Fatalf("无折扣模型成本 = %d, want %d（宁高不低）", got, wantNano)
		}
	})
	t.Run("cached 大于 prompt 不会算出负成本", func(t *testing.T) {
		m := costmeter.New()
		m.Add("gpt-4o", rig.Usage{PromptTokens: 100, CachedTokens: 100000})
		snapshot := m.Snapshot()
		if snapshot.CostNanoUSD < 0 {
			t.Fatalf("脏数据算出了负成本: %d", snapshot.CostNanoUSD)
		}
		// cached 被夹到 prompt：等价于 100 个缓存命中 token（而不是"负的未缓存部分"）。
		price, _ := modelprice.Lookup("gpt-4o")
		wantNano := int64(math.Round(100 * price.CachedInputPerMillion * 1000))
		if snapshot.CostNanoUSD != wantNano {
			t.Fatalf("cached>prompt 成本 = %d, want %d（钳制后）", snapshot.CostNanoUSD, wantNano)
		}
		if got := m.Session().CostUSD; got < 0 {
			t.Fatalf("CostUSD 为负: %v", got)
		}
	})
	t.Run("负 token 按 0 处理", func(t *testing.T) {
		m := costmeter.New()
		m.Add("gpt-4o", rig.Usage{PromptTokens: -1, CompletionTokens: -1, CachedTokens: -1})
		snapshot := m.Snapshot()
		if snapshot.CostNanoUSD != 0 || snapshot.Requests != 1 {
			t.Fatalf("负 token: 成本=%d（want 0）请求数=%d（want 1）", snapshot.CostNanoUSD, snapshot.Requests)
		}
		if sum := m.Session(); sum.PromptTokens != 0 || sum.CompletionTokens != 0 {
			t.Fatalf("负 token 被累加进汇总: %+v", sum)
		}
	})
}

// TestAuditMeterAttributionByModelAndModels 断言两个不同价位的模型分别归因正确，
// 且版本名归并到规范名（by_model）而原始名保留（models）。
func TestAuditMeterAttributionByModelAndModels(t *testing.T) {
	m := costmeter.New()
	// 同一规范模型的两个版本名 + 另一个更贵的模型 + 一个未定价模型。
	m.Add("gpt-4o-2024-08-06", rig.Usage{PromptTokens: 1_000_000})
	m.Add("openai/gpt-4o", rig.Usage{PromptTokens: 1_000_000})
	m.Add("gpt-5", rig.Usage{PromptTokens: 1_000_000})
	m.Add("mystery-model-v1", rig.Usage{PromptTokens: 1_000_000})

	sum := m.Session()

	cheap, _ := modelprice.Lookup("gpt-4o")
	expensive, _ := modelprice.Lookup("gpt-5")

	// by_model：两个 gpt-4o 版本归并到一个键。
	merged, ok := sum.ByModel["gpt-4o"]
	if !ok {
		t.Fatalf("by_model 缺少 gpt-4o: %+v", sum.ByModel)
	}
	if merged.Requests != 2 || merged.PromptTokens != 2_000_000 {
		t.Fatalf("gpt-4o 归并后用量 = %+v, want 2 次 2M token", merged)
	}
	if want := 2 * cheap.InputPerMillion; math.Abs(merged.CostUSD-want) > 1e-9 {
		t.Fatalf("gpt-4o 成本 = %v, want %v", merged.CostUSD, want)
	}
	if _, ok := sum.ByModel["gpt-5"]; !ok {
		t.Fatalf("by_model 缺少 gpt-5: %+v", sum.ByModel)
	}

	// models：保留每个原始名（含版本后缀与 provider 前缀）。
	wantNames := map[string]string{
		"gpt-4o-2024-08-06": "gpt-4o",
		"openai/gpt-4o":     "gpt-4o",
		"gpt-5":             "gpt-5",
		"mystery-model-v1":  "",
	}
	gotNames := make(map[string]string, len(sum.Models))
	for _, usage := range sum.Models {
		gotNames[usage.Model] = usage.Resolved
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("models 解析结果 = %+v, want %+v", gotNames, wantNames)
	}

	// 总额 = 各部分之和。
	wantTotal := 2*cheap.InputPerMillion + expensive.InputPerMillion
	if math.Abs(sum.CostUSD-wantTotal) > 1e-9 {
		t.Fatalf("总额 = %v, want %v", sum.CostUSD, wantTotal)
	}

	// 未定价模型：进 unpriced_models、estimated=true、成本 0，且不能悄悄算成 0 元。
	if !reflect.DeepEqual(sum.UnpricedModels, []string{"mystery-model-v1"}) {
		t.Fatalf("unpriced_models = %+v", sum.UnpricedModels)
	}
	if !sum.Estimated {
		t.Fatal("含未定价模型时 Estimated 必须为 true")
	}
	for _, usage := range sum.Models {
		if usage.Model == "mystery-model-v1" {
			if usage.Priced || usage.CostUSD != 0 {
				t.Fatalf("未定价模型 = %+v, want priced=false cost=0", usage)
			}
		}
	}
}

// TestAuditMeterUnpricedDoesNotLowerPrice 断言"查不到价目"不会被当成免费：总额
// 只按已定价部分累计，Estimated 置 true 让 UI 能提示"金额偏低"。
func TestAuditMeterUnpricedDoesNotLowerPrice(t *testing.T) {
	onlyPriced := costmeter.New()
	onlyPriced.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CompletionTokens: 1000})
	withUnpriced := costmeter.New()
	withUnpriced.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CompletionTokens: 1000})
	withUnpriced.Add("unknown-xyz", rig.Usage{PromptTokens: 9_000_000, CompletionTokens: 9_000_000})

	first, second := onlyPriced.Session(), withUnpriced.Session()
	if first.CostUSD != second.CostUSD {
		t.Fatalf("未定价用量改变了金额: %v vs %v", first.CostUSD, second.CostUSD)
	}
	if first.Estimated == true {
		// 表内价目本身都标了 Estimated，这里只是提示：金额是估算值。
		t.Log("注意：表内价目 Estimated=true，因此任何已定价用量也会让 Summary.Estimated=true")
	}
	if !second.Estimated || len(second.UnpricedModels) != 1 {
		t.Fatalf("未定价模型没有被标记: %+v", second)
	}
	if second.Requests != 2 || second.PromptTokens != 1000+9_000_000 {
		t.Fatalf("用量统计漏了未定价调用: %+v", second)
	}
}

// TestAuditMeterResetClearsWarn 断言 Reset 之后可以再次告警（宿主按月/按会话重置）。
func TestAuditMeterResetClearsWarn(t *testing.T) {
	pricer := costmeter.WithPricer(func(string) (modelprice.Price, bool) {
		return modelprice.Price{Model: "unit", InputPerMillion: 1000}, true
	})
	var warnings int32
	m := costmeter.New(
		costmeter.WithBudget(1.0),
		pricer,
		costmeter.WithWarnFunc(func(costmeter.Summary) { atomic.AddInt32(&warnings, 1) }),
	)
	m.Add("unit", rig.Usage{PromptTokens: 800}) // 80%
	m.Add("unit", rig.Usage{PromptTokens: 100})
	if got := atomic.LoadInt32(&warnings); got != 1 {
		t.Fatalf("首次跨阈值 warn 次数 = %d, want 1", got)
	}
	m.Reset()
	if m.Warned() {
		t.Fatal("Reset 后 Warned() 应当为 false")
	}
	m.Add("unit", rig.Usage{PromptTokens: 800})
	if got := atomic.LoadInt32(&warnings); got != 2 {
		t.Fatalf("Reset 后再次跨阈值 warn 次数 = %d, want 2", got)
	}
}
