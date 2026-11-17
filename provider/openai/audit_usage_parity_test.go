package openai

// 审计用例（已修）：同一个 usage 报文经 provider/openai 与 provider/local 解析后，
// 必须得到**完全相同的 rig.Usage**——否则成本/上下文统计会因"走哪条 provider 路径"
// 而不同（costmeter 直接吃这个差异）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/local"
)

// usageOf 用给定 provider 跑一次只含 usage 的流，返回解析到的用量。
func usageOf(t *testing.T, build func(*httptest.Server) rig.Stream) *rig.Usage {
	t.Helper()
	stream := build(nil)
	defer func() {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
	}()
	var usage *rig.Usage
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if chunk.Usage != nil {
			copied := *chunk.Usage
			usage = &copied
		}
	}
	return usage
}

// 四种报文：缺 total_tokens、显式 total_tokens、带 cached（嵌套）、带 cached（扁平）。
func TestAuditUsageParityAcrossProviders(t *testing.T) {
	cases := map[string]string{
		"缺 total_tokens": `{"prompt_tokens":11,"completion_tokens":7}`,
		"带 total_tokens": `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}`,
		"嵌套 cached":      `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":5}}`,
		"扁平 cached":      `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_cache_hit_tokens":5}`,
	}
	events := func(usage string) []string {
		return []string{
			`{"choices":[{"delta":{"content":"x"}}]}`,
			`{"choices":[],"usage":` + usage + `}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		}
	}
	for name, usage := range cases {
		t.Run(name, func(t *testing.T) {
			openaiUsage := usageOf(t, func(*httptest.Server) rig.Stream {
				provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
					writeSSE(w, events(usage)...)
				})
				stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
				if err != nil {
					t.Fatalf("openai.Stream: %v", err)
				}
				return stream
			})
			localUsage := usageOf(t, func(*httptest.Server) rig.Stream {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, event := range events(usage) {
						_, _ = io.WriteString(w, "data: "+event+"\n\n")
					}
				}))
				t.Cleanup(server.Close)
				provider, err := local.New(local.Config{
					BaseURL:      server.URL + "/v1",
					DefaultModel: "m",
					HTTPClient:   server.Client(),
					RetryBackoff: time.Millisecond,
				})
				if err != nil {
					t.Fatalf("local.New: %v", err)
				}
				stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
				if err != nil {
					t.Fatalf("local.Stream: %v", err)
				}
				return stream
			})

			if openaiUsage == nil || localUsage == nil {
				t.Fatalf("用量没解析出来: openai=%+v local=%+v", openaiUsage, localUsage)
			}
			if *openaiUsage != *localUsage {
				t.Fatalf("两个 provider 对同一报文给出了不同用量:\n openai = %+v\n local  = %+v", openaiUsage, localUsage)
			}
			// 缺 total_tokens 时必须被推导出来（两个 provider 都要）。
			if openaiUsage.TotalTokens != 18 {
				t.Fatalf("total_tokens 没有推导: %+v", openaiUsage)
			}
		})
	}
}
