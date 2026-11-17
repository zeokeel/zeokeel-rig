package costmeter

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/modelprice"
)

// SnapshotVersion 是 Snapshot 的格式版本。加载到不认识的版本必须报错，而不是
// 猜着读——旧客户端读到新数据时静默算错金额是最糟的结果。
const SnapshotVersion = 1

// nanoPerUSD 是 1 美元等于多少纳美元（1e-9 美元）。
const nanoPerUSD = 1_000_000_000

// defaultWarnBasisPoints 是默认的告警阈值（80%），单位是万分比。
const defaultWarnBasisPoints = 8000

// Pricer 是价目查询函数。默认用 modelprice.Lookup；宿主可以换成自己的配置。
type Pricer func(model string) (modelprice.Price, bool)

// Summary 是一次会话的计量汇总，也是给 UI 的 JSON 契约。
//
// 字段名是契约：UI 直接消费 cost_usd / estimated / by_model。ByModel 里的每一项
// 都是同样的结构（但不再嵌套 ByModel）。
type Summary struct {
	// Requests 是模型调用次数（每次 Add 一次）。
	Requests int `json:"requests"`
	// PromptTokens 是输入 token 总数（**包含**缓存命中部分）。
	PromptTokens int `json:"prompt_tokens"`
	// CompletionTokens 是输出 token 总数。
	CompletionTokens int `json:"completion_tokens"`
	// CachedTokens 是命中提示缓存的输入 token 数。
	CachedTokens int `json:"cached_tokens"`
	// CostUSD 是估算金额（美元）。
	CostUSD float64 `json:"cost_usd"`
	// Estimated 为真表示金额是估算（表内价目会过时），或含未定价模型。
	Estimated bool `json:"estimated"`
	// TableUpdated 是内置价目表的采集日期（YYYY-MM-DD），供 UI 显示"价目表更新于"。
	// 注意它有两个成因不同的含义：Estimated=true 既可能是"表内价是估算值"，也可能是
	// "有未定价模型"；要判断"是否漏算"必须看 UnpricedModels 是否为空。
	TableUpdated string `json:"price_table_updated,omitempty"`
	// UnpricedModels 是查不到价目的模型名（已排序）。空表示全部计价。
	UnpricedModels []string `json:"unpriced_models,omitempty"`
	// ByModel 是按模型分组的明细（键是表内规范名或原始名）。空时省略。
	ByModel map[string]Summary `json:"by_model,omitempty"`
	// Models 是按**调用时的原始模型名**分组的明细（不归并版本），按 Model 字典序
	// 排列，序列化逐字节确定。空时省略。
	Models []ModelUsage `json:"models,omitempty"`
}

// ModelUsage 是单个原始模型名的用量明细。
//
// 与 Summary.ByModel 的区别：ByModel 把 `gpt-4o-2024-08-06` 与 `openai/gpt-4o`
// 归并到 `gpt-4o`（回答"这个模型花了多少"）；Models 保留每个原始名字（回答"这次
// 会话具体用了哪几个版本、各几次"）。UI 要显示版本明细就读 Models。
type ModelUsage struct {
	// Model 是调用方传给 Add 的原始名字（去首尾空白）。
	Model string `json:"model"`
	// Resolved 是价目表命中的规范名；未定价时为空字符串。
	Resolved string `json:"resolved"`
	// Requests/PromptTokens/CompletionTokens/CachedTokens 是该原始名的用量。
	Requests         int `json:"requests"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	// CostUSD 是该原始名的估算成本。
	CostUSD float64 `json:"cost_usd"`
	// Estimated 为 true 表示这一行的金额是估算值（表内价目会过时）或含未定价用量。
	// 本地推理模型是 Priced=true、CostUSD=0、Estimated=false——UI 应显示"本地·免费"
	// 而不是 $0.00。
	Estimated bool `json:"estimated"`
	// Priced 为 false 表示查不到价目（成本记 0）。
	Priced bool `json:"priced"`
}

// Snapshot 是 Meter 的可持久化状态（配合会话存储）。
type Snapshot struct {
	Version     int                      `json:"version"`
	Requests    int                      `json:"requests"`
	CostNanoUSD int64                    `json:"cost_nano_usd"`
	Estimated   bool                     `json:"estimated"`
	Warned      bool                     `json:"warned"`
	Models      map[string]ModelSnapshot `json:"models,omitempty"`
}

// ModelSnapshot 是单个**原始模型名**的持久化状态（Snapshot.Models 以原始名为键）。
type ModelSnapshot struct {
	// Resolved 是当初解析到的价目表规范名；空表示当时未定价。持久化它是为了
	// "价目表后来变了"时仍能还原当时的分组（历史不可追认）。
	Resolved         string `json:"resolved,omitempty"`
	Requests         int    `json:"requests"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
	CostNanoUSD      int64  `json:"cost_nano_usd"`
	Estimated        bool   `json:"estimated"`
	// Priced 为 false 表示该模型查不到价目（成本记 0）。
	Priced bool `json:"priced"`
}

