package escalate

// 本文件是宿主侧的配置校验与 argv 前缀匹配：它只处理不可信输入里"能不能执行"这一
// 部分，不做 IO、不碰状态，因此可以直接单测。
//
// 与 sandbox/escalate 的 Policy 保持同一套前缀语义（逐元素相等；末元素额外接受
// `--flag=value` 形式）——工具侧先判一次是为了让模型立刻看到清晰的拒绝，Broker 侧
// 会再判一次（纵深防御，Broker 才是信任边界）。

import (
	"fmt"
	"strings"
)

// forbiddenPrograms 是禁止出现在 argv[0] 的解释器/执行器。
//
// 为什么：Allow 列表就是权限边界，而解释器的"参数"就是任意程序 + 任意代码。一条
// 允许 `sh` 的条目等价于允许一切；`env`/`xargs`/`find`/`make` 同样能把任意命令
// 塞进参数里执行。这条检查在构造期做（fail closed），不是运行期。
var forbiddenPrograms = map[string]struct{}{
	"sh": {}, "bash": {}, "zsh": {}, "dash": {}, "ksh": {}, "fish": {}, "csh": {}, "tcsh": {},
	"python": {}, "python2": {}, "python3": {},
	"node": {}, "nodejs": {}, "deno": {}, "bun": {},
	"env": {}, "xargs": {}, "find": {}, "make": {}, "gmake": {},
	"awk": {}, "gawk": {}, "mawk": {}, "nawk": {}, "perl": {},
	"ruby": {}, "php": {}, "lua": {}, "rscript": {},
	"pwsh": {}, "powershell": {}, "cmd": {},
	"eval": {}, "exec": {},
}

// dangerousGitSubcommands 是 git 的子命令黑名单：它们本身就能执行外部程序。
//
//   - difftool / mergetool：调用用户配置的外部 diff/merge 工具（任意命令）；
//   - bisect run：在 bisect 过程中执行任意命令；
//   - submodule：会跑 `git submodule--helper` 并可能执行仓库内的钩子。
var dangerousGitSubcommands = map[string]struct{}{
	"difftool":  {},
	"mergetool": {},
	"bisect":    {},
	"submodule": {},
}

// dangerousGitFlags 是会把"只读子命令"变成执行器或越界读写的 git flag。
//
// 与 sandbox/escalate 的 builtinDangerousArgs 是同一份名单（那边是运行期兜底，
// 这边在构造期就把错误配置暴露出来）。支持 `--flag=value` 形式。
var dangerousGitFlags = []string{
	"--upload-pack", "--receive-pack", "--exec-path", "--template",
	"-c", "--config-env",
	"--ext-diff", "--textconv",
	"-O", "--open-files-in-pager",
	"--output",
	"--no-index",
}

// matchPrefix 在 list 里找第一个"argv 的前缀"，返回命中的条目。
//
// 语义与 sandbox/escalate.matchPrefix 一致：逐元素相等；唯一放宽在最后一个元素，
// 它同时匹配 `token=value` 形式（否则加个等号就能绕过拒绝列表）。
func matchPrefix(list [][]string, argv []string) ([]string, bool) {
	for _, entry := range list {
		if len(entry) == 0 || len(entry) > len(argv) {
			continue
		}
		matched := true
		for i, token := range entry {
			if argv[i] == token {
				continue
			}
			if i == len(entry)-1 && strings.HasPrefix(argv[i], token+"=") {
				continue
			}
			matched = false
			break
		}
		if matched {
			return entry, true
		}
	}
	return nil, false
}

// MatchAllow 报告 argv 是否命中 allow 中的某个前缀，并返回命中的条目。
//
// 导出给宿主用：审批回调只拿到 escalate.Request（argv + reason），如果审批提示
// 需要展示"这条命令命中的是哪一条 Allow"，宿主可以自己调这个函数算一遍（工具内部
// 用的是同一份实现，因此结论一致）。
func MatchAllow(allow [][]string, argv []string) ([]string, bool) { return matchPrefix(allow, argv) }

