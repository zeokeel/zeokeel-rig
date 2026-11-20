// Package presets 提供 agent 工具集预设（旧 agent 的 agent 模式/工具集预设的 SDK
// 部分）。
//
// 预设只做“按名字过滤工具集”，不包含系统 prompt、拓扑或权限策略：Gate/Policy
// 与预设相互独立，宿主可以把同一预设叠加到任意沙箱策略/审批策略之上。工具集
// 来源以 tools/catalog.Static 为准（测试会断言本包与账本一致，防止漂移）。
//
// 四个预设：
//
//   - minimal：极简参考风格的最小可用集——六个 terminal_* 会话工具、read_image
//     与 working_directory。它刻意不含 read_file/bash 等工具，镜像 极简参考 与
//     old 的 ModeMinimal。
//   - standard（默认）：当前全部静态工具中，去掉“兼容旧 agent 的别名”后的集合。
//     现名称（如 get_goal/create_goal/update_goal、present、todo_write、
//     team_spawn/team_list 等）保留；旧别名（goal、todo、spawn_team、list_teams、
//     team_status、close_team、wait_team、send_message、interrupt_agent、
//     list_subagent_models）剔除。
//   - all：tools/catalog.Static 的全部静态工具（含兼容别名）。
//   - personal（kit.New 的默认）：standard 去掉**全部多智能体/编排工具**
//     （subagent、subagent_fork、spawn_teammate、wait_agent、list_agents、team_*、
//     workflow、ralph）。个人客户端是"一个人和一个 agent 结对"：这些工具既要宿主
//     注入 SubagentRunner，只留"发起"不留"收结果"（wait_agent）也会让模型发完就
//     断线。需要多智能体时请用 standard/all 配 full.All(AllOptions{Runner: …})。
//
// server-bound 工具（MCP/LSP，需要配置客户端）与动态 mcp__<server>__<tool> 不在
// 静态集合中；opt-in 的 run_code（kit.Config.PTC）也不是静态工具，因此预设不会
// 隐式启用它们。
//
// 另外提供一张**权限模式 → 被裁掉的工具名**表（ForMode/ModeFilter）：`read-only`
// 档下纯写工具（write_file/edit_file/apply_patch/str_replace_editor）不再注册，
// 其余档位不裁剪。`bash`/`pwsh`/`run_code` 保留——它们经 runner/审批把关，写操作
// 由 runner/策略拒绝（`run_code` 的内层工具集同样先被裁剪），不是工具面该消失的理由。
// kit 在应用预设之后再套这一层，因此
// 被裁掉的工具也不会出现在 InitialActiveTools 或 tool_search 清单里。
package presets

