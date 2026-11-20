package kit_test

// A6：权限模式 → 工具面裁剪（kit 接线层）。
//
// read-only 档下纯写工具（write_file/edit_file/apply_patch/str_replace_editor）与
// `undo` **不注册**，因此也进不了 active 集与 tool_search 清单。`bash`/`pwsh`/
// `run_code` 只在挂了 OS runner 时保留：没有 runner 时策略拒绝**所有**命令
// （`sandbox/command.go` 的 `read-only && !confined` 分支），留着就是必然失败的死工具。
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
	// 没挂 runner 的 read-only：bash 必然报 "shell commands are disabled"，
	// 把它留在工具面里只会白烧一轮（A6 #2）。
	if slices.Contains(registered, "bash") {
		t.Fatalf("read-only 且无 runner 时 bash 不该被注册（RegisteredTools=%v）", registered)
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

// TestKitReadOnlyWithRunnerKeepsShellTools：挂了 OS runner 的 read-only 档保留
// shell 类工具——它们是 A4 的受控 shell 路径（命令真能跑，只是写不出去）。
func TestKitReadOnlyWithRunnerKeepsShellTools(t *testing.T) {
	a := mustNewKitAgent(t, kit.Config{
		Model:         "m",
		Provider:      &scriptProvider{},
		Root:          t.TempDir(),
		Mode:          sandbox.ModeReadOnly,
		SandboxRunner: &kitRunner{},
		PTC:           true,
	})
	registered := a.RegisteredTools()
	for _, want := range []string{"bash", "run_code"} {
		if !slices.Contains(registered, want) {
			t.Fatalf("read-only 挂了 runner 时 %q 不该消失（受控 shell 路径；RegisteredTools=%v）", want, registered)
		}
	}
}

// TestKitPermissionPresetFollowsPolicyMode：档位只有一个来源——策略的生效模式。
// 双向都钉住，避免"报一个档、围栏/裁剪跑另一个档"（A6 #3）。
func TestKitPermissionPresetFollowsPolicyMode(t *testing.T) {
	t.Run("policy-read-only-wins", func(t *testing.T) {
		root := t.TempDir()
		policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
		a := mustNewKitAgent(t, kit.Config{
			Model:    "m",
			Provider: &scriptProvider{},
			Root:     root,
			Policy:   policy, // cfg.Mode 留空（默认 workspace-write），策略才是真围栏
		})
		if got := a.PermissionPreset(); got != kit.PermissionPresetReadOnly {
			t.Fatalf("Policy=read-only 时 PermissionPreset() = %q，want read-only（与围栏/裁剪同源）", got)
		}
		if got := a.RegisteredTools(); slices.Contains(got, "write_file") {
			t.Fatalf("Policy=read-only 下 write_file 仍被注册：%v", got)
		}
	})

	t.Run("explicit-policy-workspace-write-wins", func(t *testing.T) {
		root := t.TempDir()
		policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
		a := mustNewKitAgent(t, kit.Config{
			Model:    "m",
			Provider: &scriptProvider{},
			Root:     root,
			Mode:     sandbox.ModeReadOnly, // 与策略冲突：策略优先
			Policy:   policy,
		})
		if got := a.PermissionPreset(); got != kit.PermissionPresetWorkspaceWrite {
			t.Fatalf("Policy=workspace-write + Mode=read-only 时 PermissionPreset() = %q，want workspace-write", got)
		}
		if got := a.RegisteredTools(); !slices.Contains(got, "write_file") {
			t.Fatalf("Policy=workspace-write 时 write_file 不该被裁掉：%v", got)
		}
	})
}

// TestKitReadOnlyDropsUndo：read-only 下不提供 undo——它本质是改工作区文件，
// 且 tools/undo 不查策略（A6 #4）。
func TestKitReadOnlyDropsUndo(t *testing.T) {
	readOnly := mustNewKitAgent(t, kit.Config{
		Model:    "m",
		Provider: &scriptProvider{},
		Root:     t.TempDir(),
		Mode:     sandbox.ModeReadOnly,
		UndoDir:  t.TempDir(),
	})
	if got := readOnly.RegisteredTools(); slices.Contains(got, "undo") {
		t.Fatalf("read-only 下 undo 仍被注册（它会改工作区文件）：%v", got)
	}
	// 对照：可写档保留 undo，证明上面不是因为"没有 tracker"才缺席。
	writable := mustNewKitAgent(t, kit.Config{
		Model:    "m",
		Provider: &scriptProvider{},
		Root:     t.TempDir(),
		UndoDir:  t.TempDir(),
	})
	if got := writable.RegisteredTools(); !slices.Contains(got, "undo") {
		t.Fatalf("workspace-write 下 undo 不该缺席：%v", got)
	}
}