// counters 是一个模型（或总计）的累加器。
type counters struct {
	requests     int
	prompt       int
	completion   int
	cached       int
	costNano     int64
	estimated    bool
	priced       bool
	hasUntrusted bool
}

// nameEntry 是"原始模型名"维度的累加器（counter + 它解析到的规范名）。
type nameEntry struct {
	counters
	resolved string
}

// Option 配置 Meter。
type Option func(*Meter)

// WithBudget 设置会话预算（美元）。usd <= 0 表示"不限制"。
//
// 预算只影响 Warned()/Exceeded() 的判定，Meter 不会自行中断任何东西。
func WithBudget(usd float64) Option {
	return func(m *Meter) {
		if usd > 0 && !math.IsNaN(usd) && !math.IsInf(usd, 0) {
			m.budgetNano = int64(math.Round(usd * nanoPerUSD))
			m.hasBudget = true
			return
		}
		m.budgetNano = 0
		m.hasBudget = false
	}
}

// WithWarnAt 设置告警阈值（0,1]，默认 0.8。超出范围时回落到 0.8。
func WithWarnAt(fraction float64) Option {
	return func(m *Meter) {
		if fraction <= 0 || fraction > 1 || math.IsNaN(fraction) {
			m.warnBasisPoints = defaultWarnBasisPoints
			return
		}
		m.warnBasisPoints = int64(math.Round(fraction * 10000))
	}
}

// WithWarnFunc 设置"达到告警阈值"时的回调。回调只会被调用一次（直到 Reset）。
//
// 回调在 Meter 的锁**之外**执行，因此可以在里面直接读 Session() 或做 UI 更新；
// 反过来不要在回调里调用 Add（会重入）。
func WithWarnFunc(fn func(Summary)) Option {
	return func(m *Meter) { m.warnFn = fn }
}

// WithPricer 替换价目查询（宿主自定义价格 / 私有模型 / 单位换算）。
func WithPricer(pricer Pricer) Option {
	return func(m *Meter) { m.pricer = pricer }
}

// Meter 是会话计量器。零值可用；并发安全。
type Meter struct {
	mu sync.Mutex

	total   counters
	byModel map[string]*counters
	byName  map[string]*nameEntry

	hasBudget       bool
	budgetNano      int64
	warnBasisPoints int64
	// warnNano 是**构造期算好的**告警阈值（纳美元），Add 里只做一次整数比较。
	//
	// 不要改回 `costNano*10000 >= budgetNano*warnBasisPoints`：预算 ≳ $1,152,922
	// 时两侧 int64 会溢出回绕，判定方向可能翻转（审计 2F 实测：$1.15M@81% 漏报、
	// $3.5M@79% 误报）。见 warn_budget_test.go。
	warnNano int64
	warned   bool
	warnFn   func(Summary)

	pricer Pricer
}

