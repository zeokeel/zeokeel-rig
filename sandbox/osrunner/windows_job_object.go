//go:build windows

package osrunner

// 本文件是进程树治理的 Win32 实现：kill-on-close 的 Job 对象、CREATE_SUSPENDED 的
// 受限 spawn（先分配进 Job 再 ResumeThread，消除"创建后到分配前"的逃逸窗口）、
// 等待与收树。纯逻辑（结果模型、退出码、诊断文案、环境块、fail-closed 策略）在
// windows_job.go，因此本文件只做"调用 + 错误包装"。
//
// 顺序约定（与 windows_helper.go 配合）：顶层命令退出 → 收掉 Job 里剩余的后代并
// 等它们真正消失 → 才允许 cleanup 恢复 per-run 形状 / 删除私有 temp。helper 被
// 宿主强杀时无法执行这段顺序，但 Job 句柄带 KILL_ON_JOB_CLOSE：内核在关闭句柄时
// 终止整棵树，因此"后代活过恢复"这件事仍然不会发生。

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	// Job 对象信息类（winnt.h）。
	winJobObjectBasicAccountingInformation = 1
	winJobObjectExtendedLimitInformation   = 9
	// winCreateSuspended 是 CREATE_SUSPENDED（标准库没有导出这位）。
	//
	// winJobObjectLimitKillOnJobClose 在 windows_layout.go：纯逻辑测试也断言它。
	winCreateSuspended = 0x00000004
	// winTreeTerminationPollInterval 是等待整棵树消失期间查询活跃进程数的间隔。
	winTreeTerminationPollInterval = 20 * time.Millisecond
)

var (
	winProcCreateJobObjectW        = winKernel32.NewProc("CreateJobObjectW")
	winProcSetInformationJobObject = winKernel32.NewProc("SetInformationJobObject")
	winProcAssignProcessToJob      = winKernel32.NewProc("AssignProcessToJobObject")
	winProcQueryInformationJob     = winKernel32.NewProc("QueryInformationJobObject")
	winProcTerminateJobObject      = winKernel32.NewProc("TerminateJobObject")
	winProcResumeThread            = winKernel32.NewProc("ResumeThread")
	winProcInitProcThreadAttrs     = winKernel32.NewProc("InitializeProcThreadAttributeList")
	winProcUpdateProcThreadAttr    = winKernel32.NewProc("UpdateProcThreadAttribute")
	winProcDeleteProcThreadAttrs   = winKernel32.NewProc("DeleteProcThreadAttributeList")
	winProcCreateProcessAsUserW    = winAdvapi32.NewProc("CreateProcessAsUserW")
)

// winJobFailure 把一次 Job 操作失败包装成 helper 失败错误（带步骤描述）。
func winJobFailure(step string, err error) error {
	if err == nil {
		err = errors.New("unknown error")
	}
	return fmt.Errorf("windows-acl-run: %s: %w", step, err)
}

// winSandboxJob 是一次受限运行的 kill-on-close Job。
type winSandboxJob struct {
	handle syscall.Handle
}

// winNewSandboxJob 创建 Job 并打开 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE。
//
// 任何一步失败都返回错误（调用方 fail closed：不 spawn）。Job 未命名是刻意的：
// 只有本 helper 持有一份句柄，句柄关闭即代表"这次运行结束"，语义没有歧义。
func winNewSandboxJob() (*winSandboxJob, error) {
	if !winJobLayoutSupported {
		return nil, winSpawnPrecondition(false, false)
	}
	handle, _, callErr := winProcCreateJobObjectW.Call(0, 0)
	if handle == 0 {
		return nil, winJobFailure("cannot create the sandbox job object", win32Error("CreateJobObjectW", callErr))
	}
	job := &winSandboxJob{handle: syscall.Handle(handle)}
	if err := job.enableKillOnClose(); err != nil {
		job.close()
		return nil, err
	}
	return job, nil
}

