package escalate_test

// 宿主侧 broker 的用例：策略、审批、重放、截断、超时、并发、审计、环境过滤。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

func TestNewBrokerRejectsBadConfig(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name   string
		policy escalate.Policy
	}{
		{"无 Roots", escalate.Policy{Allow: [][]string{{"git"}}}},
		{"相对 Roots", escalate.Policy{Allow: [][]string{{"git"}}, Roots: []string{"relative/dir"}}},
		{"空 Allow 前缀", escalate.Policy{Allow: [][]string{{}}, Roots: []string{root}}},
		{"空 Deny 前缀", escalate.Policy{Allow: [][]string{{"git"}}, Deny: [][]string{{}}, Roots: []string{root}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := escalate.NewBroker(tc.policy); err == nil {
				t.Fatal("非法配置必须在 NewBroker 阶段报错")
			}
		})
	}

	if _, err := escalate.NewBroker(escalate.Policy{Allow: [][]string{{"git"}}, Roots: []string{root}}); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
}

func TestBrokerExecutesAllowedRequest(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{
		ID: "exec-1", Argv: []string{"echo", "hello-escalate"}, Dir: root, Reason: "验证宿主侧执行",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Denied {
		t.Fatalf("不应被拒绝: %+v", resp)
	}
	if resp.ID != "exec-1" {
		t.Fatalf("响应 ID = %q, want exec-1", resp.ID)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	mustContain(t, resp.Stdout, "hello-escalate")
}

func TestBrokerPropagatesNonZeroExit(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{
		ID: "exec-exit", Argv: []string{"sh", "-c", "exit 3; "}, Dir: root, Reason: "非零退出应上报而不是拒绝",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Denied {
		t.Fatalf("非零退出不是策略拒绝: %+v", resp)
	}
	if resp.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", resp.ExitCode)
	}
}

func TestBrokerDenials(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	policy := escalate.Policy{
		Allow: [][]string{{"echo"}, {"git", "fetch"}, {"curl"}},
		Deny:  [][]string{{"git", "fetch", "--upload-pack"}},
		Roots: []string{root},
	}
	outside := t.TempDir()

	cases := []struct {
		name string
		req  escalate.Request
		want string
	}{
		{"未匹配 Allow", escalate.Request{ID: "d1", Argv: []string{"rm", "-rf", "/"}, Dir: root, Reason: "测试"}, "允许前缀"},
		{"命中 Deny", escalate.Request{ID: "d2", Argv: []string{"git", "fetch", "--upload-pack=x"}, Dir: root, Reason: "测试"}, "禁止"},
		{"空 argv", escalate.Request{ID: "d3", Dir: root, Reason: "测试"}, "argv"},
		{"空理由", escalate.Request{ID: "d4", Argv: []string{"echo", "x"}, Dir: root, Reason: "  "}, "理由"},
		{"Dir 越界", escalate.Request{ID: "d5", Argv: []string{"echo", "x"}, Dir: outside, Reason: "测试"}, "Roots"},
		{"Dir 相对", escalate.Request{ID: "d6", Argv: []string{"echo", "x"}, Dir: "relative", Reason: "测试"}, "绝对路径"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := startBroker(t, policy)
			ctx, cancel := execCtx(t)
			defer cancel()
			resp, err := client.Exec(ctx, tc.req)
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if !resp.Denied {
				t.Fatalf("应被拒绝: %+v", resp)
			}
			if resp.ExitCode != escalate.DeniedExitCode {
				t.Fatalf("拒绝的 ExitCode = %d, want %d", resp.ExitCode, escalate.DeniedExitCode)
			}
			mustContain(t, resp.DenyReason, tc.want)
			if resp.Stdout != "" {
				t.Fatalf("拒绝时不应有 stdout: %q", resp.Stdout)
			}
		})
	}
}

func TestBrokerApprovalFlow(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	policy := escalate.Policy{
		Allow:    [][]string{{"echo"}, {"curl"}},
		Approval: [][]string{{"curl"}},
		Roots:    []string{root},
	}

	t.Run("批准后执行", func(t *testing.T) {
		var seen []string
		client := startBroker(t, policy,
			escalate.WithApprover(func(_ context.Context, req escalate.Request) (bool, string, error) {
				seen = append(seen, req.ID)
				return true, "运营同意", nil
			}),
			// 用桩 runner 避免用例真的去联网；审批路径本身才是被测对象。
			escalate.WithRunner(func(_ context.Context, req escalate.Request) (escalate.Response, error) {
				return escalate.Response{ExitCode: 0, Stdout: "curl " + req.ID}, nil
			}),
		)
		ctx, cancel := execCtx(t)
		defer cancel()
		resp, err := client.Exec(ctx, escalate.Request{ID: "a1", Argv: []string{"curl", "https://example.com"}, Dir: root, Reason: "需要宿主凭据"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if resp.Denied || resp.ExitCode != 0 {
			t.Fatalf("批准后应执行: %+v", resp)
		}
		if len(seen) != 1 || seen[0] != "a1" {
			t.Fatalf("approver 调用记录 = %v", seen)
		}
	})

	t.Run("拒绝", func(t *testing.T) {
		client := startBroker(t, policy, escalate.WithApprover(func(_ context.Context, _ escalate.Request) (bool, string, error) {
			return false, "不允许外联", nil
		}))
		ctx, cancel := execCtx(t)
		defer cancel()
		resp, err := client.Exec(ctx, escalate.Request{ID: "a2", Argv: []string{"curl", "https://example.com"}, Dir: root, Reason: "需要外联"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !resp.Denied {
			t.Fatalf("审批拒绝应回 Denied: %+v", resp)
		}
		mustContain(t, resp.DenyReason, "不允许外联")
	})

	t.Run("审批报错按拒绝处理", func(t *testing.T) {
		client := startBroker(t, policy, escalate.WithApprover(func(_ context.Context, _ escalate.Request) (bool, string, error) {
			return true, "", errors.New("审批通道断开")
		}))
		ctx, cancel := execCtx(t)
		defer cancel()
		resp, err := client.Exec(ctx, escalate.Request{ID: "a3", Argv: []string{"curl", "https://example.com"}, Dir: root, Reason: "需要外联"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !resp.Denied {
			t.Fatalf("审批报错必须 fail closed: %+v", resp)
		}
		mustContain(t, resp.DenyReason, "审批失败")
	})

	t.Run("未配置 approver 一律拒绝", func(t *testing.T) {
		client := startBroker(t, policy)
		ctx, cancel := execCtx(t)
		defer cancel()
		resp, err := client.Exec(ctx, escalate.Request{ID: "a4", Argv: []string{"curl", "https://example.com"}, Dir: root, Reason: "需要外联"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !resp.Denied {
			t.Fatalf("无 approver 必须拒绝: %+v", resp)
		}
		mustContain(t, resp.DenyReason, "approver")
	})

	t.Run("不需要审批的请求不惊动 approver", func(t *testing.T) {
		called := false
		client := startBroker(t, policy, escalate.WithApprover(func(_ context.Context, _ escalate.Request) (bool, string, error) {
			called = true
			return false, "", nil
		}))
		ctx, cancel := execCtx(t)
		defer cancel()
		resp, err := client.Exec(ctx, escalate.Request{ID: "a5", Argv: []string{"echo", "x"}, Dir: root, Reason: "不需要审批"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if resp.Denied {
			t.Fatalf("不应拒绝: %+v", resp)
		}
		if called {
			t.Fatal("未命中 Approval 前缀时不应调用 approver")
		}
	})
}

func TestBrokerRejectsReplayedID(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()

	req := escalate.Request{ID: "replay-1", Argv: []string{"echo", "once"}, Dir: root, Reason: "第一次"}
	first, err := client.Exec(ctx, req)
	if err != nil {
		t.Fatalf("第一次 Exec: %v", err)
	}
	if first.Denied {
		t.Fatalf("第一次不应被拒绝: %+v", first)
	}

	second, err := client.Exec(ctx, req)
	if err != nil {
		t.Fatalf("第二次 Exec: %v", err)
	}
	if !second.Denied {
		t.Fatalf("重放的 ID 必须拒绝: %+v", second)
	}
	mustContain(t, second.DenyReason, "重复的请求 ID")
}

func TestBrokerTruncatesCustomRunnerOutput(t *testing.T) {
	policy, root := testPolicy(t)
	policy.MaxOutputBytes = 128
	big := strings.Repeat("x", 4096)
	client := startBroker(t, policy, escalate.WithRunner(func(_ context.Context, _ escalate.Request) (escalate.Response, error) {
		return escalate.Response{ExitCode: 0, Stdout: big, Stderr: big}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{ID: "t1", Argv: []string{"echo", "x"}, Dir: root, Reason: "截断测试"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !resp.Truncated {
		t.Fatalf("超限输出必须标记 Truncated: %+v", resp)
	}
	if len(resp.Stdout) != 128 || len(resp.Stderr) != 128 {
		t.Fatalf("stdout/stderr 长度 = %d/%d, want 128/128", len(resp.Stdout), len(resp.Stderr))
	}
}

func TestBrokerTruncatesDefaultRunnerOutput(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)
	policy.MaxOutputBytes = 64
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{
		ID: "t2", Argv: []string{"sh", "-c", "i=0; while [ $i -lt 512 ]; do printf x; i=$((i+1)); done"},
		Dir: root, Reason: "默认 runner 的截断",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !resp.Truncated || len(resp.Stdout) != 64 {
		t.Fatalf("Truncated=%v len=%d, want true/64", resp.Truncated, len(resp.Stdout))
	}
}

func TestBrokerAppliesDefaultOutputLimit(t *testing.T) {
	policy, root := testPolicy(t)
	huge := strings.Repeat("y", escalate.DefaultMaxOutputBytes+1024)
	client := startBroker(t, policy, escalate.WithRunner(func(_ context.Context, _ escalate.Request) (escalate.Response, error) {
		return escalate.Response{Stdout: huge}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{ID: "t3", Argv: []string{"echo", "x"}, Dir: root, Reason: "默认上限"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !resp.Truncated || len(resp.Stdout) != escalate.DefaultMaxOutputBytes {
		t.Fatalf("默认上限未生效: truncated=%v len=%d", resp.Truncated, len(resp.Stdout))
	}
}

func TestBrokerTimeout(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()

	start := time.Now()
	resp, err := client.Exec(ctx, escalate.Request{
		ID: "to1", Argv: []string{"sh", "-c", "sleep 5"}, Dir: root, Reason: "超时测试", TimeoutMS: 50,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Denied {
		t.Fatalf("超时不是策略拒绝: %+v", resp)
	}
	if resp.ExitCode != -1 {
		t.Fatalf("超时 ExitCode = %d, want -1", resp.ExitCode)
	}
	mustContain(t, resp.Stderr, "超时")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("50ms 的超时拖了 %s：超时必须真的有界", elapsed)
	}
}

func TestBrokerDoesNotHangOnLingeringGrandchild(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()

	// 后台孙进程继承输出管道：没有 WaitDelay 时 Wait 会一直等到孙进程退出。
	start := time.Now()
	resp, err := client.Exec(ctx, escalate.Request{
		ID: "gc1", Argv: []string{"sh", "-c", "sleep 5 &"}, Dir: root, Reason: "孙进程占着输出管道",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("孙进程把一次立即结束的命令拖了 %s", elapsed)
	}
}

func TestBrokerClampsTimeoutToPolicyMax(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)
	policy.MaxTimeout = 50 * time.Millisecond
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()

	start := time.Now()
	resp, err := client.Exec(ctx, escalate.Request{
		ID: "to2", Argv: []string{"sh", "-c", "sleep 5"}, Dir: root, Reason: "策略上限应压过请求", TimeoutMS: 60000,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", resp.ExitCode)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("请求的 60s 超时未被策略上限压住，耗时 %s", elapsed)
	}
}

func TestBrokerLimitsConcurrency(t *testing.T) {
	policy, root := testPolicy(t)

	var mu sync.Mutex
	var inflight, peak, total int
	client := startBroker(t, policy, escalate.WithRunner(func(ctx context.Context, _ escalate.Request) (escalate.Response, error) {
		mu.Lock()
		inflight++
		total++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()

		time.Sleep(80 * time.Millisecond)

		mu.Lock()
		inflight--
		mu.Unlock()
		return escalate.Response{ExitCode: 0, Stdout: "ok"}, nil
	}))

	ctx, cancel := execCtx(t)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = client.Exec(ctx, escalate.Request{
				ID: "c" + string(rune('a'+i)), Argv: []string{"echo", "x"}, Dir: root, Reason: "并发测试",
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个请求失败: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if total != 5 {
		t.Fatalf("完成的请求数 = %d, want 5", total)
	}
	if peak > escalate.DefaultMaxConcurrent {
		t.Fatalf("并发峰值 = %d, 超过上限 %d", peak, escalate.DefaultMaxConcurrent)
	}
	if peak < 2 {
		t.Fatalf("并发峰值 = %d，未真正并行", peak)
	}
}

func TestBrokerAuditsEveryRequest(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)

	var mu sync.Mutex
	var events []escalate.Response
	client := startBroker(t, policy, escalate.WithAuditor(func(_ escalate.Request, resp escalate.Response) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, resp)
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	if _, err := client.Exec(ctx, escalate.Request{ID: "au1", Argv: []string{"rm", "-rf", "/"}, Dir: root, Reason: "会被拒绝"}); err != nil {
		t.Fatalf("Exec(denied): %v", err)
	}
	if _, err := client.Exec(ctx, escalate.Request{ID: "au2", Argv: []string{"echo", "hi"}, Dir: root, Reason: "会被执行"}); err != nil {
		t.Fatalf("Exec(allowed): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("审计事件数 = %d, want 2（拒绝也要审计）", len(events))
	}
	if !events[0].Denied {
		t.Fatalf("第一条应为拒绝事件: %+v", events[0])
	}
	if events[1].Denied {
		t.Fatalf("第二条应为放行事件: %+v", events[1])
	}
}

func TestBrokerRunnerErrorBecomesExitMinusOne(t *testing.T) {
	policy, root := testPolicy(t)
	client := startBroker(t, policy, escalate.WithRunner(func(_ context.Context, _ escalate.Request) (escalate.Response, error) {
		return escalate.Response{}, errors.New("spawn 失败: 找不到可执行文件")
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{ID: "e1", Argv: []string{"echo", "x"}, Dir: root, Reason: "执行错误"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Denied {
		t.Fatalf("执行错误不是策略拒绝: %+v", resp)
	}
	if resp.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", resp.ExitCode)
	}
	mustContain(t, resp.Stderr, "执行失败")
}

func TestBrokerFiltersClientEnv(t *testing.T) {
	policy, root := testPolicy(t)

	var mu sync.Mutex
	var seen map[string]string
	client := startBroker(t, policy, escalate.WithRunner(func(_ context.Context, req escalate.Request) (escalate.Response, error) {
		mu.Lock()
		seen = req.Env
		mu.Unlock()
		return escalate.Response{ExitCode: 0}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{
		ID: "env1", Argv: []string{"echo", "x"}, Dir: root, Reason: "环境过滤",
		Env: map[string]string{
			"GIT_FOO":               "keep-me",
			"AWS_SECRET_ACCESS_KEY": "s3cr3t",
			"PATH":                  "/tmp/evil",
		},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if seen["GIT_FOO"] != "keep-me" {
		t.Fatalf("白名单变量应传给 runner: %v", seen)
	}
	if _, ok := seen["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Fatal("非白名单变量必须被丢弃")
	}
	if _, ok := seen["PATH"]; ok {
		t.Fatal("客户端不得覆盖 PATH")
	}
	mustContain(t, resp.Stderr, "AWS_SECRET_ACCESS_KEY")
	mustContain(t, resp.Stderr, "PATH")
	if strings.Contains(resp.Stderr, "s3cr3t") {
		t.Fatal("诊断里不能出现被丢弃变量的值")
	}
}

func TestBrokerPassesWhitelistedEnvToChild(t *testing.T) {
	requireUnix(t)
	policy, root := testPolicy(t)
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{
		ID: "env2", Argv: []string{"sh", "-c", `printf %s "$GIT_FOO"`}, Dir: root, Reason: "环境应到达子进程",
		Env: map[string]string{"GIT_FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Stdout != "bar" {
		t.Fatalf("子进程 stdout = %q, want bar (stderr=%q)", resp.Stdout, resp.Stderr)
	}
}

func TestBrokerDirRevalidatedBeforeExec(t *testing.T) {
	policy, root := testPolicy(t)
	if err := mkdir(filepath.Join(root, "work")); err != nil {
		t.Fatal(err)
	}
	var gotDir string
	client := startBroker(t, policy, escalate.WithRunner(func(_ context.Context, req escalate.Request) (escalate.Response, error) {
		gotDir = req.Dir
		return escalate.Response{ExitCode: 0}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	if _, err := client.Exec(ctx, escalate.Request{
		ID: "dir1", Argv: []string{"echo", "x"}, Dir: filepath.Join(root, "work"), Reason: "dir 应被规范化",
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !filepath.IsAbs(gotDir) {
		t.Fatalf("传给 runner 的 Dir 必须是绝对路径: %q", gotDir)
	}
	if filepath.Base(gotDir) != "work" {
		t.Fatalf("传给 runner 的 Dir 不是预期目录: %q", gotDir)
	}
}