// New 构造一个 Meter。
func New(opts ...Option) *Meter {
	m := &Meter{
		byModel:         make(map[string]*counters),
		byName:          make(map[string]*nameEntry),
		warnBasisPoints: defaultWarnBasisPoints,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	m.recomputeWarnThreshold()
	return m
}

// recomputeWarnThreshold 按当前预算与告警阈值重算纳元阈值。
//
// 只在构造期（New 应用完所有 Option 之后）调用一次：预算与阈值都是不可变配置，
// 这样 Add 的热路径上不再有乘法，也就不会溢出。
func (m *Meter) recomputeWarnThreshold() {
	if !m.hasBudget || m.budgetNano <= 0 {
		m.warnNano = 0
		return
	}
	m.warnNano = int64(math.Round(float64(m.budgetNano) * float64(m.warnBasisPoints) / 10000))
}

// warnThresholdNano 返回告警阈值的纳美元数（0 表示没有预算/阈值）。
func (m *Meter) warnThresholdNano() int64 { return m.warnNano }

// price 查询价目（nil pricer 回落到内置表）。
func (m *Meter) price(model string) (modelprice.Price, bool) {
	if m.pricer != nil {
		return m.pricer(model)
	}
	return modelprice.Lookup(model)
}

// costNano 计算一次调用的纳美元成本。
//
// 规则：
//   - cached token 优先用缓存价；缓存价为 0 视为"没有公开折扣"，按普通输入价计
//     （宁高不低，避免把成本算少）；
//   - cached > prompt（脏数据）时把 cached 夹到 prompt，避免出现负的未缓存部分；
//   - 负数 token 按 0 处理。
func costNano(price modelprice.Price, usage rig.Usage) int64 {
	prompt := usage.PromptTokens
	if prompt < 0 {
		prompt = 0
	}
	cached := usage.CachedTokens
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	completion := usage.CompletionTokens
	if completion < 0 {
		completion = 0
	}

	cachedRate := price.CachedInputPerMillion
	if cachedRate <= 0 && price.InputPerMillion > 0 {
		cachedRate = price.InputPerMillion
	}
	uncached := prompt - cached

	// 每 token 价格 = P/1e6 美元 = P*1000 纳美元。
	nano := float64(uncached)*price.InputPerMillion*1000 +
		float64(cached)*cachedRate*1000 +
		float64(completion)*price.OutputPerMillion*1000
	if nano <= 0 {
		return 0
	}
	return int64(math.Round(nano))
}

// Add 记录一次模型调用。
//
// model 只是查询价目用的名字（可以是 `gpt-4o-2024-08-06` 或 `openai/gpt-4o`）；
// usage 是 provider 上报的用量。provider 没上报用量时传零值即可——那也算一次
// 请求（Requests 会 +1），成本为 0。
func (m *Meter) Add(model string, usage rig.Usage) {
	if m == nil {
		return
	}
	price, priced := m.price(model)
	cost := int64(0)
	if priced {
		cost = costNano(price, usage)
	}
	key := summarizeName(model, price, priced)
	original := trimName(model)
	resolved := ""
	if priced {
		resolved = price.Model
	}

	m.mu.Lock()
	if m.byModel == nil {
		m.byModel = make(map[string]*counters)
	}
	if m.byName == nil {
		m.byName = make(map[string]*nameEntry)
	}
	entry := m.byModel[key]
	if entry == nil {
		entry = &counters{priced: priced}
		m.byModel[key] = entry
	}
	if !priced {
		entry.priced = false
	}
	named := m.byName[original]
	if named == nil {
		named = &nameEntry{counters: counters{priced: priced}, resolved: resolved}
		m.byName[original] = named
	}
	if !priced {
		named.priced = false
	}
	if named.resolved == "" && resolved != "" {
		named.resolved = resolved
	}
	apply := func(target *counters) {
		target.requests++
		target.prompt += maxInt(usage.PromptTokens, 0)
		target.completion += maxInt(usage.CompletionTokens, 0)
		target.cached += maxInt(usage.CachedTokens, 0)
		target.costNano += cost
		if priced && price.Estimated {
			target.estimated = true
		}
		if !priced {
			target.estimated = true
			target.hasUntrusted = true
		}
	}
	apply(entry)
	apply(&named.counters)
	apply(&m.total)

	shouldWarn := false
	var warnSummary Summary
	if m.hasBudget && !m.warned {
		if m.total.costNano >= m.warnNano {
			m.warned = true
			shouldWarn = true
			warnSummary = m.summaryLocked()
		}
	}
	m.mu.Unlock()

	if shouldWarn && m.warnFn != nil {
		m.warnFn(warnSummary)
	}
}

// maxInt 返回较大的非负值。
func maxInt(value, floor int) int {
	if value < floor {
		return floor
	}
	return value
}

// trimName 去掉首尾空白；空串归一成 "(unknown)"。
func trimName(model string) string {
	name := model
	// 去掉首尾空白（与 modelprice 的归一保持一致的"最小惊讶"）。
	for len(name) > 0 && (name[0] == ' ' || name[0] == '\t' || name[0] == '\n' || name[0] == '\r') {
		name = name[1:]
	}
	for len(name) > 0 {
		last := name[len(name)-1]
		if last != ' ' && last != '\t' && last != '\n' && last != '\r' {
			break
		}
		name = name[:len(name)-1]
	}
	if name == "" {
		return "(unknown)"
	}
	return name
}

// summarizeName 返回 ByModel 的分组键：已定价用表内规范名，未定价用原始名。
func summarizeName(model string, price modelprice.Price, priced bool) string {
	if priced && price.Model != "" {
		return price.Model
	}
	return trimName(model)
}

// Session 返回当前汇总（深拷贝，调用方可以随意修改）。
func (m *Meter) Session() Summary {
	if m == nil {
		return Summary{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.summaryLocked()
}

// summaryLocked 在持锁状态下构造 Summary。
func (m *Meter) summaryLocked() Summary {
	summary := Summary{
		Requests:         m.total.requests,
		PromptTokens:     m.total.prompt,
		CompletionTokens: m.total.completion,
		CachedTokens:     m.total.cached,
		CostUSD:          float64(m.total.costNano) / nanoPerUSD,
		Estimated:        m.total.estimated,
		TableUpdated:     modelprice.TableUpdated,
	}
	if len(m.byModel) > 0 {
		byModel := make(map[string]Summary, len(m.byModel))
		unpriced := make([]string, 0, 2)
		for name, entry := range m.byModel {
			byModel[name] = Summary{
				Requests:         entry.requests,
				PromptTokens:     entry.prompt,
				CompletionTokens: entry.completion,
				CachedTokens:     entry.cached,
				CostUSD:          float64(entry.costNano) / nanoPerUSD,
				Estimated:        entry.estimated,
			}
			if !entry.priced {
				unpriced = append(unpriced, name)
			}
		}
		summary.ByModel = byModel
		if len(unpriced) > 0 {
			sort.Strings(unpriced)
			summary.UnpricedModels = unpriced
		}
	}
	if len(m.byName) > 0 {
		models := make([]ModelUsage, 0, len(m.byName))
		for name, entry := range m.byName {
			models = append(models, ModelUsage{
				Model:            name,
				Resolved:         entry.resolved,
				Requests:         entry.requests,
				PromptTokens:     entry.prompt,
				CompletionTokens: entry.completion,
				CachedTokens:     entry.cached,
				CostUSD:          float64(entry.costNano) / nanoPerUSD,
				Priced:           entry.priced,
				Estimated:        entry.estimated,
			})
		}
		// 按原始模型名字典序：序列化逐字节确定（UI 可直接逐行渲染、测试可 golden）。
		sort.Slice(models, func(left, right int) bool { return models[left].Model < models[right].Model })
		summary.Models = models
	}
	return summary
}

// Warned 报告是否已经达到告警阈值（达到后恒为 true，直到 Reset）。
func (m *Meter) Warned() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.warned
}

// Exceeded 报告是否已经花超预算（成本 ≥ 预算）。没有预算时恒为 false。
func (m *Meter) Exceeded() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasBudget {
		return false
	}
	return m.total.costNano >= m.budgetNano
}

// Remaining 返回剩余预算（美元，可能为负）。第二个返回值为 false 表示没有预算。
func (m *Meter) Remaining() (float64, bool) {
	if m == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasBudget {
		return 0, false
	}
	return float64(m.budgetNano-m.total.costNano) / nanoPerUSD, true
}

// Budget 返回预算（美元）与是否设置了预算。
func (m *Meter) Budget() (float64, bool) {
	if m == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasBudget {
		return 0, false
	}
	return float64(m.budgetNano) / nanoPerUSD, true
}

// Reset 清零计量与告警状态（预算与回调保持不变）。
func (m *Meter) Reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total = counters{}
	m.byModel = make(map[string]*counters)
	m.byName = make(map[string]*nameEntry)
	m.warned = false
}

