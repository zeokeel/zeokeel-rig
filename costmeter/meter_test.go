package costmeter

// 本文件是会话计量的契约测试：累加、按模型分组、缓存折扣、金额精度、预算边界、
// 并发、快照往返与 UI 用的 JSON 形状。

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/modelprice"
)

// approx 比较浮点金额（Summary.CostUSD 是 nano/1e9 的商，允许最后一位误差）。
func approx(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-9
}

// TestAddAccumulatesOneModel 覆盖最基本的累加。
func TestAddAccumulatesOneModel(t *testing.T) {
	m := New()
	m.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200})
	m.Add("gpt-4o", rig.Usage{PromptTokens: 500, CompletionTokens: 100, TotalTokens: 600})

	got := m.Session()
	if got.Requests != 2 {
		t.Fatalf("Requests = %d, want 2", got.Requests)
	}
	if got.PromptTokens != 1500 || got.CompletionTokens != 300 {
		t.Fatalf("tokens = %+v, want prompt 1500 completion 300", got)
	}
	// 1500 * 2.50/M + 300 * 10/M = 0.00375 + 0.003 = 0.00675
	if !approx(got.CostUSD, 0.00675) {
		t.Fatalf("CostUSD = %v, want 0.00675", got.CostUSD)
	}
	if !got.Estimated {
		t.Fatal("云端价目应当标 Estimated")
	}
	if len(got.ByModel) != 1 {
		t.Fatalf("ByModel = %+v, want 1 项", got.ByModel)
	}
	if _, ok := got.ByModel["gpt-4o"]; !ok {
		t.Fatalf("ByModel 的键应是表内规范名: %+v", got.ByModel)
	}
}

// TestByModelGrouping 覆盖多模型分组，并要求"分组求和 == 总计"。
func TestByModelGrouping(t *testing.T) {
	m := New()
	m.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CompletionTokens: 0})
	m.Add("deepseek-chat", rig.Usage{PromptTokens: 1000, CompletionTokens: 0})
	m.Add("gpt-4o-2024-08-06", rig.Usage{PromptTokens: 1000, CompletionTokens: 0})

	got := m.Session()
	if len(got.ByModel) != 2 {
		t.Fatalf("ByModel = %+v, want 2 项（带日期后缀的应归并到 gpt-4o）", got.ByModel)
	}
	if byModel := got.ByModel["gpt-4o"]; byModel.Requests != 2 {
		t.Fatalf("gpt-4o 分组 Requests = %d, want 2", byModel.Requests)
	}
	var sum float64
	var requests int
	for _, sub := range got.ByModel {
		if len(sub.ByModel) != 0 {
			t.Fatalf("嵌套 ByModel 应为空: %+v", sub)
		}
		sum += sub.CostUSD
		requests += sub.Requests
	}
	if !approx(sum, got.CostUSD) {
		t.Fatalf("分组求和 %v != 总计 %v", sum, got.CostUSD)
	}
	if requests != got.Requests {
		t.Fatalf("分组请求数 %d != 总计 %d", requests, got.Requests)
	}
}

// TestCachedTokensUseCachedPrice 覆盖缓存折扣：cached 部分按缓存价，其余按输入价。
func TestCachedTokensUseCachedPrice(t *testing.T) {
	m := New()
	// gpt-4o：input 2.50 / cached 1.25
	m.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CachedTokens: 400, CompletionTokens: 0})
	want := (600*2.50 + 400*1.25) / 1e6
	if !approx(m.Session().CostUSD, want) {
		t.Fatalf("CostUSD = %v, want %v", m.Session().CostUSD, want)
	}
	if m.Session().CachedTokens != 400 {
		t.Fatalf("CachedTokens = %d, want 400", m.Session().CachedTokens)
	}

	// 缓存数比 prompt 还多（脏数据）：不能让成本变负。
	m2 := New()
	m2.Add("gpt-4o", rig.Usage{PromptTokens: 100, CachedTokens: 500})
	if cost := m2.Session().CostUSD; cost < 0 {
		t.Fatalf("脏 usage 让成本变成负数: %v", cost)
	}
}

