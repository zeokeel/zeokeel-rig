package osrunner

// 本文件是 Windows sandbox ACL 协调器的纯逻辑部分（不带 build tag，可在 Linux
// 上单测）：路径哈希/命名 mutex 名称、状态文件模型与校验、死 grant 清理、
// grant/cleanup 状态机。Win32 实际读写（命名 mutex、DACL/LABEL、marker 句柄）
// 由 windows_acl_backend.go（//go:build windows）实现。
//
// 并发根因：旧实现每个 helper 保存整份 workspace DACL，退出时恢复。并发 helper
// 互相覆盖：后退出者用自己进入时的快照覆盖前者的授权，导致另一进程的写失败。
//
// 新模型：每个 workspace 目录一份跨进程状态文件 + 一把跨进程命名 mutex。
//   - mutex 内 load/purge/apply/save，状态变更串行化；
//   - 每个 grant 有自己的命名 marker mutex，helper 存活期间保持打开；其他 helper
//     用 OpenMutex 判断 grant 是否仍存活（进程崩溃句柄自动消失 → 视为死 grant）；
//   - 只有最后一个 grant 退出时才恢复原始 descriptor（DACL+LABEL）并删除状态；
//   - 死 grant 的 capability ACE 在下一个 helper 的 mutex 内被移除（崩溃自愈）。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// winACLStateVersion 是状态文件格式版本。
const winACLStateVersion = 1

// 跨进程命名对象前缀（Local\ 会话命名空间：同一用户会话内有效）。
const (
	winACLStateMutexPrefix = `Local\ZeoKeelSandboxACL-`
	winACLGrantMutexPrefix = `Local\ZeoKeelSandboxGrant-`
	// winACLStateDirName 是 %LOCALAPPDATA% 下的目录名。
	winACLStateDirName = "ZeoKeel"
	// winACLStateSubdir 是状态文件子目录。
	winACLStateSubdir = "sandbox-acl"
)

// winACLGrantRecord 是一个活跃 grant 的记录：capability SID 与它的 marker
// mutex 名称（由 pathHash+sidHash 派生，持久化以便跨进程推导/校验）。
type winACLGrantRecord struct {
	SID    string `json:"sid"`
	Marker string `json:"marker"`
}

// winACLState 是每个 workspace 目录的跨进程 ACL 协调状态。
type winACLState struct {
	// Version 是状态格式版本（未知版本 fail closed）。
	Version int `json:"version"`
	// Path 是 workspace 目录路径（诊断用；跨进程一致性由 pathHash 保证）。
	Path string `json:"path"`
	// PathHash 是路径哈希（状态文件名，也是 mutex 名称的一部分）。
	PathHash string `json:"pathHash"`
	// Descriptor 是进入前的原始 self-relative security descriptor（base64）。
	Descriptor string `json:"descriptor"`
	// DenyAdded 表示 Everyone 的 FILE_DELETE_CHILD 拒绝 ACE 已添加。
	DenyAdded bool `json:"denyAdded"`
	// LabelApplied 表示 Low 强制完整性标签已成功应用。
	LabelApplied bool `json:"labelApplied"`
	// Grants 是当前活跃的 grant 列表（含自己的和并发 helper 的）。
	Grants []winACLGrantRecord `json:"grants"`
}

// winACLPathHash 返回 workspace 路径的稳定哈希（sha256 hex）。
// 调用方传入已做 filepath.Clean 的路径；这里只做首尾空白折叠，避免同一产品
// 参数在不同 helper 进程里产生不同哈希。
func winACLPathHash(path string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(path)))
	return hex.EncodeToString(sum[:])
}

// winACLSIDHash 返回 capability SID 的稳定哈希（sha256 hex）。
func winACLSIDHash(sid string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sid)))
	return hex.EncodeToString(sum[:])
}

// winACLStateMutexName 返回串行化 ACL 状态变更的跨进程 mutex 名称。
func winACLStateMutexName(pathHash string) string {
	return winACLStateMutexPrefix + pathHash
}

