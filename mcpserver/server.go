// Package mcpserver 把一组 rig.Tool 暴露成 MCP server：stdio 传输 + JSON-RPC 2.0
// over NDJSON（一行一条消息），协议子集对齐 MCP 2025-06-18 的 tools 部分。
//
// 它是 tools/mcp 的**对侧**：tools/mcp 是 client（连接别人的 MCP server 并把工具
// 包装成 rig.Tool），本包是 server（把我们的 rig.Tool 暴露给别人——另一个 agent、
// 编辑器或 CLI）。两者线格式逐字段对齐，仓库里有真实的 client↔server 端到端测试。
//
// # 支持的子集
//
//   - initialize / notifications/initialized（版本协商 + serverInfo + instructions）
//   - tools/list（inputSchema 直接透传 rig.Tool.Schema()，缺失时补 {"type":"object"}）
//   - tools/call（成功/工具级失败/panic/取消；参数必须是 JSON 对象）
//   - ping
//   - notifications/cancelled（尽力取消在途调用）
//   - notifications/message（服务端 → 客户端的日志通知，见 NotifyLog）
//
// 不支持（按需再加，都会是"明确的错误"而不是静默行为）：resources/*、prompts/*、
// sampling、roots、elicitation、批量请求、任务（tasks）、分页 cursor。
//
// # 严格协议模式（可选）
//
// 默认行为对既有客户端保持宽容：`jsonrpc` 字面值不校验、`id` 允许任意类型、
// `initialize` 之前也接受 `tools/call`（很多内嵌客户端就是这样用的）。
//
// `WithStrictProtocol(true)` 打开严格模式后按 JSON-RPC 2.0 与 MCP 契约收紧：
// `jsonrpc` 必须逐字 `"2.0"`、`id` 只能是 string/number/null、`tools/call` 必须在
// `initialize` 之后、重复 `initialize` 拒绝——违反一律 -32600 并写明原因。
// 面向第三方客户端时建议打开；对接既有宽松客户端时保持默认（升级即破坏不可接受）。
//
// 工具名在默认模式下只做**告警**（stderr）：形状不合规（不符合
// ^[A-Za-z0-9_-]{1,64}$）不会让 New 失败，严格模式下才升级为错误。空名与重名
// 属结构性错误，两种模式下都直接报错。
//
// # 错误语义（重要）
//
// 区分两类失败，避免把"工具挂了"误报成"协议错"：
//
//   - **协议/参数错误** → JSON-RPC error：坏 JSON = -32700、非法请求（含批量数组）
//     = -32600、未知方法 = -32601、未知工具名或 arguments 不是对象 = -32602。
//   - **工具执行失败** → 正常的 tools/call 结果 + `isError: true` + 文本内容。
//     工具返回 Go error 或 panic 也走这条路（工具执行失败 ≠ 协议错误）。
//
// # 输出上限（必须 ≤ 客户端读取上限）
//
// 一条响应行 = 一个 JSON-RPC 消息。tools/mcp 客户端用 bufio.Scanner 读行，上限是
// 8 MiB（mcp.MaxMessageBytes），超了就是 "bufio.Scanner: token too long"、连接
// 直接不可用。因此：
//
//   - DefaultMaxResultBytes = 4 MiB = 客户端上限的一半（留出 JSON 信封与转义的余量）；
//   - 另有与 WithMaxResultBytes 无关的硬约束：content 数组编码后必须 ≤ 6 MiB，
//     按**转义后**的真实字节数反解保留长度（`"` 翻倍、控制符 6 倍也算在内）；
//   - 超限时截断最长的 text 块 + `_meta.truncated`，仍然是正常结果（不是协议错误）。
//
// # 并发
//
// Serve 为每条请求起一个 goroutine，写回由一把 mutex 串行化，因此单个慢工具不会
// 阻塞 ping 或其它调用。Serve 返回时保证所有在途调用已经结束（或被 ctx 取消）。
//
// # 典型用法
//
//	server, err := mcpserver.New(tools,
//		mcpserver.WithServerInfo("my-rig", "1.0.0"),
//		mcpserver.WithInstructions("先 read_file 再 edit_file"),
//	)
//	if err != nil {
//		return err
//	}
//	return server.Serve(ctx, os.Stdin, os.Stdout)
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// MaxLineBytes 是单条入站消息（一行）的字节上限。超限按 -32700 处理并继续读，
	// 不断流——否则一个畸形客户端就能让 server 退出。
	MaxLineBytes = 1 << 20
	// DefaultServerName 是未指定 WithServerInfo 时的 serverInfo.name。
	DefaultServerName = "zeokeel-rig"
	// DefaultLogLevel 是 NotifyLog 收到空 level 时使用的级别。
	DefaultLogLevel = "info"
	// eofDrainGrace 是 stdin EOF 之后给在途调用的收尾宽限期。
	//
	// 半关闭（客户端关了 stdin 但仍在读 stdout）时响应还写得出，所以先等一小会；
	// 但绝不能无限等：一个不响应 ctx 的工具会把 Serve——进而整个进程退出——挂死。
	// 宽限期过后取消在途调用，再等一次；仍然不结束就返回错误。
	eofDrainGrace = 500 * time.Millisecond
)

