package escalate_test

// 对抗审计（续）：资源、协议边界与 API 误用。
//
// 这些用例大多是在"复核作者声明"，因此期望值就是作者写进 doc.go 的承诺；
// 断言失败 = 声明与实现不一致。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

// TestAuditOutputTruncationBoundary 验证 256KiB 截断的边界：恰好等于上限不截断，
// 多一字节才置 Truncated 且长度正好是上限。
func TestAuditOutputTruncationBoundary(t *testing.T) {
	root := t.TempDir()
	limit := 64 // 用小的上限跑边界，逻辑与 256KiB 相同
	policy := escalate.Policy{
		Allow:          [][]string{{"echo"}},
		Roots:          []string{root},
		MaxOutputBytes: limit,
	}
	atLimit := strings.Repeat("a", limit)
	overLimit := strings.Repeat("b", limit+17)
	client := startBroker(t, policy, escalate.WithRunner(func(_ context.Context, req escalate.Request) (escalate.Response, error) {
		if len(req.Argv) > 1 && req.Argv[1] == "over" {
			return escalate.Response{Stdout: overLimit}, nil
		}
		return escalate.Response{Stdout: atLimit}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{Argv: []string{"echo", "at"}, Dir: root, Reason: "边界"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Truncated || len(resp.Stdout) != limit {
		t.Fatalf("恰好等于上限被误截断: len=%d truncated=%v", len(resp.Stdout), resp.Truncated)
	}

	resp, err = client.Exec(ctx, escalate.Request{Argv: []string{"echo", "over"}, Dir: root, Reason: "边界"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !resp.Truncated || len(resp.Stdout) != limit {
		t.Fatalf("超限未被正确截断: len=%d truncated=%v", len(resp.Stdout), resp.Truncated)
	}
}

// TestAuditHugeArgvIsBoundedByLineLimit 验证"几十万参数"这条攻击不可达：
// 协议单行上限 1MiB 让 argv 总量远小于 Linux 的 ARG_MAX（2MiB），因此最多是
// "正常执行 + 输出被截断"，不会挂死、不会 E2BIG 打崩 broker。
func TestAuditHugeArgvIsBoundedByLineLimit(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}, MaxOutputBytes: 64}
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()

	argv := make([]string, 0, 60001)
	argv = append(argv, "echo")
	for i := 0; i < 60000; i++ {
		argv = append(argv, "x")
	}
	start := time.Now()
	resp, err := client.Exec(ctx, escalate.Request{Argv: argv, Dir: root, Reason: "超长 argv"})
	elapsed := time.Since(start)
	if err != nil {
		// 客户端本地拒绝（请求编码后超过 1MiB）也是可接受的收尾。
		if !errors.Is(err, escalate.ErrClosed) && !strings.Contains(err.Error(), "超过单行上限") {
			t.Fatalf("意外错误: %v", err)
		}
		return
	}
	if elapsed > 5*time.Second {
		t.Fatalf("6 万参数的请求耗时 %s，疑似挂死", elapsed)
	}
	if !resp.Denied && resp.ExitCode != 0 {
		t.Fatalf("既没执行也没明确拒绝: exit=%d", resp.ExitCode)
	}
	if len(resp.Stdout) > 64 && !resp.Truncated {
		t.Fatalf("输出 %d 字节超过上限 64 却没有标记截断", len(resp.Stdout))
	}
}

// TestAuditClientCancelDoesNotReachBroker 记录客户端取消的真实语义：
// 协议里没有"取消"消息，因此客户端取消只让本端停止等待，宿主侧命令会继续跑完
// （doc.go 已声明这一点；这里用测试把它钉住，避免宿主误以为取消等于终止）。
func TestAuditClientCancelDoesNotReachBroker(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	marker := filepath.Join(root, "survived.txt")
	policy := escalate.Policy{
		Allow:      [][]string{{"sh", "-c"}},
		Roots:      []string{root},
		MaxTimeout: 10 * time.Second,
	}
	client := startBroker(t, policy)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Exec(ctx, escalate.Request{
			Argv: []string{"sh", "-c", "sleep 0.6; echo survived > " + marker},
			Dir:  root, Reason: "客户端取消",
		})
		errCh <- err
	}()
	time.Sleep(120 * time.Millisecond)
	cancel()
	if err := <-errCh; err == nil {
		t.Fatal("取消后 Exec 应当立刻返回 ctx 错误")
	}
	// 宿主侧仍在执行：这是协议现状（无取消消息）。宿主要终止只能用 MinTimeout/杀进程组。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Log("符合现状：客户端取消后宿主侧命令仍然跑完（协议没有取消消息）")
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("命令既没被执行也没被取消，状态不明")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAuditServeCancelKillsChild 验证 broker 侧取消（Serve 的 ctx）会通过
// exec.CommandContext 真的杀掉直接子进程。
func TestAuditServeCancelKillsChild(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	marker := filepath.Join(root, "survived.txt")
	policy := escalate.Policy{
		Allow:      [][]string{{"sh", "-c"}},
		Roots:      []string{root},
		MaxTimeout: 10 * time.Second,
	}
	broker, err := escalate.NewBroker(policy)
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	serveCtx, cancelServe := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- broker.Serve(serveCtx, serverConn) }()
	client := escalate.NewClient(clientConn)
	defer func() { _ = client.Close() }()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = client.Exec(ctx, escalate.Request{
			Argv: []string{"sh", "-c", "sleep 1; echo survived > " + marker},
			Dir:  root, Reason: "Serve 取消",
		})
	}()
	time.Sleep(200 * time.Millisecond)
	cancelServe()
	time.Sleep(1500 * time.Millisecond)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("Serve 取消后子进程仍然跑完了：exec.CommandContext 没杀掉直接子进程")
	}
	select {
	case <-serveErr:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve 没有在取消后返回")
	}
}

// TestAuditDuplicateRequestID 验证同一 client 上的重复 ID 在本地就被拒绝
// （否则响应会串线），而服务端对重复 ID 也会拒绝。
func TestAuditDuplicateRequestID(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}

	// 本地：同一个 ID 有在途请求时并发第二次 → 本地报错。
	slowClient := startBroker(t, policy, escalate.WithRunner(func(context.Context, escalate.Request) (escalate.Response, error) {
		time.Sleep(200 * time.Millisecond)
		return escalate.Response{Stdout: "ok"}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = slowClient.Exec(ctx, escalate.Request{ID: "dup-local", Argv: []string{"echo", "a"}, Dir: root, Reason: "并发"})
	}()
	time.Sleep(30 * time.Millisecond)
	if _, err := slowClient.Exec(ctx, escalate.Request{ID: "dup-local", Argv: []string{"echo", "b"}, Dir: root, Reason: "并发"}); err == nil {
		t.Fatal("同一连接上重复的在途 ID 应当本地报错")
	}
	wg.Wait()

	// 服务端：顺序复用同一个 ID → 第二次被防重放拒绝。
	conn, _ := startRawServer(t, policy)
	reader := bufio.NewReader(conn)
	line := `{"id":"replay","argv":["echo","hi"],"dir":"` + root + `","reason":"重放"}`
	rawRoundTrip(t, conn, reader, line)
	second := rawRoundTrip(t, conn, reader, line)
	var resp escalate.Response
	if err := json.Unmarshal([]byte(second), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %q", second)
	}
	if !resp.Denied {
		t.Fatalf("重复 ID 未被拒绝: %+v", resp)
	}
}

// TestAuditClientIgnoresMismatchedResponseID 验证客户端不会把"ID 不匹配的响应"
// 错配给别的请求（伪造/串线的响应必须被丢掉）。
func TestAuditClientIgnoresMismatchedResponseID(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	client := escalate.NewClient(clientConn)
	defer func() { _ = client.Close() }()

	go func() {
		reader := bufio.NewReader(serverConn)
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		// 故意回一个别的请求的 ID。
		_, _ = serverConn.Write([]byte(`{"id":"someone-else","exitCode":0,"stdout":"STOLEN","stderr":""}` + "\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	resp, err := client.Exec(ctx, escalate.Request{
		ID: "mine", Argv: []string{"echo", "hi"}, Dir: t.TempDir(), Reason: "串线检查",
	})
	if err == nil && resp.Stdout == "STOLEN" {
		t.Fatal("客户端把 ID 不匹配的响应错配给了本请求")
	}
	if err == nil {
		t.Fatalf("ID 不匹配时应等待自己的响应直到 ctx 结束，实际返回: %+v", resp)
	}
}

// TestAuditBrokerConfigValidation 复核 NewBroker 的配置校验（API 误用范围）。
func TestAuditBrokerConfigValidation(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name    string
		policy  escalate.Policy
		wantErr bool
	}{
		{"root 正常", escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}, false},
		{"Roots 为空", escalate.Policy{Allow: [][]string{{"echo"}}}, true},
		{"Roots 相对路径", escalate.Policy{Roots: []string{"relative/dir"}}, true},
		{"Roots 不存在", escalate.Policy{Roots: []string{filepath.Join(root, "nope")}}, true},
		{"Allow 为空（= 一律拒绝）", escalate.Policy{Roots: []string{root}}, false},
		{"Deny 空前缀", escalate.Policy{Roots: []string{root}, Deny: [][]string{{}}}, true},
		{"Allow 空字符串 token", escalate.Policy{Roots: []string{root}, Allow: [][]string{{" "}}}, true},
		{"MaxTimeout 负数", escalate.Policy{Roots: []string{root}, MaxTimeout: -time.Second}, true},
		{"MaxOutputBytes 负数", escalate.Policy{Roots: []string{root}, MaxOutputBytes: -1}, true},
		{"MaxConcurrent 负数", escalate.Policy{Roots: []string{root}, MaxConcurrent: -1}, true},
		{"上限为 0（用默认值）", escalate.Policy{Roots: []string{root}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker, err := escalate.NewBroker(tc.policy)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望配置错误，实际通过: %+v", broker)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望通过，实际报错: %v", err)
			}
			if tc.policy.Allow == nil {
				// Allow 为空 = 一律拒绝（不是全放行）。
				decision := broker.Policy().Decide(escalate.Request{
					Argv: []string{"echo", "hi"}, Dir: root, Reason: "空 Allow",
				})
				if decision.Allow {
					t.Fatal("Allow 为空时必须一律拒绝")
				}
			}
		})
	}
}

