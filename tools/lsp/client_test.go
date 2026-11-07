package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testDocumentURI  = "file:///tmp/fake-root/main.go"
	testDocumentText = "package main\n\nfunc main() {}\n"
)

// startTestClient starts the fake language server through the re-exec pattern.
// It returns the client and the path of the record file the server writes.
func startTestClient(t *testing.T, mode string) (*Client, string) {
	t.Helper()
	recordPath := filepath.Join(t.TempDir(), "record.jsonl")
	client, err := Start(context.Background(), Config{
		Command:    os.Args[0],
		Env:        []string{fakeModeEnv + "=" + mode, fakeRecordEnv + "=" + recordPath},
		RootURI:    "file:///tmp/fake-root",
		LanguageID: "go",
		Timeout:    3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start(%s): %v", mode, err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return client, recordPath
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustInitialize(t *testing.T, client *Client) {
	t.Helper()
	if err := client.Initialize(testContext(t)); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
}

func mustOpenDocument(t *testing.T, client *Client) {
	t.Helper()
	if err := client.DidOpen(testContext(t), testDocumentURI, testDocumentText); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
}

// fakeRecord is one message the fake server received.
type fakeRecord struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	HasID  bool            `json:"hasID"`
}

func readFakeRecords(t *testing.T, path string) []fakeRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The server records asynchronously; callers that need a specific
			// message use waitForRecord.
			return nil
		}
		t.Fatalf("read fake server record %q: %v", path, err)
	}
	records := make([]fakeRecord, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var record fakeRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode fake server record %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// waitForRecord polls the record file until the fake server has recorded a
// message with the given method (the client's notification write returns before
// the server reads it), then returns the whole record list.
func waitForRecord(t *testing.T, path, method string) []fakeRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		records := readFakeRecords(t, path)
		for _, record := range records {
			if record.Method == method {
				return records
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake server did not record %q within 2s; got %v", method, recordMethods(records))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func recordMethods(records []fakeRecord) []string {
	methods := make([]string, len(records))
	for index, record := range records {
		methods[index] = record.Method
	}
	return methods
}

func findRecord(t *testing.T, records []fakeRecord, method string) fakeRecord {
	t.Helper()
	for _, record := range records {
		if record.Method == method {
			return record
		}
	}
	t.Fatalf("fake server did not receive %q; got %v", method, recordMethods(records))
	return fakeRecord{}
}

// --- Start -----------------------------------------------------------------

func TestStartValidatesConfig(t *testing.T) {
	if _, err := Start(context.Background(), Config{}); err == nil {
		t.Fatal("Start(empty command) succeeded")
	}
	if _, err := Start(context.Background(), Config{Command: "/nonexistent/rig-lsp-server"}); err == nil {
		t.Fatal("Start(nonexistent command) succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Start(ctx, Config{Command: os.Args[0]}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start(cancelled ctx) = %v, want context.Canceled", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	client, _ := startTestClient(t, "happy")
	if client.RootURI() != "file:///tmp/fake-root" {
		t.Fatalf("RootURI() = %q", client.RootURI())
	}
	if client.Command() != os.Args[0] {
		t.Fatalf("Command() = %q, want %q", client.Command(), os.Args[0])
	}
	cfg, err := Config{Command: "  gopls  "}.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.Command != "gopls" || cfg.LanguageID != "plaintext" || cfg.Timeout != DefaultTimeout || cfg.RootURI == "" {
		t.Fatalf("normalized config = %+v, want defaults", cfg)
	}
}

// --- initialize handshake ---------------------------------------------------

func TestInitializeHandshake(t *testing.T) {
	client, recordPath := startTestClient(t, "happy")
	mustInitialize(t, client)

	records := waitForRecord(t, recordPath, "initialized")
	methods := recordMethods(records)
	if len(methods) < 2 || methods[0] != "initialize" || methods[1] != "initialized" {
		t.Fatalf("handshake methods = %v, want initialize then initialized", methods)
	}
	initialize := findRecord(t, records, "initialize")
	if !initialize.HasID {
		t.Fatal("initialize was not sent as a request (no id)")
	}
	var params struct {
		ProcessID    *int           `json:"processId"`
		RootURI      string         `json:"rootUri"`
		ClientInfo   map[string]any `json:"clientInfo"`
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(initialize.Params, &params); err != nil {
		t.Fatalf("decode initialize params: %v", err)
	}
	if params.ProcessID != nil {
		t.Fatalf("processId = %v, want null", *params.ProcessID)
	}
	if params.RootURI != "file:///tmp/fake-root" {
		t.Fatalf("rootUri = %q, want the configured root", params.RootURI)
	}
	if got := params.ClientInfo["name"]; got != clientName {
		t.Fatalf("clientInfo.name = %v, want %q", got, clientName)
	}
	textDocument, ok := params.Capabilities["textDocument"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities.textDocument missing: %v", params.Capabilities)
	}
	for _, capability := range []string{"hover", "definition", "documentSymbol", "publishDiagnostics", "synchronization"} {
		if _, ok := textDocument[capability]; !ok {
			t.Fatalf("capabilities.textDocument lacks %q", capability)
		}
	}

	// Initialize is idempotent: the handshake is not repeated.
	if err := client.Initialize(testContext(t)); err != nil {
		t.Fatalf("second Initialize: %v", err)
	}
	initializeCount := 0
	for _, method := range recordMethods(readFakeRecords(t, recordPath)) {
		if method == "initialize" {
			initializeCount++
		}
	}
	if initializeCount != 1 {
		t.Fatalf("initialize requests = %d, want 1", initializeCount)
	}
}

// --- hover ------------------------------------------------------------------

func TestHoverMarkdown(t *testing.T) {
	client, _ := startTestClient(t, "happy")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	hover, err := client.Hover(testContext(t), testDocumentURI, 0, 0)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if hover == nil {
		t.Fatal("Hover returned nil for a markdown response")
	}
	if hover.Kind != "markdown" || !strings.Contains(hover.Contents, "**fake hover**") {
		t.Fatalf("hover = %+v, want markdown contents", hover)
	}
	if hover.Range == nil {
		t.Fatal("hover.Range is nil, want the server range")
	}
	if formatted := FormatHover(hover); !strings.Contains(formatted, "markdown body") {
		t.Fatalf("FormatHover = %q", formatted)
	}
}

func TestHoverPlainText(t *testing.T) {
	client, _ := startTestClient(t, "hover-plain")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	hover, err := client.Hover(testContext(t), testDocumentURI, 0, 0)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if hover == nil || hover.Kind != "plaintext" || hover.Contents != "plain hover text" {
		t.Fatalf("hover = %+v, want plain hover text", hover)
	}
}

// --- definition -------------------------------------------------------------

func TestDefinitionSingleLocation(t *testing.T) {
	client, _ := startTestClient(t, "happy")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	locations, err := client.Definition(testContext(t), testDocumentURI, 0, 0)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locations) != 1 {
		t.Fatalf("locations = %+v, want one", locations)
	}
	location := locations[0]
	if location.URI != testDocumentURI {
		t.Fatalf("location URI = %q", location.URI)
	}
	if location.Range.Start.Line != 2 || location.Range.Start.Character != 4 {
		t.Fatalf("location range = %+v, want 2:4", location.Range)
	}
	formatted := FormatLocations(locations)
	if formatted != "/tmp/fake-root/main.go:3:5" {
		t.Fatalf("FormatLocations = %q, want /tmp/fake-root/main.go:3:5", formatted)
	}
}

func TestDefinitionMultipleAndLocationLink(t *testing.T) {
	client, _ := startTestClient(t, "definition-multi")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	locations, err := client.Definition(testContext(t), testDocumentURI, 0, 0)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locations) != 2 {
		t.Fatalf("locations = %+v, want two", locations)
	}
	if locations[1].URI != "file:///tmp/other/target.go" {
		t.Fatalf("location[1] URI = %q, want the LocationLink target", locations[1].URI)
	}
	// The LocationLink's targetSelectionRange wins over targetRange.
	if locations[1].Range.Start.Line != 9 || locations[1].Range.Start.Character != 1 {
		t.Fatalf("location[1] range = %+v, want the selection range at 9:1", locations[1].Range)
	}
	formatted := FormatLocations(locations)
	if !strings.Contains(formatted, "/tmp/other/target.go:10:2") {
		t.Fatalf("FormatLocations = %q, want the LocationLink target", formatted)
	}
}

func TestDefinitionNull(t *testing.T) {
	client, _ := startTestClient(t, "definition-null")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	locations, err := client.Definition(testContext(t), testDocumentURI, 0, 0)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locations) != 0 {
		t.Fatalf("locations = %+v, want none", locations)
	}
	if FormatLocations(locations) != "" {
		t.Fatalf("FormatLocations(empty) = %q, want empty", FormatLocations(locations))
	}
}

// --- document symbols -------------------------------------------------------

func TestDocumentSymbolsTree(t *testing.T) {
	client, _ := startTestClient(t, "happy")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	symbols, err := client.DocumentSymbols(testContext(t), testDocumentURI)
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(symbols) != 2 {
		t.Fatalf("symbols = %+v, want two roots", symbols)
	}
	service := symbols[0]
	if service.Name != "FakeService" || service.Kind != 5 || len(service.Children) != 2 {
		t.Fatalf("symbols[0] = %+v, want FakeService with two children", service)
	}
	formatted := FormatSymbols(symbols)
	for _, want := range []string{
		"class FakeService (1:1)",
		"  function Handler (3:3)",
		"  field Ctx (6:2)",
		"function helper (8:1)",
	} {
		if !strings.Contains(formatted, want) {
			t.Fatalf("FormatSymbols = %q, want it to contain %q", formatted, want)
		}
	}
}

// --- diagnostics ------------------------------------------------------------

func TestDiagnosticsCollected(t *testing.T) {
	for _, mode := range []string{"diagnostics", "diagnostics-late"} {
		t.Run(mode, func(t *testing.T) {
			client, _ := startTestClient(t, mode)
			mustInitialize(t, client)
			mustOpenDocument(t, client)

			diagnostics, err := client.Diagnostics(testContext(t), testDocumentURI)
			if err != nil {
				t.Fatalf("Diagnostics: %v", err)
			}
			if len(diagnostics) != 2 {
				t.Fatalf("diagnostics = %+v, want two", diagnostics)
			}
			formatted := FormatDiagnostics(diagnostics)
			for _, want := range []string{
				"error undefined: foo (2:3)",
				"warning unused variable x (4:1)",
			} {
				if !strings.Contains(formatted, want) {
					t.Fatalf("FormatDiagnostics = %q, want %q", formatted, want)
				}
			}
		})
	}
}

func TestDiagnosticsBestEffortReturnsNone(t *testing.T) {
	client, _ := startTestClient(t, "happy")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	// The happy fake server never publishes diagnostics: the client waits
	// DiagnosticsWait then reports none rather than blocking forever.
	start := time.Now()
	diagnostics, err := client.Diagnostics(testContext(t), testDocumentURI)
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v, want none", diagnostics)
	}
	if elapsed := time.Since(start); elapsed < DiagnosticsWait/2 {
		t.Fatalf("Diagnostics returned after %v, want it to wait ~%v", elapsed, DiagnosticsWait)
	}
}

// --- didOpen/didChange ------------------------------------------------------

func TestDidOpenAndChange(t *testing.T) {
	client, recordPath := startTestClient(t, "happy")
	mustInitialize(t, client)
	mustOpenDocument(t, client)
	// The same text does not produce a didChange.
	if err := client.DidOpen(testContext(t), testDocumentURI, testDocumentText); err != nil {
		t.Fatalf("DidOpen (same text): %v", err)
	}
	if err := client.DidOpen(testContext(t), testDocumentURI, "package main\n\nfunc main() { /* edited */ }\n"); err != nil {
		t.Fatalf("DidOpen (new text): %v", err)
	}

	records := waitForRecord(t, recordPath, "textDocument/didChange")
	didOpen := findRecord(t, records, "textDocument/didOpen")
	var openParams struct {
		TextDocument struct {
			URI        string `json:"uri"`
			LanguageID string `json:"languageId"`
			Version    int    `json:"version"`
			Text       string `json:"text"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(didOpen.Params, &openParams); err != nil {
		t.Fatalf("decode didOpen params: %v", err)
	}
	if openParams.TextDocument.URI != testDocumentURI || openParams.TextDocument.LanguageID != "go" ||
		openParams.TextDocument.Version != 1 || openParams.TextDocument.Text != testDocumentText {
		t.Fatalf("didOpen params = %+v", openParams)
	}

	didChange := findRecord(t, records, "textDocument/didChange")
	var changeParams struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int    `json:"version"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Text string `json:"text"`
		} `json:"contentChanges"`
	}
	if err := json.Unmarshal(didChange.Params, &changeParams); err != nil {
		t.Fatalf("decode didChange params: %v", err)
	}
	if changeParams.TextDocument.Version != 2 || len(changeParams.ContentChanges) != 1 ||
		!strings.Contains(changeParams.ContentChanges[0].Text, "edited") {
		t.Fatalf("didChange params = %+v, want full-text version 2", changeParams)
	}
}

// --- failures ---------------------------------------------------------------

func TestServerErrorResponse(t *testing.T) {
	client, _ := startTestClient(t, "error")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	_, err := client.Hover(testContext(t), testDocumentURI, 0, 0)
	if err == nil {
		t.Fatal("Hover succeeded, want the fake server error")
	}
	if !strings.Contains(err.Error(), "fake hover failure") || !strings.Contains(err.Error(), "-32000") {
		t.Fatalf("Hover error = %v, want the server's code and message", err)
	}
}

func TestRequestTimeoutHonorsContext(t *testing.T) {
	client, _ := startTestClient(t, "hang-hover")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := client.Hover(ctx, testDocumentURI, 0, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Hover(expired ctx) = %v, want context.DeadlineExceeded", err)
	}
}

func TestServerCrashWithoutResponse(t *testing.T) {
	client, _ := startTestClient(t, "crash-on-hover")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	_, err := client.Hover(testContext(t), testDocumentURI, 0, 0)
	if err == nil {
		t.Fatal("Hover succeeded, want a connection error after the server crashed")
	}
	if !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("Hover error = %v, want a connection-lost error", err)
	}
	if client.Err() == nil {
		t.Fatal("Client.Err() is nil after the server crashed")
	}
}