// Server 是一个 MCP server。零值不可用，请用 New 构造。
//
// 它并发安全：Serve 期间可以由其它 goroutine 调用 NotifyLog。
type Server struct {
	name         string
	version      string
	instructions string

	tools  []rig.Tool
	byName map[string]rig.Tool

	// maxResultBytes 是单次 tools/call 结果的文本上限；0 表示不限（内部哨兵，
	// 只能由 WithMaxResultBytes(-1) 产生）。
	maxResultBytes int

	// writeMu 串行化所有出站写（响应与通知）。
	writeMu  sync.Mutex
	out      io.Writer
	serving  bool
	writeErr error

	// inflight 记录在途调用的取消函数，供 notifications/cancelled 使用。
	inflightMu sync.Mutex
	inflight   map[string]context.CancelFunc

	// wg 跟踪在途请求 goroutine（Serve 退出前等待它们）。
	wg sync.WaitGroup

	// strict 打开"严格协议模式"（WithStrictProtocol）。默认关闭，保持与既有
	// 客户端兼容；关闭时不做 JSON-RPC 2.0 的字面校验。
	strict bool
	// warnOut 是构造期告警的去处（默认 os.Stderr；测试可替换）。
	warnOut io.Writer
	// protoMu 保护 initialized（Serve 会并发处理请求）。
	protoMu sync.Mutex
	// initialized 记录是否已经完成 initialize（严格模式用来拒绝提前的 tools/call
	// 与重复 initialize）。
	initialized bool
}

// toolNamePattern 是严格模式下工具名必须匹配的形状。
//
// MCP 客户端普遍把工具名当作"标识符"拼进 prompt 与选择逻辑，因此限制成
// [A-Za-z0-9_-]{1,64}：不含空格/点号/斜杠/Unicode，长度不超过 64。
var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Option 配置 Server。
type Option func(*Server)

// WithServerInfo 设置 initialize 响应里的 serverInfo（name/version 会被 trim；
// 空值回落到 DefaultServerName 与 rig.Version）。
func WithServerInfo(name, version string) Option {
	return func(s *Server) {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			s.name = trimmed
		}
		if trimmed := strings.TrimSpace(version); trimmed != "" {
			s.version = trimmed
		}
	}
}

