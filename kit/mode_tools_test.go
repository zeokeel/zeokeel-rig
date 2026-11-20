package kit_test

// A6：权限模式 → 工具面裁剪（kit 接线层）。
//
// read-only 档下纯写工具（write_file/edit_file/apply_patch/str_replace_editor）
// **不注册**，因此也进不了 active 集与 tool_search 清单；bash/pwsh/run_code 保留。
// 自建工具集（Config.Tools 非 nil）是宿主的选择，kit 不裁剪，但必须在告警里说明。

import (
	"slices"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// writeToolNames 是 read-only 档下必须消失的纯写工具。
var writeToolNames = []string{"write_file", "edit_file", "apply_patch", "str_replace_editor"}

// TestKitReadOnlyModeHidesWriteTools：Mode=read-only → 注册表与 active 集都没有
// 写工具，但读工具照旧。
func TestKitReadOnlyModeHidesWriteTools(t *testing.T) {
	a := mustNewKitAgent(t, kit.Config{
		Model:    "m",
		Provider: &scriptProvider{},
		Root:     t.TempDir(),
		Mode:     sandbox.ModeReadOnly,
	})
	registered := a.RegisteredTools()
	active := a.Tools()
	for _, name := range writeToolNames {
		if slices.Contains(registered, name) {
			t.Fatalf("read-only 下 %q 仍被注册（RegisteredTools=%v）", name, registered)
		}
		if slices.Contains(active, name) {
			t.Fatalf("read-only 下 %q 仍在 active 集（Tools=%v）", name, active)
		}
	}
	for _, want := range []string{"read_file", "grep"} {
		if !slices.Contains(active, want) {
			t.Fatalf("read-only 下 %q 应该仍然 active（Tools=%v）", want, active)
		}
	}
	// bash 属于"经 runner/审批把关"的工具，不随 read-only 消失。
	if !slices.Contains(registered, "bash") {
		t.Fatalf("read-only 下 bash 不该被裁掉（RegisteredTools=%v）", registered)
	}

	// 宿主显式把写工具塞进 InitialActiveTools 也不该让它们复活：被裁掉的工具
	// 不在注册表里，initialActiveTools 会按名字过滤掉。
	explicit := mustNewKitAgent(t, kit.Config{
		Model:              "m",
		Provider:           &scriptProvider{},
		Root:               t.TempDir(),
		Mode:               sandbox.ModeReadOnly,
		InitialActiveTools: []string{"read_file", "write_file", "apply_patch"},
	})
	if got := explicit.Tools(); slices.Contains(got, "write_file") || slices.Contains(got, "apply_patch") {
		t.Fatalf("InitialActiveTools 让被裁掉的写工具复活了：%v", got)
	}

	// `PermissionPreset: read-only` 是用户最常走的那条路：它先把 Mode 设成只读，
	// 裁剪必须跟着生效。
	preset := mustNewKitAgent(t, kit.Config{
		Model:            "m",
		Provider:         &scriptProvider{},
		Root:             t.TempDir(),
		PermissionPreset: kit.PermissionPresetReadOnly,
	})
	if got := preset.RegisteredTools(); slices.Contains(got, "write_file") {
		t.Fatalf("PermissionPreset=read-only 下 write_file 仍被注册：%v", got)
	}
}

// TestKitDangerFullAccessKeepsWriteTools：全权档不裁剪（工具面与可写面一致）。
func TestKitDangerFullAccessKeepsWriteTools(t *testing.T) {
	a := mustNewKitAgent(t, kit.Config{
		Model:    "m",
		Provider: &scriptProvider{},
		Root:     t.TempDir(),
		Mode:     sandbox.ModeDangerFullAccess,
	})
	registered := a.RegisteredTools()
	for _, name := range writeToolNames {
		if !slices.Contains(registered, name) {
			t.Fatalf("danger-full-access 下 %q 不该消失（RegisteredTools=%v）", name, registered)
		}
	}
}

// TestKitModeFilterSkippedForHostTools：自建工具集不被模式裁剪，但告警要说明。
func TestKitModeFilterSkippedForHostTools(t *testing.T) {
	warnings := captureSlog(t)
	tool := &fakeTool{name: "write_file"}
	a := mustNewKitAgent(t, kit.Config{
		Model:    "m",
		Provider: &scriptProvider{},
		Root:     t.TempDir(),
		Mode:     sandbox.ModeReadOnly,
		Tools:    []kit.Tool{tool},
	})
	if got := a.RegisteredTools(); !slices.Contains(got, "write_file") {
		t.Fatalf("自建工具集被模式裁剪了：%v", got)
	}
	text := warnings()
	if !strings.Contains(text, "Mode") || !strings.Contains(text, "自建工具集") {
		t.Fatalf("告警里没有说明 Mode 裁剪对自建工具集不生效：%s", text)
	}
}
