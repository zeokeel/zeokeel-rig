// Package membench 是记忆检索的可复现基准：确定性数据集生成、对照组评测、报告输出。
//
// 数据集遵循 OpenViking 对齐设计（.audit/openviking/memory-v2-design.md §6）：
// 60 会话 × 30 轮，植入 240 条事实/偏好/决策/事件（每条一个唯一实体名 + 2-4 个主题词），
// 再插入 300 条共享主题词但实体不同的干扰句；480 条查询（一半关键词改写、一半意图问法），
// 每条标注 gold URI。
//
// 对照组：
//   - baseline-wiki-substring：旧 wiki 的等价语义（大小写不敏感 contains，
//     标题 > 摘要 > 正文排序，最多 20 条），用于给出"升级前"的基线；
//   - bm25：memsearch 倒排索引 + 字段加权 BM25；
//   - hybrid-hash-rrf：memsearch + 确定性 hash 词袋向量 + RRF 融合；
//   - hybrid-layer：同一份混合排名，但按命中层级注入（L0 命中只注入摘要）。
//
// 关于 hybrid 臂的诚实说明：它用的向量是 memsearch.HashVector（词袋哈希），只反映词项
// 重合，**不代表真实语义增益**——真实向量能力需要宿主提供 Embedder。这里验证的是管线
// （向量通道、RRF、层级注入）而不是语义质量。
//
// 全部随机性来自固定 seed 的 math/rand，数据集与指标可复现；只有延迟受机器负载影响。
package membench

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// 数据集规模（与设计文档 §6 一致）。
const (
	// Sessions 是会话数。
	Sessions = 60
	// TurnsPerSession 是每个会话的轮数。
	TurnsPerSession = 30
	// FactsPerSession 是每个会话植入的记忆条数。
	FactsPerSession = 4
	// PlantedFacts 是植入的记忆条数。
	PlantedFacts = Sessions * FactsPerSession
	// Distractors 是干扰句条数。
	Distractors = 300
	// QueriesPerFact 是每条植入记忆对应的查询数（关键词改写 + 意图问法）。
	QueriesPerFact = 2
	// HardFacts 是 hard 子集的植入记忆数（查询里不出现实体名）。
	HardFacts = 120
	// HardTwins 是 hard 子集每个 gold 配的"近重复干扰句"数：共享全部主题词与属性，
	// 只有实体名不同。
	HardTwins = HardFacts
	// HardVerbatimEvery 决定多少 hard 查询是"属性列表的连续片段"（子串检索能命中的形态）。
	HardVerbatimEvery = 3
)

// 查询/文档所属的子集。easy 集验证结构化字段 + BM25 相对子串的收益；
// hard 集只有描述性属性、没有实体名，用来暴露纯词法检索的上限。
const (
	// SubsetEasy 是带唯一实体名的 easy 子集。
	SubsetEasy = "easy"
	// SubsetHard 是只有描述性属性的 hard 子集。
	SubsetHard = "hard"
)

// QueryKind 是查询的构造方式。
type QueryKind string

const (
	// QueryRewrite 是原句关键词的改写（换序/同义）。
	QueryRewrite QueryKind = "rewrite"
	// QueryIntent 是意图问法（"关于 X 的偏好是什么"）。
	QueryIntent QueryKind = "intent"
	// QuerySemantic 是 hard 子集的语义改写（只给 2-3 个描述性属性）。
	QuerySemantic QueryKind = "semantic"
)

// Doc 是一条可检索条目。
type Doc struct {
	URI      memoryvfs.URI `json:"uri"`
	Session  string        `json:"session"`
	Turn     int           `json:"turn"`
	Category string        `json:"category"`
	Entity   string        `json:"entity"`
	Topics   []string      `json:"topics"`
	// Attributes 是 hard 子集用的描述性属性（如"支付对账"），easy 集为空。
	Attributes []string  `json:"attributes,omitempty"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary"`
	Overview   string    `json:"overview"`
	Content    string    `json:"content"`
	Updated    time.Time `json:"updated"`
	// Gold 报告这条是否是被查询瞄准的植入记忆（false = 干扰句）。
	Gold bool `json:"gold"`
	// TwinOf 是近重复干扰句对应的 gold URI（非 twin 为空）。
	TwinOf memoryvfs.URI `json:"twin_of,omitempty"`
	// Hard 报告这条属于 hard 子集的植入记忆。
	Hard bool `json:"hard,omitempty"`
	// Twin 报告这条是 hard gold 的近重复干扰句（主题词与属性全同、实体不同）。
	Twin bool `json:"twin,omitempty"`
}