// WithMaxResultBytes 设置单次 tools/call 结果的文本字节上限（默认
// DefaultMaxResultBytes = 4 MiB）。
//
// 默认值取 tools/mcp 客户端读取上限（8 MiB）的一半：服务端写出的行必须**严格
// 小于**对端的 scanner 上界，否则真实客户端会以 "bufio.Scanner: token too long"
// 失败（不是优雅报错）。调用方把上限调大时自己承担这个风险；无论如何，编码后的
// content 仍受 6 MiB 的 transport 硬上限约束。
//
//	n > 0                  → 使用 n
//	n == MaxResultBytesUnlimited (-1) → 不限（显式关闭保护）
//	其它（0、< -1）        → 回落到 DefaultMaxResultBytes
//
// 超限时只截断最长的 text 块并加截断注释 + `_meta.truncated`，仍然是正常结果
// （不是 JSON-RPC error）。0 刻意**不**表示"不限"：参数拼错不应该静默关掉保护。
func WithMaxResultBytes(n int) Option {
	return func(s *Server) {
		switch {
		case n == MaxResultBytesUnlimited:
			s.maxResultBytes = 0
		case n > 0:
			s.maxResultBytes = n
		default:
			s.maxResultBytes = DefaultMaxResultBytes
		}
	}
}

// WithStrictProtocol 打开/关闭**严格协议模式**（默认关闭）。
//
// 关闭时行为与历史版本完全一致：只校验"能定位问题"的部分（坏 JSON、缺 method、
// 批量数组），对 `jsonrpc` 字面值与 `id` 类型保持宽容——很多既有客户端不发
// `jsonrpc` 字段，或把 id 发成非标量。
//
// 打开后额外强制（判定失败一律 -32600，错误信息写明原因）：
//
//   - `jsonrpc` 必须逐字为 `"2.0"`；
//   - `id` 只能是 string / number / null（对象、数组、布尔都拒绝）；
//   - `initialize` 之前不允许 `tools/call`；
//   - 重复 `initialize` 拒绝。
//
// 严格模式是**接线方的选择**：面向第三方客户端时建议打开；对接既有宽松客户端时
// 保持默认，避免升级即破坏。
func WithStrictProtocol(strict bool) Option {
	return func(s *Server) { s.strict = strict }
}

// WithInstructions 设置 initialize 响应里的 instructions（给模型的使用说明）。
func WithInstructions(text string) Option {
	return func(s *Server) {
		s.instructions = text
	}
}

// New 构造一个 server。tools 为空是合法的（tools/list 返回空数组）。
//
// 工具名必须非空、唯一：重名会让 tools/call 无法确定目标，因此在构造期就失败，
// 而不是运行期随机挑一个。
//
// 名字**形状**（^[A-Za-z0-9_-]{1,64}$）默认只告警不报错（写到 stderr，可用
// WithStrictProtocol(true) 升级为错误）：很多既有工具名带点号或空格也能正常工作，
// 升级即破坏不是好默认；严格模式下才要求在契约内。
func New(tools []rig.Tool, opts ...Option) (*Server, error) {
	server := &Server{
		name:           DefaultServerName,
		version:        rig.Version,
		maxResultBytes: DefaultMaxResultBytes,
		byName:         make(map[string]rig.Tool, len(tools)),
		warnOut:        os.Stderr,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(server)
		}
	}
	for index, tool := range tools {
		if tool == nil {
			return nil, fmt.Errorf("mcpserver: tool %d is nil", index)
		}
		name := strings.TrimSpace(tool.Name())
		if name == "" {
			return nil, fmt.Errorf("mcpserver: tool %d has an empty name", index)
		}
		if _, duplicate := server.byName[name]; duplicate {
			return nil, fmt.Errorf("mcpserver: duplicate tool name %q", name)
		}
		// 名字形状：空名与重名是结构性错误（无法索引），上面已经直接报错；
		// 形状不合规则是"能跑但可能被严格客户端拒"的问题——默认只告警，
		// 严格协议模式下才升级为错误（升级即破坏既有宿主是不可接受的）。
		if !toolNamePattern.MatchString(name) {
			if server.strict {
				return nil, fmt.Errorf("mcpserver: tool name %q must match %s (strict protocol mode)",
					name, toolNamePattern.String())
			}
			server.warnf("mcpserver: tool name %q does not match %s; strict MCP clients may reject it "+
				"(enable WithStrictProtocol(true) to make this an error)", name, toolNamePattern.String())
		}
		server.byName[name] = tool
		// 注册顺序即 tools/list 顺序：宿主用排列表达优先级（"重要工具放前面"），
		// 我们不去重排它。new 之后工具集固定，因此列表天然稳定。
		server.tools = append(server.tools, tool)
	}
	return server, nil
}

