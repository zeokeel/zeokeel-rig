package memwiki

// 给"本包之外的存储后端"使用的规则导出。
//
// 存在的原因只有一个：语义必须只有一份来源。第二个实现（目前是 session/sqlite
// 的 memory 后端）如果自己抄一遍校验与追加规则，第一次改规则时两个后端就会开始
// 漂移——而模型看到的是"同样的操作在文件后端能成、在数据库后端报错"这种最难解释
// 的差异。

// NormalizeSummary 校验并规整一句话摘要（去首尾空白、折叠换行）。
func NormalizeSummary(summary string) (string, error) { return validateSummary(summary) }

// NormalizeTitle 返回展示用标题；空则回退到页名。
func NormalizeTitle(name, title string) string { return normalizeTitle(name, title) }

// AppendLog 把一条带日期的条目追加到正文的 Log 段。
func AppendLog(content, text string) string { return appendLogLine(content, text) }

// RewriteLog 追加 Write 覆盖已有页时那一行 `rewritten (was N bytes)`。
func RewriteLog(content string, previousBytes int) string {
	return appendLogLine(content, rewriteLogText(previousBytes))
}

// SortMetas 按 Index/Search 要求的顺序排序元数据。
func SortMetas(metas []PageMeta) { sortMetas(metas) }