// TestCheapModelPrecision 钉住"整数累加不丢精度"：便宜模型的单次调用价远小于
// 1 微美元，若按微美元逐次四舍五入会全部变成 0。
func TestCheapModelPrecision(t *testing.T) {
	m := New()
	// deepseek-chat 输入 $0.27/M → 1 token = 0.27 微美元。
	for i := 0; i < 1000; i++ {
		m.Add("deepseek-chat", rig.Usage{PromptTokens: 1, TotalTokens: 1})
	}
	got := m.Session()
	if got.Requests != 1000 {
		t.Fatalf("Requests = %d, want 1000", got.Requests)
	}
	// 1000 × 0.27µ$ = 270µ$ = $0.00027
	if !approx(got.CostUSD, 0.00027) {
		t.Fatalf("CostUSD = %v, want 0.00027（逐次舍入会丢精度）", got.CostUSD)
	}
	// 再乘 1000 倍也必须精确（无浮点漂移）。
	m2 := New()
	for i := 0; i < 100000; i++ {
		m2.Add("deepseek-chat", rig.Usage{PromptTokens: 1})
	}
	if !approx(m2.Session().CostUSD, 0.027) {
		t.Fatalf("10 万次累加 = %v, want 0.027", m2.Session().CostUSD)
	}
}

// TestBudgetWarnBoundaries 覆盖 79% / 80% / 81% 与 100% 边界。
func TestBudgetWarnBoundaries(t *testing.T) {
	// 预算 $10，gpt-4o 输出 $10/M：每 1 tok 输出 = 0.001 美分… 直接构造金额更清楚：
	// 用自定义 pricer 让 1 token = 恰好 1 美分（10_000 微美元）。
	pricer := func(string) (modelprice.Price, bool) {
		return modelprice.Price{Model: "unit", InputPerMillion: 10_000, OutputPerMillion: 10_000}, true
	}
	// 1 token = 10000/1e6 = 0.01 USD
	newMeter := func() *Meter {
		return New(WithBudget(10), WithPricer(pricer))
	}

	m := newMeter()
	m.Add("unit", rig.Usage{PromptTokens: 799}) // $7.99
	if m.Warned() {
		t.Fatal("79.9% 就 warn 了")
	}
	if m.Exceeded() {
		t.Fatal("79.9% 就超预算了")
	}
	m.Add("unit", rig.Usage{PromptTokens: 1}) // $8.00
	if !m.Warned() {
		t.Fatal("恰好 80% 应当 warn")
	}
	if m.Exceeded() {
		t.Fatal("80% 不该算超预算")
	}
	m.Add("unit", rig.Usage{PromptTokens: 1}) // $8.01
	if !m.Warned() {
		t.Fatal("81% 应当保持 warn")
	}
	m.Add("unit", rig.Usage{PromptTokens: 199}) // $10.00
	if !m.Exceeded() {
		t.Fatal("恰好 100% 应当算超预算")
	}
	if got, ok := m.Remaining(); !ok || !approx(got, 0) {
		t.Fatalf("Remaining() = %v ok=%v, want 0/true", got, ok)
	}
	m.Add("unit", rig.Usage{PromptTokens: 1}) // $10.01
	if left, _ := m.Remaining(); left > 0 {
		t.Fatalf("超预算后 Remaining() = %v, want <= 0", left)
	}
}

// TestBudgetWarnCallbackOnce 覆盖"一次性 warn 回调"。
func TestBudgetWarnCallbackOnce(t *testing.T) {
	pricer := func(string) (modelprice.Price, bool) {
		return modelprice.Price{Model: "unit", InputPerMillion: 10_000}, true
	}
	var calls int
	var last Summary
	m := New(
		WithBudget(1),
		WithPricer(pricer),
		WithWarnFunc(func(s Summary) { calls++; last = s }),
	)
	for i := 0; i < 20; i++ {
		m.Add("unit", rig.Usage{PromptTokens: 10}) // 每次 1 美分
	}
	if calls != 1 {
		t.Fatalf("warn 回调被调用 %d 次, want 1", calls)
	}
	if last.Requests == 0 {
		t.Fatal("warn 回调没有拿到当时的 Summary")
	}
	// Reset 之后可以再次 warn。
	m.Reset()
	if m.Warned() {
		t.Fatal("Reset 后 Warned() 仍为 true")
	}
	m.Add("unit", rig.Usage{PromptTokens: 100})
	if calls != 2 {
		t.Fatalf("Reset 后 warn 回调没有再触发（calls=%d）", calls)
	}
}

