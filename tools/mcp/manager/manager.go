// Package manager supervises MCP servers for a host: one live rig mcp.Client
// per configured server, a bounded concurrent startup phase, per-server failure
// isolation, transparent reconnect after a dead transport, and one flat
// model-visible tool list.
//
// The package is host-agnostic. It does not read settings, own a tool registry,
// persist OAuth tokens or render status: the host supplies mcp.Config values
// (including an optional mcp.OAuthClient), a logger callback and the output
// budget, then consumes Manager.Tools/Clients/Errors and calls Reconnect/Close
// at its own lifecycle points.
//
// Semantics ported from the product's MCP runtime:
//   - servers start concurrently under one overall timeout; a server that fails
//     to start (bad command, unsupported URL, hung handshake) is recorded and
//     logged without failing the manager or the healthy servers;
//   - a disabled server never starts and contributes no tools;
//   - model-visible names are the rig MCP names (<prefix><server>__<tool>);
//     duplicate names across servers keep the first configured server's tool and
//     log the skip, so the host can always pass the result to agent.New;
//   - every tool dispatches through the server's current connection and, when
//     the transport died, rebuilds the connection and retries once;
//   - tool results are truncated to the configured byte budget with an explicit
//     truncation marker (head and tail preserved).
package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
	"github.com/zeokeel/zeokeel-rig/tools/mcp/auth"
)

const (
	// DefaultStartupTimeout bounds the whole MCP startup phase when
	// Config.StartupTimeout is zero.
	DefaultStartupTimeout = 5 * time.Second
	// DefaultReconnectTimeout bounds one transparent or explicit reconnect when
	// Config.ReconnectTimeout is zero.
	DefaultReconnectTimeout = 5 * time.Second
	// DefaultMaxOutputBytes bounds one tool result when Config.MaxOutputBytes is
	// zero (64 KiB: large enough for real payloads, small enough to protect the
	// model context).
	DefaultMaxOutputBytes = 64 << 10
)

var (
	// ErrClosed reports a Manager operation after Close.
	ErrClosed = errors.New("mcp/manager: manager is closed")
	// ErrNotStarted reports Reconnect before Start.
	ErrNotStarted = errors.New("mcp/manager: manager is not started")
	// ErrUnknownServer reports Reconnect for a name that is not configured.
	ErrUnknownServer = errors.New("mcp/manager: unknown server")
)

// Server is one configured MCP server.
type Server struct {
	// Name is the configured display name and the default manager identity used
	// by Clients/Errors/Reconnect and log lines. Empty falls back to
	// Config.ServerName, then "server-<n>".
	Name string
	// Key is an optional stable identity/namespace. When set it replaces Name
	// as the manager identity (Clients/Errors/Reconnect), the model-visible tool
	// namespace (mcp__<key>__<tool>) and the default OAuth ServerName, so two
	// configured servers may share a display Name as long as their Keys differ.
	// Empty keeps today's behavior (Name is the namespace).
	Key string
	// Config is the rig MCP client configuration (transport, command/url,
	// environment, headers, timeout, client info and optional OAuth client).
	Config mcp.Config
	// Disabled keeps the server from starting and from contributing tools.
	Disabled bool
	// DisabledTools lists model-visible tool names (for example
	// "mcp__files__read") this server must not expose.
	DisabledTools []string
	// OAuth, when set, builds an OAuth client through the manager's auth
	// runtime and injects it into Config before connecting. OAuth requires the
	// HTTP transport; an unfinished authorization simply leaves the server in
	// its OAuth-required state.
	OAuth *auth.Config
}

