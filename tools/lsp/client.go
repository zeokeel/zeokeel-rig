package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"

	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultTimeout 限制单次请求（initialize、hover、definition、document symbols、shutdown）：
// 调用方上下文没有更早截止时间时，请求在此之后失败。
const DefaultTimeout = 15 * time.Second

// DiagnosticsWait 是 Diagnostics 等待首个 textDocument/publishDiagnostics 通知、之后报告
// 服务端未发布任何诊断的时长。诊断发布是异步且可选的，因此这是尽力而为的等待而非保证。
const DiagnosticsWait = 500 * time.Millisecond

const (
	// closeShutdownTimeout 限制 Close 发出的优雅 shutdown 请求。
	closeShutdownTimeout = 2 * time.Second
	// exitGrace 是 Close 等待子进程退出的时长，超时后强杀。
	exitGrace = time.Second
	// stderrTailBytes 是客户端为错误消息保留的服务端 stderr 长度（服务端 stderr 是诊断数据，
	// 绝非协议流量）。
	stderrTailBytes = 4 << 10
	// clientName 在 initialize 握手中标识本客户端。
	clientName = "zeokeel-rig"
)

// ErrClosed 是客户端关闭后发起的请求返回的错误。
var ErrClosed = errors.New("lsp: client is closed")

// Config 描述一个语言服务器进程。
type Config struct {
	// Command 是语言服务器可执行文件。必填。
	Command string
	// Args 原样传给 Command。
	Args []string
	// Env 条目（"KEY=VALUE"）会追加到客户端进程环境后传给服务端。可选。
	Env []string
	// Dir 是服务端进程的工作目录。可选。
	Dir string
	// RootURI 是 initialize 中发送的工作区根。可选：为空时使用 Dir（或当前目录）的文件
	// URI。
	RootURI string
	// LanguageID 是 didOpen 时发送的语言 id（"go"、"python" 等）。可选：默认
	// "plaintext"。
	LanguageID string
	// Timeout 限制单次请求；零表示 DefaultTimeout。
	Timeout time.Duration
}

// normalize 校验配置并填充默认值。
func (c Config) normalize() (Config, error) {
	c.Command = strings.TrimSpace(c.Command)
	if c.Command == "" {
		return Config{}, errors.New("lsp: Config.Command is required")
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	c.LanguageID = strings.TrimSpace(c.LanguageID)
	if c.LanguageID == "" {
		c.LanguageID = "plaintext"
	}
	c.RootURI = strings.TrimSpace(c.RootURI)
	if c.RootURI == "" {
		dir := strings.TrimSpace(c.Dir)
		if dir == "" {
			workingDirectory, err := os.Getwd()
			if err != nil {
				return Config{}, fmt.Errorf("lsp: resolve working directory: %w", err)
			}
			dir = workingDirectory
		}
		uri, err := PathToURI(dir)
		if err != nil {
			return Config{}, err
		}
		c.RootURI = uri
	}
	return c, nil
}

// rpcOutcome 是投递给等待请求的一次响应。err 要么是服务端的 JSON-RPC 错误，要么是连接的
// 终止错误（保留错误链，调用方可用 errors.Is 判断上下文取消）。
type rpcOutcome struct {
	result json.RawMessage
	err    error
}

// docState 是客户端为某个已打开文档最后发送的文本。
type docState struct {
	text    string
	version int
}

// Client 是一条运行中的语言服务器连接。
//
// 它可安全并发使用：请求按 ID 多路复用，文档状态由互斥锁保护，诊断由唯一的读 goroutine
// 收集。Close 幂等；Close 返回之后发起的每个请求都会失败。
type Client struct {
	cfg    Config
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tailBuffer

	writeMu sync.Mutex
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan rpcOutcome

	done     chan struct{}
	doneOnce sync.Once

	stateMu      sync.Mutex
	closedErr    error
	initialized  bool
	shutdownSent bool
	exitSent     bool

	processDone chan struct{}
	processMu   sync.Mutex
	processErr  error

	readLoopDone chan struct{}
	stopWatch    func() bool
	closeOnce    sync.Once
	closeErr     error

	diagMu      sync.Mutex
	diagnostics map[string][]Diagnostic
	diagWaiters map[string][]chan struct{}

	docMu sync.Mutex
	docs  map[string]docState
}

// Start 启动 cfg 描述的语言服务器并运行协议泵。调用方随后必须调用 Initialize（除非只是
// 需要进程）并必须 Close 客户端。
//
// 取消 ctx 会终止服务端进程并让挂起请求失败；它是进程生命周期上下文，不是单请求截止
// 时间（每个请求有自己的上下文）。
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}

	command := exec.Command(cfg.Command, cfg.Args...)
	command.Dir = cfg.Dir
	command.Env = append(os.Environ(), cfg.Env...)

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: open server stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("lsp: open server stdout: %w", err)
	}
	client := &Client{
		cfg:          cfg,
		cmd:          command,
		stdin:        stdin,
		stdout:       stdout,
		stderr:       newTailBuffer(stderrTailBytes),
		pending:      make(map[int64]chan rpcOutcome),
		done:         make(chan struct{}),
		processDone:  make(chan struct{}),
		readLoopDone: make(chan struct{}),
		diagnostics:  make(map[string][]Diagnostic),
		diagWaiters:  make(map[string][]chan struct{}),
		docs:         make(map[string]docState),
	}
	command.Stderr = client.stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("lsp: start language server %q: %w", cfg.Command, err)
	}
	client.stopWatch = context.AfterFunc(ctx, func() {
		client.terminate(ctx.Err())
	})
	go client.waitProcess()
	go client.readLoop()
	return client, nil
}

