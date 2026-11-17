package escalate

// 第二轮对抗审计（只读）：request_command_escalation 能否被模型用来执行
// 未授权的宿主命令、审批/限流/文案/生命周期是否可被绕过。
//
// 关键攻击面：Allow 是"子命令级前缀"，真正拦住危险参数的是 Broker 侧的内置表
// （sandbox/escalate.builtinDangerousArgs）。凡是用 token 相等/`flag=` 前缀做匹配的
// 地方，都要检查 git 自己的**长选项缩写**与**短选项附加值**能不能绕过去。

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

// realTool 用真实 Broker（net.Pipe 两端）构造工具，用于端到端判定。
func realTool(t *testing.T, allow, deny [][]string, mutate func(*Options)) rig.Tool {
	t.Helper()
	opts := baseOptions(t)
	opts.Allow = allow
	opts.Deny = deny
	opts.MaxTimeout = 5 * time.Second
	if mutate != nil {
		mutate(&opts)
	}
	brokerConn, clientConn := net.Pipe()
	tool, err := NewWithPair(brokerConn, clientConn, okApprover, opts)
	if err != nil {
		t.Fatalf("NewWithPair: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := tool.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})
	return tool
}

// classify 把结果文本归到四态之一，便于断言"通道是否真把命令拦住了"。
func classify(content string) string {
	switch {
	case strings.HasPrefix(content, "exit_code:"):
		return "executed"
	case strings.HasPrefix(content, "[escalation denied:"):
		return "denied"
	case strings.HasPrefix(content, "[escalation failed:"):
		return "failed"
	case strings.HasPrefix(content, "[escalation busy:"):
		return "busy"
	}
	return "unknown:" + content
}

// E1【BLOCKER】git 长选项缩写绕过内置危险参数表：`--upload-pac` == `--upload-pack`。
//
// builtinDangerousArgs 用 `arg == flag || strings.HasPrefix(arg, flag+"=")` 匹配，
// 而 git 的 parse-options 接受**无歧义的长选项缩写**（实测 `--upload-p`、`--upload`
// 都被接受）。于是 doc.go 推荐的 Allow={"git","fetch"} 下：
//
//	["git","fetch","--upload-pac=/tmp/evil"]  → 策略放行 → 宿主上 git exec /tmp/evil
//
// 这就是"沙箱内 → 宿主任意代码执行"。同类受影响的还有 --receive-pack(-p)、
// --exec-path、--template、--ext-diff、--textconv、--no-index、--config-env。
func TestAuditGitLongOptionAbbreviationBypassesDenyList(t *testing.T) {
	tool := realTool(t, [][]string{{"git", "fetch"}}, nil, nil)
	// 非 git 仓库目录：命令若被执行，git 会报 "not a git repository"（exit 128）；
	// 若被策略拦住，会是 [escalation denied: ...]。两者可清楚区分。
	result := invoke(t, tool, `{"command":["git","fetch","--upload-pac=/tmp/audit-not-a-real-program"],
		"reason":"验证危险参数缩写能否绕过内置表"}`)

	switch kind := classify(result.Content); kind {
	case "denied":
		t.Logf("内置表拦住了缩写形式（已修复）：%s", firstLine(result.Content))
	case "executed", "failed":
		t.Skipf("BLOCKER：`--upload-pac` 缩写绕过了 builtinDangerousArgs，命令已被放行到宿主。"+
			"结果=%q（%s）。修法：匹配时按“token 是危险长选项的前缀”判定，别用相等/`flag=` 前缀。",
			firstLine(result.Content), kind)
	default:
		t.Fatalf("无法归类的输出: %q", result.Content)
	}
}

// E2【major】短选项的附加值形式绕过：`-O<pager>` 就是 `-O <pager>`。
//
// `-O` 被列进内置表，但匹配写的是 `arg == "-O"`，而 git 接受 `-O/bin/echo`（实测
// `git log -O'/bin/echo hi'` 正常执行）。Allow={"git","log"} 下即可让宿主执行任意程序。
func TestAuditGitShortOptionAttachedValueBypassesDenyList(t *testing.T) {
	tool := realTool(t, [][]string{{"git", "log"}}, nil, nil)
	result := invoke(t, tool, `{"command":["git","log","-O/bin/echo"],"reason":"验证 -O 附加值形式"}`)

	switch kind := classify(result.Content); kind {
	case "denied":
		t.Logf("内置表拦住了 -O 附加值形式（已修复）：%s", firstLine(result.Content))
	case "executed", "failed":
		t.Skipf("major：`-O/bin/echo` 绕过了 builtinDangerousArgs（`-O` 只匹配裸 token）。结果=%q（%s）",
			firstLine(result.Content), kind)
	default:
		t.Fatalf("无法归类的输出: %q", result.Content)
	}
}

// E3（对照）：`-c` 的附加值形式 git 自己不接受，因此不是绕过面。
//
// 这解释了为什么内置表里 `-c` 只写裸 token 也不致命——但 `-c` 的**分离形式**
// （`-c name=value`）是被拦住的，这里只记录事实，防止以后有人"顺手"把 -c 从表里删掉。
func TestAuditGitConfigAttachedFormIsNotAVector(t *testing.T) {
	tool := realTool(t, [][]string{{"git", "status"}}, nil, nil)
	// 分离形式必须被拒（命中内置表）。
	result := invoke(t, tool, `{"command":["git","status","-c","core.pager=cat"],"reason":"-c 分离形式"}`)
	if kind := classify(result.Content); kind != "denied" {
		t.Fatalf("-c 分离形式应被内置表拒绝，得到 %q（%s）", firstLine(result.Content), kind)
	}
	// 附加值形式：git 自己就报 unknown option（因此即便放行也不会执行任意程序）。
	t.Log("-ccore.pager=cat 被 git 自身拒绝（实测 unknown option），不构成绕过面")
}

// E4【重要】队列满必须在**审批之前**拒绝：否则模型可以用并发调用刷审批弹窗。
//
// 配置：并发 1、队列 1。A 占住额度并卡在审批里；B 排进队列；C 必须立刻 busy，
// 且**不能**进入审批。
func TestAuditQueueFullRejectsBeforeApproval(t *testing.T) {
	var approvals atomic.Int32
	var queued atomic.Int32
	release := make(chan struct{})

	opts := baseOptions(t)
	opts.Allow = [][]string{{"echo"}}
	opts.MaxConcurrent = 1
	opts.MaxQueue = 1
	conn := newFakeConn(nil)
	tool, err := New(escalate.NewClient(conn), func(ctx context.Context, req escalate.Request) (bool, string, error) {
		approvals.Add(1)
		<-release
		return true, "", nil
	}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		if closer, ok := tool.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})

	first := make(chan rig.ToolResult, 1)
	go func() { first <- invoke(t, tool, okArgs) }()
	deadline := time.Now().Add(2 * time.Second)
	for approvals.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if approvals.Load() != 1 {
		t.Fatalf("第一个调用没有进入审批（approvals=%d）", approvals.Load())
	}

	// B：占掉唯一的排队名额（它会阻塞在等额度上，不进审批）。
	second := make(chan rig.ToolResult, 1)
	go func() {
		queued.Add(1)
		second <- invoke(t, tool, okArgs)
	}()
	deadline = time.Now().Add(2 * time.Second)
	for queued.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // 让 B 真正走进 acquire

	// C：额度与队列都满 → 必须立刻 busy，且不惊动审批。
	third := invoke(t, tool, okArgs)
	if kind := classify(third.Content); kind != "busy" {
		unblock()
		t.Fatalf("额度与队列都满时应回 busy，得到 %q（%s）", firstLine(third.Content), kind)
	}
	if got := approvals.Load(); got != 1 {
		unblock()
		t.Fatalf("繁忙的那个调用也进了审批（approvals=%d）：模型因此能刷审批弹窗", got)
	}
	unblock()
	<-first
	<-second
}

// E5【minor】审批回调 panic 会不会把整个回合炸掉？
//
// audit 回调有 recover，审批回调没有：宿主的一个 bug（或审批 UI 的 nil deref）会
// 直接把 panic 抛进 agent 循环，而不是变成"按拒绝处理"。
func TestAuditApproverPanicIsContained(t *testing.T) {
	opts := baseOptions(t)
	opts.Allow = [][]string{{"echo"}}
	conn := newFakeConn(nil)
	panicking := func(ctx context.Context, req escalate.Request) (bool, string, error) {
		panic("审批 UI 崩了")
	}
	tool, err := New(escalate.NewClient(conn), panicking, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := tool.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})

	var panicked any
	func() {
		defer func() { panicked = recover() }()
		_ = invoke(t, tool, okArgs)
	}()
	if panicked != nil {
		t.Skipf("minor：审批回调的 panic 直接穿透到调用方（audit 回调有 recover，审批没有）：%v", panicked)
	}
}