// enableKillOnClose 打开 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE。
//
// 这是"helper 被强杀也能收树"的唯一机制：句柄的最后一个引用随进程死亡被内核关闭，
// 内核随即终止 Job 里的所有进程。
func (job *winSandboxJob) enableKillOnClose() error {
	var limits winJobExtendedLimitInformation
	limits.BasicLimitInformation.LimitFlags = winJobObjectLimitKillOnJobClose
	result, _, callErr := winProcSetInformationJobObject.Call(
		uintptr(job.handle),
		uintptr(winJobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&limits)),
		uintptr(unsafe.Sizeof(limits)),
	)
	runtime.KeepAlive(limits)
	if result == 0 {
		return winJobFailure("cannot enable kill-on-close on the sandbox job object", win32Error("SetInformationJobObject", callErr))
	}
	return nil
}

// assign 把子进程分配进 Job。必须在 ResumeThread 之前调用。
func (job *winSandboxJob) assign(process syscall.Handle) error {
	if job == nil || job.handle == 0 {
		return winSpawnPrecondition(false, winJobLayoutSupported && winLayoutSupported)
	}
	result, _, callErr := winProcAssignProcessToJob.Call(uintptr(job.handle), uintptr(process))
	if result == 0 {
		return winJobFailure("cannot assign the child process to the sandbox job object", win32Error("AssignProcessToJobObject", callErr))
	}
	return nil
}

// activeProcesses 返回 Job 里当前活跃的进程数。
func (job *winSandboxJob) activeProcesses() (int, error) {
	if job == nil || job.handle == 0 {
		return 0, winSpawnPrecondition(false, winJobLayoutSupported && winLayoutSupported)
	}
	var accounting winJobBasicAccountingInformation
	result, _, callErr := winProcQueryInformationJob.Call(
		uintptr(job.handle),
		uintptr(winJobObjectBasicAccountingInformation),
		uintptr(unsafe.Pointer(&accounting)),
		uintptr(unsafe.Sizeof(accounting)),
		0,
	)
	runtime.KeepAlive(accounting)
	if result == 0 {
		return 0, winJobFailure("cannot query the sandbox job object", win32Error("QueryInformationJobObject", callErr))
	}
	return int(accounting.ActiveProcesses), nil
}

// terminate 请求内核终止 Job 里的所有进程。
func (job *winSandboxJob) terminate(exitCode uint32) error {
	if job == nil || job.handle == 0 {
		return winSpawnPrecondition(false, winJobLayoutSupported && winLayoutSupported)
	}
	result, _, callErr := winProcTerminateJobObject.Call(uintptr(job.handle), uintptr(exitCode))
	if result == 0 {
		return winJobFailure("cannot terminate the sandbox job object", win32Error("TerminateJobObject", callErr))
	}
	return nil
}

// waitForEmptyTree 等待 Job 里不再有活跃进程（有界），返回等待上限之后仍存活的
// 进程数（正常为 0）。
//
// 等待是必须的：恢复 per-run 形状（撤 Everyone 拒绝 / Low 标签、删私有 temp）不能
// 落在仍存活的后代头上，否则它们会在自己的脚下失去授权，表现为随机失败与半截文件。
func (job *winSandboxJob) waitForEmptyTree() (int, error) {
	deadline := time.Now().Add(winTreeTerminationWait)
	for {
		active, err := job.activeProcesses()
		if err != nil {
			return 0, err
		}
		if !winShouldTerminateTree(active) {
			return 0, nil
		}
		if !time.Now().Before(deadline) {
			return active, nil
		}
		time.Sleep(winTreeTerminationPollInterval)
	}
}

// terminateTree 终止 Job 里剩余的所有进程，并**等待它们真正消失**（有界）。
//
// 返回的结果用于诊断：Terminated 是顶层命令之外被收掉的进程数（调用前顶层命令必须
// 已经退出，否则它也会被计进去），Remaining 是等待上限之后仍存活的进程数。
func (job *winSandboxJob) terminateTree(exitCode uint32) (winTreeKillResult, error) {
	result := winTreeKillResult{}
	active, err := job.activeProcesses()
	if err != nil {
		return result, err
	}
	if !winShouldTerminateTree(active) {
		return result, nil
	}
	result.Terminated = active
	if err := job.terminate(exitCode); err != nil {
		return result, err
	}
	result.Remaining, err = job.waitForEmptyTree()
	return result, err
}

