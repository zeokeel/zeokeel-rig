package kit

import (
	"context"
	"fmt"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
)

// MemoryActivations 是 rig.ToolActivationStore 的**进程内**实现。
//
// 用途有两个：测试，以及"没有落盘需求"的无头宿主（进程重启后回到
// InitialActiveTools，但同一进程里多个 agent 共享激活结果）。要跨重启保留，宿主
// 自己实现 rig.ToolActivationStore（落文件/数据库都行）。
//
// 并发安全：内部互斥锁，同一实例可以给多个 agent / 多个会话共用。
type MemoryActivations struct {
	mu        sync.Mutex
	bySession map[string][]string
}

// 编译期断言：MemoryActivations 必须满足根包的可选接口。
var _ rig.ToolActivationStore = (*MemoryActivations)(nil)

// NewMemoryActivations 构造一个空的进程内激活集存储。
func NewMemoryActivations() *MemoryActivations {
	return &MemoryActivations{bySession: make(map[string][]string)}
}

// Load 返回会话已保存的工具名副本。
//
// 没有记录返回 rig.ErrNoActivationRecord（键不存在）；空切片表示**有记录且为空**
// （该会话的工具全被取消激活）——两者不能混用，否则"全部取消激活"在重启后复活。
func (m *MemoryActivations) Load(_ context.Context, sessionID string) ([]string, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil MemoryActivations", rig.ErrNoActivationRecord)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.bySession[sessionID]
	if !ok {
		return nil, fmt.Errorf("%w: session %q", rig.ErrNoActivationRecord, sessionID)
	}
	return append([]string{}, stored...), nil
}

// Save 覆盖写入会话的完整 active 集合（保存副本，调用方之后改切片不影响存储）。
//
// 空集合也**保留成一条记录**：它表示"该会话的全部工具都被取消激活了"，与"没有
// 记录"（Load 返回 ErrNoActivationRecord）不是一回事。
func (m *MemoryActivations) Save(_ context.Context, sessionID string, tools []string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bySession == nil {
		m.bySession = make(map[string][]string)
	}
	m.bySession[sessionID] = append([]string{}, tools...)
	return nil
}
