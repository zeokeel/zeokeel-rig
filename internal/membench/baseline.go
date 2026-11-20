package membench

import (
	"sort"
	"strings"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// MaxSearchResults 是 baseline 的返回条数上限（与 internal/memwiki 一致）。
const MaxSearchResults = 20

// wikiSubstringIndex 是旧 wiki `Wiki.Search` 语义的等价实现：
//
//   - 查询与字段都做小写化后做**子串**匹配（不分词、不做同义、不看词序）；
//   - 标题命中 > 摘要命中 > 正文命中；同档按 Updated 降序、URI 升序；
//   - 最多返回 MaxSearchResults(20) 条。
//
// 刻意不 import internal/memwiki 或 tools/memory：membench 要能独立运行，
// 也避免基准与实现互相牵引。
type wikiSubstringIndex struct {
	docs []Doc
	// lowered 是构造期预存的小写字段：语义与"每次搜索都 ToLower"完全一致，
	// 只是省掉重复分配（真实 FileWiki 每次还要从磁盘读页面，见 arm note）。
	lowered []loweredDoc
}

type loweredDoc struct {
	title   string
	summary string
	content string
}

func newWikiSubstringIndex(docs []Doc) *wikiSubstringIndex {
	copied := append([]Doc(nil), docs...)
	lowered := make([]loweredDoc, len(copied))
	for idx, doc := range copied {
		lowered[idx] = loweredDoc{
			title:   strings.ToLower(doc.Title),
			summary: strings.ToLower(doc.Summary),
			content: strings.ToLower(doc.Content),
		}
	}
	return &wikiSubstringIndex{docs: copied, lowered: lowered}
}

// rankedHit 是各对照组统一的输出形态。
type rankedHit struct {
	URI   memoryvfs.URI
	Level memoryvfs.Level
}

// search 返回按旧 wiki 语义排好序的命中。
func (idx *wikiSubstringIndex) search(query string) []rankedHit {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return nil
	}
	type scored struct {
		doc   Doc
		rank  int
		level memoryvfs.Level
	}
	matches := make([]scored, 0, len(idx.docs))
	for index, doc := range idx.docs {
		fields := idx.lowered[index]
		var rank int
		var level memoryvfs.Level
		switch {
		case strings.Contains(fields.title, needle):
			rank, level = 0, memoryvfs.Level0
		case strings.Contains(fields.summary, needle):
			rank, level = 1, memoryvfs.Level0
		case strings.Contains(fields.content, needle):
			rank, level = 2, memoryvfs.Level2
		default:
			continue
		}
		matches = append(matches, scored{doc: doc, rank: rank, level: level})
	}
	sort.SliceStable(matches, func(a, b int) bool {
		if matches[a].rank != matches[b].rank {
			return matches[a].rank < matches[b].rank
		}
		if !matches[a].doc.Updated.Equal(matches[b].doc.Updated) {
			return matches[a].doc.Updated.After(matches[b].doc.Updated)
		}
		return matches[a].doc.URI < matches[b].doc.URI
	})
	if len(matches) > MaxSearchResults {
		matches = matches[:MaxSearchResults]
	}
	out := make([]rankedHit, 0, len(matches))
	for _, match := range matches {
		out = append(out, rankedHit{URI: match.doc.URI, Level: match.level})
	}
	return out
}