// warnf 写一条构造期告警（默认 stderr）。
//
// 告警不是错误：MCP 生态里工具名的合法性由客户端说了算，server 只负责让宿主
// 知道"这个名字可能被拒"，而不是替宿主做决定。
func (s *Server) warnf(format string, args ...any) {
	if s == nil || s.warnOut == nil {
		return
	}
	fmt.Fprintf(s.warnOut, format+"\n", args...)
}

// Serve 在 in/out 上服务，直到 in 结束（EOF）或 ctx 被取消。
//
// 返回约定：
//   - in EOF（且没有 I/O 失败）→ nil。EOF 之后先给在途调用一个宽限期
//     （eofDrainGrace，半关闭场景下响应仍写得出），随后取消它们；一个完全无视
//     ctx 的调用最多再拖一个宽限期，之后返回错误（不阻塞进程退出）；
//   - ctx 取消 → ctx.Err()（返回前已取消在途调用并等待它们结束）；
//   - 读写失败 → 对应错误。
//
// 同一个 Server 同时只允许一个 Serve。
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if s == nil {
		return errors.New("mcpserver: nil server")
	}
	if in == nil || out == nil {
		return errors.New("mcpserver: Serve requires both an input and an output")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.attach(out); err != nil {
		return err
	}
	defer s.detach()

	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()

	lines := make(chan lineItem, 16)
	terminal := make(chan error, 1)
	// stop 在 Serve 返回时关闭：让读取 goroutine 能退出，而不是永久阻塞在 lines 上
	// （取消路径下主循环不再排空队列）。
	stop := make(chan struct{})
	defer close(stop)
	go readMessages(in, lines, terminal, stop)

	for {
		select {
		case <-ctx.Done():
			// 取消在途调用，等它们真正结束，再返回。
			cancelServe()
			s.cancelAllInflight()
			s.wg.Wait()
			return ctx.Err()
		case item, ok := <-lines:
			if !ok {
				// 输入结束：不再接受新请求。先给在途调用一个短暂宽限（半关闭场景
				// 下它们的响应还写得出），然后取消；绝不无限等待，详见 eofDrainGrace。
				if err := s.drain(serveCtx, cancelServe); err != nil {
					return err
				}
				if err := <-terminal; err != nil {
					return err
				}
				return s.takeWriteError()
			}
			if item.tooLong {
				// 超长行：回 parse error 并继续服务。
				payload, err := encodeFailure(nil, &rpcFailure{
					Code:    CodeParseError,
					Message: fmt.Sprintf("message exceeds %d bytes", MaxLineBytes),
				})
				if err != nil {
					return err
				}
				if err := s.writeLine(payload); err != nil {
					return err
				}
				continue
			}
			s.dispatchLine(serveCtx, item.line)
		}
	}
}

// HandleLine 处理单独一行输入，返回应当写回的行。
//
// handled=false 表示这是一条通知（或空行）：调用方不应写任何东西。第二个返回值
// 也用于"超长行/坏 JSON"这类必须应答的情况（此时 handled=true 且返回 -32700）。
//
// err 只在**服务端自身**无法产生应答时非 nil（例如内部编码失败）；协议层问题都
// 体现在返回的 JSON-RPC error 对象里。
func (s *Server) HandleLine(ctx context.Context, line []byte) ([]byte, bool, error) {
	if s == nil {
		return nil, false, errors.New("mcpserver: nil server")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil, false, nil
	}
	if len(trimmed) > MaxLineBytes {
		payload, err := encodeFailure(nil, &rpcFailure{
			Code:    CodeParseError,
			Message: fmt.Sprintf("message exceeds %d bytes", MaxLineBytes),
		})
		if err != nil {
			return nil, true, err
		}
		return payload, true, nil
	}
	request, failure := parseRequest(trimmed)
	if failure != nil {
		payload, err := encodeFailure(request.ID, failure)
		if err != nil {
			return nil, true, err
		}
		return payload, true, nil
	}
	if s.strict {
		if failure := s.checkStrictProtocol(request); failure != nil {
			payload, err := encodeFailure(request.ID, failure)
			if err != nil {
				return nil, true, err
			}
			return payload, true, nil
		}
	}
	if request.isNotification() {
		s.handleNotification(request)
		return nil, false, nil
	}
	payload, err := s.handleRequest(ctx, request)
	if err != nil {
		return nil, true, err
	}
	return payload, true, nil
}

