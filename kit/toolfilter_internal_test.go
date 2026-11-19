package kit

// 个人客户端审计 2C 的修复验收（内部视角）：工具集的"装配体检"。
//
// 这些用例需要直接看 cfgUnavailableTools 的返回，所以放在 package kit 里。

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"sort"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/presets"
	"github.com/zeokeel/zeokeel-rig/tools/web"
)

// A2C-I1：`pwsh` 只在 Windows 上保留（P2 验收）。
//
// 别的平台上 pwsh 根本没装，留着就是一个必然报错的工具。这里用可注入的
// goosForToolFilter 覆盖两个分支（不依赖跑测试的机器）。
func TestAudit2CToolFilterDropsPwshOutsideWindows(t *testing.T) {
	original := goosForToolFilter
	t.Cleanup(func() { goosForToolFilter = original })

	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		goosForToolFilter = goos
		if !slices.Contains(cfgUnavailableTools(Config{}), "pwsh") {
			t.Fatalf("%s 上应当摘掉 pwsh（未安装时它只会报错）", goos)
		}
	}
	goosForToolFilter = "windows"
	if slices.Contains(cfgUnavailableTools(Config{}), "pwsh") {
		t.Fatal("Windows 上不该摘掉 pwsh")
	}
}

// A2C-I2：`web_fetch` 只在允许联网时保留（P1-3 验收）。
func TestAudit2CToolFilterDropsWebFetchWithoutNetwork(t *testing.T) {
	dropped := cfgUnavailableTools(Config{})
	if !slices.Contains(dropped, "web_fetch") {
		t.Fatalf("AllowNetwork=false 时应摘掉 web_fetch，实际摘掉：%v", dropped)
	}
	kept := cfgUnavailableTools(Config{AllowNetwork: true})
	if slices.Contains(kept, "web_fetch") {
		t.Fatalf("AllowNetwork=true 时不该摘掉 web_fetch，实际摘掉：%v", kept)
	}
}

// A2C-I3：委派工具已不在 personal 预设里，体检表里也不该再列它们（P1-4 验收）。
func TestAudit2CToolFilterNoLongerListsSubagentTools(t *testing.T) {
	dropped := cfgUnavailableTools(Config{})
	for _, name := range []string{"subagent", "subagent_fork"} {
		if slices.Contains(dropped, name) {
			t.Fatalf("体检表仍列着 %q，但它已经不在 personal 预设里（这是死配置）", name)
		}
	}
}

// A2C-I4：`ask_user` 只在宿主给了 Asker 时保留（P1-1 相关的不变量）。
func TestAudit2CToolFilterKeepsAskUserOnlyWithAsker(t *testing.T) {
	if !slices.Contains(cfgUnavailableTools(Config{}), "ask_user") {
		t.Fatal("没有 Asker 时应摘掉 ask_user")
	}
	if slices.Contains(cfgUnavailableTools(Config{Asker: auditInternalAsker{}}), "ask_user") {
		t.Fatal("配了 Asker 之后不该摘掉 ask_user")
	}
}

// auditInternalAsker 满足 rig.Asker（只用于过滤判定，不会被调用）。
type auditInternalAsker struct{}

func (auditInternalAsker) Ask(ctx context.Context, question string, options []string) (string, error) {
	return "", nil
}

// boolPtr 是包内测试用的小工具。
func boolPtr(v bool) *bool { return &v }

// sandboxGrantsEnabled 复刻 kit.New 的三态解析（nil = 开）。
func sandboxGrantsEnabled(cfg Config) bool {
	return cfg.SandboxGrants == nil || *cfg.SandboxGrants
}

