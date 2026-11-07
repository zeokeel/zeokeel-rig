// Package hosttools 提供需要宿主能力的 SDK 侧工具：ask_user、present（文件交付）、
// present_artifact、plan_review、exit_plan_mode、goal，以及与 DSH 同名的目标工具
// get_goal、create_goal、update_goal。
//
// 工具保持无头：只依赖根包的宿主接口（rig.Asker、rig.Presenter、rig.FilePresenter、
// rig.PlanController、rig.GoalStore）以及本包声明的可选 GoalRevisionStore 能力，从不 import
// UI 或存储。产品通过 Options 注入自己的实现；只需要确定性工具的独立 agent 可以使用
// NewMemoryHost（其 *Host 用并发安全的进程内状态实现全部宿主接口）。
//
// 宿主未提供对应接口时，每个工具都会返回可读的 IsError 结果：模型被告知能力不可用，而
// 不是让 agent panic。ask_user 还遵循 Options.Interactive：非交互运行即使接入了 Asker 也
// 返回 "no interactive UI"。
//
// 工具可安全并发调用；自身不持有状态。
//
// # 模型可见工具
//
//   - ask_user {question 必填, options []string 可选}：向人类提问并返回答案；非交互运行
//     直接返回不可用错误。
//   - present {files: [{path 必填, description 可选}]}：声明已存在的普通文件为最终交付物
//     （每次 1-8 个）；相对路径按 Root 解析，只做存在性/类型检查，不复制内容。
//   - present_artifact {title 必填, body 可选}：渲染文本产物并返回产物 id（Rig 原有语义）。
//   - plan_review {content 必填}：把计划提交给宿主评审并返回 verdict。
//   - exit_plan_mode {action: approve|cancel|status}：批准/取消/查询 plan 状态。
//   - goal {action: set|get|clear|update, update?, text?}：Rig 旧版目标状态工具；update
//     支持 complete/blocked/pause/resume/edit（需要 store 实现 rig.GoalUpdater）。
//   - get_goal {}、create_goal {objective, max_goal_rounds?}、update_goal {goal_id,
//     revision, action, objective?, max_goal_rounds?, blocked_reason?}：DSH 同名的带修订号
//     CAS 目标工具，需要宿主实现 GoalRevisionStore；create_goal 的默认续跑轮次上限为
//     DefaultMaxGoalRounds=256。
//
// # 组装与权限
//
//	host := hosttools.NewMemoryHost()
//	reg := tools.NewRegistry(hosttools.Tools(hosttools.Options{
//		Root: root, Asker: host, Presenter: host, FilePresenter: host,
//		Plan: host, Goal: host, Interactive: true,
//	})...)
//
// 工具不访问网络或 shell；present 会通过 os.Lstat 检查交付文件（相对 Root 解析），其余
// 能力由宿主实现负责。审批在 agent 层由 rig.Gate 控制。
package hosttools
