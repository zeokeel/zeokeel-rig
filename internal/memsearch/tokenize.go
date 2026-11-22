package memsearch

import (
	"strings"
	"unicode"
)

// separatorChars 是代码标识符里被视为"分词边界"的字符。它们仍然保留在整体词项里，
// 因此 `user_name` 既能被 `user_name` 精确命中，也能被 `name` 召回。
const separatorChars = "_-."

// Tokenize 把文本切成小写词项，查询与索引必须使用同一个函数：
//
//   - 英文/数字按连续段小写化（`Hello` → `hello`）；
//   - 代码标识符按 `_`/`-`/`.` 切开，**同时**保留整体（`user_name.v2` →
//     `user_name.v2`、`user`、`name`、`v2`）；
//   - CJK（汉/日文假名/韩文）连续段按二元组切（`部署策略` → `部署`、`署策`、`策略`），
//     单字段保留单字；
//   - 纯标点不产生词项，也不会产生空词项。
//
// 返回值保留重复项（BM25 需要词频）。确定性：同一输入永远返回同一序列。
func Tokenize(text string) []string {
	if text == "" {
		return nil
	}
	lower := strings.ToLower(text)
	var tokens []string
	var ident strings.Builder
	var cjk []rune

	flushIdent := func() {
		if ident.Len() == 0 {
			return
		}
		raw := strings.Trim(ident.String(), separatorChars)
		ident.Reset()
		if raw == "" {
			return
		}
		if !strings.ContainsAny(raw, separatorChars) {
			tokens = append(tokens, raw)
			return
		}
		tokens = append(tokens, raw)
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
			return strings.ContainsRune(separatorChars, r)
		}) {
			if part != "" {
				tokens = append(tokens, part)
			}
		}
	}

	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		if len(cjk) == 1 {
			tokens = append(tokens, string(cjk[0]))
		} else {
			for idx := 0; idx+1 < len(cjk); idx++ {
				tokens = append(tokens, string(cjk[idx:idx+2]))
			}
		}
		cjk = cjk[:0]
	}

	for _, r := range lower {
		switch {
		case isAlnum(r) || strings.ContainsRune(separatorChars, r):
			flushCJK()
			ident.WriteRune(r)
		case isCJK(r):
			flushIdent()
			cjk = append(cjk, r)
		default:
			flushIdent()
			flushCJK()
		}
	}
	flushIdent()
	flushCJK()
	if len(tokens) == 0 {
		return nil
	}
	return tokens
}

func isAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
		unicode.IsLetter(r) && unicode.IsLower(r) && !isCJK(r)
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}