// Command 返回配置的语言服务器可执行文件（用于诊断）。
func (c *Client) Command() string {
	if c == nil {
		return ""
	}
	return c.cfg.Command
}

// RootURI 返回 initialize 中发送的工作区根 URI。
func (c *Client) RootURI() string {
	if c == nil {
		return ""
	}
	return c.cfg.RootURI
}

// Err 返回连接的终止错误：启动上下文被取消时为上下文原因；服务端消失时为读侧失败
// （附带服务端 stderr 尾部）。连接健康时为 nil。
func (c *Client) Err() error {
	if c == nil {
		return nil
	}
	if err := c.closedError(); err != nil {
		return err
	}
	return c.processError()
}

// --- 进程/生命周期管线 ------------------------------------------------------

func (c *Client) waitProcess() {
	err := c.cmd.Wait()
	c.processMu.Lock()
	c.processErr = err
	c.processMu.Unlock()
	close(c.processDone)
}

func (c *Client) processError() error {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	if c.processErr == nil {
		return nil
	}
	return fmt.Errorf("lsp: language server %q exited: %w", c.cfg.Command, c.processErr)
}

func (c *Client) processExited() bool {
	select {
	case <-c.processDone:
		return true
	default:
		return false
	}
}

func (c *Client) closedError() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.closedErr
}

// markClosed 记录第一个终止错误、唤醒所有等待者并拒绝新工作。可从任意 goroutine 安全
// 调用，且可重复调用。
func (c *Client) markClosed(err error) {
	c.doneOnce.Do(func() {
		c.stateMu.Lock()
		if c.closedErr == nil {
			c.closedErr = err
		}
		terminal := c.closedErr
		c.stateMu.Unlock()
		close(c.done)
		c.failPending(terminal)
	})
}

// terminate 是启动上下文取消路径：先让所有请求失败，再强杀子进程，避免留下测试或服务端
// goroutine。
func (c *Client) terminate(cause error) {
	if cause == nil {
		cause = context.Canceled
	}
	c.markClosed(fmt.Errorf("lsp: client context ended: %w", cause))
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.stdin.Close()
}

// --- 请求/通知管线 ----------------------------------------------------------

func (c *Client) registerPending(id int64) chan rpcOutcome {
	outcome := make(chan rpcOutcome, 1)
	c.pendingMu.Lock()
	c.pending[id] = outcome
	c.pendingMu.Unlock()
	return outcome
}