// Config configures a Manager.
type Config struct {
	// Servers are the configured MCP servers, in tool-priority order: when two
	// servers produce the same visible tool name the earlier one wins.
	Servers []Server
	// StartupTimeout bounds the whole startup phase (0 = DefaultStartupTimeout;
	// negative = the caller's context only).
	StartupTimeout time.Duration
	// ReconnectTimeout bounds one reconnect (0 = DefaultReconnectTimeout;
	// negative = the caller's context only).
	ReconnectTimeout time.Duration
	// MaxOutputBytes caps one tool result (0 = DefaultMaxOutputBytes; negative
	// disables truncation).
	MaxOutputBytes int
	// Resources also exposes the rig resource/prompt helper tools (list/read
	// resources, templates, prompts). Off by default.
	Resources bool
	// Logger receives connection lifecycle and skip diagnostics (nil discards).
	Logger func(format string, args ...any)
	// Auth optionally supplies a host-owned OAuth runtime. nil makes the
	// manager create (and close) its own runtime.
	Auth *auth.Runtime
	// OnAuthorized is called after an authorization completion reconnected a
	// server successfully (with the manager identity).
	OnAuthorized func(server string)
	// OnReconnect is called after every successful reconnect (transparent after
	// a dead transport, explicit Manager.Reconnect, or an authorization
	// completion), with the manager identity (Server.Key when set, else Name).
	// It is never called by the initial Start.
	OnReconnect func(server string)
}

// Manager owns the live MCP connections of one host activation. It is safe for
// concurrent use after Start.
type Manager struct {
	cfg  Config
	logf func(format string, args ...any)

	mu      sync.Mutex
	started bool
	closed  bool
	servers []*managedServer
	byName  map[string]*managedServer
	errs    map[string]error

	auth        *auth.Runtime
	ownsAuth    bool
	onAuthDone  func(server string)
	onReconnect func(server string)
	// startDone closes when the first Start finishes its startup and tool
	// registration, so Reconnect issued concurrently waits for a stable view.
	startDone     chan struct{}
	startDoneOnce sync.Once
}

// managedServer owns one configured server's live client and tool set.
type managedServer struct {
	name       string
	config     Server
	reconnectM sync.Mutex

	mu      sync.Mutex
	client  *mcp.Client
	inner   map[string]rig.Tool
	order   []string
	proxies map[string]rig.Tool
}

// New returns a manager for cfg. Nothing connects until Start.
func New(cfg Config) *Manager {
	cfg.Servers = append([]Server(nil), cfg.Servers...)
	logf := cfg.Logger
	if logf == nil {
		logf = func(string, ...any) {}
	}
	manager := &Manager{
		cfg:         cfg,
		logf:        logf,
		byName:      make(map[string]*managedServer),
		errs:        make(map[string]error),
		onAuthDone:  cfg.OnAuthorized,
		onReconnect: cfg.OnReconnect,
		startDone:   make(chan struct{}),
	}
	if cfg.Auth != nil {
		manager.auth = cfg.Auth
	} else {
		manager.ownsAuth = true
		manager.auth = auth.New(
			auth.WithLogger(logf),
			auth.WithOnAuthorized(func(server string) { _ = manager.Authorized(server) }),
		)
	}
	return manager
}

