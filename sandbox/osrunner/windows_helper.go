//go:build windows

package osrunner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// 本文件是 Windows ACL helper 的执行入口：解析 argv、创建受限令牌、给工作区与
// 私有临时目录授权、尽力打 Low 完整性标签、用 syscall.SysProcAttr{Token: ...}
// 在受限令牌下 spawn 目标命令，镜像其退出码，退出时恢复目录安全描述符。
//
// 失败契约：helper 侧任何失败都向 stderr 打印
// "windows-acl-run: <detail>" 并以 127 退出；子进程绝不会以未受限方式 spawn。

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
	tempRoot := os.TempDir()
	workspace, err := os.MkdirTemp(tempRoot, "rig-sbx-probe-ws-*")
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
func runWindowsACLPlanConfined(plan winACLPlan) (int, error) {
	shape, err := windowsGrantShape(plan.Mode)
	if err != nil {
		return 0, err
	}
	workspaces, err := winValidateWorkspaces(plan.Workspaces)
	if err != nil {
		return 0, err
	}

	// cleanup 是唯一的安全描述符恢复点：无论从哪条失败路径返回，都先恢复
	// 已授权的目录、删掉本次的私有临时目录，再释放令牌句柄（延迟调用）。
	var tempDir string
	var grants []*winACLGrant
	cleanup := func() error {
		var firstErr error
		for index := len(grants) - 1; index >= 0; index-- {
			if err := grants[index].restore(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		grants = nil
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
		tempRoot := os.TempDir()
		if strings.TrimSpace(tempRoot) == "" {
			return 0, errors.New("windows-acl: no temp directory for the private scratch space")
		}
		if !windowsTempRootUsable(workspaces, tempRoot) {
			return 0, fmt.Errorf("windows-acl: temp root %q is inside a workspace root; refusing to merge write capabilities", tempRoot)
		}
		tempDir, err = os.MkdirTemp(tempRoot, "rig-sbx-tmp-*")
		if err != nil {
			return 0, fmt.Errorf("create private temp directory: %w", err)
		}
		environment = winPrivateTempEnvironment(tempDir)
	}

	logonSID, err := winCurrentLogonSID()
	if err != nil {
		return 0, err
	}
	var workspaceCapability, tempCapability string
	if shape.WorkspaceWrite {
		workspaceCapability, err = newCapabilitySID()
		if err != nil {
			return 0, err
		}
		if shape.TempWrite {
			tempCapability, err = newCapabilitySID()
			if err != nil {
				return 0, err
			}
		}
	}
	restricting, err := windowsRestrictingSIDStrings(plan.Mode, logonSID, winEveryoneSIDString, workspaceCapability, tempCapability)
	if err != nil {
		return 0, err
	}
	token, err := winNewRestrictedToken(restricting)
	if err != nil {
		return 0, err
	}
	defer token.close()

	everyone, err := winParseSID(winEveryoneSIDString)
	if err != nil {
		return 0, err
	}
	lowLabel, err := winParseSID(winLowIntegritySIDString)
	if err != nil {
		return 0, err
	}

	if shape.WorkspaceWrite {
		workspaceSID, err := winParseSID(workspaceCapability)
		if err != nil {
			return 0, err
		}
		for _, workspace := range workspaces {
			grant, err := winGrantWrite(workspace, workspaceSID, everyone)
			if err != nil {
				return 0, err
			}
			grants = append(grants, grant)
		}
		if shape.TempWrite {
			tempSID, err := winParseSID(tempCapability)
			if err != nil {
				return 0, err
			}
			grant, err := winGrantWrite(tempDir, tempSID, everyone)
			if err != nil {
				return 0, err
			}
			grants = append(grants, grant)
		}
		// 受限令牌新建的对象（匿名管道等）需要默认 DACL 里有一个 restricting SID。
		if err := winSetTokenDefaultDACLGrant(token.handle, workspaceSID); err != nil {
			return 0, err
		}
	} else {
		if err := winSetTokenDefaultDACLGrant(token.handle, everyone); err != nil {
			return 0, err
		}
	}

	// Low 完整性是尽力而为的纵深防御：标签写不进去（缺少 WRITE_OWNER /
	// SeSecurityPrivilege）时退化为只依赖受限令牌，绝不因此放宽写授权。
	lowIntegrity := shape.LowIntegrityToken
	if shape.WorkspaceWrite && lowIntegrity {
		for _, grant := range grants {
			if err := grant.applyLowLabel(lowLabel); err != nil {
				lowIntegrity = false
				break
			}
		}
	}
	if lowIntegrity {
		if err := winRestrictTokenIntegrity(token.handle, lowLabel); err != nil {
			return 0, err
		}
	}

	code, runErr := winRunConfinedCommand(plan.Argv, token.handle, environment)
	cleanupErr := cleanup()
	token.close()
	if runErr != nil {
		return 0, runErr
	}
	if cleanupErr != nil {
		return 0, fmt.Errorf("cleanup: %w", cleanupErr)
	}
	return code, nil
}

// winValidateWorkspaces 校验工作区根存在且是目录，并返回 Clean 后的路径。
func winValidateWorkspaces(paths []string) ([]string, error) {
	workspaces := make([]string, 0, len(paths))
	for _, path := range paths {
		cleaned := filepath.Clean(strings.TrimSpace(path))
		if cleaned == "" || cleaned == "." {
			return nil, fmt.Errorf("windows-acl: invalid workspace root %q", path)
		}
		info, err := os.Stat(cleaned)
		if err != nil {
			return nil, fmt.Errorf("workspace %q: %w", cleaned, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("workspace %q is not a directory", cleaned)
		}
		workspaces = append(workspaces, cleaned)
	}
	if len(workspaces) == 0 {
		return nil, errors.New("windows-acl: no workspace roots")
	}
	return workspaces, nil
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

// winRunConfinedCommand 在受限令牌下 spawn 目标 argv，透传 stdio 并镜像退出码。
func winRunConfinedCommand(argv []string, token syscall.Token, environment []string) (int, error) {
	if len(argv) == 0 {
		return 0, errors.New("windows-acl: empty command argv")
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if environment != nil {
		command.Env = environment
	}
	command.SysProcAttr = &syscall.SysProcAttr{Token: token}
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			code := exitError.ExitCode()
			if code < 0 {
				code = 1
			}
			return code, nil
		}
		return 0, fmt.Errorf("spawn %q: %w", argv[0], err)
	}
	return 0, nil
}
