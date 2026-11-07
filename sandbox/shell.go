package sandbox

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Shell 解析与 shell 命令策略。
//
// Rig 的命令允许列表是进程内策略层，不是安全边界：它的存在是为了让宿主约束一个
// 配合的模型能运行什么，而审批 Gate（对不可信模型还有 OS 级沙箱）仍然才是真正的
// 边界。
//
// 策略仍必须与 shell 对“命令是什么”的理解一致。为了避免重新实现 POSIX 引号处理
// （之前的版本自带三份手写分词器，却仍然漏掉了反斜杠-换行续行），策略改用业界
// 标准的 mvdan.cc/sh 解析器处理 shell 语法。该解析器与 /bin/sh 唯一不同的一点，
// 是在分词前先移除反斜杠-换行续行，因此 stripShellLineContinuations 先执行这一步，
// 与 shell 的行为完全一致（单引号内除外）。
//
// 在严格（默认）模式下，随后检查 AST 是否为单条简单命令，并拒绝每种能启动或拼接
// 另一个程序的构造：命令替换（包括双引号内、参数展开与算术表达式中的）、反引号
// 替换、进程替换、管道、列表（; / && / ||）、子 shell、重定向、后台任务、函数
// 定义与控制结构。纯算术展开是允许的（其中嵌套的命令替换仍会被遍历拒绝）。
//
// 使用 WithAllowShellOperators 后，策略不拒绝 shell 语法；此时返回第一条简单命令
// 的各个词，以便仍能应用允许列表（该模式下仅作参考）。

// ParseShellCommand 按 shell 语法策略校验 command，并返回其词元（argv）供命令
// 允许/拒绝检查使用。它是内置 bash、pwsh、jobs 与 terminal 工具共用的唯一解析器，
// 因此语法校验与 argv[0] 检查永远不会在对引号的理解上产生分歧。
//
// 调用方仍必须对返回的 argv 应用 CheckCommand，并且必须把结果当作策略输入而非
// 安全边界：审批 Gate 才是不可逆操作的真边界，模型不可信时还需要 OS 级沙箱；
// 需要 OS 隔离时请把宿主运行在容器/VM 中。
//
// 使用 WithAllowShellOperators 时，策略不拒绝 shell 语法；命令会被解析，并返回
// 第一条简单命令的词元（此时允许列表仅作参考）。
func (p *Policy) ParseShellCommand(command string) ([]string, error) {
	if p == nil {
		return nil, deny("nil policy")
	}
	normalized := stripShellLineContinuations(command)
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(normalized), "")
	if err != nil {
		return nil, deny("shell command is not valid POSIX sh: %v", err)
	}
	if p.allowShellOperators {
		return firstCommandWords(file, normalized), nil
	}
	return validateSimpleCommand(file, normalized)
}

// CheckShellCommand 校验一个将由 /bin/sh -c（或等价脚本引擎）执行的命令串，作为
// CheckCommand 的 argv[0] 检查之外的补充。它是 ParseShellCommand 的校验部分；
// 同时需要 argv[0] 的工具应只调用一次 ParseShellCommand，而不是两个函数都调用。
//
// 允许列表只能看到第一条命令，因此任何能启动或拼接另一个程序的构造都会被拒绝，
// 除非策略用 WithAllowShellOperators 显式关闭该检查：
//
//   - 未加引号的 shell 控制操作符：;、换行、|、&、&&、||、重定向；
//   - 命令替换 `$(...)` 与反引号，包括双引号内、参数展开与算术表达式中的；
//   - 进程替换 `<(...)` 与 `>(...)`；
//   - 续行在解析前被移除（策略模拟 /bin/sh 实际看到的输入），因此
//     `"$\<换行>(id)"` 会按它最终变成的 `$(...)` 被拒绝。
//
// 单引号内文本完全按字面处理。双引号内文本是字面文本，但上述替换构造除外。被转义
// 的操作符保持字面（echo a\;b）。命令前的前置环境变量赋值由 CheckCommand 拒绝。
// WithAllowShellOperators 会完全关闭该检查。
//
// 不使用 /bin/sh -c 的工具（例如 PowerShell）会得到同样保守的拒绝；请传入已经把
// 目标操作符加好引号的命令，或按策略显式关闭该检查。
func (p *Policy) CheckShellCommand(command string) error {
	if p == nil {
		return deny("nil policy")
	}
	if p.allowShellOperators {
		return nil
	}
	_, err := p.ParseShellCommand(command)
	return err
}

