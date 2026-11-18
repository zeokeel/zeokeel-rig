//go:build windows

package osrunner

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// 本文件是 Windows ACL helper 的执行入口：解析 argv、创建受限令牌、给工作区与
// 私有临时目录授权（跨进程 mutex + 状态文件协调，见 windows_acl_state.go /
// windows_acl_backend.go）、尽力打 Low 完整性标签、在受限令牌 + kill-on-close
// Job 里 spawn 目标命令（见 windows_job_object.go），镜像其退出码。
//
// # 退出顺序（后代的存活期）
//
// 1. 顶层命令退出；
// 2. **收掉整棵进程树**：终止 Job 里剩余的后代并等它们真正消失
//    （windows_job_object.go 的 terminateTree）；
// 3. 才执行 cleanup：最后一名 helper 退出时恢复 per-run 的 Everyone 拒绝与 Low
//    标签（capability ACE 与 protect deny ACE 是缓存资产，保留），并删除本次的
//    私有临时目录。
//
// 顺序不能反：`cmd /c start /b …`、`pwsh -Command "Start-Process …"` 这类的后代
// 可能活过顶层命令，如果先恢复 per-run 形状，它们就会在"脚下"失去授权（Low 标签
// 被撤 → NO_WRITE_UP 拒绝；私有 temp 已被删）。helper 自己也可能被宿主强杀
// （ctx 超时/取消）：那时第 2 步不会执行，但 Job 句柄的 KILL_ON_JOB_CLOSE 保证内核
// 在 helper 进程死亡时终止整棵树——这是唯一能覆盖"helper 被强杀"的机制。
//
// 失败契约：helper 侧任何失败都向 stderr 打印
// "windows-acl-run: <detail>" 并以 127 退出；子进程绝不会以未受限方式 spawn。
// Job 创建/分配失败时**不 spawn**（fail closed）：没有 Job 就没有"后代随 helper
// 一起死"的保证，静默降级比明确跑不起来更糟（见 winSpawnPrecondition）。

// helper 自身失败的固定契约。它们只被 Windows 构建使用，因此放在本文件而不是
// windows_plan.go（后者在 Linux 上也会编译，未使用常量会被 staticcheck U1000
// 判为 dead code）。
const (
	// winHelperFailurePrefix 是 helper 自身失败时 stderr 的前缀。
	winHelperFailurePrefix = "windows-acl-run: "
	// winHelperFailureCode 是 helper 自身失败时进程退出的退出码。
	winHelperFailureCode = 127
)

// runWindowsACLHelper 处理 HelperArgWindowsACL 隐藏子命令。
func runWindowsACLHelper(args []string) (handled bool, code int) {
	if len(args) == 0 || args[0] != HelperArgWindowsACL {
		return false, 0
	}
	if !winLayoutSupported {
		winHelperFail(errors.New("helper requires 64-bit Windows (Win32 ACL struct layout mismatch)"))
		return true, winHelperFailureCode
	}
	plan, err := parseWinACLArgs(args[1:])
	if err != nil {
		winHelperFail(err)
		return true, winHelperFailureCode
	}
	if plan.Probe {
		return true, runWindowsACLProbe()
	}
	return true, runWindowsACLPlan(plan)
}

// winHelperFail 打印 helper 失败签名。
func winHelperFail(err error) {
	fmt.Fprintf(os.Stderr, "%s%v\n", winHelperFailurePrefix, err)
}

// runWindowsACLProbe 执行功能探测：跑一遍完整管线，只在探测用的临时目录上操作。
func runWindowsACLProbe() int {
	if err := probeWindowsACL(); err != nil {
		winHelperFail(fmt.Errorf("probe: %w", err))
		return winHelperFailureCode
	}
	return 0
}

