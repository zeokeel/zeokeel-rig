package presets_test

// 权限模式 → 工具面裁剪（A6）。
//
// 语义：`read-only` 档下**纯写**工具根本不该出现在工具列表里——注册了也只会让
// 模型点开一次、被审批层拒一次，白烧一轮上下文。`bash`/`pwsh`/`run_code` 不在
// 裁剪表里：它们经 runner 与审批把关，写操作由策略拒绝，不是"工具面该消失"的
// 理由。
//
// 本包只提供"模式 → 被裁掉的名字"这张表与过滤函数，不 import `kit`（装配层）。

import (
	"slices"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/presets"
)

// allNamesPlusExtras 返回 all 预设的名字表，再补上不在静态账本里的 opt-in 工具
// （PTC 的 run_code），便于断言"它不被模式裁剪"。
func allNamesPlusExtras(t *testing.T) []string {
	t.Helper()
	names, ok := presets.Names(presets.PresetAll)
	if !ok {
		t.Fatal("presets 没有 all")
	}
	return append(names, "run_code")
}

// TestForModeReadOnlyDropsWriteTools：read-only 只裁纯写工具，其余原样保留。
func TestForModeReadOnlyDropsWriteTools(t *testing.T) {
	names := allNamesPlusExtras(t)
	original := slices.Clone(names)

	kept := presets.ForMode(sandbox.ModeReadOnly, names)

	for _, dropped := range []string{"write_file", "edit_file", "apply_patch", "str_replace_editor"} {
		if slices.Contains(kept, dropped) {
			t.Fatalf("read-only 下 %q 仍在工具面里（纯写工具应该被裁掉）", dropped)
		}
	}
	for _, want := range []string{"bash", "pwsh", "run_code", "search", "read_file", "grep", "list_dir"} {
		if !slices.Contains(kept, want) {
			t.Fatalf("read-only 下 %q 不该消失（它由 runner/审批把关）", want)
		}
	}
	// 纯过滤：不改入参、保持输入顺序。
	if !slices.Equal(names, original) {
		t.Fatalf("ForMode 改动了入参：%v", names)
	}
	if want := without(original, "write_file", "edit_file", "apply_patch", "str_replace_editor"); !slices.Equal(kept, want) {
		t.Fatalf("ForMode 结果顺序不对：%v", kept)
	}
}

// TestForModeWorkspaceWriteKeepsEverything：可写档不裁剪（返回原名，含顺序）。
func TestForModeWorkspaceWriteKeepsEverything(t *testing.T) {
	names := allNamesPlusExtras(t)
	kept := presets.ForMode(sandbox.ModeWorkspaceWrite, names)
	if !slices.Equal(kept, names) {
		t.Fatalf("workspace-write 不该裁剪工具，得到 %v", kept)
	}
	// danger-full-access 同样不裁剪。
	dangerous := presets.ForMode(sandbox.ModeDangerFullAccess, names)
	if !slices.Equal(dangerous, names) {
		t.Fatalf("danger-full-access 不该裁剪工具，得到 %v", dangerous)
	}
}

// TestForModeUnknownModeKeepsEverything：未知/空模式一律不裁剪（宽松失败，避免
// 因为一个拼错的档位把工具面悄悄砍掉一半）。
func TestForModeUnknownModeKeepsEverything(t *testing.T) {
	names := allNamesPlusExtras(t)
	for _, mode := range []sandbox.Mode{"", "bogus"} {
		if kept := presets.ForMode(mode, names); !slices.Equal(kept, names) {
			t.Fatalf("未知模式 %q 不该裁剪工具，得到 %v", mode, kept)
		}
	}
}

// TestModeFilterMatchesForMode：ModeFilter 与 ForMode 是同一张表的两种形态。
func TestModeFilterMatchesForMode(t *testing.T) {
	names := allNamesPlusExtras(t)
	for _, mode := range []sandbox.Mode{sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite, sandbox.ModeDangerFullAccess, "bogus"} {
		keep := presets.ModeFilter(mode)
		var viaModeFilter []string
		for _, name := range names {
			if keep(name) {
				viaModeFilter = append(viaModeFilter, name)
			}
		}
		if want := presets.ForMode(mode, names); !slices.Equal(viaModeFilter, want) {
			t.Fatalf("mode=%q 时 ModeFilter 与 ForMode 不一致：%v vs %v", mode, viaModeFilter, want)
		}
	}
}

// without 返回去掉给定名字后的新切片（保持顺序）。
func without(names []string, dropped ...string) []string {
	skip := make(map[string]bool, len(dropped))
	for _, name := range dropped {
		skip[name] = true
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !skip[name] {
			out = append(out, name)
		}
	}
	return out
}
