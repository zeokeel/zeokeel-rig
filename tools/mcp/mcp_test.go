package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// helperConfig builds the config that re-executes the test binary as a fake MCP
// server in the given scenario.
func helperConfig(scenario string) Config {
	return Config{
		Command: os.Args[0],
		Env:     append(os.Environ(), helperEnv+"=1", scenarioEnv+"="+scenario),
		Timeout: 5 * time.Second,
	}
}

// startClient starts one fake-server client and registers its cleanup.
func startClient(t *testing.T, scenario string, tune func(*Config)) *Client {
	t.Helper()
	cfg := helperConfig(scenario)
	if tune != nil {
		tune(&cfg)
	}
	client, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start(%s): %v", scenario, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestStartHandshake verifies the initialize/initialized handshake, negotiated
// identity and ping. The fake server rejects tools/list unless it received the
// notifications/initialized notification, so the follow-up list proves the
// notification was sent after initialize.
func TestStartHandshake(t *testing.T) {
	client := startClient(t, "default", nil)
	if got := client.ServerName(); got != helperServer {
		t.Fatalf("ServerName() = %q, want %q", got, helperServer)
	}
	if got := client.ServerVersion(); got != helperVersion {
		t.Fatalf("ServerVersion() = %q, want %q", got, helperVersion)
	}
	if got := client.ProtocolVersion(); got != SupportedProtocolVersions[0] {
		t.Fatalf("ProtocolVersion() = %q, want %q", got, SupportedProtocolVersions[0])
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools after handshake: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("ListTools returned no tools")
	}
}

// TestServerNameOverride verifies Config.ServerName wins over serverInfo.name.
func TestServerNameOverride(t *testing.T) {
	client := startClient(t, "default", func(cfg *Config) { cfg.ServerName = "alias" })
	if got := client.ServerName(); got != "alias" {
		t.Fatalf("ServerName() = %q, want alias", got)
	}
}

// TestStartValidation covers the constructor's failure paths.
func TestStartValidation(t *testing.T) {
	t.Run("empty command", func(t *testing.T) {
		if _, err := Start(context.Background(), Config{}); err == nil {
			t.Fatal("Start accepted an empty command")
		}
	})
	t.Run("missing executable", func(t *testing.T) {
		_, err := Start(context.Background(), Config{Command: "rig-mcp-no-such-command-xyz"})
		if err == nil || !strings.Contains(err.Error(), "start server") {
			t.Fatalf("Start error = %v, want start-server failure", err)
		}
	})
	t.Run("initialize missing serverInfo", func(t *testing.T) {
		_, err := Start(context.Background(), helperConfig("badinit"))
		if err == nil || !strings.Contains(err.Error(), "serverInfo") {
			t.Fatalf("Start error = %v, want missing serverInfo", err)
		}
	})
	t.Run("unsupported protocol version", func(t *testing.T) {
		_, err := Start(context.Background(), helperConfig("unsupported"))
		if err == nil || !strings.Contains(err.Error(), "protocol version") {
			t.Fatalf("Start error = %v, want unsupported protocol version", err)
		}
	})
}

// TestListToolsPagination verifies cursor pagination and duplicate suppression.
func TestListToolsPagination(t *testing.T) {
	client := startClient(t, "pagination", nil)
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	want := []string{"echo", "fail", "picture"} // page 2 repeats echo; it must be deduplicated
	if len(names) != len(want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tools = %v, want %v", names, want)
		}
	}
}

// TestCallTool verifies text results, tool-level isError and JSON-RPC errors.
func TestCallTool(t *testing.T) {
	client := startClient(t, "default", nil)
	ctx := context.Background()

	text, err := client.CallTool(ctx, "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("CallTool(echo): %v", err)
	}
	if text.IsError || len(text.Content) != 1 || text.Content[0].Type != "text" || text.Content[0].Text != "echo: hi" {
		t.Fatalf("echo result = %+v, want one text content %q", text, "echo: hi")
	}

	failed, err := client.CallTool(ctx, "fail", nil)
	if err != nil {
		t.Fatalf("CallTool(fail): %v", err)
	}
	if !failed.IsError || len(failed.Content) != 1 || failed.Content[0].Text != "tool failed" {
		t.Fatalf("fail result = %+v, want IsError with %q", failed, "tool failed")
	}

	_, err = client.CallTool(ctx, "rpc_error", nil)
	if err == nil {
		t.Fatal("CallTool(rpc_error) returned nil error")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v (%T), want *RPCError", err, err)
	}
	if rpcErr.Code != -32000 || rpcErr.Message != "tool exploded" {
		t.Fatalf("RPCError = %+v, want code -32000 message 'tool exploded'", rpcErr)
	}
	if !strings.Contains(err.Error(), "data:") || !strings.Contains(err.Error(), "test") {
		t.Fatalf("RPCError text = %q, want data summary", err)
	}
}