// A2C-I5：零配置**注册面**（39 个工具，且必须是这 39 个名字）。
//
// 2026-11 钉子复核（见 docs/PLATFORM.md §10）：原来的断言只有 `base != 39`，
// 而那个 39 是**算出来的**（personal 预设 − cfgUnavailableTools − 无快照的 undo
// − Grants 关掉的两项），测的是这张算术表，不是 kit 真正注册出来的那一组名字。
// 两个后果：
//   - "换掉一个工具但仍是 39 个"照样绿（例如 web_search 被换成别的名字）；
//   - kit 若绕过体检表自己追加/漏掉一个工具，算术仍得 39，一个字节都没测到。
//
// 现在改成**名字集合**断言（组合律），并把计数口径保留为发布数字：
//
//	New(cfg).RegisteredTools() == personal − cfgUnavailableTools(cfg)
//	                              − (无回合快照时的 undo)
//	                              − (SandboxGrants=false 时的 2 个授权工具)
//	                              + (PTC 时的 run_code)
//
// 这是用户可见契约（"零配置给我哪些工具"），不绑定任何实现类型：Storage 落地
// 后若真的增删工具，这里会红，逼人同批改 AGENTS.md / docs/TOOLS.md——这正是要的。
func TestAudit2CToolSurfaceComposition(t *testing.T) {
	base := auditInternalRegistered(t, Config{})
	if len(base) != 39 {
		t.Fatalf("零配置注册工具数 = %d，want 39（改了这个数字记得同步 AGENTS.md 与 docs/TOOLS.md）", len(base))
	}
	cases := []struct {
		label string
		cfg   Config
	}{
		{"零配置（默认）", Config{}},
		{"+ Asker", Config{Asker: auditInternalAsker{}}},
		{"+ Searcher", Config{Searcher: auditInternalSearcher{}}},
		{"+ SessionLog", Config{SessionLog: auditInternalSessionLog{}}},
		{"+ AllowNetwork", Config{AllowNetwork: true}},
		{"+ SandboxGrants（默认已开）", Config{SandboxGrants: boolPtr(true)}},
		{"SandboxGrants=false", Config{SandboxGrants: boolPtr(false)}},
		{"+ PTC", Config{PTC: true}},
		{"+ Presenter", Config{Presenter: auditInternalPresenter{}}},
		{"全部叠加（无快照）", Config{
			Asker: auditInternalAsker{}, Searcher: auditInternalSearcher{},
			SessionLog: auditInternalSessionLog{}, AllowNetwork: true,
			SandboxGrants: boolPtr(true), PTC: true,
		}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			got := auditInternalRegistered(t, c.cfg)
			want := auditInternalExpectedNames(t, c.cfg)
			t.Logf("%-22s → %d 个工具", c.label, len(got))
			if !slices.Equal(got, want) {
				added, removed := auditInternalDelta(want, got)
				t.Fatalf("注册面与组合律不一致：多出 %v，少了 %v\n got = %v\nwant = %v",
					added, removed, got, want)
			}
		})
	}
}

// A2C-I6：能力增量断言——接上某项宿主能力，**恰好**多出（或摘掉）那几个工具。
//
// 原来这里是 `count(...) != base+5` 一串算术钉子。问题同上：它只数个数，
// 把 5 个 session_* 之一换成别的名字仍然是 +5，而模型看到的是名字。
// 断言改成名字级集合差之后，"接了能干什么"这个契约才真的被测到。
func TestAudit2CToolSurfaceCapabilityDeltas(t *testing.T) {
	base := auditInternalRegistered(t, Config{})
	cases := []struct {
		label   string
		cfg     Config
		added   []string
		removed []string
	}{
		{"Asker → ask_user", Config{Asker: auditInternalAsker{}}, []string{"ask_user"}, nil},
		{"Searcher → web_search", Config{Searcher: auditInternalSearcher{}}, []string{"web_search"}, nil},
		{"SessionLog → 5 个 session_*", Config{SessionLog: auditInternalSessionLog{}},
			[]string{"session_event_read", "session_event_search", "session_event_trace", "session_search", "session_trace"}, nil},
		{"AllowNetwork → web_fetch", Config{AllowNetwork: true}, []string{"web_fetch"}, nil},
		{"Presenter → present", Config{Presenter: auditInternalPresenter{}}, []string{"present"}, nil},
		{"SandboxGrants=false → 摘掉 2 个授权工具", Config{SandboxGrants: boolPtr(false)},
			nil, []string{"request_sandbox_access", "sandbox_status"}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			got := auditInternalRegistered(t, c.cfg)
			added, removed := auditInternalDelta(base, got)
			if !slices.Equal(added, c.added) || !slices.Equal(removed, c.removed) {
				t.Fatalf("新增=%v 摘掉=%v，want 新增=%v 摘掉=%v", added, removed, c.added, c.removed)
			}
		})
	}
	// 回合快照只在 git 工作区成立：GitUndo 打开后 undo 回来，且只回来它一个。
	t.Run("GitUndo（git 仓库）→ undo", func(t *testing.T) {
		root := auditInternalGitRepo(t)
		got := auditInternalRegistered(t, Config{GitUndo: true, Root: root})
		added, removed := auditInternalDelta(base, got)
		if !slices.Equal(added, []string{"undo"}) || len(removed) != 0 {
			t.Fatalf("新增=%v 摘掉=%v，want 只新增 undo", added, removed)
		}
	})
}