// close 关闭 Job 句柄。因为开着 KILL_ON_JOB_CLOSE，这一步同时是"收掉一切残留"的
// 最后兜底（错误路径、helper 崩溃、宿主强杀都走内核路径）。幂等。
func (job *winSandboxJob) close() {
	if job == nil || job.handle == 0 {
		return
	}
	_ = winCloseHandle(job.handle)
	job.handle = 0
}

// winCommandLine 把 argv 拼成 CreateProcess 需要的命令行（与 os/exec 的拼装规则
// 一致：逐个 syscall.EscapeArg 后用空格连接）。
func winCommandLine(argv []string) string {
	escaped := make([]string, len(argv))
	for index, arg := range argv {
		escaped[index] = syscall.EscapeArg(arg)
	}
	return strings.Join(escaped, " ")
}

// winDuplicateStandardHandles 把本进程的 stdin/stdout/stderr 复制成**可继承**的
// 句柄（CreateProcess 的 bInheritHandles 只对可继承句柄生效）。返回的句柄由调用方
// 在 CreateProcess 之后立即关闭；无效的流对应 0（子进程拿不到该标准句柄，与
// os/exec 的行为一致）。
func winDuplicateStandardHandles() ([3]syscall.Handle, func(), error) {
	var handles [3]syscall.Handle
	closeAll := func() {
		for _, handle := range handles {
			if handle != 0 {
				_ = winCloseHandle(handle)
			}
		}
	}
	current, err := syscall.GetCurrentProcess()
	if err != nil {
		return handles, closeAll, fmt.Errorf("GetCurrentProcess: %w", err)
	}
	streams := [3]*os.File{os.Stdin, os.Stdout, os.Stderr}
	for index, stream := range streams {
		if stream == nil {
			continue
		}
		source := syscall.Handle(stream.Fd())
		if source == 0 || source == syscall.InvalidHandle {
			continue
		}
		duplicate := syscall.InvalidHandle
		if err := syscall.DuplicateHandle(current, source, current, &duplicate, 0, true, syscall.DUPLICATE_SAME_ACCESS); err != nil {
			closeAll()
			return handles, closeAll, fmt.Errorf("DuplicateHandle(stream %d): %w", index, err)
		}
		handles[index] = duplicate
	}
	return handles, closeAll, nil
}

// winProcThreadAttributeList 持有一份已初始化的进程/线程属性表（handle list）。
//
// 缓冲区必须至少 8 字节对齐：InitializeProcThreadAttributeList 校验指针，若不对齐
// 会直接返回 ERROR_INVALID_PARAMETER（不会写坏内存），调用方据此 fail closed。
type winProcThreadAttributeList struct {
	buffer []byte
	head   unsafe.Pointer
}

// winNewProcThreadAttributeList 初始化一个只含 attributeCount 项的属性表。
func winNewProcThreadAttributeList(attributeCount uint32) (*winProcThreadAttributeList, error) {
	var size uintptr
	// 第一次调用只用于取所需字节数：它以 ERROR_INSUFFICIENT_BUFFER 失败。
	if result, _, _ := winProcInitProcThreadAttrs.Call(0, uintptr(attributeCount), 0, uintptr(unsafe.Pointer(&size))); result != 0 {
		// 不该发生，但不要据此继续。
		return nil, winJobFailure("cannot size the process attribute list", errors.New("InitializeProcThreadAttributeList unexpectedly succeeded"))
	}
	if size == 0 {
		return nil, winJobFailure("cannot size the process attribute list", errors.New("InitializeProcThreadAttributeList reported 0 bytes"))
	}
	buffer := make([]byte, size)
	head := unsafe.Pointer(&buffer[0])
	if uintptr(head)%uintptr(unsafe.Alignof(uintptr(0))) != 0 {
		return nil, winJobFailure("cannot initialize the process attribute list", errors.New("attribute list buffer is not pointer-aligned"))
	}
	if result, _, callErr := winProcInitProcThreadAttrs.Call(uintptr(head), uintptr(attributeCount), 0, uintptr(unsafe.Pointer(&size))); result == 0 {
		return nil, winJobFailure("cannot initialize the process attribute list", win32Error("InitializeProcThreadAttributeList", callErr))
	}
	return &winProcThreadAttributeList{buffer: buffer, head: head}, nil
}