// TestNoBudget 覆盖未设置预算：永不超限、Remaining 不可用。
func TestNoBudget(t *testing.T) {
	m := New()
	m.Add("gpt-4o", rig.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000})
	if m.Exceeded() {
		t.Fatal("没有预算却报了超预算")
	}
	if m.Warned() {
		t.Fatal("没有预算却 warn 了")
	}
	if _, ok := m.Remaining(); ok {
		t.Fatal("没有预算时 Remaining 应返回 ok=false")
	}
}

// TestConcurrentAdd 覆盖并发安全（配合 -race）。
func TestConcurrentAdd(t *testing.T) {
	m := New(WithBudget(1000))
	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			m.Add("gpt-4o", rig.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11})
			_ = m.Session()
			_ = m.Exceeded()
		}()
	}
	wg.Wait()

	got := m.Session()
	if got.Requests != goroutines {
		t.Fatalf("Requests = %d, want %d", got.Requests, goroutines)
	}
	if got.PromptTokens != goroutines*10 || got.CompletionTokens != goroutines {
		t.Fatalf("tokens = %+v", got)
	}
	// 100 × (10*2.50 + 1*10)/1e6 = 100 × 35e-6 = 0.0035
	if !approx(got.CostUSD, 0.0035) {
		t.Fatalf("CostUSD = %v, want 0.0035", got.CostUSD)
	}
}

// TestSnapshotRestoreRoundTrip 覆盖持久化往返（含 JSON）。
func TestSnapshotRestoreRoundTrip(t *testing.T) {
	m := New(WithBudget(10))
	m.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CompletionTokens: 200, CachedTokens: 100})
	m.Add("deepseek-chat", rig.Usage{PromptTokens: 500, CompletionTokens: 50})
	m.Add("mystery-model", rig.Usage{PromptTokens: 10})

	snap := m.Snapshot()
	encoded, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Snapshot 不能序列化: %v", err)
	}

	var decoded Snapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Snapshot 不能反序列化: %v", err)
	}
	restored := New(WithBudget(10))
	if err := restored.Restore(decoded); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	want := m.Session()
	got := restored.Session()
	if got.Requests != want.Requests || got.PromptTokens != want.PromptTokens ||
		got.CompletionTokens != want.CompletionTokens || got.CachedTokens != want.CachedTokens {
		t.Fatalf("还原后计数不一致: got %+v want %+v", got, want)
	}
	if got.CostUSD != want.CostUSD {
		t.Fatalf("还原后金额不一致: got %v want %v", got.CostUSD, want.CostUSD)
	}
	if strings.Join(got.UnpricedModels, ",") != strings.Join(want.UnpricedModels, ",") {
		t.Fatalf("还原后未定价列表不一致: got %v want %v", got.UnpricedModels, want.UnpricedModels)
	}
	// 还原后继续累加必须接着算（不是重置）。
	restored.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CompletionTokens: 0})
	if restored.Session().Requests != want.Requests+1 {
		t.Fatalf("Restore 之后累加没有接上: %+v", restored.Session())
	}
}

// TestRestoreRejectsBadSnapshot 覆盖脏快照：版本不符或负数一律拒绝，且不破坏当前状态。
func TestRestoreRejectsBadSnapshot(t *testing.T) {
	m := New()
	m.Add("gpt-4o", rig.Usage{PromptTokens: 100, CompletionTokens: 10})
	before := m.Session()

	bad := []Snapshot{
		{Version: SnapshotVersion + 1},
		{Version: 0},
		{Version: SnapshotVersion, Requests: -1},
		{Version: SnapshotVersion, CostNanoUSD: -5},
		{Version: SnapshotVersion, Models: map[string]ModelSnapshot{"x": {PromptTokens: -1}}},
	}
	for _, snap := range bad {
		if err := m.Restore(snap); err == nil {
			t.Fatalf("Restore(%+v) 应当报错", snap)
		}
		if got := m.Session(); got.Requests != before.Requests || got.CostUSD != before.CostUSD {
			t.Fatalf("失败的 Restore 破坏了当前状态: before %+v after %+v", before, got)
		}
	}
}

// TestReset 覆盖重置。
func TestReset(t *testing.T) {
	m := New(WithBudget(1))
	m.Add("gpt-4o", rig.Usage{PromptTokens: 1000})
	if m.Session().Requests != 1 {
		t.Fatal("前置失败")
	}
	m.Reset()
	got := m.Session()
	if got.Requests != 0 || got.CostUSD != 0 || len(got.ByModel) != 0 || got.Estimated {
		t.Fatalf("Reset 后 = %+v, want 零值", got)
	}
	if m.Exceeded() || m.Warned() {
		t.Fatal("Reset 后预算状态未清零")
	}
}

