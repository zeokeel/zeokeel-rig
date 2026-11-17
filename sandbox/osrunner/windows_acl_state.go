package osrunner

// 本文件是 Windows sandbox ACL 协调器的纯逻辑部分（不带 build tag，可在 Linux
// 上单测）：路径哈希/命名 mutex 名称、状态文件模型与校验、死 grant 清理、
// grant/cleanup/回滚状态机。Win32 实际读写（命名 mutex、DACL/LABEL、marker
// 句柄）由 windows_acl_backend.go（//go:build windows）实现。
//
// # 并发根因（v0.19）
//
// 旧实现每个 helper 保存整份 workspace DACL，退出时恢复。并发 helper 互相覆盖：
// 后退出者用自己进入时的快照覆盖前者的授权，导致另一进程的写失败。
//
// # 协调模型（保留）
//
// 每个 workspace 目录一份跨进程状态文件 + 一把跨进程命名 mutex：
//   - mutex 内 load/purge/apply/save，状态变更串行化；
//   - 每个 grant 有自己的命名 marker mutex，helper 存活期间保持打开；其他 helper
//     用 OpenMutex 判断 grant 是否仍存活（进程崩溃句柄自动消失 → 视为死 grant）；
//   - 只有最后一个 grant 退出时才做恢复；
//   - 死 grant 的 capability ACE 在下一个 helper 的 mutex 内被移除（崩溃自愈）。
//
// # 缓存语义（v0.20，B2 修复）
//
// 工作区能力 SID 由规范化路径派生（winWorkspaceCapabilitySID），同一工作区跨
// 进程/会话是**同一个 SID**，因此目录上的 ACE 可以跨运行复用。据此把 ACL 改动
// 分成两类：
//
//   - **缓存资产**（保留不撤）：capability SID 的允许 ACE、保护路径子树上的 deny
//     ACE。它们只对持有该 capability SID 的受限子进程有意义，对宿主用户/其它
//     进程没有影响，残留也无害；下次运行直接认领，避免每条命令重写全树 ACL
//     （大仓库上这是分钟级开销）。
//   - **per-run 部分**（最后一名 helper 退出时恢复）：Everyone 的
//     FILE_DELETE_CHILD 拒绝与 Low 强制完整性标签。它们会影响宿主用户（Everyone
//     拒绝会让用户也删不掉自己的文件），绝不能长期留在真实目录上。
//
// 描述符快照仍然是**异常路径**的恢复依据：每次在没有活跃 grant 且 per-run 部分
// 已经干净时刷新基线，回滚/迁移时用它做完整恢复。
//
// # 部分应用失败（B1 修复）
//
// 旧实现先 CaptureOriginal，再分三次独立调用改 DACL（Everyone 拒绝）、改标签、
// 加 capability ACE，最后才 SaveState。中途任何一步失败 → 状态文件从未落盘 →
// cleanup 走 LoadState()==nil 分支只撤销 capability ACE 就返回 →
// **Everyone 拒绝与 Low 标签永久留在真实目录上**，下一次运行还把它当原始基线。
//
// 现在有两条独立防线的保证：
//   - 先落盘：拿到原始描述符后**立即** SaveState（此时尚未改任何 ACL），因此
//     "改了东西但状态没落盘"的窗口不再存在；任何后续失败都能从状态文件恢复。
//   - 合并写入：workspace 根的形状用**一次** SetNamedSecurityInfoW 同时设置 DACL
//     （Everyone 拒绝 + capability 允许）与标签，消除 DACL/SACL 之间的中间态
//     （SetNamedSecurityInfoW 本身可以同时接收 pDacl 与 pSacl）。
//
// 失败时 grantLocked 会在同一个 mutex 内显式回滚（撤 ACE + 完整恢复 + 删状态），
// 而不是只依赖 helper 的 cleanup。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	// winACLStateVersion 是当前状态文件格式版本。
	//
	// 与 v1 的差异：新增 Protected（保护路径账本）与缓存语义。v1 是"随机能力
	// SID + 每次恢复原始描述符"，与新语义（确定性 SID + 缓存资产）不兼容。
	winACLStateVersion = 2
	// winACLStateVersionLegacy 是 v0.19.x 的格式。读到它时按"完整恢复 + 重建"
	// 处理：用快照恢复 DACL/标签、删掉状态，再按新语义重新建立。
	winACLStateVersionLegacy = 1
)

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