// validateSimpleCommand 对已解析的 POSIX 文件执行严格策略，并返回其唯一简单命令
// 的各个词。
func validateSimpleCommand(file *syntax.File, source string) ([]string, error) {
	if file == nil {
		return nil, deny("shell command is empty")
	}
	if len(file.Stmts) == 0 {
		return nil, nil // 空命令或只有注释；CheckCommand 会拒绝空 argv
	}
	if len(file.Stmts) > 1 {
		return nil, deny("shell command must be a single simple command: compound commands and separators (;, &&, ||, newline) are not allowed (use WithAllowShellOperators to opt out)")
	}
	stmt := file.Stmts[0]
	if stmt.Negated {
		return nil, deny("shell command negation (!) is not allowed (use WithAllowShellOperators to opt out)")
	}
	if stmt.Background {
		return nil, deny("background shell commands (&) are not allowed (use WithAllowShellOperators to opt out)")
	}
	if stmt.Coprocess {
		return nil, deny("coprocess shell commands are not allowed (use WithAllowShellOperators to opt out)")
	}
	if len(stmt.Redirs) > 0 {
		return nil, deny("shell redirections are not allowed (use WithAllowShellOperators to opt out)")
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok {
		return nil, deny("shell command must be a simple command: pipes, subshells, blocks, tests and control structures are not allowed (use WithAllowShellOperators to opt out)")
	}
	if len(call.Assigns) > 0 {
		return nil, deny("environment variable assignments before the command are not allowed in sandboxed commands")
	}
	if len(call.Args) == 0 {
		return nil, nil
	}
	if err := rejectSubstitutions(file); err != nil {
		return nil, err
	}
	argv := make([]string, 0, len(call.Args))
	for index, word := range call.Args {
		value, static := decodeShellWord(word, source)
		if index == 0 && !static {
			return nil, deny("the command name must be a literal word (expansions in argv[0] cannot be checked against the allowlist)")
		}
		argv = append(argv, value)
	}
	return argv, nil
}

// rejectSubstitutions 遍历整个 AST，拒绝每个能运行另一个程序的构造，无论它出现在
// 哪里（双引号、参数展开、算术表达式、here-string 等）。
func rejectSubstitutions(file *syntax.File) error {
	var found error
	syntax.Walk(file, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst:
			found = deny("command substitution $(...) and backticks are not allowed (use WithAllowShellOperators to opt out)")
			return false
		case *syntax.ProcSubst:
			found = deny("process substitution <(...) and >(...) are not allowed (use WithAllowShellOperators to opt out)")
			return false
		}
		return true
	})
	return found
}

// firstCommandWords 返回文件中第一条简单命令的各个词。用于
// WithAllowShellOperators 关闭检查的模式，此时允许列表仅作参考，只有 argv[0]
// 有意义。
func firstCommandWords(file *syntax.File, source string) []string {
	if file == nil {
		return nil
	}
	var call *syntax.CallExpr
	syntax.Walk(file, func(node syntax.Node) bool {
		if call != nil {
			return false
		}
		if candidate, ok := node.(*syntax.CallExpr); ok {
			call = candidate
			return false
		}
		return true
	})
	if call == nil || len(call.Args) == 0 {
		return nil
	}
	argv := make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		value, _ := decodeShellWord(word, source)
		argv = append(argv, value)
	}
	return argv
}