// auditInternalExpectedNames 是组合律的期望集（见 TestAudit2CToolSurfaceComposition）。
//
// 它复刻 kit 的**装配口径**而不是 kit 的实现：名单来自 public 的 personal 预设，
// 摘除项来自 public 的 cfgUnavailableTools。kit 若改变"零配置给什么"，这里必须
// 一起改——那是一次有意的产品决策，不是一次静默漂移。
func auditInternalExpectedNames(t *testing.T, cfg Config) []string {
	t.Helper()
	personal, ok := presets.Names(presets.PresetPersonal)
	if !ok {
		t.Fatal("personal 预设缺失")
	}
	drop := make(map[string]bool, len(personal))
	for _, name := range cfgUnavailableTools(cfg) {
		drop[name] = true
	}
	// 没有回合快照就不给 undo（只有 git 工作区 + GitUndo 才回来）。
	drop["undo"] = true
	if !sandboxGrantsEnabled(cfg) {
		drop["sandbox_status"] = true
		drop["request_sandbox_access"] = true
	}
	names := make([]string, 0, len(personal))
	for _, name := range personal {
		if !drop[name] {
			names = append(names, name)
		}
	}
	if cfg.PTC {
		names = append(names, "run_code")
	}
	sort.Strings(names)
	return names
}

// auditInternalRegistered 走**真实的** kit.New 路径，返回它注册的工具名（已排序）。
//
// 故意不复刻装配逻辑：只有真的建一个 agent，才能发现"kit 自己多注册/少注册了什么"。
func auditInternalRegistered(t *testing.T, cfg Config) []string {
	t.Helper()
	if cfg.Model == "" {
		cfg.Model = "m"
	}
	if cfg.Provider == nil {
		cfg.Provider = auditInternalProvider{}
	}
	if cfg.Root == "" {
		cfg.Root = t.TempDir()
	}
	// 零配置会按工作区自动建文件 wiki（用户级状态目录）；测试必须落在临时目录里，
	// 否则会往跑测试的人的真实 `$HOME` 里写记忆。
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("LOCALAPPDATA", stateDir)
	agent, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	names := agent.RegisteredTools()
	sort.Strings(names)
	return names
}

// auditInternalDelta 返回 after 相对 before 的工具名增减（已排序）。
func auditInternalDelta(before, after []string) (added, removed []string) {
	for _, name := range after {
		if !slices.Contains(before, name) {
			added = append(added, name)
		}
	}
	for _, name := range before {
		if !slices.Contains(after, name) {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// auditInternalGitRepo 建一个最小 git 仓库（GitUndo 的前提）。
func auditInternalGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git 不可用: %v", err)
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q", ".")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("config", "commit.gpgsign", "false")
	return root
}

// auditInternalProvider 是只为装配用的 provider（永远返回错误，不会被真正调用）。
type auditInternalProvider struct{}

func (auditInternalProvider) Name() string { return "audit-internal" }

func (auditInternalProvider) Stream(context.Context, Request) (Stream, error) {
	return nil, context.Canceled
}

// auditInternalPresenter 满足 rig.FilePresenter（只用于过滤判定）。
type auditInternalPresenter struct{}

func (auditInternalPresenter) PresentFiles(context.Context, []rig.PresentedFile) error { return nil }

// auditInternalSearcher 满足 web.Searcher。
type auditInternalSearcher struct{}

func (auditInternalSearcher) Search(ctx context.Context, query string, limit int) ([]web.SearchResult, error) {
	return nil, nil
}

// auditInternalSessionLog 满足 rig.SessionLog。
type auditInternalSessionLog struct{}

func (auditInternalSessionLog) Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error) {
	return nil, nil
}
func (auditInternalSessionLog) Search(ctx context.Context, query string, limit int) ([]rig.SessionEvent, error) {
	return nil, nil
}
func (auditInternalSessionLog) Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error) {
	return nil, nil
}
