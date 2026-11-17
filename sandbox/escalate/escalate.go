package escalate

// 线协议的数据类型与 NDJSON 读写。
//
// 这一层只做"字节 ↔ 结构体"：它不判定策略、不执行命令，因此可以被两端共用。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const (
	// DefaultMaxOutputBytes 是单条响应里 stdout / stderr 各自默认的字节上限
	// （256 KiB）。超出部分被截断并置 Response.Truncated。
	DefaultMaxOutputBytes = 256 << 10
	// DefaultMaxTimeout 是单条请求默认的执行时长上限（60s）。
	DefaultMaxTimeout = 60 * time.Second
	// DefaultMaxConcurrent 是 broker 允许同时在途的执行数上限。
	DefaultMaxConcurrent = 4
	// DefaultMaxQueue 是并发额度已满时允许排队等待的请求数上限。
	DefaultMaxQueue = 64
	// QueueFullReason 是排队数超过 Policy.MaxQueue 时的拒绝原因。宿主可以按这个
	// 常量判断"该退避重试"，而不是把它当成策略拒绝或超时。
	QueueFullReason = "服务器繁忙：排队请求过多"
	// DefaultMaxSeenIDs 是防重放集合的容量上限；超过后按 FIFO 淘汰最老的 ID，
	// 保证内存有界（淘汰只影响很久以前的 ID 能否重放，不影响近期请求）。
	DefaultMaxSeenIDs = 8192
	// MaxLineBytes 是 NDJSON 单行上限（1 MiB）。超过即拒绝，不解析。
	MaxLineBytes = 1 << 20
	// DeniedExitCode 是"策略拒绝、命令未执行"的退出码，沿用 shell 对
	// "不可执行"的 126 约定，便于调用方在没有 Denied 字段时也能区分。
	DeniedExitCode = 126
)

// ErrClosed 表示连接已经关闭：Serve 侧用于"对端断开"，Client 侧用于
// "关闭后仍在使用"。两侧的错误都用 %w 包装它，因此可以 errors.Is 判定。
var ErrClosed = errors.New("escalate: 连接已关闭")

// Request 是沙箱内进程发出的一次提权执行请求。
//
// 所有字段都来自不受信的一端：broker 侧必须重新校验，不能直接采信。
type Request struct {
	// ID 是请求标识，用于把并发响应配对回请求；同时参与防重放（同一 broker
	// 生命周期内不得重复）。为空会被 broker 拒绝。
	ID string `json:"id"`
	// Argv 是要执行的完整参数向量（不含 shell）；为空会被拒绝。
	Argv []string `json:"argv"`
	// Dir 是工作目录，必须是绝对路径，且规范化（EvalSymlinks）后落在 Policy.Roots 内。
	Dir string `json:"dir"`
	// Env 是希望附加给子进程的环境变量；只有白名单前缀会被保留，其余丢弃并在
	// stderr 里留下诊断（只记键名，不记值）。
	Env map[string]string `json:"env,omitempty"`
	// Reason 是模型/工具给出的提权理由，必填：它进入审批提示与审计日志，
	// 是"为什么需要脱离沙箱"的唯一解释。
	Reason string `json:"reason"`
	// TimeoutMS 是期望的超时（毫秒）。<=0 表示用策略默认值，且永远被
	// Policy.MaxTimeout 压住。
	TimeoutMS int `json:"timeoutMs,omitempty"`
}

// Response 是一次提权请求的结果。
type Response struct {
	// ID 与 Request.ID 一致，用于配对。
	ID string `json:"id"`
	// ExitCode 是子进程退出码；-1 表示没有正常退出（超时、取消、执行失败），
	// DeniedExitCode(126) 表示策略拒绝、命令根本没有执行。
	ExitCode int `json:"exitCode"`
	// Stdout 是标准输出（可能被截断）。
	Stdout string `json:"stdout"`
	// Stderr 是标准错误（可能被截断），并可能附加 broker 的诊断行。
	Stderr string `json:"stderr"`
	// Denied 表示"策略/审批/校验拒绝，命令未执行"。
	Denied bool `json:"denied,omitempty"`
	// DenyReason 是拒绝原因（面向模型与用户，必须可读）。
	DenyReason string `json:"denyReason,omitempty"`
	// Truncated 表示 stdout 或 stderr 因超限被截断。
	Truncated bool `json:"truncated,omitempty"`
}

// Decision 是策略对一条请求的判定结果。
type Decision struct {
	// Allow 表示命令本身被策略允许（还需要通过 Dir 校验与可能的审批）。
	Allow bool
	// NeedApproval 表示该命令必须经人工审批才能执行。
	NeedApproval bool
	// Reason 是判定说明：拒绝时是拒绝原因；需要审批时是命中审批列表的说明。
	Reason string
}

// readBoundedLine 从 r 读一行（含或不含结尾换行都算一行）。
//
// 超过 limit 字节时返回 tooLong=true，并把**本行剩余字节全部丢弃**（读到换行为止），
// 这样调用方可以回一条拒绝响应后继续使用同一条连接，而不是被一行垃圾数据带崩。
func readBoundedLine(r *bufio.Reader, limit int) (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, readErr := r.ReadSlice('\n')
		if len(buf)+len(chunk) > limit {
			// 丢弃本行剩余字节；期间真正的 IO 错误要上报。
			for readErr == bufio.ErrBufferFull {
				if _, readErr = r.ReadSlice('\n'); readErr == nil {
					break
				}
			}
			if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) && !errors.Is(readErr, io.EOF) {
				return nil, false, readErr
			}
			return nil, true, nil
		}
		buf = append(buf, chunk...)
		switch {
		case readErr == nil:
			return buf, false, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF) && len(buf) > 0:
			// 末行没有换行符也当作完整一行。
			return buf, false, nil
		default:
			return nil, false, readErr
		}
	}
}

// marshalLine 把一个值编码成"一行 JSON + 换行"。
func marshalLine(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// writeJSONLine 把一个值作为一行 JSON 写出去（一次 Write，避免与其他写入交错）。
func writeJSONLine(w io.Writer, v any) error {
	line, err := marshalLine(v)
	if err != nil {
		return err
	}
	_, err = w.Write(line)
	return err
}

// isBlankLine 判断一行是否只有空白（用于忽略心跳空行）。
func isBlankLine(line []byte) bool { return len(bytes.TrimSpace(line)) == 0 }
