package web_test

// Tests for SSRF-safe dialing: when the policy implements web.HostResolver the
// fetcher resolves each host itself, validates/pins the IP, and refuses a
// blocked DNS alias (including on redirect).

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/web"
)

// resolvingPolicy is a web.Policy + web.HostResolver with an explicit host map.
type resolvingPolicy struct {
	hosts   map[string][]net.IP
	blocked map[string]bool
}

func (p *resolvingPolicy) CheckURL(*url.URL) error { return nil }

func (p *resolvingPolicy) ResolveHost(_ context.Context, host string) ([]net.IP, error) {
	if p.blocked[host] {
		return nil, fmt.Errorf("host %q is blocked", host)
	}
	ips, ok := p.hosts[host]
	if !ok {
		return nil, fmt.Errorf("host %q is not resolvable", host)
	}
	return ips, nil
}

func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
}

func TestFetchResolveHostBlocksDNSAlias(t *testing.T) {
	clearProxyEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "SECRET-INTERNAL-SERVICE")
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port

	policy := &resolvingPolicy{blocked: map[string]bool{"internal.example": true}}
	result, err := web.NewHTTPFetcher(web.WithPolicy(policy)).Invoke(context.Background(), rig.ToolCall{
		Name: "web_fetch", Arguments: fmt.Sprintf(`{"url":"http://internal.example:%d/"}`, port),
	})
	if err == nil && !result.IsError {
		t.Fatalf("blocked DNS alias was fetched: %q", result.Content)
	}
	if strings.Contains(result.Content, "SECRET-INTERNAL-SERVICE") {
		t.Fatalf("internal content leaked: %q", result.Content)
	}
}

func TestFetchResolveHostPinsValidatedIP(t *testing.T) {
	clearProxyEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "public-content-ok")
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port

	policy := &resolvingPolicy{hosts: map[string][]net.IP{
		"public.example": {net.ParseIP("127.0.0.1")},
	}}
	result, err := web.NewHTTPFetcher(web.WithPolicy(policy)).Invoke(context.Background(), rig.ToolCall{
		Name: "web_fetch", Arguments: fmt.Sprintf(`{"url":"http://public.example:%d/"}`, port),
	})
	if err != nil || result.IsError {
		t.Fatalf("pinned fetch failed: err=%v result=%+v", err, result)
	}
	if !strings.Contains(result.Content, "public-content-ok") {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestFetchResolveHostBlocksRedirectAlias(t *testing.T) {
	clearProxyEnv(t)
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "SECRET-INTERNAL-SERVICE")
	}))
	defer secret.Close()
	secretPort := secret.Listener.Addr().(*net.TCPAddr).Port

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://internal.example:%d/", secretPort), http.StatusFound)
	}))
	defer redirector.Close()
	redirectPort := redirector.Listener.Addr().(*net.TCPAddr).Port

	policy := &resolvingPolicy{
		hosts:   map[string][]net.IP{"redirect.example": {net.ParseIP("127.0.0.1")}},
		blocked: map[string]bool{"internal.example": true},
	}
	result, err := web.NewHTTPFetcher(web.WithPolicy(policy)).Invoke(context.Background(), rig.ToolCall{
		Name: "web_fetch", Arguments: fmt.Sprintf(`{"url":"http://redirect.example:%d/"}`, redirectPort),
	})
	if err == nil && !result.IsError {
		t.Fatalf("redirect to a blocked alias was followed: %q", result.Content)
	}
	if strings.Contains(result.Content, "SECRET-INTERNAL-SERVICE") {
		t.Fatalf("internal content leaked through redirect: %q", result.Content)
	}
}

func TestFetchPolicyWithoutHostResolverStillWorks(t *testing.T) {
	clearProxyEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "plain-policy-ok")
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port

	result, err := web.NewHTTPFetcher(web.WithPolicy(web.PolicyFunc(nil))).Invoke(context.Background(), rig.ToolCall{
		Name: "web_fetch", Arguments: fmt.Sprintf(`{"url":"http://127.0.0.1:%d/"}`, port),
	})
	if err != nil || result.IsError {
		t.Fatalf("policy without HostResolver broke fetch: err=%v result=%+v", err, result)
	}
	if !strings.Contains(result.Content, "plain-policy-ok") {
		t.Fatalf("content = %q", result.Content)
	}
}