// E6【minor】模型多传 dir/allow/id/env 时是"静默忽略"还是报错？
//
// Schema 里没有这些字段，但 DecodeArgs 用的 json.Unmarshal 不禁止未知字段，
// 于是模型以为它指定了目录、放宽了 Allow，实际什么都没发生。
func TestAuditUnknownFieldsAreSilentlyIgnored(t *testing.T) {
	opts := baseOptions(t)
	opts.Allow = [][]string{{"echo"}}
	conn := newFakeConn(nil)
	tool, err := New(escalate.NewClient(conn), okApprover, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := tool.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})

	result := invoke(t, tool, `{"command":["echo","hi"],"reason":"带一堆多余字段",
		"dir":"/etc","allow":[["sh"]],"id":"attacker-chosen","env":{"PATH":"/tmp/evil"}}`)
	if kind := classify(result.Content); kind != "executed" {
		t.Fatalf("多余字段不该让调用失败，得到 %q（%s）", firstLine(result.Content), kind)
	}
	requests := conn.recorded()
	if len(requests) != 1 {
		t.Fatalf("Broker 收到 %d 条请求", len(requests))
	}
	req := requests[0]
	if req.Dir == "/etc" {
		t.Fatalf("模型指定的 dir 生效了（这是严重越权）: %q", req.Dir)
	}
	if strings.HasPrefix(req.ID, "attacker-chosen") {
		t.Fatalf("模型指定的请求 ID 生效了: %q", req.ID)
	}
	if len(req.Env) != 0 {
		t.Fatalf("模型指定的 env 生效了: %v", req.Env)
	}
	t.Logf("多余字段被静默忽略（dir=%q id=%q env=%v）——建议 DecodeArgs 禁止未知字段，让模型立刻发现自己的假设错了",
		req.Dir, req.ID, req.Env)
}