// winACLGrantMarkerName 返回一个 grant 的存活 marker mutex 名称。
func winACLGrantMarkerName(pathHash, sidHash string) string {
	return winACLGrantMutexPrefix + pathHash + "-" + sidHash
}

// winACLStateDirectory 返回状态文件目录。
//
// localAppData 优先（%LOCALAPPDATA%\ZeoKeel\sandbox-acl）；不可用时退回
// tempDir（%TEMP%\ZeoKeel\sandbox-acl）。两者都为空时返回空串（调用方 fail
// closed 或退化为不使用跨进程状态）。
func winACLStateDirectory(localAppData, tempDir string) string {
	if base := strings.TrimSpace(localAppData); base != "" {
		return filepath.Join(base, winACLStateDirName, winACLStateSubdir)
	}
	if base := strings.TrimSpace(tempDir); base != "" {
		return filepath.Join(base, winACLStateDirName, winACLStateSubdir)
	}
	return ""
}

// winACLStateFilePath 返回一个 workspace 的状态文件路径。
func winACLStateFilePath(localAppData, tempDir, pathHash string) string {
	dir := winACLStateDirectory(localAppData, tempDir)
	if dir == "" || strings.TrimSpace(pathHash) == "" {
		return ""
	}
	return filepath.Join(dir, pathHash+".json")
}

// validateWinACLState 校验状态文件内容（损坏/不版本化一律 fail closed）。
func validateWinACLState(state *winACLState) error {
	if state == nil {
		return errors.New("windows-acl: nil ACL state")
	}
	if state.Version != winACLStateVersion {
		return fmt.Errorf("windows-acl: unsupported ACL state version %d (want %d)", state.Version, winACLStateVersion)
	}
	if strings.TrimSpace(state.PathHash) == "" {
		return errors.New("windows-acl: ACL state has no path hash")
	}
	if description := strings.TrimSpace(state.Descriptor); description != "" {
		if _, err := base64.StdEncoding.DecodeString(description); err != nil {
			return fmt.Errorf("windows-acl: ACL state descriptor is not valid base64: %w", err)
		}
	} else if state.DenyAdded || state.LabelApplied {
		return errors.New("windows-acl: ACL state records applied changes without an original descriptor")
	}
	seen := make(map[string]bool, len(state.Grants))
	for _, grant := range state.Grants {
		sid := strings.TrimSpace(grant.SID)
		if sid == "" || strings.TrimSpace(grant.Marker) == "" {
			return errors.New("windows-acl: ACL state has a grant without SID/marker")
		}
		if seen[sid] {
			return fmt.Errorf("windows-acl: ACL state has a duplicate grant for %q", sid)
		}
		seen[sid] = true
	}
	return nil
}