// validatePrefixLists 校验 Allow/Deny/Approval 的形状：非空、无非空 token、无 NUL。
//
// checkDangerous 为真时（Allow 与 Approval）还会拒绝解释器与危险的 git 子命令/flag；
// Deny 不检查——把 `sh` 列进禁止列表是收紧而不是放开，不该报错。
func validatePrefixLists(name string, list [][]string, checkDangerous bool) error {
	for i, entry := range list {
		if len(entry) == 0 {
			return fmt.Errorf("%s[%d] 是空前缀（空前缀会匹配一切）", name, i)
		}
		for j, token := range entry {
			if strings.TrimSpace(token) == "" {
				return fmt.Errorf("%s[%d][%d] 是空字符串", name, i, j)
			}
			if strings.ContainsRune(token, 0) {
				return fmt.Errorf("%s[%d][%d] 含 NUL 字节", name, i, j)
			}
		}
		if checkDangerous {
			if err := validateNoInterpreter(name, i, entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateNoInterpreter 拒绝"允许解释器"或"允许 git 的危险子命令/flag"的条目。
func validateNoInterpreter(name string, index int, entry []string) error {
	program := normalizeProgram(entry[0])
	if _, forbidden := forbiddenPrograms[program]; forbidden {
		return fmt.Errorf("%s[%d] 允许了解释器/执行器 %q：它的参数就是任意程序，等价于允许一切（不要把它放进 Allow）",
			name, index, entry[0])
	}
	if program != "git" {
		return nil
	}
	for _, token := range entry[1:] {
		sub := strings.ToLower(token)
		if _, dangerous := dangerousGitSubcommands[sub]; dangerous {
			return fmt.Errorf("%s[%d] 含 git 子命令 %q：它会执行外部程序或仓库钩子，不能作为提权目标",
				name, index, token)
		}
		for _, flag := range dangerousGitFlags {
			if token == flag || strings.HasPrefix(token, flag+"=") {
				return fmt.Errorf("%s[%d] 含 git 危险参数 %q：它能把只读子命令变成执行器或越界读写",
					name, index, token)
			}
		}
	}
	return nil
}

// normalizeProgram 把程序名规范化成小写、去扩展名、去目录的形态，用于黑名单比较
// （`/usr/bin/BASH` 与 `bash.exe` 都要被认出来）。
func normalizeProgram(program string) string {
	base := program
	if index := strings.LastIndexAny(base, `/\`); index >= 0 {
		base = base[index+1:]
	}
	base = strings.ToLower(base)
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, ".cmd")
	base = strings.TrimSuffix(base, ".bat")
	return base
}

// entryCovered 报告 want 是否被 list 中的某条前缀覆盖（逐元素相等，末元素允许
// `--flag=value`）。用于检查"Approval 条目必须同时被 Allow 覆盖"这类配置矛盾。
func entryCovered(list [][]string, want []string) bool {
	if len(want) == 0 {
		return false
	}
	_, ok := matchPrefix(list, want)
	return ok
}

// validateApprovalCoverage 检查 Approval 与 Allow/Deny 的一致性：
//   - 每条 Approval 必须被 Allow 覆盖（否则它永远不会被允许，属于配置矛盾）；
//   - 每条 Approval 不得被 Deny 覆盖（Deny 优先，写在同一处只会让审批永远无法通过）。
func validateApprovalCoverage(allow, deny, approval [][]string) error {
	for i, entry := range approval {
		if !entryCovered(allow, entry) {
			return fmt.Errorf("选项 Approval[%d]（%s）没有被任何 Allow 前缀覆盖：它在策略上永远不会被执行",
				i, strings.Join(entry, " "))
		}
		if _, denied := matchPrefix(deny, entry); denied {
			return fmt.Errorf("选项 Approval[%d]（%s）同时被 Deny 覆盖：Deny 优先级更高，审批永远不会生效",
				i, strings.Join(entry, " "))
		}
	}
	return nil
}