// Start connects every enabled server concurrently and collects their tools.
// A failing server is recorded and logged; Start itself only fails for a
// canceled/closed manager, so one bad connector never takes the healthy ones
// down. Calling Start again is a no-op.
func (m *Manager) Start(ctx context.Context) error {
	if m == nil {
		return errors.New("mcp/manager: nil manager")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.ownsAuth {
		m.auth.Start()
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	defer m.finishStart()
	servers := make([]*managedServer, len(m.cfg.Servers))
	byName := make(map[string]*managedServer, len(m.cfg.Servers))
	for index, server := range m.cfg.Servers {
		name := serverName(server, index)
		if _, duplicate := byName[name]; duplicate {
			// Configured names must be unique: the later entry is ignored rather
			// than overwriting the first one's client/error slot.
			m.logf("[mcp/manager] duplicate server name %q (entry %d skipped)", name, index+1)
			continue
		}
		managed := &managedServer{
			name:    name,
			config:  server,
			inner:   make(map[string]rig.Tool),
			proxies: make(map[string]rig.Tool),
		}
		servers[index] = managed
		byName[name] = managed
	}
	m.servers = servers
	m.byName = byName
	m.mu.Unlock()

	startCtx := ctx
	if m.cfg.StartupTimeout > 0 {
		var cancel context.CancelFunc
		startCtx, cancel = context.WithTimeout(ctx, m.cfg.StartupTimeout)
		defer cancel()
	}
	var group sync.WaitGroup
	for _, managed := range servers {
		if managed == nil {
			continue
		}
		if managed.config.Disabled {
			m.logf("[mcp/manager] server %q is disabled", managed.name)
			continue
		}
		group.Add(1)
		go func(managed *managedServer) {
			defer group.Done()
			m.startServer(startCtx, managed)
		}(managed)
	}
	group.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		for _, managed := range servers {
			if managed != nil {
				managed.closeClient()
			}
		}
		return ErrClosed
	}
	// Register in configuration order so duplicate visible names deterministically
	// keep the first server's tool.
	seen := make(map[string]bool)
	for _, managed := range servers {
		if managed == nil || managed.config.Disabled {
			continue
		}
		registered := 0
		for _, name := range managed.toolOrder() {
			if seen[name] {
				m.logf("[mcp/manager] server %q: skip duplicate tool %q", managed.name, name)
				continue
			}
			seen[name] = true
			meta := managed.currentTool(name)
			if meta == nil {
				continue
			}
			managed.addProxy(name, m.newReconnectTool(managed, name, meta))
			registered++
		}
		if managed.currentClient() != nil {
			m.logf("[mcp/manager] server %q connected: %d tool(s)", managed.name, registered)
		}
	}
	return nil
}

// startServer connects one server and lists its tools. Failures are recorded
// and logged, never returned (startup isolation).
func (m *Manager) startServer(ctx context.Context, managed *managedServer) {
	clientConfig, err := m.clientConfig(managed.config, managed.name)
	if err != nil {
		m.recordError(managed.name, err)
		m.logf("[mcp/manager] server %q oauth setup failed: %v", managed.name, err)
		return
	}
	client, err := mcp.Start(ctx, clientConfig)
	if err != nil {
		m.recordError(managed.name, err)
		m.logf("[mcp/manager] server %q connect failed: %v", managed.name, err)
		return
	}
	infos, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		m.recordError(managed.name, fmt.Errorf("list tools: %w", err))
		m.logf("[mcp/manager] server %q list tools failed: %v", managed.name, err)
		return
	}
	inner, order := m.buildTools(managed, client, infos)
	managed.mu.Lock()
	managed.client = client
	managed.inner = inner
	managed.order = order
	managed.mu.Unlock()
	m.clearError(managed.name)
}

// buildTools adapts a server's tool list (plus optional resource helpers),
// applies the disabled lists and the output budget, and returns the live tools
// in list order.
func (m *Manager) buildTools(managed *managedServer, client *mcp.Client, infos []mcp.ToolInfo) (map[string]rig.Tool, []string) {
	// The namespace is the optional Server.Key, else the client's server name
	// (today's behavior). AggregateTools namespaces both the regular and the
	// resource/prompt helper tools from the same server part.
	tools, _, err := mcp.AggregateTools([]mcp.ServerTools{{
		Client:    client,
		Server:    managed.toolNamespace(client),
		Tools:     infos,
		Resources: m.cfg.Resources,
	}}, "")
	if err != nil {
		m.logf("[mcp/manager] server %q: build tools: %v", managed.name, err)
		return map[string]rig.Tool{}, nil
	}
	disabled := make(map[string]bool, len(m.cfg.Servers)+len(managed.config.DisabledTools))
	for _, name := range managed.config.DisabledTools {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			disabled[trimmed] = true
		}
	}
	inner := make(map[string]rig.Tool, len(tools))
	order := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(tool.Name())
		if name == "" {
			continue
		}
		if disabled[name] {
			m.logf("[mcp/manager] server %q: skip disabled tool %q", managed.name, name)
			continue
		}
		if _, duplicate := inner[name]; duplicate {
			m.logf("[mcp/manager] server %q: skip duplicate tool %q", managed.name, name)
			continue
		}
		inner[name] = m.limitTool(tool)
		order = append(order, name)
	}
	return inner, order
}

// limitTool applies the configured output budget to one tool.
func (m *Manager) limitTool(tool rig.Tool) rig.Tool {
	limit := m.cfg.MaxOutputBytes
	if limit == 0 {
		limit = DefaultMaxOutputBytes
	}
	if limit < 0 {
		return tool
	}
	return &limitedTool{inner: tool, limit: limit}
}