// TestUnknownModelIsUnpriced 覆盖"未知模型不算钱但记账"。
func TestUnknownModelIsUnpriced(t *testing.T) {
	m := New()
	m.Add("mystery-model", rig.Usage{PromptTokens: 1000, CompletionTokens: 100})
	got := m.Session()
	if got.Requests != 1 || got.PromptTokens != 1000 {
		t.Fatalf("未知模型也应当计数: %+v", got)
	}
	if got.CostUSD != 0 {
		t.Fatalf("未知模型被编了价格: %v", got.CostUSD)
	}
	if !got.Estimated {
		t.Fatal("有未定价模型时 Estimated 必须为 true（总额不可信）")
	}
	if len(got.UnpricedModels) != 1 || got.UnpricedModels[0] != "mystery-model" {
		t.Fatalf("UnpricedModels = %v, want [mystery-model]", got.UnpricedModels)
	}
}

// TestSummaryJSONShape 钉住给 UI 用的 JSON 形状（字段名是契约）。
func TestSummaryJSONShape(t *testing.T) {
	m := New(WithBudget(10))
	m.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CachedTokens: 400, CompletionTokens: 200})
	m.Add("deepseek-chat", rig.Usage{PromptTokens: 1000, CompletionTokens: 0})

	encoded, err := json.Marshal(m.Session())
	if err != nil {
		t.Fatalf("Session 不能序列化: %v", err)
	}
	got := string(encoded)

	// 顶层键（顺序即结构体字段顺序，UI 依赖字段名而不是顺序，但快照要稳定）。
	for _, want := range []string{
		`"requests":2`,
		`"prompt_tokens":2000`,
		`"completion_tokens":200`,
		`"cached_tokens":400`,
		`"cost_usd":0.00427`,
		`"estimated":true`,
		`"by_model":{`,
		`"gpt-4o":{`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Summary JSON 缺少 %s：%s", want, got)
		}
	}
	// 没有未定价模型时该字段应被省略（omitempty）。
	if strings.Contains(got, "unpriced_models") {
		t.Fatalf("无未定价模型时不该出现 unpriced_models: %s", got)
	}
	// 分组内不得嵌套 by_model。
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("解码: %v", err)
	}
	byModel := decoded["by_model"].(map[string]any)
	for name, raw := range byModel {
		sub := raw.(map[string]any)
		if _, nested := sub["by_model"]; nested {
			t.Fatalf("分组 %q 里嵌套了 by_model", name)
		}
	}
	// 键顺序稳定：同样输入两次序列化逐字节相同。
	other, err := json.Marshal(m.Session())
	if err != nil {
		t.Fatalf("再次序列化: %v", err)
	}
	if string(other) != got {
		t.Fatalf("Summary 序列化不确定:\n%s\n%s", got, other)
	}
	// 分组键在**序列化文本**里按字典序（encoding/json 对 map 排键），UI 可以直接
	// 逐行渲染。注意：不能把 JSON 解码回 map 再检查顺序——Go 的 map 迭代顺序是
	// 随机化的，那样写出来的是 flaky 断言（本用例最初就这么错过一次）。
	first := strings.Index(got, `"deepseek-chat":{`)
	second := strings.Index(got, `"gpt-4o":{`)
	if first < 0 || second < 0 {
		t.Fatalf("by_model 里缺少分组: %s", got)
	}
	if first > second {
		t.Fatalf("by_model 的键没有按字典序序列化（deepseek-chat 应在 gpt-4o 之前）: %s", got)
	}
}

// TestPricerOverride 覆盖宿主自定义价目（未知模型按自己的配置算）。
func TestPricerOverride(t *testing.T) {
	pricer := func(model string) (modelprice.Price, bool) {
		if model == "my-private-model" {
			return modelprice.Price{Model: "my-private-model", InputPerMillion: 1, OutputPerMillion: 2}, true
		}
		return modelprice.Lookup(model)
	}
	m := New(WithPricer(pricer))
	m.Add("my-private-model", rig.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000})
	m.Add("gpt-4o", rig.Usage{PromptTokens: 1_000_000})
	got := m.Session()
	// 1M input @1 + 1M output @2 = 3；再加 gpt-4o 1M input @2.5 = 5.5
	if !approx(got.CostUSD, 5.5) {
		t.Fatalf("CostUSD = %v, want 5.5", got.CostUSD)
	}
	if len(got.UnpricedModels) != 0 {
		t.Fatalf("自定义价目命中的模型不该算未定价: %v", got.UnpricedModels)
	}
}