import (
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 预设名。传入 Names/Filter 时会 trim 并转小写；空字符串等价于
// PresetStandard（默认预设）。
const (
	// PresetMinimal 是最小可用工具集（极简参考风格）。
	PresetMinimal = "minimal"
	// PresetStandard 是默认工具集：全部现代静态工具，不含兼容旧 agent 的别名。
	PresetStandard = "standard"
	// PresetAll 是全部静态工具（含兼容别名）。
	PresetAll = "all"
	// PresetPersonal 是**个人 agent 客户端**的推荐默认：standard 去掉多智能体
	// 编排一族（team_*/workflow/ralph/队友间通信）。
	// 理由：个人用户是一个人和一个 agent 结对，11 个 team_* 工具只会让模型
	// 在错误的方向上探索；需要时显式用 "standard"/"all" 打开。
	PresetPersonal = "personal"
	// DefaultPreset 是空 preset 参数解析到的预设。
	DefaultPreset = PresetStandard
)

// allToolNames 是 tools/catalog.Static 的完整名字表（保持账本顺序）。测试会断言
// 它与 catalog.Static 完全一致；tools/presets 不 import catalog，避免运行时把
// 发布账本数据变成依赖。
var allToolNames = []string{
	// 文件系统 / 编辑器 / 图片
	"read_file", "write_file", "edit_file", "str_replace_editor", "apply_patch",
	"list_dir", "glob", "grep", "search", "read_image", "undo",
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
	"inspect",
	// 宿主交互 / 计划 / 目标
	"ask_user", "present", "plan_review", "exit_plan_mode", "goal",
	"get_goal", "create_goal", "update_goal",
	// 动态工具加载
	"tool_search",
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
	// 沙箱可观测性与运行期授权（与 catalog.Static 保序一致）
	"sandbox_status", "request_sandbox_access",
}

// minimalToolNames 是 minimal 预设的精确 allowlist（极简参考 / old ModeMinimal）。
// 使用精确名单而不是 terminal_* 前缀，避免未来新增的 terminal 前缀工具被意外纳入。
var minimalToolNames = []string{
	"terminal_open", "terminal_list", "terminal_read",
	"terminal_send", "terminal_signal", "terminal_close",
	"read_image", "working_directory",
}

// compatAliasNames 是 standard 预设剔除的兼容别名：线协议现代名称已覆盖同名能力，
// 同时暴露两种名字会让模型看到重复能力。all 预设仍保留它们（兼容旧会话/提示词）。
var compatAliasNames = map[string]bool{
	"goal":                 true, // 由 get_goal/create_goal/update_goal 取代
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

// personalExcluded 是个人客户端默认不暴露的编排/合规工具：它们服务的是
// "一堆 agent 协作 + 审计留痕"，不是"一个人和一个 agent 干活"。
var personalExcluded = map[string]bool{
	"team_close": true, "team_interrupt": true, "team_last": true, "team_list": true,
	"team_send": true, "team_spawn": true, "team_status": true,
	"team_task_create": true, "team_task_get": true, "team_task_list": true,
	"team_task_update": true, "team_wait": true,
	"spawn_teammate": true, "list_agents": true, "send_message": true,
	"interrupt_agent": true, "wait_agent": true, "wait_team": true,
	"spawn_team": true, "close_team": true, "list_teams": true,
	"workflow": true, "ralph": true,
	// subagent/subagent_fork 需要 SubagentRunner 注入，而个人客户端默认不提供
	// 多智能体编排；只留"发起"不留"收结果"（wait_agent/list_agents）会让模型
	// 发完就断线。需要多智能体的宿主请用 standard/all + full.All(SubagentRunner)。
	"subagent": true, "subagent_fork": true,
}

// personalToolNames 由 standard 减去 personalExcluded 得出。
func personalToolNames() []string {
	out := make([]string, 0, len(standardToolNames()))
	for _, name := range standardToolNames() {
		if personalExcluded[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// presetNames 是解析后的预设表。standard 在包初始化时由 all - compatAlias 计算，
// 因此只要 allToolNames 更新，standard 自动跟随。
var presetNames = map[string][]string{
	PresetMinimal:  minimalToolNames,
	PresetStandard: standardToolNames(),
	PresetAll:      allToolNames,
	PresetPersonal: personalToolNames(),
}

// Available 返回支持的预设名（固定展示顺序：minimal、standard、all）。
func Available() []string {
	return []string{PresetMinimal, PresetPersonal, PresetStandard, PresetAll}
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

// modeDroppedTools 是"权限模式 → 该模式下一律不注册的工具名"表。
//
// 只登记**纯写**工具：它们在 read-only 档下注定失败，留着只会让模型点开一次、
// 被审批层拒一次（白烧一轮上下文与一次工具往返）。**有意不登记**：
//
//   - `bash`/`pwsh`：经 runner 与审批把关，read-only 档下同样能跑只读命令
//     （`ls`、`git log`），写操作由策略拒绝——工具本身仍然有用。
//   - `run_code`（PTC，opt-in，不在静态账本里）：同样经 runner/策略，内层工具
//     调用走同一个 Gate；kit 在追加 run_code **之前**就裁完工具面，所以它拿到的
//     内层工具集里本来就没有这些写工具——保留它不会绕开裁剪。
//   - `undo`、`memory`、`todo_write` 等：不是"改工作区文件"的写工具。
//
// 未登记的模式（含空串与未知值）一律不裁剪：宽松失败，避免一个拼错的档位把
// 工具面悄悄砍掉一半。
var modeDroppedTools = map[sandbox.Mode][]string{
	sandbox.ModeReadOnly: {"write_file", "edit_file", "apply_patch", "str_replace_editor"},
}

// ModeFilter 返回按权限模式判定"这个工具名是否保留"的谓词。
//
// 它与 ForMode 共用同一张表（modeDroppedTools），因此 kit 的接线与宿主的自查
// 不会出现两套口径。未知模式返回"全部保留"的谓词。
func ModeFilter(mode sandbox.Mode) func(string) bool {
	dropped := modeDroppedTools[mode]
	if len(dropped) == 0 {
		return func(string) bool { return true }
	}
	skip := make(map[string]bool, len(dropped))
	for _, name := range dropped {
		skip[name] = true
	}
	return func(name string) bool { return !skip[name] }
}

// ForMode 按权限模式裁剪工具名：保持输入顺序、不改入参，纯过滤（不去重）。
//
// 名字不在表里的模式原样返回。它只回答"这个档位下这个名字该不该出现"，与
// 预设（minimal/personal/standard/all）正交，两者可以叠加使用。
func ForMode(mode sandbox.Mode, names []string) []string {
	keep := ModeFilter(mode)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if keep(name) {
			out = append(out, name)
		}
	}
	return out
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