// probeWindowsACL 用探针计划验证受限令牌、ACL 授权与受限 spawn 在本机可用。
func probeWindowsACL() error {
	workspace, err := winNewTempDir(nil, "rig-sbx-probe-ws-*")
	if err != nil {
		return fmt.Errorf("create probe workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	command, arguments := winProbeCommand()
	plan := winACLPlan{
		Workspaces: []string{workspace},
		Mode:       winModeWorkspaceWrite,
		Argv:       append([]string{command}, arguments...),
	}
	code, err := runWindowsACLPlanConfined(plan)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("probe child exited with code %d", code)
	}
	return nil
}

// winNewTempDir 在第一个可写、且不在任何工作区内的临时基址下创建私有目录。
// 候选顺序见 winTempBaseCandidates；SystemRoot 回退已被剔除，所以 TEMP/TMP
// 缺失时不会退化到 C:\WINDOWS（普通用户不可写）。
func winNewTempDir(workspaces []string, pattern string) (string, error) {
	var attempts []string
	extras := winKnownTempCandidates()
	if systemTemp := winGetTempPath(); systemTemp != "" {
		extras = append(extras, systemTemp)
	}
	windowsDir := winWindowsDirectory()
	for _, base := range winTempBaseCandidates(os.Getenv, os.TempDir(), extras...) {
		// GetTempPath falls back to the Windows directory when TEMP/TMP are
		// absent; never try to create the private scratch there.
		if windowsDir != "" && winPathWithinFold(windowsDir, base) {
			attempts = append(attempts, base+" (Windows directory)")
			continue
		}
		if !windowsTempRootUsable(workspaces, base) {
			attempts = append(attempts, base+" (inside a workspace root)")
			continue
		}
		if err := os.MkdirAll(base, 0o700); err != nil {
			attempts = append(attempts, fmt.Sprintf("%s (%v)", base, err))
			continue
		}
		dir, err := os.MkdirTemp(base, pattern)
		if err == nil {
			return dir, nil
		}
		attempts = append(attempts, fmt.Sprintf("%s (%v)", base, err))
	}
	return "", fmt.Errorf("no writable temp directory outside the workspace (set TEMP/TMP or RIG_SANDBOX_TEMP; tried %s)",
		strings.Join(attempts, "; "))
}

// winProbeCommand 返回一个总是立即成功退出的系统命令。
func winProbeCommand() (string, []string) {
	command := strings.TrimSpace(os.Getenv("COMSPEC"))
	if command == "" {
		command = "cmd.exe"
	}
	return command, []string{"/c", "exit", "0"}
}

// runWindowsACLPlan 执行一次真实受限运行，返回 helper 退出码。
func runWindowsACLPlan(plan winACLPlan) int {
	code, err := runWindowsACLPlanConfined(plan)
	if err != nil {
		winHelperFail(err)
		return winHelperFailureCode
	}
	return code
}

// runWindowsACLPlanConfined 是 helper 的主流程；任何错误都意味着“不要 spawn”。
// 它运行一次受 ACL 约束的计划并返回子进程退出码。
//
// 它是 runWindowsACLPlanConfinedOutcome 的薄包装：外部只关心退出码，测试需要看
// 进程树结果（Terminated/Remaining）来断言"收树发生在恢复 ACL 之前"。
func runWindowsACLPlanConfined(plan winACLPlan) (int, error) {
	_, code, err := runWindowsACLPlanConfinedOutcome(plan)
	return code, err
}

// runWindowsACLPlanConfinedOutcome 与 runWindowsACLPlanConfined 相同，但把本次
// 运行的进程树结果也返回给调用方（顺序断言的直接证据，见 windows_runtime_test.go）。
func runWindowsACLPlanConfinedOutcome(plan winACLPlan) (winProcessOutcome, int, error) {
	shape, err := windowsGrantShape(plan.Mode)
	if err != nil {
		return winProcessOutcome{}, 0, err
	}
	workspaces, err := winValidateWorkspaces(plan.Workspaces)
	if err != nil {
		return winProcessOutcome{}, 0, err
	}
	// 保护路径（--protect）：规范化 + 校验（必须存在、必须是目录、必须落在已授权
	// 工作区之内，否则 fail closed）。落在授权面之外的路径本来就不能写，跳过并在
	// stderr 记一行诊断——但绝不如同"已保护"那样静默。
	//
	// read-only 模式整盘只读、没有可写面，因此没有可保护的东西：protect 值仍然
	// 走同一套校验（非法值照样 fail closed），只是不产生任何 session。
	protected, err := winResolveProtectedPaths(plan.Protected, workspaces)
	if err != nil {
		return winProcessOutcome{}, 0, err
	}
	for _, note := range protected.Notes {
		fmt.Fprintln(os.Stderr, note)
	}

	// 进程树治理的前置条件：kill-on-close 的 Job 必须在 spawn 之前就绪。建不出来
	// 一律 fail closed（不 spawn）——见 winSpawnPrecondition 的理由。defer close
	// 同时是"helper 崩溃/被强杀"路径的兜底：句柄随进程死亡被关闭 → 内核终止树。
	job, err := winNewSandboxJob()
	if err != nil {
		return winProcessOutcome{}, 0, err
	}
	defer job.close()

	// cleanup 是唯一的授权释放点：无论从哪条失败路径返回，都先释放本 helper 的
	// grant（最后一个 grant 会恢复原始 descriptor）、删掉本次的私有临时目录，
	// 再释放令牌句柄（延迟调用）。
	var tempDir string
	var sessions []*winACLSession
	cleanup := func() error {
		var firstErr error
		for index := len(sessions) - 1; index >= 0; index-- {
			if err := sessions[index].Cleanup(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		sessions = nil
		if tempDir != "" {
			if err := os.RemoveAll(tempDir); err != nil && firstErr == nil {
				firstErr = err
			}
			tempDir = ""
		}
		return firstErr
	}
	defer func() { _ = cleanup() }()

	var environment []string
	if shape.TempWrite {
		tempDir, err = winNewTempDir(workspaces, "rig-sbx-tmp-*")
		if err != nil {
			return winProcessOutcome{}, 0, fmt.Errorf("create private temp directory: %w", err)
		}
		environment = winPrivateTempEnvironment(tempDir)
	}

	logonSID, err := winCurrentLogonSID()
	if err != nil {
		return winProcessOutcome{}, 0, err
	}
	// 工作区能力 SID 由规范化路径派生：同一目录跨进程/跨会话是同一个 SID，
	// 因此目录上已有的那条 ACE 可以跨运行复用（缓存语义，见 windows_acl_state.go）。
	// 私有临时目录每次运行唯一，能力 SID 仍然随机生成——它只活一次运行。
	workspaceCapabilities := make(map[string]string, len(workspaces))
	workspaceSIDs := make([]string, 0, len(workspaces))
	var tempCapability string
	if shape.WorkspaceWrite {
		for _, workspace := range workspaces {
			sid, sidErr := winWorkspaceCapabilitySID(workspace)
			if sidErr != nil {
				return winProcessOutcome{}, 0, sidErr
			}
			workspaceCapabilities[workspace] = sid
			workspaceSIDs = append(workspaceSIDs, sid)
		}
		if shape.TempWrite {
			tempCapability, err = newCapabilitySID()
			if err != nil {
				return winProcessOutcome{}, 0, err
			}
		}
	}
	restricting, err := windowsRestrictingSIDStrings(plan.Mode, logonSID, winEveryoneSIDString, workspaceSIDs, tempCapability)
	if err != nil {
		return winProcessOutcome{}, 0, err
	}
	token, err := winNewRestrictedToken(restricting)
	if err != nil {
		return winProcessOutcome{}, 0, err
	}
	defer token.close()

	everyone, err := winParseSID(winEveryoneSIDString)
	if err != nil {
		return winProcessOutcome{}, 0, err
	}
	lowLabel, err := winParseSID(winLowIntegritySIDString)
	if err != nil {
		return winProcessOutcome{}, 0, err
	}

	if shape.WorkspaceWrite && winACLModeWritesACL(plan.Mode) {
		// labelAppliedAll tracks whether EVERY granted directory carries the Low
		// label; only then is the token itself restricted to Low integrity.
		labelAppliedAll := true
		grantDirectory := func(directory, capability string, options winACLSessionOptions) error {
			session := newWinACLSession(newWinWindowsACLBackend(directory), directory, capability, options)
			// Register before Grant so a failure after the ACE insertion is still
			// cleaned up by the deferred cleanup.
			sessions = append(sessions, session)
			applied, grantErr := session.Grant()
			if !applied {
				labelAppliedAll = false
			}
			return grantErr
		}
		for _, workspace := range workspaces {
			assigned := protected.For(workspace)
			if len(assigned) == 0 && len(protected.Enforced) > 0 {
				// 绝不静默丢保护：解析出的保护路径没有任何一条归到本工作区，
				// 通常意味着两边的路径写法不一致（8.3 短名/大小写/长路径前缀）。
				// 真机 CI 上正是这样让 `.git` 实际可写的——留一行可搜索的诊断。
				fmt.Fprintf(os.Stderr,
					"windows-acl: %d protected path(s) resolved but none belongs to workspace %q; protection is NOT enforced\n",
					len(protected.Enforced), workspace)
			}
			options := winACLSessionOptions{
				Protected:       assigned,
				WorkspaceDeny:   true,
				LowLabel:        shape.LowIntegrityToken,
				CacheCapability: true,
			}
			if err := grantDirectory(workspace, workspaceCapabilities[workspace], options); err != nil {
				return winProcessOutcome{}, 0, err
			}
		}
		if shape.TempWrite {
			options := winACLSessionOptions{
				WorkspaceDeny:   true,
				LowLabel:        shape.LowIntegrityToken,
				CacheCapability: false,
			}
			if err := grantDirectory(tempDir, tempCapability, options); err != nil {
				return winProcessOutcome{}, 0, err
			}
		}
		// Low 完整性是尽力而为的纵深防御：标签写不进去（缺少 WRITE_OWNER /
		// SeSecurityPrivilege）时退化为只依赖受限令牌，绝不因此放宽写授权。
		lowIntegrity := shape.LowIntegrityToken && labelAppliedAll
		// 受限令牌新建的对象（匿名管道等）需要默认 DACL 里有一个 restricting SID。
		// 每个能力 SID 都授一遍：子进程可能同时在多个工作区 + 私有临时目录里创建
		// 无父对象的句柄，任何一个 SID 缺失都会让那条路径的第二次访问检查失败。
		defaultDACLSIDs := append([]string(nil), workspaceSIDs...)
		if tempCapability != "" {
			defaultDACLSIDs = append(defaultDACLSIDs, tempCapability)
		}
		for _, capability := range defaultDACLSIDs {
			capabilitySID, sidErr := winParseSID(capability)
			if sidErr != nil {
				return winProcessOutcome{}, 0, sidErr
			}
			if err := winSetTokenDefaultDACLGrant(token.handle, capabilitySID); err != nil {
				return winProcessOutcome{}, 0, err
			}
		}
		if lowIntegrity {
			if err := winRestrictTokenIntegrity(token.handle, lowLabel); err != nil {
				return winProcessOutcome{}, 0, err
			}
		}
	} else {
		if err := winSetTokenDefaultDACLGrant(token.handle, everyone); err != nil {
			return winProcessOutcome{}, 0, err
		}
		// read-only 的形状声明了 LowIntegrityToken：没有写授权、也没有可打标签的
		// 目录，所以只把令牌降到 Low——no-write-up 会拒绝写任何 Medium 对象（读仍
		// 允许），这正是"只读"。标签写不进去时退化为只依赖 restricting 交集并记
		// 一行诊断，绝不因此放宽写授权。
		if shape.LowIntegrityToken {
			if err := winRestrictTokenIntegrity(token.handle, lowLabel); err != nil {
				fmt.Fprintf(os.Stderr, "windows-acl: read-only token could not be lowered to Low integrity (%v); falling back to the restricted token only\n", err)
			}
		}
	}
	outcome, runErr := winRunConfinedCommand(plan.Argv, token.handle, environment, job, 0)
	// 顺序关键：winRunConfinedCommand 在返回前已经收掉整棵进程树并等它们消失，
	// 所以下面的 cleanup（恢复 Everyone 拒绝 / Low 标签、删私有 temp）不会落在
	// 仍存活的后代头上。
	if notice := winTreeTerminationNotice(outcome); notice != "" {
		fmt.Fprintln(os.Stderr, notice)
	}
	cleanupErr := cleanup()
	token.close()
	if runErr != nil {
		return outcome, 0, runErr
	}
	if cleanupErr != nil {
		return outcome, 0, fmt.Errorf("cleanup: %w", cleanupErr)
	}
	return outcome, winOutcomeExitCode(outcome), nil
}

// winPrivateTempEnvironment 返回把 TMP/TEMP 指向私有临时目录的环境块。
func winPrivateTempEnvironment(tempDir string) []string {
	environment := os.Environ()
	out := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			out = append(out, entry)
			continue
		}
		if strings.EqualFold(name, "TMP") || strings.EqualFold(name, "TEMP") || strings.EqualFold(name, "TMPDIR") {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "TMP="+tempDir, "TEMP="+tempDir)
	return out
}
