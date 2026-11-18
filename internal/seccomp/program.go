package seccomp

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// BPF 指令编码（linux/bpf_common.h）。
const (
	codeLoad = 0x00
	codeWord = 0x00
	codeAbs  = 0x20

	codeJump = 0x05
	codeJEQ  = 0x10

	codeRet = 0x06
	codeK   = 0x00

	// retAllow 是 SECCOMP_RET_ALLOW。
	retAllow = 0x7fff0000
	// retErrno 是 SECCOMP_RET_ERRNO 基址；低 16 位是 errno。
	retErrno = 0x00050000

	// errnoEPERM 是 EPERM：与"权限不足"语义一致，工具/模型能读懂，
	// 也不会像 ENOSYS 那样被程序误判成"内核不支持"。
	errnoEPERM = 1

	// afUnix 是 AF_UNIX（本地 unix socket 族）。
	afUnix = 1

	// seccompDataArgs0 是 struct seccomp_data.args[0] 的偏移：nr(4) + arch(4) +
	// instruction_pointer(8) = 16。
	seccompDataArgs0 = 16
	// seccompDataArch 是 struct seccomp_data.arch 的偏移。
	seccompDataArch = 4
	// seccompDataNR 是 struct seccomp_data.nr 的偏移。
	seccompDataNR = 0
)

// ErrUnsupported 表示当前架构没有内置的 seccomp 系统调用号表。
var ErrUnsupported = errors.New("seccomp: unsupported architecture")

// sockFilter 是 struct sock_filter（8 字节）。
type sockFilter struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

// Program 是一个已编译的 seccomp BPF 程序（sock_filter 数组的字节串）。
type Program struct {
	bytes []byte
}

// Bytes 返回可直接交给内核 / bwrap `--seccomp` 的程序字节。
func (p Program) Bytes() []byte { return append([]byte(nil), p.bytes...) }

// Len 返回指令条数。
func (p Program) Len() int { return len(p.bytes) / 8 }

// Empty 报告程序是否为空（空程序表示"不装过滤器"）。
func (p Program) Empty() bool { return len(p.bytes) == 0 }

// NetworkDeny 返回"禁止一切网络连接"的过滤器。
//
// 形状（照 Codex 的做法，已被 cargo/npm/go 等真实工具链验证过）：
//   - 默认放行（RET ALLOW），只对黑名单 syscall 返回 EPERM；
//   - `socket(2)`/`socketpair(2)` 只在 domain == AF_UNIX 时放行——unix socket
//     本身无害（本地管道、nscd、terminal 都要用），有害的是"连上宿主守护进程"；
//   - 拦 `connect`/`accept`/`accept4`/`bind`/`listen`/`sendto`/`sendmsg`/
//     `sendmmsg`/`recvmsg`/`recvmmsg`/`ptrace`；
//   - `recvfrom` 不在黑名单里（显式放行，便于阅读）。
//
// 注意：它连**回环**一起拦。只在 AllowNetwork=false 时使用。
func NetworkDeny() (Program, error) {
	blocked, socketNR, arch, ok := networkBlockedSyscalls()
	if !ok {
		return Program{}, ErrUnsupported
	}
	// 布局：
	//   0  LD  arch
	//   1  JEQ <audit arch>  (不匹配 → 拒)
	//   2  RET EPERM
	//   3  LD  nr
	//   4  JEQ socket → 跳到 socket 块
	//   5.. 每条黑名单一条 JEQ → 跳到 DENY
	//   N  RET ALLOW
	//   N+1 RET EPERM (DENY)
	//   N+2 LD args[0]   (socket 块)
	//   N+3 JEQ AF_UNIX
	//   N+4 RET ALLOW
	//   N+5 RET EPERM
	denyIdx := 5 + len(blocked) + 1
	sockIdx := denyIdx + 1

	filters := make([]sockFilter, 0, sockIdx+4)
	filters = append(filters,
		sockFilter{code: codeLoad | codeWord | codeAbs, k: seccompDataArch},
		sockFilter{code: codeJump | codeJEQ, jt: 1, k: arch},
		sockFilter{code: codeRet | codeK, k: retErrno | errnoEPERM},
		sockFilter{code: codeLoad | codeWord | codeAbs, k: seccompDataNR},
		sockFilter{code: codeJump | codeJEQ, jt: uint8(sockIdx - 5), k: socketNR},
	)
	for _, nr := range blocked {
		idx := len(filters)
		filters = append(filters, sockFilter{
			code: codeJump | codeJEQ,
			jt:   uint8(denyIdx - idx - 1),
			k:    nr,
		})
	}
	filters = append(filters,
		sockFilter{code: codeRet | codeK, k: retAllow},
		sockFilter{code: codeRet | codeK, k: retErrno | errnoEPERM},
		sockFilter{code: codeLoad | codeWord | codeAbs, k: seccompDataArgs0},
		sockFilter{code: codeJump | codeJEQ, jt: 0, jf: 1, k: afUnix},
		sockFilter{code: codeRet | codeK, k: retAllow},
		sockFilter{code: codeRet | codeK, k: retErrno | errnoEPERM},
	)
	return Program{bytes: marshal(filters)}, nil
}

// marshal 把指令数组序列化成字节串（小端；amd64 与 arm64 都是小端）。
func marshal(filters []sockFilter) []byte {
	var buf bytes.Buffer
	buf.Grow(len(filters) * 8)
	for _, f := range filters {
		_ = binary.Write(&buf, binary.LittleEndian, f.code)
		_ = binary.Write(&buf, binary.LittleEndian, f.jt)
		_ = binary.Write(&buf, binary.LittleEndian, f.jf)
		_ = binary.Write(&buf, binary.LittleEndian, f.k)
	}
	return buf.Bytes()
}
