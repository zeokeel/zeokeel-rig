package escalate_test

// 对抗审计：正面攻击清单。
//
// 每个用例都是"攻击者视角的一次尝试"，断言的是**修好之后**应有的行为（拒绝/丢弃/
// 不挂死）。文件名以 audit_ 开头，与产品测试区分；不带 build tag，全平台编译，
// 依赖真实 git 的用例在缺少 git 时跳过。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

// ---------- 攻击 1：Deny 被"子命令之前的全局选项"绕过 ----------

// TestAuditDenyBypassGlobalOption 验证：策略用 Deny 摘掉危险子命令时，攻击者不能靠
// 在子命令前插入全局选项（git 的 -c/-C/--git-dir/--no-pager …）来跳过位置匹配。
//
// 攻击：Allow={"git"} + Deny={"git","push"}（policy.go 文档推荐的"宽 Allow + 摘危险
// 子命令"用法），请求 `git -c core.pager='sh -c …' push --force`。
// 期望：拒绝。修复前：Allow（Deny 在 argv[1] 处就失配）。
func TestAuditDenyBypassGlobalOption(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{
		Allow: [][]string{{"git"}},
		Deny:  [][]string{{"git", "push"}},
		Roots: []string{root},
	}
	attacks := [][]string{
		{"git", "-c", "core.pager=sh -c 'id'", "push", "--force", "origin", "main"},
		{"git", "-C", "/tmp", "push", "origin", "main"},
		{"git", "--git-dir=/tmp/x", "push"},
		{"git", "--work-tree=/tmp/y", "push"},
		{"git", "--no-pager", "push"},
		{"git", "--exec-path=/tmp/evil", "push"},
	}
	for _, argv := range attacks {
		req := escalate.Request{ID: "atk", Argv: argv, Dir: root, Reason: "攻击: 全局选项绕过 Deny"}
		if decision := policy.Decide(req); decision.Allow {
			t.Errorf("Deny 被绕过: %q 仍然被允许（%s）", strings.Join(argv, " "), decision.Reason)
		}
	}
	// 反向断言：不带全局选项时该允许的仍然允许、该拒绝的仍然拒绝。
	if d := policy.Decide(escalate.Request{Argv: []string{"git", "status"}, Dir: root, Reason: "正常"}); !d.Allow {
		t.Fatalf("git status 不该被拒: %s", d.Reason)
	}
	if d := policy.Decide(escalate.Request{Argv: []string{"git", "push"}, Dir: root, Reason: "正常"}); d.Allow {
		t.Fatal("git push 应当被 Deny")
	}
}

// ---------- 攻击 2：Allow 前缀之后的参数把命令变成执行器 ----------

