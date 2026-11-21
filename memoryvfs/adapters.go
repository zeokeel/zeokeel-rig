package memoryvfs

import "context"

// ExtractorFunc 让宿主的 LLM 抽取器用一个函数实现 Extractor，不必定义类型。
//
// 典型用法（写入侧 LLM wiki）：
//
//	store, _ := memvfs.Open(dir, memoryvfs.WithExtractor(
//	    memoryvfs.ExtractorFunc(func(ctx context.Context, s memoryvfs.Session) ([]memoryvfs.Candidate, error) {
//	        return llm.ExtractMemories(ctx, s)
//	    }),
//	))
//
// 读取侧仍走 BM25；不注入 Embedder 时不会产生向量依赖。
type ExtractorFunc func(ctx context.Context, session Session) ([]Candidate, error)

func (f ExtractorFunc) Extract(ctx context.Context, session Session) ([]Candidate, error) {
	if f == nil {
		return nil, nil
	}
	return f(ctx, session)
}

// SummarizerFunc 让宿主的 LLM 摘要器用一个函数实现 Summarizer。
// 返回 (summary, overview, error)；空串会回退到确定性摘要。
type SummarizerFunc func(ctx context.Context, entry Entry) (summary, overview string, err error)

func (f SummarizerFunc) Summarize(ctx context.Context, entry Entry) (string, string, error) {
	if f == nil {
		return "", "", nil
	}
	return f(ctx, entry)
}