// checkStrictProtocol 执行严格模式的额外校验（见 WithStrictProtocol）。
//
// 只做"请求合法性"判定，不改变各方法自身的语义：返回非 nil 表示应当回 -32600。
func (s *Server) checkStrictProtocol(request wireRequest) *rpcFailure {
	if request.JSONRPC != "2.0" {
		return &rpcFailure{
			Code:    CodeInvalidRequest,
			Message: fmt.Sprintf("jsonrpc must be exactly %q (got %q)", "2.0", request.JSONRPC),
		}
	}
	if failure := validateRequestID(request.ID); failure != nil {
		return failure
	}
	if request.Method != methodToolsCall {
		return nil
	}
	s.protoMu.Lock()
	initialized := s.initialized
	s.protoMu.Unlock()
	if !initialized {
		return &rpcFailure{
			Code:    CodeInvalidRequest,
			Message: "tools/call before initialize: send initialize first",
		}
	}
	return nil
}

// validateRequestID 校验 JSON-RPC id 的类型：string / number / null 之外一律拒绝。
//
// 复合 id（对象、数组）不在 JSON-RPC 2.0 允许范围内，回显它们会让客户端的
// 请求关联逻辑失效；布尔同理。空 id（通知）与 null（无法应答的请求）保持宽容。
func validateRequestID(id json.RawMessage) *rpcFailure {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	switch trimmed[0] {
	case '"':
		return nil
	case '{', '[':
		return &rpcFailure{Code: CodeInvalidRequest, Message: "id must be a string, number or null"}
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err != nil {
		return &rpcFailure{Code: CodeInvalidRequest, Message: "id must be a string, number or null"}
	}
	return nil
}

// markInitialized 记录 initialize 已完成（严格模式用）。
func (s *Server) markInitialized() {
	s.protoMu.Lock()
	s.initialized = true
	s.protoMu.Unlock()
}

// initializedAlready 报告是否已经 initialize 过。
func (s *Server) initializedAlready() bool {
	s.protoMu.Lock()
	defer s.protoMu.Unlock()
	return s.initialized
}

// NotifyLog 发送一条 MCP 日志通知（notifications/message）。
//
// level 为空时用 DefaultLogLevel。注意：**必须在 Serve 期间调用**（需要输出流），
// 否则返回错误；tools/mcp 的 stdio 客户端目前忽略服务端通知，所以它主要用于
// 编辑器类客户端与 --verbose 调试。
func (s *Server) NotifyLog(level, message string) error {
	if s == nil {
		return errors.New("mcpserver: nil server")
	}
	if strings.TrimSpace(level) == "" {
		level = DefaultLogLevel
	}
	payload, err := encodeMessage(wireNotification{
		JSONRPC: "2.0",
		Method:  methodLogMessage,
		Params: map[string]any{
			"level":  level,
			"logger": s.name,
			"data":   message,
		},
	})
	if err != nil {
		return err
	}
	return s.writeLine(payload)
}

// drain 在 stdin EOF 之后收尾在途调用。
//
// 顺序：宽限等待 → 取消全部在途调用 → 再宽限等待 → 仍未结束则返回错误（不再阻塞）。
// 返回 nil 表示"可以干净退出了"。
func (s *Server) drain(serveCtx context.Context, cancelServe context.CancelFunc) error {
	finished := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		cancelServe()
		return nil
	case <-time.After(eofDrainGrace):
	}
	// 宽限期到了还有人在跑：取消它们（ctx 会传递到工具），再给一次机会。
	cancelServe()
	s.cancelAllInflight()
	select {
	case <-finished:
		return nil
	case <-time.After(eofDrainGrace):
		return errors.New("mcpserver: in-flight tool calls did not finish after stdin closed " +
			"(a tool ignored context cancellation); not waiting any longer so shutdown cannot hang")
	}
}

