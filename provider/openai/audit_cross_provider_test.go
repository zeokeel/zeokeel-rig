package openai

// 跨 provider 一致性契约：**同一份 SSE 报文**分别喂给 provider/openai（严格）与
// provider/local（宽松），两者的差异必须被固化成契约，而不是"碰巧不一样"。
//
// 两者现在共用同一套 SSE 引擎（provider/local 的 NewStream），差异只由 Quirks 表达：
//
//	openai: local.Quirks{RequireDone: <Config.RequireCompleteStream>, StrictFinishReasons: true, StrictDeltas: true}
//	local : 默认（宽松）
//
// 本文件把四条差异逐条钉住：
//  1. 缺终止标记（既无 finish_reason 也无 [DONE]）：严格模式下 openai 报
//     ErrIncompleteStream，宽松模式下 local 正常收尾；
//  2. finish_reason 变体（eos/max_tokens/tool_call）：openai 原样透传，local 归一化；
//  3. choice.message 回退：openai 忽略（无分片），local 接受；
//  4. 对象型 tool_calls.arguments：openai 整条事件解码失败并跳过，local 紧凑化为 JSON。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/local"
)

// crossProviderStreams 用同一份响应体分别开一条 openai 流（严格）与一条 local 流
// （宽松）。requireComplete 是 openai 的 Config.RequireCompleteStream。
func crossProviderStreams(t *testing.T, body string, requireComplete bool) (strict, lenient rig.Stream) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	strictProvider, err := New(Config{
		BaseURL:               server.URL + "/v1",
		APIKey:                "audit-key",
		DefaultModel:          "m",
		HTTPClient:            server.Client(),
		RetryBackoff:          time.Millisecond,
		RequireCompleteStream: requireComplete,
	})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	lenientProvider, err := local.New(local.Config{
		BaseURL:      server.URL + "/v1",
		DefaultModel: "m",
		HTTPClient:   server.Client(),
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	strict, err = strictProvider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("openai.Stream: %v", err)
	}
	t.Cleanup(func() { closeIfCloser(strict) })
	lenient, err = lenientProvider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("local.Stream: %v", err)
	}
	t.Cleanup(func() { closeIfCloser(lenient) })
	return strict, lenient
}

func closeIfCloser(stream rig.Stream) {
	if closer, ok := stream.(io.Closer); ok {
		_ = closer.Close()
	}
}

// collectAll 读尽一条流，返回分片与终止错误。
func collectAll(t *testing.T, stream rig.Stream) ([]rig.Chunk, error) {
	t.Helper()
	var chunks []rig.Chunk
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return chunks, nil
			}
			return chunks, err
		}
		chunks = append(chunks, chunk)
	}
}

// TestCrossProviderMissingTerminatorIsStrictOnly 是 lead 点名的那条：缺 [DONE]
// 时严格模式报错、宽松模式容忍。
func TestCrossProviderMissingTerminatorIsStrictOnly(t *testing.T) {
	// 既没有 finish_reason 也没有 [DONE]，只有一段正文。
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"

	strict, lenient := crossProviderStreams(t, body, true)
	strictChunks, strictErr := collectAll(t, strict)
	if !errors.Is(strictErr, ErrIncompleteStream) {
		t.Fatalf("openai(严格) 错误 = %v, want ErrIncompleteStream", strictErr)
	}
	if !errors.Is(strictErr, io.ErrUnexpectedEOF) {
		t.Fatalf("openai(严格) 错误 = %v, want io.ErrUnexpectedEOF", strictErr)
	}
	// 报错之前已经发出的分片不能丢（回答是部分可用的）。
	if len(strictChunks) != 1 || strictChunks[0].Delta != "partial" {
		t.Fatalf("openai(严格) 分片 = %+v, want 一条 partial", strictChunks)
	}
	if strings.Contains(strictErr.Error(), "local:") {
		t.Fatalf("openai 的错误前缀必须是 openai:，实际 %v", strictErr)
	}

	lenientChunks, lenientErr := collectAll(t, lenient)
	if lenientErr != nil {
		t.Fatalf("local(宽松) 不该报错: %v", lenientErr)
	}
	if len(lenientChunks) != 1 || lenientChunks[0].Delta != "partial" {
		t.Fatalf("local(宽松) 分片 = %+v", lenientChunks)
	}

	// 对照：openai 关掉 RequireCompleteStream 时与宽松模式一致（历史默认）。
	strictOff, _ := crossProviderStreams(t, body, false)
	if _, err := collectAll(t, strictOff); err != nil {
		t.Fatalf("openai(RequireCompleteStream=false) 不该报错: %v", err)
	}
}