// newReconnectTool wraps one live tool in the reconnect-and-retry proxy.
func (m *Manager) newReconnectTool(managed *managedServer, name string, meta rig.Tool) rig.Tool {
	return &reconnectTool{manager: m, server: managed, name: name, meta: meta}
}

// Reconnect rebuilds one server's connection on demand (for example after an
// OAuth authorization). The old connection is closed on success; on failure the
// previous client stays installed and the error is also recorded in Errors.
func (m *Manager) Reconnect(ctx context.Context, name string) error {
	if m == nil {
		return errors.New("mcp/manager: nil manager")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	closed, started := m.closed, m.started
	managed := m.byName[strings.TrimSpace(name)]
	m.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if !started {
		return ErrNotStarted
	}
	// A Reconnect issued while Start is still handshaking waits for the startup
	// and tool-registration phase instead of racing the initial client install.
	select {
	case <-m.startDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	if managed == nil {
		return fmt.Errorf("%w: %q", ErrUnknownServer, strings.TrimSpace(name))
	}
	return m.reconnectServer(ctx, managed, nil)
}

// Auth returns the OAuth runtime the manager uses. A host that configured
// Config.Auth receives its own runtime; otherwise this is the manager-owned one.
func (m *Manager) Auth() *auth.Runtime {
	if m == nil {
		return nil
	}
	return m.auth
}

// AuthStatus returns the OAuth status snapshot of one configured server. The
// bool is false when the server has no OAuth configuration. It is a pure read
// (no discovery, no token I/O).
func (m *Manager) AuthStatus(server string) (mcp.OAuthStatus, bool) {
	if m == nil || m.auth == nil {
		return mcp.OAuthStatus{}, false
	}
	m.mu.Lock()
	managed := m.managedByNameLocked(server)
	m.mu.Unlock()
	if managed == nil {
		return mcp.OAuthStatus{}, false
	}
	oauthConfig, ok := oauthConfigFor(managed.config, managed.name)
	if !ok {
		return mcp.OAuthStatus{}, false
	}
	return m.auth.Status(oauthConfig), true
}

// RefreshAuth proactively refreshes every authorized OAuth client whose token
// expires within the runtime's refresh window. It returns the number refreshed.
func (m *Manager) RefreshAuth(ctx context.Context) int {
	if m == nil || m.auth == nil {
		return 0
	}
	return m.auth.RefreshDue(ctx, 0)
}

// Authorized is called when an interactive authorization completed for server:
// it rebuilds the server connection and then invokes Config.OnAuthorized. The
// manager wires its own auth runtime to this method; a host supplying its own
// runtime should call it from that runtime's WithOnAuthorized callback.
func (m *Manager) Authorized(server string) error {
	if m == nil {
		return errors.New("mcp/manager: nil manager")
	}
	m.mu.Lock()
	managed := m.managedByNameLocked(server)
	m.mu.Unlock()
	if managed == nil {
		return fmt.Errorf("%w: %q", ErrUnknownServer, strings.TrimSpace(server))
	}
	if err := m.reconnectServer(context.Background(), managed, nil); err != nil {
		return err
	}
	if m.onAuthDone != nil {
		m.onAuthDone(managed.name)
	}
	return nil
}

// managedByNameLocked resolves a server by configured name or manager identity.
func (m *Manager) managedByNameLocked(name string) *managedServer {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if managed := m.byName[name]; managed != nil {
		return managed
	}
	for _, managed := range m.servers {
		if managed != nil && strings.EqualFold(strings.TrimSpace(managed.config.Name), name) {
			return managed
		}
	}
	return nil
}

// oauthConfigFor derives a server's OAuth configuration: the configured name
// identifies the stored token and the MCP URL is the default resource.
func oauthConfigFor(server Server, managerKey string) (auth.Config, bool) {
	if server.OAuth == nil {
		return auth.Config{}, false
	}
	oauth := *server.OAuth
	if key := strings.TrimSpace(server.Key); key != "" {
		// A keyed server is a distinct identity even when two rows share a
		// display name: its tokens/cache must not be shared.
		oauth.ServerName = key
	} else if strings.TrimSpace(oauth.ServerName) == "" {
		oauth.ServerName = strings.TrimSpace(server.Name)
	}
	if strings.TrimSpace(oauth.ServerName) == "" {
		oauth.ServerName = managerKey
	}
	if strings.TrimSpace(oauth.Resource) == "" {
		oauth.Resource = strings.TrimSpace(server.Config.URL)
	}
	oauth.Scopes = append([]string(nil), oauth.Scopes...)
	return oauth, true
}

// clientConfig builds the rig MCP client configuration for one server, injecting
// the OAuth HTTP client when the row configures OAuth.
func (m *Manager) clientConfig(server Server, managerKey string) (mcp.Config, error) {
	config := server.Config
	oauthConfig, ok := oauthConfigFor(server, managerKey)
	if !ok {
		return config, nil
	}
	if strings.TrimSpace(config.Command) != "" ||
		strings.EqualFold(strings.TrimSpace(config.Transport), mcp.TransportStdio) {
		return mcp.Config{}, errors.New("mcp/manager: OAuth requires the http transport")
	}
	client, err := m.auth.Client(oauthConfig)
	if err != nil {
		return mcp.Config{}, err
	}
	config.OAuth = client
	if strings.TrimSpace(config.Transport) == "" {
		config.Transport = mcp.TransportHTTP
	}
	return config, nil
}

// reconnectServer rebuilds one server's connection. observed is the client the
// caller saw fail: when another goroutine already replaced it this is a no-op
// success (nil observed forces an explicit reconnect).
func (m *Manager) reconnectServer(ctx context.Context, managed *managedServer, observed *mcp.Client) error {
	managed.reconnectM.Lock()
	defer managed.reconnectM.Unlock()
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if observed != nil && managed.currentClient() != observed {
		// Someone else already reconnected; the connection is usable again.
		return nil
	}
	reconnectCtx := ctx
	if m.cfg.ReconnectTimeout > 0 {
		var cancel context.CancelFunc
		reconnectCtx, cancel = context.WithTimeout(ctx, m.cfg.ReconnectTimeout)
		defer cancel()
	}
	clientConfig, err := m.clientConfig(managed.config, managed.name)
	if err != nil {
		m.recordError(managed.name, err)
		m.logf("[mcp/manager] server %q oauth setup failed: %v", managed.name, err)
		return fmt.Errorf("mcp/manager: server %q oauth setup: %w", managed.name, err)
	}
	client, err := mcp.Start(reconnectCtx, clientConfig)
	if err != nil {
		m.recordError(managed.name, err)
		m.logf("[mcp/manager] server %q reconnect failed: %v", managed.name, err)
		return fmt.Errorf("mcp/manager: server %q reconnect: %w", managed.name, err)
	}
	infos, err := client.ListTools(reconnectCtx)
	if err != nil {
		_ = client.Close()
		m.recordError(managed.name, fmt.Errorf("list tools: %w", err))
		m.logf("[mcp/manager] server %q reconnect list tools failed: %v", managed.name, err)
		return fmt.Errorf("mcp/manager: server %q reconnect: list tools: %w", managed.name, err)
	}
	inner, order := m.buildTools(managed, client, infos)

	// The final guarded swap holds the manager lock close uses, so a Close that
	// landed during the handshake can never be overwritten by this reconnect.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = client.Close()
		return ErrClosed
	}
	managed.mu.Lock()
	old := managed.client
	managed.client = client
	managed.inner = inner
	managed.order = order
	proxies := make(map[string]rig.Tool, len(order))
	for _, name := range order {
		proxy := managed.proxies[name]
		if proxy == nil {
			proxy = m.newReconnectTool(managed, name, inner[name])
		}
		proxies[name] = proxy
	}
	managed.proxies = proxies
	managed.mu.Unlock()
	delete(m.errs, managed.name)
	m.mu.Unlock()
	if old != nil && old != client {
		_ = old.Close()
	}
	m.logf("[mcp/manager] server %q reconnected: %d tool(s)", managed.name, len(order))
	if m.onReconnect != nil {
		m.onReconnect(managed.name)
	}
	return nil
}

// Clients returns a snapshot of the live clients keyed by configured server
// name. After Close it is empty.
func (m *Manager) Clients() map[string]*mcp.Client {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return map[string]*mcp.Client{}
	}
	clients := make(map[string]*mcp.Client, len(m.servers))
	for _, managed := range m.servers {
		if managed == nil {
			continue
		}
		if client := managed.currentClient(); client != nil {
			clients[managed.name] = client
		}
	}
	return clients
}

