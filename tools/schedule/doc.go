// Package schedule 提供 schedule_* 工具与一个进程内 rig.Scheduler 实现。
//
// 宿主可以通过 Tools 注入持久化的 rig.Scheduler；为 nil 时工具回退到 NewScheduler——
// 并发安全、进程内、重启即丢失。
//
// 工具是对任务描述做纯 CRUD：求值 schedule 并触发任务属于宿主，因此工具从不计算 next-run；
// 宿主提供的 NextRun 会原样记录并展示。存储的 Schedule 字符串要么是旧版自由格式值（原样
// 保存），要么是 DSH 兼容的类型化时间选择器的规范形式（after_seconds、at、every_seconds、
// daily、weekly、cron），语法见 selector.go。
//
//	schedule_create {name|title, prompt, <一个时间选择器>?, enabled?}
//	schedule_list
//	schedule_update {id, name|title?, prompt?, enabled?, <一个时间选择器>?}
//	schedule_delete {id}
//
// # 类型化选择器与规范形式
//
// 选择器会被校验并规范化为宿主可解析的稳定字符串：
//
//	after <seconds>s                                  一次性相对延迟
//	at <RFC 3339 UTC 毫秒精度时刻>                    一次性绝对时间
//	every <seconds>s                                  固定间隔重复
//	daily <HH:MM:SS.SSS> <时区>                       每日墙钟时间
//	weekly <HH:MM:SS.SSS> <时区> <ISO 星期 CSV>        每周墙钟时间
//	cron <规范五字段表达式> <时区>
//
// 旧版自由格式 schedule 字符串原样保存、永不重写。校验规则对齐 DSH：every_seconds 至少
// MinEveryIntervalSeconds=60 秒；at 必须是严格未来时刻；时间带可选 1-3 位小数；时区必须是
// UTC 或 IANA Area/Location；weekday 去重升序；cron 规范化为最短等价写法。任务标题最多
// MaxTitleLength=120 个字符（Unicode 码点）。
//
// # 模型可见工具与限制
//
//   - schedule_create {prompt 必填, name 或 title, enabled?, schedule? 或一个类型化选择器}：
//     创建任务并返回其 id/name/schedule/enabled 等；name 与 title 至少给一个。
//   - schedule_list {}：按创建时间与 ID 排序列出 ID/NAME/SCHEDULE/ENABLED/NEXT RUN。
//   - schedule_update {id 必填, name/title/prompt/enabled/schedule/选择器?}：按 ID 更新，
//     未给出的字段保持不变；空 schedule 字符串表示清除。
//   - schedule_delete {id 必填}：删除任务；未知 ID 返回可读错误。
//
// 单次响应上限 DefaultMaxOutputBytes=32 KiB；旧版自由格式 schedule 与类型化选择器互斥。
//
// # 组装与权限
//
//	reg := tools.NewRegistry(schedule.Tools(myScheduler)...)
//
// 工具不访问网络、shell 或文件系统，也不触发任何任务；是否真正执行由宿主 scheduler 决定。
// 审批在 agent 层由 rig.Gate 控制。
package schedule