// TestCrossProviderFinishReasonStrictPassThrough：finish_reason 变体在严格模式下
// 原样透传，在宽松模式下归一化。
func TestCrossProviderFinishReasonStrictPassThrough(t *testing.T) {
	cases := []struct {
		raw     string
		strict  string
		lenient string
	}{
		{"eos", "eos", "stop"},
		{"max_tokens", "max_tokens", "length"},
		{"tool_call", "tool_call", "tool_calls"},
		{"stop", "stop", "stop"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.raw, func(t *testing.T) {
			body := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", testCase.raw)
			strict, lenient := crossProviderStreams(t, body, true)
			strictChunks, strictErr := collectAll(t, strict)
			if strictErr != nil {
				t.Fatalf("openai: %v", strictErr)
			}
			if got := lastFinish(strictChunks); got != testCase.strict {
				t.Fatalf("openai finish = %q, want %q（严格模式原样透传）", got, testCase.strict)
			}
			lenientChunks, lenientErr := collectAll(t, lenient)
			if lenientErr != nil {
				t.Fatalf("local: %v", lenientErr)
			}
			if got := lastFinish(lenientChunks); got != testCase.lenient {
				t.Fatalf("local finish = %q, want %q（宽松模式归一化）", got, testCase.lenient)
			}
		})
	}
}

// TestCrossProviderMessageFallbackIsLenientOnly：choice.message（而不是 delta）
// 只有宽松模式接受。
func TestCrossProviderMessageFallbackIsLenientOnly(t *testing.T) {
	body := "data: {\"choices\":[{\"message\":{\"content\":\"whole message\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	strict, lenient := crossProviderStreams(t, body, true)
	strictChunks, strictErr := collectAll(t, strict)
	if strictErr != nil {
		t.Fatalf("openai: %v", strictErr)
	}
	if text := joinDelta(strictChunks); text != "" {
		t.Fatalf("openai 文本 = %q, want 空（严格模式不认 message 回退）", text)
	}
	lenientChunks, lenientErr := collectAll(t, lenient)
	if lenientErr != nil {
		t.Fatalf("local: %v", lenientErr)
	}
	if text := joinDelta(lenientChunks); text != "whole message" {
		t.Fatalf("local 文本 = %q, want whole message（宽松模式接受 message 回退）", text)
	}
}

// TestCrossProviderObjectArgumentsIsLenientOnly：对象型 arguments 在严格模式
// 整条事件被跳过（历史行为），宽松模式紧凑化成 JSON 字符串。
func TestCrossProviderObjectArgumentsIsLenientOnly(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}}]}}]}\n\ndata: [DONE]\n\n"
	strict, lenient := crossProviderStreams(t, body, true)

	strictChunks, strictErr := collectAll(t, strict)
	if strictErr != nil {
		t.Fatalf("openai: %v", strictErr)
	}
	if len(toolDeltas(strictChunks)) != 0 {
		t.Fatalf("openai tool_calls = %+v, want 空（严格模式跳过形状不符的事件）", toolDeltas(strictChunks))
	}

	lenientChunks, lenientErr := collectAll(t, lenient)
	if lenientErr != nil {
		t.Fatalf("local: %v", lenientErr)
	}
	deltas := toolDeltas(lenientChunks)
	if len(deltas) != 1 || deltas[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("local tool_calls = %+v, want 一条紧凑化的对象型参数", deltas)
	}
}

// lastFinish 返回最后一条非空 finish_reason。
func lastFinish(chunks []rig.Chunk) string {
	finish := ""
	for _, chunk := range chunks {
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
	}
	return finish
}

// joinDelta 拼接所有正文分片。
func joinDelta(chunks []rig.Chunk) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		builder.WriteString(chunk.Delta)
	}
	return builder.String()
}

// toolDeltas 收集所有工具调用分片。
func toolDeltas(chunks []rig.Chunk) []rig.ToolCallDelta {
	var out []rig.ToolCallDelta
	for _, chunk := range chunks {
		out = append(out, chunk.ToolCalls...)
	}
	return out
}
