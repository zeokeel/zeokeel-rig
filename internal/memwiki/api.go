package memwiki

// Exported wrappers for the pieces a storage backend outside this package needs
// to implement the same rules (the SQLite memory store in session/sqlite). They
// exist so the semantics have one source: a second implementation that copied
// them would drift the first time a rule changed.

// NormalizeSummary validates and normalises a one-line summary.
func NormalizeSummary(summary string) (string, error) { return validateSummary(summary) }

// NormalizeTitle returns the display title, falling back to the name.
func NormalizeTitle(name, title string) string { return normalizeTitle(name, title) }

// AppendLog appends a dated entry to the page's Log section.
func AppendLog(content, text string) string { return appendLogLine(content, text) }

// RewriteLog appends the "rewritten" line a Write records when it replaces a
// page that already existed.
func RewriteLog(content string, previousBytes int) string {
	return appendLogLine(content, rewriteLogText(previousBytes))
}

// SortMetas orders page metadata the way Index and Search return it.
func SortMetas(metas []PageMeta) { sortMetas(metas) }
