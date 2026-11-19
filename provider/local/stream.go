package local

// 本文件是 SSE 流解码：把 {BaseURL}{ChatPath} 的流式响应变成 rig.Chunk 序列。
//
// 与 provider/openai 的差别都在"容忍度"上（见 Quirks）：本地服务可能不发 [DONE]、
// 可能把 tool_calls 的 arguments 发成对象、可能用 function_call 旧字段、也可能把
// content 发成分片数组。这些差异在这里被吸收，agent 侧看到的永远是干净的 rig.Chunk。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// maxSSELine 限制单条 SSE 行；工具调用参数片段可能很大。
	maxSSELine = 4 << 20
	// DoneData 是 OpenAI 流式 API 的终止载荷。
	DoneData = "[DONE]"
	// toolCallIndexUnknown 表示这条 delta 没有携带 index，需要在流级别按 id/name 归并。
	// 它**不是**合法 index（合法值 >= 0），解码器用它把"缺失"与"0"区分开。
	toolCallIndexUnknown = -1
)

// toolCallIndexer 在**流级别**给"没有 index 的工具调用片段"分配稳定槽位。
//
// 本地服务常见两种形态：①有 index（标准 OpenAI）；②完全没有 index，只能靠 id 或
// name 区分（逐事件到达的并行调用）。旧实现用"事件内位置"补 index，于是每个事件都
// 落到 0：id/name 互相覆盖、arguments 首尾相接成非法 JSON。规则：
//
//  1. 有 index → 以 index 为准，并记下 id/name 的映射；
//  2. 没有 index 但有 id → 同一 id 归并到同一槽位，新 id 分配新槽位；
//  3. 没有 index 也没有 id 但有 name → 按 name 归并（同名视为同一次调用）；
//  4. 三者都没有 → 只有在恰好一个槽位时才能唯一归属，否则报错（绝不静默拼接）。
type toolCallIndexer struct {
	byID    map[string]int
	byName  map[string]int
	highest int
	// slots 是"已经存在的调用槽位"的集合（而不是一个自增计数器）：一次调用可能
	// 先后带来 index、id、name，用计数器会把同一次调用数成多次（评审 review-1 M1）。
	slots map[int]struct{}
}

func newToolCallIndexer() *toolCallIndexer {
	return &toolCallIndexer{byID: map[string]int{}, byName: map[string]int{}, highest: -1,
		slots: map[int]struct{}{}}
}

// assign 就地改写 delta.Index；无法归属时返回错误。
func (x *toolCallIndexer) assign(delta *rig.ToolCallDelta) error {
	if delta.Index >= 0 {
		x.record(delta.ID, delta.Name, delta.Index)
		return nil
	}
	if delta.ID != "" {
		if index, ok := x.byID[delta.ID]; ok {
			delta.Index = index
			return nil
		}
		delta.Index = x.allocate()
		x.record(delta.ID, delta.Name, delta.Index)
		return nil
	}
	if delta.Name != "" {
		if index, ok := x.byName[delta.Name]; ok {
			delta.Index = index
			return nil
		}
		delta.Index = x.allocate()
		x.record("", delta.Name, delta.Index)
		return nil
	}
	switch len(x.slots) {
	case 0:
		// 流里还没有任何槽位：这一段只能是第一个（也是唯一一个）调用的开头，
		// 直接开一个槽位（旧实现的"事件内位置"等价行为，且不猜错）。
		delta.Index = x.allocate()
		return nil
	case 1:
		for index := range x.slots {
			delta.Index = index
			return nil
		}
	}
	return fmt.Errorf("streaming tool_calls delta carries no index, id or function name and %d calls are open; refusing to guess which call it extends", len(x.slots))
}

func (x *toolCallIndexer) allocate() int {
	x.highest++
	x.slots[x.highest] = struct{}{}
	return x.highest
}

