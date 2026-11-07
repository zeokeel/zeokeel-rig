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

// DefaultTimeout bounds one request (initialize, hover, definition, document
// symbols, shutdown): when the caller's context has no shorter deadline, the
// request fails after this long.
const DefaultTimeout = 15 * time.Second

// DiagnosticsWait is how long Diagnostics waits for a first
// textDocument/publishDiagnostics notification before reporting that the server
// published none. Diagnostic publishing is asynchronous and optional, so this
// is a best-effort wait, not a guarantee.
const DiagnosticsWait = 500 * time.Millisecond

const (
	// closeShutdownTimeout bounds the graceful shutdown request Close makes.
	closeShutdownTimeout = 2 * time.Second
	// exitGrace is how long Close waits for the child to exit before killing it.
	exitGrace = time.Second
	// stderrTailBytes is how much of the server's stderr the client keeps for
	// error messages (server stderr is diagnostic data, never protocol traffic).
	stderrTailBytes = 4 << 10
	// clientName identifies this client in the initialize handshake.
	clientName = "zeokeel-rig"
)

// ErrClosed is returned by requests made after the client was closed.
var ErrClosed = errors.New("lsp: client is closed")

// Config describes one language server process.
type Config struct {
	// Command is the language server executable. Required.
	Command string
	// Args are passed to Command verbatim.
	Args []string
	// Env entries ("KEY=VALUE") are appended to the client process's
	// environment for the server. Optional.
	Env []string
	// Dir is the server process's working directory. Optional.
	Dir string
	// RootURI is the workspace root sent in initialize. Optional: when empty
	// the file URI of Dir (or of the current directory) is used.
	RootURI string
	// LanguageID is the language id sent with didOpen ("go", "python", ...).
	// Optional: defaults to "plaintext".
	LanguageID string
	// Timeout bounds one request; zero means DefaultTimeout.
	Timeout time.Duration
}

// normalize validates the config and fills in defaults.
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

// rpcOutcome is one response delivered to a waiting request. err carries
// either the server's JSON-RPC error or the connection's terminal error (which
// keeps its error chain, so callers can errors.Is a cancelled context).
type rpcOutcome struct {
	result json.RawMessage
	err    error
}

// docState is the last text this client sent for one open document.
type docState struct {
	text    string
	version int
}

// Client is one running language server connection.
//
// It is safe for concurrent use: requests are multiplexed by ID, document state
// is mutex-guarded and diagnostics are collected from the single read
// goroutine. Close is idempotent; every request made after it returns fails.
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

// Start launches the language server described by cfg and starts the protocol
// pump. The caller must follow with Initialize (unless it only needs the
// process) and must Close the client.
//
// Cancelling ctx terminates the server process and fails pending requests; it
// is the process-lifetime context, not a per-request deadline (requests take
// their own contexts).
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

// Command returns the configured language server executable (for diagnostics).
func (c *Client) Command() string {
	if c == nil {
		return ""
	}
	return c.cfg.Command
}

// RootURI returns the workspace root URI sent in initialize.
func (c *Client) RootURI() string {
	if c == nil {
		return ""
	}
	return c.cfg.RootURI
}

// Err returns the connection's terminal error: the context cause when the
// start context was cancelled, or the read-side failure (with the server's
// stderr tail) when the server went away. It is nil while the connection is
// healthy.
func (c *Client) Err() error {
	if c == nil {
		return nil
	}
	if err := c.closedError(); err != nil {
		return err
	}
	return c.processError()
}

// --- process/lifecycle plumbing --------------------------------------------

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

// markClosed records the first terminal error, wakes every waiter and rejects
// new work. It is safe to call from any goroutine and more than once.
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

// terminate is the start-context cancellation path: fail everything, then kill
// the child so no test or server goroutine is left behind.
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

// --- request/notification plumbing -----------------------------------------

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

// request sends one JSON-RPC request and waits for its response.
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

// notify sends one JSON-RPC notification (no response is expected).
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

// --- read loop --------------------------------------------------------------

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

// answerServerRequest answers server-initiated requests. The client advertises
// no dynamic capabilities, so unknown requests get a JSON-RPC "method not
// found" reply as the protocol requires, while the few startup requests servers
// commonly send are answered with an empty result.
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

// --- LSP operations ---------------------------------------------------------

// Initialize performs the initialize request/response handshake and sends the
// initialized notification. It is idempotent per client.
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

// DidOpen syncs one document to the server: didOpen the first time, a full-text
// didChange afterwards (the same text is a no-op). uri must be a file:// URI
// (see PathToURI).
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

// Hover asks for hover information at one LSP position (zero-based line and
// UTF-16 code-unit character). It returns (nil, nil) when the server has none.
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

// Definition resolves the definition at one LSP position (zero-based line and
// UTF-16 code-unit character). It normalizes Location, Location[] and
// LocationLink results into one list; an empty list means "no definition".
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

// DocumentSymbols returns the symbol tree of one document. Hierarchical
// DocumentSymbol and flat SymbolInformation results both normalize to
// DocumentSymbol values.
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

// Diagnostics returns the newest diagnostics the server published for uri. If
// none were published yet it waits up to DiagnosticsWait for the next
// notification, then returns whatever is known (possibly nothing). Publishing is
// asynchronous and optional, so an empty result is not an error.
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

// Shutdown asks the server to shut down. It is idempotent and returns nil when
// the server is already gone.
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

// Exit sends the exit notification. It is idempotent and returns nil when the
// server is already gone.
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

// Close tears the connection down: a bounded graceful shutdown/exit attempt,
// then the child is waited for and, if necessary, killed. It is idempotent and
// safe to call after a crash. It returns an error only when the server process
// could not be stopped.
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

// --- helpers ----------------------------------------------------------------

// validateQuery validates one position query's URI and LSP-native position.
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

// parseID parses a JSON-RPC response id. Only the numeric ids this client sends
// can match a pending request; anything else is ignored.
func parseID(raw json.RawMessage) (int64, error) {
	text := strings.TrimSpace(string(raw))
	var id int64
	if err := json.Unmarshal([]byte(text), &id); err != nil {
		return 0, err
	}
	return id, nil
}

// clientCapabilities is the (deliberately small) capability set the client
// advertises: full-text document sync plus the four read-only operations.
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

// symbolKindValueSet lists the SymbolKind values the client understands (1-26).
func symbolKindValueSet() []int {
	values := make([]int, 0, 26)
	for kind := 1; kind <= 26; kind++ {
		values = append(values, kind)
	}
	return values
}

// tailBuffer is a bounded io.Writer that keeps the last max bytes written to it.
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
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.max:]...)
	}
	return len(data), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