// setHandleList 设置 PROC_THREAD_ATTRIBUTE_HANDLE_LIST：子进程只继承列出的句柄。
func (list *winProcThreadAttributeList) setHandleList(handles []syscall.Handle) error {
	if list == nil || list.head == nil || len(handles) == 0 {
		return winJobFailure("cannot restrict inherited handles", errors.New("empty process attribute list or handle list"))
	}
	size := uintptr(len(handles)) * unsafe.Sizeof(handles[0])
	result, _, callErr := winProcUpdateProcThreadAttr.Call(
		uintptr(list.head),
		0,
		uintptr(winProcThreadAttributeHandleList),
		uintptr(unsafe.Pointer(&handles[0])),
		size,
		0,
		0,
	)
	runtime.KeepAlive(handles)
	if result == 0 {
		return winJobFailure("cannot restrict inherited handles", win32Error("UpdateProcThreadAttribute", callErr))
	}
	return nil
}

// close 释放属性表（幂等）。
func (list *winProcThreadAttributeList) close() {
	if list == nil || list.head == nil {
		return
	}
	winProcDeleteProcThreadAttrs.Call(uintptr(list.head))
	list.head = nil
	list.buffer = nil
}

// winCreateSuspendedProcess 用受限令牌创建**挂起**的子进程。
//
// 挂起是必须的：只有这样才能在子进程执行第一条指令之前把它分配进 Job，从而彻底
// 消除"先 CreateProcess 再 AssignProcessToJobObject"的逃逸窗口（那段时间里子进程
// 可以 spawn 出不被 Job 管辖的后代）。
//
// 继承面刻意收窄到三个标准句柄（PROC_THREAD_ATTRIBUTE_HANDLE_LIST，与 os/exec
// 一致）：Go 在 Windows 上默认把文件句柄创建为可继承，只靠 handle list 挡住，
// 不用它就会把 helper 里所有可继承句柄交给受沙子进程。
func winCreateSuspendedProcess(argv []string, token syscall.Token, environment []string) (syscall.Handle, syscall.Handle, error) {
	if len(argv) == 0 {
		return 0, 0, errors.New("windows-acl: empty command argv")
	}
	if !winStartupInfoEXSupported {
		return 0, 0, errors.New("windows-acl-run: the STARTUPINFOEX struct layout does not match 64-bit Windows")
	}
	applicationName, err := syscall.UTF16PtrFromString(argv[0])
	if err != nil {
		return 0, 0, fmt.Errorf("CreateProcessAsUserW: encode %q: %w", argv[0], err)
	}
	commandLine, err := syscall.UTF16PtrFromString(winCommandLine(argv))
	if err != nil {
		return 0, 0, fmt.Errorf("CreateProcessAsUserW: encode argv: %w", err)
	}
	// nil 环境 = 继承 helper 的环境（与 os/exec 的 command.Env == nil 语义一致）；
	// 非 nil 才是"用这份环境块替换"（workspace-write 会把 TMP/TEMP 指到私有 temp）。
	environmentBlock, err := winEnvironmentBlockFor(environment)
	if err != nil {
		return 0, 0, err
	}
	var environmentPointer *uint16
	if environmentBlock != nil {
		environmentPointer = &environmentBlock[0]
	}
	handles, closeHandles, err := winDuplicateStandardHandles()
	if err != nil {
		return 0, 0, err
	}
	defer closeHandles()
	inherited := make([]syscall.Handle, 0, len(handles))
	for _, handle := range handles {
		if handle != 0 {
			inherited = append(inherited, handle)
		}
	}
	var attributeList *winProcThreadAttributeList
	if len(inherited) > 0 {
		attributeList, err = winNewProcThreadAttributeList(1)
		if err != nil {
			return 0, 0, err
		}
		defer attributeList.close()
		if err := attributeList.setHandleList(inherited); err != nil {
			return 0, 0, err
		}
	}
	startup := winStartupInfoEX{}
	startup.Cb = uint32(winStartupInfoEXSize)
	startup.Flags = syscall.STARTF_USESTDHANDLES | syscall.STARTF_USESHOWWINDOW
	// 隐藏子进程的控制台窗口：宿主是 GUI 子系统进程，不隐藏会让每个工具调用都
	// 弹一个黑框（与 procattr.HideWindow 在本包其它 spawn 点上的效果一致）。
	startup.ShowWindow = syscall.SW_HIDE
	startup.StdInput = uintptr(handles[0])
	startup.StdOutput = uintptr(handles[1])
	startup.StdErr = uintptr(handles[2])
	if attributeList != nil {
		startup.AttributeList = attributeList.head
	}
	inheritHandles := len(inherited) > 0
	var processInformation syscall.ProcessInformation
	flags := uint32(winCreateSuspended) | syscall.CREATE_UNICODE_ENVIRONMENT | uint32(winExtendedStartupInfoPresent)
	result, _, callErr := winProcCreateProcessAsUserW.Call(
		uintptr(token),
		uintptr(unsafe.Pointer(applicationName)),
		uintptr(unsafe.Pointer(commandLine)),
		0, // 进程安全属性
		0, // 线程安全属性
		boolToUintptr(inheritHandles),
		uintptr(flags),
		uintptr(unsafe.Pointer(environmentPointer)),
		0, // 工作目录：继承 helper 的当前目录（与 os/exec 未设 Dir 时一致）
		uintptr(unsafe.Pointer(&startup)),
		uintptr(unsafe.Pointer(&processInformation)),
	)
	runtime.KeepAlive(applicationName)
	runtime.KeepAlive(commandLine)
	runtime.KeepAlive(environmentBlock)
	runtime.KeepAlive(startup)
	// handle list 的数组必须在 CreateProcess 返回前一直有效（属性表里存的是指向
	// 它的数据），与 os/exec 的 KeepAlive(fd) 同理。
	runtime.KeepAlive(inherited)
	if result == 0 {
		return 0, 0, fmt.Errorf("CreateProcessAsUserW(%q): %w", argv[0], win32Error("CreateProcessAsUserW", callErr))
	}
	return processInformation.Process, processInformation.Thread, nil
}