func TestServerCrashAtStartup(t *testing.T) {
	client, _ := startTestClient(t, "crash-on-start")
	err := client.Initialize(testContext(t))
	if err == nil {
		t.Fatal("Initialize succeeded, want the startup crash to surface")
	}
	if client.Err() == nil {
		t.Fatal("Client.Err() is nil after the startup crash")
	}
}

// --- lifecycle --------------------------------------------------------------

func TestShutdownExitCloseIdempotent(t *testing.T) {
	client, _ := startTestClient(t, "happy")
	mustInitialize(t, client)
	mustOpenDocument(t, client)

	ctx := testContext(t)
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if err := client.Exit(); err != nil {
		t.Fatalf("Exit: %v", err)
	}
	if err := client.Exit(); err != nil {
		t.Fatalf("second Exit: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if _, err := client.Hover(ctx, testDocumentURI, 0, 0); err == nil {
		t.Fatal("Hover after Close succeeded, want a closed error")
	}
	if client.Err() == nil {
		t.Fatal("Client.Err() is nil after Close")
	}
}

func TestCloseWithoutInitialize(t *testing.T) {
	client, _ := startTestClient(t, "happy")
	if err := client.Close(); err != nil {
		t.Fatalf("Close (never initialized): %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStartContextCancellationTerminatesServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client, err := Start(ctx, Config{
		Command: os.Args[0],
		Env:     []string{fakeModeEnv + "=happy"},
		Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	mustInitialize(t, client)
	cancel()

	deadline := time.Now().Add(3 * time.Second)
	for client.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("client did not notice the cancelled start context")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err = client.Hover(testContext(t), testDocumentURI, 0, 0)
	if err == nil {
		t.Fatal("Hover after context cancellation succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Hover error = %v, want it to wrap context.Canceled", err)
	}
}