// stripShellLineContinuations 在单引号之外移除 POSIX 续行（反斜杠后跟换行），
// 模拟 shell 在分词前所做的处理。单引号内每个字节都是字面量，因此其中的
// 反斜杠-换行会被保留。双引号不改变规则：其中的反斜杠-换行同样会被移除。
func stripShellLineContinuations(command string) string {
	if !strings.Contains(command, "\\\n") {
		return command
	}
	var out strings.Builder
	out.Grow(len(command))
	state := shellUnquoted
	runes := []rune(command)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		switch state {
		case shellSingleQuoted:
			if r == '\'' {
				state = shellUnquoted
			}
			out.WriteRune(r)
		case shellDoubleQuoted:
			switch r {
			case '\\':
				if index+1 < len(runes) && runes[index+1] == '\n' {
					index++
					continue
				}
				out.WriteRune(r)
				if index+1 < len(runes) {
					out.WriteRune(runes[index+1])
					index++
				}
			case '"':
				state = shellUnquoted
				out.WriteRune(r)
			default:
				out.WriteRune(r)
			}
		default:
			switch r {
			case '\\':
				if index+1 < len(runes) && runes[index+1] == '\n' {
					index++
					continue
				}
				out.WriteRune(r)
				if index+1 < len(runes) {
					out.WriteRune(runes[index+1])
					index++
				}
			case '\'':
				state = shellSingleQuoted
				out.WriteRune(r)
			case '"':
				state = shellDoubleQuoted
				out.WriteRune(r)
			default:
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

// shellQuoteState 记录应用续行规则所需的引号上下文。
type shellQuoteState int

const (
	shellUnquoted shellQuoteState = iota
	shellSingleQuoted
	shellDoubleQuoted
)

// decodeShellWord 返回 shell 词的值，以及它是否为静态词（不含展开）。静态词会被
// 去引号/去转义，供允许列表检查使用；非静态词按源码文本返回，以便调用方看到
// 原始写法而不必展开它。
func decodeShellWord(word *syntax.Word, source string) (string, bool) {
	if word == nil {
		return "", true
	}
	var builder strings.Builder
	static := true
	for _, part := range word.Parts {
		switch typed := part.(type) {
		case *syntax.Lit:
			builder.WriteString(unescapeShellLit(typed.Value, false))
		case *syntax.SglQuoted:
			if typed.Dollar {
				static = false // $'...' 会执行转义；保守处理
			}
			builder.WriteString(typed.Value)
		case *syntax.DblQuoted:
			for _, inner := range typed.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					static = false
					continue
				}
				builder.WriteString(unescapeShellLit(lit.Value, true))
			}
		default:
			static = false
		}
	}
	if static {
		return builder.String(), true
	}
	if word.Pos().IsValid() && word.End().IsValid() {
		start, end := int(word.Pos().Offset()), int(word.End().Offset())
		if start >= 0 && end <= len(source) && start < end {
			return source[start:end], false
		}
	}
	return builder.String(), false
}

// unescapeShellLit 移除一个字面片段上的 POSIX 反斜杠引用。双引号内只有 $、`、"
// 与 \（以及换行）会因反斜杠而失去含义；其他位置每个字符都会失去反斜杠。
func unescapeShellLit(raw string, inDoubleQuotes bool) string {
	if !strings.Contains(raw, "\\") {
		return raw
	}
	var out strings.Builder
	out.Grow(len(raw))
	for index := 0; index < len(raw); index++ {
		char := raw[index]
		if char != '\\' || index+1 >= len(raw) {
			out.WriteByte(char)
			continue
		}
		next := raw[index+1]
		if inDoubleQuotes {
			switch next {
			case '$', '`', '"', '\\':
				out.WriteByte(next)
				index++
				continue
			default:
				out.WriteByte(char)
				continue
			}
		}
		out.WriteByte(next)
		index++
	}
	return out.String()
}