// Query 是一条带 gold URI 的查询。
type Query struct {
	Text string        `json:"text"`
	Gold memoryvfs.URI `json:"gold"`
	Kind QueryKind     `json:"kind"`
	// Verbatim 表示这条查询是 gold 摘要的连续片段（旧 wiki 子串检索能命中的形态）。
	Verbatim bool `json:"verbatim,omitempty"`
	// Subset 是这条查询属于哪个子集（easy/hard）。
	Subset string `json:"subset"`
}

// Dataset 是生成出来的一整套数据。
type Dataset struct {
	Seed            int64   `json:"seed"`
	Sessions        int     `json:"sessions"`
	TurnsPerSession int     `json:"turns_per_session"`
	Docs            []Doc   `json:"docs"`
	Queries         []Query `json:"queries"`
}

var topicPool = []string{
	"部署", "缓存", "超时", "重试", "日志", "灰度", "回滚", "限流",
	"备份", "监控", "鉴权", "带宽", "索引", "分片", "队列", "调度",
}

// attributePool 是 hard 子集的描述性属性。池子越小，属性组合越容易撞车——
// 这是 hard 集"纯词法无法区分"的来源，而不是靠同义替换制造不可命中。
var attributePool = []string{
	"支付对账", "Go 服务", "夜间批处理", "风控告警", "日终结算",
	"灰度发布", "数据回填", "指标看板", "消息队列", "定时任务",
}

// entityWords 给实体名提供可读的词根；唯一性由追加的序号保证。
var entityWords = []string{
	"aster", "basalt", "cinder", "delta", "ember", "flux", "glacier", "harbor",
	"iris", "juniper", "kelp", "lumen", "mica", "nectar", "onyx", "prism",
	"quartz", "ridge", "slate", "topaz", "umber", "velvet", "willow", "xenon",
	"yarrow", "zephyr",
}

// categories 轮转植入的记忆类型（对应 memoryvfs 的内置类型）。
var categories = []string{
	memoryvfs.TypeEntities,
	memoryvfs.TypePreferences,
	memoryvfs.TypeCases,
	memoryvfs.TypeEvents,
}

// categoryLabels 是意图问法里用的中文类别词。
var categoryLabels = map[string]string{
	memoryvfs.TypeEntities:    "事实",
	memoryvfs.TypePreferences: "偏好",
	memoryvfs.TypeCases:       "决策",
	memoryvfs.TypeEvents:      "事件",
}

// synonyms 给关键词改写提供同义替换（替换后该词项不再与原文重合，制造难度）。
var synonyms = map[string]string{
	"部署": "上线",
	"缓存": "暂存",
	"超时": "逾时",
	"重试": "重跑",
	"日志": "记录",
	"灰度": "金丝雀",
	"回滚": "还原",
	"限流": "节流",
	"备份": "副本",
	"监控": "观测",
}

// intentTemplates 是意图问法模板；%s 依次是实体名与类别/主题词。
var intentTemplates = []string{
	"关于 %s 的%s是什么",
	"%s 的%s之前是怎么定的",
	"我们对 %s 的%s做过什么决定",
	"%s 现在的%s方案是什么",
	"%s 出过什么问题",
}

