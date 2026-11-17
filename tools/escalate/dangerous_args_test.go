package escalate

// 本文件钉死"危险参数形状"的判定：长选项缩写（--upload-pac）与短选项附加值
// （-O/bin/echo）都必须被拦住。第二轮对抗审计报告了这条 P0（宿主 RCE），
// 这里用穷举缩写 + 真实 Broker 端到端两条路径防回归。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox/escalate"
)

// dangerousLongFlags 是内置表里的 git 长选项（与 sandbox/escalate 的表保持一致）。
var dangerousLongFlags = []string{
	"--upload-pack", "--receive-pack", "--exec-path", "--template",
	"--config-env", "--ext-diff", "--textconv", "--open-files-in-pager",
	"--output", "--no-index", "--git-dir", "--work-tree",
}

// TestDangerousArgLongOptionAbbreviations 对每个危险长选项枚举其所有前缀缩写，
// 断言都被判定为危险（git 的 parse-options 接受无歧义缩写）。
func TestDangerousArgLongOptionAbbreviations(t *testing.T) {
	for _, flag := range dangerousLongFlags {
		flag := flag
		for length := 3; length < len(flag); length++ {
			abbreviation := flag[:length]
			t.Run(abbreviation, func(t *testing.T) {
				hit, dangerous := escalate.MatchDangerousArg("git", abbreviation)
				if !dangerous {
					t.Fatalf("缩写 %q 未被判定为危险（完整选项 %q）", abbreviation, flag)
				}
				// 缩写可能有歧义（--e 同时是 --exec-path 与 --ext-diff 的前缀），
				// 命中的条目只要求也是该缩写的候选之一。
				if !strings.HasPrefix(flag, abbreviation) || !strings.HasPrefix(hit, abbreviation) {
					t.Fatalf("缩写 %q 命中的条目 = %q（完整选项 %q）", abbreviation, hit, flag)
				}
				// 等号形式（缩写 + 值）同样危险。
				withValue := abbreviation + "=/tmp/evil"
				if _, dangerous := escalate.MatchDangerousArg("git", withValue); !dangerous {
					t.Fatalf("%q 未被判定为危险", withValue)
				}
			})
		}
	}
}

// TestDangerousArgShortOptionAttachedValues 覆盖短选项的附加值形式。
func TestDangerousArgShortOptionAttachedValues(t *testing.T) {
	cases := map[string]string{
		"-O":              "-O",
		"-O/bin/echo":     "-O",
		"-O/bin/echo -n":  "-O",
		"-c":              "-c",
		"-ccore.pager=x":  "-c",
		"-c=core.pager=x": "-c",
	}
	for arg, want := range cases {
		arg, want := arg, want
		t.Run(arg, func(t *testing.T) {
			hit, dangerous := escalate.MatchDangerousArg("git", arg)
			if !dangerous || hit != want {
				t.Fatalf("MatchDangerousArg(git, %q) = (%q,%v), want (%q,true)", arg, hit, dangerous, want)
			}
		})
	}
}

// TestDangerousArgAllowsSafeForms 保证没有过度拒绝（真实使用里这些必须放行）。
func TestDangerousArgAllowsSafeForms(t *testing.T) {
	safe := []string{
		"--depth=1", "--forc", "--verbos", "--all", "--no-single-branch",
		"--upload-package", // 比 --upload-pack 更长，不是 git 的缩写形式
		"origin", "main", "-p", "--prune", "-v",
	}
	for _, arg := range safe {
		arg := arg
		t.Run(arg, func(t *testing.T) {
			if hit, dangerous := escalate.MatchDangerousArg("git", arg); dangerous {
				t.Fatalf("%q 被误判为危险参数（命中 %q）", arg, hit)
			}
		})
	}
	// 非 git 程序没有危险参数表。
	if _, dangerous := escalate.MatchDangerousArg("echo", "--upload-pack=/x"); dangerous {
		t.Fatal("echo 不该命中 git 的危险参数表")
	}
}

// TestDangerousArgNormalizesProgram 确认程序名规范化：绝对路径/大写/扩展名都要
// 命中同一张表（否则换一种写法的 Allow 就能绕过 Broker 的表）。
func TestDangerousArgNormalizesProgram(t *testing.T) {
	for _, program := range []string{"git", "/usr/bin/git", "GIT", "git.exe", `C:\Program Files\Git\git.exe`} {
		program := program
		t.Run(program, func(t *testing.T) {
			if _, dangerous := escalate.MatchDangerousArg(program, "--upload-pac=/x"); !dangerous {
				t.Fatalf("%q 未被规范化命中危险参数表", program)
			}
		})
	}
}