// lineItem 是读取 goroutine 交给主循环的一行。
type lineItem struct {
	line    []byte
	tooLong bool
}

// readMessages 把输入切成行送进 lines，结束时把终止错误送进 terminal 并关闭 lines。
//
// stop 关闭表示 Serve 已经返回：此时不再尝试投递（否则读取 goroutine 会永久阻塞
// 在一个没人排空的 channel 上）。
func readMessages(in io.Reader, lines chan<- lineItem, terminal chan<- error, stop <-chan struct{}) {
	defer close(lines)
	reader := bufio.NewReaderSize(in, 64*1024)
	for {
		line, tooLong, err := readLine(reader, MaxLineBytes)
		if tooLong || len(bytes.TrimSpace(line)) > 0 {
			select {
			case lines <- lineItem{line: line, tooLong: tooLong}:
			case <-stop:
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				terminal <- nil
			} else {
				terminal <- fmt.Errorf("mcpserver: read message: %w", err)
			}
			return
		}
	}
}

// dispatchLine 处理 Serve 循环里的一行：请求并行执行，通知就地处理。
func (s *Server) dispatchLine(ctx context.Context, line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	request, failure := parseRequest(trimmed)
	if failure != nil {
		payload, err := encodeFailure(request.ID, failure)
		if err == nil {
			_ = s.writeLine(payload)
		}
		return
	}
	if request.isNotification() {
		// 通知必须就地处理：notifications/cancelled 要尽快生效。
		s.handleNotification(request)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		payload, err := s.handleRequest(ctx, request)
		if err != nil {
			failure := &rpcFailure{Code: CodeInternalError, Message: err.Error()}
			if fallback, encodeErr := encodeFailure(request.ID, failure); encodeErr == nil {
				_ = s.writeLine(fallback)
			}
			return
		}
		_ = s.writeLine(payload)
	}()
}

// handleRequest 分发一条请求并返回编码后的响应。
func (s *Server) handleRequest(ctx context.Context, request wireRequest) ([]byte, error) {
	switch request.Method {
	case methodInitialize:
		return s.handleInitialize(request)
	case methodPing:
		return encodeResult(request.ID, struct{}{})
	case methodToolsList:
		return s.handleToolsList(request)
	case methodToolsCall:
		return s.handleToolsCall(ctx, request)
	default:
		return encodeFailure(request.ID, &rpcFailure{
			Code:    CodeMethodNotFound,
			Message: "method not found: " + request.Method,
		})
	}
}

// initializeResult 是 initialize 的响应体。
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

// serverInfo 是服务端身份。
type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// handleInitialize 协商协议版本并公布能力。
//
// 版本策略：客户端请求的版本在我们支持的列表里就回显它（双方都能接受）；否则回落到
// ProtocolVersion。capabilities 只声明 tools（listChanged=false：工具集在构造期固定，
// 不会变更）。
func (s *Server) handleInitialize(request wireRequest) ([]byte, error) {
	if s.strict && s.initializedAlready() {
		return encodeFailure(request.ID, &rpcFailure{
			Code:    CodeInvalidRequest,
			Message: "initialize has already been called",
		})
	}
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(bytes.TrimSpace(request.Params)) > 0 {
		// 解析失败不算错误：MCP 客户端可能只发一个空对象，协议版本按默认处理。
		_ = json.Unmarshal(request.Params, &params)
	}
	version := ProtocolVersion
	if supportedProtocolVersion(strings.TrimSpace(params.ProtocolVersion)) {
		version = strings.TrimSpace(params.ProtocolVersion)
	}
	s.markInitialized()
	result := initializeResult{
		ProtocolVersion: version,
		Capabilities: map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		ServerInfo:   serverInfo{Name: s.name, Version: s.version},
		Instructions: s.instructions,
	}
	return encodeResult(request.ID, result)
}

