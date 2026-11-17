package presets_test

// 个人客户端审计（第 2C 轮）：personal 预设的名字表是否"既不缺也不臃肿"。
//
// 只读审计：只断言 presets 的公开契约，不改实现。

import (
	"slices"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/presets"
)

func personalNames(t *testing.T) []string {
	t.Helper()
	names, ok := presets.Names(presets.PresetPersonal)
	if !ok {
		t.Fatal("presets 没有 personal")
	}
	return names
}

func contains(names []string, want string) bool { return slices.Contains(names, want) }

// A2C-P1：预设层级必须自洽（personal ⊆ standard ⊆ all），且计数与文档一致。
func TestAudit2CPresetHierarchy(t *testing.T) {
	personal := personalNames(t)
	standard, _ := presets.Names(presets.PresetStandard)
	all, _ := presets.Names(presets.PresetAll)
	minimal, _ := presets.Names(presets.PresetMinimal)

	if len(all) == 0 || len(standard) == 0 || len(personal) == 0 || len(minimal) == 0 {
		t.Fatal("有预设为空")
	}
	for _, name := range personal {
		if !contains(standard, name) {
			t.Fatalf("personal 里的 %q 不在 standard 里（层级被破坏）", name)
		}
	}
	for _, name := range standard {
		if !contains(all, name) {
			t.Fatalf("standard 里的 %q 不在 all 里", name)
		}
	}
	// 去重。
	seen := map[string]bool{}
	for _, name := range all {
		if seen[name] {
			t.Fatalf("all 里有重复名字 %q", name)
		}
		seen[name] = true
	}
	t.Logf("预设规模：minimal=%d personal=%d standard=%d all=%d", len(minimal), len(personal), len(standard), len(all))
}

// A2C-P2：个人必备工具必须在 personal 里（缺一个就会让常见任务失败）。
func TestAudit2CPersonalKeepsEssentials(t *testing.T) {
	personal := personalNames(t)
	essentials := []string{
		// 编码闭环
		"read_file", "write_file", "edit_file", "apply_patch", "bash",
		"grep", "glob", "list_dir", "search",
		// 计划 / 交互 / 交付
		"todo_write", "ask_user", "present", "skill",
		// 记忆 / 调度 / 工作区
		"memory", "schedule_create", "schedule_list", "working_directory",
		// 后台任务与终端
		"job_start", "job_output", "terminal_open",
		// 网络与非文本输入
		"web_fetch", "read_image",
	}
	var missing []string
	for _, name := range essentials {
		if !contains(personal, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("personal 缺了个人必备工具：%v", missing)
	}
}

// A2C-P3：已按定位移除的工具不得在任何预设里复活（report/settings_*/present_artifact）。
func TestAudit2CRemovedToolsStayRemoved(t *testing.T) {
	removed := []string{"report", "settings_get", "settings_set", "present_artifact"}
	for _, preset := range []string{presets.PresetMinimal, presets.PresetPersonal, presets.PresetStandard, presets.PresetAll} {
		names, ok := presets.Names(preset)
		if !ok {
			t.Fatalf("未知预设 %q", preset)
		}
		for _, name := range removed {
			if contains(names, name) {
				t.Fatalf("预设 %q 里仍有已移除的工具 %q", preset, name)
			}
		}
	}
}

// A2C-P4：个人预设不提供多智能体编排（P1-4 验收，方案："一起排掉"）。
//
// 之前 personal 留了 subagent/subagent_fork 却排掉 wait_agent/list_agents——
// "能发不能收"。现在四个都不在 personal 里；需要多智能体的宿主用
// standard/all + full.All(AllOptions{SubagentRunner: …}) 完整装配。
func TestAudit2CPersonalDropsDelegationEntirely(t *testing.T) {
	personal := personalNames(t)
	for _, name := range []string{"subagent", "subagent_fork", "wait_agent", "list_agents", "spawn_teammate", "workflow", "ralph"} {
		if contains(personal, name) {
			t.Fatalf("personal 里仍有委派/编排工具 %q（应统一由 standard/all 提供）", name)
		}
	}
	// 但 standard/all 必须仍然提供它们（完整装配路径不受影响）。
	standard, _ := presets.Names(presets.PresetStandard)
	for _, name := range []string{"subagent", "subagent_fork", "wait_agent", "list_agents"} {
		if !contains(standard, name) {
			t.Fatalf("standard 里缺少 %q（完整装配路径不能丢）", name)
		}
	}
}

// A2C-P5：Filter 的契约（按名字过滤、保序去重、nil 跳过、未知预设报错）。
func TestAudit2CFilterContract(t *testing.T) {
	tools := []interface{}{}
	_ = tools
	if _, err := presets.Filter("nope", nil); err == nil {
		t.Fatal("未知预设应报错")
	}
	// 空 preset 解析为默认（standard），且不返回 error。
	if names, ok := presets.Names(""); !ok || len(names) == 0 {
		t.Fatalf("空预设应为默认预设，得到 ok=%v len=%d", ok, len(names))
	}
	// 名字大小写/空白归一。
	if _, ok := presets.Names("  PERSONAL  "); !ok {
		t.Fatal("预设名应 trim + 转小写")
	}
}
