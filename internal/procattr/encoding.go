package procattr

// 本文件是**子进程输出与脚本的字符编码**约定，与平台无关（纯函数），因此可以在
// 任意宿主上单测。它解决两类乱码：
//
//  1. 子进程按"活动代码页"输出（中文 Windows 默认 936/GBK），我们却按 UTF-8
//     解码 → 每个双字节汉字变成两个 U+FFFD（再被上层当成"锟斤拷"式的乱码）；
//     修法见 DecodeOutput 与 WrapPwshScript。
//  2. 命令文本里的中文路径/字面量：Go 的 os/exec 在 Windows 上用
//     CreateProcessW 传 UTF-16 命令行，这一层本来就正确（不要在这里"转码"）。
//
// 原则：**不猜编码**。我们没有、也不引入 GBK/CP936 码表（仓库只允许一个第三方
// 依赖）；无法解码的字节一律替换成 U+FFFD，并在结果里明确告知调用方，而不是把
// 乱码原样交给模型。

import (
	"strings"
	"unicode/utf8"
)

// NonUTF8OutputWarning 是"输出不是合法 UTF-8"时追加到结果末尾的提示行。
//
// 它的措辞面向模型与用户：说明**发生了什么**（不是 UTF-8）与**为什么**（命令
// 可能用了非 UTF-8 控制台代码页），而不是暗示内容可以还原。
const NonUTF8OutputWarning = "[warning: output was not valid UTF-8; the command may have used a non-UTF-8 console code page]"

// DecodeOutput 把子进程的原始输出解码成字符串。
//
// 第二个返回值表示**是否发生了替换**（原始字节不是合法 UTF-8）。调用方应当据
// 此给用户/模型一个提示（见 NonUTF8OutputWarning），而不是静默吞掉。
//
// 为什么不用 GBK 兜底转码：一是要维护码表（违反"只允许一个依赖"）；二是猜错
// 比替换更糟——猜错会把乱码变成**看起来正常**的错误文本，那比明显的 U+FFFD
// 更难排查。U+FFFD 至少诚实地表示"这里的内容我们读不懂"。
func DecodeOutput(raw []byte) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	// 输出被截断时，尾巴可能是半个多字节字符：那是**截断**，不是编码错误，
	// 静默丢弃这几个字节即可（否则每次按字节截断都会凭空多一个 U+FFFD 与
	// 一条误导性的告警）。
	raw = trimIncompleteTail(raw)
	if utf8.Valid(raw) {
		return string(raw), false
	}
	return strings.ToValidUTF8(string(raw), "\uFFFD"), true
}

// DecodeOutputString 与 DecodeOutput 相同，但接受已转成 string 的字节。
//
// 注意：Go 字符串可以承载任意字节，所以 string→[]byte 往返不会丢信息；上游
// 若已经把原始字节塞进 string（例如 bytes.Buffer.String()），用它即可。
func DecodeOutputString(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	if utf8.ValidString(text) {
		return text, false
	}
	return DecodeOutput([]byte(text))
}

// truncationTailLimit 是一个 UTF-8 序列的最大长度减一（最多回退 3 字节）。
const truncationTailLimit = utf8.UTFMax - 1

// trimIncompleteTail 丢掉尾部"半个多字节字符"。
//
// 只在**尾部**动手：中间的非法字节是真实的编码问题，必须留给调用方替换并告警。
func trimIncompleteTail(raw []byte) []byte {
	for back := 1; back <= truncationTailLimit && back <= len(raw); back++ {
		tail := raw[len(raw)-back:]
		if !utf8.RuneStart(tail[0]) {
			// 尾字节落在续字节上：继续向前找序列的首字节。
			continue
		}
		if incompleteTail(tail) {
			return raw[:len(raw)-back]
		}
		return raw
	}
	return raw
}

// incompleteTail 报告 tail 是否是一个"合法序列的合法前缀"（即被截断了）。
func incompleteTail(tail []byte) bool {
	size := runeByteLen(tail[0])
	if size <= len(tail) {
		return false
	}
	for _, b := range tail[1:] {
		if b&0xC0 != 0x80 {
			return false
		}
	}
	return true
}

// runeByteLen 返回以 lead 字节开头的 UTF-8 序列长度（非法 lead 返回 1）。
func runeByteLen(lead byte) int {
	switch {
	case lead&0x80 == 0x00:
		return 1
	case lead&0xE0 == 0xC0:
		return 2
	case lead&0xF0 == 0xE0:
		return 3
	case lead&0xF8 == 0xF0:
		return 4
	default:
		return 1
	}
}