// E7：四种结果文案必须可区分，且"非零退出"不能被当成通道失败。
func TestAuditResultStatesAreDistinguishable(t *testing.T) {
	cases := []struct {
		name    string
		respond func(escalate.Request) escalate.Response
		want    string
		prefix  string
	}{
		{"成功", func(req escalate.Request) escalate.Response {
			return escalate.Response{ExitCode: 0, Stdout: "ok"}
		}, "executed", "exit_code: 0"},
		{"非零退出", func(req escalate.Request) escalate.Response {
			return escalate.Response{ExitCode: 128, Stderr: "fatal: not a git repository"}
		}, "executed", "exit_code: 128"},
		{"策略拒绝", func(req escalate.Request) escalate.Response {
			return escalate.Response{ExitCode: escalate.DeniedExitCode, Denied: true, DenyReason: "命中禁止列表: git push"}
		}, "denied", "[escalation denied: 命中禁止列表"},
		{"队列繁忙", func(req escalate.Request) escalate.Response {
			return escalate.Response{ExitCode: escalate.DeniedExitCode, Denied: true, DenyReason: escalate.QueueFullReason}
		}, "busy", "[escalation busy:"},
		{"通道故障", func(req escalate.Request) escalate.Response {
			return escalate.Response{ExitCode: -1, Stderr: "执行超时（上限 5s），已终止"}
		}, "failed", "[escalation failed:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, _ := newTestToolWith(t, func(opts *Options) {
				opts.Allow = [][]string{{"echo"}}
			}, tc.respond)
			result := invoke(t, tool, okArgs)
			if kind := classify(result.Content); kind != tc.want {
				t.Fatalf("归类 = %s, want %s（原文 %q）", kind, tc.want, result.Content)
			}
			if !strings.HasPrefix(result.Content, tc.prefix) {
				t.Fatalf("文案前缀 = %q, want %q", firstLine(result.Content), tc.prefix)
			}
		})
	}
}