// supportedProtocolVersion 报告 version 是否在 SupportedProtocolVersions 里。
func supportedProtocolVersion(version string) bool {
	for _, supported := range SupportedProtocolVersions {
		if version == supported {
			return true
		}
	}
	return false
}

// handleToolsList 公布工具清单。
func (s *Server) handleToolsList(request wireRequest) ([]byte, error) {
	// 空工具集必须是 []，不能是 null。
	tools := make([]wireTool, 0, len(s.tools))
	for _, tool := range s.tools {
		tools = append(tools, wireTool{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: toolSchema(tool),
		})
	}
	return encodeResult(request.ID, wireToolListResult{Tools: tools})
}

// toolSchema 返回工具的 inputSchema，并保证它是一个 JSON 对象。
//
// rig.Tool.Schema() 允许返回 nil（"无参数"），但 MCP 的 inputSchema 必须是对象；
// 缺 type 的 schema 也补上 "type":"object"，否则客户端做参数校验时会困惑。
func toolSchema(tool rig.Tool) map[string]any {
	schema := tool.Schema()
	if schema == nil {
		return map[string]any{"type": "object"}
	}
	if _, ok := schema["type"]; ok {
		return schema
	}
	// $ref schema 不能补 type：JSON Schema 规定 $ref 与其他关键字并列时的行为
	// 依赖方言（draft-07 里同级的 type 会被 $ref 覆盖，2020-12 里则是并集），
	// 两种都可能导致客户端校验失真。原样透传引用最安全。
	if _, ok := schema["$ref"]; ok {
		return schema
	}
	patched := make(map[string]any, len(schema)+1)
	for key, value := range schema {
		patched[key] = value
	}
	patched["type"] = "object"
	return patched
}

// callParams 是 tools/call 的入参。
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// handleToolsCall 执行一次工具调用。
//
// 参数形状错误（未知工具、arguments 不是对象）→ -32602；工具自身的失败（error /
// IsError / panic）→ 正常结果 + isError=true。
func (s *Server) handleToolsCall(ctx context.Context, request wireRequest) ([]byte, error) {
	var params callParams
	if len(bytes.TrimSpace(request.Params)) > 0 {
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return encodeFailure(request.ID, &rpcFailure{
				Code:    CodeInvalidParams,
				Message: "params must be an object: " + err.Error(),
			})
		}
	}
	name := strings.TrimSpace(params.Name)
	if name == "" {
		return encodeFailure(request.ID, &rpcFailure{
			Code:    CodeInvalidParams,
			Message: "params.name is required",
		})
	}
	tool, ok := s.byName[name]
	if !ok {
		return encodeFailure(request.ID, &rpcFailure{
			Code:    CodeInvalidParams,
			Message: fmt.Sprintf("unknown tool %q", name),
		})
	}
	arguments, failure := normalizeArguments(params.Arguments)
	if failure != nil {
		return encodeFailure(request.ID, &rpcFailure{
			Code:    CodeInvalidParams,
			Message: fmt.Sprintf("tool %q %s", name, failure.Message),
		})
	}

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.registerInflight(request.idKey(), cancel)
	defer s.unregisterInflight(request.idKey())

	result := invokeTool(callCtx, tool, name, arguments)
	return encodeResult(request.ID, truncateResult(callResult(result), s.maxResultBytes))
}

// normalizeArguments 把 arguments 规整成 JSON 对象字符串。
//
// 缺省/null → "{}"；显式对象 → 原始字节（工具能看到调用方写的原文）；其它形状
// （数组、字符串、数字）→ invalid params。
func normalizeArguments(raw json.RawMessage) (string, *rpcFailure) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "{}", nil
	}
	if trimmed[0] != '{' {
		return "", &rpcFailure{Code: CodeInvalidParams, Message: "arguments must be a JSON object"}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return "", &rpcFailure{Code: CodeInvalidParams, Message: "arguments must be a JSON object: " + err.Error()}
	}
	return string(trimmed), nil
}