// PwshUTF8Prologue 是 Windows 上强制 UTF-8 的 PowerShell 前置语句。
//
// 三件事，缺一不可（中文 Windows 的默认活动代码页是 936）：
//   - `[Console]::OutputEncoding = UTF8`：PowerShell 自己写 stdout 时按 UTF-8
//     编码，**并且**按 UTF-8 解释原生子进程（cmd.exe 等）的输出；
//   - `$OutputEncoding = UTF8`：PowerShell 管道把文本传给原生子进程时的编码；
//   - `chcp.com 65001`：把**控制台代码页**也切成 UTF-8，否则 cmd.exe / 原生
//     子进程仍按 936 输出（只设前两项会让它们的输出被错解释）。
//
// 每步都包 try/catch：没有控制台（输出被重定向）时设置 OutputEncoding 或 chcp
// 都可能抛异常，而"编码没设置成功"绝不能让整条命令失败。
//
// 为什么用 `New-Object System.Text.UTF8Encoding($false)` 而不是
// `[System.Text.Encoding]::UTF8`：后者带 BOM 前导，在 Windows PowerShell 5.1 上
// 赋给 `[Console]::OutputEncoding` 会让**第一段输出带一个 EF BB BF**，污染模型
// 看到的内容与按行解析。`$false` 关掉前导；`::new()` 在 5.1 上不存在，因此用
// New-Object 写法（5.1 与 7+ 都支持）。
//
// **退出码**：chcp 会改写 `$LASTEXITCODE`，所以前置语句先保存、末尾恢复它；
// 这样"用户脚本本身的退出码"语义完全不变（纯 cmdlet 脚本仍然是 0，原生命令
// 仍然透传它自己的码）。
const PwshUTF8Prologue = `$__rig_sbx_lec = $global:LASTEXITCODE; ` +
	`$__rig_sbx_enc = $null; ` +
	`try { $__rig_sbx_enc = New-Object System.Text.UTF8Encoding($false) } catch { }; ` +
	`if ($__rig_sbx_enc) { ` +
	`try { [Console]::OutputEncoding = $__rig_sbx_enc } catch { }; ` +
	`try { $global:OutputEncoding = $__rig_sbx_enc } catch { } }; ` +
	`try { $null = & chcp.com 65001 } catch { }; ` +
	`$global:LASTEXITCODE = $__rig_sbx_lec;`

// WrapPwshScript 在脚本前插入 PwshUTF8Prologue。
//
// 不能前置的脚本原样返回（见 PwshScriptAllowsPrologue）——那些脚本通常自带
// `param()`/`using` 声明，只能在命令的最前面，插入任何语句都会让整条命令变成
// 语法错误。这种情况下我们**不**牺牲可用性去换编码：输出仍走 DecodeOutput 的
// U+FFFD 兜底。
func WrapPwshScript(script string) string {
	if !PwshScriptAllowsPrologue(script) {
		return script
	}
	return PwshUTF8Prologue + "\n" + script
}

// pwshPrologueBlockedPrefixes 列出"必须是第一条语句"的 PowerShell 声明。
//
//   - `param(...)`：只能是脚本/脚本块的第一条语句；
//   - `using module|namespace|assembly`：只能出现在脚本最前面；
//   - `#requires`：必须是第一行（可以在注释前，但不能被其它语句顶掉）。
var pwshPrologueBlockedPrefixes = []string{"param", "using", "#requires"}

// PwshScriptAllowsPrologue 报告能否安全地在脚本前插入前置语句。
func PwshScriptAllowsPrologue(script string) bool {
	trimmed := strings.TrimLeft(script, " \t\r\n")
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(strings.ToLower(trimmed), "#requires") {
		// 普通注释开头：插入语句后注释仍然自成一行，安全。
		return true
	}
	lower := strings.ToLower(trimmed)
	for _, prefix := range pwshPrologueBlockedPrefixes {
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		switch prefix {
		case "param":
			// `param(` 与 `param (` 都是声明；`parameter` 这种标识符不是。
			rest := strings.TrimLeft(trimmed[len(prefix):], " \t")
			if strings.HasPrefix(rest, "(") {
				return false
			}
		case "using":
			// `using` 之后必须跟空白才是声明（`$using:x` 不是开头声明）。
			rest := trimmed[len(prefix):]
			if rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
				return false
			}
		case "#requires":
			return false
		}
	}
	return true
}
