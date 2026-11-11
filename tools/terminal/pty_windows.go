//go:build windows

package terminal

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// Windows ConPTY（Pseudo Console）实现：CreatePseudoConsole +
// STARTUPINFOEX/PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE + CreateProcessW。
// 这样 terminal_* 在 Windows 上是真正的交互式控制台（原生命令行），
// 而不是 pipe 传输。ConPTY 需要 Windows 10 1809+；旧系统 ptySupported
// 返回 false，manager 自动回退到 pipe。

const (
	procThreadAttributePseudoConsole = 0x00020016
	extendedStartupInfoPresent       = 0x00080000
	createUnicodeEnvironment         = 0x00000400
)

type winCoord struct {
	X int16
	Y int16
}

type winStartupInfoEx struct {
	startupInfo   syscall.StartupInfo
	attributeList uintptr
}

var (
	winKernel32 = syscall.NewLazyDLL("kernel32.dll")

	winCreatePseudoConsole       = winKernel32.NewProc("CreatePseudoConsole")
	winClosePseudoConsole        = winKernel32.NewProc("ClosePseudoConsole")
	winInitializeProcThreadAttr  = winKernel32.NewProc("InitializeProcThreadAttributeList")
	winUpdateProcThreadAttribute = winKernel32.NewProc("UpdateProcThreadAttribute")
	winDeleteProcThreadAttr      = winKernel32.NewProc("DeleteProcThreadAttributeList")
	winGetSystemDirectoryW       = winKernel32.NewProc("GetSystemDirectoryW")
)

// ptySupported 报告 ConPTY 是否可用（Windows 10 1809+ 才有对应导出）。
func ptySupported() bool {
	for _, proc := range []*syscall.LazyProc{
		winCreatePseudoConsole,
		winClosePseudoConsole,
		winInitializeProcThreadAttr,
		winUpdateProcThreadAttribute,
	} {
		if err := proc.Find(); err != nil {
			return false
		}
	}
	return true
}

// startPTYCommand 创建一个 ConPTY，并把 argv 作为其首进程启动（原生
// PowerShell/cmd），返回可读写的 master 端与进程句柄。
func startPTYCommand(argv []string, workdir string, env []string, cols, rows int) (*ptyEnd, process, error) {
	if !ptySupported() {
		return nil, nil, errors.New("terminal: ConPTY is not available on this Windows version")
	}
	if len(argv) == 0 {
		return nil, nil, errors.New("pty shell argv is empty")
	}
	var inputRead, inputWrite, outputRead, outputWrite syscall.Handle
	if err := syscall.CreatePipe(&inputRead, &inputWrite, nil, 0); err != nil {
		return nil, nil, fmt.Errorf("ConPTY: create input pipe: %w", err)
	}
	if err := syscall.CreatePipe(&outputRead, &outputWrite, nil, 0); err != nil {
		_ = syscall.CloseHandle(inputRead)
		_ = syscall.CloseHandle(inputWrite)
		return nil, nil, fmt.Errorf("ConPTY: create output pipe: %w", err)
	}
	hPC, err := createPseudoConsole(cols, rows, inputRead, outputWrite)
	// The pseudo console has its own copies of the child-side ends.
	_ = syscall.CloseHandle(inputRead)
	_ = syscall.CloseHandle(outputWrite)
	if err != nil {
		_ = syscall.CloseHandle(inputWrite)
		_ = syscall.CloseHandle(outputRead)
		return nil, nil, err
	}
	proc, err := startProcessWithPseudoConsole(hPC, argv, workdir, env)
	if err != nil {
		_ = syscall.CloseHandle(inputWrite)
		_ = syscall.CloseHandle(outputRead)
		winClosePseudoConsole.Call(hPC)
		return nil, nil, err
	}
	reader := os.NewFile(uintptr(outputRead), "conpty-output")
	writer := os.NewFile(uintptr(inputWrite), "conpty-input")
	terminal := &ptyEnd{
		reader: reader,
		writer: writer,
		closeFn: func() error {
			// ClosePseudoConsole waits for the output to drain on older
			// Windows; keep the reader open until it returns.
			_ = writer.Close()
			winClosePseudoConsole.Call(hPC)
			_ = reader.Close()
			return nil
		},
	}
	return terminal, proc, nil
}