// TestSummaryIsCopy 覆盖返回值语义：外部改 ByModel 不能反向污染 meter。
func TestSummaryIsCopy(t *testing.T) {
	m := New()
	m.Add("gpt-4o", rig.Usage{PromptTokens: 100})
	got := m.Session()
	got.ByModel["gpt-4o"] = Summary{Requests: 999}
	delete(got.ByModel, "gpt-4o")
	again := m.Session()
	if sub, ok := again.ByModel["gpt-4o"]; !ok || sub.Requests != 1 {
		t.Fatalf("Session() 返回了内部 map: %+v", again.ByModel)
	}
}

// TestZeroValueMeterUsable 覆盖"零值 Meter 可用"这条文档承诺。
func TestZeroValueMeterUsable(t *testing.T) {
	var m Meter
	m.Add("gpt-4o", rig.Usage{PromptTokens: 1000})
	if got := m.Session(); got.Requests != 1 || !approx(got.CostUSD, 0.0025) {
		t.Fatalf("零值 Meter 不可用: %+v", got)
	}
	if m.Exceeded() || m.Warned() {
		t.Fatal("零值 Meter 不该有预算状态")
	}
	if _, ok := m.Remaining(); ok {
		t.Fatal("零值 Meter 没有预算，Remaining 应 ok=false")
	}
	var nilMeter *Meter
	nilMeter.Add("gpt-4o", rig.Usage{PromptTokens: 1}) // 不 panic
	if nilMeter.Session().Requests != 0 {
		t.Fatal("nil Meter 的 Session 应返回零值")
	}
}

// TestZeroUsageCountsAsRequest：provider 没上报用量时仍应记一次请求（否则 UI 的
// "本轮调用了几次"会偏小），成本为 0。
func TestZeroUsageCountsAsRequest(t *testing.T) {
	m := New()
	m.Add("gpt-4o", rig.Usage{})
	got := m.Session()
	if got.Requests != 1 {
		t.Fatalf("Requests = %d, want 1（零用量也算一次调用）", got.Requests)
	}
	if got.PromptTokens != 0 || got.CompletionTokens != 0 || got.CostUSD != 0 {
		t.Fatalf("零用量应当零成本: %+v", got)
	}
}

// TestNegativeTokensClampToZero：脏 usage（负数）不能让成本变成负数或倒扣。
func TestNegativeTokensClampToZero(t *testing.T) {
	m := New()
	m.Add("gpt-4o", rig.Usage{PromptTokens: -100, CompletionTokens: -50, CachedTokens: -10})
	got := m.Session()
	if got.CostUSD < 0 {
		t.Fatalf("脏 usage 让成本变负: %v", got.CostUSD)
	}
	if got.PromptTokens != 0 || got.CompletionTokens != 0 {
		t.Fatalf("负数 token 应当被夹到 0: %+v", got)
	}
}

// TestWarnAtCustomFraction 覆盖自定义告警阈值（50%）。
func TestWarnAtCustomFraction(t *testing.T) {
	pricer := func(string) (modelprice.Price, bool) {
		return modelprice.Price{Model: "unit", InputPerMillion: 10_000}, true
	}
	m := New(WithBudget(1), WithWarnAt(0.5), WithPricer(pricer))
	m.Add("unit", rig.Usage{PromptTokens: 49}) // $0.49
	if m.Warned() {
		t.Fatal("49% 就 warn 了（阈值应为 50%）")
	}
	m.Add("unit", rig.Usage{PromptTokens: 1}) // $0.50
	if !m.Warned() {
		t.Fatal("恰好 50% 应当 warn")
	}
}