// TestInvokeDeniesAbbreviatedDangerousArg 是 P0 的工具层回归：Allow 命中之后、
// 审批与通道之前，危险参数必须被本地拒绝。
func TestInvokeDeniesAbbreviatedDangerousArg(t *testing.T) {
	var approvals atomic.Int32
	opts := baseOptions(t)
	opts.Allow = [][]string{{"git", "fetch"}}
	conn := newFakeConn(nil)
	tool, err := New(escalate.NewClient(conn), func(context.Context, escalate.Request) (bool, string, error) {
		approvals.Add(1)
		return true, "", nil
	}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := tool.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})

	attacks := []string{
		`{"command":["git","fetch","--upload-pac=/tmp/evil"],"reason":"r"}`,
		`{"command":["git","fetch","--upload-p=/tmp/evil"],"reason":"r"}`,
		`{"command":["git","fetch","--upload=/tmp/evil"],"reason":"r"}`,
		`{"command":["git","fetch","--upload-pack=/tmp/evil"],"reason":"r"}`,
		`{"command":["git","fetch","--no-inde"],"reason":"r"}`,
		`{"command":["git","fetch","--exec-pat=/tmp"],"reason":"r"}`,
		`{"command":["git","fetch","--config-env=x"],"reason":"r"}`,
		`{"command":["git","fetch","--git-dir=/outside/repo"],"reason":"r"}`,
		`{"command":["git","fetch","--work-tre=/outside"],"reason":"r"}`,
	}
	for _, arguments := range attacks {
		arguments := arguments
		t.Run(arguments, func(t *testing.T) {
			result := invoke(t, tool, arguments)
			if !strings.HasPrefix(result.Content, DeniedPrefix) {
				t.Fatalf("必须本地拒绝，实际 %q", result.Content)
			}
			if !strings.Contains(result.Content, "内置危险参数") {
				t.Fatalf("拒绝原因应指出危险参数: %q", result.Content)
			}
		})
	}
	if got := approvals.Load(); got != 0 {
		t.Fatalf("危险参数不该进入审批（approvals=%d）", got)
	}
	if len(conn.recorded()) != 0 {
		t.Fatalf("危险参数不该发往通道（%d 条请求）", len(conn.recorded()))
	}
	// 对照：合法的 git fetch 仍然可以走完全流程。
	if result := invoke(t, tool, `{"command":["git","fetch","--depth=1","origin"],"reason":"r"}`); result.IsError {
		t.Fatalf("合法 fetch 不该被拒: %q", result.Content)
	}
	if got := approvals.Load(); got != 1 {
		t.Fatalf("合法请求应进入审批一次（approvals=%d）", got)
	}
}

// TestBrokerDeniesAbbreviatedDangerousArg 是 Broker 层的同一回归（纵深防御：
// 即使请求不是经过本工具发出的，宿主 Broker 也必须拦住）。
func TestBrokerDeniesAbbreviatedDangerousArg(t *testing.T) {
	root := t.TempDir()
	policy := escalate.Policy{Roots: []string{root}, Allow: [][]string{{"git", "fetch"}}}
	for _, arg := range []string{
		"--upload-pac=/tmp/x", "--upload-p=/tmp/x", "--upload=/tmp/x",
		"--receive-pac=x", "--exec-pat=/tmp", "--templat=/tmp", "--ext-dif",
		"--textcon", "--no-inde", "--config-env=x",
		"--git-dir=/outside/repo", "--work-tre=/outside",
	} {
		arg := arg
		t.Run(arg, func(t *testing.T) {
			decision := policy.Decide(escalate.Request{
				ID: "r-" + arg, Argv: []string{"git", "fetch", arg}, Dir: root, Reason: "攻击",
			})
			if decision.Allow {
				t.Fatalf("Broker 放行了危险参数 %q", arg)
			}
			if !strings.Contains(decision.Reason, "内置危险参数") {
				t.Fatalf("拒绝原因不对: %q", decision.Reason)
			}
		})
	}
}

// TestApproverPanicBecomesDenial 钉死"审批回调 panic 不得穿透、且必须 fail closed"。
func TestApproverPanicBecomesDenial(t *testing.T) {
	opts := baseOptions(t)
	opts.Allow = [][]string{{"echo"}}
	conn := newFakeConn(nil)
	tool, err := New(escalate.NewClient(conn), func(context.Context, escalate.Request) (bool, string, error) {
		panic("审批 UI 崩了")
	}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := tool.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})
	result := invoke(t, tool, okArgs) // invoke 内部会 Fatal：panic 若穿透，这里就炸
	if !strings.HasPrefix(result.Content, DeniedPrefix) {
		t.Fatalf("审批 panic 必须变成 denied: %q", result.Content)
	}
	if !strings.Contains(result.Content, "panic") {
		t.Fatalf("拒绝原因应包含 panic 信息: %q", result.Content)
	}
	if len(conn.recorded()) != 0 {
		t.Fatal("审批 panic 之后不得执行命令")
	}
}
