package kit

import (
	"io"

	toolescalate "github.com/zeokeel/zeokeel-rig/tools/escalate"
)

// EscalationConfig 打开 request_command_escalation 工具所需的全部输入。
//
// 这是一条**危险能力**：它让模型可以请求宿主以其自身权限执行命令（用宿主凭据
// 访问网络、读写 Roots 之外的东西）。因此没有默认值——Conn（提权通道）、
// Approver（每次都问人）、Options.Allow（子命令级白名单）三者缺一不可。
//
// 装配要点：
//   - Conn 用 net.Pipe 的两端（broker 端与 client 端）或 socketpair；
//   - Gate 应当**自动放行** request_command_escalation（它自己会问审批），
//     否则用户会被问两次；
//   - Options.Allow 只写子命令级前缀（{"git","fetch"}），绝不要放解释器。
type EscalationConfig struct {
	Conn     io.ReadWriteCloser
	Approver toolescalate.Approver
	Options  toolescalate.Options
}
