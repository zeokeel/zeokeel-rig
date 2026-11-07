package presets_test

// P3-9 工具集预设的单元测试：成员关系与 catalog 账本一致、未知预设报错、Filter
// 保序/去重、minimal/standard 的边界。

import (
	"context"
	"slices"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/catalog"
	"github.com/zeokeel/zeokeel-rig/tools/presets"
)

// fakeTool 是只带名字的 rig.Tool 测试替身。
type fakeTool struct{ name string }

func (t fakeTool) Name() string           { return t.name }
func (t fakeTool) Description() string    { return "test tool " + t.name }
func (t fakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t fakeTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{Content: t.name}, nil
}

// filterNames 返回 Filter 结果中的工具名，便于断言顺序。
func filterNames(ts []rig.Tool) []string {
	names := make([]string, 0, len(ts))
	for _, ts := range ts {
		names = append(names, ts.Name())
	}
	return names
}

func TestAvailable(t *testing.T) {
	want := []string{presets.PresetMinimal, presets.PresetStandard, presets.PresetAll}
	if got := presets.Available(); !slices.Equal(got, want) {
		t.Fatalf("Available() = %v, want %v", got, want)
	}
}

func TestNamesMinimalExact(t *testing.T) {
	want := []string{
		"terminal_open", "terminal_list", "terminal_read",
		"terminal_send", "terminal_signal", "terminal_close",
		"read_image", "working_directory",
	}
	got, ok := presets.Names(presets.PresetMinimal)
	if !ok {
		t.Fatal("Names(minimal) ok = false")
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Names(minimal) = %v, want %v", got, want)
	}
	all, _ := presets.Names(presets.PresetAll)
	inAll := make(map[string]bool, len(all))
	for _, name := range all {
		inAll[name] = true
	}
	for _, name := range got {
		if !inAll[name] {
			t.Errorf("minimal name %q is not in the static catalog", name)
		}
	}
}

func TestNamesAllMatchesCatalog(t *testing.T) {
	got, ok := presets.Names(presets.PresetAll)
	if !ok {
		t.Fatal("Names(all) ok = false")
	}
	if !slices.Equal(got, catalog.Static) {
		t.Fatalf("Names(all) drifted from catalog.Static:\n got=%v\nwant=%v", got, catalog.Static)
	}
	if len(got) != 81 {
		t.Fatalf("len(Names(all)) = %d, want 81", len(got))
	}
}

func TestNamesStandard(t *testing.T) {
	all, _ := presets.Names(presets.PresetAll)
	standard, ok := presets.Names(presets.PresetStandard)
	if !ok {
		t.Fatal("Names(standard) ok = false")
	}
	// standard = all - 兼容别名（goal/present_artifact/todo + 8 个旧 team 名称）。
	alias := map[string]bool{
		"goal": true, "present_artifact": true, "todo": true,
		"spawn_team": true, "list_teams": true, "team_status": true,
		"close_team": true, "wait_team": true, "send_message": true,
		"interrupt_agent": true, "list_subagent_models": true,
	}
	want := make([]string, 0, len(all)-len(alias))
	for _, name := range all {
		if !alias[name] {
			want = append(want, name)
		}
	}
	if !slices.Equal(standard, want) {
		t.Fatalf("Names(standard) = %v, want all minus compat aliases (%v)", standard, want)
	}
	if got, wantLen := len(standard), len(all)-len(alias); got != wantLen {
		t.Fatalf("len(Names(standard)) = %d, want %d", got, wantLen)
	}
	for _, name := range []string{"read_file", "bash", "get_goal", "present", "todo_write", "team_spawn", "spawn_teammate", "team_task_create"} {
		if !slices.Contains(standard, name) {
			t.Errorf("standard misses modern tool %q", name)
		}
	}
	for name := range alias {
		if slices.Contains(standard, name) {
			t.Errorf("standard still contains compat alias %q", name)
		}
	}
}

func TestNamesUnknownAndNormalization(t *testing.T) {
	for _, preset := range []string{"bogus", "creator", "full"} {
		if _, ok := presets.Names(preset); ok {
			t.Errorf("Names(%q) ok = true, want false", preset)
		}
	}
	// 空串与大小写/空白等价于 standard。
	empty, ok := presets.Names("")
	if !ok {
		t.Fatal("Names(\"\") ok = false, want default preset")
	}
	standard, _ := presets.Names(presets.PresetStandard)
	if !slices.Equal(empty, standard) {
		t.Fatal("Names(\"\") is not the standard preset")
	}
	for _, preset := range []string{" MINIMAL ", "Minimal"} {
		got, ok := presets.Names(preset)
		if !ok {
			t.Fatalf("Names(%q) ok = false", preset)
		}
		minimal, _ := presets.Names(presets.PresetMinimal)
		if !slices.Equal(got, minimal) {
			t.Fatalf("Names(%q) is not the minimal preset", preset)
		}
	}
}

