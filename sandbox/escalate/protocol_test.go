package escalate_test

// 协议健壮性：坏 JSON、超长行、未知字段、空行、对端断开、取消后无泄漏、并发响应配对。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

func TestServeBadJSONLineRespondsDeniedAndContinues(t *testing.T) {
	policy, root := testPolicy(t)
	conn, _ := startRawServer(t, policy)
	r := bufio.NewReader(conn)

	got := rawRoundTrip(t, conn, r, `{"id": "broken", "argv": [`)
	var resp escalate.Response
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%q)", err, got)
	}
	if !resp.Denied {
		t.Fatalf("坏 JSON 必须回 Denied: %+v", resp)
	}
	mustContain(t, resp.DenyReason, "协议")

	// 流仍然可用：下一条正常请求必须成功。
	line := fmt.Sprintf(`{"id":"after-bad","argv":["echo","ok"],"dir":%q,"reason":"坏行之后"}`, root)
	resp2 := decodeResp(t, rawRoundTrip(t, conn, r, line))
	if resp2.Denied || resp2.ExitCode != 0 {
		t.Fatalf("坏行之后协议应继续工作: %+v", resp2)
	}
	if resp2.ID != "after-bad" {
		t.Fatalf("响应 ID = %q", resp2.ID)
	}
}

func TestServeOversizeLineRejectedAndContinues(t *testing.T) {
	policy, root := testPolicy(t)
	conn, _ := startRawServer(t, policy)
	r := bufio.NewReader(conn)

	huge := strings.Repeat("x", escalate.MaxLineBytes+64)
	resp := decodeResp(t, rawRoundTrip(t, conn, r, huge))
	if !resp.Denied {
		t.Fatalf("超长行必须拒绝: %+v", resp)
	}
	mustContain(t, resp.DenyReason, "1MiB")

	line := fmt.Sprintf(`{"id":"after-huge","argv":["echo","ok"],"dir":%q,"reason":"超长行之后"}`, root)
	resp2 := decodeResp(t, rawRoundTrip(t, conn, r, line))
	if resp2.Denied || resp2.ID != "after-huge" {
		t.Fatalf("超长行之后协议应继续工作: %+v", resp2)
	}
}

func TestServeIgnoresUnknownFields(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)
	conn, _ := startRawServer(t, policy)
	r := bufio.NewReader(conn)

	raw, err := os.ReadFile(filepath.Join("testdata", "request_unknown_fields.json"))
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	line := strings.ReplaceAll(strings.TrimSpace(string(raw)), "__ROOT__", root)
	resp := decodeResp(t, rawRoundTrip(t, conn, r, line))
	if resp.Denied {
		t.Fatalf("未知字段必须忽略而不是拒绝: %+v", resp)
	}
	if resp.ID != "wire-1" || resp.ExitCode != 0 {
		t.Fatalf("fixture 请求执行结果不对: %+v", resp)
	}
	mustContain(t, resp.Stdout, "wire")
}

func TestWireResponseFixtureDecodes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "response_denied.json"))
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	var resp escalate.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("响应 fixture 解析失败: %v", err)
	}
	if !resp.Denied || resp.ID != "wire-denied" || resp.DenyReason == "" {
		t.Fatalf("响应 fixture 字段不匹配: %+v", resp)
	}
}

func TestServeIgnoresBlankLines(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)
	conn, _ := startRawServer(t, policy)
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("\n\n   \n")); err != nil {
		t.Fatalf("写空行: %v", err)
	}
	line := fmt.Sprintf(`{"id":"after-blank","argv":["echo","ok"],"dir":%q,"reason":"空行之后"}`, root)
	resp := decodeResp(t, rawRoundTrip(t, conn, r, line))
	if resp.ID != "after-blank" || resp.Denied {
		t.Fatalf("空行不应产生响应或影响后续请求: %+v", resp)
	}
}

func TestServeDeniesEmptyID(t *testing.T) {
	policy, root := testPolicy(t)
	conn, _ := startRawServer(t, policy)
	r := bufio.NewReader(conn)

	line := fmt.Sprintf(`{"id":"","argv":["echo","x"],"dir":%q,"reason":"缺少 ID"}`, root)
	resp := decodeResp(t, rawRoundTrip(t, conn, r, line))
	if !resp.Denied {
		t.Fatalf("空 ID 必须拒绝（响应无法配对，也是防重放的一部分）: %+v", resp)
	}
	mustContain(t, resp.DenyReason, "id")
}

func TestServePeerCloseReturnsError(t *testing.T) {
	policy, _ := testPolicy(t)
	broker, err := escalate.NewBroker(policy)
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	server, clientConn := pipePair(t)
	errCh := make(chan error, 1)
	go func() { errCh <- broker.Serve(context.Background(), server) }()

	if err := clientConn.Close(); err != nil {
		t.Fatalf("关闭客户端连接: %v", err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("对端断开时 Serve 必须返回错误")
		}
		if !errors.Is(err, escalate.ErrClosed) {
			t.Fatalf("Serve 错误应可归因为 ErrClosed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve 未在对端断开后返回")
	}
}

func TestServeContextCancelReleasesGoroutines(t *testing.T) {
	requireUnix(t)
	baseline := runtime.NumGoroutine()

	policy, root := testPolicy(t)
	broker, err := escalate.NewBroker(policy)
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	server, clientConn := pipePair(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- broker.Serve(ctx, server) }()

	client := escalate.NewClient(clientConn)
	execCtx, execCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer execCancel()
	if _, err := client.Exec(execCtx, escalate.Request{
		ID: "cancel-1", Argv: []string{"echo", "hi"}, Dir: root, Reason: "取消前先确认在线",
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消后 Serve 应返回 context.Canceled，得到 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后 Serve 未返回")
	}
	_ = client.Close()
	waitGoroutines(t, baseline, 2)
}

func TestServeConcurrentResponsesMatchRequestIDs(t *testing.T) {
	policy, root := testPolicy(t)
	// runner 把请求 ID 作为 stdout 回显，便于核对响应与请求的配对。
	client := startBroker(t, policy, escalate.WithRunner(func(_ context.Context, req escalate.Request) (escalate.Response, error) {
		time.Sleep(20 * time.Millisecond)
		return escalate.Response{ExitCode: 0, Stdout: "echo:" + req.ID}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	const n = 8
	var wg sync.WaitGroup
	results := make([]escalate.Response, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("multi-%d", i)
			results[i], errs[i] = client.Exec(ctx, escalate.Request{
				ID: id, Argv: []string{"echo", "x"}, Dir: root, Reason: "并发配对",
			})
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个请求失败: %v", i, errs[i])
		}
		wantID := fmt.Sprintf("multi-%d", i)
		if results[i].ID != wantID {
			t.Fatalf("响应 ID = %q, want %q", results[i].ID, wantID)
		}
		if results[i].Stdout != "echo:"+wantID {
			t.Fatalf("响应串线: stdout=%q, want echo:%s", results[i].Stdout, wantID)
		}
	}
}

// decodeResp 把一行 JSON 解析成 Response。
func decodeResp(t *testing.T, line string) escalate.Response {
	t.Helper()
	var resp escalate.Response
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%q)", err, line)
	}
	return resp
}