// TestAuditUploadPackIsDenied 验证：即使 Allow 只写了 {"git","fetch"}（doc.go 的
// 示例配置），`git fetch --upload-pack=<任意程序>` 也不能执行宿主程序。
//
// 攻击：--upload-pack 指向沙箱自己写出来的脚本 → 宿主身份执行 → 完全逃逸。
func TestAuditUploadPackIsDenied(t *testing.T) {
	requireUnix(t)
	work, bare, outside := gitLab(t)
	policy := escalate.Policy{
		Allow: [][]string{{"git", "fetch"}},
		Roots: []string{work},
	}
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()
	marker := filepath.Join(outside, "upload-pack-pwned.txt")
	script := filepath.Join(outside, "evil-upload-pack.sh")
	writeExecutable(t, script, "#!/bin/sh\necho pwned > "+marker+"\n")

	resp, err := client.Exec(ctx, escalate.Request{
		Argv: []string{"git", "fetch", "--upload-pack=" + script, "file://" + bare},
		Dir:  work, Reason: "攻击: --upload-pack 执行任意程序",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !resp.Denied {
		t.Fatalf("--upload-pack 未被拒绝（exit=%d stdout=%q stderr=%q）", resp.ExitCode, resp.Stdout, resp.Stderr)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("攻击成功：--upload-pack 指定的程序真的在宿主上执行了")
	}
}

// TestAuditGitExecFlagsAreDenied 覆盖同一类的其它"把 git 变成执行器"的参数。
func TestAuditGitExecFlagsAreDenied(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{
		Allow: [][]string{{"git"}},
		Roots: []string{root},
	}
	for _, flag := range []string{
		"--upload-pack=/tmp/x", "--receive-pack=/tmp/x", "--exec-path=/tmp/x",
		"--template=/tmp/x", "-c", "--config-env=core.pager=EDITOR",
		"--ext-diff", "--textconv", "--open-files-in-pager=sh", "-O",
	} {
		argv := []string{"git", "fetch", flag, "origin"}
		if d := policy.Decide(escalate.Request{Argv: argv, Dir: root, Reason: "攻击"}); d.Allow {
			t.Errorf("危险参数未被拦: %q", strings.Join(argv, " "))
		}
	}
}

// TestAuditGitWriteAndReadEscapesAreDenied 验证：被允许的只读 git 子命令不能借
// --output / --no-index 在 Roots 之外写或读。
func TestAuditGitWriteAndReadEscapesAreDenied(t *testing.T) {
	requireUnix(t)
	work, _, outside := gitLab(t)
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-OUTSIDE-ROOTS\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "written-by-git.txt")

	policy := escalate.Policy{
		Allow: [][]string{{"git", "log"}, {"git", "diff"}},
		Roots: []string{work},
	}
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()

	resp, err := client.Exec(ctx, escalate.Request{
		Argv: []string{"git", "log", "--output=" + target, "-1"},
		Dir:  work, Reason: "攻击: --output 写到 Roots 之外",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !resp.Denied {
		t.Fatalf("--output 未被拒绝（exit=%d）", resp.ExitCode)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("攻击成功：git log --output 在 Roots 之外写了文件")
	}

	resp, err = client.Exec(ctx, escalate.Request{
		Argv: []string{"git", "diff", "--no-index", "--", secret, "/dev/null"},
		Dir:  work, Reason: "攻击: --no-index 读 Roots 之外",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !resp.Denied {
		t.Fatalf("--no-index 未被拒绝（exit=%d）", resp.ExitCode)
	}
	if strings.Contains(resp.Stdout, "TOP-SECRET") {
		t.Fatal("攻击成功：--no-index 把 Roots 之外的文件内容读出来了")
	}
}

// ---------- 攻击 3：环境变量注入 ----------

// TestAuditEnvDangerousNamesCaseInsensitive 验证高危变量名按大小写不敏感匹配：
// Windows 的环境块不区分大小写，`GIT_ssh_command` 在那里就是 GIT_SSH_COMMAND。
func TestAuditEnvDangerousNamesCaseInsensitive(t *testing.T) {
	policy := escalate.Policy{Allow: [][]string{{"true"}}}
	for _, key := range []string{
		"GIT_SSH_COMMAND", "GIT_ssh_command", "git_ssh_command", "Git_Ssh_Command",
		"GIT_EXEC_PATH", "git_exec_path", "GIT_CONFIG_KEY_0", "git_config_value_0",
		"GIT_PAGER", "git_pager", "GIT_ASKPASS", "GIT_TRACE2_EVENT",
	} {
		kept, dropped := policy.FilterEnv(map[string]string{key: "sh -c 'id'"})
		if _, ok := kept[key]; ok {
			t.Errorf("高危变量未被丢弃: %q", key)
		}
		if len(dropped) != 1 {
			t.Errorf("丢弃列表 = %v，期望包含 %q", dropped, key)
		}
	}
	// 正常变量仍然保留（否则过滤器会把功能也封掉）。
	kept, _ := policy.FilterEnv(map[string]string{"GIT_AUTHOR_NAME": "n", "LC_ALL": "C"})
	if len(kept) != 2 {
		t.Fatalf("正常变量被误删: %v", kept)
	}
}

// TestAuditEnvInvalidKeysDropped 验证畸形键被丢弃：含 NUL、含 '='、含空格、
// 以数字开头、空键。含 '=' 的键会改写变量名，含 NUL 会在 env 块里截断出一条
// 新变量（LD_PRELOAD 注入）。
func TestAuditEnvInvalidKeysDropped(t *testing.T) {
	policy := escalate.Policy{Allow: [][]string{{"true"}}}
	for _, key := range []string{
		"GIT_A\x00LD_PRELOAD",
		"GIT_A=1",
		"GIT A",
		"1GIT_A",
		"",
		"GIT_\xff",
	} {
		kept, _ := policy.FilterEnv(map[string]string{key: "/tmp/evil.so"})
		if _, ok := kept[key]; ok {
			t.Errorf("畸形环境变量键未被丢弃: %q", key)
		}
	}
}

// TestAuditEnvValuesReachChildSafely 验证：过滤后的环境真的进了子进程，且基线里
// 没有把宿主无关变量抖给客户端。
func TestAuditEnvValuesReachChildSafely(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	policy := escalate.Policy{
		Allow: [][]string{{"sh", "-c"}},
		Roots: []string{root},
		// 故意只留最小基线环境，避免依赖宿主 PATH。
		BaseEnv: []string{"PATH=/usr/bin:/bin"},
	}
	client := startBroker(t, policy)
	ctx, cancel := execCtx(t)
	defer cancel()
	resp, err := client.Exec(ctx, escalate.Request{
		Argv: []string{"sh", "-c", "printf '%s' \"$GIT_AUTHOR_NAME-${LD_PRELOAD:-unset}\""},
		Dir:  root, Reason: "环境变量注入检查",
		Env: map[string]string{
			"GIT_AUTHOR_NAME": "kept",
			"LD_PRELOAD":      "/tmp/evil.so",
		},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Denied {
		t.Fatalf("不该被拒: %s", resp.DenyReason)
	}
	if got := strings.TrimSpace(resp.Stdout); got != "kept-unset" {
		t.Fatalf("子进程环境 = %q，期望 %q", got, "kept-unset")
	}
}

// ---------- 攻击 4：协议与资源 ----------

// TestAuditOversizeRequestDoesNotHang 验证：超长请求（>1MiB 单行）不能让调用方
// 永久挂住。broker 会回一条无法配对（ID 为空）的 Denied，客户端必须把它当成
// 通道级错误而不是静默丢弃。
func TestAuditOversizeRequestDoesNotHang(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}
	client := startBroker(t, policy)

	done := make(chan struct{})
	var (
		resp escalate.Response
		err  error
	)
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		resp, err = client.Exec(ctx, escalate.Request{
			Argv:   []string{"echo", strings.Repeat("A", escalate.MaxLineBytes+1024)},
			Dir:    root,
			Reason: "攻击: 超长请求",
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("超长请求把 Exec 永久挂住了")
	}
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec 因超长请求挂到 ctx 超时（客户端把无法配对的 Denied 丢掉了）: %v", err)
	}
	if err == nil && !resp.Denied {
		t.Fatalf("超长请求既没有报错也没有被拒: %+v", resp)
	}
}

// TestAuditProtocolErrorsReachAuditor 验证：坏 JSON 与超长行这两类"没进策略判定"的
// 拒绝也必须进审计（doc.go 声称每个请求都会调用一次 auditor）。
func TestAuditProtocolErrorsReachAuditor(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}

	var mu sync.Mutex
	var audited []escalate.Response
	auditor := func(_ escalate.Request, resp escalate.Response) {
		mu.Lock()
		audited = append(audited, resp)
		mu.Unlock()
	}
	conn, _ := startRawServer(t, policy, escalate.WithAuditor(auditor))
	reader := bufio.NewReader(conn)

	rawRoundTrip(t, conn, reader, `{"id":"ok","argv":["echo","hi"],"dir":"`+root+`","reason":"正常"}`)
	rawRoundTrip(t, conn, reader, `{ this is not json`)
	rawRoundTrip(t, conn, reader, `{"id":"`+strings.Repeat("x", escalate.MaxLineBytes+16)+`"}`)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		count := len(audited)
		mu.Unlock()
		if count >= 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("审计条数 = %d，期望 3（正常 + 坏 JSON + 超长行）", count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAuditAuditorPanicDoesNotKillServe 验证：宿主回调 panic 不能拖垮整个 Serve
// （否则一个写错的审计函数就让提权通道静默失效，甚至杀掉宿主进程）。
func TestAuditAuditorPanicDoesNotKillServe(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}

	var calls atomic.Int32
	auditor := func(escalate.Request, escalate.Response) {
		if calls.Add(1) == 1 {
			panic("审计器故意炸一次")
		}
	}
	client := startBroker(t, policy, escalate.WithAuditor(auditor))
	ctx, cancel := execCtx(t)
	defer cancel()

	if _, err := client.Exec(ctx, escalate.Request{Argv: []string{"echo", "one"}, Dir: root, Reason: "第一次"}); err != nil {
		t.Fatalf("第一次请求: %v", err)
	}
	// panic 之后连接必须仍然可用。
	resp, err := client.Exec(ctx, escalate.Request{Argv: []string{"echo", "two"}, Dir: root, Reason: "第二次"})
	if err != nil {
		t.Fatalf("审计器 panic 后连接不可用: %v", err)
	}
	if resp.Denied || !strings.Contains(resp.Stdout, "two") {
		t.Fatalf("审计器 panic 后的请求结果异常: %+v", resp)
	}
}

// TestAuditTimeoutOverflowIsClamped 验证 TimeoutMS 极大值不会因 duration 溢出变成
// 负超时（会让命令被立刻杀掉）。
func TestAuditTimeoutOverflowIsClamped(t *testing.T) {
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
		Argv:      []string{"sh", "-c", "sleep 0.05; echo done"},
		Dir:       root,
		Reason:    "溢出检查",
		TimeoutMS: 1<<62 - 1,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.ExitCode != 0 || !strings.Contains(resp.Stdout, "done") {
		t.Fatalf("超大 TimeoutMS 被溢出成负值，命令被立刻杀掉: exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

// TestAuditConcurrencyLimit 复核作者的并发额度声明：50 个并发请求，同时在执行的
// 不超过 MaxConcurrent（这里是 4）。
func TestAuditConcurrencyLimit(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	policy := escalate.Policy{
		Allow:         [][]string{{"sh", "-c"}},
		Roots:         []string{root},
		MaxConcurrent: 4,
		MaxTimeout:    10 * time.Second,
	}
	var inFlight, peak atomic.Int32
	client := startBroker(t, policy, escalate.WithRunner(func(ctx context.Context, req escalate.Request) (escalate.Response, error) {
		now := inFlight.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return escalate.Response{Stdout: "ok"}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = client.Exec(ctx, escalate.Request{
				ID: fmt.Sprintf("c-%d", i), Argv: []string{"sh", "-c", "true"},
				Dir: root, Reason: "并发压测",
			})
		}(i)
	}
	wg.Wait()
	if got := peak.Load(); got > 4 {
		t.Fatalf("并发峰值 = %d，超过 MaxConcurrent=4", got)
	}
	if got := peak.Load(); got < 2 {
		t.Fatalf("并发峰值 = %d，额度根本没被用起来（测试无效）", got)
	}
}

// TestAuditHalfLineRequestIsServed 验证：客户端只写半行（没有换行）然后 CloseWrite，
// broker 仍要把它当成一条完整请求处理，而不是丢进黑洞。
func TestAuditHalfLineRequestIsServed(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}
	broker, err := escalate.NewBroker(policy)
	if err != nil {
		t.Fatalf("NewBroker: %v", err)
	}
	socket := filepath.Join(t.TempDir(), "escalate.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix socket 不可用: %v", err)
	}
	defer func() { _ = listener.Close() }()
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		serverErr <- broker.Serve(context.Background(), conn)
	}()

	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	line, _ := json.Marshal(escalate.Request{ID: "half", Argv: []string{"echo", "hi"}, Dir: root, Reason: "半行"})
	if _, err := conn.Write(line); err != nil { // 故意不写换行
		t.Fatalf("write: %v", err)
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	respLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("半行请求没有得到响应: %v", err)
	}
	var resp escalate.Response
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %q", respLine)
	}
	if resp.ID != "half" || !strings.Contains(resp.Stdout, "hi") {
		t.Fatalf("响应异常: %+v", resp)
	}
}

// TestAuditTruncationKeepsValidUTF8 验证截断点不劈开多字节字符（doc.go 的声明）。
func TestAuditTruncationKeepsValidUTF8(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{
		Allow:          [][]string{{"echo"}},
		Roots:          []string{root},
		MaxOutputBytes: 7, // 让 4 字节的 emoji 正好被劈开
	}
	client := startBroker(t, policy, escalate.WithRunner(func(context.Context, escalate.Request) (escalate.Response, error) {
		return escalate.Response{Stdout: "ab😀cd"}, nil
	}))
	ctx, cancel := execCtx(t)
	defer cancel()
	resp, err := client.Exec(ctx, escalate.Request{Argv: []string{"echo", "x"}, Dir: root, Reason: "截断"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !resp.Truncated {
		t.Fatalf("没有标记截断: %+v", resp)
	}
	if !utf8ValidString(resp.Stdout) {
		t.Fatalf("截断结果不是合法 UTF-8: %q", resp.Stdout)
	}
}

// TestAuditUnknownFieldsIgnored 复核作者的前后兼容声明。
func TestAuditUnknownFieldsIgnored(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Allow: [][]string{{"echo"}}, Roots: []string{root}}
	conn, _ := startRawServer(t, policy)
	reader := bufio.NewReader(conn)
	line := `{"id":"u1","argv":["echo","hi"],"dir":"` + root + `","reason":"未知字段","future":{"x":[1,2]},"extra":true}`
	raw := rawRoundTrip(t, conn, reader, line)
	var resp escalate.Response
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %q", raw)
	}
	if resp.Denied || resp.ID != "u1" {
		t.Fatalf("未知字段导致请求失败: %+v", resp)
	}
}

// ---------- 测试辅助 ----------

// gitLab 搭一个"根目录内的工作仓库 + 根目录外的一个空目录"，返回工作仓库、
// 裸仓库与根外目录。缺少 git 时跳过用例。
func gitLab(t *testing.T) (work, bare, outside string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git，跳过")
	}
	base := t.TempDir()
	work = filepath.Join(base, "work")
	bare = filepath.Join(base, "bare.git")
	outside = filepath.Join(base, "outside")
	for _, dir := range []string{work, outside} {
		if err := mkdir(dir); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	runGit(t, base, "init", "-q", "--bare", bare)
	runGit(t, work, "init", "-q", ".")
	runGit(t, work, "config", "user.email", "audit@example.com")
	runGit(t, work, "config", "user.name", "audit")
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit(t, work, "add", "f.txt")
	runGit(t, work, "commit", "-qm", "one")
	return work, bare, outside
}

// runGit 在 dir 里跑一条 git 命令，失败即终止用例。
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeExecutable 写一个可执行脚本。
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// utf8ValidString 报告 s 是否是合法 UTF-8。
func utf8ValidString(s string) bool {
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			return false
		}
		s = s[size:]
	}
	return true
}