// TestBudgetNonPositiveMeansUnlimited：WithBudget(0) 与负数是"不限制"，不是"禁止花钱"。
func TestBudgetNonPositiveMeansUnlimited(t *testing.T) {
	for _, budget := range []float64{0, -5} {
		m := New(WithBudget(budget))
		m.Add("gpt-4o", rig.Usage{PromptTokens: 1_000_000})
		if m.Exceeded() {
			t.Fatalf("WithBudget(%v) 不应算超预算", budget)
		}
		if _, ok := m.Remaining(); ok {
			t.Fatalf("WithBudget(%v) 应当等价于没有预算", budget)
		}
		if got, ok := m.Budget(); ok || got != 0 {
			t.Fatalf("Budget() = %v/%v, want 0/false", got, ok)
		}
	}
	// 合法预算可以被读出来，UI 用来画进度条。
	m := New(WithBudget(12.5))
	if got, ok := m.Budget(); !ok || !approx(got, 12.5) {
		t.Fatalf("Budget() = %v/%v, want 12.5/true", got, ok)
	}
}

// TestModelsKeepsOriginalNames 覆盖"版本级明细不归并"。
func TestModelsKeepsOriginalNames(t *testing.T) {
	m := New()
	m.Add("openai/gpt-4o-2024-08-06", rig.Usage{PromptTokens: 1000, CompletionTokens: 100})
	m.Add("openai/gpt-4o-2024-08-06", rig.Usage{PromptTokens: 1000, CompletionTokens: 100})
	m.Add("gpt-4o", rig.Usage{PromptTokens: 1000, CompletionTokens: 100})

	summary := m.Session()
	if len(summary.Models) != 2 {
		t.Fatalf("Models = %+v, want 2 个原始名", summary.Models)
	}
	// 归并视图仍然只有一项。
	if len(summary.ByModel) != 1 {
		t.Fatalf("ByModel = %+v, want 1 项（归并到 gpt-4o）", summary.ByModel)
	}
	if got := summary.ByModel["gpt-4o"]; got.Requests != 3 {
		t.Fatalf("ByModel[gpt-4o].Requests = %d, want 3", got.Requests)
	}
	// 明细视图：长名字 2 次、短名字 1 次，Resolved 都是 gpt-4o。
	var long, short *ModelUsage
	for index := range summary.Models {
		switch summary.Models[index].Model {
		case "openai/gpt-4o-2024-08-06":
			long = &summary.Models[index]
		case "gpt-4o":
			short = &summary.Models[index]
		}
	}
	if long == nil || short == nil {
		t.Fatalf("Models 缺少原始名: %+v", summary.Models)
	}
	if long.Requests != 2 || short.Requests != 1 {
		t.Fatalf("Requests 计数错误: long=%d short=%d", long.Requests, short.Requests)
	}
	if long.Resolved != "gpt-4o" || short.Resolved != "gpt-4o" {
		t.Fatalf("Resolved = %q/%q, want gpt-4o", long.Resolved, short.Resolved)
	}
	if !long.Priced || !short.Priced {
		t.Fatal("两个条目都应 Priced=true")
	}
	// 明细求和必须等于总计。
	var requests int
	var cost float64
	for _, model := range summary.Models {
		requests += model.Requests
		cost += model.CostUSD
	}
	if requests != summary.Requests || !approx(cost, summary.CostUSD) {
		t.Fatalf("明细求和(%d, %v) != 总计(%d, %v)", requests, cost, summary.Requests, summary.CostUSD)
	}
}

// TestModelsUnpriced 覆盖未定价条目在明细里的形状。
func TestModelsUnpriced(t *testing.T) {
	m := New()
	m.Add("my-private-model", rig.Usage{PromptTokens: 500, CompletionTokens: 100})
	models := m.Session().Models
	if len(models) != 1 {
		t.Fatalf("Models = %+v", models)
	}
	entry := models[0]
	if entry.Model != "my-private-model" || entry.Resolved != "" || entry.Priced {
		t.Fatalf("未定价条目形状错误: %+v", entry)
	}
	if entry.CostUSD != 0 {
		t.Fatalf("未定价条目被编了价格: %v", entry.CostUSD)
	}
}

