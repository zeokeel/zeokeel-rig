package presets_test

// 权限模式 → 工具面裁剪（A6）。
//
// 语义：`read-only` 档下**纯写**工具根本不该出现在工具列表里——注册了也只会让
// 模型点开一次、被审批层拒一次，白烧一轮上下文。
//
// shell 类工具（`bash`/`pwsh`/`run_code`）只在**挂了 OS runner** 时保留：
// `sandbox` 策略在 `read-only && !confined` 时拒绝**所有**命令，此时它们是必然
// 失败的死工具；有 runner 时它们经受控路径跑只读命令，才值得留在工具面里。
// `undo` 本质是改工作区文件，`read-only` 下一律裁掉（它不查策略）。
//
// 本包只提供"模式 → 被裁掉的名字"这张表与过滤函数，不 import `sandbox`（策略层）
// 也不 import `kit`（装配层）：预设是纯名字表，必须能单独拿走。

import (
	"slices"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/presets"
)

// allNamesPlusExtras 返回 all 预设的名字表，再补上不在静态账本里的 opt-in 工具
// （PTC 的 run_code），便于断言"它在什么条件下被裁"。
func allNamesPlusExtras(t *testing.T) []string {
	t.Helper()
	names, ok := presets.Names(presets.PresetAll)
	if !ok {
		t.Fatal("presets 没有 all")
	}
	return append(names, "run_code")
}

// TestForModeReadOnlyDropsWriteToolsAndShell：没有 runner 的 read-only 档下，
// 纯写工具与 shell 类工具一起消失（保留它们只会白烧一轮）。
func TestForModeReadOnlyDropsWriteToolsAndShell(t *testing.T) {
	names := allNamesPlusExtras(t)
	original := slices.Clone(names)

	kept := presets.ForMode(presets.ModeReadOnly, false, names)

	for _, dropped := range []string{
		"write_file", "edit_file", "apply_patch", "str_replace_editor",
		"bash", "pwsh", "run_code", "undo",
	} {
		if slices.Contains(kept, dropped) {
			t.Fatalf("read-only 且无 runner 时 %q 仍在工具面里：它注定失败，不该注册", dropped)
		}
	}
	for _, want := range []string{"read_file", "grep", "list_dir", "search", "glob"} {
		if !slices.Contains(kept, want) {
			t.Fatalf("read-only 下读工具 %q 不该消失", want)
		}
	}
	// 纯过滤：不改入参、保持输入顺序。
	if !slices.Equal(names, original) {
		t.Fatalf("ForMode 改动了入参：%v", names)
	}
	want := without(original,
		"write_file", "edit_file", "apply_patch", "str_replace_editor",
		"bash", "pwsh", "run_code", "undo")
	if !slices.Equal(kept, want) {
		t.Fatalf("ForMode 结果顺序不对：%v", kept)
	}
}

// TestForModeReadOnlyWithRunnerKeepsShellTools：挂了 runner 的 read-only 档保留
// shell 类工具（受控 shell 路径），写工具与 undo 仍然裁掉。
func TestForModeReadOnlyWithRunnerKeepsShellTools(t *testing.T) {
	names := allNamesPlusExtras(t)

	kept := presets.ForMode(presets.ModeReadOnly, true, names)

	for _, want := range []string{"bash", "pwsh", "run_code", "read_file", "grep"} {
		if !slices.Contains(kept, want) {
			t.Fatalf("read-only 挂了 runner 时 %q 不该消失（受控 shell 路径）", want)
		}
	}
	for _, dropped := range []string{"write_file", "edit_file", "apply_patch", "str_replace_editor", "undo"} {
		if slices.Contains(kept, dropped) {
			t.Fatalf("read-only 下 %q 仍应被裁掉（与有没有 runner 无关）", dropped)
		}
	}
}

// TestForModeReadOnlyDropsUndo：undo 是写操作，read-only 下不提供。
func TestForModeReadOnlyDropsUndo(t *testing.T) {
	for _, confined := range []bool{false, true} {
		keep := presets.ModeFilter(presets.ModeReadOnly, confined)
		if keep("undo") {
			t.Fatalf("read-only（confined=%v）下 undo 不该保留：它会改工作区文件", confined)
		}
	}
	if !presets.ModeFilter(presets.ModeWorkspaceWrite, false)("undo") {
		t.Fatal("workspace-write 下 undo 不该被裁掉")
	}
}

// TestForModeWorkspaceWriteKeepsEverything：可写档不裁剪（返回原名，含顺序）。
func TestForModeWorkspaceWriteKeepsEverything(t *testing.T) {
	names := allNamesPlusExtras(t)
	for _, confined := range []bool{false, true} {
		if kept := presets.ForMode(presets.ModeWorkspaceWrite, confined, names); !slices.Equal(kept, names) {
			t.Fatalf("workspace-write（confined=%v）不该裁剪工具，得到 %v", confined, kept)
		}
		if kept := presets.ForMode(presets.ModeDangerFullAccess, confined, names); !slices.Equal(kept, names) {
			t.Fatalf("danger-full-access（confined=%v）不该裁剪工具，得到 %v", confined, kept)
		}
	}
}

// TestForModeUnknownModeKeepsEverything：未知/空模式一律不裁剪（宽松失败，避免
// 因为一个拼错的档位把工具面悄悄砍掉一半）。
func TestForModeUnknownModeKeepsEverything(t *testing.T) {
	names := allNamesPlusExtras(t)
	for _, mode := range []string{"", "bogus"} {
		if kept := presets.ForMode(mode, false, names); !slices.Equal(kept, names) {
			t.Fatalf("未知模式 %q 不该裁剪工具，得到 %v", mode, kept)
		}
	}
}

// TestModeFilterMatchesForMode：ModeFilter 与 ForMode 是同一张表的两种形态。
func TestModeFilterMatchesForMode(t *testing.T) {
	names := allNamesPlusExtras(t)
	for _, mode := range []string{
		presets.ModeReadOnly, presets.ModeWorkspaceWrite, presets.ModeDangerFullAccess, "bogus",
	} {
		for _, confined := range []bool{false, true} {
			keep := presets.ModeFilter(mode, confined)
			var viaModeFilter []string
			for _, name := range names {
				if keep(name) {
					viaModeFilter = append(viaModeFilter, name)
				}
			}
			if want := presets.ForMode(mode, confined, names); !slices.Equal(viaModeFilter, want) {
				t.Fatalf("mode=%q confined=%v 时 ModeFilter 与 ForMode 不一致：%v vs %v",
					mode, confined, viaModeFilter, want)
			}
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