// record 登记一次 delta 的归属：index 决定槽位是否存在，id/name 只是到槽位的别名。
// 同一次调用多次到达（index/id/name 分散在不同片段）都只算一个槽位。
func (x *toolCallIndexer) record(id, name string, index int) {
	if index < 0 {
		return
	}
	x.slots[index] = struct{}{}
	if index > x.highest {
		x.highest = index
	}
	if id != "" {
		x.byID[id] = index
	}
	if name != "" {
		x.byName[name] = index
	}
}

// stream 是 rig.Stream 的实现：SSE 分帧 + 终止判定 + 分片排队。**协议实现只有
// 这一份**——provider/openai（严格）与本地 provider（宽松）都走它，差别只在
// eventDecoder 与 quirks。
type stream struct {
	ctx     context.Context
	body    io.ReadCloser
	scanner *bufio.Scanner
	name    string
	quirks  quirks
	decoder eventDecoder
	// incompleteErr 是"提前 EOF"要包装的哨兵（provider/openai 用自己的）。
	incompleteErr error

	pending      []rig.Chunk
	finishReason string
	streamDone   bool
	closed       bool

	sawFinishReason bool
	sawTerminator   bool
	// toolIdx 是流级别的工具调用槽位分配器（见 toolCallIndexer）。
	toolIdx *toolCallIndexer
}

var _ rig.Stream = (*stream)(nil)

// StreamOptions 配置共享的 SSE 解码器。
type StreamOptions struct {
	// Name 是错误前缀（"local"、"openai"、"ollama"…）。
	Name string
	// Body 是已建立的 SSE 响应体；所有权转移给返回的流。
	Body io.ReadCloser
	// Quirks 决定容忍度；Strict* 开关用于托管服务。
	Quirks Quirks
	// IncompleteStream 是"提前 EOF"要包装的哨兵；nil 时用 ErrIncompleteStream。
	IncompleteStream error
}

// NewStream 用共享解码器包装一个已建立的 SSE 响应。最后一个分片之后 Recv 返回
// io.EOF 并释放响应体。
//
// provider/openai 与本地 provider 都走这里：同一份分帧/终止/用量/关闭语义，
// 靠 StreamOptions.Quirks 表达"严格 vs 宽松"。
func NewStream(ctx context.Context, opts StreamOptions) rig.Stream {
	return newStreamWith(ctx, opts.Body, opts.Name, normalizeQuirks(opts.Quirks), opts.IncompleteStream)
}

// newStreamWith 用已归一化的内部 quirks 构造流（本地 provider 自己的入口）。
func newStreamWith(ctx context.Context, body io.ReadCloser, name string, q quirks, incompleteErr error) *stream {
	if ctx == nil {
		ctx = context.Background()
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultName
	}
	s := &stream{
		ctx:           ctx,
		body:          body,
		scanner:       bufio.NewScanner(body),
		name:          name,
		quirks:        q,
		incompleteErr: incompleteErr,
	}
	s.scanner.Buffer(make([]byte, 0, 64*1024), maxSSELine)
	if q.strictDeltas {
		s.decoder = strictDecoder{}
	} else {
		s.decoder = flexDecoder{quirks: q}
	}
	if s.incompleteErr == nil {
		s.incompleteErr = ErrIncompleteStream
	}
	return s
}

// normalizeQuirks 把对外 Quirks 归一化成内部形态（合并内置 finish_reason 别名表）。
func normalizeQuirks(cfg Quirks) quirks {
	q := quirks{
		requireDone:         cfg.RequireDone,
		disableTools:        cfg.DisableTools,
		disableStreamOption: cfg.DisableStreamOptions,
		finishReasonAliases: mergeAliases(defaultFinishReasonAliases, cfg.FinishReasonAliases),
		reasoningKeys:       append([]string(nil), cfg.ReasoningKeys...),
		strictFinishReasons: cfg.StrictFinishReasons,
		strictDeltas:        cfg.StrictDeltas,
	}
	if cfg.StrictFinishReasons {
		// 托管服务不归一化：既不套内置别名，也不套用户自定义（finish_reason 原样透传）。
		q.finishReasonAliases = nil
	}
	return q
}

