package host_test

// TDD tests for the optional LSP host lifecycle. The fake language server is
// the test binary re-executed with RIG_LSP_HOST_TEST_MODE set: TestMain runs it
// before any testing flags matter, so no external LSP binary is required.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/lsp"
	lspHost "github.com/zeokeel/zeokeel-rig/tools/lsp/host"
)

const (
	envFakeMode  = "RIG_LSP_HOST_TEST_MODE"
	envStartFile = "RIG_LSP_HOST_TEST_START_FILE"
	envCwdFile   = "RIG_LSP_HOST_TEST_CWD_FILE"
	envArgsFile  = "RIG_LSP_HOST_TEST_ARGS_FILE"
	envEnvKey    = "RIG_LSP_HOST_TEST_ENV_KEY"
	envEnvFile   = "RIG_LSP_HOST_TEST_ENV_FILE"
	envRootFile  = "RIG_LSP_HOST_TEST_ROOT_FILE"

	fakeModeNormal      = "normal"
	fakeModeCrashOnInit = "crash-on-init"
)

func TestMain(m *testing.M) {
	if mode := strings.TrimSpace(os.Getenv(envFakeMode)); mode != "" {
		runFakeServer(mode)
		return
	}
	os.Exit(m.Run())
}

type fakeMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type fakeResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

func runFakeServer(mode string) {
	recordFile(envStartFile, "start\n", true)
	recordFile(envCwdFile, mustGetwd()+"\n", false)
	recordFile(envArgsFile, mustJSONString(os.Args[1:])+"\n", false)
	if key := strings.TrimSpace(os.Getenv(envEnvKey)); key != "" {
		recordFile(envEnvFile, os.Getenv(key)+"\n", false)
	}
	if mode == fakeModeCrashOnInit {
		os.Exit(3)
	}
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	for {
		message, err := readFakeMessage(reader)
		if err != nil {
			return
		}
		switch message.Method {
		case "initialize":
			if params := rootURIFromParams(message.Params); params != "" {
				recordFile(envRootFile, params+"\n", false)
			}
			_ = writeFakeResponse(writer, message.ID, map[string]any{"capabilities": map[string]any{}})
		case "shutdown":
			_ = writeFakeResponse(writer, message.ID, nil)
		case "exit":
			return
		default:
			if len(message.ID) > 0 {
				_ = writeFakeResponse(writer, message.ID, nil)
			}
		}
	}
}

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