// Generate 用固定 seed 生成完整数据集。同一 seed 永远得到同一份数据。
func Generate(seed int64) Dataset {
	rng := rand.New(rand.NewSource(seed))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	docs := make([]Doc, 0, PlantedFacts+Distractors)

	for idx := 0; idx < PlantedFacts; idx++ {
		category := categories[idx%len(categories)]
		topics := pickTopics(rng, 3+rng.Intn(2))
		entity := fmt.Sprintf("%s-%03d", entityWords[idx%len(entityWords)], idx+1)
		doc := buildFact(
			entity,
			category,
			topics,
			idx/FactsPerSession,
			(idx%FactsPerSession)*(TurnsPerSession/FactsPerSession)+rng.Intn(7),
			base.Add(time.Duration(idx)*time.Minute),
		)
		docs = append(docs, doc)
	}

	for idx := 0; idx < Distractors; idx++ {
		topics := pickTopics(rng, 2+rng.Intn(3))
		entity := fmt.Sprintf("noise-%03d", idx+1)
		doc := buildDistractor(
			entity,
			topics,
			idx%Sessions,
			rng.Intn(TurnsPerSession),
			base.Add(time.Duration(PlantedFacts+idx)*time.Minute),
			idx%2,
		)
		docs = append(docs, doc)
	}

	// hard 子集：每条 gold 配一条近重复干扰句（主题词与属性全同、只有实体不同）。
	// 干扰句的 Updated 晚于 gold（模拟"最近写入的相似记录"），baseline 的
	// Updated 排序因此更容易被它们顶掉——这是刻意保留的真实处境。
	for idx := 0; idx < HardFacts; idx++ {
		topics := pickTopics(rng, 2+rng.Intn(3))
		attributes := pickAttributes(rng)
		entity := fmt.Sprintf("svc-%03d", idx+1)
		hardDoc := buildHardDoc(
			entity,
			topics,
			attributes,
			idx%Sessions,
			rng.Intn(TurnsPerSession),
			base.Add(time.Duration(PlantedFacts+Distractors+idx)*time.Minute),
			false,
		)
		docs = append(docs, hardDoc)
		twin := buildHardDoc(
			fmt.Sprintf("atwin-%03d", idx+1),
			topics,
			attributes,
			idx%Sessions,
			rng.Intn(TurnsPerSession),
			base.Add(time.Duration(PlantedFacts+Distractors+HardFacts+idx)*time.Minute),
			true,
		)
		twin.TwinOf = hardDoc.URI
		docs = append(docs, twin)
	}

	// easy 查询先生成（随机序列与 v1 保持一致），再追加 hard 查询。
	queries := buildQueries(rng, docs)
	queries = append(queries, buildHardQueries(rng, docs)...)

	return Dataset{
		Seed:            seed,
		Sessions:        Sessions,
		TurnsPerSession: TurnsPerSession,
		Docs:            docs,
		Queries:         queries,
	}
}

// pickAttributes 为一条 hard 记忆取 2-3 个描述性属性（不重复）。
func pickAttributes(rng *rand.Rand) []string {
	count := 2
	if rng.Intn(3) == 0 {
		count = 3
	}
	perm := rng.Perm(len(attributePool))
	out := make([]string, count)
	for idx := 0; idx < count; idx++ {
		out[idx] = attributePool[perm[idx]]
	}
	return out
}

// buildHardDoc 构造 hard 子集的条目：标题/摘要/概览只有属性与主题词，**没有**实体名，
// 实体只出现在正文里。twin=true 时生成近重复干扰句（其余文本与 gold 逐字相同）。
func buildHardDoc(entity string, topics, attributes []string, session, turn int, updated time.Time, twin bool) Doc {
	t1, t2 := topics[0], topics[1%len(topics)]
	list := strings.Join(attributes, "、")
	title := fmt.Sprintf("%s · %s", attributes[0], attributes[1%len(attributes)])
	summary := fmt.Sprintf("服务画像：%s，%s走%s。", list, t1, t2)
	overview := fmt.Sprintf("服务画像：%s。背景：%s 与 %s 已复盘，%s 排在下个窗口。",
		list, t1, t2, attributes[len(attributes)-1])
	content := fmt.Sprintf("%s 的服务画像：%s；%s 走 %s。补充：%s 与 %s 未变。",
		entity, list, attributes[len(attributes)-1], t1, t2, t1)
	uri, err := memoryvfs.Join("user", "self", memoryvfs.TypeEntities, entity)
	if err != nil {
		panic(fmt.Sprintf("membench: invalid hard URI for %q: %v", entity, err))
	}
	return Doc{
		URI:        uri,
		Session:    fmt.Sprintf("session-%02d", session+1),
		Turn:       turn,
		Category:   memoryvfs.TypeEntities,
		Entity:     entity,
		Topics:     topics,
		Attributes: attributes,
		Title:      title,
		Summary:    summary,
		Overview:   overview,
		Content:    content,
		Updated:    updated,
		Gold:       !twin,
		Hard:       true,
		Twin:       twin,
	}
}

