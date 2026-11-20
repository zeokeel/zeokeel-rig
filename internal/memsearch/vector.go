package memsearch

import "math"

// HashVector 用确定性哈希把文本的词项投到固定维度上（词袋 + 桶计数）。
//
// 这是**管线演示与测试**用的向量：它只反映词项重合，不具备真实语义能力。宿主有
// Embedder 时应通过 Query.Vector / SetVector 传入真实向量。
// 同一文本 + 同一维度永远得到同一向量；dimension<=0 时返回 nil。
func HashVector(text string, dimension int) []float32 {
	if dimension <= 0 {
		return nil
	}
	vector := make([]float32, dimension)
	for _, token := range Tokenize(text) {
		hash := uint64(1469598103934665603)
		for idx := 0; idx < len(token); idx++ {
			hash ^= uint64(token[idx])
			hash *= 1099511628211
		}
		vector[hash%uint64(dimension)]++
	}
	return vector
}

// cosine 返回两个向量的余弦相似度；维度不符或任一为零向量时返回 0。
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for idx := range a {
		dot += float64(a[idx]) * float64(b[idx])
		normA += float64(a[idx]) * float64(a[idx])
		normB += float64(b[idx]) * float64(b[idx])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
