package costmeter

// 预算告警的整数溢出回归（审计 2F 的 P2）。
//
// 原实现用 `costNano*10000 >= budgetNano*warnBasisPoints`：当预算 ≳ $1,152,922
// 时两侧 int64 溢出回绕，判定方向可能翻转（该告警不告警 / 不该告警却告警）。
// 修法：构造期把阈值算成纳美元（warnNano），Add 里只做一次整数比较。

import (
	"math"
	"sync/atomic"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/modelprice"
)

// unitPricer 让 1 token = $1，于是 "tokens == 美元数"，便于精确构造百分比。
func unitPricer() Option {
	return WithPricer(func(string) (modelprice.Price, bool) {
		return modelprice.Price{Model: "unit", InputPerMillion: 1_000_000}, true
	})
}

// TestWarnThresholdSurvivesLargeBudgets 是审计实测的两个反例 + 边界扫描。
func TestWarnThresholdSurvivesLargeBudgets(t *testing.T) {
	tests := []struct {
		name   string
		budget float64 // 美元
		pct    float64
		want   bool
	}{
		{"1.15M@81% 必须告警（旧实现溢出后漏报）", 1_150_000, 0.81, true},
		{"3.5M@79% 不该告警（旧实现溢出后误报）", 3_500_000, 0.79, false},
		{"5.8M@79% 不该告警（旧实现溢出后误报）", 5_800_000, 0.79, false},
		{"1.15M@79% 不告警", 1_150_000, 0.79, false},
		{"1.15M@80% 恰好阈值要告警", 1_150_000, 0.80, true},
		{"10M@81% 必须告警", 10_000_000, 0.81, true},
		{"10M@79% 不告警", 10_000_000, 0.79, false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			m := New(WithBudget(testCase.budget), unitPricer())
			m.Add("unit", rig.Usage{PromptTokens: int(math.Round(testCase.budget * testCase.pct))})
			if got := m.Warned(); got != testCase.want {
				t.Fatalf("Warned() = %v, want %v（预算 %v，用量 %v token，成本 %v）",
					got, testCase.want, testCase.budget,
					math.Round(testCase.budget*testCase.pct), m.Session().CostUSD)
			}
		})
	}
}

// TestWarnThresholdSweepLargeBudgets 扫描 0.9M–6M 美元（步长 5 万），确保没有任何
// 预算下出现"79% 告警"或"81% 不告警"。
func TestWarnThresholdSweepLargeBudgets(t *testing.T) {
	for budget := 900_000.0; budget <= 6_000_000; budget += 50_000 {
		for _, pct := range []float64{0.79, 0.81} {
			m := New(WithBudget(budget), unitPricer())
			m.Add("unit", rig.Usage{PromptTokens: int(math.Round(budget * pct))})
			want := pct > 0.80
			if got := m.Warned(); got != want {
				t.Fatalf("预算 $%.0f 用量 %.0f%%: Warned()=%v, want %v", budget, pct*100, got, want)
			}
		}
	}
}

// TestWarnThresholdWithCustomWarnAt 断言自定义阈值也按纳元阈值计算（不再相乘）。
func TestWarnThresholdWithCustomWarnAt(t *testing.T) {
	var warnings int32
	m := New(
		WithBudget(4_000_000),
		WithWarnAt(0.5),
		WithWarnFunc(func(Summary) { atomic.AddInt32(&warnings, 1) }),
		unitPricer(),
	)
	m.Add("unit", rig.Usage{PromptTokens: 1_900_000}) // 47.5%
	if m.Warned() {
		t.Fatal("47.5% 未到 50% 阈值")
	}
	m.Add("unit", rig.Usage{PromptTokens: 100_000}) // 50%
	if !m.Warned() {
		t.Fatal("50% 应当告警")
	}
	if got := atomic.LoadInt32(&warnings); got != 1 {
		t.Fatalf("warn 回调 %d 次, want 1", got)
	}
}

// TestWarnThresholdRoundsLikeMoney 断言阈值按四舍五入到纳元（不是截断），
// 避免 $0.0000000005 级别的边界漂移。
func TestWarnThresholdRoundsLikeMoney(t *testing.T) {
	// 预算 $1、阈值 33.33% → 阈值 = 333300000 纳元（0.3333 * 1e9）。
	m := New(WithBudget(1), WithWarnAt(0.3333), unitPricer())
	const wantNano = int64(333_300_000) // 0.3333 = 3333 万分比 → 1e9*3333/10000 = 333300000
	if got := m.warnThresholdNano(); got != wantNano {
		t.Fatalf("warnThresholdNano() = %d, want %d", got, wantNano)
	}
	if m.Exceeded() {
		t.Fatal("Exceeded() 在零用量时不应为真")
	}
}