// hardFrames 是 hard 查询的问法框架（只有一个 %s，填入打乱顺序的属性列表）。
var hardFrames = []string{
	"负责 %s 的服务是哪个",
	"哪个服务同时做 %s",
	"我们要找的是 %s 的服务",
	"%s 这套能力归谁负责",
}

// buildHardQueries 为每条 hard gold 生成一条语义改写查询：只给属性、不给实体名。
func buildHardQueries(rng *rand.Rand, docs []Doc) []Query {
	queries := make([]Query, 0, HardFacts)
	index := 0
	for _, doc := range docs {
		if !doc.Hard || !doc.Gold {
			continue
		}
		verbatim := index%HardVerbatimEvery == 0
		// 查询只回忆 2 个属性（文档可能有第 3 个）：属性对在池子里必然复用，
		// 这就是"词法分不清 gold 与近重复句"的来源。
		recalled := doc.Attributes
		text := ""
		if verbatim {
			// 属性列表本身（摘要的连续片段）：子串检索唯一可能命中的形态。
			text = strings.Join(recalled, "、")
		} else {
			list := append([]string(nil), recalled...)
			rng.Shuffle(len(list), func(a, b int) { list[a], list[b] = list[b], list[a] })
			text = fmt.Sprintf(hardFrames[rng.Intn(len(hardFrames))], strings.Join(list, "、"))
		}
		queries = append(queries, Query{
			Text:     text,
			Gold:     doc.URI,
			Kind:     QuerySemantic,
			Verbatim: verbatim,
			Subset:   SubsetHard,
		})
		index++
	}
	return queries
}

// pickTopics 从主题池里取 count 个不重复主题（确定性）。
func pickTopics(rng *rand.Rand, count int) []string {
	if count > len(topicPool) {
		count = len(topicPool)
	}
	perm := rng.Perm(len(topicPool))
	out := make([]string, count)
	for idx := 0; idx < count; idx++ {
		out[idx] = topicPool[perm[idx]]
	}
	return out
}

// buildFact 构造一条植入记忆（L0/L1/L2 齐全，标题带实体名）。
func buildFact(entity, category string, topics []string, session, turn int, updated time.Time) Doc {
	t1, t2, t3 := topics[0], topics[1], topics[2%len(topics)]
	summary := ""
	switch category {
	case memoryvfs.TypeEntities:
		summary = fmt.Sprintf("%s 的事实：%s 依赖 %s，通过 %s 通道同步。", entity, t1, t2, t3)
	case memoryvfs.TypePreferences:
		summary = fmt.Sprintf("用户偏好：%s 在 %s 上优先 %s，不要改动 %s。", entity, t1, t2, t3)
	case memoryvfs.TypeCases:
		summary = fmt.Sprintf("已决策：%s 的 %s 改为 %s，%s 保留旧值。", entity, t1, t2, t3)
	default:
		summary = fmt.Sprintf("事件记录：%s 的 %s 在 %s 之后超时，%s 已回滚。", entity, t1, t2, t3)
	}
	title := fmt.Sprintf("%s · %s%s", entity, t1, t2)
	overview := fmt.Sprintf("%s 背景：%s 与 %s 的取舍已经复盘，%s 保持现状。", summary, t1, t2, t3)
	content := fmt.Sprintf(
		"%s\n%s\n补充细节：%s 的%s变更由值班同学发起，评估了 %s 与 %s 两条路径，最终保留 %s，并把结论写进%s复盘。",
		title, summary, entity, t1, t2, t3, t1, t2,
	)
	uri, err := memoryvfs.Join("user", "self", category, entity)
	if err != nil {
		panic(fmt.Sprintf("membench: invalid planted URI for %q: %v", entity, err))
	}
	return Doc{
		URI:      uri,
		Session:  fmt.Sprintf("session-%02d", session+1),
		Turn:     turn,
		Category: category,
		Entity:   entity,
		Topics:   topics,
		Title:    title,
		Summary:  summary,
		Overview: overview,
		Content:  content,
		Updated:  updated,
		Gold:     true,
	}
}

