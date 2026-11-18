package procattr_test

// 子进程输出/脚本编码约定的纯函数测试（任意宿主可跑）。
//
// 这里的字节序列是**真实的 GBK/CP936 编码**（中文 Windows 的控制台默认代码页），
// 不是"随便几个非法字节"：`中文` 在 GBK 里是 D6 D0 CE C4，在 UTF-8 里是
// E4 B8 AD E6 96 87。把前者当后者解码，就是用户看到的乱码。

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig/internal/procattr"
)

// gbkZhongWen 是 "中文" 的 GBK 字节（0xD6D0 0xCEC4）。
var gbkZhongWen = []byte{0xD6, 0xD0, 0xCE, 0xC4}

// utf8ZhongWen 是 "中文" 的 UTF-8 字节。
var utf8ZhongWen = []byte{0xE4, 0xB8, 0xAD, 0xE6, 0x96, 0x87}

func TestDecodeOutput_GBKBecomesReplacementNotMojibake(t *testing.T) {
	text, replaced := procattr.DecodeOutput(gbkZhongWen)
	if !replaced {
		t.Fatalf("GBK 字节没有被标记为替换：%q", text)
	}
	if !utf8.ValidString(text) {
		t.Fatalf("解码结果不是合法 UTF-8：%q", text)
	}
	// 关键：不能原样保留 GBK 字节（那才是"乱码原样吐给模型"）。
	if strings.Contains(text, string(gbkZhongWen)) {
		t.Fatalf("原始 GBK 字节被原样保留：%q", text)
	}
	// 每个连续非法字节段变成一个 U+FFFD（而不是每个字节一个）。
	if count := strings.Count(text, "\uFFFD"); count == 0 {
		t.Fatalf("结果里没有 U+FFFD：%q", text)
	}
	if text != "\uFFFD" {
		t.Fatalf("两个汉字（一段连续非法字节）应替换成一个 U+FFFD，got %q", text)
	}
}

func TestDecodeOutput_ValidUTF8Untouched(t *testing.T) {
	input := "中文输出\n第二行"
	text, replaced := procattr.DecodeOutput([]byte(input))
	if replaced {
		t.Fatalf("合法 UTF-8 被误判为需要替换：%q", text)
	}
	if text != input {
		t.Fatalf("合法 UTF-8 被改动：%q != %q", text, input)
	}
}

func TestDecodeOutput_Empty(t *testing.T) {
	if text, replaced := procattr.DecodeOutput(nil); text != "" || replaced {
		t.Fatalf("DecodeOutput(nil) = %q, %v", text, replaced)
	}
}

func TestDecodeOutput_MixedValidAndGBK(t *testing.T) {
	raw := append([]byte("prefix "), gbkZhongWen...)
	raw = append(raw, []byte(" suffix")...)
	text, replaced := procattr.DecodeOutput(raw)
	if !replaced {
		t.Fatalf("混合输出应标记替换：%q", text)
	}
	if !strings.HasPrefix(text, "prefix ") || !strings.HasSuffix(text, " suffix") {
		t.Fatalf("合法部分被破坏：%q", text)
	}
	if !utf8.ValidString(text) {
		t.Fatalf("结果不是合法 UTF-8：%q", text)
	}
}

func TestDecodeOutput_TruncatedTailIsDroppedNotReplaced(t *testing.T) {
	// 按字节截断可能把 3 字节的"中"切成前 2 字节：那是截断，不是编码错误，
	// 不该冒出一个 U+FFFD 与一条误导性的非 UTF-8 告警。
	for cut := 1; cut <= 2; cut++ {
		raw := append([]byte("ok "), utf8ZhongWen[:cut]...)
		text, replaced := procattr.DecodeOutput(raw)
		if replaced {
			t.Fatalf("cut=%d：截断尾巴被误判成编码错误：%q", cut, text)
		}
		if strings.Contains(text, "\uFFFD") {
			t.Fatalf("cut=%d：截断尾巴产生了 U+FFFD：%q", cut, text)
		}
		if want := "ok "; text != want {
			t.Fatalf("cut=%d：DecodeOutput = %q，want %q（残缺尾巴应丢弃）", cut, text, want)
		}
	}
	// 刚好切在字符边界（3 字节完整）时，字符必须保留。
	if text, _ := procattr.DecodeOutput(append([]byte("ok "), utf8ZhongWen...)); text != "ok 中文" {
		t.Fatalf("完整字符被丢弃：%q", text)
	}
	if text, replaced := procattr.DecodeOutput(append([]byte("ok "), utf8ZhongWen[:3]...)); text != "ok 中" || replaced {
		t.Fatalf("字符边界截断：got %q, replaced=%v，want \"ok 中\", false", text, replaced)
	}
}

func TestDecodeOutputString_MatchesBytesVersion(t *testing.T) {
	if text, replaced := procattr.DecodeOutputString("中文"); text != "中文" || replaced {
		t.Fatalf("DecodeOutputString(合法) = %q, %v", text, replaced)
	}
	text, replaced := procattr.DecodeOutputString(string(gbkZhongWen))
	if !replaced || utf8.ValidString(text) == false || strings.Contains(text, string(gbkZhongWen)) {
		t.Fatalf("DecodeOutputString(GBK) = %q, %v", text, replaced)
	}
}