func mustJSONString(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func recordFile(envKey, content string, appendContent bool) {
	path := strings.TrimSpace(os.Getenv(envKey))
	if path == "" {
		return
	}
	flags := os.O_CREATE | os.O_WRONLY
	if appendContent {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return
	}
	_, _ = file.WriteString(content)
	_ = file.Close()
}

func rootURIFromParams(params json.RawMessage) string {
	var payload struct {
		RootURI string `json:"rootUri"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return ""
	}
	return payload.RootURI
}

func readFakeMessage(reader *bufio.Reader) (fakeMessage, error) {
	length := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fakeMessage{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			value := strings.TrimSpace(line[len("content-length:"):])
			parsed, parseErr := strconv.Atoi(value)
			if parseErr != nil || parsed < 0 {
				return fakeMessage{}, io.ErrUnexpectedEOF
			}
			length = parsed
		}
	}
	if length < 0 {
		return fakeMessage{}, io.ErrUnexpectedEOF
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return fakeMessage{}, err
	}
	var message fakeMessage
	if err := json.Unmarshal(body, &message); err != nil {
		return fakeMessage{}, err
	}
	return message, nil
}

func writeFakeResponse(writer *bufio.Writer, id json.RawMessage, result any) error {
	payload, err := json.Marshal(fakeResponse{JSONRPC: "2.0", ID: id, Result: result})
	if err != nil {
		return err
	}
	if _, err := writer.WriteString("Content-Length: " + strconv.Itoa(len(payload)) + "\r\n\r\n"); err != nil {
		return err
	}
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	return writer.Flush()
}

// captureLogger records host diagnostics without failing the test on them.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (l *captureLogger) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

func (l *captureLogger) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func readFileEventually(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("read %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func tempFile(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

func fakeConfig(t *testing.T, mode string, env ...string) (lspHost.Config, *captureLogger) {
	t.Helper()
	logger := &captureLogger{}
	config := lspHost.Config{
		Command: os.Args[0],
		Env:     append([]string{envFakeMode + "=" + mode}, env...),
		Logger:  logger.Logf,
	}
	return config, logger
}

func toolNames(tools []rig.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			names = append(names, "<nil>")
			continue
		}
		names = append(names, tool.Name())
	}
	return names
}

func wantLSPToolNames() []string {
	return []string{
		lsp.ToolHover,
		lsp.ToolDefinition,
		lsp.ToolReferences,
		lsp.ToolImplementation,
		lsp.ToolDiagnostics,
		lsp.ToolSymbols,
	}
}

func assertToolSet(t *testing.T, tools []rig.Tool) {
	t.Helper()
	names := toolNames(tools)
	want := wantLSPToolNames()
	if len(names) != len(want) {
		t.Fatalf("tools = %v, want %d LSP tools", names, len(want))
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			t.Fatalf("duplicate tool name %q in %v", name, names)
		}
		seen[name] = true
	}
	for _, name := range want {
		if !seen[name] {
			t.Fatalf("missing tool %q in %v", name, names)
		}
	}
}

func TestNewValidatesConfig(t *testing.T) {
	for _, config := range []lspHost.Config{{}, {Command: "   "}, {Command: "\t\n"}} {
		if host, err := lspHost.New(config); err == nil {
			_ = host.Close()
			t.Fatalf("New(%+v) accepted a missing command", config)
		}
	}
	host, err := lspHost.New(lspHost.Config{Command: "  go  ", Args: []string{"  ", ""}, Env: []string{" ", ""}})
	if err != nil {
		t.Fatalf("New(valid) = %v", err)
	}
	if host == nil {
		t.Fatal("New(valid) = nil Host")
	}
	if err := host.Close(); err != nil {
		t.Fatalf("Close before Start = %v", err)
	}
	if tools := host.Tools(); len(tools) != 0 {
		t.Fatalf("Tools before Start = %v", toolNames(tools))
	}
}

func TestStartFailureIsIsolated(t *testing.T) {
	logger := &captureLogger{}
	host, err := lspHost.New(lspHost.Config{
		Command: filepath.Join(t.TempDir(), "definitely-not-a-language-server"),
		Logger:  logger.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Start must never panic or abort host startup on an LSP error, including
	// a nil context.
	host.Start(context.TODO())
	if tools := host.Tools(); len(tools) != 0 {
		t.Fatalf("failed start exposed tools: %v", toolNames(tools))
	}
	if !logger.contains("start") {
		t.Fatalf("start failure was not logged: %q", logger.all())
	}
	host.Start(context.Background())
	if tools := host.Tools(); len(tools) != 0 {
		t.Fatalf("second failed start exposed tools: %v", toolNames(tools))
	}
	if err := host.Close(); err != nil {
		t.Fatalf("Close after failed start = %v", err)
	}
}

func TestInitializeFailureRegistersNoTools(t *testing.T) {
	config, logger := fakeConfig(t, fakeModeCrashOnInit, envStartFile+"="+tempFile(t, "starts"))
	host, err := lspHost.New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	host.Start(context.Background())
	if tools := host.Tools(); len(tools) != 0 {
		t.Fatalf("initialize failure exposed tools: %v", toolNames(tools))
	}
	if !logger.contains("initialize") {
		t.Fatalf("initialize failure was not logged: %q", logger.all())
	}
	if err := host.Close(); err != nil {
		t.Fatalf("Close after failed initialize = %v", err)
	}
}

func TestStartExposesUniqueToolsAndIsIdempotent(t *testing.T) {
	startFile := tempFile(t, "starts")
	config, _ := fakeConfig(t, fakeModeNormal, envStartFile+"="+startFile)
	host, err := lspHost.New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = host.Close() }()

	host.Start(context.Background())
	assertToolSet(t, host.Tools())
	first := host.Tools()
	if len(first) != 6 {
		t.Fatalf("first Tools = %v", toolNames(first))
	}
	// Mutating the returned slice must not corrupt the host's registry.
	first[0] = nil
	assertToolSet(t, host.Tools())

	// Starting an already-running host is a no-op: one server, one tool set.
	host.Start(context.Background())
	assertToolSet(t, host.Tools())
	if starts := strings.Count(readFileEventually(t, startFile), "start"); starts != 1 {
		t.Fatalf("server starts = %d, want 1 (idempotent Start)", starts)
	}
}

func TestCloseIsIdempotentAndClearsTools(t *testing.T) {
	config, _ := fakeConfig(t, fakeModeNormal)
	host, err := lspHost.New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	host.Start(context.Background())
	assertToolSet(t, host.Tools())
	for attempt := 1; attempt <= 3; attempt++ {
		if err := host.Close(); err != nil {
			t.Fatalf("Close #%d = %v", attempt, err)
		}
		if tools := host.Tools(); len(tools) != 0 {
			t.Fatalf("Tools after Close #%d = %v", attempt, toolNames(tools))
		}
	}
}

func TestRestartStartsAFreshServer(t *testing.T) {
	startFile := tempFile(t, "starts")
	config, _ := fakeConfig(t, fakeModeNormal, envStartFile+"="+startFile)
	host, err := lspHost.New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = host.Close() }()

	host.Start(context.Background())
	assertToolSet(t, host.Tools())
	if err := host.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if tools := host.Tools(); len(tools) != 0 {
		t.Fatalf("Tools after first Close = %v", toolNames(tools))
	}

	host.Start(context.Background())
	assertToolSet(t, host.Tools())
	if starts := strings.Count(readFileEventually(t, startFile), "start"); starts != 2 {
		t.Fatalf("server starts = %d, want 2 after restart", starts)
	}
}

func TestStartPassesDirEnvArgsAndRootURI(t *testing.T) {
	workDir := t.TempDir()
	startFile := tempFile(t, "starts")
	cwdFile := tempFile(t, "cwd")
	argsFile := tempFile(t, "args")
	envFile := tempFile(t, "env")
	rootFile := tempFile(t, "root")
	const envKey = "RIG_LSP_HOST_TEST_CUSTOM"
	const envValue = "custom-value"

	config := lspHost.Config{
		Command: "  " + os.Args[0] + "  ",
		Args:    []string{"  --alpha  ", "", "   ", "beta"},
		Env: []string{
			envFakeMode + "=" + fakeModeNormal,
			envStartFile + "=" + startFile,
			envCwdFile + "=" + cwdFile,
			envArgsFile + "=" + argsFile,
			envEnvKey + "=" + envKey,
			envEnvFile + "=" + envFile,
			envRootFile + "=" + rootFile,
			"  " + envKey + "=" + envValue + "  ",
		},
		Dir:     "  " + workDir + "  ",
		RootURI: "  file:///tmp/host-root  ",
		Logger:  (&captureLogger{}).Logf,
	}
	host, err := lspHost.New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = host.Close() }()
	host.Start(context.Background())
	assertToolSet(t, host.Tools())

	wantCwd, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", workDir, err)
	}
	if got := strings.TrimSpace(readFileEventually(t, cwdFile)); got != wantCwd {
		t.Fatalf("server cwd = %q, want %q", got, wantCwd)
	}
	if got := strings.TrimSpace(readFileEventually(t, envFile)); got != envValue {
		t.Fatalf("server env %s = %q, want %q", envKey, got, envValue)
	}
	var args []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(readFileEventually(t, argsFile))), &args); err != nil {
		t.Fatalf("decode server args: %v", err)
	}
	if len(args) != 2 || args[0] != "--alpha" || args[1] != "beta" {
		t.Fatalf("server args = %v, want [--alpha beta]", args)
	}
	if got := strings.TrimSpace(readFileEventually(t, rootFile)); got != "file:///tmp/host-root" {
		t.Fatalf("initialize rootUri = %q, want file:///tmp/host-root", got)
	}
}

func TestStartCanceledContextIsIsolated(t *testing.T) {
	config, logger := fakeConfig(t, fakeModeNormal)
	host, err := lspHost.New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	host.Start(ctx)
	if tools := host.Tools(); len(tools) != 0 {
		t.Fatalf("canceled start exposed tools: %v", toolNames(tools))
	}
	if !logger.contains("start") {
		t.Fatalf("canceled start was not logged: %q", logger.all())
	}
	if err := host.Close(); err != nil {
		t.Fatalf("Close after canceled start = %v", err)
	}
}