// boolToUintptr 把 bool 变成 Win32 需要的 1/0 参数。
func boolToUintptr(value bool) uintptr {
	if value {
		return 1
	}
	return 0
}

// winResumeThread 恢复（挂起的）子进程主线程。
func winResumeThread(thread syscall.Handle) error {
	result, _, callErr := winProcResumeThread.Call(uintptr(thread))
	if result == 0xFFFFFFFF {
		return win32Error("ResumeThread", callErr)
	}
	return nil
}

// winProcessExitCode 读取已退出进程的退出码。
func winProcessExitCode(process syscall.Handle) (int, error) {
	var code uint32
	if err := syscall.GetExitCodeProcess(process, &code); err != nil {
		return 0, fmt.Errorf("GetExitCodeProcess: %w", err)
	}
	return int(code), nil
}

// winWaitForProcessExit 等顶层进程退出。exited=false 表示看门狗时限到（进程仍在跑），
// 不是错误——由调用方决定收树。
func winWaitForProcessExit(process syscall.Handle, timeout time.Duration) (bool, error) {
	event, err := syscall.WaitForSingleObject(process, winJobWaitMilliseconds(timeout))
	switch event {
	case syscall.WAIT_OBJECT_0:
		return true, nil
	case syscall.WAIT_TIMEOUT:
		return false, nil
	default:
		return false, win32Error("WaitForSingleObject", err)
	}
}