// TestAuditClientMisuse 复核 Client.Exec 的误用边界：nil ctx、空 argv、空 reason。
func TestAuditClientMisuse(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}
	client := startBroker(t, policy)

	// nil ctx 在库内等价于 Background；这里直接用 Background（SA1012 不允许显式传 nil）。
	resp, err := client.Exec(context.Background(), escalate.Request{Argv: []string{"echo", "hi"}, Dir: root, Reason: "background ctx"})
	if err != nil {
		t.Fatalf("nil ctx 应当按 Background 处理: %v", err)
	}
	if resp.Denied || !strings.Contains(resp.Stdout, "hi") {
		t.Fatalf("nil ctx 请求异常: %+v", resp)
	}

	ctx, cancel := execCtx(t)
	defer cancel()
	if resp, err = client.Exec(ctx, escalate.Request{Argv: nil, Dir: root, Reason: "空 argv"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !resp.Denied {
		t.Fatalf("空 argv 未被拒绝: %+v", resp)
	}
	if resp, err = client.Exec(ctx, escalate.Request{Argv: []string{"echo", "hi"}, Dir: root, Reason: "  "}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !resp.Denied {
		t.Fatalf("空 reason 未被拒绝: %+v", resp)
	}

	// 关闭后继续用。
	_ = client.Close()
	if _, err = client.Exec(ctx, escalate.Request{Argv: []string{"echo", "hi"}, Dir: root, Reason: "关闭后"}); err == nil {
		t.Fatal("Close 之后 Exec 必须返回错误")
	}
}

// TestAuditTimeoutMSNegativeUsesPolicy 复核负 TimeoutMS 不会变成"立即超时"。
func TestAuditTimeoutMSNegativeUsesPolicy(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	policy := escalate.Policy{
		Allow:      [][]string{{"sh", "-c"}},
		Roots:      []string{root},
		MaxTimeout: 5 * time.Second,
	}
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()
	resp, err := client.Exec(ctx, escalate.Request{
		Argv: []string{"sh", "-c", "sleep 0.05; echo done"}, Dir: root,
		Reason: "负超时", TimeoutMS: -1,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.ExitCode != 0 || !strings.Contains(resp.Stdout, "done") {
		t.Fatalf("负 TimeoutMS 影响了执行: exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	_ = math.MaxInt64 // 与 TestAuditTimeoutOverflowIsClamped 成对
}

// TestAuditEnvDroppedKeysAreReported 验证被丢弃的环境变量名进了 stderr 诊断
// （审计可用性），且**不泄漏值**。
func TestAuditEnvDroppedKeysAreReported(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}
	client := startBroker(t, policy, escalate.WithRunner(func(context.Context, escalate.Request) (escalate.Response, error) {
		return escalate.Response{Stdout: "ok"}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()
	resp, err := client.Exec(ctx, escalate.Request{
		Argv: []string{"echo", "hi"}, Dir: root, Reason: "env 诊断",
		Env: map[string]string{"AWS_SECRET_ACCESS_KEY": "super-secret-value", "GIT_AUTHOR_NAME": "n"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(resp.Stderr, "AWS_SECRET_ACCESS_KEY") {
		t.Fatalf("被丢弃的键名没有出现在诊断里: %q", resp.Stderr)
	}
	if strings.Contains(resp.Stderr, "super-secret-value") {
		t.Fatal("诊断泄漏了被丢弃变量的值")
	}
}

// TestAuditAuditorSeesDeniedRequests 验证拒绝也进审计（doc.go 的承诺）。
func TestAuditAuditorSeesDeniedRequests(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}

	var mu sync.Mutex
	deniedSeen := 0
	auditor := func(_ escalate.Request, resp escalate.Response) {
		if resp.Denied {
			mu.Lock()
			deniedSeen++
			mu.Unlock()
		}
	}
	client := startBroker(t, policy, escalate.WithAuditor(auditor))
	ctx, cancel := execCtx(t)
	defer cancel()
	for _, argv := range [][]string{{"rm", "-rf", "/"}, {"echo", "hi"}} {
		if _, err := client.Exec(ctx, escalate.Request{
			ID: fmt.Sprintf("r-%s", argv[0]), Argv: argv, Dir: root, Reason: "审计",
		}); err != nil {
			t.Fatalf("Exec: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if deniedSeen == 0 {
		t.Fatal("被策略拒绝的请求没有进审计")
	}
}

// TestAuditConnectionUsableAfterOversizeLine 复核"超长行之后连接仍可用"的声明：
// 裸客户端写一条 >1MiB 的行，拿到拒绝后同一条连接还能继续发正常请求。
func TestAuditConnectionUsableAfterOversizeLine(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}
	conn, _ := startRawServer(t, policy)
	reader := bufio.NewReader(conn)

	oversize := `{"id":"big","argv":["echo","` + strings.Repeat("A", escalate.MaxLineBytes) + `"],"dir":"` + root + `","reason":"超长"}`
	first := rawRoundTrip(t, conn, reader, oversize)
	var deniedResp escalate.Response
	if err := json.Unmarshal([]byte(first), &deniedResp); err != nil {
		t.Fatalf("超长行的响应不是合法 JSON: %q", first)
	}
	if !deniedResp.Denied {
		t.Fatalf("超长行未被拒绝: %+v", deniedResp)
	}

	second := rawRoundTrip(t, conn, reader, `{"id":"ok","argv":["echo","still-alive"],"dir":"`+root+`","reason":"连接复用"}`)
	var okResp escalate.Response
	if err := json.Unmarshal([]byte(second), &okResp); err != nil {
		t.Fatalf("第二条响应不是合法 JSON: %q", second)
	}
	if okResp.Denied || !strings.Contains(okResp.Stdout, "still-alive") {
		t.Fatalf("超长行之后的连接不可用: %+v", okResp)
	}
}

// TestAuditDirEscapeVariants 覆盖 Dir 逃逸的各种拼写（绝对路径 + ..、符号链接、
// 前缀伪装），全部必须拒绝。
func TestAuditDirEscapeVariants(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := mkdir(root); err != nil {
		t.Fatal(err)
	}
	if err := mkdir(outside); err != nil {
		t.Fatal(err)
	}
	// root 里放一个指向外面的符号链接。
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	// 另一个 root，用于验证前缀伪装（/root 与 /root-evil）。
	other := filepath.Join(base, "root-evil")
	if err := mkdir(other); err != nil {
		t.Fatal(err)
	}
	policy := escalate.Policy{
		Allow: [][]string{{"echo"}},
		Roots: []string{root},
	}
	bad := []string{
		root + "/../outside",
		root + "/../../etc",
		link,
		link + "/sub",
		other,
		"relative/path",
		"",
		"/",
		base,
	}
	for _, dir := range bad {
		if decision := policy.Decide(escalate.Request{Argv: []string{"echo", "x"}, Dir: dir, Reason: "逃逸"}); decision.Allow {
			t.Errorf("Dir %q 越界却被允许", dir)
		}
	}
	if decision := policy.Decide(escalate.Request{Argv: []string{"echo", "x"}, Dir: root, Reason: "正常"}); !decision.Allow {
		t.Fatalf("Roots 内的 Dir 被误拒: %s", decision.Reason)
	}
}