// Tools returns the current model-visible tools in configuration order. The
// slice is a snapshot: call it again after a reconnect that changed the tool
// set. After Close it is empty.
func (m *Manager) Tools() []rig.Tool {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	var tools []rig.Tool
	for _, managed := range m.servers {
		if managed == nil {
			continue
		}
		tools = append(tools, managed.toolProxies()...)
	}
	return tools
}

// Errors returns a snapshot of per-server connection failures (startup or
// reconnect). A later successful reconnect removes the entry.
func (m *Manager) Errors() map[string]error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	errs := make(map[string]error, len(m.errs))
	for name, err := range m.errs {
		errs[name] = err
	}
	return errs
}

// Close closes every live client and makes the manager terminal. It is
// idempotent.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	servers := m.servers
	m.servers = nil
	m.byName = nil
	m.errs = make(map[string]error)
	m.mu.Unlock()
	for _, managed := range servers {
		if managed != nil {
			managed.closeClient()
		}
	}
	if m.ownsAuth {
		m.auth.Stop()
		_ = m.auth.Close()
	}
	return nil
}

// finishStart closes the startup gate exactly once.
func (m *Manager) finishStart() {
	m.startDoneOnce.Do(func() { close(m.startDone) })
}

// recordError stores one server's last failure.
func (m *Manager) recordError(name string, err error) {
	m.mu.Lock()
	if !m.closed && m.errs != nil {
		m.errs[name] = err
	}
	m.mu.Unlock()
}