// winRunConfinedCommand 在受限令牌 + Job 治理下运行 argv，透传 stdio 并返回完整
// 结果（顶层退出码、是否超时、进程树收树情况）。
//
// 失败即收树：任何错误路径（创建失败之外的等待/查询失败）都会在返回前把 Job 里
// 剩余进程收掉，绝不让它们落到"已经恢复 per-run 形状"的目录上。Job 句柄本身的
// KILL_ON_JOB_CLOSE 是最后兜底。
func winRunConfinedCommand(argv []string, token syscall.Token, environment []string, job *winSandboxJob, timeout time.Duration) (winProcessOutcome, error) {
	outcome := winProcessOutcome{}
	if err := winSpawnPrecondition(job != nil, winJobLayoutSupported && winLayoutSupported); err != nil {
		return outcome, err
	}
	if len(argv) == 0 {
		return outcome, errors.New("windows-acl: empty command argv")
	}
	process, thread, err := winCreateSuspendedProcess(argv, token, environment)
	if err != nil {
		return outcome, err
	}
	defer func() { _ = winCloseHandle(process) }()
	defer func() { _ = winCloseHandle(thread) }()
	if err := job.assign(process); err != nil {
		// 进程还挂在 CREATE_SUSPENDED 上、从未执行过一条指令：直接杀掉，绝不 Resume。
		_ = syscall.TerminateProcess(process, uint32(winHelperFailureCode))
		return outcome, err
	}
	if err := winResumeThread(thread); err != nil {
		_ = syscall.TerminateProcess(process, uint32(winHelperFailureCode))
		return outcome, err
	}
	exited, waitErr := winWaitForProcessExit(process, timeout)
	if waitErr != nil {
		if tree, killErr := job.terminateTree(uint32(winHelperFailureCode)); killErr != nil {
			return outcome, fmt.Errorf("%w (also failed to terminate the process tree: %v)", waitErr, killErr)
		} else {
			outcome.Tree = tree
		}
		return outcome, waitErr
	}
	if !exited {
		// 看门狗时限到：收掉整棵树（顶层进程也在 Job 里），返回可识别的 124。
		// 这里刻意**不**用 terminateTree：它会把顶层进程也算成"后代"。超时场景
		// 只需要说明"树被终止了"，真正要盯的是"有没有进程在恢复形状前还活着"。
		outcome.TimedOut = true
		outcome.Timeout = timeout
		outcome.ExitCode = winTreeTerminatedExitCode
		if err := job.terminate(uint32(winTreeTerminatedExitCode)); err != nil {
			return outcome, err
		}
		// 顶层进程属于同一个 Job：终止后它的句柄会信号。等它消失，再等整棵树空。
		if _, waitErr := syscall.WaitForSingleObject(process, winJobWaitMilliseconds(winTreeTerminationWait)); waitErr != nil {
			return outcome, win32Error("WaitForSingleObject(terminated)", waitErr)
		}
		remaining, drainErr := job.waitForEmptyTree()
		outcome.Tree.Remaining = remaining
		if drainErr != nil {
			return outcome, drainErr
		}
		return outcome, nil
	}
	code, codeErr := winProcessExitCode(process)
	if codeErr != nil {
		if tree, killErr := job.terminateTree(uint32(winHelperFailureCode)); killErr != nil {
			return outcome, fmt.Errorf("%w (also failed to terminate the process tree: %v)", codeErr, killErr)
		} else {
			outcome.Tree = tree
		}
		return outcome, codeErr
	}
	outcome.ExitCode = code
	// 顶层命令已退出：把 Job 里剩余的后代收掉并等它们消失，然后才允许 cleanup
	// 恢复 per-run 形状 / 删除私有 temp。
	tree, killErr := job.terminateTree(0)
	outcome.Tree = tree
	if killErr != nil {
		return outcome, killErr
	}
	return outcome, nil
}