func TestNamesReturnsCopy(t *testing.T) {
	first, _ := presets.Names(presets.PresetMinimal)
	if len(first) == 0 {
		t.Fatal("minimal preset is empty")
	}
	first[0] = "mutated"
	second, _ := presets.Names(presets.PresetMinimal)
	if second[0] == "mutated" {
		t.Fatal("Names returned an aliased slice; mutating it changed the package table")
	}
}

func TestFilterUnknownPreset(t *testing.T) {
	if _, err := presets.Filter("bogus", nil); err == nil {
		t.Fatal("Filter(unknown) err = nil, want error")
	}
}

func TestFilterPreservesOrderAndDedupes(t *testing.T) {
	input := []rig.Tool{
		fakeTool{"grep"},
		fakeTool{"read_file"},
		nil,
		fakeTool{"not_a_real_tool"},
		fakeTool{"grep"}, // 重复
		fakeTool{"write_file"},
	}
	got, err := presets.Filter(presets.PresetAll, input)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	want := []string{"grep", "read_file", "write_file"}
	if names := filterNames(got); !slices.Equal(names, want) {
		t.Fatalf("Filter names = %v, want %v (input order, deduped, unknown skipped)", names, want)
	}
	if len(got) != 3 {
		t.Fatalf("len(Filter) = %d, want 3", len(got))
	}
}

func TestFilterMinimal(t *testing.T) {
	input := []rig.Tool{
		fakeTool{"read_file"},
		fakeTool{"terminal_open"},
		fakeTool{"bash"},
		fakeTool{"read_image"},
		fakeTool{"terminal_list"},
		fakeTool{"working_directory"},
	}
	got, err := presets.Filter(presets.PresetMinimal, input)
	if err != nil {
		t.Fatalf("Filter(minimal): %v", err)
	}
	want := []string{"terminal_open", "read_image", "terminal_list", "working_directory"}
	if names := filterNames(got); !slices.Equal(names, want) {
		t.Fatalf("Filter(minimal) = %v, want %v", names, want)
	}
}

func TestFilterStandardExcludesCompatAliases(t *testing.T) {
	input := []rig.Tool{
		fakeTool{"goal"},
		fakeTool{"get_goal"},
		fakeTool{"present_artifact"},
		fakeTool{"present"},
		fakeTool{"todo"},
		fakeTool{"todo_write"},
		fakeTool{"spawn_team"},
		fakeTool{"team_spawn"},
		fakeTool{"list_subagent_models"},
	}
	got, err := presets.Filter(presets.PresetStandard, input)
	if err != nil {
		t.Fatalf("Filter(standard): %v", err)
	}
	want := []string{"get_goal", "present", "todo_write", "team_spawn"}
	if names := filterNames(got); !slices.Equal(names, want) {
		t.Fatalf("Filter(standard) = %v, want %v", names, want)
	}
}

func TestFilterEmpty(t *testing.T) {
	for _, preset := range presets.Available() {
		got, err := presets.Filter(preset, nil)
		if err != nil {
			t.Fatalf("Filter(%s, nil): %v", preset, err)
		}
		if len(got) != 0 {
			t.Fatalf("Filter(%s, nil) = %d tools, want 0", preset, len(got))
		}
		got, err = presets.Filter(preset, []rig.Tool{fakeTool{"not_a_real_tool"}})
		if err != nil {
			t.Fatalf("Filter(%s, other): %v", preset, err)
		}
		if len(got) != 0 {
			t.Fatalf("Filter(%s, other) = %d tools, want 0", preset, len(got))
		}
	}
}

func TestFilterAllRoundTripPreservesEveryStaticTool(t *testing.T) {
	input := make([]rig.Tool, 0, len(catalog.Static))
	for i := len(catalog.Static) - 1; i >= 0; i-- { // 反序输入
		input = append(input, fakeTool{catalog.Static[i]})
	}
	got, err := presets.Filter(presets.PresetAll, input)
	if err != nil {
		t.Fatalf("Filter(all): %v", err)
	}
	if len(got) != len(catalog.Static) {
		t.Fatalf("len(Filter(all)) = %d, want %d", len(got), len(catalog.Static))
	}
	reversed := make([]string, 0, len(catalog.Static))
	for i := len(catalog.Static) - 1; i >= 0; i-- {
		reversed = append(reversed, catalog.Static[i])
	}
	if names := filterNames(got); !slices.Equal(names, reversed) {
		t.Fatal("Filter(all) did not preserve input order")
	}
}