// winACLState 是每个 workspace（或私有临时目录）的跨进程 ACL 协调状态。
type winACLState struct {
	// Version 是状态格式版本（未知版本 fail closed）。
	Version int `json:"version"`
	// Path 是目录路径（诊断用；跨进程一致性由 pathHash 保证）。
	Path string `json:"path"`
	// PathHash 是路径哈希（状态文件名，也是 mutex 名称的一部分）。
	PathHash string `json:"pathHash"`
	// Descriptor 是进入前的原始 self-relative security descriptor（base64）。
	// 它是异常路径（回滚/迁移）的唯一恢复依据，也是缓存 ACE 的"基线"。
	Descriptor string `json:"descriptor"`
	// DenyAdded 表示 Everyone 的 FILE_DELETE_CHILD 拒绝 ACE 当前**存在**
	// （因为本协调器而存在）。per-run 部分：最后一名 helper 退出时恢复。
	DenyAdded bool `json:"denyAdded"`
	// LabelApplied 表示 Low 强制完整性标签当前**存在**（因为本协调器而存在）。
	// per-run 部分，同上。
	LabelApplied bool `json:"labelApplied"`
	// Protected 是已经（或正在）在 workspace 子树里强制 deny ACE 的保护路径。
	//
	// 这是**账本**而不是一次性输入：它跨运行保留，用于
	//   (a) 幂等跳过重复的全树遍历（大仓库上这是分钟级开销）；
	//   (b) 策略删除某条保护路径时精确撤销对应 ACE（否则会留下一条永远拒绝写入
	//       的陈旧 deny ACE）；
	//   (c) 异常回滚时撤掉本次/历史写下的所有 deny ACE。
	Protected []string `json:"protected,omitempty"`
	// Grants 是当前活跃的 grant 列表（含自己的和并发 helper 的）。
	Grants []winACLGrantRecord `json:"grants"`
}