// clearError removes one server's failure after a successful connect.
func (m *Manager) clearError(name string) {
	m.mu.Lock()
	if !m.closed && m.errs != nil {
		delete(m.errs, name)
	}
	m.mu.Unlock()
}

// serverName resolves the manager identity of one configured server: the
// optional Key, then the display Name, then Config.ServerName, then a generated
// fallback.
func serverName(server Server, index int) string {
	if key := strings.TrimSpace(server.Key); key != "" {
		return key
	}
	if name := strings.TrimSpace(server.Name); name != "" {
		return name
	}
	if name := strings.TrimSpace(server.Config.ServerName); name != "" {
		return name
	}
	return fmt.Sprintf("server-%d", index+1)
}

// toolNamespace is the model-visible server part: Server.Key when set, else the
// live client's server name (today's behavior).
func (s *managedServer) toolNamespace(client *mcp.Client) string {
	if s != nil {
		if key := strings.TrimSpace(s.config.Key); key != "" {
			return key
		}
	}
	if client != nil {
		return client.ServerName()
	}
	return ""
}

// currentClient returns the live client, if any.
func (s *managedServer) currentClient() *mcp.Client {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// currentTool returns the live implementation of one visible tool.
func (s *managedServer) currentTool(name string) rig.Tool {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner[name]
}

// toolOrder returns a copy of the live tool order.
func (s *managedServer) toolOrder() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// addProxy records one exposed proxy.
func (s *managedServer) addProxy(name string, proxy rig.Tool) {
	s.mu.Lock()
	s.proxies[name] = proxy
	s.mu.Unlock()
}

// toolProxies returns the exposed proxies in live tool order.
func (s *managedServer) toolProxies() []rig.Tool {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tools := make([]rig.Tool, 0, len(s.order))
	for _, name := range s.order {
		if proxy := s.proxies[name]; proxy != nil {
			tools = append(tools, proxy)
		}
	}
	return tools
}

// closeClient closes and drops the live client.
func (s *managedServer) closeClient() {
	if s == nil {
		return
	}
	s.mu.Lock()
	client := s.client
	s.client = nil
	s.inner = nil
	s.order = nil
	s.proxies = nil
	s.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}
