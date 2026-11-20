package mcp

import "strings"

// legacyResourcePrefix 是 v0.24.0 之前资源/提示工具名共同的前缀：旧名是
// `mcp_<base>`，完全没有 server 维度。
const legacyResourcePrefix = "mcp_"

// MigrateLegacyNames 把 v0.24.0 之前的 MCP 工具名映射成新形状。
// 需要的 server 名从 legacy 名字里推不出来（旧名没有 server 维度），
// 所以给定 server 时返回可迁移的新名；推不出来的进 needsUser。
//
// 规则：
//
//   - 旧名 `mcp_<base>`（base 取自 resource_tools.go 的五个基础名：
//     list_resources、read_resource、list_resource_templates、list_prompts、
//     get_prompt）在给定 server 时迁移成 `mcp__<sanitize(server)>__<base>`，
//     顺序与输入一致；
//   - server 为空（或只有空白）时无法确定新名，五个旧名全部原样进 needsUser；
//   - 无法识别的名字（不是五个旧名之一，例如 `search_web`）原样进 needsUser，
//     不静默丢掉：宿主可以把它们展示给用户重选，或原样保留在配置里。
//
// 迁移结果是新名，需要宿主替换旧配置项；needsUser 非空意味着这次迁移需要用户
// 补一个 server 名或重新选择工具。
func MigrateLegacyNames(names []string, server string) (migrated []string, needsUser []string) {
	server = strings.TrimSpace(server)
	for _, name := range names {
		base, known := legacyResourceBase(name)
		if !known || server == "" {
			needsUser = append(needsUser, name)
			continue
		}
		migrated = append(migrated, DefaultPrefix+sanitizeName(server)+"__"+base)
	}
	return migrated, needsUser
}

// legacyResourceBase 判断一个名字是否是 v0.24.0 之前的资源/提示工具名，并返回
// 它对应的基础名。
func legacyResourceBase(name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	base, ok := strings.CutPrefix(trimmed, legacyResourcePrefix)
	if !ok {
		return "", false
	}
	for _, known := range resourceToolBases {
		if base == known {
			return base, true
		}
	}
	return "", false
}