func TestPwshUTF8Prologue_CoversAllThreeEncodingsAndExitCode(t *testing.T) {
	prologue := procattr.PwshUTF8Prologue
	for _, want := range []string{
		"[Console]::OutputEncoding = $__rig_sbx_enc",  // 自己写 stdout
		"$global:OutputEncoding = $__rig_sbx_enc",     // 管道给原生子进程
		"New-Object System.Text.UTF8Encoding($false)", // BOM-free: 5.1 上 Encoding::UTF8 会带 EF BB BF
		"chcp.com 65001",                        // 控制台代码页（cmd.exe 等）
		"$__rig_sbx_lec = $global:LASTEXITCODE", // 保存退出码
		"$global:LASTEXITCODE = $__rig_sbx_lec", // 恢复退出码
	} {
		if !strings.Contains(prologue, want) {
			t.Fatalf("前置语句缺少 %q：\n%s", want, prologue)
		}
	}
	// 每一步都必须被 try/catch 包住：没有控制台时这些调用会抛异常。
	if got, want := strings.Count(prologue, "try {"), 4; got != want {
		t.Fatalf("try 块数量 = %d，want %d（编码对象构造 + 两处赋值 + chcp 各自容错）", got, want)
	}
	// 不能直接用 Encoding::UTF8：它在 Windows PowerShell 5.1 上会把 BOM 写进 stdout。
	if strings.Contains(prologue, "[System.Text.Encoding]::UTF8") {
		t.Fatalf("前置语句使用了带 BOM 的 Encoding::UTF8：\n%s", prologue)
	}
	// 前置语句里不能出现非 ASCII：它会进 `-Command`，任何编码歧义都不该由我们引入。
	for _, r := range prologue {
		if r > 0x7E {
			t.Fatalf("前置语句含非 ASCII 字符 %q", r)
		}
	}
}

func TestWrapPwshScript(t *testing.T) {
	script := "Write-Output '中文输出'"
	wrapped := procattr.WrapPwshScript(script)
	if !strings.HasPrefix(wrapped, procattr.PwshUTF8Prologue) {
		t.Fatalf("包裹后没有以前置语句开头：\n%s", wrapped)
	}
	if !strings.HasSuffix(wrapped, script) {
		t.Fatalf("用户脚本没有原样保留在末尾：\n%s", wrapped)
	}
	// 前置语句与用户脚本必须在不同的行：用户脚本里的 `#` 注释、here-string
	// 都依赖行边界。
	rest := strings.TrimPrefix(wrapped, procattr.PwshUTF8Prologue)
	if !strings.HasPrefix(rest, "\n") {
		t.Fatalf("前置语句与脚本之间缺少换行：%q", rest)
	}
}

func TestPwshScriptAllowsPrologue(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   bool
	}{
		{"普通命令", "Write-Output 'hi'", true},
		{"带前导空白", "  \n\tWrite-Output 'hi'", true},
		{"普通注释开头", "# 说明\nWrite-Output 'hi'", true},
		{"param 声明", "param($name)\nWrite-Output $name", false},
		{"param 声明带空格", "param ($name)\nWrite-Output $name", false},
		{"param 大小写", "PARAM($name)", false},
		{"using namespace", "using namespace System.Text\n[Encoding]::UTF8", false},
		{"using module", "using module ./x.psm1", false},
		{"#requires", "#requires -Version 7.0\nWrite-Output 'hi'", false},
		{"#requires 大小写", "#REQUIRES -Version 7.0", false},
		{"空脚本", "   \n\t", false},
		{"param 作为标识符前缀", "$parameter = 1\nWrite-Output $parameter", true},
		{"using 作为变量名", "$usingValue = 1", true},
	}
	for _, testCase := range cases {
		if got := procattr.PwshScriptAllowsPrologue(testCase.script); got != testCase.want {
			t.Errorf("%s: PwshScriptAllowsPrologue(%q) = %v，want %v", testCase.name, testCase.script, got, testCase.want)
		}
		if got := procattr.WrapPwshScript(testCase.script); (got != testCase.script) != testCase.want {
			t.Errorf("%s: WrapPwshScript 行为与判定不一致", testCase.name)
		}
	}
}

// TestDecodeOutput_SilentGBKMojibakeIsUndetectable 明确记录**兜底的边界**。
//
// GBK 里有一批字节对恰好构成合法 UTF-8：`一` 的 GBK 是 D2 BB，而 D2 BB 作为
// UTF-8 是合法的 `һ`（西里尔字母）。这类输出**没有任何非法字节**，因此：
//   - DecodeOutput 不会替换、不会告警；
//   - 结果里是"看起来正常"的错误字符，比 U+FFFD 更难发现。
//
// 这不是 bug 而是信息论边界：从字节层面无法区分"GBK 文本"和"恰好长这样的
// UTF-8 文本"。因此**真正的修法是在源头强制 UTF-8**（pwsh 前置语句把控制台代码页
// 切到 65001），兜底只负责"明显坏掉的那部分"。这个用例把边界钉住，避免以后
// 有人把"有告警"误解成"一定能发现乱码"。
func TestDecodeOutput_SilentGBKMojibakeIsUndetectable(t *testing.T) {
	gbkYi := []byte{0xD2, 0xBB} // GBK 的 "一"
	text, replaced := procattr.DecodeOutput(gbkYi)
	if replaced {
		t.Fatalf("这组字节是合法 UTF-8，不该被判为替换：%q", text)
	}
	if text == "一" {
		t.Fatalf("意外地解出了正确的中文，说明用例前提变了")
	}
	if text != "һ" {
		t.Fatalf("预期是 'һ'（UTF-8 合法但语义错误的字符），got %q", text)
	}
}