// invokeTool 执行工具并做 panic 隔离 + 结果映射。
//
// panic 一定来自工具实现，不能让它把整个 server 掀翻：转成 isError 结果，并保留
// panic 值便于定位。
func invokeTool(ctx context.Context, tool rig.Tool, name, arguments string) (result rig.ToolResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = rig.ToolResult{
				Content: fmt.Sprintf("工具 %s panic: %v", name, recovered),
				IsError: true,
			}
		}
	}()
	result, err := tool.Invoke(ctx, rig.ToolCall{Name: name, Arguments: arguments})
	if err != nil {
		// 工具执行失败是"工具级失败"，不是协议错误。
		return rig.ToolResult{Content: err.Error(), IsError: true}
	}
	return result
}

// callResult 把 rig.ToolResult 映射成 MCP 的 content 块列表。
//
// Content 是主文本块；Parts 追加为额外文本块（图片附件以 "[image: URL]" 形式降级：
// MCP 的 image 块要求 base64 数据，而 rig 的 Parts 只是 URL）。
func callResult(result rig.ToolResult) wireCallResult {
	content := []wireContent{{Type: "text", Text: result.Content}}
	for _, part := range result.Parts {
		switch {
		case part.Type == rig.PartImageURL && part.ImageURL != "":
			content = append(content, wireContent{Type: "text", Text: "[image: " + part.ImageURL + "]"})
		case part.Text != "":
			content = append(content, wireContent{Type: "text", Text: part.Text})
		}
	}
	return wireCallResult{Content: content, IsError: result.IsError}
}

// handleNotification 处理客户端通知（永远不产生输出）。
func (s *Server) handleNotification(request wireRequest) {
	switch request.Method {
	case methodInitialized:
		// 握手完成；本 server 不维护会话状态，无事可做。
	case methodCancelled:
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
			Reason    string          `json:"reason"`
		}
		if len(bytes.TrimSpace(request.Params)) > 0 {
			_ = json.Unmarshal(request.Params, &params)
		}
		key := string(bytes.TrimSpace(params.RequestID))
		if key == "" || key == "null" {
			return
		}
		s.cancelInflight(key)
	default:
		// 未知通知：忽略（JSON-RPC 规定不得为通知产生响应）。
	}
}

// registerInflight 记录一次在途调用的取消函数。
func (s *Server) registerInflight(key string, cancel context.CancelFunc) {
	if key == "" {
		return
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflight == nil {
		s.inflight = make(map[string]context.CancelFunc)
	}
	s.inflight[key] = cancel
}

// unregisterInflight 移除在途记录。
func (s *Server) unregisterInflight(key string) {
	if key == "" {
		return
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	delete(s.inflight, key)
}

// cancelInflight 取消指定请求 ID 的在途调用（尽力而为）。
func (s *Server) cancelInflight(key string) {
	s.inflightMu.Lock()
	cancel := s.inflight[key]
	delete(s.inflight, key)
	s.inflightMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// cancelAllInflight 取消所有在途调用（Serve 退出路径）。
func (s *Server) cancelAllInflight() {
	s.inflightMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.inflight))
	for _, cancel := range s.inflight {
		cancels = append(cancels, cancel)
	}
	s.inflight = nil
	s.inflightMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// attach 绑定输出流（每次 Serve 一次）。
func (s *Server) attach(out io.Writer) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.serving {
		return errors.New("mcpserver: Serve is already running on this server")
	}
	s.out = out
	s.serving = true
	s.writeErr = nil
	return nil
}

// detach 解绑输出流。
func (s *Server) detach() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.out = nil
	s.serving = false
}

// writeLine 写出一行（内部已含 '\n'），并记录首个写错误。
func (s *Server) writeLine(payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.out == nil {
		return errors.New("mcpserver: no output stream (call Serve first)")
	}
	if _, err := s.out.Write(payload); err != nil {
		if s.writeErr == nil {
			s.writeErr = fmt.Errorf("mcpserver: write message: %w", err)
		}
		return s.writeErr
	}
	return nil
}

// takeWriteError 返回并清除首个写错误。
func (s *Server) takeWriteError() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.writeErr
	s.writeErr = nil
	return err
}