// Snapshot 导出可持久化状态。
//
// 快照以**原始模型名**为键（并记下当时解析到的规范名），因此即使价目表在两次
// 运行之间变了，Restore 也能还原"当时按什么分组的"——历史不追认。
func (m *Meter) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{Version: SnapshotVersion}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := Snapshot{
		Version:     SnapshotVersion,
		Requests:    m.total.requests,
		CostNanoUSD: m.total.costNano,
		Estimated:   m.total.estimated,
		Warned:      m.warned,
	}
	if len(m.byName) > 0 {
		snap.Models = make(map[string]ModelSnapshot, len(m.byName))
		for name, entry := range m.byName {
			snap.Models[name] = ModelSnapshot{
				Resolved:         entry.resolved,
				Requests:         entry.requests,
				PromptTokens:     entry.prompt,
				CompletionTokens: entry.completion,
				CachedTokens:     entry.cached,
				CostNanoUSD:      entry.costNano,
				Estimated:        entry.estimated,
				Priced:           entry.priced,
			}
		}
	}
	return snap
}

// Restore 用快照**整体替换**当前状态。
//
// 校验失败时返回错误且**当前状态保持不变**（先校验、后替换）。版本不符一律拒绝：
// 静默读错版本会导致金额错得离谱。
func (m *Meter) Restore(snapshot Snapshot) error {
	if m == nil {
		return errors.New("costmeter: Restore on nil meter")
	}
	if snapshot.Version != SnapshotVersion {
		return fmt.Errorf("costmeter: snapshot version %d is not supported (want %d)",
			snapshot.Version, SnapshotVersion)
	}
	if snapshot.Requests < 0 || snapshot.CostNanoUSD < 0 {
		return fmt.Errorf("costmeter: snapshot has negative counters (requests=%d cost=%d)",
			snapshot.Requests, snapshot.CostNanoUSD)
	}

	// 先在同一份临时结构里重建两个视图，校验通过后再一次性换上去。
	total := counters{}
	byName := make(map[string]*nameEntry, len(snapshot.Models))
	byModel := make(map[string]*counters)
	names := make([]string, 0, len(snapshot.Models))
	for name := range snapshot.Models {
		names = append(names, name)
	}
	// 排序只为让错误信息稳定可复现。
	sort.Strings(names)
	for _, name := range names {
		model := snapshot.Models[name]
		if name == "" {
			return errors.New("costmeter: snapshot has an empty model name")
		}
		if model.Requests < 0 || model.PromptTokens < 0 || model.CompletionTokens < 0 ||
			model.CachedTokens < 0 || model.CostNanoUSD < 0 {
			return fmt.Errorf("costmeter: snapshot model %q has negative counters", name)
		}
		entry := &nameEntry{
			counters: counters{
				requests:     model.Requests,
				prompt:       model.PromptTokens,
				completion:   model.CompletionTokens,
				cached:       model.CachedTokens,
				costNano:     model.CostNanoUSD,
				estimated:    model.Estimated,
				priced:       model.Priced,
				hasUntrusted: !model.Priced,
			},
			resolved: model.Resolved,
		}
		if !model.Priced {
			entry.estimated = true
		}
		byName[name] = entry

		// 归并视图：按当初解析到的规范名分组（空则退回原始名）。
		group := model.Resolved
		if group == "" {
			group = name
		}
		merged := byModel[group]
		if merged == nil {
			merged = &counters{priced: model.Priced}
			byModel[group] = merged
		}
		merged.requests += entry.requests
		merged.prompt += entry.prompt
		merged.completion += entry.completion
		merged.cached += entry.cached
		merged.costNano += entry.costNano
		merged.estimated = merged.estimated || entry.estimated
		merged.hasUntrusted = merged.hasUntrusted || entry.hasUntrusted
		if !model.Priced {
			merged.priced = false
		}

		total.requests += entry.requests
		total.prompt += entry.prompt
		total.completion += entry.completion
		total.cached += entry.cached
		total.costNano += entry.costNano
		total.estimated = total.estimated || entry.estimated
		total.hasUntrusted = total.hasUntrusted || entry.hasUntrusted
		total.priced = total.priced || entry.priced
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.total = total
	m.byModel = byModel
	m.byName = byName
	m.warned = snapshot.Warned
	return nil
}