// Recv 实现 rig.Stream。
func (s *stream) Recv() (rig.Chunk, error) {
	for {
		if err := s.ctx.Err(); err != nil {
			s.close()
			return rig.Chunk{}, canceledError(s.name, err)
		}
		if len(s.pending) > 0 {
			chunk := s.pending[0]
			s.pending = s.pending[1:]
			return chunk, nil
		}
		if s.streamDone {
			if s.finishReason != "" {
				chunk := rig.Chunk{FinishReason: s.finishReason}
				s.finishReason = ""
				return chunk, nil
			}
			s.close()
			return rig.Chunk{}, io.EOF
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				s.close()
				if ctxErr := s.ctx.Err(); ctxErr != nil {
					return rig.Chunk{}, canceledError(s.name, ctxErr)
				}
				return rig.Chunk{}, fmt.Errorf("%s: read stream: %w", s.name, err)
			}
			// 本地服务常常直接关连接而不发 [DONE]：默认容忍（Quirks.RequireDone
			// 打开时才把"既无 finish_reason 也无 [DONE]"当成截断）。
			if s.quirks.requireDone && !s.sawFinishReason && !s.sawTerminator {
				s.close()
				return rig.Chunk{}, fmt.Errorf("%w: the connection closed before finish_reason or [DONE]; the response may be truncated", s.incompleteErr)
			}
			s.streamDone = true
			continue
		}
		payload, ok := SSEData(s.scanner.Text())
		if !ok {
			continue
		}
		if payload == DoneData {
			s.sawTerminator = true
			s.streamDone = true
			continue
		}
		if err := s.handleEvent(payload); err != nil {
			s.close()
			return rig.Chunk{}, err
		}
	}
}

// Close 释放响应体（Recv 在 EOF/错误时也会调用）。
func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.body.Close()
}

func (s *stream) close() { _ = s.Close() }

