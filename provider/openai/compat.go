package openai

// 本文件是**共享实现的本地别名**：协议本体（请求体构造、SSE 分帧/终止判定/用量、
// 重试与退避）都在 provider/local，这里只保留 openai 侧需要的薄封装——内部单测、
// fuzz 与基准通过它们驱动同一份实现，因此"两份同协议实现"不再存在。
//
// 刻意不在这里放任何协议逻辑：任何一条规则要改，只改 provider/local。

import (
	"context"
	"net/http"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/local"
)

// parsedRetryAfter 是 local.ParseRetryAfter 的薄封装（历史内部调用点）。
func parseRetryAfter(value string) (time.Duration, bool) {
	return local.ParseRetryAfter(value)
}

// retryDelay 委托给共享传输层：Retry-After 优先、指数退避、上限 maxRetryDelay。
func (p *Provider) retryDelay(attempt int, retryAfter string) time.Duration {
	return p.transport.RetryDelay(attempt, retryAfter)
}

// doneData 是 SSE 终止载荷（与共享解码器同一个常量）。
const doneData = local.DoneData

// sseData 是 SSE 行分类器的薄封装（fuzz 与回归用例用）。
func sseData(line string) (string, bool) { return local.SSEData(line) }

// wireContent 是消息内容映射的薄封装（基准用）。
func wireContent(message rig.Message) any { return local.WireContent(message) }

// sseStream 是严格解码器的**测试入口**：只接收一条 data 载荷并累计排队的分片。
//
// 生产路径不经过它——Provider.Stream 直接把响应体交给 local.NewStream。保留它是
// 为了 fuzz 能对"任意载荷"驱动与生产完全相同的严格解码逻辑（local.DecodeStrictEvent）。
type sseStream struct {
	pending []rig.Chunk
}

// handleEvent 解码一条 data 载荷；带内 error 对象返回错误（与生产路径一致）。
func (s *sseStream) handleEvent(payload string) error {
	chunks, _, err := local.DecodeStrictEvent(payload)
	if err != nil {
		return err
	}
	s.pending = append(s.pending, chunks...)
	return nil
}

// newSSEStream 包装一个已建立的 SSE 响应（fuzz 与回归用例用）：严格档案 + openai
// 自己的"提前 EOF"哨兵，所以错误文案仍是 openai: 前缀。
func newSSEStream(ctx context.Context, resp *http.Response, requireComplete bool) rig.Stream {
	return local.NewStream(ctx, local.StreamOptions{
		Name: "openai",
		Body: resp.Body,
		Quirks: local.Quirks{
			RequireDone:         requireComplete,
			StrictFinishReasons: true,
			StrictDeltas:        true,
		},
		IncompleteStream: ErrIncompleteStream,
	})
}