// createPseudoConsole 调用 CreatePseudoConsole；COORD 通过一个 uintptr 传入。
func createPseudoConsole(cols, rows int, input, output syscall.Handle) (uintptr, error) {
	size := uintptr(uint16(cols)) | uintptr(uint16(rows))<<16
	var hPC uintptr
	hr, _, callErr := winCreatePseudoConsole.Call(
		size,
		uintptr(input),
		uintptr(output),
		0,
		uintptr(unsafe.Pointer(&hPC)),
	)
	if hr != 0 {
		if callErr != nil && callErr != syscall.Errno(0) {
			return 0, fmt.Errorf("ConPTY: CreatePseudoConsole: %v (hr=0x%x)", callErr, hr)
		}
		return 0, fmt.Errorf("ConPTY: CreatePseudoConsole: hr=0x%x", hr)
	}
	if hPC == 0 {
		return 0, errors.New("ConPTY: CreatePseudoConsole returned an empty handle")
	}
	return hPC, nil
}

// startProcessWithPseudoConsole 用 STARTUPINFOEX + pseudoconsole 属性启动原生
// shell，返回可 Wait/Kill/ExitCode 的 process。
func startProcessWithPseudoConsole(hPC uintptr, argv []string, workdir string, env []string) (process, error) {
	commandLine, err := syscall.UTF16PtrFromString(winCommandLine(argv))
	if err != nil {
		return nil, fmt.Errorf("ConPTY: command line: %w", err)
	}
	var cwd *uint16
	if strings.TrimSpace(workdir) != "" {
		if cwd, err = syscall.UTF16PtrFromString(workdir); err != nil {
			return nil, fmt.Errorf("ConPTY: working directory: %w", err)
		}
	}
	var envBlock *uint16
	if len(env) > 0 {
		envBlock = winEnvironmentBlock(env)
	}

	var size uintptr
	_, _, _ = winInitializeProcThreadAttr.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return nil, errors.New("ConPTY: InitializeProcThreadAttributeList returned no size")
	}
	list := make([]byte, size)
	listPtr := uintptr(unsafe.Pointer(&list[0]))
	if ok, _, callErr := winInitializeProcThreadAttr.Call(listPtr, 1, 0, uintptr(unsafe.Pointer(&size))); ok == 0 {
		return nil, fmt.Errorf("ConPTY: InitializeProcThreadAttributeList: %v", callErr)
	}
	defer winDeleteProcThreadAttr.Call(listPtr)
	// The attribute value is the HPCON itself (not a pointer to it).
	if ok, _, callErr := winUpdateProcThreadAttribute.Call(
		listPtr, 0, procThreadAttributePseudoConsole,
		hPC, unsafe.Sizeof(hPC), 0, 0,
	); ok == 0 {
		return nil, fmt.Errorf("ConPTY: UpdateProcThreadAttribute: %v", callErr)
	}

	startup := winStartupInfoEx{attributeList: listPtr}
	startup.startupInfo.Cb = uint32(unsafe.Sizeof(startup))
	// Attach the child to the pseudoconsole rather than inheriting this
	// process's redirected standard handles.
	startup.startupInfo.Flags = syscall.STARTF_USESTDHANDLES
	startup.startupInfo.StdInput = syscall.InvalidHandle
	startup.startupInfo.StdOutput = syscall.InvalidHandle
	startup.startupInfo.StdErr = syscall.InvalidHandle

	var info syscall.ProcessInformation
	flags := uint32(extendedStartupInfoPresent | createUnicodeEnvironment)
	if err := syscall.CreateProcess(nil, commandLine, nil, nil, false, flags,
		envBlock, cwd, &startup.startupInfo, &info); err != nil {
		return nil, fmt.Errorf("ConPTY: CreateProcess: %w", err)
	}
	_ = syscall.CloseHandle(info.Thread)
	return &conPTYProcess{handle: info.Process, pid: int(info.ProcessId)}, nil
}