// buildDistractor 构造一条干扰句：实体不同、主题词与植入记忆共享。
func buildDistractor(entity string, topics []string, session, turn int, updated time.Time, variant int) Doc {
	t1, t2 := topics[0], topics[1%len(topics)]
	summary := ""
	if variant == 0 {
		summary = fmt.Sprintf("%s 的巡检记录：%s 与 %s 在试运行中出现偏差。", entity, t1, t2)
	} else {
		summary = fmt.Sprintf("%s 的备忘：%s 需要复核，%s 暂缓上线。", entity, t1, t2)
	}
	title := fmt.Sprintf("%s · %s", entity, t1)
	overview := fmt.Sprintf("%s 背景：%s 的窗口排在%s之后，%s 另议。", summary, t1, t2, t1)
	content := fmt.Sprintf("%s\n%s\n补充细节：%s 的%s仍在观察，%s 未做调整。", title, summary, entity, t2, t1)
	uri, err := memoryvfs.Join("user", "self", memoryvfs.TypeEntities, entity)
	if err != nil {
		panic(fmt.Sprintf("membench: invalid distractor URI for %q: %v", entity, err))
	}
	return Doc{
		URI:      uri,
		Session:  fmt.Sprintf("session-%02d", session+1),
		Turn:     turn,
		Category: memoryvfs.TypeEntities,
		Entity:   entity,
		Topics:   topics,
		Title:    title,
		Summary:  summary,
		Overview: overview,
		Content:  content,
		Updated:  updated,
		Gold:     false,
	}
}

// buildQueries 为每条植入记忆生成 2 条查询：关键词改写 + 意图问法。
//
// 一半的关键词查询是"原句连续片段"——这是旧 wiki 子串检索唯一能命中的形态，
// 保留它是为了让 baseline 有一个真实（而不是恒为 0）的召回率。
func buildQueries(rng *rand.Rand, docs []Doc) []Query {
	var gold []Doc
	for _, doc := range docs {
		if doc.Gold && !doc.Hard {
			gold = append(gold, doc)
		}
	}
	queries := make([]Query, 0, len(gold)*QueriesPerFact)
	for idx, doc := range gold {
		verbatim := idx%2 == 0
		queries = append(queries, Query{
			Text:     rewriteText(rng, doc, verbatim),
			Gold:     doc.URI,
			Kind:     QueryRewrite,
			Verbatim: verbatim,
			Subset:   SubsetEasy,
		})
	}
	for _, doc := range gold {
		queries = append(queries, Query{
			Text:   intentText(rng, doc),
			Gold:   doc.URI,
			Kind:   QueryIntent,
			Subset: SubsetEasy,
		})
	}
	return queries
}

// rewriteText 生成关键词改写查询：实体 + 主题词换序，并按 seed 做同义替换。
// verbatim=true 时返回原句的一个连续片段（子串检索可命中）。
func rewriteText(rng *rand.Rand, doc Doc, verbatim bool) string {
	if verbatim {
		return verbatimChunk(doc.Summary, 0.65)
	}
	words := make([]string, 0, 1+len(doc.Topics))
	words = append(words, doc.Entity)
	words = append(words, doc.Topics...)
	rng.Shuffle(len(words), func(a, b int) { words[a], words[b] = words[b], words[a] })
	for idx := range words {
		if replacement, ok := synonyms[words[idx]]; ok && rng.Intn(2) == 0 {
			words[idx] = replacement
		}
	}
	return strings.Join(words, " ")
}

// verbatimChunk 取 text 的前 ratio 比例（rune 边界），保证结果是原文的连续子串。
func verbatimChunk(text string, ratio float64) string {
	runes := []rune(text)
	cut := int(float64(len(runes)) * ratio)
	if cut < 8 {
		cut = len(runes)
	}
	if cut > len(runes) {
		cut = len(runes)
	}
	return string(runes[:cut])
}

// intentText 生成意图问法查询。
func intentText(rng *rand.Rand, doc Doc) string {
	template := intentTemplates[rng.Intn(len(intentTemplates))]
	if strings.Count(template, "%s") == 1 {
		return fmt.Sprintf(template, doc.Entity)
	}
	context := categoryLabels[doc.Category]
	if context == "" {
		context = doc.Topics[0]
	}
	return fmt.Sprintf(template, doc.Entity, context)
}