// TestModelsOrderIsDeterministic 覆盖序列化确定性：按 Model 字典序、逐字节相同。
func TestModelsOrderIsDeterministic(t *testing.T) {
	build := func() *Meter {
		m := New()
		m.Add("openai/gpt-4o-2024-08-06", rig.Usage{PromptTokens: 1000, CompletionTokens: 200, CachedTokens: 400})
		m.Add("gpt-4o", rig.Usage{PromptTokens: 500, CompletionTokens: 100})
		m.Add("ollama/llama3.1:8b", rig.Usage{PromptTokens: 9000, CompletionTokens: 1200})
		m.Add("my-private-model", rig.Usage{PromptTokens: 500, CompletionTokens: 100})
		return m
	}
	first, err := json.Marshal(build().Session())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for round := 0; round < 20; round++ {
		again, err := json.Marshal(build().Session())
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("第 %d 轮序列化不一致:\n%s\n%s", round, first, again)
		}
	}
	// 明细顺序按 Model 字典序。
	want := []string{"gpt-4o", "my-private-model", "ollama/llama3.1:8b", "openai/gpt-4o-2024-08-06"}
	got := build().Session().Models
	if len(got) != len(want) {
		t.Fatalf("Models 数量 = %d, want %d", len(got), len(want))
	}
	for index, name := range want {
		if got[index].Model != name {
			t.Fatalf("Models[%d] = %q, want %q", index, got[index].Model, name)
		}
	}
}

// TestSummaryGolden 是 UI 契约的 golden：整个 Summary（含 Models）逐字节比对。
// 改任何字段名/顺序/数字都会在这里炸，从而强制同步 UI 文档。
func TestSummaryGolden(t *testing.T) {
	m := New()
	m.Add("openai/gpt-4o-2024-08-06", rig.Usage{PromptTokens: 1000, CompletionTokens: 200, CachedTokens: 400})
	m.Add("gpt-4o", rig.Usage{PromptTokens: 500, CompletionTokens: 100})
	m.Add("ollama/llama3.1:8b", rig.Usage{PromptTokens: 9000, CompletionTokens: 1200})
	m.Add("my-private-model", rig.Usage{PromptTokens: 500, CompletionTokens: 100})

	encoded, err := json.Marshal(m.Session())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"requests":4,"prompt_tokens":11000,"completion_tokens":1600,"cached_tokens":400,` +
		`"cost_usd":0.00625,"estimated":true,"unpriced_models":["my-private-model"],` +
		`"by_model":{` +
		`"gpt-4o":{"requests":2,"prompt_tokens":1500,"completion_tokens":300,"cached_tokens":400,"cost_usd":0.00625,"estimated":true},` +
		`"llama3.1:8b":{"requests":1,"prompt_tokens":9000,"completion_tokens":1200,"cached_tokens":0,"cost_usd":0,"estimated":false},` +
		`"my-private-model":{"requests":1,"prompt_tokens":500,"completion_tokens":100,"cached_tokens":0,"cost_usd":0,"estimated":true}},` +
		`"models":[` +
		`{"model":"gpt-4o","resolved":"gpt-4o","requests":1,"prompt_tokens":500,"completion_tokens":100,"cached_tokens":0,"cost_usd":0.00225,"priced":true},` +
		`{"model":"my-private-model","resolved":"","requests":1,"prompt_tokens":500,"completion_tokens":100,"cached_tokens":0,"cost_usd":0,"priced":false},` +
		`{"model":"ollama/llama3.1:8b","resolved":"llama3.1:8b","requests":1,"prompt_tokens":9000,"completion_tokens":1200,"cached_tokens":0,"cost_usd":0,"priced":true},` +
		`{"model":"openai/gpt-4o-2024-08-06","resolved":"gpt-4o","requests":1,"prompt_tokens":1000,"completion_tokens":200,"cached_tokens":400,"cost_usd":0.004,"priced":true}` +
		`]}`
	if string(encoded) != want {
		t.Fatalf("Summary 形状变了（UI 契约）:\n got  %s\n want %s", encoded, want)
	}
}

// TestSnapshotRestoresModels 覆盖版本级明细的持久化往返。
func TestSnapshotRestoresModels(t *testing.T) {
	m := New()
	m.Add("openai/gpt-4o-2024-08-06", rig.Usage{PromptTokens: 1000, CompletionTokens: 200})
	m.Add("gpt-4o", rig.Usage{PromptTokens: 500, CompletionTokens: 100})
	m.Add("mystery", rig.Usage{PromptTokens: 10})

	encoded, err := json.Marshal(m.Snapshot())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	restored := New()
	if err := restored.Restore(decoded); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	want, _ := json.Marshal(m.Session())
	got, _ := json.Marshal(restored.Session())
	if string(got) != string(want) {
		t.Fatalf("还原后 Summary 不一致:\n got  %s\n want %s", got, want)
	}
}
