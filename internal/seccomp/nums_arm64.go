//go:build linux && arm64

package seccomp

// auditArchAarch64 是 AUDIT_ARCH_AARCH64。
const auditArchAarch64 = 0xC00000B7

// nrSocket 等是 arm64 的系统调用号（asm-generic 表）。
const (
	nrSocket     = 198
	nrSocketpair = 199
	nrBind       = 200
	nrListen     = 201
	nrAccept     = 202
	nrConnect    = 203
	nrSendto     = 206
	nrRecvfrom   = 207
	nrSendmsg    = 211
	nrRecvmsg    = 212
	nrAccept4    = 242
	nrRecvmmsg   = 243
	nrSendmmsg   = 269
	nrPtrace     = 117
)

// networkBlockedSyscalls 返回（黑名单, socket 号, Audit 架构号, 是否支持）。
func networkBlockedSyscalls() ([]uint32, uint32, uint32, bool) {
	return []uint32{
		nrConnect, nrAccept, nrAccept4, nrBind, nrListen,
		nrSendto, nrSendmsg, nrSendmmsg, nrRecvmsg, nrRecvmmsg, nrPtrace,
	}, nrSocket, auditArchAarch64, true
}
