package manager

// Internal unit tests for the Server.Key precedence rules that are not directly
// observable through the public read model (the OAuth ServerName default).

import (
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/mcp"
	"github.com/zeokeel/zeokeel-rig/tools/mcp/auth"
)

func TestOAuthConfigServerNamePrecedence(t *testing.T) {
	base := Server{
		Name:   "display",
		Config: mcp.Config{URL: "https://example.test/mcp"},
		OAuth:  &auth.Config{},
	}
	cases := []struct {
		name           string
		server         Server
		managerKey     string
		wantServerName string
	}{
		{
			name:           "configured name without key",
			server:         base,
			managerKey:     "display",
			wantServerName: "display",
		},
		{
			name:           "key overrides the configured name",
			server:         func() Server { keyed := base; keyed.Key = "display#2"; return keyed }(),
			managerKey:     "display#2",
			wantServerName: "display#2",
		},
		{
			name: "explicit OAuth server name is kept without key",
			server: func() Server {
				server := base
				server.OAuth = &auth.Config{ServerName: "stored-identity"}
				return server
			}(),
			managerKey:     "display",
			wantServerName: "stored-identity",
		},
		{
			name: "key overrides an explicit OAuth server name",
			server: func() Server {
				server := base
				server.Key = "display#2"
				server.OAuth = &auth.Config{ServerName: "stored-identity"}
				return server
			}(),
			managerKey:     "display#2",
			wantServerName: "display#2",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config, ok := oauthConfigFor(test.server, test.managerKey)
			if !ok {
				t.Fatal("oauthConfigFor reported no OAuth configuration")
			}
			if config.ServerName != test.wantServerName {
				t.Fatalf("ServerName = %q, want %q", config.ServerName, test.wantServerName)
			}
		})
	}
}
