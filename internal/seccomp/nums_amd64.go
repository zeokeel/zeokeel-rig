//go:build linux && amd64

package seccomp

// auditArchX86_64 是 AUDIT_ARCH_X86_64（`<linux/audit.h>`）。
const auditArchX86_64 = 0xC000003E

// nrSocket 等是 amd64 的系统调用号（`<asm/unistd_64.h>`）。
const (
	nrSocket     = 41
	nrConnect    = 42
	nrAccept     = 43
	nrSendto     = 44
	nrRecvfrom   = 45
	nrSendmsg    = 46
	nrRecvmsg    = 47
	nrBind       = 49
	nrListen     = 50
	nrSocketpair = 53
	nrPtrace     = 101
	nrAccept4    = 288
	nrRecvmmsg   = 299
	nrSendmmsg   = 307
)

// networkBlockedSyscalls 返回（黑名单, socket 号, Audit 架构号, 是否支持）。
func networkBlockedSyscalls() ([]uint32, uint32, uint32, bool) {
	return []uint32{
		nrConnect, nrAccept, nrAccept4, nrBind, nrListen,
		nrSendto, nrSendmsg, nrSendmmsg, nrRecvmsg, nrRecvmmsg, nrPtrace,
	}, nrSocket, auditArchX86_64, true
}
