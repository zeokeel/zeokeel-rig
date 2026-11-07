// Package skill 提供 skill 工具与基于文件系统的 rig.SkillStore 实现。
//
// skill 是一份 Markdown 文档（约定为 <dir>/SKILL.md），agent 可以按需列出、搜索并加载。
// 工具刻意保持很小——只有一个模型可见的 skill 工具（action=list|search|load），使宿主可以
// 挂载很大的技能库而不必预先消耗上下文：
//
//	{"action":"list"}
//	{"action":"search","query":"release"}
//	{"action":"load","name":"release-checklist"}
//
// 为兼容 DSH 的 name-only 调用，省略 action 时：给出 name 视为 load，否则视为 list。
//
// # 技能发现
//
// FileStore 会扫描一个或多个目录下的 */SKILL.md 以及（可选的）顶层 *.md 文件。技能名取
// 目录名（或去掉 .md 的文件名）；描述来自最简 description: 行或 YAML 风格 front matter，
// 找不到时回退到第一个 "# 标题" 或首行文本。读取被限制在配置目录内：逃逸目录的符号链接
// 会被跳过，缺失目录忽略，重名保留先出现者（按给定目录顺序扫描）。单个技能文件最多读取
// MaxSkillBytes，超出部分截断。
//
// # 模型可见工具
//
//   - skill {action 可选, query 可选, name 可选}：action=list 列出 "- name: description"；
//     action=search 对名称、描述与正文做大小写不敏感匹配（多词查询要求每个词都出现）；
//     action=load 返回完整技能内容，列表/正文输出均受 MaxOutputBytes（32 KiB）限制。
//
// # 组装与依赖
//
// 通过 Tools(store) 构造并注册。store 为 nil（或带类型 nil）时回退到
// NewDefaultFileStore，搜索路径为冒号分隔的 $ZEOKEEL_SKILLS、<root>/.zeokeel/skills 与
// ~/.zeokeel/skills：
//
//	reg := tools.NewRegistry(skill.Tools(nil)...)
//
// 权限：工具只读，不写文件、不访问网络或 shell；文件访问由 rig.SkillStore 实现控制，默认
// FileStore 会跳过越界符号链接。审批在 agent 层由 rig.Gate 控制。
package skill