// conPTYProcess 是 ConPTY 子进程的 process 实现。
type conPTYProcess struct {
	handle syscall.Handle
	pid    int
	exit   int
	waited bool
}

func (p *conPTYProcess) Wait() error {
	if p == nil || p.handle == 0 || p.waited {
		return errors.New("ConPTY: process is not running")
	}
	if _, err := syscall.WaitForSingleObject(p.handle, 0xFFFFFFFF); err != nil {
		return err
	}
	var code uint32
	err := syscall.GetExitCodeProcess(p.handle, &code)
	_ = syscall.CloseHandle(p.handle)
	p.handle = 0
	p.waited = true
	p.exit = int(code)
	return err
}

func (p *conPTYProcess) Kill() error {
	if p == nil || p.handle == 0 {
		return nil
	}
	return syscall.TerminateProcess(p.handle, 1)
}

func (p *conPTYProcess) Pid() int {
	if p == nil {
		return 0
	}
	return p.pid
}

func (p *conPTYProcess) ExitCode() int {
	if p == nil || !p.waited {
		return -1
	}
	return p.exit
}

// winCommandLine 把 argv 拼成 CreateProcessW 需要的命令行（Windows 引号规则）。
func winCommandLine(argv []string) string {
	parts := make([]string, len(argv))
	for index, arg := range argv {
		parts[index] = winQuoteArg(arg)
	}
	return strings.Join(parts, " ")
}

func winQuoteArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t\n\v\"") {
		return arg
	}
	var builder strings.Builder
	builder.WriteByte('"')
	backslashes := 0
	for _, char := range arg {
		switch char {
		case '\\':
			backslashes++
			builder.WriteRune(char)
		case '"':
			builder.WriteString(strings.Repeat("\\", backslashes+1))
			builder.WriteByte('"')
			backslashes = 0
		default:
			if backslashes > 0 {
				builder.WriteString(strings.Repeat("\\", backslashes))
				backslashes = 0
			}
			builder.WriteRune(char)
		}
	}
	if backslashes > 0 {
		builder.WriteString(strings.Repeat("\\", backslashes))
	}
	builder.WriteByte('"')
	return builder.String()
}

// winEnvironmentBlock 把环境变量渲染成 CreateProcess 的 UTF-16 双 NUL 块。
func winEnvironmentBlock(env []string) *uint16 {
	var block []uint16
	for _, entry := range env {
		block = append(block, syscall.StringToUTF16(entry)...)
	}
	block = append(block, 0)
	return &block[0]
}

// fallbackShellPath 在 PATH 缺失时用 Win32 System32 定位 PowerShell/cmd。
func fallbackShellPath(name string) (string, error) {
	base := strings.ToLower(strings.TrimSpace(name))
	if base == "" {
		return "", errors.New("terminal: empty shell name")
	}
	buffer := make([]uint16, syscall.MAX_PATH)
	length, _, _ := winGetSystemDirectoryW.Call(uintptr(len(buffer)), uintptr(unsafe.Pointer(&buffer[0])))
	if length == 0 || length >= uintptr(len(buffer)) {
		return "", errors.New("terminal: GetSystemDirectoryW failed")
	}
	system32 := syscall.UTF16ToString(buffer[:length])
	switch base {
	case "powershell.exe", "powershell":
		return system32 + `\WindowsPowerShell\v1.0\powershell.exe`, nil
	case "cmd.exe", "cmd":
		return system32 + `\cmd.exe`, nil
	case "pwsh.exe", "pwsh":
		return "", errors.New("terminal: pwsh is not in PATH")
	}
	return "", fmt.Errorf("terminal: shell %q is not in PATH and has no system fallback", name)
}