// parseWinACLState 解析并校验状态文件内容。
func parseWinACLState(data []byte) (*winACLState, error) {
	var state winACLState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("windows-acl: parse ACL state: %w", err)
	}
	if err := validateWinACLState(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

// marshalWinACLState 序列化状态（写入前调用 validate 保持 fail closed）。
func marshalWinACLState(state *winACLState) ([]byte, error) {
	if err := validateWinACLState(state); err != nil {
		return nil, err
	}
	return json.MarshalIndent(state, "", "  ")
}

// hasActiveGrants 报告状态里是否还有活跃 grant。
func (state *winACLState) hasActiveGrants() bool {
	return state != nil && len(state.Grants) > 0
}

// addGrant 记录一个 grant（同一 SID 覆盖为最新 marker）。
func (state *winACLState) addGrant(sid, marker string) {
	sid = strings.TrimSpace(sid)
	marker = strings.TrimSpace(marker)
	for index := range state.Grants {
		if state.Grants[index].SID == sid {
			state.Grants[index].Marker = marker
			return
		}
	}
	state.Grants = append(state.Grants, winACLGrantRecord{SID: sid, Marker: marker})
}

// removeGrant 移除一个 grant；返回是否移除。
func (state *winACLState) removeGrant(sid string) bool {
	sid = strings.TrimSpace(sid)
	for index := range state.Grants {
		if state.Grants[index].SID != sid {
			continue
		}
		state.Grants = append(state.Grants[:index], state.Grants[index+1:]...)
		return true
	}
	return false
}

// pruneDead 移除 marker 已不存在的 grant，返回被移除的记录（调用方负责移除
// 它们残留在真实目录上的 capability ACE）。
func (state *winACLState) pruneDead(alive func(marker string) bool) []winACLGrantRecord {
	if state == nil || alive == nil {
		return nil
	}
	kept := state.Grants[:0]
	var dead []winACLGrantRecord
	for _, grant := range state.Grants {
		if alive(grant.Marker) {
			kept = append(kept, grant)
			continue
		}
		dead = append(dead, grant)
	}
	state.Grants = kept
	return dead
}

// needsRestore 报告"已无活跃 grant 但状态里仍有已应用的修改"（清理残留）。
func (state *winACLState) needsRestore() bool {
	if state == nil || state.hasActiveGrants() {
		return false
	}
	return strings.TrimSpace(state.Descriptor) != "" && (state.DenyAdded || state.LabelApplied)
}

// winACLModeWritesACL 报告一个 helper 模式是否允许修改 workspace ACL。
// read-only 模式必须保持目录安全描述符原样（限制完全来自受限令牌/restricting
// SID），因此不创建状态文件、不加 ACE、不打标签。
func winACLModeWritesACL(mode string) bool {
	return strings.TrimSpace(mode) == winModeWorkspaceWrite
}

// winACLBackend 是状态机依赖的 Win32/文件系统副作用接缝；Linux 单测用 fake
// 实现驱动同一状态机。
type winACLBackend interface {
	// LoadState 读取状态；不存在返回 (nil, nil)，损坏返回错误（fail closed）。
	LoadState() (*winACLState, error)
	// SaveState 原子写状态。
	SaveState(state *winACLState) error
	// RemoveState 删除状态文件（不存在视为成功）。
	RemoveState() error
	// WithACLMutex 在跨进程 ACL mutex 内执行 fn（串行化 load/apply/save）。
	WithACLMutex(pathHash string, fn func() error) error
	// CaptureOriginal 读取并序列化当前 self-relative security descriptor（base64）。
	CaptureOriginal() (string, error)
	// AddWorkspaceDeny 添加 Everyone 的 FILE_DELETE_CHILD 拒绝 ACE（仅容器继承）。
	AddWorkspaceDeny() error
	// ApplyLowLabel 尽力应用 Low 强制完整性标签；返回是否成功。
	ApplyLowLabel() (bool, error)
	// AddCapabilityACE 给 capability SID 添加写+删允许 ACE。
	AddCapabilityACE(capabilitySID string) error
	// RevokeCapabilityACE 移除 capability SID 的所有 ACE（REVOKE_ACCESS）。
	RevokeCapabilityACE(capabilitySID string) error
	// RestoreOriginal 恢复原始 descriptor（restoreLabel 为 false 时只恢复 DACL）。
	RestoreOriginal(state *winACLState, restoreLabel bool) error
	// MarkerAlive 报告 marker mutex 是否仍被某个 helper 持有。
	MarkerAlive(marker string) bool
	// CreateMarker 创建并持有本 grant 的 marker mutex，返回不透明句柄。
	CreateMarker(name string) (any, error)
	// CloseMarker 释放 marker mutex 句柄。
	CloseMarker(handle any)
}

// winACLSession 是一次 helper 对某个目录的 grant/cleanup 生命周期。
type winACLSession struct {
	backend       winACLBackend
	path          string
	pathHash      string
	capabilitySID string
	marker        string
	markerHandle  any
	markerOpen    bool
}

// newWinACLSession 构造一个 grant 会话（路径需已清理）。
func newWinACLSession(backend winACLBackend, path, capabilitySID string) *winACLSession {
	pathHash := winACLPathHash(path)
	return &winACLSession{
		backend:       backend,
		path:          path,
		pathHash:      pathHash,
		capabilitySID: strings.TrimSpace(capabilitySID),
		marker:        winACLGrantMarkerName(pathHash, winACLSIDHash(capabilitySID)),
	}
}

// Grant 获取目录授权：创建 marker、进入跨进程 mutex、清理死 grant/残留状态、
// 首次保存原始 descriptor、添加 deny/标签/自己的 ACE 并持久化状态。
// 返回 Low 标签是否已是生效状态（供 helper 决定令牌完整性降级）。
func (session *winACLSession) Grant() (labelApplied bool, err error) {
	if session == nil || session.backend == nil {
		return false, errors.New("windows-acl: nil ACL session")
	}
	if session.capabilitySID == "" {
		return false, errors.New("windows-acl: empty capability SID")
	}
	if strings.TrimSpace(session.path) == "" {
		return false, errors.New("windows-acl: empty workspace path")
	}
	handle, err := session.backend.CreateMarker(session.marker)
	if err != nil {
		return false, fmt.Errorf("windows-acl: create grant marker: %w", err)
	}
	session.markerHandle = handle
	session.markerOpen = true
	// A failure before spawn must not leave the marker open or a partial grant
	// marked alive; cleanup is the helper's error path.
	err = session.backend.WithACLMutex(session.pathHash, func() error {
		applied, grantErr := session.grantLocked()
		labelApplied = applied
		return grantErr
	})
	if err != nil {
		session.closeMarker()
		return labelApplied, err
	}
	return labelApplied, nil
}

// grantLocked 是 Grant 的 mutex 内逻辑。
func (session *winACLSession) grantLocked() (bool, error) {
	state, err := session.backend.LoadState()
	if err != nil {
		return false, fmt.Errorf("windows-acl: load ACL state: %w", err)
	}
	if state != nil {
		if _, purgeErr := session.purgeDeadLocked(state); purgeErr != nil {
			return false, purgeErr
		}
		switch {
		case state.hasActiveGrants():
			// Another helper is still alive: keep the shared deny/label and add ours.
		case state.needsRestore():
			// Stale state from a crash/partial cleanup: restore the original
			// descriptor first so the fresh grant starts from a clean baseline.
			if restoreErr := session.backend.RestoreOriginal(state, state.LabelApplied); restoreErr != nil {
				return false, fmt.Errorf("windows-acl: restore stale ACL state: %w", restoreErr)
			}
			if removeErr := session.backend.RemoveState(); removeErr != nil {
				return false, fmt.Errorf("windows-acl: remove stale ACL state: %w", removeErr)
			}
			state = nil
		default:
			// State left nothing applied: drop it before starting over.
			if removeErr := session.backend.RemoveState(); removeErr != nil {
				return false, fmt.Errorf("windows-acl: remove empty ACL state: %w", removeErr)
			}
			state = nil
		}
	}
	if state == nil {
		descriptor, captureErr := session.backend.CaptureOriginal()
		if captureErr != nil {
			return false, fmt.Errorf("windows-acl: capture original security descriptor: %w", captureErr)
		}
		if strings.TrimSpace(descriptor) == "" {
			return false, errors.New("windows-acl: empty original security descriptor")
		}
		state = &winACLState{
			Version:    winACLStateVersion,
			Path:       session.path,
			PathHash:   session.pathHash,
			Descriptor: descriptor,
		}
	}
	if !state.DenyAdded {
		if denyErr := session.backend.AddWorkspaceDeny(); denyErr != nil {
			return false, fmt.Errorf("windows-acl: add workspace delete-child deny: %w", denyErr)
		}
		state.DenyAdded = true
	}
	if !state.LabelApplied {
		applied, labelErr := session.backend.ApplyLowLabel()
		if applied {
			state.LabelApplied = true
		} else if labelErr != nil {
			// Best-effort downgrade: the restricted token still carries the
			// restricting SIDs; a missing label never widens access. Record the
			// failure in the helper log by returning it nowhere: state keeps
			// LabelApplied=false so a later grant may retry once.
			_ = labelErr
		}
	}
	if addErr := session.backend.AddCapabilityACE(session.capabilitySID); addErr != nil {
		return false, fmt.Errorf("windows-acl: add capability ACE: %w", addErr)
	}
	state.addGrant(session.capabilitySID, session.marker)
	if saveErr := session.backend.SaveState(state); saveErr != nil {
		return false, fmt.Errorf("windows-acl: save ACL state: %w", saveErr)
	}
	return state.LabelApplied, nil
}

// Cleanup 释放本 grant：移除自己的 ACE、清理死 grant，最后一个 grant 退出时恢复
// 原始 descriptor 并删除状态，然后关闭 marker。幂等；任何错误都会返回（helper
// 上报 cleanup 失败）但绝不放宽授权。
func (session *winACLSession) Cleanup() error {
	if session == nil || session.backend == nil {
		return nil
	}
	defer session.closeMarker()
	return session.backend.WithACLMutex(session.pathHash, session.cleanupLocked)
}

// cleanupLocked 是 Cleanup 的 mutex 内逻辑。
func (session *winACLSession) cleanupLocked() error {
	state, err := session.backend.LoadState()
	if err != nil {
		// Corrupted state: do not guess/rewrite security descriptors. Fail closed
		// and leave the (already restricted) directory untouched.
		return fmt.Errorf("windows-acl: load ACL state for cleanup: %w", err)
	}
	var firstErr error
	remember := func(candidate error) {
		if candidate != nil && firstErr == nil {
			firstErr = candidate
		}
	}
	// Always revoke our own capability ACE: it is the one grant we know about even
	// when the state file is missing (crash between ACE add and state save).
	if revokeErr := session.backend.RevokeCapabilityACE(session.capabilitySID); revokeErr != nil {
		remember(fmt.Errorf("windows-acl: revoke capability ACE: %w", revokeErr))
	}
	if state == nil {
		return firstErr
	}
	state.removeGrant(session.capabilitySID)
	if _, purgeErr := session.purgeDeadLocked(state); purgeErr != nil {
		remember(purgeErr)
	}
	if state.hasActiveGrants() {
		if saveErr := session.backend.SaveState(state); saveErr != nil {
			remember(fmt.Errorf("windows-acl: save ACL state: %w", saveErr))
		}
		return firstErr
	}
	if state.needsRestore() {
		if restoreErr := session.backend.RestoreOriginal(state, state.LabelApplied); restoreErr != nil {
			// Keep the state file: the next helper retries the restore instead of
			// losing the original descriptor forever.
			return fmt.Errorf("windows-acl: restore original security descriptor: %w", restoreErr)
		}
	}
	if removeErr := session.backend.RemoveState(); removeErr != nil {
		remember(fmt.Errorf("windows-acl: remove ACL state: %w", removeErr))
	}
	return firstErr
}

// purgeDeadLocked 移除死 grant 并把它们残留的 capability ACE 从目录上移除。
func (session *winACLSession) purgeDeadLocked(state *winACLState) (int, error) {
	dead := state.pruneDead(session.backend.MarkerAlive)
	for _, grant := range dead {
		if revokeErr := session.backend.RevokeCapabilityACE(grant.SID); revokeErr != nil {
			return len(dead), fmt.Errorf("windows-acl: revoke dead grant %q: %w", grant.SID, revokeErr)
		}
	}
	return len(dead), nil
}

// closeMarker 关闭 marker mutex 句柄（幂等）。
func (session *winACLSession) closeMarker() {
	if session == nil || !session.markerOpen {
		return
	}
	session.markerOpen = false
	if session.markerHandle != nil {
		session.backend.CloseMarker(session.markerHandle)
		session.markerHandle = nil
	}
}
