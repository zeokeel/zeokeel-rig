package membench

import (
	"fmt"
	"strings"
)

// FormatTable 把报告渲染成 Markdown（可直接贴进 docs/MEMORY.md）。
//
// 结构：数据集与索引规模 → easy 子集表 → hard 子集表 → 分层注入曲线 → 结论与说明。
// 所有数字都来自 Run 的真实计算，不做平滑；延迟与构建耗时是单机测量值。
func FormatTable(report Report) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "## membench（seed=%d）\n\n", report.Seed)

	writeDatasetTable(&builder, report)
	writeArmTable(&builder, "easy 集（查询带唯一实体名，验证结构化字段 + BM25 相对子串的排序收益）",
		report, func(arm Arm) Metrics { return arm.Easy })
	writeArmTable(&builder, "hard 集（只给 2-3 个描述性属性、无实体名，暴露纯词法检索的上限）",
		report, func(arm Arm) Metrics { return arm.Hard })
	writeLayerTable(&builder, report)
	writeNotes(&builder, report)
	return builder.String()
}

func writeDatasetTable(builder *strings.Builder, report Report) {
	builder.WriteString("### 数据集与索引\n\n")
	builder.WriteString("| 指标 | 值 |\n|---|---|\n")
	fmt.Fprintf(builder, "| 会话 × 轮 | %d × %d |\n", report.Dataset.Sessions, report.Dataset.TurnsPerSession)
	fmt.Fprintf(builder, "| 植入记忆（easy / hard） | %d / %d |\n", report.Dataset.Facts, report.Dataset.HardFacts)
	fmt.Fprintf(builder, "| 干扰句（easy 干扰 / hard 近重复双胞胎） | %d / %d |\n",
		report.Dataset.Distractors, report.Dataset.Twins)
	fmt.Fprintf(builder, "| 条目总数 | %d |\n", report.Index.Docs)
	fmt.Fprintf(builder, "| write bytes（四字段 UTF-8 合计） | %d |\n", report.Index.WriteBytes)
	fmt.Fprintf(builder, "| 向量（条数 × 维度） | %d × %d（%d bytes） |\n",
		report.Index.VectorCount, hashDimension, report.Index.VectorBytes)
	fmt.Fprintf(builder, "| 索引构建 baseline / bm25 / hybrid (ms) | %.3f / %.3f / %.3f |\n",
		report.Index.BaselineBuildMS, report.Index.BM25BuildMS, report.Index.HybridBuildMS)
	fmt.Fprintf(builder, "| 查询（easy 改写 / easy 意图 / hard 语义改写） | %d + %d + %d = %d |\n",
		report.Dataset.RewriteQueries, report.Dataset.IntentQueries,
		report.Dataset.SemanticQueries, report.Dataset.Queries)
	fmt.Fprintf(builder, "| 其中原句连续片段（easy / hard） | %d / %d |\n",
		report.Dataset.VerbatimQueries-report.Dataset.HardVerbatimQueries, report.Dataset.HardVerbatimQueries)
	builder.WriteString("\n")
}

func writeArmTable(builder *strings.Builder, title string, report Report, pick func(Arm) Metrics) {
	fmt.Fprintf(builder, "### %s\n\n", title)
	builder.WriteString("| arm | recall@5 | MRR | avg tokens | p50 (ms) | p95 (ms) | index build (ms) |\n")
	builder.WriteString("|---|---|---|---|---|---|---|\n")
	for _, arm := range report.Arms {
		metrics := pick(arm)
		fmt.Fprintf(builder, "| %s | %.3f | %.3f | %.1f | %.3f | %.3f | %.3f |\n",
			arm.Name, metrics.RecallAt5, metrics.MRR, metrics.AvgInjectedTokens,
			metrics.P50LatencyMS, metrics.P95LatencyMS, arm.BuildMS)
	}
	builder.WriteString("\n")
}

func writeLayerTable(builder *strings.Builder, report Report) {
	builder.WriteString("### 分层注入（同一份 hybrid 排名，只改注入层级）\n\n")
	builder.WriteString("| 子集 | 注入层级 | recall@5 | avg tokens |\n|---|---|---|---|\n")
	for _, point := range report.LayerCurve {
		fmt.Fprintf(builder, "| %s | %s | %.3f | %.1f |\n",
			point.Subset, point.Level, point.RecallAt5, point.AvgInjectedTokens)
	}
	builder.WriteString("\n")
}

func writeNotes(builder *strings.Builder, report Report) {
	builder.WriteString("### 结论与说明\n\n")
	builder.WriteString("- easy 集的差异来自**结构化字段 + BM25 排序**：同一批条目上，子串语义只能命中" +
		"原句连续片段，BM25 还能命中换序/同义的改写。\n")
	builder.WriteString("- hard 集的低分不是 bug，而是**纯词法没有语义**的上限：查询里没有实体名，" +
		"属性词被大量共享，近重复干扰句与 gold 的 L0/L1 文本逐字相同。要跨越这一步，" +
		"需要宿主提供 Embedder（真实语义向量）或 Reranker（交叉编码精排）。\n")
	for _, arm := range report.Arms {
		if arm.Note != "" {
			fmt.Fprintf(builder, "- %s：%s\n", arm.Name, arm.Note)
		}
	}
	for _, note := range report.Notes {
		fmt.Fprintf(builder, "- %s\n", note)
	}
}