// SSEData 提取一条 SSE `data:` 行的载荷；空行、注释、非 data 字段与损坏行返回
// false，因此一个坏事件永远不会中断整个响应。
//
// 导出以便 provider/openai 的内部 fuzz/回归入口复用同一套行分类。
func SSEData(line string) (string, bool) {
	line = strings.TrimSuffix(line, "\r")
	if line == "" || strings.HasPrefix(line, ":") {
		return "", false
	}
	field, value, found := strings.Cut(line, ":")
	if !found || field != "data" {
		return "", false
	}
	value = strings.TrimPrefix(value, " ")
	if strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

// handleEvent 解码一个 data 载荷并把分片排队。坏 JSON 会被跳过（容忍），带内 error
// 对象会变成 Recv 的错误（本地服务用它在流中报错）。
func (s *stream) handleEvent(payload string) error {
	chunks, finishReason, hasFinish, err := s.decoder.decode(payload)
	if err != nil {
		return fmt.Errorf("%s: stream error: %s", s.name, err.Error())
	}
	// 没有 index 的片段在**流级别**归并（按 id → name → 唯一槽位），绝不按事件内位置猜。
	if s.toolIdx == nil {
		s.toolIdx = newToolCallIndexer()
	}
	for chunkIndex := range chunks {
		for deltaIndex := range chunks[chunkIndex].ToolCalls {
			if err := s.toolIdx.assign(&chunks[chunkIndex].ToolCalls[deltaIndex]); err != nil {
				return fmt.Errorf("%s: %w", s.name, err)
			}
		}
	}
	s.pending = append(s.pending, chunks...)
	if hasFinish {
		s.finishReason = finishReason
		s.sawFinishReason = true
	}
	return nil
}

// eventDecoder 把一条 data 载荷解码成分片。两种实现只差"容忍度"：
// flexDecoder（本地服务）与 strictDecoder（托管服务）。
type eventDecoder interface {
	// decode 返回要排队的分片、结束原因（hasFinish 为 true 时有效）。
	// err 非 nil 表示带内 error 对象（会变成 Recv 的错误）。
	decode(payload string) (chunks []rig.Chunk, finishReason string, hasFinish bool, err error)
}

// flexDecoder 是本地服务的宽松解码：容忍 message 回退、旧式 function_call、
// 对象型 arguments 与 finish_reason 变体。
type flexDecoder struct{ quirks quirks }

func (d flexDecoder) decode(payload string) ([]rig.Chunk, string, bool, error) {
	var event streamEvent
	if err := unmarshalEvent(payload, &event); err != nil {
		return nil, "", false, nil
	}
	if event.Error != nil {
		return nil, "", false, fmt.Errorf("%s", event.Error.text())
	}
	var chunks []rig.Chunk
	finishReason := ""
	hasFinish := false
	for _, choice := range event.Choices {
		delta := choice.Delta
		if delta.isEmpty() && choice.Message != nil {
			// 少数服务在流里发 message 而不是 delta（把整条消息放在最后一个事件）。
			delta = *choice.Message
		}
		chunk := rig.Chunk{
			Delta:     delta.Content.text(),
			Reasoning: d.reasoningOf(delta),
		}
		for _, call := range delta.ToolCalls {
			// index 缺失时交给流级别归并（按 id/name），不按事件内位置猜。
			index := toolCallIndexUnknown
			if call.Index != nil {
				index = *call.Index
			}
			chunk.ToolCalls = append(chunk.ToolCalls, rig.ToolCallDelta{
				Index:     index,
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments.value,
			})
		}
		// 旧式 function_call（单数、没有 index）：同样交给流级别归并。
		if call := delta.FunctionCall; call != nil {
			chunk.ToolCalls = append(chunk.ToolCalls, rig.ToolCallDelta{
				Index:     toolCallIndexUnknown,
				ID:        call.ID,
				Name:      call.Name,
				Arguments: call.Arguments.value,
			})
		}
		if chunk.Delta != "" || chunk.Reasoning != "" || len(chunk.ToolCalls) > 0 {
			chunks = append(chunks, chunk)
		}
		if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
			finishReason = d.normalizeFinishReason(*choice.FinishReason)
			hasFinish = true
		}
	}
	if event.Usage != nil {
		chunks = append(chunks, rig.Chunk{Usage: usageFromWire(event.Usage)})
	}
	return chunks, finishReason, hasFinish, nil
}

// reasoningOf 依次尝试 reasoning_content / reasoning / 配置的额外键名。
func (d flexDecoder) reasoningOf(delta streamDelta) string {
	if text := delta.ReasoningContent.text(); text != "" {
		return text
	}
	if text := delta.Reasoning.text(); text != "" {
		return text
	}
	for _, key := range d.quirks.reasoningKeys {
		if text := delta.Extra[key].text(); text != "" {
			return text
		}
	}
	return ""
}

// normalizeFinishReason 把服务端变体归一化成 stop / length / tool_calls。
func (d flexDecoder) normalizeFinishReason(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if alias, ok := d.quirks.finishReasonAliases[normalized]; ok && alias != "" {
		return alias
	}
	return normalized
}

// usageFromWire 把用量对象转成 rig.Usage（只承载四类 token）。
func usageFromWire(usage *streamUsage) *rig.Usage {
	if usage == nil {
		return nil
	}
	out := &rig.Usage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	}
	switch {
	case usage.PromptTokensDetails != nil:
		out.CachedTokens = usage.PromptTokensDetails.CachedTokens
	case usage.PromptCacheHitTokens > 0:
		out.CachedTokens = usage.PromptCacheHitTokens
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}
