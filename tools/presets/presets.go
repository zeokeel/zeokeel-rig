// Package presets 提供 agent 工具集预设（旧 agent 的 agent 模式/工具集预设的 SDK
// 部分）。
//
// 预设只做“按名字过滤工具集”，不包含系统 prompt、拓扑或权限策略：Gate/Policy
// 与预设相互独立，宿主可以把同一预设叠加到任意沙箱策略/审批策略之上。工具集
// 来源以 tools/catalog.Static 为准（测试会断言本包与账本一致，防止漂移）。
//
// 三个预设：
//
//   - minimal：DSH minimal 风格的最小可用集——六个 terminal_* 会话工具、read_image
//     与 working_directory。它刻意不含 read_file/bash 等工具，镜像 DSH minimal 与
//     old 的 ModeMinimal。
//   - standard（默认）：当前全部静态工具中，去掉“兼容旧 agent 的别名”后的集合。
//     现代 DSH 名称（如 get_goal/create_goal/update_goal、present、todo_write、
//     team_spawn/team_list 等）保留；旧别名（goal、present_artifact、todo、
//     spawn_team、list_teams、team_status、close_team、wait_team、send_message、
//     interrupt_agent、list_subagent_models）剔除。
//   - all：tools/catalog.Static 的全部静态工具（81 个，含兼容别名）。
//
// server-bound 工具（MCP/LSP，需要配置客户端）与动态 mcp__<server>__<tool> 不在
// 静态集合中；opt-in 的 run_code（kit.Config.PTC）也不是静态工具，因此预设不会
// 隐式启用它们。
package presets

import (
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// 预设名。传入 Names/Filter 时会 trim 并转小写；空字符串等价于
// PresetStandard（默认预设）。
const (
	// PresetMinimal 是最小可用工具集（DSH minimal 风格）。
	PresetMinimal = "minimal"
	// PresetStandard 是默认工具集：全部现代静态工具，不含兼容旧 agent 的别名。
	PresetStandard = "standard"
	// PresetAll 是全部静态工具（含兼容别名）。
	PresetAll = "all"
	// DefaultPreset 是空 preset 参数解析到的预设。
	DefaultPreset = PresetStandard
)

// allToolNames 是 tools/catalog.Static 的完整名字表（保持账本顺序）。测试会断言
// 它与 catalog.Static 完全一致；tools/presets 不 import catalog，避免运行时把
// 发布账本数据变成依赖。
var allToolNames = []string{
	// 文件系统 / 编辑器 / 图片
	"read_file", "write_file", "edit_file", "str_replace_editor",
	"list_dir", "glob", "grep", "read_image",
	// shell / 进程
	"bash", "pwsh",
	"terminal_open", "terminal_send", "terminal_read", "terminal_signal",
	"terminal_close", "terminal_list",
	"job_start", "job_output", "job_kill", "job_list", "job_wait",
	// web
	"web_fetch", "web_search",
	// 会话 / 诊断
	"session_search", "session_trace",
	"session_event_read", "session_event_search", "session_event_trace",
	"inspect", "report",
	// 宿主交互 / 计划 / 目标 / 设置
	"ask_user", "present", "present_artifact", "plan_review", "exit_plan_mode", "goal",
	"get_goal", "create_goal", "update_goal",
	"settings_get", "settings_set",
	// 记忆 / 调度 / worktree
	"memory",
	"schedule_create", "schedule_list", "schedule_update", "schedule_delete",
	"enter_worktree", "exit_worktree", "list_worktrees",
	// 编排
	"subagent", "subagent_fork", "spawn_teammate", "list_agents", "wait_agent", "workflow", "ralph",
	"team_spawn", "team_list", "team_send", "team_wait", "team_last",
	"team_close", "team_interrupt",
	"spawn_team", "list_teams", "team_status", "close_team", "wait_team",
	"send_message", "interrupt_agent", "list_subagent_models",
	// 杂项
	"working_directory", "todo", "todo_write",
	// skills、团队任务板、评审
	"skill", "code_review", "auto_review",
	"team_task_create", "team_task_list", "team_task_get", "team_task_update",
}

// minimalToolNames 是 minimal 预设的精确 allowlist（DSH minimal / old ModeMinimal）。
// 使用精确名单而不是 terminal_* 前缀，避免未来新增的 terminal 前缀工具被意外纳入。
var minimalToolNames = []string{
	"terminal_open", "terminal_list", "terminal_read",
	"terminal_send", "terminal_signal", "terminal_close",
	"read_image", "working_directory",
}

// compatAliasNames 是 standard 预设剔除的兼容别名：DSH 现代名称已覆盖同名能力，
// 同时暴露两种名字会让模型看到重复能力。all 预设仍保留它们（兼容旧会话/提示词）。
var compatAliasNames = map[string]bool{
	"goal":                 true, // 由 get_goal/create_goal/update_goal 取代
	"present_artifact":     true, // 由 DSH 的 present（声明文件）取代
	"todo":                 true, // 由 todo_write 取代
	"spawn_team":           true, // 旧 team 名称，现代名称：spawn_teammate/list_agents/wait_agent
	"list_teams":           true,
	"team_status":          true,
	"close_team":           true,
	"wait_team":            true,
	"send_message":         true,
	"interrupt_agent":      true,
	"list_subagent_models": true,
}

// presetNames 是解析后的预设表。standard 在包初始化时由 all - compatAlias 计算，
// 因此只要 allToolNames 更新，standard 自动跟随。
var presetNames = map[string][]string{
	PresetMinimal:  minimalToolNames,
	PresetStandard: standardToolNames(),
	PresetAll:      allToolNames,
}

// Available 返回支持的预设名（固定展示顺序：minimal、standard、all）。
func Available() []string {
	return []string{PresetMinimal, PresetStandard, PresetAll}
}

// Names 返回预设包含的工具名（拷贝，调用方可安全修改）。ok=false 表示未知预设；
// 空 preset 解析为 DefaultPreset（standard）。
func Names(preset string) (names []string, ok bool) {
	resolved := normalizePreset(preset)
	names, ok = presetNames[resolved]
	if !ok {
		return nil, false
	}
	return append([]string(nil), names...), true
}

// Filter 按预设过滤 tools，保持输入顺序并按工具名去重（保留第一次出现）；nil
// 工具被跳过。未知预设返回错误。返回的切片是新切片，不改动入参。
func Filter(preset string, tools []rig.Tool) ([]rig.Tool, error) {
	names, ok := Names(preset)
	if !ok {
		return nil, fmt.Errorf("presets: unknown preset %q (available: %s)", preset, strings.Join(Available(), ", "))
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	out := make([]rig.Tool, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(tool.Name())
		if name == "" || !allowed[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, tool)
	}
	return out, nil
}

// standardToolNames 返回 all 去掉兼容别名后的名字表（保持 all 的顺序）。
func standardToolNames() []string {
	out := make([]string, 0, len(allToolNames))
	for _, name := range allToolNames {
		if compatAliasNames[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// normalizePreset 归一化预设名：trim + 小写，空值解析为 DefaultPreset。
func normalizePreset(preset string) string {
	preset = strings.ToLower(strings.TrimSpace(preset))
	if preset == "" {
		return DefaultPreset
	}
	return preset
}
