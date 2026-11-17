package kit

// 个人客户端审计 2C 的修复验收（内部视角）：工具集的"装配体检"。
//
// 这些用例需要直接看 cfgUnavailableTools 的返回，所以放在 package kit 里。

import (
	"context"
	"slices"
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

// A2C-I5：零配置与"逐项加回"的工具数表（发布说明用，也是回归）。
//
// 口径 = personal 预设名单 − 体检摘掉的 − 没有 tracker 的 undo + 显式开关追加的。
func TestAudit2CToolCountTable(t *testing.T) {
	personal, ok := presets.Names(presets.PresetPersonal)
	if !ok {
		t.Fatal("personal 预设缺失")
	}
	presetSet := make(map[string]bool, len(personal))
	for _, name := range personal {
		presetSet[name] = true
	}
	count := func(cfg Config, tracker bool) int {
		dropped := 0
		for _, name := range cfgUnavailableTools(cfg) {
			if presetSet[name] {
				dropped++
			}
		}
		total := len(personal) - dropped
		if !tracker && presetSet["undo"] {
			total--
		}
		// sandbox_status/request_sandbox_access 在预设名单里，但只有开了 SandboxGrants
		// 才由 kit 追加进工具集。
		for _, name := range []string{"sandbox_status", "request_sandbox_access"} {
			if presetSet[name] && !cfg.SandboxGrants {
				total--
			}
		}
		if cfg.PTC {
			total++ // run_code
		}
		return total
	}

	base := count(Config{}, false)
	full := Config{
		Asker:         auditInternalAsker{},
		Searcher:      auditInternalSearcher{},
		SessionLog:    auditInternalSessionLog{},
		AllowNetwork:  true,
		SandboxGrants: true,
		PTC:           true,
	}
	rows := []struct {
		label string
		n     int
	}{
		{"零配置（默认）", base},
		{"+ Asker", count(Config{Asker: auditInternalAsker{}}, false)},
		{"+ Searcher", count(Config{Searcher: auditInternalSearcher{}}, false)},
		{"+ SessionLog", count(Config{SessionLog: auditInternalSessionLog{}}, false)},
		{"+ AllowNetwork", count(Config{AllowNetwork: true}, false)},
		{"+ SandboxGrants", count(Config{SandboxGrants: true}, false)},
		{"+ PTC", count(Config{PTC: true}, false)},
		{"+ GitUndo（git 仓库）", count(Config{}, true)},
		{"全部叠加", count(full, true)},
	}
	t.Logf("personal 预设名单 = %d", len(personal))
	for _, row := range rows {
		t.Logf("  %-22s → %d 个工具", row.label, row.n)
	}
	if base != 37 {
		t.Fatalf("零配置工具数 = %d，want 37（改了这个数字记得同步 AGENTS.md 与发布说明）", base)
	}
	if count(Config{Asker: auditInternalAsker{}}, false) != base+1 {
		t.Fatalf("接 Asker 应 +1（ask_user）")
	}
	if count(Config{Searcher: auditInternalSearcher{}}, false) != base+1 {
		t.Fatalf("接 Searcher 应 +1（web_search）")
	}
	if count(Config{SessionLog: auditInternalSessionLog{}}, false) != base+5 {
		t.Fatalf("接 SessionLog 应 +5（5 个 session_*）")
	}
	if count(Config{AllowNetwork: true}, false) != base+1 {
		t.Fatalf("开 AllowNetwork 应 +1（web_fetch）")
	}
	if count(Config{SandboxGrants: true}, false) != base+2 {
		t.Fatalf("开 SandboxGrants 应 +2")
	}
	if count(Config{PTC: true}, false) != base+1 {
		t.Fatalf("开 PTC 应 +1")
	}
	if count(Config{}, true) != base+1 {
		t.Fatalf("有 tracker 时应 +1（undo）")
	}
}

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