func (c *Client) removePending(id int64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan rpcOutcome)
	c.pendingMu.Unlock()
	for _, outcome := range pending {
		outcome <- rpcOutcome{err: err}
	}
}

func (c *Client) write(message wireMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeMessage(c.stdin, message)
}

// request 发送一个 JSON-RPC 请求并等待其响应。
func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := c.closedError(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paramsRaw, err := encodeParams(params)
	if err != nil {
		return nil, err
	}
	id := c.nextID.Add(1)
	outcome := c.registerPending(id)
	if err := c.write(wireMessage{JSONRPC: "2.0", ID: rawID(id), Method: method, Params: paramsRaw}); err != nil {
		c.removePending(id)
		return nil, fmt.Errorf("lsp: send %s: %w", method, err)
	}

	waitCtx := ctx
	if c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}
	select {
	case result := <-outcome:
		if result.err != nil {
			return nil, result.err
		}
		return result.result, nil
	case <-waitCtx.Done():
		c.removePending(id)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("lsp: %s timed out after %s: %w", method, c.cfg.Timeout, context.DeadlineExceeded)
	case <-c.done:
		c.removePending(id)
		if err := c.closedError(); err != nil {
			return nil, err
		}
		return nil, ErrClosed
	}
}

// notify 发送一个 JSON-RPC 通知（不期待响应）。
func (c *Client) notify(ctx context.Context, method string, params any) error {
	if err := c.closedError(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	paramsRaw, err := encodeParams(params)
	if err != nil {
		return err
	}
	if err := c.write(wireMessage{JSONRPC: "2.0", Method: method, Params: paramsRaw}); err != nil {
		return fmt.Errorf("lsp: send %s: %w", method, err)
	}
	return nil
}

// --- 读循环 -----------------------------------------------------------------

func (c *Client) readLoop() {
	defer close(c.readLoopDone)
	reader := bufio.NewReader(c.stdout)
	for {
		message, err := readMessage(reader)
		if err != nil {
			if c.closedError() == nil {
				c.markClosed(c.connectionError(err))
			}
			return
		}
		c.dispatch(message)
	}
}

func (c *Client) connectionError(cause error) error {
	base := fmt.Errorf("lsp: language server %q connection lost: %w", c.cfg.Command, cause)
	if detail := c.stderr.String(); detail != "" {
		return fmt.Errorf("%w; server stderr: %s", base, strings.Join(strings.Fields(detail), " "))
	}
	return base
}

func (c *Client) dispatch(message wireMessage) {
	switch {
	case message.Method != "" && len(message.ID) > 0:
		c.answerServerRequest(message)
	case message.Method != "":
		c.handleNotification(message.Method, message.Params)
	case len(message.ID) > 0:
		c.handleResponse(message)
	}
}

func (c *Client) handleResponse(message wireMessage) {
	id, err := parseID(message.ID)
	if err != nil {
		return
	}
	c.pendingMu.Lock()
	outcome, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
	if !ok {
		return
	}
	settled := rpcOutcome{result: message.Result}
	if message.Error != nil {
		settled.err = message.Error
	}
	outcome <- settled
}

// answerServerRequest 应答服务端发起的请求。客户端不声明动态能力，因此未知请求会得到
// JSON-RPC "method not found" 回复（协议要求），而服务端常见的少数启动请求以空结果应答。
func (c *Client) answerServerRequest(message wireMessage) {
	reply := wireMessage{JSONRPC: "2.0", ID: message.ID}
	switch message.Method {
	case "workspace/configuration":
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(message.Params, &params)
		count := len(params.Items)
		if count == 0 {
			count = 1
		}
		values := make([]any, count)
		data, err := json.Marshal(values)
		if err != nil {
			reply.Error = &wireError{Code: -32603, Message: "encode configuration result"}
		} else {
			reply.Result = data
		}
	case "client/registerCapability", "client/unregisterCapability",
		"window/workDoneProgress/create", "workspace/workspaceFolders":
		reply.Result = json.RawMessage("null")
	default:
		reply.Error = &wireError{Code: -32601, Message: "client does not support " + message.Method}
	}
	_ = c.write(reply)
}

func (c *Client) handleNotification(method string, params json.RawMessage) {
	if method != "textDocument/publishDiagnostics" {
		return
	}
	published, err := parseDiagnostics(params)
	if err != nil || published.URI == "" {
		return
	}
	c.diagMu.Lock()
	c.diagnostics[published.URI] = slices.Clone(published.Diagnostics)
	waiters := c.diagWaiters[published.URI]
	delete(c.diagWaiters, published.URI)
	c.diagMu.Unlock()
	for _, waiter := range waiters {
		close(waiter)
	}
}

// --- LSP 操作 ---------------------------------------------------------------

// Initialize 执行 initialize 请求/响应握手并发送 initialized 通知。每个 client 幂等。
func (c *Client) Initialize(ctx context.Context) error {
	c.stateMu.Lock()
	alreadyInitialized := c.initialized
	c.stateMu.Unlock()
	if alreadyInitialized {
		return nil
	}
	params := map[string]any{
		"processId": nil,
		"clientInfo": map[string]any{
			"name": clientName,
		},
		"rootUri":      c.cfg.RootURI,
		"capabilities": clientCapabilities(),
	}
	raw, err := c.request(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("lsp: initialize %q: %w", c.cfg.Command, err)
	}
	if !isJSONNull(raw) {
		var result struct {
			Capabilities map[string]any `json:"capabilities"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("lsp: decode initialize result: %w", err)
		}
	}
	c.stateMu.Lock()
	c.initialized = true
	c.stateMu.Unlock()
	if err := c.notify(ctx, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("lsp: initialized notification: %w", err)
	}
	return nil
}

// DidOpen 把一个文档同步到服务端：首次 didOpen，之后全文 didChange（相同文本是 no-op）。
// uri 必须是 file:// URI（见 PathToURI）。
func (c *Client) DidOpen(ctx context.Context, uri, text string) error {
	if strings.TrimSpace(uri) == "" {
		return errors.New("lsp: document URI is required")
	}
	c.docMu.Lock()
	state, opened := c.docs[uri]
	if opened && state.text == text {
		c.docMu.Unlock()
		return nil
	}
	var method string
	var params any
	if opened {
		state.text = text
		state.version++
		c.docs[uri] = state
		method = "textDocument/didChange"
		params = map[string]any{
			"textDocument": map[string]any{"uri": uri, "version": state.version},
			"contentChanges": []any{
				map[string]any{"text": text},
			},
		}
	} else {
		state = docState{text: text, version: 1}
		c.docs[uri] = state
		method = "textDocument/didOpen"
		params = map[string]any{
			"textDocument": textDocumentItem{
				URI:        uri,
				LanguageID: c.cfg.LanguageID,
				Version:    state.version,
				Text:       text,
			},
		}
	}
	c.docMu.Unlock()
	return c.notify(ctx, method, params)
}

// Hover 在一个 LSP 位置（0 基行 + UTF-16 code unit 列）请求 hover 信息。服务端没有时返回
// (nil, nil)。
func (c *Client) Hover(ctx context.Context, uri string, line, character int) (*HoverResult, error) {
	if err := validateQuery(uri, line, character); err != nil {
		return nil, err
	}
	raw, err := c.request(ctx, "textDocument/hover", textDocumentPositionParams{
		TextDocument: textDocumentIdentifier{URI: uri},
		Position:     Position{Line: line, Character: character},
	})
	if err != nil {
		return nil, fmt.Errorf("lsp: hover: %w", err)
	}
	return parseHover(raw)
}

// Definition 解析一个 LSP 位置（0 基行 + UTF-16 code unit 列）的定义。它把 Location、
// Location[] 与 LocationLink 结果统一为一个列表；空列表表示“没有定义”。
func (c *Client) Definition(ctx context.Context, uri string, line, character int) ([]Location, error) {
	if err := validateQuery(uri, line, character); err != nil {
		return nil, err
	}
	raw, err := c.request(ctx, "textDocument/definition", textDocumentPositionParams{
		TextDocument: textDocumentIdentifier{URI: uri},
		Position:     Position{Line: line, Character: character},
	})
	if err != nil {
		return nil, fmt.Errorf("lsp: definition: %w", err)
	}
	return parseLocations(raw)
}

// DocumentSymbols 返回一个文档的符号树。层级 DocumentSymbol 与扁平 SymbolInformation
// 结果都规范化为 DocumentSymbol 值。
func (c *Client) DocumentSymbols(ctx context.Context, uri string) ([]DocumentSymbol, error) {
	if strings.TrimSpace(uri) == "" {
		return nil, errors.New("lsp: document URI is required")
	}
	raw, err := c.request(ctx, "textDocument/documentSymbol", documentSymbolParams{
		TextDocument: textDocumentIdentifier{URI: uri},
	})
	if err != nil {
		return nil, fmt.Errorf("lsp: document symbols: %w", err)
	}
	return parseSymbols(raw)
}

// Diagnostics 返回服务端为 uri 发布的最新诊断。若尚未发布，会等待至多 DiagnosticsWait
// 等待下一个通知，然后返回已知内容（可能为空）。发布是异步且可选的，因此空结果不是错误。
func (c *Client) Diagnostics(ctx context.Context, uri string) ([]Diagnostic, error) {
	if strings.TrimSpace(uri) == "" {
		return nil, errors.New("lsp: document URI is required")
	}
	if diagnostics, ok := c.latestDiagnostics(uri); ok {
		return diagnostics, nil
	}
	waiter := make(chan struct{})
	c.diagMu.Lock()
	if diagnostics, ok := c.diagnostics[uri]; ok {
		c.diagMu.Unlock()
		return slices.Clone(diagnostics), nil
	}
	c.diagWaiters[uri] = append(c.diagWaiters[uri], waiter)
	c.diagMu.Unlock()
	defer c.removeDiagnosticsWaiter(uri, waiter)

	timer := time.NewTimer(DiagnosticsWait)
	defer timer.Stop()
	select {
	case <-waiter:
		diagnostics, _ := c.latestDiagnostics(uri)
		return diagnostics, nil
	case <-timer.C:
		diagnostics, _ := c.latestDiagnostics(uri)
		return diagnostics, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		if err := c.closedError(); err != nil {
			return nil, err
		}
		return nil, ErrClosed
	}
}

func (c *Client) latestDiagnostics(uri string) ([]Diagnostic, bool) {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	diagnostics, ok := c.diagnostics[uri]
	if !ok {
		return nil, false
	}
	return slices.Clone(diagnostics), true
}

func (c *Client) removeDiagnosticsWaiter(uri string, waiter chan struct{}) {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	waiters := c.diagWaiters[uri]
	for index, candidate := range waiters {
		if candidate == waiter {
			waiters = slices.Delete(waiters, index, index+1)
			c.diagWaiters[uri] = waiters
			break
		}
	}
	if len(c.diagWaiters[uri]) == 0 {
		delete(c.diagWaiters, uri)
	}
}

// Shutdown 请求服务端关闭。幂等；服务端已消失时返回 nil。
func (c *Client) Shutdown(ctx context.Context) error {
	c.stateMu.Lock()
	done := c.shutdownSent || c.closedErr != nil
	c.stateMu.Unlock()
	if done {
		return nil
	}
	if _, err := c.request(ctx, "shutdown", nil); err != nil {
		if c.closedError() != nil {
			return nil
		}
		return fmt.Errorf("lsp: shutdown: %w", err)
	}
	c.stateMu.Lock()
	c.shutdownSent = true
	c.stateMu.Unlock()
	return nil
}

// Exit 发送 exit 通知。幂等；服务端已消失时返回 nil。
func (c *Client) Exit() error {
	c.stateMu.Lock()
	done := c.exitSent || c.closedErr != nil
	c.stateMu.Unlock()
	if done {
		return nil
	}
	if err := c.notify(context.Background(), "exit", nil); err != nil {
		if c.closedError() != nil {
			return nil
		}
		return err
	}
	c.stateMu.Lock()
	c.exitSent = true
	c.stateMu.Unlock()
	return nil
}

// Close 拆除连接：有界优雅 shutdown/exit 尝试，然后等待子进程，必要时强杀。幂等，且在
// 崩溃后仍可安全调用。只有服务端进程无法停止时才返回错误。
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closeErr = c.teardown()
	})
	return c.closeErr
}

func (c *Client) teardown() error {
	if c.stopWatch != nil {
		c.stopWatch()
	}
	if !c.processExited() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), closeShutdownTimeout)
		_ = c.Shutdown(shutdownCtx)
		cancel()
		_ = c.Exit()
	}
	_ = c.stdin.Close()
	var failure error
	select {
	case <-c.processDone:
	case <-time.After(exitGrace):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		select {
		case <-c.processDone:
		case <-time.After(exitGrace):
			failure = fmt.Errorf("lsp: language server %q did not exit", c.cfg.Command)
		}
	}
	_ = c.stdout.Close()
	select {
	case <-c.readLoopDone:
	case <-time.After(exitGrace):
		if failure == nil {
			failure = fmt.Errorf("lsp: language server %q protocol reader did not stop", c.cfg.Command)
		}
	}
	c.markClosed(ErrClosed)
	return failure
}

// --- 辅助函数 ---------------------------------------------------------------

// validateQuery 校验一次位置查询的 URI 与 LSP 原生位置。
func validateQuery(uri string, line, character int) error {
	if strings.TrimSpace(uri) == "" {
		return errors.New("lsp: document URI is required")
	}
	if line < 0 {
		return fmt.Errorf("lsp: line %d is invalid: LSP lines are zero-based and non-negative", line)
	}
	if character < 0 {
		return fmt.Errorf("lsp: character %d is invalid: LSP characters are zero-based and non-negative", character)
	}
	return nil
}

// parseID 解析 JSON-RPC 响应 id。只有本客户端发送的数字 id 能匹配挂起请求；其他形式
// 忽略。
func parseID(raw json.RawMessage) (int64, error) {
	text := strings.TrimSpace(string(raw))
	var id int64
	if err := json.Unmarshal([]byte(text), &id); err != nil {
		return 0, err
	}
	return id, nil
}

// clientCapabilities 是客户端声明的（刻意很小的）能力集：全文文档同步加四个只读操作。
func clientCapabilities() map[string]any {
	return map[string]any{
		"textDocument": map[string]any{
			"synchronization": map[string]any{
				"dynamicRegistration": false,
				"willSave":            false,
				"willSaveWaitUntil":   false,
				"didSave":             false,
			},
			"hover": map[string]any{
				"dynamicRegistration": false,
				"contentFormat":       []string{"markdown", "plaintext"},
			},
			"definition": map[string]any{
				"dynamicRegistration": false,
				"linkSupport":         true,
			},
			"documentSymbol": map[string]any{
				"dynamicRegistration":               false,
				"hierarchicalDocumentSymbolSupport": true,
				"symbolKind":                        map[string]any{"valueSet": symbolKindValueSet()},
			},
			"publishDiagnostics": map[string]any{
				"relatedInformation": false,
				"versionSupport":     false,
			},
		},
		"workspace": map[string]any{
			"workspaceFolders": false,
		},
	}
}

// symbolKindValueSet 列出客户端理解的 SymbolKind 值（1-26）。
func symbolKindValueSet() []int {
	values := make([]int, 0, 26)
	for kind := 1; kind <= 26; kind++ {
		values = append(values, kind)
	}
	return values
}

// tailBuffer 是有界 io.Writer，只保留最后 max 字节。
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, data...)
	if len(b.buf) > b.max {
		b.buf = slices.Clone(b.buf[len(b.buf)-b.max:])
	}
	return len(data), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