// winACLPathHash 返回目录路径的稳定哈希（sha256 hex）。
//
// 键是 winPathKey（大小写不敏感、分隔符统一）：Windows 路径大小写不敏感，同一
// 目录的两种写法必须落到同一份状态文件与同一把 mutex 上，否则并发协调会失效。
func winACLPathHash(path string) string {
	sum := sha256.Sum256([]byte(winPathKey(path)))
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
//
// nonce 让**每次 grant**（= 每个 helper 进程）都有唯一的 marker，即使两个并发
// helper 共享同一个 capability SID（workspace 的 SID 由路径确定性派生，必须共享
// ACE）。marker 唯一是整套并发语义的基础：
//   - MarkerAlive 精确回答"这个 grant 还活着吗"，purgeDead 才能只清理死记录；
//   - 每个 helper 只删自己的 grant 记录，因此 A 退出时不会误判"自己是最后一个"
//     而恢复 B 还在用的 Everyone 拒绝/Low 标签（那会立刻削弱 B 的子进程边界）。
func winACLGrantMarkerName(pathHash, sidHash, nonce string) string {
	return winACLGrantMutexPrefix + pathHash + "-" + sidHash + "-" + nonce
}

// winACLGrantNonce 生成一次 grant 的唯一后缀（8 字节熵，hex）。
func winACLGrantNonce() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("windows-acl: grant nonce entropy: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
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

// winACLStateFilePath 返回一个目录的状态文件路径。
func winACLStateFilePath(localAppData, tempDir, pathHash string) string {
	dir := winACLStateDirectory(localAppData, tempDir)
	if dir == "" || strings.TrimSpace(pathHash) == "" {
		return ""
	}
	return filepath.Join(dir, pathHash+".json")
}

// legacy 报告状态来自 v1 格式（没有缓存 ACE / protect 账本的概念）。
func (state *winACLState) legacy() bool {
	return state != nil && state.Version < winACLStateVersion
}

// validateWinACLState 校验状态文件内容（损坏/不版本化一律 fail closed）。
func validateWinACLState(state *winACLState) error {
	if state == nil {
		return errors.New("windows-acl: nil ACL state")
	}
	if state.Version != winACLStateVersion && state.Version != winACLStateVersionLegacy {
		return fmt.Errorf("windows-acl: unsupported ACL state version %d (want %d or legacy %d)",
			state.Version, winACLStateVersion, winACLStateVersionLegacy)
	}
	if strings.TrimSpace(state.PathHash) == "" {
		return errors.New("windows-acl: ACL state has no path hash")
	}
	if description := strings.TrimSpace(state.Descriptor); description != "" {
		if _, err := base64.StdEncoding.DecodeString(description); err != nil {
			return fmt.Errorf("windows-acl: ACL state descriptor is not valid base64: %w", err)
		}
	}
	// 身份是 marker（每次 grant 唯一）；同一 capability SID 允许多条记录
	// （并发 helper 共享确定性的 workspace SID）。
	seen := make(map[string]bool, len(state.Grants))
	for _, grant := range state.Grants {
		sid := strings.TrimSpace(grant.SID)
		marker := strings.TrimSpace(grant.Marker)
		if sid == "" || marker == "" {
			return errors.New("windows-acl: ACL state has a grant without SID/marker")
		}
		if seen[marker] {
			return fmt.Errorf("windows-acl: ACL state has a duplicate grant marker %q", marker)
		}
		seen[marker] = true
	}
	protected := make(map[string]bool, len(state.Protected))
	for _, path := range state.Protected {
		path = strings.TrimSpace(path)
		if path == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("windows-acl: ACL state has a protected path that is empty or relative: %q", path)
		}
		key := winPathKey(path)
		if protected[key] {
			return fmt.Errorf("windows-acl: ACL state has a duplicate protected path %q", path)
		}
		protected[key] = true
	}
	if (state.DenyAdded || state.LabelApplied || len(state.Protected) > 0) && strings.TrimSpace(state.Descriptor) == "" {
		return errors.New("windows-acl: ACL state records applied changes without an original descriptor")
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

// addGrant 记录一个 grant（以 marker 为身份：同一 SID 可以有多个并发 grant）。
func (state *winACLState) addGrant(sid, marker string) {
	sid = strings.TrimSpace(sid)
	marker = strings.TrimSpace(marker)
	for index := range state.Grants {
		if state.Grants[index].Marker == marker {
			state.Grants[index].SID = sid
			return
		}
	}
	state.Grants = append(state.Grants, winACLGrantRecord{SID: sid, Marker: marker})
}

// removeGrant 按 marker 移除一个 grant；返回是否移除。
//
// 只删自己那条：同一工作区的并发 helper 共享 capability SID，用 SID 当身份会让
// 先退出的 helper 误删后退出者的记录（进而误判自己是最后一个）。
func (state *winACLState) removeGrant(marker string) bool {
	marker = strings.TrimSpace(marker)
	for index := range state.Grants {
		if state.Grants[index].Marker != marker {
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

// winACLModeWritesACL 报告一个 helper 模式是否允许修改 workspace ACL。
// read-only 模式必须保持目录安全描述符原样（限制完全来自受限令牌/restricting
// SID），因此不创建状态文件、不加 ACE、不打标签。
func winACLModeWritesACL(mode string) bool {
	return strings.TrimSpace(mode) == winModeWorkspaceWrite
}

// winACLShape 是一个目录当前 ACL 形状的摘要，用于"已经是我们要的形状就跳过
// apply"的幂等判定。
type winACLShape struct {
	// CapabilityAllow 表示存在精确的 capability SID 允许 ACE（缓存资产）。
	CapabilityAllow bool
	// WorkspaceDeny 表示存在精确的 Everyone FILE_DELETE_CHILD 拒绝 ACE。
	WorkspaceDeny bool
	// LowLabel 表示存在精确的 Low 完整性标签。
	LowLabel bool
}

// winShapeRequest 是一次形状建立请求。
type winShapeRequest struct {
	CapabilitySID string
	// WorkspaceDeny 表示同时添加 Everyone 的 FILE_DELETE_CHILD 拒绝 ACE。
	WorkspaceDeny bool
	// LowLabel 表示同时应用 Low 强制完整性标签。
	LowLabel bool
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
	// ACLShape 读取本目录当前的 ACL 形状摘要（幂等判定）。
	ACLShape(capabilitySID string) (winACLShape, error)
	// ApplyShape 用**一次** SetNamedSecurityInfoW 建立形状（DACL + 标签），返回
	// 建立后的形状（标签可能因缺少权限而退化为未应用）。
	ApplyShape(request winShapeRequest) (winACLShape, error)
	// RestoreShape 恢复 per-run 部分（Everyone 拒绝 + Low 标签），保留缓存资产
	// （capability 允许 ACE；保护路径的 deny ACE 不在本目录上，不受影响）。
	RestoreShape(state *winACLState, capabilitySID string, restoreLabel bool) error
	// RestoreOriginal 用快照完整恢复 DACL（+标签）；异常路径与私有临时目录使用。
	RestoreOriginal(state *winACLState, restoreLabel bool) error
	// RevokeCapabilityACE 移除本目录上该 SID 的所有 ACE（REVOKE_ACCESS）。
	RevokeCapabilityACE(capabilitySID string) error
	// Object 返回某个路径的 ACE 接缝（保护子树遍历使用）。
	Object(path string) winProtectObject
	// MarkerAlive 报告 marker mutex 是否仍被某个 helper 持有。
	MarkerAlive(marker string) bool
	// CreateMarker 创建并持有本 grant 的 marker mutex，返回不透明句柄。
	CreateMarker(name string) (any, error)
	// CloseMarker 释放 marker mutex 句柄。
	CloseMarker(handle any)
}

// winACLSessionOptions 决定一个 grant 会话负责哪一部分形状。
type winACLSessionOptions struct {
	// Protected 是本次要在 workspace 子树里强制 deny ACE 的保护路径（已校验）。
	Protected []string
	// WorkspaceDeny 表示给本目录加 Everyone 的 FILE_DELETE_CHILD 拒绝 ACE。
	WorkspaceDeny bool
	// LowLabel 表示尝试给本目录打 Low 强制完整性标签。
	LowLabel bool
	// CacheCapability 表示 capability 允许 ACE 跨运行保留（workspace 根）。
	//
	// false 用于一次运行的私有临时目录：cleanup 会撤销它的 ACE 并完整恢复快照。
	CacheCapability bool
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
	options       winACLSessionOptions
}

// newWinACLSession 构造一个 grant 会话（路径需已清理）。
func newWinACLSession(backend winACLBackend, path, capabilitySID string, options winACLSessionOptions) *winACLSession {
	pathHash := winACLPathHash(path)
	return &winACLSession{
		backend:       backend,
		path:          path,
		pathHash:      pathHash,
		capabilitySID: strings.TrimSpace(capabilitySID),
		options:       options,
	}
}

// Grant 获取目录授权：创建 marker、进入跨进程 mutex、认领/建立形状、记录 grant
// 并持久化状态。失败时在同一把 mutex 内回滚，绝不留下部分应用的 ACL。
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
	if strings.TrimSpace(session.marker) == "" {
		nonce, nonceErr := winACLGrantNonce()
		if nonceErr != nil {
			return false, nonceErr
		}
		session.marker = winACLGrantMarkerName(session.pathHash, winACLSIDHash(session.capabilitySID), nonce)
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
func (session *winACLSession) grantLocked() (labelApplied bool, err error) {
	state, err := session.backend.LoadState()
	if err != nil {
		return false, fmt.Errorf("windows-acl: load ACL state: %w", err)
	}
	if state != nil {
		if _, purgeErr := session.purgeDeadLocked(state); purgeErr != nil {
			return false, purgeErr
		}
		if state.legacy() {
			// v1 状态：完整恢复到进入前的描述符，丢掉旧账本，按新语义重建。
			if restoreErr := session.backend.RestoreOriginal(state, state.LabelApplied); restoreErr != nil {
				return false, fmt.Errorf("windows-acl: restore legacy ACL state: %w", restoreErr)
			}
			if removeErr := session.backend.RemoveState(); removeErr != nil {
				return false, fmt.Errorf("windows-acl: remove legacy ACL state: %w", removeErr)
			}
			state = nil
		}
	}
	// 读形状必须在 v1 迁移之后：迁移会完整恢复描述符，形状会变。
	shape, shapeErr := session.backend.ACLShape(session.capabilitySID)
	if shapeErr != nil {
		return false, fmt.Errorf("windows-acl: read workspace ACL shape: %w", shapeErr)
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
		// B1 修复的核心：先落盘，再改 ACL。此后的任何失败都有恢复锚点。
		if saveErr := session.backend.SaveState(state); saveErr != nil {
			return false, fmt.Errorf("windows-acl: save ACL state: %w", saveErr)
		}
	} else if !state.hasActiveGrants() && !shape.WorkspaceDeny && !shape.LowLabel {
		// 没有活跃 grant，且 per-run 部分已经干净：刷新基线快照。这样"首次运行
		// 以来第三方对该目录 DACL 的改动"会被纳入基线，日后回滚不会把它们一起
		// 覆盖掉。（per-run 部分不干净时不刷新：否则会把 Everyone 拒绝/Low 标签
		// 写进基线，恢复时永远删不掉——那正是 B1 的残留形态。）
		if descriptor, captureErr := session.backend.CaptureOriginal(); captureErr == nil && strings.TrimSpace(descriptor) != "" {
			state.Descriptor = descriptor
		}
	}
	if shapeErr := session.ensureShapeLocked(state, shape); shapeErr != nil {
		return false, session.abortLocked(state, shapeErr)
	}
	if protectErr := session.ensureProtectLocked(state); protectErr != nil {
		return false, session.abortLocked(state, protectErr)
	}
	state.addGrant(session.capabilitySID, session.marker)
	if saveErr := session.backend.SaveState(state); saveErr != nil {
		state.removeGrant(session.marker)
		return false, session.abortLocked(state, fmt.Errorf("windows-acl: save ACL state: %w", saveErr))
	}
	return state.LabelApplied, nil
}

// ensureShapeLocked 认领既有形状或一次性建立它（幂等）。
func (session *winACLSession) ensureShapeLocked(state *winACLState, shape winACLShape) error {
	needCapability := !shape.CapabilityAllow
	needDeny := session.options.WorkspaceDeny && !shape.WorkspaceDeny
	needLabel := session.options.LowLabel && !shape.LowLabel
	if needCapability || needDeny || needLabel {
		applied, err := session.backend.ApplyShape(winShapeRequest{
			CapabilitySID: session.capabilitySID,
			WorkspaceDeny: session.options.WorkspaceDeny,
			LowLabel:      session.options.LowLabel,
		})
		if err != nil {
			return fmt.Errorf("windows-acl: apply workspace ACL shape: %w", err)
		}
		shape = applied
	}
	// 记录 per-run 部分的**实际**状态：cleanup 必须把因为本协调器而存在的
	// Everyone 拒绝与 Low 标签恢复掉（它们会影响宿主用户，不能像 capability ACE
	// 那样缓存）。若形状本来就在（并发 helper 或崩溃残留），也照实记录。
	state.DenyAdded = session.options.WorkspaceDeny && shape.WorkspaceDeny
	state.LabelApplied = session.options.LowLabel && shape.LowLabel
	return nil
}

// ensureProtectLocked 把保护路径账本与策略对齐：
//
//   - 新增路径：遍历子树加精确 deny ACE（已有精确 ACE 的对象跳过）；
//   - 删除路径（策略不再保护它）：遍历子树撤掉我们那条 deny ACE——否则会留下
//     一条永久拒绝写入的陈旧 ACE，让"取消保护"变成静默失败；
//   - 未变化路径：只抽检保护根上的精确 ACE 是否还在，命中就完全跳过遍历（缓存
//     复用，避免每条命令一次全树 ACL 重传）；不在则重放整棵子树（自愈）。
func (session *winACLSession) ensureProtectLocked(state *winACLState) error {
	desired := session.options.Protected
	added := winProtectDifference(desired, state.Protected)
	removed := winProtectDifference(state.Protected, desired)
	unchanged := winProtectIntersection(state.Protected, desired)
	objects := session.backend.Object
	for _, path := range removed {
		if _, err := winRelaxProtectTree(objects, path, session.capabilitySID); err != nil {
			return fmt.Errorf("windows-acl: lift protection from %q: %w", path, err)
		}
	}
	for _, path := range added {
		if _, err := winEnforceProtectTree(objects, path, session.capabilitySID); err != nil {
			return fmt.Errorf("windows-acl: enforce protection on %q: %w", path, err)
		}
	}
	for _, path := range unchanged {
		object := objects(path)
		if object == nil {
			return fmt.Errorf("windows-acl: no ACL handle for protected path %q", path)
		}
		present, err := object.DenyPresent(session.capabilitySID)
		if err != nil {
			return fmt.Errorf("windows-acl: read protected ACL %q: %w", path, err)
		}
		if present {
			continue
		}
		if _, err := winEnforceProtectTree(objects, path, session.capabilitySID); err != nil {
			return fmt.Errorf("windows-acl: re-enforce protection on %q: %w", path, err)
		}
	}
	state.Protected = append([]string(nil), desired...)
	return nil
}

// abortLocked 回滚一次失败的 grant：撤掉保护路径上的 deny ACE、用快照完整恢复
// DACL/标签、删除状态文件。
//
// 并发安全：如果状态里还有 grant 记录，就**不**回滚共享形状——把别的 helper 的
// 授权面一起撤掉是更坏的结果。marker 每次 grant 唯一，因此"还有记录"精确等于
// "还有别的 helper 活着"（本次的记录在失败路径上已经被摘掉）。
//
// 这种情况下不改形状，但把本次可能已经写下的保护路径并进账本：否则一条"写到
// 一半"的 deny ACE 会永远留在子树里，将来策略取消保护时也撤不掉。
func (session *winACLSession) abortLocked(state *winACLState, cause error) error {
	if state == nil {
		return cause
	}
	if state.hasActiveGrants() {
		if ledgerErr := session.recordProtectLedgerLocked(state); ledgerErr != nil {
			return fmt.Errorf("%w (recording the protect ledger also failed: %v)", cause, ledgerErr)
		}
		return cause
	}
	var firstErr error
	remember := func(candidate error) {
		if candidate != nil && firstErr == nil {
			firstErr = candidate
		}
	}
	// 保护路径的 deny ACE 只对这个 capability SID 有影响，撤掉永远安全（最多让
	// 下一次运行多走一次全树遍历）。账本与本次期望值取并集：中途失败时账本可能
	// 还没更新。
	pending := append([]string(nil), state.Protected...)
	pending = append(pending, winProtectDifference(session.options.Protected, pending)...)
	for _, path := range pending {
		if _, err := winRelaxProtectTree(session.backend.Object, path, session.capabilitySID); err != nil {
			remember(fmt.Errorf("windows-acl: lift protection from %q: %w", path, err))
		}
	}
	if err := session.backend.RestoreOriginal(state, true); err != nil {
		remember(fmt.Errorf("windows-acl: restore original security descriptor: %w", err))
	}
	if err := session.backend.RemoveState(); err != nil {
		remember(fmt.Errorf("windows-acl: remove ACL state: %w", err))
	}
	if firstErr != nil {
		return fmt.Errorf("%w (rollback also failed: %v)", cause, firstErr)
	}
	return cause
}

// Cleanup 释放本 grant：移除自己的 grant 记录、清理死 grant；最后一个 grant 退出
// 时恢复 per-run 部分（workspace）或完整恢复并删状态（私有临时目录），然后关闭
// marker。幂等；任何错误都会返回（helper 上报 cleanup 失败）但绝不放宽授权。
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
	if state == nil {
		// 状态文件不存在：本会话没有改过任何 ACL（状态在改 ACL 之前落盘），只剩
		// "上一次运行留下的 capability ACE" 这一种可能，而那只对一次性私有临时
		// 目录有意义（workspace 的 ACE 是缓存资产，必须保留）。
		if !session.options.CacheCapability {
			if revokeErr := session.backend.RevokeCapabilityACE(session.capabilitySID); revokeErr != nil {
				remember(fmt.Errorf("windows-acl: revoke capability ACE: %w", revokeErr))
			}
		}
		return firstErr
	}
	if state.legacy() {
		if restoreErr := session.backend.RestoreOriginal(state, state.LabelApplied); restoreErr != nil {
			return fmt.Errorf("windows-acl: restore legacy ACL state: %w", restoreErr)
		}
		if removeErr := session.backend.RemoveState(); removeErr != nil {
			remember(fmt.Errorf("windows-acl: remove legacy ACL state: %w", removeErr))
		}
		return firstErr
	}
	state.removeGrant(session.marker)
	if _, purgeErr := session.purgeDeadLocked(state); purgeErr != nil {
		remember(purgeErr)
	}
	if state.hasActiveGrants() {
		if saveErr := session.backend.SaveState(state); saveErr != nil {
			remember(fmt.Errorf("windows-acl: save ACL state: %w", saveErr))
		}
		return firstErr
	}
	if session.options.CacheCapability {
		// 缓存语义：capability ACE 与 protect deny ACE 保留（下次运行直接复用，
		// 避免每条命令一次全树 ACL 重传）；只恢复会影响宿主用户的 per-run 部分。
		if state.DenyAdded || state.LabelApplied {
			if restoreErr := session.backend.RestoreShape(state, session.capabilitySID, state.LabelApplied); restoreErr != nil {
				// 保留状态文件：下一个 helper 重试恢复，而不是把原始描述符弄丢。
				return fmt.Errorf("windows-acl: restore workspace ACL shape: %w", restoreErr)
			}
			state.DenyAdded = false
			state.LabelApplied = false
		}
		if saveErr := session.backend.SaveState(state); saveErr != nil {
			remember(fmt.Errorf("windows-acl: save ACL state: %w", saveErr))
		}
		return firstErr
	}
	// 一次运行的私有临时目录：完整恢复快照并删除状态。
	if restoreErr := session.backend.RestoreOriginal(state, state.LabelApplied); restoreErr != nil {
		// Keep the state file: the next helper retries the restore instead of
		// losing the original descriptor forever.
		return fmt.Errorf("windows-acl: restore original security descriptor: %w", restoreErr)
	}
	if removeErr := session.backend.RemoveState(); removeErr != nil {
		remember(fmt.Errorf("windows-acl: remove ACL state: %w", removeErr))
	}
	return firstErr
}

// recordProtectLedgerLocked 把本会话的期望保护路径并进账本（幂等）。
//
// 只用于"无法回滚"的失败路径：让后续运行能按账本精确撤销写到一半的 deny ACE。
func (session *winACLSession) recordProtectLedgerLocked(state *winACLState) error {
	merged := append([]string(nil), state.Protected...)
	added := winProtectDifference(session.options.Protected, merged)
	if len(added) == 0 {
		return nil
	}
	state.Protected = append(merged, added...)
	return session.backend.SaveState(state)
}

// purgeDeadLocked 移除死 grant 并把它们的 capability ACE 从目录上移除。
//
// 例外：SID 等于本会话 capability SID 的记录不撤——workspace 的能力 SID 是确定性的
// 缓存资产，撤掉会让并发的其它 helper 立刻失去写授权，并让下次运行重新付一次全树
// ACL 开销。死记录本身仍然从状态里移除（否则会无限增长）。
func (session *winACLSession) purgeDeadLocked(state *winACLState) (int, error) {
	dead := state.pruneDead(session.backend.MarkerAlive)
	for _, grant := range dead {
		if strings.TrimSpace(grant.SID) == session.capabilitySID {
			continue
		}
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