// TestCallToolInvalidArguments verifies argument validation happens before I/O.
func TestCallToolInvalidArguments(t *testing.T) {
	client := startClient(t, "default", nil)
	ctx := context.Background()
	for _, arguments := range []string{`{"text":`, `[1,2]`, `"text"`} {
		if _, err := client.CallTool(ctx, "echo", json.RawMessage(arguments)); err == nil {
			t.Fatalf("CallTool accepted invalid arguments %q", arguments)
		}
	}
	// No arguments at all is legal (the server decides whether it is useful).
	if _, err := client.CallTool(ctx, "echo", nil); err != nil {
		t.Fatalf("CallTool with nil arguments: %v", err)
	}
}

// TestConcurrentCalls verifies parallel calls are matched to the right ids.
func TestConcurrentCalls(t *testing.T) {
	client := startClient(t, "default", nil)
	const calls = 8
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			result, err := client.CallTool(context.Background(), "echo",
				json.RawMessage(fmt.Sprintf(`{"text":"%d"}`, index)))
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", index, err)
				return
			}
			want := fmt.Sprintf("echo: %d", index)
			if len(result.Content) != 1 || result.Content[0].Text != want {
				errs <- fmt.Errorf("call %d result = %+v, want %q", index, result, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestContextCancel verifies caller cancellation unblocks a pending call.
func TestContextCancel(t *testing.T) {
	client := startClient(t, "slow", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client.CallTool(ctx, "slow", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CallTool error = %v, want context.DeadlineExceeded", err)
	}
}

// TestClientTimeout verifies Config.Timeout bounds one request even when the
// caller context has no deadline.
func TestClientTimeout(t *testing.T) {
	client := startClient(t, "slow", func(cfg *Config) { cfg.Timeout = 100 * time.Millisecond })
	if _, err := client.CallTool(context.Background(), "slow", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CallTool error = %v, want context.DeadlineExceeded", err)
	}
}

// TestProcessExit verifies an unexpected server exit is reported clearly and
// that Close stays safe and idempotent afterwards.
func TestProcessExit(t *testing.T) {
	client := startClient(t, "default", nil)
	_, err := client.CallTool(context.Background(), "crash", nil)
	if err == nil {
		t.Fatal("CallTool(crash) returned nil error")
	}
	if !strings.Contains(err.Error(), "server process exited") {
		t.Fatalf("CallTool error = %v, want a process-exit error", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close after crash: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := client.ListTools(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("ListTools after Close = %v, want ErrClosed", err)
	}
}

// TestCloseIdempotent verifies Close is idempotent and later calls fail with
// ErrClosed instead of hanging or panicking.
func TestCloseIdempotent(t *testing.T) {
	client := startClient(t, "default", nil)
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := client.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	if _, err := client.CallTool(context.Background(), "echo", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("CallTool after Close = %v, want ErrClosed", err)
	}
	if _, err := client.ListTools(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("ListTools after Close = %v, want ErrClosed", err)
	}
	if err := client.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ping after Close = %v, want ErrClosed", err)
	}
	if tools := Tools(client, ""); tools != nil {
		t.Fatalf("Tools after Close = %v, want nil", tools)
	}
}

// TestToolsMapping verifies the MCP -> contracts/rig.Tool adapter: names,
// descriptions, schema passthrough, content rendering and the client binding.
func TestToolsMapping(t *testing.T) {
	client := startClient(t, "default", nil)
	ctx := context.Background()
	infos, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tools := Tools(client, "")
	if len(tools) != len(infos) {
		t.Fatalf("Tools returned %d tools, want %d", len(tools), len(infos))
	}
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}

	echo, ok := byName[DefaultPrefix+helperServer+"__echo"]
	if !ok {
		t.Fatalf("missing %q in %v", DefaultPrefix+helperServer+"__echo", toolNames(tools))
	}
	if got := echo.Description(); got != "Echo text back" {
		t.Fatalf("echo description = %q", got)
	}
	schema := echo.Schema()
	if schema["type"] != "object" {
		t.Fatalf("echo schema = %v, want an object schema", schema)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("echo schema properties = %v", schema["properties"])
	}
	if _, ok := properties["text"]; !ok {
		t.Fatalf("echo schema properties = %v, want text", properties)
	}

	result, err := echo.Invoke(ctx, rig.ToolCall{ID: "1", Name: echo.Name(), Arguments: `{"text":"hi"}`})
	if err != nil {
		t.Fatalf("Invoke echo: %v", err)
	}
	if result.IsError || result.Content != "echo: hi" {
		t.Fatalf("echo result = %+v, want %q", result, "echo: hi")
	}

	fail := byName[DefaultPrefix+helperServer+"__fail"]
	failed, err := fail.Invoke(ctx, rig.ToolCall{Arguments: `{}`})
	if err != nil {
		t.Fatalf("Invoke fail: %v", err)
	}
	if !failed.IsError || failed.Content != "tool failed" {
		t.Fatalf("fail result = %+v, want IsError", failed)
	}

	picture := byName[DefaultPrefix+helperServer+"__picture"]
	image, err := picture.Invoke(ctx, rig.ToolCall{Arguments: `{}`})
	if err != nil {
		t.Fatalf("Invoke picture: %v", err)
	}
	if !strings.Contains(image.Content, "here") || !strings.Contains(image.Content, "[image: image/png]") {
		t.Fatalf("picture result = %q, want text plus image placeholder", image.Content)
	}

	structured := byName[DefaultPrefix+helperServer+"__structured"]
	structuredResult, err := structured.Invoke(ctx, rig.ToolCall{Arguments: `{}`})
	if err != nil {
		t.Fatalf("Invoke structured: %v", err)
	}
	if !strings.Contains(structuredResult.Content, `"answer":42`) {
		t.Fatalf("structured result = %q, want JSON fallback", structuredResult.Content)
	}

	// Custom prefix and server alias are applied by NewTools.
	prefixed := Tools(client, "x__")
	if _, ok := findTool(prefixed, "x__"+helperServer+"__echo"); !ok {
		t.Fatalf("custom prefix missing in %v", toolNames(prefixed))
	}
	aliased := NewTools(client, "alias server!", infos, "")
	if _, ok := findTool(aliased, DefaultPrefix+"alias_server__echo"); !ok {
		t.Fatalf("aliased tool missing in %v", toolNames(aliased))
	}
	fallback := NewTools(client, "srv", []ToolInfo{{Name: "misc"}}, "")
	if len(fallback) != 1 || fallback[0].Description() == "" {
		t.Fatalf("fallback description is empty: %+v", fallback)
	}
	if got := fallback[0].Name(); got != "mcp__srv__misc" {
		t.Fatalf("fallback name = %q", got)
	}
}

// TestToolsNilClient verifies the adapter handles a nil client gracefully.
func TestToolsNilClient(t *testing.T) {
	if tools := Tools(nil, ""); tools != nil {
		t.Fatalf("Tools(nil) = %v, want nil", tools)
	}
	tools := NewTools(nil, "srv", []ToolInfo{{Name: "x"}}, "")
	if len(tools) != 1 {
		t.Fatalf("NewTools(nil) = %v", tools)
	}
	if _, err := tools[0].Invoke(context.Background(), rig.ToolCall{Arguments: "{}"}); err == nil {
		t.Fatal("Invoke on a client-less tool returned nil error")
	}
}

// toolNames lists adapted tool names for failure messages.
func toolNames(tools []rig.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

// findTool finds one adapted tool by name.
func findTool(tools []rig.Tool, name string) (rig.Tool, bool) {
	for _, tool := range tools {
		if tool.Name() == name {
			return tool, true
		}
	}
	return nil, false
}

// TestCloseWhileCallInFlight verifies Close fails a pending call instead of
// hanging, and that the process is still reaped.
func TestCloseWhileCallInFlight(t *testing.T) {
	client := startClient(t, "slow", nil)
	done := make(chan error, 1)
	go func() {
		_, err := client.CallTool(context.Background(), "slow", nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the request reach the server
	start := time.Now()
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > closeGrace+time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("in-flight CallTool returned nil after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight CallTool did not return after Close")
	}
}

// TestMalformedAndEmptyServerMessages verifies non-JSON lines are ignored and a
// response carrying neither result nor error is reported as a protocol error.
func TestMalformedAndEmptyServerMessages(t *testing.T) {
	t.Run("malformed line ignored", func(t *testing.T) {
		client := startClient(t, "garbage", nil)
		if err := client.Ping(context.Background()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})
	t.Run("empty response", func(t *testing.T) {
		client := startClient(t, "emptyresult", nil)
		err := client.Ping(context.Background())
		if err == nil || !strings.Contains(err.Error(), "neither result nor error") {
			t.Fatalf("Ping error = %v, want protocol error", err)
		}
	})
}