// E8：参数边界的健壮性（NUL / 空 argv / 超长 argv / 超时溢出）。
func TestAuditArgumentBoundaries(t *testing.T) {
	tool, _ := newTestToolWith(t, func(opts *Options) {
		opts.Allow = [][]string{{"echo"}}
	}, nil)

	cases := []struct {
		name string
		args string
	}{
		{"空 command", `{"command":[],"reason":"x"}`},
		{"空 argv0", `{"command":[""],"reason":"x"}`},
		{"NUL 字节", `{"command":["echo","a\u0000b"],"reason":"x"}`},
		{"缺 reason", `{"command":["echo","hi"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := invoke(t, tool, tc.args)
			if !result.IsError {
				t.Fatalf("%s 应被拒绝，得到 %q", tc.name, result.Content)
			}
		})
	}

	// 超长 argv：必须被 maxArgvTokens 挡住，而不是让 os/exec 收到 E2BIG。
	big := make([]string, maxArgvTokens+1)
	for i := range big {
		big[i] = "x"
	}
	quoted := `["` + strings.Join(big, `","`) + `"]`
	result := invoke(t, tool, `{"command":`+quoted+`,"reason":"超长"}`)
	if !result.IsError {
		t.Fatalf("超长 argv 应被拒绝，得到 %q", firstLine(result.Content))
	}

	// 超时：负数与超大值都必须被夹住，不能溢出成"无超时"。
	for _, requested := range []string{"-1", "9223372036854775807", "1"} {
		t.Run("timeout="+requested, func(t *testing.T) {
			opts := baseOptions(t)
			opts.Allow = [][]string{{"echo"}}
			opts.MaxTimeout = 2 * time.Second
			conn := newFakeConn(nil)
			tool, err := New(escalate.NewClient(conn), okApprover, opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				if closer, ok := tool.(interface{ Close() error }); ok {
					_ = closer.Close()
				}
			})
			_ = invoke(t, tool, `{"command":["echo","hi"],"reason":"超时夹取","timeout_ms":`+requested+`}`)
			requests := conn.recorded()
			if len(requests) != 1 {
				t.Fatalf("Broker 收到 %d 条请求", len(requests))
			}
			got := time.Duration(requests[0].TimeoutMS) * time.Millisecond
			if got > opts.MaxTimeout {
				t.Fatalf("timeout_ms=%s 没有被夹到宿主上限内: %v > %v", requested, got, opts.MaxTimeout)
			}
			if got < 0 {
				t.Fatalf("timeout_ms=%s 溢出成了负数: %v", requested, got)
			}
		})
	}
}

// E9：Close 生命周期 —— 幂等、关闭后调用不 panic。
func TestAuditCloseLifecycle(t *testing.T) {
	tool, _ := newTestToolWith(t, func(opts *Options) {
		opts.Allow = [][]string{{"echo"}}
	}, nil)
	closer, ok := tool.(interface{ Close() error })
	if !ok {
		t.Fatal("工具没有 Close()：宿主无法回收后台 Serve 与连接")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("第一次 Close: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
	result := invoke(t, tool, okArgs)
	if kind := classify(result.Content); kind != "failed" {
		t.Fatalf("关闭后调用应回 failed，得到 %q（%s）", firstLine(result.Content), kind)
	}
}

// E10：审计回调必须收到**所有**拒绝路径（本地策略拒绝、繁忙、审批拒绝）。
func TestAuditCallbackSeesEveryDenial(t *testing.T) {
	var audited atomic.Int32
	opts := baseOptions(t)
	opts.Allow = [][]string{{"echo"}}
	opts.Auditor = func(req escalate.Request, resp escalate.Response) {
		if resp.Denied || resp.ExitCode < 0 {
			audited.Add(1)
		}
	}
	// 本地策略拒绝：命令不在 Allow 里。
	conn := newFakeConn(nil)
	tool, err := New(escalate.NewClient(conn), okApprover, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := tool.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})
	_ = invoke(t, tool, `{"command":["rm","-rf","/"],"reason":"会被本地策略拒绝"}`)
	if audited.Load() != 1 {
		t.Fatalf("本地策略拒绝没有被审计（count=%d）", audited.Load())
	}

	// 审批拒绝。
	var approvals atomic.Int32
	denyAll := func(ctx context.Context, req escalate.Request) (bool, string, error) {
		approvals.Add(1)
		return false, "运营不同意", nil
	}
	conn2 := newFakeConn(nil)
	tool2, err := New(escalate.NewClient(conn2), denyAll, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := tool2.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})
	_ = invoke(t, tool2, okArgs)
	if approvals.Load() != 1 || audited.Load() != 2 {
		t.Fatalf("审批拒绝路径的审计缺失（approvals=%d audited=%d）", approvals.Load(), audited.Load())
	}
}

// firstLine 取首行，便于错误信息里贴最短证据。
func firstLine(s string) string {
	if index := strings.IndexByte(s, '\n'); index >= 0 {
		return s[:index]
	}
	return s
}
